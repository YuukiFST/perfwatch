//go:build !windows

package sampler

import (
	"fmt"
	"runtime"
	"time"
)

// NewLive reports that this platform has no live backend yet.
func NewLive(time.Duration) (Sampler, error) {
	return nil, fmt.Errorf("live sampling is not implemented on %s; use --replay", runtime.GOOS)
}
