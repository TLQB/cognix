// End-to-end test for the slide export passthrough: the editor POSTs a deck
// (files.html[] + files.css[]) to /v1/slides/export/pptx, the bridge forwards
// it to the upstream /sandbox/html-to-ppt with session auth + WAF headers,
// and the pptx blob is relayed byte-for-byte with its Content-Disposition.
package tests

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zai-api/internal/zbridge"
)

const fakePPTX = "FAKE-PPTX-BYTES-0001"

func TestSlidesExportPptxEndToEnd(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("authorization")
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.presentationml.presentation")
		w.Header().Set("Content-Disposition", `attachment; filename="presentation.pptx"`)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakePPTX)
	}))
	defer upstream.Close()

	oldBase := zbridge.BASE_URL
	zbridge.BASE_URL = upstream.URL
	restoreSession := zbridge.OverrideSessionState("test-token", "test-user", true)
	cfg := zbridge.GetConfig()
	time.Sleep(30 * time.Millisecond)

	defer func() {
		restoreSession()
		zbridge.BASE_URL = oldBase
	}()

	deck := map[string]interface{}{
		"chatId":    "conv-e2e-export",
		"versionId": "v1",
		"upload":    false,
		"filename":  "presentation.pptx",
		"files": map[string]interface{}{
			"html": []string{"<section class=slide>1</section>", "<section class=slide>2</section>"},
			"css":  []string{".slide{width:1280px;height:720px}"},
		},
	}
	bodyJSON, _ := json.Marshal(deck)
	req := httptest.NewRequest("POST", "/v1/slides/export/pptx", bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Auth.Token)
	rec := httptest.NewRecorder()
	zbridge.NewHandler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/sandbox/html-to-ppt" {
		t.Errorf("upstream path = %q, want /sandbox/html-to-ppt", gotPath)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("upstream auth = %q, want session token bearer", gotAuth)
	}
	// The deck body reached upstream untouched.
	var fwd struct {
		Files struct {
			HTML []string `json:"html"`
			CSS  []string `json:"css"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(gotBody), &fwd); err != nil {
		t.Fatalf("forwarded body decode: %v (%s)", err, gotBody)
	}
	if len(fwd.Files.HTML) != 2 || !strings.Contains(fwd.Files.CSS[0], "1280px") {
		t.Errorf("forwarded deck = %s", gotBody)
	}
	// The blob was relayed with its disposition header.
	if rec.Body.String() != fakePPTX {
		t.Errorf("relayed body = %q", rec.Body.String())
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "presentation.pptx") {
		t.Errorf("content-disposition = %q", cd)
	}
}
