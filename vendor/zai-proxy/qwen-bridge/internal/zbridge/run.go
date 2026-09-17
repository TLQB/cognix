// Entry point of the Qwen bridge (package zbridge).
//
// Run() is what the thin root main.go calls: it parses the CLI flags, opens
// the token database, starts the captcha cache (agent mode), attaches the
// session pool, serves NewHandler() and blocks until CTRL+C / SIGTERM —
// then drains in-flight requests and clears every still-pooled chat session
// on Qwen before exiting (ported from the DeepseekFreeAPI reference).
//
// NewHandler() is exported on its own so integration tests can drive the
// full HTTP surface (all routes + auth + CORS) without starting a listener.

package zbridge

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// ============================================================================
// ENTRY POINT
// ============================================================================

// NewHandler assembles the bridge's complete HTTP surface: every route with
// the auth and CORS middleware applied. Used by Run and by the blackbox
// integration tests in tests/.
func NewHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", dashboardHandler)
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/status", statusHandler)
	mux.HandleFunc("/v1/models", authMiddleware(modelsHandler))
	mux.HandleFunc("/models", authMiddleware(modelsHandler2))
	mux.HandleFunc("/v1/chat/completions", authMiddleware(chatCompletionsHandler))
	mux.HandleFunc("/v1/messages", authMiddleware(anthropicMessagesHandler))

	// OpenAI-path compatibility aliases. Some clients and SDKs build their
	// OpenAI-compatible endpoint as path.Join(baseURL, "/api/v1", endpoint)
	// and therefore hit /api/v1/chat/completions instead of the standard
	// /v1/chat/completions. Registering both prefixes lets the same server
	// serve both path conventions without any rewriting shim in between.
	mux.HandleFunc("/api/v1/chat/completions", authMiddleware(chatCompletionsHandler))
	mux.HandleFunc("/api/v1/models", authMiddleware(modelsHandler))
	mux.HandleFunc("/features", authMiddleware(featuresHandler))
	mux.HandleFunc("/admin/stats", statsHandler)
	mux.HandleFunc("/admin/health", healthHandler)
	mux.HandleFunc("/admin/clients", clientsHandler)
	mux.HandleFunc("/inject.js", injectHandler)
	mux.HandleFunc("/stop", authMiddleware(stopHandler))

	// Access log every request path so wiring experiments against CLI tools
	// can be observed in the log file.
	logged := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logInfo(fmt.Sprintf("ACCESS %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr))
		mux.ServeHTTP(w, r)
	})

	return corsMiddleware(logged)
}

// Run starts the bridge server and blocks until a fatal error or a
// termination signal. Called from the root package's main().
func Run() {
	flag.BoolVar(&config.AgentMode, "agent-mode", config.AgentMode, "Enable agent mode: translate tools & roles for Qwen compatibility (native multi-turn variant by default)")
	flag.StringVar(&config.AgentModeVariant, "agent-mode-variant", config.AgentModeVariant, "Agent mode shim variant: native (default, multi-turn messages upstream with tool exchanges defused to text), modern (XML-sectioned fold), or legacy ([ROLE: ...] rewrite)")
	flag.BoolVar(&config.SyncMode, "sync-mode", config.SyncMode, "Legacy synchronous session flow: create a fresh chat per request instead of drawing from the pre-warmed session pool (used sessions are still deleted on Qwen after each response)")
	flag.IntVar(&config.Server.Port, "port", config.Server.Port, "Listen port (overrides PORT env — used by the bundle launcher to place qwen-proxy on :3003 next to zai-proxy on :3002)")
	flag.Parse()

	logInfo("Starting qwen-proxy verbose=true")

	gRunning.Store(true)

	if config.AgentMode {
		if config.agentNative() {
			logInfo("Agent mode variant: NATIVE (multi-turn messages upstream, tool contract in leading user message)")
		} else if config.agentModern() {
			logInfo("Agent mode variant: MODERN (XML-sectioned prompt shim, tolerant marker/payload parsing)")
		} else {
			logInfo("Agent mode variant: LEGACY ([ROLE: ...] message rewrite shim)")
		}
	}

	if summary := initTokenPool(); summary != "" {
		logInfo("[TokenPool] " + summary + " — sticky selection, daily-cap auto-rotation")
	}

	handler := NewHandler()

	addr := fmt.Sprintf("%s:%d", config.Server.Host, config.Server.Port)

	tokenPadded := fmt.Sprintf("%-44s", config.Auth.Token)
	fmt.Printf(`
╔═══════════════════════════════════════════════════════════════╗
║           Qwen Direct Bridge Server Started                    ║
╠═══════════════════════════════════════════════════════════════╣
║  Mode:          DIRECT HTTP (no browser needed)               ║
║  Captcha IPC:   IN-MEMORY (no FIFO / named pipe)             ║
║  Health:        http://localhost:%d/health               ║
╠═══════════════════════════════════════════════════════════════╣
║  OpenAI API:    http://localhost:%d/v1/chat/completions
║  Anthropic API: http://localhost:%d/v1/messages  ║
╠═══════════════════════════════════════════════════════════════╣
║  Auth Token:    %s║
╚═══════════════════════════════════════════════════════════════╝
`, config.Server.Port, config.Server.Port, config.Server.Port, tokenPadded)

	go func() {
		if err := initializeSession(); err != nil {
			log.Println("[Startup] Session init deferred — will retry on first request.")
		}
		// Warm up model cache
		fetchModelsFromQwen()
	}()

	// ── Session lifecycle (ported from the DeepseekFreeAPI reference) ─────
	// Every stateless request runs on a throwaway chat session that is
	// deleted on Qwen right after its response is fully processed, so no
	// server-side history outlives a request and the account never
	// accumulates dead sessions. By default the async flow keeps a standing
	// batch of pre-made sessions ready (SESSION_POOL_SIZE); --sync-mode
	// restores the legacy per-request flow (still garbage-collected).
	if config.SyncMode {
		log.Println("[Startup] Session mode: SYNC (--sync-mode: fresh chat per request, deleted on Qwen after use)")
	} else {
		poolWait = time.Duration(config.SessionAcquireTimeout) * time.Second
		if config.SessionAcquireTimeout <= 0 {
			poolWait = 0 // 0 => wait indefinitely for a pooled session
		}
		sessionPool = NewSessionPool(NewQwenChatBackend(), config.SessionPoolSize)
		log.Printf("[Startup] Session mode: ASYNC (pre-made chat batch x%d, throwaway: deleted on Qwen + refilled after each response)", sessionPool.Size())
		log.Printf("[Startup]               SESSION_POOL_SIZE=%d SESSION_ACQUIRE_TIMEOUT=%ds", sessionPool.Size(), config.SessionAcquireTimeout)
	}
	// WAF egress identity (see QWEN_RESEARCH_WAF.md: the qwen WAF keys on
	// the REAL connection IP and ignores every client-IP header, so XFF is
	// an operator opt-in experiment, not a bypass).
	if os.Getenv("QWEN_XFF") == "on" {
		if os.Getenv("QWEN_XFF_IP") != "" {
			log.Printf("[Startup] XFF: PINNED (%s via QWEN_XFF_IP) — opt-in, ignored by the qwen WAF", os.Getenv("QWEN_XFF_IP"))
		} else if os.Getenv("QWEN_XFF_IPS") != "" {
			log.Println("[Startup] XFF: POOL (QWEN_XFF_IPS) — opt-in, ignored by the qwen WAF")
		} else if xffStrategy() == "pool" {
			log.Println("[Startup] XFF: POOL (QWEN_XFF_STRATEGY=pool) — opt-in, ignored by the qwen WAF")
		} else {
			log.Printf("[Startup] XFF: RANDOM-BANK (%d CIDR banks) — opt-in, ignored by the qwen WAF", len(qwenXffCIDRBanks))
		}
	} else {
		log.Println("[Startup] XFF: OFF (default — the qwen WAF keys on the real egress IP; use HTTPS_PROXY to change identity)")
	}

	srv := &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	// Start serving before blocking on signals.
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.ListenAndServe()
	}()

	// Warm the standing session batch in the background; requests are served
	// meanwhile (they simply queue on Acquire until sessions appear).
	if sessionPool != nil {
		sessionPool.Start()
	}

	// ── Graceful shutdown (ported from the DeepseekFreeAPI reference) ─────
	// CTRL+C / SIGTERM stops accepting new connections, lets in-flight
	// responses finish (10s drain deadline), then deletes every still-pooled
	// chat session on Qwen so nothing is left behind on the account. A
	// second CTRL+C force-exits immediately (default handling is re-armed).
	ctx, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignal()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	case <-ctx.Done():
		stopSignal()
		log.Println("[Shutdown] Graceful shutdown requested — draining connections and clearing all chat sessions...")

		drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := srv.Shutdown(drainCtx); err != nil {
			log.Printf("[Shutdown] drain deadline hit (%v); closing remaining connections", err)
			_ = srv.Close()
		}
		cancel()

		// Clear any sessions still pooled so nothing is left behind on the
		// Qwen account (checked-out ones are deleted by their own Release).
		if sessionPool != nil {
			sessionPool.Shutdown()
		}
		log.Println("[Shutdown] All chat sessions cleared. Goodbye.")
	}
}
