package main_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestExport replays episodeMachine (2026-03-02, 09:00 to 09:04 UTC), exports the last 7 days
// and diffs the report's data against the export golden; the page must hold everything inline.
func TestExport(t *testing.T) {
	dataDir, outDir := t.TempDir(), t.TempDir()
	_, fixture := episodeMachine(t)
	env := []string{"PERFWATCH_DATA_DIR=" + dataDir, "PERFWATCH_NOW=2026-03-02T09:10:00Z"}
	if res := runPerfwatch(t, env, "run", "--replay", fixture); res.exitCode != 0 {
		t.Fatalf("run --replay failed:\n%s", res.transcript())
	}
	mask := strings.NewReplacer(dataDir, "<data dir>", outDir, "<out dir>")
	reportPath := filepath.Join(outDir, "report.html")

	var golden strings.Builder
	for _, args := range [][]string{
		{"export", "--html", reportPath},
		{"export"},
		{"export", "--html", "report.html", "--since", "366d"}, // past the minute history
	} {
		res := runPerfwatch(t, env, args...)
		res.stdout = regexp.MustCompile(`\d+ KiB`).ReplaceAllString(res.stdout, "<size> KiB")
		golden.WriteString(mask.Replace(res.transcript()) + "\n")
	}
	// The reason comes from the OS in its own language, so only its start is checked.
	if res := runPerfwatch(t, env, "export", "--html", filepath.Join(outDir, "missing", "report.html")); res.exitCode != 1 || !strings.HasPrefix(res.stderr, "perfwatch: write report: ") {
		t.Errorf("export into a missing directory should fail to write:\n%s", res.transcript())
	}

	// A report aimed at the database it reads must leave the database whole.
	if res := runPerfwatch(t, env, "export", "--html", filepath.Join(dataDir, "perfwatch.db")); res.exitCode != 1 || !strings.Contains(res.stderr, "outside the data directory") {
		t.Errorf("export over the database should be refused:\n%s", res.transcript())
	}
	if res := runPerfwatch(t, env, "peaks"); res.exitCode != 0 {
		t.Errorf("peaks after the refused export failed:\n%s", res.transcript())
	}

	page, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(page, []byte(`<html lang="pt-BR">`)) {
		t.Errorf("the report should declare pt-BR")
	}
	// Everything inline: no file or URL to fetch, and no address off the machine.
	if refs := regexp.MustCompile(`\s(src|href)=`).FindAll(page, -1); len(refs) > 0 {
		t.Errorf("the report loads files: %q", refs)
	}
	withoutBanner := bytes.Replace(page, []byte("/*! https://github.com/leeoniya/uPlot (v1.6.32) */"), nil, 1)
	if refs := regexp.MustCompile(`(https?:)?//[a-z0-9.-]+\.[a-z]{2,}`).FindAll(withoutBanner, -1); len(refs) > 0 {
		t.Errorf("the report refers to the network: %q", refs)
	}

	m := regexp.MustCompile(`<script>window\.PERFWATCH_REPORT = (.*);</script>`).FindSubmatch(page)
	if m == nil {
		t.Fatal("the report carries no data")
	}
	// The page script reads the data when it starts, so the data must come first.
	data, app := bytes.Index(page, []byte("<script>window.PERFWATCH_REPORT")), bytes.Index(page, []byte("const report = window.PERFWATCH_REPORT"))
	if app < 0 || data > app {
		t.Errorf("the data script should precede the page script (data at %d, page script at %d)", data, app)
	}
	var report struct {
		Generated    string                     `json:"generated"`
		Timeline     json.RawMessage            `json:"timeline"`
		Ranking      any                        `json:"ranking"`
		RankingPeaks any                        `json:"ranking_peaks"`
		Episodes     map[string]json.RawMessage `json:"episodes"`
	}
	if err := json.Unmarshal(m[1], &report); err != nil {
		t.Fatalf("report data: %v", err)
	}
	golden.WriteString("generated: " + report.Generated + "\n\ntimeline\n")
	writeTimelineGolden(t, &golden, report.Timeline)
	golden.WriteString("\nranking\n")
	writeIndentedJSON(t, &golden, report.Ranking)
	golden.WriteString("\nranking during peaks\n")
	writeIndentedJSON(t, &golden, report.RankingPeaks)
	ids := make([]string, 0, len(report.Episodes))
	for id := range report.Episodes {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		golden.WriteString("\nepisode " + id + "\n")
		writeEpisodeGolden(t, &golden, report.Episodes[id])
	}
	assertGolden(t, "export", golden.String())
}
