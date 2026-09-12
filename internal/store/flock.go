package store

import (
	"os"
	"path/filepath"
)

// WithFileLock runs fn while holding an exclusive lock on <path>.lock — the
// cross-PROCESS guard for state files the daemon and one-shot CLI both touch.
func WithFileLock(path string, fn func() error) error {
	lockPath := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := lockFile(f.Fd()); err != nil {
		return err
	}
	defer unlockFile(f.Fd())
	return fn()
}
