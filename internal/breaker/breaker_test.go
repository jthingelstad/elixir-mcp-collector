package breaker

import (
	"testing"
	"time"
)

func at(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestHalfOpenProbe(t *testing.T) {
	clock := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	b := New(func() time.Time { return clock }, DefaultThreshold, DefaultCooldownS)
	for i := 0; i < 5; i++ {
		b.Record403()
	}
	if !b.IsOpen() {
		t.Fatal("open after threshold")
	}
	clock = clock.Add(6 * time.Minute)
	if b.IsOpen() {
		t.Fatal("half-open after cooldown: one probe allowed")
	}
	// a 403 during the probe re-opens immediately at threshold
	if !b.Record403() {
		t.Fatal("probe 403 must re-open")
	}
	// success fully resets
	b.RecordSuccess()
	if b.IsOpen() || b.consecutive403 != 0 {
		t.Fatal("success resets")
	}
}

// The server owns these numbers; the client only applies them.
func TestServerNamedThresholdAndCooldown(t *testing.T) {
	clock := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	b := New(func() time.Time { return clock }, 2, 30)

	if b.Record403() {
		t.Fatal("one 403 must not open a threshold-2 breaker")
	}
	if !b.Record403() {
		t.Fatal("the SECOND 403 must open it: the server said 2, not 5")
	}

	clock = clock.Add(29 * time.Second)
	if !b.IsOpen() {
		t.Fatal("still open one second before the server's cooldown")
	}
	clock = clock.Add(1 * time.Second)
	if b.IsOpen() {
		t.Fatal("the server said 30s, so a probe is allowed at 30s")
	}
}

// A missing or nonsensical value must not disable the breaker or wedge
// it shut; it falls back to the documented default.
func TestZeroConfigFallsBackToDefaults(t *testing.T) {
	clock := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	b := New(at(clock), 0, 0)
	for i := 0; i < DefaultThreshold-1; i++ {
		if b.Record403() {
			t.Fatal("opened before the default threshold")
		}
	}
	if !b.Record403() {
		t.Fatal("must open at the default threshold")
	}
	if b.cooldown != DefaultCooldownS*time.Second {
		t.Fatalf("cooldown = %v, want the default", b.cooldown)
	}
	b = New(at(clock), -3, -9)
	if b.threshold != DefaultThreshold || b.cooldown != DefaultCooldownS*time.Second {
		t.Fatal("negative values must fall back, not invert the breaker")
	}
}

// The bug that made the hourly config refresh dangerous: rebuilding the
// breaker cleared an OPEN one, so a collector the server had stopped
// resumed fetching every hour.
func TestConfigureKeepsAnOpenBreakerOpen(t *testing.T) {
	clock := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	b := New(func() time.Time { return clock }, 2, 300)
	b.Record403()
	b.Record403()
	if !b.IsOpen() {
		t.Fatal("precondition: open")
	}

	b.Configure(2, 300) // an hourly refresh naming the same limits
	if !b.IsOpen() {
		t.Fatal("a config refresh must NOT clear an open breaker")
	}

	// New limits still apply to the breaker that is already open: the
	// cooldown is measured against whatever the server most recently said.
	b.Configure(2, 30)
	clock = clock.Add(30 * time.Second)
	if b.IsOpen() {
		t.Fatal("a shortened cooldown should let the probe through")
	}
}

// Lowering the threshold below an existing streak opens on the next 403
// rather than retroactively or never.
func TestLoweredThresholdAppliesToTheNext403(t *testing.T) {
	clock := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	b := New(at(clock), 10, 300)
	for i := 0; i < 3; i++ {
		b.Record403()
	}
	if b.IsOpen() {
		t.Fatal("precondition: below a threshold of 10")
	}
	b.Configure(3, 300)
	if b.IsOpen() {
		t.Fatal("Configure must not open it retroactively")
	}
	if !b.Record403() {
		t.Fatal("the next 403 must open it against the new threshold")
	}
}
