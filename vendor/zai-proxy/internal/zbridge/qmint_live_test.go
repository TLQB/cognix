package zbridge

import (
	"os"
	"testing"
)

// TestQblessMintLive mints one device token on the current qbless.json Q
// and runs a live captcha verify with it. Skipped unless EXP_QMINT=1.
//
//	go test ./internal/zbridge -run TestQblessMintLive -v -count=1
//
// Use it after a fresh q-bless to prove the new Q really works before
// blaming the bridge for "captcha generation returned empty payload".
func TestQblessMintLive(t *testing.T) {
	if os.Getenv("EXP_QMINT") != "1" {
		t.Skip("set EXP_QMINT=1 to run the live qbless mint probe")
	}
	// tryCompute → removeToken → globalDB.Exec panics on the nil DB a bare
	// test binary has; open the real token DB like bridge startup does.
	if err := initDB(); err != nil {
		t.Fatalf("initDB: %v", err)
	}
	s := qblessLoad()
	if s == nil {
		t.Fatal("no usable qbless.json session found")
	}
	t.Logf("session: Q=%s blessedAt=%d", s.Q, s.BlessedAt)
	if qblessIsDead() {
		t.Fatal("Q is marked DEAD in the health tracker — run q-bless to re-bless")
	}
	tok, ok, err := qmintMint()
	if err != nil {
		t.Fatalf("qmintMint: %v", err)
	}
	if !ok {
		t.Fatal("qmintMint returned ok=false (dead Q / no session)")
	}
	t.Logf("minted token: %.60s…", tok)
	payload, err := tryCompute(tok)
	if err != nil {
		t.Fatalf("tryCompute: %v", err)
	}
	if payload == "" {
		t.Fatal("verify returned EMPTY payload — the Q is not blessed (or dead). Run q-bless again.")
	}
	t.Logf("verify OK: payload %d bytes", len(payload))
}
