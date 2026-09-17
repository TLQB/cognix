// Generic SSE-streaming helpers shared by the bridge regardless of upstream.
// Extracted verbatim from the Qwen transport (qwen.go in the parent project)
// holdback windows, emitters, and the stall-aware idle-deadline reader.
// The upstream-specific SSE parsers live in qwen.go.

package zbridge

import (
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// utf16IndexToByteIndex converts a UTF-16 code-unit offset — the indexing
// semantics of JavaScript strings, used by the official Qwen web frontend
// (`content.substring(0, edit_index)`, see the prod-fe bundle) — into a
// byte offset within s. It clamps at the end of s and never returns an
// offset inside a multi-byte rune: an offset landing between the two
// units of a surrogate pair is clamped to the start of that rune.
func utf16IndexToByteIndex(s string, utf16Idx int) int {
	if utf16Idx <= 0 {
		return 0
	}
	byteIdx, units := 0, 0
	for byteIdx < len(s) {
		if units == utf16Idx {
			return byteIdx
		}
		r, size := utf8.DecodeRuneInString(s[byteIdx:])
		ru := utf16.RuneLen(r)
		if ru < 0 {
			ru = 1 // invalid byte: the JS frontend also sees one unit here
		}
		if units+ru > utf16Idx {
			return byteIdx // inside a surrogate pair — clamp to rune start
		}
		units += ru
		byteIdx += size
	}
	return len(s)
}

// commonPrefixLen returns the byte length of the longest common prefix of
// a and b. The result is always on a rune boundary, so slicing either
// string at that offset cannot produce invalid UTF-8.
func commonPrefixLen(a, b string) int {
	i := 0
	for i < len(a) && i < len(b) {
		ra, sa := utf8.DecodeRuneInString(a[i:])
		rb, _ := utf8.DecodeRuneInString(b[i:])
		if ra != rb {
			break
		}
		i += sa
	}
	return i
}

// holdBackTail trims up to n runes from the end of s (rune-safe).
func holdBackTail(s string, n int) string {
	if n <= 0 || s == "" {
		return s
	}
	i, count := len(s), 0
	for i > 0 && count < n {
		_, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
		count++
	}
	return s[:i]
}

// hasToolCallMarkerSuffix reports whether s ends with a tool-call
// marker or partial marker. It checks if the string contains the
// keyword TOOL_CALL or END_TOOL_CALL (or a prefix of it) preceded by
// at least one '<' char (making it a valid marker, not just bare text).
// Case-insensitive to match the tolerant findAgentMarker behavior.
func hasToolCallMarkerSuffix(s string) bool {
	if len(s) < 4 {
		return false
	}
	sLower := strings.ToLower(s)
	for _, m := range []string{"end_tool_call", "tool_call"} {
		// Full keyword preceded by '<'
		if idx := strings.Index(sLower, m); idx > 0 && s[idx-1] == '<' {
			return true
		}
		// Partial keyword at end preceded by '<' (streaming in progress)
		for plen := len(m) - 1; plen >= 3; plen-- {
			if strings.HasSuffix(sLower, m[:plen]) {
				idx := strings.Index(sLower, m[:plen])
				if idx > 0 && s[idx-1] == '<' {
					return true
				}
			}
		}
	}
	return false
}

// holdBackTailSafe trims up to n runes from the end of s (rune-safe)
// but never into a partial or complete <<<TOOL_CALL>>> /
// <<<END_TOOL_CALL>>> marker so the agent interceptor can always
// detect the start/end fence.
func holdBackTailSafe(s string, n int) string {
	if n <= 0 || s == "" {
		return s
	}
	if hasToolCallMarkerSuffix(s) {
		return s // don't trim at all into marker zone
	}
	return holdBackTail(s, n)
}

// holdBackPartialDetailsTag trims a trailing fragment that could still be
// the beginning of a <details> tag whose completion has not arrived yet,
// so a tag streamed character by character never leaks to the client.
// A COMPLETE "</details>" literal is kept (legitimate text); a complete
// "<details" is held (waiting for its ">" to decide whether it is a tag).
func holdBackPartialDetailsTag(s string) string {
	i := strings.LastIndex(s, "<")
	if i < 0 {
		return s
	}
	suffix := s[i:]
	if len(suffix) <= len("<details") && strings.HasPrefix("<details", suffix) {
		return s[:i]
	}
	if len(suffix) < len("</details>") && strings.HasPrefix("</details>", suffix) {
		return s[:i]
	}
	return s
}

// holdBackPartialQuoteMarker trims a trailing ">" that forms the entire
// last line of s. Lines inside a <details> reasoning body are markdown-
// quoted ("> ..."), and while the body streams in one character at a
// time a new line's quote marker is transiently present as a bare ">"
// — which stripDetailsTags cannot strip yet (TrimPrefix needs the
// space) but strips one character later when the "> " completes.
// Forwarding that transient ">" makes the stripped-reasoning snapshot
// sequence non-monotonic, diverging the reasoning emitter: every later
// snapshot re-emits everything after the stale ">" — the growing-prefix
// reasoning_content duplication seen by clients. Holding the marker
// back keeps the sequence monotonic; the final flush releases it if
// the text really ends there.
func holdBackPartialQuoteMarker(s string) string {
	if !strings.HasSuffix(s, ">") {
		return s
	}
	body := s[:len(s)-1]
	if body == "" || strings.HasSuffix(body, "\n") {
		return body
	}
	return s
}

// sseEmitter forwards snapshots of a growing (and occasionally rewritten)
// text to an append-only consumer as rune-safe deltas. It tracks exactly
// what the consumer has received so far and never emits a slice that
// starts inside a multi-byte rune, so the consumer can never receive
// invalid UTF-8 (which its JSON renderer would show as U+FFFD
// replacement garble — the symptom reported in issue #23).
type sseEmitter struct {
	clientView string // exactly what the consumer has received so far
}

// empty reports whether the emitter has ever released anything to the
// consumer. Used by the captcha-gate retry: a retry is only safe when
// nothing has been forwarded yet.
func (e *sseEmitter) empty() bool {
	return e.clientView == ""
}

// delta returns the text to append to the consumer so it converges on
// target as closely as possible, and updates the tracked view:
//   - target extends the view   -> the new suffix (normal growth)
//   - target is a prefix of the view (a deep edit truncated the text)
//     -> "" — nothing can be taken back; the view is kept as-is so the
//     following growth is not re-sent from a rewound base
//   - target rewrote part of the view -> everything after the longest
//     common prefix; the stale fragment in between stays on the consumer
//     (unavoidable for append-only SSE, but it remains valid UTF-8).
//     The tracked state then re-syncs to target so later growth emits
//     only the genuinely new suffix; keeping the stale fragment in the
//     tracked view would make every later snapshot diverge at the same
//     point and re-emit everything after it on every call (cascading
//     growing-prefix duplication).
func (e *sseEmitter) delta(target string) string {
	if target == e.clientView {
		return ""
	}
	if strings.HasPrefix(target, e.clientView) {
		delta := target[len(e.clientView):]
		e.clientView = target
		return delta
	}
	cp := commonPrefixLen(e.clientView, target)
	if cp == len(target) {
		return "" // consumer already has everything target contains
	}
	delta := target[cp:]
	e.clientView = target
	return delta
}

// splitDetails extracts every complete <details ...>...</details> block
// from raw: the block bodies (concatenated) become reasoning, everything
// else becomes content. A trailing opener whose '>' has not arrived yet is
// held pending (neither reasoning nor content) until more data arrives.
func splitDetails(raw string) (reasoning, content string) {
	var rb, cb strings.Builder
	rest := raw
	for {
		idx := strings.Index(rest, "<details")
		if idx < 0 {
			cb.WriteString(rest)
			break
		}
		cb.WriteString(rest[:idx])
		tagEnd := strings.Index(rest[idx:], ">")
		if tagEnd < 0 {
			break // incomplete opener at the tail — hold pending
		}
		afterTag := rest[idx+tagEnd+1:]
		closeIdx := strings.Index(afterTag, "</details>")
		if closeIdx < 0 {
			rb.WriteString(afterTag) // reasoning still streaming
			break
		}
		rb.WriteString(afterTag[:closeIdx])
		rest = afterTag[closeIdx+len("</details>"):]
	}
	return rb.String(), cb.String()
}

// extendingReader is the idle-aware stream guard: its timeout measures time
// WITHOUT DATA, not time since stream start. Every successful Read pushes the deadline forward by the
// idle window, so an actively streaming generation (a large code file
// trickling SSE chunks for many minutes) runs to completion, while a silent
// upstream is cut exactly one idle-window after its last byte.
type extendingReader struct {
	r       io.Reader
	closer  io.Closer
	idleWin time.Duration
	timer   *time.Timer
	// done releases the watchdog goroutine on terminal EOF/error and on
	// Close — timer.Stop() sends nothing on the channel, so without it the
	// watchdog would block on <-er.timer.C forever after a clean stream end.
	done     chan struct{}
	closeOne sync.Once
	// deadlineFired latches once the idle window elapses with no data. A
	// closed http body makes blocked Reads return EOF (not an error), which
	// would look like a CLEAN stream end to the scanner — so the flag turns
	// every subsequent Read into context.DeadlineExceeded and the retry path
	// (tracer exit=upstream-stall-kill) stays reachable.
	deadlineFired atomic.Bool
}

func newExtendingReader(r io.Reader, closer io.Closer, idle time.Duration) *extendingReader {
	er := &extendingReader{
		r:       r,
		closer:  closer,
		idleWin: idle,
		timer:   time.NewTimer(idle),
		done:    make(chan struct{}),
	}
	// Watchdog: a timer alone cannot interrupt a Read that is ALREADY
	// blocked on the body — only closing the body can (http semantics: a
	// blocked Read returns once the body is closed). The watchdog waits
	// out the idle window, latches the deadline, and closes the body, which
	// unblocks the in-flight Read; the latch makes it return as a stall
	// error instead of a clean EOF.
	go func() {
		select {
		case <-er.timer.C:
			if !er.deadlineFired.Load() {
				er.deadlineFired.Store(true)
				if er.closer != nil {
					er.closer.Close()
				}
			}
		case <-er.done:
		}
	}()
	return er
}

func (er *extendingReader) Read(p []byte) (int, error) {
	// Latched deadline (idle window elapsed in an earlier Read or while
	// this one is blocked): the read returns as a stall error, never a
	// clean EOF — the retry path keys off context.DeadlineExceeded.
	if er.deadlineFired.Load() {
		return 0, context.DeadlineExceeded
	}
	n, err := er.r.Read(p)
	if er.deadlineFired.Load() {
		return 0, context.DeadlineExceeded
	}
	if n > 0 {
		// Data arrived — renew the idle window. Canonical Stop/drain/Reset:
		// if the watchdog fired WHILE this Read was blocked (data raced the
		// deadline), the race check above has already decided this read is a
		// stall; only a still-live timer gets reset here.
		if !er.timer.Stop() {
			select {
			case <-er.timer.C:
			default:
			}
		}
		er.timer.Reset(er.idleWin)
		return n, nil
	}
	if err != nil {
		// Terminal (EOF or transport error): disarm the timer and stop the
		// watchdog from firing on a pooled reader after stream end.
		er.stop()
	}
	return n, err
}

// stop disarms the timer and releases the watchdog goroutine exactly once.
func (er *extendingReader) stop() {
	er.closeOne.Do(func() {
		er.timer.Stop()
		close(er.done)
	})
}

// Close stops the timer and closes the underlying closer. streamSSEResponse's
// callers always resp.Body.Close() directly, so this mostly matters for
// correctness if anyone routes the reader through an io.Closer path.
func (er *extendingReader) Close() error {
	er.stop()
	if er.closer != nil {
		return er.closer.Close()
	}
	return nil
}
