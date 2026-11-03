// Package logfile is an append-only log file that rotates by size: when a write would take it
// past MaxBytes, perfwatch.log becomes perfwatch.log.1, .1 becomes .2, and so on up to Keep.
package logfile

import (
	"fmt"
	"os"
	"sync"
)

const (
	// MaxBytes is the size a log file reaches before it rotates.
	MaxBytes = 5 << 20
	// Keep is how many rotated files stay next to the current one.
	Keep = 3
)

// File is a rotating log file; it is safe for concurrent writes.
type File struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

// Open opens path for appending, creating it if needed.
func Open(path string) (*File, error) {
	l := &File{path: path}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *File) open() error {
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("open log: %w", err)
	}
	l.f, l.size = f, info.Size()
	return nil
}

// Write appends p, rotating first when p would take the file past MaxBytes.
func (l *File) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size > 0 && l.size+int64(len(p)) > MaxBytes {
		if err := l.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

func (l *File) rotate() error {
	if err := l.f.Close(); err != nil {
		return fmt.Errorf("rotate log: %w", err)
	}
	for i := Keep - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", l.path, i), fmt.Sprintf("%s.%d", l.path, i+1)) // a missing file is fine
	}
	// Reopen even when the rename fails, so logging goes on in the oversized file.
	renameErr := os.Rename(l.path, l.path+".1")
	if err := l.open(); err != nil {
		return err
	}
	if renameErr != nil {
		return fmt.Errorf("rotate log: %w", renameErr)
	}
	return nil
}

// Close closes the file.
func (l *File) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
