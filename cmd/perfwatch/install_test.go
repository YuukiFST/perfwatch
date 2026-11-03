//go:build windows

package main_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// TestInstall installs into a scratch registry key, never the real autostart list: install
// starts one collector, a second install and the registered command leave that one, and
// uninstall stops it, removes the entry and keeps the data.
func TestInstall(t *testing.T) {
	dataDir := t.TempDir()
	scratch := `Software\perfwatch-e2e-` + strconv.FormatInt(time.Now().UnixNano(), 36)
	runKey := scratch + `\Run`
	// A service name no service has keeps uninstall away from a real perfwatch service.
	seams := []string{"PERFWATCH_RUN_KEY=" + runKey, "PERFWATCH_SERVICE=perfwatch-e2e-absent"}
	env := append([]string{"PERFWATCH_DATA_DIR=" + dataDir}, seams...)
	// uninstall runs from a shell whose default data directory is another one: it must still stop
	// the collector the autostart entry started.
	otherEnv := append([]string{"PERFWATCH_DATA_DIR=" + t.TempDir()}, seams...)
	t.Cleanup(func() {
		runPerfwatch(t, env, "uninstall") // stops a collector a failed check left running
		registry.DeleteKey(registry.CURRENT_USER, runKey)
		registry.DeleteKey(registry.CURRENT_USER, scratch)
	})

	mask := strings.NewReplacer(longPath(t, perfwatchBin), "<perfwatch>", longPath(t, dataDir), "<data dir>", runKey, "<run key>")
	pid := regexp.MustCompile(`\(process \d+\)`)
	var golden strings.Builder
	record := func(res cliResult) {
		golden.WriteString(pid.ReplaceAllString(mask.Replace(res.transcript()), "(process <pid>)") + "\n")
	}
	record(runPerfwatch(t, env, "install"))
	record(runPerfwatch(t, env, "install"))

	// Logon runs the registered command: with the collector up, it must leave that one alone.
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		t.Fatalf("install left no key: %v", err)
	}
	command, _, err := k.GetStringValue("perfwatch")
	k.Close()
	if err != nil {
		t.Fatalf("install left no autostart value: %v", err)
	}
	args, err := windows.DecomposeCommandLine(command)
	if err != nil {
		t.Fatal(err)
	}
	logon := exec.Command(args[0], args[1:]...)
	logon.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	logon.Stderr = &stderr
	err = logon.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || !strings.Contains(stderr.String(), "already writing") {
		t.Errorf("the registered command should refuse to start a second collector: %v\n%s", err, stderr.String())
	}

	record(runPerfwatch(t, otherEnv, "uninstall"))
	record(runPerfwatch(t, env, "uninstall"))
	if k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE); err == nil {
		if _, _, err := k.GetStringValue("perfwatch"); err == nil {
			t.Errorf("uninstall left the autostart value")
		}
		k.Close()
	}
	if _, err := os.Stat(filepath.Join(dataDir, "perfwatch.db")); err != nil {
		t.Errorf("uninstall should keep the data: %v", err)
	}
	if log := readFile(t, filepath.Join(dataDir, "perfwatch.log")); !strings.Contains(log, `"msg":"collector stopped"`) || strings.Contains(log, `"level":"ERROR"`) {
		t.Errorf("the collector should have stopped cleanly:\n%s", log)
	}
	assertGolden(t, "install", golden.String())

	if !windows.GetCurrentProcessToken().IsElevated() {
		res := runPerfwatch(t, env, "install", "--service")
		if res.exitCode != 1 || !strings.Contains(res.stderr, "needs an elevated prompt") {
			t.Errorf("install --service without admin should say it needs an elevated prompt:\n%s", res.transcript())
		}
	}
}

// longPath spells path with long names, as perfwatch prints it.
func longPath(t *testing.T, path string) string {
	t.Helper()
	p, err := windows.UTF16FromString(path)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetLongPathName(&p[0], &buf[0], uint32(len(buf)))
	if err != nil {
		t.Fatalf("long path of %s: %v", path, err)
	}
	return windows.UTF16ToString(buf[:n])
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
