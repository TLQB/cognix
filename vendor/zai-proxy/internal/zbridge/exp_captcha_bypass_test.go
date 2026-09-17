// exp_captcha_bypass_test.go — EXPERIMENTAL live probes: can we bypass the
// Aliyun captcha requirement on /api/v2/chat/completions entirely, instead
// of harvesting device tokens with a browser?
//
// Probes (all gated behind EXP_CAPTCHA=1):
//
//	no-captcha      — omit captcha_verify_param from the request body
//	empty-captcha   — captcha_verify_param = ""
//	null-captcha    — captcha_verify_param = null
//	garbage-captcha — captcha_verify_param = random base64
//	verify-no-token — InitCaptchaV3 + VerifyCaptchaV3 with EMPTY deviceToken
//	                  (pure HTTP; if it returns a SecurityToken, the whole
//	                  device-token pool becomes unnecessary)
//	verify-fake-tok — same, with a syntactically plausible fake deviceToken
//	mint-empty      — VerifyCaptchaV3 with a PURE-GO MINTED token that is
//	                  structurally byte-exact per the Fielin report
//	                  (base64(tF#Q#w#tC#MD5), w empty, tC=0, fresh uuids)
//	mint-replay     — minted token replaying a harvested w-blob + tC with
//	                  fresh uuid1/uuid2
//	mint-replay-dev — mint-replay but keeping the harvested device uuid1
//	reuse-captcha   — generate ONE param (1 token) and reuse it for 3
//	                  consecutive chat requests (tests single-use assumption)
//
// Select a subset with EXP_CAPTCHA_PROBES=name1,name2.

package zbridge

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// expFeVersion overrides the x-fe-Version header for all experimental
// probes (env EXP_FE_VERSION). Empty falls back to the old hardcoded spike
// value, so existing probes keep their historical behavior.
var expFeVersion = os.Getenv("EXP_FE_VERSION")

// resolveALBIP returns an ALB IP that fronts chat.z.ai by resolving z.ai
// (whose DNS is a CNAME to the Aliyun ALB). Falls back to a known-good IP.
func resolveALBIP() string {
	if ips, err := net.LookupIP("z.ai"); err == nil {
		for _, ip := range ips {
			if ip4 := ip.To4(); ip4 != nil {
				return ip4.String()
			}
		}
	}
	return "8.216.131.99" // last-known ALB IP fallback
}

// dialUTLSAlb dials the ALB IP directly but keeps TLS SNI = original host,
// mirroring `curl --resolve host:443:ALB_IP`. Honors HTTPS_PROXY like dialUTLS.
func dialUTLSAlb(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	albIP := resolveALBIP()
	dialTarget := net.JoinHostPort(albIP, port)

	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	var rawConn net.Conn

	proxyStr := os.Getenv("HTTPS_PROXY")
	if proxyStr == "" {
		proxyStr = os.Getenv("HTTP_PROXY")
	}
	if proxyStr != "" {
		proxyURL, perr := url.Parse(proxyStr)
		if perr == nil && proxyURL.Host != "" {
			proxyConn, perr := dialer.DialContext(ctx, "tcp", proxyURL.Host)
			if perr != nil {
				return nil, fmt.Errorf("proxy connect: %w", perr)
			}
			connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", dialTarget, dialTarget)
			if _, perr = proxyConn.Write([]byte(connectReq)); perr != nil {
				proxyConn.Close()
				return nil, fmt.Errorf("proxy CONNECT write: %w", perr)
			}
			br := bufio.NewReader(proxyConn)
			line, perr := br.ReadString('\n')
			if perr != nil {
				proxyConn.Close()
				return nil, fmt.Errorf("proxy CONNECT read: %w", perr)
			}
			if !strings.Contains(line, "200") {
				proxyConn.Close()
				return nil, fmt.Errorf("proxy CONNECT failed: %s", strings.TrimSpace(line))
			}
			for {
				line, perr = br.ReadString('\n')
				if perr != nil || strings.TrimSpace(line) == "" {
					break
				}
			}
			rawConn = proxyConn
		} else {
			rawConn, err = dialer.DialContext(ctx, network, dialTarget)
			if err != nil {
				return nil, err
			}
		}
	} else {
		rawConn, err = dialer.DialContext(ctx, network, dialTarget)
		if err != nil {
			return nil, err
		}
	}

	cfg := &utls.Config{
		ServerName:         host, // keep SNI = chat.z.ai
		NextProtos:         []string{"http/1.1"},
		InsecureSkipVerify: false,
	}
	uConn := utls.UClient(rawConn, cfg, utls.HelloChrome_120)
	if err := uConn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		return nil, err
	}
	return uConn, nil
}

func expCaptchaChat(t *testing.T, client *http.Client, body map[string]interface{}, prompt string) (int, string) {
	t.Helper()
	// expFeVersion overrides the x-fe-Version header for all experimental
	// probes; empty falls back to the old hardcoded spike value. This lets
	// A/B tests compare scraped-vs-hardcoded fe versions byte-for-byte.
	feVer := expFeVersion
	if feVer == "" {
		feVer = "prod-fe-1.1.92"
	}
	userID, _ := decodeJWT(config.ZaiToken)
	signature, _, _ := generateZaSignature(prompt, config.ZaiToken, userID)
	bodyBytes, _ := json.Marshal(body)

	// Retry with XFF rotation on WAF 405, mirroring sendToZAIStream: the
	// Aliyun WAF blocklists spoofed X-Forwarded-For addresses on a rolling
	// basis, so mark the blocked address bad and rotate to the next one.
	var lastCode int
	var lastOut string
	for attempt := 0; attempt < 8; attempt++ {
		xff := zaiXffIP()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		req, err := http.NewRequestWithContext(ctx, "POST", BASE_URL+"/api/v2/chat/completions", bytes.NewReader(bodyBytes))
		if err != nil {
			cancel()
			t.Fatalf("build req: %v", err)
		}
		req.Header.Set("authorization", "Bearer "+config.ZaiToken)
		req.Header.Set("User-Agent", zaiUserAgent)
		req.Header.Set("content-type", "application/json")
		req.Header.Set("x-fe-Version", feVer)
		req.Header.Set("x-region", "overseas")
		req.Header.Set("x-signature", signature)
		req.Header.Set("X-Forwarded-For", xff)

		resp, err := client.Do(req)
		if err != nil {
			cancel()
			return 0, "REQUEST ERROR: " + err.Error()
		}
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 2500))
		resp.Body.Close()
		cancel()
		out := strings.ReplaceAll(string(buf), "\n", " | ")
		if len(out) > 600 {
			out = out[:600]
		}
		lastCode, lastOut = resp.StatusCode, out

		// 405 with the Aliyun error page = WAF geo-block on this XFF; rotate.
		if resp.StatusCode == 405 && strings.Contains(out, "errors.aliyun.com") {
			markXffIPBad(xff)
			rotateXffIP()
			t.Logf("  attempt %d: WAF 405 on XFF=%s — rotated", attempt+1, xff)
			time.Sleep(300 * time.Millisecond)
			continue
		}
		return resp.StatusCode, out
	}
	return lastCode, lastOut
}

func TestExpCaptchaBypass(t *testing.T) {
	if os.Getenv("EXP_CAPTCHA") == "" {
		t.Skip("set EXP_CAPTCHA=1 to run these experimental live probes")
	}
	verbose = true
	dbPath = "../../tokens.sqlite"
	if err := initDB(); err != nil {
		t.Fatalf("initDB: %v", err)
	}
	if config.ZaiToken == "" {
		t.Fatal("ZAI_TOKEN not set")
	}

	want := map[string]bool{}
	if ev := os.Getenv("EXP_CAPTCHA_PROBES"); ev != "" {
		for _, n := range strings.Split(ev, ",") {
			want[strings.TrimSpace(n)] = true
		}
	}
	run := func(name string) bool { return len(want) == 0 || want[name] }

	prompt := "Reply with exactly: OK"
	msgs := []map[string]interface{}{{"role": "user", "content": prompt}}
	baseBody := func() map[string]interface{} {
		return map[string]interface{}{
			"model":            "glm-4.7",
			"messages":         msgs,
			"signature_prompt": prompt,
			"stream":           false,
			"features":         map[string]interface{}{"flags": []interface{}{}, "image_generation": false},
		}
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialTLSContext:      dialUTLSAlb,
			MaxIdleConns:        10,
			IdleConnTimeout:     30 * time.Second,
			TLSHandshakeTimeout: 15 * time.Second,
			ForceAttemptHTTP2:   false,
		},
		Timeout: 90 * time.Second,
	}

	// ── Probes that send NO valid captcha ─────────────────────────────────
	if run("no-captcha") {
		code, out := expCaptchaChat(t, client, baseBody(), prompt)
		t.Logf("no-captcha: HTTP %d | %s", code, out)
	}
	if run("empty-captcha") {
		b := baseBody()
		b["captcha_verify_param"] = ""
		code, out := expCaptchaChat(t, client, b, prompt)
		t.Logf("empty-captcha: HTTP %d | %s", code, out)
	}
	if run("null-captcha") {
		b := baseBody()
		b["captcha_verify_param"] = nil
		code, out := expCaptchaChat(t, client, b, prompt)
		t.Logf("null-captcha: HTTP %d | %s", code, out)
	}
	if run("garbage-captcha") {
		b := baseBody()
		b["captcha_verify_param"] = "eyJjZXJ0aWZ5SWQiOiJmYWtlIiwiaXNTaWduIjp0cnVlLCJzY2VuZUlkIjoiZGlkazMzZTAiLCJzZWN1cml0eVRva2VuIjoiZmFrZSJ9"
		code, out := expCaptchaChat(t, client, b, prompt)
		t.Logf("garbage-captcha: HTTP %d | %s", code, out)
	}

	// ── VerifyCaptchaV3 without a real device token (pure HTTP) ──────────
	doVerify := func(name, deviceToken string) bool {
		certifyID, err := initCaptcha()
		if err != nil {
			t.Logf("%s: initCaptcha error: %v", name, err)
			return false
		}
		argValue := generateArg(certifyID)
		ct := currentTimeMillis()
		track := Track{
			TrackList:      TrackList{StartTime: ct},
			TrackStartTime: ct,
			VerifyTime:     ct + 300,
			Arg:            argValue,
		}
		jsonBytes, err := jsonMarshal(track)
		if err != nil {
			t.Logf("%s: marshal error: %v", name, err)
			return false
		}
		h := aliHash(string(jsonBytes), "0000")
		compressed := zlibCompress([]byte(h + string(jsonBytes)))
		finalVal := encrypt([]byte(base64Encode(compressed)))

		payload, err := verifyCaptcha(certifyID, finalVal, deviceToken)
		if err != nil {
			t.Logf("%s: verifyCaptcha error: %v", name, err)
			return false
		}
		if payload == "" {
			t.Logf("%s: VerifyResult=false (empty payload)", name)
			return false
		}
		t.Logf("%s: *** SUCCESS *** payload %db — captcha possible WITHOUT device tokens", name, len(payload))

		// Bonus: use it for a real chat request.
		b := baseBody()
		b["captcha_verify_param"] = payload
		code, out := expCaptchaChat(t, client, b, prompt)
		t.Logf("%s chat: HTTP %d | %s", name, code, out)
		return code == 200
	}
	verifyWithoutToken := func(name, deviceToken string) {
		if !run(name) {
			return
		}
		doVerify(name, deviceToken)
	}
	verifyWithoutToken("verify-no-token", "")
	verifyWithoutToken("verify-fake-tok", "U_WEB#00000000-0000-0000-0000-000000000000#h-0-0000000000000000000000000000000#fake#0#00000000000000000000000000000000")

	// ── Pure-Go minted device tokens (Fielin report §10/§17/§20) ──────────
	// deviceToken = base64(tF#Q#w#tC#p) where
	//   Q = uuid1 + "-h-" + nowMillis + "-" + uuid2 (32-hex uuids, no dashes)
	//   p = MD5(tF#Q#w#tC#secret), secret = "daye,raolewoba!"
	// Formula verified byte-exact against harvested tokens. NOTE: the
	// verify-fake-tok probe above used a structurally INVALID token (wrong
	// prefix, no base64, bogus MD5), so it never tested whether a correctly
	// minted token passes VerifyCaptchaV3. These probes do.
	mintDeviceToken := func(w, tC, uuid1, uuid2 string) string {
		const tF = "SG_WEB"
		q := uuid1 + "-h-" + strconv.FormatInt(time.Now().UnixMilli(), 10) + "-" + uuid2
		sum := md5.Sum([]byte(strings.Join([]string{tF, q, w, tC, "daye,raolewoba!"}, "#")))
		p := fmt.Sprintf("%x", sum)
		return base64Encode([]byte(strings.Join([]string{tF, q, w, tC, p}, "#")))
	}
	hexUUID := func() string { return strings.ReplaceAll(generateUUID(), "-", "") }

	// Replay material (w blob, tC, device uuid1) from a harvested token.
	// EXP_CAPTCHA_TOKEN_DB selects the source DB (default: the bridge DB);
	// newest row is picked so we don't reuse an already-consumed token.
	replaySource := os.Getenv("EXP_CAPTCHA_TOKEN_DB")
	if replaySource == "" {
		replaySource = dbPath
	}
	var replayW, replayTC, replayUUID1 string
	if db2, err := sql.Open("sqlite", replaySource); err == nil {
		var raw string
		if err := db2.QueryRow("SELECT token FROM tokens ORDER BY id DESC LIMIT 1").Scan(&raw); err == nil {
			if dec, err := base64.StdEncoding.DecodeString(raw); err == nil {
				parts := strings.Split(string(dec), "#")
				if len(parts) == 5 {
					replayW, replayTC = parts[2], parts[3]
					if hh := strings.SplitN(parts[1], "-h-", 2); len(hh) == 2 {
						replayUUID1 = hh[0]
					}
				}
			}
		}
		db2.Close()
	}

	// ── mint-replay-full: replay EVERYTHING from an unconsumed token ──
	// Rebuild the token byte-for-byte from the newest row (uuid1 + ORIGINAL
	// timestamp + uuid2 + w + tC, MD5 recomputed). If this passes while
	// mint-replay-dev fails, the binding is on the Q timestamp or uuid2; if
	// it also fails, the token is more than the sum of its fields (e.g.
	// server-side state bound to first-verify). Uses the NEWEST row so it is
	// unconsumed at this point (verify-newest runs later and reads the same
	// row — order matters: run this probe FIRST to consume it).
	if run("mint-replay-full") {
		var raw string
		if db2, err := sql.Open("sqlite", replaySource); err == nil {
			_ = db2.QueryRow("SELECT token FROM tokens ORDER BY id DESC LIMIT 1").Scan(&raw)
			db2.Close()
		}
		if raw == "" {
			t.Logf("mint-replay-full: no token in %s", replaySource)
		} else {
			dec, _ := base64.StdEncoding.DecodeString(raw)
			parts := strings.Split(string(dec), "#")
			if len(parts) != 5 {
				t.Logf("mint-replay-full: unexpected token format (%d parts)", len(parts))
			} else {
				q := parts[1]
				sum := md5.Sum([]byte(strings.Join([]string{parts[0], q, parts[2], parts[3], "daye,raolewoba!"}, "#")))
				p := fmt.Sprintf("%x", sum)
				tok := base64Encode([]byte(strings.Join([]string{parts[0], q, parts[2], parts[3], p}, "#")))
				// Sanity: rebuilt token must be byte-identical to the original.
				identical := tok == raw
				t.Logf("mint-replay-full: rebuilt %db token (uuid1+ts+uuid2+w+tC all original) — byte-identical=%v", len(tok), identical)
				verifyWithoutToken("mint-replay-full", tok)
			}
		}
	}

	// ── mint-replay-dev: replay w-blob + tC + device uuid1 (fresh ts+uuid2) ──
	// Same as mint-replay-dev but isolated: run it right after consuming the
	// newest row above? No — needs its OWN unconsumed row. Reads the SECOND
	// newest (the newest was consumed by mint-replay-full when both run).
	if run("mint-replay-dev2") {
		var raw string
		if db2, err := sql.Open("sqlite", replaySource); err == nil {
			_ = db2.QueryRow("SELECT token FROM tokens ORDER BY id DESC LIMIT 1 OFFSET 1").Scan(&raw)
			db2.Close()
		}
		if raw == "" {
			t.Logf("mint-replay-dev2: no token in %s (offset 1)", replaySource)
		} else {
			dec, _ := base64.StdEncoding.DecodeString(raw)
			parts := strings.Split(string(dec), "#")
			if len(parts) != 5 {
				t.Logf("mint-replay-dev2: unexpected token format")
			} else {
				uuid1 := strings.SplitN(parts[1], "-h-", 2)[0]
				rest := strings.SplitN(parts[1], "-h-", 2)[1]
				oldTS := strings.SplitN(rest, "-", 2)[0]
				uuid2 := strings.SplitN(rest, "-", 2)[1]
				// Keep uuid1 + uuid2, refresh ONLY the timestamp.
				q := uuid1 + "-h-" + strconv.FormatInt(time.Now().UnixMilli(), 10) + "-" + uuid2
				sum := md5.Sum([]byte(strings.Join([]string{parts[0], q, parts[2], parts[3], "daye,raolewoba!"}, "#")))
				p := fmt.Sprintf("%x", sum)
				tok := base64Encode([]byte(strings.Join([]string{parts[0], q, parts[2], parts[3], p}, "#")))
				t.Logf("mint-replay-dev2: rebuilt %db (uuid1+uuid2 original, ts %s→now, w+tC original)", len(tok), oldTS)
				verifyWithoutToken("mint-replay-dev2", tok)
			}
		}
	}

	// ── mint-replay-uuid2: keep uuid1+ts, swap ONLY uuid2 ──────
	// Isolates the last variable: if mint-replay-dev2 (ts swapped) failed
	// but this passes, then uuid2 is free and ONLY the timestamp is bound;
	// if this also fails, both ts and uuid2 are bound. Uses OFFSET 2 (rows
	// 0 and 1 were consumed by mint-replay-full / mint-replay-dev2).
	if run("mint-replay-uuid2") {
		var raw string
		if db2, err := sql.Open("sqlite", replaySource); err == nil {
			_ = db2.QueryRow("SELECT token FROM tokens ORDER BY id DESC LIMIT 1 OFFSET 2").Scan(&raw)
			db2.Close()
		}
		if raw == "" {
			t.Logf("mint-replay-uuid2: no token in %s (offset 2)", replaySource)
		} else {
			dec, _ := base64.StdEncoding.DecodeString(raw)
			parts := strings.Split(string(dec), "#")
			if len(parts) != 5 {
				t.Logf("mint-replay-uuid2: unexpected token format")
			} else {
				oldQ := parts[1]
				uuid1 := strings.SplitN(oldQ, "-h-", 2)[0]
				rest := strings.SplitN(oldQ, "-h-", 2)[1]
				tsStr := strings.SplitN(rest, "-", 2)[0]
				q := uuid1 + "-h-" + tsStr + "-" + hexUUID()
				sum := md5.Sum([]byte(strings.Join([]string{parts[0], q, parts[2], parts[3], "daye,raolewoba!"}, "#")))
				p := fmt.Sprintf("%x", sum)
				tok := base64Encode([]byte(strings.Join([]string{parts[0], q, parts[2], parts[3], p}, "#")))
				t.Logf("mint-replay-uuid2: rebuilt %db (uuid1+ts original, uuid2 fresh, w+tC original)", len(tok))
				verifyWithoutToken("mint-replay-uuid2", tok)
			}
		}
	}

	// ── verify-offset: positive control for the OFFSET rows used above ──
	// The mint-replay-* probes at OFFSET N consume/mutate assumptions about
	// which rows are still unconsumed. This control verifies the row at
	// EXP_CAPTCHA_VERIFY_OFFSET (default 2) AS-IS: if it fails, the row was
	// already consumed and any mint probe reading it is inconclusive.
	if run("verify-offset") {
		off := 2
		if ev := os.Getenv("EXP_CAPTCHA_VERIFY_OFFSET"); ev != "" {
			if v, err := strconv.Atoi(ev); err == nil && v >= 0 {
				off = v
			}
		}
		var raw string
		if db2, err := sql.Open("sqlite", replaySource); err == nil {
			_ = db2.QueryRow("SELECT token FROM tokens ORDER BY id DESC LIMIT 1 OFFSET ?", off).Scan(&raw)
			db2.Close()
		}
		if raw == "" {
			t.Logf("verify-offset: no token at OFFSET %d in %s", off, replaySource)
		} else {
			dec, _ := base64.StdEncoding.DecodeString(raw)
			parts := strings.Split(string(dec), "#")
			age := "unknown"
			if len(parts) == 5 {
				if hh := strings.SplitN(parts[1], "-h-", 2); len(hh) == 2 {
					if ms, err := strconv.ParseInt(strings.SplitN(hh[1], "-", 2)[0], 10, 64); err == nil {
						age = fmt.Sprintf("%.0fs", float64(time.Now().UnixMilli()-ms)/1000.0)
					}
				}
			}
			t.Logf("verify-offset: verifying token at OFFSET %d (age %s) AS-IS — positive control", off, age)
			verifyWithoutToken("verify-offset", raw)
		}
	}

	if run("mint-empty") {
		tok := mintDeviceToken("", "0", hexUUID(), hexUUID())
		t.Logf("mint-empty: minted %db token (w=<empty>, tC=0, fresh uuids)", len(tok))
		verifyWithoutToken("mint-empty", tok)
	}
	if run("mint-replay") {
		if replayW == "" {
			t.Logf("mint-replay: no harvested token found in %s", replaySource)
		} else {
			tok := mintDeviceToken(replayW, replayTC, hexUUID(), hexUUID())
			t.Logf("mint-replay: minted %db token (replayed w-blob %db + tC=%s, fresh uuids)", len(tok), len(replayW), replayTC)
			verifyWithoutToken("mint-replay", tok)
		}
	}
	if run("mint-replay-dev") {
		if replayW == "" {
			t.Logf("mint-replay-dev: no harvested token found in %s", replaySource)
		} else {
			tok := mintDeviceToken(replayW, replayTC, replayUUID1, hexUUID())
			t.Logf("mint-replay-dev: minted %db token (replayed w-blob + device uuid1 %.12s…, fresh uuid2)", len(tok), replayUUID1)
			verifyWithoutToken("mint-replay-dev", tok)
		}
	}

	// ── verify-newest: verify the NEWEST harvested token as-is (TTL probe) ─
	// Consumes that token (single-use). Used to measure how long a harvested
	// token stays valid after collection.
	if run("verify-newest") {
		var newest string
		if db2, err := sql.Open("sqlite", replaySource); err == nil {
			_ = db2.QueryRow("SELECT token FROM tokens ORDER BY id DESC LIMIT 1").Scan(&newest)
			db2.Close()
		}
		if newest == "" {
			t.Logf("verify-newest: no token in %s", replaySource)
		} else {
			age := "unknown"
			if dec, err := base64.StdEncoding.DecodeString(newest); err == nil {
				parts := strings.Split(string(dec), "#")
				if len(parts) == 5 {
					if hh := strings.SplitN(parts[1], "-h-", 2); len(hh) == 2 {
						tsStr := strings.SplitN(hh[1], "-", 2)[0]
						if ms, err := strconv.ParseInt(tsStr, 10, 64); err == nil {
							age = fmt.Sprintf("%.0fs", float64(time.Now().UnixMilli()-ms)/1000.0)
						}
					}
				}
			}
			t.Logf("verify-newest: verifying harvested token (age %s) from %s", age, replaySource)
			verifyWithoutToken("verify-newest", newest)
		}
	}

	// ── verify-batch: verify+chat the N NEWEST harvested tokens in order ──
	// Measures batch quality (how many harvested tokens actually pass) when
	// consumed back-to-back like the bridge does. Each used token is deleted
	// from the source DB (it is single-use either way). Count via
	// EXP_CAPTCHA_VERIFY_COUNT (default 3).
	if run("verify-batch") {
		n := 3
		if ev := os.Getenv("EXP_CAPTCHA_VERIFY_COUNT"); ev != "" {
			if v, err := strconv.Atoi(ev); err == nil && v > 0 {
				n = v
			}
		}
		db2, err := sql.Open("sqlite", replaySource)
		if err != nil {
			t.Logf("verify-batch: open %s: %v", replaySource, err)
		} else {
			ok := 0
			for k := 0; k < n; k++ {
				var id int
				var tok string
				if err := db2.QueryRow("SELECT id, token FROM tokens ORDER BY id DESC LIMIT 1").Scan(&id, &tok); err != nil {
					t.Logf("verify-batch: DB empty after %d/%d", k, n)
					break
				}
				age := "?"
				if dec, err := base64.StdEncoding.DecodeString(tok); err == nil {
					parts := strings.Split(string(dec), "#")
					if len(parts) == 5 {
						if hh := strings.SplitN(parts[1], "-h-", 2); len(hh) == 2 {
							tsStr := strings.SplitN(hh[1], "-", 2)[0]
							if ms, err := strconv.ParseInt(tsStr, 10, 64); err == nil {
								age = fmt.Sprintf("%.0fs", float64(time.Now().UnixMilli()-ms)/1000.0)
							}
						}
					}
				}
				name := fmt.Sprintf("verify-batch#%d(id=%d,age=%s)", k+1, id, age)
				if doVerify(name, tok) {
					ok++
				}
				if _, err := db2.Exec("DELETE FROM tokens WHERE id = ?", id); err != nil {
					t.Logf("verify-batch: delete id=%d: %v", id, err)
				}
				time.Sleep(3 * time.Second)
			}
			t.Logf("verify-batch: RESULT %d/%d verify+chat OK", ok, n)
			db2.Close()
		}
	}

	// ── Verify the SAME deviceToken twice (two certifyIDs) ───────────────
	// If a deviceToken survives multiple VerifyCaptchaV3 calls, one harvested
	// token can mint many captcha params (and we can pre-validate tokens at
	// collection time).
	if run("verify-twice") {
		deviceToken, ok := getNextToken()
		if !ok {
			t.Logf("verify-twice: no tokens in DB")
		} else {
			t.Logf("verify-twice: using token %.40s... (kept in DB)", deviceToken)
			for round := 1; round <= 2; round++ {
				certifyID, err := initCaptcha()
				if err != nil {
					t.Logf("verify-twice r%d: initCaptcha error: %v", round, err)
					break
				}
				argValue := generateArg(certifyID)
				ct := currentTimeMillis()
				track := Track{
					TrackList:      TrackList{StartTime: ct},
					TrackStartTime: ct,
					VerifyTime:     ct + 300,
					Arg:            argValue,
				}
				jsonBytes, _ := jsonMarshal(track)
				h := aliHash(string(jsonBytes), "0000")
				compressed := zlibCompress([]byte(h + string(jsonBytes)))
				finalVal := encrypt([]byte(base64Encode(compressed)))
				payload, err := verifyCaptcha(certifyID, finalVal, deviceToken)
				if err != nil {
					t.Logf("verify-twice r%d: error: %v", round, err)
					break
				}
				if payload == "" {
					t.Logf("verify-twice r%d: VerifyResult=false — token single-use or bad", round)
					break
				}
				t.Logf("verify-twice r%d: *** SUCCESS *** %db payload from SAME token", round, len(payload))
				time.Sleep(1500 * time.Millisecond)
			}
		}
	}

	// ── verify-minter: pull a token from the on-demand minter daemon
	// (cmd/token-minter, see EXP_CAPTCHA_RESULTS.md §9) and verify it
	// live via VerifyCaptchaV3 + a real chat request. Proves a LIVE PAGE
	// can mint unlimited fresh tokens on demand (26ms avg) — no batch
	// harvesting needed. Minter URL via EXP_CAPTCHA_MINTER (default
	// http://127.0.0.1:7331/mint).
	if run("verify-minter") {
		minterURL := os.Getenv("EXP_CAPTCHA_MINTER")
		if minterURL == "" {
			minterURL = "http://127.0.0.1:7331/mint"
		}
		mresp, merr := http.Get(minterURL)
		if merr != nil {
			t.Logf("verify-minter: minter daemon unreachable at %s: %v", minterURL, merr)
		} else {
			mbody, _ := io.ReadAll(io.LimitReader(mresp.Body, 4096))
			mresp.Body.Close()
			var mj struct {
				OK     bool   `json:"ok"`
				Token  string `json:"token"`
				MintMs int64  `json:"mint_ms"`
				Error  string `json:"error"`
			}
			if err := json.Unmarshal(mbody, &mj); err != nil {
				t.Logf("verify-minter: bad minter response: %v (%.200s)", err, mbody)
			} else if !mj.OK || mj.Token == "" {
				t.Logf("verify-minter: minter error: %s", mj.Error)
			} else {
				t.Logf("verify-minter: got %db token from minter in %dms — verifying live", len(mj.Token), mj.MintMs)
				if doVerify("verify-minter", mj.Token) {
					t.Logf("verify-minter: *** CONFIRMED *** on-demand minter tokens pass live verification")
				}
			}
		}
	}

	// ── verify-qbless: mint via the q-bless session (qbless.json) using
	// the BRIDGE's own getNextToken path — proves the pure-Go mint on a
	// blessed Q works end-to-end inside the bridge (no daemon, no DB).
	if run("verify-qbless") {
		t0 := time.Now()
		tok, ok := getNextToken()
		mintMs := time.Since(t0).Milliseconds()
		if !ok {
			t.Logf("verify-qbless: no token from qbless path (session missing?)")
		} else {
			t.Logf("verify-qbless: minted %db token in %dms — verifying live", len(tok), mintMs)
			if doVerify("verify-qbless", tok) {
				t.Logf("verify-qbless: *** CONFIRMED *** pure-Go mint on blessed Q passes live verification")
			}
		}
	}

	// ── Reuse ONE captcha param across multiple requests ─────────────────
	if run("reuse-captcha") {
		param, err := getCaptchaVerifyParam()
		if err != nil {
			t.Fatalf("reuse-captcha: generate param: %v", err)
		}
		for i := 1; i <= 3; i++ {
			b := baseBody()
			b["captcha_verify_param"] = param
			code, out := expCaptchaChat(t, client, b, prompt)
			t.Logf("reuse-captcha #%d: HTTP %d | %s", i, code, out)
			time.Sleep(2 * time.Second)
		}
	}
}
