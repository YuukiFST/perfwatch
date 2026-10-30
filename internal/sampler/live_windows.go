//go:build windows

package sampler

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// GetSystemTimes and GlobalMemoryStatusEx are not wrapped by golang.org/x/sys/windows.
// Signatures: https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-getsystemtimes
// and https://learn.microsoft.com/en-us/windows/win32/api/sysinfoapi/nf-sysinfoapi-globalmemorystatusex
var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemTimes       = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
)

// memoryStatusEx mirrors MEMORYSTATUSEX field for field:
// https://learn.microsoft.com/en-us/windows/win32/api/sysinfoapi/ns-sysinfoapi-memorystatusex
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// processInfoSize is the fixed part of one SYSTEM_PROCESS_INFORMATION entry.
const processInfoSize = int(unsafe.Sizeof(windows.SYSTEM_PROCESS_INFORMATION{}))

// Live samples the running Windows system once per interval.
type Live struct {
	interval   time.Duration
	ticker     *time.Ticker
	buf        []byte
	identities *identityCache
}

// NewLive returns a sampler whose first Next reads immediately and later ones wait for the interval.
func NewLive(interval time.Duration) (Sampler, error) {
	return &Live{interval: interval, buf: alignedBuffer(512 << 10), identities: newIdentityCache()}, nil
}

// Next waits for the next tick and reads processes, CPU times and memory.
func (l *Live) Next(ctx context.Context) (Snapshot, error) {
	if l.ticker == nil {
		l.ticker = time.NewTicker(l.interval)
	} else {
		select {
		case <-ctx.Done():
			return Snapshot{}, ctx.Err()
		case <-l.ticker.C:
		}
	}
	// Process and machine counters are subtracted from each other later, so read them back to
	// back and resolve identities (handles, service enumeration) only afterwards.
	if err := l.querySystemProcesses(); err != nil {
		return Snapshot{}, err
	}
	cpu, err := readCPUTimes()
	if err != nil {
		return Snapshot{}, err
	}
	now := time.Now()
	mem, err := readMemory()
	if err != nil {
		return Snapshot{}, err
	}
	procs, err := l.parseProcesses(now)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Time: now, CPU: cpu, Memory: mem, Processes: procs}, nil
}

// Close stops the ticker.
func (l *Live) Close() error {
	if l.ticker != nil {
		l.ticker.Stop()
	}
	return nil
}

// parseProcesses walks the SYSTEM_PROCESS_INFORMATION list in l.buf; the field layout is
// windows.SYSTEM_PROCESS_INFORMATION, which names the members Microsoft's winternl.h keeps as Reserved1.
func (l *Live) parseProcesses(now time.Time) ([]Process, error) {
	l.identities.beginPass(now)
	procs := make([]Process, 0, l.identities.lastCount)
	for off := 0; ; {
		if off+processInfoSize > len(l.buf) {
			return nil, fmt.Errorf("process entry at offset %d overruns the %d-byte buffer", off, len(l.buf))
		}
		info := (*windows.SYSTEM_PROCESS_INFORMATION)(unsafe.Pointer(&l.buf[off]))
		if pid := uint32(info.UniqueProcessID); pid != 0 {
			id := l.identities.lookup(pid, info.CreateTime, &info.ImageName)
			procs = append(procs, Process{
				PID:          pid,
				ParentPID:    uint32(info.InheritedFromUniqueProcessID),
				CreateTime:   info.CreateTime,
				Name:         id.name,
				Path:         id.path,
				ServiceGroup: id.serviceGroup,
				CPUTicks:     uint64(info.KernelTime) + uint64(info.UserTime),
				PrivateBytes: uint64(info.WorkingSetPrivateSize),
				CommitBytes:  uint64(info.PagefileUsage),
			})
		}
		if info.NextEntryOffset == 0 {
			break
		}
		off += int(info.NextEntryOffset)
	}
	l.identities.endPass(len(procs))
	return procs, nil
}

// querySystemProcesses fills l.buf, growing it while the process list does not fit.
func (l *Live) querySystemProcesses() error {
	for {
		var needed uint32
		err := windows.NtQuerySystemInformation(windows.SystemProcessInformation, unsafe.Pointer(&l.buf[0]), uint32(len(l.buf)), &needed)
		if err == nil {
			return nil
		}
		if !errors.Is(err, windows.STATUS_INFO_LENGTH_MISMATCH) {
			return fmt.Errorf("NtQuerySystemInformation(SystemProcessInformation): %w", err)
		}
		// Processes can start between two calls, so leave headroom over the reported size.
		l.buf = alignedBuffer(max(int(needed)+int(needed)/4, 2*len(l.buf)))
	}
}

func readCPUTimes() (CPUTimes, error) {
	var idle, kernel, user windows.Filetime
	ok, _, err := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user)))
	if ok == 0 {
		return CPUTimes{}, fmt.Errorf("GetSystemTimes: %w", err)
	}
	idleTicks := filetimeTicks(idle)
	// Kernel time includes idle time, per the GetSystemTimes documentation.
	busy := filetimeTicks(kernel) - idleTicks + filetimeTicks(user)
	return CPUTimes{IdleTicks: idleTicks, BusyTicks: busy}, nil
}

func readMemory() (Memory, error) {
	status := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	ok, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&status)))
	if ok == 0 {
		return Memory{}, fmt.Errorf("GlobalMemoryStatusEx: %w", err)
	}
	return Memory{TotalBytes: status.TotalPhys, AvailableBytes: status.AvailPhys}, nil
}

// alignedBuffer returns n bytes aligned for the pointer-sized fields Windows writes into
// output buffers; a plain []byte can land on the stack only 4-byte aligned.
func alignedBuffer(n int) []byte {
	words := make([]uint64, (n+7)/8)
	return unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), n)
}

func filetimeTicks(ft windows.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

var _ Sampler = (*Live)(nil)
