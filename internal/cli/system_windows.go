package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

// serviceName is the Windows service install --service creates.
const serviceName = "perfwatch"

var (
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	// DWORD GetConsoleProcessList(LPDWORD lpdwProcessList, DWORD dwProcessCount);
	// https://learn.microsoft.com/en-us/windows/console/getconsoleprocesslist
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
	// BOOL FreeConsole(void); https://learn.microsoft.com/en-us/windows/console/freeconsole
	procFreeConsole = kernel32.NewProc("FreeConsole")
)

// detachLoneConsole lets go of the console when no other process shares it: the window Windows
// opened for a collector started at logon. "A console is closed when the last process attached
// to it terminates or calls FreeConsole." Run from a shell, the shell shares the console and it
// stays.
func detachLoneConsole() {
	var pids [2]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	if n == 1 {
		procFreeConsole.Call()
	}
}

// stopEventName names the event uninstall signals to stop the collector of dir. Global, so an
// elevated uninstall reaches a collector in another session too.
func stopEventName(dir string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(canonicalDir(dir))))
	return `Global\perfwatch-stop-` + hex.EncodeToString(sum[:8])
}

// canonicalDir spells dir as an absolute path with long names, not 8.3 short names such as
// DESENV~1, so every spelling of one directory names one collector.
func canonicalDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	p, err := windows.UTF16FromString(abs)
	if err != nil {
		return abs
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetLongPathName(&p[0], &buf[0], uint32(len(buf)))
	if err != nil || n == 0 || int(n) > len(buf) {
		return abs // the directory may not exist yet
	}
	return windows.UTF16ToString(buf[:n])
}

// watchStop returns a context canceled when the stop event of dir is signaled; unwatch frees it.
func watchStop(ctx context.Context, dir string) (context.Context, func(), error) {
	name, err := windows.UTF16PtrFromString(stopEventName(dir))
	if err != nil {
		return nil, nil, err
	}
	h, err := windows.CreateEvent(nil, 1, 0, name)
	if err != nil {
		return nil, nil, fmt.Errorf("create stop event: %w", err)
	}
	// A stop signaled for a collector that already exited is not meant for this one.
	if err := windows.ResetEvent(h); err != nil {
		windows.CloseHandle(h)
		return nil, nil, fmt.Errorf("reset stop event: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if ev, _ := windows.WaitForSingleObject(h, windows.INFINITE); ev == windows.WAIT_OBJECT_0 {
			cancel()
		}
	}()
	unwatch := func() {
		cancel()
		windows.SetEvent(h) // wakes the waiter, which ends
		<-done
		windows.CloseHandle(h)
	}
	return ctx, unwatch, nil
}

// signalStop asks the collector of dir to stop, and reports false when none is listening.
func signalStop(dir string) (bool, error) {
	name, err := windows.UTF16PtrFromString(stopEventName(dir))
	if err != nil {
		return false, err
	}
	h, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open stop event: %w", err)
	}
	defer windows.CloseHandle(h)
	return true, windows.SetEvent(h)
}

func inService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// runAsService runs collect under the service control manager, which stops it through ctx.
func runAsService(collect func(ctx context.Context) error) error {
	return svc.Run(serviceName, serviceHandler(collect))
}

type serviceHandler func(ctx context.Context) error

func (collect serviceHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- collect(ctx) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-done:
			if err != nil {
				return true, 1
			}
			return false, 0
		case r := <-requests:
			switch r.Cmd {
			case svc.Interrogate:
				status <- r.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
			}
		}
	}
}
