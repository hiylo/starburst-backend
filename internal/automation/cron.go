package automation

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// nextCron computes the next fire time after t for a 6-field cron expression
// (second minute hour day month weekday). It supports exact values and simple
// "*", "*/n", and comma lists. A minimal but correct implementation for the
// common patterns; complex ranges (a-b) are not supported yet.
func nextCron(after time.Time, fields []string) (time.Time, error) {
	if len(fields) != 6 {
		return time.Time{}, fmt.Errorf("cron: expected 6 fields, got %d", len(fields))
	}
	secs, err := parseField(fields[0], 0, 59)
	if err != nil {
		return time.Time{}, fmt.Errorf("cron second: %w", err)
	}
	mins, err := parseField(fields[1], 0, 59)
	if err != nil {
		return time.Time{}, fmt.Errorf("cron minute: %w", err)
	}
	hours, err := parseField(fields[2], 0, 23)
	if err != nil {
		return time.Time{}, fmt.Errorf("cron hour: %w", err)
	}
	days, err := parseField(fields[3], 1, 31)
	if err != nil {
		return time.Time{}, fmt.Errorf("cron day: %w", err)
	}
	months, err := parseField(fields[4], 1, 12)
	if err != nil {
		return time.Time{}, fmt.Errorf("cron month: %w", err)
	}
	weeks, err := parseField(fields[5], 0, 6)
	if err != nil {
		return time.Time{}, fmt.Errorf("cron weekday: %w", err)
	}

	// Scan forward second by second up to a sane horizon (3 years).
	t := after.Add(time.Second)
	horizon := after.AddDate(3, 0, 0)
	for t.Before(horizon) {
		if contains(months, int(t.Month())) &&
			contains(days, t.Day()) &&
			contains(weeks, int(t.Weekday())) &&
			contains(hours, t.Hour()) &&
			contains(mins, t.Minute()) &&
			contains(secs, t.Second()) {
			return t, nil
		}
		t = t.Add(time.Second)
	}
	return time.Time{}, fmt.Errorf("cron: no next time within horizon")
}

// parseField parses a cron field into a sorted set of allowed values.
func parseField(f string, min, max int) ([]int, error) {
	f = strings.TrimSpace(f)
	if f == "" {
		return nil, fmt.Errorf("empty field")
	}
	var out []int
	seen := make(map[int]bool)
	for _, part := range strings.Split(f, ",") {
		part = strings.TrimSpace(part)
		if part == "*" {
			for v := min; v <= max; v++ {
				if !seen[v] {
					seen[v] = true
					out = append(out, v)
				}
			}
			continue
		}
		if strings.HasPrefix(part, "*/") {
			step, err := strconv.Atoi(strings.TrimPrefix(part, "*/"))
			if err != nil || step <= 0 {
				return nil, fmt.Errorf("bad step %q", part)
			}
			for v := min; v <= max; v += step {
				if !seen[v] {
					seen[v] = true
					out = append(out, v)
				}
			}
			continue
		}
		v, err := strconv.Atoi(part)
		if err != nil || v < min || v > max {
			return nil, fmt.Errorf("bad value %q (want %d-%d)", part, min, max)
		}
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out, nil
}

func contains(set []int, v int) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

// ParseCron validates a 6-field cron expression and returns the split fields.
// It returns an error when the expression is malformed, so callers can reject
// invalid schedules up front.
func ParseCron(expr string) ([]string, error) {
	fields := strings.Fields(expr)
	if len(fields) != 6 {
		return nil, fmt.Errorf("cron: expected 6 fields, got %d", len(fields))
	}
	_, err := nextCron(time.Now(), fields)
	if err != nil {
		return nil, err
	}
	return fields, nil
}

// NextCron returns the next fire time strictly after `after` for a 6-field
// cron expression. It is the exported entry point used by the task scheduler.
func NextCron(after time.Time, expr string) (time.Time, error) {
	fields, err := ParseCron(expr)
	if err != nil {
		return time.Time{}, err
	}
	return nextCron(after, fields)
}
