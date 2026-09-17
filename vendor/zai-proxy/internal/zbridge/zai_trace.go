// zai_trace.go
//
// Request-scoped trace logging for debugging streaming/agent issues in
// production. The normal debug logging (LOG_LEVEL=debug) floods one line
// per SSE event and offers no lifecycle view; this trace answers the
// questions that matter when a turn goes wrong:
//
//   - how long did the request run, and how did it END (clean done, client
//     disconnect, upstream error, stall-kill, exhausted retries)?
//   - how many content/reasoning bytes were forwarded to the client vs
//     dropped (interceptor hold-back, stall-retry discard, tail drop)?
//   - which tool calls were emitted, at which index, with how many bytes
//     of arguments?
//
// Enable with TRACE=1 (or LOG_TRACE=1). Add LOG_FILE=<path> to tee all log
// output to a file as well, so logs survive a closed terminal and can be
// attached to bug reports.
//
// Trace lines are prefixed [trace req=<id>] so they can be grepped out of
// a noisy debug log: `grep '\[trace ' bridge.log`.

package zbridge

import (
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// traceEnabled reports whether request tracing is on (TRACE / LOG_TRACE env).
var traceEnabled = os.Getenv("TRACE") == "1" || os.Getenv("LOG_TRACE") == "1"

// traceMu serializes trace line writes.
var traceMu sync.Mutex

// reqTracer tracks the lifecycle of one client request (one HTTP handler
// invocation). It is safe for concurrent use: the SSE handler emits from
// the stream loop while the keep-alive goroutine also writes chunks.
// All methods are no-ops when tracing is off, so callers never need
// nil checks; a nil *reqTracer is also safe.
type reqTracer struct {
	id      string
	started time.Time

	mu sync.Mutex
	// Forwarded totals (bytes), for the END line.
	contentOut int
	reasonOut  int
	sseWrites  int // every SSE payload written (init/keep-alive/content/tool/DONE)
	// Tool calls emitted: index -> argument-fragment bytes (merged).
	toolArgs map[int]int
	toolHdrs int // header deltas seen
	// Dropped content, tagged by the dropping mechanism: bytes silently
	// NOT forwarded to the client.
	dropped map[string]int
	// exit path recorded at END; empty until then.
	exit  string
	extra map[string]string // misc facts reported at END (model, variant…)
}

func newReqTracer(id string) *reqTracer {
	return &reqTracer{
		id:       id,
		started:  time.Now(),
		toolArgs: map[int]int{},
		dropped:  map[string]int{},
		extra:    map[string]string{},
	}
}

// tr writes one trace line for this request.
func (t *reqTracer) tr(format string, args ...interface{}) {
	if t == nil || !traceEnabled {
		return
	}
	traceMu.Lock()
	defer traceMu.Unlock()
	log.Printf("[trace req=%s] "+format, append([]interface{}{t.id}, args...)...)
}

// trToolCall records one tool-call delta: header deltas count once (with
// the name and first-fragment size), argument fragments accumulate per
// index.
func (t *reqTracer) trToolCall(tc map[string]interface{}) {
	if t == nil || !traceEnabled {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	idx, _ := tc["index"].(int)
	fn, _ := tc["function"].(map[string]interface{})
	if fn == nil {
		return
	}
	if id, _ := tc["id"].(string); id != "" {
		t.toolHdrs++
		name, _ := fn["name"].(string)
		args, _ := fn["arguments"].(string)
		t.trLocked("tool-call header idx=%d name=%q args0=%d", idx, name, len(args))
	} else if frag, _ := fn["arguments"].(string); frag != "" {
		t.toolArgs[idx] += len(frag)
	}
}

// trSSE counts every payload written to the wire (init, keep-alive,
// content, tool calls, DONE) without logging each one — the count alone
// is enough to spot a silent/looping stream in the END line.
func (t *reqTracer) trSSE(data string) {
	if t == nil || !traceEnabled {
		return
	}
	t.mu.Lock()
	t.sseWrites++
	t.mu.Unlock()
}

// trContentBytes accumulates forwarded content/reasoning bytes.
func (t *reqTracer) trContentBytes(kind string, n int) {
	if t == nil || n == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch kind {
	case "content":
		t.contentOut += n
	case "reasoning":
		t.reasonOut += n
	}
}

// trDrop records content bytes that were NOT forwarded to the client,
// tagged by the dropping mechanism, and logs it immediately.
func (t *reqTracer) trDrop(reason string, n int) {
	if t == nil || n == 0 || !traceEnabled {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dropped[reason] += n
	t.trLocked("DROP %s: +%d (total %d)", reason, n, t.dropped[reason])
}

// trFact records a one-off fact for the END summary (model, variant…).
func (t *reqTracer) trFact(key, value string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.extra[key] = value
}

// trExit records the exit path and writes the END summary line. Call it
// exactly once, from the point the handler has fully answered the client
// (after [DONE]) or aborted (client disconnect). The first call wins.
func (t *reqTracer) trExit(path string) {
	if t == nil || !traceEnabled {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.exit != "" {
		return
	}
	t.exit = path

	var parts []string
	parts = append(parts, "dur="+time.Since(t.started).Round(time.Millisecond).String())
	if m := t.extra["model"]; m != "" {
		parts = append(parts, "model="+m)
	}
	if v := t.extra["variant"]; v != "" {
		parts = append(parts, "variant="+v)
	}
	parts = append(parts, fmt.Sprintf("hdrs=%d", t.toolHdrs))
	if len(t.toolArgs) > 0 {
		var idxs []string
		for _, i := range sortedIntKeys(t.toolArgs) {
			idxs = append(idxs, strconv.Itoa(i)+":"+strconv.Itoa(t.toolArgs[i]))
		}
		parts = append(parts, "toolArgs="+strings.Join(idxs, ","))
	}
	parts = append(parts, fmt.Sprintf("contentOut=%d", t.contentOut))
	parts = append(parts, fmt.Sprintf("reasonOut=%d", t.reasonOut))
	if len(t.dropped) > 0 {
		var ds []string
		for _, k := range sortedKeys(t.dropped) {
			ds = append(ds, k+":"+strconv.Itoa(t.dropped[k]))
		}
		parts = append(parts, "dropped="+strings.Join(ds, ","))
	}

	t.trLocked("END exit=%s %s", path, strings.Join(parts, " "))
}

// trLocked writes a trace line; caller holds t.mu (NOT traceMu — acquired
// here) — used internally so counters and their log lines stay ordered.
func (t *reqTracer) trLocked(format string, args ...interface{}) {
	if !traceEnabled {
		return
	}
	traceMu.Lock()
	defer traceMu.Unlock()
	log.Printf("[trace req=%s] "+format, append([]interface{}{t.id}, args...)...)
}

func sortedIntKeys(m map[int]int) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// setupLogFile tees the standard log output to LOG_FILE when set. Opened
// in append mode so restarts keep history. Called from init() so startup
// lines are captured too.
func setupLogFile() {
	path := os.Getenv("LOG_FILE")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("[trace] LOG_FILE %s unavailable: %v", path, err)
		return
	}
	log.SetOutput(io.MultiWriter(os.Stderr, f))
}

func init() {
	setupLogFile()
}
