package sandbox

import (
	"crypto/rand"
	"encoding/hex"
)

// newResourceID mints res_<hex> ids; uniqueness only, no meaning.
func newResourceID() string {
	b := make([]byte, 10)
	rand.Read(b)
	return "res_" + hex.EncodeToString(b)
}
