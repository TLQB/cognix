package zbridge

import (
	"sync"
	"time"
)

// Upstream concurrency gate (AIMD — additive increase, multiplicative
// decrease), modeled on TCP congestion control.
//
// Problem it solves: Z.AI rejects over-quota completions in-band with
// MODEL_CONCURRENCY_LIMIT (error-in-200). The remote reject loop is
// expensive — every rejection is a full upstream round-trip (paced +
// RTT) followed by a growing 2s-step backoff (2..14s), so a burst pays
// seconds of dead time per request before it even reaches the model.
// Observed live on glm-5.3/5.2 (production log 2026-09-09): 31 capacity
// stalls with backoffs up to 14s.
//
// The gate keeps this proxy's in-flight completions at or below a limit
// that adapts to what upstream actually accepts:
//   - DECREASE: every observed MODEL_CONCURRENCY_LIMIT halves the limit
//     (floor 1) — back off from the remote quota edge before it rejects.
//   - INCREASE: after every streak of upstreamGateIncreaseAfter clean
//     completions (no capacity error), the limit climbs by 1 (capped at
//     maxUpstreamConcurrency) — probe for headroom as the quota breathes.
//
// Requests wait in a local queue instead of being rejected remotely, so a
// burst rides out at the front of the gate rather than through repeated
// remote rejects. Local queue time is bounded by upstreamAcquireTimeout;
// on timeout the request proceeds un-gated (fail-open) rather than failing
// the user's request — the remote capacity retry loop remains the safety
// net.
//
// The gate is disabled in mock/CI mode (BASE_URL != production) because
// mock upstreams have no quota; gating them would only serialize tests.
const (
	// upstreamGateInitialLimit is the starting admission limit. The live
	// account quota has shown itself to be around this scale (capacity
	// rejects appear with bursts of ~6+ on a shared token), so starting
	// lower than the ceiling keeps the first burst from over-firing.
	upstreamGateInitialLimit = 5
	// upstreamGateIncreaseAfter is the number of consecutive clean
	// completions needed to raise the limit by 1.
	upstreamGateIncreaseAfter = 8
	// maxUpstreamConcurrency is the hard ceiling for the adaptive limit.
	maxUpstreamConcurrency = 8
	// upstreamAcquireTimeout bounds how long a request waits for a gate
	// slot before proceeding un-gated (fail-open).
	upstreamAcquireTimeout = 30 * time.Second
	// gateMinLimit is the floor for the adaptive limit.
	gateMinLimit = 1
	// gatePollInterval is how often waiters re-check for a free slot.
	// Only contended bursts pay it, and it is tiny next to the multi-second
	// remote rejects the gate exists to avoid.
	gatePollInterval = 20 * time.Millisecond
)

type upstreamGate struct {
	mu          sync.Mutex
	inFlight    int
	limit       int
	cleanStreak int
}

var uGate = newUpstreamGate()

func newUpstreamGate() *upstreamGate {
	return &upstreamGate{limit: upstreamGateInitialLimit}
}

// gateEnabled reports whether the gate should pace this deployment.
// Mock/CI upstreams have no quota; gating them only serializes tests.
func gateEnabled() bool {
	return BASE_URL == zaiProdURL
}

// gateToken reports whether a request was admitted through the gate.
// The zero token means "bypassed" (nothing to release).
type gateToken struct{ admitted bool }

// Acquire blocks until a slot is available or the timeout elapses (then
// the request proceeds un-gated — fail-open).
func (g *upstreamGate) Acquire(timeout time.Duration) gateToken {
	deadline := time.Now().Add(timeout)
	for {
		g.mu.Lock()
		if g.inFlight < g.limit {
			g.inFlight++
			g.mu.Unlock()
			return gateToken{admitted: true}
		}
		g.mu.Unlock()
		if time.Now().After(deadline) {
			return gateToken{}
		}
		time.Sleep(gatePollInterval)
	}
}

// Release returns a slot. A bypassed token releases nothing.
func (g *upstreamGate) Release(tok gateToken) {
	if !tok.admitted {
		return
	}
	g.mu.Lock()
	if g.inFlight > 0 {
		g.inFlight--
	}
	g.mu.Unlock()
}

// ReportCapacity halves the limit after a remote capacity reject. The
// remote retry loop still runs (the gate is a damper on top of it, not a
// replacement), but subsequent requests queue locally instead of eating
// another remote reject round-trip first.
func (g *upstreamGate) ReportCapacity() {
	g.mu.Lock()
	g.limit /= 2
	if g.limit < gateMinLimit {
		g.limit = gateMinLimit
	}
	g.cleanStreak = 0
	g.mu.Unlock()
}

// ReportClean counts a completion that never hit a capacity reject; after
// upstreamGateIncreaseAfter in a row, the limit grows by one (probe for
// headroom, capped).
func (g *upstreamGate) ReportClean() {
	g.mu.Lock()
	g.cleanStreak++
	if g.cleanStreak >= upstreamGateIncreaseAfter && g.limit < maxUpstreamConcurrency {
		g.limit++
		g.cleanStreak = 0
	}
	g.mu.Unlock()
}
