package main_test

import (
	"bufio"
	"encoding/json"
	"os"
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

	last := snaps[len(snaps)-1]
	foundSelf, svchosts, grouped := false, 0, 0
	for _, p := range last.Processes {
		if strings.EqualFold(p.Path, perfwatchBin) {
			foundSelf = true
			if p.CPUTicks == 0 || p.PrivateBytes == 0 || p.Name != filepath.Base(perfwatchBin) {
				t.Errorf("the collector's own entry lacks CPU, private bytes or name: %+v", p)
			}
		}
		if strings.EqualFold(p.Name, "svchost.exe") {
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
