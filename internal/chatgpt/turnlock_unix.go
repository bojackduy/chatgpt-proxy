//go:build darwin || linux

package chatgpt

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func tryTurnLock(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func turnLockContended(err error) bool {
	return errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN)
}
