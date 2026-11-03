package main_test

import (
	"strings"
	"testing"
)

// TestTopAndPeaks replays episodeMachine (2026-03-02, 09:00 to 09:04 UTC), then reads it back
// with top and peaks at a fixed now, as tables and as JSON.
func TestTopAndPeaks(t *testing.T) {
	dataDir := t.TempDir()
	_, fixture := episodeMachine(t)
	env := []string{"PERFWATCH_DATA_DIR=" + dataDir, "PERFWATCH_NOW=2026-03-02T09:10:00Z"}
	if res := runPerfwatch(t, env, "run", "--replay", fixture); res.exitCode != 0 {
		t.Fatalf("run --replay failed:\n%s", res.transcript())
	}
	transcripts := func(runs ...[]string) string {
		var b strings.Builder
		for _, args := range runs {
			b.WriteString(strings.ReplaceAll(runPerfwatch(t, env, args...).transcript(), dataDir, "<data dir>"))
			b.WriteString("\n")
		}
		return b.String()
	}
	assertGolden(t, "top", transcripts(
		[]string{"top"},
		[]string{"top", "--json", "--limit", "5"},
		[]string{"top", "--since", "8m", "--limit", "3"}, // minutes from 09:02 on
		[]string{"top", "--peaks-only"},
		[]string{"top", "--peaks-only", "--json", "--limit", "3"},
		[]string{"top", "--since", "1m"}, // after the last sample
		[]string{"top", "--since", "0"},
		[]string{"top", "--since", "3x"},
	))
	assertGolden(t, "peaks", transcripts(
		[]string{"peaks"},
		[]string{"peaks", "--json"},
		[]string{"peaks", "--since", "7m"}, // from 09:03 on: the two latest episodes
	))
	empty := []string{"PERFWATCH_DATA_DIR=" + t.TempDir()}
	for _, cmd := range []string{"top", "peaks"} {
		res := runPerfwatch(t, empty, cmd)
		if res.exitCode != 1 || !strings.Contains(res.stderr, "no data in") {
			t.Errorf("%s on an empty data directory should fail with no data:\n%s", cmd, res.transcript())
		}
	}
}
