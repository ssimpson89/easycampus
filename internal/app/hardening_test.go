package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestValidID(t *testing.T) {
	for _, id := range []string{"20240101_120000", "20240101_120000_ab12cd", "bm_001", "a-b_C9"} {
		if !validID(id) {
			t.Errorf("validID(%q) = false, want true", id)
		}
	}
	for _, id := range []string{"", ".", "..", "../x", "a/b", `a\b`, "a b", "a.b", "/abs", strings.Repeat("a", 200)} {
		if validID(id) {
			t.Errorf("validID(%q) = true, want false", id)
		}
	}
}

func TestSafeSegmentName(t *testing.T) {
	for _, n := range []string{"seg_00001.m4s", "init.mp4", "stream0.ts"} {
		if !safeSegmentName(n) {
			t.Errorf("safeSegmentName(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"", ".", "..", "../evil.ts", "a/b.ts", `..\evil.ts`, "/etc/passwd",
		"http://evil/x.ts", "file:x", "C:evil", "x..ts"} {
		if safeSegmentName(n) {
			t.Errorf("safeSegmentName(%q) = true, want false", n)
		}
	}
}

func TestCreateEventDirUnique(t *testing.T) {
	base := t.TempDir()
	seen := map[string]bool{}
	for range 20 {
		id, dir, err := createEventDir(base)
		if err != nil {
			t.Fatalf("createEventDir: %v", err)
		}
		if !validID(id) || seen[id] {
			t.Fatalf("bad or duplicate id %q", id)
		}
		seen[id] = true
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Fatalf("dir %s not created", dir)
		}
	}
}

func serve(s *Server, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.httpServer.Handler.ServeHTTP(w, req)
	return w
}

func TestCacheStartRejectsUnsafeInput(t *testing.T) {
	s := newTestServerNoListener(t)
	cases := []string{
		`{"event_id":"../../etc","remote_url":"http://peer:8000"}`,
		`{"event_id":"","remote_url":"http://peer:8000"}`,
		`{"event_id":"evt1","remote_url":"file:///etc/passwd"}`,
		`{"event_id":"evt1","remote_url":"peer:8000"}`,
	}
	for _, body := range cases {
		w := serve(s, http.MethodPost, "/cache/start", body, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: status=%d, want 400", body, w.Code)
		}
	}
	if n := s.cache.activeCount(); n != 0 {
		t.Errorf("activeCount=%d after rejected starts, want 0", n)
	}
	if entries, _ := os.ReadDir(s.settings.CacheDir); len(entries) != 0 {
		t.Errorf("CacheDir has %d entries after rejected starts, want 0", len(entries))
	}
}

func TestCacheDeleteEmptyOrInvalidID(t *testing.T) {
	s := newTestServerNoListener(t)
	marker := filepath.Join(s.settings.CacheDir, "keep", "x.ts")
	if err := os.MkdirAll(filepath.Dir(marker), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/cache/", "/api/cache/", "/cache/bad.id"} {
		w := serve(s, http.MethodDelete, path, "", nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("DELETE %s: status=%d, want 400", path, w.Code)
		}
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("cache content removed by invalid delete: %v", err)
	}
	if err := s.cache.DeleteCache(context.Background(), ""); err == nil {
		t.Error("DeleteCache(\"\") = nil, want error")
	}
}

// TestCacheLoopSkipsUnsafeSegments feeds a playlist with a traversal
// segment name and checks only the safe segment is written, inside the
// event's cache dir, and that the playlist lands after it.
func TestCacheLoopSkipsUnsafeSegments(t *testing.T) {
	outside := t.TempDir()
	evil := filepath.Join(outside, "evil.ts")
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/master.m3u8"):
			fmt.Fprintf(w, "#EXTM3U\n#EXTINF:2,\n../../%s\n#EXTINF:2,\n%s\n#EXTINF:2,\nok.ts\n#EXT-X-ENDLIST\n",
				filepath.Base(outside)+"/evil.ts", evil)
		default:
			_, _ = w.Write([]byte("segment"))
		}
	}))
	defer peer.Close()

	svc := newTestCacheService(t)
	if err := svc.StartCaching(context.Background(), "evt1", peer.URL); err != nil {
		t.Fatalf("StartCaching: %v", err)
	}
	defer svc.StopAll()

	playlist := filepath.Join(svc.settings.CacheDir, "evt1", "master.m3u8")
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(playlist); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("playlist never written")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Playlist is written after segments, so ok.ts must already exist.
	if _, err := os.Stat(filepath.Join(svc.settings.CacheDir, "evt1", "ok.ts")); err != nil {
		t.Errorf("ok.ts missing when playlist present: %v", err)
	}
	if _, err := os.Stat(evil); err == nil {
		t.Errorf("traversal segment written outside cache dir: %s", evil)
	}
}

func TestCrossOriginProtection(t *testing.T) {
	s := newTestServerNoListener(t)
	allowed := s.allowedOrigins[0]

	// Cross-site browser POST is rejected before reaching the handler.
	w := serve(s, http.MethodPost, "/output/stop", "", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "http://evil.example"})
	if w.Code != http.StatusForbidden {
		t.Errorf("cross-site POST: status=%d, want 403", w.Code)
	}
	// Origin-only (older browser) mismatch is rejected too.
	w = serve(s, http.MethodPost, "/output/stop", "", map[string]string{"Origin": "http://evil.example"})
	if w.Code != http.StatusForbidden {
		t.Errorf("foreign Origin POST: status=%d, want 403", w.Code)
	}
	// Non-browser client (no Origin / Sec-Fetch-Site) still works.
	w = serve(s, http.MethodPost, "/output/stop", "", nil)
	if w.Code != http.StatusOK {
		t.Errorf("no-Origin POST: status=%d, want 200", w.Code)
	}
	// Same-origin browser request works.
	w = serve(s, http.MethodPost, "/output/stop", "", map[string]string{"Sec-Fetch-Site": "same-origin"})
	if w.Code != http.StatusOK {
		t.Errorf("same-origin POST: status=%d, want 200", w.Code)
	}
	// Configured allowed origin is trusted even when cross-site.
	w = serve(s, http.MethodPost, "/output/stop", "", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": allowed})
	if w.Code != http.StatusOK {
		t.Errorf("trusted origin POST: status=%d, want 200", w.Code)
	}
	// Safe methods are never blocked.
	w = serve(s, http.MethodGet, "/status", "", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "http://evil.example"})
	if w.Code != http.StatusOK {
		t.Errorf("cross-site GET: status=%d, want 200", w.Code)
	}
}

func TestForceStopRefusesUntrackedPID(t *testing.T) {
	s := newTestServerNoListener(t)
	body := fmt.Sprintf(`{"pid":%d}`, os.Getpid())
	w := serve(s, http.MethodPost, "/stream/force_stop", body, nil)
	if w.Code != http.StatusForbidden {
		t.Errorf("force_stop own pid: status=%d, want 403 (body=%s)", w.Code, w.Body.String())
	}
}

func TestEventDeleteStatusCodes(t *testing.T) {
	s := newTestServerNoListener(t)
	if w := serve(s, http.MethodDelete, "/events/nope_123", "", nil); w.Code != http.StatusNotFound {
		t.Errorf("missing event: status=%d, want 404", w.Code)
	}
	if w := serve(s, http.MethodDelete, "/events/bad.id", "", nil); w.Code != http.StatusBadRequest {
		t.Errorf("invalid id: status=%d, want 400", w.Code)
	}
	id, _, err := createEventDir(s.settings.EventsDir)
	if err != nil {
		t.Fatal(err)
	}
	if w := serve(s, http.MethodDelete, "/events/"+id, "", nil); w.Code != http.StatusNoContent {
		t.Errorf("existing event: status=%d, want 204", w.Code)
	}
}

func TestBookmarkIDsNeverCollide(t *testing.T) {
	s := newTestServerNoListener(t)
	id, _, err := createEventDir(s.settings.EventsDir)
	if err != nil {
		t.Fatal(err)
	}
	base := "/events/" + id + "/bookmarks"
	create := func(body string) (int, Bookmark) {
		w := serve(s, http.MethodPost, base, body, nil)
		var resp struct {
			Bookmark Bookmark `json:"bookmark"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		return w.Code, resp.Bookmark
	}
	for range 3 {
		create(`{"name":"x"}`)
	}
	if w := serve(s, http.MethodDelete, base+"/bm_002", "", nil); w.Code != http.StatusOK {
		t.Fatalf("delete bm_002: status=%d", w.Code)
	}
	if _, bm := create(`{"name":"y"}`); bm.ID != "bm_004" {
		t.Errorf("after delete, new id=%q, want bm_004", bm.ID)
	}
	if code, bm := create(`{"id":"cue-1","name":"z"}`); code != http.StatusOK || bm.ID != "cue-1" {
		t.Errorf("client id: code=%d id=%q, want 200 cue-1", code, bm.ID)
	}
	if code, _ := create(`{"id":"cue-1"}`); code != http.StatusConflict {
		t.Errorf("duplicate client id: code=%d, want 409", code)
	}
	if code, _ := create(`{"id":"../x"}`); code != http.StatusBadRequest {
		t.Errorf("invalid client id: code=%d, want 400", code)
	}

	// Concurrent creates must all land with unique IDs.
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			create(`{"name":"c"}`)
		}()
	}
	wg.Wait()
	bms, err := readBookmarks(s.settings, id)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, bm := range bms {
		if seen[bm.ID] {
			t.Errorf("duplicate bookmark id %q", bm.ID)
		}
		seen[bm.ID] = true
	}
	if len(bms) != 24 {
		t.Errorf("bookmark count=%d, want 24 (lost update?)", len(bms))
	}
}

func TestOutputSyncUnknownCommand(t *testing.T) {
	s := newTestServerNoListener(t)
	if w := serve(s, http.MethodPost, "/output/sync", `{"command":"rewind"}`, nil); w.Code != http.StatusBadRequest {
		t.Errorf("unknown command: status=%d, want 400", w.Code)
	}
	// mpv isn't running, so play must surface an error instead of 200.
	if w := serve(s, http.MethodPost, "/output/sync", `{"command":"play"}`, nil); w.Code == http.StatusOK {
		t.Errorf("play without mpv: status=200, want error")
	}
}

func TestReconcileStaleEventsAndRecordPID(t *testing.T) {
	settings := AppSettings{EventsDir: t.TempDir()}
	write := func(id, status string) string {
		p := filepath.Join(settings.EventsDir, id, "info.json")
		if err := writeEventInfo(p, EventInfoFile{Name: id, CreatedAt: time.Now().Format(time.RFC3339), Status: status, Type: "recording"}); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("live_1", "running")
	write("done_1", "completed")
	if n := reconcileStaleEvents(settings); n != 1 {
		t.Errorf("reconciled %d events, want 1", n)
	}
	if info, _ := readEventInfo(settings, "live_1"); info.Status != "stopped" || info.StoppedAt == "" {
		t.Errorf("live_1 status=%q stopped_at=%q, want stopped + timestamp", info.Status, info.StoppedAt)
	}

	// recordInfoPID must not resurrect a job that already finished.
	p := write("job_1", "failed")
	recordInfoPID(p, 1234)
	if info, _ := readEventInfo(settings, "job_1"); info.Status != "failed" || info.PID != "" {
		t.Errorf("job_1 status=%q pid=%q, want failed with no pid", info.Status, info.PID)
	}
}

// TestShutdownForceClosesIdleConns pins the Shutdown half of the
// TestServerStartsAndStops flake: a dialed-but-idle (StateNew)
// connection must not hold Shutdown past the HTTP drain budget.
func TestShutdownForceClosesIdleConns(t *testing.T) {
	port := pickFreePort(t)
	settings := AppSettings{Port: port, NodeType: "encoder", BaseDir: t.TempDir(), EventsDir: t.TempDir(), CacheDir: t.TempDir()}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := NewServer(ServerConfig{Settings: settings, Logger: logger, HTTPDrainTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe() }()
	baseURL := "http://127.0.0.1" + srv.Addr()
	if !waitForListen(t, baseURL+"/health", 3*time.Second) {
		t.Fatal("server never became reachable")
	}
	conn, err := net.Dial("tcp", "127.0.0.1"+srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	if err := srv.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Shutdown took %s with an idle conn; want ~drain budget", elapsed)
	}
	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Error("ListenAndServe did not return after Shutdown")
	}
}
