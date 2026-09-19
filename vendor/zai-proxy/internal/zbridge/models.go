// Code moved from the original main.go monolith during the internal/ restructure.
// See README "Project Structure". Part of the Z.AI bridge core (package zbridge).

package zbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// fetchModelsFromZAI retrieves models from Z.AI /api/models,
// keeping only glm-4.7 and newer (the API returns newest-first).
func fetchModelsFromZAI() []ModelInfo {
	modelsCacheMu.Lock()
	defer modelsCacheMu.Unlock()

	if len(modelsCache) > 0 && time.Since(modelsCacheTime) < modelsCacheTTL {
		return modelsCache
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", BASE_URL+"/api/models", nil)
	if err != nil {
		logError("fetchModels request: " + err.Error())
		if len(modelsCache) > 0 {
			return modelsCache
		}
		return fallbackModels
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("authorization", "Bearer "+session.Token)
	req.Header.Set("User-Agent", zaiUserAgent)
	resp, err := zaiHTTPClient.Do(req)
	if err != nil {
		logError("fetchModels do: " + err.Error())
		if len(modelsCache) > 0 {
			return modelsCache
		}
		return fallbackModels
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		logError(fmt.Sprintf("fetchModels status: %d", resp.StatusCode))
		if len(modelsCache) > 0 {
			return modelsCache
		}
		return fallbackModels
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logError("fetchModels read: " + err.Error())
		if len(modelsCache) > 0 {
			return modelsCache
		}
		return fallbackModels
	}

	var apiResp struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Info struct {
				Name string `json:"name"`
				Meta struct {
					Description  string                 `json:"description"`
					Capabilities map[string]interface{} `json:"capabilities"`
				} `json:"meta"`
			} `json:"info"`
		} `json:"data"`
	}

	if err := json.Unmarshal(body, &apiResp); err != nil {
		logError("fetchModels parse: " + err.Error())
		if len(modelsCache) > 0 {
			return modelsCache
		}
		return fallbackModels
	}

	var filtered []ModelInfo
	for _, m := range apiResp.Data {
		filtered = append(filtered, ModelInfo{
			ID:           m.ID,
			Name:         m.Name,
			Description:  m.Info.Meta.Description,
			Capabilities: m.Info.Meta.Capabilities,
		})
		// glm-4.7 is the cutoff — stop here (inclusive)
		if m.ID == "glm-4.7" {
			break
		}
	}

	if len(filtered) > 0 {
		modelsCache = filtered
		modelsCacheTime = time.Now()
		logInfo(fmt.Sprintf("Fetched %d models from Z.AI", len(filtered)))
	}

	if len(modelsCache) > 0 {
		return modelsCache
	}
	return fallbackModels
}

// getFeaturesForModel maps a model's capabilities to a Features struct.
// enable_thinking defaults to true; web_search/auto_web_search default to false.
func getFeaturesForModel(modelID string) Features {
	f := Features{Thinking: true} // enable_thinking enabled by default
	for _, m := range fetchModelsFromZAI() {
		if strings.EqualFold(m.ID, modelID) {
			if v, ok := m.Capabilities["enable_thinking"].(bool); ok {
				f.Thinking = v
			}
			if v, ok := m.Capabilities["preview_mode"].(bool); ok {
				f.PreviewMode = v
			}
			break
		}
	}
	return f
}

// modelSupportsVision returns true only when the model's capabilities JSON
// explicitly contains "vision": true. Models without the field (or with it
// set to false) are treated as text-only.
func modelSupportsVision(modelID string) bool {
	if modelID == "" {
		return false
	}
	caps := getModelCapabilities(modelID)
	if caps == nil {
		return false
	}
	v, ok := caps["vision"].(bool)
	return ok && v
}

// getModelCapabilities returns the raw capabilities map for a model.
func getModelCapabilities(modelID string) map[string]interface{} {
	for _, m := range fetchModelsFromZAI() {
		if strings.EqualFold(m.ID, modelID) {
			return m.Capabilities
		}
	}
	return nil
}

// normalizeModelName lowers a model id/display name and strips separators so
// "glm-5.3-flash" compares equal to the display name "GLM-5.3-Flash".
func normalizeModelName(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "_", "")
	return s
}

// resolveZAIModelID maps a client-requested model id to the id Z.AI's
// /api/v2/chat/completions actually accepts. Z.AI sometimes serves new models
// under temporary preview ids (e.g. GLM-5.3-Flash as "x-preview-l"), so when
// the requested id is absent from the live model list we fall back to a
// case-insensitive display-name match. Unknown ids pass through unchanged,
// and once Z.AI promotes a preview to its permanent id the exact-id branch
// takes over automatically.
//
// Model ids may also arrive in OpenRouter-style "vendor/model" form (e.g.
// "glm/glm-5.3-flash") when the proxy serves Freebuff/Codebuff SDK clients,
// so the vendor segment is stripped before matching.
func resolveZAIModelID(requested string) string {
	if requested == "" {
		return requested
	}
	if i := strings.LastIndex(requested, "/"); i >= 0 {
		requested = requested[i+1:]
	}
	// Map freebuff/Codebuff picker ids to their EXACT Z.AI backend id,
	// checking the live model list BEFORE applying any alias.
	//
	// Z.AI serves GLM-5.3-Flash under the preview id "x-preview-l" (see
	// /api/models — id=x-preview-l, name="Lightweight flagship model").
	// The picker sends the display name "glm-5.3-flash", so we alias it
	// to the real Z.AI id — but only when the raw id is absent from the
	// live list, so a promoted preview id still wins.
	// Every other picker id (glm-5.3, glm-5.2, glm-4.7) already matches
	// a Z.AI id verbatim after the vendor prefix is stripped.
	//
	// Agent template defaults like "minimax-m3" (MiniMax M3) and
	// "mimo-v2.5" (Xiaomi MiMo) are NOT Z.AI models; Z.AI rejects them with 500. The Codebuff/Freebuff
	// CLI internally requests these ids for its editor agents regardless
	// of the user's picker choice, so they are aliased to GLM-5-Turbo
	// (fast, always present in the live list) to keep internal agent
	// traffic working instead of failing with 500.
	models := fetchModelsFromZAI()
	// Exact id wins: once Z.AI promotes a preview id to its permanent id the
	// raw id appears in the live list and must be sent verbatim (sending the
	// retired preview id would 500).
	for _, m := range models {
		if strings.EqualFold(m.ID, requested) {
			return m.ID
		}
	}
	// The alias fires only when the requested id is absent from the live
	// list — i.e. Z.AI still serves the model under its preview id.
	aliasMap := map[string]string{
		"glm-5.3-flash": "x-preview-l",
		"minimax-m3":    "GLM-5-Turbo",
		"mimo-v2.5":     "GLM-5-Turbo",
	}
	if aliased, ok := aliasMap[strings.ToLower(requested)]; ok {
		logInfo(fmt.Sprintf("model alias: %q mapped to %q", requested, aliased))
		requested = aliased
		for _, m := range models {
			if strings.EqualFold(m.ID, requested) {
				return m.ID
			}
		}
	}
	want := normalizeModelName(requested)
	for _, m := range models {
		if normalizeModelName(m.Name) == want {
			logInfo(fmt.Sprintf("model alias: %q not in Z.AI model list — using %q (%s)", requested, m.ID, m.Name))
			return m.ID
		}
	}
	return requested
}

// modelSupportsReasoningEffort returns true only when the model's capabilities
// JSON explicitly contains "reasoning_effort": true.
// Models with "reasoning_effort": false or without the field are NOT supported.
func modelSupportsReasoningEffort(modelID string) bool {
	if modelID == "" {
		return false
	}
	caps := getModelCapabilities(modelID)
	if caps == nil {
		return false
	}
	v, ok := caps["reasoning_effort"].(bool)
	return ok && v
}

// isValidReasoningEffort validates the accepted reasoning_effort values.
// Accepted: "high", "max". Any other value is rejected.
func isValidReasoningEffort(value string) bool {
	switch value {
	case "high", "max":
		return true
	default:
		return false
	}
}

func modelsHandler(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	models := fetchModelsFromZAI()
	// Qwen gateway (QWEN_PROXY_URL): append the bundled qwen-proxy's models
	// so the picker shows both families. Unreachable upstream → skipped
	// silently (models are a superset, never a blocker).
	models = append(models, fetchQwenGatewayModels()...)
	data := make([]map[string]interface{}, 0, len(models))
	for _, m := range models {
		caps := m.Capabilities
		if caps == nil {
			caps = map[string]interface{}{}
		}
		// The bridge's vision pipeline (files-array attachments) works with
		// every model the upstream serves - it is attachment-shaped, and the
		// model side decides what it can see. Advertise vision for all models
		// so clients that gate image input on this flag do not block the user.
		if _, ok := caps["vision"]; !ok {
			caps["vision"] = true
		}
		data = append(data, map[string]interface{}{
			"id":           m.ID,
			"object":       "model",
			"created":      now,
			"owned_by":     "z-ai",
			"display_name": m.Name,
			"description":  m.Description,
			"capabilities": caps,
		})
	}
	writeJSON(w, 200, map[string]interface{}{
		"object": "list",
		"data":   data,
	})
}

func modelsHandler2(w http.ResponseWriter, r *http.Request) {
	models := fetchModelsFromZAI()
	models = append(models, fetchQwenGatewayModels()...)
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	currentModel := "glm-5.2"
	if len(ids) > 0 {
		currentModel = ids[0]
	}
	writeJSON(w, 200, map[string]interface{}{
		"models":       ids,
		"currentModel": currentModel,
	})
}

// ============================================================================
// AGENT MODE (LEGACY SHIM) — Tools & Role Translation for Z.AI Compatibility
// ============================================================================
//
// NOTE: This is the LEGACY agent-mode shim, kept for backward compatibility
// (select it with --agent-mode-variant=legacy / AGENT_MODE_VARIANT=legacy).
// The default MODERN shim — XML-sectioned prompt, history summarization,
// tolerant marker/fence/payload parsing — lives in agent.go and is ported
// from the DeepseekFreeAPI reference implementation.
//
// Z.AI's unofficial /api/v2/chat/completions endpoint only accepts messages
// with role="user". System, assistant, and tool roles cause INTERNAL_ERROR.
// OpenAI-style tools/function_calls are also rejected.
//
// The legacy agent mode performs three transformations when active:
//
//   1. Mandatory System Prefix: A user message is prepended explaining the
//      prompt architecture (roles, tools) so the model can interpret the
//      rewritten conversation correctly.
//
//   2. Role Replacement: Every non-user message is rewritten as a user
//      message with a [ROLE: <original_role>] tag prepended to its content.
//      e.g. system message "Do X" becomes user message "[ROLE: system] Do X".
//
//   3. Tool Translation & Simulation: OpenAI tools JSON is rendered into a
//      user message with a strict contract: the model MUST emit any tool
//      invocation as a fenced JSON block of the form
//
//          <<<TOOL_CALL>>>
//          {"name":"<tool_name>","arguments":{...}}
//          <<<END_TOOL_CALL>>>
