package store

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// postgresReservedWords is the subset of PostgreSQL reserved keywords that
// cannot be used as bare column names. Using one breaks `--db postgres`
// migrations while passing SQLite (which is more permissive). Keep this list
// aligned with the Postgres docs (table: Keywords).
var postgresReservedWords = map[string]bool{
	"all": true, "analyse": true, "analyze": true, "and": true, "any": true,
	"array": true, "as": true, "asc": true, "asymmetric": true, "authorization": true,
	"binary": true, "both": true, "case": true, "cast": true, "check": true,
	"collate": true, "collation": true, "column": true, "concurrently": true,
	"constraint": true, "create": true, "cross": true, "current_catalog": true,
	"current_date": true, "current_role": true, "current_time": true,
	"current_timestamp": true, "current_user": true, "default": true, "deferrable": true,
	"desc": true, "distinct": true, "do": true, "else": true, "end": true,
	"except": true, "false": true, "fetch": true, "for": true, "foreign": true,
	"freeze": true, "from": true, "full": true, "grant": true, "group": true,
	"having": true, "ilike": true, "in": true, "initially": true, "inner": true,
	"intersect": true, "into": true, "is": true, "isnull": true, "join": true,
	"lateral": true, "leading": true, "left": true, "like": true, "limit": true,
	"localtime": true, "localtimestamp": true, "natural": true, "not": true,
	"notnull": true, "null": true, "offset": true, "on": true, "only": true,
	"or": true, "order": true, "outer": true, "overlaps": true, "placing": true,
	"primary": true, "references": true, "returning": true, "right": true,
	"select": true, "session_user": true, "similar": true, "some": true,
	"symmetric": true, "system_user": true, "table": true, "tablesample": true,
	"then": true, "to": true, "trailing": true, "true": true, "union": true,
	"unique": true, "user": true, "using": true, "variadic": true, "verbose": true,
	"when": true, "where": true, "window": true, "with": true,
}

var (
	reCreateTable = regexp.MustCompile(`CREATE TABLE IF NOT EXISTS (\w+) \((.*?)\)`)
	reColumns     = regexp.MustCompile(`(?m)^\s*(\w+)\s+(TEXT|INTEGER|BOOLEAN|TIMESTAMP|BLOB|SERIAL|BIGSERIAL|VARCHAR)`)
	reAlterColumn = regexp.MustCompile(`ALTER TABLE (\w+) ADD COLUMN (\w+)`)
)

// TestMigrationsNoPostgresReservedColumns scans the migration definitions for
// column names that are PostgreSQL reserved keywords. Such columns pass SQLite
// tests but fail on the production Postgres store (e.g. `user`).
func TestMigrationsNoPostgresReservedColumns(t *testing.T) {
	src, err := os.ReadFile("migrate.go")
	if err != nil {
		t.Fatalf("read migrate.go: %v", err)
	}
	s := string(src)
	var bad []string
	for _, m := range reCreateTable.FindAllStringSubmatch(s, -1) {
		table, body := m[1], m[2]
		for _, cm := range reColumns.FindAllStringSubmatch(body, -1) {
			if postgresReservedWords[strings.ToLower(cm[1])] {
				bad = append(bad, table+"."+cm[1])
			}
		}
	}
	for _, m := range reAlterColumn.FindAllStringSubmatch(s, -1) {
		if postgresReservedWords[strings.ToLower(m[2])] {
			bad = append(bad, m[1]+"+"+m[2])
		}
	}
	if len(bad) > 0 {
		t.Fatalf("migrations use PostgreSQL reserved column names: %v", bad)
	}
}
