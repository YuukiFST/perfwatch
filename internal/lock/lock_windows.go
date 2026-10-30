package lock

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// Acquire opens path with no sharing, creating it if needed. While the handle is open, every
// other open of the file fails with ERROR_SHARING_VIOLATION (CreateFileW, dwShareMode 0).
func Acquire(path string) (*Lock, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	h, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return nil, ErrHeld
	}
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return &Lock{release: func() error { return windows.CloseHandle(h) }}, nil
}
