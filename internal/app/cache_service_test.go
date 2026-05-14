package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestCacheFetchTextRespectsContextDeadline pins the architect's exact
// concern from ARCHITECTURE_REVIEW.md item #7: a slow peer must not
// hold the cache loop hostage to the http.Client's coarse Timeout —
// the per-request ctx must be load-bearing instead.
//
// Setup: an httptest.Server whose handler sleeps 30s before writing.
// We invoke CacheService.fetchText with a ctx that expires in 100ms.
// The call must return within ~200ms with a ctx-cancelled error.
//
// Before the fix the loop's Stop landed only after the full 10s
// client.Timeout fired; after the fix ctx cancellation lands within
// the dial/read budget — typically a few ms beyond the deadline.
func TestCacheFetchTextRespectsContextDeadline(t *testing.T) {
	// Server handler that blocks until the connection is torn down or
	// 30s pass — long enough that the assertion below failing means
	// we'd hang the entire test process if ctx cancellation isn't
	// wired through.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	defer server.Close()

	svc := newTestCacheService(t)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := svc.fetchText(ctx, server.URL+"/playlist.m3u8")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("fetchText: expected ctx-cancelled error, got nil after %s", elapsed)
	}
	// net/http wraps the underlying context error in a *url.Error, so
	// errors.Is sees through to context.DeadlineExceeded — that's the
	// exact signal we want to confirm.
	if !errors.Is(err, context.DeadlineExceeded) {
		// Tolerate the rare race where the dial completes and the
		// server's handler ctx-cancels before the client read deadline
		// — either way the err must originate from ctx cancellation.
		if !strings.Contains(err.Error(), "context") {
			t.Errorf("fetchText err=%v; want context.DeadlineExceeded", err)
		}
	}
	// 200ms upper bound — generous enough that CI noise won't flake it
	// but tight enough that a regression re-introducing the 10s client
	// timeout would blow through it by 50x.
	if elapsed > 200*time.Millisecond {
		t.Errorf("fetchText took %s; want <200ms (ctx deadline was 100ms)", elapsed)
	}
}

// TestCacheDownloadFileRespectsContextDeadline mirrors the fetchText
// test for the segment-download path. Both call sites pass through
// http.NewRequestWithContext; if one regresses without the other a
// peer that hangs mid-segment-transfer could still wedge the loop.
func TestCacheDownloadFileRespectsContextDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Start writing headers so dial+TLS succeed, then sleep on
		// the body — this exercises the "in-flight HTTP fetch" branch
		// of the architect's concern rather than just the connect path.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	defer server.Close()

	svc := newTestCacheService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	dst := t.TempDir() + "/segment.m4s"
	start := time.Now()
	err := svc.downloadFile(ctx, server.URL+"/segment.m4s", dst)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("downloadFile: expected ctx-cancelled error, got nil after %s", elapsed)
	}
	if elapsed > 250*time.Millisecond {
		t.Errorf("downloadFile took %s; want <250ms (ctx deadline was 100ms)", elapsed)
	}
}

// newTestCacheService returns a CacheService with a discard logger and
// throw-away temp dirs. The constructor wires no in-flight loops, so
// fetchText/downloadFile can be called directly without StartCaching.
func newTestCacheService(t *testing.T) *CacheService {
	t.Helper()
	settings := AppSettings{
		Port:      0,
		NodeType:  "decoder",
		BaseDir:   t.TempDir(),
		CacheDir:  t.TempDir(),
		EventsDir: t.TempDir(),
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewCacheService(settings, logger)
}
