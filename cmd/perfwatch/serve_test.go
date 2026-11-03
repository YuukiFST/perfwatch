package main_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestServe replays episodeMachine (2026-03-02, 09:00 to 09:04 UTC), starts serve on a free
// port and diffs its page, assets and API answers against the serve golden.
func TestServe(t *testing.T) {
	dataDir := t.TempDir()
	_, fixture := episodeMachine(t)
	env := []string{"PERFWATCH_DATA_DIR=" + dataDir, "PERFWATCH_NOW=2026-03-02T09:10:00Z"}
	if res := runPerfwatch(t, env, "run", "--replay", fixture); res.exitCode != 0 {
		t.Fatalf("run --replay failed:\n%s", res.transcript())
	}

	serve := exec.Command(perfwatchBin, "serve", "--port", "0")
	serve.Env = append(os.Environ(), env...)
	stdout, err := serve.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := serve.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
	}
	t.Cleanup(func() { serve.Process.Kill(); serve.Wait() })
	lines := bufio.NewScanner(stdout)
	if !lines.Scan() {
		t.Fatalf("serve printed no address: %v", lines.Err())
	}
	banner := lines.Text()
	m := regexp.MustCompile(`^perfwatch dashboard on http://127\.0\.0\.1:(\d+)/ `).FindStringSubmatch(banner)
	if m == nil {
		t.Fatalf("serve should print its loopback address, got %q", banner)
	}
	base := "http://127.0.0.1:" + m[1]

	var golden strings.Builder
	fmt.Fprintf(&golden, "$ perfwatch serve --port 0\n%s\n", strings.Replace(banner, m[1], "<port>", 1))
	get := func(path string, host string) (*http.Response, []byte) {
		req, err := http.NewRequest("GET", base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if host != "" {
			req.Host = host
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&golden, "\nGET %s\nstatus: %d\ncontent-type: %s\n", path, res.StatusCode, res.Header.Get("Content-Type"))
		return res, body
	}

	// The page and its assets: served from the binary, referring to nothing off the machine.
	for _, path := range []string{"/", "/app.js", "/app.css", "/vendor/uPlot.iife.min.js", "/vendor/uPlot.min.css"} {
		res, body := get(path, "")
		fmt.Fprintf(&golden, "content-security-policy: %s\n", res.Header.Get("Content-Security-Policy"))
		if path == "/vendor/uPlot.iife.min.js" {
			body = bytes.Replace(body, []byte("/*! https://github.com/leeoniya/uPlot (v1.6.32) */"), nil, 1)
		}
		if refs := regexp.MustCompile(`(https?:)?//[a-z0-9.-]+\.[a-z]{2,}`).FindAll(body, -1); len(refs) > 0 {
			t.Errorf("%s refers to the network: %q", path, refs)
		}
		if path == "/" && !bytes.Contains(body, []byte(`<html lang="pt-BR">`)) {
			t.Errorf("the page should declare pt-BR")
		}
	}
	golden.WriteString("(the page and its assets are checked for network references, not diffed)\n")

	// The API, at fixed times so the answers hold on any day.
	writeJSON := func(v any) {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		golden.Write(b)
		golden.WriteString("\n")
	}
	timeline := func(query string) {
		_, body := get("/api/timeline"+query, "")
		var rest map[string]any
		var series struct {
			TS  []int64    `json:"ts"`
			CPU []*float64 `json:"cpu"`
			RAM []*float64 `json:"ram"`
		}
		if err := errors.Join(json.Unmarshal(body, &rest), json.Unmarshal(body, &series)); err != nil {
			t.Fatalf("timeline%s: %v\n%s", query, err, body)
		}
		delete(rest, "ts")
		delete(rest, "cpu")
		delete(rest, "ram")
		writeJSON(rest)
		// One line per bucket with a sample, so gaps read as jumps in the start times.
		sampled := 0
		var rows strings.Builder
		for i, ts := range series.TS {
			if series.CPU[i] == nil {
				continue
			}
			sampled++
			fmt.Fprintf(&rows, "%s cpu %v ram %v\n", time.Unix(ts, 0).UTC().Format("15:04:05"), *series.CPU[i], *series.RAM[i])
		}
		fmt.Fprintf(&golden, "buckets: %d, with samples: %d\n%s", len(series.TS), sampled, rows.String())
	}
	timeline("")                                                   // the last 24 h, from the minute rollups
	timeline("?from=2026-03-02T09:02:50Z&to=2026-03-02T09:03:50Z") // 1 s buckets
	timeline("?from=2026-02-20T09:00:00Z&to=2026-02-20T10:00:00Z") // 1 s samples expired: minutes
	// Just under 1000 minutes, off the minute: starting on the minute must not make 1001 buckets.
	timeline("?from=2026-03-01T16:30:30Z&to=2026-03-02T09:10:30Z")

	_, body := get("/api/episodes/1", "")
	var detail struct {
		Episode json.RawMessage `json:"episode"`
		Seconds struct {
			TS           []float64 `json:"ts"`
			CPU          []float64 `json:"cpu"`
			RAM          []float64 `json:"ram"`
			Unattributed []float64 `json:"unattributed"`
		} `json:"seconds"`
	}
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatalf("episode 1: %v\n%s", err, body)
	}
	var episode any
	if err := json.Unmarshal(detail.Episode, &episode); err != nil {
		t.Fatal(err)
	}
	writeJSON(episode)
	s := detail.Seconds
	for i, ts := range s.TS {
		end := time.UnixMilli(int64(ts * 1000)).UTC().Format("15:04:05")
		fmt.Fprintf(&golden, "%s cpu %v ram %v unattributed %v\n", end, s.CPU[i], s.RAM[i], s.Unattributed[i])
	}

	for _, path := range []string{
		"/api/ranking?from=2026-03-02T09:00:00Z&to=2026-03-02T09:10:00Z&peaks_only=true",
		"/api/episodes/99",
		"/api/episodes/x",
		"/api/timeline?from=yesterday",
		"/api/timeline?from=2026-03-02T09:00:00Z&to=2026-03-02T09:00:00Z",
		"/api/timeline?from=2020-01-01T00:00:00Z",
	} {
		_, body := get(path, "")
		var v any
		if err := json.Unmarshal(body, &v); err != nil {
			t.Fatalf("%s: %v\n%s", path, err, body)
		}
		writeJSON(v)
	}

	// localhost is the loopback too; a name other than those, as a rebound DNS name would send, is refused.
	get("/api/timeline", "localhost:"+m[1])
	_, body = get("/api/timeline", "perfwatch.example:"+m[1])
	golden.Write(bytes.ReplaceAll(body, []byte(m[1]), []byte("<port>")))

	assertGolden(t, "serve", golden.String())
}
