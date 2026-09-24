package sandbox

import (
	"crypto/rand"
	"encoding/hex"
)

func newResourceID() string {
	b := make([]byte, 10)
	rand.Read(b)
	return "res_" + hex.EncodeToString(b)
}
