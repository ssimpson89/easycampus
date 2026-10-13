package app

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestBookmarkWriteAfterDeleteDoesNotResurrectEvent: once an event is
// deleted, a bookmark create for it returns 404 and must not recreate
// the event directory (atomicfile.Write would otherwise MkdirAll it).
func TestBookmarkWriteAfterDeleteDoesNotResurrectEvent(t *testing.T) {
	s := newTestServerNoListener(t)
	const id = "20261013_120000_abcdef"
	dir := filepath.Join(s.settings.EventsDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "info.json"), []byte(`{"id":"`+id+`","status":"completed"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if w := serve(s, http.MethodDelete, "/api/events/"+id, "", nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete: status=%d body=%s", w.Code, w.Body.String())
	}
	w := serve(s, http.MethodPost, "/api/events/"+id+"/bookmarks", `{"name":"late","timestamp":1}`,
		map[string]string{"Content-Type": "application/json"})
	if w.Code != http.StatusNotFound {
		t.Errorf("bookmark after delete: status=%d, want 404 (body=%s)", w.Code, w.Body.String())
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("event directory was recreated after delete (stat err=%v)", err)
	}
}

func TestPlaylistFinalized(t *testing.T) {
	dir := t.TempDir()
	partial := filepath.Join(dir, "partial.m3u8")
	done := filepath.Join(dir, "done.m3u8")
	_ = os.WriteFile(partial, []byte("#EXTM3U\n#EXTINF:2,\nsegment_000000.ts\n"), 0o644)
	_ = os.WriteFile(done, []byte("#EXTM3U\n#EXTINF:2,\nsegment_000000.ts\n#EXT-X-ENDLIST\n"), 0o644)
	if playlistFinalized(partial) {
		t.Error("partial playlist reported as finalized")
	}
	if !playlistFinalized(done) {
		t.Error("finished playlist not reported as finalized")
	}
	if playlistFinalized(filepath.Join(dir, "missing.m3u8")) {
		t.Error("missing playlist reported as finalized")
	}
}

// TestReconcileJobStatusRequiresFinalizedPlaylist: a dead job whose
// playlist was never closed is "failed", not "completed".
func TestReconcileJobStatusRequiresFinalizedPlaylist(t *testing.T) {
	s := newTestServerNoListener(t)
	for _, tc := range []struct {
		name, playlist, want string
	}{
		{"interrupted", "#EXTM3U\n#EXTINF:2,\nsegment_000000.ts\n", "failed"},
		{"finished", "#EXTM3U\n#EXTINF:2,\nsegment_000000.ts\n#EXT-X-ENDLIST\n", "completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			infoPath := filepath.Join(dir, "info.json")
			// PID 999999 is not a tracked job and almost surely not alive.
			_ = os.WriteFile(infoPath, []byte(`{"status":"processing","pid":"999999"}`), 0o644)
			_ = os.WriteFile(filepath.Join(dir, "master.m3u8"), []byte(tc.playlist), 0o644)
			info := EventInfoFile{Status: "processing", PID: "999999"}
			s.reconcileJobStatus(&info, dir, infoPath)
			if info.Status != tc.want {
				t.Errorf("status=%q, want %q", info.Status, tc.want)
			}
		})
	}
}

// TestKeyedMutexReleasesEntries: the lock registry only holds keys in
// active use, and mutual exclusion still holds while waiters queue.
func TestKeyedMutexReleasesEntries(t *testing.T) {
	var k keyedMutex
	for i := 0; i < 100; i++ {
		unlock := k.lock(fmt.Sprintf("evt-%d", i))
		unlock()
	}
	if n := k.size(); n != 0 {
		t.Fatalf("registry holds %d keys after all released, want 0", n)
	}

	unlock := k.lock("shared")
	acquired := make(chan struct{})
	go func() {
		u := k.lock("shared")
		close(acquired)
		u()
	}()
	select {
	case <-acquired:
		t.Fatal("second holder acquired the lock while the first still held it")
	case <-time.After(100 * time.Millisecond):
	}
	if n := k.size(); n != 1 {
		t.Errorf("registry holds %d keys with one holder and one waiter, want 1", n)
	}
	unlock()
	unlock() // idempotent
	<-acquired
	waitUntil(t, func() bool { return k.size() == 0 })
}

// TestEventDeleteWaitsForInFlightBookmarkWrite exercises the race from
// the review: a bookmark write is mid-flight (holding the per-event lock,
// past its existence check) when DELETE arrives. DELETE must block until
// the write finishes, then remove the directory, and the directory must
// stay gone.
func TestEventDeleteWaitsForInFlightBookmarkWrite(t *testing.T) {
	s := newTestServerNoListener(t)
	const id = "20261013_120000_fedcba"
	dir := filepath.Join(s.settings.EventsDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "info.json"), []byte(`{"id":"`+id+`","status":"completed"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Simulate a bookmark request that has taken the lock and is about
	// to write.
	unlock := s.bookmarkLocks.lock(id)

	deleted := make(chan int, 1)
	go func() {
		w := serve(s, http.MethodDelete, "/api/events/"+id, "", nil)
		deleted <- w.Code
	}()

	select {
	case code := <-deleted:
		unlock()
		t.Fatalf("DELETE finished (status %d) while a bookmark write held the event lock", code)
	case <-time.After(150 * time.Millisecond):
	}
	// The in-flight write completes inside the window DELETE is waiting on.
	if err := writeBookmarks(s.settings, id, []Bookmark{{ID: "bm_001", Name: "x"}}); err != nil {
		unlock()
		t.Fatalf("in-flight bookmark write failed: %v", err)
	}
	unlock()

	select {
	case code := <-deleted:
		if code != http.StatusNoContent {
			t.Fatalf("DELETE status=%d, want 204", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DELETE never completed after the lock was released")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("event directory still exists after DELETE (stat err=%v)", err)
	}
	if err := writeBookmarks(s.settings, id, nil); err == nil {
		t.Error("bookmark write after DELETE succeeded; want an error, not a recreated directory")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("event directory was recreated after DELETE (stat err=%v)", err)
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 2s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
