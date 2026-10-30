package main_test

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata/golden")

// perfwatchBin is the binary TestMain builds; every test drives it as a user would.
var perfwatchBin string

func TestMain(m *testing.M) {
	flag.Parse()
	dir, err := os.MkdirTemp("", "perfwatch-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create build dir:", err)
		os.Exit(1)
	}
	perfwatchBin = filepath.Join(dir, "perfwatch.exe")
	build := exec.Command("go", "build", "-o", perfwatchBin, ".")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build perfwatch:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type cliResult struct {
	args     []string
	exitCode int
	stdout   string
	stderr   string
}

// runPerfwatch runs the built binary with args and extra environment entries (KEY=value).
func runPerfwatch(t *testing.T, env []string, args ...string) cliResult {
	t.Helper()
	cmd := exec.Command(perfwatchBin, args...)
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("run perfwatch %v: %v", args, err)
	}
	return cliResult{args: args, exitCode: cmd.ProcessState.ExitCode(), stdout: stdout.String(), stderr: stderr.String()}
}

// transcript renders a run as labeled sections, the shape every CLI golden file uses.
func (r cliResult) transcript() string {
	return fmt.Sprintf("$ perfwatch %s\nexit: %d\n--- stdout\n%s--- stderr\n%s",
		strings.Join(r.args, " "), r.exitCode, r.stdout, r.stderr)
}

// assertGolden diffs got against testdata/golden/<name>.golden; -update rewrites the file.
func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name+".golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run go test ./cmd/perfwatch -update to create it): %v", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from the golden file (go test ./cmd/perfwatch -update rewrites it):\n%s", path, lineDiff(string(want), got))
	}
}

// lineDiff reports the first differing line with a few lines of context.
func lineDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	i := 0
	for i < len(w) && i < len(g) && w[i] == g[i] {
		i++
	}
	var b strings.Builder
	fmt.Fprintf(&b, "first difference at line %d\n", i+1)
	for j := max(0, i-2); j < i; j++ {
		fmt.Fprintf(&b, "  %s\n", w[j])
	}
	for j := i; j < min(len(w), i+3); j++ {
		fmt.Fprintf(&b, "- %s\n", w[j])
	}
	for j := i; j < min(len(g), i+3); j++ {
		fmt.Fprintf(&b, "+ %s\n", g[j])
	}
	return b.String()
}
