package store

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"

	_ "github.com/jackc/pgx/v5/stdlib" // register "pgx" database/sql driver
	_ "modernc.org/sqlite"             // register "sqlite" database/sql driver
)

// DriverName maps a logical driver name to the registered database/sql driver.
func DriverName(driver string) (string, error) {
	switch driver {
	case "sqlite":
		return "sqlite", nil
	case "postgres":
		return "pgx", nil
	default:
		return "", fmt.Errorf("unsupported driver %q", driver)
	}
}

// OpenFromConfig opens a store using the logical driver name and its DSN.
// driver is "sqlite" or "postgres"; dsn is a file path or a connection string.
func OpenFromConfig(ctx context.Context, driver, dsn string) (Store, error) {
	reg, err := DriverName(driver)
	if err != nil {
		return nil, err
	}
	return Open(ctx, reg, dsn)
}

// SQLiteDSN builds a SQLite DSN from a file path, enabling WAL journal mode
// and a busy timeout so concurrent writes queue instead of failing.
//
// The path is resolved to an absolute one and the URI is assembled by hand:
// url.URL renders a bare relative path as "file://name.db", which SQLite's URI
// parser reads as a hostname with an empty path and answers SQLITE_NOMEM.
// _pragma is added twice, so url.Values.Set (last value wins) must not be used.
func SQLiteDSN(path string) string {
	abs := path
	if p, err := filepath.Abs(path); err == nil {
		abs = p
	}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	return "file:" + abs + "?" + q.Encode()
}
