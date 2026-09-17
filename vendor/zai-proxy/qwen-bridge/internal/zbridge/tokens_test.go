package zbridge

import (
	"strings"
	"testing"
	"time"
)

func resetTokenPool(t *testing.T) func() {
	t.Helper()
	oldPool, oldSession := tokenPool, session
	return func() {
		tokenPool, session = oldPool, oldSession
	}
}

func TestTokenPoolStickySelection(t *testing.T) {
	defer resetTokenPool(t)()
	pool := NewTokenPool([]string{"tok-a", "tok-b", "tok-c"})
	if pool.Len() != 3 {
		t.Fatalf("Len = %d, want 3", pool.Len())
	}
	// Sticky: the first ready token serves every call until benched.
	for i := 0; i < 5; i++ {
		tok, idx := pool.Active()
		if tok != "tok-a" || idx != 0 {
			t.Fatalf("Active #%d = (%q,%d), want (tok-a,0)", i, tok, idx)
		}
	}
	// Bench the active token → the next one takes over.
	if !pool.Cooldown(0, 2*time.Hour) {
		t.Fatal("Cooldown(0) = false")
	}
	tok, idx := pool.Active()
	if tok != "tok-b" || idx != 1 {
		t.Fatalf("after bench tok-a: Active = (%q,%d), want (tok-b,1)", tok, idx)
	}
	// Short cooldown on tok-b expires → sticky selection returns to it.
	if !pool.Cooldown(1, 50*time.Millisecond) {
		t.Fatal("Cooldown(1) = false")
	}
	time.Sleep(60 * time.Millisecond)
	if _, idx := pool.Active(); idx != 1 {
		t.Fatalf("after short bench: idx = %d, want 1", idx)
	}
	// Bench everything → Active is empty, NextReset reports the earliest.
	pool.Cooldown(1, 3*time.Hour)
	pool.Cooldown(2, 1*time.Hour)
	if tok, idx := pool.Active(); tok != "" || idx != -1 {
		t.Fatalf("all benched: Active = (%q,%d), want empty", tok, idx)
	}
	next := pool.NextReset()
	if next.IsZero() {
		t.Fatal("NextReset zero with 2 cooling tokens")
	}
	// Disable is permanent — never selected again.
	pool2 := NewTokenPool([]string{"x", "y"})
	pool2.Disable(0)
	if tok, _ := pool2.Active(); tok != "y" {
		t.Fatalf("after Disable(0): tok = %q, want y", tok)
	}
	pool2.Cooldown(FindHelper(t, pool2, "y"), time.Hour)
	if tok, _ := pool2.Active(); tok != "" {
		t.Fatalf("all dead/benched: tok = %q, want empty", tok)
	}
}

func FindHelper(t *testing.T, p *TokenPool, v string) int {
	t.Helper()
	idx := p.Find(v)
	if idx < 0 {
		t.Fatalf("Find(%q) = %d", v, idx)
	}
	return idx
}

func TestTokenPoolDedupAndEmpty(t *testing.T) {
	if NewTokenPool(nil) != nil {
		t.Fatal("NewTokenPool(nil) != nil")
	}
	if NewTokenPool([]string{"", "  "}) != nil {
		t.Fatal("blank tokens should yield nil pool")
	}
	p := NewTokenPool([]string{"a", "a", "b"})
	if p.Len() != 2 {
		t.Fatalf("dedup: Len = %d, want 2", p.Len())
	}
}

func TestChatTokenBinding(t *testing.T) {
	defer resetTokenPool(t)()
	rememberChatToken("chat-1", "tok-a")
	if got := tokenForChat("chat-1"); got != "tok-a" {
		t.Fatalf("tokenForChat = %q, want tok-a", got)
	}
	forgetChatToken("chat-1")
	if got := tokenForChat("chat-1"); got != "" {
		t.Fatalf("after forget: %q, want empty", got)
	}
}

func TestActiveTokenAllExhausted(t *testing.T) {
	defer resetTokenPool(t)()
	tokenPool = NewTokenPool([]string{"t1", "t2"})
	tokenPool.Cooldown(0, time.Hour)
	tokenPool.Cooldown(1, 2*time.Hour)
	_, err := activeToken()
	if err == nil {
		t.Fatal("activeToken should fail when every token is cooling")
	}
	if !strings.Contains(err.Error(), "daily usage limit") {
		t.Fatalf("error should mention the daily cap, got: %v", err)
	}
}

func TestCooldownTokenByValue(t *testing.T) {
	defer resetTokenPool(t)()
	tokenPool = NewTokenPool([]string{"t1", "t2"})
	if !cooldownToken("t2", time.Hour) {
		t.Fatal("cooldownToken(t2) = false")
	}
	if tok, _ := tokenPool.Active(); tok != "t1" {
		t.Fatalf("t2 benched but Active = %q", tok)
	}
	if cooldownToken("nope", time.Hour) {
		t.Fatal("cooldownToken(unknown) = true")
	}
}
