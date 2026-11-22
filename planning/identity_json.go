package planning

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	json "github.com/kart-io/goagent/codec/json"
)

func invalid(format string, args ...any) error {
	reason := fmt.Sprintf(format, args...)
	return &CandidateError{Reason: boundedText(reason)}
}
func boundedText(s string) string {
	const limit = 1024
	if len(s) <= limit {
		return s
	}
	return strings.ToValidUTF8(s[:limit], "")
}

// DecodeCandidate decodes exactly one strictly typed model response.
func DecodeCandidate(data []byte, dst any) error {
	if err := json.Unmarshal(data, dst, json.Strict, json.UseNumber); err != nil {
		return invalid("候选JSON无效：%v", err)
	}
	return nil
}
func canonicalJSON(data []byte) ([]byte, error) {
	var v any
	if err := DecodeCandidate(data, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// SourceDigest computes the SHA-256 digest of a canonical JSON source body.
func SourceDigest(data []byte) (string, error) {
	b, err := canonicalJSON(data)
	if err != nil {
		return "", err
	}
	d := sha256.Sum256(b)
	return hex.EncodeToString(d[:]), nil
}
func digestJSON(v any) (string, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return "", e
	}
	return SourceDigest(b)
}
func artifactDigest(a Artifact) (string, error) {
	b, e := canonicalJSON(a.Content)
	if e != nil {
		return "", e
	}
	return digestJSON(struct {
		Content   json.RawMessage `json:"content"`
		MediaType string          `json:"media_type"`
		Sources   []EvidenceRef   `json:"sources"`
	}{b, a.MediaType, a.Sources})
}
