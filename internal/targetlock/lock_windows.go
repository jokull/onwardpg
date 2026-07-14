//go:build windows

package targetlock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func tryLockFile(file *os.File) (bool, error) {
	// Lock one byte far beyond the TOML payload so ordinary config reads do not
	// overlap the lifecycle mutex range.
	overlapped := &windows.Overlapped{OffsetHigh: 0x80000000}
	err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, overlapped,
	)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}

func unlockFile(file *os.File) error {
	overlapped := &windows.Overlapped{OffsetHigh: 0x80000000}
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped)
}
