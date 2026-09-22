package doc

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
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
