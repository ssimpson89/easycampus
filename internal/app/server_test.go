package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// testServer constructs a Server bound to a random localhost port using
// temp directories for EventsDir/CacheDir/BaseDir so two tests don't
// stomp on each other. The returned cleanup must be deferred.
func testServer(t *testing.T) (*Server, string) {
	t.Helper()

	port := pickFreePort(t)
	settings := AppSettings{
		Port:      port,
		NodeType:  "encoder",
		BaseDir:   t.TempDir(),
		EventsDir: t.TempDir(),
		CacheDir:  t.TempDir(),
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	srv, err := NewServer(ServerConfig{Settings: settings, Logger: logger})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv, "http://127.0.0.1" + srv.Addr()
}

// pickFreePort asks the kernel for a free TCP port. The listener is
// closed before returning so the chosen port can be re-bound by the
// Server under test. There is a small race window where another process
// could grab the port between Close and Listen — acceptable for tests.
func pickFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestServerStartsAndStops verifies the full Run() lifecycle: the HTTP
// server binds, /health returns 200, then SIGTERM lands and Run returns
// nil. This is the canary that catches any regression in the shutdown
// sequence (cache.StopAll / ffmpeg.StopStream / mpv.Stop order).
func TestServerStartsAndStops(t *testing.T) {
	srv, baseURL := testServer(t)
	client := noKeepAliveClient()

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	var runErr error
	go func() {
		defer wg.Done()
		runErr = srv.Run(ctx)
	}()

	// Wait for the listener to come up. We poll /health rather than
	// sleeping a fixed interval so a slow CI machine doesn't flake.
	if !waitForListen(t, baseURL+"/health", 3*time.Second) {
		cancel()
		wg.Wait()
		t.Fatalf("server never became reachable")
	}

	// Hit /health and assert the JSON shape.
	resp, err := client.Get(baseURL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /health: status=%d, want 200", resp.StatusCode)
	}
	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Errorf("GET /health: decode: %v", err)
	}
	resp.Body.Close()
	if ok, _ := body["ok"].(bool); !ok {
		t.Errorf("GET /health: ok=%v, want true", body["ok"])
	}

	// Trigger graceful shutdown by cancelling the context (this is what
	// signal.NotifyContext would do on real SIGTERM).
	cancel()
	_ = syscall.SIGTERM // suppress unused-import warning when this file is the only consumer

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			t.Errorf("Run returned error: %v", runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("server did not shut down within 5s of cancel")
	}

	// After shutdown the listener should be gone; a follow-up GET fails.
	if _, err := client.Get(baseURL + "/health"); err == nil {
		t.Errorf("GET /health after shutdown succeeded; expected connection refused")
	}
}

// TestPanicRecovery installs a handler that panics, then issues a
// request. The middleware should catch the panic, return 500 with a
// JSON body, and keep the process alive (without recover the entire
// server crashes).
func TestPanicRecovery(t *testing.T) {
	srv, _ := testServer(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/boom", func(w http.ResponseWriter, r *http.Request) {
		panic("intentional test panic")
	})
	handler := srv.recoverMiddleware(mux)

	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/boom")
	if err != nil {
		t.Fatalf("GET /boom: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status=%d, want 500", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type=%q, want application/json", got)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"internal error"`) {
		t.Errorf("body=%q, want it to mention internal error", string(body))
	}
}

// TestCORSAllowlist confirms the restrictive CORS policy: a request
// from a non-allowed origin gets no CORS headers (browser will block),
// while a request from the local default origin gets the headers
// echoed back with the concrete origin (never "*").
func TestCORSAllowlist(t *testing.T) {
	srv, _ := testServer(t)
	port := srv.settings.Port

	mux := http.NewServeMux()
	mux.HandleFunc("/x", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := srv.corsMiddleware(mux)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Allowed origin → CORS headers present, exact echo.
	allowedOrigin := "http://127.0.0.1:" + itoa(port)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/x", nil)
	req.Header.Set("Origin", allowedOrigin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("allowed origin: %v", err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != allowedOrigin {
		t.Errorf("allowed origin: ACAO=%q, want %q", got, allowedOrigin)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got == "*" {
		t.Errorf("allowed origin: ACAO returned wildcard")
	}

	// Disallowed origin → no CORS headers, browser will block.
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/x", nil)
	req.Header.Set("Origin", "http://attacker.example.com")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("denied origin: %v", err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("denied origin: ACAO=%q, want empty", got)
	}
}

// TestAPIRouteMirror confirms every primary route is also reachable at
// /api/<path>. Spot-check a few high-traffic ones to prove the
// register() loop wires both forms.
func TestAPIRouteMirror(t *testing.T) {
	srv, baseURL := testServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	if !waitForListen(t, baseURL+"/health", 3*time.Second) {
		t.Fatalf("server never became reachable")
	}

	client := noKeepAliveClient()
	for _, path := range []string{
		"/health", "/api/health",
		"/status", "/api/status",
		"/config", "/api/config",
	} {
		resp, err := client.Get(baseURL + path)
		if err != nil {
			t.Errorf("GET %s: %v", path, err)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status=%d, want 200", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// noKeepAliveClient returns a client that never pools connections. The
// lifecycle tests use it so no idle keep-alive connection is left open
// when the server shuts down (http.Server.Shutdown waits on fresh idle
// conns, which used to make TestServerStartsAndStops flaky).
func noKeepAliveClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{DisableKeepAlives: true},
		Timeout:   5 * time.Second,
	}
}

// waitForListen polls url until it returns any HTTP response or the
// timeout expires. Used by the lifecycle tests to avoid sleeping on a
// fixed schedule.
func waitForListen(t *testing.T, url string, timeout time.Duration) bool {
	t.Helper()
	client := noKeepAliveClient()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if resp, err := client.Get(url); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
