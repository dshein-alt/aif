//go:build !windows

package connect

import (
	"errors"
	"os"
	"syscall"
)

// tryLock takes flock(LOCK_EX|LOCK_NB) on f; the lock belongs to this open file, so a second
// open of the same file fails even within one process.
func tryLock(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrLocked
	}
	return err
}

// unlockFile releases the lock; closing the file drops the flock at once.
func unlockFile(f *os.File) error { return f.Close() }
