// Command perfwatch records CPU and RAM usage and ranks the processes behind each peak.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/YuukiFST/perfwatch/internal/cli"
)

func main() {
	// On Windows, Go delivers console close, logoff and shutdown as SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Main(ctx, os.Args[1:], cli.Streams{Stdout: os.Stdout, Stderr: os.Stderr})
	stop()
	os.Exit(code)
}
