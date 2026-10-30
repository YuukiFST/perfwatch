package store

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/YuukiFST/perfwatch/internal/usage"
)

// Retention windows, counted back from the newest sample.
const (
	SecondsRetention        = 7 * 24 * time.Hour
	MinutesRetention        = 365 * 24 * time.Hour
	ProgramMinutesRetention = 90 * 24 * time.Hour
)

// Second is one machine sample: the interval ending at End. It belongs to the minute End
// falls in.
type Second struct {
	End                 time.Time
	Span                time.Duration
	CapacityTicks       uint64
	BusyTicks           uint64
	UnattributedTicks   uint64
	CPUPercent          float64
	RAMPercent          float64
	UnattributedPercent float64
	RAMUsedBytes        uint64
}

// ProgramMinute is one program's usage over a minute.
type ProgramMinute struct {
	Program         usage.Program
	CPUTicks        uint64
	CPUMaxPercent   float64
	PrivateMaxBytes uint64
	InstancesMax    int
}

// AddSecond stores s and rolls it into its minute in one transaction, and reports whether it
// was new. A second at or before the newest one already in its minute is a replay and changes
// nothing, also after system_seconds has expired it.
func (db *DB) AddSecond(ctx context.Context, s Second) (added bool, err error) {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store second: %w", err)
	}
	defer tx.Rollback()
	ts := s.End.UnixMilli()
	res, err := tx.ExecContext(ctx, `
INSERT INTO system_minutes (minute, last_ts, samples, capacity_ticks, busy_ticks, unattributed_ticks,
	cpu_sum, cpu_max, ram_sum, ram_max)
VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (minute) DO UPDATE SET
	last_ts = excluded.last_ts,
	samples = samples + 1,
	capacity_ticks = capacity_ticks + excluded.capacity_ticks,
	busy_ticks = busy_ticks + excluded.busy_ticks,
	unattributed_ticks = unattributed_ticks + excluded.unattributed_ticks,
	cpu_sum = cpu_sum + excluded.cpu_sum, cpu_max = max(cpu_max, excluded.cpu_max), cpu_p95 = NULL,
	ram_sum = ram_sum + excluded.ram_sum, ram_max = max(ram_max, excluded.ram_max), ram_p95 = NULL
WHERE last_ts < excluded.last_ts`,
		s.End.Truncate(time.Minute).UnixMilli(), ts, int64(s.CapacityTicks), int64(s.BusyTicks), int64(s.UnattributedTicks),
		s.CPUPercent, s.CPUPercent, s.RAMPercent, s.RAMPercent)
	if err != nil {
		return false, fmt.Errorf("store minute: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store minute: %w", err)
	}
	if n == 0 {
		return false, nil
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO system_seconds (ts, span_ms, cpu_pct, ram_pct, unattributed_pct, ram_used_bytes)
VALUES (?, ?, ?, ?, ?, ?)`,
		ts, s.Span.Milliseconds(), s.CPUPercent, s.RAMPercent, s.UnattributedPercent, int64(s.RAMUsedBytes))
	if err != nil {
		return false, fmt.Errorf("store second: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store second: %w", err)
	}
	return true, nil
}

// CloseMinute sets the p95 of the minute starting at start from its stored seconds and adds
// the programs' usage to the minute; a minute the collector restarted within sums both runs.
func (db *DB) CloseMinute(ctx context.Context, start time.Time, programs []ProgramMinute) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("close minute: %w", err)
	}
	defer tx.Rollback()
	minute := start.UnixMilli()
	if err := setMinuteP95(ctx, tx, minute); err != nil {
		return err
	}
	staged := map[usage.ProgramKey]programRow{}
	for _, p := range programs {
		id, err := db.programID(ctx, tx, p.Program, staged)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO program_minutes (minute, program_id, cpu_ticks, cpu_max_pct, private_max_bytes, instances_max)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (minute, program_id) DO UPDATE SET
	cpu_ticks = cpu_ticks + excluded.cpu_ticks,
	cpu_max_pct = max(cpu_max_pct, excluded.cpu_max_pct),
	private_max_bytes = max(private_max_bytes, excluded.private_max_bytes),
	instances_max = max(instances_max, excluded.instances_max)`,
			minute, id, int64(p.CPUTicks), p.CPUMaxPercent, int64(p.PrivateMaxBytes), p.InstancesMax)
		if err != nil {
			return fmt.Errorf("store program minute: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("close minute: %w", err)
	}
	maps.Copy(db.programIDs, staged)
	return nil
}

// FinishOpenMinutes sets the p95 of every minute a stopped collector left open. Their program
// usage stayed in that collector's memory and is lost.
func (db *DB) FinishOpenMinutes(ctx context.Context) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("finish open minutes: %w", err)
	}
	defer tx.Rollback()
	var open []int64
	if err := collect(ctx, tx, &open, "SELECT minute FROM system_minutes WHERE cpu_p95 IS NULL"); err != nil {
		return fmt.Errorf("finish open minutes: %w", err)
	}
	for _, minute := range open {
		if err := setMinuteP95(ctx, tx, minute); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("finish open minutes: %w", err)
	}
	return nil
}

// setMinuteP95 sets the nearest-rank 95th percentile of CPU and RAM over the stored seconds of
// a minute. A minute whose seconds have all expired takes its maximum instead.
func setMinuteP95(ctx context.Context, tx *sql.Tx, minute int64) error {
	var cpu, ram []float64
	window := []any{minute, minute + time.Minute.Milliseconds()}
	if err := collect(ctx, tx, &cpu, "SELECT cpu_pct FROM system_seconds WHERE ts >= ? AND ts < ?", window...); err != nil {
		return fmt.Errorf("close minute: %w", err)
	}
	if err := collect(ctx, tx, &ram, "SELECT ram_pct FROM system_seconds WHERE ts >= ? AND ts < ?", window...); err != nil {
		return fmt.Errorf("close minute: %w", err)
	}
	_, err := tx.ExecContext(ctx, `
UPDATE system_minutes SET cpu_p95 = coalesce(?, cpu_max), ram_p95 = coalesce(?, ram_max) WHERE minute = ?`,
		p95(cpu), p95(ram), minute)
	if err != nil {
		return fmt.Errorf("close minute: %w", err)
	}
	return nil
}

// p95 returns the nearest-rank 95th percentile of values, or nil for none.
func p95(values []float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	slices.Sort(values)
	return &values[int(math.Ceil(0.95*float64(len(values))))-1]
}

// collect appends the single column of every row query returns to dest.
func collect[T any](ctx context.Context, tx *sql.Tx, dest *[]T, query string, args ...any) error {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var v T
		if err := rows.Scan(&v); err != nil {
			return err
		}
		*dest = append(*dest, v)
	}
	return rows.Err()
}

// programID returns the row id of p, creating the row on first sight and filling in a path
// an earlier instance could not reveal. Ids from tx go to staged, which the caller copies into
// the cache once tx commits.
func (db *DB) programID(ctx context.Context, tx *sql.Tx, p usage.Program, staged map[usage.ProgramKey]programRow) (int64, error) {
	key := p.Key()
	if row, ok := db.programIDs[key]; ok && (row.hasPath || p.Path == "") {
		return row.id, nil
	}
	var row programRow
	err := tx.QueryRowContext(ctx, `
INSERT INTO programs (name, name_key, service_group, path) VALUES (?, ?, ?, ?)
ON CONFLICT (name_key, service_group) DO UPDATE SET
	path = CASE WHEN programs.path = '' THEN excluded.path ELSE programs.path END
RETURNING id, path != ''`, p.Name, key.Name, key.ServiceGroup, p.Path).Scan(&row.id, &row.hasPath)
	if err != nil {
		return 0, fmt.Errorf("store program %s: %w", p.Label(), err)
	}
	staged[key] = row
	return row.id, nil
}

// Prune deletes what has aged past its retention window at now, then programs no row uses.
func (db *DB) Prune(ctx context.Context, now time.Time) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("prune: %w", err)
	}
	defer tx.Rollback()
	steps := []struct {
		query string
		age   time.Duration
	}{
		{"DELETE FROM system_seconds WHERE ts < ?", SecondsRetention},
		{"DELETE FROM system_minutes WHERE minute < ?", MinutesRetention},
		{"DELETE FROM program_minutes WHERE minute < ?", ProgramMinutesRetention},
	}
	for _, step := range steps {
		if _, err := tx.ExecContext(ctx, step.query, now.Add(-step.age).UnixMilli()); err != nil {
			return fmt.Errorf("prune: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, db.unusedPrograms); err != nil {
		return fmt.Errorf("prune programs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("prune: %w", err)
	}
	clear(db.programIDs)
	return nil
}
