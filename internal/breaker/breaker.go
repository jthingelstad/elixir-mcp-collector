// Package breaker: the 403 circuit breaker (DESIGN §5.1). The CR API's
// most surprising fact: 403 accessDenied is auth failure, IP mismatch,
// OR rate-limit overage — indistinguishable. A streak means this
// gateway is broken or too fast; either way: stop fetching, let jobs
// re-lease elsewhere, surface via metrics, probe after a cooldown.
//
// The THRESHOLD AND COOLDOWN BELONG TO THE SERVER (AGENTS.md rule 2):
// they arrive in the config response and this package only applies
// them. The constants below are the fallback for a server that names
// neither, never a second opinion about the right values.
package breaker

import "time"

const (
	DefaultThreshold = 5
	DefaultCooldownS = 300
)

type Breaker struct {
	now            func() time.Time
	threshold      int
	cooldown       time.Duration
	consecutive403 int
	openedAt       *time.Time
}

// New builds a breaker with server-named limits. Zero or negative
// values fall back to the defaults, so a config missing the field is
// safe rather than instantly-open or never-open.
func New(now func() time.Time, threshold, cooldownS int) *Breaker {
	if now == nil {
		now = time.Now
	}
	b := &Breaker{now: now}
	b.Configure(threshold, cooldownS)
	return b
}

// Configure applies new limits WITHOUT touching breaker state.
//
// This is the whole reason it exists. Config is refreshed hourly, and
// rebuilding the breaker there silently cleared an OPEN breaker every
// hour — a collector that had been told to stop would quietly resume
// hammering a CR API that is 403ing it.
func (b *Breaker) Configure(threshold, cooldownS int) {
	if threshold <= 0 {
		threshold = DefaultThreshold
	}
	if cooldownS <= 0 {
		cooldownS = DefaultCooldownS
	}
	b.threshold = threshold
	b.cooldown = time.Duration(cooldownS) * time.Second
}

func (b *Breaker) RecordSuccess() {
	b.consecutive403 = 0
	b.openedAt = nil
}

// Record403 returns true when this 403 opened (or finds open) the breaker.
func (b *Breaker) Record403() bool {
	b.consecutive403++
	if b.consecutive403 >= b.threshold && b.openedAt == nil {
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
	if b.now().Sub(*b.openedAt) >= b.cooldown {
		b.openedAt = nil
		b.consecutive403 = b.threshold - 1
		return false
	}
	return true
}
