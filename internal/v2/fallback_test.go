package v2

// The way back from a bad release (fallback.go): the self-check before
// the swap, the previous binary kept beside the new one, and the trial
// that rolls back a candidate crashing before it reaches the hub. The
// end-to-end tests run real binaries (testdata/fakecollector) through
// real restarts, so on Windows they exercise renaming a running .exe.

import (
	"errors"
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

// No waiting between trial starts, and the test process itself keeps
// Go's usual panic (the fakes exercise the real one).
// The fakes are v2.0.x, below the real install floor (trust.go), which
// has its own test.
func init() {
	trialBackoff = 0
	enterCrashMode = func() {}
	installFloor = "v0.0.0"
}

var (
	fakeMu    sync.Mutex
	fakeDir   string
	fakeBuilt = map[string]string{}
)

// fake builds testdata/fakecollector at version and mode once per run
// and returns its bytes.
func fake(t *testing.T, version, mode string) []byte {
	t.Helper()
	fakeMu.Lock()
	defer fakeMu.Unlock()
	key := version + "/" + mode
	path, ok := fakeBuilt[key]
	if !ok {
		if fakeDir == "" {
			d, err := os.MkdirTemp("", "fakecollector")
			if err != nil {
				t.Fatal(err)
			}
			fakeDir = d
		}
		path = filepath.Join(fakeDir, strings.ReplaceAll(key, "/", "-")+exeSuffix())
		cmd := exec.Command("go", "build", "-o", path,
			"-ldflags", "-X main.version="+version+" -X main.mode="+mode, "./testdata/fakecollector")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("building the fake collector: %v\n%s", err, out)
		}
		fakeBuilt[key] = path
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestMain(m *testing.M) {
	code := m.Run()
	if fakeDir != "" {
		_ = os.RemoveAll(fakeDir)
	}
	os.Exit(code)
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// installed puts data at dir/collector[.exe] as the running binary.
func installed(t *testing.T, data []byte) (dir, self string) {
	t.Helper()
	dir = t.TempDir()
	self = filepath.Join(dir, "collector"+exeSuffix())
	if err := os.WriteFile(self, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, realPath(self)
}

// releaseServer serves data as a signed release of version (trust_test.go)
// and counts the binary's downloads. URL is the asset URL the hub names.
func releaseServer(t *testing.T, version string, data []byte) (*testRelease, *int, string) {
	t.Helper()
	rel := signedRelease(t, version, data)
	return rel, &rel.binaryHits, rel.sha
}

func updater(t *testing.T, self, version string, logs *[]string) *Client {
	c := testClient("http://127.0.0.1:1", logs)
	c.Version = version
	c.Bin = self
	c.releaseKeys = testKeyLine(t)
	return c
}

func fileIs(t *testing.T, path string, want []byte) bool {
	t.Helper()
	got, err := os.ReadFile(path)
	return err == nil && string(got) == string(want)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func joined(logs []string) string { return strings.Join(logs, "\n") }

// --- layer 1: the self-check before the swap ---

func TestSelfCheckRequiresTheNamedVersion(t *testing.T) {
	_, bin := installed(t, fake(t, "v2.0.2", "prove"))
	if legacy, err := selfCheck(bin, "v2.0.2"); err != nil || legacy {
		t.Fatalf("legacy=%v err=%v", legacy, err)
	}
	if _, err := selfCheck(bin, "v2.0.3"); err == nil || !strings.Contains(err.Error(), `reports version "v2.0.2"`) {
		t.Fatalf("a candidate reporting another version passed: %v", err)
	}
}

// A wrong-architecture asset (here: not an executable at all) fails to
// exec, and the error says so.
func TestSelfCheckRefusesWhatCannotRun(t *testing.T) {
	_, bin := installed(t, []byte("\x7fELF but not really, or a binary for another CPU"))
	if _, err := selfCheck(bin, "v2.0.2"); err == nil {
		t.Fatal("a file that cannot run passed the self-check")
	}
}

// A release older than `version` ignores the argument. It must stop at
// its token check - never start collecting with this machine's tokens -
// and that is accepted, so the hub can still roll the fleet back to it.
func TestSelfCheckAcceptsALegacyReleaseWithoutLettingItCollect(t *testing.T) {
	dir, bin := installed(t, fake(t, "v2.0.1", "legacy"))
	t.Setenv("CR_API_TOKEN", "eyJreal.token.sig")
	t.Setenv("ELIXIR_API_TOKEN", "emcg_realtoken")
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("CR_API_TOKEN=x\nELIXIR_API_TOKEN=y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy, err := selfCheck(bin, "v2.0.1")
	if err != nil || !legacy {
		t.Fatalf("legacy=%v err=%v", legacy, err)
	}
	if exists(filepath.Join(dir, "started")) {
		t.Fatal("the self-check handed a legacy release the tokens and it started collecting")
	}
}

// --- the swap keeps the previous binary and opens a trial ---

func TestUpdateKeepsThePreviousBinaryAndOpensATrial(t *testing.T) {
	old := fake(t, "v2.0.1", "prove")
	cand := fake(t, "v2.0.2", "prove")
	dir, self := installed(t, old)
	srv, _, sha := releaseServer(t, "v2.0.2", cand)
	var logs []string
	c := updater(t, self, "v2.0.1", &logs)
	if err := c.applyUpdate(srv.URL, sha, "v2.0.2"); err != nil {
		t.Fatal(err)
	}
	if !fileIs(t, self, cand) {
		t.Fatal("the candidate is not in place")
	}
	if !fileIs(t, self+prevSuffix, old) {
		t.Fatal("the previous binary was not kept")
	}
	var st trialState
	if ok, err := readJSON(self+trialSuffix, &st); !ok || err != nil || st.From != "v2.0.1" || st.To != "v2.0.2" || st.Live || st.Crashes != 0 {
		t.Fatalf("trial state %+v ok=%v err=%v", st, ok, err)
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Fatalf("temp files left behind: %v", l)
	}
}

// A filesystem without hard links still keeps the previous binary.
func TestUpdateCopiesThePreviousBinaryWithoutHardLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows renames the previous binary aside")
	}
	linkFile = func(string, string) error { return errors.New("operation not permitted") }
	defer func() { linkFile = os.Link }()
	old := fake(t, "v2.0.1", "prove")
	_, self := installed(t, old)
	srv, _, sha := releaseServer(t, "v2.0.2", fake(t, "v2.0.2", "prove"))
	if err := updater(t, self, "v2.0.1", nil).applyUpdate(srv.URL, sha, "v2.0.2"); err != nil {
		t.Fatal(err)
	}
	if !fileIs(t, self+prevSuffix, old) {
		t.Fatal("the previous binary was not copied")
	}
	if st, _ := os.Stat(self + prevSuffix); st.Mode().Perm()&0o100 == 0 {
		t.Fatalf("the copy is not executable: %v", st.Mode())
	}
}

// A candidate that fails the self-check changes nothing at all.
func TestFailedSelfCheckLeavesEverythingAsItWas(t *testing.T) {
	old := fake(t, "v2.0.1", "prove")
	dir, self := installed(t, old)
	// Signed off by the hub's hash, but it is not the version named.
	srv, _, sha := releaseServer(t, "v2.0.2", fake(t, "v2.0.9", "prove"))
	err := updater(t, self, "v2.0.1", nil).applyUpdate(srv.URL, sha, "v2.0.2")
	if err == nil || !strings.Contains(err.Error(), "self-check") {
		t.Fatalf("got %v", err)
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

// A swap that fails at the rename (on Unix the step a power cut would
// interrupt) leaves the old binary at the path, and no trial or
// previous binary that a later start could mistake for an update.
func TestInterruptedSwapLeavesTheOldBinaryAndNoTrial(t *testing.T) {
	old := fake(t, "v2.0.1", "prove")
	dir, self := installed(t, old)
	srv, _, sha := releaseServer(t, "v2.0.2", fake(t, "v2.0.2", "prove"))
	renameFile = func(from, to string) error {
		if strings.Contains(filepath.Base(from), ".collector-update-") {
			return errors.New("power cut")
		}
		return os.Rename(from, to)
	}
	defer func() { renameFile = os.Rename }()
	if err := updater(t, self, "v2.0.1", nil).applyUpdate(srv.URL, sha, "v2.0.2"); err == nil {
		t.Fatal("expected the interruption to surface")
	}
	if !fileIs(t, self, old) {
		t.Fatal("the old binary is not at the path the supervisor starts")
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

// Power lost after the trial was written but before the rename: the
// binary that starts is still the old one, and it discards the trial.
func TestPowerCutBeforeTheRenameIsAStaleTrial(t *testing.T) {
	_, self := installed(t, []byte("old binary"))
	if err := keepPrevious(self, self+prevSuffix); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(self+trialSuffix, trialState{From: "v2.0.1", To: "v2.0.2"}); err != nil {
		t.Fatal(err)
	}
	var logs []string
	if tr := guard(self, "v2.0.1", logf(&logs), noExit(t)); tr != nil {
		t.Fatal("a stale trial was taken up")
	}
	if exists(self+trialSuffix) || exists(self+prevSuffix) {
		t.Fatal("the stale trial or its previous binary is still there")
	}
	if !fileIs(t, self, []byte("old binary")) {
		t.Fatal("the running binary changed")
	}
}

// --- the trial ---

func logf(logs *[]string) func(level, msg string) {
	return func(level, msg string) { *logs = append(*logs, level+" "+msg) }
}

func noExit(t *testing.T) func(int) {
	return func(code int) { t.Fatalf("unexpected exit(%d)", code) }
}

// trialAt lays out what an update leaves: prev beside self, trial state.
func trialAt(t *testing.T) (self string) {
	t.Helper()
	_, self = installed(t, []byte("new binary"))
	if err := os.WriteFile(self+prevSuffix, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(self+trialSuffix, trialState{From: "v2.0.1", To: "v2.0.2"}); err != nil {
		t.Fatal(err)
	}
	return self
}

// A hub outage right after an update: every start exits deliberately,
// fast, and without an answer. However many there are, none is a crash.
func TestOutageAfterAnUpdateNeverRollsBack(t *testing.T) {
	self := trialAt(t)
	var logs []string
	for i := 0; i < 3*maxTrialCrashes; i++ {
		tr := guard(self, "v2.0.2", logf(&logs), noExit(t))
		if tr == nil {
			t.Fatalf("start %d: no trial", i)
		}
		tr.End() // config: connection refused -> exit 1
	}
	var st trialState
	_, _ = readJSON(self+trialSuffix, &st)
	if st.Crashes != 0 || st.Live {
		t.Fatalf("outage exits were counted as crashes: %+v", st)
	}
	if !fileIs(t, self, []byte("new binary")) || !exists(self+prevSuffix) {
		t.Fatal("an outage touched the binaries")
	}
}

// A candidate that dies before the hub answers is rolled back on the
// start after its maxTrialCrashes-th crash, and its version refused.
func TestCrashLoopRollsBackAndRefusesTheVersion(t *testing.T) {
	self := trialAt(t)
	var logs []string
	for i := 0; i < maxTrialCrashes; i++ {
		if tr := guard(self, "v2.0.2", logf(&logs), noExit(t)); tr == nil {
			t.Fatalf("start %d: no trial", i)
		}
		// ...and the process dies: no End, no Proven.
	}
	exited := -1
	if tr := guard(self, "v2.0.2", logf(&logs), func(code int) { exited = code }); tr != nil {
		t.Fatal("the rolling-back start carried on")
	}
	if exited != 1 {
		t.Fatalf("exit %d, want 1 (restartable)", exited)
	}
	if !fileIs(t, self, []byte("old binary")) {
		t.Fatal("the previous binary was not restored")
	}
	if exists(self+prevSuffix) || exists(self+trialSuffix) {
		t.Fatal("the trial was not closed")
	}
	if got := readRefusal(self); got != "v2.0.2" {
		t.Fatalf("refused %q", got)
	}
	if !strings.Contains(joined(logs), "ROLLED BACK") {
		t.Fatalf("the rollback is not loud:\n%s", joined(logs))
	}
}

// Crashes and outages interleaved: only the crashes count.
func TestOnlyCrashesCount(t *testing.T) {
	self := trialAt(t)
	var logs []string
	for _, crash := range []bool{true, false, true, false, false} {
		tr := guard(self, "v2.0.2", logf(&logs), noExit(t))
		if !crash {
			tr.End()
		}
	}
	var st trialState
	_, _ = readJSON(self+trialSuffix, &st)
	if st.Crashes != 2 {
		t.Fatalf("crashes %d, want 2", st.Crashes)
	}
}

// The first answer from the hub proves the binary: the previous one is
// deleted, so a small disk does not carry two binaries.
func TestProofDeletesThePreviousBinary(t *testing.T) {
	self := trialAt(t)
	var logs []string
	tr := guard(self, "v2.0.2", logf(&logs), noExit(t))
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503) // an error is still an answer
	}))
	defer door.Close()
	c := testClient(door.URL, nil)
	c.OnResponse = tr.Proven
	_ = c.LoadConfig(false)
	if exists(self+prevSuffix) || exists(self+trialSuffix) {
		t.Fatal("proof did not close the trial")
	}
	tr.End() // after proof, a deliberate exit changes nothing
	if exists(self + trialSuffix) {
		t.Fatal("End reopened a proven trial")
	}
	if tr := guard(self, "v2.0.2", logf(&logs), noExit(t)); tr != nil {
		t.Fatal("a proven binary is still on trial")
	}
}

// State that cannot be read is dropped, never guessed into a rollback.
func TestUnreadableTrialIsDiscarded(t *testing.T) {
	self := trialAt(t)
	if err := os.WriteFile(self+trialSuffix, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs []string
	if tr := guard(self, "v2.0.2", logf(&logs), noExit(t)); tr != nil {
		t.Fatal("trial taken up from garbage")
	}
	if !fileIs(t, self, []byte("new binary")) || exists(self+trialSuffix) {
		t.Fatal("garbage state changed the binary or survived")
	}
}

// --- the refusal is one version, and it clears itself ---

func refusedAt(t *testing.T, version string) (self string) {
	t.Helper()
	_, self = installed(t, []byte("old binary"))
	if err := writeJSON(self+refusedSuffix, refusal{Version: version, Instead: "v2.0.1"}); err != nil {
		t.Fatal(err)
	}
	return self
}

func TestRestoredBinaryRefusesTheFailedVersionOnly(t *testing.T) {
	self := refusedAt(t, "v2.0.2")
	srv, hits, sha := releaseServer(t, "v2.0.2", []byte("bad"))
	var logs []string
	c := updater(t, self, "v2.0.1", &logs)
	for i := 0; i < 3; i++ { // hourly config refreshes
		c.updateTo("v2.0.2", srv.URL, sha)
	}
	if *hits != 0 {
		t.Fatalf("the refused version was downloaded %d times", *hits)
	}
	if n := strings.Count(joined(logs), "REFUSING update to v2.0.2"); n != 3 {
		t.Fatalf("refusal logged %d times, want every time:\n%s", n, joined(logs))
	}
	if readRefusal(self) != "v2.0.2" {
		t.Fatal("the refusal did not hold")
	}
}

// The hub naming anything else - a fix, or the version already running -
// clears the refusal. It never outlives the hub moving on.
func TestRefusalClearsWhenTheHubNamesAnotherVersion(t *testing.T) {
	for _, named := range []string{"v2.0.3", "v2.0.1"} {
		self := refusedAt(t, "v2.0.2")
		var logs []string
		c := updater(t, self, "v2.0.1", &logs)
		missing := httptest.NewServer(http.NotFoundHandler())
		c.updateTo(named, missing.URL, "00")
		missing.Close()
		if readRefusal(self) != "" || exists(self+refusedSuffix) {
			t.Fatalf("named %s: refusal not cleared", named)
		}
		if !strings.Contains(joined(logs), "no longer refusing v2.0.2") {
			t.Fatalf("named %s: clearing not logged:\n%s", named, joined(logs))
		}
	}
}

// --- end to end: real binaries, real restarts ---

// supervise runs the binary at self like launchd or systemd would, n
// times, and returns each run's exit code (-1: killed by a signal).
func supervise(t *testing.T, self string, n int, env ...string) []int {
	t.Helper()
	var codes []int
	for i := 0; i < n; i++ {
		cmd := exec.Command(self)
		cmd.Env = append(os.Environ(), env...)
		out, _ := cmd.CombinedOutput()
		t.Logf("run %d: %s", i+1, strings.TrimSpace(string(out)))
		codes = append(codes, cmd.ProcessState.ExitCode())
	}
	return codes
}

// A candidate that passes the self-check but panics at startup: three
// crashes, then the previous binary is back, runs, and refuses the
// candidate. On Windows this renames the running .exe aside.
func TestEndToEndCrashingReleaseIsRolledBack(t *testing.T) {
	old := fake(t, "v2.0.1", "prove")
	bad := fake(t, "v2.0.2", "crash")
	dir, self := installed(t, old)
	srv, hits, sha := releaseServer(t, "v2.0.2", bad)
	var logs []string
	if err := updater(t, self, "v2.0.1", &logs).applyUpdate(srv.URL, sha, "v2.0.2"); err != nil {
		t.Fatal(err)
	}
	codes := supervise(t, self, maxTrialCrashes+2)
	for i := 0; i < maxTrialCrashes; i++ {
		// Unix: a trial panic dies by SIGABRT, never exit 2, which
		// systemd and run-forever.sh would take as "stop".
		if codes[i] == 2 && runtime.GOOS != "windows" {
			t.Fatalf("crash %d exited 2; the supervisor would stop instead of restarting", i+1)
		}
		if codes[i] == 0 {
			t.Fatalf("run %d of the bad release exited 0", i+1)
		}
	}
	if codes[maxTrialCrashes] != 1 {
		t.Fatalf("the rolling-back run exited %d, want 1", codes[maxTrialCrashes])
	}
	if codes[maxTrialCrashes+1] != 0 {
		t.Fatalf("the restored binary exited %d", codes[maxTrialCrashes+1])
	}
	if !fileIs(t, self, old) {
		t.Fatal("the previous binary is not back")
	}
	for _, p := range []string{self + prevSuffix, self + trialSuffix, self + failedSuffix} {
		if exists(p) {
			t.Fatalf("%s left behind", filepath.Base(p))
		}
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Fatalf("temp files left behind: %v", l)
	}
	// The restored binary, told again to install the bad release, won't.
	logs = nil
	updater(t, self, "v2.0.1", &logs).updateTo("v2.0.2", srv.URL, sha)
	if *hits != 1 {
		t.Fatalf("downloads %d, want only the first", *hits)
	}
	if !strings.Contains(joined(logs), "REFUSING update to v2.0.2") {
		t.Fatalf("%s", joined(logs))
	}
}

// The same release through a hub outage: fast exit-1 restarts, never a
// rollback, and the first answer proves it and removes the old binary.
func TestEndToEndOutageThenProof(t *testing.T) {
	old := fake(t, "v2.0.1", "prove")
	cand := fake(t, "v2.0.2", "prove")
	_, self := installed(t, old)
	srv, _, sha := releaseServer(t, "v2.0.2", cand)
	if err := updater(t, self, "v2.0.1", nil).applyUpdate(srv.URL, sha, "v2.0.2"); err != nil {
		t.Fatal(err)
	}
	for i, code := range supervise(t, self, 2*maxTrialCrashes, "FAKE_MODE=outage") {
		if code != 1 {
			t.Fatalf("outage run %d exited %d", i+1, code)
		}
	}
	if !fileIs(t, self, cand) || !exists(self+prevSuffix) {
		t.Fatal("an outage rolled the update back")
	}
	if codes := supervise(t, self, 1); codes[0] != 0 {
		t.Fatalf("the proving run exited %d", codes[0])
	}
	if exists(self+prevSuffix) || exists(self+trialSuffix) {
		t.Fatal("proof left the previous binary or the trial")
	}
	if !fileIs(t, self, cand) {
		t.Fatal("the proven binary is gone")
	}
}

// An update entry whose version the hub dropped names nothing: the
// refusal stands, and nothing is downloaded.
func TestAnEmptyNamedVersionKeepsTheRefusal(t *testing.T) {
	self := refusedAt(t, "v2.0.2")
	srv, hits, sha := releaseServer(t, "v2.0.2", []byte("bad"))
	var logs []string
	updater(t, self, "v2.0.1", &logs).updateTo("", srv.URL, sha)
	if readRefusal(self) != "v2.0.2" {
		t.Fatal("an empty version cleared the refusal")
	}
	if *hits != 0 {
		t.Fatal("an empty version was downloaded")
	}
}

// crashTo leaves a trial that has crashed maxTrialCrashes times.
func crashTo(t *testing.T) (self string, logs *[]string) {
	t.Helper()
	self = trialAt(t)
	logs = new([]string)
	for i := 0; i < maxTrialCrashes; i++ {
		guard(self, "v2.0.2", logf(logs), noExit(t))
	}
	return self, logs
}

// A full disk cannot take the refusal file, but the rollback must not
// go ahead without one: the trial state is renamed into its place.
func TestRollbackOnAFullDiskStillRefuses(t *testing.T) {
	self, logs := crashTo(t)
	createTemp = func(string, string) (*os.File, error) { return nil, errors.New("no space left on device") }
	defer func() { createTemp = os.CreateTemp }()
	guard(self, "v2.0.2", logf(logs), func(int) {})
	if !fileIs(t, self, []byte("old binary")) {
		t.Fatal("the previous binary was not restored")
	}
	if got := readRefusal(self); got != "v2.0.2" {
		t.Fatalf("refused %q after a full-disk rollback", got)
	}
}

// With no way at all to record the refusal, the rollback waits: a
// restored binary would reinstall the crashing version at once.
func TestRollbackWaitsWhenNoRefusalCanBeRecorded(t *testing.T) {
	self, logs := crashTo(t)
	createTemp = func(string, string) (*os.File, error) { return nil, errors.New("no space left on device") }
	renameFile = func(string, string) error { return errors.New("read-only file system") }
	defer func() { createTemp, renameFile = os.CreateTemp, os.Rename }()
	exited := -1
	guard(self, "v2.0.2", logf(logs), func(code int) { exited = code })
	if exited != 1 {
		t.Fatalf("exit %d", exited)
	}
	if !fileIs(t, self, []byte("new binary")) || !exists(self+prevSuffix) || !exists(self+trialSuffix) {
		t.Fatal("rolled back without a refusal, or lost the state to retry with")
	}
	if !strings.Contains(joined(*logs), "Not rolling back") {
		t.Fatalf("%s", joined(*logs))
	}
}
