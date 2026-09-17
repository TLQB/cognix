package zbridge

import (
	"testing"
	"time"
)

// resetGateForTest restores a fresh gate state around a test.
func resetGateForTest(t *testing.T) {
	t.Helper()
	oldLimit, oldInFlight, oldStreak := uGate.limit, uGate.inFlight, uGate.cleanStreak
	t.Cleanup(func() {
		uGate.mu.Lock()
		uGate.limit, uGate.inFlight, uGate.cleanStreak = oldLimit, oldInFlight, oldStreak
		uGate.mu.Unlock()
	})
	uGate.mu.Lock()
	uGate.limit, uGate.inFlight, uGate.cleanStreak = upstreamGateInitialLimit, 0, 0
	uGate.mu.Unlock()
}

func TestGateAdmitsUpToLimitThenBlocks(t *testing.T) {
	resetGateForTest(t)

	toks := make([]gateToken, 0, upstreamGateInitialLimit+2)

	// Fill up to the limit: all must be admitted instantly.
	for i := 0; i < uGate.limit; i++ {
		tok := uGate.Acquire(0)
		if !tok.admitted {
			t.Fatalf("acquire %d should be admitted instantly (limit=%d)", i+1, uGate.limit)
		}
		toks = append(toks, tok)
	}
	// The next acquire must NOT be admitted within a short window.
	next := uGate.Acquire(150 * time.Millisecond)
	if next.admitted {
		t.Fatalf("acquire beyond limit was admitted (inFlight=%d limit=%d)", uGate.inFlight, uGate.limit)
	}

	// Release one slot: the next acquire must go through.
	uGate.Release(toks[0])
	tok := uGate.Acquire(time.Second)
	if !tok.admitted {
		t.Fatalf("acquire after release should be admitted")
	}
	// Cleanup: release everything so other tests are unaffected.
	uGate.Release(tok)
	for _, tk := range toks[1:] {
		uGate.Release(tk)
	}
}

func TestGateCapacityHalvesLimit(t *testing.T) {
	resetGateForTest(t)

	before := uGate.limit
	uGate.ReportCapacity()
	if after := uGate.limit; after != before/2 {
		t.Fatalf("ReportCapacity: limit = %d, want %d", after, before/2)
	}

	// Halving floors at gateMinLimit.
	for i := 0; i < 10; i++ {
		uGate.ReportCapacity()
	}
	if uGate.limit != gateMinLimit {
		t.Fatalf("limit after repeated capacity reports = %d, want floor %d", uGate.limit, gateMinLimit)
	}
}

func TestGateCleanStreakRaisesLimit(t *testing.T) {
	resetGateForTest(t)

	uGate.ReportCapacity() // 5 -> 2
	uGate.ReportCapacity() // 2 -> 1
	start := uGate.limit

	for i := 0; i < upstreamGateIncreaseAfter; i++ {
		uGate.ReportClean()
	}
	if uGate.limit != start+1 {
		t.Fatalf("after %d clean reports: limit = %d, want %d", upstreamGateIncreaseAfter, uGate.limit, start+1)
	}

	// The ceiling holds.
	for i := 0; i < 500; i++ {
		uGate.ReportClean()
	}
	if uGate.limit > maxUpstreamConcurrency {
		t.Fatalf("limit exceeded ceiling: %d > %d", uGate.limit, maxUpstreamConcurrency)
	}
}

func TestGateCapacityResetsCleanStreak(t *testing.T) {
	resetGateForTest(t)

	uGate.ReportCapacity() // halve
	start := uGate.limit
	for i := 0; i < upstreamGateIncreaseAfter-1; i++ {
		uGate.ReportClean()
	}
	uGate.ReportCapacity() // streak must reset: limit halves, not rises
	if uGate.limit != start/2 {
		t.Fatalf("capacity after near-full streak: limit = %d, want %d", uGate.limit, start/2)
	}
}

func TestGateBypassedTokenReleasesNothing(t *testing.T) {
	resetGateForTest(t)

	uGate.Acquire(0) // fill one slot
	before := uGate.inFlight
	uGate.Release(gateToken{}) // bypassed token
	if uGate.inFlight != before {
		t.Fatalf("bypassed release changed inFlight: %d -> %d", before, uGate.inFlight)
	}
}
