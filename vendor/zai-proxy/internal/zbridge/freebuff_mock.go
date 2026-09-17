// Freebuff/Codebuff backend mock endpoints.
//
// The Freebuff CLI (and the Codebuff SDK it embeds) does not only call the
// OpenAI-compatible chat route — before a run starts it also phones home to
// the backend website (getWebsiteUrl(), overridable at runtime via
// CODEBUFF_APP_URL / NEXT_PUBLIC_CODEBUFF_APP_URL) for:
//
//	GET  /api/healthz             — connection check
//	GET  /api/v1/me?fields=...    — user lookup by API key (must NOT be null,
//	                                 or the SDK cancels the run with
//	                                 "Invalid API key or user not found")
//	POST /api/v1/agent-runs       — run bookkeeping (START/FINISH actions);
//	                                 the runId only shows up in traces
//	GET  /api/v1/agents/<pub>/<agent>/<version> — published agent templates
//	GET  /api/v1/usage            — usage/credits display in the TUI
//
// Without these, a CLI pointed at the proxy either falls back to the real
// cloud or aborts every run at the user-lookup step. They are intentionally
// dumb static stubs: the proxy has no user database, so /api/v1/me echoes a
// stable synthetic user for any valid proxy auth token, and agent fetches
// return 404 so the runtime sticks to the built-in local agent templates.

package zbridge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// freebuffUserID is a stable synthetic user id. The real backend returns the
// signed-in user's row; we just need a non-empty string that the SDK can put
// in traces and acting-user headers.
const freebuffUserID = "zai-proxy-local-user"

// freebuffHealthHandler mimics the website's /api/healthz so the SDK's
// checkConnection() (which asserts body.status === "ok") succeeds.
func freebuffHealthHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]interface{}{
		"status":  "ok",
		"service": "zai-proxy freebuff stub",
	})
}

// freebuffMeHandler answers the SDK's user lookup. It must return a JSON
// object containing every field listed in ?fields=... (id is the only one
// the CLI runtime asks for) — returning null would cancel runs.
func freebuffMeHandler(w http.ResponseWriter, r *http.Request) {
	fields := r.URL.Query().Get("fields")
	out := map[string]interface{}{}
	for _, f := range strings.Split(fields, ",") {
		f = strings.TrimSpace(f)
		switch f {
		case "":
		case "id":
			out["id"] = freebuffUserID
		case "email":
			out["email"] = "zai-proxy@localhost"
		case "banned":
			out["banned"] = false
		case "created_at":
			out["created_at"] = time.Now().UTC().Format(time.RFC3339)
		default:
			// discord_id / stripe_customer_id and anything else: null is the
			// schema-plausible value for nullable columns.
			out[f] = nil
		}
	}
	writeJSON(w, 200, out)
}

// freebuffAgentRunsHandler stubs run bookkeeping. START returns a fake runId;
// FINISH/anything else is a no-op success. The return shape matches
// startAgentRun()'s expectation (it only reads responseBody.runId).
func freebuffAgentRunsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, 405, map[string]interface{}{"error": "method not allowed"})
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	// A malformed body is not fatal here — FINISH posts may legitimately race.
	_ = json.NewDecoder(r.Body).Decode(&body)

	if strings.EqualFold(body.Action, "START") {
		writeJSON(w, 200, map[string]interface{}{
			"runId": fmt.Sprintf("local-%d", time.Now().UnixNano()),
		})
		return
	}
	writeJSON(w, 200, map[string]interface{}{"ok": true})
}

// freebuffUsageHandler stubs the TUI's usage/credits query with an
// effectively-unlimited local answer so the UI never throttles.
func freebuffUsageHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]interface{}{
		"used":        0,
		"limit":       1 << 30,
		"remaining":   1 << 30,
		"unlimited":   true,
		"reset_after": nil,
	})
}

// freebuffSessionState tracks the one active freebuff session the stub
// admits. The real backend stores this per-user; the proxy has one user, so
// one in-memory slot is enough. Guarded by the HTTP server's serial per-conn
// handling for the same path, plus a mutex for safety across keep-alive.
var freebuffSessionState struct {
	mu        sync.Mutex
	instance  string
	model     string
	admitted  time.Time
	expiresAt time.Time
}

// freebuffSessionHandler stubs /api/v1/freebuff/session — the free-mode
// session gate the CLI polls before/while chatting. Semantics (from the
// CLI's freebuff-session-api.ts):
//
//	GET    → current state: {status:"none"} or {status:"active",...}
//	POST   → start a session for the model in x-freebuff-model; always admit
//	DELETE → end the session → {status:"ended"}
//
// The CLI only needs the status/instanceId/model/timing fields; everything
// else (quota snapshots, referral banners, subscription offers) is optional
// and omitted.
func freebuffSessionHandler(w http.ResponseWriter, r *http.Request) {
	freebuffSessionState.mu.Lock()
	defer freebuffSessionState.mu.Unlock()

	switch r.Method {
	case http.MethodGet:
		// Return status:"none" when no active session exists — the TUI landing
		// screen needs this to render the model picker. Only POST (triggered by
		// the user selecting a model) starts a session. A stale, expired, or
		// instance-mismatched session also collapses back to "none" so the
		// picker reappears instead of a takeover_prompt.
		//
		// NOTE: the CLI sends its instanceId via the x-freebuff-instance-id
		// header (FREEBUFF_INSTANCE_HEADER), NOT a query param. Reading from
		// the query string caused GET to always return "none" after a POST,
		// which made the chat runtime emit "Freebuff session over; holding
		// queued messages until rejoin" on every turn.
		sentInstance := r.Header.Get("x-freebuff-instance-id")
		if sentInstance == "" {
			sentInstance = r.URL.Query().Get("instanceId")
		}
		if freebuffSessionState.instance == "" ||
			sentInstance != freebuffSessionState.instance {
			writeJSON(w, 200, map[string]interface{}{"status": "none"})
			return
		}
		remaining := time.Until(freebuffSessionState.expiresAt).Milliseconds()
		writeJSON(w, 200, map[string]interface{}{
			"status":      "active",
			"accessTier":  "full",
			"instanceId":  freebuffSessionState.instance,
			"model":       freebuffSessionState.model,
			"admittedAt":  freebuffSessionState.admitted.UTC().Format(time.RFC3339),
			"expiresAt":   freebuffSessionState.expiresAt.UTC().Format(time.RFC3339),
			"remainingMs": remaining,
		})
		return

	case http.MethodPost:
		// Model comes via the x-freebuff-model header; fall back to whatever
		// the last session used so a header-less POST still works.
		model := r.Header.Get("x-freebuff-model")
		if model == "" {
			model = freebuffSessionState.model
		}
		if model == "" {
			model = "glm-5.3"
		}
		now := time.Now()
		freebuffSessionState.instance = fmt.Sprintf("local-%d", now.UnixNano())
		freebuffSessionState.model = model
		freebuffSessionState.admitted = now
		freebuffSessionState.expiresAt = now.Add(100 * 365 * 24 * time.Hour)
		writeJSON(w, 200, map[string]interface{}{
			"status":      "active",
			"accessTier":  "full",
			"instanceId":  freebuffSessionState.instance,
			"model":       freebuffSessionState.model,
			"admittedAt":  freebuffSessionState.admitted.UTC().Format(time.RFC3339),
			"expiresAt":   freebuffSessionState.expiresAt.UTC().Format(time.RFC3339),
			"remainingMs": time.Until(freebuffSessionState.expiresAt).Milliseconds(),
		})
		return

	case http.MethodDelete:
		instance := freebuffSessionState.instance
		freebuffSessionState.instance = ""
		if instance == "" {
			writeJSON(w, 200, map[string]interface{}{"status": "ended"})
			return
		}
		// In-grace ended response while the instanceId is still known.
		graceEnd := time.Now().Add(5 * time.Minute)
		writeJSON(w, 200, map[string]interface{}{
			"status":                 "ended",
			"accessTier":             "full",
			"instanceId":             instance,
			"gracePeriodEndsAt":      graceEnd.UTC().Format(time.RFC3339),
			"gracePeriodRemainingMs": 5 * 60 * 1000,
		})
		return

	default:
		writeJSON(w, 405, map[string]interface{}{"error": "method not allowed"})
	}
}

// freebuffSubscriptionHandler answers /api/user/subscription with the
// "no active subscription" shape (NoSubscriptionResponse). The hook only
// throws on !response.ok, so this keeps the TUI out of its error state.
func freebuffSubscriptionHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]interface{}{
		"hasSubscription":    false,
		"fallbackToALaCarte": false,
	})
}

// freebuffAgentsValidateHandler answers /api/agents/validate — the SDK's
// remote validation of user-defined agents. An empty validationErrors list
// means everything is valid, which is right for the proxy: it has no agent
// database to check against and the CLI bundles its own templates.
func freebuffAgentsValidateHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]interface{}{
		"validationErrors": []interface{}{},
	})
}

// freebuffAgentsHandler serves published-agent lookups. The CLI bundles its
// own local agent templates; answering 404 makes fetchAgentFromDatabase
// return null and the runtime fall back to those local templates, which is
// exactly what a proxy without a published-agent store wants.
func freebuffAgentsHandler(w http.ResponseWriter, r *http.Request) {
	http.NotFound(w, r)
}
