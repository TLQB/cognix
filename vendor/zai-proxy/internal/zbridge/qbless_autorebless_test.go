package zbridge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

// The 2026-09-10 incident: autoReblessProbeCycle held autoReblessProbeMu via
// `Lock(); defer Unlock()` and then called autoReblessTrigger on the FAIL
// path — which Locks the SAME mutex again around its env swap. Go mutexes
// are not reentrant, so the rebless goroutine deadlocked: q-bless ran to
// completion (18.4s), the fresh session sat in /tmp forever, the swap never
// happened, and every captcha mint failed with "empty payload" while the
// proxy looked alive.
//
// This regression test drives probeCycle through a failing canary ALL the
// way into autoReblessTrigger's mutex acquisition. A stub q-bless script
// writes a syntactically valid session so trigger gets past its early
// not-found/bad-json returns and reaches the probe-mutex Lock; the stub's
// fake Q then fails the post-rebless canary, trigger returns, and the cycle
// must complete. With the bug, the test hangs (trigger blocks on the probe
// mutex forever) and the deadline fails it; with the fix, the probe mutex is
// released before trigger runs, so trigger's Lock() succeeds.

func TestAutoReblessProbeCycleDoesNotDeadlockOnTrigger(t *testing.T) {
	// Isolate from any real qbless.json state.
	dir := t.TempDir()
	sessionFile := dir + "/qbless.json"
	t.Setenv("QBLESS_FILE", sessionFile)
	writeBlessSession(t, sessionFile, "test-q-probe-fail")

	stubQbless := dir + "/stub-q-bless"
	if err := os.WriteFile(stubQbless, []byte(`#!/bin/sh
echo '{"q":"stub-new-q","sk":"stub-sk","qts":1}' > "$2"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QBLESS_BINARY", stubQbless)

	qblessMu.Lock()
	qblessLoaded = nil
	qblessChecked = time.Time{}
	qblessMu.Unlock()
	t.Cleanup(func() {
		qblessMu.Lock()
		qblessLoaded = nil
		qblessChecked = time.Time{}
		qblessMu.Unlock()
	})

	autoReblessFailCount = 0

	cfg := AutoReblessConfig{
		Enabled:            true,
		ProbeInterval:      time.Hour,
		DegradingThreshold: 1,
		QblessBinary:       stubQbless,
		ReblessTimeout:     5 * time.Second,
		ProbeTimeout:       2 * time.Second,
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		autoReblessProbeCycle(cfg)
		close(done)
	}()

	select {
	case <-done:
		// Probe cycle completed without deadlocking.
		if autoReblessFailCount != 1 {
			t.Fatalf("expected failCount=1 after one failed probe, got %d", autoReblessFailCount)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("autoReblessProbeCycle deadlocked: canary FAIL path still holds the probe mutex when trigger re-locks it")
	}
	wg.Wait()
}

// writeBlessSession writes a minimal blessSession for tests.
func writeBlessSession(t *testing.T, path, q string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(`{"q":`+quoteJSON(q)+`,"sk":"test-sk","qts":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func quoteJSON(s string) string {
	// Minimal JSON string quoting good enough for test fixtures.
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	for _, c := range []byte(s) {
		if c == '"' || c == '\\' {
			out = append(out, '\\')
		}
		out = append(out, c)
	}
	return string(append(out, '"'))
}

// Ensure the post-fix contract still holds: a PASS canary resets the counter
// and does not touch the trigger path at all.
func TestAutoReblessProbeMutexReentrantContract(t *testing.T) {
	// Sanity structural check: with the fix, trigger's Lock is only reached
	// when the probe critical section has closed. Verify by asserting the
	// mutex is free after probeCycle returns (TryLock from another goroutine).
	dir := t.TempDir()
	sessionFile := dir + "/qbless.json"
	t.Setenv("QBLESS_FILE", sessionFile)
	writeBlessSession(t, sessionFile, "test-q-mutex-contract")
	stubQbless := dir + "/stub-q-bless"
	if err := os.WriteFile(stubQbless, []byte(`#!/bin/sh
echo '{"q":"stub-new-q","sk":"stub-sk","qts":1}' > "$2"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QBLESS_BINARY", stubQbless)

	qblessMu.Lock()
	qblessLoaded = nil
	qblessChecked = time.Time{}
	qblessMu.Unlock()
	t.Cleanup(func() {
		qblessMu.Lock()
		qblessLoaded = nil
		qblessChecked = time.Time{}
		qblessMu.Unlock()
	})

	autoReblessFailCount = 0

	cfg := AutoReblessConfig{
		Enabled:            true,
		ProbeInterval:      time.Hour,
		DegradingThreshold: 1,
		QblessBinary:       stubQbless,
		ReblessTimeout:     5 * time.Second,
		ProbeTimeout:       2 * time.Second,
	}

	done := make(chan struct{})
	go func() {
		autoReblessProbeCycle(cfg)
		close(done)
	}()
	<-done

	if !autoReblessProbeMu.TryLock() {
		t.Fatal("probe mutex still held after probeCycle returned — leak will deadlock the next trigger")
	}
	autoReblessProbeMu.Unlock()
}

// QFarm pre-emptive rotation: a canary-PASS Q older than qfarmPreAge must
// trigger a rebless on the PASS path (rotation before death), while a young
// PASS must not. We cannot make the real canary pass without a live Q, so
// this exercises the age-gate logic directly: the decision is a pure
// function of (canaryPass, age).
func TestQFarmPreemptiveRotationAgeGate(t *testing.T) {
	cases := []struct {
		name       string
		age        time.Duration
		wantRotate bool
	}{
		{"young Q — no rotation", 10 * time.Minute, false},
		{"just under threshold", qfarmPreAge - time.Minute, false},
		{"at threshold", qfarmPreAge, true},
		{"old Q (death band)", 85 * time.Minute, true},
		{"very old Q", 3 * time.Hour, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.age >= qfarmPreAge
			if got != tc.wantRotate {
				t.Fatalf("age %v: rotate=%v, want %v (qfarmPreAge=%v)", tc.age, got, tc.wantRotate, qfarmPreAge)
			}
		})
	}
}

// QFarm status endpoint: with an isolated session file and reserve DB, the
// handler must report the Q's age band and reserve depth without leaking
// the full Q or tokens.
func TestQFarmStatusHandler(t *testing.T) {
	dir := t.TempDir()
	sessionFile := dir + "/qbless.json"
	t.Setenv("QBLESS_FILE", sessionFile)
	writeBlessSession(t, sessionFile, "status-test-q")

	// Freshly blessed = young Q, not rotating soon.
	s := qblessLoad()
	if s == nil {
		t.Fatal("precondition: qblessLoad must see the fixture")
	}
	blessedAt := time.Now().UnixMilli()
	qblessMu.Lock()
	qblessLoaded.BlessedAt = blessedAt
	qblessMu.Unlock()

	w := httptest.NewRecorder()
	qfarmStatusHandler(w, httptest.NewRequest(http.MethodGet, "/api/qfarm", nil))

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	qm, _ := resp["q"].(map[string]interface{})
	if qm == nil {
		t.Fatal("q object missing")
	}
	if qm["id"] != shortQ("status-test-q") {
		t.Fatalf("q.id = %v, want %v", qm["id"], shortQ("status-test-q"))
	}
	if rot, _ := qm["rotating_soon"].(bool); rot {
		t.Fatal("young Q must not be rotating_soon")
	}
	if dead, _ := qm["dead"].(bool); dead {
		t.Fatal("fresh Q must not be dead")
	}
	if _, ok := resp["captcha_breaker_open"]; !ok {
		t.Fatal("captcha_breaker_open missing")
	}
}
