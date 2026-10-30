package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/YuukiFST/perfwatch/internal/sampler"
	"github.com/YuukiFST/perfwatch/internal/usage"
)

// sampleInterval is how often the collector reads the live system.
const sampleInterval = time.Second

// printedPrograms is how many programs a --print line names.
const printedPrograms = 5

func runRun(ctx context.Context, streams Streams, args []string) (err error) {
	fs := newFlagSet("run")
	duration := fs.Duration("for", 0, "stop once this much time has been sampled (0 runs until interrupted)")
	replayPath := fs.String("replay", "", "read snapshots from this JSON Lines fixture instead of the live system")
	recordPath := fs.String("record", "", "write every snapshot to this JSON Lines file, for a later --replay")
	printUsage := fs.Bool("print", false, "print one usage line per sample")
	if err := parseFlags(fs, streams, args); err != nil {
		return err
	}

	src, err := openSampler(*replayPath)
	if err != nil {
		return err
	}
	defer src.Close()
	var rec *sampler.Recorder
	if *recordPath != "" {
		if rec, err = sampler.CreateRecorder(*recordPath); err != nil {
			return err
		}
		defer func() { err = errors.Join(err, rec.Close()) }()
	}

	var prev sampler.Snapshot
	var started time.Time
	for {
		snap, err := src.Next(ctx)
		if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
			return nil
		}
		if err != nil {
			return err
		}
		if started.IsZero() {
			started = snap.Time
		}
		if rec != nil {
			if err := rec.Write(snap); err != nil {
				return err
			}
		}
		if !prev.Time.IsZero() && *printUsage {
			if iv, ok := usage.Between(prev, snap); ok {
				printInterval(streams.Stdout, iv)
			}
		}
		prev = snap
		if *duration > 0 && snap.Time.Sub(started) >= *duration {
			return nil
		}
	}
}

func openSampler(replayPath string) (sampler.Sampler, error) {
	if replayPath != "" {
		return sampler.OpenReplay(replayPath)
	}
	return sampler.NewLive(sampleInterval)
}

// printInterval writes one line: the end time in the snapshot's own zone, machine usage,
// and the programs that used the most CPU.
func printInterval(w io.Writer, iv usage.Interval) {
	var top []string
	for _, p := range iv.Programs {
		if len(top) == printedPrograms || p.CPUTicks == 0 {
			break
		}
		label := p.Label()
		if p.Instances > 1 {
			label += fmt.Sprintf(" x%d", p.Instances)
		}
		top = append(top, fmt.Sprintf("%s %.1f%%", label, p.CPUPercent))
	}
	fmt.Fprintf(w, "%s  cpu %.1f%%  ram %.1f%%  unattributed %.1f%%  | %s\n",
		iv.End.Format("15:04:05"), iv.CPUPercent, iv.RAMPercent, iv.UnattributedPercent, strings.Join(top, ", "))
}
