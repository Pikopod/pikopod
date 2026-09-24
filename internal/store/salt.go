package store

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"

	"github.com/pikopod/pikopod/internal/errfmt"
)

func LoadOrCreateSalt(path string) (string, error) {
	if info, statErr := os.Stat(path); statErr == nil && PermTooOpen(info.Mode()) {
		return "", errfmt.New("tokenization salt is readable by other users", path+" must be 0600 — group/other access lets another local user correlate tokens", "chmod 600 "+path, "docs/security.md#salt")
	}
	raw, err := os.ReadFile(path)
	if err == nil {
		if len(raw) < 32 {
			return "", errfmt.New("tokenization salt is corrupt", path+" is shorter than expected", "delete the file to regenerate (existing tokens will no longer correlate) or restore it from your secure copy", "docs/security.md#salt")
		}
		return string(raw), nil
	}
	if !os.IsNotExist(err) {
		return "", errfmt.Newf("cannot read tokenization salt", "check permissions on "+path, "docs/security.md#salt", "%v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", errfmt.Newf("cannot create data directory", "check permissions on "+filepath.Dir(path), "docs/config-reference.md#data_dir", "%v", err)
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", errfmt.Newf("cannot generate tokenization salt", "the OS random source failed; retry", "docs/security.md#salt", "%v", err)
	}
	salt := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(salt), 0o600); err != nil {
		return "", errfmt.Newf("cannot write tokenization salt", "check permissions on "+path, "docs/security.md#salt", "%v", err)
	}
	return salt, nil
}
