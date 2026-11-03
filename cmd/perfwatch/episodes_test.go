package main_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// episodeMachine replays a day with every case the peak rules name: a 2-sample blip, a CPU
// episode whose cool-down is interrupted once, a gap, a RAM episode right after the gap, and an
// episode still open when the fixture ends. It returns the machine, so a test can go on with it.
func episodeMachine(t *testing.T) (*machine, string) {
	m := newMachine(time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC))
	m.spawn(fixtureProcess{PID: 4, Name: "System", PrivateBytes: 1 * mib})
	m.spawn(fixtureProcess{PID: 3001, Name: "stress.exe", PrivateBytes: 20 * mib})
	m.spawn(fixtureProcess{PID: 3002, Name: "chrome.exe", PrivateBytes: 400 * mib})
	m.spawn(fixtureProcess{PID: 3003, Name: "chrome.exe", PrivateBytes: 300 * mib})
	m.spawn(fixtureProcess{PID: 3004, Name: "big.exe", PrivateBytes: 3 * 1024 * mib})
	shares := map[string]float64{"System": 2, "chrome.exe": 5} // chrome.exe: per instance
	for i := 1; i <= 12; i++ {
		name := fmt.Sprintf("f%02d.exe", i)
		shares[name] = 0.5
		m.spawn(fixtureProcess{PID: uint32(3100 + i), Name: name, PrivateBytes: uint64(i) * 10 * mib})
	}
	// busy is the machine's CPU %; stress.exe takes what the others leave, minus 2% unattributed.
	steps := func(n int, busy, ram float64) {
		shares["stress.exe"] = max(0, busy-2-2-2*5-12*0.5)
		m.ramPct = ram
		for range n {
			m.step(time.Second, busy, shares)
		}
	}
	steps(40, 20, 50)
	steps(2, 95, 50) // blip: 2 hot samples start nothing
	steps(10, 20, 50)
	steps(8, 95, 50) // CPU episode
	steps(2, 80, 50) // neither hot nor cool: the episode goes on
	steps(10, 95, 50)
	steps(6, 50, 50)
	steps(1, 78, 50) // not cool: the cool-down starts over
	steps(10, 20, 50)
	m.jump(time.Minute) // collector off: a gap
	steps(5, 20, 50)
	steps(5, 20, 93) // RAM episode, with only the 5 samples since the gap as pre-roll
	steps(10, 20, 80)
	steps(20, 20, 50)
	steps(3, 85, 50) // exactly the CPU start threshold, exactly start_samples long: an episode
	steps(1, 75, 50) // exactly the CPU end threshold: not cool
	steps(10, 20, 50)
	steps(3, 20, 87.5) // below the default RAM start; TestRunEpisodeConfig moves it to 87.5
	steps(1, 20, 75)   // and the RAM end to 75
	steps(10, 20, 50)
	steps(4, 95, 50) // still open when the fixture ends
	return m, m.write(t, "episodes")
}

// TestRunDetectsEpisodes replays episodeMachine with the default thresholds and dumps every
// stored episode, its per-second samples and the log lines about it; then jumps 179 and 181
// days ahead, so the episodes expire and take stress.exe, which only they still name, along.
func TestRunDetectsEpisodes(t *testing.T) {
	dataDir := t.TempDir()
	m, fixture := episodeMachine(t)
	if res := runPerfwatch(t, []string{"PERFWATCH_DATA_DIR=" + dataDir}, "run", "--replay", fixture); res.exitCode != 0 {
		t.Fatalf("run --replay failed:\n%s", res.transcript())
	}
	got := dumpEpisodes(t, filepath.Join(dataDir, "perfwatch.db"), true) + episodeLog(t, filepath.Join(dataDir, "perfwatch.log"))
	assertGolden(t, "run-episodes", got)

	// A replay of stored data changes nothing.
	if res := runPerfwatch(t, []string{"PERFWATCH_DATA_DIR=" + dataDir}, "run", "--replay", fixture); res.exitCode != 0 {
		t.Fatalf("second run --replay failed:\n%s", res.transcript())
	}
	if again := dumpEpisodes(t, filepath.Join(dataDir, "perfwatch.db"), true); !strings.HasPrefix(got, again) {
		t.Errorf("replaying the fixture again changed the episodes:\n%s", lineDiff(got, again))
	}

	m.exit(3001) // stress.exe
	start := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	var states strings.Builder
	for _, days := range []int{179, 181} {
		m.jump(time.Duration(days)*24*time.Hour - m.now.Sub(start))
		m.step(time.Second, 20, nil)
		m.step(time.Second, 20, nil)
		if res := runPerfwatch(t, []string{"PERFWATCH_DATA_DIR=" + dataDir}, "run", "--replay", m.write(t, fmt.Sprintf("day%d", days))); res.exitCode != 0 {
			t.Fatalf("run --replay failed:\n%s", res.transcript())
		}
		var episodes, seconds, programs, stress int
		err := openStore(t, filepath.Join(dataDir, "perfwatch.db")).QueryRow(`SELECT (SELECT count(*) FROM episodes),
			(SELECT count(*) FROM episode_seconds), (SELECT count(*) FROM programs),
			(SELECT count(*) FROM programs WHERE name = 'stress.exe')`).Scan(&episodes, &seconds, &programs, &stress)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&states, "after day %d: episodes=%d episode_seconds=%d programs=%d stress.exe=%d\n", days, episodes, seconds, programs, stress)
	}
	assertGolden(t, "run-episode-retention", states.String())
}

// TestRunEpisodeConfig replays episodeMachine under perfwatch.toml files: valid thresholds
// change which episodes exist, and a bad file stops run before it samples anything.
func TestRunEpisodeConfig(t *testing.T) {
	_, fixture := episodeMachine(t)
	var b strings.Builder
	for _, tc := range []struct{ name, toml string }{
		{"cpu threshold above every sample", "[peaks]\ncpu_start_percent = 96\n"},
		{"6 hot samples to start, which the 5 s and 4 s episodes lack", "[peaks]\nstart_samples = 6\n"},
		{"no pre-roll", "[peaks]\npreroll_seconds = 0\n"},
		{"ram thresholds on samples at exactly 87.5% and 75%", "[peaks]\nram_start_percent = 87.5\nram_end_percent = 75\n"},
		{"end above start", "[peaks]\ncpu_end_percent = 90\n"},
		{"not a number", "[peaks]\ncpu_end_percent = nan\n"},
		{"pre-roll beyond the memory bound", "[peaks]\npreroll_seconds = 301\n"},
		{"unknown key", "[peaks]\ncpu_start = 90\n"},
		{"not TOML", "[peaks\n"},
	} {
		dataDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dataDir, "perfwatch.toml"), []byte(tc.toml), 0o644); err != nil {
			t.Fatal(err)
		}
		res := runPerfwatch(t, []string{"PERFWATCH_DATA_DIR=" + dataDir}, "run", "--replay", fixture)
		fmt.Fprintf(&b, "== %s\n%s", tc.name, tc.toml)
		fmt.Fprintf(&b, "exit: %d\n%s", res.exitCode, strings.ReplaceAll(res.stderr, dataDir, "<data dir>"))
		if res.exitCode == 0 {
			b.WriteString(dumpEpisodes(t, filepath.Join(dataDir, "perfwatch.db"), false))
		}
	}
	assertGolden(t, "run-episode-config", b.String())
}

// dumpEpisodes renders each episode; with seconds, every stored sample too, labeled by when it
// began relative to the episode start (pre-roll negative), with the top 3 programs by CPU and the top one by RAM.
func dumpEpisodes(t *testing.T, path string, seconds bool) string {
	db := openStore(t, path)
	var b strings.Builder
	type episode struct {
		id                      int64
		preroll, start, end     int64
		trigger                 string
		samples, prerollSamples int
	}
	var episodes []episode
	eachRow(t, db, `SELECT e.id, e.preroll_ts, e.start_ts, e.end_ts, e.trigger, count(s.ts), count(s.ts) FILTER (WHERE s.ts <= e.start_ts)
		FROM episodes e LEFT JOIN episode_seconds s ON s.episode_id = e.id GROUP BY e.id ORDER BY e.start_ts`,
		func(scan func(...any)) {
			var e episode
			scan(&e.id, &e.preroll, &e.start, &e.end, &e.trigger, &e.samples, &e.prerollSamples)
			episodes = append(episodes, e)
		})
	fmt.Fprintf(&b, "episodes: %d\n", len(episodes))
	for _, e := range episodes {
		fmt.Fprintf(&b, "episode trigger=%s pre-roll from %s, start %s, end %s (%ds), %d samples, %d before the start\n",
			e.trigger, utc(e.preroll), utc(e.start), utc(e.end), (e.end-e.start)/1000, e.samples, e.prerollSamples)
		if !seconds {
			continue
		}
		eachRow(t, db, `SELECT s.ts, s.span_ms, s.cpu_pct, s.ram_pct, s.unattributed_pct, (SELECT count(*) FROM episode_programs p WHERE p.episode_id = s.episode_id AND p.ts = s.ts)
			FROM episode_seconds s WHERE s.episode_id = ? ORDER BY s.ts`,
			func(scan func(...any)) {
				var ts, span int64
				var cpu, ram, unattributed float64
				var programs int
				scan(&ts, &span, &cpu, &ram, &unattributed, &programs)
				fmt.Fprintf(&b, "  %+4ds cpu=%.1f%% ram=%.1f%% unattributed=%.1f%% programs=%d | cpu: %s | ram: %s\n",
					(ts-span-e.start)/1000, cpu, ram, unattributed, programs, topPrograms(t, db, e.id, ts, "cpu_pct", 3), topPrograms(t, db, e.id, ts, "private_bytes", 1))
			}, e.id)
	}
	return b.String()
}

// topPrograms lists the n programs of one episode sample with the most of column.
func topPrograms(t *testing.T, db *sql.DB, id, ts int64, column string, n int) string {
	var top []string
	eachRow(t, db, fmt.Sprintf(`SELECT p.name, e.instances, e.cpu_pct, e.private_bytes FROM episode_programs e JOIN programs p ON p.id = e.program_id
		WHERE e.episode_id = ? AND e.ts = ? ORDER BY e.%s DESC, p.name LIMIT ?`, column),
		func(scan func(...any)) {
			var name string
			var instances int
			var cpu float64
			var private int64
			scan(&name, &instances, &cpu, &private)
			if instances > 1 {
				name += fmt.Sprintf(" x%d", instances)
			}
			if column == "cpu_pct" {
				top = append(top, fmt.Sprintf("%s %.1f%%", name, cpu))
			} else {
				top = append(top, fmt.Sprintf("%s %dMiB", name, private/int64(mib)))
			}
		}, id, ts, n)
	return strings.Join(top, ", ")
}

// episodeLog renders the episode lines of perfwatch.log without their timestamps.
func episodeLog(t *testing.T, path string) string {
	var b strings.Builder
	b.WriteString("log\n")
	for _, line := range readLog(t, path) {
		msg, _ := line["msg"].(string)
		if !strings.HasPrefix(msg, "episode ") {
			continue
		}
		fmt.Fprintf(&b, "  %s %s", line["level"], msg)
		for _, key := range []string{"trigger", "start", "end", "seconds", "preroll_samples"} {
			if v, ok := line[key]; ok {
				fmt.Fprintf(&b, " %s=%v", key, v)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}
