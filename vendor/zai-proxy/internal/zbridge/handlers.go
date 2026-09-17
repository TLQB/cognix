// Code moved from the original main.go monolith during the internal/ restructure.
// See README "Project Structure". Part of the Z.AI bridge core (package zbridge).

package zbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// stripToolCallFromText removes tool-call XML markers from text so they
// don't leak as raw content in the Thinking/reasoning block. Handles both
// the canonical <<<TOOL_CALL>>>...<<<END_TOOL_CALL>>> format and the XML-style
// <tool_call>...</tool_call> variant (case-insensitive).
func stripToolCallFromText(text string) string {
	result := text
	// Strip <<<TOOL_CALL>>>...<<<END_TOOL_CALL>>> blocks (tolerant brackets)
	result = agentStripToolCalls(result)
	// Strip <tool_call>...</tool_call> blocks (case-insensitive)
	for {
		lower := strings.ToLower(result)
		start := strings.Index(lower, "<tool_call>")
		if start < 0 {
			break
		}
		endTag := strings.Index(lower[start:], "</tool_call>")
		if endTag < 0 {
			break
		}
		result = result[:start] + result[start+endTag+len("</tool_call>"):]
	}
	return strings.TrimSpace(result)
}

func chatCompletionsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Model           string          `json:"model"`
		Messages        json.RawMessage `json:"messages"`
		Stream          *bool           `json:"stream"`
		Reasoning       *bool           `json:"reasoning"`
		Thinking        json.RawMessage `json:"thinking"`
		WebSearch       *bool           `json:"webSearch"`
		Search          *bool           `json:"search"`
		Tools           json.RawMessage `json:"tools"`
		ToolChoice      json.RawMessage `json:"tool_choice"`
		ReasoningEffort string          `json:"reasoning_effort"`
	}
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, 400, formatOpenAIError("Failed to read body", "invalid_request_error", nil))
		return
	}
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		writeJSON(w, 400, formatOpenAIError("Invalid JSON", "invalid_request_error", nil))
		return
	}

	model := body.Model
	if model == "" {
		model = "glm-5"
	}

	// Qwen gateway: qwen3* models are forwarded to the bundled qwen-proxy
	// (QWEN_PROXY_URL) before any Z.AI-specific work happens — no session
	// mint, no agent transform. See qwen_gateway.go.
	if handled, _ := forwardToQwenGateway(w, r, bodyBytes); handled {
		return
	}

	var messages []Message
	if err := json.Unmarshal(body.Messages, &messages); err != nil || len(messages) == 0 {
		writeJSON(w, 400, formatOpenAIError("messages is required and must be an array", "invalid_request_error", nil))
		return
	}

	// Captured from the ORIGINAL (pre-agent-transform) messages: the agent
	// shim folds the conversation into a single user prompt, wiping the
	// roles, but the stall guard needs to know whether this request continues
	// a tool loop (last message = tool RESULT) — see
	// agentToolResultBlankTurn in agent.go.
	lastMsgIsToolResult := false
	if n := len(messages); n > 0 && strings.EqualFold(messages[n-1].Role, "tool") {
		lastMsgIsToolResult = true
	}

	// A chat request means the captcha cache will be drawn from soon —
	// resume background generation now instead of letting the first cache
	// miss pay the ~0.5s synchronous mint (idle pause: 3min, see captcha.go).
	if config.AgentMode {
		captchaCache.Wake()
	}

	stream := true
	if body.Stream != nil {
		stream = *body.Stream
	}

	// Stateless request: run it on a THROWAWAY chat session (ported from the
	// DeepseekFreeAPI reference). Async mode takes a pre-made session from
	// the standing pool batch; sync mode mints one on the spot. Either way
	// the chat is deleted on Z.AI once the response is fully processed
	// (deferred below), so no server-side history survives the request —
	// this is what keeps the account from accumulating dead sessions and
	// stops stale server-side context from rotting later requests.
	chatID, pooled, err := AcquireStatelessSession(r.Context())
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return // client gone; nothing to answer
		}
		writeJSON(w, 503, formatOpenAIError(err.Error(), "server_error", "shutting_down"))
		return
	}
	defer ReleaseStatelessSession(chatID, pooled)
	requestId := generateID()
	tracer := newReqTracer(requestId)
	tracer.trFact("model", model)
	tracer.tr("START /v1/chat/completions stream")

	// ── Agent mode: transform tools & roles for Z.AI compatibility ──
	// Modern shim (default): one XML-sectioned prompt in a single user message.
	// Legacy shim: [ROLE: ...] rewritten user messages + tool contract message.
	var transformedMessages json.RawMessage = body.Messages
	if config.AgentMode {
		if tm, err := agentTransformMessages(body.Messages, body.Tools); err == nil {
			transformedMessages = tm
			// Re-parse so local `messages` reflects the rewritten content
			var localMsgs []Message
			if err := json.Unmarshal(tm, &localMsgs); err == nil {
				messages = localMsgs
			}
		} else {
			logError("agent transform failed: " + err.Error())
		}
	}

	prompt := messagesToPrompt(messages)

	// Features are now resolved per-model inside sendToZAI.
	// Per-request overrides are only set if explicitly provided in the body.
	opts := SendOptions{
		Model:             model,
		ChatID:            chatID,
		ClientMessagesRaw: transformedMessages,
		ReasoningEffort:   body.ReasoningEffort,
		RequestID:         requestId,
	}

	// Parse thinking configuration:
	//   reasoning: true/false  ->  enable_thinking
	//   "thinking": {"type":"enabled"|"disabled"}  ->  enable_thinking
	if body.Reasoning != nil {
		opts.Thinking = body.Reasoning
	} else if len(body.Thinking) > 0 {
		var thinkCfg struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(body.Thinking, &thinkCfg); err == nil {
			enabled := thinkCfg.Type == "enabled"
			opts.Thinking = &enabled
		}
	}

	// Latency: throttle thinking on mid-loop agent turns (last message =
	// tool result) — GLM 5.3 burns 5k–16k reasoning chars on mechanical
	// decisions (docs/stall_evident.md). TOOL_LOOP_THINKING=off default.
	// lastMsgIsToolResult was captured from the ORIGINAL messages above —
	// NOT the agent-transformed ones (the shim folds roles into one user
	// message, masking the tool-result tail).
	config.throttleThinkingForTurn(&opts, lastMsgIsToolResult)

	if body.WebSearch != nil {
		opts.WebSearch = body.WebSearch
	} else if body.Search != nil {
		opts.WebSearch = body.Search
	}

	// Declared tool names — used by agentSnapToolName to repair glued/corrupted
	// tool names ("read_filearguments" → "read_file") before they reach the
	// client and fail with "No tool named X exists" (log_stall.md 2026-09-06).
	knownTools := agentCollectToolNames(body.Tools)
	if len(knownTools) > 0 && config.Logging.Level == "debug" {
		log.Printf("[DEBUG] known tools: %v", knownTools)
	}

	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		flusher, _ := w.(http.Flusher)
		var writeMu sync.Mutex

		writeSSE := func(data string) {
			writeMu.Lock()
			defer writeMu.Unlock()
			fmt.Fprintf(w, "data: %s\n\n", data)
			if flusher != nil {
				flusher.Flush()
			}
			tracer.trSSE(data)
		}

		initChunk := formatOpenAIResponse(ResponseResult{Content: ""}, model, requestId, true)
		writeSSE(toJSON(initChunk))

		var fullContent, fullReasoning strings.Builder
		// fullContentStr is a cheap snapshot kept in sync for prefix checks
		// that would otherwise require an O(n) Builder.String() per chunk.
		fullContentStr := ""

		var interceptor agentInterceptor
		if config.AgentMode {
			interceptor = newAgentInterceptor(body.Tools)
		}
		toolCallEmitted := false

		// Stall-retry buffering: SSE cannot un-send, so a stall retry that
		// already streamed its text would duplicate it on the client (E2E
		// log: one greeting turn rendered three concatenated greetings after
		// two retries). Only CONTENT needs this protection: reasoning is
		// attempt-scoped and honest per attempt (each retry shows its own
		// thinking), and holding it back kills perceived TTFT — the client
		// sees silence then a burst of thinking (Cognix UX report).
		// So: reasoning streams straight through, while content is held ONLY
		// while it could still trip the stall detector (contentNeedsStallHold:
		// an intent phrase is present, or the text is short enough for one to
		// complete at its tail). Phrase-free content past the guard can never
		// re-become an announcement on its existing bytes, so it is committed
		// to live streaming — and the first live content byte LOCKS the stall
		// retry out for this request, because a retry would re-emit text the
		// client already has (future text may still grow a phrase; SSE
		// cannot un-send either way).
		var pending []string
		pendingBytes := 0
		// Memory bound for the hold path: content is only held while the
		// detector could still fire, which caps it at announcementMaxLen
		// content bytes (~3000). Worst-case chunking (1-byte deltas, ~250B
		// of SSE/JSON overhead each) yields well under 1MB of pending
		// payloads; the 2MB cap below is a pure safety valve. The old 8KB
		// cap counted PAYLOAD bytes, so ~30 small chunks overflowed it,
		// flushed live WITHOUT locking the retry, and a later stall retry
		// re-emitted everything (pre-existing duplicate bug, now fixed by
		// the cap + contentLiveCommitted lock together).
		const stallBufferCap = 256 * 1024 // 256KB: pathological chunking cap, well under the 2MB that caused duplicate-leak stalls
		buffering := config.AgentMode && config.StallMaxRetries > 0
		curStallAttempt := 0 // read by stallPossible, set at each retry attempt
		contentLiveCommitted := false
		stallPossible := func() bool {
			if toolCallEmitted || curStallAttempt >= config.StallMaxRetries {
				return false
			}
			// Content already streamed live makes any stall retry impossible:
			// it would re-emit text the client already has.
			if contentLiveCommitted {
				return false
			}
			// Mirror looksLikeAnnouncementOnly: the detector examines CONTENT
			// when the model produced any, and only falls back to reasoning
			// for empty-content stalls. Using the same target keeps the gate
			// exactly as wide as the detector — never wider (duplicate leak),
			// never narrower (needless buffering).
			target := len(fullContentStr)
			if target == 0 {
				target = fullReasoning.Len()
			}
			if target > announcementMaxLen {
				return false
			}
			// Marker vocabulary in the content must stay HELD (never released
			// live): stall_evident.md 2026-09-07 — GLM 5.3 half-echoes the
			// prompt framing ("<<<TOOL_CALL ..." transcripts, dangling
			// <tool_call> pseudo-XML) that parses into nothing; if such text
			// streams live, the SSE lockout kills the stall retry and the
			// leak lands on the client verbatim. Held text can still be
			// discarded and retried when the turn stalls (the emptyNameBlock
			// drop feeds exactly this path).
			if contentNeedsStallHold(fullContentStr) {
				return true
			}
			lower := strings.ToLower(fullContentStr)
			return strings.Contains(lower, "tool_call") ||
				strings.Contains(lower, "function_call") ||
				strings.Contains(lower, "<function>") ||
				strings.Contains(lower, "<assistant>")
		}

		flushPending := func() {
			buffering = false
			if len(pending) == 0 {
				return
			}
			out := pending
			pending = nil
			pendingBytes = 0
			for _, p := range out {
				writeSSE(p)
			}
		}

		// emitOrBuffer routes a CONTENT SSE payload through the stall buffer
		// (or writes it directly once buffering is off). Reasoning bypasses
		// this entirely — see writeReasoning below.
		emitOrBuffer := func(payload string) {
			if buffering && stallPossible() {
				if contentNeedsStallHold(fullContentStr) && pendingBytes+len(payload) <= stallBufferCap {
					pending = append(pending, payload)
					pendingBytes += len(payload)
					return
				}
				// Releasing content live while a stall retry was still possible:
				// either the text can no longer trip the detector on its existing
				// bytes (phrase-free past the guard — the TTFT release), or the
				// safety-valve cap overflowed (pathological chunking). Either way
				// the retry must be locked out from this point: a retry would
				// re-emit text the client already has, and SSE cannot un-send.
				contentLiveCommitted = true
			}
			flushPending()
			writeSSE(payload)
		}

		// writeReasoning streams a reasoning delta directly to the client —
		// never buffered: thinking must appear live for perceived TTFT, and
		// duplicate thinking across stall retries is honest per-attempt
		// output (the E2E greeting bug was content-only).
		writeReasoning := func(payload string) {
			if payload == "" {
				return
			}
			writeSSE(payload)
		}

		emitToolCallDelta := func(tc map[string]interface{}) {
			// Repair glued tool names against the request's real tool list before
			// the delta reaches the client — once the client rejects a call with
			// "No tool named X exists" the turn is already lost.
			if fn, ok := tc["function"].(map[string]interface{}); ok {
				if name, _ := fn["name"].(string); name != "" {
					if snapped := agentSnapToolName(name, knownTools); snapped != name {
						log.Printf("[Agent] snapped tool name %q -> %q", name, snapped)
						fn["name"] = snapped
					}
				}
			}
			// Content must reach the client before any tool call, so drain
			// the stall buffer first (also disables further buffering — a
			// turn with a tool call is never retried).
			flushPending()
			chunk := map[string]interface{}{
				"id":      "chatcmpl-" + requestId,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   model,
				"choices": []map[string]interface{}{
					{
						"index":         0,
						"delta":         map[string]interface{}{"role": "assistant", "tool_calls": []map[string]interface{}{tc}},
						"finish_reason": nil,
					},
				},
			}
			tracer.trToolCall(tc)
			writeSSE(toJSON(chunk))
		}

		// salvageToolCallsFrom is the last-resort extraction for ERRORED
		// streams: if a tool-call block completed before the stream died,
		// the normal end-of-stream safety net (inside !errored) never runs
		// and the block would leak as raw text with no execution (E2E
		// log). Only fires when nothing was emitted yet.
		salvageToolCallsFrom := func(content, reasoning string) {
			if toolCallEmitted {
				return
			}
			for _, src := range []string{content, reasoning} {
				if src == "" {
					continue
				}
				calls := agentExtractToolCalls(src, body.Tools)
				if len(calls) > 0 {
					log.Printf("[Agent] Salvaged %d tool call(s) from errored stream", len(calls))
					for _, tc := range calls {
						emitToolCallDelta(tc)
					}
					toolCallEmitted = true
					return
				}
			}
		}

		keepAliveStop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					ka := formatOpenAIResponse(ResponseResult{Content: ""}, model, requestId, true)
					writeSSE(toJSON(ka))
				case <-keepAliveStop:
					return
				}
			}
		}()

		// Stall retry: attempt 0 streams normally. If the model only announced
		// intent without emitting any tool call, attempt 1 re-sends the prompt
		// with a nudge appended (agentStallNudge) before the stream ends.
		for stallAttempt := 0; ; stallAttempt++ {
			errored := false
			curStallAttempt = stallAttempt
			ch, err := sendToZAI(prompt, opts)
			if err != nil {
				log.Printf("[Stream] Error: %s", err.Error())
				tracer.trExit("upstream-error: " + err.Error())
				// Release whatever this attempt buffered before the error
				// (client-disconnect is the only case where writing is
				// pointless) — the streamed content was real, and the safety
				// net below may still salvage a tool call from it.
				if r.Context().Err() == nil {
					flushPending()
					salvageToolCallsFrom(fullContent.String(), fullReasoning.String())
				}
				writeSSE(toJSON(formatOpenAIError(err.Error(), "api_error", statusFromError(err.Error()))))
				writeSSE("[DONE]")
				errored = true
			} else {
				ctxDone := r.Context().Done()
			streamLoop:
				for result := range ch {
					select {
					case <-ctxDone:
						// client gone — stop streaming, drain channel, exit early
						log.Printf("[Stream] client disconnected; aborting stream (requestId=%s)", requestId)
						tracer.trExit("client-disconnect")
						errored = true
						go func() {
							for range ch {
							}
						}()
						break streamLoop
					default:
					}
					if result.Err != nil {
						log.Printf("[Stream] Error: %s", result.Err.Error())
						// stallingReader kills a silent upstream after StallTimeout;
						// surface that distinctly from other stream errors so logs
						// taken from production tell the two apart at a glance.
						if errors.Is(result.Err, context.DeadlineExceeded) {
							tracer.trExit("upstream-stall-kill")
						} else {
							tracer.trExit("stream-error: " + result.Err.Error())
						}
						// Flush the buffered real content, then salvage any
						// complete tool call from the accumulated text — the
						// old code skipped both (!errored gated) and a block
						// that completed before the error leaked as raw text
						// with no execution (E2E log).
						flushPending()
						salvageToolCallsFrom(fullContent.String(), fullReasoning.String())
						writeSSE(toJSON(formatOpenAIError(result.Err.Error(), "api_error", statusFromError(result.Err.Error()))))
						writeSSE("[DONE]")
						errored = true
						break
					}

					if result.Reasoning != "" {
						fullReasoning.WriteString(result.Reasoning)
						// Strip tool-call XML from reasoning so it doesn't leak as raw
						// text in the Thinking block. Tool calls inside <details> are
						// handled by the safety net below.
						strippedReasoning := stripToolCallFromText(result.Reasoning)
						if strippedReasoning != "" {
							tracer.trContentBytes("reasoning", len(strippedReasoning))
							rChunk := map[string]interface{}{
								"id":      "chatcmpl-" + requestId,
								"object":  "chat.completion.chunk",
								"created": time.Now().Unix(),
								"model":   model,
								"choices": []map[string]interface{}{
									{
										"index":         0,
										"delta":         map[string]interface{}{"reasoning_content": strippedReasoning},
										"finish_reason": nil,
									},
								},
							}
							writeReasoning(toJSON(rChunk))
						}
						continue
					}
					replayed := false
					if result.FullText != "" && !strings.HasPrefix(result.FullText, fullContentStr) {
						// A deep edit_content rewrite rewound text that was already
						// forwarded. The agent interceptor's buffer is stale, but a
						// bare reset loses the client-visible call-index sequence: a
						// call streamed before the rewind keeps its index and the
						// re-emitted block takes the next one (SDK accumulation
						// collision — issue #23).
						if interceptor != nil {
							interceptor = rearmAgentInterceptor(interceptor)
							tracer.tr("deep edit_content rewind: interceptor rearmed, replaying snapshot (len=%d)", len(result.FullText))
							_, replayCalls := interceptor.feed(result.FullText)
							for _, tc := range replayCalls {
								emitToolCallDelta(tc)
								toolCallEmitted = true
							}
						}
						replayed = true
					}
					if result.FullText != "" {
						fullContent.Reset()
						fullContent.WriteString(result.FullText)
						fullContentStr = result.FullText
					} else {
						fullContent.WriteString(result.Chunk)
						fullContentStr += result.Chunk
					}
					if replayed {
						// The delta re-syncs the client text; the interceptor was
						// already primed with the full snapshot above. Re-feeding the
						// delta would append it a second time inside the buffer.
						if delta := result.Chunk; delta != "" {
							if config.Logging.Level == "debug" {
								log.Printf("[DEBUG] post-deep-edit delta re-sync=%q (len=%d)", delta, len(delta))
							}
							tracer.trContentBytes("content", len(delta))
							c := formatOpenAIResponse(ResponseResult{Content: delta}, model, requestId, true)
							emitOrBuffer(toJSON(c))
						}
						continue
					}

					// The parser emits the exact rune-safe delta to forward.
					delta := result.Chunk
					if delta == "" {
						continue
					}

					if interceptor != nil {
						if config.Logging.Level == "debug" {
							log.Printf("[DEBUG] interceptor.feed delta=%q (len=%d)", delta, len(delta))
						}
						contentDelta, toolCalls := interceptor.feed(delta)
						if config.Logging.Level == "debug" {
							log.Printf("[DEBUG] interceptor returned contentDelta=%q toolCalls=%d", contentDelta, len(toolCalls))
						}
						if contentDelta != "" {
							tracer.trContentBytes("content", len(contentDelta))
							c := formatOpenAIResponse(ResponseResult{Content: contentDelta}, model, requestId, true)
							emitOrBuffer(toJSON(c))
						}
						for _, tc := range toolCalls {
							emitToolCallDelta(tc)
							toolCallEmitted = true
						}
					} else {
						tracer.trContentBytes("content", len(delta))
						c := formatOpenAIResponse(ResponseResult{Content: delta}, model, requestId, true)
						emitOrBuffer(toJSON(c))
					}
				}
			}

			if !errored {
				if interceptor != nil {
					// Drain the interceptor tail: trailing text plus any
					// tool call whose block only completed at end of stream
					// (the modern shim holds back a window while streaming).
					rem, tailCalls := interceptor.finish()
					if config.Logging.Level == "debug" {
						log.Printf("[DEBUG] interceptor.finish rem=%q tailCalls=%d", rem, len(tailCalls))
					}
					if rem != "" {
						if toolCallEmitted {
							// Tail text after the tool-call block: dropping it is
							// correct (the block is what the client acts on), but the
							// END summary must account for the bytes.
							tracer.trDrop("interceptor-tail-after-tool-call", len(rem))
						} else {
							tracer.trContentBytes("content", len(rem))
							c := formatOpenAIResponse(ResponseResult{Content: rem}, model, requestId, true)
							emitOrBuffer(toJSON(c))
						}
					}
					for _, tc := range tailCalls {
						emitToolCallDelta(tc)
						toolCallEmitted = true
					}

					// Safety net: fallback tool call extraction at stream end.
					// Check both fullContent and fullReasoning because some models
					// (e.g. GLM 5.2/5.3) emit tool calls inside <details> blocks
					// that get classified as reasoning by splitDetails.
					if !toolCallEmitted {
						if config.Logging.Level == "debug" {
							log.Printf("[DEBUG] safety net: fullContent=%q", fullContent.String())
						}
						fallbackCalls := agentExtractToolCalls(fullContent.String(), body.Tools)
						if len(fallbackCalls) > 0 {
							log.Printf("[Agent] safety net: %d tool call(s) extracted from full content at stream end", len(fallbackCalls))
							tracer.tr("safety net: %d tool call(s) from full content", len(fallbackCalls))
							for _, tc := range fallbackCalls {
								emitToolCallDelta(tc)
							}
							toolCallEmitted = true
						}
					}
					if !toolCallEmitted && fullReasoning.Len() > 0 {
						fallbackCalls := agentExtractToolCalls(fullReasoning.String(), body.Tools)
						if len(fallbackCalls) > 0 {
							log.Printf("[Agent] Found %d tool calls in reasoning content (GLM-style)", len(fallbackCalls))
							tracer.tr("safety net: %d tool call(s) from reasoning (GLM-style)", len(fallbackCalls))
							for _, tc := range fallbackCalls {
								emitToolCallDelta(tc)
							}
							toolCallEmitted = true
						}
					}

					// Stall guard: the model announced intent ("I'll ...", "Let me ...")
					// but emitted no tool call block — or, when this request
					// continues a tool loop (last message is a tool result),
					// ended with empty/junk content and no tool call (the
					// GLM 5.3 done-stop contentOut≈1 shape; agentToolResultBlankTurn)
					// — retry with a nudge appended to the prompt instead of
					// ending the turn. StallMaxRetries controls how many times we
					// retry (default 2). The buffered deltas of this attempt are
					// DISCARDED — SSE cannot un-send them, so keeping (flushing)
					// them would duplicate the announcement on the client for
					// every retry (E2E log: three concatenated greetings from one
					// user turn).
					contentStr := fullContent.String()
					reasoningStr := fullReasoning.String()
					stalled := looksLikeAnnouncementOnly(contentStr, reasoningStr)
					if !stalled && lastMsgIsToolResult {
						stalled = agentToolResultBlankTurn(contentStr, reasoningStr)
					}
					// A retry discards the attempt's streamed text (SSE cannot
					// un-send), so content that already went out live locks the
					// retry out — EXCEPT a post-tool-result blank whose only
					// committed bytes are tiny junk (0-8 stray chars released by
					// the buffering safety valve when reasoning ran past the
					// hold gate). Re-emitting that is cosmetically harmless,
					// while refusing the retry ends the turn blank and stalls
					// the tool loop (the E2E duplicate-greeting bug was full
					// meaningful text and stays locked).
					contentHeld := !contentLiveCommitted
					if !contentHeld && lastMsgIsToolResult && len(contentStr) <= 8 &&
						agentToolResultBlankTurn(contentStr, reasoningStr) {
						contentHeld = true
					}
					if config.Logging.Level == "debug" {
						log.Printf("[Stream] stall-guard decision (requestId=%s): stalled=%v contentHeld=%v liveCommitted=%v toolCallEmitted=%v attempt=%d/%d lastMsgIsToolResult=%v contentLen=%d reasonLen=%d",
							requestId, stalled, contentHeld, contentLiveCommitted, toolCallEmitted, stallAttempt, config.StallMaxRetries, lastMsgIsToolResult, len(contentStr), len(reasoningStr))
					}
					if stallAttempt < config.StallMaxRetries && !toolCallEmitted && contentHeld && stalled {
						log.Printf("[Stream] stall detected (attempt %d/%d, postToolResult=%v): no tool call and no answer; retrying with nudge (requestId=%s)", stallAttempt+1, config.StallMaxRetries, lastMsgIsToolResult, requestId)
						tracer.trDrop("stall-retry-discard", pendingBytes)
						tracer.tr("stall detected: attempt %d/%d (postToolResult=%v), discarding buffered content, retrying with nudge", stallAttempt+1, config.StallMaxRetries, lastMsgIsToolResult)
						// The nudge must reach the model. sendToZAIStream builds the
						// upstream body from opts.ClientMessagesRaw (the agent-folded
						// messages) — the `prompt` variable only feeds
						// signature_prompt, which Z.AI signs but does not show the
						// model. Appending to `prompt` alone (the old code) sent a
						// byte-identical retry the model dutifully answered the
						// same way: every stall retry burned its attempt on nothing
						// (production 2026-09-09: attempt 1/2 then 2/2 failing on
						// the same requestId 7-8s apart, both turns ending blank).
						nudge := agentStallNudge()
						prompt += nudge
						opts.ClientMessagesRaw = appendNudgeToClientMessages(opts.ClientMessagesRaw, nudge)
						pending = nil
						pendingBytes = 0
						buffering = config.AgentMode && config.StallMaxRetries > 0
						fullContent.Reset()
						fullReasoning.Reset()
						fullContentStr = ""
						contentLiveCommitted = false
						interceptor = newAgentInterceptor(body.Tools)
						continue
					}

					// Final attempt (or no stall): release everything that was
					// buffered, then close the stream.
					flushPending()

					if toolCallEmitted {
						finalChunk := map[string]interface{}{
							"id":      "chatcmpl-" + requestId,
							"object":  "chat.completion.chunk",
							"created": time.Now().Unix(),
							"model":   model,
							"choices": []map[string]interface{}{
								{
									"index":         0,
									"delta":         map[string]interface{}{},
									"finish_reason": "tool_calls",
								},
							},
						}
						writeSSE(toJSON(finalChunk))
						tracer.trExit("done-tool_calls")
					} else {
						finalChunk := formatOpenAIResponse(ResponseResult{Content: "", FinishReason: "stop"}, model, requestId, true)
						writeSSE(toJSON(finalChunk))
						tracer.trExit("done-stop")
					}
				} else {
					finalChunk := formatOpenAIResponse(ResponseResult{Content: "", FinishReason: "stop"}, model, requestId, true)
					writeSSE(toJSON(finalChunk))
					tracer.trExit("done-stop")
				}
				writeSSE("[DONE]")
			}
			break
		}

		close(keepAliveStop)
		wg.Wait()

	} else {
		ch, err := sendToZAI(prompt, opts)
		if err != nil {
			log.Printf("[API] Error: %s", err.Error())
			writeJSON(w, statusFromError(err.Error()), formatOpenAIError(err.Error(), "api_error", nil))
			return
		}

		var fullContent, fullReasoning strings.Builder
		for result := range ch {
			if result.Err != nil {
				log.Printf("[API] Error: %s", result.Err.Error())
				writeJSON(w, statusFromError(result.Err.Error()), formatOpenAIError(result.Err.Error(), "api_error", nil))
				return
			}
			if result.Reasoning != "" {
				fullReasoning.WriteString(result.Reasoning)
				continue
			}
			if result.FullText != "" {
				fullContent.Reset()
				fullContent.WriteString(result.FullText)
			} else {
				fullContent.WriteString(result.Chunk)
			}
		}
		contentStr := fullContent.String()
		reasoningStr := fullReasoning.String()

		// Agent-mode: parse out tool-call blocks for non-stream response
		if config.AgentMode {
			toolCalls := agentExtractToolCalls(contentStr, body.Tools)
			if len(toolCalls) == 0 && reasoningStr != "" {
				toolCalls = agentExtractToolCalls(reasoningStr, body.Tools)
			}
			for _, tc := range toolCalls {
				if fn, ok := tc["function"].(map[string]interface{}); ok {
					if name, _ := fn["name"].(string); name != "" {
						if snapped := agentSnapToolName(name, knownTools); snapped != name {
							log.Printf("[Agent] snapped tool name %q -> %q", name, snapped)
							fn["name"] = snapped
						}
					}
				}
			}
			if len(toolCalls) > 0 {
				stripped := agentStripToolCalls(contentStr)
				writeJSON(w, 200, map[string]interface{}{
					"id":      "chatcmpl-" + requestId,
					"object":  "chat.completion",
					"created": time.Now().Unix(),
					"model":   model,
					"choices": []map[string]interface{}{
						{
							"index": 0,
							"message": func() map[string]interface{} {
								m := map[string]interface{}{
									"role":       "assistant",
									"content":    stripped,
									"tool_calls": toolCalls,
								}
								if reasoningStr != "" {
									m["reasoning_content"] = reasoningStr
								}
								return m
							}(),
							"finish_reason": "tool_calls",
						},
					},
					"usage": map[string]interface{}{
						"prompt_tokens":     estimateTokens(prompt),
						"completion_tokens": estimateTokens(contentStr),
						"total_tokens":      estimateTokens(prompt) + estimateTokens(contentStr),
					},
				})
				return
			}
		}

		writeJSON(w, 200, formatOpenAIResponse(ResponseResult{Content: contentStr, Reasoning: reasoningStr}, model, requestId, false))
	}
}
func featuresHandler(w http.ResponseWriter, r *http.Request) {
	// ── GET: return resolved features for a model ──
	if r.Method == "GET" {
		model := r.URL.Query().Get("model")
		if model != "" {
			resolved := resolveFeaturesForModel(model)
			state := getModelFeatureState(model)
			caps := getModelCapabilities(model)
			writeJSON(w, 200, map[string]interface{}{
				"model":        model,
				"features":     resolved,
				"includeAll":   state.IncludeAll,
				"overrides":    state.Overrides,
				"capabilities": caps,
			})
			return
		}
		// No model specified — return all per-model states
		modelFeatureStatesMu.Lock()
		states := make(map[string]interface{})
		for k, v := range modelFeatureStates {
			states[k] = map[string]interface{}{
				"includeAll": v.IncludeAll,
				"overrides":  v.Overrides,
			}
		}
		modelFeatureStatesMu.Unlock()
		writeJSON(w, 200, map[string]interface{}{
			"states": states,
		})
		return
	}

	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// ── POST: update per-model feature state ──

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, 400, map[string]interface{}{"error": "Failed to read body"})
		return
	}

	// Parse as raw map to capture arbitrary capability keys
	var body map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		writeJSON(w, 400, map[string]interface{}{"error": "Invalid JSON"})
		return
	}

	model, _ := body["model"].(string)
	if model == "" {
		writeJSON(w, 400, map[string]interface{}{"error": "model is required"})
		return
	}

	// Check Include-All-Features header
	includeAllHeader := strings.EqualFold(r.Header.Get("Include-All-Features"), "true")

	modelFeatureStatesMu.Lock()
	state, ok := modelFeatureStates[model]
	if !ok {
		state = &ModelFeatureState{
			IncludeAll: false,
			Overrides:  make(map[string]interface{}),
		}
		modelFeatureStates[model] = state
	}

	// Set IncludeAll flag if header is present
	if includeAllHeader {
		state.IncludeAll = true
	}

	// Process user overrides — any key except "model" is treated as a feature override.
	// Special handling: reasoning/thinking -> enable_thinking
	for k, v := range body {
		if k == "model" {
			continue
		}

		// reasoning: true/false -> enable_thinking
		if k == "reasoning" {
			if b, ok := v.(bool); ok {
				state.Overrides["enable_thinking"] = b
			}
			continue
		}

		// "thinking": {"type":"enabled"|"disabled"} or thinking: true/false -> enable_thinking
		if k == "thinking" {
			if b, ok := v.(bool); ok {
				state.Overrides["enable_thinking"] = b
				continue
			}
			if m, ok := v.(map[string]interface{}); ok {
				if t, ok := m["type"].(string); ok {
					state.Overrides["enable_thinking"] = (t == "enabled")
				}
				continue
			}
			continue
		}

		// All other keys: convert camelCase to snake_case (no alias mapping)
		snakeKey := normalizeFeatureKey(k)
		// image_generation overrides are ignored — always forced false
		if snakeKey == "image_generation" {
			continue
		}
		// 'think' is not accepted — use enable_thinking, reasoning, or thinking
		if snakeKey == "think" {
			continue
		}
		// reasoning_effort is a per-request parameter validated against model
		// capabilities; it is NOT stored as a persistent override.
		if snakeKey == "reasoning_effort" {
			continue
		}
		state.Overrides[snakeKey] = v
	}

	// Resolve final features for response
	caps := getModelCapabilities(model)
	resolved := resolveFeaturesWithState(caps, state)
	includeAll := state.IncludeAll
	overrides := make(map[string]interface{})
	for k, v := range state.Overrides {
		overrides[k] = v
	}
	modelFeatureStatesMu.Unlock()

	// Update session.Features for backward compat (dashboard display)
	session.mu.Lock()
	if v, ok := resolved["auto_web_search"].(bool); ok {
		session.Features.WebSearch = v
		session.Features.AutoWebSearch = v
	}
	if v, ok := resolved["enable_thinking"].(bool); ok {
		session.Features.Thinking = v
	}
	if v, ok := resolved["preview_mode"].(bool); ok {
		session.Features.PreviewMode = v
	}
	session.Features.ImageGen = false
	session.mu.Unlock()

	log.Printf("[Features] model=%s includeAll=%v overrides=%+v resolved=%+v",
		model, includeAll, overrides, resolved)

	writeJSON(w, 200, map[string]interface{}{
		"success":    true,
		"model":      model,
		"includeAll": includeAll,
		"overrides":  overrides,
		"features":   resolved,
	})
}

func statsHandler(w http.ResponseWriter, r *http.Request) {
	session.mu.Lock()
	initialized := session.Initialized
	session.mu.Unlock()

	totalClients := 0
	if initialized {
		totalClients = 1
	}

	writeJSON(w, 200, map[string]interface{}{
		"mode":         "direct",
		"totalClients": totalClients,
		"stats": map[string]interface{}{
			"totalRequests": 0,
		},
	})
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	session.mu.Lock()
	healthy := session.Initialized
	session.mu.Unlock()

	status := 200
	if !healthy {
		status = 503
	}
	writeJSON(w, status, map[string]interface{}{"healthy": healthy, "mode": "direct"})
}

func clientsHandler(w http.ResponseWriter, r *http.Request) {
	session.mu.Lock()
	initialized := session.Initialized
	session.mu.Unlock()

	var clients []map[string]interface{}
	if initialized {
		clients = []map[string]interface{}{
			{"id": "session", "status": "idle"},
		}
	} else {
		clients = []map[string]interface{}{}
	}
	writeJSON(w, 200, map[string]interface{}{"clients": clients})
}

func injectHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"message":"Direct mode"}`))
}

func stopHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]interface{}{
		"success": true,
		"message": "Stop acknowledged",
	})
}
