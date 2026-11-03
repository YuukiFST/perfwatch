package main_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestRunLogsItsOwnUsage runs the live collector and checks that perfwatch.log holds JSON lines
// for its start, its own CPU and working set, and its stop.
func TestRunLogsItsOwnUsage(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("live sampling is implemented on Windows only")
	}
	dataDir := t.TempDir()
	if res := runPerfwatch(t, []string{"PERFWATCH_DATA_DIR=" + dataDir}, "run", "--for", "3s"); res.exitCode != 0 {
		t.Fatalf("run failed:\n%s", res.transcript())
	}
	var msgs []string
	var usage map[string]any
	for _, line := range readLog(t, filepath.Join(dataDir, "perfwatch.log")) {
		msg, _ := line["msg"].(string)
		msgs = append(msgs, msg)
		if msg == "collector usage" {
			usage = line
		}
	}
	// A busy machine may add episode lines in between.
	if len(msgs) < 3 || msgs[0] != "collector started" || usage == nil || msgs[len(msgs)-1] != "collector stopped" {
		t.Fatalf("log messages = %q, want collector started first, collector stopped last and a collector usage line", msgs)
	}
	// A few seconds of an idle collector can round to 0 CPU ticks.
	cpu, ok := usage["cpu_percent_of_one_core"].(float64)
	working, _ := usage["working_set_max_bytes"].(float64)
	if !ok || cpu < 0 || cpu >= 100 || working < 1<<20 || working > 1<<30 {
		t.Errorf("implausible own usage: cpu %v%% of one core, working set %v bytes", cpu, working)
	}
}

// TestRunRotatesLog starts run with perfwatch.log already at the rotation size: the old content
// moves to perfwatch.log.1 and the new run logs to a fresh perfwatch.log.
func TestRunRotatesLog(t *testing.T) {
	dataDir := t.TempDir()
	logPath := filepath.Join(dataDir, "perfwatch.log")
	full := append(bytes.Repeat([]byte("x"), 5<<20-1), '\n')
	if err := os.WriteFile(logPath, full, 0o644); err != nil {
		t.Fatal(err)
	}
	if res := runPerfwatch(t, []string{"PERFWATCH_DATA_DIR=" + dataDir}, "run", "--replay", "testdata/fixtures/usage-basic.jsonl"); res.exitCode != 0 {
		t.Fatalf("run failed:\n%s", res.transcript())
	}
	rotated, err := os.ReadFile(logPath + ".1")
	if err != nil || !bytes.Equal(rotated, full) {
		t.Fatalf("perfwatch.log.1 should hold the old 5 MiB log: %d bytes, %v", len(rotated), err)
	}
	if lines := readLog(t, logPath); len(lines) == 0 || lines[0]["msg"] != "collector started" {
		t.Errorf("the new perfwatch.log should start with collector started, got %v", lines)
	}
}

// readLog decodes every line of a perfwatch.log; a line that is not JSON fails the test.
func readLog(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()
	var lines []map[string]any
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var line map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("log line is not JSON: %q", scanner.Text())
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read log: %v", err)
	}
	return lines
}
