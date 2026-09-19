package automation

import (
	"strings"
	"testing"
	"time"
)

func TestNextCronRanges(t *testing.T) {
	// Monday 2026-09-21 00:00:00 local.
	base := time.Date(2026, 9, 21, 0, 0, 0, 0, time.Local)

	tests := []struct {
		name string
		expr string
		want time.Time
	}{
		{
			name: "weekday range Mon-Fri",
			expr: "0 0 9 * * 1-5",
			want: time.Date(2026, 9, 21, 9, 0, 0, 0, time.Local),
		},
		{
			name: "weekday range skips weekend",
			expr: "0 30 8 * * 1-5",
			want: time.Date(2026, 9, 21, 8, 30, 0, 0, time.Local),
		},
		{
			name: "hour range with step",
			expr: "0 0 8-18/4 * * *",
			want: time.Date(2026, 9, 21, 8, 0, 0, 0, time.Local),
		},
		{
			name: "day-of-month range",
			expr: "0 0 0 20-25 * *",
			want: time.Date(2026, 9, 22, 0, 0, 0, 0, time.Local),
		},
		{
			name: "single value with step runs to field max",
			expr: "5/20 * * * * *",
			want: time.Date(2026, 9, 21, 0, 0, 5, 0, time.Local),
		},
		{
			name: "mixed list and range",
			expr: "0 0,30 6-7 * * *",
			want: time.Date(2026, 9, 21, 6, 0, 0, 0, time.Local),
		},
		{
			name: "second wildcard step",
			expr: "*/15 * * * * *",
			want: time.Date(2026, 9, 21, 0, 0, 15, 0, time.Local),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NextCron(base, tc.expr)
			if err != nil {
				t.Fatalf("NextCron(%q): %v", tc.expr, err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("NextCron(%q) = %s, want %s", tc.expr, got.Format(time.RFC3339), tc.want.Format(time.RFC3339))
			}
		})
	}
}

// TestNextCronWeekendRange pins the documented AND semantics between
// day-of-month and weekday, which the classic cron OR special case would
// change.
func TestNextCronWeekendRange(t *testing.T) {
	base := time.Date(2026, 9, 21, 0, 0, 0, 0, time.Local) // Monday
	got, err := NextCron(base, "0 0 0 13 * 6,0")          // only the 13th that is also a weekend day
	if err != nil {
		t.Fatalf("NextCron: %v", err)
	}
	if got.Weekday() != time.Saturday && got.Weekday() != time.Sunday {
		t.Errorf("weekday = %v, want Sat/Sun", got.Weekday())
	}
	if got.Day() != 13 {
		t.Errorf("day = %d, want 13", got.Day())
	}
}

func TestNextCronRejectsBadFields(t *testing.T) {
	base := time.Date(2026, 9, 21, 0, 0, 0, 0, time.Local)
	bad := []struct {
		expr string
		want string // substring of the expected error
	}{
		{"0 0 0 * * 5-1", "reversed range"},
		{"0 60 * * * *", "out of range"},
		{"0 0 24 * * *", "out of range"},
		{"0 0 * * * 7", "out of range"},
		{"0 */0 * * * *", "bad step"},
		{"0 abc * * * *", "bad value"},
		{"0 1,,2 * * * *", "empty element"},
		{"0 0 0 30 2 *", "no next time"},
	}
	for _, tc := range bad {
		t.Run(tc.expr, func(t *testing.T) {
			_, err := NextCron(base, tc.expr)
			if err == nil {
				t.Fatalf("NextCron(%q) succeeded, want error containing %q", tc.expr, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("NextCron(%q) error = %q, want it to contain %q", tc.expr, err, tc.want)
			}
		})
	}
}

// TestNextCronUnsatisfiableIsFast guards the regression where validation
// scanned forward second by second, so an expression that can never fire
// (Feb 30th) cost ~2.9s to reject on every scheduler tick.
func TestNextCronUnsatisfiableIsFast(t *testing.T) {
	base := time.Date(2026, 9, 21, 0, 0, 0, 0, time.Local)
	exprs := []string{"0 0 0 30 2 *", "0 0 0 31 2 *", "0 0 0 31 4 *"}

	for _, expr := range exprs {
		start := time.Now()
		_, err := NextCron(base, expr)
		elapsed := time.Since(start)
		if err == nil {
			t.Fatalf("NextCron(%q) succeeded, want unsatisfiable error", expr)
		}
		if elapsed > 50*time.Millisecond {
			t.Errorf("NextCron(%q) took %s, want <50ms", expr, elapsed)
		}
	}
}

// TestNextCronRareButValidIsFast covers expressions that do eventually fire but
// are far away; the old scan paid 279ms for a yearly schedule.
func TestNextCronRareButValidIsFast(t *testing.T) {
	base := time.Date(2026, 9, 21, 0, 0, 0, 0, time.Local)
	start := time.Now()
	got, err := NextCron(base, "0 0 0 1 1 *")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("NextCron: %v", err)
	}
	if got.Year() != 2027 || got.Month() != time.January || got.Day() != 1 {
		t.Errorf("got %s, want 2027-01-01", got.Format(time.RFC3339))
	}
	if elapsed > 20*time.Millisecond {
		t.Errorf("yearly schedule took %s, want <20ms", elapsed)
	}
}

func TestParseFieldValues(t *testing.T) {
	tests := []struct {
		field string
		min   int
		max   int
		want  []int
	}{
		{"*", 0, 4, []int{0, 1, 2, 3, 4}},
		{"*/2", 0, 6, []int{0, 2, 4, 6}},
		{"1-3", 0, 6, []int{1, 2, 3}},
		{"1-5/2", 0, 6, []int{1, 3, 5}},
		{"2,1,2", 0, 6, []int{2, 1}}, // dedup, input order preserved
		{"4", 0, 6, []int{4}},
		{"3/2", 0, 7, []int{3, 5, 7}},
	}
	for _, tc := range tests {
		t.Run(tc.field, func(t *testing.T) {
			got, err := parseField(tc.field, tc.min, tc.max)
			if err != nil {
				t.Fatalf("parseField(%q): %v", tc.field, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parseField(%q) = %v, want %v", tc.field, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("parseField(%q) = %v, want %v", tc.field, got, tc.want)
				}
			}
		})
	}
}

func TestParseCronAcceptsRanges(t *testing.T) {
	fields, err := ParseCron("0 0 9 * * 1-5")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}
	if len(fields) != 6 || fields[5] != "1-5" {
		t.Errorf("ParseCron returned %v, want 6 fields ending in 1-5", fields)
	}
	if _, err := ParseCron("0 0 9 * *"); err == nil {
		t.Error("ParseCron accepted a 5-field expression")
	}
}
