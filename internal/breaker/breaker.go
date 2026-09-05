// Package breaker: the 403 circuit breaker (DESIGN §5.1). The CR API's
// most surprising fact: 403 accessDenied is auth failure, IP mismatch,
// OR rate-limit overage — indistinguishable. A streak means this
// gateway is broken or too fast; either way: stop fetching, let jobs
// re-lease elsewhere, surface via metrics, probe after a cooldown.
package breaker

import "time"

const (
	Threshold  = 5
	CooldownMs = 15 * 60 * 1000
)

type Breaker struct {
	now            func() time.Time
	consecutive403 int
	openedAt       *time.Time
}

func New(now func() time.Time) *Breaker {
	if now == nil {
		now = time.Now
	}
	return &Breaker{now: now}
}

func (b *Breaker) RecordSuccess() {
	b.consecutive403 = 0
	b.openedAt = nil
}

// Record403 returns true when this 403 opened (or finds open) the breaker.
func (b *Breaker) Record403() bool {
	b.consecutive403++
	if b.consecutive403 >= Threshold && b.openedAt == nil {
		t := b.now()
		b.openedAt = &t
	}
	return b.IsOpen()
}

// IsOpen: after the cooldown one probe is allowed (half-open); a 403
// during the probe re-opens immediately at threshold.
func (b *Breaker) IsOpen() bool {
	if b.openedAt == nil {
		return false
	}
	if b.now().Sub(*b.openedAt) >= CooldownMs*time.Millisecond {
		b.openedAt = nil
		b.consecutive403 = Threshold - 1
		return false
	}
	return true
}
