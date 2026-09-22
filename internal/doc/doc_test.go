package doc

import (
	"archive/zip"
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

// buildZip packs the given name→content entries into a zip archive.
func buildZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, content := range entries {
		fw, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSupported(t *testing.T) {
	cases := []struct {
		name, mime string
		want       bool
	}{
		{"a.pdf", "application/pdf", true},
		{"a.xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", true},
		{"a.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", true},
		{"a.pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation", true},
		{"a.csv", "text/csv", true},
		{"a.PDF", "", true},
		{"a.XLSX", "", true},
		{"a.bin", "application/octet-stream", false},
		{"a.jpg", "image/jpeg", false},
	}
	for _, tc := range cases {
		if got := Supported(tc.name, tc.mime); got != tc.want {
			t.Errorf("Supported(%q, %q) = %v, want %v", tc.name, tc.mime, got, tc.want)
		}
	}
}

func TestParseUnsupported(t *testing.T) {
	if _, err := Parse("x.bin", "application/octet-stream", []byte{1, 2, 3}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	if _, err := Parse("empty.docx", "", nil); err == nil {
		t.Fatal("empty input should error")
	}
}

func TestParseCSV(t *testing.T) {
	md, err := Parse("sheet.csv", "text/csv", []byte("型号,价格\nX,99\nY,199\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "| 型号 | 价格 |") || !strings.Contains(md, "| X | 99 |") {
		t.Fatalf("csv markdown = %q", md)
	}
}

func TestParseXLSX(t *testing.T) {
	f := excelize.NewFile()
	defer f.Close()
	if err := f.SetCellValue("Sheet1", "A1", "产品"); err != nil {
		t.Fatal(err)
	}
	if err := f.SetCellValue("Sheet1", "B1", "数量"); err != nil {
		t.Fatal(err)
	}
	if err := f.SetCellValue("Sheet1", "A2", "安全库存"); err != nil {
		t.Fatal(err)
	}
	if err := f.SetCellValue("Sheet1", "B2", 12000); err != nil {
		t.Fatal(err)
	}
	buf, err := f.WriteToBuffer()
	if err != nil {
		t.Fatal(err)
	}

	md, err := Parse("库存.xlsx", "", buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "## Sheet1") {
		t.Fatalf("missing sheet heading: %q", md)
	}
	if !strings.Contains(md, "| 产品 | 数量 |") || !strings.Contains(md, "| 安全库存 | 12000 |") {
		t.Fatalf("xlsx markdown = %q", md)
	}
}

func TestParseDOCX(t *testing.T) {
	const docXML = `<?xml version="1.0"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
 <w:body>
  <w:p><w:r><w:t>第一段标题</w:t></w:r></w:p>
  <w:p><w:r><w:t>第二段正文内容。</w:t></w:r><w:r><w:t> 追加内容</w:t></w:r></w:p>
 </w:body>
</w:document>`
	md, err := Parse("notes.docx", "", buildZip(t, map[string]string{"word/document.xml": docXML}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "第一段标题") || !strings.Contains(md, "第二段正文内容。 追加内容") {
		t.Fatalf("docx markdown = %q", md)
	}
}

func TestParsePPTX(t *testing.T) {
	slide := func(id string) string {
		return map[string]string{
			"1": `<?xml version="1.0"?>
<p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main">
 <p:cSld><p:spTree><p:sp><p:txBody>
  <a:p><a:r><a:t>产品发布会</a:t></a:r></a:p>
  <a:p><a:r><a:t>副标题</a:t></a:r></a:p>
 </p:txBody></p:sp></p:spTree></p:cSld></p:sld>`,
			"2": `<?xml version="1.0"?>
<p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main">
 <p:cSld><p:spTree><p:sp><p:txBody>
  <a:p><a:r><a:t>技术亮点</a:t></a:r></a:p>
 </p:txBody></p:sp></p:spTree></p:cSld></p:sld>`,
		}[id]
	}
	md, err := Parse("deck.pptx", "", buildZip(t, map[string]string{
		"ppt/slides/slide1.xml": slide("1"),
		"ppt/slides/slide2.xml": slide("2"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "## 第 1 页") || !strings.Contains(md, "## 第 2 页") {
		t.Fatalf("missing page headings: %q", md)
	}
	if !strings.Contains(md, "产品发布会") || !strings.Contains(md, "技术亮点") {
		t.Fatalf("pptx markdown = %q", md)
	}
	// slide2 内容必须排在 slide1 之后。
	if strings.Index(md, "产品发布会") > strings.Index(md, "技术亮点") {
		t.Fatalf("slide order wrong: %q", md)
	}
}

func TestSlideIndex(t *testing.T) {
	cases := map[string]int{
		"ppt/slides/slide1.xml":  1,
		"ppt/slides/slide12.xml": 12,
		"ppt/slides/slide9.xml":  9,
		"ppt/slides/slide10.xml": 10,
	}
	for name, want := range cases {
		if got := slideIndex(name); got != want {
			t.Errorf("slideIndex(%q) = %d, want %d", name, got, want)
		}
	}
}
