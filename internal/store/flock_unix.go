//go:build unix

package store

import "syscall"

func lockFile(fd uintptr) error   { return syscall.Flock(int(fd), syscall.LOCK_EX) }
func unlockFile(fd uintptr) error { return syscall.Flock(int(fd), syscall.LOCK_UN) }
