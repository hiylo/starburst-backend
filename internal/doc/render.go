package doc

import (
	"encoding/json"
	"fmt"
)

// Skeleton is the single intermediate JSON shape an LLM produces when asked to
// draft a document. It is type-agnostic: the `Type` field selects a renderer
// and the matching body section is used (Sheets for xlsx, Paragraphs for docx,
// Slides for pptx). Numbers in tables are supplied by the caller/retrieval, not
// invented by the model (see docs/DOCUMENTS.md §4.2).
type Skeleton struct {
	Type  string `json:"type"` // "xlsx" | "docx" | "pptx"
	Title string `json:"title"`

	// xlsx
	Sheets []SkeletonSheet `json:"sheets,omitempty"`

	// docx
	Paragraphs []string `json:"paragraphs,omitempty"`

	// pptx
	Slides []SkeletonSlide `json:"slides,omitempty"`
}

// SkeletonSheet is one worksheet: a name plus a 2-D grid of string cells.
type SkeletonSheet struct {
	Name string     `json:"name"`
	Rows [][]string `json:"rows"`
}

// SkeletonSlide is one presentation slide: a title plus bullet points.
type SkeletonSlide struct {
	Title   string   `json:"title"`
	Bullets []string `json:"bullets"`
}

// ValidType reports whether t names a renderable document type.
func ValidType(t string) bool {
	switch t {
	case "xlsx", "docx", "pptx":
		return true
	}
	return false
}

// RenderFromSkeletonJSON unmarshals a skeleton and renders the matching file
// bytes. It is the deterministic half of generation: given valid skeleton JSON
// it never fails and never calls out to a model.
func RenderFromSkeletonJSON(skeletonJSON []byte) (docType string, data []byte, err error) {
	var sk Skeleton
	if err := json.Unmarshal(skeletonJSON, &sk); err != nil {
		return "", nil, fmt.Errorf("doc: bad skeleton: %w", err)
	}
	if !ValidType(sk.Type) {
		return "", nil, fmt.Errorf("doc: unsupported skeleton type %q", sk.Type)
	}
	switch sk.Type {
	case "xlsx":
		data, err = renderXLSX(sk)
	case "docx":
		data, err = renderDOCX(sk)
	case "pptx":
		data, err = renderPPTX(sk)
	}
	if err != nil {
		return "", nil, err
	}
	return sk.Type, data, nil
}

// Extension returns the file extension for a document type.
func Extension(docType string) string {
	switch docType {
	case "xlsx":
		return ".xlsx"
	case "docx":
		return ".docx"
	case "pptx":
		return ".pptx"
	}
	return ".bin"
}
