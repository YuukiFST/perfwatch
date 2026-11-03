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
)

type topJSON struct {
	From           time.Time         `json:"from"`
	To             time.Time         `json:"to"`
	PeaksOnly      bool              `json:"peaks_only"`
	SampledSeconds int64             `json:"sampled_seconds"`
	CPUAvgPercent  float64           `json:"cpu_avg_percent"`
	Unattributed   unattributedJSON  `json:"unattributed"`
	Programs       []programRankJSON `json:"programs"`
}

type unattributedJSON struct {
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
	now, loc, err := clock()
	if err != nil {
		return err
	}
	db, err := openForReading(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()

	from := now.Add(-since.d)
	rank := db.RankPrograms
	if *peaksOnly {
		rank = db.RankPeakPrograms
	}
	r, err := rank(ctx, from, now)
	if err != nil {
		return err
	}
	out := topJSON{
		From: from.In(loc), To: now.In(loc), PeaksOnly: *peaksOnly, SampledSeconds: r.SampledSeconds,
		CPUAvgPercent: round2(percentOf(r.BusyTicks, r.CapacityTicks)),
		Unattributed: unattributedJSON{
			CPUSeconds:    round2(float64(r.UnattributedTicks) / sampler.TicksPerSecond),
			CPUAvgPercent: round2(percentOf(r.UnattributedTicks, r.CapacityTicks)),
		},
		Programs: []programRankJSON{},
	}
	for _, p := range r.Programs[:min(*limit, len(r.Programs))] {
		out.Programs = append(out.Programs, programRankJSON{
			Name: p.Name, ServiceGroup: p.ServiceGroup, Path: p.Path,
			CPUSeconds:      round2(float64(p.CPUTicks) / sampler.TicksPerSecond),
			CPUAvgPercent:   round2(percentOf(p.CPUTicks, r.CapacityTicks)),
			CPUMaxPercent:   round2(p.CPUMaxPercent),
			PrivateMaxBytes: p.PrivateMaxBytes, InstancesMax: p.InstancesMax,
		})
	}
	if *asJSON {
		enc := json.NewEncoder(streams.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	printTop(streams.Stdout, out)
	return nil
}

// printTop writes the ranking as a table, with unattributed CPU as a row of its own among the
// programs, where its CPU-seconds put it.
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
	unattributed := []string{"(unattributed)", fmt.Sprintf("%.1f", out.Unattributed.CPUSeconds), fmt.Sprintf("%.1f%%", out.Unattributed.CPUAvgPercent), "", "", ""}
	placed := false
	for _, p := range out.Programs {
		if !placed && out.Unattributed.CPUSeconds > p.CPUSeconds {
			rows, placed = append(rows, unattributed), true
		}
		rows = append(rows, []string{
			label(p.Name, p.ServiceGroup), fmt.Sprintf("%.1f", p.CPUSeconds), fmt.Sprintf("%.1f%%", p.CPUAvgPercent),
			fmt.Sprintf("%.1f%%", p.CPUMaxPercent), formatBytes(p.PrivateMaxBytes), fmt.Sprint(p.InstancesMax),
		})
	}
	if !placed {
		rows = append(rows, unattributed)
	}
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
