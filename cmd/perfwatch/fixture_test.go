package main_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fixtureCPUs is the logical CPU count of every generated fixture: one second of wall time
// adds 2 × 10,000,000 ticks of idle plus busy time.
const fixtureCPUs = 2

const (
	mib = uint64(1) << 20
	gib = uint64(1) << 30
)

type fixtureProcess struct {
	PID          uint32 `json:"pid"`
	ParentPID    uint32 `json:"parent_pid"`
	CreateTime   int64  `json:"create_time"`
	Name         string `json:"name"`
	Path         string `json:"path,omitempty"`
	ServiceGroup string `json:"service_group,omitempty"`
	CPUTicks     uint64 `json:"cpu_ticks"`
	PrivateBytes uint64 `json:"private_bytes"`
	CommitBytes  uint64 `json:"commit_bytes"`
}

type fixtureSnapshot struct {
	Time time.Time `json:"time"`
	CPU  struct {
		IdleTicks uint64 `json:"idle_ticks"`
		BusyTicks uint64 `json:"busy_ticks"`
	} `json:"cpu"`
	Memory struct {
		TotalBytes     uint64 `json:"total_bytes"`
		AvailableBytes uint64 `json:"available_bytes"`
	} `json:"memory"`
	Processes []fixtureProcess `json:"processes"`
}

// machine generates a replay fixture second by second: each step states how busy the machine
// was and which processes used that CPU, so expected percentages read straight off the test.
type machine struct {
	now    time.Time
	idle   uint64
	busy   uint64
	ramPct float64
	procs  []*fixtureProcess
	snaps  []fixtureSnapshot
}

func newMachine(start time.Time) *machine {
	m := &machine{now: start, idle: 1_000_000_000, busy: 500_000_000, ramPct: 50}
	m.snapshot()
	return m
}

// spawn adds a running process; it appears from the next snapshot on.
func (m *machine) spawn(p fixtureProcess) {
	if p.CreateTime == 0 {
		p.CreateTime = int64(p.PID)
	}
	m.procs = append(m.procs, &p)
}

// step advances the clock by d with the machine busyPct busy and each named process using its
// share of the whole machine, then records a snapshot.
func (m *machine) step(d time.Duration, busyPct float64, shares map[string]float64) {
	capacity := uint64(d.Seconds() * fixtureCPUs * 10_000_000)
	busy := uint64(float64(capacity) * busyPct / 100)
	m.now = m.now.Add(d)
	m.busy += busy
	m.idle += capacity - busy
	for _, p := range m.procs {
		p.CPUTicks += uint64(float64(capacity) * shares[p.Name] / 100)
	}
	m.snapshot()
}

// jump moves the clock without a snapshot in between, as when the collector was off.
func (m *machine) jump(d time.Duration) {
	m.now = m.now.Add(d)
	m.idle += uint64(d.Seconds() * fixtureCPUs * 10_000_000)
	m.snapshot()
}

func (m *machine) snapshot() {
	s := fixtureSnapshot{Time: m.now}
	s.CPU.IdleTicks, s.CPU.BusyTicks = m.idle, m.busy
	s.Memory.TotalBytes = 16 * gib
	s.Memory.AvailableBytes = uint64(float64(16*gib) * (100 - m.ramPct) / 100)
	for _, p := range m.procs {
		s.Processes = append(s.Processes, *p)
	}
	m.snaps = append(m.snaps, s)
}

// write saves the snapshots taken since the last write as a JSON Lines fixture in a test
// temp dir and returns its path; the next fixture continues the same machine.
func (m *machine) write(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, s := range m.snaps {
		if err := enc.Encode(s); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	m.snaps = nil
	return path
}
