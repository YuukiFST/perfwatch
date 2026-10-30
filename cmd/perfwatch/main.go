// Command perfwatch records CPU and RAM usage and ranks the processes behind each peak.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/YuukiFST/perfwatch/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := cli.Main(ctx, os.Args[1:], cli.Streams{Stdout: os.Stdout, Stderr: os.Stderr})
	stop()
	os.Exit(code)
}
