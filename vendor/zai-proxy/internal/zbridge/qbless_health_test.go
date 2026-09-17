package zbridge

import (
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Health tracker — Q is trusted until its OWN verify results say otherwise
// (no time-based TTL; see qmint.go lifetime-model notes). Death is tracked
// PER Q: a re-blessed session with a different Q is trusted immediately,
// while the same dead Q stays dead even across a process restart.

func resetHealth(t *testing.T) {
	t.Helper()
	qHealth.mu.Lock()
	qHealth.failStreak = 0
	qHealth.deadQ = ""
	qHealth.nagged = time.Time{}
	qHealth.mu.Unlock()
}

func TestQblessHealthStreakMarksDead(t *testing.T) {
	resetHealth(t)
	const q = "Q-alpha"

	if qblessQDead(q) {
		t.Fatal("fresh state must not be dead")
	}

	// 2 fails: transient, still alive (single fails can be param races).
	qblessReportVerifyQ(q, false)
	qblessReportVerifyQ(q, false)
	if qblessQDead(q) {
		t.Fatal("2 consecutive fails must NOT mark Q dead")
	}

	// 3rd fail: dead.
	qblessReportVerifyQ(q, false)
	if !qblessQDead(q) {
		t.Fatal("3 consecutive fails must mark Q dead")
	}

	// A different Q is alive by definition (fresh bless).
	if qblessQDead("Q-beta") {
		t.Fatal("a different Q must never inherit the death verdict")
	}

	// A success resurrects.
	qblessReportVerifyQ(q, true)
	if qblessQDead(q) {
		t.Fatal("a verify success must resurrect the Q")
	}
}

func TestQblessHealthDeadQDoesNotLeakToNewSession(t *testing.T) {
	resetHealth(t)
	const deadQ = "Q-old"
	const newQ = "Q-new"

	qHealth.mu.Lock()
	qHealth.failStreak = 3
	qHealth.deadQ = deadQ
	qHealth.mu.Unlock()

	if !qblessQDead(deadQ) {
		t.Fatal("precondition: deadQ must be dead")
	}
	if qblessQDead(newQ) {
		t.Fatal("new Q must not inherit death")
	}

	// Reports about the new Q clear the old verdict entirely.
	qblessReportVerifyQ(newQ, false) // one transient fail on the new Q
	qHealth.mu.Lock()
	stillDead := qHealth.deadQ == deadQ
	qHealth.mu.Unlock()
	if stillDead {
		t.Fatal("verifying a different Q must retire the old verdict")
	}
}

func TestQblessHealthNoMaxAgeByDefault(t *testing.T) {
	t.Setenv("QBLESS_MAX_AGE", "")
	if got := qblessMaxAge(); got != 0 {
		t.Fatalf("default qblessMaxAge must be 0 (no age limit), got %v", got)
	}
	t.Setenv("QBLESS_MAX_AGE", "12h")
	if got := qblessMaxAge(); got != 12*time.Hour {
		t.Fatalf("QBLESS_MAX_AGE=12h must parse, got %v", got)
	}
	t.Setenv("QBLESS_MAX_AGE", "4")
	if got := qblessMaxAge(); got != 4*time.Hour {
		t.Fatalf("QBLESS_MAX_AGE=4 (bare hours) must parse, got %v", got)
	}
}

func qblessTestSession(t *testing.T, q string, blessedAt int64) string {
	t.Helper()
	path := t.TempDir() + "/qbless.json"
	if err := os.WriteFile(path, []byte(`{"q":"`+q+`","sk":"97d975b9a3dc9872","blessedAt":`+strconv.FormatInt(blessedAt, 10)+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QBLESS_FILE", path)
	clearCache := func() {
		qblessMu.Lock()
		qblessLoaded = nil
		qblessChecked = time.Time{}
		qblessMu.Unlock()
	}
	clearCache()
	t.Cleanup(clearCache)
	return path
}

func TestQblessMintedTokenStructuralCheck(t *testing.T) {
	const testQ = "3795d28242a11619bc25f786f84e53d4-h-1788577217587-aaaaaaaaaaaaaaaa"
	qblessTestSession(t, testQ, time.Now().UnixMilli())

	tok, ok, err := qmintMint()
	if err != nil || !ok || tok == "" {
		t.Fatalf("qmintMint: ok=%v err=%v", ok, err)
	}
	if !qblessMintedToken(tok) {
		t.Fatal("minted token must be recognised as qbless-minted")
	}
	if got := qblessTokenQ(tok); got != testQ {
		t.Fatalf("token Q extraction: got %q want %q", got, testQ)
	}

	// A DB-reserve-style token embedding a different Q must NOT match.
	other := "U1dfV0VCIjIyMjIyMjIyMjMjIjIyMjIjIyMjMjIyMjIyMjIyMjIyMjMjIyMj#dGVzdA=="
	if qblessMintedToken(other) {
		t.Fatal("foreign token must not be recognised")
	}
	if qblessMintedToken("") {
		t.Fatal("empty token must not be recognised")
	}
}

// Regression for the 2026-09-05 outage: a process that marked the Q DEAD
// must recover by itself the moment the operator drops in a re-blessed
// session file (different Q). The old code checked a global DEAD flag
// before loading the file, so a re-bless could never clear it.
func TestQblessMintRecoversAfterReblessWhileDead(t *testing.T) {
	resetHealth(t)

	const oldQ = "3795d28242a11619bc25f786f84e53d4-h-1111111111111-11111111111111111"
	const newQ = "3795d28242a11619bc25f786f84e53d4-h-2222222222222-22222222222222222"

	path := qblessTestSession(t, oldQ, time.Now().UnixMilli())

	// Mint on the old Q, then mark it dead the hard way (3 verify FAILs
	// reported against the token's own Q — exactly what tryCompute does).
	if _, ok, _ := qmintMint(); !ok {
		t.Fatal("precondition: live Q must mint")
	}
	qHealth.mu.Lock()
	qHealth.failStreak = 3
	qHealth.deadQ = oldQ
	qHealth.mu.Unlock()

	if _, ok, _ := qmintMint(); ok {
		t.Fatal("dead Q must refuse to mint")
	}

	// Operator re-blesses: same path, NEW Q, newer mtime — no restart.
	time.Sleep(10 * time.Millisecond) // ensure mtime differs
	if err := os.WriteFile(path, []byte(`{"q":"`+newQ+`","sk":"97d975b9a3dc9872","blessedAt":`+strconv.FormatInt(time.Now().UnixMilli(), 10)+`}`), 0o600); err != nil {
		t.Fatal(err)
	}

	tok, ok, err := qmintMint()
	if err != nil || !ok || tok == "" {
		t.Fatalf("mint after re-bless must succeed without restart: ok=%v err=%v", ok, err)
	}
	if got := qblessTokenQ(tok); got != newQ {
		t.Fatalf("minted token must embed the NEW Q, got %q", got)
	}
}

// The other side of the same coin: a process restart (or first load) that
// re-reads the SAME dead Q must stay dead — the verdict must not silently
// vanish just because the cache was cold.
func TestQblessDeadQStaysDeadOnColdReload(t *testing.T) {
	resetHealth(t)
	const q = "3795d28242a11619bc25f786f84e53d4-h-3333333333333-33333333333333333"

	qblessTestSession(t, q, time.Now().UnixMilli())

	qHealth.mu.Lock()
	qHealth.failStreak = 3
	qHealth.deadQ = q
	qHealth.mu.Unlock()

	// Simulate a restart: drop the in-memory session cache; qblessLoad()
	// re-reads the same file. The per-Q verdict must survive.
	qblessMu.Lock()
	qblessLoaded = nil
	qblessChecked = time.Time{}
	qblessMu.Unlock()

	if _, ok, _ := qmintMint(); ok {
		t.Fatal("same dead Q must stay dead after a cold reload")
	}
}

func TestQblessHealthConcurrentReports(t *testing.T) {
	resetHealth(t)
	const q = "Q-concurrent"

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			qblessReportVerifyQ(q, i%2 == 0)
		}(i)
	}
	wg.Wait()

	// Must not panic or race; final state is whatever the interleaving made.
	_ = qblessQDead(q)
}
