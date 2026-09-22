package store

import (
	"strings"
	"time"
)

// rebind converts a SQLite-style statement (using ? placeholders) into a
// statement using $N placeholders for PostgreSQL. SQLite queries are returned
// unchanged. This keeps one source of truth for SQL while supporting both
// drivers through database/sql.
func rebind(driver, query string) string {
	if driver != "pgx" {
		return query
	}
	// Replace each ? with $N. This is a simple scan; the codebase never uses
	// '?' inside string literals, so a naive scan is safe here.
	var sb strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			sb.WriteString("$")
			sb.WriteString(itoa(n))
		} else {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits [16]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[i:])
}

// sqliteTime renders t in the plain UTC format SQLite stores for
// CURRENT_TIMESTAMP ("2006-01-02 15:04:05"). Binding time.Time directly on
// SQLite produces a zone-suffixed text (e.g. "+0800"), which miscompares
// against the UTC column values on non-UTC hosts; this keeps both sides on the
// same format. PostgreSQL receives time.Time as-is.
func sqliteTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05")
}

// sqliteTimePtr is the *time.Time variant of sqliteTime: nil stays nil (NULL),
// non-nil is rendered as UTC text for SQLite bindings.
func sqliteTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return sqliteTime(*t)
}
