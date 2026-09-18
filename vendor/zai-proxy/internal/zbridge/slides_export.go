package zbridge

// Export passthrough for locally generated slide decks.
//
// The chat.z.ai web client converts its agent-produced slide HTML into
// downloadable files through two endpoints:
//
//	POST /sandbox/html-to-ppt   {chatId, versionId, files:{html[],css[]}, upload, filename} -> pptx blob
//	POST /convert/ppt/stream    {chatId, options, pptVersion, pageMetadata}       -> SSE conversion
//
// A locally generated deck (see slides.go) has exactly the files.html[] /
// files.css[] shape those endpoints consume, so these handlers simply
// forward the editor's request body to the upstream endpoint with the
// session's auth + WAF headers and stream the binary/SSE reply straight
// back. No browser is involved on the client side.

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"time"
)

// exportTimeout bounds one conversion round-trip. Conversions render every
// page server-side, so large decks can take a while.
const exportTimeout = 5 * time.Minute

// slideExportHandler forwards an export request to the given upstream path
// and relays the response (status, content type, body) back to the caller.
func slideExportHandler(upstreamPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20)) // 64MB cap: decks are small, be safe
		if err != nil {
			writeJSON(w, http.StatusBadRequest, formatOpenAIError("read body: "+err.Error(), "invalid_request_error", nil))
			return
		}

		// The export endpoints need a logged-in token; guest uploads 401.
		session.mu.Lock()
		token := session.Token
		feVersion := session.FeVersion
		initialized := session.Initialized
		session.mu.Unlock()
		if token == "" || !initialized {
			if err := initializeSession(); err != nil {
				writeJSON(w, 502, formatOpenAIError("session init for export: "+err.Error(), "api_error", nil))
				return
			}
			session.mu.Lock()
			token = session.Token
			feVersion = session.FeVersion
			session.mu.Unlock()
		}

		ctx, cancel := context.WithTimeout(r.Context(), exportTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, BASE_URL+upstreamPath, bytes.NewReader(body))
		if err != nil {
			writeJSON(w, 502, formatOpenAIError("build export request: "+err.Error(), "api_error", nil))
			return
		}
		req.Header.Set("authorization", "Bearer "+token)
		req.Header.Set("User-Agent", zaiUserAgent)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if feVersion != "" {
			req.Header.Set("x-fe-Version", feVersion)
		}
		req.Header.Set("x-region", "overseas")

		resp, err := zaiHTTPClient.Do(req)
		if err != nil {
			log.Printf("[SlidesExport] %s failed: %s", upstreamPath, err.Error())
			writeJSON(w, 502, formatOpenAIError("export upstream error: "+err.Error(), "api_error", nil))
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
			log.Printf("[SlidesExport] %s upstream %d: %s", upstreamPath, resp.StatusCode, truncateString(string(respBody), 300))
			writeJSON(w, resp.StatusCode, formatOpenAIError(
				"export failed: upstream "+resp.Status+": "+truncateString(string(respBody), 300),
				"api_error", nil))
			return
		}

		// Relay success: binary blob (pptx/pdf) or SSE conversion stream.
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		if cd := resp.Header.Get("Content-Disposition"); cd != "" {
			w.Header().Set("Content-Disposition", cd)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}
