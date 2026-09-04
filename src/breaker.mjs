/**
 * 403 circuit breaker — DESIGN §5.1. The CR API's most surprising fact:
 * 403 accessDenied is auth failure, IP mismatch, OR rate-limit overage —
 * indistinguishable. A streak means this gateway is broken or too fast;
 * either way the right move is the same: stop fetching, let jobs re-lease
 * elsewhere, surface via metrics, probe again after a cooldown.
 * (Documented in cr-agent-api-docs, never encoded in elixir-bot; encoded here.)
 */

export class CircuitBreaker {
  constructor({
    threshold = 5,
    cooldownMs = 15 * 60_000,
    now = () => Date.now(),
  } = {}) {
    this.threshold = threshold;
    this.cooldownMs = cooldownMs;
    this.now = now;
    this.consecutive403 = 0;
    this.openedAt = null;
  }

  recordSuccess() {
    this.consecutive403 = 0;
    this.openedAt = null;
  }

  record403() {
    this.consecutive403 += 1;
    if (this.consecutive403 >= this.threshold && this.openedAt === null) {
      this.openedAt = this.now();
    }
    return this.isOpen();
  }

  /** Open means: do not lease. After the cooldown, one probe is allowed. */
  isOpen() {
    if (this.openedAt === null) return false;
    if (this.now() - this.openedAt >= this.cooldownMs) {
      // Half-open: allow a probe; a 403 re-opens immediately at threshold.
      this.openedAt = null;
      this.consecutive403 = this.threshold - 1;
      return false;
    }
    return true;
  }
}
