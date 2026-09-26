package v2

// The way back from a bad release. The Python twin used to be the
// insurance against a Go release that could not start; this is its
// replacement, inside the Go updater itself (AGENTS.md rules 4 and 8).
//
// Two layers:
//
//  1. Before the swap, installBinary runs the staged candidate with
//     `version` (selfCheck) and requires the version the hub named. A
//     wrong-architecture asset fails to exec and an init-time crash
//     fails the check, while the old binary is still in place.
//  2. The swap keeps the previous binary beside the new one (prevSuffix)
//     and leaves a trial state (trialSuffix). Until the new binary gets
//     any answer from the hub, each start is a trial: Guard counts the
//     starts that died without a clean exit, and after maxTrialCrashes
//     puts the previous binary back, refuses the failed version, and
//     exits for the supervisor to start the restored one.
//
// A hub outage is not a crash: every deliberate exit before the proof
// goes through Trial.End, so only a death the binary did not choose
// (panic, fatal error, kill, power cut, wedge) is counted.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Files beside the binary. They share its name so two collectors in one
// directory (collector-a, collector-b) never touch each other's.
const (
	prevSuffix    = ".prev"    // the binary this one replaced, until it is proven
	trialSuffix   = ".trial"   // trial state while the new binary is unproven
	refusedSuffix = ".refused" // the one version that failed to start here
	failedSuffix  = ".failed"  // Windows: the rolled-back .exe, parked until it exits
)

// maxTrialCrashes: after this many starts that died before reaching the
// hub, the previous binary goes back. One is not enough - a power cut or
// an OOM kill during a trial counts once - and three restarts are ~10 s
// under systemd, ~30 s under launchd, ~3 min under Task Scheduler.
const maxTrialCrashes = 3

// trialBackoff: a start that follows a crash waits this long before it
// runs on or rolls back. An instant crash under RestartSec=2 would
// otherwise pack six starts into ten seconds and trip systemd's default
// start limit (5 per 10 s), which leaves the unit failed - worse than the
// bad release. A var so the tests need not wait.
var trialBackoff = 5 * time.Second

// enterCrashMode is crashNotExit; the in-process tests keep the default.
var enterCrashMode = crashNotExit

// selfCheckTimeout bounds the candidate's `version` run. A 10 MB exec
// off an SD card on a Pi, or under a Windows virus scan, is seconds.
const selfCheckTimeout = 30 * time.Second

// selfCheck runs the staged candidate with `version` and requires the
// version the hub named. The child gets neither token and an env file
// that does not exist, so no binary - whatever it does with the
// argument - can start collecting: it stops at its token check.
//
// A release older than this check does not know `version`; it ignores
// it, reaches the token check, and exits 2 saying so. That is accepted
// (it runs on this machine; its version cannot be asked), because the
// hub's way to undo a release is to name an older one.
func selfCheck(path, want string) (legacy bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), selfCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version")
	cmd.Dir = filepath.Dir(path)
	cmd.Env = selfCheckEnv(filepath.Dir(path))
	var out, errOut capped
	out.max, errOut.max = 4096, 4096
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	if ctx.Err() != nil {
		return false, fmt.Errorf("no answer to `version` in %s", selfCheckTimeout)
	}
	if err == nil {
		got := strings.TrimSpace(out.String())
		if got != want {
			return false, fmt.Errorf("it reports version %q, the hub named %q", got, want)
		}
		return false, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 2 &&
		strings.Contains(errOut.String(), "missing required config") {
		return true, nil
	}
	if errors.As(err, &exit) {
		return false, fmt.Errorf("`version` exited %d: %s", exit.ExitCode(), firstLine(errOut.String()))
	}
	return false, fmt.Errorf("it does not run on this machine: %v", err)
}

// selfCheckEnv is this process's environment without the secrets, with
// the env file pointed at nothing.
func selfCheckEnv(dir string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(name) {
		case "CR_API_TOKEN", "ELIXIR_API_TOKEN", "ELIXIR_MCP_ENV_FILE":
			continue
		}
		env = append(env, kv)
	}
	return append(env, "ELIXIR_MCP_ENV_FILE="+filepath.Join(dir, ".collector-self-check-has-no-env"))
}

// capped keeps the first max bytes written and drops the rest.
type capped struct {
	bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.Len(); room > 0 {
		c.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// trialState is the .trial file: an update from From to To that has not
// yet reached the hub. Live is set at each trial start and cleared by a
// deliberate exit; a start that finds it still set counts a crash.
type trialState struct {
	From    string    `json:"from"`
	To      string    `json:"to"`
	Crashes int       `json:"crashes"`
	Live    bool      `json:"live"`
	Since   time.Time `json:"since"`
}

type refusal struct {
	Version string    `json:"version"`
	Instead string    `json:"rolled_back_to"`
	At      time.Time `json:"at"`
	// A .trial renamed into place when no new file could be written
	// (rollBack) names the version here instead.
	To string `json:"to"`
}

// realPath is the file behind self: os.Executable can return the link a
// binary was started through (macOS), and every file here sits beside
// the real binary, where installBinary writes.
func realPath(self string) string {
	if real, err := filepath.EvalSymlinks(self); err == nil {
		return real
	}
	return self
}

func readJSON(path string, v any) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(data, v)
}

// writeJSON replaces path in one step (temp file, fsync, rename, fsync
// the directory), so a power cut leaves the old state or the new one.
func writeJSON(path string, v any) (err error) {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	f, err := createTemp(dir, ".collector-state-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = syncFile(f); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = renameFile(tmp, path); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

// readRefusal is the version this binary refuses to install, "" for none.
func readRefusal(self string) string {
	var r refusal
	if ok, err := readJSON(self+refusedSuffix, &r); !ok || err != nil {
		return ""
	}
	if r.Version != "" {
		return r.Version
	}
	return r.To
}

// Trial is one start of an unproven update. A nil *Trial (no update in
// flight) is valid and every method is a no-op on it.
type Trial struct {
	self  string
	state trialState
	log   func(level, msg string)
	exit  func(code int)

	mu    sync.Mutex
	done  bool // proven or ended: the state file is settled
	timer *time.Timer
}

// Guard runs at the very start of main, before .env and the token
// checks. With no update in flight it returns nil. With one, it either
// rolls back (and exits) or returns the Trial that main ends or the
// first door response proves.
func Guard(version string, log func(level, msg string)) *Trial {
	if version == "dev" {
		return nil // dev builds never self-update, so never trial
	}
	self, err := os.Executable()
	if err != nil {
		return nil
	}
	return guard(realPath(self), version, log, os.Exit)
}

func guard(self, version string, log func(level, msg string), exit func(int)) *Trial {
	// Windows parks a rolled-back .exe here until it has exited.
	_ = os.Remove(self + failedSuffix)

	var st trialState
	found, err := readJSON(self+trialSuffix, &st)
	if !found {
		return nil
	}
	if err != nil {
		// Unreadable (a filesystem that cannot fsync, cut mid-write):
		// without it there is no telling a crash loop from a first
		// start, so give up the trial rather than guess at a rollback.
		log("warn", "update trial state unreadable ("+err.Error()+"); discarding it and keeping this binary")
		dropTrial(self)
		return nil
	}
	if st.To != version {
		// The swap never happened (power cut before the rename), or a
		// binary without this code was installed over the trial.
		log("info", fmt.Sprintf("discarding a stale update trial (%s -> %s); running %s", st.From, st.To, version))
		dropTrial(self)
		return nil
	}
	if st.Live {
		st.Crashes++
		log("warn", fmt.Sprintf("update to %s: a trial start ended without a clean exit and before any answer from the hub (%d of %d before rolling back to %s)",
			st.To, st.Crashes, maxTrialCrashes, st.From))
		time.Sleep(trialBackoff)
	}
	if st.Crashes >= maxTrialCrashes {
		rollBack(self, st, log)
		exit(1)
		return nil // exit is swapped out in tests
	}
	st.Live = true
	if st.Since.IsZero() {
		st.Since = time.Now().UTC()
	}
	if err := writeJSON(self+trialSuffix, st); err != nil {
		log("warn", "cannot record the update trial ("+err.Error()+"); this start cannot be rolled back")
		return nil
	}
	t := &Trial{self: self, state: st, log: log, exit: exit}
	enterCrashMode()
	// A start that neither reaches the hub nor ends in time is wedged:
	// exit without recording a clean end, so it counts (rule 6).
	t.timer = time.AfterFunc(WatchdogTimeout, func() {
		t.mu.Lock()
		done := t.done
		t.mu.Unlock()
		if !done {
			log("error", "update trial: no answer from the hub and no exit in "+WatchdogTimeout.String()+"; exiting for supervisor restart")
			exit(1)
		}
	})
	log("info", fmt.Sprintf("update trial: running %s; %s is kept until the hub answers", st.To, st.From))
	return t
}

// Proven: the hub answered. The binary works, so the previous one and
// the trial go - a small NAS should not carry two binaries for ever.
func (t *Trial) Proven() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return
	}
	t.done = true
	t.timer.Stop()
	restoreTraceback()
	dropTrial(t.self)
	t.log("info", fmt.Sprintf("update to %s proven (the hub answered); removed %s", t.state.To, filepath.Base(t.self+prevSuffix)))
}

// End records a deliberate exit before the proof: a hub outage, a
// missing token, a signal. It is not a crash and must not count as one.
func (t *Trial) End() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return
	}
	t.done = true
	t.timer.Stop()
	st := t.state
	st.Live = false
	if err := writeJSON(t.self+trialSuffix, st); err != nil {
		t.log("warn", "cannot record a clean exit in the update trial: "+err.Error())
	}
}

// Exit ends the trial cleanly, then exits with code.
func (t *Trial) Exit(code int) {
	t.End()
	if t == nil {
		os.Exit(code)
	}
	t.exit(code)
}

// dropTrial removes the trial state and the previous binary.
func dropTrial(self string) {
	_ = os.Remove(self + prevSuffix)
	_ = os.Remove(self + trialSuffix)
	syncDir(filepath.Dir(self))
}

// rollBack puts the previous binary back in place of this one and
// refuses this version until the hub names another.
func rollBack(self string, st trialState, log func(level, msg string)) {
	prev := self + prevSuffix
	if _, err := os.Lstat(prev); err != nil {
		log("error", fmt.Sprintf("update to %s keeps crashing before it reaches the hub, and there is no previous binary to roll back to (%v); reinstall by hand", st.To, err))
		_ = os.Remove(self + trialSuffix)
		return
	}
	// Refuse first: the restored binary checks the hub at startup, and
	// without the refusal it would reinstall this version at once - a
	// tight install/crash/rollback loop. A full disk (the likeliest
	// failure with two binaries on it) cannot take a new file, but the
	// trial state already names the version, and a rename needs no space.
	if err := writeJSON(self+refusedSuffix, refusal{Version: st.To, Instead: st.From, At: time.Now().UTC()}); err != nil {
		if err2 := renameFile(self+trialSuffix, self+refusedSuffix); err2 != nil {
			log("error", fmt.Sprintf("update to %s keeps crashing before it reaches the hub, but the refusal cannot be recorded (%v; %v). Not rolling back, because %s would reinstall it at once; retrying at the next start.",
				st.To, err, err2, st.From))
			return
		}
	}
	if err := restorePrevious(self, prev); err != nil {
		log("error", fmt.Sprintf("update to %s keeps crashing before it reaches the hub; rolling back to %s FAILED: %v", st.To, st.From, err))
		return
	}
	_ = os.Remove(self + trialSuffix)
	syncDir(filepath.Dir(self))
	log("error", fmt.Sprintf("ROLLED BACK: %s crashed %d times before any answer from the hub; restored %s. %s is refused on this machine until the hub names a different version (delete %s to retry it). Exiting for supervisor restart.",
		st.To, st.Crashes, st.From, st.To, filepath.Base(self+refusedSuffix)))
}

// restorePrevious renames prev over self. On Unix that is one atomic
// step, running binary or not. Windows cannot replace a running .exe,
// so this one is parked aside first; run-collector.cmd covers the
// instant between the two renames.
func restorePrevious(self, prev string) error {
	if runtime.GOOS == "windows" {
		failed := self + failedSuffix
		_ = os.Remove(failed)
		if err := renameFile(self, failed); err != nil {
			return err
		}
		if err := renameFile(prev, self); err != nil {
			_ = renameFile(failed, self)
			return err
		}
		return nil
	}
	if err := renameFile(prev, self); err != nil {
		return err
	}
	syncDir(filepath.Dir(self))
	return nil
}

// keepPrevious leaves a copy of self at prev before a swap (Unix; on
// Windows the swap itself renames self to prev). A hard link costs no
// space until the swap; a filesystem without links gets a copy.
func keepPrevious(self, prev string) error {
	_ = os.Remove(prev)
	if err := linkFile(self, prev); err == nil {
		return nil
	}
	return copyFile(self, prev)
}

// createTemp is os.CreateTemp for state files; tests swap it to fill
// the disk.
var createTemp = os.CreateTemp

// linkFile is os.Link; tests swap it to exercise the copy.
var linkFile = os.Link

func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	f, err := os.CreateTemp(filepath.Dir(dst), ".collector-prev-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err = io.Copy(f, in); err != nil {
		return err
	}
	if err = f.Chmod(0o755); err != nil {
		return err
	}
	if err = syncFile(f); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return renameFile(tmp, dst)
}
