package doc

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// OOXML parsers: .docx / .pptx are ZIP archives of XML parts. We parse them
// with the Go standard library only, so the repo does not depend on AGPL
// libraries for Word/PPT handling (docs/DOCUMENTS.md §10).

// docxText is the subset of the WordprocessingML tree we care about.
type docxText struct {
	Body struct {
		Paragraphs []struct {
			Runs []struct {
				Text string `xml:"t"`
			} `xml:"r"`
		} `xml:"p"`
	} `xml:"body"`
}

// parseDOCX extracts paragraph text from word/document.xml. Each paragraph
// becomes one Markdown paragraph; runs inside a paragraph are concatenated.
func parseDOCX(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("doc: open docx zip: %w", err)
	}
	part, err := findZip(zr, "word/document.xml")
	if err != nil {
		return "", err
	}
	body, err := io.ReadAll(part)
	part.Close()
	if err != nil {
		return "", fmt.Errorf("doc: read document.xml: %w", err)
	}
	var d docxText
	if err := xml.Unmarshal(body, &d); err != nil {
		return "", fmt.Errorf("doc: parse document.xml: %w", err)
	}
	var sb strings.Builder
	for _, p := range d.Body.Paragraphs {
		var line strings.Builder
		for _, r := range p.Runs {
			line.WriteString(r.Text)
		}
		trimmed := strings.TrimSpace(line.String())
		if trimmed != "" {
			sb.WriteString(trimmed)
			sb.WriteString("\n\n")
		}
	}
	if sb.Len() == 0 {
		return "", fmt.Errorf("doc: docx has no text")
	}
	return strings.TrimSpace(sb.String()), nil
}

// parsePPTX extracts text from every slide (ppt/slides/slideN.xml, sorted
// numerically). Each slide becomes a `## 第 N 页` section with its text lines.
func parsePPTX(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("doc: open pptx zip: %w", err)
	}
	var slides []string
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "ppt/slides/slide") && strings.HasSuffix(f.Name, ".xml") {
			slides = append(slides, f.Name)
		}
	}
	if len(slides) == 0 {
		return "", fmt.Errorf("doc: pptx has no slides")
	}
	// slide10.xml must sort after slide9.xml.
	sort.Slice(slides, func(i, j int) bool { return slideIndex(slides[i]) < slideIndex(slides[j]) })

	var sb strings.Builder
	for i, name := range slides {
		rc, err := zr.Open(name)
		if err != nil {
			continue
		}
		raw, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			continue
		}
		text := extractSlideText(raw)
		sb.WriteString("## 第 ")
		sb.WriteString(strconv.Itoa(i + 1))
		sb.WriteString(" 页\n\n")
		sb.WriteString(text)
		sb.WriteString("\n\n")
	}
	if sb.Len() == 0 {
		return "", fmt.Errorf("doc: pptx has no readable text")
	}
	return strings.TrimSpace(sb.String()), nil
}

// extractSlideText walks the slide XML and groups the text of every <a:t>
// element into lines split at <a:p> paragraph boundaries.
func extractSlideText(raw []byte) string {
	var lines []string
	var cur strings.Builder
	dec := xml.NewDecoder(bytes.NewReader(raw))
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		el, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch {
		case el.Name.Local == "p" && el.Name.Space == "a":
			if strings.TrimSpace(cur.String()) != "" {
				lines = append(lines, strings.TrimSpace(cur.String()))
			}
			cur.Reset()
		case el.Name.Local == "t":
			var t string
			if err := dec.DecodeElement(&t, &el); err == nil {
				cur.WriteString(t)
			}
		}
	}
	if strings.TrimSpace(cur.String()) != "" {
		lines = append(lines, strings.TrimSpace(cur.String()))
	}
	return strings.Join(lines, "\n")
}

// findZip returns the reader for a single zip entry by exact name.
func findZip(zr *zip.Reader, name string) (io.ReadCloser, error) {
	for _, f := range zr.File {
		if f.Name == name {
			return f.Open()
		}
	}
	return nil, fmt.Errorf("doc: missing part %s", name)
}

// slideIndex parses the trailing number from a slide file name.
func slideIndex(name string) int {
	base := name
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimPrefix(base, "slide")
	base = strings.TrimSuffix(base, ".xml")
	n := 0
	for _, c := range base {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}
