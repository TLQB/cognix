package zbridge

// Slides (PPT) generation on top of the plain chat pipeline.
//
// chat.z.ai's native PPT flow is an agent conversation whose tool calls
// (initialize_design / insert_page / update_page / remove_slides) emit one
// HTML page per slide into a server-side slides_dir, exportable through the
// /convert/ppt/stream and /sandbox/html-to-ppt endpoints. That flow needs the
// aislides agent models, which the free web-token bridge cannot select
// directly.
//
// This file provides the same outcome with the models the bridge DOES have:
// /v1/slides drives a normal completion with a slide-authoring contract,
// parses the model's <slide> blocks into separate HTML pages, keeps a
// per-conversation slide store (add/update/remove — mirroring insert_page /
// update_page / remove_slides semantics), and streams JSON progress events.
// The HTML pages are drop-in compatible with the upstream export path: POST
// them to /sandbox/html-to-ppt (files.html + files.css with a global_css)
// to get a PPTX, or /convert/ppt/stream for the streamed conversion.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ============================================================================
// SLIDE MODEL & STORE
// ============================================================================

// Slide is one generated page. HTML is a full standalone page body fragment
// (16:9 fixed-size deck page, same shape the web client's insert_page emits).
type Slide struct {
	Position int    `json:"position"`
	Title    string `json:"title"`
	HTML     string `json:"html"`
}

// SlideDeck is the server-side per-conversation slide store, mirroring the
// web client's slides_dir brief: ordered pages plus the global stylesheet.
type SlideDeck struct {
	mu        sync.Mutex
	GlobalCSS string
	slides    map[int]Slide
}

func NewSlideDeck() *SlideDeck {
	return &SlideDeck{slides: make(map[int]Slide)}
}

// Apply executes one slide operation against the deck. Ops mirror the web
// agent's tool names.
type SlideOp struct {
	Tool     string `json:"tool"`     // insert_page | update_page | remove_slides | set_css
	Position int    `json:"position"` // 1-based slide number
	Count    int    `json:"count"`    // remove_slides: how many pages from Position
	Title    string `json:"title"`
	HTML     string `json:"html"`
	CSS      string `json:"css"`
}

func (d *SlideDeck) Apply(op SlideOp) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch op.Tool {
	case "insert_page":
		if op.Position <= 0 {
			return fmt.Errorf("insert_page: position must be >= 1")
		}
		if op.HTML == "" {
			return fmt.Errorf("insert_page: html is required")
		}
		// Insert at position: shift existing pages up (same as the web
		// client's "Insert a slide after the second slide" -> position [3]).
		d.shiftFrom(op.Position, +1)
		d.slides[op.Position] = Slide{Position: op.Position, Title: op.Title, HTML: op.HTML}
	case "update_page":
		cur, ok := d.slides[op.Position]
		if !ok {
			return fmt.Errorf("update_page: no slide at position %d", op.Position)
		}
		if op.Title != "" {
			cur.Title = op.Title
		}
		if op.HTML != "" {
			cur.HTML = op.HTML
		}
		d.slides[op.Position] = cur
	case "remove_slides":
		count := op.Count
		if count <= 0 {
			count = 1
		}
		for i := 0; i < count; i++ {
			delete(d.slides, op.Position+i)
		}
		d.shiftFrom(op.Position+count, -count)
	case "set_css":
		d.GlobalCSS = op.CSS
	default:
		return fmt.Errorf("unknown slide op %q", op.Tool)
	}
	return nil
}

// shiftFrom moves every slide at position >= from by delta, renumbering.
// Must be called with the lock held.
func (d *SlideDeck) shiftFrom(from, delta int) {
	positions := make([]int, 0, len(d.slides))
	for p := range d.slides {
		if p >= from {
			positions = append(positions, p)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(positions)))
	for _, p := range positions {
		s := d.slides[p]
		delete(d.slides, p)
		s.Position = p + delta
		d.slides[s.Position] = s
	}
}

// List returns the deck in page order.
func (d *SlideDeck) List() []Slide {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Slide, 0, len(d.slides))
	for _, s := range d.slides {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Position < out[j].Position })
	return out
}

// Global deck registry keyed by conversation id.
var (
	decksMu sync.Mutex
	decks   = make(map[string]*SlideDeck)
)

func deckFor(conversationID string) *SlideDeck {
	decksMu.Lock()
	defer decksMu.Unlock()
	if decks[conversationID] == nil {
		decks[conversationID] = NewSlideDeck()
	}
	return decks[conversationID]
}

// ============================================================================
// MODEL OUTPUT CONTRACT
// ============================================================================

// The authoring contract asks the model for one fenced block per slide:
//
//	<slide n title="...">
//	...html body...
//	</slide>
//
// plus an optional <slide-css> block. This is trivially parseable and, unlike
// a JSON array, survives streaming truncation gracefully (each block is
// independent).
var (
	slideBlockRe = regexp.MustCompile(`(?is)<slide\s+(\d+)\s+title="([^"]*)"\s*>(.*?)</slide>`)
	slideCSSRe   = regexp.MustCompile(`(?is)<slide-css>(.*?)</slide-css>`)
)

// ParseSlideBlocks extracts slide ops from a completion in the authoring
// contract above. update/remove ops are written by the model in the same
// block grammar via a leading verb: title prefix "[update]" turns the block
// into an update_page op; "[remove]" blocks only need the position/count.
func ParseSlideBlocks(text string) []SlideOp {
	var ops []SlideOp
	if m := slideCSSRe.FindStringSubmatch(text); m != nil {
		ops = append(ops, SlideOp{Tool: "set_css", CSS: strings.TrimSpace(m[1])})
	}
	for _, m := range slideBlockRe.FindAllStringSubmatch(text, -1) {
		pos, _ := strconv.Atoi(m[1])
		title := m[2]
		body := strings.TrimSpace(m[3])
		switch {
		case strings.HasPrefix(title, "[remove]"):
			ops = append(ops, SlideOp{Tool: "remove_slides", Position: pos})
		case strings.HasPrefix(title, "[update]"):
			ops = append(ops, SlideOp{Tool: "update_page", Position: pos,
				Title: strings.TrimSpace(strings.TrimPrefix(title, "[update]")), HTML: body})
		default:
			ops = append(ops, SlideOp{Tool: "insert_page", Position: pos, Title: title, HTML: body})
		}
	}
	return ops
}

// SlideAuthoringPrompt is the system-side contract appended to the user ask.
const SlideAuthoringPrompt = `You are a professional slide author. Respond with ONLY the following structure, no commentary:

<slide-css>
/* one shared stylesheet for the whole deck; 1280x720 page geometry */
</slide-css>

<slide 1 title="...">
<section class="slide">...complete standalone HTML body for slide 1...</section>
</slide>

<slide 2 title="...">...</slide>

Rules: every slide is a self-contained HTML fragment sized for a 1280x720
page. Use the shared classes from <slide-css> instead of inline <style>.
To edit an existing deck, use title prefixes: "[update] Title" rewrites the
slide at that position; a block with body "remove" and title "[remove]"
deletes the slide at that position.`

// ============================================================================
// HTTP HANDLER
// ============================================================================

// slidesHandler implements POST /v1/slides (SSE): accepts an OpenAI-style
// request {model, messages, stream}, drives the completion through the normal
// send pipeline, applies every parsed slide op to the conversation deck, and
// streams progress events. Non-streaming requests get one JSON result.
type slidesRequest struct {
	Model        string          `json:"model"`
	Messages     json.RawMessage `json:"messages"`
	Stream       *bool           `json:"stream"`
	Conversation string          `json:"conversation_id"`
}

func slidesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	var req slidesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, formatOpenAIError("invalid JSON: "+err.Error(), "invalid_request_error", nil))
		return
	}
	if len(req.Messages) == 0 {
		writeJSON(w, http.StatusBadRequest, formatOpenAIError("messages is required", "invalid_request_error", nil))
		return
	}
	convID := req.Conversation
	if convID == "" {
		convID = randomUUID()
	}

	// Flatten the conversation for the prompt, then append the authoring
	// contract. Plain text in, HTML slides out.
	var msgs []Message
	if err := json.Unmarshal(req.Messages, &msgs); err != nil {
		writeJSON(w, http.StatusBadRequest, formatOpenAIError("messages must be an array", "invalid_request_error", nil))
		return
	}
	var sb strings.Builder
	for _, m := range msgs {
		s := extractAnthropicContent(json.RawMessage(m.Content))
		if s == "" {
			s = string(m.Content)
		}
		sb.WriteString(strings.ToUpper(m.Role[:1]) + m.Role[1:] + ": " + s + "\n")
	}
	deck := deckFor(convID)
	if existing := deck.List(); len(existing) > 0 {
		sb.WriteString(fmt.Sprintf("\nCurrent deck state (%d slides):", len(existing)))
		for _, s := range existing {
			sb.WriteString(fmt.Sprintf("\n- slide %d: %s", s.Position, s.Title))
		}
	}
	prompt := sb.String() + "\n\n" + SlideAuthoringPrompt

	model := req.Model
	if model == "" {
		model = "glm-5.3-flash"
	}
	opts := SendOptions{Model: model}
	ch, err := sendToZAI(prompt, opts)
	if err != nil {
		writeJSON(w, 502, formatOpenAIError(err.Error(), "api_error", nil))
		return
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	writeEv := func(v interface{}) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	var full strings.Builder
	for res := range ch {
		if res.Err != nil {
			writeEv(map[string]interface{}{"type": "error", "error": res.Err.Error()})
			return
		}
		if res.Chunk != "" {
			full.WriteString(res.Chunk)
			writeEv(map[string]interface{}{"type": "progress", "chars": len(full.String())})
		}
	}

	ops := ParseSlideBlocks(full.String())
	if len(ops) == 0 {
		writeEv(map[string]interface{}{"type": "error", "error": "model did not produce any slide blocks"})
		return
	}
	for _, op := range ops {
		if aerr := deck.Apply(op); aerr != nil {
			writeEv(map[string]interface{}{"type": "op_error", "tool": op.Tool, "error": aerr.Error()})
			continue
		}
		writeEv(map[string]interface{}{"type": "op", "tool": op.Tool, "position": op.Position})
	}
	final := deck.List()
	writeEv(map[string]interface{}{
		"type":            "deck",
		"conversation_id": convID,
		"slides":          final,
		"global_css":      deck.GlobalCSS,
	})
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}
