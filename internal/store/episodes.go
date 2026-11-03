package store

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"time"

	"github.com/YuukiFST/perfwatch/internal/usage"
)

// EpisodesRetention is how long an episode and its samples are kept after it ended.
const EpisodesRetention = 180 * 24 * time.Hour

// EpisodeSample is one second of an episode or its pre-roll: the machine and its busiest programs.
type EpisodeSample struct {
	End                 time.Time
	Span                time.Duration
	CapacityTicks       uint64
	CPUPercent          float64
	RAMPercent          float64
	UnattributedPercent float64
	Programs            []EpisodeProgram
	Processes           []EpisodeProcess
}

// EpisodeProgram is one program's usage over an episode sample.
type EpisodeProgram struct {
	Program      usage.Program
	CPUPercent   float64
	PrivateBytes uint64
	Instances    int
}

// EpisodeProcess is one process instance's usage over an episode sample.
type EpisodeProcess struct {
	PID          uint32
	CreateTime   int64
	Name         string
	CommandLine  string
	Ancestry     string // running parents, oldest first, as name[pid] joined by " > "
	CPUPercent   float64
	PrivateBytes uint64
}

type processKey struct {
	pid        uint32
	createTime int64
}

// stagedIDs holds the row ids a transaction created, for the caches once it commits.
type stagedIDs struct {
	programs  map[usage.ProgramKey]programRow
	processes map[processKey]int64
}

// StartEpisode stores an episode that started at start, with its pre-roll from preroll, and its
// first samples, in one transaction, and returns its id. lastWarm is the end of its newest sample
// that was not cool. An episode already stored with the same start (a replay) keeps its row.
func (db *DB) StartEpisode(ctx context.Context, preroll, start time.Time, trigger string, samples []EpisodeSample, lastWarm time.Time) (id int64, err error) {
	err = db.inTx(ctx, "store episode", func(tx *sql.Tx, staged stagedIDs) error {
		err := tx.QueryRowContext(ctx, `
INSERT INTO episodes (preroll_ts, start_ts, last_warm_ts, trigger) VALUES (?, ?, ?, ?)
ON CONFLICT (start_ts) DO UPDATE SET last_warm_ts = max(last_warm_ts, excluded.last_warm_ts)
RETURNING id`, preroll.UnixMilli(), start.UnixMilli(), lastWarm.UnixMilli(), trigger).Scan(&id)
		if err != nil {
			return err
		}
		return db.addEpisodeSamples(ctx, tx, staged, id, samples)
	})
	return id, err
}

// AddEpisodeSamples stores samples of episode id and moves its last warm sample to lastWarm, in
// one transaction; a sample already stored is kept.
func (db *DB) AddEpisodeSamples(ctx context.Context, id int64, samples []EpisodeSample, lastWarm time.Time) error {
	return db.inTx(ctx, "store episode samples", func(tx *sql.Tx, staged stagedIDs) error {
		_, err := tx.ExecContext(ctx, "UPDATE episodes SET last_warm_ts = max(last_warm_ts, ?) WHERE id = ?", lastWarm.UnixMilli(), id)
		if err != nil {
			return err
		}
		return db.addEpisodeSamples(ctx, tx, staged, id, samples)
	})
}

func (db *DB) addEpisodeSamples(ctx context.Context, tx *sql.Tx, staged stagedIDs, id int64, samples []EpisodeSample) error {
	for _, s := range samples {
		ts := s.End.UnixMilli()
		_, err := tx.ExecContext(ctx, `
INSERT INTO episode_seconds (episode_id, ts, span_ms, capacity_ticks, cpu_pct, ram_pct, unattributed_pct)
VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
			id, ts, s.Span.Milliseconds(), int64(s.CapacityTicks), s.CPUPercent, s.RAMPercent, s.UnattributedPercent)
		if err != nil {
			return err
		}
		for _, p := range s.Programs {
			programID, err := db.programID(ctx, tx, p.Program, staged.programs)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `
INSERT INTO episode_programs (episode_id, ts, program_id, cpu_pct, private_bytes, instances)
VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
				id, ts, programID, p.CPUPercent, int64(p.PrivateBytes), p.Instances)
			if err != nil {
				return err
			}
		}
		for _, p := range s.Processes {
			processID, err := db.processID(ctx, tx, p, staged.processes)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `
INSERT INTO episode_processes (episode_id, ts, process_id, cpu_pct, private_bytes)
VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
				id, ts, processID, p.CPUPercent, int64(p.PrivateBytes))
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// processID returns the row id of the instance p, creating the row on first sight and filling
// in a command line an earlier sample could not read.
func (db *DB) processID(ctx context.Context, tx *sql.Tx, p EpisodeProcess, staged map[processKey]int64) (int64, error) {
	key := processKey{p.PID, p.CreateTime}
	if id, ok := db.processIDs[key]; ok {
		return id, nil
	}
	if id, ok := staged[key]; ok {
		return id, nil
	}
	var id int64
	err := tx.QueryRowContext(ctx, `
INSERT INTO processes (pid, create_time, name, command_line, ancestry) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (pid, create_time) DO UPDATE SET
	command_line = CASE WHEN processes.command_line = '' THEN excluded.command_line ELSE processes.command_line END
RETURNING id`, p.PID, p.CreateTime, p.Name, p.CommandLine, p.Ancestry).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store process %s[%d]: %w", p.Name, p.PID, err)
	}
	staged[key] = id
	return id, nil
}

// inTx runs fn in a transaction and copies the program ids it staged into the cache only once
// the transaction commits.
func (db *DB) inTx(ctx context.Context, what string, fn func(tx *sql.Tx, staged stagedIDs) error) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	defer tx.Rollback()
	staged := stagedIDs{programs: map[usage.ProgramKey]programRow{}, processes: map[processKey]int64{}}
	if err := fn(tx, staged); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	maps.Copy(db.programIDs, staged.programs)
	maps.Copy(db.processIDs, staged.processes)
	return nil
}

// EndEpisode marks episode id as ended at end.
func (db *DB) EndEpisode(ctx context.Context, id int64, end time.Time) error {
	if _, err := db.sql.ExecContext(ctx, "UPDATE episodes SET end_ts = ? WHERE id = ?", end.UnixMilli(), id); err != nil {
		return fmt.Errorf("end episode: %w", err)
	}
	return nil
}

// FinishOpenEpisodes ends every episode a stopped collector left open at its last warm sample,
// where the detector would have ended it.
func (db *DB) FinishOpenEpisodes(ctx context.Context) error {
	if _, err := db.sql.ExecContext(ctx, "UPDATE episodes SET end_ts = last_warm_ts WHERE end_ts IS NULL"); err != nil {
		return fmt.Errorf("finish open episodes: %w", err)
	}
	return nil
}
