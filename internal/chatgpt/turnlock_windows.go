//go:build windows

package chatgpt

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func tryTurnLock(f *os.File) error {
	// Lock the same first byte for every holder. Windows releases the lock
	// on handle close or process exit, just like flock on Unix.
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}

func turnLockContended(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
