package v2

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"strings"
)

// What the collector tells the hub about the build it runs, on every
// door call beside x-collector-version. Self-reported telemetry, for
// honest operators (docs/THREAT-MODEL.md): an operator can send anything.
//
// The release key alone proves nothing - it is in the source, so a dev
// build or a fork carries it too. What proves a signed release is the
// binary's hash matching the signed hash the hub holds for that version
// and platform from naming it; the key says which key this build trusts.
const (
	headerBinarySHA256 = "x-collector-binary-sha256"
	headerReleaseKey   = "x-collector-release-key"
)

// BinarySHA256 is the SHA-256 of the file at path, or of the running
// executable's real file when path is "". Streamed, never read whole, so
// a NAS does not notice; computed once at startup, since a self-update
// restarts the process anyway.
func BinarySHA256(path string) (string, error) {
	if path == "" {
		self, err := os.Executable()
		if err != nil {
			return "", err
		}
		path = realPath(self)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ReleaseKeyFingerprints is the SHA256: fingerprint of each compiled-in
// release key, comma-separated (two during a rotation), or "" when this
// build carries no usable key.
func ReleaseKeyFingerprints() string { return keyFingerprints(releasePublicKeys) }

func keyFingerprints(s string) string {
	keys, err := parseReleaseKeys(s)
	if err != nil {
		return ""
	}
	fps := make([]string, len(keys))
	for i, k := range keys {
		fps[i] = k.Fingerprint
	}
	return strings.Join(fps, ",")
}

// releaseKeyHeader is ReleaseKeyFingerprints for this client (a test's
// keys when set), computed once.
func (c *Client) releaseKeyHeader() string {
	if !c.keyFPDone {
		s := c.releaseKeys
		if s == "" {
			s = releasePublicKeys
		}
		c.keyFP, c.keyFPDone = keyFingerprints(s), true
	}
	return c.keyFP
}
