package doc

import (
	"encoding/json"
	"fmt"
)

// Skeleton is the single intermediate JSON shape an LLM produces when asked to
// draft a document. It is type-agnostic: the `Type` field selects a renderer
// and the matching body section is used (Sheets for xlsx, Paragraphs/Tables for
// docx, Slides for pptx). Numbers in tables are supplied by the caller/
// retrieval, not invented by the model (see docs/DOCUMENTS.md §4.2).
type Skeleton struct {
	Type  string `json:"type"` // "xlsx" | "docx" | "pptx"
	Title string `json:"title"`

	// xlsx
	Sheets []SkeletonSheet `json:"sheets,omitempty"`

	// docx
	Paragraphs []string        `json:"paragraphs,omitempty"`
	Tables     []SkeletonTable `json:"tables,omitempty"`

	// pptx
	Slides []SkeletonSlide `json:"slides,omitempty"`
}

// SkeletonSheet is one worksheet: a name plus a 2-D grid of string cells, and
// optional charts drawn over a cell range.
type SkeletonSheet struct {
	Name   string          `json:"name"`
	Rows   [][]string      `json:"rows"`
	Charts []SkeletonChart `json:"charts,omitempty"`
}

// SkeletonChart is a chart anchored to a 1-based cell range (including header
// row + value columns) of the sheet. Type is one of bar|line|pie.
type SkeletonChart struct {
	Type  string `json:"type"`
	Title string `json:"title,omitempty"`
	// 1-based cell range covering the chart data.
	StartRow int `json:"startRow"`
	StartCol int `json:"startCol"`
	EndRow   int `json:"endRow"`
	EndCol   int `json:"endCol"`
}

// SkeletonTable is a header + rows grid, used by docx bodies and pptx table slides.
type SkeletonTable struct {
	Headers []string   `json:"headers,omitempty"`
	Rows    [][]string `json:"rows,omitempty"`
	// Widths is the optional per-column width in twentieths of a point (docx).
	Widths []int `json:"widths,omitempty"`
}

// SkeletonSlide is one presentation slide. Layout selects the renderer shape:
// "" / "bullets" (title + bullets), "cover" (title only), "table" (title +
// table), "two-col" (title + left/right bullets).
type SkeletonSlide struct {
	Title   string         `json:"title"`
	Layout  string         `json:"layout,omitempty"`
	Bullets []string       `json:"bullets,omitempty"`
	Table   *SkeletonTable `json:"table,omitempty"`
	Left    []string       `json:"left,omitempty"`
	Right   []string       `json:"right,omitempty"`
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
