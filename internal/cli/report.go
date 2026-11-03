package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/YuukiFST/perfwatch/internal/store"
)

// nowEnv fixes the time the reading commands treat as now, and shows their times in UTC, so
// tests get the same output on any day and in any time zone. Users leave it unset.
const nowEnv = "PERFWATCH_NOW"

// clock returns now and the zone times are shown in.
func clock() (time.Time, *time.Location, error) {
	v := os.Getenv(nowEnv)
	if v == "" {
		return time.Now(), time.Local, nil
	}
	now, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, nil, fmt.Errorf("%s: %w", nowEnv, err)
	}
	return now, time.UTC, nil
}

// sinceFlag is a duration that also takes whole days, as in 7d.
type sinceFlag struct{ d time.Duration }

func (f *sinceFlag) String() string {
	if f.d > 24*time.Hour && f.d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", f.d/(24*time.Hour))
	}
	return strings.TrimSuffix(strings.TrimSuffix(f.d.String(), "0s"), "0m")
}

func (f *sinceFlag) Set(s string) error {
	d, err := parseSince(s)
	if err != nil {
		return err
	}
	f.d = d
	return nil
}

func parseSince(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 || n > int(math.MaxInt64/int64(24*time.Hour)) {
			return 0, fmt.Errorf("want a positive whole number of days, as in 7d")
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("want a positive duration such as 90m, 24h or 7d")
	}
	return d, nil
}

// openForReading opens the database run writes, without creating one when run never ran.
func openForReading(ctx context.Context) (*store.DB, error) {
	dir, err := dataDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, store.FileName)
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no data in %s yet: perfwatch run collects it", dir)
	}
	return store.Open(ctx, path)
}

func percentOf(part, whole int64) float64 {
	if whole == 0 {
		return 0
	}
	return float64(part) * 100 / float64(whole)
}

// round2 keeps two decimals, so JSON output reads as the tables do.
func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

func formatBytes(b int64) string {
	const mib = 1 << 20
	if b >= 1<<30 {
		return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
	}
	return fmt.Sprintf("%.0f MiB", float64(b)/mib)
}

func label(name, group string) string {
	if group == "" {
		return name
	}
	return name + " (" + group + ")"
}
