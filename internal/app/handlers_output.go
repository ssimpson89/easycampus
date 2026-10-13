package app

import (
	"context"
	"errors"
	"net/http"

	"easycampus/internal/playback"
)

func (s *Server) handleOutputCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	ctx := r.Context()
	displays := s.detect.ListDisplays(ctx)
	decklink := s.detect.CheckDecklinkAvailable(ctx)
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"displays": displays, "decklink": decklink})
}

func (s *Server) handleOutputStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	var req PlayerStartRequest
	if err := decodeJSON(r, &req); err != nil {
		s.logger.Warn("output: bad request body", "path", r.URL.Path, "err", err)
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": err.Error()})
		return
	}
	// Displays are 0-based; only default when the client omitted the
	// field. The default stays 1 (the secondary / projector display)
	// to preserve legacy behaviour for clients that never sent it.
	screenIndex := 1
	if req.ScreenIndex != nil {
		if *req.ScreenIndex < 0 {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "screen_index must be >= 0"})
			return
		}
		screenIndex = *req.ScreenIndex
	}
	if req.OutputMode == "" {
		req.OutputMode = "display"
	}
	if req.DecklinkFormat == "" {
		req.DecklinkFormat = "720p59.94"
	}
	// The request ctx bounds only startup (stopping any previous mpv
	// and waiting for the IPC socket). If the client disconnects during
	// that window mpv.Start kills the half-started child; once it's up,
	// mpv runs until /output/stop regardless of this request.
	profile := s.resolvePlaybackProfile(req.PlaybackProfile, req.OutputMode, req.DecklinkFormat)
	if err := profile.CheckAvailable(); err != nil {
		s.logger.Warn("output start refused: profile unavailable", "profile", profile.ID, "err", err)
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": err.Error()})
		return
	}
	if err := s.mpv.Start(r.Context(), req.URL, screenIndex, profile, req.Fullscreen); err != nil {
		s.logger.Warn("output start failed", "url", req.URL, "err", err)
		status := http.StatusInternalServerError
		if errors.Is(err, errInvalidPlaybackURL) {
			status = http.StatusBadRequest
		}
		s.writeJSON(w, status, map[string]string{"detail": err.Error()})
		return
	}
	// Persist the operator's last choice in-memory so subsequent
	// /output/start calls without an explicit profile fall through to
	// it (until the daemon restarts; durable persistence is the YAML
	// playback_profile field).
	if req.PlaybackProfile != "" {
		s.setPlaybackProfile(req.PlaybackProfile)
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":          true,
		"message":          "Started output",
		"playback_profile": profile.ID,
	})
}

// resolvePlaybackProfile turns the operator's choice (explicit field,
// remembered AppSettings value, or empty) into a concrete
// playback.Profile. Falls back to playback.Default() ("auto") when no
// stored or requested profile is set.
//
// Legacy /output/start callers send only OutputMode ("display" vs
// "decklink") without a PlaybackProfile. We honor that by mapping:
//
//   - OutputMode=="decklink" + no profile  -> "decklink-broadcast"
//     (preserves "decklink path uses --vo=decklink" intuition).
//   - OutputMode=="display"  + no profile  -> Default() (auto).
//
// When the resolved profile is decklink-broadcast and the operator
// picked a format in the existing UI, we splice the format into the
// returned profile so MPVArgs() emits --decklink-format=...
func (s *Server) resolvePlaybackProfile(requested, outputMode, decklinkFormat string) playback.Profile {
	id := requested
	if id == "" {
		id = s.currentPlaybackProfile()
	}
	if id == "" {
		if outputMode == "decklink" {
			id = "decklink-broadcast"
		} else {
			id = playback.DefaultID
		}
	}
	profile, ok := playback.ByID(id)
	if !ok {
		s.logger.Warn("output: unknown playback profile, falling back to default",
			"requested", id)
		profile = playback.Default()
	}
	// Thread the UI's decklink_format choice into the resolved profile
	// — the catalog leaves DecklinkFormat empty so MPVArgs() doesn't
	// emit the flag unless we splice it in here.
	if profile.OutputMode == playback.OutputModeDecklink && decklinkFormat != "" {
		profile.DecklinkFormat = decklinkFormat
	}
	return profile
}

func (s *Server) handleOutputStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	// Stop is a teardown path — must always run to completion even if
	// the client disconnects, otherwise we leak the mpv child.
	_ = s.mpv.Stop(context.WithoutCancel(r.Context()))
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "message": "Stopped output"})
}

func (s *Server) handleOutputStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	running := s.mpv.IsRunning(r.Context())
	message := "No active output"
	if running {
		message = "Output is running"
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"is_running": running, "message": message})
}

func (s *Server) handleOutputUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	ctx := r.Context()
	if !s.mpv.IsRunning(ctx) {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "No active output to update"})
		return
	}
	var req PlayerStartRequest
	if err := decodeJSON(r, &req); err != nil {
		s.logger.Warn("output: bad request body", "path", r.URL.Path, "err", err)
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": err.Error()})
		return
	}
	if err := s.mpv.Load(ctx, req.URL); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errInvalidPlaybackURL) {
			status = http.StatusBadRequest
		}
		s.writeJSON(w, status, map[string]string{"detail": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "message": "Updated output stream"})
}

func (s *Server) handleOutputSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	var req PlayerControlRequest
	if err := decodeJSON(r, &req); err != nil {
		s.logger.Warn("output: bad request body", "path", r.URL.Path, "err", err)
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": err.Error()})
		return
	}
	ctx := r.Context()
	s.logger.Info("output sync",
		"command", req.Command, "timestamp", req.Timestamp)
	var err error
	switch req.Command {
	case "play":
		err = s.mpv.Play(ctx)
		if err == nil && req.Timestamp > 0 {
			err = s.mpv.Seek(ctx, req.Timestamp)
		}
	case "pause":
		err = s.mpv.Pause(ctx)
		if err == nil && req.Timestamp > 0 {
			err = s.mpv.Seek(ctx, req.Timestamp)
		}
	case "seek":
		err = s.mpv.Seek(ctx, req.Timestamp)
	default:
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "unknown command: " + req.Command})
		return
	}
	if err != nil {
		s.logger.Warn("output sync failed", "command", req.Command, "timestamp", req.Timestamp, "err", err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "message": "Sent " + req.Command})
}

func (s *Server) handleOutputMute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	var req MuteRequest
	if err := decodeJSON(r, &req); err != nil {
		s.logger.Warn("output: bad request body", "path", r.URL.Path, "err", err)
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": err.Error()})
		return
	}
	if err := s.mpv.SetMute(r.Context(), req.Muted); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "message": "Mute set"})
}

func (s *Server) handleOutputPosition(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	ctx := r.Context()
	if !s.mpv.IsRunning(ctx) {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"detail": "Output not running"})
		return
	}
	position, err := s.mpv.GetPosition(ctx)
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"detail": "Failed to get position from MPV"})
		return
	}
	paused, _ := s.mpv.IsPaused(ctx)
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"position": position, "paused": paused})
}

func (s *Server) handleOutputFramePosition(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	ctx := r.Context()
	if !s.mpv.IsRunning(ctx) {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"detail": "Output not running"})
		return
	}
	frameInfo, err := s.mpv.GetFrameInfo(ctx)
	if err != nil || frameInfo == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"detail": "Failed to get frame info from MPV"})
		return
	}
	paused, _ := s.mpv.IsPaused(ctx)
	frameInfo["paused"] = paused
	s.writeJSON(w, http.StatusOK, frameInfo)
}

// handlePlaybackProfiles serves the operator-facing catalog of mpv
// playback profiles. Used by the decoder UI to render the profile
// picker. Always returns 200 with the registry even when empty so the
// UI can fall back to its baked-in defaults.
//
//	GET /playback-profiles  (alias: /api/playback-profiles)
//	{
//	  "profiles": [
//	    {"id":"auto","label":"Auto","description":"...",
//	     "video_sync":"audio","interpolation":false,
//	     "display_fps_override":0,"hwdec":"auto"}, ...
//	  ],
//	  "default": "auto"
//	}
func (s *Server) handlePlaybackProfiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	// The default surfaced to the UI is the operator's stored choice
	// when set, otherwise the package's hardcoded default. This keeps
	// the first-run experience aligned with what /output/start will
	// actually do.
	def := s.currentPlaybackProfile()
	if def == "" {
		def = playback.DefaultID
	}
	// Resolve to a known id — if the stored value is stale (e.g. a
	// profile was removed in an upgrade), fall back to the canonical
	// default so the UI doesn't render a missing selection.
	if _, ok := playback.ByID(def); !ok {
		def = playback.DefaultID
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"profiles": playback.All(),
		"default":  def,
	})
}

func (s *Server) handleOutputDiagnostics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	ctx := r.Context()
	if !s.mpv.IsRunning(ctx) {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"detail": "Output not running"})
		return
	}
	diag := s.mpv.GetDiagnostics(ctx)
	diag["current_url"] = s.mpv.GetCurrentURL()
	s.writeJSON(w, http.StatusOK, diag)
}
