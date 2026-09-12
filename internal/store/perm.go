package store

import (
	"io/fs"
	"runtime"
)

// PermTooOpen reports whether a secret-carrying file's mode grants group or
// other access. Skipped on Windows, where NTFS ACLs are the isolation.
func PermTooOpen(mode fs.FileMode) bool {
	if runtime.GOOS == "windows" {
		return false
	}
	return mode.Perm()&0o077 != 0
}
