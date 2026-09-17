// Code moved from the original main.go monolith during the internal/ restructure.
// See README "Project Structure". Part of the Qwen bridge core (package zbridge).

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

// fetchModelsFromQwen retrieves models from Qwen /api/v2/models (live list,
// 5-min cache). Falls back to the static list when Qwen is unreachable.
func fetchModelsFromQwen() []ModelInfo {
	modelsCacheMu.Lock()
	defer modelsCacheMu.Unlock()

	if len(modelsCache) > 0 && time.Since(modelsCacheTime) < modelsCacheTTL {
		return modelsCache
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", BASE_URL+"/api/v2/models/", nil)
	if err != nil {
		logError("fetchModels request: " + err.Error())
		return modelsCacheOrFallback()
	}
	setQwenHeaders(req, "", activeTokenSafe())
	resp, err := qwenClient().Do(req)
	if err != nil {
		logError("fetchModels do: " + err.Error())
		return modelsCacheOrFallback()
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		logError(fmt.Sprintf("fetchModels status: %d", resp.StatusCode))
		return modelsCacheOrFallback()
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logError("fetchModels read: " + err.Error())
		return modelsCacheOrFallback()
	}

	// Qwen shape: {"data": [{"id": "...", "name": "...", ...}]}
	var apiResp struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &apiResp); err != nil {
		logError("fetchModels parse: " + err.Error())
		return modelsCacheOrFallback()
	}

	var filtered []ModelInfo
	for _, m := range apiResp.Data {
		if m.ID == "" {
			continue
		}
		filtered = append(filtered, ModelInfo{
			ID:          m.ID,
			Name:        m.Name,
			Description: "Qwen model served via chat.qwen.ai",
		})
	}

	if len(filtered) > 0 {
		modelsCache = filtered
		modelsCacheTime = time.Now()
		logInfo(fmt.Sprintf("Fetched %d models from Qwen", len(filtered)))
	}
	return modelsCacheOrFallback()
}

func modelsCacheOrFallback() []ModelInfo {
	if len(modelsCache) > 0 {
		return modelsCache
	}
	return fallbackModels
}

// getFeaturesForModel maps a model's capabilities to a Features struct.
// enable_thinking defaults to true; web_search/auto_web_search default to false.
func getFeaturesForModel(modelID string) Features {
	f := Features{Thinking: true} // enable_thinking enabled by default
	for _, m := range fetchModelsFromQwen() {
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

// getModelCapabilities returns the raw capabilities map for a model.
func getModelCapabilities(modelID string) map[string]interface{} {
	for _, m := range fetchModelsFromQwen() {
		if strings.EqualFold(m.ID, modelID) {
			return m.Capabilities
		}
	}
	return nil
}

// normalizeModelName lowers a model id/display name and strips separators so
// "qwen3.6-plus" compares equal to the display name "Qwen3.6-Plus".
func normalizeModelName(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "_", "")
	return s
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
	models := fetchModelsFromQwen()
	data := make([]map[string]interface{}, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]interface{}{
			"id":           m.ID,
			"object":       "model",
			"created":      now,
			"owned_by":     "qwen",
			"display_name": m.Name,
			"description":  m.Description,
		})
	}
	writeJSON(w, 200, map[string]interface{}{
		"object": "list",
		"data":   data,
	})
}

func modelsHandler2(w http.ResponseWriter, r *http.Request) {
	models := fetchModelsFromQwen()
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	currentModel := qwenDefaultModel
	if len(ids) > 0 {
		currentModel = ids[0]
	}
	writeJSON(w, 200, map[string]interface{}{
		"models":       ids,
		"currentModel": currentModel,
	})
}

// ============================================================================
// AGENT MODE (LEGACY SHIM) — Tools & Role Translation for Qwen Compatibility
// ============================================================================
//
// NOTE: This is the LEGACY agent-mode shim, kept for backward compatibility
// (select it with --agent-mode-variant=legacy / AGENT_MODE_VARIANT=legacy).
// The default MODERN shim — XML-sectioned prompt, history summarization,
// tolerant marker/fence/payload parsing — lives in agent.go and is ported
// from the DeepseekFreeAPI reference implementation.
//
// Qwen's unofficial /api/v2/chat/completions endpoint only accepts messages
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
