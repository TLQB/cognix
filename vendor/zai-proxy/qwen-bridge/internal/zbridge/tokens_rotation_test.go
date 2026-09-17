package zbridge

// E2E: the daily-usage RateLimited rotates the token pool mid-request.
// The mock upstream answers RateLimited for chats minted with token A and a
// normal SSE completion for token B; the request must end in a good answer
// with token B benched-in for A (cooldown recorded on the pool entry).

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestRateLimitedRotatesToNextToken(t *testing.T) {
	if tokenPool != nil {
		t.Skip("global pool already set by another test")
	}
	var mu sync.Mutex
	sawTokens := map[string]int{}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie := r.Header.Get("Cookie")
		// The bridge's global cookie jar may append other cookies when the
		// full suite runs, so extract just the token= value.
		token := ""
		for _, part := range strings.Split(cookie, ";") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, "token=") {
				token = strings.TrimPrefix(part, "token=")
			}
		}
		if i := strings.Index(token, ","); i >= 0 { // Go joins multiple Cookie values with ", "
			token = token[:i]
		}
		mu.Lock()
		sawTokens[token]++
		mu.Unlock()

		switch r.URL.Path {
		case "/api/v2/chat/completions":
			if strings.Contains(cookie, "token-limited") {
				// Account-level daily cap, app-level JSON in a 200.
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"success":false,"request_id":"r","data":{"code":"RateLimited","details":"You've reached the upper limit for today's usage.","template":"You have reached the daily usage limit. Please wait {{num}} hours before trying again.","num":10}}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello from token B\"},\"index\":0}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		case "/api/v2/chats/new":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"success":true,"data":{"id":"chat-%s"}}`, token)
			return
		case "/api/v1/chats/":
			w.WriteHeader(200)
			return
		default:
			w.WriteHeader(200)
			fmt.Fprint(w, `{}`)
		}
	}))
	defer upstream.Close()

	oldBase := BASE_URL
	BASE_URL = upstream.URL
	t.Cleanup(func() { BASE_URL = oldBase })

	// Two-token pool: the first is the one the mock marks RateLimited.
	tokenPool = NewTokenPool([]string{"token-limited", "token-fresh"})
	t.Cleanup(func() { tokenPool = nil })

	handler := NewHandler()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"qwen3.7-plus","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+config.Auth.Token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// The stream holdback chunks the answer, so assert on fragments.
	body := rec.Body.String()
	if !strings.Contains(body, "en B") {
		t.Fatalf("request did not rotate to the fresh token. Body: %s", truncateForTest(body, 600))
	}
	if strings.Contains(body, "RateLimited") {
		t.Fatalf("RateLimited leaked to the client. Body: %s", truncateForTest(body, 600))
	}
	// The benched token must be recorded with a cooldown.
	if idx := tokenPool.Find("token-limited"); idx >= 0 {
		// entry benched via cooldownToken — verify Active() no longer returns it
		if tok, _ := tokenPool.Active(); tok != "token-fresh" {
			t.Fatalf("after rotation Active = %q, want token-fresh", tok)
		}
	}
	mu.Lock()
	nLimited := sawTokens["token-limited"]
	nFresh := sawTokens["token-fresh"]
	mu.Unlock()
	if nLimited == 0 || nFresh == 0 {
		t.Fatalf("expected traffic on both tokens, got limited=%d fresh=%d", nLimited, nFresh)
	}
}

func TestRateLimitedAllTokensExhaustedFails429(t *testing.T) {
	if tokenPool != nil {
		t.Skip("global pool already set by another test")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"success":false,"data":{"code":"RateLimited","details":"You've reached the upper limit for today's usage.","num":10}}`)
			return
		case "/api/v2/chats/new":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"success":true,"data":{"id":"chat-x"}}`)
			return
		default:
			w.WriteHeader(200)
		}
	}))
	defer upstream.Close()

	oldBase := BASE_URL
	BASE_URL = upstream.URL
	t.Cleanup(func() { BASE_URL = oldBase })

	tokenPool = NewTokenPool([]string{"t1", "t2"})
	t.Cleanup(func() { tokenPool = nil })

	handler := NewHandler()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"qwen3.7-plus","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+config.Auth.Token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "daily usage limit reached on all 2 tokens") {
		t.Fatalf("expected the all-exhausted error, got: %s", truncateForTest(body, 400))
	}
	// statusFromError must map it to 429 for OpenAI clients.
	if got := statusFromError("Qwen daily usage limit reached on all 2 tokens; earliest reset 23:00"); got != 429 {
		t.Fatalf("statusFromError(all-exhausted) = %d, want 429", got)
	}
}
