package collector

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/YuukiFST/perfwatch/internal/config"
	"github.com/YuukiFST/perfwatch/internal/store"
	"github.com/YuukiFST/perfwatch/internal/usage"
)

// EpisodePrograms is how many programs an episode sample keeps by CPU, and again by private RAM.
const EpisodePrograms = 10

// detector finds peak episodes in the stream of samples. Before an episode it keeps a ring of
// the last PreRoll of samples, which becomes the episode's pre-roll when one starts.
type detector struct {
	cfg  config.Peaks
	sink Sink
	log  *slog.Logger

	lastEnd     time.Time
	ring        []store.EpisodeSample
	hot         int // consecutive hot samples outside an episode
	streakStart time.Time

	active   bool
	id       int64
	start    time.Time
	cool     int       // consecutive cool samples inside the episode
	lastWarm time.Time // end of the newest sample that was not cool
}

func (d *detector) observe(ctx context.Context, iv usage.Interval) error {
	if !d.lastEnd.IsZero() && !iv.Start.Equal(d.lastEnd) {
		// The collector did not run in between: what came before says nothing about now.
		if err := d.end(ctx); err != nil {
			return err
		}
		d.ring, d.hot = nil, 0
	}
	d.lastEnd = iv.End
	s := episodeSample(iv)
	// The ring fills during an episode too, so one that starts soon after another still gets
	// its whole pre-roll, the earlier episode's cool-down included.
	d.ring = append(d.ring, s)
	if d.active {
		d.trimRing(iv.End)
		return d.extend(ctx, iv, s)
	}
	hot := iv.CPUPercent >= d.cfg.CPUStartPercent || iv.RAMPercent >= d.cfg.RAMStartPercent
	if !hot {
		d.hot = 0
	} else if d.hot++; d.hot == 1 {
		d.streakStart = iv.Start
	}
	if d.hot == 0 {
		d.trimRing(iv.End)
	} else {
		d.trimRing(d.streakStart)
	}
	if d.hot < d.cfg.StartSamples {
		return nil
	}
	return d.begin(ctx, iv)
}

// trimRing keeps the samples that end within PreRoll before from, and every sample after it.
func (d *detector) trimRing(from time.Time) {
	for len(d.ring) > 0 && !d.ring[0].End.After(from.Add(-d.cfg.PreRoll())) {
		d.ring = d.ring[1:]
	}
}

func (d *detector) begin(ctx context.Context, iv usage.Interval) error {
	preroll := d.ring[0].End.Add(-d.ring[0].Span)
	trigger := triggerOf(d.cfg, iv)
	id, err := d.sink.StartEpisode(ctx, preroll, d.streakStart, trigger)
	if err != nil {
		return err
	}
	if err := d.sink.AddEpisodeSamples(ctx, id, d.ring); err != nil {
		return err
	}
	d.log.Info("episode started", "start", d.streakStart, "trigger", trigger, "preroll_samples", len(d.ring)-d.hot)
	d.active, d.id, d.start, d.cool, d.lastWarm = true, id, d.streakStart, 0, iv.End
	d.hot = 0
	return nil
}

func (d *detector) extend(ctx context.Context, iv usage.Interval, s store.EpisodeSample) error {
	if err := d.sink.AddEpisodeSamples(ctx, d.id, []store.EpisodeSample{s}); err != nil {
		return err
	}
	if iv.CPUPercent < d.cfg.CPUEndPercent && iv.RAMPercent < d.cfg.RAMEndPercent {
		d.cool++
	} else {
		d.cool, d.lastWarm = 0, iv.End
	}
	if d.cool < d.cfg.EndSamples {
		return nil
	}
	return d.end(ctx)
}

// end closes the open episode, if any, at its last sample that was not cool.
func (d *detector) end(ctx context.Context) error {
	if !d.active {
		return nil
	}
	d.active = false
	if err := d.sink.EndEpisode(ctx, d.id, d.lastWarm); err != nil {
		return err
	}
	d.log.Info("episode ended", "start", d.start, "end", d.lastWarm, "seconds", d.lastWarm.Sub(d.start).Seconds())
	return nil
}

func triggerOf(cfg config.Peaks, iv usage.Interval) string {
	cpu, ram := iv.CPUPercent >= cfg.CPUStartPercent, iv.RAMPercent >= cfg.RAMStartPercent
	switch {
	case cpu && ram:
		return "cpu+ram"
	case ram:
		return "ram"
	default:
		return "cpu"
	}
}

// episodeSample keeps the EpisodePrograms programs with the most CPU (none with zero) plus the
// EpisodePrograms with the most private RAM.
func episodeSample(iv usage.Interval) store.EpisodeSample {
	s := store.EpisodeSample{
		End: iv.End, Span: iv.End.Sub(iv.Start),
		CPUPercent: iv.CPUPercent, RAMPercent: iv.RAMPercent, UnattributedPercent: iv.UnattributedPercent,
	}
	keep := make([]bool, len(iv.Programs))
	for i, p := range iv.Programs[:min(EpisodePrograms, len(iv.Programs))] { // CPU descending
		keep[i] = p.CPUTicks > 0
	}
	byRAM := make([]int, len(iv.Programs))
	for i := range byRAM {
		byRAM[i] = i
	}
	slices.SortStableFunc(byRAM, func(a, b int) int { return cmp.Compare(iv.Programs[b].PrivateBytes, iv.Programs[a].PrivateBytes) })
	for _, i := range byRAM[:min(EpisodePrograms, len(byRAM))] {
		keep[i] = true
	}
	for i, p := range iv.Programs {
		if keep[i] {
			s.Programs = append(s.Programs, store.EpisodeProgram{
				Program: p.Program, CPUPercent: p.CPUPercent, PrivateBytes: p.PrivateBytes, Instances: p.Instances,
			})
		}
	}
	return s
}
