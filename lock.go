package nteedb

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const lockFile = "LOCK"

// ErrLocked is returned by Open when another process already holds the store.
// Callers that treat the store as a best-effort cache can degrade to running
// without it (the second process simply skips caching).
var ErrLocked = errors.New("nteedb: store is locked by another process")

// acquireLock takes an exclusive, non-blocking kernel lock (flock) on the
// store's LOCK file, enforcing a single writer process per store.
//
// The lock is owned by the kernel and tied to the returned file descriptor —
// NOT to the file's presence on disk — so it is released automatically the
// instant the process dies for any reason (Ctrl+C, kill -9, crash). There is
// no stale-lock state to clean up, and the LOCK file itself is deliberately
// never deleted: it is meaningless without the kernel lock, and leaving it in
// place avoids unlink/recreate races between competing openers.
func acquireLock(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("nteedb: locking store: %w", err)
	}
	return f, nil
}
