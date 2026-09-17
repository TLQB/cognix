package zbridge

// Replay the byte-exact captured failing body (fail-body2.json from the
// native proxy debug log) directly against Z.AI, reading the FULL stream
// to the done event — NOT just the first bytes. The native-variant 500
// (INTERNAL_ERROR) arrives inline at the END of an otherwise-healthy 200
// stream, so short reads give false PASSes.
//
// Run: EXP_MULTITURN=1 ZAI_TOKEN=... go test ./internal/zbridge/ -run TestExpReplayExactBody -v -count=1
//      EXP_FE_VERSION=prod-fe-1.1.93 to match the proxy's scraped header.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// expStreamChat posts the body and drains the ENTIRE SSE stream. It returns
// the HTTP status, whether an inline error event was seen, the last data
// line, the total data events received, and the joined answer text
// (delta_content + edit_content) so probes can correlate glm_block tool-call
// emissions with the terminal error.
func expStreamChat(t *testing.T, client *http.Client, body map[string]interface{}, prompt string) (int, bool, string, int, string) {
	t.Helper()
	feVer := expFeVersion
	if feVer == "" {
		feVer = "prod-fe-1.1.92"
	}
	userID, _ := decodeJWT(config.ZaiToken)
	signature, _, _ := generateZaSignature(prompt, config.ZaiToken, userID)
	bodyBytes, _ := json.Marshal(body)

	var lastCode int
	var lastLine string
	for attempt := 0; attempt < 8; attempt++ {
		xff := zaiXffIP()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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
			return 0, true, "REQUEST ERROR: " + err.Error(), 0, ""
		}
		lastCode = resp.StatusCode
		if resp.StatusCode != 200 {
			buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1200))
			resp.Body.Close()
			cancel()
			return resp.StatusCode, true, strings.ReplaceAll(string(buf), "\n", " | "), 0, ""
		}

		// Drain the full SSE stream; watch for the inline error event.
		errSeen := false
		events := 0
		var answer strings.Builder
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 256*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			events++
			lastLine = line
			answer.WriteString(expExtractAnswer(line))
			if strings.Contains(line, "\"error\"") && strings.Contains(line, "INTERNAL_ERROR") {
				errSeen = true
			}
		}
		resp.Body.Close()
		cancel()
		if lastCode == 405 && strings.Contains(lastLine, "errors.aliyun.com") {
			markXffIPBad(xff)
			rotateXffIP()
			t.Logf("  attempt %d: WAF 405 on XFF=%s — rotated", attempt+1, xff)
			time.Sleep(300 * time.Millisecond)
			continue
		}
		return lastCode, errSeen, lastLine, events, answer.String()
	}
	return lastCode, false, lastLine, 0, ""
}

// expExtractAnswer pulls delta_content/edit_content chunks out of one SSE
// data line (crude, but enough to detect a glm_block tool-call emission).
func expExtractAnswer(line string) string {
	var b strings.Builder
	for _, field := range []string{"delta_content", "edit_content"} {
		key := `"` + field + `":"`
		i := strings.Index(line, key)
		if i < 0 {
			continue
		}
		rest := line[i+len(key):]
		j := strings.Index(rest, `"`)
		if j > 0 {
			b.WriteString(rest[:j])
		}
	}
	return b.String()
}

func TestExpReplayExactBody(t *testing.T) {
	if os.Getenv("EXP_MULTITURN") == "" {
		t.Skip("set EXP_MULTITURN=1 to run this experimental live probe")
	}
	verbose = true
	dbPath = "../../tokens.sqlite"
	if err := initDB(); err != nil {
		t.Fatalf("initDB: %v", err)
	}
	if config.ZaiToken == "" {
		t.Fatal("ZAI_TOKEN not set")
	}

	raw, err := os.ReadFile("/tmp/ab-bench/fail-body2.json")
	if err != nil {
		t.Fatalf("read captured body: %v (regenerate: LOG_LEVEL=debug proxy on the pivot case, then extract)", err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Full-stream replay: the proxy's failure mode is an inline
	// INTERNAL_ERROR at the END of a healthy 200 stream, so we must drain
	// every event, not just the head.
	t.Run("exact_bytes_full_stream", func(t *testing.T) {
		fails := 0
		for i := 0; i < 6; i++ {
			b := deepCopyBody(t, body)
			b["captcha_verify_param"] = mustCaptchaParam(t)
			prompt := "x"
			if s, ok := b["signature_prompt"].(string); ok {
				prompt = s
			}
			code, errSeen, last, events, _ := expStreamChat(t, expTestClient(), b, prompt)
			t.Logf("run %d: HTTP %d | events=%d | errSeen=%v | last=%s", i+1, code, events, errSeen, truncStr(last, 220))
			if errSeen {
				fails++
			}
		}
		if fails > 0 {
			t.Errorf("inline error on %d/6 full-stream replays — body content IS the trigger", fails)
		} else {
			t.Logf("0/6 inline errors — body content is NOT the trigger; look at proxy plumbing")
		}
	})
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
