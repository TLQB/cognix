// Entry point of the Z.AI bridge (package zbridge).
//
// Run() is what the thin root main.go calls: it parses the CLI flags, opens
// the token database, starts the captcha cache (agent mode), attaches the
// session pool, serves NewHandler() and blocks until CTRL+C / SIGTERM —
// then drains in-flight requests and clears every still-pooled chat session
// on Z.AI before exiting (ported from the DeepseekFreeAPI reference).
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

	// Freebuff compatibility aliases. The Codebuff/Freebuff SDK builds its
	// OpenAI-compatible endpoint as path.Join(baseURL, "/api/v1", endpoint),
	// so a proxy pointed at via CODEBUFF_APP_URL is hit at /api/v1/chat/completions.
	// Registering both prefixes lets the same server serve standard OpenAI
	// clients and Freebuff without any rewriting shim in between.
	mux.HandleFunc("/api/v1/chat/completions", authMiddleware(chatCompletionsHandler))
	mux.HandleFunc("/api/v1/models", authMiddleware(modelsHandler))
	mux.HandleFunc("/features", authMiddleware(featuresHandler))
	mux.HandleFunc("/admin/stats", statsHandler)
	mux.HandleFunc("/admin/health", healthHandler)
	mux.HandleFunc("/admin/clients", clientsHandler)
	mux.HandleFunc("/inject.js", injectHandler)
	mux.HandleFunc("/stop", authMiddleware(stopHandler))

	// Freebuff backend stubs. The CLI runtime phones the website base URL
	// (getWebsiteUrl()) for connection health, user lookup, run bookkeeping
	// and usage before/around every run; see freebuff_mock.go for why each
	// stub needs to exist. Authed like the rest so the proxy stays consistent.
	// /api/healthz must stay unauthenticated: the SDK's checkConnection() does
	// a bare fetch with no Authorization header, and a 401 here leaves the CLI
	// stuck in "connecting..." forever (codebuff.com serves healthz publicly).
	mux.HandleFunc("/api/healthz", freebuffHealthHandler)
	mux.HandleFunc("/api/v1/me", authMiddleware(freebuffMeHandler))
	mux.HandleFunc("/api/v1/agent-runs", authMiddleware(freebuffAgentRunsHandler))
	mux.HandleFunc("/api/v1/usage", authMiddleware(freebuffUsageHandler))
	mux.HandleFunc("/api/v1/agents/", authMiddleware(freebuffAgentsHandler))
	// /api/v1/freebuff/session must stay unauthenticated: the CLI polls GET
	// before submit and, if it sees 401, logs "session over; holding queued
	// messages" and never POSTs agent-runs. The CLI sends a bare fetch with
	// no Authorization header on the session poll (codebuff.com serves this
	// publicly once the user is signed in via cookie, but a local proxy has a
	// single user so auto-admitting is safe).
	mux.HandleFunc("/api/v1/freebuff/session", freebuffSessionHandler)
	// Runtime config for the TUI (/thinking command flips the proxy's
	// tool-loop thinking policy live). See freebuff_config.go.
	mux.HandleFunc("/api/v1/freebuff/config", authMiddleware(freebuffConfigHandler))
	mux.HandleFunc("/api/user/subscription", authMiddleware(freebuffSubscriptionHandler))
	// /api/agents/validate must also stay unauthenticated: the SDK's
	// validateAgents() does a bare fetch with no Authorization header (see
	// sdk/src/validate-agents.ts), so an auth wall here returns 401, which the
	// CLI turns into a silent validation failure and blocks every submit.
	mux.HandleFunc("/api/agents/validate", freebuffAgentsValidateHandler)
	// QFarm status: one-stop operator dashboard (Q age/dead, reserve depth,
	// breaker, last rebless). Authed — it names files and internal state.
	mux.HandleFunc("/api/qfarm", authMiddleware(qfarmStatusHandler))

	// Access log every request path so wiring experiments against CLI tools
	// (freebuff et al.) can be observed in the log file.
	logged := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logInfo(fmt.Sprintf("ACCESS %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr))
		mux.ServeHTTP(w, r)
	})

	return corsMiddleware(logged)
}

// Run starts the bridge server and blocks until a fatal error or a
// termination signal. Called from the root package's main().
func Run() {
	flag.StringVar(&dbPath, "db-path", "tokens.sqlite", "Path to SQLite database")
	flag.BoolVar(&verbose, "verbose", false, "Enable verbose logging")
	flag.BoolVar(&config.AgentMode, "agent-mode", config.AgentMode, "Enable agent mode: translate tools & roles for Z.AI compatibility (native multi-turn variant by default)")
	flag.StringVar(&config.AgentModeVariant, "agent-mode-variant", config.AgentModeVariant, "Agent mode shim variant: native (default, multi-turn messages upstream with tool exchanges defused to text), modern (XML-sectioned fold), or legacy ([ROLE: ...] rewrite)")
	flag.BoolVar(&config.SyncMode, "sync-mode", config.SyncMode, "Legacy synchronous session flow: create a fresh chat per request instead of drawing from the pre-warmed session pool (used sessions are still deleted on Z.AI after each response)")
	flag.Parse()

	// The token source is qbless.json (Q-Bless; primary) with tokens.sqlite
	// as the emergency reserve. Neither file is individually required at
	// startup: initDB below creates/opens the reserve lazily, and qbless is
	// read per-need. Hard-exiting here would brick a correctly-blessed
	// deployment that simply has no reserve stock yet.

	logInfo("Starting with db-path='" + dbPath + "' verbose=true")

	if err := initDB(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer globalDB.Close()

	gRunning.Store(true)

	// Start the background canary probe + auto-rebless goroutine. No-op
	// unless QBLESS_AUTO_REBLESS=1; the goroutine periodically verifies the
	// blessed Q is still alive and spawns `q-bless` to re-bless when it
	// starts degrading — so live requests don't fail mid-flight.
	StartAutoRebless()

	if config.AgentMode {
		go captchaCache.Run()
		logInfo("Agent mode: Captcha background cache started")
		if config.agentNative() {
			logInfo("Agent mode variant: NATIVE (multi-turn messages upstream, tool contract in leading user message)")
		} else if config.agentModern() {
			logInfo("Agent mode variant: MODERN (XML-sectioned prompt shim, tolerant marker/payload parsing)")
		} else {
			logInfo("Agent mode variant: LEGACY ([ROLE: ...] message rewrite shim)")
		}
	}

	handler := NewHandler()

	addr := fmt.Sprintf("%s:%d", config.Server.Host, config.Server.Port)

	tokenPadded := fmt.Sprintf("%-44s", config.Auth.Token)
	fmt.Printf(`
╔═══════════════════════════════════════════════════════════════╗
║           Z.AI Direct Bridge Server Started                   ║
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
		fetchModelsFromZAI()
	}()

	// ── Session lifecycle (ported from the DeepseekFreeAPI reference) ─────
	// Every stateless request runs on a throwaway chat session that is
	// deleted on Z.AI right after its response is fully processed, so no
	// server-side history outlives a request and the account never
	// accumulates dead sessions. By default the async flow keeps a standing
	// batch of pre-made sessions ready (SESSION_POOL_SIZE); --sync-mode
	// restores the legacy per-request flow (still garbage-collected).
	if config.SyncMode {
		log.Println("[Startup] Session mode: SYNC (--sync-mode: fresh chat per request, deleted on Z.AI after use)")
	} else {
		poolWait = time.Duration(config.SessionAcquireTimeout) * time.Second
		if config.SessionAcquireTimeout <= 0 {
			poolWait = 0 // 0 => wait indefinitely for a pooled session
		}
		sessionPool = NewSessionPool(NewZAIChatBackend(), config.SessionPoolSize)
		log.Printf("[Startup] Session mode: ASYNC (pre-made chat batch x%d, throwaway: deleted on Z.AI + refilled after each response)", sessionPool.Size())
		log.Printf("[Startup]               SESSION_POOL_SIZE=%d SESSION_ACQUIRE_TIMEOUT=%ds", sessionPool.Size(), config.SessionAcquireTimeout)
	}
	// WAF egress identity strategy (see defaultXffCIDRBanks in util.go).
	if os.Getenv("ZAI_XFF_IP") != "" {
		log.Printf("[Startup] XFF strategy: PINNED (%s via ZAI_XFF_IP)", os.Getenv("ZAI_XFF_IP"))
	} else if os.Getenv("ZAI_XFF_IPS") != "" {
		log.Println("[Startup] XFF strategy: POOL (operator-pinned ZAI_XFF_IPS, rotation on 405)")
	} else if xffStrategy() == "pool" {
		log.Println("[Startup] XFF strategy: POOL (ZAI_XFF_STRATEGY=pool, rotation on 405)")
	} else {
		log.Printf("[Startup] XFF strategy: RANDOM-BANK (fresh foreign identity per request from %d CIDR banks; per-identity WAF budgets never reached)", len(defaultXffCIDRBanks))
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
	// chat session on Z.AI so nothing is left behind on the account. A
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
		// Z.AI account (checked-out ones are deleted by their own Release).
		if sessionPool != nil {
			sessionPool.Shutdown()
		}
		log.Println("[Shutdown] All chat sessions cleared. Goodbye.")
	}
}
