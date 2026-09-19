package zbridge

// OpenAI-compatible image generation backed by image.z.ai.
//
// Ported from the fclaude-kimi-k3 zai-proxy (commit 0c83f55). The image
// service authenticates with its OWN session cookie — separate from the
// chat.z.ai token — so ZAI_IMAGE_TOKEN overrides, falling back to the chat
// session token (works when the same account is logged into both).

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// imageGenerateURL is the image service endpoint; ZAI_IMAGE_HOST overrides
// for testing or region variants.
const imageGenerateURL = "https://image.z.ai/api/proxy/images/generate"

// imageGenerateTimeout bounds one generation round-trip.
const imageGenerateTimeout = 120 * time.Second

func imageGenerationsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	var req struct {
		Prompt      string `json:"prompt"`
		Size        string `json:"size"`
		N           *int   `json:"n"`
		Ratio       string `json:"ratio"`
		Resolution  string `json:"resolution"`
		NoWatermark *bool  `json:"no_watermark"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, formatOpenAIError("Invalid JSON: "+err.Error(), "invalid_request_error", nil))
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeJSON(w, http.StatusBadRequest, formatOpenAIError("prompt is required", "invalid_request_error", nil))
		return
	}
	if req.N != nil && *req.N != 1 {
		writeJSON(w, http.StatusBadRequest, formatOpenAIError("n must be 1", "invalid_request_error", nil))
		return
	}

	token := os.Getenv("ZAI_IMAGE_TOKEN")
	if token == "" {
		session.mu.Lock()
		token = session.Token
		session.mu.Unlock()
	}
	if token == "" {
		writeJSON(w, 401, formatOpenAIError("no session token available for image generation (set ZAI_IMAGE_TOKEN)", "authentication_error", nil))
		return
	}

	// Map OpenAI "size" (WxH) to Z.AI ratio + resolution. Defaults match
	// the image TUI: 9:16 / 1K / watermark kept.
	ratio := req.Ratio
	resolution := req.Resolution
	switch strings.ToLower(req.Size) {
	case "1024x1024", "1:1", "512x512":
		ratio, resolution = "1:1", "1K"
	case "1792x1024", "16:9":
		ratio, resolution = "16:9", "1K"
	case "1024x1792", "9:16":
		ratio, resolution = "9:16", "1K"
	}
	if ratio == "" {
		ratio = "9:16"
	}
	if resolution == "" {
		resolution = "1K"
	}
	rmLabel := true // keep watermark by default
	if req.NoWatermark != nil {
		rmLabel = !*req.NoWatermark
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"prompt":              req.Prompt,
		"ratio":               ratio,
		"resolution":          resolution,
		"rm_label_watermark":  rmLabel,
	})

	ctx, cancel := context.WithTimeout(r.Context(), imageGenerateTimeout)
	defer cancel()
	host := os.Getenv("ZAI_IMAGE_HOST")
	if host == "" {
		host = imageGenerateURL
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, host, strings.NewReader(string(payload)))
	if err != nil {
		writeJSON(w, 500, formatOpenAIError("failed to build image request", "api_error", nil))
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Cookie", "session="+token)
	httpReq.Header.Set("User-Agent", "zai-tui/1.0")

	resp, err := zaiHTTPClient.Do(httpReq)
	if err != nil {
		log.Printf("[Images] generate failed: %s", err.Error())
		writeJSON(w, 502, formatOpenAIError("image service unreachable: "+err.Error(), "api_error", nil))
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == 401 {
		writeJSON(w, 401, formatOpenAIError("session token expired or invalid", "authentication_error", nil))
		return
	}

	var parsed struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Image struct {
				ImageURL   string `json:"image_url"`
				Size       string `json:"size"`
				Ratio      string `json:"ratio"`
				Resolution string `json:"resolution"`
			} `json:"image"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		writeJSON(w, 502, formatOpenAIError("image service returned bad response", "api_error", nil))
		return
	}
	if parsed.Code != 200 || parsed.Data.Image.ImageURL == "" {
		msg := parsed.Message
		if msg == "" {
			msg = "image service error"
		}
		log.Printf("[Images] upstream code %d: %s", parsed.Code, msg)
		writeJSON(w, 502, formatOpenAIError(msg, "api_error", parsed.Code))
		return
	}

	writeJSON(w, 200, map[string]interface{}{
		"created": time.Now().Unix(),
		"data": []map[string]interface{}{
			{
				"url":        parsed.Data.Image.ImageURL,
				"size":       parsed.Data.Image.Size,
				"ratio":      parsed.Data.Image.Ratio,
				"resolution": parsed.Data.Image.Resolution,
			},
		},
	})
}
