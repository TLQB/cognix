// session_pool.go
//
// Throwaway chat sessions + async session pool, ported from the
// DeepseekFreeAPI reference implementation (see reference/internal/dsproxy:
// pool.go for the pool, proxy.go for the garbage collector) and adapted to
// the Qwen / chat.qwen.ai platform.
//
// ── Why this exists (context rot) ─────────────────────────────────────────
//
// OpenAI-compatible clients are stateless: they re-send the ENTIRE
// conversation (user + assistant turns) on every request. The bridge
// forwards that history to Qwen inside a chat identified by `chat_id`,
// minted on the bridge account (QWEN_TOKEN) together with its full
// history. Two failure modes follow:
//
//   1. Accumulation — nothing ever deletes those chats, so the account fills
//      up with dead sessions, one per proxied request.
//   2. Context rot — if a server-side chat outlives a single request, Qwen's
//      own stored history stacks on top of the history the client already
//      re-sent, the model sees duplicated/stale context, and the
//      conversation quality rots.
//
// The fix mirrors the reference project:
//
//   - Every stateless request runs on a THROWAWAY chat session. As soon as
//     the response has been fully written (or definitively failed) the chat
//     is deleted on Qwen (DELETE /api/v1/chats/{chat_id}) so no session
//     history survives the request and nothing accumulates on the account.
//   - Async mode (default): a standing batch of ready sessions
//     (SESSION_POOL_SIZE, default 5) is kept pre-made at all times; requests
//     grab one instantly, and each consumed session is deleted upstream +
//     replaced the moment its response is fully processed, so the batch
//     refills itself for as long as the app runs.
//   - Sync mode (--sync-mode / SYNC_MODE=true): the legacy flow where every
//     request creates its own session first; the used session is still
//     garbage-collected afterwards.
//   - Graceful shutdown (CTRL+C / SIGTERM): in-flight requests are drained,
//     then every still-pooled session is deleted on Qwen before exiting
//     (a second CTRL+C force-exits).
//
// ── Platform difference vs the reference ──────────────────────────────────
//
// DeepSeek creates sessions with an upstream API call (POST
// /chat_session/create) and deletes them in bulk (POST /chat_session/delete),
// so the reference pool pre-warms against the network. Qwen chat IDs are
// server-minted via POST /api/v2/chats/new (a client-generated UUID is
// rejected — CHAT_NOT_FOUND, probe 2026-09-12), and deletion is one DELETE
// per chat:
//
//     DELETE https://chat.qwen.ai/api/v1/chats/<chat_id>
//     authorization: Bearer <token>
//       -> true                                            (deleted)
//       -> {"detail":"We could not find what you're looking for :/"}
//                                                          (already gone)
//
// Therefore pool warmup here is instant and local (just mint UUIDs), and a
// session that is never consumed never touches the account at all. The
// retirement discipline — delete after use, then refill — is unchanged, and
// the "already gone" reply counts as a successful delete (idempotent GC).

package zbridge

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrPoolClosing is returned by Acquire once Shutdown has begun.
	ErrPoolClosing = errors.New("session pool is shutting down")
	// ErrPoolTimeout is returned by Acquire when no pooled session became
	// available within the configured wait window.
	ErrPoolTimeout = errors.New("timed out waiting for a pooled session")
)

const (
	// defaultPoolSize is the standing batch of pre-made ready sessions.
	defaultPoolSize = 5
	// defaultPoolWait bounds how long a completion request waits for a
	// pooled session before creating one directly (SESSION_ACQUIRE_TIMEOUT).
	defaultPoolWait = 10 * time.Second
	// poolOpTimeout bounds one upstream delete call.
	poolOpTimeout = 30 * time.Second
	// poolCreateBackoffStart / poolCreateBackoffMax shape the retry delay
	// when session creation fails (kept for parity with the reference; on
	// Qwen creation is local and does not fail unless the backend is
	// swapped for one that calls upstream).
	poolCreateBackoffStart = 1 * time.Second
	poolCreateBackoffMax   = 15 * time.Second
	// poolDrainWait bounds how long Shutdown waits for in-flight
	// retire/refill operations before reporting leftovers.
	poolDrainWait = 20 * time.Second
)

// SessionBackend is the slice of the Qwen bridge the pool needs. Tests
// substitute a stub; the production backend is qwenSessionBackend.
type SessionBackend interface {
	CreateChatSession(ctx context.Context) (string, error)
	DeleteChatSession(ctx context.Context, sessionIDs ...string) error
}

// qwenSessionBackend implements SessionBackend against chat.qwen.ai.
type qwenSessionBackend struct{}

// NewQwenChatBackend returns the production SessionBackend: Qwen chats are
// server-minted via POST /api/v2/chats/new (CreateQwenChat) and deleted via
// DELETE /api/v1/chats/{id} with the cookie+Version headers (DeleteQwenChat).
// Run attaches a pool built from it at startup; tests use it to exercise the
// real create/delete paths against a mock upstream.
func NewQwenChatBackend() SessionBackend { return qwenSessionBackend{} }

// CreateChatSession mints one fresh Qwen chat server-side. Qwen chat IDs
// are minted by the backend via POST /api/v2/chats/new — a client-generated
// UUID is rejected (CHAT_NOT_FOUND), so warmup must create real chats.
func (qwenSessionBackend) CreateChatSession(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return CreateQwenChat(qwenDefaultModel)
}

// DeleteChatSession deletes chats one by one (Qwen has no bulk endpoint).
// Best-effort: every ID is attempted; the first error is returned after the
// rest have been tried.
func (qwenSessionBackend) DeleteChatSession(ctx context.Context, sessionIDs ...string) error {
	var firstErr error
	for _, id := range sessionIDs {
		if id == "" {
			continue
		}
		if err := DeleteQwenChat(ctx, id); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// SessionPool holds the standing batch of ready stateless chat sessions.
//
// With the pool attached, every completion draws a pre-made session instead
// of minting one per request, and the moment a request's response has been
// fully written and processed the consumed session is deleted upstream and a
// replacement is created to refill the batch — so the account never
// accumulates garbage and the batch stays at full strength while the app
// runs.
type SessionPool struct {
	backend SessionBackend
	size    int

	ready chan string // buffered with size; members are unused, clean sessions

	stopOnce sync.Once
	stopCh   chan struct{}
	stopped  atomic.Bool

	wg sync.WaitGroup // outstanding create/delete operations
}

// NewSessionPool builds a pool that keeps size sessions ready. size < 1 is
// clamped to the default batch. Call Start to begin warmup.
func NewSessionPool(backend SessionBackend, size int) *SessionPool {
	if size < 1 {
		size = defaultPoolSize
	}
	return &SessionPool{
		backend: backend,
		size:    size,
		ready:   make(chan string, size),
		stopCh:  make(chan struct{}),
	}
}

// Size reports the configured batch size.
func (p *SessionPool) Size() int { return p.size }

// Ready reports how many sessions are currently stocked.
func (p *SessionPool) Ready() int { return len(p.ready) }

// Start launches the warmup goroutines that pre-make the initial batch.
func (p *SessionPool) Start() {
	log.Printf("[Pool] warming up %d stateless session(s)...", p.size)
	for i := 0; i < p.size; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.fillSlot("warmup")
		}()
	}
}

// Acquire hands out one ready session, blocking until one is available, ctx
// is done, or wait elapses (wait <= 0 waits indefinitely). The caller MUST
// eventually call Release with the returned ID — even on error paths — so
// the used session is retired and the batch refilled.
func (p *SessionPool) Acquire(ctx context.Context, wait time.Duration) (string, error) {
	var timeout <-chan time.Time
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		timeout = timer.C
	}
	for {
		select {
		case id := <-p.ready:
			if config.Logging.Level == "debug" {
				log.Printf("[Pool] handed out session %s (%d/%d ready)", id, len(p.ready), p.size)
			}
			return id, nil
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timeout:
			return "", ErrPoolTimeout
		case <-p.stopCh:
			return "", ErrPoolClosing
		}
	}
}

// Release retires a consumed session. It is called only after the response
// has been fully written and processed (or definitively failed), so the
// session is never yanked out from under an in-flight completion: a
// replacement is created right away to fill the gap in the batch, and the
// used session is deleted upstream afterwards. Both steps run in the
// background.
//
// Refill BEFORE delete: minting a session is one cheap POST (chats/new),
// while the DELETE is a paced upstream
// round-trip (50-150ms pacing + RTT, ~1s observed live). Deleting first left
// the pool starved for the whole delete window on every release, so a burst
// drained the batch and later requests waited on refills or fell back to
// on-demand sessions. Stocking the replacement first keeps the batch at
// full strength instantly; the delete still happens right after,
// best-effort as before. A crash between the two leaves one chat upstream —
// the same failure window the old order already had on a failed delete.
func (p *SessionPool) Release(sessionID string) {
	if sessionID == "" {
		return
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		if !p.stopped.Load() {
			p.fillSlot("refill")
		}
		p.deleteOne(sessionID, "used")
	}()
}

// Shutdown gracefully clears the pool (the CTRL+C path): refills are
// stopped, every still-pooled session is deleted upstream so nothing is left
// behind on the account, and we briefly wait for in-flight retire/refill
// operations to finish. Sessions already checked out are deleted by their
// request's Release call.
func (p *SessionPool) Shutdown() {
	first := false
	p.stopOnce.Do(func() {
		first = true
		p.stopped.Store(true)
		close(p.stopCh)
	})
	if !first {
		return
	}

	// Collect whatever is still stocked and delete it. Qwen deletion is one
	// call per chat, so walk the leftovers sequentially.
	var leftover []string
	for {
		select {
		case id := <-p.ready:
			leftover = append(leftover, id)
			continue
		default:
		}
		break
	}
	if len(leftover) > 0 {
		log.Printf("[Pool] clearing all remaining sessions (%d)...", len(leftover))
		ctx, cancel := context.WithTimeout(context.Background(), poolOpTimeout)
		err := p.backend.DeleteChatSession(ctx, leftover...)
		cancel()
		if err != nil {
			log.Printf("[Pool] warning: failed to clear %d session(s): %v", len(leftover), err)
		} else {
			log.Printf("[Pool] cleared %d pooled session(s): deleted %v", len(leftover), leftover)
		}
	} else {
		log.Printf("[Pool] clearing all sessions... none remaining")
	}

	// Wait (bounded) for outstanding creates/deletes to wind down.
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		log.Printf("[Pool] all sessions accounted for")
	case <-time.After(poolDrainWait):
		log.Printf("[Pool] warning: some background session operations did not finish within %s", poolDrainWait)
	}

	// Second drain: a `stock` goroutine may have slipped a session into
	// `ready` between our first drain and `wg.Wait()` (despite the
	// stopped.Load() fast-path in stock, the select race still allows a
	// narrow window). Sweep the channel once more so nothing leaks on the
	// Qwen account.
	var leftover2 []string
	for {
		select {
		case id := <-p.ready:
			leftover2 = append(leftover2, id)
			continue
		default:
		}
		break
	}
	if len(leftover2) > 0 {
		log.Printf("[Pool] sweeping %d late-stocked session(s) after wg.Wait()...", len(leftover2))
		ctx2, cancel2 := context.WithTimeout(context.Background(), poolOpTimeout)
		err := p.backend.DeleteChatSession(ctx2, leftover2...)
		cancel2()
		if err != nil {
			log.Printf("[Pool] warning: failed to clear %d late session(s): %v", len(leftover2), err)
		} else {
			log.Printf("[Pool] cleared %d late-stocked session(s): deleted %v", len(leftover2), leftover2)
		}
	}
}

// fillSlot creates one session (retrying through transient failures) and
// stocks it, unless shutdown won the race. Runs synchronously; callers wrap
// it in a goroutine where concurrency is wanted.
func (p *SessionPool) fillSlot(reason string) {
	backoff := poolCreateBackoffStart
	loggedOnce := false
	for {
		if p.stopped.Load() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), poolOpTimeout)
		id, err := p.backend.CreateChatSession(ctx)
		cancel()
		if err != nil {
			if p.stopped.Load() {
				return
			}
			// Log the first failure loudly; repeats stay quiet so a
			// misconfigured backend doesn't spam the log.
			if !loggedOnce {
				log.Printf("[Pool:%s] session creation failed (%v); retrying...", reason, err)
				loggedOnce = true
			} else if config.Logging.Level == "debug" {
				log.Printf("[Pool:%s] session creation failed again (%v); retrying in %s", reason, err, backoff)
			}
			select {
			case <-time.After(backoff):
			case <-p.stopCh:
				return
			}
			backoff *= 2
			if backoff > poolCreateBackoffMax {
				backoff = poolCreateBackoffMax
			}
			continue
		}
		p.stock(id, reason)
		return
	}
}

// stock puts a freshly created session into the batch, or deletes it if
// shutdown raced in first (never stockpile sessions nobody will consume).
func (p *SessionPool) stock(id, reason string) {
	// Fast-path check: if shutdown already started, retire immediately to
	// avoid racing between `ready <- id` and `<-p.stopCh` (both ready after
	// close(stopCh), Go picks one at random -> session could leak in a
	// drained channel forever).
	if p.stopped.Load() {
		p.deleteOne(id, "shutdown-race")
		return
	}
	select {
	case p.ready <- id:
		log.Printf("[Pool:%s] session ready: %s (%d/%d)", reason, id, len(p.ready), p.size)
	case <-p.stopCh:
		p.deleteOne(id, "shutdown-race")
	}
}

// deleteOne deletes a single session upstream, best-effort.
func (p *SessionPool) deleteOne(id, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), poolOpTimeout)
	defer cancel()
	if err := p.backend.DeleteChatSession(ctx, id); err != nil {
		log.Printf("[Pool:%s] warning: failed to delete session %s: %v", reason, id, err)
		return
	}
	if config.Logging.Level == "debug" {
		log.Printf("[Pool:%s] deleted chat session: %s", reason, id)
	}
}

// ── Bridge glue ─────────────────────────────────────────────────────────────

var (
	// sessionPool holds the standing batch of ready throwaway chat sessions.
	// nil when running in sync mode (--sync-mode / SYNC_MODE=true); the
	// legacy per-request flow still garbage-collects used sessions.
	sessionPool *SessionPool
	// poolWait bounds how long a request waits for a pooled session before
	// creating one directly (0 waits forever). See SESSION_ACQUIRE_TIMEOUT.
	poolWait = defaultPoolWait
)

// AttachSessionPool swaps the async session pool (and its acquire wait
// window) used by stateless requests and returns a function that restores
// the previous attachment. Passing nil detaches the pool, i.e. switches to
// the sync (legacy per-request) flow. Run uses it once at startup; the
// blackbox tests in tests/ use it to exercise both flows.
func AttachSessionPool(p *SessionPool, wait time.Duration) func() {
	oldPool, oldWait := sessionPool, poolWait
	sessionPool, poolWait = p, wait
	return func() {
		sessionPool, poolWait = oldPool, oldWait
	}
}

// AcquireStatelessSession returns a throwaway chat ID for one stateless
// request. Async mode takes a pre-made session from the standing batch so no
// per-request creation cost is paid; if a burst exhausts the batch the
// request waits up to poolWait and then creates a session directly instead
// of stalling indefinitely. Sync mode mints a fresh session per request.
//
// The second return value reports whether the session is pool-owned (retired
// through pool.Release) or on-demand (retired through gcSessions).
func AcquireStatelessSession(ctx context.Context) (chatID string, pooled bool, err error) {
	if sessionPool == nil {
		// Sync mode: mint a real server-side chat. A randomUUID() here used
		// to "work" under the web protocol (the completions endpoint
		// materialised unknown chat ids on the fly) but the APK protocol
		// rejects ids it never minted with CHAT_NOT_FOUND (probe
		// 2026-09-12) — /api/v2/chats/new is the only way to obtain a
		// valid chat id, exactly like the Android app.
		id, mintErr := CreateQwenChat(qwenDefaultModel)
		if mintErr != nil {
			return "", false, fmt.Errorf("sync-mode chat mint failed: %s", mintErr.Error())
		}
		return id, false, nil
	}
	id, acqErr := sessionPool.Acquire(ctx, poolWait)
	switch {
	case acqErr == nil:
		log.Printf("[Pool] stateless session: %s (%d/%d ready)", id, sessionPool.Ready(), sessionPool.Size())
		return id, true, nil
	case errors.Is(acqErr, ErrPoolTimeout):
		// Pool busy: mint on demand instead of stalling. A client-generated
		// UUID would be rejected upstream (CHAT_NOT_FOUND).
		id, mintErr := CreateQwenChat(qwenDefaultModel)
		if mintErr != nil {
			return "", false, fmt.Errorf("on-demand chat mint failed: %s", mintErr.Error())
		}
		log.Printf("[Pool] busy — stateless session minted on demand: %s", id)
		return id, false, nil
	default: // ErrPoolClosing, or the request's client went away
		if errors.Is(acqErr, ErrPoolClosing) {
			return "", false, errors.New("server is shutting down")
		}
		return "", false, acqErr
	}
}

// ReleaseStatelessSession retires a used stateless chat session. It must be
// called only after the response has been fully written and processed (or
// definitively failed): the chat is deleted on Qwen so its history never
// outlives the request, and in async mode the pool immediately stocks a
// replacement to keep the batch at full strength.
func ReleaseStatelessSession(chatID string, pooled bool) {
	if chatID == "" {
		return
	}
	if pooled && sessionPool != nil {
		sessionPool.Release(chatID)
		return
	}
	gcSessions("stateless", chatID)
}

// gcSessions asynchronously deletes used-up chat sessions on Qwen so their
// history doesn't accumulate on the account (port of the reference's GC):
// sessions are temporary by design and are removed right after their last
// use, in a background goroutine so response latency is unaffected. Deletion
// is best-effort — failures are logged and otherwise ignored.
func gcSessions(reason string, sessionIDs ...string) {
	ids := make([]string, 0, len(sessionIDs))
	for _, id := range sessionIDs {
		if id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	go func() {
		// Own context: the triggering request may already be gone by now.
		ctx, cancel := context.WithTimeout(context.Background(), poolOpTimeout)
		defer cancel()
		backend := qwenSessionBackend{}
		if err := backend.DeleteChatSession(ctx, ids...); err != nil {
			log.Printf("[GC:%s] failed to delete chat session(s) %v: %v", reason, ids, err)
			return
		}
		log.Printf("[GC:%s] deleted chat session(s): %v", reason, ids)
	}()
}

// sessionPoolStatus reports the session-lifecycle mode for /status.
func sessionPoolStatus() map[string]interface{} {
	if sessionPool == nil {
		return map[string]interface{}{
			"mode":       "sync",
			"throwaway":  true,
			"gc_enabled": true,
		}
	}
	return map[string]interface{}{
		"mode":       "async",
		"throwaway":  true,
		"gc_enabled": true,
		"size":       sessionPool.Size(),
		"ready":      sessionPool.Ready(),
	}
}
