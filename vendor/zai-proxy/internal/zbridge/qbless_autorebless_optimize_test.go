package zbridge

import (
	"testing"
	"time"
)

func TestAutoReblessBackoff(t *testing.T) {
	cfg := AutoReblessConfig{ProbeInterval: 10 * time.Minute}
	autoReblessMu.Lock()
	oldT, oldTs := autoReblessTriggers, autoReblessLastTrigger
	autoReblessMu.Unlock()
	defer func() {
		autoReblessMu.Lock()
		autoReblessTriggers, autoReblessLastTrigger = oldT, oldTs
		autoReblessMu.Unlock()
	}()
	autoReblessMu.Lock()
	autoReblessTriggers = 0
	autoReblessLastTrigger = time.Time{}
	autoReblessMu.Unlock()
	if r := autoReblessBackoffRemaining(cfg, time.Now()); r != 0 {
		t.Fatalf("triggers=0: want 0, got %s", r)
	}
	now := time.Now()
	autoReblessMu.Lock()
	autoReblessTriggers = 2
	autoReblessLastTrigger = now
	autoReblessMu.Unlock()
	r := autoReblessBackoffRemaining(cfg, now.Add(15*time.Minute))
	if r <= 4*time.Minute || r > 5*time.Minute {
		t.Fatalf("triggers=2 after 15m: want ~5m (gap 20m), got %s", r)
	}
	autoReblessMu.Lock()
	autoReblessTriggers = 30
	autoReblessLastTrigger = now
	autoReblessMu.Unlock()
	if r := autoReblessBackoffRemaining(cfg, now.Add(3*time.Hour)); r != 0 {
		t.Fatalf("capped gap (2h) should expire after 3h, got %s", r)
	}
}

func TestAutoReblessConfigReserveTarget(t *testing.T) {
	t.Setenv("QFARM_RESERVE_TARGET", "37")
	if cfg := resolveAutoReblessConfig(); cfg.ReserveTarget != 37 {
		t.Fatalf("ReserveTarget = %d, want 37", cfg.ReserveTarget)
	}
	t.Setenv("QFARM_RESERVE_TARGET", "")
	if cfg := resolveAutoReblessConfig(); cfg.ReserveTarget != qfarmReserveTarget {
		t.Fatalf("default ReserveTarget = %d, want %d", cfg.ReserveTarget, qfarmReserveTarget)
	}
}
