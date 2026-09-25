//go:build windows

package connect

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// tryLock takes LockFileEx(LOCKFILE_EXCLUSIVE_LOCK|LOCKFILE_FAIL_IMMEDIATELY) on the first byte
// of f; the lock belongs to this handle and is released when it is closed.
func tryLock(f *os.File) error {
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return ErrLocked
	}
	return err
}

// unlockFile unlocks the byte before closing: Windows may release a closed handle's lock lazily.
func unlockFile(f *os.File) error {
	err := windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
