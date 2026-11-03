package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/YuukiFST/perfwatch/internal/sampler"
	"github.com/YuukiFST/perfwatch/internal/store"
)

type topJSON struct {
	From           time.Time    `json:"from"`
	To             time.Time    `json:"to"`
	PeaksOnly      bool         `json:"peaks_only"`
	SampledSeconds int64        `json:"sampled_seconds"`
	CPUAvgPercent  float64      `json:"cpu_avg_percent"`
	Unattributed   cpuShareJSON `json:"unattributed"`
	// OtherPrograms is busy CPU of programs not listed: cut by --limit, ranked out of a
	// minute's top 20 or an episode second's top 10, or in a minute not closed yet.
	OtherPrograms cpuShareJSON      `json:"other_programs"`
	Programs      []programRankJSON `json:"programs"`
}

type cpuShareJSON struct {
	CPUSeconds    float64 `json:"cpu_seconds"`
	CPUAvgPercent float64 `json:"cpu_avg_percent"`
}

type programRankJSON struct {
	Name            string  `json:"name"`
	ServiceGroup    string  `json:"service_group,omitempty"`
	Path            string  `json:"path,omitempty"`
	CPUSeconds      float64 `json:"cpu_seconds"`
	CPUAvgPercent   float64 `json:"cpu_avg_percent"`
	CPUMaxPercent   float64 `json:"cpu_max_percent"`
	PrivateMaxBytes int64   `json:"private_max_bytes"`
	InstancesMax    int     `json:"instances_max"`
}

func runTop(ctx context.Context, streams Streams, args []string) (err error) {
	fs := newFlagSet("top")
	since := &sinceFlag{24 * time.Hour}
	fs.Var(since, "since", "rank the usage of this much time back from now: 90m, 24h, 7d")
	peaksOnly := fs.Bool("peaks-only", false, "rank only the seconds inside peak episodes")
	asJSON := fs.Bool("json", false, "print JSON instead of a table")
	limit := fs.Int("limit", 20, "show at most this many programs")
	if err := parseFlags(fs, streams, args); err != nil {
		return err
	}
	if *limit < 1 {
		fmt.Fprintf(streams.Stderr, "invalid value %d for flag -limit: want 1 or more\n", *limit)
		fs.SetOutput(streams.Stderr)
		fs.Usage()
		return errUsage
	}
	now, loc, err := clock()
	if err != nil {
		return err
	}
	db, err := openForReading(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()

	out, err := rankTop(ctx, db, now.Add(-since.d), now, loc, *peaksOnly, *limit)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(streams.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	printTop(streams.Stdout, out)
	return nil
}

// rankTop ranks the programs over [from, to), or over the peak seconds in it, and lists the
// top limit of them with the CPU of the rest.
func rankTop(ctx context.Context, db *store.DB, from, to time.Time, loc *time.Location, peaksOnly bool, limit int) (topJSON, error) {
	// Programs are stored per minute, so the range starts on a minute in both modes.
	from = from.Truncate(time.Minute)
	rank := db.RankPrograms
	if peaksOnly {
		rank = db.RankPeakPrograms
	}
	r, err := rank(ctx, from, to)
	if err != nil {
		return topJSON{}, err
	}
	out := topJSON{
		From: from.In(loc), To: to.In(loc), PeaksOnly: peaksOnly, SampledSeconds: r.SampledSeconds,
		CPUAvgPercent: round2(percentOf(r.BusyTicks, r.CapacityTicks)),
		Unattributed:  cpuShare(r.UnattributedTicks, r.CapacityTicks),
		Programs:      []programRankJSON{},
	}
	listed := r.Programs[:min(limit, len(r.Programs))]
	other := r.BusyTicks - r.UnattributedTicks
	for _, p := range listed {
		other -= p.CPUTicks
	}
	// Per-program shares are rounded per second during peaks, so the remainder can dip below 0.
	out.OtherPrograms = cpuShare(max(other, 0), r.CapacityTicks)
	for _, p := range listed {
		out.Programs = append(out.Programs, programRankJSON{
			Name: p.Name, ServiceGroup: p.ServiceGroup, Path: p.Path,
			CPUSeconds:      round2(float64(p.CPUTicks) / sampler.TicksPerSecond),
			CPUAvgPercent:   round2(percentOf(p.CPUTicks, r.CapacityTicks)),
			CPUMaxPercent:   round2(p.CPUMaxPercent),
			PrivateMaxBytes: p.PrivateMaxBytes, InstancesMax: p.InstancesMax,
		})
	}
	return out, nil
}

func cpuShare(ticks, capacity int64) cpuShareJSON {
	return cpuShareJSON{
		CPUSeconds:    round2(float64(ticks) / sampler.TicksPerSecond),
		CPUAvgPercent: round2(percentOf(ticks, capacity)),
	}
}

// printTop writes the ranking as a table, with unattributed CPU and the programs not listed as
// rows of their own among the programs, where their CPU-seconds put them. Unattributed CPU is
// always a row; the programs not listed are one only when they used some CPU.
func printTop(w io.Writer, out topJSON) {
	scope := "Top programs"
	if out.PeaksOnly {
		scope = "Top programs during peak episodes"
	}
	fmt.Fprintf(w, "%s, %s to %s\n", scope, out.From.Format("2006-01-02 15:04"), out.To.Format("2006-01-02 15:04 MST"))
	if out.SampledSeconds == 0 {
		fmt.Fprintln(w, "No samples in this range.")
		return
	}
	fmt.Fprintf(w, "%s sampled, machine CPU %.1f%% on average\n\n", time.Duration(out.SampledSeconds)*time.Second, out.CPUAvgPercent)
	rows := [][]string{{"PROGRAM", "CPU-S", "AVG CPU", "MAX CPU", "MAX RAM", "INSTANCES"}}
	type namedShare struct {
		name string
		cpuShareJSON
	}
	shares := []namedShare{{"(unattributed)", out.Unattributed}}
	if other := (namedShare{"(other programs)", out.OtherPrograms}); other.CPUSeconds > out.Unattributed.CPUSeconds {
		shares = []namedShare{other, shares[0]}
	} else if other.CPUSeconds > 0 {
		shares = append(shares, other)
	}
	placeShares := func(above float64) {
		for len(shares) > 0 && shares[0].CPUSeconds > above {
			rows = append(rows, []string{shares[0].name, fmt.Sprintf("%.1f", shares[0].CPUSeconds), fmt.Sprintf("%.1f%%", shares[0].CPUAvgPercent), "", "", ""})
			shares = shares[1:]
		}
	}
	for _, p := range out.Programs {
		placeShares(p.CPUSeconds)
		rows = append(rows, []string{
			label(p.Name, p.ServiceGroup), fmt.Sprintf("%.1f", p.CPUSeconds), fmt.Sprintf("%.1f%%", p.CPUAvgPercent),
			fmt.Sprintf("%.1f%%", p.CPUMaxPercent), formatBytes(p.PrivateMaxBytes), fmt.Sprint(p.InstancesMax),
		})
	}
	placeShares(-1)
	printTable(w, rows)
}

// printTable aligns the first column to the start and every other column to the end.
func printTable(w io.Writer, rows [][]string) {
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			widths[i] = max(widths[i], len([]rune(cell)))
		}
	}
	for _, row := range rows {
		line := fmt.Sprintf("%-*s", widths[0], row[0])
		for i := 1; i < len(row); i++ {
			line += fmt.Sprintf("  %*s", widths[i], row[i])
		}
		fmt.Fprintln(w, strings.TrimRight(line, " "))
	}
}
