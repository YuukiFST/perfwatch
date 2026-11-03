package main_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// hookEvent is the stdin JSON a Claude Code hook gets, reduced to what perfwatch hook reads.
type hookEvent struct {
	HookEventName string         `json:"hook_event_name"`
	SessionID     string         `json:"session_id"`
	Cwd           string         `json:"cwd"`
	ToolName      string         `json:"tool_name"`
	ToolUseID     string         `json:"tool_use_id"`
	ToolInput     map[string]any `json:"tool_input"`
}

// TestAgentActionsExplainPeaks answers what the user asked of perfwatch: "qual AÇÃO em si faz o
// uso da CPU bater no topo? Qual AÇÃO em si faz a ram estourar? [...] saber exatamente qual
// comando o agente rodou e o que era o comando que fez o uso aumentar drasticamente."
// An agent session records its tool calls through perfwatch hook while a replayed machine runs
// a build the agent started (claude.exe > bash.exe > node.exe) into a CPU peak that also grows
// RAM; actions and peaks must name that call, its command, and the process that burned the CPU.
func TestAgentActionsExplainPeaks(t *testing.T) {
	dataDir := t.TempDir()
	start := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	at := func(seconds float64) string {
		return start.Add(time.Duration(seconds * float64(time.Second))).Format(time.RFC3339Nano)
	}
	var log strings.Builder
	hook := func(seconds float64, event hookEvent) {
		t.Helper()
		event.SessionID, event.Cwd = "session-1", `C:\work\app`
		input, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		res := runPerfwatchWithInput(t, []string{"PERFWATCH_DATA_DIR=" + dataDir, "PERFWATCH_NOW=" + at(seconds)}, string(input), "hook")
		log.WriteString(strings.ReplaceAll(res.transcript(), dataDir, "<data dir>") + "\n")
	}
	read := map[string]any{"file_path": `C:\work\app\package.json`}
	build := map[string]any{"command": "cd C:/work/app && npm run build", "description": "Build the app"}
	listing := map[string]any{"command": "Get-ChildItem -Recurse C:\\missing"}
	devServer := map[string]any{"command": "npm run dev", "run_in_background": true}
	tests := map[string]any{"command": "npm test -- --watch=false\necho done"}
	hook(5, hookEvent{HookEventName: "PreToolUse", ToolName: "Read", ToolUseID: "toolu_read", ToolInput: read})
	hook(5.2, hookEvent{HookEventName: "PostToolUse", ToolName: "Read", ToolUseID: "toolu_read", ToolInput: read})
	hook(38, hookEvent{HookEventName: "PreToolUse", ToolName: "Bash", ToolUseID: "toolu_build", ToolInput: build})
	hook(38, hookEvent{HookEventName: "PreToolUse", ToolName: "Bash", ToolUseID: "toolu_build", ToolInput: build}) // repeated: changes nothing
	hook(72, hookEvent{HookEventName: "PostToolUse", ToolName: "Bash", ToolUseID: "toolu_build", ToolInput: build})
	// The end of a call can arrive before its start: hooks run in parallel.
	hook(91, hookEvent{HookEventName: "PostToolUseFailure", ToolName: "PowerShell", ToolUseID: "toolu_listing", ToolInput: listing})
	hook(90, hookEvent{HookEventName: "PreToolUse", ToolName: "PowerShell", ToolUseID: "toolu_listing", ToolInput: listing})
	hook(92, hookEvent{HookEventName: "PreToolUse", ToolName: "Bash", ToolUseID: "toolu_dev", ToolInput: devServer})
	hook(92.1, hookEvent{HookEventName: "PostToolUse", ToolName: "Bash", ToolUseID: "toolu_dev", ToolInput: devServer})
	hook(95, hookEvent{HookEventName: "PreToolUse", ToolName: "Bash", ToolUseID: "toolu_tests", ToolInput: tests}) // never ends
	hook(96, hookEvent{HookEventName: "Stop"})
	hook(96, hookEvent{HookEventName: "PreToolUse", ToolName: "Bash"})
	for _, input := range []string{"not json", ""} {
		res := runPerfwatchWithInput(t, []string{"PERFWATCH_DATA_DIR=" + dataDir}, input, "hook")
		log.WriteString(res.transcript() + "\n")
	}

	m := newMachine(start)
	m.spawn(fixtureProcess{PID: 4, Name: "System", PrivateBytes: 1 * mib})
	m.spawn(fixtureProcess{PID: 100, ParentPID: 50, Name: "claude.exe", CommandLine: `"C:\npm\claude.exe"`, PrivateBytes: 900 * mib})
	m.spawn(fixtureProcess{PID: 600, Name: "brave.exe", CommandLine: `"C:\Brave\brave.exe" --type=renderer`, PrivateBytes: 2 * 1024 * mib})
	shares := map[string]float64{"System": 1, "claude.exe": 3, "brave.exe": 2}
	steps := func(n int, busy, ram float64) {
		m.ramPct = ram
		for range n {
			m.step(time.Second, busy, shares)
		}
	}
	steps(40, 20, 50)
	m.spawn(fixtureProcess{PID: 200, ParentPID: 100, CreateTime: 150, Name: "bash.exe", CommandLine: `bash.exe -c "cd C:/work/app && npm run build"`, PrivateBytes: 5 * mib})
	m.spawn(fixtureProcess{PID: 300, ParentPID: 200, CreateTime: 250, Name: "node.exe", CommandLine: `node C:\work\app\node_modules\vite\bin\vite.js build`, PrivateBytes: 1536 * mib})
	// A parent PID now owned by a process that started after the child is a reused PID: no parent.
	m.spawn(fixtureProcess{PID: 400, ParentPID: 999, CreateTime: 260, Name: "conhost.exe", PrivateBytes: 2 * mib})
	m.spawn(fixtureProcess{PID: 999, CreateTime: 270, Name: "late.exe", PrivateBytes: 1 * mib})
	shares["node.exe"], shares["bash.exe"], shares["conhost.exe"] = 80, 0.5, 0.7
	steps(30, 95, 80)
	shares["node.exe"] = 0
	m.exit(300)
	m.exit(200)
	steps(30, 20, 50)
	if res := runPerfwatch(t, []string{"PERFWATCH_DATA_DIR=" + dataDir}, "run", "--replay", m.write(t, "agent")); res.exitCode != 0 {
		t.Fatalf("run --replay failed:\n%s", res.transcript())
	}

	env := []string{"PERFWATCH_DATA_DIR=" + dataDir, "PERFWATCH_NOW=2026-03-02T09:05:00Z"}
	var b strings.Builder
	b.WriteString(log.String())
	for _, args := range [][]string{
		{"actions"},
		{"actions", "--sort", "cpu", "--limit", "2"},
		{"actions", "--sort", "ram", "--json", "--limit", "1"},
		{"actions", "--since", "1m"},
		{"actions", "--sort", "disk"},
		{"peaks"},
		{"peaks", "--json"},
	} {
		b.WriteString(strings.ReplaceAll(runPerfwatch(t, env, args...).transcript(), dataDir, "<data dir>") + "\n")
	}
	assertGolden(t, "agent-actions", b.String())
}
