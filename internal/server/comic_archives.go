// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"image"
	"io"
	"os"
	"path"
	"strings"
	"unicode"

	"github.com/bodgit/sevenzip"
	"github.com/nwaples/rardecode/v2"
	"github.com/ulikunitz/xz/lzma"
)

const comicDictionaryLimit = 64 << 20

// Override codecs that otherwise accept dictionaries of several gigabytes.
// Encryption is deliberately unsupported, including archives with an empty password.
func init() {
	sevenzip.RegisterDecompressor([]byte{3, 1, 1}, func(p []byte, size uint64, r []io.ReadCloser) (io.ReadCloser, error) {
		if len(p) != 5 || len(r) != 1 {
			return nil, ErrInvalid
		}
		header := append([]byte(nil), p...)
		header = binary.LittleEndian.AppendUint64(header, size)
		decoder, err := (lzma.ReaderConfig{DictCap: comicDictionaryLimit}).NewReader(io.MultiReader(bytes.NewReader(header), r[0]))
		if err != nil {
			return nil, err
		}
		return &comicDecoder{Reader: decoder, Closer: r[0]}, nil
	})
	sevenzip.RegisterDecompressor([]byte{0x21}, func(p []byte, _ uint64, r []io.ReadCloser) (io.ReadCloser, error) {
		if len(p) != 1 || p[0] > 40 || len(r) != 1 {
			return nil, ErrInvalid
		}
		capacity := uint64(2|int(p[0])&1) << (p[0]/2 + 11)
		if capacity > comicDictionaryLimit {
			return nil, ErrInvalid
		}
		decoder, err := (lzma.Reader2Config{DictCap: int(capacity)}).NewReader2(r[0])
		if err != nil {
			return nil, err
		}
		return &comicDecoder{Reader: decoder, Closer: r[0]}, nil
	})
	// These uncommon codecs lack a configurable allocation budget in sevenzip.
	for _, method := range [][]byte{{6, 0xf1, 7, 1}, {3, 4, 1}, {4, 0xf7, 0x11, 1}, {4, 0xf7, 0x11, 2}, {4, 0xf7, 0x11, 4}} {
		sevenzip.RegisterDecompressor(method, func([]byte, uint64, []io.ReadCloser) (io.ReadCloser, error) { return nil, ErrInvalid })
	}
}

type comicDecoder struct {
	io.Reader
	io.Closer
}

type comicInspection struct {
	total     uint64
	budget    uint64
	seen      map[string]bool
	images    int
	coverName string
	infoName  string
	metadata  bookMetadata
}

func (c *comicInspection) member(name string, size uint64, mode os.FileMode, encrypted bool) error {
	clean := strings.TrimSuffix(name, "/")
	if len(c.seen) >= 10000 || len(name) > 4096 || clean == "" || strings.ContainsAny(clean, "\\:") || strings.IndexFunc(clean, unicode.IsControl) >= 0 || strings.HasPrefix(clean, "/") || path.Clean(clean) != clean || clean == ".." || strings.HasPrefix(clean, "../") || c.seen[clean] || mode&os.ModeSymlink != 0 || (!mode.IsRegular() && !mode.IsDir()) || encrypted || size > c.budget-c.total {
		return ErrInvalid
	}
	c.seen[clean] = true
	c.total += size
	if comicImage(name) && size > 32<<20 {
		return ErrInvalid
	}
	if strings.EqualFold(path.Base(name), "ComicInfo.xml") && size > 1<<20 {
		return ErrInvalid
	}
	return nil
}

func (c *comicInspection) consume(name string, size uint64, r io.Reader) error {
	isImage := comicImage(name)
	isInfo := strings.EqualFold(path.Base(name), "ComicInfo.xml")
	if !isImage && !isInfo {
		n, err := io.Copy(io.Discard, io.LimitReader(r, int64(size)+1))
		if err != nil || uint64(n) != size {
			return ErrInvalid
		}
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(r, int64(size)+1))
	if err != nil || uint64(len(b)) != size {
		return ErrInvalid
	}
	if isImage {
		if strings.EqualFold(path.Ext(name), ".avif") {
			if !validAVIF(b) {
				return ErrInvalid
			}
		} else {
			cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
			if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 100_000_000 {
				return ErrInvalid
			}
		}
		c.images++
		if len(b) <= 8<<20 && (c.coverName == "" || name < c.coverName) {
			if cover := thumbnail(b); cover != "" {
				c.coverName = name
				c.metadata.Cover = cover
			}
		}
	}
	if isInfo && (c.infoName == "" || name < c.infoName) {
		var m struct{ Title, Writer, Series string }
		if xml.Unmarshal(b, &m) == nil {
			c.infoName = name
			c.metadata.Title = cleanBookText(m.Title)
			c.metadata.Author = cleanBookText(m.Writer)
			c.metadata.Series = cleanBookText(m.Series)
		}
	}
	return nil
}

// Readers receive only the uploaded file, so multipart archives cannot read
// neighbouring server files. No archive member is ever written to disk.
func inspectComicArchive(filename, format string) (bookMetadata, error) {
	f, err := os.Open(filename)
	if err != nil {
		return bookMetadata{}, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() < 0 || stat.Size() > MaxStoredBookBytes {
		return bookMetadata{}, ErrInvalid
	}
	c := comicInspection{budget: uint64(stat.Size()) + (512 << 20), seen: map[string]bool{}, metadata: bookMetadata{Format: format}}
	if format == "cbr" {
		reader, err := rardecode.NewReader(f, rardecode.MaxDictionarySize(comicDictionaryLimit))
		if err != nil {
			return bookMetadata{}, ErrInvalid
		}
		for {
			h, err := reader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return bookMetadata{}, ErrInvalid
			}
			if h.UnKnownSize || h.UnPackedSize < 0 || h.LinkType != rardecode.LinkTypeNone || c.member(h.Name, uint64(h.UnPackedSize), h.Mode(), h.Encrypted || h.HeaderEncrypted) != nil {
				return bookMetadata{}, ErrInvalid
			}
			if err = c.consume(h.Name, uint64(h.UnPackedSize), reader); err != nil {
				return bookMetadata{}, err
			}
		}
	} else if format == "cb7" {
		// Bound the encoded header before invoking the archive parser.
		var head [32]byte
		if _, err = f.ReadAt(head[:], 0); err != nil {
			return bookMetadata{}, ErrInvalid
		}
		offset, size := binary.LittleEndian.Uint64(head[12:20]), binary.LittleEndian.Uint64(head[20:28])
		if size > 8<<20 || offset > uint64(stat.Size()) || size > uint64(stat.Size())-offset || offset+size > uint64(stat.Size())-32 {
			return bookMetadata{}, ErrInvalid
		}
		reader, err := sevenzip.NewReader(f, stat.Size())
		if err != nil {
			return bookMetadata{}, ErrInvalid
		}
		for _, h := range reader.File {
			if err = c.member(h.Name, h.UncompressedSize, h.Mode(), false); err != nil {
				return bookMetadata{}, err
			}
		}
		// Preserve archive order to avoid repeatedly inflating preceding solid members.
		for _, h := range reader.File {
			r, err := h.Open()
			if err != nil {
				return bookMetadata{}, ErrInvalid
			}
			err = c.consume(h.Name, h.UncompressedSize, r)
			closeErr := r.Close()
			if err != nil || closeErr != nil {
				return bookMetadata{}, ErrInvalid
			}
		}
	} else {
		return bookMetadata{}, ErrInvalid
	}
	if c.images == 0 {
		return bookMetadata{}, ErrInvalid
	}
	return c.metadata, nil
}
