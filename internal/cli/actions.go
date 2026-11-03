package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/YuukiFST/perfwatch/internal/store"
)

// actionCommandWidth is how many characters of a command a table row shows.
const actionCommandWidth = 100

type actionsJSON struct {
	From    time.Time    `json:"from"`
	To      time.Time    `json:"to"`
	Sort    string       `json:"sort"`
	Total   int          `json:"total"`
	Actions []actionJSON `json:"actions"`
}

type actionJSON struct {
	ToolUseID      string    `json:"tool_use_id"`
	SessionID      string    `json:"session_id"`
	Tool           string    `json:"tool"`
	Command        string    `json:"command"`
	Cwd            string    `json:"cwd,omitempty"`
	Background     bool      `json:"background"`
	Failed         bool      `json:"failed"`
	Start          time.Time `json:"start"`
	End            time.Time `json:"end"`
	Open           bool      `json:"open"`
	Seconds        float64   `json:"seconds"`
	Samples        int       `json:"samples"`
	CPUAvgPercent  float64   `json:"cpu_avg_percent"`
	CPUMaxPercent  float64   `json:"cpu_max_percent"`
	RAMMaxPercent  float64   `json:"ram_max_percent"`
	RAMGrowthBytes int64     `json:"ram_growth_bytes"`
	EpisodeID      int64     `json:"episode_id,omitempty"`
}

var actionSorts = map[string]func(a, b actionJSON) int{
	"time": func(a, b actionJSON) int { return b.Start.Compare(a.Start) },
	"cpu": func(a, b actionJSON) int {
		return cmp.Or(cmp.Compare(b.CPUMaxPercent, a.CPUMaxPercent), cmp.Compare(b.CPUAvgPercent, a.CPUAvgPercent))
	},
	"ram": func(a, b actionJSON) int {
		return cmp.Or(cmp.Compare(b.RAMGrowthBytes, a.RAMGrowthBytes), cmp.Compare(b.RAMMaxPercent, a.RAMMaxPercent))
	},
}

func runActions(ctx context.Context, streams Streams, args []string) (err error) {
	fs := newFlagSet("actions")
	since := &sinceFlag{24 * time.Hour}
	fs.Var(since, "since", "list the tool calls of this much time back from now: 90m, 24h, 7d")
	sortBy := fs.String("sort", "time", "order by time (newest first), cpu (highest machine CPU) or ram (most RAM growth)")
	limit := fs.Int("limit", 50, "show at most this many tool calls")
	asJSON := fs.Bool("json", false, "print JSON instead of a table")
	if err := parseFlags(fs, streams, args); err != nil {
		return err
	}
	order, ok := actionSorts[*sortBy]
	if !ok || *limit < 1 {
		fmt.Fprintf(streams.Stderr, "invalid -sort %q or -limit %d: want time, cpu or ram, and 1 or more\n", *sortBy, *limit)
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

	from := now.Add(-since.d)
	actions, err := db.AgentActions(ctx, from, now, now)
	if err != nil {
		return err
	}
	out := actionsJSON{From: from.In(loc), To: now.In(loc), Sort: *sortBy, Total: len(actions), Actions: []actionJSON{}}
	for _, a := range actions {
		out.Actions = append(out.Actions, newActionJSON(a, loc))
	}
	byTime := actionSorts["time"]
	slices.SortStableFunc(out.Actions, func(a, b actionJSON) int { return cmp.Or(order(a, b), byTime(a, b)) })
	out.Actions = out.Actions[:min(*limit, len(out.Actions))]
	if *asJSON {
		enc := json.NewEncoder(streams.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	printActions(streams.Stdout, out)
	return nil
}

func newActionJSON(a store.AgentAction, loc *time.Location) actionJSON {
	return actionJSON{
		ToolUseID: a.ToolUseID, SessionID: a.SessionID, Tool: a.Tool, Command: a.Command, Cwd: a.Cwd,
		Background: a.Background, Failed: a.Failed, Start: a.Start.In(loc), End: a.End.In(loc), Open: a.Open,
		Seconds: round2(a.End.Sub(a.Start).Seconds()), Samples: a.Samples,
		CPUAvgPercent: round2(a.CPUAvgPercent), CPUMaxPercent: round2(a.CPUMaxPercent),
		RAMMaxPercent: round2(a.RAMMaxPercent), RAMGrowthBytes: a.RAMGrowthBytes, EpisodeID: a.EpisodeID,
	}
}

func printActions(w io.Writer, out actionsJSON) {
	fmt.Fprintf(w, "Agent tool calls, %s to %s: %d", out.From.Format("2006-01-02 15:04"), out.To.Format("2006-01-02 15:04 MST"), out.Total)
	if len(out.Actions) < out.Total {
		fmt.Fprintf(w, ", the %d first by %s", len(out.Actions), out.Sort)
	}
	fmt.Fprintln(w)
	if len(out.Actions) == 0 {
		fmt.Fprintln(w, "No tool calls in this range: register perfwatch hook as a Claude Code hook to record them.")
		return
	}
	fmt.Fprintln(w)
	printActionTable(w, "", out.Actions)
}

// printActionTable writes one row per tool call, each cell prefixed by indent's width.
func printActionTable(w io.Writer, indent string, actions []actionJSON) {
	rows := [][]string{{indent + "START", "TIME", "AVG CPU", "MAX CPU", "MAX RAM", "RAM GROWTH", "PEAK", "ACTION"}}
	for _, a := range actions {
		row := []string{indent + a.Start.Format("2006-01-02 15:04:05"), actionDuration(a), "-", "-", "-", "-", "", ""}
		if a.Samples > 0 {
			row[2], row[3], row[4] = fmt.Sprintf("%.1f%%", a.CPUAvgPercent), fmt.Sprintf("%.1f%%", a.CPUMaxPercent), fmt.Sprintf("%.1f%%", a.RAMMaxPercent)
			row[5] = formatSignedBytes(a.RAMGrowthBytes)
		}
		if a.EpisodeID != 0 {
			row[6] = fmt.Sprintf("#%d", a.EpisodeID)
		}
		row[7] = actionTool(a) + ": " + oneLine(a.Command, actionCommandWidth)
		rows = append(rows, row)
	}
	printTableLastLeft(w, rows)
}

func actionDuration(a actionJSON) string {
	if a.Open {
		return "open"
	}
	return (time.Duration(a.Seconds*10) * time.Second / 10).String()
}

func actionTool(a actionJSON) string {
	tool := a.Tool
	if a.Background {
		tool += " bg"
	}
	if a.Failed {
		tool += " failed"
	}
	return tool
}

// oneLine shows the line breaks of s as \n, collapses its runs of spaces and cuts it to width
// characters.
func oneLine(s string, width int) string {
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", ` \n `)), " ")
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	return string(r[:width-3]) + "..."
}

func formatSignedBytes(b int64) string {
	sign, size := "+", formatBytes(b)
	if b < 0 {
		sign, size = "-", formatBytes(-b)
	}
	if size == formatBytes(0) {
		return size
	}
	return sign + size
}

// printTableLastLeft is printTable with the last column aligned to the start, for free text.
func printTableLastLeft(w io.Writer, rows [][]string) {
	last := len(rows[0]) - 1
	head := make([][]string, len(rows))
	for i, row := range rows {
		head[i] = row[:last]
	}
	var b strings.Builder
	printTable(&b, head)
	lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	width := 0
	for _, line := range lines {
		width = max(width, len([]rune(line)))
	}
	for i, line := range lines {
		fmt.Fprintln(w, strings.TrimRight(fmt.Sprintf("%-*s  %s", width, line, rows[i][last]), " "))
	}
}
