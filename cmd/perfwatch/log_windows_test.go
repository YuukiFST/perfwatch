package main_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// TestRunLogsWhenRotationFails holds perfwatch.log open without delete sharing, as a log
// viewer may, so it cannot be renamed: run must keep logging to it and leave the rotated
// files as they were.
func TestRunLogsWhenRotationFails(t *testing.T) {
	dataDir := t.TempDir()
	logPath := filepath.Join(dataDir, "perfwatch.log")
	full := append(bytes.Repeat([]byte("x"), 5<<20-1), '\n')
	for path, content := range map[string][]byte{logPath: full, logPath + ".1": []byte("one\n"), logPath + ".2": []byte("two\n")} {
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	name, err := windows.UTF16PtrFromString(logPath)
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	res := runPerfwatch(t, []string{"PERFWATCH_DATA_DIR=" + dataDir}, "run", "--replay", "testdata/fixtures/usage-basic.jsonl")
	windows.CloseHandle(viewer)
	if res.exitCode != 0 {
		t.Fatalf("run failed:\n%s", res.transcript())
	}

	log, err := os.ReadFile(logPath)
	if err != nil || !bytes.HasPrefix(log, full) || !bytes.Contains(log[len(full):], []byte(`"msg":"collector started"`)) {
		t.Errorf("perfwatch.log should keep the old 5 MiB and go on with this run's lines: %d bytes, %v", len(log), err)
	}
	for suffix, want := range map[string]string{".1": "one\n", ".2": "two\n"} {
		if got, err := os.ReadFile(logPath + suffix); err != nil || string(got) != want {
			t.Errorf("perfwatch.log%s = %q, %v; want %q", suffix, got, err, want)
		}
	}
	for _, suffix := range []string{".3", ".rotating"} {
		if _, err := os.Stat(logPath + suffix); !os.IsNotExist(err) {
			t.Errorf("perfwatch.log%s should not exist: %v", suffix, err)
		}
	}
}
