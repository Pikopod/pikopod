//go:build windows

package store

import "golang.org/x/sys/windows"

// lockFile takes an exclusive whole-file LockFileEx (blocking), unlockFile
// releases it — the Windows equivalent of the Unix advisory flock.
func lockFile(fd uintptr) error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(windows.Handle(fd), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, ol)
}

func unlockFile(fd uintptr) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(fd), 0, 1, 0, ol)
}
