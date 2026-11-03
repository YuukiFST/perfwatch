package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/YuukiFST/perfwatch/internal/config"
	"github.com/YuukiFST/perfwatch/internal/store"
)

// reportJSON is everything the dashboard page asks the server for, answered ahead of time.
type reportJSON struct {
	Generated    time.Time                    `json:"generated"`
	Timeline     timelineJSON                 `json:"timeline"`
	Ranking      topJSON                      `json:"ranking"`
	RankingPeaks topJSON                      `json:"ranking_peaks"`
	Episodes     map[string]episodeDetailJSON `json:"episodes"`
}

func runExport(ctx context.Context, streams Streams, args []string) (err error) {
	fs := newFlagSet("export")
	htmlPath := fs.String("html", "", "write the report to this HTML file")
	since := &sinceFlag{7 * 24 * time.Hour}
	fs.Var(since, "since", "report this much time back from now: 90m, 24h, 7d")
	if err := parseFlags(fs, streams, args); err != nil {
		return err
	}
	if *htmlPath == "" {
		fmt.Fprintln(streams.Stderr, "perfwatch export: --html names the file to write")
		fs.SetOutput(streams.Stderr)
		fs.Usage()
		return errUsage
	}
	now, loc, err := clock()
	if err != nil {
		return err
	}
	if since.d > store.MinutesRetention {
		fmt.Fprintf(streams.Stderr, "invalid value %q for flag -since: at most %dd, as long as perfwatch keeps its minute history\n",
			since.String(), store.MinutesRetention/(24*time.Hour))
		fs.SetOutput(streams.Stderr)
		fs.Usage()
		return errUsage
	}
	dir, err := dataDir()
	if err != nil {
		return err
	}
	out, err := filepath.Abs(*htmlPath)
	if err != nil {
		return err
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	// The data directory holds the database the report is read from; a report never replaces it.
	if strings.EqualFold(filepath.Dir(out), absDir) {
		return fmt.Errorf("write the report outside the data directory %s", dir)
	}
	cfg, err := config.Load(filepath.Join(dir, config.FileName))
	if err != nil {
		return err
	}
	db, err := openForReading(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()

	from := now.Add(-since.d)
	report := reportJSON{Generated: now.In(loc), Episodes: map[string]episodeDetailJSON{}}
	if report.Timeline, err = buildTimeline(ctx, db, cfg.Peaks, from, now, now, loc); err != nil {
		return err
	}
	if report.Ranking, err = rankTop(ctx, db, from, now, loc, false, rankedPrograms); err != nil {
		return err
	}
	if report.RankingPeaks, err = rankTop(ctx, db, from, now, loc, true, rankedPrograms); err != nil {
		return err
	}
	for _, e := range report.Timeline.Episodes {
		detail, found, err := buildEpisodeDetail(ctx, db, e.ID, loc)
		if err != nil {
			return err
		}
		if found {
			report.Episodes[strconv.FormatInt(e.ID, 10)] = detail
		}
	}
	page, err := reportPage(report)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(out, page); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	t := report.Timeline
	fmt.Fprintf(streams.Stdout, "Wrote %s: %s to %s, peak episodes: %d, %d KiB\n", *htmlPath,
		t.From.Format("2006-01-02 15:04"), t.To.Format("2006-01-02 15:04 MST"), len(t.Episodes), (len(page)+1023)/1024)
	return nil
}

// writeFileAtomic writes data to a temporary file beside path and renames it over path, so a
// failed write leaves an existing file as it was.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if err = errors.Join(err, tmp.Close()); err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// reportPage inlines the dashboard page's styles, scripts and the report data into one file
// that opens from disk with no server and no network.
func reportPage(report reportJSON) ([]byte, error) {
	read := func(name string) (string, error) {
		b, err := webFiles.ReadFile("web/" + name)
		// An inline script or style ends at the first "</", so none may hold one.
		return strings.ReplaceAll(string(b), "</", `<\/`), err
	}
	page, err := webFiles.ReadFile("web/index.html")
	if err != nil {
		return nil, err
	}
	// json.Marshal escapes <, > and &, so the data cannot close its script element.
	data, err := json.Marshal(report)
	if err != nil {
		return nil, err
	}
	html := string(page)
	inline := []struct{ tag, open, file, close, before string }{
		{`<link rel="stylesheet" href="vendor/uPlot.min.css">`, "<style>", "vendor/uPlot.min.css", "</style>", ""},
		{`<link rel="stylesheet" href="app.css">`, "<style>", "app.css", "</style>", ""},
		{`<script src="vendor/uPlot.iife.min.js"></script>`, "<script>", "vendor/uPlot.iife.min.js", "</script>", ""},
		{`<script src="app.js"></script>`, "<script>", "app.js", "</script>", "<script>window.PERFWATCH_REPORT = " + string(data) + ";</script>\n"},
	}
	for _, in := range inline {
		body, err := read(in.file)
		if err != nil {
			return nil, err
		}
		if !strings.Contains(html, in.tag) {
			return nil, fmt.Errorf("report page: index.html no longer holds %s", in.tag)
		}
		html = strings.Replace(html, in.tag, in.before+in.open+body+in.close, 1)
	}
	return []byte(html), nil
}
