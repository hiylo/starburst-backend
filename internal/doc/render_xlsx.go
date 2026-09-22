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
	}
	if _, err := f.WriteTo(&buf); err != nil {
		return nil, fmt.Errorf("doc: write xlsx: %w", err)
	}
	return buf.Bytes(), nil
}