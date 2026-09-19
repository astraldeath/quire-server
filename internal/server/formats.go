// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"image"
	"image/jpeg"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
)

var bookFormats = []string{"epub", "cbz", "fb2", "fbz", "mobi", "azw3", "pdf"}

func validBookFormat(format string) bool {
	for _, f := range bookFormats {
		if f == format {
			return true
		}
	}
	return false
}
func bookMIME(format string) string {
	switch format {
	case "pdf":
		return "application/pdf"
	case "epub":
		return "application/epub+zip"
	case "cbz":
		return "application/vnd.comicbook+zip"
	case "fb2":
		return "application/x-fictionbook+xml"
	case "fbz":
		return "application/zip"
	case "mobi":
		return "application/x-mobipocket-ebook"
	case "azw3":
		return "application/vnd.amazon.ebook"
	}
	return "application/octet-stream"
}
func validFolder(folder string) bool {
	if folder == "" {
		return true
	}
	if len(folder) > 1024 || strings.ContainsAny(folder, "\\:") || strings.IndexFunc(folder, unicode.IsControl) >= 0 {
		return false
	}
	parts := strings.Split(folder, "/")
	if len(parts) > 32 {
		return false
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || len(p) > 255 || strings.TrimSpace(p) != p {
			return false
		}
	}
	return true
}
func supportedBookFilename(name string) bool {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
	return validBookFormat(ext) || strings.HasSuffix(strings.ToLower(name), ".fb2.zip")
}

func comicImage(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".avif":
		return true
	}
	return false
}

// AVIF uses ISO BMFF. Validate the bounded container without requiring an AV1
// decoder; the browser handles pixel decoding and these covers remain absent.
func validAVIF(b []byte) bool {
	brand, meta, data := false, false, false
	for offset := 0; offset < len(b); {
		if len(b)-offset < 8 {
			return false
		}
		size := uint64(binary.BigEndian.Uint32(b[offset : offset+4]))
		header := 8
		if size == 1 {
			if len(b)-offset < 16 {
				return false
			}
			size = binary.BigEndian.Uint64(b[offset+8 : offset+16])
			header = 16
		}
		if size == 0 {
			size = uint64(len(b) - offset)
		}
		if size < uint64(header) || size > uint64(len(b)-offset) {
			return false
		}
		typ := string(b[offset+4 : offset+8])
		payload := b[offset+header : offset+int(size)]
		switch typ {
		case "ftyp":
			if len(payload) < 8 {
				return false
			}
			for i := 0; i+4 <= len(payload); i += 4 {
				if i != 4 && (string(payload[i:i+4]) == "avif" || string(payload[i:i+4]) == "avis") {
					brand = true
				}
			}
		case "meta":
			meta = len(payload) > 4
		case "mdat":
			data = len(payload) > 0
		}
		offset += int(size)
	}
	return brand && meta && data
}
func readMember(f *zip.File, limit int64) ([]byte, error) {
	if f.UncompressedSize64 > uint64(limit) {
		return nil, ErrInvalid
	}
	r, err := f.Open()
	if err != nil {
		return nil, ErrInvalid
	}
	defer r.Close()
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, ErrInvalid
	}
	return b, nil
}
func validateArchive(z *zip.ReadCloser) error {
	if len(z.File) > 10000 {
		return ErrInvalid
	}
	var total uint64
	seen := map[string]bool{}
	for _, f := range z.File {
		name := strings.TrimSuffix(f.Name, "/")
		if seen[f.Name] || name == "" || strings.ContainsAny(name, "\\:") || strings.HasPrefix(name, "/") || path.Clean(name) != name || strings.HasPrefix(name, "../") || name == ".." || f.Mode()&os.ModeSymlink != 0 || f.Flags&1 != 0 {
			return ErrInvalid
		}
		seen[f.Name] = true
		if f.UncompressedSize64 > 512<<20-total {
			return ErrInvalid
		}
		total += f.UncompressedSize64
	}
	return nil
}

// Detection is based on bounded content inspection, independent of caller MIME or extension.
func detectBookFormat(filename string) (string, error) {
	if z, err := zip.OpenReader(filename); err == nil {
		defer z.Close()
		if validateArchive(z) != nil {
			return "", ErrInvalid
		}
		hasEPUB, fb2Count, images := false, 0, 0
		for _, f := range z.File {
			if f.Name == "mimetype" || f.Name == "META-INF/container.xml" {
				hasEPUB = true
			}
		}
		if hasEPUB {
			if validateEPUB(filename) != nil {
				return "", ErrInvalid
			}
			return "epub", nil
		}
		for _, f := range z.File {
			if f.FileInfo().IsDir() {
				continue
			}
			ext := strings.ToLower(path.Ext(f.Name))
			if ext == ".fb2" {
				b, e := readMember(f, 32<<20)
				if e != nil || validateFB2(b) != nil {
					return "", ErrInvalid
				}
				fb2Count++
				continue
			}
			if comicImage(f.Name) {
				b, e := readMember(f, 32<<20)
				if e != nil {
					return "", ErrInvalid
				}
				if ext == ".avif" {
					if !validAVIF(b) {
						return "", ErrInvalid
					}
					images++
					continue
				}
				c, _, e := image.DecodeConfig(bytes.NewReader(b))
				if e != nil || c.Width <= 0 || c.Height <= 0 || int64(c.Width)*int64(c.Height) > 100_000_000 {
					return "", ErrInvalid
				}
				images++
			}
		}
		if fb2Count == 1 && images == 0 {
			return "fbz", nil
		}
		if images > 0 && fb2Count == 0 {
			return "cbz", nil
		}
		return "", ErrInvalid
	}
	f, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer f.Close()
	head := make([]byte, 78)
	n, _ := io.ReadFull(f, head)
	if n >= 8 && bytes.HasPrefix(head[:n], []byte("%PDF-")) {
		if validatePDF(f, head[:n]) != nil {
			return "", ErrInvalid
		}
		return "pdf", nil
	}
	if n == 78 && string(head[60:68]) == "BOOKMOBI" {
		return validateMOBI(f, head)
	}
	if _, err = f.Seek(0, 0); err != nil {
		return "", err
	}
	b, err := io.ReadAll(io.LimitReader(f, 32<<20+1))
	if err != nil || len(b) > 32<<20 || validateFB2(b) != nil {
		return "", ErrInvalid
	}
	return "fb2", nil
}

// PDF inspection checks the bounded file envelope and cross-reference target.
// It does not render, decompress streams, execute actions, or validate every object.
var pdfVersion = regexp.MustCompile(`^%PDF-(1\.[0-7]|2\.0)[\r\n]`)
var pdfTrailer = regexp.MustCompile(`startxref[\x00\t\n\f\r ]+([0-9]+)[\x00\t\n\f\r ]+%%EOF[\x00\t\n\f\r ]*$`)
var pdfXRefStream = regexp.MustCompile(`^[0-9]+\s+[0-9]+\s+obj\s*<<`)
var pdfXRefType = regexp.MustCompile(`/Type\s*/XRef\b`)

func validatePDF(f *os.File, head []byte) error {
	if !pdfVersion.Match(head) {
		return ErrInvalid
	}
	info, err := f.Stat()
	if err != nil || info.Size() < 20 {
		return ErrInvalid
	}
	size := info.Size()
	tailSize := min(size, int64(4096))
	tail := make([]byte, tailSize)
	if _, err = f.ReadAt(tail, size-tailSize); err != nil {
		return ErrInvalid
	}
	trailer := pdfTrailer.FindSubmatch(tail)
	if trailer == nil {
		return ErrInvalid
	}
	offset, err := strconv.ParseInt(string(trailer[1]), 10, 64)
	if err != nil || offset < 9 || offset >= size-tailSize+int64(bytes.LastIndex(tail, []byte("startxref"))) {
		return ErrInvalid
	}
	target := make([]byte, min(int64(4096), size-offset))
	if _, err = f.ReadAt(target, offset); err != nil {
		return ErrInvalid
	}
	if bytes.HasPrefix(target, []byte("xref")) && len(target) > 4 && bytes.ContainsAny(target[4:5], "\r\n \t") {
		return nil
	}
	// Stream dictionaries precede binary data; limit inspection to that dictionary.
	if end := bytes.Index(target, []byte("stream")); end >= 0 {
		target = target[:end]
	}
	if pdfXRefStream.Match(target) && pdfXRefType.Match(target) {
		return nil
	}
	return ErrInvalid
}

func validateFB2(b []byte) error {
	d := xml.NewDecoder(bytes.NewReader(b))
	depth, roots := 0, 0
	body := false
	for {
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ErrInvalid
		}
		switch t := token.(type) {
		case xml.Directive:
			return ErrInvalid
		case xml.StartElement:
			if depth == 0 {
				roots++
				if roots != 1 || t.Name.Local != "FictionBook" {
					return ErrInvalid
				}
			}
			depth++
			if depth > 128 {
				return ErrInvalid
			}
			if depth == 2 && t.Name.Local == "body" {
				body = true
			}
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(t)) != "" {
				return ErrInvalid
			}
		}
	}
	if roots != 1 || depth != 0 || !body {
		return ErrInvalid
	}
	return nil
}
func validateMOBI(f *os.File, head []byte) (string, error) {
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	count := int(binary.BigEndian.Uint16(head[76:78]))
	if count < 2 || count > 10000 {
		return "", ErrInvalid
	}
	table := make([]byte, count*8)
	if _, err = io.ReadFull(f, table); err != nil {
		return "", ErrInvalid
	}
	offsets := make([]int64, count+1)
	offsets[count] = info.Size()
	for i := 0; i < count; i++ {
		offsets[i] = int64(binary.BigEndian.Uint32(table[i*8 : i*8+4]))
		if offsets[i] < int64(78+count*8) || offsets[i] >= info.Size() || (i > 0 && offsets[i] <= offsets[i-1]) {
			return "", ErrInvalid
		}
	}
	if offsets[1]-offsets[0] < 132 {
		return "", ErrInvalid
	}
	rec := make([]byte, min(offsets[1]-offsets[0], 1<<20))
	if _, err = f.ReadAt(rec, offsets[0]); err != nil {
		return "", ErrInvalid
	}
	compression := binary.BigEndian.Uint16(rec[:2])
	textRecords := int(binary.BigEndian.Uint16(rec[8:10]))
	if (compression != 1 && compression != 2 && compression != 17480) || binary.BigEndian.Uint16(rec[12:14]) != 0 || textRecords < 1 || textRecords >= count || string(rec[16:20]) != "MOBI" {
		return "", ErrInvalid
	}
	length := int(binary.BigEndian.Uint32(rec[20:24]))
	version := binary.BigEndian.Uint32(rec[36:40])
	if length < 116 || length > len(rec)-16 || version > 8 {
		return "", ErrInvalid
	}
	if length >= 152 {
		drm := binary.BigEndian.Uint32(rec[164:168])
		if drm != 0 && drm != 0xffffffff {
			return "", ErrInvalid
		}
	}
	if version >= 8 {
		return "azw3", nil
	}
	return "mobi", nil
}

func cleanBookText(v string) string {
	v = strings.Join(strings.Fields(v), " ")
	r := []rune(v)
	if len(r) > 1000 {
		r = r[:1000]
	}
	return string(r)
}

func mobiMetadata(filename string) bookMetadata {
	result := bookMetadata{}
	f, err := os.Open(filename)
	if err != nil {
		return result
	}
	defer f.Close()
	head := make([]byte, 94)
	if _, err = io.ReadFull(f, head); err != nil {
		return result
	}
	start, end := int64(binary.BigEndian.Uint32(head[78:82])), int64(binary.BigEndian.Uint32(head[86:90]))
	if start < 94 || end-start < 132 || end-start > 1<<20 {
		return result
	}
	rec := make([]byte, end-start)
	if _, err = f.ReadAt(rec, start); err != nil {
		return result
	}
	text := func(b []byte) string {
		return cleanBookText(strings.ToValidUTF8(string(bytes.TrimRight(b, "\x00")), "�"))
	}
	offset, length := uint64(binary.BigEndian.Uint32(rec[84:88])), uint64(binary.BigEndian.Uint32(rec[88:92]))
	if offset+length <= uint64(len(rec)) {
		result.Title = text(rec[offset : offset+length])
	}
	if result.Title == "" {
		result.Title = text(head[:32])
	}
	exth := uint64(16) + uint64(binary.BigEndian.Uint32(rec[20:24]))
	if exth+12 <= uint64(len(rec)) && string(rec[exth:exth+4]) == "EXTH" {
		end := exth + uint64(binary.BigEndian.Uint32(rec[exth+4:exth+8]))
		count := binary.BigEndian.Uint32(rec[exth+8 : exth+12])
		pos := exth + 12
		if end > uint64(len(rec)) || count > 1000 {
			return result
		}
		for i := uint32(0); i < count && pos+8 <= end; i++ {
			kind := binary.BigEndian.Uint32(rec[pos : pos+4])
			size := uint64(binary.BigEndian.Uint32(rec[pos+4 : pos+8]))
			if size < 8 || pos+size > end {
				break
			}
			value := text(rec[pos+8 : pos+size])
			if kind == 100 {
				if result.Author != "" {
					result.Author += ", "
				}
				result.Author += value
			}
			if kind == 503 {
				result.Title = value
			}
			pos += size
		}
	}
	result.Author = cleanBookText(result.Author)
	return result
}
func thumbnail(b []byte) string {
	c, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil || c.Width <= 0 || c.Height <= 0 || int64(c.Width)*int64(c.Height) > 20_000_000 {
		return ""
	}
	src, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return ""
	}
	w, h := c.Width, c.Height
	if w > 320 {
		h = h * 320 / w
		w = 320
	}
	if h > 480 {
		w = w * 480 / h
		h = 480
	}
	w = max(w, 1)
	h = max(h, 1)
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	bounds := src.Bounds()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dst.Set(x, y, src.At(bounds.Min.X+x*c.Width/w, bounds.Min.Y+y*c.Height/h))
		}
	}
	var out bytes.Buffer
	if jpeg.Encode(&out, dst, &jpeg.Options{Quality: 85}) != nil {
		return ""
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(out.Bytes())
}
func fb2Metadata(b []byte) bookMetadata {
	var doc struct {
		Title struct {
			Name    string `xml:"book-title"`
			Authors []struct {
				First  string `xml:"first-name"`
				Middle string `xml:"middle-name"`
				Last   string `xml:"last-name"`
			} `xml:"author"`
			Sequence struct {
				Name   string   `xml:"name,attr"`
				Number *float64 `xml:"number,attr"`
			} `xml:"sequence"`
			Cover struct {
				Href string `xml:"href,attr"`
			} `xml:"coverpage>image"`
		} `xml:"description>title-info"`
		Binary []struct {
			ID    string `xml:"id,attr"`
			Value string `xml:",chardata"`
		} `xml:"binary"`
	}
	result := bookMetadata{}
	if xml.Unmarshal(b, &doc) != nil {
		return result
	}
	result.Title = cleanBookText(doc.Title.Name)
	authors := []string{}
	for _, a := range doc.Title.Authors {
		authors = append(authors, cleanBookText(a.First+" "+a.Middle+" "+a.Last))
	}
	result.Author = cleanBookText(strings.Join(authors, ", "))
	result.Series = cleanBookText(doc.Title.Sequence.Name)
	result.Volume = doc.Title.Sequence.Number
	if result.Volume != nil && !(*result.Volume >= 0 && *result.Volume < 1e6) {
		result.Volume = nil
	}
	for _, bin := range doc.Binary {
		if "#"+bin.ID == doc.Title.Cover.Href && len(bin.Value) <= 12<<20 {
			data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(bin.Value), ""))
			if err == nil {
				result.Cover = thumbnail(data)
			}
			break
		}
	}
	return result
}
func formatMetadata(filename string) bookMetadata {
	format := strings.TrimPrefix(filepath.Ext(filename), ".")
	result := bookMetadata{Format: format}
	switch format {
	case "epub":
		result = epubMetadata(filename)
	case "fb2":
		if b, err := os.ReadFile(filename); err == nil && len(b) <= 32<<20 {
			result = fb2Metadata(b)
		}
	case "mobi", "azw3":
		result = mobiMetadata(filename)
	case "cbz", "fbz":
		z, err := zip.OpenReader(filename)
		if err != nil {
			return result
		}
		defer z.Close()
		sort.Slice(z.File, func(i, j int) bool { return z.File[i].Name < z.File[j].Name })
		for _, f := range z.File {
			if format == "fbz" && strings.EqualFold(path.Ext(f.Name), ".fb2") {
				if b, e := readMember(f, 32<<20); e == nil {
					result = fb2Metadata(b)
				}
				break
			}
			if format == "cbz" && result.Cover == "" {
				if comicImage(f.Name) {
					if b, e := readMember(f, 8<<20); e == nil {
						result.Cover = thumbnail(b)
					}
				}
			}
			if format == "cbz" && strings.EqualFold(path.Base(f.Name), "ComicInfo.xml") {
				if b, e := readMember(f, 1<<20); e == nil {
					var m struct{ Title, Writer, Series string }
					if xml.Unmarshal(b, &m) == nil {
						result.Title = cleanBookText(m.Title)
						result.Author = cleanBookText(m.Writer)
						result.Series = cleanBookText(m.Series)
					}
				}
			}
		}
	}
	result.Format = format
	return result
}
