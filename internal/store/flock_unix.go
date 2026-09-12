//go:build unix

package store

import "syscall"

// lockFile takes an exclusive advisory flock (blocking), unlockFile releases
// it; advisory is enough because every writer goes through WithFileLock.
func lockFile(fd uintptr) error   { return syscall.Flock(int(fd), syscall.LOCK_EX) }
func unlockFile(fd uintptr) error { return syscall.Flock(int(fd), syscall.LOCK_UN) }
