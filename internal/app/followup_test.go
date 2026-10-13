package app

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
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
