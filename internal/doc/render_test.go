package doc

import (
	"encoding/json"
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