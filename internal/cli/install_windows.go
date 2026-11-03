package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	// runKeyEnv points install at another key under HKCU; tests use a scratch key so they never
	// touch the real autostart list. Users leave it unset.
	runKeyEnv = "PERFWATCH_RUN_KEY"
	// defaultRunKey is the per-user autostart list Windows reads at logon; writing it needs no admin.
	defaultRunKey = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValueName  = "perfwatch"
	// collectorWait bounds how long install and uninstall wait for a collector to start or stop.
	collectorWait = 15 * time.Second
)

func runKey() string {
	if k := os.Getenv(runKeyEnv); k != "" {
		return k
	}
	return defaultRunKey
}

func runInstall(_ context.Context, streams Streams, args []string) error {
	fs := newFlagSet("install")
	service := fs.Bool("service", false, "install a Windows service that starts at boot instead (needs an elevated prompt)")
	if err := parseFlags(fs, streams, args); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	dir, err := dataDir()
	if err != nil {
		return err
	}
	dir = canonicalDir(dir)
	if *service {
		return installService(streams, exe, dir)
	}

	// The data directory goes into the command only when it is not the one a logon would find.
	runArgs := []string{"run"}
	if os.Getenv(dataDirEnv) != "" {
		runArgs = append(runArgs, "--data-dir", dir)
	}
	command := windows.EscapeArg(exe)
	for _, a := range runArgs {
		command += " " + windows.EscapeArg(a)
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey(), registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open HKCU\\%s: %w", runKey(), err)
	}
	defer k.Close()
	if err := k.SetStringValue(runValueName, command); err != nil {
		return fmt.Errorf("register autostart: %w", err)
	}
	fmt.Fprintf(streams.Stdout, "Autostart at logon: HKCU\\%s\\%s = %s\n", runKey(), runValueName, command)
	return startCollector(streams, exe, runArgs, dir)
}

// startCollector starts `perfwatch run` with no console, unless a collector already runs for dir.
func startCollector(streams Streams, exe string, runArgs []string, dir string) error {
	if collectorRunning(dir) {
		fmt.Fprintf(streams.Stdout, "The collector already runs for %s.\n", dir)
		return nil
	}
	cmd := exec.Command(exe, runArgs...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the collector: %w", err)
	}
	pid := cmd.Process.Pid
	cmd.Process.Release()
	if !waitFor(func() bool { return collectorRunning(dir) }) {
		return fmt.Errorf("the collector did not start within %s; see %s", collectorWait, filepath.Join(dir, logFileName))
	}
	fmt.Fprintf(streams.Stdout, "Collector started for %s (process %d).\n", dir, pid)
	return nil
}

// collectorRunning reports whether a collector listens on the stop event of dir, which it
// creates once it holds the data directory's lock and keeps until it exits.
func collectorRunning(dir string) bool {
	name, err := windows.UTF16PtrFromString(stopEventName(dir))
	if err != nil {
		return false
	}
	h, err := windows.OpenEvent(windows.SYNCHRONIZE, false, name)
	if err != nil {
		return false
	}
	windows.CloseHandle(h)
	return true
}

func waitFor(done func() bool) bool {
	for deadline := time.Now().Add(collectorWait); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if done() {
			return true
		}
	}
	return done()
}

func installService(streams Streams, exe, dir string) error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("install --service needs an elevated prompt: run it as administrator")
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to the service manager: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		// The service runs as LocalSystem, whose profile is not the user's: name the directory.
		s, err = m.CreateService(serviceName, exe, mgr.Config{
			DisplayName: "perfwatch",
			Description: "Records CPU and RAM usage and the processes behind each peak.",
			StartType:   mgr.StartAutomatic,
		}, "run", "--data-dir", dir)
		if err != nil {
			return fmt.Errorf("create the service: %w", err)
		}
		fmt.Fprintf(streams.Stdout, "Service %s installed: starts at boot, data in %s.\n", serviceName, dir)
	} else {
		fmt.Fprintf(streams.Stdout, "Service %s is already installed.\n", serviceName)
	}
	defer s.Close()
	if err := s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return fmt.Errorf("start the service: %w", err)
	}
	// One way to start is enough: the service replaces a logon autostart entry.
	if removed, err := removeAutostart(); err != nil {
		return err
	} else if removed {
		fmt.Fprintf(streams.Stdout, "Removed the logon autostart entry; the service starts the collector now.\n")
	}
	fmt.Fprintln(streams.Stdout, "Service started.")
	return nil
}

func removeAutostart() (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey(), registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open HKCU\\%s: %w", runKey(), err)
	}
	defer k.Close()
	err = k.DeleteValue(runValueName)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("remove autostart: %w", err)
	}
	return true, nil
}

func runUninstall(_ context.Context, streams Streams, args []string) error {
	fs := newFlagSet("uninstall")
	if err := parseFlags(fs, streams, args); err != nil {
		return err
	}
	dir, err := dataDir()
	if err != nil {
		return err
	}
	dir = canonicalDir(dir)
	removed, err := removeAutostart()
	if err != nil {
		return err
	}
	if removed {
		fmt.Fprintf(streams.Stdout, "Removed autostart: HKCU\\%s\\%s.\n", runKey(), runValueName)
	} else {
		fmt.Fprintln(streams.Stdout, "No autostart entry to remove.")
	}
	if err := uninstallService(streams); err != nil {
		return err
	}
	stopped, err := signalStop(dir)
	if err != nil {
		return err
	}
	switch {
	case !stopped:
		fmt.Fprintf(streams.Stdout, "No collector runs for %s.\n", dir)
	case waitFor(func() bool { return !collectorRunning(dir) }):
		fmt.Fprintln(streams.Stdout, "Collector stopped.")
	default:
		return fmt.Errorf("the collector did not stop within %s; see %s", collectorWait, filepath.Join(dir, logFileName))
	}
	fmt.Fprintf(streams.Stdout, "Data kept in %s.\n", dir)
	return nil
}

// uninstallService stops and deletes the service when one is installed; that needs admin.
func uninstallService(streams Streams) error {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return fmt.Errorf("connect to the service manager: %w", err)
	}
	name, _ := windows.UTF16PtrFromString(serviceName)
	h, err := windows.OpenService(scm, name, windows.SERVICE_QUERY_STATUS)
	windows.CloseServiceHandle(scm)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("look up the service: %w", err)
	}
	windows.CloseServiceHandle(h)
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("perfwatch is installed as a service: run uninstall from an elevated prompt")
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to the service manager: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("open the service: %w", err)
	}
	defer s.Close()
	if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
		return fmt.Errorf("stop the service: %w", err)
	}
	if !waitFor(func() bool {
		st, err := s.Query()
		return err == nil && st.State == svc.Stopped
	}) {
		return fmt.Errorf("the service did not stop within %s", collectorWait)
	}
	if err := s.Delete(); err != nil {
		return fmt.Errorf("delete the service: %w", err)
	}
	fmt.Fprintf(streams.Stdout, "Removed service %s.\n", serviceName)
	return nil
}
