//go:build !windows

package cli

import (
	"context"
	"fmt"
	"runtime"
)

func detachLoneConsole() {}

// watchStop has no stop event to watch here: Ctrl+C and signals stop the collector.
func watchStop(ctx context.Context, _ string) (context.Context, func(), error) {
	return ctx, func() {}, nil
}

func inService() bool { return false }

func runAsService(func(ctx context.Context) error) error {
	return fmt.Errorf("services are a Windows feature, not available on %s", runtime.GOOS)
}
