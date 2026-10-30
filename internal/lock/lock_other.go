//go:build !windows && (!unix || aix || solaris)

package lock

import (
	"fmt"
	"runtime"
)

// Acquire fails: this platform has no lock implementation yet.
func Acquire(path string) (*Lock, error) {
	return nil, fmt.Errorf("lock %s: not implemented on %s", path, runtime.GOOS)
}
