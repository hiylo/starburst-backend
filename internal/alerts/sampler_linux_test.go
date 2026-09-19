//go:build linux

package alerts

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// absent marks a fixture that must not be created, so the sampler sees a
// missing /proc file.
const absent = "\x00absent"

// pointStatAt points procStatPath at a fixture holding content and restores it
// afterwards. Re-calling it per reading rewrites the same fixture.
func pointStatAt(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stat")
	old := procStatPath
	t.Cleanup(func() { procStatPath = old })
	if content == absent {
		procStatPath = filepath.Join(t.TempDir(), "definitely-missing")
		return
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write stat fixture: %v", err)
	}
	procStatPath = path
}

// pointMeminfoAt is the /proc/meminfo counterpart of pointStatAt.
func pointMeminfoAt(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "meminfo")
	old := procMeminfoPath
	t.Cleanup(func() { procMeminfoPath = old })
	if content == absent {
		procMeminfoPath = filepath.Join(t.TempDir(), "definitely-missing")
		return
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write meminfo fixture: %v", err)
	}
	procMeminfoPath = path
}

// statLine renders a /proc/stat aggregate cpu line:
// "cpu user nice system idle iowait [irq softirq steal ...]".
func statLine(user, nice, system, idle, iowait uint64, rest ...uint64) string {
	counters := append([]uint64{user, nice, system, idle, iowait}, rest...)
	parts := make([]string, 0, len(counters)+1)
	parts = append(parts, "cpu")
	for _, c := range counters {
		parts = append(parts, strconv.FormatUint(c, 10))
	}
	return strings.Join(parts, " ") + "\n"
}

// readCPU drives cpuPercent over a series of fixture contents with one sampler,
// so the baseline carries from one reading to the next.
func readCPU(t *testing.T, readings ...string) []float64 {
	t.Helper()
	s := &linuxSampler{}
	out := make([]float64, 0, len(readings))
	for _, r := range readings {
		pointStatAt(t, r)
		out = append(out, s.cpuPercent())
	}
	return out
}

func TestCPUPercentDelta(t *testing.T) {
	const (
		// total 520, idle+iowait 310
		base = "cpu 100 10 100 300 10 0 0 0\n"
		// total 940, idle+iowait 520 -> 50% busy
		halfBusy = "cpu 200 20 200 500 20 0 0 0\n"
	)

	tests := []struct {
		name     string
		readings []string
		want     []float64
	}{
		{
			name:     "first read only primes the baseline",
			readings: []string{base},
			want:     []float64{0},
		},
		{
			name:     "steady delta",
			readings: []string{base, halfBusy},
			want:     []float64{0, 50},
		},
		{
			name:     "identical counters mean zero elapsed work",
			readings: []string{base, base},
			want:     []float64{0, 0},
		},
		{
			name:     "fully busy leaves idle frozen",
			readings: []string{base, "cpu 200 10 100 300 10 0 0 0\n"},
			want:     []float64{0, 100},
		},
		{
			name:     "iowait counts as idle time",
			readings: []string{"cpu 0 0 0 100 0 0 0 0\n", "cpu 100 0 0 100 100 0 0 0\n"},
			want:     []float64{0, 50},
		},
		{
			name:     "extra kernel fields are ignored",
			readings: []string{"cpu 100 10 100 300 10 5 5 5 2 2\n", "cpu 200 20 200 500 20 5 5 5 2 2\n"},
			want:     []float64{0, 50},
		},
		{
			name:     "only the aggregate cpu line is parsed",
			readings: []string{base + "cpu0 1 1 1 1 1 1 1 1\n", halfBusy + "cpu0 999 999 999 999 999 999 999 999\n"},
			want:     []float64{0, 50},
		},
		{
			name:     "counter reset re-primes instead of underflowing",
			readings: []string{base, "cpu 1 1 1 10 1 0 0 0\n", "cpu 53 0 0 60 1 0 0 0\n"},
			want:     []float64{0, 0, 50},
		},
		{
			name:     "idle counter going backwards re-primes the baseline",
			readings: []string{base, "cpu 200 20 200 200 20 0 0 0\n"},
			want:     []float64{0, 0},
		},
		{
			name:     "missing file yields zero and keeps the sampler usable",
			readings: []string{absent, base, halfBusy},
			want:     []float64{0, 0, 50},
		},
		{
			name:     "empty file yields zero",
			readings: []string{"", base, halfBusy},
			want:     []float64{0, 0, 50},
		},
		{
			name:     "first line is not the aggregate cpu line",
			readings: []string{"intr 12345 6 7\n" + base, base, halfBusy},
			want:     []float64{0, 0, 50},
		},
		{
			name:     "too few counter fields",
			readings: []string{"cpu 1 2 3\n", base, halfBusy},
			want:     []float64{0, 0, 50},
		},
		{
			name:     "non numeric counter",
			readings: []string{"cpu 100 10 x 300 10\n", base, halfBusy},
			want:     []float64{0, 0, 50},
		},
		{
			name:     "truncated line without trailing newline still parses",
			readings: []string{strings.TrimSuffix(base, "\n"), strings.TrimSuffix(halfBusy, "\n")},
			want:     []float64{0, 50},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := readCPU(t, tc.readings...)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d readings, want %d", len(got), len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("reading %d = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestCPUPercentPrimesBaselines(t *testing.T) {
	pointStatAt(t, "cpu 100 10 100 300 10 0 0 0\n")
	s := &linuxSampler{}
	if got := s.cpuPercent(); got != 0 {
		t.Fatalf("first cpuPercent = %v, want 0", got)
	}
	// total 520, idle+iowait 310 must have been stored as the baseline.
	if s.prevTotal != 520 || s.prevIdle != 310 {
		t.Fatalf("baseline = total %d idle %d, want 520/310", s.prevTotal, s.prevIdle)
	}
}

func TestMemPercent(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    float64
	}{
		{
			name:    "typical meminfo",
			content: "MemTotal:      1000000 kB\nMemFree:         50000 kB\nMemAvailable:    250000 kB\nBuffers:          1000 kB\n",
			want:    75,
		},
		{
			name:    "fields in reverse order",
			content: "MemAvailable:    250000 kB\nMemTotal:      1000000 kB\n",
			want:    75,
		},
		{
			name:    "no available line means everything is used",
			content: "MemTotal:      1000000 kB\nMemFree:         50000 kB\n",
			want:    100,
		},
		{
			// total-avail is computed in unsigned arithmetic, so this corrupt
			// (kernel-impossible) input wraps and clamps to the top, not to 0.
			// Pinned to document that it degrades to "fully used", never a panic.
			name:    "available larger than total clamps to the top",
			content: "MemTotal:      1000000 kB\nMemAvailable:    2000000 kB\n",
			want:    100,
		},
		{
			name:    "zero total avoids a division by zero",
			content: "MemTotal:           0 kB\nMemAvailable:    200000 kB\n",
			want:    0,
		},
		{
			name:    "no meminfo lines at all",
			content: "HugePages_Total:       0\nHugepagesize:       2048 kB\n",
			want:    0,
		},
		{
			name:    "non numeric value",
			content: "MemTotal:      lots kB\nMemAvailable:    250000 kB\n",
			want:    0,
		},
		{
			name:    "value with no unit column",
			content: "MemTotal:      1000\nMemAvailable:    500\n",
			want:    50,
		},
		{
			name:    "truncated line with only a key",
			content: "MemTotal:\nMemAvailable:    500\n",
			want:    0,
		},
		{
			name:    "empty file",
			content: "",
			want:    0,
		},
		{
			name:    "missing file",
			content: absent,
			want:    0,
		},
		{
			name:    "negative value is rejected",
			content: "MemTotal:      -1000 kB\nMemAvailable:    500 kB\n",
			want:    0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pointMeminfoAt(t, tc.content)
			if got := memPercent(); got != tc.want {
				t.Fatalf("memPercent = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMemValue(t *testing.T) {
	tests := []struct {
		line string
		want uint64
	}{
		{"MemTotal:      1024 kB", 1024},
		{"MemAvailable: 0 kB", 0},
		{"MemTotal:", 0},
		{"MemTotal: abc kB", 0},
		{"MemTotal: -5 kB", 0},
		{"MemTotal: 1024", 1024},
		{"MemTotal: 12 34 kB", 12},
		{"MemSlab: 999999999999999999999 kB", 0}, // overflow must not panic
	}
	for _, tc := range tests {
		if got := memValue(tc.line); got != tc.want {
			t.Fatalf("memValue(%q) = %d, want %d", tc.line, got, tc.want)
		}
	}
}

func TestClampPct(t *testing.T) {
	tests := []struct {
		in, want float64
	}{
		{-50, 0},
		{-0.0001, 0},
		{0, 0},
		{33.3, 33.3},
		{100, 100},
		{100.5, 100},
		{1e9, 100},
	}
	for _, tc := range tests {
		if got := clampPct(tc.in); got != tc.want {
			t.Fatalf("clampPct(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestSampleWithFixtures covers the one-pass Sample() wiring on top of the
// fixture seams: memory comes straight from the file, the first CPU read only
// primes, and disk always reports a usable percentage of the real root fs.
func TestSampleWithFixtures(t *testing.T) {
	pointStatAt(t, "cpu 100 10 100 300 10 0 0 0\n")
	pointMeminfoAt(t, "MemTotal:      1000 kB\nMemAvailable:    250 kB\n")

	s := &linuxSampler{}
	first := s.Sample()
	if first.CPU != 0 {
		t.Fatalf("first sample cpu = %v, want 0 (priming)", first.CPU)
	}
	if first.Mem != 75 {
		t.Fatalf("sample mem = %v, want 75", first.Mem)
	}
	if first.Disk <= 0 || first.Disk > 100 {
		t.Fatalf("sample disk = %v, want a real usage percentage", first.Disk)
	}

	pointStatAt(t, "cpu 200 20 200 500 20 0 0 0\n")
	second := s.Sample()
	if second.CPU != 50 {
		t.Fatalf("second sample cpu = %v, want 50", second.CPU)
	}
	if second.Disk > 100 || second.Disk < 0 {
		t.Fatalf("sample disk = %v out of range", second.Disk)
	}
}

// TestSampleWithBrokenProc checks that a host where /proc is unreadable still
// returns an all-but-disk zero metric set rather than panicking.
func TestSampleWithBrokenProc(t *testing.T) {
	pointStatAt(t, absent)
	pointMeminfoAt(t, absent)

	m := (&linuxSampler{}).Sample()
	if m.CPU != 0 || m.Mem != 0 {
		t.Fatalf("sample with missing /proc = %+v, want zero cpu and mem", m)
	}
}

func TestNewPlatformSamplerIsLinux(t *testing.T) {
	if _, ok := newPlatformSampler().(*linuxSampler); !ok {
		t.Fatalf("newPlatformSampler returned %T, want *linuxSampler", newPlatformSampler())
	}
}
