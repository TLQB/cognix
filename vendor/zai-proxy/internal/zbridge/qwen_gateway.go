// qwen_gateway.go
//
// Optional passthrough to a local qwen-proxy (see ../qwen-bridge): when
// QWEN_PROXY_URL is set (the bundle launcher sets it to http://localhost:3003),
// the bridge merges the Qwen model list into /v1/models and forwards
// qwen3* /v1/chat/completions requests to that upstream, streaming the SSE
// response back unchanged. Everything else stays on the Z.AI path.
//
// The gateway is deliberately thin: no protocol translation (qwen-proxy
// already speaks OpenAI), no buffering (SSE flows through as chunks arrive),
// and a short fail-fast health probe so a dead qwen-proxy never stalls the
// model picker — modelsHandler just skips the Qwen section.

package zbridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// qwenGatewayURL returns the configured qwen-proxy base URL, or "" when the
// gateway is off. QWEN_PROXY_URL=off disables explicitly.
func qwenGatewayURL() string {
	u := strings.TrimSpace(os.Getenv("QWEN_PROXY_URL"))
	if u == "" || strings.EqualFold(u, "off") {
		return ""
	}
	return strings.TrimRight(u, "/")
}

var (
	qwenGatewayMu     sync.Mutex
	qwenGatewayModels []ModelInfo
	qwenGatewayAt     time.Time
	qwenGatewayDown   bool // sticky: after a failed probe, retry only every TTL
)

const qwenGatewayTTL = 5 * time.Minute

// isQwenModel reports whether a /v1/chat/completions model id routes to the
// qwen gateway. Qwen ids look like qwen3.8-max, qwen3.5-plus, qwen3-coder-plus.
func isQwenModel(model string) bool {
	if qwenGatewayURL() == "" {
		return false
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "qwen")
}

func formatOpenAIError3(message, errType string) interface{} {
	return map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    errType,
			"code":    nil,
			"param":   nil,
		},
	}
}

// fetchQwenGatewayModels pulls /v1/models from the qwen-proxy (auth: the
// bridge's own auth token — the bundled qwen-proxy runs with AUTH_TOKEN
// matched to ours). Cached for the same TTL as the Z.AI list. Returns nil
// when the gateway is off or the upstream is unreachable (model picker then
// shows Z.AI models only, never an error).
func fetchQwenGatewayModels() []ModelInfo {
	if qwenGatewayURL() == "" {
		return nil
	}
	qwenGatewayMu.Lock()
	if len(qwenGatewayModels) > 0 && time.Since(qwenGatewayAt) < qwenGatewayTTL {
		down := qwenGatewayDown
		qwenGatewayMu.Unlock()
		if down {
			return nil
		}
		return qwenGatewayModels
	}
	qwenGatewayMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", qwenGatewayURL()+"/v1/models", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+config.Auth.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		qwenGatewayMu.Lock()
		qwenGatewayDown = true
		qwenGatewayMu.Unlock()
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		qwenGatewayMu.Lock()
		qwenGatewayDown = true
		qwenGatewayMu.Unlock()
		return nil
	}

	var list struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			Description string `json:"description"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(body, &list); err != nil {
		return nil
	}
	models := make([]ModelInfo, 0, len(list.Data))
	for _, m := range list.Data {
		name := m.DisplayName
		if name == "" {
			name = m.ID
		}
		models = append(models, ModelInfo{ID: m.ID, Name: name, Description: m.Description})
	}
	qwenGatewayMu.Lock()
	qwenGatewayModels = models
	qwenGatewayAt = time.Now()
	qwenGatewayDown = false
	qwenGatewayMu.Unlock()
	return models
}

// forwardToQwenGateway proxies a /v1/chat/completions request to the
// qwen-proxy and streams the response back. It returns (false, nil) when the
// gateway is off / the model is not a Qwen model (caller continues on the
// Z.AI path) and (false, err) after a failed forwarding attempt that already
// wrote an error response.
func forwardToQwenGateway(w http.ResponseWriter, r *http.Request, body []byte) (handled bool, err error) {
	if !isQwenModel(modelFromBody(body)) {
		return false, nil
	}
	base := qwenGatewayURL()
	if base == "" {
		return false, nil
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/v1/chat/completions", strings.NewReader(string(body)))
	if err != nil {
		writeJSON(w, 502, formatOpenAIError3("qwen gateway: request build: "+err.Error(), "api_error"))
		return true, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+config.Auth.Token)
	req.Header.Set("Accept", r.Header.Get("Accept"))
	if r.Header.Get("X-Request-Id") != "" {
		req.Header.Set("X-Request-Id", r.Header.Get("X-Request-Id"))
	}

	client := &http.Client{Timeout: 0} // stream: r.Context governs lifetime
	resp, err := client.Do(req)
	if err != nil {
		writeJSON(w, 502, formatOpenAIError3("qwen gateway: upstream unreachable ("+base+"): "+err.Error(), "api_error"))
		return true, err
	}
	defer resp.Body.Close()

	// Copy SSE (or JSON) through untouched.
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return true, werr
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if rerr != nil {
			return true, nil // io.EOF or upstream abort; copied bytes are delivered
		}
	}
}

// modelFromBody extracts the model id from a raw /v1/chat/completions body
// without disturbing the handler's own parse (best-effort, "" on error).
func modelFromBody(body []byte) string {
	var b struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return ""
	}
	return b.Model
}
