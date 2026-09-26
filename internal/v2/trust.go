package v2

// The release trust chain (issue #5). The hub is the update authority:
// it names the version, SHA-256 and URL this platform installs. On top
// of that, and before anything runs the download, a release must prove
// who published it:
//
//  1. The URL is this repository's release asset for exactly the named
//     version and this platform (checkUpdateURL); a redirect may only
//     go to GitHub's asset hosts (updateRedirect).
//  2. The release's SHA256SUMS carries an OpenSSH signature (SSHSIG,
//     ed25519) by a key compiled into this binary (releasekey.go), and
//     covers both the hub-named hash for this platform's asset and a
//     VERSION line for the named version (verifyRelease). The VERSION
//     line is what stops a signed SHA256SUMS being replayed under
//     another version's name.
//  3. The download matches the hub-named hash.
//
// Only then does installBinary run the self-check (fallback.go), which
// executes the file. Every check is in addition to the hub's; none
// replaces it, and a failure is an ordinary failed update: collection
// carries on and the next hourly config call tries again.
//
// Verification is crypto/ed25519 and crypto/sha512 over the SSHSIG wire
// format, so the module keeps its zero third-party dependencies, and
// operators and CI can check the very same signature with stock
// `ssh-keygen -Y verify`.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// signatureNamespace is the SSHSIG namespace every release is signed
// under (`ssh-keygen -Y sign -n`), so a signature made with the same key
// for any other purpose never verifies as a release.
const signatureNamespace = "elixir-mcp-collector-release"

// releaseDownloadPath is where this repository's release assets live on
// github.com; a hub-named URL must be exactly <this><version>/<asset>.
const releaseDownloadPath = "/jthingelstad/elixir-mcp-collector/releases/download/"

// assetHosts are where github.com redirects a release download. Their
// URLs are short-lived signed links, so only the host is checked.
var assetHosts = map[string]bool{
	"objects.githubusercontent.com":        true,
	"release-assets.githubusercontent.com": true,
}

// Bounds for the signed metadata; SHA256SUMS is under 1 KB.
const (
	maxSumsBytes = 64 << 10
	maxSigBytes  = 16 << 10
)

// installFloor is the oldest release self-update will install, however
// validly signed and whatever the hub names. Naming an older release is
// the hub's rollback lever and keeps working above it; below it are
// releases the door no longer serves (its min_client_version). This is
// the version-monotonicity rule: it only ever moves up, in a reviewed
// change carried by a signed release - raise it when a release fixes a
// security problem, so a replayed older one cannot bring it back. A var
// so the tests can use small version numbers.
var installFloor = "v2.0.30"

// versionRe is the shape of a release version: vMAJOR.MINOR.PATCH with an
// optional -suffix (the systemd test's smoke builds). It is also what
// keeps a version safe to put in a URL path.
var versionRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)(-[0-9A-Za-z.]+)?$`)

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// parseVersion reads vMAJOR.MINOR.PATCH; ok is false for anything else
// ("dev", a garbled string).
func parseVersion(v string) (n [3]int, ok bool) {
	m := versionRe.FindStringSubmatch(v)
	if m == nil {
		return n, false
	}
	for i := range n {
		x, err := strconv.Atoi(m[i+1])
		if err != nil {
			return n, false
		}
		n[i] = x
	}
	return n, true
}

// compareVersions is -1, 0 or 1 as a is older than, the same as, or
// newer than b, numerically, the way the hub's min_client_version does.
func compareVersions(a, b [3]int) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// releaseAsset is the release asset this platform installs: the name
// release.yml builds, which is not always GOOS_GOARCH (armv7 is GOARCH
// arm, and Windows carries .exe).
func releaseAsset(goos, goarch string) string {
	arch := goarch
	if arch == "arm" {
		arch = "armv7"
	}
	name := "collector_" + goos + "_" + arch
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// releaseKey is one trusted signing key.
type releaseKey struct {
	pub  ed25519.PublicKey
	blob []byte // SSH wire encoding, as it appears inside a signature
	// Fingerprint is ssh-keygen's SHA256:... form, for the log.
	Fingerprint string
}

// parseReleaseKeys reads OpenSSH public key lines ("ssh-ed25519 AAAA...
// comment"). Blank lines and # comments are skipped; anything else that
// is not an ed25519 key is an error, so a placeholder or a garbled key
// never silently trusts nothing-in-particular.
func parseReleaseKeys(s string) ([]releaseKey, error) {
	var keys []releaseKey
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "ssh-ed25519" {
			return nil, fmt.Errorf("not an ssh-ed25519 public key: %.40q", line)
		}
		blob, err := base64.StdEncoding.DecodeString(f[1])
		if err != nil {
			return nil, fmt.Errorf("release key is not base64 (still the placeholder?): %.40q", f[1])
		}
		r := &wire{b: blob}
		typ, ok1 := r.str()
		pub, ok2 := r.str()
		if !ok1 || !ok2 || string(typ) != "ssh-ed25519" || len(pub) != ed25519.PublicKeySize || len(r.b) != 0 {
			return nil, errors.New("release key is not a well-formed ssh-ed25519 key")
		}
		sum := sha256.Sum256(blob)
		keys = append(keys, releaseKey{
			pub:         ed25519.PublicKey(bytes.Clone(pub)),
			blob:        blob,
			Fingerprint: "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]),
		})
	}
	if len(keys) == 0 {
		return nil, errors.New("no release key")
	}
	return keys, nil
}

// wire reads SSH wire-format strings (uint32 length, then bytes).
type wire struct{ b []byte }

func (r *wire) str() ([]byte, bool) {
	if len(r.b) < 4 {
		return nil, false
	}
	n := binary.BigEndian.Uint32(r.b)
	if uint64(n) > uint64(len(r.b)-4) {
		return nil, false
	}
	s := r.b[4 : 4+n]
	r.b = r.b[4+n:]
	return s, true
}

func sshString(b []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(b)))
	return append(out, b...)
}

const (
	sigBegin = "-----BEGIN SSH SIGNATURE-----"
	sigEnd   = "-----END SSH SIGNATURE-----"
)

// verifySSHSig checks an armored SSHSIG (PROTOCOL.sshsig in OpenSSH) over
// message, made by one of keys under namespace, and returns that key.
func verifySSHSig(armored, message []byte, keys []releaseKey, namespace string) (releaseKey, error) {
	text := strings.TrimSpace(strings.ReplaceAll(string(armored), "\r", ""))
	if !strings.HasPrefix(text, sigBegin) || !strings.HasSuffix(text, sigEnd) {
		return releaseKey{}, errors.New("signature is not an SSH signature")
	}
	body := strings.Join(strings.Fields(text[len(sigBegin):len(text)-len(sigEnd)]), "")
	raw, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return releaseKey{}, errors.New("signature is not valid base64")
	}
	if !bytes.HasPrefix(raw, []byte("SSHSIG")) || len(raw) < 10 {
		return releaseKey{}, errors.New("signature has no SSHSIG preamble")
	}
	if v := binary.BigEndian.Uint32(raw[6:10]); v != 1 {
		return releaseKey{}, fmt.Errorf("signature version %d is not 1", v)
	}
	r := &wire{b: raw[10:]}
	pubBlob, ok1 := r.str()
	ns, ok2 := r.str()
	reserved, ok3 := r.str()
	hashAlg, ok4 := r.str()
	sigBlob, ok5 := r.str()
	if !(ok1 && ok2 && ok3 && ok4 && ok5) || len(r.b) != 0 {
		return releaseKey{}, errors.New("signature is malformed")
	}
	if string(ns) != namespace {
		return releaseKey{}, fmt.Errorf("signature is for %q, not %q", ns, namespace)
	}
	var key *releaseKey
	for i := range keys {
		if bytes.Equal(keys[i].blob, pubBlob) {
			key = &keys[i]
			break
		}
	}
	if key == nil {
		return releaseKey{}, errors.New("signature is by a key this collector does not trust")
	}
	var digest []byte
	switch string(hashAlg) {
	case "sha512":
		d := sha512.Sum512(message)
		digest = d[:]
	case "sha256":
		d := sha256.Sum256(message)
		digest = d[:]
	default:
		return releaseKey{}, fmt.Errorf("signature hash %q is not supported", hashAlg)
	}
	s := &wire{b: sigBlob}
	sigType, ok1 := s.str()
	sig, ok2 := s.str()
	if !ok1 || !ok2 || string(sigType) != "ssh-ed25519" || len(sig) != ed25519.SignatureSize {
		return releaseKey{}, errors.New("signature is not an ssh-ed25519 signature")
	}
	signed := []byte("SSHSIG")
	signed = append(signed, sshString(ns)...)
	signed = append(signed, sshString(reserved)...)
	signed = append(signed, sshString(hashAlg)...)
	signed = append(signed, sshString(digest)...)
	if !ed25519.Verify(key.pub, signed, sig) {
		return releaseKey{}, errors.New("BAD SIGNATURE: SHA256SUMS does not match its signature")
	}
	return *key, nil
}

// sumsEntries is every hash SHA256SUMS lists for name (sha256sum's
// "<hash>  <name>" or binary-mode "<hash> *<name>").
func sumsEntries(sums []byte, name string) []string {
	var hashes []string
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			hashes = append(hashes, strings.ToLower(f[0]))
		}
	}
	return hashes
}

// versionFile is the content of a release's VERSION asset, whose hash
// SHA256SUMS carries: the version, one line.
func versionFile(version string) []byte { return []byte(version + "\n") }

// verifyRelease is step 2: sums is signed by a trusted key, lists
// exactly one hash for asset and it is wantSha, and lists exactly one
// VERSION and it is version's.
func verifyRelease(sums, sig []byte, keys []releaseKey, version, asset, wantSha string) (releaseKey, error) {
	key, err := verifySSHSig(sig, sums, keys, signatureNamespace)
	if err != nil {
		return releaseKey{}, err
	}
	vsum := sha256.Sum256(versionFile(version))
	if got := sumsEntries(sums, "VERSION"); len(got) != 1 || got[0] != hex.EncodeToString(vsum[:]) {
		return releaseKey{}, fmt.Errorf("the signed SHA256SUMS is not release %s's", version)
	}
	got := sumsEntries(sums, asset)
	if len(got) != 1 {
		return releaseKey{}, fmt.Errorf("the signed SHA256SUMS lists %d hashes for %s, not 1", len(got), asset)
	}
	if got[0] != wantSha {
		return releaseKey{}, fmt.Errorf("the hub named sha256 %s for %s, but the signed SHA256SUMS says %s", wantSha, asset, got[0])
	}
	return key, nil
}

// devUpdates: the door is on loopback, which SecureBase allows only for
// local development - and the one place update URLs may be loopback too
// (the tests and the systemd test serve releases from 127.0.0.1). The
// signature is still required; only the host rule relaxes.
func (c *Client) devUpdates() bool {
	u, err := url.Parse(c.Base)
	return err == nil && isLoopback(u.Hostname())
}

// checkUpdateURL is step 1 for the hub-named URL: this repository's
// release asset for exactly this version and platform. It returns the
// release's directory, where SHA256SUMS and its signature live.
func (c *Client) checkUpdateURL(raw, version, asset string) (dir string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("update URL does not parse: %v", err)
	}
	tail := version + "/" + asset
	plain := u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.Opaque == "" && u.RawPath == ""
	switch {
	case plain && u.Scheme == "https" && strings.EqualFold(u.Host, "github.com") &&
		u.Path == releaseDownloadPath+tail:
	case plain && c.devUpdates() && (u.Scheme == "http" || u.Scheme == "https") &&
		isLoopback(u.Hostname()) && strings.HasSuffix(u.Path, "/releases/download/"+tail):
	default:
		return "", fmt.Errorf("update URL %q is not https://github.com%s%s; refusing it", raw, releaseDownloadPath, tail)
	}
	return strings.TrimSuffix(raw, asset), nil
}

// updateRedirect is step 1 for every redirect a download follows: to
// GitHub's asset hosts (or this repository on github.com) over https
// only, or loopback to loopback in development.
func (c *Client) updateRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return untrusted{errors.New("update download: too many redirects")}
	}
	u := req.URL
	if c.devUpdates() && isLoopback(u.Hostname()) {
		return nil
	}
	host := strings.ToLower(u.Host) // with any port, so a port is refused
	if u.Scheme == "https" && u.User == nil &&
		(assetHosts[host] || host == "github.com" && strings.HasPrefix(u.Path, "/jthingelstad/elixir-mcp-collector/")) {
		return nil
	}
	return untrusted{fmt.Errorf("update download redirected to %s://%s, outside the release hosts; refusing it", u.Scheme, u.Host)}
}

// untrusted marks a failed update that failed because the release did
// not prove itself (a check above), not because a download broke.
type untrusted struct{ error }

func (u untrusted) Unwrap() error { return u.error }

// trustedKeys is the release key this client verifies with: the
// compiled-in one, or a test's.
func (c *Client) trustedKeys() ([]releaseKey, error) {
	s := c.releaseKeys
	if s == "" {
		s = releasePublicKeys
	}
	keys, err := parseReleaseKeys(s)
	if err != nil {
		return nil, fmt.Errorf("this build carries no usable release signing key (%v), so it cannot verify a release and installs none", err)
	}
	return keys, nil
}
