package zbridge

import (
	"context"
	"sync"
	"testing"
	"time"
)

// orderingBackend records the order of backend calls with timestamps.
type orderingBackend struct {
	mu    sync.Mutex
	calls []string // "create" / "delete:<id>"
}

func (b *orderingBackend) CreateChatSession(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	b.mu.Lock()
	b.calls = append(b.calls, "create")
	b.mu.Unlock()
	// Simulate a slow network delete for deletion only — creation is local.
	return "sess-" + time.Now().Format("150405.000000000"), nil
}

func (b *orderingBackend) DeleteChatSession(ctx context.Context, sessionIDs ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	for _, id := range sessionIDs {
		b.calls = append(b.calls, "delete:"+id)
	}
	b.mu.Unlock()
	// Slow delete: the whole point is that refill must not wait for it.
	time.Sleep(300 * time.Millisecond)
	return nil
}

// TestPoolReleaseRefillsBeforeDelete pins the release ordering: the
// replacement session must be stocked (create is a free local UUID mint)
// BEFORE the paced upstream DELETE runs. The old order (delete → refill)
// starved the pool for the whole delete round-trip on every release, which
// is what drained the batch during bursts.
func TestPoolReleaseRefillsBeforeDelete(t *testing.T) {
	backend := &orderingBackend{}
	pool := NewSessionPool(backend, 2)
	pool.Start()
	defer func() {
		pool.Shutdown()
	}()

	// Warmup: 2 creates.
	deadline := time.Now().Add(2 * time.Second)
	for pool.Ready() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if pool.Ready() < 2 {
		t.Fatalf("warmup did not stock the pool: %d/2 ready", pool.Ready())
	}

	// Acquire one and release it.
	id, err := pool.Acquire(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	pool.Release(id)

	// Give the release goroutine time to finish both steps.
	time.Sleep(100 * time.Millisecond)

	// Refill (create) must appear immediately, without waiting for the
	// 300ms simulated delete. The pool must be full-strength right away.
	if got := pool.Ready(); got < 2 {
		t.Errorf("pool not refilled promptly after Release: %d/2 ready (refill waited for the upstream delete)", got)
	}

	// Wait for the delete to complete, then check ordering: a create for
	// the refill must come BEFORE the delete of the used session.
	time.Sleep(400 * time.Millisecond)
	backend.mu.Lock()
	defer backend.mu.Unlock()

	firstRefillIdx, usedDeleteIdx := -1, -1
	for i, c := range backend.calls {
		if c == "create" && i >= 2 && firstRefillIdx == -1 { // post-warmup create
			firstRefillIdx = i
		}
		if c == "delete:"+id && usedDeleteIdx == -1 {
			usedDeleteIdx = i
		}
	}
	if firstRefillIdx == -1 {
		t.Fatalf("no refill create recorded; calls: %v", backend.calls)
	}
	if usedDeleteIdx == -1 {
		t.Fatalf("used session never deleted; calls: %v", backend.calls)
	}
	if firstRefillIdx > usedDeleteIdx {
		t.Errorf("release ordering broken: refill create at call #%d AFTER used delete at #%d — pool starves during every release (calls: %v)", firstRefillIdx, usedDeleteIdx, backend.calls)
	}
}
