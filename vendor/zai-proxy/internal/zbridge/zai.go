// Code moved from the original main.go monolith during the internal/ restructure.
// See README "Project Structure". Part of the Z.AI bridge core (package zbridge).

package zbridge

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// ============================================================================
// Z.AI SIGNATURE GENERATION
// ============================================================================

func generateZaSignature(prompt, token, userID string) (signature, timestamp, urlParams string) {
	tsMs := time.Now().UnixMilli()
	timestamp = strconv.FormatInt(tsMs, 10)
	requestId := randomUUID()
	bucket := tsMs / 300000

	mac := hmac.New(sha256.New, []byte(session.SaltKey))
	mac.Write([]byte(strconv.FormatInt(bucket, 10)))
	wKey := hex.EncodeToString(mac.Sum(nil))

	type kv struct{ k, v string }
	payloadDict := []kv{
		{"requestId", requestId},
		{"timestamp", timestamp},
		{"user_id", userID},
	}
	sort.Slice(payloadDict, func(i, j int) bool {
		return payloadDict[i].k < payloadDict[j].k
	})
	var parts []string
	for _, p := range payloadDict {
		parts = append(parts, p.k+","+p.v)
	}
	sortedPayload := strings.Join(parts, ",")

	promptB64 := base64.StdEncoding.EncodeToString([]byte(strings.TrimSpace(prompt)))
	dataToSign := sortedPayload + "|" + promptB64 + "|" + timestamp

	mac2 := hmac.New(sha256.New, []byte(wKey))
	mac2.Write([]byte(dataToSign))
	signature = hex.EncodeToString(mac2.Sum(nil))

	params := url.Values{}
	params.Set("timestamp", timestamp)
	params.Set("requestId", requestId)
	params.Set("user_id", userID)
	params.Set("version", "0.0.1")
	params.Set("platform", "web")
	params.Set("token", token)
	params.Set("user_agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/120.0.0.0")
	params.Set("language", "en-US")
	params.Set("screen_resolution", "1920x1080")
	params.Set("viewport_size", "1920x1080")
	params.Set("timezone", "Europe/Paris")
	params.Set("timezone_offset", "-60")
	params.Set("signature_timestamp", timestamp)
	urlParams = params.Encode()

	return
}

// ============================================================================
// JWT DECODE
// ============================================================================

func decodeJWT(token string) (id, name string) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", ""
	}
	decoded, err := base64Decode(parts[1])
	if err != nil {
		return "", ""
	}
	var data map[string]interface{}
	if err := json.Unmarshal(decoded, &data); err != nil {
		return "", ""
	}
	id, _ = data["id"].(string)
	email, _ := data["email"].(string)
	name = "Guest"
	if email != "" {
		name = strings.Split(email, "@")[0]
	}
	return id, name
}

// ============================================================================
// Z.AI SESSION INITIALIZATION
// ============================================================================

func scrapeConfig() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", BASE_URL, nil)
	if err != nil {
		log.Printf("[Config] Scrape error: %s, using default feVersion", err.Error())
		return
	}
	resp, err := zaiHTTPClient.Do(req)
	if err != nil {
		log.Printf("[Config] Scrape error: %s, using default feVersion", err.Error())
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if match := feVersionRe.FindString(string(body)); match != "" {
		session.mu.Lock()
		session.FeVersion = match
		session.mu.Unlock()
		log.Printf("[Config] fe_version: %s", match)
	}
}

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

	if config.ZaiToken != "" {
		log.Println("[Session] Using hardcoded ZAI_TOKEN, skipping guest init.")
		session.Token = config.ZaiToken
		id, name := decodeJWT(session.Token)
		session.UserID = id
		if name != "" {
			session.UserName = name
		}
		if session.UserID == "" {
			session.UserName = "User"
		}
		uidPreview := session.UserID
		if len(uidPreview) > 8 {
			uidPreview = uidPreview[:8]
		}
		log.Printf("[Session] Token user: %s... (%s)", uidPreview, session.UserName)
		session.Initialized = true
		// The token path used to return early, so FeVersion stayed at the
		// compiled-in DEFAULT_FE_VERSION forever. Z.AI rolls the site fe build
		// regularly (observed 1.1.88 → 1.1.93 overnight), and Aliyun's WAF
		// starts answering stale x-fe-Version values with 405 block pages.
		// Scrape is idempotent and cheap (one GET), and a failed scrape keeps
		// the current value, so run it on every init — hardcoded-token or not.
		scrapeConfig()
		// Z.AI's /api/v2/chat/completions started rejecting requests whose
		// cookie jar is empty even when the Bearer token itself is valid
		// (verified 2026-09-07: /api/v1/auths/ returns 200 for the token while
		// v2 chat returns 401 until the auth endpoints are hit once so their
		// Set-Cookie lands in zaiJar). The guest flow gets these cookies for
		// free via its /auths/guest + GET /auths/ sequence; the hardcoded-token
		// flow must do the same GET /auths/ with the token attached, or every
		// v2 chat call 401s and retries itself to death. Failure here is
		// non-fatal: v2 chat may still work for tokens that don't need cookies.
		ctxAuth, cancelAuth := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancelAuth()
		reqAuth, err := http.NewRequestWithContext(ctxAuth, "GET", BASE_URL+"/api/v1/auths/", nil)
		if err == nil {
			reqAuth.Header.Set("Origin", BASE_URL)
			reqAuth.Header.Set("Referer", BASE_URL+"/")
			reqAuth.Header.Set("User-Agent", zaiUserAgent)
			if authResp, err := zaiHTTPClient.Do(reqAuth); err == nil {
				io.Copy(io.Discard, authResp.Body)
				authResp.Body.Close()
				log.Printf("[Session] Auth warmup for token session: /auths/ %d", authResp.StatusCode)
			} else {
				log.Printf("[Session] Auth warmup request failed (non-fatal): %s", err.Error())
			}
		}
		return nil
	}

	log.Println("[Session] Initializing Z.AI session...")

	scrapeConfig()

	headers := map[string]string{
		"Origin":       BASE_URL,
		"Referer":      BASE_URL + "/",
		"User-Agent":   zaiUserAgent,
		"Content-Type": "application/json",
	}

	// Initial guest POST (fire-and-forget)
	ctx1, cancel1 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel1()
	req1, _ := http.NewRequestWithContext(ctx1, "POST", BASE_URL+"/api/v1/auths/guest", strings.NewReader("{}"))
	for k, v := range headers {
		req1.Header.Set(k, v)
	}
	zaiHTTPClient.Do(req1)

	// GET /api/v1/auths/
	ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel2()
	req2, _ := http.NewRequestWithContext(ctx2, "GET", BASE_URL+"/api/v1/auths/", nil)
	for k, v := range headers {
		req2.Header.Set(k, v)
	}
	resp, err := zaiHTTPClient.Do(req2)
	if err != nil {
		log.Printf("[Session] Initialization error: %s", err.Error())
		session.Initialized = false
		return err
	}

	if resp.StatusCode != 200 {
		resp.Body.Close()
		err := fmt.Errorf("Auth failed: %d", resp.StatusCode)
		log.Printf("[Session] Initialization error: %s", err.Error())
		session.Initialized = false
		return err
	}

	var authData struct {
		Token string `json:"token"`
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	json.Unmarshal(body, &authData)
	session.Token = authData.Token

	if session.Token == "" {
		ctx3, cancel3 := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel3()
		req3, _ := http.NewRequestWithContext(ctx3, "POST", BASE_URL+"/api/v1/auths/guest", strings.NewReader("{}"))
		for k, v := range headers {
			req3.Header.Set(k, v)
		}
		guestResp, err := zaiHTTPClient.Do(req3)
		if err == nil {
			var gd struct {
				Token string `json:"token"`
			}
			gb, _ := io.ReadAll(guestResp.Body)
			guestResp.Body.Close()
			json.Unmarshal(gb, &gd)
			session.Token = gd.Token
		}
	}

	if session.Token != "" {
		id, name := decodeJWT(session.Token)
		session.UserID = id
		if name != "" {
			session.UserName = name
		}
		uidPreview := session.UserID
		if len(uidPreview) > 8 {
			uidPreview = uidPreview[:8]
		}
		log.Printf("[Session] Connected. UserID: %s... (%s)", uidPreview, session.UserName)
		session.Initialized = true
		return nil
	}

	session.Initialized = false
	return errors.New("No token received from Z.AI")
}

// ============================================================================
// Z.AI COMMUNICATION
// ============================================================================

func sendToZAI(prompt string, opts SendOptions) (<-chan ZAIResult, error) {
	session.mu.Lock()
	defaultChatID := session.ChatID
	defaultMessages := session.Messages
	initialized := session.Initialized
	session.mu.Unlock()

	model := opts.Model
	if model == "" {
		model = "glm-5"
	}

	// Map the client-facing id to the id Z.AI's /api/v2/chat/completions
	// actually accepts (e.g. GLM-5.3-Flash is served under the preview id
	// "x-preview-l"; sending the raw id returns a 500). Resolving here also
	// lets the feature/capability lookups below key off the real Z.AI id.
	model = resolveZAIModelID(model)

	// Resolve features dynamically from per-model state
	// (server defaults + stored user overrides)
	featuresMap := resolveFeaturesForModel(model)

	// Apply per-request overrides (highest precedence)
	if opts.WebSearch != nil {
		if *opts.WebSearch {
			featuresMap["auto_web_search"] = true
			featuresMap["web_search"] = true
		} else {
			delete(featuresMap, "auto_web_search")
			delete(featuresMap, "web_search")
		}
	}
	if opts.Thinking != nil {
		featuresMap["enable_thinking"] = *opts.Thinking
	}
	if opts.ImageGen != nil {
		featuresMap["image_generation"] = *opts.ImageGen
	}
	if opts.PreviewMode != nil {
		featuresMap["preview_mode"] = *opts.PreviewMode
	}

	// ── reasoning_effort handling ──
	// Defensive: always strip any stale reasoning_effort first so unsupported
	// models never receive a placeholder value (would cause malfunction).
	delete(featuresMap, "reasoning_effort")

	if opts.ReasoningEffort != "" {
		if modelSupportsReasoningEffort(model) {
			if isValidReasoningEffort(opts.ReasoningEffort) {
				// Forward reasoning_effort INSIDE the features payload
				featuresMap["reasoning_effort"] = opts.ReasoningEffort
				// When reasoning_effort is active, enable_thinking MUST be true
				// and any user modification on enable_thinking is ignored.
				featuresMap["enable_thinking"] = true
				logInfo(fmt.Sprintf(
					"[reasoning_effort] model=%s effort=%s enabled (enable_thinking forced true)",
					model, opts.ReasoningEffort))
			} else {
				logError(fmt.Sprintf(
					"[reasoning_effort] invalid value '%s' for model=%s (accepted: high, max); ignored",
					opts.ReasoningEffort, model))
			}
		} else {
			logInfo(fmt.Sprintf(
				"[reasoning_effort] model=%s does not support reasoning_effort; parameter ignored",
				model))
		}
	}

	// Remove 'think' entirely; ALWAYS force image_generation to false
	delete(featuresMap, "think")
	featuresMap["image_generation"] = false

	chatID := opts.ChatID
	if chatID == "" {
		chatID = defaultChatID
	}
	messages := opts.Messages
	if messages == nil {
		messages = defaultMessages
	}

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
		Files             []map[string]interface{}
		RequestID         string
	}{
		Model:             model,
		ChatID:            chatID,
		FeaturesMap:       featuresMap,
		Messages:          messages,
		ClientMessagesRaw: opts.ClientMessagesRaw,
		Files:             opts.Files,
		RequestID:         opts.RequestID,
	}

	ch := make(chan ZAIResult, 100)
	go func() {
		defer close(ch)
		err := sendToZAIStream(prompt, resolvedOpts, ch)
		if err != nil {
			ch <- ZAIResult{Err: err}
		}
	}()
	return ch, nil
}

func sendToZAIStream(prompt string, opts struct {
	Model, ChatID     string
	FeaturesMap       map[string]interface{}
	Messages          []Message
	ClientMessagesRaw json.RawMessage
	Files             []map[string]interface{}
	RequestID         string
}, ch chan<- ZAIResult) error {

	// sawCapacityError marks that this request hit MODEL_CONCURRENCY_LIMIT
	// at least once — the deferred ReportClean for the gate must not fire
	// for a request that only got through after capacity rejects.
	sawCapacityError := false

	// Local AIMD concurrency gate: keep this proxy's in-flight
	// completions at or below the adaptive limit so a burst queues locally
	// (milliseconds) instead of being rejected remotely by
	// MODEL_CONCURRENCY_LIMIT (a paced round-trip + a growing 2s-step
	// backoff every reject). See concurrency_gate.go. In mock/CI mode
	// (BASE_URL != production) the gate is a no-op.
	if gateEnabled() {
		gateTok := uGate.Acquire(upstreamAcquireTimeout)
		defer uGate.Release(gateTok)
	}

	// wafRotations counts WAF-405-driven XFF rotations within one logical
	// request. The loop's attempt counter stays reserved for semantic
	// retries (401 re-init, 5xx backoff, captcha regen); a 405 rotation only
	// changes the egress address, so it must not consume an attempt — the
	// Aliyun WAF blocklists whole prefixes at the head of the pool (observed
	// live 2026-09-05: 1.0.0.1…149.112.112.112 all 405), and with the old
	// single-rotation budget every request until the head was burned failed
	// client-visibly. A full pool sweep is bounded and each 405 returns in
	// ~1s, so worst case is one quick sweep before the error surfaces.
	wafRotations := 0
	maxWafRotations := len(xffPool())
	// capacityRetries counts MODEL_CONCURRENCY_LIMIT waits within one logical
	// request (see modelCapacityError). Observed live on glm-5.3 (2026-09-05):
	// the burst can last tens of seconds (~50s in the worst e2e run), far
	// beyond the original 3×1.5s budget. A growing 2s-step backoff (2..14s,
	// ~56s total) rides out a long burst; the keep-alive ticker keeps the
	// client fed while waiting, and each retry re-acquires a fresh captcha
	// param via the loop head.
	capacityRetries := 0
	maxCapacityRetries := 7

	for attempt := 0; attempt < 2; attempt++ {
		session.mu.Lock()
		token := session.Token
		userID := session.UserID
		feVersion := session.FeVersion
		session.mu.Unlock()

		signature, _, _ := generateZaSignature(prompt, token, userID)
		urlStr := BASE_URL + "/api/v2/chat/completions"

		var messagesField interface{}
		if len(opts.ClientMessagesRaw) > 0 {
			messagesField = json.RawMessage(opts.ClientMessagesRaw)
		} else {
			forwarded := make([]Message, 0, len(opts.Messages)+1)
			forwarded = append(forwarded, opts.Messages...)
			promptJSON, _ := json.Marshal(prompt)
			forwarded = append(forwarded, Message{Role: "user", Content: json.RawMessage(promptJSON)})
			messagesField = forwarded
		}

		captchaParam, err := getCaptchaVerifyParam()
		if err != nil {
			return err
		}

		// Build features payload from dynamically resolved per-model features.
		// reasoning_effort is only present in opts.FeaturesMap if the model supports
		// it AND a valid value was provided — no placeholder is added for
		// unsupported models (would cause malfunction).
		featuresPayload := make(map[string]interface{})
		for k, v := range opts.FeaturesMap {
			featuresPayload[k] = v
		}
		// Remove 'think' entirely — only enable_thinking reaches the request
		delete(featuresPayload, "think")
		featuresPayload["flags"] = []interface{}{}
		// image_generation is ALWAYS false
		featuresPayload["image_generation"] = false

		requestBody := map[string]interface{}{
			"model":                opts.Model,
			"chat_id":              opts.ChatID,
			"messages":             messagesField,
			"signature_prompt":     prompt,
			"stream":               true,
			"captcha_verify_param": captchaParam,
			"features":             featuresPayload,
		}

		// Attach uploaded files (images) only when present - text-only requests
		// keep the exact body shape they always had.
		if len(opts.Files) > 0 {
			requestBody["files"] = opts.Files
		}

		bodyBytes, _ := json.Marshal(requestBody)

		// Capture the XFF IP once per attempt so the debug log, the actual
		// request header, and the 405 bad-IP marker all refer to the same
		// address. zaiXffIP() is otherwise idempotent within a goroutine,
		// but resolving it once avoids any chance of drift between calls.
		var currentXffIP string
		if albBypassEnabled() {
			currentXffIP = zaiXffIP()
		}

		if config.Logging.Level == "debug" {
			log.Println("[DEBUG] Z.AI url", urlStr)
			log.Println("[DEBUG] Z.AI request body:", string(bodyBytes))
			// Never log the raw token/credentials — show a short preview only.
			authPreview := token
			if len(authPreview) > 12 {
				authPreview = authPreview[:8] + "..." + authPreview[len(authPreview)-4:]
			}
			hdrMap := map[string]string{
				"authorization": "Bearer " + authPreview,
				"content-type":  "application/json",
				"x-fe-Version":  feVersion,
				"x-region":      "overseas",
				"x-signature":   signature,
			}
			if currentXffIP != "" {
				hdrMap["X-Forwarded-For"] = currentXffIP
			}
			hdrJSON, _ := json.MarshalIndent(hdrMap, "", "  ")
			log.Println("[DEBUG] Z.AI request headers", string(hdrJSON))
		}

		// Timeout policy: NO fixed request-level deadline for streams.
		// The old code set context.WithTimeout(Timeouts.Default*2 = 600s),
		// which killed long generations mid-flight (large code files take
		// many minutes to stream) even though the stream was alive and
		// healthy. Liveness is now enforced by IDLE timeouts only:
		//   - connection phase: transport-level TLSHandshakeTimeout (15s)
		//     guards the dial/handshake
		//   - header/write phase: the headerArmed watchdog below cancels if
		//     headers haven't arrived within headerTimeout, then is DISARMED
		//     the moment they do
		//   - body phase: the stall guard (STALL_TIMEOUT, default 120s of
		//     SSE silence) plus the idle-deadline reader below, which resets
		//     its deadline on every received byte — an actively streaming
		//     generation is never cut off, a silent one dies fast.
		headerTimeout := 60 * time.Second
		if t := time.Duration(config.Timeouts.Default) * time.Millisecond; t > 0 && t < headerTimeout {
			headerTimeout = t
		}
		// A WithTimeout on the request ctx would govern the whole lifecycle
		// INCLUDING body reads (net/http wires ctx into the transport), so any
		// deadline here is a hard cap on total stream duration — the very bug
		// being fixed. Use a cancel-only ctx; the watchdog applies the deadline
		// to the header phase alone.
		ctx, cancel := context.WithCancel(context.Background())
		headerArmed := make(chan struct{})
		headerTimer := time.NewTimer(headerTimeout)
		go func() {
			select {
			case <-headerTimer.C:
				// Fired — headers may still have arrived in the same instant;
				// never kill a stream that already got its headers.
				select {
				case <-headerArmed:
					return
				default:
				}
				cancel()
			case <-headerArmed:
			case <-ctx.Done():
			}
		}()
		req, err := http.NewRequestWithContext(ctx, "POST", urlStr, bytes.NewReader(bodyBytes))
		if err != nil {
			cancel()
			return fmt.Errorf("Z.AI connection error: %s", err.Error())
		}
		req.Header.Set("authorization", "Bearer "+token)
		req.Header.Set("User-Agent", zaiUserAgent)
		req.Header.Set("content-type", "application/json")
		req.Header.Set("x-fe-Version", feVersion)
		req.Header.Set("x-region", "overseas")
		req.Header.Set("x-signature", signature)
		if currentXffIP != "" {
			req.Header.Set("X-Forwarded-For", currentXffIP)
		}

		resp, err := zaiHTTPClient.Do(req)
		if err != nil {
			cancel()
			// A pooled keep-alive connection the server closed between requests
			// fails during the write phase ("use of closed network connection" /
			// "connection reset by peer" / "broken pipe" — production log
			// 2026-09-09 23:19:50). The request never reached upstream, so
			// re-sending it on a fresh connection cannot double-execute; one
			// free retry keeps a routine pool churn from surfacing to the client.
			if attempt == 0 && isDeadConnError(err.Error()) {
				log.Printf("[Retry] Z.AI dead pooled connection on write (%s) — retrying on a fresh connection", err.Error())
				continue
			}
			return fmt.Errorf("Z.AI connection error: %s", err.Error())
		}
		// Headers arrived — disarm the header watchdog. From here the body is
		// guarded ONLY by the idle-aware extendingReader (no total-duration
		// cap), so a multi-minute generation can stream to completion.
		close(headerArmed)
		headerTimer.Stop()

		if config.Logging.Level == "debug" {
			log.Printf("[DEBUG] Z.AI response status: %d %s", resp.StatusCode, resp.Status)
			hdrs := map[string]string{}
			for k, v := range resp.Header {
				if strings.EqualFold(k, "Set-Cookie") {
					// Cookies may carry session credentials — log names only.
					redacted := make([]string, 0, len(v))
					for _, c := range v {
						if i := strings.IndexByte(c, '='); i >= 0 {
							redacted = append(redacted, c[:i]+"=...")
						} else {
							redacted = append(redacted, "...")
						}
					}
					hdrs[k] = strings.Join(redacted, ", ")
					continue
				}
				hdrs[k] = strings.Join(v, ", ")
			}
			hdrJSON, _ := json.MarshalIndent(hdrs, "", "  ")
			log.Println("[DEBUG] Z.AI response headers:", string(hdrJSON))
		}

		if resp.StatusCode == 401 {
			resp.Body.Close()
			cancel()
			session.mu.Lock()
			session.Initialized = false
			session.mu.Unlock()
			if err := initializeSession(); err != nil {
				return err
			}
			continue
		}

		// Aliyun's WAF periodically blocklists individual spoofed XFF
		// addresses (and whole prefixes at the head of the pool) and answers
		// them with a 405 HTML page. Nothing has been streamed to the client
		// yet, so rotate to the next pool address (see rotateXffIP) and retry
		// until a passing address is found or the sweep budget is exhausted.
		// Only meaningful with the ALB geo-bypass active, since that is the
		// only path that sends X-Forwarded-For.
		if resp.StatusCode == 405 && albBypassEnabled() {
			resp.Body.Close()
			cancel()
			// Quarantine the XFF address that just got WAF-blocked so future
			// requests skip it until xffBadTTL elapses; rotateXffIP then
			// picks the next good address. If the entire pool becomes bad
			// the bad-set is auto-cleared (see markXffIPBad) so the bridge
			// self-heals instead of surfacing 405 forever.
			if currentXffIP != "" {
				markXffIPBad(currentXffIP)
			}
			if wafRotations >= maxWafRotations {
				return fmt.Errorf("Z.AI error 405: WAF blocked every egress XFF address in the rotation pool (%d rotations)", wafRotations)
			}
			newIP := rotateXffIP()
			wafRotations++
			log.Printf("[Retry] Z.AI 405 (WAF blocked egress XFF %s) — rotated to %s, retrying (rotation %d)",
				currentXffIP, newIP, wafRotations)
			attempt-- // a WAF rotation is not a semantic retry; see wafRotations
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			cancel()
			if config.Logging.Level == "debug" {
				log.Println("[DEBUG] Z.AI error body:", string(errBody))
			}
			// Transient upstream 5xx (see isRetryableUpstreamStatus) is usually
			// a momentary Z.AI backend hiccup or a stale upstream session.
			// Nothing has been streamed to the client yet — the failure is at
			// the status check, before streamSSEResponse — so it is safe to
			// retry with a freshly signed request (new requestId/timestamp).
			// Surface the error to the client only after the retry is
			// exhausted: fclaude treats a surfaced bridge error as final and
			// does not retry on top of it.
			if isRetryableUpstreamStatus(resp.StatusCode) && attempt == 0 {
				log.Printf("[Retry] Z.AI upstream %d on model=%s — retrying in %s (attempt %d/2)",
					resp.StatusCode, opts.Model, upstreamRetryBackoff, attempt+1)
				time.Sleep(upstreamRetryBackoff)
				continue
			}
			return fmt.Errorf("Z.AI error %d: %s", resp.StatusCode, string(errBody))
		}

		// Wrap the response body with a stall-aware reader that monitors
		// for upstream silence. If no SSE byte arrives within StallTimeout
		// seconds the body is closed and Read returns immediately, breaking
		// the scanner loop and triggering a retry. The deadline RESETS on
		// every received byte (see extendingReader) — a stream that is
		// actively delivering (long code generation) is never cut off, only
		// true silence kills it. Before this fix the ctx was armed once for
		// the whole stream, so every generation longer than StallTimeout was
		// murdered mid-flight ("context deadline exceeded" on large files).
		var stallBody io.Reader = resp.Body
		var stallGuard *extendingReader
		if config.StallTimeout > 0 {
			sr := newExtendingReader(resp.Body, resp.Body, time.Duration(config.StallTimeout)*time.Second)
			stallBody = sr
			stallGuard = sr
		}
		// release disarms the stall guard (stopping its watchdog goroutine)
		// and closes the body. The guard's Close is idempotent, so a stall-kill
		// that already closed the body passes through harmlessly.
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
		if err != nil {
			// Captcha gate error-in-200 (FRONTEND_CAPTCHA_REQUIRED): Z.AI
			// consumed/rejected the captcha_verify_param (F018 = already used
			// or stale, F019 = verification failed; observed live in the
			// 2026-09-02 probes). Params are single-use, so the only recovery
			// is a fresh param + one re-send. Safe only while nothing has
			// been forwarded on ch (emitted=false guards a mid-stream surprise).
			if capErr, ok := err.(*captchaFlowError); ok && !capErr.emitted {
				log.Printf("[Retry] Z.AI captcha gate (%s%s) — regenerating param and retrying (attempt %d/2)",
					capErr.errorType, verifyCodeSuffix(capErr.verifyCode), attempt+1)
				if attempt == 0 {
					continue
				}
				return capErr
			}
			// Model-capacity error-in-200 (MODEL_CONCURRENCY_LIMIT): the model
			// is over its concurrent-request quota — a transient burst observed
			// live on glm-5.3/5.2 (2026-09-05) lasting a few seconds. The error
			// arrives before any content is streamed (emitted=false), so waiting
			// and re-sending is safe. Backoff grows per capacity retry so a
			// longer burst does not hammer the endpoint.
			if mErr, ok := err.(*modelCapacityError); ok && !mErr.emitted && capacityRetries < maxCapacityRetries {
				capacityRetries++
				sawCapacityError = true
				uGate.ReportCapacity()
				backoff := time.Duration(capacityRetries) * 2000 * time.Millisecond
				log.Printf("[Retry] Z.AI model at capacity (MODEL_CONCURRENCY_LIMIT) — retrying in %s (capacity retry %d/%d)",
					backoff, capacityRetries, maxCapacityRetries)
				time.Sleep(backoff)
				attempt-- // a capacity wait is not a semantic retry; see capacityRetries
				continue
			}
		}
		// A completion that streamed to the end without hitting the
		// capacity gate is evidence upstream has headroom — feed the AIMD
		// increase path. A request that hit MODEL_CONCURRENCY_LIMIT and
		// eventually got through only via backoff must NOT count as clean
		// (the ReportCapacity in the retry branch already backed the
		// limit off).
		if err == nil && !sawCapacityError {
			uGate.ReportClean()
		}
		return err
	}
	return errors.New("Max retries exceeded")
}

// verifyCodeSuffix formats a captcha verify_code for log lines.
func verifyCodeSuffix(code string) string {
	if code == "" {
		return ""
	}
	return " " + code
}

// modelCapacityError is the typed error returned when Z.AI answers HTTP 200
// with an inline SSE error code MODEL_CONCURRENCY_LIMIT ("Model is currently
// at capacity") instead of a completion. Observed live on glm-5.3/5.2 during
// the 2026-09-05 e2e runs: the burst lasts a few seconds (several requests in
// a row hit it, then the model frees up), so it is transient — sendToZAIStream
// retries with a longer backoff than the 5xx path before surfacing it.
type modelCapacityError struct {
	detail  string
	emitted bool // true if streamSSEResponse already pushed content/reasoning on ch
}

func (e *modelCapacityError) Error() string {
	return "Z.AI error: " + e.detail + " (code: MODEL_CONCURRENCY_LIMIT)"
}

// modelCapacityFromObj returns a modelCapacityError when the parsed SSE error
// object carries code/error_code MODEL_CONCURRENCY_LIMIT, or nil otherwise.
func modelCapacityFromObj(errObj map[string]interface{}) *modelCapacityError {
	code, _ := errObj["code"].(string)
	if code == "" {
		code, _ = errObj["error_code"].(string)
	}
	if code != "MODEL_CONCURRENCY_LIMIT" {
		return nil
	}
	e := &modelCapacityError{}
	e.detail, _ = errObj["detail"].(string)
	if e.detail == "" {
		e.detail, _ = errObj["message"].(string)
	}
	return e
}

// extractModelCapacityError inspects a parsed Z.AI SSE payload for the
// model-capacity gate, mirroring extractCaptchaFlowError's shapes
// (data.error, data.data.error, top-level error).
func extractModelCapacityError(j map[string]interface{}) *modelCapacityError {
	if data, ok := j["data"].(map[string]interface{}); ok {
		if errObj, ok := data["error"].(map[string]interface{}); ok {
			if e := modelCapacityFromObj(errObj); e != nil {
				return e
			}
		}
		if nested, ok := data["data"].(map[string]interface{}); ok {
			if errObj, ok := nested["error"].(map[string]interface{}); ok {
				if e := modelCapacityFromObj(errObj); e != nil {
					return e
				}
			}
		}
	}
	if errObj, ok := j["error"].(map[string]interface{}); ok {
		return modelCapacityFromObj(errObj)
	}
	return nil
}

// captchaFlowError is the typed error returned when Z.AI answers HTTP 200
// with an SSE body that embeds the captcha gate error (FRONTEND_CAPTCHA_REQUIRED)
// instead of a completion. Observed live during the 2026-09-02 captcha probes
// (exp_captcha_bypass_test.go): the error object carries captcha_error_type
// (missing_param | verify_failed) and a verify_code (F018 = param already
// consumed/stale, F019 = verification failed). sendToZAIStream uses the type
// to regenerate the captcha param and retry once when nothing has been
// streamed to the client yet.
type captchaFlowError struct {
	detail     string
	errorType  string // captcha_error_type
	verifyCode string // F018 / F019 / ""
	emitted    bool   // true if streamSSEResponse already pushed content/reasoning on ch
}

func (e *captchaFlowError) Error() string {
	msg := "Z.AI captcha error: " + e.detail
	if e.errorType != "" {
		msg += " (captcha_error_type: " + e.errorType
		if e.verifyCode != "" {
			msg += ", verify_code: " + e.verifyCode
		}
		msg += ")"
	}
	return msg
}

// captchaErrorFromObj returns a captchaFlowError when the parsed SSE error
// object is the captcha gate (code/error_code FRONTEND_CAPTCHA_REQUIRED),
// or nil for any other error shape.
func captchaErrorFromObj(errObj map[string]interface{}) *captchaFlowError {
	code, _ := errObj["code"].(string)
	if code == "" {
		code, _ = errObj["error_code"].(string)
	}
	if code != "FRONTEND_CAPTCHA_REQUIRED" {
		return nil
	}
	e := &captchaFlowError{}
	e.detail, _ = errObj["detail"].(string)
	e.errorType, _ = errObj["captcha_error_type"].(string)
	e.verifyCode, _ = errObj["verify_code"].(string)
	return e
}

// extractCaptchaFlowError inspects a parsed Z.AI SSE payload for the captcha
// gate error. Z.AI duplicates the error object at data.error and data.data.error
// (both shapes observed live); a top-level error is checked defensively too.
func extractCaptchaFlowError(j map[string]interface{}) *captchaFlowError {
	if data, ok := j["data"].(map[string]interface{}); ok {
		if errObj, ok := data["error"].(map[string]interface{}); ok {
			if e := captchaErrorFromObj(errObj); e != nil {
				return e
			}
		}
		if nested, ok := data["data"].(map[string]interface{}); ok {
			if errObj, ok := nested["error"].(map[string]interface{}); ok {
				if e := captchaErrorFromObj(errObj); e != nil {
					return e
				}
			}
		}
	}
	if errObj, ok := j["error"].(map[string]interface{}); ok {
		return captchaErrorFromObj(errObj)
	}
	return nil
}

// extractZAIError inspects a parsed Z.AI SSE payload for an embedded error
// (Z.AI sometimes returns HTTP 200 with the error inside the JSON body).
// Returns the human-readable detail string, or "" if no error is present.
func extractZAIError(j map[string]interface{}) string {
	if data, ok := j["data"].(map[string]interface{}); ok {
		// data.error
		if errObj, ok := data["error"].(map[string]interface{}); ok {
			detail, _ := errObj["detail"].(string)
			if detail == "" {
				if s, ok := errObj["message"].(string); ok {
					detail = s
				}
			}
			if detail != "" {
				if code, ok := errObj["code"]; ok && code != nil {
					return fmt.Sprintf("%s (code: %v)", detail, code)
				}
				return detail
			}
		}
		// data.data.error (nested variant observed in production)
		if nested, ok := data["data"].(map[string]interface{}); ok {
			if errObj, ok := nested["error"].(map[string]interface{}); ok {
				detail, _ := errObj["detail"].(string)
				if detail == "" {
					if s, ok := errObj["message"].(string); ok {
						detail = s
					}
				}
				if detail != "" {
					if code, ok := errObj["code"]; ok && code != nil {
						return fmt.Sprintf("%s (code: %v)", detail, code)
					}
					return detail
				}
			}
		}
	}
	// Top-level error (non-Z.AI shape, just in case)
	if errObj, ok := j["error"].(map[string]interface{}); ok {
		detail, _ := errObj["detail"].(string)
		if detail == "" {
			if s, ok := errObj["message"].(string); ok {
				detail = s
			}
		}
		if detail != "" {
			return detail
		}
	}
	return ""
}

// statusFromError maps a Z.AI/bridge error string to an HTTP status code.
func statusFromError(errMsg string) int {
	switch {
	case strings.Contains(errMsg, "MODEL_CONCURRENCY_LIMIT"):
		return 503
	case strings.Contains(errMsg, "401"):
		return 401
	case strings.Contains(errMsg, "403"):
		return 403
	case strings.Contains(errMsg, "405"):
		return 405
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

// upstreamRetryBackoff is how long to wait before retrying a transient
// upstream 5xx. Short enough to keep latency low, long enough for a
// momentary Z.AI backend hiccup to clear.
const upstreamRetryBackoff = 800 * time.Millisecond

// isRetryableUpstreamStatus reports whether an upstream Z.AI HTTP status is a
// transient server-side failure worth a single retry. Rate limiting (429) is
// deliberately excluded — fclaude applies its own client-side backoff for
// those (see agent_utils.py); retrying them here would only add latency.
func isRetryableUpstreamStatus(code int) bool {
	switch code {
	case 500, 502, 503, 504:
		return true
	}
	return false
}

// utf16IndexToByteIndex converts a UTF-16 code-unit offset — the indexing
// semantics of JavaScript strings, used by the official Z.AI web frontend
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

func streamSSEResponse(body io.Reader, ch chan<- ZAIResult, requestId string) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	// ── Accumulated state across SSE lines ──
	var fullText strings.Builder      // raw upstream content, verbatim
	contentEmitter := &sseEmitter{}   // tracks what the client has received
	reasoningEmitter := &sseEmitter{} // same for the reasoning channel
	// reasoningClosed latches once content appears (</details> closed):
	// the thinking block is final, the held-back tail is released once,
	// and later reasoning growth is ignored (see flush).
	reasoningClosed := false

	// stripDetailsTags removes <details ...> and </details> wrappers
	// and leading "> " markdown-quote prefixes from each line.
	stripDetailsTags := func(s string) string {
		if idx := strings.Index(s, "<details"); idx >= 0 {
			if end := strings.Index(s[idx:], ">"); end >= 0 {
				s = s[:idx] + s[idx+end+1:]
			}
		}
		s = strings.ReplaceAll(s, "</details>", "")
		lines := strings.Split(s, "\n")
		for i, l := range lines {
			lines[i] = strings.TrimPrefix(l, "> ")
		}
		return strings.TrimSpace(strings.Join(lines, "\n"))
	}

	// emitContent forwards the part of `target` the client has not seen
	// yet. All slicing is rune-safe, so clients can never receive invalid
	// UTF-8 (which their JSON renderer would show as U+FFFD replacement
	// garble — the symptom reported in issue #23). FullText carries the
	// authoritative upstream snapshot for non-stream consumers and for
	// deep-edit re-sync detection in the handlers.
	emitContent := func(target string) {
		if delta := contentEmitter.delta(target); delta != "" {
			if config.Logging.Level == "debug" {
				log.Printf("[DEBUG][%s] emitContent delta=%q (len=%d) target_len=%d", requestId, delta, len(delta), len(target))
			}
			ch <- ZAIResult{Chunk: delta, FullText: target}
		}
	}

	flush := func(final bool) {
		raw := fullText.String()

		// Split <details ...> ... </details> into reasoning vs content
		reasoning, content := splitDetails(raw)
		if reasoning != "" {
			reasoning = stripDetailsTags(reasoning)
		}
		reasoningFull := reasoning

		// Emit reasoning delta (prefix-aware, rune-safe). Reasoning rides
		// the same edit-based stream as content, so while the stream is
		// live it gets the same protection: hold back a small tail so
		// trailing edit_content backtracks are absorbed invisibly, hold
		// back a partial </details> close tag streamed character by
		// character (splitDetails folds it into the reasoning body until
		// it completes, so forwarding it would leak the fragment and
		// then rewind the snapshot), and hold back a partially-streamed
		// "> " quote marker that a later character would strip again
		// (non-monotonic snapshots diverge the emitter and duplicate
		// everything after them). The final flush releases everything.
		if !final {
			reasoning = holdBackTail(reasoning, config.StreamHoldback)
			reasoning = holdBackPartialDetailsTag(reasoning)
			reasoning = holdBackPartialQuoteMarker(reasoning)
		}
		// Once content appears (</details> closed), the reasoning is final:
		// release the held-back tail ONCE against the FULL reasoning so the
		// thinking block completes instead of ending mid-word — then latch
		// closed. Without the release, earlier flushes advanced the emitter
		// view while the delta was discarded, permanently truncating every
		// thinking block mid-word (E2E log: "...friendly greetin",
		// "...related fi"). After the latch, reasoning growth is ignored:
		// the edit_content thinking→answer transition adds new thinking
		// text inside <details> at the same time it starts the answer, and
		// emitting that as reasoning_content after the client has already
		// rendered content creates a second "Thinking" block in the UI.
		if content == "" {
			reasoningClosed = false
			if delta := reasoningEmitter.delta(reasoning); delta != "" {
				ch <- ZAIResult{Reasoning: delta}
			}
		} else if !reasoningClosed {
			if delta := reasoningEmitter.delta(reasoningFull); delta != "" {
				ch <- ZAIResult{Reasoning: delta}
			}
			reasoningClosed = true
		}

		// While the stream is live, keep a small tail pending so ordinary
		// trailing edit_content backtracks are absorbed invisibly, and
		// never forward a fragment that could still grow into a <details>
		// tag. The final flush releases everything.
		//
		// Use holdBackTailSafe (not holdBackTail) so the trailing holdback
		// never trims into a <<<TOOL_CALL>>> / <<<END_TOOL_CALL>>> marker;
		// trimming into the marker breaks the agent interceptor's start/end
		// detection and causes the entire tool call to leak as text.
		target := content
		if !final {
			target = holdBackTailSafe(target, config.StreamHoldback)
			target = holdBackPartialDetailsTag(target)
		}
		emitContent(target)
	}

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if config.Logging.Level == "debug" && trimmed != "" {
			log.Printf("[DEBUG][%s] Z.AI SSE line: %s", requestId, trimmed)
		}

		if !strings.HasPrefix(trimmed, "data: ") {
			continue
		}
		dataStr := trimmed[6:]
		if dataStr == "[DONE]" {
			flush(true)
			return nil
		}

		var j map[string]interface{}
		if err := json.Unmarshal([]byte(dataStr), &j); err != nil {
			if config.Logging.Level == "debug" {
				log.Printf("[DEBUG][%s] Z.AI failed to parse SSE: %s", requestId, dataStr)
			}
			continue
		}

		// ── Detect inline errors (HTTP 200 with error in body) ──
		// The captcha gate error (FRONTEND_CAPTCHA_REQUIRED) gets a typed
		// error so sendToZAIStream can regenerate the captcha param and
		// retry once. The captcha gate fires BEFORE any content is streamed
		// (Z.AI replaces the whole completion with the error object), so the
		// retry never duplicates content — but the emitted flag still guards
		// against any unexpected mid-stream shape.
		if errDetail := extractZAIError(j); errDetail != "" {
			if config.Logging.Level == "debug" {
				log.Printf("[DEBUG][%s] Z.AI inline SSE error: %s", requestId, errDetail)
			}
			if capErr := extractCaptchaFlowError(j); capErr != nil {
				capErr.emitted = !contentEmitter.empty() || !reasoningEmitter.empty()
				return capErr
			}
			if mErr := extractModelCapacityError(j); mErr != nil {
				mErr.emitted = !contentEmitter.empty() || !reasoningEmitter.empty()
				return mErr
			}
			return fmt.Errorf("Z.AI error: %s", errDetail)
		}

		if data, ok := j["data"].(map[string]interface{}); ok {
			if phase, ok := data["phase"].(string); ok && phase == "done" {
				flush(true)
				return nil
			}
		}

		// ── Content accumulation ──
		// Semantics mirror the official Z.AI web frontend (prod-fe bundle):
		//   edit_content:  content = content.substring(0, edit_index) + edit_content
		//                  where edit_index is a UTF-16 code-unit offset
		//                  (JavaScript string indexing); a missing
		//                  edit_index defaults to 0 (full replacement)
		//   content:       full replacement of the accumulated text
		//   delta_content: plain append
		if data, ok := j["data"].(map[string]interface{}); ok {
			if ec, ok := data["edit_content"].(string); ok && ec != "" {
				editIndex := 0
				if ei, ok := data["edit_index"].(float64); ok {
					editIndex = int(ei)
				}
				current := fullText.String()
				byteIdx := utf16IndexToByteIndex(current, editIndex)
				fullText.Reset()
				fullText.WriteString(current[:byteIdx] + ec)
			} else if tc, ok := data["content"].(string); ok && tc != "" {
				fullText.Reset()
				fullText.WriteString(tc)
			} else if dc, ok := data["delta_content"].(string); ok && dc != "" {
				fullText.WriteString(dc)
			}
		}

		flush(false)
	}

	flush(true)
	return scanner.Err()
}
