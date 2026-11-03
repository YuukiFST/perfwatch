// Package usage turns two consecutive sampler snapshots into machine and per-program usage.
package usage

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/YuukiFST/perfwatch/internal/sampler"
)

// Program is what rankings sum instances under: one image name, compared without case, and
// for svchost.exe one service group. Path is the first path an instance revealed, since
// processes that deny access to an unelevated collector have none.
type Program struct {
	Name         string
	Path         string
	ServiceGroup string
}

// ProgramKey is what makes two instances the same Program.
type ProgramKey struct {
	Name         string // lower case
	ServiceGroup string
}

// Key returns the identity rankings sum p's instances under.
func (p Program) Key() ProgramKey {
	return ProgramKey{Name: strings.ToLower(p.Name), ServiceGroup: p.ServiceGroup}
}

// Label is the name a ranking shows: the image name, plus the service group of a svchost.exe.
func (p Program) Label() string {
	if p.ServiceGroup == "" {
		return p.Name
	}
	return p.Name + " (" + p.ServiceGroup + ")"
}

// ProgramUsage sums every instance of one Program over an interval.
type ProgramUsage struct {
	Program
	Instances    int
	CPUTicks     uint64
	CPUPercent   float64 // share of the whole machine: 100 means every logical CPU was busy
	PrivateBytes uint64  // at the end of the interval
	CommitBytes  uint64  // at the end of the interval
}

// ProcessUsage is one process instance over an interval.
type ProcessUsage struct {
	PID          uint32
	ParentPID    uint32
	CreateTime   int64
	Name         string
	CommandLine  string
	CPUTicks     uint64
	CPUPercent   float64 // share of the whole machine
	PrivateBytes uint64  // at the end of the interval
}

// Interval is the usage between two snapshots.
type Interval struct {
	Start time.Time
	End   time.Time
	// CapacityTicks is idle plus busy time: the wall time times the number of logical CPUs.
	CapacityTicks uint64
	BusyTicks     uint64
	// UnattributedTicks is busy time no sampled process accounts for: interrupts, DPCs,
	// processes that started and exited between the two snapshots, and the last partial
	// interval of every process that exited before cur.
	UnattributedTicks   uint64
	CPUPercent          float64
	UnattributedPercent float64
	RAMPercent          float64
	RAMUsedBytes        uint64
	Programs            []ProgramUsage // CPU descending, then label
	Processes           []ProcessUsage // every instance in cur, in snapshot order
	byPID               map[uint32]int // index into Processes
}

type instance struct {
	pid        uint32
	createTime int64
}

// Between returns the usage from prev to cur. ok is false when cur is not later than prev
// or a machine counter went backwards, so no interval can be measured.
func Between(prev, cur sampler.Snapshot) (iv Interval, ok bool) {
	if !cur.Time.After(prev.Time) || cur.CPU.IdleTicks < prev.CPU.IdleTicks || cur.CPU.BusyTicks < prev.CPU.BusyTicks {
		return Interval{}, false
	}
	busy := cur.CPU.BusyTicks - prev.CPU.BusyTicks
	capacity := busy + cur.CPU.IdleTicks - prev.CPU.IdleTicks
	if capacity == 0 {
		return Interval{}, false
	}
	programs, processes, attributed := sumPrograms(prev.Processes, cur.Processes)
	for i := range programs {
		programs[i].CPUPercent = percent(programs[i].CPUTicks, capacity)
	}
	byPID := make(map[uint32]int, len(processes))
	for i := range processes {
		processes[i].CPUPercent = percent(processes[i].CPUTicks, capacity)
		byPID[processes[i].PID] = i
	}
	unattributed := uint64(0)
	if busy > attributed {
		unattributed = busy - attributed
	}
	used := cur.Memory.TotalBytes - cur.Memory.AvailableBytes
	return Interval{
		Start:               prev.Time,
		End:                 cur.Time,
		CapacityTicks:       capacity,
		BusyTicks:           busy,
		UnattributedTicks:   unattributed,
		CPUPercent:          percent(busy, capacity),
		UnattributedPercent: percent(unattributed, capacity),
		RAMPercent:          percent(used, cur.Memory.TotalBytes),
		RAMUsedBytes:        used,
		Programs:            programs,
		Processes:           processes,
		byPID:               byPID,
	}, true
}

// maxAncestors bounds the walk up the parent chain.
const maxAncestors = 16

// Ancestry names the running ancestors of Processes[i], oldest first, as name[pid] joined by
// " > ". A parent PID whose current owner started after the child names a reused PID, not the
// parent, so the walk stops there.
func (iv Interval) Ancestry(i int) string {
	var chain []string
	child := iv.Processes[i]
	for range maxAncestors {
		j, ok := iv.byPID[child.ParentPID]
		if !ok || child.ParentPID == child.PID {
			break
		}
		parent := iv.Processes[j]
		if parent.CreateTime > child.CreateTime {
			break
		}
		chain = append(chain, fmt.Sprintf("%s[%d]", parent.Name, parent.PID))
		child = parent
	}
	slices.Reverse(chain)
	return strings.Join(chain, " > ")
}

// sumPrograms groups the processes of cur by Program and lists each instance. A process absent
// from prev started after it, so all of its CPU time falls inside the interval.
func sumPrograms(prev, cur []sampler.Process) (programs []ProgramUsage, processes []ProcessUsage, attributed uint64) {
	before := make(map[instance]uint64, len(prev))
	for _, p := range prev {
		before[instance{p.PID, p.CreateTime}] = p.CPUTicks
	}
	index := map[ProgramKey]int{}
	processes = make([]ProcessUsage, 0, len(cur))
	for _, p := range cur {
		delta := p.CPUTicks
		if start, seen := before[instance{p.PID, p.CreateTime}]; seen {
			delta = 0
			if p.CPUTicks > start {
				delta = p.CPUTicks - start
			}
		}
		attributed += delta
		processes = append(processes, ProcessUsage{
			PID: p.PID, ParentPID: p.ParentPID, CreateTime: p.CreateTime, Name: p.Name,
			CommandLine: p.CommandLine, CPUTicks: delta, PrivateBytes: p.PrivateBytes,
		})
		program := Program{Name: p.Name, Path: p.Path, ServiceGroup: p.ServiceGroup}
		i, ok := index[program.Key()]
		if !ok {
			i = len(programs)
			index[program.Key()] = i
			programs = append(programs, ProgramUsage{Program: program})
		}
		if programs[i].Path == "" {
			programs[i].Path = p.Path
		}
		programs[i].Instances++
		programs[i].CPUTicks += delta
		programs[i].PrivateBytes += p.PrivateBytes
		programs[i].CommitBytes += p.CommitBytes
	}
	slices.SortFunc(programs, func(a, b ProgramUsage) int {
		if c := cmp.Compare(b.CPUTicks, a.CPUTicks); c != 0 {
			return c
		}
		return cmp.Compare(a.Label(), b.Label())
	})
	return programs, processes, attributed
}

func percent(part, whole uint64) float64 {
	if whole == 0 {
		return 0
	}
	return float64(part) * 100 / float64(whole)
}
