// Blackbox end-to-end test for POST /v1/slides through the REAL HTTP stack:
//
//	NewHandler -> authMiddleware -> slidesHandler -> sendToZAI ->
//	sendToZAIStream (mock upstream SSE) -> ParseSlideBlocks -> SlideDeck ->
//	deck SSE events on the wire.
//
// The Z.AI upstream is mocked (httptest) exactly like the vision E2E; the
// mock's completion stream carries a two-slide authoring output plus an edit
// turn, so the deck state machine (insert then update) is exercised through
// the real handler.
package tests

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"zai-api/internal/zbridge"
)

const slidesModelOutput = `<slide-css>
.slide { width: 1280px; height: 720px; font-family: sans-serif; }
h1 { color: #141618; }
</slide-css>

<slide 1 title="Zagent Overview">
<section class="slide"><h1>Zagent</h1><p>A Zed fork with an embedded model proxy.</p></section>
</slide>

<slide 2 title="Vision">
<section class="slide"><h1>Vision</h1><p>Image inputs via the files pipeline.</p></section>
</slide>`

const slidesEditOutput = `<slide 2 title="[update] Vision v2">
<section class="slide"><h1>Vision v2</h1><p>Updated after review.</p></section>
</slide>

<slide 3 title="Roadmap">
<section class="slide"><h1>Roadmap</h1><p>Editor wiring next.</p></section>
</slide>`

type slidesMock struct {
	mu              sync.Mutex
	completionsBody []byte
	urn             int
}

func (sm *slidesMock) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, modelsPayload)

		case r.URL.Path == "/api/v2/chat/completions" && r.Method == "POST":
			body, _ := io.ReadAll(r.Body)
			sm.mu.Lock()
			sm.completionsBody = body
			sm.urn++
			turn := sm.urn
			sm.mu.Unlock()
			out := slidesModelOutput
			if turn == 2 {
				out = slidesEditOutput
			}
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			// Stream in fragments so chunk reassembly is exercised.
			mid := len(out) / 2
			for _, frag := range []string{out[:mid], out[mid:]} {
				e, _ := json.Marshal(map[string]interface{}{"data": map[string]interface{}{"delta_content": frag}})
				fmt.Fprintf(w, "data: %s\n\n", e)
				if flusher != nil {
					flusher.Flush()
				}
			}
			fmt.Fprintf(w, "data: %s\n\n", `{"data":{"phase":"done"}}`)
			fmt.Fprintf(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}

		case strings.HasPrefix(r.URL.Path, "/api/v1/chats/") && r.Method == "DELETE":
			io.WriteString(w, "true")

		default:
			http.NotFound(w, r)
		}
	}
}

func (sm *slidesMock) getCompletionsBody() []byte {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	out := make([]byte, len(sm.completionsBody))
	copy(out, sm.completionsBody)
	return out
}

func setupSlidesE2E(t *testing.T) (*slidesMock, func()) {
	t.Helper()
	sm := &slidesMock{}
	upstream := httptest.NewServer(sm.handler())

	oldBase := zbridge.BASE_URL
	zbridge.BASE_URL = upstream.URL
	restoreSession := zbridge.OverrideSessionState("test-token", "test-user", true)
	cfg := zbridge.GetConfig()
	oldAgentMode := cfg.AgentMode
	cfg.AgentMode = true
	zbridge.SeedCaptchaParam("test-captcha-param")
	zbridge.FlushModelsCache()

	cleanup := func() {
		time.Sleep(50 * time.Millisecond)
		cfg.AgentMode = oldAgentMode
		restoreSession()
		upstream.Close()
		zbridge.BASE_URL = oldBase
	}
	return sm, cleanup
}

func postSlides(t *testing.T, conversationID string, prompt string) *httptest.ResponseRecorder {
	t.Helper()
	cfg := zbridge.GetConfig()
	body := map[string]interface{}{
		"model":           "glm-5.3-flash",
		"stream":          true,
		"conversation_id": conversationID,
		"messages": []map[string]interface{}{
			{"role": "user", "content": prompt},
		},
	}
	bodyJSON, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/v1/slides", bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Auth.Token)
	rec := httptest.NewRecorder()
	zbridge.NewHandler().ServeHTTP(rec, req)
	return rec
}

type slideDeckEvent struct {
	Type           string `json:"type"`
	ConversationID string `json:"conversation_id"`
	GlobalCSS      string `json:"global_css"`
	Slides         []struct {
		Position int    `json:"position"`
		Title    string `json:"title"`
		HTML     string `json:"html"`
	} `json:"slides"`
}

func deckEventFrom(t *testing.T, sse string) slideDeckEvent {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(sse))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(payload), &probe) != nil {
			continue
		}
		if probe.Type == "deck" {
			var ev slideDeckEvent
			if err := json.Unmarshal([]byte(payload), &ev); err != nil {
				t.Fatalf("deck event decode: %v", err)
			}
			return ev
		}
	}
	t.Fatalf("no deck event in SSE:\n%s", sse)
	return slideDeckEvent{}
}

// TestSlidesEndToEnd generates a two-slide deck, then edits it in a second
// conversation turn (update slide 2 + insert slide 3) and asserts the deck
// state carried across turns.
func TestSlidesEndToEnd(t *testing.T) {
	sm, cleanup := setupSlidesE2E(t)
	defer cleanup()

	// ── Turn 1: generate the initial deck ──
	rec := postSlides(t, "conv-slides-e2e", "Make a short deck about Zagent")
	if rec.Code != 200 {
		t.Fatalf("turn1 status = %d, body = %s", rec.Code, rec.Body.String())
	}
	deck1 := deckEventFrom(t, rec.Body.String())
	if deck1.Type != "deck" || deck1.ConversationID != "conv-slides-e2e" {
		t.Fatalf("deck1 = %+v", deck1)
	}
	if len(deck1.Slides) != 2 {
		t.Fatalf("turn1 slides = %d, want 2", len(deck1.Slides))
	}
	if deck1.Slides[0].Title != "Zagent Overview" || !strings.Contains(deck1.Slides[0].HTML, "Zagent") {
		t.Errorf("turn1 slide1 = %+v", deck1.Slides[0])
	}
	if deck1.Slides[1].Title != "Vision" {
		t.Errorf("turn1 slide2 = %+v", deck1.Slides[1])
	}
	if !strings.Contains(deck1.GlobalCSS, "1280px") {
		t.Errorf("global css = %q", deck1.GlobalCSS)
	}

	// The prompt sent upstream carried the authoring contract.
	var sent struct {
		SignaturePrompt string `json:"signature_prompt"`
	}
	if err := json.Unmarshal(sm.getCompletionsBody(), &sent); err != nil {
		t.Fatalf("upstream body decode: %v", err)
	}
	if !strings.Contains(sent.SignaturePrompt, "Make a short deck about Zagent") ||
		!strings.Contains(sent.SignaturePrompt, "<slide-css>") {
		t.Errorf("upstream prompt missing user ask or authoring contract")
	}

	// ── Turn 2: edit the deck (update 2, insert 3) — same conversation id ──
	rec2 := postSlides(t, "conv-slides-e2e", "Update slide 2 and add a roadmap slide")
	if rec2.Code != 200 {
		t.Fatalf("turn2 status = %d, body = %s", rec2.Code, rec2.Body.String())
	}
	deck2 := deckEventFrom(t, rec2.Body.String())
	if len(deck2.Slides) != 3 {
		t.Fatalf("turn2 slides = %d, want 3: %+v", len(deck2.Slides), deck2.Slides)
	}
	if deck2.Slides[0].Title != "Zagent Overview" {
		t.Errorf("turn2 slide1 = %+v", deck2.Slides[0])
	}
	if deck2.Slides[1].Title != "Vision v2" || !strings.Contains(deck2.Slides[1].HTML, "Updated after review") {
		t.Errorf("turn2 slide2 not updated: %+v", deck2.Slides[1])
	}
	if deck2.Slides[2].Title != "Roadmap" || deck2.Slides[2].Position != 3 {
		t.Errorf("turn2 slide3 = %+v", deck2.Slides[2])
	}
	// CSS survived across turns (set only in turn 1).
	if !strings.Contains(deck2.GlobalCSS, "1280px") {
		t.Errorf("turn2 css lost: %q", deck2.GlobalCSS)
	}
}
