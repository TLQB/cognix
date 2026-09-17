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

// Auth: Anthropic clients send the API key via x-api-key header (handled in
// checkAuth). The anthropic-version header is allowed via CORS.

// formatAnthropicError builds an Anthropic-style error envelope.
func formatAnthropicError(errType, message string) interface{} {
	return map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    errType,
			"message": message,
		},
	}
}

// extractAnthropicContent coerces an Anthropic content field (string or
// array of content blocks) into a plain string.
func extractAnthropicContent(content interface{}) string {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	if arr, ok := content.([]interface{}); ok {
		var parts []string
		for _, item := range arr {
			if m, ok := item.(map[string]interface{}); ok {
				if t, _ := m["type"].(string); t == "text" {
					if txt, ok := m["text"].(string); ok {
						parts = append(parts, txt)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	b, _ := json.Marshal(content)
	return string(b)
}

// anthropicToOpenAIRequest converts an Anthropic /v1/messages request body
// to an OpenAI /v1/chat/completions request body for the existing sendToZAI
// pipeline.
func anthropicToOpenAIRequest(bodyBytes []byte) ([]byte, error) {
	var req map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		return nil, err
	}

	out := make(map[string]interface{})
	if m, ok := req["model"]; ok {
		out["model"] = m
	}
	if s, ok := req["stream"]; ok {
		out["stream"] = s
	}
	if mt, ok := req["max_tokens"]; ok {
		out["max_tokens"] = mt
	}
	if temp, ok := req["temperature"]; ok {
		out["temperature"] = temp
	}
	if tp, ok := req["top_p"]; ok {
		out["top_p"] = tp
	}
	if ss, ok := req["stop_sequences"]; ok {
		out["stop"] = ss
	}

	var messages []map[string]interface{}

	// System field -> system message
	if sys, ok := req["system"]; ok {
		sysStr := extractAnthropicContent(sys)
		if sysStr != "" {
			messages = append(messages, map[string]interface{}{
				"role":    "system",
				"content": sysStr,
			})
		}
	}

	// Convert messages
	if msgs, ok := req["messages"].([]interface{}); ok {
		for _, m := range msgs {
			mm, ok := m.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := mm["role"].(string)
			content := mm["content"]

			// Handle tool_result content blocks in user messages
			if arr, ok := content.([]interface{}); ok {
				hasToolResult := false
				for _, item := range arr {
					if mp, ok := item.(map[string]interface{}); ok {
						if t, _ := mp["type"].(string); t == "tool_result" {
							hasToolResult = true
							toolUseID, _ := mp["tool_use_id"].(string)
							resultContent := extractAnthropicContent(mp["content"])
							messages = append(messages, map[string]interface{}{
								"role":         "tool",
								"tool_call_id": toolUseID,
								"content":      resultContent,
							})
						}
					}
				}
				if hasToolResult {
					continue
				}
			}

			// Handle assistant tool_use blocks -> OpenAI tool_calls
			if role == "assistant" {
				if arr, ok := content.([]interface{}); ok {
					var textParts []string
					var toolCalls []map[string]interface{}
					for _, item := range arr {
						if mp, ok := item.(map[string]interface{}); ok {
							switch t, _ := mp["type"].(string); t {
							case "text":
								if txt, ok := mp["text"].(string); ok {
									textParts = append(textParts, txt)
								}
							case "tool_use":
								id, _ := mp["id"].(string)
								name, _ := mp["name"].(string)
								args := mp["input"]
								if args == nil {
									args = map[string]interface{}{}
								}
								argsJSON, _ := json.Marshal(args)
								toolCalls = append(toolCalls, map[string]interface{}{
									"id":   id,
									"type": "function",
									"function": map[string]interface{}{
										"name":      name,
										"arguments": string(argsJSON),
									},
								})
							}
						}
					}
					msg := map[string]interface{}{
						"role":    "assistant",
						"content": strings.Join(textParts, "\n"),
					}
					if len(toolCalls) > 0 {
						msg["tool_calls"] = toolCalls
					}
					messages = append(messages, msg)
					continue
				}
			}

			messages = append(messages, map[string]interface{}{
				"role":    role,
				"content": extractAnthropicContent(content),
			})
		}
	}
	out["messages"] = messages

	// Convert tools: input_schema -> parameters, wrap in function
	if tools, ok := req["tools"].([]interface{}); ok && len(tools) > 0 {
		var openaiTools []map[string]interface{}
		for _, t := range tools {
			tm, ok := t.(map[string]interface{})
			if !ok {
				continue
			}
			name, _ := tm["name"].(string)
			desc, _ := tm["description"].(string)
			fn := map[string]interface{}{
				"name":        name,
				"description": desc,
			}
			if is, ok := tm["input_schema"]; ok {
				fn["parameters"] = is
			}
			openaiTools = append(openaiTools, map[string]interface{}{
				"type":     "function",
				"function": fn,
			})
		}
		out["tools"] = openaiTools
	}

	// Convert thinking config
	if thinking, ok := req["thinking"].(map[string]interface{}); ok {
		out["thinking"] = thinking
		if t, _ := thinking["type"].(string); t == "enabled" {
			out["reasoning"] = true
		}
	}

	return json.Marshal(out)
}

// thinkingEnabled returns true if the Anthropic thinking config is enabled.
func thinkingEnabled(thinkingCfg json.RawMessage) bool {
	if len(thinkingCfg) == 0 {
		return false
	}
	var tc struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(thinkingCfg, &tc); err != nil {
		return false
	}
	return tc.Type == "enabled"
}

// anthropicMessagesHandler exposes the Anthropic Messages API at /v1/messages.
func anthropicMessagesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, 405, formatAnthropicError("invalid_request_error", "Method not allowed"))
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, 400, formatAnthropicError("invalid_request_error", "Failed to read body"))
		return
	}

	openaiBody, err := anthropicToOpenAIRequest(bodyBytes)
	if err != nil {
		writeJSON(w, 400, formatAnthropicError("invalid_request_error", "Invalid JSON: "+err.Error()))
		return
	}

	var body struct {
		Model           string          `json:"model"`
		Messages        json.RawMessage `json:"messages"`
		Stream          *bool           `json:"stream"`
		Reasoning       *bool           `json:"reasoning"`
		Thinking        json.RawMessage `json:"thinking"`
		Tools           json.RawMessage `json:"tools"`
		ReasoningEffort string          `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(openaiBody, &body); err != nil {
		writeJSON(w, 400, formatAnthropicError("invalid_request_error", "Conversion error: "+err.Error()))
		return
	}

	var anthReq struct {
		Stream   bool            `json:"stream"`
		Thinking json.RawMessage `json:"thinking"`
	}
	json.Unmarshal(bodyBytes, &anthReq)

	model := body.Model
	if model == "" {
		model = "glm-5"
	}

	var messages []Message
	if err := json.Unmarshal(body.Messages, &messages); err != nil || len(messages) == 0 {
		writeJSON(w, 400, formatAnthropicError("invalid_request_error", "messages is required and must be an array"))
		return
	}

	stream := anthReq.Stream
	// Resume captcha cache generation now — see chatCompletionsHandler.
	if config.AgentMode {
		captchaCache.Wake()
	}
	// Stateless request: run it on a THROWAWAY chat session (see
	// chatCompletionsHandler / session_pool.go). The chat is deleted on
	// Z.AI once the response is fully processed (deferred below).
	chatID, pooled, err := AcquireStatelessSession(r.Context())
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return // client gone; nothing to answer
		}
		writeJSON(w, 503, formatAnthropicError("api_error", err.Error()))
		return
	}
	defer ReleaseStatelessSession(chatID, pooled)
	requestId := "msg_" + generateID()

	var transformedMessages json.RawMessage = body.Messages
	if config.AgentMode {
		if tm, err := agentTransformMessages(body.Messages, body.Tools); err == nil {
			transformedMessages = tm
			var localMsgs []Message
			if err := json.Unmarshal(tm, &localMsgs); err == nil {
				messages = localMsgs
			}
		}
	}

	prompt := messagesToPrompt(messages)

	opts := SendOptions{
		Model:             model,
		ChatID:            chatID,
		ClientMessagesRaw: transformedMessages,
		ReasoningEffort:   body.ReasoningEffort,
	}

	if body.Reasoning != nil {
		opts.Thinking = body.Reasoning
	} else if len(body.Thinking) > 0 {
		enabled := thinkingEnabled(body.Thinking)
		opts.Thinking = &enabled
	}

	// Latency: throttle thinking on mid-loop agent turns (last message =
	// tool result), same policy as chatCompletionsHandler. Anthropic
	// tool_result blocks are converted to role=tool ABOVE (before opts),
	// so the check sees the tool-result tail.
	config.throttleThinkingForTurn(&opts, lastMessageIsToolResult(messages))

	// Declared tool names — used by agentSnapToolName to repair glued tool
	// names, and by agentInferToolName to name args-only payloads
	// (see chatCompletionsHandler). Anthropic tools convert to the OpenAI
	// spelling inside anthropicToOpenAIRequest, so collect after it.
	knownTools := agentCollectToolNames(body.Tools)

	if stream {
		anthropicStreamResponse(w, prompt, opts, model, requestId, knownTools, body.Tools)
	} else {
		anthropicNonStreamResponse(w, prompt, opts, model, requestId, knownTools, body.Tools)
	}
}

// anthropicStreamResponse converts a ZAIResult stream to Anthropic SSE events.
func anthropicStreamResponse(w http.ResponseWriter, prompt string, opts SendOptions, model, requestId string, knownTools []string, toolsRaw json.RawMessage) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, _ := w.(http.Flusher)
	var writeMu sync.Mutex

	writeEvent := func(eventType string, data interface{}) {
		writeMu.Lock()
		defer writeMu.Unlock()
		d, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, string(d))
		if flusher != nil {
			flusher.Flush()
		}
	}

	inputTokens := estimateTokens(prompt)

	// message_start
	writeEvent("message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            requestId,
			"type":          "message",
			"role":          "assistant",
			"content":       []interface{}{},
			"model":         model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]interface{}{
				"input_tokens":  inputTokens,
				"output_tokens": 0,
			},
		},
	})

	writeEvent("ping", map[string]interface{}{"type": "ping"})

	// Keep-alive pings
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
				writeEvent("ping", map[string]interface{}{"type": "ping"})
			case <-keepAliveStop:
				return
			}
		}
	}()

	blockIndex := -1
	currentBlockType := ""
	outputTokens := 0

	startBlock := func(bType string, extra map[string]interface{}) {
		blockIndex++
		currentBlockType = bType
		cb := map[string]interface{}{"type": bType}
		for k, v := range extra {
			cb[k] = v
		}
		writeEvent("content_block_start", map[string]interface{}{
			"type":          "content_block_start",
			"index":         blockIndex,
			"content_block": cb,
		})
	}

	stopBlock := func() {
		if currentBlockType != "" {
			writeEvent("content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": blockIndex,
			})
			currentBlockType = ""
		}
	}

	toolCallEmitted := false

	// emitToolCallEvent converts one parsed tool-call (sub)delta into Anthropic
	// content-block events: a delta carrying an id starts a new tool_use block,
	// an id-less delta only appends partial arguments JSON. Both shims stream
	// a header first, then argument fragments (a header may already carry the
	// first fragment when one upstream chunk covered both).
	emitToolCallEvent := func(tc map[string]interface{}) {
		fn, _ := tc["function"].(map[string]interface{})
		// Repair glued tool names against the request's real tool list before
		// the block reaches the client (see chatCompletionsHandler).
		if fn != nil {
			if name, _ := fn["name"].(string); name != "" {
				if snapped := agentSnapToolName(name, knownTools); snapped != name {
					log.Printf("[Agent] snapped tool name %q -> %q", name, snapped)
					fn["name"] = snapped
				}
			}
		}
		argsStr, _ := fn["arguments"].(string)
		if id, ok := tc["id"].(string); ok && id != "" {
			// Header delta — start new tool_use block
			stopBlock()
			name, _ := fn["name"].(string)
			tooluID := "toolu_" + strings.TrimPrefix(id, "call_")
			startBlock("tool_use", map[string]interface{}{
				"id":    tooluID,
				"name":  name,
				"input": map[string]interface{}{},
			})
			toolCallEmitted = true
			if argsStr != "" {
				writeEvent("content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": blockIndex,
					"delta": map[string]interface{}{
						"type":         "input_json_delta",
						"partial_json": argsStr,
					},
				})
			}
		} else {
			// Arguments delta — emit partial JSON
			if argsStr != "" {
				writeEvent("content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": blockIndex,
					"delta": map[string]interface{}{
						"type":         "input_json_delta",
						"partial_json": argsStr,
					},
				})
			}
		}
	}

	var interceptor agentInterceptor
	if config.AgentMode {
		interceptor = newAgentInterceptor(toolsRaw)
	}

	fullContent := ""

	ch, err := sendToZAI(prompt, opts)
	if err != nil {
		close(keepAliveStop)
		wg.Wait()
		writeEvent("error", formatAnthropicError("api_error", err.Error()))
		writeEvent("message_stop", map[string]interface{}{"type": "message_stop"})
		return
	}

	for result := range ch {
		if result.Err != nil {
			stopBlock()
			close(keepAliveStop)
			wg.Wait()
			writeEvent("error", formatAnthropicError("api_error", result.Err.Error()))
			writeEvent("message_stop", map[string]interface{}{"type": "message_stop"})
			return
		}

		// Reasoning -> thinking content block
		if result.Reasoning != "" {
			if currentBlockType != "thinking" {
				stopBlock()
				startBlock("thinking", map[string]interface{}{"thinking": ""})
			}
			writeEvent("content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]interface{}{
					"type":     "thinking_delta",
					"thinking": result.Reasoning,
				},
			})
			outputTokens += estimateTokens(result.Reasoning)
			continue
		}

		// Content delta
		if result.FullText != "" && !strings.HasPrefix(result.FullText, fullContent) {
			// A deep edit_content rewrite rewound text that was already
			// forwarded: reset the stale agent interceptor, preserving the
			// client-visible call-index sequence (issue #23).
			if interceptor != nil {
				interceptor = rearmAgentInterceptor(interceptor)
			}
		}
		if result.FullText != "" {
			fullContent = result.FullText
		} else {
			fullContent += result.Chunk
		}

		// The parser emits the exact rune-safe delta to forward.
		delta := result.Chunk
		if delta == "" {
			continue
		}

		if interceptor != nil {
			contentDelta, toolCalls := interceptor.feed(delta)
			if contentDelta != "" {
				if currentBlockType != "text" {
					stopBlock()
					startBlock("text", map[string]interface{}{"text": ""})
				}
				writeEvent("content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": blockIndex,
					"delta": map[string]interface{}{
						"type": "text_delta",
						"text": contentDelta,
					},
				})
				outputTokens += estimateTokens(contentDelta)
			}
			for _, tc := range toolCalls {
				emitToolCallEvent(tc)
			}
		} else {
			if currentBlockType != "text" {
				stopBlock()
				startBlock("text", map[string]interface{}{"text": ""})
			}
			writeEvent("content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]interface{}{
					"type": "text_delta",
					"text": delta,
				},
			})
			outputTokens += estimateTokens(delta)
		}
	}

	// Drain the interceptor tail: trailing text plus any tool call whose
	// block only completed at end of stream (modern shim hold-back window).
	if interceptor != nil {
		rem, tailCalls := interceptor.finish()
		if rem != "" && !toolCallEmitted {
			if currentBlockType != "text" {
				stopBlock()
				startBlock("text", map[string]interface{}{"text": ""})
			}
			writeEvent("content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]interface{}{
					"type": "text_delta",
					"text": rem,
				},
			})
			outputTokens += estimateTokens(rem)
		}
		for _, tc := range tailCalls {
			emitToolCallEvent(tc)
		}

		// Safety net: fallback tool call extraction at stream end
		if !toolCallEmitted {
			toolCalls := agentExtractToolCalls(fullContent, toolsRaw)
			for _, tc := range toolCalls {
				emitToolCallEvent(tc)
			}
		}
	}

	stopBlock()

	close(keepAliveStop)
	wg.Wait()

	stopReason := "end_turn"
	if toolCallEmitted {
		stopReason = "tool_use"
	}

	writeEvent("message_delta", map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]interface{}{
			"output_tokens": outputTokens,
		},
	})

	writeEvent("message_stop", map[string]interface{}{"type": "message_stop"})
}

// anthropicNonStreamResponse produces a single Anthropic message object.
func anthropicNonStreamResponse(w http.ResponseWriter, prompt string, opts SendOptions, model, requestId string, knownTools []string, toolsRaw json.RawMessage) {
	ch, err := sendToZAI(prompt, opts)
	if err != nil {
		writeJSON(w, statusFromError(err.Error()), formatAnthropicError("api_error", err.Error()))
		return
	}

	fullContent := ""
	fullReasoning := ""
	for result := range ch {
		if result.Err != nil {
			writeJSON(w, statusFromError(result.Err.Error()), formatAnthropicError("api_error", result.Err.Error()))
			return
		}
		if result.Reasoning != "" {
			fullReasoning += result.Reasoning
			continue
		}
		if result.FullText != "" {
			fullContent = result.FullText
		} else {
			fullContent += result.Chunk
		}
	}

	content := []interface{}{}
	stopReason := "end_turn"

	if fullReasoning != "" {
		content = append(content, map[string]interface{}{
			"type":     "thinking",
			"thinking": fullReasoning,
		})
	}

	if config.AgentMode {
		toolCalls := agentExtractToolCalls(fullContent, toolsRaw)
		if len(toolCalls) > 0 {
			stripped := agentStripToolCalls(fullContent)
			if stripped != "" {
				content = append(content, map[string]interface{}{
					"type": "text",
					"text": stripped,
				})
			}
			for _, tc := range toolCalls {
				fn, _ := tc["function"].(map[string]interface{})
				name, _ := fn["name"].(string)
				if snapped := agentSnapToolName(name, knownTools); snapped != name {
					log.Printf("[Agent] snapped tool name %q -> %q", name, snapped)
					name = snapped
				}
				argsStr, _ := fn["arguments"].(string)
				id, _ := tc["id"].(string)
				tooluID := "toolu_" + strings.TrimPrefix(id, "call_")
				var args interface{}
				json.Unmarshal([]byte(argsStr), &args)
				if args == nil {
					args = map[string]interface{}{}
				}
				content = append(content, map[string]interface{}{
					"type":  "tool_use",
					"id":    tooluID,
					"name":  name,
					"input": args,
				})
			}
			stopReason = "tool_use"
		} else {
			content = append(content, map[string]interface{}{
				"type": "text",
				"text": fullContent,
			})
		}
	} else {
		content = append(content, map[string]interface{}{
			"type": "text",
			"text": fullContent,
		})
	}

	writeJSON(w, 200, map[string]interface{}{
		"id":            requestId,
		"type":          "message",
		"role":          "assistant",
		"content":       content,
		"model":         model,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]interface{}{
			"input_tokens":  estimateTokens(prompt),
			"output_tokens": estimateTokens(fullContent),
		},
	})
}
