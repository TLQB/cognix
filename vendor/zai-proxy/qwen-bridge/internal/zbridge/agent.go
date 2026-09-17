// agent.go
//
// ============================================================================
// AGENT MODE (MODERN) — XML-Sectioned Prompt Shim for Qwen Compatibility
// ============================================================================
//
// Port of the modern agentMode compatibility shim from DeepseekFreeAPI
// (internal/dsproxy/agent.go), adapted for the Qwen bridge.
//
// Enable with --agent-mode / AGENT_MODE=true (native variant is the
// default; the modern fold and the old [ROLE: ...] rewrite shim stay
// available via --agent-mode-variant=modern|legacy / AGENT_MODE_VARIANT).
//
// Why the modern shim replaces the legacy one:
//
//   - Legacy flattened the conversation into "[ROLE: x]" prefixed user
//     messages plus a [TOOL CONTRACT] blob. Models suffer context rot on
//     that flat format: marker ambiguity, no recency anchor, full verbatim
//     replay of long tool histories.
//
//   - Modern structures the prompt with explicit XML-like section tags
//     (<system>, <tools>, <history_summary>, <recent>, <current_task>,
//     <output_rules>), summarizes older tool exchanges, anchors the latest
//     user message as the current task, and repeats the output contract at
//     the very end (recency bias).
//
//   - Modern parsing is tolerant where legacy was strict:
//       * markers matched with 2..4 angle brackets per side (models
//         miscount brackets in the wild),
//       * ```json fences adjacent to markers are stripped,
//       * payload shapes beyond {"name","arguments"} are accepted
//         (flat {"tool": ..., params...} and alternate key spellings),
//       * the streaming interceptor holds back a trailing window so a
//         marker split across upstream chunks can never leak as content.
//
// No native tool calling is used: the model emits the tool-call protocol
// below, which is converted back to OpenAI tool_calls on the way out (parsed
// from the finished text, or incrementally while streaming).

package zbridge

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const agentToolStart = "<<<TOOL_CALL>>>"
const agentToolEnd = "<<<END_TOOL_CALL>>>"

// ── marker tolerance ─────────────────────────────────────────────────────────
// Models occasionally miscount the angle brackets framing the markers —
// observed in the wild with deepseek-v4-pro emitting "<<TOOL_CALL>>>" (two
// leading '<') while producing a well-formed "<<<END_TOOL_CALL>>>". An
// exact-literal matcher silently misses such blocks and the whole tool call
// leaks to the client as plain content. Both markers are therefore matched
// with a bracket run of 2..4 on each side; emission above stays canonical.
const (
	agentStartWord   = "TOOL_CALL"
	agentEndWord     = "END_TOOL_CALL"
	agentMinBrackets = 1
	agentMaxBrackets = 6
)

// agentWorstMarkerLen is the longest accepted spelling ("<<<<<<TOOL_CALL>>>>>>").
// Also account for single-bracket and zero-bracket variants.
const agentWorstMarkerLen = 2*agentMaxBrackets + len(agentStartWord)

// bracketRunBack counts the run of b bytes ending immediately before s[i].
func bracketRunBack(s string, i int, b byte) int {
	n := 0
	for i-n-1 >= 0 && s[i-n-1] == b {
		n++
	}
	return n
}

// bracketRunForward counts the run of b bytes starting at s[0].
func bracketRunForward(s string, b byte) int {
	n := 0
	for n < len(s) && s[n] == b {
		n++
	}
	return n
}

// Sentinel results for findAgentMarker.
const (
	markerNone       = -1 // no framed occurrence of word in s
	markerIncomplete = -2 // a candidate needs more bytes before it can match
)

// findAgentMarker locates the first occurrence of word framed by 0..6 '<'
// immediately before and 1..6 '>' immediately after. A zero-bracket lead is
// accepted only for standalone words, catching distorted openings where the
// model dropped every leading bracket. It returns the index of the first
// bracket and the full marker length, or markerNone / markerIncomplete.
// Occurrences not so framed (the TOOL_CALL inside an END marker, prose,
// code) are skipped.
//
// Matching is case-insensitive so that both the canonical <<<TOOL_CALL>>>
// and the lowercase variant emitted by some models are accepted.
//
// Streaming correctness: a trailing '>' run that reaches the end of s has no
// terminating byte yet, so the run may still grow — with final=false that is
// reported as markerIncomplete instead of matching short (which would leak
// the missing brackets as content) or rejecting outright. With final=true
// (finished text) an end-of-string run is taken as is.
func findAgentMarker(s, word string, final bool) (int, int) {
	wordLower := strings.ToLower(word)
	sLower := strings.ToLower(s)
	for from := 0; ; {
		j := strings.Index(sLower[from:], wordLower)
		if j < 0 {
			return markerNone, 0
		}
		w := from + j
		lead := bracketRunBack(s, w, '<')
		if lead > agentMaxBrackets {
			from = w + len(word)
			continue
		}
		if lead < agentMinBrackets {
			if w > 0 {
				prev := s[w-1]
				if prev == '_' || prev == '/' || prev == '>' {
					from = w + len(word)
					continue
				}
			}
		}
		after := s[w+len(word):]
		trail := bracketRunForward(after, '>')
		switch {
		case trail > agentMaxBrackets:
			// Definitively over-long; more bytes cannot shrink the run.
		case trail == len(after) && !final:
			// The '>' run touches the end of the available data and may
			// still grow past min/max — wait for a terminating byte.
			return markerIncomplete, 0
		case trail >= agentMinBrackets:
			return w - lead, lead + len(word) + trail
		}
		from = w + len(word)
	}
}

// agentEndTagWords lists the words accepted inside an XML-style closing tag
// that terminates a tool-call block. The first entry is the protocol's own end
// tag; the rest mirror the accepted argument-key spellings (agentArgKeys)
// because models that emit an args-only payload sometimes close the
// arguments object with the tag matching the key they just wrote
// (production: {"arguments":{...}} followed by </arguments> —
// error_toolcall_log.md 2026-09-06). The set is deliberately bounded so
// prose tags like </p> or </div> can never terminate a span. Trade-off: an
// argument string VALUE that itself contains one of these closers truncates
// the span early; the JSON repair pass usually still recovers the call from
// the truncated body, and that shape has not been observed in the wild.
var agentEndTagWords = []string{"tool_call", "arguments", "parameters", "args", "params", "input"}

// findAgentEndMarkerInBuffer searches for either END_TOOL_CALL (with
// brackets) or an XML-style closing tag. It returns the offset and total
// length of whichever appears first, or (markerNone, 0) / markerIncomplete.
func findAgentEndMarkerInBuffer(s string, final bool) (int, int) {
	// Try the standard END_TOOL_CALL marker first.
	idx, elen := findAgentMarker(s, agentEndWord, final)
	// An incomplete END run must resolve before anything later in the
	// buffer can be trusted as the block end: keep holding.
	if idx == markerIncomplete {
		return markerIncomplete, 0
	}
	// XML-style closing tags (case-insensitive). The earliest occurrence
	// across the bracketed marker plus every accepted spelling wins: a
	// mirror closer before a later END marker must close the span rather
	// than let the END marker swallow everything between them.
	sLower := strings.ToLower(s)
	best, bestLen := -1, 0
	for _, w := range agentEndTagWords {
		tag := "<" + "/" + w + ">"
		if j := strings.Index(sLower, tag); j >= 0 && (best < 0 || j < best) {
			best, bestLen = j, len(tag)
		}
	}
	if idx >= 0 && (best < 0 || idx < best) {
		return idx, elen
	}
	if best >= 0 {
		return best, bestLen
	}
	return markerNone, 0
}

// agentSpan marks one complete tool-call block in finished text:
// [start,end) covers both markers, [bodyStart,bodyEnd) the JSON between them.
type agentSpan struct {
	start, bodyStart, bodyEnd, end int
}

// findAgentSpans walks every complete tolerant tool-call block in text.
// An unterminated opening marker is ignored, matching the old literal scan.
// Supports <<<END_TOOL_CALL>>>, </tool_call>, and mirror closing tags built
// from accepted argument-key spellings (see agentEndTagWords).
func findAgentSpans(text string) []agentSpan {
	var spans []agentSpan
	for pos := 0; ; {
		s, slen := findAgentMarker(text[pos:], agentStartWord, true)
		if s < 0 {
			return spans
		}
		bodyStart := pos + s + slen
		e, elen := findAgentEndMarkerInBuffer(text[bodyStart:], true)
		if e < 0 {
			return spans
		}
		spans = append(spans, agentSpan{
			start:     pos + s,
			bodyStart: bodyStart,
			bodyEnd:   bodyStart + e,
			end:       bodyStart + e + elen,
		})
		pos = bodyStart + e + elen
	}
}

// ── prompt architecture ──────────────────────────────────────────────────────
// The prompt is structured with explicit XML-like section tags so the model
// can clearly distinguish instructions, tools, conversation history, and the
// current task. This eliminates context rot: the model no longer has to parse
// a flat blob of role-tagged text.
//
// Structure:
//   <system>        — compact output contract
//   <tools>         — available tool definitions
//   <history>       — older conversation turns (summarized if too long)
//   <recent>        — recent turns with grouped tool exchanges
//   <current_task>  — the latest user message (recency anchor)
//   <output_rules>  — final reminder at the very end (heaviest weight)

// agentCallSchema is the exact JSON payload shape required inside a
// tool-call block. It is stated verbatim in the prompt (and repeated in the
// final reminder): a bare "{JSON}" placeholder let models invent flat
// payloads like {"tool": "bash", "command": ...} that the runtime cannot
// map back to OpenAI tool_calls reliably.
const agentCallSchema = `{"name":"<tool_name>","arguments":{<parameter JSON>}}`

const agentSystemPrefix = "<system>\n" +
	"You are a helpful assistant with access to tools. Follow these rules strictly:\n" +
	"\n" +
	"REPLY FORMAT — exactly ONE of:\n" +
	"(A) TOOL CALL: <<<TOOL_CALL>>>" + agentCallSchema + "<<<END_TOOL_CALL>>> — nothing before or after.\n" +
	"    The JSON object has EXACTLY two keys: \"name\" (the tool to call, spelled exactly as in <tools>) and \"arguments\" (an object with ONLY that tool's parameters).\n" +
	"(B) FINAL ANSWER: plain text, only when no tool applies.\n" +
	"\n" +
	"RULES:\n" +
	"- A reply that only ANNOUNCES an intention (\u201cI'll now...\u201d, \u201cLet me...\u201d) without a tool-call block is a PROTOCOL VIOLATION: it stalls the task and forces the user to repeat the request. If any tool is needed for the next step, the tool-call block MUST be in this very reply.\n" +
	"- Never print code fences (" + "```bash" + ", " + "```json" + "). Only the runtime executes tools.\n" +
	"- Never wrap tool-call markers in code fences.\n" +
	"- Never invent results. Stop at <<<END_TOOL_CALL>>> and wait for tool output.\n" +
	"- Never call a tool not listed in <tools>.\n" +
	"- A tool call whose identical result already appears in <recent> or <history_summary> is DONE — do not repeat it; move to the next step or produce the final answer.\n" +
	"- PROGRESS: every turn must move the task forward. If the last tool result already answers the current step, do NOT call the same tool again — either advance to the next step or give the final answer.\n" +
	"- NEVER REPEAT: do not re-issue any tool call already listed in <already_called>. If its result was insufficient, change the call (different arguments or different tool), never resend it as-is.\n" +
	"- If the task is fully done, answer with the result in plain text — do not start another tool call.\n" +
	"</system>"

// agentFinalReminder is appended at the very end of the prompt. Models weight
// the end of the prompt most heavily (recency bias), so the output contract
// is repeated here as the last thing the model sees.
const agentFinalReminder = `<output_rules>
RESPOND WITH EXACTLY ONE OF:
1. <<<TOOL_CALL>>>{"name":"<tool_name>","arguments":{...}}<<<END_TOOL_CALL>>> (no fences, no other text)
2. Plain text final answer (only if NO tool applies to this step — announcing a plan is NOT a final answer)
The tool-call JSON uses EXACTLY the keys "name" and "arguments" — never a "tool" key, never bare top-level parameters.
NO REPEATS: a call already listed in <already_called> must not be re-issued. Same step answered? Move on or answer in plain text.
If the last message is a tool result, you MUST either issue the next tool call or the final answer in this reply — never stop after describing what you will do.
</output_rules>`

// ── OpenAI wire types ────────────────────────────────────────────────────────

// agentMessage is one OpenAI-style message of the incoming request. Content
// stays raw so both string and typed-part arrays are accepted. Unlike the
// minimal Message type used for the Qwen wire, it carries the tool fields
// the modern prompt builder needs to replay prior tool exchanges.
type agentMessage struct {
	Role       string              `json:"role"`
	Content    json.RawMessage     `json:"content"`
	ToolCallID string              `json:"tool_call_id,omitempty"`
	ToolCalls  []assistantToolCall `json:"tool_calls,omitempty"`
	Name       string              `json:"name,omitempty"`
}

// openAITool is one entry of the OpenAI `tools` array. Both the nested form
// ({type:"function",function:{...}}) and flat definitions are accepted,
// mirroring the JS `(tool?.function || tool)` handling.
type openAITool struct {
	Type       string          `json:"type"`
	Function   *openAIFnSpec   `json:"function,omitempty"`
	Name       string          `json:"name,omitempty"`
	Descr      string          `json:"description,omitempty"`
	Parameters json.RawMessage `json:"parameters,omitempty"`
}

type openAIFnSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

func (t *openAITool) fnName() string {
	if t.Function != nil && t.Function.Name != "" {
		return t.Function.Name
	}
	return t.Name
}

func (t *openAITool) fnDescription() string {
	if t.Function != nil && t.Function.Description != "" {
		return t.Function.Description
	}
	return t.Descr
}

func (t *openAITool) fnParameters() json.RawMessage {
	if t.Function != nil && len(t.Function.Parameters) > 0 {
		return t.Function.Parameters
	}
	return t.Parameters
}

// assistantToolCall is a tool call inside an assistant message of the
// incoming request (the client replaying previous calls).
type assistantToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"` // JSON-encoded string per spec
	} `json:"function"`
}

// ── prompt building ──────────────────────────────────────────────────────────

// contentToText flattens OpenAI message content (string or typed parts) to text.
func contentToText(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	var s string
	if json.Unmarshal(trimmed, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(trimmed, &parts) == nil {
		texts := make([]string, 0, len(parts))
		for _, p := range parts {
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return string(trimmed)
}

func jsonIndent(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, bytes.TrimSpace(raw), "", "  "); err != nil {
		return string(bytes.TrimSpace(raw))
	}
	return buf.String()
}

// renderAgentTools renders the OpenAI tools array as the [TOOL CONTRACT] block.
func renderAgentTools(tools []openAITool) string {
	if len(tools) == 0 {
		return "(no tools provided)"
	}
	var b strings.Builder
	for i, tool := range tools {
		name := tool.fnName()
		if name == "" {
			continue
		}
		b.WriteString(fmt.Sprintf("### Tool %d: %s", i+1, name))
		if desc := tool.fnDescription(); desc != "" {
			b.WriteString("\nDescription: " + desc)
		}
		if params := tool.fnParameters(); len(params) > 0 && !bytes.Equal(bytes.TrimSpace(params), []byte("null")) {
			b.WriteString("\nParameters JSON Schema:\n" + jsonIndent(params))
		}
		b.WriteString("\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// agentDanglingXMLCallMarker reports whether s contains a half-open tool-call
// transcript the model can still continue later in the SAME turn (one that	// parser recovery cannot currently see: no JSON payload matched). GLM 5.3
// pseudo-XML stall (stall_evident.md 2026-09-07): the model echoes the
// prompt's XML framing as a transcript — "...<assistant>...\n<tool_call>\n{"
// name":"read_file","arguments":{"path":"/a"}}\n</arguments>" — and stops
// mid-transcript. Because the content contains "<tool_call" the early return
// waved it through as an 'attempted call'; nothing parsed, the client saw a
// raw protocol leak, and no retry fired. If the model CONTINUES, recovery
// parses the call; if the stream ends, this predicate keeps the stall retry
// reachable.
//
// Only the LAST "tool_call" occurrence is judged (the model's final attempt).
// A call the parser CAN recover short-circuits to false (nothing dangling),
// and prose mentions followed by ordinary sentences ("send a <tool_call>
// now.") are NOT dangling — a closed tag with prose after it is ordinary
// text.
func agentDanglingXMLCallMarker(s string) bool {
	if len(ParseAgentToolCalls(s)) > 0 {
		return false // a real call was recovered — nothing dangling
	}
	lower := strings.ToLower(s)
	i := strings.LastIndex(lower, "tool_call")
	if i < 0 {
		return false
	}
	after := lower[i+len("tool_call"):]
	gt := 0
	for gt < len(after) && after[gt] == '>' {
		gt++
	}
	rest := strings.TrimLeft(after[gt:], " \t\r\n")
	if rest == "" {
		return true // tag at end of content: the attempt died right there
	}
	if rest[0] == '{' {
		return true // pseudo-block body the parser could not recover
	}
	if !strings.Contains(rest, "{") {
		return false // prose after the vocabulary — ordinary text
	}
	// Braces later in the tail: a glued pseudo-tag name (e.g.
	// "<tool_callgrep>{...}") only when the first byte continues a name on
	// an UNCLOSED tag; a closed tag followed by a brace was already handled
	// above.
	return gt == 0 && isToolNameByte(rest[0])
}

// isToolNameByte reports whether c can appear inside a tool name.
func isToolNameByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-' || c == '.'
}

// agentCallPayload is the JSON object emitted inside a tool-call block.
// A struct (not a map) keeps the documented name-first key order.
type agentCallPayload struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// renderToolCallBlock renders a single assistant tool-call block in the wire
// protocol format (used both in prompt history and response parsing).
func renderToolCallBlock(call assistantToolCall) string {
	payload, err := json.Marshal(agentCallPayload{
		Name:      call.Function.Name,
		Arguments: json.RawMessage(agentParseArguments(call.Function.Arguments)),
	})
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%s\n%s\n%s", agentToolStart, payload, agentToolEnd)
}

// renderAssistantTurn renders an assistant message with optional text and tool
// calls inside an XML-like tag.
func renderAssistantTurn(m agentMessage) string {
	text := contentToText(m.Content)
	var blocks []string
	if text != "" {
		blocks = append(blocks, text)
	}
	for _, call := range m.ToolCalls {
		if block := renderToolCallBlock(call); block != "" {
			blocks = append(blocks, block)
		}
	}
	content := strings.Join(blocks, "\n")
	return fmt.Sprintf("<assistant>\n%s\n</assistant>", content)
}

// renderUserTurn renders a user message inside an XML-like tag.
func renderUserTurn(m agentMessage) string {
	text := contentToText(m.Content)
	if text == "" {
		return ""
	}
	return fmt.Sprintf("<user>\n%s\n</user>", text)
}

// renderSystemTurn renders a system message inside an XML-like tag.
func renderSystemTurn(m agentMessage) string {
	text := contentToText(m.Content)
	if text == "" {
		return ""
	}
	return fmt.Sprintf("<system_message>\n%s\n</system_message>", text)
}

// renderToolResult renders a tool result inside an XML-like tag with the
// call_id attribute for unambiguous matching.
func renderToolResult(m agentMessage) string {
	text := contentToText(m.Content)
	attr := ""
	if m.ToolCallID != "" {
		attr = fmt.Sprintf(` call_id="%s"`, m.ToolCallID)
	}
	return fmt.Sprintf("<tool_result%s>\n%s\n</tool_result>", attr, text)
}

// renderAgentMessage renders one OpenAI message using XML-like section tags.
// This replaces the old [ROLE: ...] format with clearly delimited sections
// that the model can parse unambiguously.
func renderAgentMessage(m agentMessage) string {
	role := strings.TrimSpace(m.Role)
	if role == "" {
		role = "user"
	}
	switch role {
	case "system":
		return renderSystemTurn(m)
	case "user":
		return renderUserTurn(m)
	case "assistant":
		return renderAssistantTurn(m)
	case "tool":
		return renderToolResult(m)
	default:
		// Unknown role: render as user with role annotation.
		text := contentToText(m.Content)
		return fmt.Sprintf("<user role=%s>\n%s\n</user>", role, text)
	}
}

// ── history summarization ────────────────────────────────────────────────────
//
// For long conversations (many tool exchanges), the full replay causes context
// rot: the model loses focus on the current task. We summarize older turns
// into a compact context block while keeping the most recent turns verbatim.

// maxRecentToolExchanges is the number of recent tool-exchange pairs to keep
// in full detail. Older ones get summarized.
const maxRecentToolExchanges = 6

// toolExchange records one assistant→tool exchange for summarization.
type toolExchange struct {
	toolName string
	summary  string // truncated tool result
}

// summarizeOldHistory extracts tool-exchange summaries from older messages and
// returns a compact <history_summary> block. Returns empty string if there's
// nothing to summarize.
func summarizeOldHistory(exchanges []toolExchange) string {
	if len(exchanges) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<history_summary>\nPreviously completed tool calls:\n")
	for i, ex := range exchanges {
		b.WriteString(fmt.Sprintf("%d. %s → %s\n", i+1, ex.toolName, ex.summary))
	}
	b.WriteString("</history_summary>")
	return b.String()
}

// extractToolExchanges scans messages and returns (old exchanges beyond the
// recent window, messages to render verbatim).
func extractToolExchanges(messages []agentMessage) (old []toolExchange, recent []agentMessage) {
	// First pass: identify tool-exchange boundaries.
	// A tool exchange = assistant with tool_calls followed by 1+ tool results.
	type exchange struct{ start, end int } // indices into messages
	var exchanges []exchange
	i := 0
	for i < len(messages) {
		if messages[i].Role == "assistant" && len(messages[i].ToolCalls) > 0 {
			ex := exchange{start: i}
			i++
			// skip tool results
			for i < len(messages) && messages[i].Role == "tool" {
				i++
			}
			ex.end = i
			exchanges = append(exchanges, ex)
		} else {
			i++
		}
	}

	// If there aren't enough exchanges to summarize, keep everything.
	if len(exchanges) <= maxRecentToolExchanges {
		return nil, messages
	}

	// Summarize exchanges before the recent window.
	//
	// Keep every non-exchange message (user / system / plain assistant) that
	// falls between summarized exchanges in the recent window instead of
	// dropping it: user instructions interleaved with tool loops are task
	// context, and losing them made the model lose the task mid-conversation
	// (observed in production logs).
	splitIdx := exchanges[len(exchanges)-maxRecentToolExchanges].start
	prevEnd := 0
	for _, ex := range exchanges[:len(exchanges)-maxRecentToolExchanges] {
		if ex.start >= splitIdx {
			break
		}
		// Carry over any non-exchange messages preceding this exchange.
		for k := prevEnd; k < ex.start; k++ {
			if messages[k].Role != "assistant" || len(messages[k].ToolCalls) == 0 {
				recent = append(recent, messages[k])
			}
		}
		prevEnd = ex.end
		// Collect tool names and truncated results from this exchange.
		assistant := messages[ex.start]
		names := make([]string, 0, len(assistant.ToolCalls))
		for _, tc := range assistant.ToolCalls {
			names = append(names, tc.Function.Name)
		}
		toolName := strings.Join(names, ", ")
		// Grab first tool result as summary.
		summary := "ok"
		if ex.end > ex.start+1 {
			result := contentToText(messages[ex.start+1].Content)
			if len(result) > 80 {
				cut := 77
				for cut > 0 && !utf8.RuneStart(result[cut]) {
					cut-- // never split a multi-byte rune (issue #23)
				}
				result = result[:cut] + "..."
			}
			summary = result
		}
		old = append(old, toolExchange{toolName: toolName, summary: summary})
	}
	// Carry over trailing non-exchange messages up to the split point.
	for k := prevEnd; k < splitIdx; k++ {
		if messages[k].Role != "assistant" || len(messages[k].ToolCalls) == 0 {
			recent = append(recent, messages[k])
		}
	}
	recent = append(recent, messages[splitIdx:]...)
	return old, recent
}

// buildAgentPrompt constructs the prompt sent to Qwen. The prompt is
// structured with explicit XML-like section tags so the model can clearly
// distinguish instructions, tools, history, and the current task.
//
// Structure:
//
//	<system>             — compact output contract
//	<tools>              — available tool definitions
//	<history_summary>    — summarized older turns (if conversation is long)
//	<recent>             — recent turns in full detail with grouped tool exchanges
//	<current_task>       — the latest user message (recency anchor)
//	<output_rules>       — final reminder at the very end (heaviest weight)
func buildAgentPrompt(messages []agentMessage, tools []openAITool) string {
	var b strings.Builder

	// 1. System instructions (compact).
	b.WriteString(agentSystemPrefix)
	b.WriteString("\n\n")

	// 2. Tool contract.
	b.WriteString("<tools>\n")
	b.WriteString(renderAgentTools(tools))
	b.WriteString("\n</tools>\n\n")

	// 3. Split messages into old (summarizable) and recent.
	oldExchanges, recentMessages := extractToolExchanges(messages)

	if summary := summarizeOldHistory(oldExchanges); summary != "" {
		b.WriteString(summary)
		b.WriteString("\n\n")
	}

	// 4. Render recent conversation turns with grouped tool exchanges.
	if len(recentMessages) > 0 {
		b.WriteString("<recent>\n")
		renderRecentConversation(&b, recentMessages)
		b.WriteString("</recent>\n\n")
	}

	// 5. Anti-loop state: the deduplicated list of calls already made. Placed
	//    late in the prompt (recency) so the model checks it against the
	//    current task before emitting anything.
	if already := agentAlreadyCalledList(messages); already != "" {
		b.WriteString(already)
		b.WriteString("\n\n")
	}

	// 6. Extract the LAST user message as the explicit current task.
	//    Mid-task requests (assistant turns / tool results after the last user
	//    message) must NOT re-present the user's imperative as a fresh task:
	//    the model weights the end of the prompt most, and a stale "build it
	//    again" anchor here made it re-run the identical tool call forever
	//    (observed in production logs). Instead the anchor tells it to continue
	//    from the tool results already rendered in <recent>.
	lastUserIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			lastUserIdx = i
			break
		}
	}
	if lastUserIdx >= 0 {
		text := contentToText(messages[lastUserIdx].Content)
		if text != "" {
			b.WriteString("<current_task>\n")
			if lastUserIdx == len(messages)-1 {
				b.WriteString(text)
			} else {
				b.WriteString("Continue the user's task using the progress above (the latest tool results are at the end of <recent>).\n")
				b.WriteString("User's request: \"" + text + "\"\n")
				b.WriteString("Decide the next step now: ONE new tool call, or the final answer if the results above already satisfy the request. Do NOT repeat a tool call that already has a result in <recent>.\n")
			}
			b.WriteString("\n</current_task>\n\n")
		}
	}

	// 7. Final output contract reminder (recency anchor).
	b.WriteString(agentFinalReminder)

	return b.String()
}

// looksLikeAnnouncementOnly detects the agent stall: the turn ended with
// intent language and NO tool call block. Used by handlers to auto-retry.
//
// Intent language counts only when it appears in the ANSWER (content).
// Reasoning is where models plan on every turn ("I should...", "Let
// me...") — including turns that end in a complete final answer. Counting
// reasoning-only phrases made a simple greeting (thinking: "I should
// respond with a friendly greeting") trigger two stall retries, and since
// SSE cannot un-send, the client received three concatenated greetings
// (E2E log). Reasoning is consulted only when the content is empty — the
// GLM 5.2/5.3 stall shape where the whole turn is an unfinished
// announcement inside <details>. Any tool-call marker anywhere means the
// turn produced a tool call (possibly leaked as text, handled by the
// safety net) — not a stall.
func looksLikeAnnouncementOnly(content, reasoning string) bool {
	contentTrim := strings.TrimSpace(content)
	reasoningTrim := strings.TrimSpace(reasoning)
	if contentTrim == "" && reasoningTrim == "" {
		return false
	}
	combined := content + "\n" + reasoning
	lower := strings.ToLower(combined)
	open := "<<<" + "TOOL_CALL" + ">>>"
	close := "<<<" + "END_TOOL_CALL" + ">>>"
	contentLower := strings.ToLower(content)
	contentHasMarker := strings.Contains(contentLower, open) ||
		strings.Contains(contentLower, close) ||
		strings.Contains(contentLower, "<tool_call") ||
		strings.Contains(contentLower, "function_call") ||
		strings.Contains(contentLower, "<function>")
	if contentHasMarker {
		// Escape hatch (stall_evident.md 2026-09-07): a leaked block the
		// parser genuinely recovered IS a tool call — not a stall. But the
		// model also half-echoes the marker vocabulary ("<<<TOOL_CALL ...
		// transcript fragments, dangling <tool_call> pseudo-XML) that parse
		// into NOTHING; when the final marker attempt died unrecovered, the
		// announced intent is the stall target and must stay detectable,
		// not be waved through. Prose MENTIONS of the vocabulary ("send a
		// <tool_call> now.") stay non-stalls as before.
		if len(ParseAgentToolCalls(content)) > 0 {
			return false
		}
		if agentDanglingXMLCallMarker(content) {
			if !contentJunkLike(strings.TrimSpace(content)) &&
				contentHasAnnouncementPhrase(content) {
				return true
			}
		}
		return false
	}
	if strings.Contains(lower, open) ||
		strings.Contains(lower, close) ||
		strings.Contains(lower, "<tool_call") ||
		strings.Contains(lower, "function_call") ||
		strings.Contains(lower, "<function>") {
		// Marker only in the reasoning: the model produced a block there
		// and the reasoning safety net owns it (original contract).
		return false
	}
	// The stall target is the content: a stall is an ANSWER that is just
	// an announcement. When the content carries no meaningful text the
	// reasoning body carries the announcement to detect. This covers two
	// observed stall shapes: fully empty content, and junk content — 1-3
	// stray bytes like a truncated marker fragment or a stray punctuation
	// the model emits before dying (bridge.log 2026-09-05: contentOut=1
	// with 494b of planning-only reasoning). A real answer, however short,
	// has letters/digits; junk does not.
	target := contentTrim
	if contentJunkLike(contentTrim) {
		target = reasoningTrim
	}
	if target == "" {
		return false
	}
	// GLM 5.3 can emit longer preambles before stalling; a body this long
	// is far more likely a genuine answer than an announcement.
	if len(target) > announcementMaxLen {
		return false
	}
	if contentHasAnnouncementPhrase(target) && !contentHasOfferPhrase(target) {
		return true
	}
	return false
}

// contentJunkLike reports whether content carries no meaningful text:
// empty, or only punctuation/whitespace/bytes that are not letters or
// digits (in any script — unicode.IsLetter covers ASCII + CJK + Vietnamese
// alike). Junk content is the classic pre-tool-call fragment the model
// emits right before the stream dies mid-announcement.
func contentJunkLike(content string) bool {
	for _, r := range content {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// agentToolResultBlankTurn detects the post-tool-result stall shape: the
// prompt's last turn was a tool RESULT, so the model's reply MUST be the
// next tool-call block or the plain-text final answer. GLM 5.3 sometimes
// closes a long thinking block and ends the turn with content that is empty
// or junk-only (0-3 stray bytes — bridge.log done-stop contentOut≈1 right
// after a tool result), producing no tool call and no answer. The task then
// stalls invisibly because there is no intent phrase for
// looksLikeAnnouncementOnly to catch (the planning, if any, lives in
// reasoning and often has no phrase either). An empty/junk reply after a
// tool result is never a legitimate final answer — a real one, however
// short ("done"), has letters/digits — so this needs NO phrase list and NO
// reasoning-length bound: the retry budget (StallMaxRetries, default 2)
// bounds the cost even when the stall followed a very long thinking block.
// Callers apply it ONLY when the last user-side message role is "tool" (the
// tool-loop context).
func agentToolResultBlankTurn(content, reasoning string) bool {
	contentTrim := strings.TrimSpace(content)
	if contentTrim == "" {
		return true
	}
	// A dangling marker transcript after a tool result is never a finished
	// reply (stall_evident.md 2026-09-07): the model half-echoed the prompt
	// framing and died mid-transcript. toolCallEmitted gating in the handler
	// keeps this harmless when the safety net DID recover a call from the
	// same text.
	if agentDanglingXMLCallMarker(contentTrim) {
		return true
	}
	return contentJunkLike(contentTrim)
}

// announcementPhrases is the intent-phrase list shared by the stall
// detector (looksLikeAnnouncementOnly) and the streaming stall buffer
// (handlers.go): the buffer only holds content that could still trip the
// detector — everything else streams live.
var announcementPhrases = []string{
	// English
	"i'll ", "i will ", "let me ", "i'm going to ", "i now",
	"i am going to ", "i'll now", "let's ",
	"i need to ", "i should ", "i would like to ",
	"i want to ", "i have to ", "i must ", "i plan to ",
	"now i ", "next i ", "first i ", "then i ",
	// Vietnamese
	"tôi sẽ", "để tôi", "mình sẽ", "để mình", "em sẽ",
	"tôi xác minh", "tôi kiểm tra", "mình kiểm tra", "tôi đọc", "mình đọc",
	"tôi tìm", "mình tìm", "tôi xem", "mình xem", "tôi bắt đầu", "mình bắt đầu",
	// Missing intent shapes observed in production stalls: “Mình cần
	// xem…”, “Giờ mình viết…”, “Tôi cần đọc…” — pronoun + cần/phải, and
	// giờ/bây giờ openers, none of which matched the list above, so the
	// turn ended text-only with no stall retry.
	"mình cần", "tôi cần", "em cần", "mình phải", "tôi phải", "em phải",
	"giờ mình", "giờ tôi", "bây giờ mình", "bây giờ tôi", "bây giờ",
	"trước khi", // bridge.log 17:42 stall: preamble “Trước khi khuyến nghị…, tôi xác minh…” — appears in the first bytes so the streaming gate holds the preamble before the later intent phrase completes
	// Chinese (GLM 5.3 is a Chinese-first model)
	"我会", "让我", "接下来", "现在我", "我需要",
	"我将", "我来", "我先", "现在来", "我得", "我要",
}

// contentHasAnnouncementPhrase reports whether content contains any intent
// phrase that could make it an "announcement" stall at stream end.
func contentHasAnnouncementPhrase(content string) bool {
	lower := strings.ToLower(content)
	for _, p := range announcementPhrases {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// offerPhrases are user-directed offers, greetings, and requests for
// input that happen to CONTAIN intent vocabulary ("let me know", "mình có
// thể giúp") but are legitimate final answers awaiting the user's reply —
// not self-announcements. Production duplicate bug (2026-09-09,
// fclaude-kimi-k3 session): GLM-5-Turbo put a whole greeting inside
// <details>, the reasoning fallback matched "let me ", the nudge retry
// re-generated the answer, and the client rendered two concatenated
// greetings (proxy log: two "stall detected ... postToolResult=false").
var offerPhrases = []string{
	// English — "let me know" is an offer for input, unlike "let me check".
	"let me know", "let us know",
	"happy to help", "here to help", "glad to help",
	"how can i help", "what can i do", "what would you like",
	"would you like", "do you want", "please tell me", "tell me what",
	// Vietnamese — ability offers "Mình có thể giúp bạn..." and
	// "Bạn cần mình giúp gì?"; agent-self need "Mình cần xem..." stays a stall.
	"mình có thể giúp", "tôi có thể giúp", "em có thể giúp",
	"mình có thể hỗ trợ", "tôi có thể hỗ trợ", "em có thể hỗ trợ",
	"bạn cần", "bạn muốn", "cho mình biết", "cho tôi biết", "hãy cho mình biết",
	"cần mình giúp", "mình giúp gì",
	// Chinese
	"需要我帮", "有什么可以帮", "请告诉我",
}

// contentHasOfferPhrase reports whether text contains an offer phrase that
// exempts it from stall detection: it addresses the USER (an answer awaiting
// their reply) rather than announcing the model's own next action.
func contentHasOfferPhrase(s string) bool {
	lower := strings.ToLower(s)
	for _, p := range offerPhrases {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// phraseGuardBytes is the streaming stall buffer's commit guard: a content
// prefix shorter than this could still GROW an intent phrase at its tail
// (longest phrase is ~14 bytes; UTF-8 needs headroom), so it stays held.
// Once accumulated content passes the guard with NO phrase, it can never
// trip the detector on its existing text and is committed to live
// streaming — the stall retry is then suppressed for the request (SSE
// cannot un-send already-streamed text).
const phraseGuardBytes = 32

// contentNeedsStallHold reports whether the accumulated content could still
// become an announcement stall: it already contains an intent phrase, or it
// is short enough that one could complete at the tail. Mirrors the detector
// exactly — content without a phrase can never trip it.
func contentNeedsStallHold(content string) bool {
	if len(content) < phraseGuardBytes {
		return true
	}
	// Mirror looksLikeAnnouncementOnly's offer exemption: offer text can
	// never trip the detector, so releasing it live locks the retry out and
	// streams immediately (TTFT) — a stall on such a turn would duplicate
	// text the client already has.
	return contentHasAnnouncementPhrase(content) && !contentHasOfferPhrase(content)
}

// agentStallNudge returns the nudge appended to the prompt on stall.
func agentStallNudge() string {
	open := "<<<" + "TOOL_CALL" + ">>>"
	close := "<<<" + "END_TOOL_CALL" + ">>>"
	return "\n\n<system_nudge>\n" +
		"Your previous turn announced an action but emitted NO tool call block. The task is NOT done.\n" +
		"Emit the " + open + "{...}" + close + " block for the next step RIGHT NOW, or produce the FINAL ANSWER if results already satisfy the request.\n" +
		"Do not describe intent — execute it. Do not repeat a tool call that already has a result in <recent>.\n" +
		"The marker vocabulary (" + open + ", " + close + ", <tool_call>, </tool_call>, <tool_result>) belongs to this runtime ONLY. Never echo, quote, or role-play it as transcript text (\"<assistant>\", \"<tool_result>\" lines) — write plain prose, or a REAL " + open + " block.\n" +
		"A tool-call block must carry a NON-EMPTY tool name: \"name\":\"\" is invalid — if unsure which tool fits, infer it from the arguments or reply in prose.\n" +
		"</system_nudge>"
}

// appendNudgeToClientMessages appends the stall nudge to the message payload
// the upstream actually reads. sendToQwenStream builds the request body from
// ClientMessagesRaw (the agent-folded messages array) — signature_prompt is
// only signed, never shown to the model — so the stall retry must splice the
// nudge in here or the retried request is byte-identical to the stalled one.
// The nudge is appended to the LAST message's text content (string or
// content-array form) so the folded agent prompt stays one user message; a
// separate new message would break the single-message contract the shim's
// prompt builder (buildAgentPrompt) established. On any parse failure the
// input is returned unchanged — the retry still re-sends, just unnudged,
// which is exactly the old (broken but harmless) behavior.
func appendNudgeToClientMessages(raw json.RawMessage, nudge string) json.RawMessage {
	if len(raw) == 0 || nudge == "" {
		return raw
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(raw, &msgs); err != nil || len(msgs) == 0 {
		return raw
	}
	last := msgs[len(msgs)-1]
	var m struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(last, &m); err != nil {
		return raw
	}
	content := m.Content
	if m.Role == "" {
		m.Role = "user"
	}
	switch {
	case len(content) == 0:
		// No content field — replace with a string carrying the nudge.
		content = json.RawMessage(strconv.Quote(nudge))
	default:
		var s string
		if err := json.Unmarshal(content, &s); err == nil {
			content = json.RawMessage(strconv.Quote(s + nudge))
			break
		}
		// Content-array (OpenAI multi-part form): append a text part.
		var arr []json.RawMessage
		if err := json.Unmarshal(content, &arr); err != nil {
			return raw
		}
		part, err := json.Marshal(map[string]string{"type": "text", "text": nudge})
		if err != nil {
			return raw
		}
		arr = append(arr, json.RawMessage(part))
		content, err = json.Marshal(arr)
		if err != nil {
			return raw
		}
	}
	newLast, err := json.Marshal(map[string]json.RawMessage{
		"role":    json.RawMessage(strconv.Quote(m.Role)),
		"content": content,
	})
	if err != nil {
		return raw
	}
	msgs[len(msgs)-1] = newLast
	out, err := json.Marshal(msgs)
	if err != nil {
		return raw
	}
	return out
}

// renderRecentConversation renders recent messages with tool exchanges grouped.
// Tool calls and their results are wrapped in <tool_exchange> tags so the
// model can clearly see the call→result pairing.
func renderRecentConversation(b *strings.Builder, messages []agentMessage) {
	i := 0
	for i < len(messages) {
		m := messages[i]

		// Skip the last user message — it goes in <current_task>.
		isLastUser := false
		if m.Role == "user" {
			isLastUser = true
			for j := i + 1; j < len(messages); j++ {
				if messages[j].Role == "user" {
					isLastUser = false
					break
				}
			}
		}

		if isLastUser {
			i++
			continue
		}

		// Group assistant tool-calls with following tool results.
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			b.WriteString("<tool_exchange>\n")
			// Render assistant's tool calls.
			b.WriteString(renderAssistantTurn(m))
			b.WriteString("\n")
			i++
			// Render tool results.
			for i < len(messages) && messages[i].Role == "tool" {
				b.WriteString(renderToolResult(messages[i]))
				b.WriteString("\n")
				i++
			}
			b.WriteString("</tool_exchange>\n")
			continue
		}

		// Regular message.
		if rendered := renderAgentMessage(m); rendered != "" {
			b.WriteString(rendered)
			b.WriteString("\n")
		}
		i++
	}
}

// agentAlreadyCalledList renders the <already_called> section: a deduplicated,
// order-preserving list of every tool call the model already issued in this
// conversation. Anti-loop anchor (issue #31): long agent sessions drift into
// re-issuing identical calls (or endless thinking) because the flat replay
// never tells the model what is already done; an explicit, compact list plus
// the no-repeat rules above gives it a hard, current state to check against.
// Returns "" when no calls have been made yet (section omitted).
func agentAlreadyCalledList(messages []agentMessage) string {
	var lines []string
	seen := make(map[string]bool)
	for _, m := range messages {
		if m.Role != "assistant" {
			continue
		}
		for _, call := range m.ToolCalls {
			key := call.Function.Name + "\n" + agentParseArguments(call.Function.Arguments)
			if seen[key] {
				continue
			}
			seen[key] = true
			lines = append(lines, fmt.Sprintf("- %s %s", call.Function.Name, agentParseArguments(call.Function.Arguments)))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<already_called>\n")
	b.WriteString("Calls already made in this conversation (do NOT re-issue any of them):\n")
	for _, line := range lines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("</already_called>")
	return b.String()
}

// wrapAgentPromptAsMessages wraps the built agent prompt as a Qwen messages
// array containing a single user message. Qwen's /api/v2/chat/completions
// only accepts role="user", and the modern shim folds the entire
// conversation into one prompt, so one user message carries it all.
func wrapAgentPromptAsMessages(prompt string) ([]byte, error) {
	return json.Marshal([]map[string]interface{}{
		{"role": "user", "content": prompt},
	})
}

// ── response parsing ─────────────────────────────────────────────────────────

var (
	agentFenceLead = regexp.MustCompile(`(?i)^` + "```" + `(?:json)?\s*`)
	agentFenceTail = regexp.MustCompile(`(?i)\s*` + "```" + `$`)
)

// ── fence tolerance ──────────────────────────────────────────────────────────
// Models often wrap tool-call blocks in ```json … ``` fences even when told
// not to. These helpers strip fence lines sitting DIRECTLY against the
// markers (never ordinary code blocks elsewhere in the answer).

// Tolerant bracket runs around both markers, e.g. "<<TOOL_CALL>>>" or
// "<<<END_TOOL_CALL>>>" as well as the canonical spellings.
const agentMarkerPat = "(?:<{0,6})TOOL_CALL(?:>{1,6})"
const agentEndMarkerPat = "(?:<{0,6})END_TOOL_CALL(?:>{1,6})"

var (
	// fence line immediately before a tool-call opening marker
	agentFenceBeforeCallRe = regexp.MustCompile("(?:\\A|\r?\n)[ \t]*```(?:json)?[ \t]*\r?\n(" + agentMarkerPat + ")")
	// fence line right after a tool-call closing marker (keeps the newline that follows)
	agentFenceAfterEndRe = regexp.MustCompile("(" + agentEndMarkerPat + ")[ \t]*\r?\n[ \t]*```(?:json)?[ \t]*(?:(\r?\n)|\\z)")
	// bare fence line hanging at the very end of a streamed content piece
	agentTrailFenceRe = regexp.MustCompile("(?:\\A|\r?\n)[ \t]*```(?:json)?[ \t]*(?:\r?\n)?\\z")
)

const agentFenceJSON = "```json"

// agentStreamKeep is the minimum number of trailing bytes the streaming
// interceptor keeps un-flushed while no marker has matched: enough to cover
// a fence line plus a partially received marker at its worst tolerated
// spelling, so neither can ever leak as content. The actual cut is pulled
// back to a rune boundary, so up to 3 extra bytes may be held.
const agentStreamKeep = agentWorstMarkerLen + len("```json\n") + 5

// announcementMaxLen bounds how long a body may be and still count as a
// stall "announcement" in looksLikeAnnouncementOnly. Shared by the
// streaming stall buffer gate in handlers.go: past this bound the stall
// detector can never fire, so content streams directly instead of being
// buffered until stream end (TTFT).
const announcementMaxLen = 3000

// NormalizeAgentFences removes fence lines adjacent to tool-call markers from
// finished text (non-streaming path).
func NormalizeAgentFences(text string) string {
	for {
		t := agentFenceAfterEndRe.ReplaceAllString(text, "${1}${2}")
		t = agentFenceBeforeCallRe.ReplaceAllString(t, "$1")
		if t == text {
			return t
		}
		text = t
	}
}

// TrimTrailingAgentFence drops one fence line hanging at the end of s
// (the fence the model placed immediately before <<<TOOL_CALL>>>).
func TrimTrailingAgentFence(s string) string {
	return agentTrailFenceRe.ReplaceAllString(s, "")
}

// agentPossibleFencePrefix reports whether s is empty or could still grow
// into a bare ``` / ```json fence line — i.e. it's too early to treat the
// bytes after a tool-call block as ordinary content.
func agentPossibleFencePrefix(s string) bool {
	if s == "" {
		return true // can't judge yet; wait for more chunks
	}
	for k := 1; k <= len(s) && k <= len(agentFenceJSON)+1; k++ {
		if strings.HasPrefix("```json\n", s[:k]) || strings.HasPrefix("```\n", s[:k]) {
			return true
		}
	}
	return false
}

// SkipLeadingAgentFence returns the length of a bare fence line at the start
// of s (the ``` the model places immediately after <<<END_TOOL_CALL>>>), or 0
// if s does not begin with one.
func SkipLeadingAgentFence(s string) int {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	if !strings.HasPrefix(s[i:], "```") {
		return 0
	}
	j := i + 3
	if strings.HasPrefix(s[j:], "json") {
		j += len("json")
	}
	for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
		j++
	}
	if j < len(s) && s[j] != '\n' && s[j] != '\r' {
		return 0 // not a bare fence line (e.g. an ordinary ```bash block)
	}
	if j < len(s) { // consume one line terminator
		if s[j] == '\r' {
			j++
		}
		if j < len(s) && s[j] == '\n' {
			j++
		}
	}
	return j
}

// ── payload tolerance ────────────────────────────────────────────────────────
// The contract asks the model for {"name": "<tool>", "arguments": {...}}, but
// models observed in the wild invent their own payload shapes when the schema
// is under-specified — most commonly the FLAT form {"tool": "bash",
// "command": "...", "timeout": 10} where the tool name sits under "tool" and
// the parameters are the remaining top-level keys. A strict {name, arguments}
// unmarshal accepts such objects with Name == "", so the whole block leaks to
// the client as plain content with finish_reason "stop" and the tool is never
// executed. We therefore accept every shape that unambiguously names a tool
// and its parameters.

// agentNameKeys are accepted spellings of the "which tool" key, in priority
// order. Explicit tool-* keys outrank "name": in a flat payload a "name"
// entry is more likely a tool PARAMETER named "name" than the tool itself,
// while a "tool" entry is never a canonical-shape artifact.
var agentNameKeys = []string{"tool", "tool_name", "function", "function_name", "name"}

// agentArgKeys are accepted spellings of the explicit "parameters" key.
var agentArgKeys = []string{"arguments", "parameters", "args", "params", "input"}

// agentExtractCall resolves (name, arguments) from one decoded tool-call
// payload object, accepting the canonical shape, alternate key spellings,
// and flat payloads where the parameters are the remaining top-level keys.
func agentExtractCall(obj map[string]json.RawMessage, schemas []agentToolSchema) (name string, args json.RawMessage, ok bool) {
	// Locate the tool name under any accepted key spelling.
	nameKey := ""
	for _, k := range agentNameKeys {
		raw, present := obj[k]
		if !present {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil && strings.TrimSpace(s) != "" {
			name, nameKey = strings.TrimSpace(s), k
			break
		}
	}
	if nameKey == "" {
		// Args-only payload (no name key under any spelling): the arguments
		// still identify the tool via its parameter fingerprint against the
		// request's declared schemas. Emit the args with the inferred name so
		// the call executes instead of leaking the whole block as raw text
		// (production: error_toolcall_log.md 2026-09-06).
		for _, k := range agentArgKeys {
			if raw, present := obj[k]; present && !isJSONNull(raw) {
				if n := agentInferToolName(raw, schemas); n != "" {
					return n, raw, true
				}
				return "", nil, false // args without a name we cannot infer
			}
		}
		return "", nil, false
	}

	// An explicit arguments key wins over the flat fallback.
	for _, k := range agentArgKeys {
		if raw, present := obj[k]; present {
			if isJSONNull(raw) {
				// Key present but null — the model explicitly passed null
				// args, not a flat payload. Return empty args, not the
				// whole object as a flat parameter.
				return name, json.RawMessage("{}"), true
			}
			return name, raw, true
		}
	}

	// Flat payload: every remaining top-level key is a parameter.
	rest := make(map[string]json.RawMessage, len(obj)-1)
	for k, v := range obj {
		if k != nameKey {
			rest[k] = v
		}
	}
	if len(rest) == 0 {
		return name, json.RawMessage("{}"), true
	}
	marshaled, err := json.Marshal(rest)
	if err != nil {
		return name, json.RawMessage("{}"), true
	}
	return name, marshaled, true
}

// isJSONNull reports whether raw is whitespace, JSON null, or empty.
func isJSONNull(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || bytes.Equal(t, []byte("null"))
}

// agentRepairJSON repairs the invalid JSON models most commonly emit inside
// tool-call markers:
//
//   - raw control characters inside string literals — a multi-line shell
//     "command" written with literal newlines instead of \n escapes,
//   - invalid escape sequences inside string literals — grep regexes written
//     verbatim (e.g. \| or \d); JSON only allows \" \\ \/ \b \f \n \r
//     \t \uXXXX,
//   - unescaped interior quotes inside string literals — the model writes
//     grep -rn "PATTERN" ... with bare double quotes (see
//     agentQuoteEndsString for the disambiguation heuristic).
//
// The scan only rewrites bytes inside string literals; everything outside
// (including pretty-printed layout whitespace) passes through unchanged, and
// s is returned verbatim when nothing needed repairing.
func agentRepairJSON(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inString := false
	escaped := false
	changed := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			b.WriteByte(c)
			continue
		}
		if escaped {
			b.WriteByte(c)
			escaped = false
			continue
		}
		switch c {
		case '\\':
			if agentValidJSONEscapeAt(s, i) {
				escaped = true
				b.WriteByte(c)
			} else {
				b.WriteString("\\\\")
				changed = true
			}
		case '"':
			// A bare quote only closes the string when JSON could
			// structurally continue right after it; otherwise it is an
			// interior quote the model forgot to escape.
			if agentQuoteEndsString(s, i) {
				inString = false
				b.WriteByte(c)
			} else {
				b.WriteString("\\\"")
				changed = true
			}
		case '\n':
			b.WriteString("\\n")
			changed = true
		case '\r':
			b.WriteString("\\r")
			changed = true
		case '\t':
			b.WriteString("\\t")
			changed = true
		default:
			if c < 0x20 {
				fmt.Fprintf(&b, "\\u%04x", c)
				changed = true
			} else {
				b.WriteByte(c)
			}
		}
	}
	if !changed {
		return s
	}
	return b.String()
}

// agentValidJSONEscapeAt reports whether the backslash at s[i] starts a valid
// JSON escape sequence (\" \\ \/ \b \f \n \r \t \uXXXX).
func agentValidJSONEscapeAt(s string, i int) bool {
	if i+1 >= len(s) {
		return false
	}
	switch s[i+1] {
	case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
		return true
	case 'u':
		if i+5 >= len(s) {
			return false
		}
		for k := i + 2; k <= i+5; k++ {
			c := s[k]
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
		return true
	}
	return false
}

// agentQuoteEndsString reports whether the quote at s[i] can close the
// current string literal: JSON is only continuable when the next
// non-whitespace character is one of , : } ] (or the input ends). Any other
// continuation means the quote sits INSIDE the string value — the classic
// shape is a shell command containing grep -rn "PATTERN" ... — so the caller
// escapes it instead of closing.
func agentQuoteEndsString(s string, i int) bool {
	j := i + 1
	for j < len(s) && isASCIISpace(s[j]) {
		j++
	}
	if j >= len(s) {
		return true
	}
	switch s[j] {
	case ',', ':', '}', ']':
		return true
	}
	return false
}

// agentStripTrailingCommas drops commas followed only by whitespace and a
// closing '}' or ']' outside string literals — another frequent model
// deviation that strict JSON rejects. s is returned verbatim when there is
// nothing to strip.
func agentStripTrailingCommas(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inString := false
	escaped := false
	changed := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			b.WriteByte(c)
			continue
		}
		if c == '"' {
			inString = true
			b.WriteByte(c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(s) && isASCIISpace(s[j]) {
				j++
			}
			if j < len(s) && (s[j] == '}' || s[j] == ']') {
				changed = true
				continue // drop the trailing comma
			}
		}
		b.WriteByte(c)
	}
	if !changed {
		return s
	}
	return b.String()
}

// agentCloseUnbalancedBraces repairs the truncated-JSON class the streaming
// scanner cannot recover from on its own: the model writes a structurally
// complete payload but drops one or more closing braces/brackets before the
// end marker (observed: ..."timeout_ms":120000} + <<<END_TOOL_CALL>>> with
// the outer object never closed). Scan once tracking string/escape state and
// the open-bracket stack, then append exactly the missing closers in reverse
// order. A trailing comma or colon before the cut is dropped so the appended
// closer still yields valid JSON. Empty input returns as-is.
func agentCloseUnbalancedBraces(s string) string {
	if s == "" {
		return s
	}
	var stack []byte
	inString := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			stack = append(stack, c)
		case '}', ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	if len(stack) == 0 {
		return s
	}
	// Trim a trailing comma or colon (plus whitespace) so the appended
	// closers do not sit after a dangling separator.
	t := strings.TrimRight(s, " \t\r\n")
	for len(t) > 0 && (t[len(t)-1] == ',' || t[len(t)-1] == ':') {
		t = strings.TrimRight(t[:len(t)-1], " \t\r\n")
	}
	if t == "" {
		return s
	}
	var b strings.Builder
	b.WriteString(t)
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i] == '{' {
			b.WriteByte('}')
		} else {
			b.WriteByte(']')
		}
	}
	return b.String()
}

// agentLooseParse parses one tool-call body, tolerating markdown fences, the
// payload shape deviations listed at agentNameKeys / agentArgKeys, and — via
// staged repair retries — the invalid-JSON classes models actually emit (raw
// control characters, bad escapes and unescaped interior quotes inside
// strings, trailing commas, prose wrapped around the braces). A failed parse
// leaks the whole block to the client as visible text and the tool never
// executes, so repairing is worth the extra pass.
func agentLooseParse(body string, schemas []agentToolSchema) (name string, args json.RawMessage, ok bool) {
	raw := strings.TrimSpace(body)
	raw = agentFenceLead.ReplaceAllString(raw, "")
	raw = agentFenceTail.ReplaceAllString(raw, "")

	// Save original raw for key-value fallback (before JSON extraction).
	origRaw := raw

	// Collect EVERY top-level brace-balanced JSON object in the body, in
	// order. Models sometimes emit a malformed "double attempt": a tag-style
	// pseudo-call (<tool_call>list_directory{"path": ...}) followed by the
	// canonical payload, so the FIRST object may be an args-only fragment
	// while the real {"name","arguments"} payload sits right after it
	// (observed in production E2E logs — the whole block leaked as raw text
	// when only the first object was tried). Trying every object recovers
	// those blocks instead of leaking them. A brace-balanced scan also keeps
	// a stray '}' in trailing prose from truncating the extraction.
	var objects []string
	rest := raw
	for {
		l := strings.IndexByte(rest, '{')
		if l < 0 {
			break
		}
		r := agentMatchBrace(rest, l)
		if r < 0 {
			// Unterminated object: fall back to the last '}' if any.
			if r = strings.LastIndexByte(rest, '}'); r <= l {
				break
			}
		}
		objects = append(objects, rest[l:r+1])
		rest = rest[r+1:]
	}
	if len(objects) == 0 {
		objects = []string{raw}
	}

	// Strict parse first, then progressively repaired retries, for every
	// candidate object (deduplicated, order preserved).
	var candidates []string
	seen := map[string]bool{}
	addCand := func(c string) {
		if !seen[c] {
			seen[c] = true
			candidates = append(candidates, c)
		}
	}
	for _, obj := range objects {
		repaired := agentRepairJSON(obj)
		stripped := agentStripTrailingCommas(repaired)
		addCand(obj)
		addCand(repaired)
		addCand(stripped)
		// Truncated tail: the model dropped closing braces before the end
		// marker (observed in production: ..."timeout_ms":120000} with the
		// outer object never closed). The brace-balanced fallback cut already
		// captured the payload; close what is still open so json.Unmarshal
		// accepts it instead of leaking the whole block as visible text.
		addCand(agentCloseUnbalancedBraces(stripped))
	}
	// Named payloads win regardless of position: a leading args-only
	// fragment (a tag-style pseudo-attempt) must not shadow an explicit
	// named payload later in the same body via schema inference. Pass 0
	// considers objects carrying a name key; pass 1 the args-only rest.
	for pass := 0; pass < 2; pass++ {
		for _, cand := range candidates {
			var obj map[string]json.RawMessage
			if err := json.Unmarshal([]byte(cand), &obj); err != nil || len(obj) == 0 {
				continue
			}
			named := false
			for _, k := range agentNameKeys {
				if _, ok := obj[k]; ok {
					named = true
					break
				}
			}
			if named != (pass == 0) {
				continue
			}
			if name, args, ok := agentExtractCall(obj, schemas); ok && name != "" {
				return name, args, true
			}
		}
	}
	// Third attempt: non-JSON key=value format used by some GLM models.
	// Pattern: tool_name key1="value1" key2="value2" ...
	// Also handles GLM 5.2 broken JSON: key": "value ...
	if name, args, ok := agentParseKeyValueArgs(origRaw); ok && name != "" {
		return name, args, true
	}
	return "", nil, false
}

// agentValidToolName reports whether s looks like a tool identifier
// (letters, digits, '_', '-', '.'). It rejects JSON fragments such as
// `{"command":` that the key-value fallback would otherwise accept as a
// tool name and forward as a garbage tool call.
func agentValidToolName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	c := s[0]
	if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_') {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_' || c == '-' || c == '.':
		default:
			return false
		}
	}
	return true
}

// agentMatchBrace returns the index of the '}' that closes the '{' at s[open],
// respecting string literals and nested objects/arrays. Returns -1 if no
// matching brace is found. This avoids the first-'{' / last-'}' heuristic
// being fooled by a stray '}' in trailing prose.
func agentMatchBrace(s string, open int) int {
	depth := 0
	inStr := false
	escaped := false
	for i := open; i < len(s); i++ {
		c := s[i]
		if inStr {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// agentParseKeyValueArgs handles the non-JSON format some models emit:
//
//	read_file path="README.md" start_line="1" end_line="200"
//
// The first token is the tool name; subsequent tokens are key="value" pairs.
// Also handles GLM 5.2 broken JSON format:
//
//	edit_file path": "kimi_chat.py
//	edits": [{"old_text": "...", "new_text": "..."}]
func agentParseKeyValueArgs(raw string) (name string, args json.RawMessage, ok bool) {
	// Split first line to extract tool name.
	lines := strings.SplitN(raw, "\n", 2)
	firstLine := strings.TrimSpace(lines[0])
	toolParts := strings.Fields(firstLine)
	if len(toolParts) == 0 {
		return "", nil, false
	}
	toolName := toolParts[0]
	if !agentValidToolName(toolName) {
		return "", nil, false
	}
	// Build rest body: rest of first line + subsequent lines.
	var restBody string
	if len(toolParts) > 1 {
		restBody = strings.Join(toolParts[1:], " ")
	}
	if len(lines) > 1 {
		if restBody != "" {
			restBody += "\n"
		}
		restBody += strings.Join(lines[1:], "\n")
	}
	if restBody == "" {
		return toolName, json.RawMessage("{}"), true
	}
	// Detect format: GLM 5.2 broken JSON (key": "value per-line)
	// vs standard key="value" tokens.
	isColonFormat := strings.Contains(restBody, ": \"") || strings.Contains(restBody, ": ")
	isEqualsFormat := strings.Contains(restBody, "=")
	if isColonFormat && !isEqualsFormat {
		return agentParseColonKeyValueArgs(toolName, restBody)
	}
	// Standard key="value" token format.
	return agentParseEqualsKeyValueArgs(toolName, restBody)
}

// agentParseEqualsKeyValueArgs handles the standard format:
//
//	tool_name key1="value1" key2="value2" ...
func agentParseEqualsKeyValueArgs(toolName, body string) (name string, args json.RawMessage, ok bool) {
	tokens := agentSplitQuotedTokens(body)
	argsMap := make(map[string]interface{})
	for _, tok := range tokens {
		eqIdx := strings.IndexByte(tok, '=')
		if eqIdx < 0 {
			continue
		}
		key := tok[:eqIdx]
		val := tok[eqIdx+1:]
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = val[1 : len(val)-1]
		}
		argsMap[key] = agentCoerceScalar(val)
	}
	if len(argsMap) == 0 {
		return toolName, json.RawMessage("{}"), true
	}
	marshaled, err := json.Marshal(argsMap)
	if err != nil {
		return toolName, json.RawMessage("{}"), true
	}
	return toolName, marshaled, true
}

// agentParseColonKeyValueArgs handles the GLM 5.2 broken JSON format:
//
//	edit_file path": "kimi_chat.py
//	edits": [{"old_text": "..."}]
//
// Values may not have closing quotes (broken JSON) — read per-line.
func agentParseColonKeyValueArgs(toolName, body string) (name string, args json.RawMessage, ok bool) {
	argsMap := make(map[string]interface{})
	lines := strings.Split(body, "\n")
	i := 0
	for i < len(lines) {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			i++
			continue
		}
		colonIdx := strings.Index(line, ": \"")
		wasQuoted := colonIdx >= 0
		valOff := colonIdx + 3
		if colonIdx < 0 {
			colonIdx = strings.Index(line, ": ")
			valOff = colonIdx + 2
		}
		if colonIdx < 0 {
			i++
			continue
		}
		key := strings.TrimSpace(line[:colonIdx])
		key = strings.Trim(key, `"'`)
		if key == "" {
			i++
			continue
		}
		val := strings.TrimSpace(line[valOff:])

		// If the value starts a JSON array or object, it may span multiple
		// lines — collect until braces are balanced so multi-line values
		// (common with edits/old_text) are not truncated.
		if len(val) > 0 && (val[0] == '[' || val[0] == '{') {
			collected := val
			for agentBraceDepth(collected) > 0 && i+1 < len(lines) {
				i++
				collected += "\n" + lines[i]
			}
			var parsed interface{}
			if err := json.Unmarshal([]byte(collected), &parsed); err == nil {
				argsMap[key] = parsed
				i++
				continue
			}
			val = collected
		}

		// Multi-line quoted string: when the value was introduced by `: "`,
		// the opening quote was already consumed by valOff. If no closing
		// quote exists on the same line, the string continues on subsequent
		// lines — collect until an unescaped closing quote is found. This
		// handles GLM 5.2/5.3 emitting broken JSON where old_text/new_text
		// span multiple literal lines inside the tool-call block.
		if wasQuoted && !agentHasClosingQuote(val) {
			collected := val
			for i+1 < len(lines) {
				i++
				collected += "\n" + lines[i]
				if agentHasClosingQuote(collected) {
					break
				}
			}
			val = collected
		}

		// Strip surrounding quotes if present. Broken JSON often leaves an
		// opening quote without a closing one - drop that stray quote too.
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = val[1 : len(val)-1]
		} else if len(val) >= 1 && val[0] == '"' {
			val = val[1:]
		}
		argsMap[key] = agentCoerceScalar(val)
		i++
	}
	if len(argsMap) == 0 {
		return toolName, json.RawMessage("{}"), true
	}
	marshaled, err := json.Marshal(argsMap)
	if err != nil {
		return toolName, json.RawMessage("{}"), true
	}
	return toolName, marshaled, true
}

// agentHasClosingQuote reports whether s contains an unescaped closing
// double-quote. It scans from the start, respecting backslash escapes.
func agentHasClosingQuote(s string) bool {
	for i := 1; i < len(s); i++ {
		if s[i] == '\\' {
			i++ // skip escaped character
			continue
		}
		if s[i] == '"' {
			return true
		}
	}
	return false
}

// agentBraceDepth returns the net open-brace/bracket count of s, respecting
// string literals and escape sequences. A positive result means more opens
// than closes — the caller should read more input before the value is complete.
func agentBraceDepth(s string) int {
	depth := 0
	inStr := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		}
	}
	return depth
}

// agentCoerceScalar attempts to interpret s as an int64, float64, or bool,
// falling back to the raw string. It replaces the hand-rolled fmt_ScanInt
// which could silently overflow and only handled integers.
func agentCoerceScalar(s string) interface{} {
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		return v
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return v
	}
	switch strings.ToLower(s) {
	case "true":
		return true
	case "false":
		return false
	}
	return s
}

// (fmt_ScanInt removed — replaced by agentCoerceScalar using strconv.)

// agentSplitQuotedTokens splits a string by whitespace, respecting double-quoted
// values (so key="hello world" is one token).
func agentSplitQuotedTokens(s string) []string {
	var tokens []string
	i := 0
	for i < len(s) {
		// Skip whitespace.
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
			i++
		}
		if i >= len(s) {
			break
		}
		// Read one token.
		start := i
		if s[i] == '"' {
			// Token starts with a quote: read until unescaped closing quote.
			i++
			for i < len(s) {
				if s[i] == '\\' && i+1 < len(s) {
					i += 2
					continue
				}
				if s[i] == '"' {
					i++ // include closing quote
					break
				}
				i++
			}
		} else {
			// Read an unquoted segment. If we hit a '=' followed by a '"',
			// consume the quoted value as part of this token (key="value").
			for i < len(s) && s[i] != ' ' && s[i] != '\t' && s[i] != '\n' && s[i] != '\r' {
				if s[i] == '"' && i > start && s[i-1] == '=' {
					// Quoted value after '=': read until unescaped closing quote.
					i++
					for i < len(s) {
						if s[i] == '\\' && i+1 < len(s) {
							i += 2
							continue
						}
						if s[i] == '"' {
							i++ // include closing quote
							break
						}
						i++
					}
				} else {
					i++
				}
			}
		}
		tokens = append(tokens, s[start:i])
	}
	return tokens
}

// agentParseArguments normalizes model-provided arguments to compact JSON
// text (objects pass through, JSON-encoded strings are parsed, unparsable
// strings stay quoted).
func agentParseArguments(raw json.RawMessage) string {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return "{}"
	}
	if t[0] == '"' {
		var s string
		if err := json.Unmarshal(t, &s); err == nil {
			var c bytes.Buffer
			if json.Compact(&c, []byte(strings.TrimSpace(s))) == nil && json.Valid(c.Bytes()) {
				return c.String()
			}
			quoted, _ := json.Marshal(s)
			return string(quoted)
		}
	}
	var c bytes.Buffer
	if json.Compact(&c, t) == nil {
		return c.String()
	}
	return "{}"
}

// agentStreamArguments mirrors the stream path: non-string values are
// compacted, string values are used verbatim.
func agentStreamArguments(raw json.RawMessage) string {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return "{}"
	}
	if t[0] == '"' {
		var s string
		if err := json.Unmarshal(t, &s); err == nil {
			return s
		}
	}
	var c bytes.Buffer
	if json.Compact(&c, t) == nil {
		return c.String()
	}
	return "{}"
}

// agentRandomHex returns n random bytes as lowercase hex (call-id suffixes).
func agentRandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// ParseAgentToolCalls extracts every complete tool-call block from finished
// text and returns OpenAI-format tool_calls objects.
func ParseAgentToolCalls(text string) []map[string]interface{} {
	return ParseAgentToolCallsWithTools(text, nil)
}

// ParseAgentToolCallsWithTools additionally resolves the request's declared
// tool schemas, enabling name inference for args-only payloads (see
// agentInferToolName).
func ParseAgentToolCallsWithTools(text string, toolsRaw json.RawMessage) []map[string]interface{} {
	schemas := agentCollectToolSchemas(toolsRaw)
	text = NormalizeAgentFences(text)
	var calls []map[string]interface{}
	for _, span := range findAgentSpans(text) {
		name, args, ok := agentLooseParse(text[span.bodyStart:span.bodyEnd], schemas)
		if !ok || name == "" {
			// Unroutable blank-name payload (stall_evident.md 2026-09-07:
			// the live tool_call {"name":""} shape). Before discarding, snap
			// the arguments against the declared schemas: a unique parameter
			// fingerprint recovers the intended call (normally inference is
			// handled inside agentLooseParse, but a literal "name":"" key
			// makes it return the blank name). Without this the safety net
			// sees no call, so the turn cannot be retried and the block
			// leaks as content.
			if args != nil {
				if n := agentInferToolName(args, schemas); n != "" {
					calls = append(calls, map[string]interface{}{
						"id":   "call_" + agentRandomHex(12),
						"type": "function",
						"function": map[string]interface{}{
							"name":      n,
							"arguments": agentParseArguments(args),
						},
					})
					continue
				}
			}
			continue
		}
		calls = append(calls, map[string]interface{}{
			"id":   "call_" + agentRandomHex(12),
			"type": "function",
			"function": map[string]interface{}{
				"name":      name,
				"arguments": agentParseArguments(args),
			},
		})
	}
	if len(calls) == 0 {
		// Fallback pass for the XML-tag open form (below) — only when the
		// canonical pass found nothing, so a block that is canonical in one
		// place and tag-style in another never yields duplicate calls.
		calls = append(calls, parseXMLTagToolCalls(text)...)
	}
	if len(calls) == 0 {
		// Second fallback: END marker present but the open marker was lost
		// entirely (prose + JSON + <<<END_TOOL_CALL>>> with no opener) — same
		// dormancy rule, so it cannot double-recover a call found above.
		calls = append(calls, parseBareEndToolCalls(text)...)
	}
	return calls
}

// parseXMLTagToolCalls recovers tool calls from the XML-tag open form
// observed in production (bridge.log 2026-09-05 18:10, req=a1464731): the
// model names the tool in an XML-style tag instead of the canonical open
// marker and drops the JSON "name" key entirely:
//
//	<terminal>
//	{"arguments":{"cd":"...","command":"echo ..."}}
//	<<<END_TOOL_CALL>>>
//
// findAgentSpans only recognizes TOOL_CALL open markers, so such a block
// has no span and previously leaked whole as content — the agent stalled
// with the tool never executed. This pass pairs an XML-style <toolname>
// opener with a standard END marker sitting right after the JSON payload
// (that pairing is what separates a tool call from any other XML tag that
// happens to precede JSON in prose/reasoning).
func parseXMLTagToolCalls(text string) []map[string]interface{} {
	var calls []map[string]interface{}
	rest := text
	for {
		tagStart := strings.IndexByte(rest, '<')
		if tagStart < 0 {
			return calls
		}
		tagName, tagEnd := agentXMLTagAt(rest, tagStart)
		if tagName == "" {
			rest = rest[tagStart+1:]
			continue
		}
		if strings.EqualFold(tagName, agentStartWord) || strings.EqualFold(tagName, agentEndWord) {
			// Protocol words, not tool names: the model framed the call with
			// the marker vocabulary itself. Recovering it here would fabricate
			// a bogus tool named after the protocol; the canonical span pass
			// (plus schema inference) owns that shape.
			rest = rest[tagEnd:]
			continue
		}
		afterTag := rest[tagEnd:]
		ws := agentSkipWS(afterTag)
		if ws >= len(afterTag) || afterTag[ws] != '{' {
			// Not a tag-over-JSON shape: some prose follows the tag.
			rest = rest[tagEnd:]
			continue
		}
		jr := agentMatchBrace(afterTag, ws)
		if jr < 0 {
			if last := strings.LastIndexByte(afterTag, '}'); last > ws {
				jr = last
			} else {
				rest = rest[tagEnd:]
				continue
			}
		}
		tail := afterTag[jr+1:]
		tws := agentSkipWS(tail)
		if tws < len(tail) {
			if e, elen := findAgentEndMarkerInBuffer(tail[tws:], true); e == 0 && elen > 0 {
				if call, ok := agentBuildXMLTagCall(tagName, afterTag[ws:jr+1]); ok {
					calls = append(calls, call)
				}
				rest = tail[tws+elen:]
				continue
			}
		}
		// Payload parsed but no END marker right after: not a tool call.
		rest = afterTag[jr+1:]
	}
}

// parseBareEndToolCalls recovers tool calls from blocks that kept the END
// marker but lost the open marker entirely: the model emitted prose
// (optionally ending in the tool name), a JSON payload, then
// <<<END_TOOL_CALL>>> with no opening marker anywhere.
//
// findAgentSpans requires an open marker, so such blocks previously yielded
// no call and the agent stalled waiting for a tool that never ran. This pass
// pairs the JSON object immediately preceding an END marker with a tool
// name taken from the JSON body (any accepted spelling) or, failing that,
// the trailing word of the prose right before the JSON. Like the XML-tag
// pass it only runs when the canonical pass found nothing, so it can never
// duplicate a call recovered above.
func parseBareEndToolCalls(text string) []map[string]interface{} {
	var calls []map[string]interface{}
	rest := text
	for {
		e, elen := findAgentMarker(rest, agentEndWord, true)
		if e < 0 {
			return calls
		}
		before := strings.TrimRight(rest[:e], " \t\r\n")
		if strings.HasSuffix(before, "}") {
			if bo := agentMatchBraceBack(before, len(before)-1); bo >= 0 {
				body := before[bo:]
				hint := agentTrailingToolWord(before[:bo])
				if call, ok := agentBuildXMLTagCall(hint, body); ok {
					if fn, _ := call["function"].(map[string]interface{}); fn != nil {
						if n, _ := fn["name"].(string); n != "" {
							calls = append(calls, call)
						}
					}
				}
			}
		}
		rest = rest[e+elen:]
	}
}

// agentMatchBraceBack finds the index of the '{' that balances the '}' at
// s[close], scanning left with string-literal awareness. Right-to-left
// escape handling is approximate; malformed bodies still pass through
// agentBuildXMLTagCall's repair pipeline, so a -1 here only skips recovery.
func agentMatchBraceBack(s string, close int) int {
	depth := 0
	inStr := false
	esc := false
	for i := close; i >= 0; i-- {
		c := s[i]
		if esc {
			esc = false
			continue
		}
		if inStr {
			if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '}':
			depth++
		case '{':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// agentTrailingToolWord scans backwards from the end of s (which ends just
// before a JSON body) over whitespace, then over tool-name characters, and
// returns the word if it is a valid tool name — the "read_file" in
// "...HTTP server trong code.read_file\n". Empty string means no usable hint.
func agentTrailingToolWord(s string) string {
	i := len(s)
	for i > 0 {
		switch s[i-1] {
		case ' ', '\t', '\r', '\n':
			i--
		default:
			goto words
		}
	}
words:
	end := i
	for i > 0 {
		c := s[i-1]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			i--
			continue
		}
		break
	}
	word := s[i:end]
	if word == "" || !agentValidToolName(word) {
		return ""
	}
	return word
}

// agentCollectToolNames extracts the declared tool names from a request's
// `tools` array (OpenAI spelling, both wrapped and flat shapes).
func agentCollectToolNames(toolsRaw json.RawMessage) []string {
	if len(toolsRaw) == 0 {
		return nil
	}
	var tools []openAITool
	if json.Unmarshal(toolsRaw, &tools) != nil {
		return nil
	}
	names := make([]string, 0, len(tools))
	for i := range tools {
		if n := tools[i].fnName(); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// agentSnapToolName repairs a glued/corrupted tool name against the request's
// real tool list. GLM 5.2/5.3 production failures (log_stall.md 2026-09-06):
// "read_filearguments" (name glued with the next JSON key), "read_fileailing"
// (glued with trailing prose), "grep_arguments_placeholder", "code.read_file"
// (dotted prefix). Each is a REAL tool with junk appended — the client
// rejected them with "No tool named X exists" and the agent lost a turn.
// Repair rules, most specific first: exact match (case-insensitive) passes;
// a dotted tail segment that matches a known tool snaps to it; otherwise the
// LONGEST known tool that is a prefix of the name wins when the leftover is
// short glue (≤24 bytes). No match returns the name unchanged — the client
// error is then genuine (the model invented a tool).
func agentSnapToolName(name string, known []string) string {
	if name == "" || len(known) == 0 {
		return name
	}
	lower := strings.ToLower(name)
	for _, t := range known {
		if lower == strings.ToLower(t) {
			return t
		}
	}
	if i := strings.LastIndexByte(lower, '.'); i >= 0 && i+1 < len(lower) {
		seg := lower[i+1:]
		for _, t := range known {
			if seg == strings.ToLower(t) {
				return t
			}
		}
	}
	best := ""
	for _, t := range known {
		lt := strings.ToLower(t)
		if !strings.HasPrefix(lower, lt) {
			continue
		}
		if len(lower)-len(lt) > 24 {
			continue
		}
		if len(lt) > len(best) {
			best = t
		}
	}
	if best != "" {
		return best
	}
	return name
}

// agentToolSchema is one declared tool's name plus the parameter names its
// JSON-schema properties advertise. Used to infer a tool name from an
// args-only payload (see agentInferToolName).
type agentToolSchema struct {
	name  string
	props map[string]bool
}

// agentCollectToolSchemas extracts (name, parameter-name set) pairs from the
// request's `tools` array, both wrapped (function.parameters) and flat
// (parameters) spellings.
func agentCollectToolSchemas(toolsRaw json.RawMessage) []agentToolSchema {
	if len(toolsRaw) == 0 {
		return nil
	}
	var tools []openAITool
	if json.Unmarshal(toolsRaw, &tools) != nil {
		return nil
	}
	schemas := make([]agentToolSchema, 0, len(tools))
	for i := range tools {
		name := tools[i].fnName()
		if name == "" {
			continue
		}
		var spec struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		props := map[string]bool{}
		if raw := tools[i].fnParameters(); len(raw) > 0 && json.Unmarshal(raw, &spec) == nil {
			for k := range spec.Properties {
				props[k] = true
			}
		}
		schemas = append(schemas, agentToolSchema{name: name, props: props})
	}
	return schemas
}

// agentInferToolName names an args-only payload. GLM 5.3 production failure
// (error_toolcall_log.md 2026-09-06): the model wrote the arguments object
// and simply never emitted the "name" key —
//
//	<tool_call>{"arguments":{"command":...,"cd":...,...}}</arguments>
//
// The argument KEYS fingerprint the intended tool: score every declared
// tool by how many argument keys its schema advertises, and accept only a
// STRICTLY unique best match covering at least half the argument keys (and
// at least one). Ambiguous or unknown payloads return "" — a wrong guess
// executes the wrong tool, which is worse than dropping the call.
func agentInferToolName(args json.RawMessage, schemas []agentToolSchema) string {
	if len(args) == 0 || len(schemas) == 0 {
		return ""
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(args, &obj) != nil || len(obj) == 0 {
		return ""
	}
	best, bestScore, second := "", 0, 0
	for i := range schemas {
		if len(schemas[i].props) == 0 {
			continue
		}
		score := 0
		for k := range obj {
			if schemas[i].props[k] {
				score++
			}
		}
		if score > bestScore {
			best, bestScore, second = schemas[i].name, score, bestScore
		} else if score > second {
			second = score
		}
	}
	if best == "" || bestScore*2 < len(obj) || bestScore == second {
		return ""
	}
	return best
}

// agentXMLTagAt reports the tool-name tag opening at rest[i] (which must be
// '<'): returns ("", 0) unless rest[i:] starts with <name> where name passes
// agentValidToolName. Canonical markers (<<<TOOL_CALL>>>) never match — the
// byte after '<' is another '<', not a letter. Closing tags (</name>) are
// rejected too (leading '/').
func agentXMLTagAt(rest string, i int) (string, int) {
	if i+1 >= len(rest) {
		return "", 0
	}
	c := rest[i+1]
	if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_') {
		return "", 0
	}
	close := strings.IndexByte(rest[i+1:], '>')
	if close <= 0 {
		return "", 0
	}
	name := rest[i+1 : i+1+close]
	if !agentValidToolName(name) {
		return "", 0
	}
	return name, i + 1 + close + 1
}

func agentSkipWS(s string) int {
	i := 0
	for i < len(s) {
		switch s[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return i
		}
	}
	return i
}

// agentBuildXMLTagCall assembles one OpenAI tool_calls object from a tag
// name and the raw JSON body found under it. A JSON-declared name (any
// accepted spelling) wins over the tag; when the JSON drops the name (the
// observed production shape) the tag carries it, and arguments come from
// an explicit arg key or the flat body.
func agentBuildXMLTagCall(tagName, jsonBody string) (map[string]interface{}, bool) {
	name := tagName
	var args json.RawMessage
	candidates := []string{jsonBody}
	if repaired := agentCloseUnbalancedBraces(agentStripTrailingCommas(agentRepairJSON(jsonBody))); repaired != jsonBody {
		candidates = append(candidates, repaired)
	}
	resolved := false
	for _, cand := range candidates {
		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(cand), &obj) != nil || len(obj) == 0 {
			continue
		}
		if n, a, ok := agentExtractCall(obj, nil); ok && n != "" {
			name, args = n, a
			resolved = true
			break
		}
		for _, k := range agentArgKeys {
			if raw, present := obj[k]; present {
				if isJSONNull(raw) {
					args = json.RawMessage("{}")
				} else {
					args = raw
				}
				break
			}
		}
		if args == nil {
			if m, err := json.Marshal(obj); err == nil {
				args = json.RawMessage(m)
			}
		}
		resolved = true
		break
	}
	if !resolved {
		// Payload not parseable even after repair — use the repaired body
		// as-is: the client's tool runtime is in a better position to reject
		// it than we are to silently drop the call (dropping stalls the
		// agent, the exact failure this pass exists to prevent).
		if len(candidates) > 1 {
			args = json.RawMessage(candidates[1])
		} else {
			args = json.RawMessage(jsonBody)
		}
	}
	return map[string]interface{}{
		"id":   "call_" + agentRandomHex(12),
		"type": "function",
		"function": map[string]interface{}{
			"name":      name,
			"arguments": agentParseArguments(args),
		},
	}, true
}

// StripAgentToolCalls removes all tool-call blocks from finished text.
func StripAgentToolCalls(text string) string {
	text = NormalizeAgentFences(text)
	var kept strings.Builder
	prev := 0
	for _, span := range findAgentSpans(text) {
		kept.WriteString(text[prev:span.start])
		prev = span.end
	}
	kept.WriteString(text[prev:])
	return strings.TrimSpace(kept.String())
}

// ── streaming interceptor ────────────────────────────────────────────────────

// AgentStreamInterceptor incrementally separates ordinary text from tool-call
// blocks. It retains a short suffix so a marker split across upstream chunks
// is never leaked to the client.
//
// Tool calls STREAM like the OpenAI / Anthropic APIs: as soon as the block's
// {"name": ..., "arguments": { has arrived the interceptor emits the header
// delta (index + id + type + function.name), then forwards the arguments
// object byte-by-byte as id-less fragments while the model is still writing
// it — the client sees live tool-call SSE instead of a long silence followed
// by one buffered blob. Payload shapes that cannot be streamed safely
// (non-object arguments, flat payloads, a name that never resolves) fall
// back to the old behaviour: the whole block is buffered between the markers
// and emitted as one complete call.
type AgentStreamInterceptor struct {
	buffer       string
	offset       int
	callIndex    int
	pendingSep   bool   // a tool-call block just closed: watch for a stray fence
	pendingPiece string // content before the open marker, held until the block's fate is known

	// Declared tool schemas from the request — enable name inference for
	// args-only payloads (agentInferToolName) in the fallback parse paths.
	toolSchemas []agentToolSchema

	// ── incremental tool-call streaming state ──
	inCall         bool   // between the opening and closing markers
	tcBlockStart   int    // offset of the opening marker (for leak-as-content)
	tcName         string // resolved tool name, once complete
	tcNameFound    bool
	tcArgsFound    bool // the arguments value's opening '{' was located
	tcArgsPos      int  // absolute offset of that '{' in buffer
	tcArgsStreamed int  // bytes of the arguments object already emitted
	tcBraceDepth   int  // brace depth while scanning arguments
	tcInString     bool // inside a JSON string while scanning arguments
	tcEscapeNext   bool // next byte is escaped while scanning arguments
	tcArgsDone     bool // the arguments object's closing '}' was emitted
	tcFallback     bool // un-streamable shape: buffer the block, parse at the end
}

type AgentParsedChunk struct {
	Content   string
	ToolCalls []map[string]interface{}
}

// agentPseudoCallTail matches a tag-style pseudo tool call
// "<name>{json}</name>" anchored at the END of the text emitted
// immediately before a real tool-call block (see agentStripTrailingPseudoCall).
var agentPseudoCallTail = regexp.MustCompile(`(?s)\s*<([A-Za-z_][A-Za-z0-9_.-]*)>\s*(\{.*\})\s*(?:</([A-Za-z_][A-Za-z0-9_.-]*)>)?\s*$`)

// agentPseudoHoldMax bounds how many bytes of a potential pseudo-call tail
// the streaming interceptor will hold back. A real dropped-first-attempt
// pseudo-call is small; anything this large is ordinary text.
const agentPseudoHoldMax = 1 << 16

// agentStripTrailingPseudoCall removes a trailing tag-style pseudo tool call
// from the content emitted immediately before a REAL tool-call block that
// parsed successfully. GLM 5.2/5.3 sometimes emit both formats in one turn:
// a first discarded attempt "<list_directory>{\"path\":...}</list_directory>"
// followed by the canonical block (E2E log). The canonical block parsing OK
// proves the pseudo-call was a dropped first attempt, so strip it instead of
// leaking it as raw text. Conservative: the WHOLE tail must match, the tag
// must be a plausible tool name, open/close tags must agree, and the body
// must be valid JSON; anything else is left untouched.
func agentStripTrailingPseudoCall(s string) string {
	m := agentPseudoCallTail.FindStringSubmatch(s)
	if m == nil {
		return s
	}
	name, body, closeName := m[1], m[2], m[3]
	if (closeName != "" && name != closeName) || !agentValidToolName(name) || !json.Valid([]byte(body)) {
		return s
	}
	return s[:len(s)-len(m[0])]
}

// agentPotentialPseudoTail returns the length of the trailing run of s
// that could still grow into a tag-style pseudo tool call — the discarded
// first attempt "<name>{json}" or "<name>{json}</name>" that GLM sometimes
// emits right before the canonical block (E2E double-attempt). The
// streaming interceptor must not flush those bytes: agentStripTrailingPseudoCall
// can only strip the pseudo-call if it reaches the pre-marker piece WHOLE.
// Scans from the EARLIEST '<' so a partially-arriving start marker ('<<<T')
// at the very end cannot shadow an earlier real pseudo-call candidate.
// Returns 0 when the tail is definitively ordinary text.
func agentPotentialPseudoTail(s string) int {
	for p := 0; p < len(s); p++ {
		if s[p] != '<' {
			continue
		}
		if len(s)-p > agentPseudoHoldMax {
			break
		}
		if agentPseudoPrefixAt(s, p) {
			// The earliest candidate is always the longest hold (len(s)-p
			// shrinks as p grows) and subsumes any later partial-tag match —
			// e.g. the "<T" tail of an incoming "<<<TOOL_CALL>>>" marker. Take
			// the first match and stop; a later overwrite would clobber it with
			// a uselessly short hold and flush the pseudo-call's head (E2E
			// double-attempt regression).
			return len(s) - p
		}
	}
	return 0
}

// agentPseudoPrefixAt reports whether s[p:] is a prefix of a potential pseudo
// tool call "<name>{json}["</name>"]". Conservative: the tag must be a
// valid/partial tool name; after the JSON body only whitespace or a partial
// closing tag may follow, so ordinary prose flushes as soon as real text
// resumes after the braces.
func agentPseudoPrefixAt(s string, p int) bool {
	i := p + 1
	if i >= len(s) {
		return true // bare "<" could still grow into a tag
	}
	if !nameStartByte(s[i]) {
		return false
	}
	i = skipNameBytes(s, i+1)
	if i >= len(s) {
		return true // tag name still arriving
	}
	if s[i] != '>' {
		return false
	}
	i = skipSpaceBytes(s, i+1)
	if i >= len(s) {
		return true // "<name>" — JSON may follow
	}
	if s[i] != '{' {
		return false
	}
	j := agentMatchBrace(s, i)
	if j < 0 {
		return true // JSON still open
	}
	i = skipSpaceBytes(s, j+1)
	if i >= len(s) {
		return true // JSON closed, tail may end here (unclosed pseudo-call shape)
	}
	// Optional closing tag "</name>", possibly still arriving byte by byte
	// (leak-2 shape: "<grep>{...}</grep>").
	if s[i] == '<' && i+1 < len(s) && s[i+1] == '/' {
		i += 2
		if i >= len(s) {
			return true // "</" still arriving
		}
		if !nameStartByte(s[i]) {
			return false // "</" must be followed by a name
		}
		i = skipNameBytes(s, i+1)
		if i >= len(s) {
			return true // closing name still arriving
		}
		if s[i] != '>' {
			return false
		}
		i = skipSpaceBytes(s, i+1)
		if i >= len(s) {
			return true // fully closed; the canonical marker may follow
		}
	}
	// A partially-arriving canonical start marker ('<', '<<<', '<<<T', ...)
	// after the JSON body (or after the closing tag) is fine too: the marker
	// resolves the block and the pseudo-call gets stripped in resolvePiece.
	// Otherwise this is plain prose after the braces — flush.
	return agentStartMarkerPrefix(s[i:])
}

// agentStartMarkerPrefix reports whether tail looks like the beginning of a
// canonical tool-call start marker: optional leading whitespace, a '<' run,
// or a case-insensitive prefix of '<<<TOOL_CALL>>>' possibly still in flight
// across chunk boundaries.
func agentStartMarkerPrefix(tail string) bool {
	tail = strings.TrimLeft(tail, " \t\n\r\v\f")
	if tail == "" {
		return true
	}
	i := 0
	for i < len(tail) && tail[i] == '<' {
		i++
	}
	if i >= len(tail) {
		return true
	}
	if i > agentMaxBrackets {
		return false
	}
	word := strings.ToLower(agentStartWord) // "tool_call"
	rest := strings.ToLower(tail[i:])
	return strings.HasPrefix(word, rest) || strings.HasPrefix(rest, word)
}

func agentJSONClosure(prefix string) string {
	const (
		expectKey   = iota // object: the next entry must start with a key string
		afterKey           // key string closed, ':' not seen yet
		expectValue        // after ':' (object) or '['/',' (array)
		afterValue         // a complete value: expect ',' or container close
	)
	var stack []byte // open '{' / '[', innermost last
	state := expectKey
	valueStarted := false
	inString := false
	stringIsKey := false
	escapeNext := false
	runStart := -1 // start of the current bare literal/number run, -1 when none
	for i := 0; i < len(prefix); i++ {
		c := prefix[i]
		if inString {
			switch {
			case escapeNext:
				escapeNext = false
			case c == '\\':
				escapeNext = true
			case c == '"':
				inString = false
				if stringIsKey {
					state = afterKey
				} else {
					state = afterValue
				}
				valueStarted, runStart = true, -1
			}
			continue
		}
		switch c {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			runStart = -1
		case '"':
			inString = true
			stringIsKey = state == expectKey
			valueStarted, runStart = true, -1
		case '{':
			stack = append(stack, '{')
			state, valueStarted, runStart = expectKey, false, -1
		case '[':
			stack = append(stack, '[')
			state, valueStarted, runStart = expectValue, false, -1
		case '}':
			if n := len(stack); n > 0 && stack[n-1] == '{' {
				stack = stack[:n-1]
			}
			state, valueStarted, runStart = afterValue, true, -1
		case ']':
			if n := len(stack); n > 0 && stack[n-1] == '[' {
				stack = stack[:n-1]
			}
			state, valueStarted, runStart = afterValue, true, -1
		case ':':
			state, valueStarted, runStart = expectValue, false, -1
		case ',':
			if len(stack) > 0 && stack[len(stack)-1] == '{' {
				state = expectKey
			} else {
				state = expectValue
			}
			valueStarted, runStart = false, -1
		default:
			// number / true / false / null fragment — a value is building
			if runStart < 0 {
				runStart = i
			}
			valueStarted = true
		}
	}

	closure := ""
	if inString {
		if escapeNext {
			closure += `\\` // neutralize the dangling escape before closing the string
		}
		if stringIsKey {
			closure += `":null` // the key never finished: give the entry a null value
		} else {
			closure += `"`
		}
	} else {
		switch {
		case state == afterKey:
			closure = ":null"
		case state == expectValue && !valueStarted:
			closure = "null" // ':' seen but the value never arrived
		case state == expectKey && !valueStarted && len(stack) > 0 && stack[len(stack)-1] == '{':
			closure = `"~":null` // ','/open-brace seen but the entry never started
		case state == expectValue && runStart >= 0:
			closure = completeBareLiteral(prefix[runStart:])
		}
	}
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i] == '{' {
			closure += "}"
		} else {
			closure += "]"
		}
	}
	return closure
}

// completeBareLiteral finishes a truncated bare token in value position so the
// closed JSON stays parseable: a strict prefix of true/false/null completes to
// the keyword, an unfinished number gets a trailing 0, a complete number gets
// nothing. Anything else (garbage the model wrote) is left alone — the output
// stays exactly as invalid as the input, never worse.
func completeBareLiteral(run string) string {
	for _, kw := range []string{"true", "false", "null"} {
		if strings.HasPrefix(kw, run) {
			return kw[len(run):]
		}
	}
	if run == "-" || run == "+" || run == "" {
		return "0"
	}
	numeric := true
	for i := 0; i < len(run); i++ {
		c := run[i]
		if !(c >= '0' && c <= '9') && c != '.' && c != 'e' && c != 'E' && c != '+' && c != '-' {
			numeric = false
			break
		}
	}
	if numeric && strings.ContainsAny(run[len(run)-1:], ".eE+-") {
		return "0" // "12." → "12.0", "1e" → "1e0", "1e-" → "1e-0"
	}
	return ""
}

func nameStartByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func skipNameBytes(s string, i int) int {
	for i < len(s) {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' || c == '.' {
			i++
			continue
		}
		break
	}
	return i
}

func skipSpaceBytes(s string, i int) int {
	for i < len(s) && isASCIISpace(s[i]) {
		i++
	}
	return i
}

func (in *AgentStreamInterceptor) Feed(chunk string) AgentParsedChunk {
	in.buffer += chunk
	return in.drain(false)
}

// Finish drains the interceptor at end of upstream stream, treating the
// buffered tail as complete data: a marker whose trailing '>' run touches the
// very end can now match, and whatever remains unparsed is ordinary content.
// Tool calls discovered here must still be forwarded to the client.
func (in *AgentStreamInterceptor) Finish() AgentParsedChunk {
	parsed := in.drain(true)
	in.offset = len(in.buffer)
	return parsed
}

// finishCall closes the current tool-call block: scanning resumes after it
// and the pendingSep pass swallows a stray fence the model may append.
func (in *AgentStreamInterceptor) finishCall() {
	in.inCall = false
	in.pendingSep = true
	in.tcName = ""
	in.tcNameFound = false
	in.tcArgsFound = false
	in.tcArgsPos = 0
	in.tcArgsStreamed = 0
	in.tcBraceDepth = 0
	in.tcInString = false
	in.tcEscapeNext = false
	in.tcArgsDone = false
	in.tcFallback = false
}

// jsonStringValueAt reads a `"key": "value"` string value starting the scan
// at pos (just past the quoted key). ok is false when the value is not a
// string or its closing quote has not arrived yet.
func jsonStringValueAt(s string, pos int) (string, bool) {
	for pos < len(s) && isASCIISpace(s[pos]) {
		pos++
	}
	if pos >= len(s) || s[pos] != ':' {
		return "", false
	}
	pos++
	for pos < len(s) && isASCIISpace(s[pos]) {
		pos++
	}
	if pos >= len(s) || s[pos] != '"' {
		return "", false
	}
	pos++
	start := pos
	for pos < len(s) {
		switch s[pos] {
		case '\\':
			pos += 2
			continue
		case '"':
			return s[start:pos], true
		}
		pos++
	}
	return "", false
}

// agentStreamExtractName resolves the tool name from partial block body text,
// accepting the same key spellings as agentExtractCall (priority order). The
// search stops before any arguments key so a nested "name" parameter inside
// the arguments object can never be mistaken for the tool name. ok is false
// while no accepted key carries a complete string value yet.
func agentStreamExtractName(body string) (name string, ok bool) {
	searchEnd := len(body)
	for _, k := range agentArgKeys {
		if i := strings.Index(body, `"`+k+`"`); i >= 0 && i < searchEnd {
			searchEnd = i
		}
	}
	region := body[:searchEnd]
	for _, k := range agentNameKeys {
		keyIdx := strings.Index(region, `"`+k+`"`)
		if keyIdx < 0 {
			continue
		}
		if v, ok := jsonStringValueAt(region, keyIdx+len(k)+2); ok {
			if strings.TrimSpace(v) == "" {
				// BLANK tool name — the production "Tool Call:" failure
				// (stall_evident.md 2026-09-07: the client showed an unnamed
				// call and rejected the body with "tool input was not fully
				// received"). A blank name must never resolve: keep waiting
				// for a real one; at block end the fallback parse routes
				// through agentExtractCall (schema inference) or drops the
				// block (emptyNameBlock) instead of emitting an unroutable
				// call header.
				return "", false
			}
			return v, true
		}
	}
	return "", false
}

// emptyNameBlock reports whether raw is a tool-call block body whose name
// keys are all present but BLANK — e.g. {"name":""} or
// {"name":"","arguments":{...}} (stall_evident.md 2026-09-07). Such a block
// can never be routed to a tool, and schema inference (agentInferToolName)
// has already had its chance by the time this is consulted, so callers drop
// the block instead of leaking raw protocol JSON as content; the blank turn
// then falls to the stall guard, which can retry the attempt.
func emptyNameBlock(raw string) bool {
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(agentRepairJSON(strings.TrimSpace(raw))), &obj) != nil {
		return false
	}
	named := false
	for _, k := range agentNameKeys {
		v, present := obj[k]
		if !present {
			continue
		}
		named = true
		var s string
		if json.Unmarshal(v, &s) == nil && strings.TrimSpace(s) != "" {
			return false // a real name exists — not an empty-name block
		}
	}
	return named
}

// Outcomes of agentStreamFindArgs.
const (
	argsPending   = iota // no complete arguments value start visible yet
	argsObject           // value starts with '{' — bracePos is its offset
	argsNonObject        // value exists but is not an object — cannot stream
)

// agentStreamFindArgs locates the arguments value in partial block body text
// using the accepted key spellings (agentArgKeys, priority order).
func agentStreamFindArgs(body string) (bracePos int, state int) {
	for _, k := range agentArgKeys {
		keyIdx := strings.Index(body, `"`+k+`"`)
		if keyIdx < 0 {
			continue
		}
		pos := keyIdx + len(k) + 2
		for pos < len(body) && isASCIISpace(body[pos]) {
			pos++
		}
		if pos >= len(body) {
			return 0, argsPending // key arrived, colon pending
		}
		if body[pos] != ':' {
			return 0, argsNonObject // malformed — buffered parse will judge
		}
		pos++
		for pos < len(body) && isASCIISpace(body[pos]) {
			pos++
		}
		if pos >= len(body) {
			return 0, argsPending // value start pending
		}
		if body[pos] != '{' {
			return 0, argsNonObject // string/number arguments — buffer mode
		}
		return pos, argsObject
	}
	return 0, argsPending
}

// States of agentEndMarkerPrefix.
const (
	agentPrefixNo       = 0 // s can never grow into a tolerated marker
	agentPrefixMaybe    = 1 // s is a strict prefix of some tolerated marker
	agentPrefixComplete = 2 // s starts with a complete tolerated marker
)

// agentEndMarkerPrefix classifies s (which starts with '<') against the
// tolerated END_TOOL_CALL marker spellings (agentMinBrackets..agentMaxBrackets
// on each side). A "maybe" verdict means the bytes so far are consistent with
// a marker that is still arriving across upstream chunks — the caller must
// hold them back rather than stream them as argument content.
func agentEndMarkerPrefix(s string) (state int, markerLen int) {
	L := bracketRunForward(s, '<')
	if L > agentMaxBrackets {
		return agentPrefixNo, 0
	}
	rest := s[L:]
	word := agentEndWord
	k := 0
	for k < len(rest) && k < len(word) && rest[k] == word[k] {
		k++
	}
	if k == 0 {
		if len(rest) == 0 {
			return agentPrefixMaybe, 0 // bare '<' run, word may still start
		}
		return agentPrefixNo, 0 // '<' followed by a non-'E' byte: ordinary
	}
	if L < agentMinBrackets {
		return agentPrefixNo, 0 // lead run finalized below tolerance
	}
	if k < len(word) {
		if k == len(rest) {
			return agentPrefixMaybe, 0 // word still arriving
		}
		return agentPrefixNo, 0 // word mismatched mid-way
	}
	after := rest[len(word):]
	trail := bracketRunForward(after, '>')
	if trail > agentMaxBrackets {
		return agentPrefixNo, 0
	}
	if trail < agentMinBrackets && trail == len(after) {
		return agentPrefixMaybe, 0 // '>' run may still grow to tolerance
	}
	if trail < agentMinBrackets {
		return agentPrefixNo, 0 // non-'>' byte right after the word
	}
	return agentPrefixComplete, L + len(word) + trail
}

// runeSafeCut returns the largest prefix length of s that ends on a rune
// boundary, holding back at most utf8.UTFMax-1 trailing bytes of an
// incomplete multi-byte sequence. Argument fragments must be valid UTF-8 on
// their own: an upstream chunk boundary can split a rune, and forwarding the
// halves separately would surface as U+FFFD garble on the client (issue #23).
func runeSafeCut(s string) int {
	n := len(s)
	for n > 0 && !utf8.ValidString(s[:n]) && len(s)-n < utf8.UTFMax {
		n--
	}
	return n
}

func (in *AgentStreamInterceptor) drain(final bool) AgentParsedChunk {
	var content []string
	var toolCalls []map[string]interface{}

	// resolvePiece releases the text held from the moment the open marker was
	// spotted until the block's fate is known. ok=true means the block parsed
	// into a tool call: a trailing tag-style pseudo-call in the piece was a
	// dropped first attempt and is stripped (E2E log). ok=false keeps the
	// piece verbatim — the block was invalid and stays visible.
	resolvePiece := func(ok bool) {
		if in.pendingPiece == "" {
			return
		}
		piece := in.pendingPiece
		in.pendingPiece = ""
		if ok {
			piece = agentStripTrailingPseudoCall(piece)
		}
		if piece != "" {
			content = append(content, piece)
		}
	}

	for {
		// Immediately after a tool-call block, swallow blank space and stray
		// ```json / ``` fence lines the model appends despite instructions
		// (possibly split across chunks). Ordinary content elsewhere —
		// including its leading spaces and real code blocks — is untouched.
		if in.pendingSep {
			for {
				for in.offset < len(in.buffer) && isASCIISpace(in.buffer[in.offset]) {
					in.offset++
				}
				n := SkipLeadingAgentFence(in.buffer[in.offset:])
				if n == 0 {
					break
				}
				in.offset += n
			}
			if agentPossibleFencePrefix(in.buffer[in.offset:]) && !final {
				break // could still become a fence; wait for more chunks
			}
			in.pendingSep = false
		}

		// If we're inside an incomplete block from a previous Feed call,
		// keep streaming its body — the scanner resumes where it left off.
		if in.inCall {
			if !in.drainToolCall(final, &content, &toolCalls, resolvePiece) {
				break // block still streaming: wait for more chunks
			}
			continue
		}

		rest := in.buffer[in.offset:]
		start, markerLen := findAgentMarker(rest, agentStartWord, final)
		if start < 0 {
			if final {
				// End of data: everything left is ordinary content.
				if rest != "" {
					content = append(content, rest)
					in.offset = len(in.buffer)
				}
				break
			}
			// Hold back a window big enough for a fence line + partial marker
			// so neither can leak as content while split across chunks. A
			// marker reported incomplete keeps its bytes inside this window,
			// so nothing here can be part of a future match. The cut is
			// backed up to a rune boundary so a multi-byte character is
			// never split across emissions (invalid UTF-8 would render as
			// replacement-char garble on the client — issue #23).
			const keep = agentStreamKeep
			if len(rest) > keep {
				cut := len(rest) - keep
				for cut > 0 && !utf8.RuneStart(rest[cut]) {
					cut--
				}
				// A trailing run that could still grow into a pseudo tool call
				// must stay un-flushed: agentStripTrailingPseudoCall needs the
				// pseudo-call contiguous in the pre-marker piece (E2E log
				// double-attempt regression).
				if h := agentPotentialPseudoTail(rest); h > 0 {
					if limit := len(rest) - h; limit < cut {
						cut = limit
						for cut > 0 && !utf8.RuneStart(rest[cut]) {
							cut--
						}
					}
				}
				if cut > 0 {
					content = append(content, rest[:cut])
					in.offset += cut
				}
			}
			break
		}
		if start > 0 {
			piece := TrimTrailingAgentFence(rest[:start])
			if piece != "" {
				// Hold the pre-marker text until the block's fate is known —
				// it may end in a tag-style pseudo-call that gets stripped
				// when the block parses (see resolvePiece).
				in.pendingPiece = piece
			}
			in.offset += start
		}
		// Enter the tool-call block: the opening marker is consumed now and
		// the body streams incrementally until the closing marker.
		in.inCall = true
		in.tcBlockStart = in.offset
		in.offset += markerLen
	}
	return AgentParsedChunk{Content: strings.Join(content, ""), ToolCalls: toolCalls}
}

// emitArgsFragment forwards argsText[:emitEnd] as the next streamed delta: it
// merges into the header emitted earlier in the same pass (exactly what
// OpenAI does when one chunk covers the name and the first argument bytes),
// or rides its own id-less fragment delta when the header went out earlier.
func (in *AgentStreamInterceptor) emitArgsFragment(argsText string, emitEnd int, headerIdx int, toolCalls *[]map[string]interface{}) {
	if emitEnd <= in.tcArgsStreamed {
		return // nothing new to forward
	}
	frag := argsText[in.tcArgsStreamed:emitEnd]
	in.tcArgsStreamed = emitEnd
	if headerIdx >= 0 {
		// Merge into the header emitted earlier in the SAME pass (exactly what
		// OpenAI does when one chunk covers the name and the first argument
		// bytes). A pass can emit twice — the streamed bytes, then a synthetic
		// JSON closure when the block ends malformed (truncatedAt/final) —
		// so concatenate: the chunk's delta is every new byte of this pass.
		fn := (*toolCalls)[headerIdx]["function"].(map[string]interface{})
		fn["arguments"] = fn["arguments"].(string) + frag
		return
	}
	*toolCalls = append(*toolCalls, map[string]interface{}{
		"index": in.callIndex,
		"function": map[string]interface{}{
			"arguments": frag,
		},
	})
}

// drainToolCall processes the body of one open tool-call block. It returns
// false when more upstream bytes are needed, true once the block is fully
// consumed (drain continues scanning for what follows).
//
// resolvePiece releases the pre-marker text held in drain() — ok=true when
// the block resolved into a tool call (a trailing pseudo-call is stripped),
// ok=false when it leaked as content (the piece stays verbatim).
func (in *AgentStreamInterceptor) drainToolCall(final bool, content *[]string, toolCalls *[]map[string]interface{}, resolvePiece func(bool)) bool {
	headerIdx := -1 // toolCalls slot of the header emitted in this pass

	for {
		body := in.buffer[in.offset:]

		// ── Fallback mode: buffer everything, parse the complete block ──
		if in.tcFallback {
			idx, markerLen := findAgentEndMarkerInBuffer(body, final)
			if idx < 0 {
				if !final {
					return false
				}
				// Stream ended mid-block: the tail is either a parsable unclosed
				// call (emit it — the E2E unclosed-marker contract) or unparsable
				// junk (drop it; the held piece releases verbatim).
				if name, args, ok := agentLooseParse(strings.TrimSpace(body), in.toolSchemas); ok && name != "" {
					*toolCalls = append(*toolCalls, map[string]interface{}{
						"index": in.callIndex,
						"id":    "call_" + agentRandomHex(12),
						"type":  "function",
						"function": map[string]interface{}{
							"name":      name,
							"arguments": agentStreamArguments(args),
						},
					})
					in.callIndex++
					resolvePiece(true)
				} else if emptyNameBlock(strings.TrimSpace(body)) {
					// Blank-name block: drop, never leak (see drainToolCall).
					resolvePiece(false)
				} else {
					resolvePiece(false)
				}
				in.offset = len(in.buffer)
				in.finishCall()
				return true
			}
			end := in.offset + idx
			raw := strings.TrimSpace(in.buffer[in.offset:end])
			if name, args, ok := agentLooseParse(raw, in.toolSchemas); ok && name != "" {
				*toolCalls = append(*toolCalls, map[string]interface{}{
					"index": in.callIndex,
					"id":    "call_" + agentRandomHex(12),
					"type":  "function",
					"function": map[string]interface{}{
						"name":      name,
						"arguments": agentStreamArguments(args),
					},
				})
				in.callIndex++
				resolvePiece(true)
			} else if emptyNameBlock(raw) {
				// Unroutable blank-name block (stall_evident.md 2026-09-07):
				// drop it instead of leaking protocol JSON as content. The
				// blank turn falls to the stall guard, which retries with a
				// nudge (post-tool-result) or ends the turn honestly.
				resolvePiece(false)
			} else {
				// invalid model block: leave it as visible text
				resolvePiece(false)
				*content = append(*content, in.buffer[in.tcBlockStart:end+markerLen])
			}
			in.offset = end + markerLen
			in.finishCall()
			return true
		}

		// ── Phase 1: resolve the tool name ──
		if !in.tcNameFound {
			if name, ok := agentStreamExtractName(body); ok {
				in.tcName = name
				in.tcNameFound = true
			} else {
				// No complete name yet: if the block already closed (or the
				// stream ended) it never will — parse the whole body instead.
				if idx, _ := findAgentEndMarkerInBuffer(body, final); idx >= 0 || final {
					in.tcFallback = true
					continue
				}
				return false
			}
		}

		// ── Phase 2: locate the arguments object, then emit the header ──
		if !in.tcArgsFound {
			pos, state := agentStreamFindArgs(body)
			switch state {
			case argsObject:
				in.tcArgsFound = true
				in.tcArgsPos = in.offset + pos
				// Release the pre-marker prose NOW, minus a run that could still
				// grow into a tag-style pseudo-call (agentStripTrailingPseudoCall
				// needs it contiguous when resolvePiece(true) runs at block close).
				// OpenAI-contract clients drop content deltas that arrive AFTER
				// tool-call deltas (Vercel AI SDK), so the prose must go out before
				// the header — holding the whole piece until block close truncated
				// every prose tail at its last keep-window emission (issue #24).
				if in.pendingPiece != "" {
					hold := agentPotentialPseudoTail(in.pendingPiece)
					rel := in.pendingPiece
					if hold > 0 {
						rel = in.pendingPiece[:len(in.pendingPiece)-hold]
						in.pendingPiece = in.pendingPiece[len(in.pendingPiece)-hold:]
					} else {
						in.pendingPiece = ""
					}
					if rel != "" {
						*content = append(*content, rel)
					}
				}
				*toolCalls = append(*toolCalls, map[string]interface{}{
					"index": in.callIndex,
					"id":    "call_" + agentRandomHex(12),
					"type":  "function",
					"function": map[string]interface{}{
						"name":      in.tcName,
						"arguments": "",
					},
				})
				headerIdx = len(*toolCalls) - 1
			case argsNonObject:
				in.tcFallback = true // e.g. arguments as a JSON-encoded string
				continue
			default: // argsPending
				if idx, _ := findAgentEndMarkerInBuffer(body, final); idx >= 0 || final {
					in.tcFallback = true // flat payload or name-only block
					continue
				}
				return false
			}
		}

		// ── Phase 3: stream the arguments object byte-by-byte ──
		if !in.tcArgsDone {
			argsText := in.buffer[in.tcArgsPos:]
			i := in.tcArgsStreamed
			truncatedAt, truncatedLen := -1, 0
		scan:
			for i < len(argsText) {
				c := argsText[i]
				if in.tcEscapeNext {
					in.tcEscapeNext = false
					i++
					continue
				}
				if c == '\\' {
					in.tcEscapeNext = true
					i++
					continue
				}
				if c == '"' {
					in.tcInString = !in.tcInString
					i++
					continue
				}
				if !in.tcInString {
					switch c {
					case '<':
						// Outside a string this can only be the closing marker
						// arriving before the braces balanced (the model bailed
						// mid-JSON). Markers INSIDE a string value are content
						// and deliberately never match here; a partial marker
						// split across upstream chunks is held back, never
						// streamed as argument bytes. NOTE: the XML-style close
						// </TOOL_CALL> only reaches the scanner after the braces
						// balanced — the '/' classifies it as No (ordinary
						// malformed-JSON byte) and Phase 4 consumes it via
						// findAgentEndMarkerInBuffer, which accepts XML closes.
						state, mLen := agentEndMarkerPrefix(argsText[i:])
						switch {
						case state == agentPrefixComplete:
							truncatedAt, truncatedLen = i, mLen
							break scan
						case state == agentPrefixMaybe && !final:
							in.emitArgsFragment(argsText, i, headerIdx, toolCalls)
							return false // marker still arriving: wait for it
						default:
							i++ // ordinary byte inside malformed JSON
							continue
						}
					case '{':
						in.tcBraceDepth++
					case '}':
						in.tcBraceDepth--
						if in.tcBraceDepth == 0 {
							i++
							in.tcArgsDone = true
							break scan
						}
					}
				}
				i++
			}

			emitEnd := i
			if !in.tcArgsDone && truncatedAt < 0 {
				// Also at final: an incomplete trailing rune can never
				// complete — forwarding it would surface as U+FFFD garble.
				emitEnd = runeSafeCut(argsText[:i])
				// Held-back bytes are only the tail of one incomplete rune
				// (never a quote/backslash/brace), so re-scanning them next
				// pass cannot corrupt the string/escape/depth state.
			}
			in.emitArgsFragment(argsText, emitEnd, headerIdx, toolCalls)

			switch {
			case in.tcArgsDone:
				in.offset = in.tcArgsPos + in.tcArgsStreamed
			case truncatedAt >= 0:
				// Malformed call (unbalanced JSON): the header already went
				// out, so close the call at the marker instead of leaking the
				// marker bytes into the arguments string. Append the same JSON
				// closure as the final path so the reassembled arguments stay
				// parseable — without it the client rejects the input with
				// "tool input was not fully received" and the turn is lost
				// (log_stall.md 2026-09-06).
				if c := agentJSONClosure(argsText[:truncatedAt]); c != "" {
					in.emitArgsFragment(argsText[:truncatedAt]+c, truncatedAt+len(c), headerIdx, toolCalls)
				}
				in.offset = in.tcArgsPos + truncatedAt + truncatedLen
				in.callIndex++
				in.finishCall()
				resolvePiece(true)
				return true
			case final:
				// Stream died mid-arguments (upstream hang killed by the stall
				// watchdog or connection cut). The header and the bytes so far
				// are already on the wire and cannot be un-sent; the client
				// reassembles arguments by concatenating fragments, so append a
				// minimal JSON closure — the open string, then the open
				// containers — to keep the accumulated arguments PARSEABLE. Without
				// this the tool runtime rejects the input with "tool input was
				// not fully received"/JSON parse errors (log_stall.md 2026-09-06,
				// 10 occurrences) and the whole turn is lost. The closure is
				// honest about being synthetic: the payload stays truncated, but
				// a parseable-but-partial call that fails loudly at the tool
				// beats an unparseable one that fails the whole reply.
				prefixEnd := emitEnd
				if prefixEnd < in.tcArgsStreamed {
					prefixEnd = in.tcArgsStreamed
				}
				closure := agentJSONClosure(argsText[:prefixEnd])
				if closure != "" {
					// The synthetic fragment repeats the streamed prefix so the
					// closure lands after the last byte the client already holds.
					in.emitArgsFragment(argsText[:prefixEnd]+closure, prefixEnd+len(closure), headerIdx, toolCalls)
				}
				in.callIndex++
				in.finishCall()
				resolvePiece(true)
				return true
			default:
				return false // arguments still streaming
			}
		}

		// ── Phase 4: arguments complete — consume the closing marker ──
		rest := in.buffer[in.offset:]
		idx, markerLen := findAgentEndMarkerInBuffer(rest, final)
		if idx < 0 {
			if final {
				// Truncated block: the streamed call stands; drop the tail.
				in.offset = len(in.buffer)
				in.callIndex++
				in.finishCall()
				resolvePiece(true)
				return true
			}
			return false
		}
		in.offset += idx + markerLen
		in.callIndex++
		in.finishCall()
		resolvePiece(true)
		return true
	}
}

func isASCIISpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}

// ============================================================================
// SHIM DISPATCH GLUE — lets the request handlers stay variant-agnostic
// ============================================================================
//
// The two agent-mode implementations (modern in this file, legacy in main.go)
// expose slightly different APIs. These thin adapters/dispatchers present one
// uniform surface so chatCompletionsHandler / anthropicMessagesHandler pick
// the active shim purely from config.

// transformMessagesForAgentModern folds the whole conversation + tool contract
// into one XML-sectioned prompt and wraps it as a single Qwen user message.
func transformMessagesForAgentModern(rawMessages json.RawMessage, toolsRaw json.RawMessage) ([]byte, error) {
	var msgs []agentMessage
	if err := json.Unmarshal(rawMessages, &msgs); err != nil {
		return nil, fmt.Errorf("agent transform (modern): parse messages: %w", err)
	}
	var tools []openAITool
	if len(toolsRaw) > 0 {
		_ = json.Unmarshal(toolsRaw, &tools)
	}
	prompt := buildAgentPrompt(msgs, tools)
	return wrapAgentPromptAsMessages(prompt)
}

// agentTransformMessages rewrites the incoming OpenAI messages array for the
// active agent shim, returning the JSON-encoded messages to send upstream.
func agentTransformMessages(rawMessages, toolsRaw json.RawMessage) ([]byte, error) {
	if config.agentNative() {
		return transformMessagesForAgentNative(rawMessages, toolsRaw)
	}
	if config.agentModern() {
		return transformMessagesForAgentModern(rawMessages, toolsRaw)
	}
	var tools []interface{}
	if len(toolsRaw) > 0 {
		_ = json.Unmarshal(toolsRaw, &tools)
	}
	return transformMessagesForAgent(rawMessages, tools)
}

// agentExtractToolCalls parses tool-call blocks out of finished assistant text
// using the active shim's parser. toolsRaw (the request's declared tools)
// enables name inference for args-only payloads in the modern shim.
func agentExtractToolCalls(text string, toolsRaw json.RawMessage) []map[string]interface{} {
	if config.agentModern() {
		return ParseAgentToolCallsWithTools(text, toolsRaw)
	}
	return extractAgentToolCalls(text)
}

// agentStripToolCalls removes tool-call blocks from finished assistant text
// using the active shim's stripper.
func agentStripToolCalls(text string) string {
	if config.agentModern() {
		return StripAgentToolCalls(text)
	}
	return stripAgentToolCallBlocks(text)
}

// agentInterceptor is the uniform streaming-interceptor surface used by both
// the OpenAI and Anthropic handlers. feed processes one upstream chunk;
// finish drains the tail at end of stream.
type agentInterceptor interface {
	feed(chunk string) (content string, toolCalls []map[string]interface{})
	finish() (content string, toolCalls []map[string]interface{})
}

// modernAgentInterceptor adapts AgentStreamInterceptor (incremental streaming:
// header delta first, then id-less argument fragments, tolerant markers,
// hold-back window) to the agentInterceptor interface.
type modernAgentInterceptor struct{ in *AgentStreamInterceptor }

func (m *modernAgentInterceptor) feed(chunk string) (string, []map[string]interface{}) {
	p := m.in.Feed(chunk)
	return p.Content, p.ToolCalls
}

func (m *modernAgentInterceptor) finish() (string, []map[string]interface{}) {
	p := m.in.Finish()
	return p.Content, p.ToolCalls
}

// legacyAgentInterceptor adapts the legacy agentStreamInterceptor
// (incremental argument streaming) to the agentInterceptor interface. Its
// finish returns only trailing content; end-of-stream tool calls are caught
// by the caller's extractAgentToolCalls safety net, as before.
type legacyAgentInterceptor struct{ in *agentStreamInterceptor }

func (l *legacyAgentInterceptor) feed(chunk string) (string, []map[string]interface{}) {
	content, toolCalls, _ := l.in.feed(chunk)
	return content, toolCalls
}

func (l *legacyAgentInterceptor) finish() (string, []map[string]interface{}) {
	return l.in.flushFinal(), nil
}

// newAgentInterceptor constructs the streaming interceptor for the active
// shim. toolsRaw (the request's declared tools) feeds name inference for
// args-only payloads in the modern shim (agentInferToolName).
func newAgentInterceptor(toolsRaw json.RawMessage) agentInterceptor {
	if config.agentModern() {
		return &modernAgentInterceptor{in: &AgentStreamInterceptor{
			toolSchemas: agentCollectToolSchemas(toolsRaw),
		}}
	}
	return &legacyAgentInterceptor{in: newAgentStreamInterceptor()}
}

// rearmAgentInterceptor replaces a stale interceptor (an upstream
// edit_content rewrite rewound text it had already consumed — issue #23)
// while preserving the client-visible call-index sequence: a call streamed
// before the rewind keeps its index, and the re-emitted block takes the
// next one instead of colliding inside SDK accumulation.
func rearmAgentInterceptor(old agentInterceptor) agentInterceptor {
	fresh := newAgentInterceptor(nil)
	switch o := old.(type) {
	case *modernAgentInterceptor:
		if f, ok := fresh.(*modernAgentInterceptor); ok {
			f.in.callIndex = o.in.callIndex
			f.in.toolSchemas = o.in.toolSchemas
		}
	case *legacyAgentInterceptor:
		if f, ok := fresh.(*legacyAgentInterceptor); ok {
			f.in.callIndex = o.in.callIndex
		}
	}
	return fresh
}
