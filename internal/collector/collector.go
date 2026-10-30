// Package collector turns the stream of usage intervals into stored samples, per-minute
// rollups and hourly retention.
package collector

import (
	"cmp"
	"context"
	"math"
	"slices"
	"time"

	"github.com/YuukiFST/perfwatch/internal/store"
	"github.com/YuukiFST/perfwatch/internal/usage"
)

const (
	// MaxIntervalSpan is the longest interval kept as a sample. A longer one means the machine
	// slept or the collector stalled; its average says nothing about any moment, so it is a gap.
	MaxIntervalSpan = 5 * time.Second
	// PruneInterval is how often retention runs, in sample time.
	PruneInterval = time.Hour
	// TopPrograms is how many programs a minute keeps by CPU, and again by private RAM.
	TopPrograms = 20
)

// Sink is where the collector writes; *store.DB is the production one.
type Sink interface {
	AddSecond(ctx context.Context, s store.Second) (added bool, err error)
	AddMinute(ctx context.Context, m store.Minute) error
	Prune(ctx context.Context, now time.Time) error
}

// Collector accumulates the current minute in memory and writes each sample as it arrives.
type Collector struct {
	sink      Sink
	minute    *minuteRollup
	nextPrune time.Time
}

// New returns a collector writing to sink; retention runs on the first sample.
func New(sink Sink) *Collector {
	return &Collector{sink: sink}
}

// Record stores one interval and closes the previous minute when iv ends in a new one.
func (c *Collector) Record(ctx context.Context, iv usage.Interval) error {
	if iv.End.Sub(iv.Start) > MaxIntervalSpan {
		return nil
	}
	start := iv.End.Truncate(time.Minute)
	if c.minute != nil && !c.minute.start.Equal(start) {
		if err := c.flushMinute(ctx); err != nil {
			return err
		}
	}
	added, err := c.sink.AddSecond(ctx, store.Second{
		End:                 iv.End,
		Span:                iv.End.Sub(iv.Start),
		CPUPercent:          iv.CPUPercent,
		RAMPercent:          iv.RAMPercent,
		UnattributedPercent: iv.UnattributedPercent,
		RAMUsedBytes:        iv.RAMUsedBytes,
	})
	if err != nil {
		return err
	}
	if added {
		if c.minute == nil {
			c.minute = newMinuteRollup(start)
		}
		c.minute.add(iv)
	}
	if !iv.End.Before(c.nextPrune) {
		if err := c.sink.Prune(ctx, iv.End); err != nil {
			return err
		}
		c.nextPrune = iv.End.Add(PruneInterval)
	}
	return nil
}

// Close writes the partial minute in progress.
func (c *Collector) Close(ctx context.Context) error {
	return c.flushMinute(ctx)
}

func (c *Collector) flushMinute(ctx context.Context) error {
	if c.minute == nil {
		return nil
	}
	m := c.minute.build()
	c.minute = nil
	return c.sink.AddMinute(ctx, m)
}

type minuteRollup struct {
	start        time.Time
	cpu, ram     []float64
	capacity     uint64
	busy         uint64
	unattributed uint64
	programs     map[usage.ProgramKey]*store.ProgramMinute
}

func newMinuteRollup(start time.Time) *minuteRollup {
	return &minuteRollup{start: start, programs: map[usage.ProgramKey]*store.ProgramMinute{}}
}

func (r *minuteRollup) add(iv usage.Interval) {
	r.cpu = append(r.cpu, iv.CPUPercent)
	r.ram = append(r.ram, iv.RAMPercent)
	r.capacity += iv.CapacityTicks
	r.busy += iv.BusyTicks
	r.unattributed += iv.UnattributedTicks
	for _, p := range iv.Programs {
		pm := r.programs[p.Key()]
		if pm == nil {
			pm = &store.ProgramMinute{Program: p.Program}
			r.programs[p.Key()] = pm
		}
		if pm.Program.Path == "" {
			pm.Program.Path = p.Path
		}
		pm.CPUTicks += p.CPUTicks
		pm.CPUMaxPercent = max(pm.CPUMaxPercent, p.CPUPercent)
		pm.PrivateMaxBytes = max(pm.PrivateMaxBytes, p.PrivateBytes)
		pm.InstancesMax = max(pm.InstancesMax, p.Instances)
	}
}

func (r *minuteRollup) build() store.Minute {
	return store.Minute{
		Start:             r.start,
		Samples:           len(r.cpu),
		CapacityTicks:     r.capacity,
		BusyTicks:         r.busy,
		UnattributedTicks: r.unattributed,
		CPU:               summarize(r.cpu),
		RAM:               summarize(r.ram),
		Programs:          r.topPrograms(),
	}
}

// topPrograms keeps the TopPrograms programs with the most CPU time (none with zero) plus the
// TopPrograms with the most private RAM, so an idle but memory-heavy program still ranks.
func (r *minuteRollup) topPrograms() []store.ProgramMinute {
	all := make([]*store.ProgramMinute, 0, len(r.programs))
	for _, pm := range r.programs {
		all = append(all, pm)
	}
	byLabel := func(a, b *store.ProgramMinute) int { return cmp.Compare(a.Program.Label(), b.Program.Label()) }
	keep := map[*store.ProgramMinute]bool{}
	slices.SortFunc(all, func(a, b *store.ProgramMinute) int {
		return cmp.Or(cmp.Compare(b.CPUTicks, a.CPUTicks), byLabel(a, b))
	})
	for _, pm := range all[:min(TopPrograms, len(all))] {
		if pm.CPUTicks > 0 {
			keep[pm] = true
		}
	}
	slices.SortFunc(all, func(a, b *store.ProgramMinute) int {
		return cmp.Or(cmp.Compare(b.PrivateMaxBytes, a.PrivateMaxBytes), byLabel(a, b))
	})
	for _, pm := range all[:min(TopPrograms, len(all))] {
		keep[pm] = true
	}
	top := make([]store.ProgramMinute, 0, len(keep))
	for _, pm := range all {
		if keep[pm] {
			top = append(top, *pm)
		}
	}
	slices.SortFunc(top, func(a, b store.ProgramMinute) int { return cmp.Compare(a.Program.Label(), b.Program.Label()) })
	return top
}

// summarize returns the mean, maximum and nearest-rank 95th percentile of values.
func summarize(values []float64) store.Stats {
	if len(values) == 0 {
		return store.Stats{}
	}
	sorted := slices.Sorted(slices.Values(values))
	sum := 0.0
	for _, v := range sorted {
		sum += v
	}
	rank := int(math.Ceil(0.95*float64(len(sorted)))) - 1
	return store.Stats{Avg: sum / float64(len(sorted)), Max: sorted[len(sorted)-1], P95: sorted[rank]}
}
