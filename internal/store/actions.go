package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// AgentActionsRetention is how long a coding agent's tool call is kept after it ended.
const AgentActionsRetention = EpisodesRetention

// OpenActionWindow bounds the window of a tool call whose end never arrived: the agent was
// killed, or the call is still running.
const OpenActionWindow = 10 * time.Minute

// ActionPhase is the hook an agent action event came from.
type ActionPhase int

const (
	ActionStarted ActionPhase = iota // PreToolUse
	ActionEnded                      // PostToolUse
	ActionFailed                     // PostToolUseFailure
)

// ActionEvent is one hook call about a coding agent's tool call.
type ActionEvent struct {
	ToolUseID  string
	SessionID  string
	Tool       string
	Command    string
	Cwd        string
	Background bool
	Phase      ActionPhase
	At         time.Time
}

// RecordAction stores e. The start and end of one tool call may arrive in either order; each
// sets its own timestamp once, so a repeated hook changes nothing.
func (db *DB) RecordAction(ctx context.Context, e ActionEvent) error {
	var start, end sql.NullInt64
	if e.Phase == ActionStarted {
		start = sql.NullInt64{Int64: e.At.UnixMilli(), Valid: true}
	} else {
		end = sql.NullInt64{Int64: e.At.UnixMilli(), Valid: true}
	}
	_, err := db.sql.ExecContext(ctx, `
INSERT INTO agent_actions (tool_use_id, session_id, tool, command, cwd, background, start_ts, end_ts, failed)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (tool_use_id) DO UPDATE SET
	start_ts = coalesce(agent_actions.start_ts, excluded.start_ts),
	end_ts = coalesce(agent_actions.end_ts, excluded.end_ts),
	failed = max(agent_actions.failed, excluded.failed)`,
		e.ToolUseID, e.SessionID, e.Tool, e.Command, e.Cwd, e.Background, start, end, e.Phase == ActionFailed)
	if err != nil {
		return fmt.Errorf("store agent action: %w", err)
	}
	return nil
}

// AgentAction is one stored tool call with the machine's usage over its window.
type AgentAction struct {
	ToolUseID  string
	SessionID  string
	Tool       string
	Command    string
	Cwd        string
	Background bool
	Failed     bool
	Start      time.Time
	// End is when the tool call returned; Open calls have none, and their window runs to now or
	// OpenActionWindow after Start, whichever comes first.
	End  time.Time
	Open bool
	// Samples is how many 1 s machine samples overlap the window; 0 leaves the usage fields 0.
	Samples       int
	CPUAvgPercent float64
	CPUMaxPercent float64
	RAMMaxPercent float64
	// RAMGrowthBytes is the most RAM in use during the window minus the RAM in use at its start;
	// negative when usage only fell.
	RAMGrowthBytes int64
	// EpisodeID is the first peak episode the window overlaps, 0 for none.
	EpisodeID int64
}

// AgentActions returns the tool calls whose window overlaps [from, to], oldest first, each with
// the machine's usage over it; now closes the window of open calls.
func (db *DB) AgentActions(ctx context.Context, from, to, now time.Time) ([]AgentAction, error) {
	rows, err := db.sql.QueryContext(ctx, `
SELECT tool_use_id, session_id, tool, command, cwd, background, failed, coalesce(start_ts, end_ts), end_ts
FROM agent_actions
WHERE coalesce(start_ts, end_ts) <= ? AND coalesce(end_ts, start_ts + ?) >= ?
ORDER BY 8, tool_use_id`, to.UnixMilli(), OpenActionWindow.Milliseconds(), from.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("list agent actions: %w", err)
	}
	var actions []AgentAction
	for rows.Next() {
		var a AgentAction
		var start int64
		var end sql.NullInt64
		if err := rows.Scan(&a.ToolUseID, &a.SessionID, &a.Tool, &a.Command, &a.Cwd, &a.Background, &a.Failed, &start, &end); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list agent actions: %w", err)
		}
		a.Start = time.UnixMilli(start)
		if end.Valid {
			a.End = time.UnixMilli(end.Int64)
		} else {
			a.Open = true
			a.End = a.Start.Add(OpenActionWindow)
			if now.Before(a.End) {
				a.End = now
			}
		}
		actions = append(actions, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list agent actions: %w", err)
	}
	// The connection is free again only once rows is closed, so usage is read after.
	for i := range actions {
		if err := db.actionUsage(ctx, &actions[i]); err != nil {
			return nil, err
		}
	}
	return actions, nil
}

// actionUsage reads the machine samples that overlap a's window: a sample covers the span
// (ts - span_ms, ts].
func (db *DB) actionUsage(ctx context.Context, a *AgentAction) error {
	start, end := a.Start.UnixMilli(), a.End.UnixMilli()
	var ramMax sql.NullInt64
	err := db.sql.QueryRowContext(ctx, `
SELECT count(*), coalesce(avg(cpu_pct), 0), coalesce(max(cpu_pct), 0), coalesce(max(ram_pct), 0), max(ram_used_bytes)
FROM system_seconds WHERE ts > ? AND ts - span_ms < ?`, start, max(end, start+1)).Scan(
		&a.Samples, &a.CPUAvgPercent, &a.CPUMaxPercent, &a.RAMMaxPercent, &ramMax)
	if err != nil {
		return fmt.Errorf("read agent action usage: %w", err)
	}
	if ramMax.Valid {
		var before int64
		err := db.sql.QueryRowContext(ctx, "SELECT ram_used_bytes FROM system_seconds WHERE ts <= ? ORDER BY ts DESC LIMIT 1", start).Scan(&before)
		switch {
		case err == sql.ErrNoRows:
		case err != nil:
			return fmt.Errorf("read agent action usage: %w", err)
		default:
			a.RAMGrowthBytes = ramMax.Int64 - before
		}
	}
	err = db.sql.QueryRowContext(ctx, `
SELECT id FROM episodes WHERE start_ts <= ? AND coalesce(end_ts, last_warm_ts) >= ? ORDER BY start_ts LIMIT 1`, end, start).Scan(&a.EpisodeID)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("read agent action episode: %w", err)
	}
	return nil
}

// EpisodeProcessRank is one process instance's usage over an episode.
type EpisodeProcessRank struct {
	PID             uint32
	Name            string
	CommandLine     string
	Ancestry        string
	CPUAvgPercent   float64
	CPUMaxPercent   float64
	PrivateMaxBytes int64
}

// EpisodeProcesses returns the limit process instances of episode id with the highest average
// CPU share over its seconds, or with the most private RAM when byRAM.
func (db *DB) EpisodeProcesses(ctx context.Context, id int64, byRAM bool, limit int) ([]EpisodeProcessRank, error) {
	order := "5 DESC, 7 DESC"
	if byRAM {
		order = "7 DESC, 5 DESC"
	}
	rows, err := db.sql.QueryContext(ctx, `
SELECT pr.pid, pr.name, pr.command_line, pr.ancestry, sum(ep.cpu_pct) / n.seconds, max(ep.cpu_pct), max(ep.private_bytes)
FROM episode_processes ep JOIN episodes e ON e.id = ep.episode_id JOIN processes pr ON pr.id = ep.process_id,
	(SELECT count(*) AS seconds FROM episode_seconds s JOIN episodes e ON e.id = s.episode_id
	 WHERE s.episode_id = ? AND s.ts > e.start_ts AND s.ts <= coalesce(e.end_ts, e.last_warm_ts)) n
WHERE ep.episode_id = ? AND ep.ts > e.start_ts AND ep.ts <= coalesce(e.end_ts, e.last_warm_ts)
GROUP BY pr.id ORDER BY `+order+`, pr.pid LIMIT ?`, id, id, limit)
	if err != nil {
		return nil, fmt.Errorf("list episode processes: %w", err)
	}
	defer rows.Close()
	var ranks []EpisodeProcessRank
	for rows.Next() {
		var r EpisodeProcessRank
		var avg sql.NullFloat64
		if err := rows.Scan(&r.PID, &r.Name, &r.CommandLine, &r.Ancestry, &avg, &r.CPUMaxPercent, &r.PrivateMaxBytes); err != nil {
			return nil, fmt.Errorf("list episode processes: %w", err)
		}
		r.CPUAvgPercent = avg.Float64
		ranks = append(ranks, r)
	}
	return ranks, rows.Err()
}
