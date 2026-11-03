package store

import (
	"context"
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
	CPUPercent          float64
	RAMPercent          float64
	UnattributedPercent float64
	Programs            []EpisodeProgram
}

// EpisodeProgram is one program's usage over an episode sample.
type EpisodeProgram struct {
	Program      usage.Program
	CPUPercent   float64
	PrivateBytes uint64
	Instances    int
}

// StartEpisode stores an episode that started at start, with its pre-roll from preroll, and
// returns its id. An episode already stored with the same start (a replay) keeps its row.
func (db *DB) StartEpisode(ctx context.Context, preroll, start time.Time, trigger string) (id int64, err error) {
	err = db.sql.QueryRowContext(ctx, `
INSERT INTO episodes (preroll_ts, start_ts, trigger) VALUES (?, ?, ?)
ON CONFLICT (start_ts) DO UPDATE SET start_ts = excluded.start_ts
RETURNING id`, preroll.UnixMilli(), start.UnixMilli(), trigger).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store episode: %w", err)
	}
	return id, nil
}

// AddEpisodeSamples stores samples of episode id in one transaction; a sample already stored is kept.
func (db *DB) AddEpisodeSamples(ctx context.Context, id int64, samples []EpisodeSample) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store episode samples: %w", err)
	}
	defer tx.Rollback()
	staged := map[usage.ProgramKey]programRow{}
	for _, s := range samples {
		ts := s.End.UnixMilli()
		_, err := tx.ExecContext(ctx, `
INSERT INTO episode_seconds (episode_id, ts, span_ms, cpu_pct, ram_pct, unattributed_pct)
VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
			id, ts, s.Span.Milliseconds(), s.CPUPercent, s.RAMPercent, s.UnattributedPercent)
		if err != nil {
			return fmt.Errorf("store episode sample: %w", err)
		}
		for _, p := range s.Programs {
			programID, err := db.programID(ctx, tx, p.Program, staged)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `
INSERT INTO episode_programs (episode_id, ts, program_id, cpu_pct, private_bytes, instances)
VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
				id, ts, programID, p.CPUPercent, int64(p.PrivateBytes), p.Instances)
			if err != nil {
				return fmt.Errorf("store episode program: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store episode samples: %w", err)
	}
	maps.Copy(db.programIDs, staged)
	return nil
}

// EndEpisode marks episode id as ended at end.
func (db *DB) EndEpisode(ctx context.Context, id int64, end time.Time) error {
	if _, err := db.sql.ExecContext(ctx, "UPDATE episodes SET end_ts = ? WHERE id = ?", end.UnixMilli(), id); err != nil {
		return fmt.Errorf("end episode: %w", err)
	}
	return nil
}

// FinishOpenEpisodes ends every episode a stopped collector left open at its last stored sample.
func (db *DB) FinishOpenEpisodes(ctx context.Context) error {
	_, err := db.sql.ExecContext(ctx, `
UPDATE episodes SET end_ts = coalesce((SELECT max(ts) FROM episode_seconds WHERE episode_id = episodes.id), start_ts)
WHERE end_ts IS NULL`)
	if err != nil {
		return fmt.Errorf("finish open episodes: %w", err)
	}
	return nil
}
