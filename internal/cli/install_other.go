//go:build !windows

package cli

import (
	"context"
	"fmt"
	"runtime"
)

func runInstall(_ context.Context, _ Streams, _ []string) error {
	return fmt.Errorf("install registers Windows autostart, not available on %s", runtime.GOOS)
}

func runUninstall(_ context.Context, _ Streams, _ []string) error {
	return fmt.Errorf("uninstall removes Windows autostart, not available on %s", runtime.GOOS)
}
