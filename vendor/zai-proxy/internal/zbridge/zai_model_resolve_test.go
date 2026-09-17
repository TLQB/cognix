package zbridge

import (
	"testing"
	"time"
)

// seedModelsCache primes the in-memory model cache so resolveZAIModelID and
// fetchModelsFromZAI are exercised without network access. Restores the
// previous cache state when the test finishes.
func seedModelsCache(t *testing.T, models []ModelInfo) {
	t.Helper()
	modelsCacheMu.Lock()
	oldCache, oldTime := modelsCache, modelsCacheTime
	modelsCache = models
	modelsCacheTime = time.Now()
	modelsCacheMu.Unlock()
	t.Cleanup(func() {
		modelsCacheMu.Lock()
		modelsCache, modelsCacheTime = oldCache, oldTime
		modelsCacheMu.Unlock()
	})
}

// TestResolveZAIModelID locks in the client-id -> Z.AI-id mapping. The
// load-bearing case is glm-5.3-flash: Z.AI serves it under the preview id
// "x-preview-l" and answers the raw id with a 500, so the display-name
// fallback must kick in whenever the requested id is absent from the live
// model list.
func TestResolveZAIModelID(t *testing.T) {
	seedModelsCache(t, []ModelInfo{
		{ID: "x-preview-l", Name: "GLM-5.3-Flash"},
		{ID: "glm-5.3", Name: "GLM-5.3"},
		{ID: "glm-5.2", Name: "GLM-5.2"},
		{ID: "glm-4.7", Name: "GLM-4.7"},
	})

	cases := []struct {
		in   string
		want string
	}{
		{"glm-5.2", "glm-5.2"},             // exact id present in list
		{"GLM-5.2", "glm-5.2"},             // exact match is case-insensitive
		{"glm-5.3-flash", "x-preview-l"},   // preview alias via display name
		{"GLM-5.3-Flash", "x-preview-l"},   // display name verbatim
		{"glm_5.3 flash", "x-preview-l"},   // separator-insensitive
		{"glm-5.3", "glm-5.3"},             // exact id present in list
		{"unknown-model", "unknown-model"}, // unknown ids pass through
		{"", ""},                           // empty passes through
		{"z-ai-proxy/glm-5.3-flash", "x-preview-l"}, // Freebuff picker id: vendor stripped, then aliased
		{"z-ai-proxy/glm-4.7", "glm-4.7"},           // Freebuff picker id: vendor stripped
		{"z-ai/glm-5.3", "glm-5.3"},                 // Freebuff picker id: vendor stripped
		{"z-ai/glm-5.2", "glm-5.2"},                 // Freebuff picker id: vendor stripped
		{"minimax-m3", "GLM-5-Turbo"},               // CLI editor-agent default: aliased so Z.AI doesn't 500
		{"mimo-v2.5", "GLM-5-Turbo"},                // CLI editor-agent default: aliased so Z.AI doesn't 500
	}
	for _, c := range cases {
		if got := resolveZAIModelID(c.in); got != c.want {
			t.Errorf("resolveZAIModelID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestResolveZAIModelIDPromotedPreview verifies that once Z.AI promotes a
// preview to its permanent id, the exact-id branch wins and no alias log/
// fallback is needed.
func TestResolveZAIModelIDPromotedPreview(t *testing.T) {
	seedModelsCache(t, []ModelInfo{
		{ID: "glm-5.3-flash", Name: "GLM-5.3-Flash"},
		{ID: "glm-5.2", Name: "GLM-5.2"},
	})
	if got := resolveZAIModelID("glm-5.3-flash"); got != "glm-5.3-flash" {
		t.Errorf("resolveZAIModelID(glm-5.3-flash) = %q, want glm-5.3-flash", got)
	}
}

func TestNormalizeModelName(t *testing.T) {
	cases := [][2]string{
		{"GLM-5.3-Flash", "glm5.3flash"},
		{"glm-5.3-flash", "glm5.3flash"},
		{"glm_5.3 flash", "glm5.3flash"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeModelName(c[0]); got != c[1] {
			t.Errorf("normalizeModelName(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}
