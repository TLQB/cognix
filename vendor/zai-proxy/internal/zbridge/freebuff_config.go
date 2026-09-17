// Runtime-config endpoint for the Freeclaude TUI: GET/POST
// /api/v1/freebuff/config.
//
// The proxy's TOOL_LOOP_THINKING policy (disable thinking on mid-loop agent
// turns — last message = tool result) is env-only by default, which forces a
// proxy restart to flip it. This endpoint makes it runtime-tunable so the
// TUI's /thinking command can flip latency vs reasoning depth live, without
// touching the running session pool or streams.
//
// Shape (both directions): {"toolLoopThinking": "off" | "on"}
//
// Authed like the other /api/v1 routes; the TUI sends the proxy auth token.

package zbridge

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

// runtimeConfigMu guards writes to config fields that handlers mutate at
// runtime. Reads on the streaming hot path stay lock-free (the policy check
// is a single string compare; a torn read flips it one request late at most).
var runtimeConfigMu sync.Mutex

func normalizeToolLoopThinking(v string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "0", "false", "no":
		return "off", true
	case "on", "1", "true", "yes":
		return "on", true
	}
	return "", false
}

func freebuffConfigHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		runtimeConfigMu.Lock()
		value := config.ToolLoopThinking
		runtimeConfigMu.Unlock()
		writeJSON(w, 200, map[string]interface{}{
			"toolLoopThinking": value,
		})
		return

	case http.MethodPost:
		var body struct {
			ToolLoopThinking string `json:"toolLoopThinking"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, 400, map[string]interface{}{"error": "invalid JSON body"})
			return
		}
		value, ok := normalizeToolLoopThinking(body.ToolLoopThinking)
		if !ok {
			writeJSON(w, 400, map[string]interface{}{
				"error": "toolLoopThinking must be \"off\" or \"on\"",
			})
			return
		}
		runtimeConfigMu.Lock()
		config.ToolLoopThinking = value
		runtimeConfigMu.Unlock()
		logOperational("[Config] toolLoopThinking=" + value + " (set via /api/v1/freebuff/config)")
		writeJSON(w, 200, map[string]interface{}{
			"toolLoopThinking": value,
		})
		return

	default:
		writeJSON(w, 405, map[string]interface{}{"error": "method not allowed"})
	}
}
