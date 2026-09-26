package envfile

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func write(t *testing.T, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte("ENVFILE_TEST_A=one\nENVFILE_TEST_B=emcg_two\n# comment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { // WriteFile honours umask
		t.Fatal(err)
	}
	return p
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

func TestLoadSetsVariablesWithoutOverriding(t *testing.T) {
	t.Setenv("ENVFILE_TEST_A", "")
	t.Setenv("ENVFILE_TEST_B", "already")
	r := Load(write(t, 0o600), true)
	if !r.Found || r.Loose || r.Warning() != "" {
		t.Fatalf("a private file is no finding: %+v", r)
	}
	if os.Getenv("ENVFILE_TEST_A") != "one" || os.Getenv("ENVFILE_TEST_B") != "already" {
		t.Fatalf("got A=%q B=%q", os.Getenv("ENVFILE_TEST_A"), os.Getenv("ENVFILE_TEST_B"))
	}
}

func TestMissingFileIsEnvOnly(t *testing.T) {
	r := Load(filepath.Join(t.TempDir(), "nope"), true)
	if r.Found || r.Warning() != "" {
		t.Fatalf("%+v", r)
	}
}

// Weak permissions are repaired, not refused: a collector that ran with a
// 0644 .env before an automatic update keeps running after it.
func TestLooseFileIsTightenedAndReported(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes; Windows ACLs are install.ps1's")
	}
	for _, tc := range []struct{ before, after os.FileMode }{
		{0o644, 0o600}, {0o640, 0o600}, {0o604, 0o600}, {0o660, 0o600}, {0o444, 0o400},
	} {
		p := write(t, tc.before)
		r := Load(p, true)
		if !r.Loose || !r.Repaired || r.Mode != tc.before {
			t.Fatalf("%o: %+v", tc.before, r)
		}
		if got := mode(t, p); got != tc.after {
			t.Fatalf("%o: tightened to %o, want %o", tc.before, got, tc.after)
		}
		w := r.Warning()
		if !strings.Contains(w, "tightened to") || !strings.Contains(w, p) {
			t.Fatalf("warning should say what changed: %q", w)
		}
	}
}

// Doctor loads without repair: it reports and leaves the box alone.
func TestLooseFileWithoutRepairIsOnlyReported(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	p := write(t, 0o644)
	r := Load(p, false)
	if !r.Loose || r.Repaired || mode(t, p) != 0o644 {
		t.Fatalf("%+v mode %o", r, mode(t, p))
	}
	if !strings.Contains(r.Warning(), "chmod 600 "+p) {
		t.Fatalf("%q", r.Warning())
	}
}

func TestWarningNeverCarriesAValue(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	t.Setenv("ENVFILE_TEST_A", "")
	t.Setenv("ENVFILE_TEST_B", "")
	r := Load(write(t, 0o644), true)
	if w := r.Warning(); strings.Contains(w, "emcg_two") || strings.Contains(w, "one\n") {
		t.Fatalf("secret in warning: %q", w)
	}
}
