// Tests for the runtime-config endpoint (/api/v1/freebuff/config) and the
// captcha-cache Wake() resume path.

package zbridge

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFreebuffConfigEndpoint(t *testing.T) {
	origAuth := config.Auth.Enabled
	origToken := config.Auth.Token
	origThinking := config.ToolLoopThinking
	config.Auth.Enabled = true
	config.Auth.Token = "test-token"
	t.Cleanup(func() {
		config.Auth.Enabled = origAuth
		config.Auth.Token = origToken
		config.ToolLoopThinking = origThinking
	})

	rec := httptest.NewRecorder()

	// GET returns the current policy.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/freebuff/config", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	freebuffConfigHandler(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET status = %d, want 200", rec.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("GET body not JSON: %v", err)
	}
	if _, ok := got["toolLoopThinking"]; !ok {
		t.Fatalf("GET body missing toolLoopThinking: %s", rec.Body.String())
	}

	// POST flips the policy.
	config.ToolLoopThinking = "off"
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/freebuff/config",
		bytes.NewReader([]byte(`{"toolLoopThinking":"on"}`)))
	req.Header.Set("Authorization", "Bearer test-token")
	freebuffConfigHandler(rec, req)
	if rec.Code != 200 {
		t.Fatalf("POST status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if config.ToolLoopThinking != "on" {
		t.Fatalf("policy not applied: %q", config.ToolLoopThinking)
	}

	// The flip must be observable through the throttle path (mode=on never
	// touches Thinking).
	opts := SendOptions{}
	config.throttleThinkingForTurn(&opts, true)
	if opts.Thinking != nil {
		t.Fatal("mode=on must never touch Thinking")
	}

	// Invalid value → 400, policy unchanged.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/freebuff/config",
		bytes.NewReader([]byte(`{"toolLoopThinking":"maybe"}`)))
	req.Header.Set("Authorization", "Bearer test-token")
	freebuffConfigHandler(rec, req)
	if rec.Code != 400 {
		t.Fatalf("invalid POST status = %d, want 400", rec.Code)
	}
	if config.ToolLoopThinking != "on" {
		t.Fatalf("policy changed by invalid POST: %q", config.ToolLoopThinking)
	}

	// Normalize synonyms.
	for in, want := range map[string]string{"1": "on", "no": "off", " TRUE ": "on"} {
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodPost, "/api/v1/freebuff/config",
			bytes.NewReader([]byte(`{"toolLoopThinking":"`+in+`"}`)))
		req.Header.Set("Authorization", "Bearer test-token")
		freebuffConfigHandler(rec, req)
		if rec.Code != 200 || config.ToolLoopThinking != want {
			t.Fatalf("normalize %q: status=%d policy=%q want=%q", in, rec.Code, config.ToolLoopThinking, want)
		}
	}
}

func TestFreebuffConfigEndpointAuth(t *testing.T) {
	origEnabled := config.Auth.Enabled
	origToken := config.Auth.Token
	config.Auth.Enabled = true
	config.Auth.Token = "test-token"
	t.Cleanup(func() {
		config.Auth.Enabled = origEnabled
		config.Auth.Token = origToken
	})

	// The route is wrapped in authMiddleware — a bare request must 401
	// (the TUI always sends the proxy token).
	handler := authMiddleware(freebuffConfigHandler)
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/api/v1/freebuff/config", nil))
	if rec.Code != 401 {
		t.Fatalf("unauthenticated GET status = %d, want 401", rec.Code)
	}
}

func TestCaptchaCacheWake(t *testing.T) {
	// Simulate the idle pause: lastActive pushed 5 minutes back, so Run()'s
	// ticker would skip generation.
	c := &CaptchaCache{maxParams: 2}
	c.mu.Lock()
	c.lastActive = time.Now().Add(-5 * time.Minute)
	stale := c.lastActive
	c.mu.Unlock()

	c.Wake()

	c.mu.Lock()
	nowActive := c.lastActive
	c.mu.Unlock()
	if !nowActive.After(stale) {
		t.Fatal("Wake() must refresh lastActive past the idle threshold")
	}
	if nowActive.Sub(stale) < 4*time.Minute {
		t.Fatal("Wake() must move lastActive to now, not nudge it")
	}
}
