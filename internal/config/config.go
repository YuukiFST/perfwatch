// Package config reads perfwatch.toml from the data directory; a missing file means defaults.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// FileName is the config file inside the data directory.
const FileName = "perfwatch.toml"

// MaxPreRollSeconds bounds the pre-roll ring, which the collector keeps in memory.
const MaxPreRollSeconds = 300

// Config is the whole perfwatch.toml.
type Config struct {
	Peaks Peaks `toml:"peaks"`
}

// Peaks decides when a peak episode starts and ends. A sample is hot when CPU or RAM reaches
// its start threshold, and cool when both are below their end thresholds.
type Peaks struct {
	CPUStartPercent float64 `toml:"cpu_start_percent"`
	CPUEndPercent   float64 `toml:"cpu_end_percent"`
	RAMStartPercent float64 `toml:"ram_start_percent"`
	RAMEndPercent   float64 `toml:"ram_end_percent"`
	// StartSamples consecutive hot samples start an episode; EndSamples consecutive cool ones end it.
	StartSamples   int `toml:"start_samples"`
	EndSamples     int `toml:"end_samples"`
	PreRollSeconds int `toml:"preroll_seconds"`
}

// PreRoll is how much history before its start an episode keeps.
func (p Peaks) PreRoll() time.Duration {
	return time.Duration(p.PreRollSeconds) * time.Second
}

// Default is the config used when perfwatch.toml is absent, and the base a file overrides.
func Default() Config {
	return Config{Peaks: Peaks{
		CPUStartPercent: 85, CPUEndPercent: 75,
		RAMStartPercent: 90, RAMEndPercent: 85,
		StartSamples: 3, EndSamples: 10, PreRollSeconds: 30,
	}}
}

// Load reads path over the defaults. An unknown key or an impossible threshold is an error,
// so a typo never silently falls back to a default.
func Load(path string) (Config, error) {
	cfg := Default()
	md, err := toml.DecodeFile(path, &cfg)
	if errors.Is(err, fs.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	if keys := md.Undecoded(); len(keys) > 0 {
		names := make([]string, len(keys))
		for i, k := range keys {
			names[i] = k.String()
		}
		return Config{}, fmt.Errorf("read %s: unknown key %s", path, strings.Join(names, ", "))
	}
	if err := cfg.Peaks.validate(); err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	return cfg, nil
}

func (p Peaks) validate() error {
	for _, t := range []struct {
		name       string
		start, end float64
	}{{"cpu", p.CPUStartPercent, p.CPUEndPercent}, {"ram", p.RAMStartPercent, p.RAMEndPercent}} {
		// Written as what is accepted, so a NaN, which fails every comparison, is rejected.
		if !(0 < t.end && t.end <= t.start && t.start <= 100) {
			return fmt.Errorf("peaks: want 0 < %s_end_percent <= %s_start_percent <= 100, got %g and %g", t.name, t.name, t.end, t.start)
		}
	}
	if p.StartSamples < 1 || p.EndSamples < 1 || p.PreRollSeconds < 0 || p.PreRollSeconds > MaxPreRollSeconds {
		return fmt.Errorf("peaks: want start_samples and end_samples >= 1 and 0 <= preroll_seconds <= %d", MaxPreRollSeconds)
	}
	return nil
}
