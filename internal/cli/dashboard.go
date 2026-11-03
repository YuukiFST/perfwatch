package cli

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/YuukiFST/perfwatch/internal/config"
	"github.com/YuukiFST/perfwatch/internal/store"
)

// The dashboard's page, script, styles and the vendored uPlot; served as is, with no network.
//
//go:embed web
var webFiles embed.FS

const (
	// maxTimelinePoints bounds how many buckets one timeline request returns.
	maxTimelinePoints = 1000
	// maxTimelineRange is the longest range a timeline request takes: the minute rollups' life.
	maxTimelineRange = store.MinutesRetention
	// rankedPrograms and episodeDetailPrograms are how many programs the dashboard lists.
	rankedPrograms        = 20
	episodeDetailPrograms = 10
)

// timelineSteps are the bucket lengths a timeline picks from, the shortest that fits the range.
var timelineSteps = []time.Duration{
	time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 15 * time.Second, 30 * time.Second,
	time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute, 30 * time.Minute,
	time.Hour, 2 * time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour,
}

// assetTypes are the content types of the embedded files; "/" serves index.html.
var assetTypes = map[string]string{
	"":      "text/html; charset=utf-8",
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
}

type dashboard struct {
	db    *store.DB
	peaks config.Peaks
	port  int
}

func newDashboard(db *store.DB, peaks config.Peaks, port int) http.Handler {
	d := &dashboard{db: db, peaks: peaks, port: port}
	static, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err) // the embedded tree always holds web
	}
	files := http.FileServerFS(static)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		// Fixed here: Go otherwise takes them from the Windows registry, which varies by machine.
		if ctype, ok := assetTypes[path.Ext(r.URL.Path)]; ok {
			w.Header().Set("Content-Type", ctype)
		}
		files.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /api/timeline", d.timeline)
	mux.HandleFunc("GET /api/ranking", d.ranking)
	mux.HandleFunc("GET /api/episodes/{id}", d.episode)
	return d.sameHost(mux)
}

// sameHost answers only requests addressed to the loopback name and port it listens on, so a
// web page cannot reach the dashboard through a DNS name rebound to 127.0.0.1.
func (d *dashboard) sameHost(next http.Handler) http.Handler {
	port := strconv.Itoa(d.port)
	allowed := map[string]bool{net.JoinHostPort("127.0.0.1", port): true, net.JoinHostPort("localhost", port): true}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[r.Host] {
			http.Error(w, "perfwatch answers only on 127.0.0.1:"+port, http.StatusMisdirectedRequest)
			return
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

type thresholdsJSON struct {
	CPUStartPercent float64 `json:"cpu_start_percent"`
	CPUEndPercent   float64 `json:"cpu_end_percent"`
	RAMStartPercent float64 `json:"ram_start_percent"`
	RAMEndPercent   float64 `json:"ram_end_percent"`
}

// timelineJSON holds one value per bucket in parallel arrays, as uPlot takes them: TS is each
// bucket's start in Unix seconds, and null in CPU and RAM marks a bucket with no sample.
type timelineJSON struct {
	From        time.Time      `json:"from"`
	To          time.Time      `json:"to"`
	StepSeconds float64        `json:"step_seconds"`
	Thresholds  thresholdsJSON `json:"thresholds"`
	TS          []int64        `json:"ts"`
	CPU         []*float64     `json:"cpu"`
	RAM         []*float64     `json:"ram"`
	Episodes    []episodeJSON  `json:"episodes"`
}

func (d *dashboard) timeline(w http.ResponseWriter, r *http.Request) {
	from, to, now, loc, err := requestRange(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	step := timelineSteps[len(timelineSteps)-1]
	for _, s := range timelineSteps {
		if to.Sub(from) <= s*maxTimelinePoints {
			step = s
			break
		}
	}
	// The 1 s samples are kept for a week; older ranges come from the minute rollups.
	if step < time.Minute && from.Before(now.Add(-store.SecondsRetention)) {
		step = time.Minute
	}
	if step >= time.Minute {
		from = from.Truncate(time.Minute)
	}
	points, err := d.db.Timeline(r.Context(), from, to, step)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	episodes, err := d.db.Episodes(r.Context(), from, to, 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := timelineJSON{
		From: from.In(loc), To: to.In(loc), StepSeconds: step.Seconds(),
		Thresholds: thresholdsJSON{
			CPUStartPercent: d.peaks.CPUStartPercent, CPUEndPercent: d.peaks.CPUEndPercent,
			RAMStartPercent: d.peaks.RAMStartPercent, RAMEndPercent: d.peaks.RAMEndPercent,
		},
		Episodes: []episodeJSON{},
	}
	next := 0
	for start := from; start.Before(to); start = start.Add(step) {
		out.TS = append(out.TS, start.Unix())
		var cpu, ram *float64
		if next < len(points) && points[next].Start.Equal(start) {
			c, m := round2(points[next].CPUPercent), round2(points[next].RAMPercent)
			cpu, ram = &c, &m
			next++
		}
		out.CPU, out.RAM = append(out.CPU, cpu), append(out.RAM, ram)
	}
	// Oldest first, as the timeline reads.
	for i := len(episodes) - 1; i >= 0; i-- {
		out.Episodes = append(out.Episodes, newEpisodeJSON(episodes[i], loc))
	}
	writeJSON(w, out)
}

func (d *dashboard) ranking(w http.ResponseWriter, r *http.Request) {
	from, to, _, loc, err := requestRange(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	out, err := rankTop(r.Context(), d.db, from, to, loc, r.URL.Query().Get("peaks_only") == "true", rankedPrograms)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, out)
}

type episodeSecondsJSON struct {
	TS           []float64 `json:"ts"`
	CPU          []float64 `json:"cpu"`
	RAM          []float64 `json:"ram"`
	Unattributed []float64 `json:"unattributed"`
}

func (d *dashboard) episode(w http.ResponseWriter, r *http.Request) {
	_, loc, err := clock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("episode id %q is not a number", r.PathValue("id")))
		return
	}
	e, found, err := d.db.EpisodeByID(r.Context(), id, episodeDetailPrograms)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, fmt.Errorf("no episode %d", id))
		return
	}
	seconds, err := d.db.EpisodeSeconds(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// Each sample is drawn at the end of its interval, in Unix seconds as uPlot takes them.
	series := episodeSecondsJSON{TS: []float64{}, CPU: []float64{}, RAM: []float64{}, Unattributed: []float64{}}
	for _, s := range seconds {
		series.TS = append(series.TS, float64(s.End.UnixMilli())/1000)
		series.CPU = append(series.CPU, round2(s.CPUPercent))
		series.RAM = append(series.RAM, round2(s.RAMPercent))
		series.Unattributed = append(series.Unattributed, round2(s.UnattributedPercent))
	}
	writeJSON(w, struct {
		Episode episodeJSON        `json:"episode"`
		Seconds episodeSecondsJSON `json:"seconds"`
	}{newEpisodeJSON(e, loc), series})
}

// requestRange reads the from and to query parameters as RFC 3339 times; to defaults to now and
// from to 24 hours before to.
func requestRange(r *http.Request) (from, to, now time.Time, loc *time.Location, err error) {
	now, loc, err = clock()
	if err != nil {
		return
	}
	to, from = now, now.Add(-24*time.Hour)
	q := r.URL.Query()
	if v := q.Get("to"); v != "" {
		if to, err = time.Parse(time.RFC3339, v); err != nil {
			return from, to, now, loc, fmt.Errorf("to: want an RFC 3339 time: %w", err)
		}
		from = to.Add(-24 * time.Hour)
	}
	if v := q.Get("from"); v != "" {
		if from, err = time.Parse(time.RFC3339, v); err != nil {
			return from, to, now, loc, fmt.Errorf("from: want an RFC 3339 time: %w", err)
		}
	}
	switch {
	case !from.Before(to):
		err = errors.New("from must be before to")
	case to.Sub(from) > maxTimelineRange:
		err = fmt.Errorf("the range may span at most %d days", maxTimelineRange/(24*time.Hour))
	}
	return from, to, now, loc, err
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
