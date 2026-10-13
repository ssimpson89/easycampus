package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (s *Server) handleStreamStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	var req StreamStartRequest
	if err := decodeJSON(r, &req); err != nil {
		s.logger.Warn("stream start: bad request body", "err", err, "path", r.URL.Path)
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": err.Error()})
		return
	}
	config := s.streamConfigFromRequest(req)
	if strings.TrimSpace(config.EventName) == "" {
		config.EventName = time.Now().Format("January 02, 2006, 03:04 PM")
	}
	if s.ffmpeg.IsRunning() {
		s.writeJSON(w, http.StatusConflict, map[string]string{"detail": "stream already running"})
		return
	}
	// Bail before touching disk if the client is already gone.
	if err := r.Context().Err(); err != nil {
		s.logger.Info("stream start aborted: client disconnected", "err", err)
		return
	}

	// os.Mkdir under a fresh random-suffixed ID: never adopts an
	// existing event's directory, even for two starts in one second.
	eventID, eventDir, err := createEventDir(s.settings.EventsDir)
	if err != nil {
		s.logger.Warn("stream start: create event dir failed", "err", err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	preset := presetFromStreamConfig(config, s.settings)
	info := EventInfoFile{
		Name:          config.EventName,
		CreatedAt:     time.Now().Format(time.RFC3339),
		Status:        "running",
		Type:          "recording",
		FPS:           presetFPS(preset),
		QualityPreset: preset.ID(),
	}
	// info.json is written before StartStream so the supervisor's
	// failure hook always has a file to mark "failed"; any start error
	// removes the whole directory so no phantom "running" event remains.
	if err := writeEventInfo(filepath.Join(eventDir, "info.json"), info); err != nil {
		_ = os.RemoveAll(eventDir)
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}

	s.logger.Info("stream start requested",
		"event_id", eventID, "event_name", config.EventName,
		"framerate", config.VideoFramerate, "bitrate", config.VideoBitrate,
		"device", config.VideoDeviceIndex, "format", config.VideoDeviceFormat)

	// StartStream is mostly synchronous setup; the long-running
	// supervisor it spawns has its own internal lifetime ctx.
	// We pass r.Context() so a client disconnect during setup
	// (mkdir, validation, supervisor preflight) shorts out promptly
	// instead of completing pointless work the caller has already
	// abandoned.
	if err := s.ffmpeg.StartStream(r.Context(), eventID, config); err != nil {
		_ = os.RemoveAll(eventDir)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			s.logger.Info("stream start aborted: client disconnected",
				"event_id", eventID, "err", err)
			// Client is gone — no point writing a response body.
			return
		}
		if errors.Is(err, errStreamRunning) {
			s.writeJSON(w, http.StatusConflict, map[string]string{"detail": err.Error()})
			return
		}
		s.logger.Warn("stream start failed", "event_id", eventID, "err", err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}

	status := s.ffmpeg.GetStatus(r.Context())
	s.writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleStreamStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	currentEventID := s.ffmpeg.CurrentEventID()
	// Stop is a teardown path — once we've started, we must finish
	// even if the client disconnects, otherwise we leave a half-stopped
	// supervisor. Detach the request ctx so cancellation doesn't abort
	// the shutdown.
	_ = s.ffmpeg.StopStream(context.WithoutCancel(r.Context()))

	if currentEventID != "" {
		infoPath := filepath.Join(s.settings.EventsDir, currentEventID, "info.json")
		updateInfoStatus(infoPath, "completed", 0)
		s.logger.Info("stream stopped", "event_id", currentEventID)
	} else {
		s.logger.Info("stream stop called with no active event")
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

func (s *Server) handleStreamForceStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	var req ForceStopRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": err.Error()})
		return
	}
	if req.PID <= 0 {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "PID is required"})
		return
	}
	// Force-stop is a teardown path; detach from the request ctx so a
	// client disconnect doesn't leave the supervisor half-killed.
	killCtx := context.WithoutCancel(r.Context())
	// Capture the live event before killing: if this PID is the live
	// encoder, its info.json must leave "running" just like a normal
	// stop, or the event shows as live until the next restart.
	liveEventID := ""
	if s.ffmpeg.IsRunning() && s.ffmpeg.GetStatus(r.Context()).FFMpegPID == req.PID {
		liveEventID = s.ffmpeg.CurrentEventID()
	}
	killed, err := s.ffmpeg.ForceKillPID(killCtx, req.PID)
	if killed && liveEventID != "" {
		updateInfoStatus(filepath.Join(s.settings.EventsDir, liveEventID, "info.json"), "completed", 0)
	}
	if errors.Is(err, errPIDNotTracked) {
		s.logger.Warn("force stop refused: untracked pid", "pid", req.PID)
		s.writeJSON(w, http.StatusForbidden, map[string]string{"detail": err.Error()})
		return
	}
	status := s.ffmpeg.GetStatus(r.Context())
	message := "Failed to stop stream"
	if killed {
		message = "Stream stopped"
	}
	s.logger.Warn("force stop requested",
		"pid", req.PID, "event_id", req.EventID,
		"killed", killed)
	s.writeJSON(w, http.StatusOK, ForceStopResult{
		Killed:    killed,
		KilledPID: req.PID,
		Message:   message,
		Stream:    status,
	})
}

// presetFPS returns the preset's frame rate as a float, honouring
// fractional rates like "30000/1001".
func presetFPS(p interface {
	FPS() int
	FPSExpr() string
}) float64 {
	expr := p.FPSExpr()
	if num, den, ok := strings.Cut(expr, "/"); ok {
		n, err1 := strconv.ParseFloat(num, 64)
		d, err2 := strconv.ParseFloat(den, 64)
		if err1 == nil && err2 == nil && d != 0 {
			return n / d
		}
	} else if v, err := strconv.ParseFloat(expr, 64); err == nil {
		return v
	}
	return float64(p.FPS())
}

// streamConfigFromRequest translates the on-wire StreamStartRequest into
// the typed StreamConfig the supervisor consumes. The canonical field
// wins when both canonical and legacy aliases are set; legacy is the
// fallback. AppSettings defaults fill any remaining empty fields.
func (s *Server) streamConfigFromRequest(req StreamStartRequest) StreamConfig {
	config := StreamConfig{
		VideoDeviceFormat: s.settings.InputFormat,
		VideoDeviceIndex:  s.settings.InputDevice,
		AudioDeviceIndex:  "0",
		VideoDeviceType:   "webcam",
		VideoDeviceName:   "Default",
		AudioDeviceName:   "Default",
		VideoFramerate:    s.settings.DefaultFramerate,
		VideoBitrate:      fmt.Sprintf("%dk", s.settings.DefaultBitrate/1000),
	}

	switch {
	case req.EventName != "":
		config.EventName = req.EventName
	case req.Name != "":
		config.EventName = req.Name
	}

	switch {
	case req.VideoFramerate > 0:
		config.VideoFramerate = req.VideoFramerate
	case req.Framerate > 0:
		config.VideoFramerate = req.Framerate
	}

	switch {
	case req.VideoBitrate != "":
		config.VideoBitrate = req.VideoBitrate
	case req.BitrateKbps > 0:
		// Legacy "bitrate" field is kbps-flavored. Two historical UIs
		// sent it differently — bare-numbers like 4000 (kbps) and
		// full-rate numbers like 4000000 (bps). Treat values >=
		// 1_000_000 as bps; treat anything smaller as kbps. The cutoff
		// is unambiguous because nobody runs a 1 Mbps live broadcast.
		if req.BitrateKbps >= 1_000_000 {
			config.VideoBitrate = fmt.Sprintf("%dk", req.BitrateKbps/1000)
		} else {
			config.VideoBitrate = fmt.Sprintf("%dk", req.BitrateKbps)
		}
	}

	switch {
	case req.VideoDeviceIndex != "":
		config.VideoDeviceIndex = req.VideoDeviceIndex
	case req.InputID != "":
		config.VideoDeviceIndex = req.InputID
	}

	if req.VideoDeviceFormat != "" {
		config.VideoDeviceFormat = req.VideoDeviceFormat
	}
	if req.VideoDeviceType != "" {
		config.VideoDeviceType = req.VideoDeviceType
	}
	// quality_preset (e.g. "broadcast-1080p60") is the tier-aware
	// identifier. When set, the supervisor uses the preset's
	// bitrate/framerate envelope and the legacy video_bitrate /
	// video_framerate fields are advisory only.
	if req.QualityPreset != "" {
		config.QualityPreset = req.QualityPreset
	}

	return config
}

// decodeJSON is a small wrapper around json.NewDecoder that rejects
// trailing data so a typo like {"a":1}{"a":2} surfaces as 400 instead
// of silently dropping the second object. Empty bodies decode as the
// zero value to preserve "no body == use defaults" semantics.
//
// Unknown fields are intentionally ignored — the legacy UI may send
// fields the canonical DTO doesn't define (e.g. a stale build still
// posting `audio_device_index`); dropping them silently is the right
// migration story.
func decodeJSON(r *http.Request, target interface{}) error {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(target); err != nil {
		// EOF is the empty-body case — treat as defaults.
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	if dec.More() {
		return fmt.Errorf("unexpected trailing data in request body")
	}
	return nil
}
