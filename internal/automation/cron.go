package automation

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// nextCron computes the next fire time after t for a 6-field cron expression
// (second minute hour day month weekday). Each field accepts exact values,
// "*", comma lists, ranges ("a-b") and steps ("*\/n", "a-b/n").
//
// Day-of-month and weekday are combined with AND, so "0 0 0 13 * 5" fires only
// on a Friday the 13th. OR semantics (the classic cron special case) are
// deliberately not implemented.
func nextCron(after time.Time, fields []string) (time.Time, error) {
	if len(fields) != 6 {
		return time.Time{}, fmt.Errorf("cron: expected 6 fields, got %d", len(fields))
	}
	secs, err := fieldSet(fields[0], 0, 59, "second")
	if err != nil {
		return time.Time{}, err
	}
	mins, err := fieldSet(fields[1], 0, 59, "minute")
	if err != nil {
		return time.Time{}, err
	}
	hours, err := fieldSet(fields[2], 0, 23, "hour")
	if err != nil {
		return time.Time{}, err
	}
	days, err := fieldSet(fields[3], 1, 31, "day")
	if err != nil {
		return time.Time{}, err
	}
	months, err := fieldSet(fields[4], 1, 12, "month")
	if err != nil {
		return time.Time{}, err
	}
	weeks, err := fieldSet(fields[5], 0, 6, "weekday")
	if err != nil {
		return time.Time{}, err
	}

	loc := after.Location()
	horizon := after.AddDate(3, 0, 0)
	// Drop the sub-second remainder before advancing so the result is always a
	// whole second after `after`.
	t := time.Date(after.Year(), after.Month(), after.Day(), after.Hour(), after.Minute(), after.Second(), 0, loc).Add(time.Second)

	// Advance field by field instead of second by second: an expression that
	// never matches (e.g. Feb 30) would otherwise cost ~95M iterations of
	// validation, and this runs on every scheduler tick.
	for t.Before(horizon) {
		switch {
		case !months[int(t.Month())]:
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
		case !days[t.Day()] || !weeks[int(t.Weekday())]:
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
		case !hours[t.Hour()]:
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc)
		case !mins[t.Minute()]:
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute()+1, 0, 0, loc)
		case !secs[t.Second()]:
			t = t.Add(time.Second)
		default:
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cron: no next time within horizon")
}

// fieldSet parses one cron field into an indexable membership set.
func fieldSet(f string, min, max int, name string) ([]bool, error) {
	vals, err := parseField(f, min, max)
	if err != nil {
		return nil, fmt.Errorf("cron %s: %w", name, err)
	}
	set := make([]bool, max+1)
	for _, v := range vals {
		set[v] = true
	}
	return set, nil
}

// parseField parses a cron field into the set of allowed values.
func parseField(f string, min, max int) ([]int, error) {
	f = strings.TrimSpace(f)
	if f == "" {
		return nil, fmt.Errorf("empty field")
	}
	var out []int
	seen := make(map[int]bool)
	add := func(v int) {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}

	for _, part := range strings.Split(f, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("empty element in %q", f)
		}

		body, step := part, 1
		hasStep := false
		if i := strings.Index(part, "/"); i >= 0 {
			body = strings.TrimSpace(part[:i])
			n, err := strconv.Atoi(strings.TrimSpace(part[i+1:]))
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("bad step %q", part)
			}
			step, hasStep = n, true
		}

		switch {
		case body == "*":
			for v := min; v <= max; v += step {
				add(v)
			}
		case strings.Contains(body, "-"):
			bounds := strings.SplitN(body, "-", 2)
			start, err := parseValue(bounds[0], min, max, part)
			if err != nil {
				return nil, err
			}
			end, err := parseValue(bounds[1], min, max, part)
			if err != nil {
				return nil, err
			}
			if start > end {
				return nil, fmt.Errorf("reversed range %q (want %d-%d)", part, min, max)
			}
			for v := start; v <= end; v += step {
				add(v)
			}
		default:
			start, err := parseValue(body, min, max, part)
			if err != nil {
				return nil, err
			}
			end := start
			if hasStep {
				end = max // "N/step" means N..max stepping by step
			}
			for v := start; v <= end; v += step {
				add(v)
			}
		}
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("no values in %q", f)
	}
	return out, nil
}

func parseValue(s string, min, max int, part string) (int, error) {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("bad value %q (want %d-%d)", part, min, max)
	}
	if v < min || v > max {
		return 0, fmt.Errorf("value %d out of range %d-%d in %q", v, min, max, part)
	}
	return v, nil
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
