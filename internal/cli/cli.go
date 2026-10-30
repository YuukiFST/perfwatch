// Package cli routes perfwatch subcommands and maps their errors to exit codes.
package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
)

// Streams carries the writers a command prints to.
type Streams struct {
	Stdout io.Writer
	Stderr io.Writer
}

// Exit codes returned by Main.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// errUsage marks bad arguments; the command already printed what was wrong.
var errUsage = errors.New("usage error")

type command struct {
	name    string
	summary string
	run     func(ctx context.Context, streams Streams, args []string) error
}

func commands() []command {
	return []command{
		{name: "version", summary: "print the perfwatch version", run: runVersion},
	}
}

// Main runs the subcommand named by args[0] and returns the process exit code:
// 0 on success, 1 when the command fails, 2 on a usage error.
func Main(ctx context.Context, args []string, streams Streams) int {
	if len(args) == 0 {
		printUsage(streams.Stderr)
		return exitUsage
	}
	name := args[0]
	if name == "help" || name == "-h" || name == "--help" {
		printUsage(streams.Stdout)
		return exitOK
	}
	for _, cmd := range commands() {
		if cmd.name == name {
			return exitCode(cmd.run(ctx, streams, args[1:]), streams.Stderr)
		}
	}
	fmt.Fprintf(streams.Stderr, "perfwatch: unknown command %q\n\n", name)
	printUsage(streams.Stderr)
	return exitUsage
}

func exitCode(err error, stderr io.Writer) int {
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return exitOK
	case errors.Is(err, errUsage):
		return exitUsage
	default:
		fmt.Fprintf(stderr, "perfwatch: %v\n", err)
		return exitError
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, "Usage: perfwatch <command> [flags]\n\nCommands:\n")
	for _, cmd := range commands() {
		fmt.Fprintf(w, "  %-10s %s\n", cmd.name, cmd.summary)
	}
	fmt.Fprint(w, "\nRun \"perfwatch <command> -h\" for the flags of a command.\n")
}

// newFlagSet returns a flag set that reports parse errors instead of exiting.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("perfwatch "+name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: %s [flags]\n", fs.Name())
		fs.PrintDefaults()
	}
	return fs
}

// parseFlags parses args and rejects positional arguments, which no command takes.
// Help asked for with -h goes to stdout; usage printed for a bad argument goes to stderr.
func parseFlags(fs *flag.FlagSet, streams Streams, args []string) error {
	var msg bytes.Buffer
	fs.SetOutput(&msg)
	err := fs.Parse(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		io.Copy(streams.Stdout, &msg)
		return err
	case err != nil:
		io.Copy(streams.Stderr, &msg)
		return errUsage
	case fs.NArg() > 0:
		fmt.Fprintf(streams.Stderr, "%s: unexpected argument %q\n", fs.Name(), fs.Arg(0))
		fs.SetOutput(streams.Stderr)
		fs.Usage()
		return errUsage
	}
	return nil
}
