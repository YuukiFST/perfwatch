package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Ranking is program usage over a time range, busiest first.
type Ranking struct {
	// SampledSeconds is how many 1 s samples the range holds; less than its length when the
	// collector did not run all of it.
	SampledSeconds int64
	CapacityTicks  int64
	BusyTicks      int64
	// UnattributedTicks is busy time no sampled program accounts for; it is never dropped.
	UnattributedTicks int64
	Programs          []ProgramRank
}

// ProgramRank is one program's usage over a ranking's range.
type ProgramRank struct {
	Name            string
	ServiceGroup    string
	Path            string
	CPUTicks        int64
	CPUMaxPercent   float64
	PrivateMaxBytes int64
	InstancesMax    int
}

// RankPrograms ranks programs by CPU time over the minutes starting in [from, to). It reads
// the per-minute top 20s, so a program ranked out of a minute adds nothing for that minute.
func (db *DB) RankPrograms(ctx context.Context, from, to time.Time) (Ranking, error) {
	var r Ranking
	window := []any{from.UnixMilli(), to.UnixMilli()}
	err := db.sql.QueryRowContext(ctx, `
SELECT coalesce(sum(samples), 0), coalesce(sum(capacity_ticks), 0), coalesce(sum(busy_ticks), 0), coalesce(sum(unattributed_ticks), 0)
FROM system_minutes WHERE minute >= ? AND minute < ?`, window...).Scan(&r.SampledSeconds, &r.CapacityTicks, &r.BusyTicks, &r.UnattributedTicks)
	if err != nil {
		return Ranking{}, fmt.Errorf("rank programs: %w", err)
	}
	r.Programs, err = db.rankRows(ctx, `
SELECT p.name, p.service_group, p.path, sum(m.cpu_ticks), max(m.cpu_max_pct), max(m.private_max_bytes), max(m.instances_max)
FROM program_minutes m JOIN programs p ON p.id = m.program_id
WHERE m.minute >= ? AND m.minute < ?
GROUP BY p.id ORDER BY 4 DESC, p.name_key, p.service_group`, window)
	return r, err
}

// RankPeakPrograms ranks programs by CPU time over the episode seconds that end in (from, to],
// from each episode's start through its end; pre-roll and cool-down seconds are left out.
func (db *DB) RankPeakPrograms(ctx context.Context, from, to time.Time) (Ranking, error) {
	var r Ranking
	window := []any{from.UnixMilli(), to.UnixMilli()}
	const inPeak = `
FROM episode_seconds s JOIN episodes e ON e.id = s.episode_id
WHERE s.ts > e.start_ts AND s.ts <= coalesce(e.end_ts, e.last_warm_ts) AND s.ts > ? AND s.ts <= ?`
	err := db.sql.QueryRowContext(ctx, `
SELECT count(*), coalesce(sum(s.capacity_ticks), 0), coalesce(CAST(sum(round(s.cpu_pct * s.capacity_ticks / 100)) AS INTEGER), 0),
	coalesce(CAST(sum(round(s.unattributed_pct * s.capacity_ticks / 100)) AS INTEGER), 0)`+inPeak, window...).Scan(&r.SampledSeconds, &r.CapacityTicks, &r.BusyTicks, &r.UnattributedTicks)
	if err != nil {
		return Ranking{}, fmt.Errorf("rank peak programs: %w", err)
	}
	r.Programs, err = db.rankRows(ctx, `
SELECT p.name, p.service_group, p.path, CAST(sum(round(ep.cpu_pct * s.capacity_ticks / 100)) AS INTEGER), max(ep.cpu_pct), max(ep.private_bytes), max(ep.instances)
FROM episode_programs ep JOIN episode_seconds s ON s.episode_id = ep.episode_id AND s.ts = ep.ts
JOIN episodes e ON e.id = s.episode_id JOIN programs p ON p.id = ep.program_id
WHERE s.ts > e.start_ts AND s.ts <= coalesce(e.end_ts, e.last_warm_ts) AND s.ts > ? AND s.ts <= ?
GROUP BY p.id ORDER BY 4 DESC, p.name_key, p.service_group`, window)
	return r, err
}

func (db *DB) rankRows(ctx context.Context, query string, args []any) ([]ProgramRank, error) {
	rows, err := db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("rank programs: %w", err)
	}
	defer rows.Close()
	var ranks []ProgramRank
	for rows.Next() {
		var p ProgramRank
		if err := rows.Scan(&p.Name, &p.ServiceGroup, &p.Path, &p.CPUTicks, &p.CPUMaxPercent, &p.PrivateMaxBytes, &p.InstancesMax); err != nil {
			return nil, fmt.Errorf("rank programs: %w", err)
		}
		ranks = append(ranks, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rank programs: %w", err)
	}
	return ranks, nil
}

// Episode is a stored peak episode with its summary over [Start, End].
type Episode struct {
	PreRoll time.Time
	Start   time.Time
	// End is the last sample that was not cool; Open episodes have not ended yet and End is
	// their newest warm sample so far.
	End                    time.Time
	Open                   bool
	Trigger                string
	CPUMaxPercent          float64
	RAMMaxPercent          float64
	UnattributedAvgPercent float64
	Programs               []EpisodeRank
}

// EpisodeRank is one program's average and peak usage over an episode.
type EpisodeRank struct {
	Name            string
	ServiceGroup    string
	CPUAvgPercent   float64
	CPUMaxPercent   float64
	PrivateMaxBytes int64
}

// Episodes returns the episodes that overlap [from, to], newest first, each with its top
// programs by average CPU share.
func (db *DB) Episodes(ctx context.Context, from, to time.Time, programs int) ([]Episode, error) {
	rows, err := db.sql.QueryContext(ctx, `
SELECT e.id, e.preroll_ts, e.start_ts, coalesce(e.end_ts, e.last_warm_ts), e.end_ts IS NULL, e.trigger,
	coalesce(max(s.cpu_pct), 0), coalesce(max(s.ram_pct), 0), coalesce(avg(s.unattributed_pct), 0)
FROM episodes e LEFT JOIN episode_seconds s ON s.episode_id = e.id AND s.ts > e.start_ts AND s.ts <= coalesce(e.end_ts, e.last_warm_ts)
WHERE e.start_ts <= ? AND coalesce(e.end_ts, e.last_warm_ts) >= ?
GROUP BY e.id ORDER BY e.start_ts DESC`, to.UnixMilli(), from.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("list episodes: %w", err)
	}
	type row struct {
		id int64
		Episode
	}
	var found []row
	for rows.Next() {
		var r row
		var preroll, start, end int64
		if err := rows.Scan(&r.id, &preroll, &start, &end, &r.Open, &r.Trigger, &r.CPUMaxPercent, &r.RAMMaxPercent, &r.UnattributedAvgPercent); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list episodes: %w", err)
		}
		r.PreRoll, r.Start, r.End = time.UnixMilli(preroll), time.UnixMilli(start), time.UnixMilli(end)
		found = append(found, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list episodes: %w", err)
	}
	episodes := make([]Episode, len(found))
	for i, r := range found {
		if r.Programs, err = db.episodePrograms(ctx, r.id, programs); err != nil {
			return nil, err
		}
		episodes[i] = r.Episode
	}
	return episodes, nil
}

func (db *DB) episodePrograms(ctx context.Context, id int64, limit int) ([]EpisodeRank, error) {
	rows, err := db.sql.QueryContext(ctx, `
SELECT p.name, p.service_group, sum(ep.cpu_pct) / n.seconds, max(ep.cpu_pct), max(ep.private_bytes)
FROM episode_programs ep JOIN episodes e ON e.id = ep.episode_id JOIN programs p ON p.id = ep.program_id,
	(SELECT count(*) AS seconds FROM episode_seconds s JOIN episodes e ON e.id = s.episode_id
	 WHERE s.episode_id = ? AND s.ts > e.start_ts AND s.ts <= coalesce(e.end_ts, e.last_warm_ts)) n
WHERE ep.episode_id = ? AND ep.ts > e.start_ts AND ep.ts <= coalesce(e.end_ts, e.last_warm_ts)
GROUP BY p.id ORDER BY 3 DESC, p.name_key, p.service_group LIMIT ?`, id, id, limit)
	if err != nil {
		return nil, fmt.Errorf("list episode programs: %w", err)
	}
	defer rows.Close()
	var ranks []EpisodeRank
	for rows.Next() {
		var r EpisodeRank
		var avg sql.NullFloat64
		if err := rows.Scan(&r.Name, &r.ServiceGroup, &avg, &r.CPUMaxPercent, &r.PrivateMaxBytes); err != nil {
			return nil, fmt.Errorf("list episode programs: %w", err)
		}
		r.CPUAvgPercent = avg.Float64
		ranks = append(ranks, r)
	}
	return ranks, rows.Err()
}
