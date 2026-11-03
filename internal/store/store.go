// Package store persists perfwatch samples in SQLite: per-second machine samples, per-minute
// rollups for the machine and for the busiest programs, and the retention that ages them out.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"

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
-- Updated with every second, so a killed collector loses no machine rollup. last_ts is the
-- newest second rolled in: an older one is a replay, even after system_seconds has expired.
-- The p95 columns stay NULL until the minute is closed from its stored seconds.
CREATE TABLE system_minutes (
	minute             INTEGER PRIMARY KEY,
	last_ts            INTEGER NOT NULL,
	samples            INTEGER NOT NULL,
	capacity_ticks     INTEGER NOT NULL,
	busy_ticks         INTEGER NOT NULL,
	unattributed_ticks INTEGER NOT NULL,
	cpu_sum REAL NOT NULL, cpu_max REAL NOT NULL, cpu_p95 REAL,
	ram_sum REAL NOT NULL, ram_max REAL NOT NULL, ram_p95 REAL
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
	`
-- A peak episode. Its samples run from preroll_ts through end_ts and on through the samples
-- that cooled it down; end_ts stays NULL while the episode is open. last_warm_ts is the end of
-- its newest sample that was not cool, which becomes end_ts when the episode ends, also when
-- the next collector closes an episode a killed one left open.
CREATE TABLE episodes (
	id           INTEGER PRIMARY KEY,
	preroll_ts   INTEGER NOT NULL,
	start_ts     INTEGER NOT NULL UNIQUE,
	end_ts       INTEGER,
	last_warm_ts INTEGER NOT NULL,
	trigger      TEXT NOT NULL
);
CREATE TABLE episode_seconds (
	episode_id       INTEGER NOT NULL REFERENCES episodes (id) ON DELETE CASCADE,
	ts               INTEGER NOT NULL,
	span_ms          INTEGER NOT NULL,
	cpu_pct          REAL NOT NULL,
	ram_pct          REAL NOT NULL,
	unattributed_pct REAL NOT NULL,
	PRIMARY KEY (episode_id, ts)
) WITHOUT ROWID;
CREATE TABLE episode_programs (
	episode_id    INTEGER NOT NULL REFERENCES episodes (id) ON DELETE CASCADE,
	ts            INTEGER NOT NULL,
	program_id    INTEGER NOT NULL REFERENCES programs (id),
	cpu_pct       REAL NOT NULL,
	private_bytes INTEGER NOT NULL,
	instances     INTEGER NOT NULL,
	PRIMARY KEY (episode_id, ts, program_id)
) WITHOUT ROWID;
CREATE INDEX episode_programs_by_program ON episode_programs (program_id);
`,
	`
-- The machine's capacity over each episode second, so per-program shares become CPU-seconds.
-- Rows stored before this column existed read 0 and count as no capacity.
ALTER TABLE episode_seconds ADD COLUMN capacity_ticks INTEGER NOT NULL DEFAULT 0;
`,
	`
-- One process instance an episode sample named, with what it ran and who started it. ancestry
-- is the running parent chain when first stored, oldest first, as name[pid] joined by " > ".
CREATE TABLE processes (
	id           INTEGER PRIMARY KEY,
	pid          INTEGER NOT NULL,
	create_time  INTEGER NOT NULL,
	name         TEXT NOT NULL,
	command_line TEXT NOT NULL DEFAULT '',
	ancestry     TEXT NOT NULL DEFAULT '',
	UNIQUE (pid, create_time)
);
CREATE TABLE episode_processes (
	episode_id    INTEGER NOT NULL REFERENCES episodes (id) ON DELETE CASCADE,
	ts            INTEGER NOT NULL,
	process_id    INTEGER NOT NULL REFERENCES processes (id),
	cpu_pct       REAL NOT NULL,
	private_bytes INTEGER NOT NULL,
	PRIMARY KEY (episode_id, ts, process_id)
) WITHOUT ROWID;
CREATE INDEX episode_processes_by_process ON episode_processes (process_id);
-- One tool call of a coding agent, from its PreToolUse and PostToolUse hooks, which may arrive
-- in either order; a timestamp stays NULL until its hook arrives.
CREATE TABLE agent_actions (
	tool_use_id TEXT PRIMARY KEY,
	session_id  TEXT NOT NULL,
	tool        TEXT NOT NULL,
	command     TEXT NOT NULL,
	cwd         TEXT NOT NULL DEFAULT '',
	background  INTEGER NOT NULL DEFAULT 0,
	start_ts    INTEGER,
	end_ts      INTEGER,
	failed      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX agent_actions_by_start ON agent_actions (start_ts);
`,
}

// DB is an open perfwatch database.
type DB struct {
	sql        *sql.DB
	programIDs map[usage.ProgramKey]programRow
	processIDs map[processKey]int64
	// unusedPrograms deletes programs no table references; built from the schema's foreign keys.
	unusedPrograms string
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
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// One connection serializes this process's writers and keeps one page cache.
	sqlDB.SetMaxOpenConns(1)
	if err := migrate(ctx, sqlDB); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	unused, err := unusedProgramsQuery(ctx, sqlDB)
	if err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return &DB{sql: sqlDB, programIDs: map[usage.ProgramKey]programRow{}, processIDs: map[processKey]int64{}, unusedPrograms: unused}, nil
}

// migrate applies each pending migration under BEGIN IMMEDIATE and re-reads user_version
// inside it, so two processes opening a new database at once apply each step only once.
func migrate(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	for {
		if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			return err
		}
		done, err := migrateStep(ctx, conn)
		if err != nil {
			conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
			return err
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

func migrateStep(ctx context.Context, conn *sql.Conn) (done bool, err error) {
	var version int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return false, err
	}
	if version > len(migrations) {
		return false, fmt.Errorf("schema version %d is newer than this perfwatch (%d); upgrade perfwatch", version, len(migrations))
	}
	if version == len(migrations) {
		return true, nil
	}
	if _, err := conn.ExecContext(ctx, migrations[version]); err != nil {
		return false, fmt.Errorf("to version %d: %w", version+1, err)
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version+1)); err != nil {
		return false, err
	}
	return false, nil
}

// unusedProgramsQuery builds the DELETE for programs that no referencing table uses, from the
// foreign keys the schema declares, so a new table referencing programs is covered by itself.
func unusedProgramsQuery(ctx context.Context, db *sql.DB) (string, error) {
	rows, err := db.QueryContext(ctx, `
SELECT m.name, f."from" FROM sqlite_schema m JOIN pragma_foreign_key_list(m.name) f
WHERE m.type = 'table' AND f."table" = 'programs' ORDER BY m.name`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var unused []string
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			return "", err
		}
		unused = append(unused, fmt.Sprintf("NOT EXISTS (SELECT 1 FROM %s WHERE %s = programs.id)", table, column))
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(unused) == 0 {
		return "", fmt.Errorf("no table references programs")
	}
	return "DELETE FROM programs WHERE " + strings.Join(unused, " AND "), nil
}

// Close closes the database.
func (db *DB) Close() error {
	return db.sql.Close()
}
