package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/YuukiFST/perfwatch/internal/store"
)

// maxHookInput bounds what hook reads from stdin; a Write call carries a whole file.
const maxHookInput = 8 << 20

// maxActionCommand bounds the command stored for one tool call, in bytes.
const maxActionCommand = 8 << 10

// hookInput is the part of a Claude Code hook's stdin JSON that hook reads:
// https://code.claude.com/docs/en/hooks
type hookInput struct {
	HookEventName string          `json:"hook_event_name"`
	SessionID     string          `json:"session_id"`
	Cwd           string          `json:"cwd"`
	ToolName      string          `json:"tool_name"`
	ToolUseID     string          `json:"tool_use_id"`
	ToolInput     json.RawMessage `json:"tool_input"`
}

var hookPhases = map[string]store.ActionPhase{
	"PreToolUse":         store.ActionStarted,
	"PostToolUse":        store.ActionEnded,
	"PostToolUseFailure": store.ActionFailed,
}

func runHook(ctx context.Context, streams Streams, args []string) (err error) {
	fs := newFlagSet("hook")
	if err := parseFlags(fs, streams, args); err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(streams.Stdin, maxHookInput+1))
	if err != nil {
		return fmt.Errorf("read hook input: %w", err)
	}
	if len(raw) > maxHookInput {
		return fmt.Errorf("hook input is over %d bytes", maxHookInput)
	}
	event, err := parseHookInput(raw)
	if err != nil {
		return err
	}
	now, _, err := clock()
	if err != nil {
		return err
	}
	event.At = now
	dir, err := dataDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	db, err := store.Open(ctx, filepath.Join(dir, store.FileName))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	return db.RecordAction(ctx, event)
}

func parseHookInput(raw []byte) (store.ActionEvent, error) {
	var in hookInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return store.ActionEvent{}, fmt.Errorf("hook input is not JSON: %w", err)
	}
	phase, ok := hookPhases[in.HookEventName]
	if !ok {
		return store.ActionEvent{}, fmt.Errorf("hook event %q: want PreToolUse, PostToolUse or PostToolUseFailure", in.HookEventName)
	}
	if in.ToolUseID == "" || in.ToolName == "" {
		return store.ActionEvent{}, fmt.Errorf("hook input has no tool_use_id or tool_name")
	}
	command, background := describeToolInput(in.ToolInput)
	return store.ActionEvent{
		ToolUseID: in.ToolUseID, SessionID: in.SessionID, Tool: in.ToolName, Command: command,
		Cwd: in.Cwd, Background: background, Phase: phase,
	}, nil
}

// describeToolInput returns what a tool call ran: the command of a shell tool, otherwise the
// whole input as compact JSON, cut to maxActionCommand bytes.
func describeToolInput(input json.RawMessage) (command string, background bool) {
	var shell struct {
		Command         *string `json:"command"`
		RunInBackground bool    `json:"run_in_background"`
	}
	if json.Unmarshal(input, &shell) == nil && shell.Command != nil {
		return truncateUTF8(*shell.Command, maxActionCommand), shell.RunInBackground
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, input); err != nil {
		return "", false
	}
	return truncateUTF8(compact.String(), maxActionCommand), false
}

// truncateUTF8 cuts s to at most n bytes without splitting a character.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
