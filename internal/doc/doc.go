// Package doc provides document parsing and (future) generation for the
// knowledge-base subsystem (docs/DOCUMENTS.md). Parsers turn PDF / Excel /
// Word / PPT files into plain text or Markdown so the KB ingest pipeline can
// chunk and embed them uniformly.
//
// License posture: only permissive (MIT / Apache-2.0) dependencies are used.
// .docx / .pptx parse via the Go standard library (zip + xml) to avoid bringing
// AGPL libs (e.g. unioffice) into a MIT repo.
package doc

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrUnsupported is returned when the document type cannot be parsed yet
// (e.g. a binary format outside the supported set).
var ErrUnsupported = errors.New("doc: unsupported document type")

// Supported reports whether the document (by name/mime) has a parser.
func Supported(name, mime string) bool {
	ext := extOf(name)
	m := strings.ToLower(mime)
	switch {
	case ext == ".pdf" || m == "application/pdf":
		return true
	case ext == ".xlsx" || ext == ".xls" || ext == ".csv" ||
		m == "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" ||
		m == "text/csv":
		return true
	case ext == ".docx" || m == "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		ext == ".pptx" || m == "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return true
	}
	return false
}

// Parse converts a document into Markdown text. name is used to pick the
// parser by extension; mime is a fallback when the extension is ambiguous.
func Parse(name, mime string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("doc: empty document")
	}
	ext := extOf(name)
	switch {
	case ext == ".pdf" || strings.EqualFold(mime, "application/pdf"):
		return parsePDF(data)
	case ext == ".xlsx" || ext == ".xls":
		return parseXLSX(data)
	case ext == ".csv" || strings.EqualFold(mime, "text/csv"):
		return parseCSV(data)
	case ext == ".docx":
		return parseDOCX(data)
	case ext == ".pptx":
		return parsePPTX(data)
	}
	return "", fmt.Errorf("%w: %s (%s)", ErrUnsupported, name, mime)
}

func extOf(name string) string {
	return strings.ToLower(filepath.Ext(name))
}
