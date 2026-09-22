package doc

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"

	"github.com/xuri/excelize/v2"
	"strings"
	"testing"
)

func roundTripParse(t *testing.T, sk Skeleton) string {
	t.Helper()
	raw, err := json.Marshal(sk)
	if err != nil {
		t.Fatal(err)
	}
	docType, data, err := RenderFromSkeletonJSON(raw)
	if err != nil {
		t.Fatalf("render %s: %v", sk.Type, err)
	}
	if docType != sk.Type {
		t.Fatalf("rendered type = %s, want %s", docType, sk.Type)
	}
	md, err := Parse("doc"+Extension(docType), "", data)
	if err != nil {
		t.Fatalf("re-parse %s: %v", sk.Type, err)
	}
	return md
}

func TestRenderXLSXRoundTrip(t *testing.T) {
	sk := Skeleton{
		Type:  "xlsx",
		Title: "报价单",
		Sheets: []SkeletonSheet{{
			Name: "Sheet1",
			Rows: [][]string{{"型号", "价格"}, {"X", "99"}, {"Y", "199"}},
		}},
	}
	md := roundTripParse(t, sk)
	if !strings.Contains(md, "| 型号 | 价格 |") || !strings.Contains(md, "| Y | 199 |") {
		t.Fatalf("xlsx round-trip = %q", md)
	}
}

func TestRenderDOCXRoundTrip(t *testing.T) {
	sk := Skeleton{
		Type:       "docx",
		Title:      "会议纪要",
		Paragraphs: []string{"# 标题段", "正文段落内容。", "另一个 # 不是标题前缀的段落其实"},
	}
	md := roundTripParse(t, sk)
	if !strings.Contains(md, "标题段") || !strings.Contains(md, "正文段落内容。") {
		t.Fatalf("docx round-trip = %q", md)
	}
}

func TestRenderPPTXRoundTrip(t *testing.T) {
	sk := Skeleton{
		Type:  "pptx",
		Title: "发布会",
		Slides: []SkeletonSlide{
			{Title: "封面", Bullets: []string{"产品发布会", "子标题"}},
			{Title: "技术亮点", Bullets: []string{"低延迟", "可扩展"}},
		},
	}
	md := roundTripParse(t, sk)
	if !strings.Contains(md, "产品发布会") || !strings.Contains(md, "技术亮点") ||
		!strings.Contains(md, "低延迟") || !strings.Contains(md, "## 第 2 页") {
		t.Fatalf("pptx round-trip = %q", md)
	}
	if strings.Index(md, "产品发布会") > strings.Index(md, "技术亮点") {
		t.Fatalf("slide order wrong: %q", md)
	}
}

func TestRenderSkeletonErrors(t *testing.T) {
	if _, _, err := RenderFromSkeletonJSON([]byte("not json")); err == nil {
		t.Fatal("bad JSON should error")
	}
	if _, _, err := RenderFromSkeletonJSON([]byte(`{"type":"pdf"}`)); err == nil {
		t.Fatal("unsupported type should error")
	}
	if _, _, err := RenderFromSkeletonJSON([]byte(`{"type":"pptx"}`)); err == nil {
		t.Fatal("pptx without slides should error")
	}
	if _, _, err := RenderFromSkeletonJSON([]byte(`{"type":"xlsx"}`)); err == nil {
		t.Fatal("xlsx without sheets should error")
	}
}

func TestExtension(t *testing.T) {
	cases := map[string]string{"xlsx": ".xlsx", "docx": ".docx", "pptx": ".pptx", "nope": ".bin"}
	for in, want := range cases {
		if got := Extension(in); got != want {
			t.Errorf("Extension(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRenderXLSXChart verifies a chart is embedded in the rendered workbook.
func TestRenderXLSXChart(t *testing.T) {
	sk := Skeleton{
		Type: "xlsx", Title: "销售",
		Sheets: []SkeletonSheet{{
			Name: "销售", Rows: [][]string{{"月份", "金额"}, {"1月", "100"}, {"2月", "150"}, {"3月", "120"}},
			Charts: []SkeletonChart{{Type: "bar", Title: "月度金额", StartRow: 1, StartCol: 1, EndRow: 4, EndCol: 2}},
		}},
	}
	raw, err := json.Marshal(sk)
	if err != nil {
		t.Fatal(err)
	}
	_, data, err := RenderFromSkeletonJSON(raw)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	// excelize 把图表存为 xl/charts/chartN.xml 的 zip part；检查它存在即可。
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	foundChart := false
	for _, zf := range zr.File {
		if strings.HasPrefix(zf.Name, "xl/charts/chart") && strings.HasSuffix(zf.Name, ".xml") {
			foundChart = true
			break
		}
	}
	if !foundChart {
		t.Fatal("expected an xl/charts/chartN.xml part in the workbook")
	}
	// 非法图表类型/范围必须报错。
	bad := sk
	bad.Sheets[0].Charts = []SkeletonChart{{Type: "donut-chart", StartRow: 1, StartCol: 1, EndRow: 2, EndCol: 2}}
	badJSON, _ := json.Marshal(bad)
	if _, _, err := RenderFromSkeletonJSON(badJSON); err == nil {
		t.Fatal("unsupported chart type should error")
	}
}

// TestRenderDOCXTable verifies a WordprocessingML table is emitted.
func TestRenderDOCXTable(t *testing.T) {
	sk := Skeleton{
		Type: "docx", Title: "商务报告",
		Paragraphs: []string{"# 概览", "本季度整体向好。"},
		Tables: []SkeletonTable{{
			Headers: []string{"指标", "数值"},
			Rows:    [][]string{{"收入", "100"}, {"利润", "20"}},
		}},
	}
	raw, _ := json.Marshal(sk)
	_, data, err := RenderFromSkeletonJSON(raw)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	part, err := findZip(zr, "word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	xmlBody, _ := io.ReadAll(part)
	part.Close()
	s := string(xmlBody)
	for _, want := range []string{"<w:tbl>", "指标", "数值", "收入", "利润"} {
		if !strings.Contains(s, want) {
			t.Errorf("document.xml missing %q", want)
		}
	}
}

// TestRenderPPTXLayouts covers cover / table / two-col slide layouts and that
// their text survives our own parser (round-trip).
func TestRenderPPTXLayouts(t *testing.T) {
	sk := Skeleton{
		Type: "pptx", Title: "发布会",
		Slides: []SkeletonSlide{
			{Title: "封面", Layout: "cover"},
			{Title: "对比", Layout: "table", Table: &SkeletonTable{Headers: []string{"方案", "成本"}, Rows: [][]string{{"A", "10"}, {"B", "20"}}}},
			{Title: "优劣势", Layout: "two-col", Left: []string{"快", "稳"}, Right: []string{"贵", "慢"}},
			{Title: "要点", Bullets: []string{"默认布局", "要点二"}},
		},
	}
	raw, _ := json.Marshal(sk)
	_, data, err := RenderFromSkeletonJSON(raw)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	md, err := Parse("deck.pptx", "", data)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	for _, want := range []string{"封面", "方案", "成本", "快", "稳", "贵", "慢", "默认布局", "要点二"} {
		if !strings.Contains(md, want) {
			t.Errorf("round-trip missing %q in:\n%s", want, md)
		}
	}
	if !strings.Contains(md, "## 第 4 页") {
		t.Errorf("expected 4 slides")
	}
}
