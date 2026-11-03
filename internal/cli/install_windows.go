package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	// runKeyEnv points install at another key under HKCU; tests use a scratch key so they never
	// touch the real autostart list. Users leave it unset.
	runKeyEnv = "PERFWATCH_RUN_KEY"
	// serviceNameEnv names another service for install and uninstall; tests use a name no
	// service has, so they never stop or delete a real one. Users leave it unset.
	serviceNameEnv = "PERFWATCH_SERVICE"
	// defaultRunKey is the per-user autostart list Windows reads at logon; writing it needs no admin.
	defaultRunKey = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValueName  = "perfwatch"
	// collectorWait bounds how long install and uninstall wait for a collector or the service to
	// start or stop.
	collectorWait = 15 * time.Second
)

func runKey() string {
	if k := os.Getenv(runKeyEnv); k != "" {
		return k
	}
	return defaultRunKey
}

func serviceName() string {
	if n := os.Getenv(serviceNameEnv); n != "" {
		return n
	}
	return defaultServiceName
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
	if *service {
		return installService(streams, exe)
	}
	// A logon collector next to the service would only add a second copy of the same samples.
	if installed, err := serviceInstalled(); err != nil {
		return err
	} else if installed {
		return fmt.Errorf("perfwatch already runs as the %s service; run uninstall from an elevated prompt first", serviceName())
	}
	dir, err := dataDir()
	if err != nil {
		return err
	}
	dir = canonicalDir(dir)

	// The data directory goes into the command only when it is not the one a logon would find.
	runArgs := []string{"run"}
	if os.Getenv(dataDirEnv) != "" {
		runArgs = append(runArgs, "--data-dir", dir)
	}
	command := commandLine(exe, runArgs)
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

// commandLine quotes exe and args as one Windows command line.
func commandLine(exe string, args []string) string {
	command := windows.EscapeArg(exe)
	for _, a := range args {
		command += " " + windows.EscapeArg(a)
	}
	return command
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
	// The collector has no console to report a failure on: an exit before it is up is the failure.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	var exitErr error
	up := waitFor(func() bool {
		select {
		case exitErr = <-exited:
			return true
		default:
			return collectorRunning(dir)
		}
	})
	switch {
	case exitErr != nil || (up && !collectorRunning(dir)):
		return fmt.Errorf("the collector exited before it started (%v); run %s in a console to see why",
			exitErr, commandLine(exe, runArgs))
	case !up:
		return fmt.Errorf("the collector did not start within %s; see %s", collectorWait, filepath.Join(dir, logFileName))
	}
	fmt.Fprintf(streams.Stdout, "Collector started for %s (process %d).\n", dir, cmd.Process.Pid)
	return nil
}

// collectorRunning reports whether a collector listens on the stop event of dir, which it
// creates once it holds the data directory's lock and keeps until it exits. Only a missing event
// means none: an event this process may not open still belongs to a live collector.
func collectorRunning(dir string) bool {
	name, err := windows.UTF16PtrFromString(stopEventName(dir))
	if err != nil {
		return false
	}
	h, err := windows.OpenEvent(windows.SYNCHRONIZE, false, name)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return false
	}
	if err == nil {
		windows.CloseHandle(h)
	}
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

// removeAutostart deletes the logon entry and returns the command it held.
func removeAutostart() (command string, removed bool, err error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey(), registry.QUERY_VALUE|registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("open HKCU\\%s: %w", runKey(), err)
	}
	defer k.Close()
	command, _, err = k.GetStringValue(runValueName)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read autostart: %w", err)
	}
	if err := k.DeleteValue(runValueName); err != nil {
		return "", false, fmt.Errorf("remove autostart: %w", err)
	}
	return command, true, nil
}

// registeredDataDir returns the data directory a logon command runs with: its --data-dir, or the
// default one when it names none or there is no command.
func registeredDataDir(command string) (string, error) {
	if command != "" {
		args, err := windows.DecomposeCommandLine(command)
		if err != nil {
			return "", fmt.Errorf("read the autostart command %s: %w", command, err)
		}
		for i, a := range args {
			if a == "--data-dir" && i+1 < len(args) {
				return canonicalDir(args[i+1]), nil
			}
		}
	}
	dir, err := dataDir()
	if err != nil {
		return "", err
	}
	return canonicalDir(dir), nil
}

func runUninstall(_ context.Context, streams Streams, args []string) error {
	fs := newFlagSet("uninstall")
	if err := parseFlags(fs, streams, args); err != nil {
		return err
	}
	command, removed, err := removeAutostart()
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
	// Stop the collector the entry started, which may write somewhere other than this shell's default.
	dir, err := registeredDataDir(command)
	if err != nil {
		return err
	}
	if err := stopCollector(streams.Stdout, dir); err != nil {
		return err
	}
	fmt.Fprintf(streams.Stdout, "Data kept in %s.\n", dir)
	return nil
}

// stopCollector signals the collector of dir to stop and waits until it has.
func stopCollector(out io.Writer, dir string) error {
	stopped, err := signalStop(dir)
	if err != nil {
		return err
	}
	switch {
	case !stopped:
		fmt.Fprintf(out, "No collector runs for %s.\n", dir)
	case waitFor(func() bool { return !collectorRunning(dir) }):
		fmt.Fprintln(out, "Collector stopped.")
	default:
		return fmt.Errorf("the collector did not stop within %s; see %s", collectorWait, filepath.Join(dir, logFileName))
	}
	return nil
}
