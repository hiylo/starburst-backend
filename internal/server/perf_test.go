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

// TestDedupAdjacentRepeatTruncatesLongText verifies the scan cap: the O(n³)
// scan is bounded to the prefix window, and the tail beyond the window passes
// through unchanged (never dropped, never rescanned).
func TestDedupAdjacentRepeatTruncatesLongText(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 500; i++ {
		sb.WriteString("今天我们一起去超市买东西然后回家做饭")
	}
	long := sb.String()
	runes := []rune(long)
	inLen := len(runes)
	if inLen <= dedupScanRunes {
		t.Fatalf("test input too short: %d", inLen)
	}
	tailLen := inLen - dedupScanRunes
	start := time.Now()
	out := dedupAdjacentRepeat(long)
	elapsed := time.Since(start)
	outLen := len([]rune(out))
	// 扫描窗口外的正文必须原样保留：输出 ≥ 窗口后尾部长度。
	if outLen < tailLen {
		t.Fatalf("output length = %d, want >= tail %d (扫描窗口外正文被截断)", outLen, tailLen)
	}
	// 输入里是 15 字完整句子重复，前缀窗口（2000 字）应被折叠到很短，
	// 因此输出应明显小于原长——证明窗口确实执行了去重。
	if outLen >= inLen {
		t.Fatalf("output length = %d, want < %d (扫描窗口未去重)", outLen, inLen)
	}
	// 扫描窗口去重必须快速完成（O(n³) 上限防护）。
	if elapsed > 500*time.Millisecond {
		t.Fatalf("dedupAdjacentRepeat took %v, want < 500ms", elapsed)
	}
}
