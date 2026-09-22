package doc

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
)

// Minimal WordprocessingML package, built with the standard library only
// (docs/DOCUMENTS.md §10: no AGPL deps for OOXML).

const docxContentTypes = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
<Default Extension="xml" ContentType="application/xml"/>
<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>
</Types>`

const docxRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>
</Relationships>`

// renderDOCX builds a .docx where every paragraph becomes a Word paragraph.
// Paragraphs starting with "# " are emitted as Heading1 for basic structure.
func renderDOCX(sk Skeleton) ([]byte, error) {
	var body strings.Builder
	body.WriteString("<w:body>")
	for _, p := range sk.Paragraphs {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		heading := strings.HasPrefix(p, "# ")
		text := strings.TrimPrefix(p, "# ")
		body.WriteString("<w:p>")
		if heading {
			body.WriteString(`<w:pPr><w:pStyle w:val="Heading1"/></w:pPr>`)
		}
		body.WriteString("<w:r><w:t xml:space=\"preserve\">")
		body.WriteString(xmlEscape(text))
		body.WriteString("</w:t></w:r></w:p>")
	}
	for _, t := range sk.Tables {
		widths := t.Widths
		if len(widths) == 0 && len(t.Headers) > 0 {
			widths = make([]int, len(t.Headers))
			for i := range widths {
				widths[i] = 4800 / len(t.Headers)
			}
		}
		body.WriteString(renderWordTable(t, widths))
	}
	body.WriteString("<w:sectPr/></w:body>")

	documentXML := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		body.String() + "</w:document>"

	return buildZipDoc(map[string]string{
		"[Content_Types].xml": docxContentTypes,
		"_rels/.rels":         docxRels,
		"word/document.xml":   documentXML,
	}), nil
}

// buildZipDoc packs raw entries into a zip archive (used by the OOXML
// renderers, which must produce a streamed package, not a file).
// renderWordTable emits a WordprocessingML table (header row bold + borders).
func renderWordTable(t SkeletonTable, widths []int) string {
	padWidths := func(n int, extra int) []int {
		out := make([]int, n)
		w := extra
		if n > 0 {
			w = extra / n
		}
		for i := range out {
			if i < len(widths) && widths[i] > 0 {
				out[i] = widths[i]
			} else {
				out[i] = w
			}
		}
		return out
	}
	var sb strings.Builder
	sb.WriteString("<w:tbl><w:tblPr><w:tblW w:w=\"0\" w:type=\"auto\"/><w:tblBorders>")
	for _, side := range []string{"top", "left", "bottom", "right", "insideH", "insideV"} {
		fmt.Fprintf(&sb, "<w:%s w:val=\"single\" w:sz=\"4\" w:space=\"0\" w:color=\"auto\"/>", side)
	}
	sb.WriteString("</w:tblBorders></w:tblPr>")

	cols := [][]string{t.Headers}
	cols = append(cols, t.Rows...)
	cellWidths := padWidths(maxCols(t.Headers, t.Rows), 4800)
	for i, row := range cols {
		sb.WriteString("<w:tr>")
		for j, cell := range row {
			sb.WriteString("<w:tc><w:tcPr><w:tcW w:w=\"")
			if j < len(cellWidths) {
				sb.WriteString(strconv.Itoa(cellWidths[j]))
			} else {
				sb.WriteString("2400")
			}
			sb.WriteString("\"/></w:tcPr><w:p>")
			if i == 0 {
				sb.WriteString(`<w:pPr><w:pStyle w:val="Heading1"/></w:pPr>`)
			}
			sb.WriteString("<w:r><w:t xml:space=\"preserve\">")
			sb.WriteString(xmlEscape(cell))
			sb.WriteString("</w:t></w:r></w:p></w:tc>")
		}
		sb.WriteString("</w:tr>")
	}
	sb.WriteString("</w:tbl>")
	return sb.String()
}

func maxCols(header []string, rows [][]string) int {
	n := len(header)
	for _, r := range rows {
		if len(r) > n {
			n = len(r)
		}
	}
	return n
}

func buildZipDoc(entries map[string]string) []byte {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	// [Content_Types].xml must be first for strict readers.
	order := make([]string, 0, len(entries))
	if _, ok := entries["[Content_Types].xml"]; ok {
		order = append(order, "[Content_Types].xml")
	}
	for name := range entries {
		if name != "[Content_Types].xml" {
			order = append(order, name)
		}
	}
	for _, name := range order {
		fw, _ := w.Create(name)
		_, _ = fw.Write([]byte(entries[name]))
	}
	_ = w.Close()
	return buf.Bytes()
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
