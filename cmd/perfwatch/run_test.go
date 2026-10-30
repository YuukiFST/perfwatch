package main_test

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestRunReplayPrintsUsage replays a fixture whose expected percentages are worked out in
// its comments: 2 logical CPUs, so 20,000,000 ticks of capacity per 1 s interval.
func TestRunReplayPrintsUsage(t *testing.T) {
	res := runPerfwatch(t, nil, "run", "--replay", "testdata/fixtures/usage-basic.jsonl", "--print")
	assertGolden(t, "run-replay-print", res.transcript())
}

// recordedSnapshot is the --record line format, decoded independently of the producer's types.
type recordedSnapshot struct {
	Time time.Time `json:"time"`
	CPU  struct {
		IdleTicks uint64 `json:"idle_ticks"`
		BusyTicks uint64 `json:"busy_ticks"`
	} `json:"cpu"`
	Memory struct {
		TotalBytes     uint64 `json:"total_bytes"`
		AvailableBytes uint64 `json:"available_bytes"`
	} `json:"memory"`
	Processes []struct {
		PID          uint32 `json:"pid"`
		CreateTime   int64  `json:"create_time"`
		Name         string `json:"name"`
		Path         string `json:"path"`
		ServiceGroup string `json:"service_group"`
		CPUTicks     uint64 `json:"cpu_ticks"`
		PrivateBytes uint64 `json:"private_bytes"`
	} `json:"processes"`
}

func TestRunRecordsLiveSnapshots(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("live sampling is implemented on Windows only")
	}
	recording := filepath.Join(t.TempDir(), "live.jsonl")
	if res := runPerfwatch(t, nil, "run", "--for", "2s", "--record", recording); res.exitCode != 0 {
		t.Fatalf("run --record failed:\n%s", res.transcript())
	}
	snaps := readRecording(t, recording)
	if len(snaps) < 3 {
		t.Fatalf("got %d snapshots in 2 s, want at least 3 (one per second plus the first)", len(snaps))
	}
	for i, s := range snaps {
		if s.Memory.TotalBytes == 0 || s.Memory.AvailableBytes > s.Memory.TotalBytes {
			t.Errorf("snapshot %d: implausible memory %+v", i, s.Memory)
		}
		if i == 0 {
			continue
		}
		prev := snaps[i-1]
		if !s.Time.After(prev.Time) || s.CPU.IdleTicks < prev.CPU.IdleTicks || s.CPU.BusyTicks < prev.CPU.BusyTicks {
			t.Errorf("snapshot %d: time or CPU counters went backwards", i)
		}
	}

	// A svchost.exe started during the run may not have reported its service yet; every one
	// that was already running at the first snapshot must carry its group.
	type instance struct {
		pid        uint32
		createTime int64
	}
	runningAtStart := map[instance]bool{}
	for _, p := range snaps[0].Processes {
		runningAtStart[instance{p.PID, p.CreateTime}] = true
	}
	last := snaps[len(snaps)-1]
	foundSelf, svchosts, grouped := false, 0, 0
	for _, p := range last.Processes {
		if strings.EqualFold(p.Path, perfwatchBin) {
			foundSelf = true
			if p.CPUTicks == 0 || p.PrivateBytes == 0 || p.Name != filepath.Base(perfwatchBin) {
				t.Errorf("the collector's own entry lacks CPU, private bytes or name: %+v", p)
			}
		}
		if strings.EqualFold(p.Name, "svchost.exe") && runningAtStart[instance{p.PID, p.CreateTime}] {
			svchosts++
			if p.ServiceGroup != "" {
				grouped++
			}
		}
	}
	if !foundSelf {
		t.Errorf("no process in the last snapshot has the collector's path %s", perfwatchBin)
	}
	if svchosts == 0 || grouped != svchosts {
		t.Errorf("%d of %d svchost.exe instances carry a service group, want all", grouped, svchosts)
	}

	res := runPerfwatch(t, nil, "run", "--replay", recording, "--print")
	lines := strings.Split(strings.TrimSuffix(res.stdout, "\n"), "\n")
	if res.exitCode != 0 || len(lines) != len(snaps)-1 {
		t.Fatalf("replaying %d recorded snapshots printed %d lines, want %d:\n%s", len(snaps), len(lines), len(snaps)-1, res.transcript())
	}
	for _, line := range lines {
		if !strings.Contains(line, "  cpu ") || !strings.Contains(line, "  unattributed ") {
			t.Errorf("replayed line lacks the usage fields: %q", line)
		}
	}
}

// TestRunAllowsOneCollectorPerDataDir starts a live collector, checks that a second run on the
// same data directory refuses to start, then kills the first one hard: its stored seconds must
// all be in their minute rollups, and the next run must start and close the minute it left open.
func TestRunAllowsOneCollectorPerDataDir(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("live sampling is implemented on Windows only")
	}
	dataDir := t.TempDir()
	env := []string{"PERFWATCH_DATA_DIR=" + dataDir}
	first := exec.Command(perfwatchBin, "run", "--print")
	first.Env = append(os.Environ(), env...)
	stdout, err := first.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Start(); err != nil {
		t.Fatalf("start the first collector: %v", err)
	}
	t.Cleanup(func() { first.Process.Kill(); first.Wait() })
	// The first --print line comes after the first sample is stored, so the lock is held by then.
	lines := bufio.NewScanner(stdout)
	if !lines.Scan() {
		t.Fatalf("the first collector printed no sample: %v", lines.Err())
	}
	go io.Copy(io.Discard, stdout)

	fixture := "testdata/fixtures/usage-basic.jsonl"
	second := runPerfwatch(t, env, "run", "--replay", fixture)
	assertGolden(t, "run-second-collector", strings.ReplaceAll(second.transcript(), dataDir, "<data dir>"))

	if err := first.Process.Kill(); err != nil {
		t.Fatalf("kill the first collector: %v", err)
	}
	first.Wait()
	db := openStore(t, filepath.Join(dataDir, "perfwatch.db"))
	var stored, rolledUp int
	err = db.QueryRow(`SELECT (SELECT count(*) FROM system_seconds),
		(SELECT coalesce(sum(samples), 0) FROM system_minutes)`).Scan(&stored, &rolledUp)
	if err != nil {
		t.Fatal(err)
	}
	if stored == 0 || rolledUp != stored {
		t.Errorf("after a hard kill: %d seconds stored, %d of them in minute rollups; want all of at least one", stored, rolledUp)
	}

	if res := runPerfwatch(t, env, "run", "--replay", fixture); res.exitCode != 0 {
		t.Fatalf("run after the hard kill failed:\n%s", res.transcript())
	}
	var open int
	if err := db.QueryRow("SELECT count(*) FROM system_minutes WHERE cpu_p95 IS NULL OR ram_p95 IS NULL").Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open != 0 {
		t.Errorf("%d minutes still open after the next run started, want 0", open)
	}
}

func readRecording(t *testing.T, path string) []recordedSnapshot {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open recording: %v", err)
	}
	defer f.Close()
	var snaps []recordedSnapshot
	scanner := bufio.NewScanner(f)
	scanner.Buffer(nil, 16<<20)
	for scanner.Scan() {
		var s recordedSnapshot
		if err := json.Unmarshal(scanner.Bytes(), &s); err != nil {
			t.Fatalf("recording line %d: %v", len(snaps)+1, err)
		}
		snaps = append(snaps, s)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read recording: %v", err)
	}
	return snaps
}
