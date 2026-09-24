package store

import (
	"io/fs"
	"runtime"
)

func PermTooOpen(mode fs.FileMode) bool {
	if runtime.GOOS == "windows" {
		return false
	}
	return mode.Perm()&0o077 != 0
}
