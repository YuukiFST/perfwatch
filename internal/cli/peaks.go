package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/YuukiFST/perfwatch/internal/store"
)

// episodePrograms is how many programs each listed episode names.
const episodePrograms = 5

type peaksJSON struct {
	From     time.Time     `json:"from"`
	To       time.Time     `json:"to"`
	Episodes []episodeJSON `json:"episodes"`
}

type episodeJSON struct {
	ID                     int64                `json:"id"`
	Start                  time.Time            `json:"start"`
	End                    time.Time            `json:"end"`
	Open                   bool                 `json:"open"`
	Seconds                float64              `json:"seconds"`
	PreRollSeconds         float64              `json:"preroll_seconds"`
	Trigger                string               `json:"trigger"`
	CPUMaxPercent          float64              `json:"cpu_max_percent"`
	RAMMaxPercent          float64              `json:"ram_max_percent"`
	UnattributedAvgPercent float64              `json:"unattributed_avg_percent"`
	Programs               []episodeProgramJSON `json:"programs"`
}

type episodeProgramJSON struct {
	Name            string  `json:"name"`
	ServiceGroup    string  `json:"service_group,omitempty"`
	CPUAvgPercent   float64 `json:"cpu_avg_percent"`
	CPUMaxPercent   float64 `json:"cpu_max_percent"`
	PrivateMaxBytes int64   `json:"private_max_bytes"`
}

func runPeaks(ctx context.Context, streams Streams, args []string) (err error) {
	fs := newFlagSet("peaks")
	since := &sinceFlag{7 * 24 * time.Hour}
	fs.Var(since, "since", "list the episodes of this much time back from now: 90m, 24h, 7d")
	asJSON := fs.Bool("json", false, "print JSON instead of a list")
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
	episodes, err := db.Episodes(ctx, from, now, episodePrograms)
	if err != nil {
		return err
	}
	out := peaksJSON{From: from.In(loc), To: now.In(loc), Episodes: []episodeJSON{}}
	for _, e := range episodes {
		out.Episodes = append(out.Episodes, newEpisodeJSON(e, loc))
	}
	if *asJSON {
		enc := json.NewEncoder(streams.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	printPeaks(streams.Stdout, out)
	return nil
}

func newEpisodeJSON(e store.Episode, loc *time.Location) episodeJSON {
	ej := episodeJSON{
		ID: e.ID, Start: e.Start.In(loc), End: e.End.In(loc), Open: e.Open,
		Seconds: e.End.Sub(e.Start).Seconds(), PreRollSeconds: e.Start.Sub(e.PreRoll).Seconds(),
		Trigger: e.Trigger, CPUMaxPercent: round2(e.CPUMaxPercent), RAMMaxPercent: round2(e.RAMMaxPercent),
		UnattributedAvgPercent: round2(e.UnattributedAvgPercent), Programs: []episodeProgramJSON{},
	}
	for _, p := range e.Programs {
		ej.Programs = append(ej.Programs, episodeProgramJSON{
			Name: p.Name, ServiceGroup: p.ServiceGroup, CPUAvgPercent: round2(p.CPUAvgPercent),
			CPUMaxPercent: round2(p.CPUMaxPercent), PrivateMaxBytes: p.PrivateMaxBytes,
		})
	}
	return ej
}

// printPeaks writes one block per episode, newest first.
func printPeaks(w io.Writer, out peaksJSON) {
	fmt.Fprintf(w, "Peak episodes, %s to %s: %d\n", out.From.Format("2006-01-02 15:04"), out.To.Format("2006-01-02 15:04 MST"), len(out.Episodes))
	for _, e := range out.Episodes {
		end := e.End.Format("15:04:05")
		if e.Open {
			end += " (still open)"
		}
		fmt.Fprintf(w, "\n%s to %s, %s, %s: CPU max %.1f%%, RAM max %.1f%%, unattributed %.1f%% on average, %s pre-roll\n",
			e.Start.Format("2006-01-02 15:04:05"), end, time.Duration(e.Seconds*float64(time.Second)), triggerText(e.Trigger),
			e.CPUMaxPercent, e.RAMMaxPercent, e.UnattributedAvgPercent, time.Duration(e.PreRollSeconds*float64(time.Second)))
		rows := [][]string{{"  PROGRAM", "AVG CPU", "MAX CPU", "MAX RAM"}}
		for _, p := range e.Programs {
			rows = append(rows, []string{"  " + label(p.Name, p.ServiceGroup), fmt.Sprintf("%.1f%%", p.CPUAvgPercent),
				fmt.Sprintf("%.1f%%", p.CPUMaxPercent), formatBytes(p.PrivateMaxBytes)})
		}
		printTable(w, rows)
	}
}

func triggerText(trigger string) string {
	return strings.NewReplacer("cpu+ram", "CPU and RAM", "cpu", "CPU", "ram", "RAM").Replace(trigger) + " peak"
}
