package app

import (
	"net/http"
	"os"
	"strings"

	"easycampus/internal/version"
)

func (s *Server) handleInputs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	devices, err := s.ffmpeg.ListInputDevices(r.Context())
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, devices)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	ctx := r.Context()
	status := s.ffmpeg.GetStatus(ctx)
	writable := false
	if info, err := os.Stat(s.settings.EventsDir); err == nil && info.IsDir() {
		writable = true
	}
	v := version.Current()
	response := HealthStatus{
		OK:                true,
		Version:           VersionInfo{Version: v.Version, Commit: v.Commit, Date: v.Date},
		FFMpegAvailable:   s.ffmpeg.FFMpegAvailable(ctx),
		InputFormat:       s.settings.InputFormat,
		InputDevice:       s.settings.InputDevice,
		EventsDir:         s.settings.EventsDir,
		EventsDirWritable: writable,
		Stream:            status,
	}
	s.writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}

	// If EncoderHost is set and not localhost, prefer constructing the URL from it
	encoderURL := s.settings.DefaultEncoderURL
	if s.settings.EncoderHost != "" && s.settings.EncoderHost != "localhost" {
		// Assume standard port 8000 if not specified in host
		host := s.settings.EncoderHost
		if !strings.Contains(host, ":") {
			host = host + ":8000"
		}
		// Add protocol if missing
		if !strings.HasPrefix(host, "http") {
			encoderURL = "http://" + host
		} else {
			encoderURL = host
		}
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"DEFAULT_ENCODER_URL": encoderURL,
		"NODE_TYPE":           s.settings.NodeType,
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	s.writeJSON(w, http.StatusOK, s.ffmpeg.GetStatus(r.Context()))
}

// pathSegmentAfter returns the URL path segment immediately after the
// given segment name. Used by handlers that need to extract an event ID
// or bookmark ID from a path like /events/<id>/bookmarks/<bookmark>.
func pathSegmentAfter(path, segment string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i, p := range parts {
		if p == segment && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}
