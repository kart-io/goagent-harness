package sandbox

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
)

// The inner shell starts only after the OS runner has applied its policy. Its
// private marker distinguishes startup failures from any command exit code or
// diagnostic text, without interpreting model-controlled arguments as shell code.
func commandStart(argv []string) ([]string, []byte, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, nil, err
	}
	marker := "harness-command-start-" + hex.EncodeToString(nonce[:])
	wrapped := []string{"/bin/sh", "-c", "printf '%s\\n' \"$1\" >&2; shift; exec \"$@\"", "harness-run", marker}
	return append(wrapped, argv...), []byte(marker + "\n"), nil
}

func removeCommandStart(output, marker []byte) ([]byte, bool) {
	index := bytes.Index(output, marker)
	if index < 0 {
		return output, false
	}
	return append(output[:index:index], output[index+len(marker):]...), true
}
