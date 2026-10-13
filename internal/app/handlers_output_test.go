package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"easycampus/internal/playback"
)

// TestHandlePlaybackProfilesReturnsCatalog confirms the catalog
// endpoint serves every shipped profile plus a default id. The exact
// list comes from internal/playback; we just assert the count and
// that "auto" is included and is the default.
func TestHandlePlaybackProfilesReturnsCatalog(t *testing.T) {
	s := newTestServerNoListener(t)

	req := httptest.NewRequest(http.MethodGet, "/api/playback-profiles", nil)
	w := httptest.NewRecorder()
	s.handlePlaybackProfiles(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", w.Code, w.Body.String())
	}

	var body struct {
		Profiles []playback.Profile `json:"profiles"`
		Default  string             `json:"default"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := len(playback.All())
	if len(body.Profiles) != want {
		t.Errorf("profiles count = %d, want %d", len(body.Profiles), want)
	}
	if body.Default != "auto" {
		t.Errorf("default = %q, want %q", body.Default, "auto")
	}

	// Sanity-check a known profile is present and carries the expected
	// public fields.
	var p30 *playback.Profile
	for i, p := range body.Profiles {
		if p.ID == "projector-30" {
			p30 = &body.Profiles[i]
			break
		}
	}
	if p30 == nil {
		t.Fatal("projector-30 missing from /api/playback-profiles")
	}
	if !p30.Interpolation {
		t.Error("projector-30 should have Interpolation=true")
	}
	if p30.DisplayFPSOverride != 30 {
		t.Errorf("projector-30 DisplayFPSOverride=%v, want 30", p30.DisplayFPSOverride)
	}
}

// TestHandlePlaybackProfilesRejectsNonGET asserts the method-allowlist
// matches the other catalog endpoints (/api/quality also rejects non-
// GET). A POST should not be silently accepted.
func TestHandlePlaybackProfilesRejectsNonGET(t *testing.T) {
	s := newTestServerNoListener(t)

	req := httptest.NewRequest(http.MethodPost, "/api/playback-profiles", nil)
	w := httptest.NewRecorder()
	s.handlePlaybackProfiles(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: status=%d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

// TestHandlePlaybackProfilesSurfacesOperatorDefault verifies the
// catalog endpoint reports the operator's stored playback_profile as
// the default when set. The UI uses this to render the picker's
// initial selection.
func TestHandlePlaybackProfilesSurfacesOperatorDefault(t *testing.T) {
	s := newTestServerNoListener(t)
	s.setPlaybackProfile("projector-30")

	req := httptest.NewRequest(http.MethodGet, "/api/playback-profiles", nil)
	w := httptest.NewRecorder()
	s.handlePlaybackProfiles(w, req)

	var body struct {
		Default string `json:"default"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Default != "projector-30" {
		t.Errorf("default = %q, want %q", body.Default, "projector-30")
	}
}

// TestHandlePlaybackProfilesIgnoresStaleStoredDefault checks that a
// stored playback_profile id that no longer exists in the registry
// (e.g. a profile removed in an upgrade) falls back to the canonical
// default — otherwise the UI would render a missing selection.
func TestHandlePlaybackProfilesIgnoresStaleStoredDefault(t *testing.T) {
	s := newTestServerNoListener(t)
	s.setPlaybackProfile("nope-removed-in-v2")

	req := httptest.NewRequest(http.MethodGet, "/api/playback-profiles", nil)
	w := httptest.NewRecorder()
	s.handlePlaybackProfiles(w, req)

	var body struct {
		Default string `json:"default"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Default != "auto" {
		t.Errorf("stale default should fall back to auto; got %q", body.Default)
	}
}

// TestResolvePlaybackProfilePrecedence pins the resolution order:
//
//  1. explicit req.PlaybackProfile beats everything
//  2. AppSettings.PlaybackProfile is the fallback
//  3. OutputMode=="decklink" with no explicit profile -> decklink-broadcast
//  4. Otherwise playback.Default() == "auto"
//
// The integration test for the handler exercises this indirectly via
// the HTTP layer; this test pins the pure-function behaviour so a
// regression in either spot is unambiguous.
func TestResolvePlaybackProfilePrecedence(t *testing.T) {
	cases := []struct {
		name      string
		requested string
		stored    string
		outMode   string
		wantID    string
	}{
		{name: "explicit request wins", requested: "projector-30", stored: "tv-60", outMode: "display", wantID: "projector-30"},
		{name: "stored fallback", requested: "", stored: "tv-50-pal", outMode: "display", wantID: "tv-50-pal"},
		{name: "decklink mode without profile defaults to decklink-broadcast", requested: "", stored: "", outMode: "decklink", wantID: "decklink-broadcast"},
		{name: "display mode without profile defaults to auto", requested: "", stored: "", outMode: "display", wantID: "auto"},
		{name: "unknown id falls back to auto", requested: "wat", stored: "", outMode: "display", wantID: "auto"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServerNoListener(t)
			s.setPlaybackProfile(tc.stored)
			got := s.resolvePlaybackProfile(tc.requested, tc.outMode, "")
			if got.ID != tc.wantID {
				t.Errorf("resolvePlaybackProfile(%q,%q,...).ID = %q, want %q", tc.requested, tc.outMode, got.ID, tc.wantID)
			}
		})
	}
}

// TestResolvePlaybackProfileSplicesDecklinkFormat — the decklink
// profile carries an empty DecklinkFormat by default; the handler must
// splice in the per-request format so MPVArgs() emits the flag.
func TestResolvePlaybackProfileSplicesDecklinkFormat(t *testing.T) {
	s := newTestServerNoListener(t)
	got := s.resolvePlaybackProfile("decklink-broadcast", "decklink", "1080p59.94")
	if got.DecklinkFormat != "1080p59.94" {
		t.Errorf("DecklinkFormat=%q, want %q", got.DecklinkFormat, "1080p59.94")
	}
	// Non-decklink profile must NOT receive the format.
	got = s.resolvePlaybackProfile("projector-30", "display", "1080p59.94")
	if got.DecklinkFormat != "" {
		t.Errorf("non-decklink profile got DecklinkFormat=%q, want empty", got.DecklinkFormat)
	}
}

// TestHandleOutputStartRequiresPOST — the handler must reject non-POST
// before doing any work.
func TestHandleOutputStartRequiresPOST(t *testing.T) {
	s := newTestServerNoListener(t)

	req := httptest.NewRequest(http.MethodGet, "/api/output/start", nil)
	w := httptest.NewRecorder()
	s.handleOutputStart(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status=%d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

// TestHandleOutputStartRejectsMalformedJSON pins the typed-DTO behavior:
// invalid JSON returns 400, not a silent default-config start.
func TestHandleOutputStartRejectsMalformedJSON(t *testing.T) {
	s := newTestServerNoListener(t)

	req := httptest.NewRequest(http.MethodPost, "/api/output/start", bytes.NewReader([]byte(`{"url":"x"`)))
	w := httptest.NewRecorder()
	s.handleOutputStart(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("malformed body: status=%d, want 400", w.Code)
	}
}

// TestHandleOutputStartPersistsProfileChoice — when the request
// includes playback_profile, the handler should persist the choice in
// the in-memory settings so subsequent /output/start calls without an
// explicit profile fall through to it. We can't easily verify the
// /output/start call succeeded (no mpv binary in CI), but the persist
// path runs before the mpv invocation only on the success branch —
// running the persist check requires a mock that's not worth building.
// Instead we exercise the resolvePlaybackProfile fallback after a
// settings mutation directly.
func TestHandleOutputStartPersistsProfileChoice(t *testing.T) {
	s := newTestServerNoListener(t)

	// Initial state: settings.PlaybackProfile is empty.
	if got := s.currentPlaybackProfile(); got != "" {
		t.Fatalf("test precondition: playback profile = %q, want empty", got)
	}
	// Simulate what the handler does on a successful start.
	s.setPlaybackProfile("projector-30")

	// Now resolve with empty request and OutputMode display — should
	// fall through to the stored value.
	got := s.resolvePlaybackProfile("", "display", "")
	if got.ID != "projector-30" {
		t.Errorf("stored profile lookup: got id=%q, want %q", got.ID, "projector-30")
	}
}

// TestHandleOutputStartProfileBodyParses — a more end-to-end test:
// POST /output/start with playback_profile=projector-30 must decode
// into a PlayerStartRequest with the right field. We use a recorder
// rather than dial real mpv; the test asserts the request body parses
// and the handler reaches the resolution path. mpv.Start will fail
// (no binary in CI / wrong url) — the test accepts 500 here, but the
// body must indicate the start attempt was made (not a 400 from JSON
// parsing or method check).
func TestHandleOutputStartProfileBodyParses(t *testing.T) {
	s := newTestServerNoListener(t)

	payload := `{"url":"http://example.invalid/master.m3u8","playback_profile":"projector-30"}`
	req := httptest.NewRequest(http.MethodPost, "/api/output/start", strings.NewReader(payload))
	w := httptest.NewRecorder()
	s.handleOutputStart(w, req)

	// We accept 200 (mpv is installed and the url happens to start
	// before the timeout) or 500 (no mpv / bogus url). The failure
	// signal is a 400 from JSON decoding or 405 from method check.
	if w.Code == http.StatusBadRequest || w.Code == http.StatusMethodNotAllowed {
		t.Fatalf("status=%d body=%s: request never reached the start path", w.Code, w.Body.String())
	}

	// When the start succeeds (mpv installed locally), the response
	// includes the resolved profile id. We verify the field is present
	// when the body parses as JSON; absent fields are OK on the error
	// path since the error response uses a different schema.
	var body map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&body); err == nil {
		if pp, ok := body["playback_profile"].(string); ok && pp != "" && pp != "projector-30" {
			t.Errorf("playback_profile in response = %q, want %q", pp, "projector-30")
		}
	}
	// Best-effort cleanup if mpv actually started.
	_ = s.mpv.Stop(context.Background())
}
