package cli

import (
	"context"
	"time"

	"github.com/YuukiFST/perfwatch/internal/config"
	"github.com/YuukiFST/perfwatch/internal/store"
)

type thresholdsJSON struct {
	CPUStartPercent float64 `json:"cpu_start_percent"`
	CPUEndPercent   float64 `json:"cpu_end_percent"`
	RAMStartPercent float64 `json:"ram_start_percent"`
	RAMEndPercent   float64 `json:"ram_end_percent"`
}

// timelineJSON holds one value per bucket in parallel arrays, as uPlot takes them: TS is each
// bucket's start in Unix seconds, and null in CPU and RAM marks a bucket with no sample.
type timelineJSON struct {
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
	StepSeconds float64   `json:"step_seconds"`
	// Samples and the averages cover the whole range, weighted by the samples in each bucket.
	Samples       int64          `json:"samples"`
	CPUAvgPercent float64        `json:"cpu_avg_percent"`
	RAMAvgPercent float64        `json:"ram_avg_percent"`
	Thresholds    thresholdsJSON `json:"thresholds"`
	TS            []int64        `json:"ts"`
	CPU           []*float64     `json:"cpu"`
	RAM           []*float64     `json:"ram"`
	Episodes      []episodeJSON  `json:"episodes"`
}

// buildTimeline averages machine CPU and RAM over [from, to) in at most maxTimelinePoints
// buckets, with the thresholds and the episodes in the range.
func buildTimeline(ctx context.Context, db *store.DB, peaks config.Peaks, from, to, now time.Time, loc *time.Location) (timelineJSON, error) {
	step := timelineStep(to.Sub(from))
	// The 1 s samples are kept for a week; older ranges come from the minute rollups.
	if step < time.Minute && from.Before(now.Add(-store.SecondsRetention)) {
		step = time.Minute
	}
	if step >= time.Minute {
		// Minute rollups start on the minute; the longer range may need the next step.
		from = from.Truncate(time.Minute)
		step = max(step, timelineStep(to.Sub(from)))
	}
	points, err := db.Timeline(ctx, from, to, step)
	if err != nil {
		return timelineJSON{}, err
	}
	episodes, err := db.Episodes(ctx, from, to, 0)
	if err != nil {
		return timelineJSON{}, err
	}
	out := timelineJSON{
		From: from.In(loc), To: to.In(loc), StepSeconds: step.Seconds(),
		Thresholds: thresholdsJSON{
			CPUStartPercent: peaks.CPUStartPercent, CPUEndPercent: peaks.CPUEndPercent,
			RAMStartPercent: peaks.RAMStartPercent, RAMEndPercent: peaks.RAMEndPercent,
		},
		Episodes: []episodeJSON{},
	}
	var cpuSum, ramSum float64
	for _, p := range points {
		out.Samples += p.Samples
		cpuSum += p.CPUPercent * float64(p.Samples)
		ramSum += p.RAMPercent * float64(p.Samples)
	}
	if out.Samples > 0 {
		out.CPUAvgPercent = round2(cpuSum / float64(out.Samples))
		out.RAMAvgPercent = round2(ramSum / float64(out.Samples))
	}
	next := 0
	for start := from; start.Before(to); start = start.Add(step) {
		out.TS = append(out.TS, start.Unix())
		var cpu, ram *float64
		if next < len(points) && points[next].Start.Equal(start) {
			c, m := round2(points[next].CPUPercent), round2(points[next].RAMPercent)
			cpu, ram = &c, &m
			next++
		}
		out.CPU, out.RAM = append(out.CPU, cpu), append(out.RAM, ram)
	}
	// Oldest first, as the timeline reads.
	for i := len(episodes) - 1; i >= 0; i-- {
		out.Episodes = append(out.Episodes, newEpisodeJSON(episodes[i], loc))
	}
	return out, nil
}

// timelineStep returns the shortest step that splits span into at most maxTimelinePoints buckets.
func timelineStep(span time.Duration) time.Duration {
	for _, s := range timelineSteps {
		if span <= s*maxTimelinePoints {
			return s
		}
	}
	return timelineSteps[len(timelineSteps)-1]
}

type episodeSecondsJSON struct {
	TS           []float64 `json:"ts"`
	CPU          []float64 `json:"cpu"`
	RAM          []float64 `json:"ram"`
	Unattributed []float64 `json:"unattributed"`
}

type episodeDetailJSON struct {
	Episode episodeJSON        `json:"episode"`
	Seconds episodeSecondsJSON `json:"seconds"`
}

// buildEpisodeDetail returns episode id with its top programs and every stored sample, and false
// when no episode has id.
func buildEpisodeDetail(ctx context.Context, db *store.DB, id int64, loc *time.Location) (episodeDetailJSON, bool, error) {
	e, found, err := db.EpisodeByID(ctx, id, episodeDetailPrograms)
	if err != nil || !found {
		return episodeDetailJSON{}, false, err
	}
	seconds, err := db.EpisodeSeconds(ctx, id)
	if err != nil {
		return episodeDetailJSON{}, false, err
	}
	// Each sample is drawn at the end of its interval, in Unix seconds as uPlot takes them.
	series := episodeSecondsJSON{TS: []float64{}, CPU: []float64{}, RAM: []float64{}, Unattributed: []float64{}}
	for _, s := range seconds {
		series.TS = append(series.TS, float64(s.End.UnixMilli())/1000)
		series.CPU = append(series.CPU, round2(s.CPUPercent))
		series.RAM = append(series.RAM, round2(s.RAMPercent))
		series.Unattributed = append(series.Unattributed, round2(s.UnattributedPercent))
	}
	return episodeDetailJSON{newEpisodeJSON(e, loc), series}, true, nil
}
