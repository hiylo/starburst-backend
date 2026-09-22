package doc

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strings"

	"github.com/xuri/excelize/v2"
)

// parseXLSX renders every sheet as a Markdown table. Excel text is chunked
// later by the KB pipeline; keeping the sheet/row structure as a Markdown
// table preserves the column layout for retrieval.
func parseXLSX(data []byte) (string, error) {
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("doc: open xlsx: %w", err)
	}
	defer f.Close()

	var sb strings.Builder
	for _, sheet := range f.GetSheetList() {
		rows, err := f.GetRows(sheet)
		if err != nil {
			continue
		}
		writeSheetMarkdown(&sb, sheet, rows)
	}
	if sb.Len() == 0 {
		return "", fmt.Errorf("doc: xlsx has no readable sheets")
	}
	return strings.TrimSpace(sb.String()), nil
}

// parseCSV renders CSV content as a single Markdown table (sheet name falls
// back to the file base name).
func parseCSV(data []byte) (string, error) {
	reader := csv.NewReader(bytes.NewReader(data))
	rows, err := reader.ReadAll()
	if err != nil {
		return "", fmt.Errorf("doc: parse csv: %w", err)
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("doc: csv is empty")
	}
	var sb strings.Builder
	writeSheetMarkdown(&sb, "Sheet1", rows)
	return strings.TrimSpace(sb.String()), nil
}

// writeSheetMarkdown writes `## <sheet>` followed by a Markdown table of rows.
func writeSheetMarkdown(sb *strings.Builder, sheet string, rows [][]string) {
	if len(rows) == 0 {
		return
	}
	width := 0
	for _, row := range rows {
		if len(row) > width {
			width = len(row)
		}
	}
	if width == 0 {
		return
	}
	sb.WriteString("## ")
	sb.WriteString(sheet)
	sb.WriteString("\n\n")
	for i, row := range rows {
		cells := make([]string, width)
		for j := 0; j < width; j++ {
			if j < len(row) {
				cells[j] = markdownCell(row[j])
			} else {
				cells[j] = ""
			}
		}
		sb.WriteString("| ")
		sb.WriteString(strings.Join(cells, " | "))
		sb.WriteString(" |\n")
		if i == 0 {
			sb.WriteString("| ")
			sb.WriteString(strings.TrimSpace(strings.Repeat("--- | ", width)))
			sb.WriteString("\n")
		}
	}
	sb.WriteString("\n")
}

// markdownCell escapes pipe characters that would otherwise break the table.
func markdownCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", "<br>")
	return s
}
