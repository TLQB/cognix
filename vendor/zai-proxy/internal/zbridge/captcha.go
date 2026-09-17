// Code moved from the original main.go monolith during the internal/ restructure.
// See README "Project Structure". Part of the Z.AI bridge core (package zbridge).

package zbridge

import (
	"bytes"
	"compress/zlib"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver registration (no CGO)
)

// ============================================================================
// DATABASE — SQLite, pure-Go driver (no CGO)
// ============================================================================

func initDB() error {
	var err error
	globalDB, err = sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}
	globalDB.SetMaxOpenConns(1)
	globalDB.SetMaxIdleConns(1)
	// Ensure the reserve table exists so the proxy is self-sufficient:
	// qfarmRefillReserve/qfarmStatusHandler depend on it even before the
	// first token is ever read (schema kept in sync with scripts/refill_reserve.go).
	if _, err := globalDB.Exec(`CREATE TABLE IF NOT EXISTS tokens (
		id    INTEGER PRIMARY KEY AUTOINCREMENT,
		token TEXT    NOT NULL,
		batch INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("create tokens table: %w", err)
	}
	return nil
}

func getNextToken() (string, bool) {
	// PRIMARY source: pure-Go mint on a browser-blessed Q (qbless.json,
	// refreshed every 1-3h by cmd/q-bless — ~0ms/token, unlimited).
	// Falls through to the tokens.sqlite emergency reserve when no usable
	// session exists. See qmint.go.
	if tok, ok, err := qmintMint(); err != nil {
		logError("[QBless] mint: " + err.Error())
	} else if ok {
		return tok, true
	}

	dbMu.Lock()
	defer dbMu.Unlock()

	if _, err := os.Stat(dbPath); err != nil {
		logError("Database file not found: " + dbPath)
		return "", false
	}

	var token string
	err := globalDB.QueryRow("SELECT token FROM tokens ORDER BY id LIMIT 1;").Scan(&token)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			logError("No device tokens available in table 'tokens'")
		} else {
			logError("Failed to query token: " + err.Error())
		}
		return "", false
	}
	return token, true
}

func removeToken(token string) {
	// qbless-minted tokens never live in the DB; the DELETE is a no-op
	// for them (harmless), so this stays unconditional.
	dbMu.Lock()
	defer dbMu.Unlock()

	_, err := globalDB.Exec("DELETE FROM tokens WHERE token = ?;", token)
	if err != nil {
		logError("Failed to delete consumed token: " + err.Error())
	}
}

// ============================================================================
// ALIYUN SIGNATURE
// ============================================================================

func generateSignature(params map[string]string, secKey string) string {
	keys := make([]string, 0, len(params)+1)
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var canonical strings.Builder
	canonical.Grow(512)
	for i, k := range keys {
		if i > 0 {
			canonical.WriteByte('&')
		}
		canonical.WriteString(urlEncode(k, ""))
		canonical.WriteByte('=')
		canonical.WriteString(urlEncode(params[k], ""))
	}

	stringToSign := "POST&" + urlEncode("/", "") + "&" + urlEncode(canonical.String(), "")
	signingKey := secKey + "&"
	return base64Encode(hmacSHA1([]byte(signingKey), []byte(stringToSign)))
}

func buildQueryString(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.Grow(512)
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(urlEncode(k, ""))
		b.WriteByte('=')
		b.WriteString(urlEncode(params[k], ""))
	}
	return b.String()
}

// ============================================================================
// HTTP POST — pooled buffer for response, connection reuse
// ============================================================================

func httpPost(targetURL, body string, extraHeaders map[string]string) (string, error) {
	req, err := http.NewRequest("POST", targetURL, strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.ContentLength = int64(len(body))
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := aliyunHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	if _, err := io.Copy(buf, resp.Body); err != nil {
		bufPool.Put(buf)
		return "", err
	}
	result := buf.String()
	bufPool.Put(buf)
	return result, nil
}

// ============================================================================
// CAPTCHA GENERATION — PART 1: InitCaptchaV3
// ============================================================================

func initCaptcha() (string, error) {
	if accessKey == "" || secretKey == "" {
		return "", errors.New("aliyun captcha credentials missing (set ALIYUN_ACCESS_KEY / ALIYUN_SECRET_KEY)")
	}
	params := map[string]string{
		"AccessKeyId":      accessKey,
		"Action":           "InitCaptchaV3",
		"Format":           "JSON",
		"Language":         "en",
		"Mode":             "popup",
		"SceneId":          sceneID,
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureNonce":   generateUUID(),
		"SignatureVersion": "1.0",
		"Timestamp":        getTimestampUTC(),
		"UpLang":           "true",
		"Version":          "2023-03-05",
	}
	params["Signature"] = generateSignature(params, secretKey)

	body := buildQueryString(params)
	resp, err := httpPost(
		"https://no8xfe.captcha-open-southeast.aliyuncs.com/", body, nil)
	if err != nil {
		return "", err
	}

	var result InitCaptchaResponse
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		return "", fmt.Errorf("parse InitCaptchaV3 response: %w (body: %.200s)", err, resp)
	}
	if result.CertifyID == "" {
		return "", fmt.Errorf("InitCaptchaV3 returned no CertifyId (body: %.200s)", resp)
	}
	return result.CertifyID, nil
}

// ============================================================================
// CAPTCHA GENERATION — PART 2: Generate arg (RC4-like stream cipher)
// ============================================================================

var argPermTable = [64]int{
	32, 50, 10, 51, 6, 44, 37, 16, 46, 11, 62, 19, 43, 25, 23, 30,
	60, 33, 53, 34, 7, 26, 12, 48, 5, 2, 20, 4, 61, 13, 47, 49,
	18, 29, 27, 22, 1, 17, 39, 56, 41, 38, 55, 31, 15, 58, 52, 40,
	8, 57, 45, 35, 59, 36, 42, 54, 63, 3, 24, 28, 14, 9, 0, 21,
}

const argConstant = "4xrihv8zb8tf1mfj"

func generateArg(certifyID string) string {
	encoded := urlEncode(certifyID, "")

	// URL-decode (identity for already-decoded strings, kept for faithfulness)
	o := make([]byte, 0, len(encoded))
	for i := 0; i < len(encoded); {
		if encoded[i] == '%' && i+2 < len(encoded) {
			o = append(o, fromHex(encoded[i+1])<<4|fromHex(encoded[i+2]))
			i += 3
		} else {
			o = append(o, encoded[i])
			i++
		}
	}

	// KSA
	r := argPermTable
	n := argConstant
	rlen := 64

	i, j := 0, 0
	for i < rlen {
		j = (((i + j + r[i] + r[j]) >> 1) + int(n[i%len(n)])) & (rlen - 1)
		if i != j {
			r[i], r[j] = r[j], r[i]
		}
		i++
	}

	// PRGA
	t := make([]byte, 0, len(o))
	e, a := 0, 0
	for idx := 0; idx < len(o); idx++ {
		a = ((e ^ a) + (r[e] ^ r[a])) & (rlen - 1)
		if e != a {
			r[e], r[a] = r[a], r[e]
		}
		m := int(o[idx])
		m = m + e + r[e] - a - r[a]
		m = m ^ (r[e] + r[a])
		m = m ^ r[(r[e]+r[a])&(rlen-1)]
		m = m & 255
		t = append(t, byte(m))
		e = (e + 1) & (rlen - 1)
	}
	return base64Encode(t)
}

// ============================================================================
// CAPTCHA GENERATION — PART 4: ali_hash (custom hash with 16-byte state)
// ============================================================================

func aliHash(inputStr, saltStr string) string {
	o := inputStr
	r := saltStr
	aLen := len(o)
	m := len(r)

	var e [16]int
	for i := 0; i < 16; i++ {
		e[i] = (i << 4) + (i % 16)
	}
	f := 16

	i, j := 0, 0
	for i < f {
		j = (((i + j + e[i] + e[j]) >> 1) + int(r[i%m])) & (f - 1)
		e[i], e[j] = e[j], e[i]
		i++
	}

	idx, p, q := 0, 0, 0
	for idx < aLen {
		q = ((p ^ q) + (e[p] ^ e[q])) & (f - 1)
		e[p], e[q] = e[q], e[p]
		C := int(o[idx])
		C = (C + p + q) ^ e[p] ^ e[q]
		C = C & 255
		e[p] = C
		p = (p + 1) & (f - 1)
		idx++
	}

	for step := 0; step < 2*f; step++ {
		pos := step % f
		if pos != 0 {
			e[pos] ^= e[pos-1]
		} else {
			e[0] ^= e[f-1]
		}
	}

	var result [32]byte
	for i, b := range e {
		result[i*2] = hexLower[(b>>4)&0xF]
		result[i*2+1] = hexLower[b&0xF]
	}
	return string(result[:])
}

// ============================================================================
// CAPTCHA GENERATION — PART 7: encrypt (same RC4-like cipher, different key)
// ============================================================================

const encryptKey = "3e627e1b4c63f913"

func encrypt(plaintext []byte) string {
	o := plaintext
	n := encryptKey
	r := argPermTable
	rlen := 64

	oKsa, tKsa := 0, 0
	for oKsa < rlen {
		tKsa = (((oKsa + tKsa + r[oKsa] + r[tKsa]) >> 1) + int(n[oKsa%len(n)])) & (rlen - 1)
		if oKsa != tKsa {
			r[oKsa], r[tKsa] = r[tKsa], r[oKsa]
		}
		oKsa++
	}

	t := make([]byte, 0, len(o))
	e, a := 0, 0
	for nPrga := 0; nPrga < len(o); nPrga++ {
		a = ((e ^ a) + (r[e] ^ r[a])) & (rlen - 1)
		if e != a {
			r[e], r[a] = r[a], r[e]
		}
		m := int(o[nPrga])
		m = m + e + r[e] - a - r[a]
		m = m ^ (r[e] + r[a])
		m = m ^ r[(r[e]+r[a])&(rlen-1)]
		m = m & 255
		t = append(t, byte(m))
		e = (e + 1) & (rlen - 1)
	}
	return base64Encode(t)
}

// ============================================================================
// ZLIB COMPRESS — pooled writer, pooled output buffer
// ============================================================================

func zlibCompress(data []byte) []byte {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	buf.Grow(len(data) + len(data)/2 + 128)

	w := zlibWriterPool.Get().(*zlib.Writer)
	w.Reset(buf)
	w.Write(data)
	w.Close()
	zlibWriterPool.Put(w)

	result := make([]byte, buf.Len())
	copy(result, buf.Bytes())
	bufPool.Put(buf)
	return result
}

// ============================================================================
// CAPTCHA GENERATION — PART 8: VerifyCaptchaV3
// ============================================================================

func verifyCaptcha(certifyID, dataValue, deviceToken string) (string, error) {
	cvpJSON, err := jsonMarshal(CVP{
		CertifyID:   certifyID,
		Data:        dataValue,
		DeviceToken: deviceToken,
		SceneID:     sceneID,
	})
	if err != nil {
		return "", err
	}

	params := map[string]string{
		"AccessKeyId":        accessKey,
		"Action":             "VerifyCaptchaV3",
		"Format":             "JSON",
		"SignatureMethod":    "HMAC-SHA1",
		"SignatureVersion":   "1.0",
		"Timestamp":          getTimestampUTC(),
		"Version":            "2023-03-05",
		"SceneId":            sceneID,
		"CertifyId":          certifyID,
		"CaptchaVerifyParam": string(cvpJSON),
		"SignatureNonce":     generateUUID(),
	}
	params["Signature"] = generateSignature(params, secretKey)

	body := buildQueryString(params)
	resp, err := httpPost(
		"https://no8xfe-verify.captcha-open-southeast.aliyuncs.com/",
		body, map[string]string{"Referer": ""})
	if err != nil {
		return "", err
	}

	var respJSON VerifyCaptchaResponse
	if err := json.Unmarshal([]byte(resp), &respJSON); err != nil {
		return "", fmt.Errorf("parse VerifyCaptchaV3 response: %w", err)
	}

	if respJSON.Success && respJSON.Result.VerifyResult {
		st := respJSON.Result.SecurityToken
		ci := respJSON.Result.CertifyID
		if st != "" && ci != "" {
			fpJSON, err := jsonMarshal(FinalPayload{
				CertifyID:     ci,
				IsSign:        true,
				SceneID:       sceneID,
				SecurityToken: st,
			})
			if err != nil {
				return "", err
			}
			return base64Encode(fpJSON), nil
		}
		logError("VerifyCaptchaV3 succeeded but securityToken/certifyId empty for deviceToken=" + shortTokenForLog(deviceToken))
	} else if respJSON.Success {
		logError("deviceToken failed verification (VerifyResult=false): " + shortTokenForLog(deviceToken))
	} else {
		logError("VerifyCaptchaV3 request unsuccessful for deviceToken=" + shortTokenForLog(deviceToken) + " response=" + resp)
	}
	return "", nil
}

// ============================================================================
// CAPTCHA BREAKER — protects the token pool from bad batches
//
// A bad device-token batch fails verification instantly, and the background
// cache (500ms ticker, 2 parallel generations x maxTokenRetries tokens each)
// would otherwise burn through the entire pool in seconds. After N
// consecutive failed token attempts the breaker opens and pauses ALL captcha
// generation (background AND synchronous) for a cooldown, giving the operator
// time to replenish tokens instead of losing the pool. Any success resets it.
// ============================================================================

const (
	captchaBreakerThreshold = 6               // consecutive failed tokens that trip the breaker
	captchaBreakerCooldown  = 3 * time.Minute // pause duration once tripped
)

var (
	captchaBreakerMu    sync.Mutex
	captchaConsecFails  int
	captchaBreakerUntil time.Time
)

func captchaBreakerOpen() bool {
	captchaBreakerMu.Lock()
	defer captchaBreakerMu.Unlock()
	return time.Now().Before(captchaBreakerUntil)
}

func recordCaptchaOutcome(success bool) {
	captchaBreakerMu.Lock()
	defer captchaBreakerMu.Unlock()
	if success {
		captchaConsecFails = 0
		captchaBreakerUntil = time.Time{}
		return
	}
	captchaConsecFails++
	if captchaConsecFails >= captchaBreakerThreshold && time.Now().After(captchaBreakerUntil) {
		captchaBreakerUntil = time.Now().Add(captchaBreakerCooldown)
		logOperational(fmt.Sprintf("[Captcha Breaker] %d consecutive token failures — pausing captcha generation for %s (tokens likely bad or pool empty; replenish tokens.sqlite)",
			captchaConsecFails, captchaBreakerCooldown))
	}
}

// ============================================================================
// COMPUTE FINAL PAYLOAD — tries tokens until success or exhausted
// ============================================================================

func computeFinalPayload() string {
	if captchaBreakerOpen() {
		logError("[Captcha Breaker] open — skipping generation to protect the token pool")
		return ""
	}
	for attempt := 0; attempt < maxTokenRetries; attempt++ {
		deviceToken, ok := getNextToken()
		if !ok {
			// Both sources refused: either no session / Q DEAD, or the reserve
			// is empty. This is a total outage — make it visible without --verbose.
			logOperational("[Captcha] no device token available (qbless unusable AND tokens.sqlite reserve empty) — run q-bless, then refill the reserve with scripts/refill_reserve.go")
			logError(fmt.Sprintf("No device tokens remaining (attempt %d/%d)",
				attempt+1, maxTokenRetries))
			recordCaptchaOutcome(false)
			return ""
		}
		logInfo(fmt.Sprintf("Attempt %d/%d using deviceToken=%s…",
			attempt+1, maxTokenRetries, shortTokenForLog(deviceToken)))

		payload, err := tryCompute(deviceToken)
		if err != nil {
			logError(fmt.Sprintf("Attempt %d failed for deviceToken=%s: %v",
				attempt+1, shortTokenForLog(deviceToken), err))
			recordCaptchaOutcome(false)
			if captchaBreakerOpen() {
				return ""
			}
			continue
		}
		if payload != "" {
			recordCaptchaOutcome(true)
			return payload
		}
		logError("deviceToken=" + shortTokenForLog(deviceToken) + " produced empty payload, retrying")
		recordCaptchaOutcome(false)
		if captchaBreakerOpen() {
			return ""
		}
	}
	logError(fmt.Sprintf("All %d token retries exhausted", maxTokenRetries))
	return ""
}

func tryCompute(deviceToken string) (string, error) {
	certifyID, err := initCaptcha()
	if err != nil {
		// initCaptcha failures are account/credential problems (bad
		// AccessKeyId, signature, network) — NOT device-token problems.
		// Keep the token; burning it would drain the pool on a
		// configuration error.
		return "", fmt.Errorf("initCaptcha: %w", err)
	}

	argValue := generateArg(certifyID)
	ct := currentTimeMillis()

	track := Track{
		TrackList: TrackList{
			StartTime: ct,
		},
		TrackStartTime: ct,
		VerifyTime:     ct + 300,
		Arg:            argValue,
	}
	jsonBytes, err := jsonMarshal(track)
	if err != nil {
		removeToken(deviceToken)
		return "", err
	}

	h := aliHash(string(jsonBytes), "0000")
	combined := h + string(jsonBytes)
	compressed := zlibCompress([]byte(combined))
	fb64 := base64Encode(compressed)
	finalVal := encrypt([]byte(fb64))

	// Always remove token after use — prevents conflicts
	removeToken(deviceToken)

	payload, err := verifyCaptcha(certifyID, finalVal, deviceToken)
	if err != nil {
		return "", fmt.Errorf("verifyCaptcha: %w", err)
	}
	// Feed the qbless health tracker: a token minted on the blessed Q that
	// FAILS live verify is evidence the Q may be dying. Empty payload with
	// nil error = VerifyResult=false (or an empty securityToken) — that's
	// the Q-death signal. Extract the Q structurally from the token so the
	// verdict is bound to the exact Q, never to whichever session file is
	// current at report time.
	if q := qblessTokenQ(deviceToken); q != "" {
		qblessReportVerifyQ(q, payload != "")
	}
	return payload, nil
}

// ============================================================================
// CAPTCHA CACHE — Background async generation for speed
// ============================================================================

type cachedCaptcha struct {
	value       string
	generatedAt time.Time
}

type CaptchaCache struct {
	mu         sync.Mutex
	params     []cachedCaptcha
	maxParams  int
	generating int
	active     bool
	lastActive time.Time
}

var captchaCache = &CaptchaCache{
	maxParams:  2,
	lastActive: time.Now(),
}

func (c *CaptchaCache) markActive() {
	c.mu.Lock()
	c.lastActive = time.Now()
	c.active = true
	c.mu.Unlock()
}

// Wake resumes background generation after an idle pause. Run() stops
// generating once nobody has drawn from the cache for 3 minutes (token
// conservation); the next real request then eats a synchronous ~0.5s
// captcha mint on a cold miss. Chat requests call Wake() so the cache is
// already refilling by the time follow-up turns need params. Best-effort
// and race-free by construction: it only touches lastActive under the
// same mutex Run() uses.
func (c *CaptchaCache) Wake() {
	c.markActive()
}

func (c *CaptchaCache) Get() (string, bool) {
	c.markActive()
	c.mu.Lock()
	defer c.mu.Unlock()

	// Sweep expired (75s TTL)
	var valid []cachedCaptcha
	for _, p := range c.params {
		if time.Since(p.generatedAt) < 75*time.Second {
			valid = append(valid, p)
		}
	}
	c.params = valid

	if len(c.params) > 0 {
		val := c.params[0].value
		c.params = c.params[1:]
		return val, true
	}
	return "", false
}

func (c *CaptchaCache) Run() {
	// Wait a moment before starting to allow session to init
	time.Sleep(2 * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		c.mu.Lock()

		// If no activity in the last 3 minutes, pause generation to save tokens
		if time.Since(c.lastActive) > 3*time.Minute {
			c.active = false
			c.mu.Unlock()
			continue
		}
		c.active = true

		// Sweep expired
		var valid []cachedCaptcha
		for _, p := range c.params {
			if time.Since(p.generatedAt) < 75*time.Second {
				valid = append(valid, p)
			}
		}
		c.params = valid

		needed := c.maxParams - len(c.params) - c.generating
		if needed > 0 {
			c.generating += needed
			c.mu.Unlock()
			// Launch generation in parallel
			for i := 0; i < needed; i++ {
				go c.generate()
			}
		} else {
			c.mu.Unlock()
		}
	}
}

func (c *CaptchaCache) generate() {
	startedAt := time.Now()
	payload := computeFinalPayload()

	c.mu.Lock()
	c.generating--
	if payload != "" {
		c.params = append(c.params, cachedCaptcha{
			value:       payload,
			generatedAt: time.Now(),
		})
		logInfo(fmt.Sprintf("[Captcha Cache] ✓ generated param in %.1fs (cache size: %d)", time.Since(startedAt).Seconds(), len(c.params)))
	} else {
		logError("[Captcha Cache] ✗ failed to generate param")
	}
	c.mu.Unlock()
}

// ============================================================================
// CAPTCHA VERIFICATION PARAM — IN-MEMORY (no FIFO / named pipe)
// ============================================================================

func getCaptchaVerifyParam() (string, error) {
	if config.AgentMode {
		if val, ok := captchaCache.Get(); ok {
			logInfo("[Captcha Cache] hit - using cached param")
			return val, nil
		}
		logInfo("[Captcha Cache] miss - generating synchronously")
	}

	// Mock/CI mode: when the bridge is pointed at a mock upstream
	// (ZAI_BASE_URL), no real Aliyun captcha is needed — the mock accepts
	// anything. Skip token minting (which would fail or hang without
	// qbless/tokens) and return a stub param so e2e can drive the full
	// request path.
	if BASE_URL != zaiProdURL {
		return `{"certifyId":"mock","isSign":true,"sceneId":"mock","securityToken":"mock"}`, nil
	}

	startedAt := time.Now()
	log.Printf("[Captcha] → computing CaptchaVerifyParam (in-memory IPC)")

	type result struct {
		val string
		err error
	}
	ch := make(chan result, 1)

	go func() {
		payload := computeFinalPayload()
		if payload == "" {
			ch <- result{"", errors.New("captcha generation returned empty payload")}
			return
		}
		ch <- result{payload, nil}
	}()

	select {
	case r := <-ch:
		elapsed := time.Since(startedAt).Seconds()
		if r.err != nil {
			log.Printf("[Captcha] ✗ error: %s", r.err.Error())
			return "", r.err
		}
		if r.val == "" {
			log.Printf("[Captcha] ✗ empty response after %.1fs", elapsed)
			return "", errors.New("captcha generation returned empty response")
		}
		log.Printf("[Captcha] ✓ got %db in %.1fs", len(r.val), elapsed)
		return r.val, nil
	case <-time.After(90 * time.Second):
		elapsed := time.Since(startedAt).Seconds()
		log.Printf("[Captcha] ✗ timeout after %.1fs", elapsed)
		return "", errors.New("captcha generation timeout after 90s")
	}
}
