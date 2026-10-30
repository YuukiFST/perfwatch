// Package store persists perfwatch samples in SQLite: per-second machine samples, per-minute
// rollups for the machine and for the busiest programs, and the retention that ages them out.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"

	"github.com/YuukiFST/perfwatch/internal/usage"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver; no cgo
)

// FileName is the database file inside the data directory.
const FileName = "perfwatch.db"

// migrations[i] moves the schema from user_version i to i+1.
var migrations = []string{
	`
CREATE TABLE programs (
	id            INTEGER PRIMARY KEY,
	name          TEXT NOT NULL,
	name_key      TEXT NOT NULL,
	service_group TEXT NOT NULL DEFAULT '',
	path          TEXT NOT NULL DEFAULT '',
	UNIQUE (name_key, service_group)
);
CREATE TABLE system_seconds (
	ts               INTEGER PRIMARY KEY,
	span_ms          INTEGER NOT NULL,
	cpu_pct          REAL NOT NULL,
	ram_pct          REAL NOT NULL,
	unattributed_pct REAL NOT NULL,
	ram_used_bytes   INTEGER NOT NULL
) WITHOUT ROWID;
CREATE TABLE system_minutes (
	minute             INTEGER PRIMARY KEY,
	samples            INTEGER NOT NULL,
	capacity_ticks     INTEGER NOT NULL,
	busy_ticks         INTEGER NOT NULL,
	unattributed_ticks INTEGER NOT NULL,
	cpu_avg REAL NOT NULL, cpu_max REAL NOT NULL, cpu_p95 REAL NOT NULL,
	ram_avg REAL NOT NULL, ram_max REAL NOT NULL, ram_p95 REAL NOT NULL
) WITHOUT ROWID;
CREATE TABLE program_minutes (
	minute            INTEGER NOT NULL,
	program_id        INTEGER NOT NULL REFERENCES programs (id),
	cpu_ticks         INTEGER NOT NULL,
	cpu_max_pct       REAL NOT NULL,
	private_max_bytes INTEGER NOT NULL,
	instances_max     INTEGER NOT NULL,
	PRIMARY KEY (minute, program_id)
) WITHOUT ROWID;
CREATE INDEX program_minutes_by_program ON program_minutes (program_id);
`,
}

// DB is an open perfwatch database.
type DB struct {
	sql        *sql.DB
	programIDs map[usage.ProgramKey]programRow
}

type programRow struct {
	id      int64
	hasPath bool
}

// Open opens or creates the database at path in WAL mode and brings its schema up to date.
func Open(ctx context.Context, path string) (*DB, error) {
	// A plain path, not a file: URI, so SQLite takes it verbatim; the driver strips the query.
	dsn := path + "?" + url.Values{"_pragma": {
		"busy_timeout(5000)", "journal_mode(WAL)", "synchronous(NORMAL)", "foreign_keys(1)",
		// The collector runs for months under a 30 MB working-set budget: cap the page cache at 2 MB.
		"cache_size(-2000)",
	}}.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// One connection serializes writers and keeps one page cache.
	db.SetMaxOpenConns(1)
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return &DB{sql: db, programIDs: map[usage.ProgramKey]programRow{}}, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("schema version %d is newer than this perfwatch (%d); upgrade perfwatch", version, len(migrations))
	}
	for ; version < len(migrations); version++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[version]); err != nil {
			tx.Rollback()
			return fmt.Errorf("to version %d: %w", version+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Close closes the database.
func (db *DB) Close() error {
	return db.sql.Close()
}
