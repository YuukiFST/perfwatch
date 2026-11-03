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
	// maxTimelineRange is the longest range a request takes: the page offers up to 30 days, and a
	// longer ranking would hold the one database connection for every other request.
	maxTimelineRange = 31 * 24 * time.Hour
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
	if d.port == 80 {
		// Browsers leave the scheme's default port out of Host.
		allowed["127.0.0.1"], allowed["localhost"] = true, true
	}
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

func (d *dashboard) timeline(w http.ResponseWriter, r *http.Request) {
	from, to, now, loc, err := requestRange(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	out, err := buildTimeline(r.Context(), d.db, d.peaks, from, to, now, loc)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
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
	detail, found, err := buildEpisodeDetail(r.Context(), d.db, id, loc)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, fmt.Errorf("no episode %d", id))
		return
	}
	writeJSON(w, detail)
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
