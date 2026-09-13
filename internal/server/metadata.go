// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/url"
	"path"
	"strconv"
	"strings"
)

type bookMetadata struct {
	Title  string   `json:"title"`
	Author string   `json:"author"`
	Series string   `json:"series"`
	Volume *float64 `json:"volume"`
	Cover  string   `json:"cover"`
}

// Read only bounded archive members. Never execute publisher content or fetch external URLs.
func epubMetadata(filename string) bookMetadata {
	result := bookMetadata{}
	z, err := zip.OpenReader(filename)
	if err != nil {
		return result
	}
	defer z.Close()
	read := func(name string, limit int64) ([]byte, error) {
		for _, f := range z.File {
			if f.Name == name {
				if f.UncompressedSize64 > uint64(limit) {
					return nil, errors.New("member too large")
				}
				r, e := f.Open()
				if e != nil {
					return nil, e
				}
				defer r.Close()
				b, e := io.ReadAll(io.LimitReader(r, limit+1))
				if int64(len(b)) > limit {
					return nil, errors.New("member too large")
				}
				return b, e
			}
		}
		return nil, errors.New("member not found")
	}
	var container struct {
		Roots []struct {
			Path string `xml:"full-path,attr"`
		} `xml:"rootfiles>rootfile"`
	}
	b, err := read("META-INF/container.xml", 1<<20)
	if err != nil || xml.Unmarshal(b, &container) != nil || len(container.Roots) == 0 {
		return result
	}
	opf := container.Roots[0].Path
	var pkg struct {
		Metadata struct {
			Title    string   `xml:"title"`
			Creators []string `xml:"creator"`
			Meta     []struct {
				Name     string `xml:"name,attr"`
				Content  string `xml:"content,attr"`
				Property string `xml:"property,attr"`
				Value    string `xml:",chardata"`
			} `xml:"meta"`
		} `xml:"metadata"`
		Items []struct {
			ID         string `xml:"id,attr"`
			Href       string `xml:"href,attr"`
			Properties string `xml:"properties,attr"`
		} `xml:"manifest>item"`
	}
	b, err = read(opf, 2<<20)
	if err != nil || xml.Unmarshal(b, &pkg) != nil {
		return result
	}
	clean := func(v string) string {
		v = strings.Join(strings.Fields(v), " ")
		r := []rune(v)
		if len(r) > 1000 {
			r = r[:1000]
		}
		return string(r)
	}
	result.Title = clean(pkg.Metadata.Title)
	result.Author = clean(strings.Join(pkg.Metadata.Creators, ", "))
	coverID := ""
	for _, m := range pkg.Metadata.Meta {
		switch m.Name {
		case "cover":
			coverID = m.Content
		case "calibre:series":
			result.Series = clean(m.Content)
		case "calibre:series_index":
			if n, e := strconv.ParseFloat(m.Content, 64); e == nil && n >= 0 && n < 1e6 {
				result.Volume = &n
			}
		}
		if m.Property == "belongs-to-collection" && result.Series == "" {
			result.Series = clean(m.Value)
		}
		if m.Property == "group-position" && result.Volume == nil {
			if n, e := strconv.ParseFloat(m.Value, 64); e == nil && n >= 0 && n < 1e6 {
				result.Volume = &n
			}
		}
	}
	for _, item := range pkg.Items {
		if item.ID != coverID && !strings.Contains(" "+item.Properties+" ", " cover-image ") {
			continue
		}
		u, e := url.Parse(item.Href)
		if e != nil || u.IsAbs() || u.Host != "" || strings.HasPrefix(u.Path, "/") {
			continue
		}
		name := path.Clean(path.Join(path.Dir(opf), u.Path))
		if strings.HasPrefix(name, "../") {
			continue
		}
		b, e = read(name, 8<<20)
		if e != nil {
			continue
		}
		config, _, e := image.DecodeConfig(bytes.NewReader(b))
		if e != nil || config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 20_000_000 {
			continue
		}
		src, _, e := image.Decode(bytes.NewReader(b))
		if e != nil {
			continue
		}
		w, h := config.Width, config.Height
		if w > 320 {
			h = h * 320 / w
			w = 320
		}
		if h > 480 {
			w = w * 480 / h
			h = 480
		}
		w = max(1, w)
		h = max(1, h)
		thumb := image.NewRGBA(image.Rect(0, 0, w, h))
		bounds := src.Bounds()
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				thumb.Set(x, y, src.At(bounds.Min.X+x*config.Width/w, bounds.Min.Y+y*config.Height/h))
			}
		}
		var out bytes.Buffer
		if jpeg.Encode(&out, thumb, &jpeg.Options{Quality: 85}) == nil {
			result.Cover = "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(out.Bytes())
		}
		break
	}
	return result
}
