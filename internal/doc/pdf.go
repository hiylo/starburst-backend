package doc

import (
	"bytes"
	"fmt"
	"io"

	pdf "github.com/ledongthuc/pdf"
)

// parsePDF extracts the raw text of a PDF. It uses ledongthuc/pdf (MIT), a
// lightweight reader that pulls each page's text into a single buffer.
func parsePDF(data []byte) (string, error) {
	r := bytes.NewReader(data)
	reader, err := pdf.NewReader(r, int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("doc: open pdf: %w", err)
	}
	plain, err := reader.GetPlainText()
	if err != nil {
		return "", fmt.Errorf("doc: extract pdf text: %w", err)
	}
	out, err := io.ReadAll(plain)
	if err != nil {
		return "", fmt.Errorf("doc: read pdf text: %w", err)
	}
	return string(out), nil
}