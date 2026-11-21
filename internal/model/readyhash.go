package model

import (
	"crypto/sha256"
	"encoding/hex"
)

// ReadyHash is the hash of a task's title and body that F29 stores when the
// user marks the task ready: auto-spawn starts it only while the text still
// hashes to the stored value.
func ReadyHash(title, body string) string {
	sum := sha256.Sum256([]byte(title + "\x00" + body))
	return hex.EncodeToString(sum[:])
}
