package store

import (
	"context"
	"fmt"
	"time"
)

// TimelinePoint is the machine's average usage over one bucket of a timeline.
type TimelinePoint struct {
	Start      time.Time
	Samples    int64
	CPUPercent float64
	RAMPercent float64
}

// Timeline averages machine CPU and RAM over buckets of step from from up to to, oldest first.
// A bucket with no sample is left out, so the caller sees where the collector did not run.
// A step under a minute reads the 1 s samples, which cover the last 7 days; a longer one reads
// the minute rollups, and from must then start on a minute.
func (db *DB) Timeline(ctx context.Context, from, to time.Time, step time.Duration) ([]TimelinePoint, error) {
	stepMs, fromMs := step.Milliseconds(), from.UnixMilli()
	// A second belongs to the bucket its interval ends in; a minute to the one it starts in.
	query := `
SELECT (ts - ? - 1) / ?, count(*), sum(cpu_pct * span_ms) / sum(span_ms), sum(ram_pct * span_ms) / sum(span_ms)
FROM system_seconds WHERE ts > ? AND ts <= ? GROUP BY 1 ORDER BY 1`
	if step >= time.Minute {
		query = `
SELECT (minute - ?) / ?, sum(samples), sum(cpu_sum) / sum(samples), sum(ram_sum) / sum(samples)
FROM system_minutes WHERE minute >= ? AND minute < ? AND samples > 0 GROUP BY 1 ORDER BY 1`
	}
	rows, err := db.sql.QueryContext(ctx, query, fromMs, stepMs, fromMs, to.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("read timeline: %w", err)
	}
	defer rows.Close()
	var points []TimelinePoint
	for rows.Next() {
		var bucket int64
		var p TimelinePoint
		if err := rows.Scan(&bucket, &p.Samples, &p.CPUPercent, &p.RAMPercent); err != nil {
			return nil, fmt.Errorf("read timeline: %w", err)
		}
		p.Start = from.Add(time.Duration(bucket) * step)
		points = append(points, p)
	}
	return points, rows.Err()
}

// EpisodeSecond is one machine sample stored with an episode.
type EpisodeSecond struct {
	End                 time.Time
	CPUPercent          float64
	RAMPercent          float64
	UnattributedPercent float64
}

// EpisodeSeconds returns every sample stored with episode id, from its pre-roll through the
// samples that cooled it down, oldest first.
func (db *DB) EpisodeSeconds(ctx context.Context, id int64) ([]EpisodeSecond, error) {
	rows, err := db.sql.QueryContext(ctx, `
SELECT ts, cpu_pct, ram_pct, unattributed_pct FROM episode_seconds WHERE episode_id = ? ORDER BY ts`, id)
	if err != nil {
		return nil, fmt.Errorf("read episode samples: %w", err)
	}
	defer rows.Close()
	var seconds []EpisodeSecond
	for rows.Next() {
		var s EpisodeSecond
		var ts int64
		if err := rows.Scan(&ts, &s.CPUPercent, &s.RAMPercent, &s.UnattributedPercent); err != nil {
			return nil, fmt.Errorf("read episode samples: %w", err)
		}
		s.End = time.UnixMilli(ts)
		seconds = append(seconds, s)
	}
	return seconds, rows.Err()
}
