package v2

// The release trust chain (trust.go, issue #5): a signed SHA256SUMS, an
// allowlisted URL and redirects, the install floor, and how all of it
// fits the self-check, trial and refusal from fallback.go. Every key
// here is a throwaway generated inside the test run.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// --- a throwaway signer ---

var (
	testKeyOnce sync.Once
	testPriv    ed25519.PrivateKey
)

func testKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	testKeyOnce.Do(func() {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			panic(err)
		}
		testPriv = priv
	})
	return testPriv
}

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

func keyBlob(pub ed25519.PublicKey) []byte {
	return append(sshString([]byte("ssh-ed25519")), sshString(pub)...)
}

// pubLine is priv's public key as an OpenSSH .pub line.
func pubLine(priv ed25519.PrivateKey) string {
	pub := priv.Public().(ed25519.PublicKey)
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(keyBlob(pub)) + " test-release-key"
}

func testKeyLine(t *testing.T) string { return pubLine(testKey(t)) }

// flipLastSigByte corrupts the last byte of an armored SSHSIG, which is
// the last byte of the ed25519 signature itself.
func flipLastSigByte(t *testing.T, armored []byte) []byte {
	t.Helper()
	text := strings.TrimSpace(string(armored))
	raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(text[len(sigBegin):len(text)-len(sigEnd)]), ""))
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	return []byte(sigBegin + "\n" + base64.StdEncoding.EncodeToString(raw) + "\n" + sigEnd + "\n")
}

// sshSign makes the armored SSHSIG `ssh-keygen -Y sign -n namespace`
// would (PROTOCOL.sshsig), with sha512 like ssh-keygen's default.
func sshSign(priv ed25519.PrivateKey, namespace string, message []byte) []byte {
	digest := sha512.Sum512(message)
	signed := []byte("SSHSIG")
	signed = append(signed, sshString([]byte(namespace))...)
	signed = append(signed, sshString(nil)...)
	signed = append(signed, sshString([]byte("sha512"))...)
	signed = append(signed, sshString(digest[:])...)
	sig := ed25519.Sign(priv, signed)

	blob := []byte("SSHSIG")
	blob = binary.BigEndian.AppendUint32(blob, 1)
	blob = append(blob, sshString(keyBlob(priv.Public().(ed25519.PublicKey)))...)
	blob = append(blob, sshString([]byte(namespace))...)
	blob = append(blob, sshString(nil)...)
	blob = append(blob, sshString([]byte("sha512"))...)
	blob = append(blob, sshString(append(sshString([]byte("ssh-ed25519")), sshString(sig)...))...)
	b64 := base64.StdEncoding.EncodeToString(blob)
	var out strings.Builder
	out.WriteString(sigBegin + "\n")
	for len(b64) > 70 {
		out.WriteString(b64[:70] + "\n")
		b64 = b64[70:]
	}
	out.WriteString(b64 + "\n" + sigEnd + "\n")
	return []byte(out.String())
}

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// sumsFor is what release.yml writes: every asset, the installers, and
// the VERSION line.
func sumsFor(version string, data []byte) []byte {
	return []byte(fmt.Sprintf("%s  %s\n%s  install.sh\n%s  VERSION\n",
		hexSum(data), thisAsset(), hexSum([]byte("#!/bin/sh\n")), hexSum(versionFile(version))))
}

func thisAsset() string { return releaseAsset(runtime.GOOS, runtime.GOARCH) }

// --- a signed release on a test server ---

// testRelease is one release directory: the asset, SHA256SUMS and its
// signature, served at /releases/download/<version>/. Tests may change
// sums, sig or binary before the update runs.
type testRelease struct {
	URL        string // the asset URL the hub names
	sha        string
	sums, sig  []byte
	binary     func(w http.ResponseWriter, r *http.Request) // nil: serve data
	binaryHits int
	requests   []string
}

func signedRelease(t *testing.T, version string, data []byte) *testRelease {
	t.Helper()
	rel := &testRelease{sha: hexSum(data)}
	rel.sums = sumsFor(version, data)
	rel.sig = sshSign(testKey(t), signatureNamespace, rel.sums)
	srv := httptest.NewServer(rel.handler(version, data))
	t.Cleanup(srv.Close)
	rel.URL = srv.URL + "/releases/download/" + version + "/" + thisAsset()
	return rel
}

func (rel *testRelease) handler(version string, data []byte) http.HandlerFunc {
	dir := "/releases/download/" + version + "/"
	return func(w http.ResponseWriter, r *http.Request) {
		rel.requests = append(rel.requests, r.URL.Path)
		switch r.URL.Path {
		case dir + thisAsset():
			rel.binaryHits++
			if rel.binary != nil {
				rel.binary(w, r)
				return
			}
			_, _ = w.Write(data)
		case dir + "SHA256SUMS":
			_, _ = w.Write(rel.sums)
		case dir + "SHA256SUMS.sig":
			if rel.sig == nil {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(rel.sig)
		default:
			http.NotFound(w, r)
		}
	}
}

// refusedUntouched: the update failed as untrusted, nothing ran and
// nothing changed - the binary, no trial, no temp files, no download of
// the binary itself.
func refusedUntouched(t *testing.T, err error, want string, rel *testRelease, dir, self string, old []byte) {
	t.Helper()
	var u untrusted
	if err == nil || !errors.As(err, &u) || !strings.Contains(err.Error(), want) {
		t.Fatalf("got %v, want an untrusted refusal containing %q", err, want)
	}
	if rel != nil && rel.binaryHits != 0 {
		t.Fatalf("the binary was downloaded %d times before the release proved itself", rel.binaryHits)
	}
	if !fileIs(t, self, old) {
		t.Fatal("the running binary changed")
	}
	for _, p := range []string{self + prevSuffix, self + trialSuffix} {
		if exists(p) {
			t.Fatalf("%s left behind", filepath.Base(p))
		}
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Fatalf("temp files left behind: %v", l)
	}
}

// --- the signature ---

func TestSignedReleaseInstalls(t *testing.T) {
	old := fake(t, "v2.0.1", "prove")
	cand := fake(t, "v2.0.2", "prove")
	_, self := installed(t, old)
	rel, _, sha := releaseServer(t, "v2.0.2", cand)
	var logs []string
	if err := updater(t, self, "v2.0.1", &logs).applyUpdate(rel.URL, sha, "v2.0.2"); err != nil {
		t.Fatal(err)
	}
	if !fileIs(t, self, cand) {
		t.Fatal("the signed candidate is not in place")
	}
	if !strings.Contains(joined(logs), "is signed by release key SHA256:") {
		t.Fatalf("the verification is not logged:\n%s", joined(logs))
	}
	// The metadata is checked before the binary is fetched.
	if len(rel.requests) != 3 || !strings.HasSuffix(rel.requests[2], thisAsset()) {
		t.Fatalf("requests %v: the binary must come last", rel.requests)
	}
}

func TestBadSignatureIsRefused(t *testing.T) {
	cand := fake(t, "v2.0.2", "prove")
	for name, tamper := range map[string]func(rel *testRelease){
		// SHA256SUMS changed after signing (any byte of it).
		"tampered sums": func(rel *testRelease) {
			rel.sums = bytes.Replace(rel.sums, []byte("install.sh"), []byte("install.sx"), 1)
		},
		// Signed, but not by the key this collector trusts.
		"untrusted key": func(rel *testRelease) {
			rel.sig = sshSign(newKey(t), signatureNamespace, rel.sums)
		},
		// The trusted key, signing something that is not a release.
		"other namespace": func(rel *testRelease) {
			rel.sig = sshSign(testKey(t), "file", rel.sums)
		},
		// A signature that is not one.
		"garbage": func(rel *testRelease) { rel.sig = []byte("not a signature") },
		// The right key and namespace, one bit of the signature wrong.
		"flipped bit": func(rel *testRelease) {
			rel.sig = flipLastSigByte(t, sshSign(testKey(t), signatureNamespace, rel.sums))
		},
		// A release from before signing: no SHA256SUMS.sig at all.
		"unsigned release": func(rel *testRelease) { rel.sig = nil },
	} {
		t.Run(name, func(t *testing.T) {
			old := fake(t, "v2.0.1", "prove")
			dir, self := installed(t, old)
			rel, _, sha := releaseServer(t, "v2.0.2", cand)
			tamper(rel)
			err := updater(t, self, "v2.0.1", nil).applyUpdate(rel.URL, sha, "v2.0.2")
			if name == "unsigned release" {
				// A 404 is a failed download, not a verdict on the release.
				if err == nil || !strings.Contains(err.Error(), "SHA256SUMS.sig") || rel.binaryHits != 0 || !fileIs(t, self, old) {
					t.Fatalf("got %v, hits %d", err, rel.binaryHits)
				}
				return
			}
			refusedUntouched(t, err, "", rel, dir, self, old)
		})
	}
}

// The hub's hash and the signed one must agree: a compromised hub cannot
// name a binary the signature does not cover, nor a signed one under a
// hash it made up.
func TestHubHashMustBeTheSignedOne(t *testing.T) {
	old := fake(t, "v2.0.1", "prove")
	dir, self := installed(t, old)
	rel, _, _ := releaseServer(t, "v2.0.2", fake(t, "v2.0.2", "prove"))
	other := hexSum([]byte("some other binary"))
	err := updater(t, self, "v2.0.1", nil).applyUpdate(rel.URL, other, "v2.0.2")
	refusedUntouched(t, err, "signed SHA256SUMS says", rel, dir, self, old)
}

// Replay: a validly signed SHA256SUMS from one release served as
// another's. Its VERSION line gives it away, so an old signed binary can
// never be installed under a newer version's name.
func TestReplayedReleaseIsRefused(t *testing.T) {
	old := fake(t, "v2.0.1", "prove")
	dir, self := installed(t, old)
	v2 := fake(t, "v2.0.2", "prove")
	signedFor2 := sumsFor("v2.0.2", v2)
	rel, _, sha := releaseServer(t, "v2.0.3", v2)
	rel.sums, rel.sig = signedFor2, sshSign(testKey(t), signatureNamespace, signedFor2)
	err := updater(t, self, "v2.0.1", nil).applyUpdate(rel.URL, sha, "v2.0.3")
	refusedUntouched(t, err, "is not release v2.0.3's", rel, dir, self, old)

	// And a signed SHA256SUMS with no VERSION line at all.
	rel.sums = []byte(fmt.Sprintf("%s  %s\n", sha, thisAsset()))
	rel.sig = sshSign(testKey(t), signatureNamespace, rel.sums)
	err = updater(t, self, "v2.0.1", nil).applyUpdate(rel.URL, sha, "v2.0.3")
	refusedUntouched(t, err, "is not release v2.0.3's", rel, dir, self, old)
}

// A build still carrying the placeholder key cannot verify anything, so
// it installs nothing - and asks for nothing.
func TestPlaceholderKeyInstallsNothing(t *testing.T) {
	saved := releasePublicKeys
	releasePublicKeys = "ssh-ed25519 REPLACE_WITH_THE_RELEASE_PUBLIC_KEY elixir-mcp-collector-release"
	defer func() { releasePublicKeys = saved }()
	old := fake(t, "v2.0.1", "prove")
	dir, self := installed(t, old)
	rel, _, sha := releaseServer(t, "v2.0.2", fake(t, "v2.0.2", "prove"))
	c := updater(t, self, "v2.0.1", nil)
	c.releaseKeys = ""
	err := c.applyUpdate(rel.URL, sha, "v2.0.2")
	refusedUntouched(t, err, "no usable release signing key", rel, dir, self, old)
	if len(rel.requests) != 0 {
		t.Fatalf("requests %v", rel.requests)
	}
}

// Rotation: a second key line is trusted alongside the first.
func TestEitherTrustedKeyVerifies(t *testing.T) {
	next := newKey(t)
	cand := fake(t, "v2.0.2", "prove")
	_, self := installed(t, fake(t, "v2.0.1", "prove"))
	rel, _, sha := releaseServer(t, "v2.0.2", cand)
	rel.sig = sshSign(next, signatureNamespace, rel.sums)
	c := updater(t, self, "v2.0.1", nil)
	c.releaseKeys = testKeyLine(t) + "\n# the next key\n" + pubLine(next) + "\n"
	if err := c.applyUpdate(rel.URL, sha, "v2.0.2"); err != nil {
		t.Fatal(err)
	}
	if !fileIs(t, self, cand) {
		t.Fatal("not installed")
	}
}

// --- the URL and its redirects ---

// fakeGitHub answers for every host (github.com, the asset hosts, and
// anything else) from one TLS test server, so the real URL rules run
// against production-shaped URLs. handle sees every request.
func fakeGitHub(t *testing.T, handle http.HandlerFunc) (*http.Client, *[]string) {
	t.Helper()
	var hosts []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hosts = append(hosts, r.Host)
		handle(w, r)
	}))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		// The test server's certificate is for 127.0.0.1, not github.com.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}, &hosts
}

const prodBase = "https://elixir.poapkings.com/api/collector"

func prodUpdater(t *testing.T, self, version string, client *http.Client) *Client {
	c := updater(t, self, version, nil)
	c.Base = prodBase
	c.HTTP = client
	return c
}

func assetURL(version string) string {
	return "https://github.com" + releaseDownloadPath + version + "/" + thisAsset()
}

// The URL the hub names must be exactly this repository's asset for the
// named version and this platform. Anything else is refused before a
// single request.
func TestWrongHostIsRefused(t *testing.T) {
	old := fake(t, "v2.0.1", "prove")
	client, hosts := fakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("requested %s%s", r.Host, r.URL)
	})
	path := releaseDownloadPath + "v2.0.2/" + thisAsset()
	for _, u := range []string{
		"https://evil.example" + path,
		"https://github.com.evil.example" + path,
		"https://objects.githubusercontent.com" + path, // only a redirect may go there
		"http://github.com" + path,
		"https://github.com:8443" + path,
		"https://user@github.com" + path,
		"https://github.com" + path + "?x=1",
		"https://github.com" + path + "#frag",
		"https://github.com/someone-else/elixir-mcp-collector/releases/download/v2.0.2/" + thisAsset(),
		"https://github.com" + releaseDownloadPath + "v2.0.1/" + thisAsset(), // another version
		"https://github.com" + releaseDownloadPath + "v2.0.2/collector_plan9_386",
		"https://github.com" + releaseDownloadPath + "v2.0.2/%2e%2e/" + thisAsset(),
		"https://github.com" + releaseDownloadPath + "v2.0.2/../v2.0.2/" + thisAsset(),
		// Loopback is for a door on loopback only.
		"http://127.0.0.1:9" + "/releases/download/v2.0.2/" + thisAsset(),
		"github.com" + path,
	} {
		dir, self := installed(t, old)
		err := prodUpdater(t, self, "v2.0.1", client).applyUpdate(u, hexSum([]byte("x")), "v2.0.2")
		refusedUntouched(t, err, "refusing it", nil, dir, self, old)
	}
	if len(*hosts) != 0 {
		t.Fatalf("requests were made: %v", *hosts)
	}
}

// A github.com URL that redirects anywhere but GitHub's asset hosts over
// https is refused at the redirect - for the metadata and the binary.
func TestRedirectEscapeIsRefused(t *testing.T) {
	old := fake(t, "v2.0.1", "prove")
	cand := fake(t, "v2.0.2", "prove")
	sums := sumsFor("v2.0.2", cand)
	sig := sshSign(testKey(t), signatureNamespace, sums)
	for _, escape := range []struct{ name, to, file string }{
		{"binary to another host", "https://evil.example/collector", thisAsset()},
		{"binary to plain http", "http://objects.githubusercontent.com/x", thisAsset()},
		{"binary to an asset host on another port", "https://release-assets.githubusercontent.com:444/x", thisAsset()},
		{"binary to loopback", "https://127.0.0.1/x", thisAsset()},
		{"sums to another host", "https://evil.example/SHA256SUMS", "SHA256SUMS"},
		{"signature to another repo", "https://github.com/evil/repo/SHA256SUMS.sig", "SHA256SUMS.sig"},
	} {
		t.Run(escape.name, func(t *testing.T) {
			client, hosts := fakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Host != "github.com" {
					t.Errorf("followed the redirect to %s", r.Host)
					_, _ = w.Write(cand)
					return
				}
				switch filepath.Base(r.URL.Path) {
				case escape.file:
					http.Redirect(w, r, escape.to, http.StatusFound)
				case "SHA256SUMS":
					_, _ = w.Write(sums)
				case "SHA256SUMS.sig":
					_, _ = w.Write(sig)
				default:
					http.NotFound(w, r)
				}
			})
			dir, self := installed(t, old)
			err := prodUpdater(t, self, "v2.0.1", client).applyUpdate(assetURL("v2.0.2"), hexSum(cand), "v2.0.2")
			refusedUntouched(t, err, "outside the release hosts", nil, dir, self, old)
			for _, h := range *hosts {
				if h != "github.com" {
					t.Fatalf("hosts %v", *hosts)
				}
			}
		})
	}
}

// The production shape end to end: github.com redirects every file to
// GitHub's asset host, as it does today, and the update goes through.
func TestRedirectToTheAssetHostInstalls(t *testing.T) {
	old := fake(t, "v2.0.1", "prove")
	cand := fake(t, "v2.0.2", "prove")
	sums := sumsFor("v2.0.2", cand)
	sig := sshSign(testKey(t), signatureNamespace, sums)
	client, hosts := fakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		file := filepath.Base(r.URL.Path)
		if r.Host == "github.com" {
			http.Redirect(w, r, "https://release-assets.githubusercontent.com/github-production-release-asset/1/"+file+"?sp=r&sig=abc", http.StatusFound)
			return
		}
		switch file {
		case thisAsset():
			_, _ = w.Write(cand)
		case "SHA256SUMS":
			_, _ = w.Write(sums)
		case "SHA256SUMS.sig":
			_, _ = w.Write(sig)
		}
	})
	_, self := installed(t, old)
	if err := prodUpdater(t, self, "v2.0.1", client).applyUpdate(assetURL("v2.0.2"), hexSum(cand), "v2.0.2"); err != nil {
		t.Fatal(err)
	}
	if !fileIs(t, self, cand) {
		t.Fatal("not installed")
	}
	if want := 6; len(*hosts) != want {
		t.Fatalf("requests to %v, want %d (three files, each redirected once)", *hosts, want)
	}
}

// --- downgrade, rollback, and the floor ---

// Naming an older signed release is the hub's rollback lever: it goes
// through the same checks and the same trial as any update.
func TestHubNamedRollbackInstallsAndOpensATrial(t *testing.T) {
	running := fake(t, "v2.0.3", "prove")
	older := fake(t, "v2.0.2", "prove")
	_, self := installed(t, running)
	rel, _, sha := releaseServer(t, "v2.0.2", older)
	var logs []string
	if err := updater(t, self, "v2.0.3", &logs).applyUpdate(rel.URL, sha, "v2.0.2"); err != nil {
		t.Fatal(err)
	}
	if !fileIs(t, self, older) || !fileIs(t, self+prevSuffix, running) {
		t.Fatal("the rollback did not swap and keep the previous binary")
	}
	var st trialState
	if ok, _ := readJSON(self+trialSuffix, &st); !ok || st.From != "v2.0.3" || st.To != "v2.0.2" {
		t.Fatalf("trial %+v", st)
	}
	if !strings.Contains(joined(logs), "older than this v2.0.3: rolling back") {
		t.Fatalf("the rollback is not said:\n%s", joined(logs))
	}
}

// Below the floor, no signature helps: that is the version-monotonicity
// rule, and it is checked before any request.
func TestDowngradeBelowTheFloorIsRefused(t *testing.T) {
	installFloor = "v2.0.30"
	defer func() { installFloor = "v0.0.0" }()
	old := fake(t, "v2.0.31", "prove")
	dir, self := installed(t, old)
	rel, _, sha := releaseServer(t, "v2.0.29", fake(t, "v2.0.29", "prove"))
	err := updater(t, self, "v2.0.31", nil).applyUpdate(rel.URL, sha, "v2.0.29")
	refusedUntouched(t, err, "below v2.0.30", rel, dir, self, old)
	if len(rel.requests) != 0 {
		t.Fatalf("requests %v", rel.requests)
	}
	// The floor itself is installable.
	rel, _, sha = releaseServer(t, "v2.0.30", fake(t, "v2.0.30", "prove"))
	if err := updater(t, self, "v2.0.31", nil).applyUpdate(rel.URL, sha, "v2.0.30"); err != nil {
		t.Fatal(err)
	}
}

// A rollback to a release from before signing: refused until its
// SHA256SUMS is signed (sign-release.yml), then installed like any other
// - and a legacy release's version is bound by the signed VERSION line,
// since it cannot report one itself.
func TestRollbackToAReleaseFromBeforeSigning(t *testing.T) {
	running := fake(t, "v2.0.3", "prove")
	legacy := fake(t, "v2.0.1", "legacy")
	dir, self := installed(t, running)
	rel, _, sha := releaseServer(t, "v2.0.1", legacy)
	rel.sig = nil // as published, before signing existed
	var logs []string
	c := updater(t, self, "v2.0.3", &logs)
	c.updateTo("v2.0.1", rel.URL, sha)
	if rel.binaryHits != 0 || !fileIs(t, self, running) || exists(self+trialSuffix) {
		t.Fatal("an unsigned release was installed")
	}
	if !strings.Contains(joined(logs), "self-update failed") {
		t.Fatalf("%s", joined(logs))
	}
	if readRefusal(self) != "" {
		t.Fatal("a failed verification wrote a crash refusal")
	}
	// The maintainer backfills its signature; the next hourly check installs it.
	rel.sig = sshSign(testKey(t), signatureNamespace, rel.sums)
	if err := c.applyUpdate(rel.URL, sha, "v2.0.1"); err != nil {
		t.Fatal(err)
	}
	if !fileIs(t, self, legacy) || !fileIs(t, self+prevSuffix, running) {
		t.Fatal("the backfilled release was not installed with the previous binary kept")
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Fatalf("%v", l)
	}
}

// A refused version (fallback.go) is refused before any verification or
// download, and a verification failure never writes a refusal: the next
// hourly check simply tries again.
func TestVerificationFailureIsRetriedNotRefused(t *testing.T) {
	old := fake(t, "v2.0.1", "prove")
	_, self := installed(t, old)
	cand := fake(t, "v2.0.2", "prove")
	rel, _, sha := releaseServer(t, "v2.0.2", cand)
	good := rel.sig
	rel.sig = sshSign(newKey(t), signatureNamespace, rel.sums)
	var logs []string
	c := updater(t, self, "v2.0.1", &logs)
	c.updateTo("v2.0.2", rel.URL, sha)
	if !strings.Contains(joined(logs), "self-update REFUSED v2.0.2") {
		t.Fatalf("the refusal is not loud:\n%s", joined(logs))
	}
	if exists(self + refusedSuffix) {
		t.Fatal("a bad signature wrote a crash refusal")
	}
	rel.sig = good
	if err := c.applyUpdate(rel.URL, sha, "v2.0.2"); err != nil {
		t.Fatal(err)
	}
	if !fileIs(t, self, cand) {
		t.Fatal("the next check did not install the properly signed release")
	}
}

// --- interrupted and oversized transfers ---

// The connection dies halfway through the binary: nothing changes.
func TestInterruptedDownloadChangesNothing(t *testing.T) {
	old := fake(t, "v2.0.1", "prove")
	cand := fake(t, "v2.0.2", "prove")
	dir, self := installed(t, old)
	rel, _, sha := releaseServer(t, "v2.0.2", cand)
	rel.binary = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(cand)))
		_, _ = w.Write(cand[:len(cand)/2])
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}
	err := updater(t, self, "v2.0.1", nil).applyUpdate(rel.URL, sha, "v2.0.2")
	if err == nil {
		t.Fatal("a truncated download was accepted")
	}
	if !fileIs(t, self, old) || exists(self+prevSuffix) || exists(self+trialSuffix) {
		t.Fatal("a truncated download changed the install")
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Fatalf("%v", l)
	}
}

func TestOversizedMetadataIsRefused(t *testing.T) {
	old := fake(t, "v2.0.1", "prove")
	_, self := installed(t, old)
	rel, _, sha := releaseServer(t, "v2.0.2", fake(t, "v2.0.2", "prove"))
	rel.sums = bytes.Repeat([]byte("x"), maxSumsBytes+1)
	err := updater(t, self, "v2.0.1", nil).applyUpdate(rel.URL, sha, "v2.0.2")
	if err == nil || !strings.Contains(err.Error(), "download over") || rel.binaryHits != 0 {
		t.Fatalf("got %v", err)
	}
}

// --- the pieces ---

func TestReleaseAssetNames(t *testing.T) {
	// The names release.yml builds; name-collector-release.mjs maps the
	// same ones to config keys.
	for _, tc := range [][3]string{
		{"darwin", "arm64", "collector_darwin_arm64"},
		{"darwin", "amd64", "collector_darwin_amd64"},
		{"linux", "arm64", "collector_linux_arm64"},
		{"linux", "amd64", "collector_linux_amd64"},
		{"linux", "arm", "collector_linux_armv7"},
		{"windows", "amd64", "collector_windows_amd64.exe"},
		{"windows", "arm64", "collector_windows_arm64.exe"},
	} {
		if got := releaseAsset(tc[0], tc[1]); got != tc[2] {
			t.Errorf("%s/%s: %s, want %s", tc[0], tc[1], got, tc[2])
		}
	}
}

func TestVersionOrder(t *testing.T) {
	v := func(s string) [3]int {
		n, ok := parseVersion(s)
		if !ok {
			t.Fatalf("%s did not parse", s)
		}
		return n
	}
	// v3 is above every v2.0.x, whatever its patch (the hub compares the
	// same way).
	if compareVersions(v("v3.0.0"), v("v2.0.999")) != 1 || compareVersions(v("v2.0.10"), v("v2.0.9")) != 1 ||
		compareVersions(v("v2.0.30"), v("v2.0.30")) != 0 || compareVersions(v("v9.9.1-smoke"), v("v9.9.2-smoke")) != -1 {
		t.Fatal("order")
	}
	for _, bad := range []string{"dev", "2.0.1", "v2.0", "v2.0.1/../x", "v2.0.1 ", "v2.0.1-", "py-v2.0.1"} {
		if _, ok := parseVersion(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

// --- interop with ssh-keygen, which signs releases in CI ---

// release.yml signs with `ssh-keygen -Y sign`: what it produces, with a
// key ssh-keygen generates for the test, verifies here, and this
// package's fingerprint is ssh-keygen's. CI signs on Linux only, and
// Windows ssh-keygen refuses a private key under a temp dir's default
// ACL, so this direction is not run there.
func TestSSHKeygenSignatureVerifies(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("no ssh-keygen here")
	}
	if runtime.GOOS == "windows" {
		t.Skip("releases are signed on Linux")
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "release")
	if out, err := exec.Command(keygen, "-q", "-t", "ed25519", "-N", "", "-C", "test", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	// CI has the private key only, no .pub beside it.
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(key + ".pub"); err != nil {
		t.Fatal(err)
	}
	sums := filepath.Join(dir, "SHA256SUMS")
	content := sumsFor("v2.0.2", []byte("binary"))
	if err := os.WriteFile(sums, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(keygen, "-Y", "sign", "-q", "-f", key, "-n", signatureNamespace, sums).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	keys, err := parseReleaseKeys(string(pub))
	if err != nil {
		t.Fatal(err)
	}
	sig, _ := os.ReadFile(sums + ".sig")
	if _, err := verifyRelease(content, sig, keys, "v2.0.2", thisAsset(), hexSum([]byte("binary"))); err != nil {
		t.Fatalf("ssh-keygen's signature does not verify here: %v", err)
	}
	if err := os.WriteFile(key+".pub", pub, 0o600); err != nil {
		t.Fatal(err)
	}
	fp, _ := exec.Command(keygen, "-l", "-f", key+".pub").Output()
	if !strings.Contains(string(fp), keys[0].Fingerprint) {
		t.Fatalf("fingerprint %s is not ssh-keygen's: %s", keys[0].Fingerprint, fp)
	}
}

// Operators verify with `ssh-keygen -Y verify` (README) on macOS, Linux
// and Windows: it accepts what this test signs, so the test signer is
// the real format and the README command works on each platform.
func TestSSHKeygenVerifiesTheFormat(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("no ssh-keygen here")
	}
	dir := t.TempDir()
	content := sumsFor("v2.0.2", []byte("binary"))
	priv := newKey(t)
	sig := filepath.Join(dir, "SHA256SUMS.sig")
	if err := os.WriteFile(sig, sshSign(priv, signatureNamespace, content), 0o600); err != nil {
		t.Fatal(err)
	}
	signers := filepath.Join(dir, "allowed_signers")
	if err := os.WriteFile(signers, []byte(signatureNamespace+" "+pubLine(priv)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	verify := func(message []byte) error {
		cmd := exec.Command(keygen, "-Y", "verify", "-f", signers, "-I", signatureNamespace, "-n", signatureNamespace, "-s", sig)
		cmd.Stdin = bytes.NewReader(message)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		return nil
	}
	if err := verify(content); err != nil {
		t.Fatalf("ssh-keygen rejects the test signer: %v", err)
	}
	if verify(append(content, 'x')) == nil {
		t.Fatal("ssh-keygen accepted a changed SHA256SUMS")
	}
}

// --- the compiled-in key and the release that ships it ---

// release.yml runs this with REQUIRE_RELEASE_KEY=1: no release is built
// while the key is the placeholder.
func TestCompiledReleaseKey(t *testing.T) {
	keys, err := parseReleaseKeys(releasePublicKeys)
	if os.Getenv("REQUIRE_RELEASE_KEY") == "" {
		t.Skipf("informational: compiled key parses: %v", err == nil)
	}
	if err != nil {
		t.Fatalf("internal/v2/releasekey.go does not carry a usable key (%v); see SECURITY.md, \"Generating the release key\"", err)
	}
	for _, k := range keys {
		t.Logf("release key %s", k.Fingerprint)
	}
}

// Operators verify an installer with the key the docs publish, so the
// docs must publish the one compiled in.
func TestPublishedKeyMatchesTheCompiledOne(t *testing.T) {
	for _, doc := range []string{"../../README.md", "../../SECURITY.md", "../../docs/recipes/cloud-init.yaml"} {
		data, err := os.ReadFile(doc)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(releasePublicKeys, "\n") {
			f := strings.Fields(line)
			if len(f) < 2 || strings.HasPrefix(f[0], "#") {
				continue
			}
			if !strings.Contains(string(data), f[0]+" "+f[1]) {
				t.Errorf("%s does not publish the compiled release key %s %.16s...", doc, f[0], f[1])
			}
		}
	}
}

// release.yml and sign-release.yml run this with RELEASE_DIR set to the
// directory they are about to publish: the exact verifier a collector
// runs, with the compiled-in key, accepts it. A signing secret that does
// not match the compiled key fails the release here, not on the fleet.
func TestReleaseDirVerifies(t *testing.T) {
	dir := os.Getenv("RELEASE_DIR")
	if dir == "" {
		t.Skip("RELEASE_DIR not set")
	}
	keys, err := parseReleaseKeys(releasePublicKeys)
	if err != nil {
		t.Fatal(err)
	}
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	sums, sig := read("SHA256SUMS"), read("SHA256SUMS.sig")
	version := strings.TrimSpace(string(read("VERSION")))
	if _, ok := parseVersion(version); !ok {
		t.Fatalf("VERSION %q", version)
	}
	for _, goos := range [][2]string{{"darwin", "arm64"}, {"darwin", "amd64"}, {"linux", "arm64"}, {"linux", "amd64"}, {"linux", "arm"}, {"windows", "amd64"}, {"windows", "arm64"}} {
		asset := releaseAsset(goos[0], goos[1])
		got := hexSum(read(asset))
		if _, err := verifyRelease(sums, sig, keys, version, asset, got); err != nil {
			t.Errorf("%s: %v", asset, err)
		}
	}
	// Every line names a file that is there and matches.
	for _, line := range strings.Split(strings.TrimSpace(string(sums)), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || hexSum(read(strings.TrimPrefix(f[1], "*"))) != f[0] {
			t.Errorf("SHA256SUMS line %q does not match the file", line)
		}
	}
	// And the way README tells an operator to check it.
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Fatal("ssh-keygen is needed to check the release the way operators do")
	}
	signers := filepath.Join(t.TempDir(), "allowed_signers")
	var lines []string
	for _, line := range strings.Split(releasePublicKeys, "\n") {
		if f := strings.Fields(line); len(f) >= 2 && !strings.HasPrefix(f[0], "#") {
			lines = append(lines, signatureNamespace+" "+f[0]+" "+f[1])
		}
	}
	if err := os.WriteFile(signers, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(keygen, "-Y", "verify", "-f", signers, "-I", signatureNamespace, "-n", signatureNamespace, "-s", filepath.Join(dir, "SHA256SUMS.sig"))
	cmd.Stdin = bytes.NewReader(sums)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen -Y verify: %v: %s", err, out)
	} else {
		t.Logf("%s", bytes.TrimSpace(out))
	}
}
