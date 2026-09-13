package server

import (
	"strings"
	"testing"
	"time"
)

// TestDedupAdjacentRepeatPerformance guards the O(n³) worst case: highly
// duplicated text must finish quickly, not block the backend.
func TestDedupAdjacentRepeatPerformance(t *testing.T) {
	// 构造高度重复的文本（1000 字，块重复）
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		sb.WriteString("我们今天一起去看电影然后吃饭然后再回家")
	}
	long := sb.String()
	if len([]rune(long)) < 500 {
		t.Fatalf("test input too short: %d", len([]rune(long)))
	}
	start := time.Now()
	_ = dedupTranscript(long)
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("dedupTranscript took %v, want < 500ms", elapsed)
	}
}

// TestDedupAdjacentRepeatTruncatesLongText verifies the scan cap.
func TestDedupAdjacentRepeatTruncatesLongText(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 500; i++ {
		sb.WriteString("今天我们一起去超市买东西然后回家做饭")
	}
	long := sb.String()
	if len([]rune(long)) <= dedupScanRunes {
		t.Fatalf("test input too short: %d", len([]rune(long)))
	}
	out := dedupAdjacentRepeat(long)
	// 输出不应超过 dedupScanRunes + 一些边距
	if len([]rune(out)) > dedupScanRunes+10 {
		t.Fatalf("output too long: %d", len([]rune(out)))
	}
}
