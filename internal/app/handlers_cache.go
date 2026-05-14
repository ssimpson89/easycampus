package app

import (
	"context"
	"fmt"
	"net/http"
	"os"
)

func (s *Server) handleCacheStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	var req CacheStartRequest
	if err := readJSON(r, &req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": err.Error()})
		return
	}
	// The cache loop must outlive the request that started it — a
	// client disconnect after POST /cache/start should not kill the
	// downloader. CacheService.StartCaching internally derives a
	// loop ctx via context.WithoutCancel(parent) + WithCancel, so
	// passing r.Context() here lets future per-tenant ctx values
	// (e.g. request_id) flow through without giving the request
	// cancellation control over the loop's lifetime.
	s.cache.StartCaching(r.Context(), req.EventID, req.RemoteURL)
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "message": fmt.Sprintf("Started caching event %s", req.EventID)})
}

func (s *Server) handleCacheStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	eventID := pathSegmentAfter(r.URL.Path, "stop")
	if eventID == "" {
		eventID = pathSegmentAfter(r.URL.Path, "cache")
	}
	// Stop is a teardown path — detach from the request ctx so a
	// client disconnect doesn't abort halfway through draining the
	// loop's goroutine.
	s.cache.StopCaching(context.WithoutCancel(r.Context()), eventID)
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "message": fmt.Sprintf("Stopped caching event %s", eventID)})
}

func (s *Server) handleCacheDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	eventID := pathSegmentAfter(r.URL.Path, "cache")
	// Detach from the request ctx for the same reason as Stop: the
	// delete must complete (loop drain + os.RemoveAll) regardless of
	// whether the client is still listening for the response.
	s.cache.DeleteCache(context.WithoutCancel(r.Context()), eventID)
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "message": fmt.Sprintf("Deleted cache for event %s", eventID)})
}

func (s *Server) handleCacheStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	eventID := pathSegmentAfter(r.URL.Path, "cache")
	if eventID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "missing event id"})
		return
	}
	status := s.cache.Status(r.Context(), eventID)
	s.writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleCacheList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	entries, err := os.ReadDir(s.settings.CacheDir)
	if err != nil {
		s.writeJSON(w, http.StatusOK, []CachedEventInfo{})
		return
	}

	ctx := r.Context()
	cached := make([]CachedEventInfo, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		eventID := entry.Name()
		status := s.cache.Status(ctx, eventID)
		if status.Cached {
			cached = append(cached, CachedEventInfo{
				EventID:    eventID,
				Segments:   status.Segments,
				SizeBytes:  status.SizeBytes,
				IsComplete: !status.Active,
			})
		}
	}
	s.writeJSON(w, http.StatusOK, cached)
}
