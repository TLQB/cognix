// tokens.go — multi-account token pool.
//
// The Qwen daily-usage cap keys on the ACCOUNT (JWT), not the egress IP
// (probe quotakey, 2026-09-11: same token → RateLimited from two different
// IPs). The only practical throughput lever is therefore more accounts.
//
// QWEN_TOKENS="jwt1,jwt2,..." (comma-separated) or QWEN_TOKENS_FILE pointing
// at a file with one token per line (default ~/.config/qwen-proxy/tokens when
// it exists) builds the pool. Single-token setups behave exactly as before.
//
// Sticky selection: Active() always returns the FIRST token that is not in
// cooldown, so requests concentrate on one account until it hits the daily
// cap; the pool then advances to the next token and a cooldown timer (the
// server-reported "num" hours) brings the first one back. Round-robin per
// request would spread quota thin across accounts without extending total
// runtime, and would churn the chat↔token binding far more often.
package zbridge

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// TokenEntry is one account in the pool with its cooldown state.
type TokenEntry struct {
	Value         string
	CooldownUntil time.Time
	Permanent     bool // true = invalid token (401): never used again
}

// TokenPool is an ordered set of Qwen JWTs with sticky active selection.
type TokenPool struct {
	mu      sync.Mutex
	entries []TokenEntry
}

// tokenPool is the process-wide pool; nil in single-token setups, in which
// case every accessor falls back to the legacy session.Token behavior.
var tokenPool *TokenPool

// NewTokenPool builds a pool from non-empty, de-duplicated tokens (keeping
// first-seen order so the sticky selection is stable across restarts).
func NewTokenPool(tokens []string) *TokenPool {
	seen := map[string]bool{}
	var entries []TokenEntry
	for _, t := range tokens {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		entries = append(entries, TokenEntry{Value: t})
	}
	if len(entries) == 0 {
		return nil
	}
	return &TokenPool{entries: entries}
}

// Active returns the first token not in cooldown/permanent-fail, and its
// index. ("", -1) when every token is cooling down.
func (p *TokenPool) Active() (string, int) {
	if p == nil {
		return "", -1
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for i := range p.entries {
		e := &p.entries[i]
		if e.Permanent || now.Before(e.CooldownUntil) {
			continue
		}
		return e.Value, i
	}
	return "", -1
}

// Cooldown benches one token for d (the server-reported hours). Returns true
// when the active token changed because of it.
func (p *TokenPool) Cooldown(idx int, d time.Duration) bool {
	if p == nil || idx < 0 || idx >= len(p.entries) {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := &p.entries[idx]
	if e.Permanent {
		return false
	}
	e.CooldownUntil = time.Now().Add(d)
	return true
}

// Disable permanently benches an invalid token (401: expired/revoked JWT).
func (p *TokenPool) Disable(idx int) {
	if p == nil || idx < 0 || idx >= len(p.entries) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries[idx].Permanent = true
}

// Find returns the pool index of a token value, or -1.
func (p *TokenPool) Find(token string) int {
	if p == nil || token == "" {
		return -1
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.entries {
		if p.entries[i].Value == token {
			return i
		}
	}
	return -1
}

// Len / ReadyCount describe pool health (for the startup banner and logs).
func (p *TokenPool) Len() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

func (p *TokenPool) ReadyCount() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	n := 0
	for i := range p.entries {
		if !p.entries[i].Permanent && now.After(p.entries[i].CooldownUntil) {
			n++
		}
	}
	return n
}

// NextReset returns when the earliest cooling token becomes active again
// ("" when nothing is cooling) — used in the all-exhausted error message.
func (p *TokenPool) NextReset() time.Time {
	if p == nil {
		return time.Time{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var next time.Time
	for i := range p.entries {
		e := &p.entries[i]
		if e.Permanent || now.After(e.CooldownUntil) {
			continue
		}
		if next.IsZero() || e.CooldownUntil.Before(next) {
			next = e.CooldownUntil
		}
	}
	return next
}

// loadTokenPoolSources collects tokens from QWEN_TOKENS (comma-separated) and
// QWEN_TOKENS_FILE (one per line; defaults to ~/.config/qwen-proxy/tokens when
// present). Returns nil when no multi-token source yields anything.
func loadTokenPoolSources() []string {
	var tokens []string
	if env := os.Getenv("QWEN_TOKENS"); env != "" {
		for _, t := range strings.Split(env, ",") {
			if t = strings.TrimSpace(t); t != "" {
				tokens = append(tokens, t)
			}
		}
	}
	path := os.Getenv("QWEN_TOKENS_FILE")
	if path == "" {
		if def := defaultTokensFilePath(); def != "" {
			if _, err := os.Stat(def); err == nil {
				path = def
			}
		}
	}
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
					tokens = append(tokens, line)
				}
			}
		} else {
			log.Printf("[TokenPool] cannot read QWEN_TOKENS_FILE %s: %v", path, err)
		}
	}
	return tokens
}

// defaultTokensFilePath mirrors tokenFilePath() for the multi-token file.
func defaultTokensFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return home + "/.config/qwen-proxy/tokens"
}

// initTokenPool builds the global pool at startup. Returns a summary for the
// banner. The single-token path (QWEN_TOKEN / ~/.config/qwen-proxy/token)
// stays authoritative when no multi-token source is configured.
func initTokenPool() string {
	src := loadTokenPoolSources()
	if len(src) == 0 {
		return ""
	}
	pool := NewTokenPool(src)
	if pool == nil {
		return ""
	}
	tokenPool = pool
	return fmt.Sprintf("%d token(s) from QWEN_TOKENS/QWEN_TOKENS_FILE", pool.Len())
}

// activeToken returns the token every non-chat-bound Qwen call should use.
// With a pool it is the sticky-active entry; otherwise the legacy
// session.Token. ("", nil) means every account is cooling down.
func activeToken() (string, error) {
	if tokenPool != nil {
		if t, idx := tokenPool.Active(); idx >= 0 {
			return t, nil
		}
		next := tokenPool.NextReset()
		if next.IsZero() {
			return "", fmt.Errorf("token pool: all tokens are invalid (401)")
		}
		return "", fmt.Errorf("token pool: all tokens hit the daily usage limit; earliest reset %s", next.Format("15:04:05"))
	}
	if t := sessionToken(); t != "" {
		return t, nil
	}
	return "", fmt.Errorf("no Qwen token configured (QWEN_TOKEN / token file / QWEN_TOKENS)")
}

// cooldownToken benches the pool entry matching a token value for d.
// Returns true when a pool entry was benched.
func cooldownToken(token string, d time.Duration) bool {
	if tokenPool == nil {
		return false
	}
	idx := tokenPool.Find(token)
	if idx < 0 {
		return false
	}
	return tokenPool.Cooldown(idx, d)
}

// disableToken permanently benches the pool entry matching a token value.
func disableToken(token string) {
	if tokenPool == nil {
		return
	}
	if idx := tokenPool.Find(token); idx >= 0 {
		tokenPool.Disable(idx)
	}
}

// ---- chat ↔ token binding -------------------------------------------------
//
// A Qwen chat belongs to the account that minted it: referencing chat X with
// token Y ≠ owner gets a 404/401 upstream. The pool rotates the active token
// on RateLimited, so the token that created a chat must travel with the chat
// ID through completions and the garbage-collecting DELETE. A small map keyed
// by chatID carries that binding without changing any public interface.

var (
	chatTokenMu  sync.RWMutex
	chatTokenMap = map[string]string{}
)

// rememberChatToken records which account minted a chat.
func rememberChatToken(chatID, token string) {
	if chatID == "" || token == "" {
		return
	}
	chatTokenMu.Lock()
	chatTokenMap[chatID] = token
	chatTokenMu.Unlock()
}

// tokenForChat returns the minting account of a chat ("" = use active token:
// chats minted before a binding was recorded fall back to the active one).
func tokenForChat(chatID string) string {
	if chatID == "" {
		return ""
	}
	chatTokenMu.RLock()
	t := chatTokenMap[chatID]
	chatTokenMu.RUnlock()
	return t
}

// forgetChatToken drops a chat binding after the chat is deleted.
func forgetChatToken(chatID string) {
	if chatID == "" {
		return
	}
	chatTokenMu.Lock()
	delete(chatTokenMap, chatID)
	chatTokenMu.Unlock()
}

// tokenForRequest resolves the token a completions call must use: the
// chat-minting token when known (async pool chats), else the active token.
// The second return reports whether that token is usable; when the whole
// pool is cooling down the request fails fast with a clear message.
func tokenForRequest(chatID string) (string, error) {
	if t := tokenForChat(chatID); t != "" {
		if tokenPool != nil {
			if idx := tokenPool.Find(t); idx >= 0 {
				// The minting token is benched (daily cap). A fresh chat on
				// the next active token is the recovery path, not this token.
				if _, activeIdx := tokenPool.Active(); activeIdx >= 0 && idx != activeIdx {
					return "", errChatTokenBenched{}
				}
			}
		}
		return t, nil
	}
	return activeToken()
}

// errChatTokenBenched signals: the chat's minting account hit its daily cap
// and another account is active — re-mint the chat on the active token.
type errChatTokenBenched struct{}

func (errChatTokenBenched) Error() string {
	return "chat minting token is rate-limited; re-create chat on active token"
}

// activeTokenSafe is activeToken without the error: call sites that already
// hold a verified token context (session init ping, model list) simply fall
// back to the legacy session token when the pool has nothing active.
func activeTokenSafe() string {
	t, err := activeToken()
	if err != nil {
		return sessionToken()
	}
	return t
}
