package sampler

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// maxSnapshotLine bounds one fixture line; a snapshot of a few hundred processes takes ~60 KB.
const maxSnapshotLine = 16 << 20

// Replay yields the snapshots stored in a JSON Lines fixture, one per line, without waiting.
type Replay struct {
	path    string
	file    *os.File
	scanner *bufio.Scanner
	line    int
	last    Snapshot
}

// OpenReplay opens a fixture written by Recorder or by hand.
func OpenReplay(path string) (*Replay, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open replay fixture: %w", err)
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), maxSnapshotLine)
	return &Replay{path: path, file: f, scanner: scanner}, nil
}

// Next returns the fixture's next snapshot, or io.EOF after the last one.
func (r *Replay) Next(ctx context.Context) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if !r.scanner.Scan() {
		if err := r.scanner.Err(); err != nil {
			return Snapshot{}, fmt.Errorf("%s:%d: %w", r.path, r.line+1, err)
		}
		return Snapshot{}, io.EOF
	}
	r.line++
	var snap Snapshot
	if err := json.Unmarshal(r.scanner.Bytes(), &snap); err != nil {
		return Snapshot{}, fmt.Errorf("%s:%d: %w", r.path, r.line, err)
	}
	if err := r.validate(snap); err != nil {
		return Snapshot{}, fmt.Errorf("%s:%d: %w", r.path, r.line, err)
	}
	r.last = snap
	return snap, nil
}

func (r *Replay) validate(snap Snapshot) error {
	switch {
	case snap.Time.IsZero():
		return fmt.Errorf("snapshot has no time")
	case r.line > 1 && !snap.Time.After(r.last.Time):
		return fmt.Errorf("snapshot time %s is not after the previous one", snap.Time.Format("2006-01-02T15:04:05Z07:00"))
	case snap.Memory.TotalBytes == 0 || snap.Memory.AvailableBytes > snap.Memory.TotalBytes:
		return fmt.Errorf("memory needs 0 < available_bytes <= total_bytes")
	}
	return nil
}

// Close releases the fixture file.
func (r *Replay) Close() error {
	return r.file.Close()
}

// Recorder writes snapshots as JSON Lines that OpenReplay reads back.
type Recorder struct {
	file *os.File
	buf  *bufio.Writer
	enc  *json.Encoder
}

// CreateRecorder creates or truncates the fixture at path.
func CreateRecorder(path string) (*Recorder, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("create recording: %w", err)
	}
	buf := bufio.NewWriter(f)
	return &Recorder{file: f, buf: buf, enc: json.NewEncoder(buf)}, nil
}

// Write appends one snapshot.
func (r *Recorder) Write(snap Snapshot) error {
	if err := r.enc.Encode(snap); err != nil {
		return fmt.Errorf("record snapshot: %w", err)
	}
	return nil
}

// Close flushes buffered snapshots and closes the file.
func (r *Recorder) Close() error {
	flushErr := r.buf.Flush()
	closeErr := r.file.Close()
	if flushErr != nil {
		return fmt.Errorf("flush recording: %w", flushErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close recording: %w", closeErr)
	}
	return nil
}

var _ Sampler = (*Replay)(nil)
