package doc

import (
	"bytes"
	"fmt"

	"github.com/xuri/excelize/v2"
)

// renderXLSX builds a workbook: each sheet in the skeleton becomes a worksheet
// with its grid written A1-forward. The first sheet is renamed if the skeleton
// carried a name.
func renderXLSX(sk Skeleton) ([]byte, error) {
	var buf bytes.Buffer
	f := excelize.NewFile()
	defer f.Close()

	if len(sk.Sheets) == 0 {
		return nil, fmt.Errorf("doc: xlsx skeleton has no sheets")
	}
	for i, sheet := range sk.Sheets {
		if sheet.Name == "" {
			sheet.Name = fmt.Sprintf("Sheet%d", i+1)
		}
		if i == 0 {
			f.SetSheetName("Sheet1", sheet.Name)
		} else {
			idx, err := f.NewSheet(sheet.Name)
			if err != nil {
				return nil, fmt.Errorf("doc: create sheet %q: %w", sheet.Name, err)
			}
			_ = idx
		}
		for r, row := range sheet.Rows {
			for c, cell := range row {
				cellName, err := excelize.CoordinatesToCellName(c+1, r+1)
				if err != nil {
					continue
				}
				if err := f.SetCellValue(sheet.Name, cellName, cell); err != nil {
					return nil, fmt.Errorf("doc: write cell %s: %w", cellName, err)
				}
			}
		}
		for _, ch := range sheet.Charts {
			if err := addChart(f, sheet.Name, ch); err != nil {
				return nil, err
			}
		}
	}
	if _, err := f.WriteTo(&buf); err != nil {
		return nil, fmt.Errorf("doc: write xlsx: %w", err)
	}
	return buf.Bytes(), nil
}

// addChart adds one chart over the sheet's cell range via excelize.
func addChart(f *excelize.File, sheet string, ch SkeletonChart) error {
	chartType, ok := chartTypeFor(ch.Type)
	if !ok {
		return fmt.Errorf("doc: unsupported chart type %q", ch.Type)
	}
	if ch.StartRow < 1 || ch.StartCol < 1 || ch.EndRow < ch.StartRow || ch.EndCol < ch.StartCol {
		return fmt.Errorf("doc: invalid chart range (%d,%d)-(%d,%d)", ch.StartRow, ch.StartCol, ch.EndRow, ch.EndCol)
	}
	// 首列 = 分类，末列 = 数值。
	categories := cellRange(sheet, ch.StartRow, ch.StartCol, ch.EndRow, ch.StartCol)
	values := cellRange(sheet, ch.StartRow, ch.EndCol, ch.EndRow, ch.EndCol)
	if categories == "" || values == "" {
		return fmt.Errorf("doc: chart range has no cells")
	}
	chart := &excelize.Chart{
		Type: chartType,
		Series: []excelize.ChartSeries{{
			Name:       fmt.Sprintf("%s!$%s$%d", sheet, colLetter(ch.StartCol), ch.StartRow),
			Categories: categories,
			Values:     values,
		}},
		Title: []excelize.RichTextRun{{Text: ch.Title}},
	}
	anchor := cellRef(ch.EndRow+2, ch.StartCol)
	if err := f.AddChart(sheet, anchor, chart); err != nil {
		return fmt.Errorf("doc: add chart: %w", err)
	}
	return nil
}

// chartTypeFor maps a skeleton chart-type name to the excelize chart type.
func chartTypeFor(name string) (excelize.ChartType, bool) {
	switch name {
	case "bar", "col":
		return excelize.Col, true // excelize 柱状图为 col
	case "line":
		return excelize.Line, true
	case "pie":
		return excelize.Pie, true
	case "area":
		return excelize.Area, true
	case "doughnut":
		return excelize.Doughnut, true
	case "radar":
		return excelize.Radar, true
	case "scatter":
		return excelize.Scatter, true
	}
	return 0, false
}

// cellRange returns an excelize absolute range string for a cell block, empty
// when the range is degenerate.
func cellRange(sheet string, startRow, startCol, endRow, endCol int) string {
	if endRow < startRow || endCol < startCol {
		return ""
	}
	return fmt.Sprintf("%s!$%s$%d:$%s$%d", sheet, colLetter(startCol), startRow, colLetter(endCol), endRow)
}

// cellRef returns an excelize absolute cell reference.
func cellRef(row, col int) string {
	return fmt.Sprintf("%s%d", colLetter(col), row)
}

// colLetter converts a 1-based column index to its letter (A, B, …, Z, AA…).
func colLetter(col int) string {
	if col < 1 {
		col = 1
	}
	s := ""
	for col > 0 {
		col--
		s = string(rune('A'+col%26)) + s
		col /= 26
	}
	return s
}
