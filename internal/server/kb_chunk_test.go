package server

import (
	"strings"
	"testing"
)

// TestSplitKnowledgeText covers the chunker: heading grouping, size-based
// splitting, blank-line flush and no-heading fallback.
func TestSplitKnowledgeText(t *testing.T) {
	// Short document with a heading stays one chunk titled by the heading.
	short := splitKnowledgeText("readme.md", "# 概述\n这是短文档内容。")
	if len(short) != 1 || short[0].title != "概述" {
		t.Fatalf("short = %+v", short)
	}
	if !strings.Contains(short[0].content, "这是短文档内容") {
		t.Fatalf("short content = %q", short[0].content)
	}

	// Headings split into separate chunks.
	two := splitKnowledgeText("doc.md", "# 第一章\n甲内容。\n\n## 第二章\n乙内容。")
	if len(two) != 2 {
		t.Fatalf("expected 2 chunks, got %d: %+v", len(two), two)
	}
	if two[0].title != "第一章" || two[1].title != "第二章" {
		t.Fatalf("titles = %q / %q", two[0].title, two[1].title)
	}

	// Long body splits on line boundaries.
	var big strings.Builder
	for i := 0; i < 120; i++ {
		big.WriteString("第")
		big.WriteByte(byte('a' + i%26))
		big.WriteString("行内容非常长，用来把片段撑到超过目标长度以触发切分。\n")
	}
	long := splitKnowledgeText("long.md", big.String())
	if len(long) < 2 {
		t.Fatalf("long doc should split, got %d chunks", len(long))
	}
	for i, p := range long {
		if p.title != "long.md" {
			t.Fatalf("no-heading fallback title %d = %q", i, p.title)
		}
	}

	// Blank content yields nothing.
	if got := splitKnowledgeText("empty.md", "   \n\n "); len(got) != 0 {
		t.Fatalf("blank content should produce 0 chunks, got %+v", got)
	}
}

// TestSplitKnowledgeTextHeadingDetection pins the heading syntax accepted by
// the chunker: ATX headings (`#`..`######`) with a following space/tab.
func TestSplitKnowledgeTextHeadingDetection(t *testing.T) {
	for _, line := range []string{"# Title", "## Sub", "###### Deep", "#\tTabbed"} {
		if !isMarkdownHeading(line) {
			t.Errorf("%q should be a heading", line)
		}
	}
	for _, line := range []string{"", "plain text", "#", "#nospace", "##3"} {
		if isMarkdownHeading(line) {
			t.Errorf("%q should NOT be a heading", line)
		}
	}
}
