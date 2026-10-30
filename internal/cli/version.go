package cli

import (
	"context"
	"fmt"
	"runtime"
	"runtime/debug"
)

func runVersion(_ context.Context, streams Streams, args []string) error {
	fs := newFlagSet("version", streams)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	fmt.Fprintf(streams.Stdout, "perfwatch %s (%s %s/%s)\n", buildVersion(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return nil
}

// buildVersion returns the module version go build stamped from the VCS state, or "dev".
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return "dev"
	}
	return info.Main.Version
}
