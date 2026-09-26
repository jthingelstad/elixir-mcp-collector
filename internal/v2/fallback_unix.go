//go:build unix

package v2

import (
	"runtime/debug"
	"syscall"
)

// crashNotExit makes a panic or fatal error during a trial die by
// SIGABRT instead of exiting 2. Exit 2 means "bad configuration" to the
// supervisors: systemd's RestartPreventExitStatus=2 and run-forever.sh
// both stop on it, so a panicking candidate would never be restarted
// into the rollback. A signal death is restarted everywhere. No core
// file: a crash loop must not fill a small NAS disk.
func crashNotExit() {
	_ = syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{})
	debug.SetTraceback("crash")
}

// restoreTraceback puts the default back once the binary is proven, so
// a proven collector exits on a panic exactly as it did before. It
// cannot go below a GOTRACEBACK the operator set.
func restoreTraceback() { debug.SetTraceback("single") }
