// Package lock takes an exclusive, process-lifetime lock on a file, so only one perfwatch
// collector writes to a data directory. The OS drops the lock when the process exits, even
// on a hard kill, so a stale lock file never blocks the next start.
package lock

import "errors"

// ErrHeld means another process holds the lock.
var ErrHeld = errors.New("held by another process")

// Lock is a held lock; Release frees it.
type Lock struct {
	release func() error
}

// Release frees the lock. The lock file stays, so the next Acquire reuses it.
func (l *Lock) Release() error {
	return l.release()
}
