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

// episodeCPUProcesses and episodeRAMProcesses are how many process instances each listed
// episode names by CPU and by private RAM.
const (
	episodeCPUProcesses = 5
	episodeRAMProcesses = 3
)

// processCommandWidth is how many characters of a command line a peaks block shows.
const processCommandWidth = 160

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
	// Peaks fills the fields below; the dashboard leaves them out.
	CPUProcesses []episodeProcessJSON `json:"cpu_processes,omitempty"`
	RAMProcesses []episodeProcessJSON `json:"ram_processes,omitempty"`
	Actions      []actionJSON         `json:"agent_actions,omitempty"`
}

type episodeProcessJSON struct {
	PID             uint32  `json:"pid"`
	Name            string  `json:"name"`
	CommandLine     string  `json:"command_line"`
	Ancestry        string  `json:"ancestry"`
	CPUAvgPercent   float64 `json:"cpu_avg_percent"`
	CPUMaxPercent   float64 `json:"cpu_max_percent"`
	PrivateMaxBytes int64   `json:"private_max_bytes"`
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
		ej := newEpisodeJSON(e, loc)
		if err := addEpisodeCauses(ctx, db, &ej, e, now, loc); err != nil {
			return err
		}
		out.Episodes = append(out.Episodes, ej)
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

// addEpisodeCauses fills in the process instances and agent tool calls behind episode e: the
// calls whose window overlaps the episode, its pre-roll included.
func addEpisodeCauses(ctx context.Context, db *store.DB, ej *episodeJSON, e store.Episode, now time.Time, loc *time.Location) error {
	ej.CPUProcesses, ej.RAMProcesses, ej.Actions = []episodeProcessJSON{}, []episodeProcessJSON{}, []actionJSON{}
	for _, list := range []struct {
		dest  *[]episodeProcessJSON
		byRAM bool
		limit int
	}{{&ej.CPUProcesses, false, episodeCPUProcesses}, {&ej.RAMProcesses, true, episodeRAMProcesses}} {
		procs, err := db.EpisodeProcesses(ctx, e.ID, list.byRAM, list.limit)
		if err != nil {
			return err
		}
		for _, p := range procs {
			*list.dest = append(*list.dest, episodeProcessJSON{
				PID: p.PID, Name: p.Name, CommandLine: p.CommandLine, Ancestry: p.Ancestry,
				CPUAvgPercent: round2(p.CPUAvgPercent), CPUMaxPercent: round2(p.CPUMaxPercent), PrivateMaxBytes: p.PrivateMaxBytes,
			})
		}
	}
	actions, err := db.AgentActions(ctx, e.PreRoll, e.End, now)
	if err != nil {
		return err
	}
	for _, a := range actions {
		ej.Actions = append(ej.Actions, newActionJSON(a, loc))
	}
	return nil
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
		printEpisodeProcesses(w, "By CPU", e.CPUProcesses)
		printEpisodeProcesses(w, "By RAM", e.RAMProcesses)
		if len(e.Actions) > 0 {
			fmt.Fprintln(w, "  Agent tool calls during the episode and its pre-roll:")
			printActionTable(w, "    ", e.Actions)
		}
	}
}

// printEpisodeProcesses writes each process instance with its usage, then who started it and
// what it ran.
func printEpisodeProcesses(w io.Writer, title string, procs []episodeProcessJSON) {
	if len(procs) == 0 {
		return
	}
	fmt.Fprintf(w, "  %s, process by process:\n", title)
	for _, p := range procs {
		fmt.Fprintf(w, "    %s[%d]  avg CPU %.1f%%, max CPU %.1f%%, max RAM %s\n",
			p.Name, p.PID, p.CPUAvgPercent, p.CPUMaxPercent, formatBytes(p.PrivateMaxBytes))
		if p.Ancestry != "" {
			fmt.Fprintf(w, "      started by: %s\n", p.Ancestry)
		}
		if p.CommandLine != "" {
			fmt.Fprintf(w, "      command: %s\n", oneLine(p.CommandLine, processCommandWidth))
		}
	}
}

func triggerText(trigger string) string {
	return strings.NewReplacer("cpu+ram", "CPU and RAM", "cpu", "CPU", "ram", "RAM").Replace(trigger) + " peak"
}
