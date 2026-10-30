package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/YuukiFST/perfwatch/internal/usage"
)

// Retention windows, counted back from the newest sample.
const (
	SecondsRetention        = 7 * 24 * time.Hour
	MinutesRetention        = 365 * 24 * time.Hour
	ProgramMinutesRetention = 90 * 24 * time.Hour
)

// Second is one machine sample: the interval ending at End.
type Second struct {
	End                 time.Time
	Span                time.Duration
	CPUPercent          float64
	RAMPercent          float64
	UnattributedPercent float64
	RAMUsedBytes        uint64
}

// Stats summarizes one metric over a minute.
type Stats struct {
	Avg float64
	Max float64
	P95 float64
}

// Minute is the rollup of the samples whose interval ended within one minute.
type Minute struct {
	Start             time.Time
	Samples           int
	CapacityTicks     uint64
	BusyTicks         uint64
	UnattributedTicks uint64
	CPU               Stats
	RAM               Stats
	Programs          []ProgramMinute
}

// ProgramMinute is one program's usage over a minute.
type ProgramMinute struct {
	Program         usage.Program
	CPUTicks        uint64
	CPUMaxPercent   float64
	PrivateMaxBytes uint64
	InstancesMax    int
}

// AddSecond stores s and reports whether it was new; a sample already stored at the same
// end time is kept as it is, so replaying known data changes nothing.
func (db *DB) AddSecond(ctx context.Context, s Second) (added bool, err error) {
	res, err := db.sql.ExecContext(ctx, `
INSERT INTO system_seconds (ts, span_ms, cpu_pct, ram_pct, unattributed_pct, ram_used_bytes)
VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (ts) DO NOTHING`,
		s.End.UnixMilli(), s.Span.Milliseconds(), s.CPUPercent, s.RAMPercent, s.UnattributedPercent, int64(s.RAMUsedBytes))
	if err != nil {
		return false, fmt.Errorf("store second: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store second: %w", err)
	}
	return n == 1, nil
}

// AddMinute stores a minute rollup. A minute already present (the collector restarted within
// it) is merged: counts and ticks add up, averages are weighted, maxima and p95 take the larger.
func (db *DB) AddMinute(ctx context.Context, m Minute) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store minute: %w", err)
	}
	defer tx.Rollback()
	minute := m.Start.UnixMilli()
	_, err = tx.ExecContext(ctx, `
INSERT INTO system_minutes (minute, samples, capacity_ticks, busy_ticks, unattributed_ticks,
	cpu_avg, cpu_max, cpu_p95, ram_avg, ram_max, ram_p95)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (minute) DO UPDATE SET
	cpu_avg = (cpu_avg * samples + excluded.cpu_avg * excluded.samples) / (samples + excluded.samples),
	ram_avg = (ram_avg * samples + excluded.ram_avg * excluded.samples) / (samples + excluded.samples),
	samples = samples + excluded.samples,
	capacity_ticks = capacity_ticks + excluded.capacity_ticks,
	busy_ticks = busy_ticks + excluded.busy_ticks,
	unattributed_ticks = unattributed_ticks + excluded.unattributed_ticks,
	cpu_max = max(cpu_max, excluded.cpu_max), cpu_p95 = max(cpu_p95, excluded.cpu_p95),
	ram_max = max(ram_max, excluded.ram_max), ram_p95 = max(ram_p95, excluded.ram_p95)`,
		minute, m.Samples, int64(m.CapacityTicks), int64(m.BusyTicks), int64(m.UnattributedTicks),
		m.CPU.Avg, m.CPU.Max, m.CPU.P95, m.RAM.Avg, m.RAM.Max, m.RAM.P95)
	if err != nil {
		return fmt.Errorf("store minute: %w", err)
	}
	for _, p := range m.Programs {
		id, err := db.programID(ctx, tx, p.Program)
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
		return fmt.Errorf("store minute: %w", err)
	}
	return nil
}

// programID returns the row id of p, creating the row on first sight and filling in a path
// an earlier instance could not reveal.
func (db *DB) programID(ctx context.Context, tx *sql.Tx, p usage.Program) (int64, error) {
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
	db.programIDs[key] = row
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
	if _, err := tx.ExecContext(ctx, `
DELETE FROM programs WHERE NOT EXISTS (SELECT 1 FROM program_minutes WHERE program_id = programs.id)`); err != nil {
		return fmt.Errorf("prune programs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("prune: %w", err)
	}
	clear(db.programIDs)
	return nil
}
