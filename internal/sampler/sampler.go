// Package sampler reads cumulative CPU and memory counters for the machine and for each
// process. Usage over an interval is the difference between two snapshots; see package usage.
package sampler

import (
	"context"
	"time"
)

// TicksPerSecond is the unit of every CPU time in a Snapshot: 100 ns ticks, as Windows reports them.
const TicksPerSecond = 10_000_000

// Snapshot is one reading of cumulative counters.
type Snapshot struct {
	Time      time.Time `json:"time"`
	CPU       CPUTimes  `json:"cpu"`
	Memory    Memory    `json:"memory"`
	Processes []Process `json:"processes"`
}

// CPUTimes are machine-wide cumulative times in ticks, summed over every logical CPU,
// so idle plus busy grows by wall time times the number of logical CPUs.
type CPUTimes struct {
	IdleTicks uint64 `json:"idle_ticks"`
	BusyTicks uint64 `json:"busy_ticks"`
}

// Memory is the physical memory state when the snapshot was taken.
type Memory struct {
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
}

// Process is one running process; the idle pseudo-process is never included.
// PID and CreateTime together identify an instance, because PIDs are reused.
type Process struct {
	PID          uint32 `json:"pid"`
	ParentPID    uint32 `json:"parent_pid"`
	CreateTime   int64  `json:"create_time"` // opaque start stamp; FILETIME ticks on Windows
	Name         string `json:"name"`
	Path         string `json:"path,omitempty"`          // empty when the process denies access
	ServiceGroup string `json:"service_group,omitempty"` // the -k group of a svchost.exe instance
	CPUTicks     uint64 `json:"cpu_ticks"`               // kernel + user time, cumulative
	PrivateBytes uint64 `json:"private_bytes"`           // private working set
	CommitBytes  uint64 `json:"commit_bytes"`
}

// Sampler yields snapshots in time order.
type Sampler interface {
	// Next blocks until the next snapshot is due and returns it. A finite source ends with io.EOF.
	Next(ctx context.Context) (Snapshot, error)
	Close() error
}
