package cli

import (
	"log/slog"
	"os"
	"time"

	"github.com/YuukiFST/perfwatch/internal/sampler"
)

// selfUsage logs the collector's own CPU and memory once per minute of samples, and when run
// stops, so the overhead budget can be checked from perfwatch.log.
type selfUsage struct {
	log        *slog.Logger
	pid        uint32
	createTime int64
	from       time.Time
	fromTicks  uint64
	last       time.Time
	lastTicks  uint64
	maxWorking uint64
	maxPrivate uint64
}

func newSelfUsage(log *slog.Logger) *selfUsage {
	return &selfUsage{log: log, pid: uint32(os.Getpid())}
}

// observe finds the collector in snap and logs the minute that snap closes.
func (u *selfUsage) observe(snap sampler.Snapshot) {
	i := u.find(snap.Processes)
	if i < 0 {
		return
	}
	p := snap.Processes[i]
	if u.from.IsZero() {
		u.createTime, u.from, u.fromTicks = p.CreateTime, snap.Time, p.CPUTicks
	}
	u.last, u.lastTicks = snap.Time, p.CPUTicks
	u.maxWorking = max(u.maxWorking, p.WorkingSetBytes)
	u.maxPrivate = max(u.maxPrivate, p.PrivateBytes)
	if !snap.Time.Truncate(time.Minute).Equal(u.from.Truncate(time.Minute)) {
		u.flush()
	}
}

func (u *selfUsage) find(procs []sampler.Process) int {
	for i, p := range procs {
		if p.PID == u.pid && (u.from.IsZero() || p.CreateTime == u.createTime) {
			return i
		}
	}
	return -1
}

// flush logs the usage since the last flush and starts the next period at the last snapshot.
func (u *selfUsage) flush() {
	wall := u.last.Sub(u.from)
	if wall <= 0 {
		return
	}
	cpuSeconds := float64(u.lastTicks-u.fromTicks) / sampler.TicksPerSecond
	u.log.Info("collector usage",
		"from", u.from, "to", u.last,
		"cpu_percent_of_one_core", cpuSeconds*100/wall.Seconds(),
		"working_set_max_bytes", u.maxWorking, "private_max_bytes", u.maxPrivate)
	u.from, u.fromTicks, u.maxWorking, u.maxPrivate = u.last, u.lastTicks, 0, 0
}
