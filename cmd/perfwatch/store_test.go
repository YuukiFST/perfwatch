package main_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestStoreRollupsAndRetention replays a machine with 22 programs across a minute boundary,
// then jumps 8, 100 and 400 days ahead so each retention window expires in turn.
func TestStoreRollupsAndRetention(t *testing.T) {
	dataDir := t.TempDir()
	env := []string{"PERFWATCH_DATA_DIR=" + dataDir}
	replay := func(fixture string) {
		t.Helper()
		if res := runPerfwatch(t, env, "run", "--replay", fixture); res.exitCode != 0 {
			t.Fatalf("run --replay failed:\n%s", res.transcript())
		}
	}

	m := newMachine(time.Date(2026, 1, 5, 10, 0, 55, 0, time.UTC))
	// p01..p20 rank by CPU and, except p20, by RAM; p21 has the least CPU and RAM of all,
	// so it misses both top 20s; p22 uses no CPU but the most RAM.
	shares := map[string]float64{"p21.exe": 0.1}
	for i := 1; i <= 20; i++ {
		name := fmt.Sprintf("p%02d.exe", i)
		shares[name] = float64(21-i) * 0.2
		m.spawn(fixtureProcess{PID: uint32(1000 + i), Name: name, PrivateBytes: uint64(25-i) * 10 * mib})
	}
	m.spawn(fixtureProcess{PID: 1021, Name: "p21.exe", PrivateBytes: 1 * mib})
	m.spawn(fixtureProcess{PID: 1022, Name: "p22.exe", PrivateBytes: 900 * mib})
	busyBase := 2.0 // unattributed
	for _, s := range shares {
		busyBase += s
	}
	for k := range 10 {
		m.ramPct = 50 + float64(k)
		shares["p01.exe"] = 4 + float64(k)
		m.step(time.Second, busyBase+float64(k), shares)
	}
	day0 := m.write(t, "day0")

	db := filepath.Join(dataDir, "perfwatch.db")
	replay(day0)
	assertGolden(t, "store-rollups", dumpRollups(t, db))

	var states strings.Builder
	states.WriteString(storeState(t, db, "after day 0"))
	replay(day0)
	states.WriteString(storeState(t, db, "after replaying day 0 again"))
	for _, days := range []int{8, 100, 400} {
		m.jump(time.Duration(days)*24*time.Hour - m.now.Sub(time.Date(2026, 1, 5, 10, 1, 5, 0, time.UTC)))
		m.step(time.Second, busyBase, shares)
		m.step(time.Second, busyBase, shares)
		replay(m.write(t, fmt.Sprintf("day%d", days)))
		states.WriteString(storeState(t, db, fmt.Sprintf("after day %d", days)))
	}
	assertGolden(t, "store-retention", states.String())
}

func openStore(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=query_only(1)")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func utc(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05Z")
}

// dumpRollups renders every stored sample and rollup as one labeled line each.
func dumpRollups(t *testing.T, path string) string {
	db := openStore(t, path)
	var b strings.Builder
	b.WriteString("system_seconds\n")
	eachRow(t, db, `SELECT ts, span_ms, cpu_pct, ram_pct, unattributed_pct, ram_used_bytes FROM system_seconds ORDER BY ts`,
		func(scan func(...any)) {
			var ts, span, ramUsed int64
			var cpu, ram, unattributed float64
			scan(&ts, &span, &cpu, &ram, &unattributed, &ramUsed)
			fmt.Fprintf(&b, "  %s span=%dms cpu=%.1f%% ram=%.1f%% unattributed=%.1f%% ram_used=%.2fGiB\n",
				utc(ts), span, cpu, ram, unattributed, float64(ramUsed)/float64(gib))
		})
	b.WriteString("system_minutes\n")
	eachRow(t, db, `SELECT minute, samples, capacity_ticks, busy_ticks, unattributed_ticks,
		cpu_avg, cpu_max, cpu_p95, ram_avg, ram_max, ram_p95 FROM system_minutes ORDER BY minute`,
		func(scan func(...any)) {
			var minute, samples, capacity, busy, unattributed int64
			var cpuAvg, cpuMax, cpuP95, ramAvg, ramMax, ramP95 float64
			scan(&minute, &samples, &capacity, &busy, &unattributed, &cpuAvg, &cpuMax, &cpuP95, &ramAvg, &ramMax, &ramP95)
			fmt.Fprintf(&b, "  %s samples=%d capacity=%.1fs busy=%.2fs unattributed=%.2fs cpu avg/max/p95=%.2f/%.1f/%.1f%% ram avg/max/p95=%.2f/%.1f/%.1f%%\n",
				utc(minute), samples, seconds(capacity), seconds(busy), seconds(unattributed), cpuAvg, cpuMax, cpuP95, ramAvg, ramMax, ramP95)
		})
	b.WriteString("program_minutes\n")
	eachRow(t, db, `SELECT pm.minute, p.name, p.service_group, pm.cpu_ticks, pm.cpu_max_pct, pm.private_max_bytes, pm.instances_max
		FROM program_minutes pm JOIN programs p ON p.id = pm.program_id ORDER BY pm.minute, p.name`,
		func(scan func(...any)) {
			var minute, ticks, private, instances int64
			var name, group string
			var cpuMax float64
			scan(&minute, &name, &group, &ticks, &cpuMax, &private, &instances)
			fmt.Fprintf(&b, "  %s %s cpu=%.2fs cpu_max=%.1f%% private_max=%dMiB instances=%d\n",
				utc(minute), name, seconds(ticks), cpuMax, private/int64(mib), instances)
		})
	return b.String()
}

// storeState renders each table's row count and time range.
func storeState(t *testing.T, path, label string) string {
	db := openStore(t, path)
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", label)
	for _, table := range []struct{ name, column string }{
		{"system_seconds", "ts"}, {"system_minutes", "minute"}, {"program_minutes", "minute"},
	} {
		var rows int
		var oldest, newest sql.NullInt64
		query := fmt.Sprintf("SELECT count(*), min(%s), max(%s) FROM %s", table.column, table.column, table.name)
		if err := db.QueryRow(query).Scan(&rows, &oldest, &newest); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		span := "empty"
		if rows > 0 {
			span = utc(oldest.Int64) + " .. " + utc(newest.Int64)
		}
		fmt.Fprintf(&b, "  %-15s %3d rows  %s\n", table.name, rows, span)
	}
	var programs int
	if err := db.QueryRow("SELECT count(*) FROM programs").Scan(&programs); err != nil {
		t.Fatalf("count programs: %v", err)
	}
	fmt.Fprintf(&b, "  %-15s %3d rows\n", "programs", programs)
	return b.String()
}

func eachRow(t *testing.T, db *sql.DB, query string, row func(scan func(...any))) {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	for rows.Next() {
		row(func(dest ...any) {
			if err := rows.Scan(dest...); err != nil {
				t.Fatalf("scan: %v", err)
			}
		})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func seconds(ticks int64) float64 {
	return float64(ticks) / 10_000_000
}
