// Tests for the idle-aware stream guard (extendingReader) and the
// no-fixed-deadline streaming policy.
//
// Regression context: the old stall guard armed ONE 120s deadline for the
// whole stream, so any generation longer than 120s was killed mid-flight
// ("context deadline exceeded") — large code files streaming for many
// minutes died even though bytes were arriving steadily. The reader must
// measure time WITHOUT DATA, not total stream duration.

package zbridge

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// slowTrickleReader delivers one chunk every interval, forever, until
// stopCh closes — models an SSE body that streams a large file steadily.
type slowTrickleReader struct {
	chunk    []byte
	interval time.Duration
	stopCh   chan struct{}
	sawClose bool
}

func (s *slowTrickleReader) Read(p []byte) (int, error) {
	select {
	case <-s.stopCh:
		return 0, io.EOF
	case <-time.After(s.interval):
		n := copy(p, s.chunk)
		return n, nil
	}
}

func (s *slowTrickleReader) Close() error {
	s.sawClose = true
	return nil
}

func TestExtendingReader_LongStreamIsNeverKilled(t *testing.T) {
	// Stream: 50ms per chunk, idle window 100ms. Total runtime ~2s across
	// 40 chunks — far past the 100ms window; with the old fixed-deadline
	// reader this died at chunk 2.
	body := &slowTrickleReader{chunk: []byte("data: x\n\n"), interval: 50 * time.Millisecond, stopCh: make(chan struct{})}
	defer close(body.stopCh)

	er := newExtendingReader(body, body, 100*time.Millisecond)
	buf := make([]byte, 64)
	reads := 0
	deadline := time.After(10 * time.Second)
	for reads < 40 {
		select {
		case <-deadline:
			t.Fatalf("stream stalled: only %d reads in 10s (guard killed a live stream)", reads)
		default:
		}
		n, err := er.Read(buf)
		if err != nil {
			t.Fatalf("Read %d: unexpected error %v (live stream must not be killed)", reads, err)
		}
		if n == 0 {
			t.Fatalf("Read %d: zero bytes", reads)
		}
		reads++
	}
}

func TestExtendingReader_SilentStreamDiesAfterIdleWindow(t *testing.T) {
	// A body that never sends anything: the guard must cut it ~one idle
	// window after start (the original stall-kill contract).
	silent := &blockedReader{unblock: make(chan struct{})}
	// NOTE: no defer close(silent.unblock) — the watchdog closes the body
	// (idempotently) to cut the stream; a second close would panic.

	start := time.Now()
	er := newExtendingReader(silent, silent, 100*time.Millisecond)
	buf := make([]byte, 64)
	_, err := er.Read(buf)
	if err == nil {
		t.Fatal("silent stream must be cut with an error")
	}
	if err != context.DeadlineExceeded {
		t.Fatalf("silent stream must die with context.DeadlineExceeded, got %v", err)
	}
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond {
		t.Fatalf("killed too early: %v (idle window is 100ms)", elapsed)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("killed too late: %v", elapsed)
	}
}

func TestExtendingReader_ResetsAfterLongPause(t *testing.T) {
	// Chunks separated by pauses SHORTER than the idle window must each
	// renew the deadline: total stream time exceeds the window while every
	// single gap is legal.
	body := &pacedReader{gaps: []time.Duration{70 * time.Millisecond, 70 * time.Millisecond, 70 * time.Millisecond}, chunk: []byte("x")}
	er := newExtendingReader(body, body, 100*time.Millisecond)
	buf := make([]byte, 64)
	for i := 0; i < 3; i++ {
		n, err := er.Read(buf)
		if err != nil || n == 0 {
			t.Fatalf("read %d failed: n=%d err=%v (gap 70ms < window 100ms must be legal)", i, n, err)
		}
	}
}

func TestExtendingReader_PauseLongerThanWindowKills(t *testing.T) {
	// Second gap exceeds the window — the stream dies there, not earlier.
	body := &pacedReader{gaps: []time.Duration{50 * time.Millisecond, 500 * time.Millisecond}, chunk: []byte("x")}
	er := newExtendingReader(body, body, 100*time.Millisecond)
	buf := make([]byte, 64)
	if n, err := er.Read(buf); err != nil || n == 0 {
		t.Fatalf("first read failed: %v", err)
	}
	if _, err := er.Read(buf); err != context.DeadlineExceeded {
		t.Fatalf("second read (gap 500ms > window 100ms) must fail with DeadlineExceeded, got %v", err)
	}
}

func TestExtendingReader_EOFStopsTimer(t *testing.T) {
	// After EOF the guard must not fire on later reads.
	er := newExtendingReader(io.NopCloser(strings.NewReader("hi")), nil, 50*time.Millisecond)
	buf := make([]byte, 64)
	if n, err := er.Read(buf); err != nil || n == 0 {
		t.Fatalf("read: %v", err)
	}
	if _, err := er.Read(buf); err != io.EOF {
		t.Fatalf("want EOF after body drained, got %v", err)
	}
	// Drain any pending token, then confirm a subsequent Read still EOFs
	// (not a spurious DeadlineExceeded from a fired timer).
	time.Sleep(80 * time.Millisecond)
	if _, err := er.Read(buf); err != io.EOF {
		t.Fatalf("post-EOF read must stay EOF, got %v", err)
	}
}

// blockedReader blocks Read until unblock closes (a permanently silent body).
// Close() unblocks Read — mirroring http.resp.Body semantics, where closing
// the body causes a blocked Read to return; the guard relies on that to
// convert an idle timeout into a Read error. Close is idempotent: the
// watchdog and the test/caller may both close it.
type blockedReader struct {
	unblock chan struct{}
	closed  bool
}

func (b *blockedReader) Read(p []byte) (int, error) {
	<-b.unblock
	return 0, io.EOF
}

func (b *blockedReader) Close() error {
	if b.closed {
		return nil
	}
	b.closed = true
	close(b.unblock)
	return nil
}

// pacedReader sends len(gaps) chunks, sleeping gaps[i] before each.
type pacedReader struct {
	gaps  []time.Duration
	chunk []byte
	i     int
}

func (p *pacedReader) Read(b []byte) (int, error) {
	if p.i >= len(p.gaps) {
		return 0, io.EOF
	}
	time.Sleep(p.gaps[p.i])
	p.i++
	return copy(b, p.chunk), nil
}

func (p *pacedReader) Close() error { return nil }
