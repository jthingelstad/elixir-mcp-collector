package breaker

import (
	"testing"
	"time"
)

func TestHalfOpenProbe(t *testing.T) {
	clock := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	b := New(func() time.Time { return clock })
	for i := 0; i < 5; i++ {
		b.Record403()
	}
	if !b.IsOpen() {
		t.Fatal("open after threshold")
	}
	clock = clock.Add(16 * time.Minute)
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
