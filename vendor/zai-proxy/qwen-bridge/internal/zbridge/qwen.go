// Qwen transport layer — chat.qwen.ai.
//
// Forked from the Z.AI bridge transport (zai.go in the parent project); the
// upstream protocol below is Qwen-specific, everything above it (agent-mode
// interceptor, stall detection, session pool, OpenAI/Anthropic handlers) is
// inherited unchanged.
//
// Qwen specifics (verified empirically in the Qwen-Free-Api research,
// 2026-08-31, ported here):
//   - Auth rides the `token` cookie (JWT), NOT a Bearer header: the Aliyun
//     WAF in front of chat.qwen.ai punishes Bearer-only requests with a
//     CAPTCHA HTML page. A browser User-Agent is likewise mandatory.
//   - The `Version` header (frontend bundle version) is REQUIRED by
//     /api/v2/chat/completions; without it the endpoint answers 200 JSON
//     {"code":"Bad_Request","details":"Internal error..."}. It is scraped
//     from the homepage asset path (/qwen-chat-fe/<version>/), cached, and
//     force-refreshed once when a completion comes back Bad_Request.
//   - Chats are created via POST /api/v2/chats/new and deleted via
//     DELETE /api/v1/chats/{id} (both take the cookie+Version headers).
//   - SSE stream: choices[0].delta with phase "thinking_summary" (reasoning;
//     the text rides delta.extra.summary_thought.content as line array) or
//     plain delta.content (answer text). [DONE] terminates.

package zbridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// ============================================================================
// QWEN CONSTANTS
// ============================================================================

const (
	// qwenUserAgent must look like a browser: non-browser UAs get the Aliyun
	// WAF CAPTCHA punish page instead of JSON/SSE.
	qwenUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"

	// qwenDefaultFEVersion is the fallback when the homepage scrape fails
	// (air-gapped start). initializeSession scrapes the live value on every
	// init, so this only matters until the first successful scrape.
	qwenDefaultFEVersion = "0.2.87"

	// qwenDefaultModel is the model used when the client sends none.
	qwenDefaultModel = "qwen3.6-plus"
)

// qwenProdURL is the unchanged default; anything else means a mock upstream.
const qwenProdURL = "https://chat.qwen.ai"

// BASE_URL is a var (not const) only so tests can point the bridge at a
// mock upstream; the QWEN_BASE_URL env override lets e2e harnesses run the
// real binary against a mock upstream without code changes.
var BASE_URL = qwenProdURL

// qwenFEVersionRe extracts the frontend bundle version from an asset URL:
// /qwen-chat-fe/<version>/...
var qwenFEVersionRe = regexp.MustCompile(`/qwen-chat-fe/([0-9][0-9A-Za-z._-]*)/`)

// qwenWAFMarkers are the Aliyun WAF challenge fingerprints seen in HTML
// bodies. A text/html response on an API route is always a challenge.
var qwenWAFMarkers = regexp.MustCompile(`aliyun_waf|Access[_ ]?Verification|captcha|_____tmd_____|x5secdata|x5referer`)

// ============================================================================
// QWEN SESSION INITIALIZATION
// ============================================================================

// initializeSession validates the Qwen token (GET /api/v2/configs/) and
// resolves the frontend Version header. It sets session.Token from
// QWEN_TOKEN / the shared token file; without a token every chat request
// will fail (unlike Qwen there is no guest flow — Qwen requires a JWT).
func initializeSession() error {
	session.mu.Lock()
	if session.Initializing {
		session.mu.Unlock()
		for {
			time.Sleep(100 * time.Millisecond)
			session.mu.Lock()
			if !session.Initializing {
				session.mu.Unlock()
				return nil
			}
			session.mu.Unlock()
		}
	}
	session.Initializing = true
	session.mu.Unlock()

	defer func() {
		session.mu.Lock()
		session.Initializing = false
		session.mu.Unlock()
	}()

	if summary := initTokenPool(); summary != "" {
		log.Printf("[TokenPool] %s — sticky selection, auto-cooldown on daily cap", summary)
	}
	if tokenPool != nil {
		// Multi-token mode: the pool is authoritative. Seed session.Token
		// with the sticky-active entry so legacy single-token readers (JWT
		// decode banner, DELETE fallback) keep working unchanged.
		if t, idx := tokenPool.Active(); idx >= 0 {
			session.Token = t
		}
		session.mu.Lock()
		session.Initialized = true
		session.mu.Unlock()
		id, name := decodeJWT(session.Token)
		session.UserID = id
		if name != "" {
			session.UserName = name
		}
		log.Printf("[Session] Token pool active: %d/%d token(s) ready", tokenPool.ReadyCount(), tokenPool.Len())
		return nil
	}
	if config.QwenToken == "" {
		session.mu.Lock()
		session.Initialized = false
		session.mu.Unlock()
		return errors.New("Qwen token required: set QWEN_TOKEN, QWEN_TOKENS, or ~/.config/qwen-proxy/token(s)")
	}
	session.Token = config.QwenToken

	// Decode userId from the JWT payload (no network needed).
	id, name := decodeJWT(session.Token)
	session.UserID = id
	if name != "" {
		session.UserName = name
	}
	if session.UserID == "" {
		session.UserName = "User"
	}

	// Resolve the frontend `Version` header before any Qwen call.
	scrapeQwenFEVersion()

	// Quick ping to verify the token: 401 = expired/invalid.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", BASE_URL+"/api/v2/configs/", nil)
	if err != nil {
		return fmt.Errorf("Qwen session init: %s", err.Error())
	}
	setQwenHeaders(req, "", activeTokenSafe())
	resp, err := qwenClient().Do(req)
	if err != nil {
		log.Printf("[Session] Qwen configs ping failed (non-fatal): %s", err.Error())
		// Network hiccup must not brick the bridge — token presence is enough
		// to attempt serving; a dead token will surface as 401 on the first
		// completion, which re-inits.
		session.mu.Lock()
		session.Initialized = true
		session.mu.Unlock()
		return nil
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode == 401 {
		session.mu.Lock()
		session.Initialized = false
		session.mu.Unlock()
		return errors.New("Qwen token is invalid or expired (401)")
	}

	uidPreview := session.UserID
	if len(uidPreview) > 8 {
		uidPreview = uidPreview[:8]
	}
	log.Printf("[Session] Qwen token validated. UserID: %s... (%s)", uidPreview, session.UserName)
	session.mu.Lock()
	session.Initialized = true
	session.mu.Unlock()
	return nil
}

// scrapeQwenFEVersion fetches the Qwen homepage and extracts the frontend
// bundle version from the asset path. Failure keeps the current value.
func scrapeQwenFEVersion() {
	if apkTransportEnabled() {
		// APK mode: the app protocol has no web frontend Version header; the
		// APK headers do not include one, so there is nothing to scrape.
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", BASE_URL+"/", nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", qwenUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := qwenClient().Do(req)
	if err != nil {
		log.Printf("[Version] Scrape error: %s, keeping %s", err.Error(), getQwenFEVersion())
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if m := qwenFEVersionRe.FindStringSubmatch(string(body)); m != nil {
		session.mu.Lock()
		old := session.FeVersion
		session.FeVersion = m[1]
		session.mu.Unlock()
		if old != m[1] {
			log.Printf("[Version] Resolved frontend version: %s", m[1])
		}
	} else {
		log.Printf("[Version] Could not scrape frontend version from homepage — keeping %s", getQwenFEVersion())
	}
}

// getQwenFEVersion returns the current frontend Version header value
// (falling back to the compiled-in default before the first scrape).
func getQwenFEVersion() string {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.FeVersion == "" {
		return qwenDefaultFEVersion
	}
	return session.FeVersion
}

// setQwenHeaders applies the header set every Qwen API call needs: the
// auth cookie, the browser UA, the Version header, and source/X-Request-Id
// (required by /api/v2/chats/new). An explicit feVersion overrides the
// cached value (used by the force-refresh retry path). The token rides the
// Cookie header; with a token pool the caller passes the chat-minting token
// (see tokenForRequest) so a chat is always served by the account that
// minted it. An empty token falls back to the legacy single-token session.
func setQwenHeaders(req *http.Request, feVersion, token string) {
	if feVersion == "" {
		feVersion = getQwenFEVersion()
	}
	if token == "" {
		token = sessionToken()
	}
	req.Header.Set("User-Agent", qwenUserAgent)
	req.Header.Set("Cookie", "token="+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Version", feVersion)
	req.Header.Set("source", "web")
	req.Header.Set("X-Request-Id", randomUUID())
	req.Header.Set("X-Accel-Buffering", "no")
	// APK mode (QWEN_TRANSPORT=apk) overrides the UA/source family above;
	// web-only headers are removed. No-op in web mode.
	applyQwenTransportHeaders(req, feVersion, token)
}

// sessionToken returns the current Qwen JWT.
func sessionToken() string {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.Token
}

// errQwenSilentPunish marks the Aliyun WAF "silent punish" signature: the
// completions endpoint answers 200 + Content-Type: text/event-stream, then
// closes the stream without a single data event (probe 2026-09-11). The
// stream machinery would otherwise treat it as a clean empty completion.
var errQwenSilentPunish = errors.New("Qwen silent punish: stream opened but zero data events (WAF rate-limit on egress IP)")

// isQwenWAFChallenge reports whether a response is an Aliyun WAF challenge
// page (HTML content-type on an API route, or a JSON body carrying WAF
// punish markers).
func isQwenWAFChallenge(ct string, body []byte) bool {
	if strings.Contains(ct, "text/html") {
		return true
	}
	return body != nil && qwenWAFMarkers.Match(body)
}

// ============================================================================
// QWEN CHAT LIFECYCLE
// ============================================================================

// CreateQwenChat mints one fresh Qwen chat via POST /api/v2/chats/new.
func CreateQwenChat(model string) (string, error) {
	session.mu.Lock()
	initialized := session.Initialized
	session.mu.Unlock()
	if !initialized {
		if err := initializeSession(); err != nil {
			return "", err
		}
	}
	if model == "" {
		model = qwenDefaultModel
	}
	body, _ := json.Marshal(map[string]interface{}{
		"title":      "New Chat",
		"models":     []string{model},
		"chat_mode":  "normal",
		"chat_type":  "t2t",
		"timestamp":  time.Now().UnixMilli(),
		"project_id": "",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	token, err := activeToken()
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", BASE_URL+"/api/v2/chats/new", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("Qwen chat creation request build failed: %s", err.Error())
	}
	setQwenHeaders(req, "", token)
	resp, err := qwenClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("Qwen chat creation connection error: %s", err.Error())
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if isQwenWAFChallenge(resp.Header.Get("Content-Type"), respBody) {
		return "", fmt.Errorf("Qwen chat creation returned WAF challenge page (%d)", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("Qwen chat creation failed %d: %s", resp.StatusCode, truncateForLog(respBody, 200))
	}
	var data struct {
		Success bool `json:"success"`
		Data    struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &data); err != nil || !data.Success || data.Data.ID == "" {
		return "", fmt.Errorf("Qwen chat creation bad response: %s", truncateForLog(respBody, 200))
	}
	rememberChatToken(data.Data.ID, token)
	return data.Data.ID, nil
}

// DeleteQwenChat deletes one chat from the Qwen account (throwaway session
// cleanup). Idempotent: a 404 ("already gone") is treated as success.
func DeleteQwenChat(ctx context.Context, chatID string) error {
	if chatID == "" {
		return nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		// The deleting token must be the minting account (a foreign token
		// gets a 404), then the active pool token for pool-less chats.
		token := tokenForChat(chatID)
		if token == "" {
			token = sessionToken()
		}
		if token == "" {
			if err := initializeSession(); err != nil {
				return fmt.Errorf("session init for chat delete: %s", err.Error())
			}
			continue
		}

		urlStr := BASE_URL + "/api/v1/chats/" + chatID
		req, err := http.NewRequestWithContext(ctx, "DELETE", urlStr, nil)
		if err != nil {
			return fmt.Errorf("chat delete request build failed: %s", err.Error())
		}
		setQwenHeaders(req, "", token)

		resp, err := qwenClient().Do(req)
		if err != nil {
			return fmt.Errorf("Qwen chat delete connection error: %s", err.Error())
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		text := strings.TrimSpace(string(body))

		switch {
		case resp.StatusCode == 401:
			// A minting token that went invalid mid-flight: bench it
			// permanently (pool mode) and let the chat die as "already gone"
			// — the account that owns it cannot authorize the delete.
			disableToken(token)
			forgetChatToken(chatID)
			return nil
		case resp.StatusCode == 404 || strings.Contains(text, "could not find"):
			forgetChatToken(chatID)
			return nil
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			forgetChatToken(chatID)
			return nil
		default:
			return fmt.Errorf("Qwen chat delete failed: %d: %s", resp.StatusCode, text)
		}
	}
	return errors.New("chat delete: max retries exceeded")
}

// truncateForLog clips a byte slice for an error message.
func truncateForLog(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}

// ============================================================================
// QWEN MODEL RESOLUTION
// ============================================================================

// resolveQwenModel maps a client-facing model name to the id Qwen's
// /api/v2/chat/completions accepts. OpenAI-ish names (gpt-4o etc.) land on
// the default model; qwen3.x ids pass through unchanged.
func resolveQwenModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case m == "":
		return qwenDefaultModel
	case strings.Contains(m, "max"):
		return "qwen3.8-max"
	case strings.Contains(m, "flash"):
		return "qwen3.5-plus"
	case strings.Contains(m, "coder") || strings.Contains(m, "code"):
		return "qwen3-coder-plus"
	case strings.Contains(m, "qwen"):
		return model // pass qwen models through directly
	default:
		return qwenDefaultModel
	}
}

// ============================================================================
// QWEN COMMUNICATION
// ============================================================================

func sendToQwen(prompt string, opts QwenSendOptions) (<-chan QwenResult, error) {
	session.mu.Lock()
	initialized := session.Initialized
	session.mu.Unlock()

	model := resolveQwenModel(opts.Model)

	if !initialized {
		if err := initializeSession(); err != nil {
			return nil, err
		}
	}

	resolvedOpts := struct {
		Model, ChatID     string
		FeaturesMap       map[string]interface{}
		Messages          []Message
		ClientMessagesRaw json.RawMessage
		RequestID         string
	}{
		Model:             model,
		ChatID:            opts.ChatID,
		FeaturesMap:       map[string]interface{}{},
		Messages:          opts.Messages,
		ClientMessagesRaw: opts.ClientMessagesRaw,
		RequestID:         opts.RequestID,
	}
	ch := make(chan QwenResult, 100)
	go func() {
		defer close(ch)
		err := sendToQwenStream(prompt, resolvedOpts, ch)
		if err != nil {
			ch <- QwenResult{Err: err}
		}
	}()
	return ch, nil
}

func sendToQwenStream(prompt string, opts struct {
	Model, ChatID     string
	FeaturesMap       map[string]interface{}
	Messages          []Message
	ClientMessagesRaw json.RawMessage
	RequestID         string
}, ch chan<- QwenResult) error {

	wafRetries := 3 // WAF HTML challenges: retry with backoff + fresh X-Request-Id

	for attempt := 0; ; attempt++ {
		if attempt > wafRetries {
			return fmt.Errorf("Qwen retries exceeded (WAF)")
		}

		session.mu.Lock()
		feVersion := session.FeVersion
		session.mu.Unlock()
		if feVersion == "" {
			feVersion = qwenDefaultFEVersion
		}

		// Chat session: pooled (stateless throwaway), explicit, or on-demand.
		chatID := opts.ChatID
		if chatID == "" {
			var err error
			chatID, err = CreateQwenChat(opts.Model)
			if err != nil {
				return fmt.Errorf("Qwen chat session create failed: %s", err.Error())
			}
			defer DeleteQwenChat(context.Background(), chatID)
		}

		// The token this completion must travel with: the minting account
		// of the chat (pool chats are minted on the sticky-active token), or
		// the active token for sync-mode client-minted UUIDs. When the
		// minting token has been benched (daily cap) and another account is
		// active, re-mint the chat on that account and retry.
		reqToken, err := tokenForRequest(chatID)
		if err != nil {
			if errors.As(err, new(errChatTokenBenched)) {
				log.Printf("[TokenPool] chat %s was minted on a rate-limited token — re-minting on active token", chatID)
				oldChat := chatID
				chatID, err = CreateQwenChat(opts.Model)
				if err != nil {
					return fmt.Errorf("Qwen chat re-mint failed: %s", err.Error())
				}
				defer DeleteQwenChat(context.Background(), oldChat)
				reqToken, err = tokenForRequest(chatID)
			}
			if err != nil {
				return err
			}
		}

		// Build the message array, Qwen-shaped (browser metadata fields).
		// The exact field set mirrors the verified-working browser request:
		// fid carries a real UUID, feature_config + extra.meta ride every
		// message, and parentId is an EMPTY STRING at the top level while
		// parent_id stays null — the WAF/backend punishes deviations.
		// Agent-mode folds the whole conversation into one prompt; plain
		// OpenAI requests forward their message list, the last message being
		// the new user turn.
		qwenMsgs := make([]map[string]interface{}, 0, len(opts.Messages)+1)
		appendMsg := func(text string) {
			qwenMsgs = append(qwenMsgs, map[string]interface{}{
				"fid":         randomUUID(),
				"parentId":    nil,
				"childrenIds": []interface{}{},
				"role":        "user",
				"content":     text,
				"user_action": "chat",
				"files":       []interface{}{},
				"timestamp":   time.Now().Unix(),
				"models":      []string{opts.Model},
				"chat_type":   "t2t",
				"feature_config": map[string]interface{}{
					"thinking_enabled": true,
					"output_schema":    "phase",
					"research_mode":    "advance",
					"auto_thinking":    false,
					"thinking_mode":    "Thinking",
					"thinking_format":  "summary",
					"auto_search":      true,
				},
				"extra": map[string]interface{}{
					"meta": map[string]interface{}{"subChatType": "t2t"},
				},
				"sub_chat_type": "t2t",
				"parent_id":     nil,
				"id":            nil,
			})
		}
		appendMsg(prompt)

		requestBody := map[string]interface{}{
			"stream":             true,
			"version":            "2.1",
			"incremental_output": true,
			"chatId":             chatID,
			"chat_id":            chatID,
			"parentId":           "",
			"chat_mode":          "normal",
			"model":              opts.Model,
			"parent_id":          nil,
			"messages":           qwenMsgs,
			"timestamp":          time.Now().Unix(),
		}
		bodyBytes, _ := json.Marshal(requestBody)

		urlStr := BASE_URL + "/api/v2/chat/completions?chat_id=" + chatID
		ctx, cancel := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(ctx, "POST", urlStr, bytes.NewReader(bodyBytes))
		if err != nil {
			cancel()
			return fmt.Errorf("Qwen connection error: %s", err.Error())
		}
		setQwenHeaders(req, feVersion, reqToken)
		if !apkTransportEnabled() {
			req.Header.Set("Referer", BASE_URL+"/c/"+chatID)
		}

		// XFF is IGNORED by the qwen WAF (probe 2026-09-11: 24 fresh
		// foreign XFF values all failed while the real egress IP was
		// punished, and a blocklisted canary had no effect either — the
		// WAF keys on the REAL connection IP only). Default OFF; set
		// QWEN_XFF=on to send it anyway (operator opt-in).
		if os.Getenv("QWEN_XFF") == "on" {
			if xffIP := qwenXffIP(); xffIP != "" {
				req.Header.Set("X-Forwarded-For", xffIP)
			}
		}

		resp, err := qwenClient().Do(req)
		if err != nil {
			cancel()
			if attempt == 0 && isDeadConnError(err.Error()) {
				log.Printf("[Retry] Qwen dead pooled connection on write (%s) — retrying on a fresh connection", err.Error())
				continue
			}
			return fmt.Errorf("Qwen connection error: %s", err.Error())
		}

		if resp.StatusCode == 401 {
			resp.Body.Close()
			cancel()
			// Pool mode: this account is dead (expired/revoked JWT). Bench
			// it permanently and re-mint the chat on the next active token;
			// when the whole pool is 401 the request fails fast below.
			disableToken(reqToken)
			if tokenPool != nil && tokenPool.ReadyCount() > 0 {
				log.Printf("[TokenPool] token 401 — re-minting chat on next active token")
				_ = DeleteQwenChat(context.Background(), chatID)
				chatID, err = CreateQwenChat(opts.Model)
				if err != nil {
					return err
				}
				defer DeleteQwenChat(context.Background(), chatID)
				reqToken, err = tokenForRequest(chatID)
				if err != nil {
					return err
				}
				continue
			}
			session.mu.Lock()
			session.Initialized = false
			session.mu.Unlock()
			if err := initializeSession(); err != nil {
				return err
			}
			continue
		}

		ct := resp.Header.Get("Content-Type")
		if strings.Contains(ct, "text/html") {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			cancel()
			log.Printf("[WAF] Qwen challenge page on completions (attempt %d/%d) — backing off and retrying", attempt+1, wafRetries+1)
			if os.Getenv("QWEN_XFF") == "on" {
				rotateQwenXffIP()
			}
			// On qwen the challenge page is an egress-IP punish signal (the
			// WAF keys on the REAL connection IP — probe 2026-09-11), so use
			// the same long backoff as the silent-punish path: fast retries
			// only re-trip the window.
			time.Sleep(time.Duration(attempt+1) * 15 * time.Second)
			continue
		}
		if !strings.Contains(ct, "application/json") && resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			cancel()
			return fmt.Errorf("Qwen error %d: %s", resp.StatusCode, truncateForLog(body, 300))
		}

		// 200 with JSON body on a streaming route = an error-in-200 (WAF
		// punish RGV587, a stale Version signature Bad_Request, or the
		// account-level daily-usage RateLimited).
		if strings.Contains(ct, "application/json") {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			cancel()
			// Daily-cap on this account: bench the token for the reported
			// window and re-mint the chat on the next active token. When the
			// whole pool is cooling the request fails with a clear message.
			var rl struct {
				Data struct {
					Code string `json:"code"`
					Num  int    `json:"num"`
				} `json:"data"`
			}
			if err := json.Unmarshal(body, &rl); err == nil && rl.Data.Code == "RateLimited" {
				wait := time.Duration(rl.Data.Num) * time.Hour
				if wait <= 0 {
					wait = 10 * time.Hour // server omitted num; documented cap
				}
				if cooldownToken(reqToken, wait) {
					log.Printf("[TokenPool] token hit daily limit — benched for %s", wait)
				}
				if tokenPool != nil && tokenPool.ReadyCount() > 0 {
					_ = DeleteQwenChat(context.Background(), chatID)
					chatID, err = CreateQwenChat(opts.Model)
					if err != nil {
						return err
					}
					defer DeleteQwenChat(context.Background(), chatID)
					reqToken, err = tokenForRequest(chatID)
					if err != nil {
						return err
					}
					continue
				}
				if tokenPool != nil {
					return fmt.Errorf("Qwen daily usage limit reached on all %d tokens; earliest reset %s", tokenPool.Len(), tokenPool.NextReset().Format("15:04"))
				}
				// single-token: surface as 429 (statusFromError maps it)
				return fmt.Errorf("Qwen error (200 JSON): %s", truncateForLog(body, 300))
			}
			if qwenWAFMarkers.Match(body) {
				log.Printf("[WAF] Qwen punish-in-JSON (attempt %d/%d) — retrying", attempt+1, wafRetries+1)
				time.Sleep(time.Duration(attempt+1) * 1500 * time.Millisecond)
				continue
			}
			// Stale Version header: Qwen bumps its frontend and completions
			// answer Bad_Request until the Version header matches. Force a
			// re-scrape; if the version changed, retry once with it.
			var j struct {
				Data struct {
					Code    string `json:"code"`
					Details string `json:"details"`
				} `json:"data"`
			}
			if err := json.Unmarshal(body, &j); err == nil && strings.Contains(strings.ToLower(j.Data.Code), "bad_request") {
				prev := getQwenFEVersion()
				scrapeQwenFEVersion()
				if getQwenFEVersion() != prev {
					log.Printf("[Version] Completions rejected with stale Version=%s — retrying with %s", prev, getQwenFEVersion())
					continue
				}
				return fmt.Errorf("Qwen rejected request (Bad_Request): %s", truncateForLog(body, 300))
			}
			return fmt.Errorf("Qwen error (200 JSON): %s", truncateForLog(body, 300))
		}

		// SSE stream — guarded by the stall-aware reader, same policy as the
		// Qwen bridge: no total-duration cap, only idle deadlines.
		var stallBody io.Reader = resp.Body
		var stallGuard *extendingReader
		if config.StallTimeout > 0 {
			sr := newExtendingReader(resp.Body, resp.Body, time.Duration(config.StallTimeout)*time.Second)
			stallBody = sr
			stallGuard = sr
		}
		release := func() {
			if stallGuard != nil {
				stallGuard.Close()
			} else {
				resp.Body.Close()
			}
		}

		err = streamSSEResponse(stallBody, ch, opts.RequestID)
		release()
		cancel()
		if errors.Is(err, errQwenSilentPunish) && attempt < wafRetries {
			// Longer backoff than the HTML-challenge path: silent punish
			// means the real egress IP exhausted its ~40-req budget; an
			// immediate retry only re-trips it. 15s/30s/45s per attempt.
			wait := time.Duration(attempt+1) * 15 * time.Second
			log.Printf("[WAF] Qwen silent punish (attempt %d/%d) — backing off %s and retrying", attempt+1, wafRetries+1, wait)
			time.Sleep(wait)
			continue
		}
		return err
	}
}

// streamSSEResponse parses Qwen's SSE stream. Phases:
//   - "thinking_summary" (status typing): reasoning text rides
//     delta.extra.summary_thought.content (array of lines) → Reasoning delta
//   - delta.content: answer text → content delta
//   - [DONE] / end of stream: final flush
func streamSSEResponse(body io.Reader, ch chan<- QwenResult, requestId string) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var fullText strings.Builder      // accumulated answer text
	contentEmitter := &sseEmitter{}   // tracks what the client has received
	reasoningEmitter := &sseEmitter{} // same for the reasoning channel
	sawDataEvent := false             // any "data: " line seen (silent-punish check)

	emitContent := func(target string) {
		if delta := contentEmitter.delta(target); delta != "" {
			if config.Logging.Level == "debug" {
				log.Printf("[DEBUG][%s] emitContent delta=%q (len=%d)", requestId, delta, len(delta))
			}
			ch <- QwenResult{Chunk: delta, FullText: target}
		}
	}
	emitReasoning := func(target string) {
		if delta := reasoningEmitter.delta(target); delta != "" {
			ch <- QwenResult{Reasoning: delta}
		}
	}

	// While the stream is live, keep a small tail pending (same policy as the
	// Qwen bridge) so trailing structural fragments are never forwarded torn.
	flush := func(final bool) {
		target := fullText.String()
		if !final {
			target = holdBackTailSafe(target, config.StreamHoldback)
		}
		emitContent(target)
	}

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if config.Logging.Level == "debug" && trimmed != "" {
			log.Printf("[DEBUG][%s] Qwen SSE line: %s", requestId, trimmed)
		}

		if !strings.HasPrefix(trimmed, "data: ") {
			continue
		}
		dataStr := trimmed[6:]
		sawDataEvent = true
		if dataStr == "[DONE]" {
			flush(true)
			return nil
		}

		var j map[string]interface{}
		if err := json.Unmarshal([]byte(dataStr), &j); err != nil {
			if config.Logging.Level == "debug" {
				log.Printf("[DEBUG][%s] Qwen failed to parse SSE: %s", requestId, dataStr)
			}
			continue
		}

		// Inline error-in-SSE (Qwen error payload inside a data event).
		if errDetail := extractQwenError(j); errDetail != "" {
			return fmt.Errorf("Qwen error: %s", errDetail)
		}

		// response.created carries the new parent_id for threading.
		if created, ok := j["response.created"].(map[string]interface{}); ok {
			_ = created // threading handled at the pool layer (stateless: ignore)
			continue
		}

		choices, ok := j["choices"].([]interface{})
		if !ok || len(choices) == 0 {
			continue
		}
		choice, ok := choices[0].(map[string]interface{})
		if !ok {
			continue
		}
		delta, ok := choice["delta"].(map[string]interface{})
		if !ok {
			continue
		}

		// Reasoning channel: thinking_summary phase.
		if phase, _ := delta["phase"].(string); phase == "thinking_summary" {
			if status, _ := delta["status"].(string); status == "typing" {
				if extra, ok := delta["extra"].(map[string]interface{}); ok {
					if st, ok := extra["summary_thought"].(map[string]interface{}); ok {
						if lines, ok := st["content"].([]interface{}); ok && len(lines) > 0 {
							parts := make([]string, 0, len(lines))
							for _, l := range lines {
								if s, ok := l.(string); ok {
									parts = append(parts, s)
								}
							}
							emitReasoning(strings.Join(parts, "\n"))
						}
					}
				}
			}
			continue
		}

		if content, ok := delta["content"].(string); ok && content != "" {
			fullText.WriteString(content)
			flush(false)
		}
	}

	flush(true)
	if !sawDataEvent {
		return errQwenSilentPunish
	}
	return scanner.Err()
}

// extractQwenError pulls a human-readable error out of a Qwen SSE payload
// when the stream carries an error object instead of content.
func extractQwenError(j map[string]interface{}) string {
	// {"error": {"message": ...}} or {"data": {"code": ..., "details": ...}}
	if e, ok := j["error"].(map[string]interface{}); ok {
		if msg, ok := e["message"].(string); ok && msg != "" {
			return msg
		}
	}
	if d, ok := j["data"].(map[string]interface{}); ok {
		code, _ := d["code"].(string)
		details, _ := d["details"].(string)
		if code != "" || details != "" {
			return strings.TrimSpace(code + " " + details)
		}
	}
	return ""
}

// decodeJWT extracts id and email-name from a Qwen JWT payload.
func decodeJWT(token string) (id, name string) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", ""
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Try padded fallback.
		decoded, err = base64.StdEncoding.DecodeString(parts[1] + "==")
		if err != nil {
			return "", ""
		}
	}
	var data map[string]interface{}
	if err := json.Unmarshal(decoded, &data); err != nil {
		return "", ""
	}
	id, _ = data["id"].(string)
	if id == "" {
		// Qwen JWTs carry the user id under "sub" as well.
		id, _ = data["sub"].(string)
	}
	email, _ := data["email"].(string)
	name = "User"
	if email != "" {
		name = strings.Split(email, "@")[0]
	}
	return id, name
}

// statusFromError maps a bridge error message to the HTTP status the OpenAI
// client should see (ported from the Qwen transport).
func statusFromError(errMsg string) int {
	switch {
	case strings.Contains(errMsg, "401"):
		return 401
	case strings.Contains(errMsg, "403"):
		return 403
	case strings.Contains(errMsg, "404"):
		return 404
	case strings.Contains(errMsg, "405"):
		return 405
	case strings.Contains(errMsg, "RateLimited") || strings.Contains(errMsg, "daily usage limit"):
		// Account-level quota (probe 2026-09-11: keys on the JWT, not the
		// egress IP — same token hits the cap from two different IPs).
		return 429
	case strings.Contains(errMsg, "429"):
		return 429
	case strings.Contains(errMsg, "400"):
		return 400
	case strings.Contains(errMsg, "502"):
		return 502
	case strings.Contains(errMsg, "503"):
		return 503
	case strings.Contains(errMsg, "504"):
		return 504
	default:
		return 500
	}
}

// isRetryableUpstreamStatus reports whether a Qwen HTTP status is a transient
// server-side failure worth a single retry.
func isRetryableUpstreamStatus(code int) bool {
	switch code {
	case 500, 502, 503, 504:
		return true
	}
	return false
}
