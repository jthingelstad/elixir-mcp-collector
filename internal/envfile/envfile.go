// Package envfile loads the collector's .env (two secrets, KEY=value
// lines) into the environment, and keeps it private on POSIX hosts.
//
// Issue #6: a group- or world-readable .env hands both secrets to every
// account on the box. The collector repairs rather than refuses - a
// released binary updates itself, and a refusal would stop a collector
// that ran fine the hour before. It tightens the file to owner-only and
// says so; when it cannot (the file belongs to another account), it
// warns and carries on, and `collector doctor` shows the same finding.
package envfile

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"runtime"
)

// Result says what Load found, for the startup log and for doctor.
type Result struct {
	Path  string
	Found bool
	// POSIX only: the mode the file had when opened, and whether it was
	// readable by group or others at that moment.
	Mode  os.FileMode
	Loose bool
	// Repaired: Load tightened it (Mode is still the old mode).
	// RepairErr: it tried and could not.
	Repaired  bool
	RepairErr error
}

var line = regexp.MustCompile(`^([A-Z0-9_]+)=(.*)$`)

// Load reads path into the environment without overriding a variable
// that is already set. With repair, a loose file is tightened to its
// owner's bits only (0644 -> 0600, 0640 -> 0600, 0400 stays). The
// chmod goes through the open descriptor, so what is tightened is the
// file that was read, not whatever the path names a moment later.
// Doctor loads without repair: it reports, it does not change the box.
func Load(path string, repair bool) Result {
	r := Result{Path: path}
	f, err := os.Open(path)
	if err != nil {
		return r // env vars only
	}
	defer f.Close()
	r.Found = true
	if runtime.GOOS != "windows" {
		if st, err := f.Stat(); err == nil {
			r.Mode = st.Mode().Perm()
			r.Loose = r.Mode&0o077 != 0
			if r.Loose && repair {
				if err := f.Chmod(r.Mode &^ 0o077); err != nil {
					r.RepairErr = err
				} else {
					r.Repaired = true
				}
			}
		}
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := line.FindStringSubmatch(sc.Text()); m != nil {
			if os.Getenv(m[1]) == "" {
				os.Setenv(m[1], m[2])
			}
		}
	}
	return r
}

// Warning is the log line for a loose file, or "" when there is none.
// It names the path and the modes, never a value.
func (r Result) Warning() string {
	switch {
	case !r.Loose:
		return ""
	case r.Repaired:
		return fmt.Sprintf("%s was mode %o (readable by other accounts); tightened to %o - it holds two secrets",
			r.Path, r.Mode, r.Mode&^0o077)
	case r.RepairErr != nil:
		return fmt.Sprintf("%s is mode %o (readable by other accounts) and could not be tightened: %v - run: chmod 600 %s",
			r.Path, r.Mode, r.RepairErr, r.Path)
	default:
		return fmt.Sprintf("%s is mode %o (readable by other accounts) - run: chmod 600 %s", r.Path, r.Mode, r.Path)
	}
}
