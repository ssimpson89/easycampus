package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	events, err := listEvents(s.settings)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, events)
}

func (s *Server) handleEventDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	eventID := pathSegmentAfter(r.URL.Path, "events")
	if eventID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "missing event id"})
		return
	}
	if err := deleteEvent(s.settings, eventID); err != nil {
		s.logger.Warn("event delete failed", "event_id", eventID, "err", err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	s.logger.Info("event deleted", "event_id", eventID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleEventBookmarks(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	path = strings.TrimPrefix(path, "/api")
	path = strings.TrimPrefix(path, "/events/")
	parts := strings.Split(path, "/")

	if len(parts) < 2 || parts[1] != "bookmarks" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "invalid URL"})
		return
	}

	eventID := parts[0]

	if len(parts) == 2 {
		switch r.Method {
		case http.MethodGet:
			bookmarks, err := readBookmarks(s.settings, eventID)
			if err != nil {
				s.writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
				return
			}
			s.writeJSON(w, http.StatusOK, map[string]interface{}{"bookmarks": bookmarks})
		case http.MethodPost:
			var req BookmarkCreateRequest
			if err := decodeJSON(r, &req); err != nil {
				s.logger.Warn("bookmark create: bad body", "event_id", eventID, "err", err)
				s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": err.Error()})
				return
			}
			bookmark, err := createBookmark(s.settings, eventID, req)
			if err != nil {
				s.writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
				return
			}
			s.writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "bookmark": bookmark})
		default:
			s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		}
		return
	}

	if len(parts) == 3 {
		bookmarkID := parts[2]
		switch r.Method {
		case http.MethodPut:
			var req BookmarkUpdateRequest
			if err := decodeJSON(r, &req); err != nil {
				s.logger.Warn("bookmark update: bad body", "event_id", eventID, "bookmark_id", bookmarkID, "err", err)
				s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": err.Error()})
				return
			}
			bookmark, err := updateBookmark(s.settings, eventID, bookmarkID, req)
			if err != nil {
				s.writeJSON(w, http.StatusNotFound, map[string]string{"detail": "Bookmark not found"})
				return
			}
			s.writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "bookmark": bookmark})
		case http.MethodDelete:
			removed, err := deleteBookmark(s.settings, eventID, bookmarkID)
			if err != nil {
				s.writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
				return
			}
			if !removed {
				s.writeJSON(w, http.StatusNotFound, map[string]string{"detail": "Bookmark not found"})
				return
			}
			s.writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "message": fmt.Sprintf("Deleted bookmark %s", bookmarkID)})
		default:
			s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		}
		return
	}

	s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "invalid URL"})
}

// apiEventEnvelope is a lightweight peek-only struct used by
// handleAPIEvents to dispatch on the "type" discriminator. We decode
// twice — once into the envelope to pick the branch, then into the
// branch-specific DTO so each handler sees a typed request.
type apiEventEnvelope struct {
	Type string `json:"type"`
}

func (s *Server) handleAPIEvents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleEvents(w, r)
	case http.MethodPost:
		body, err := readBody(r)
		if err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": err.Error()})
			return
		}
		var env apiEventEnvelope
		if err := json.Unmarshal(body, &env); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": err.Error()})
			return
		}
		if env.Type == "external" {
			var req ExternalSourceRequest
			if err := json.Unmarshal(body, &req); err != nil {
				s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": err.Error()})
				return
			}
			// The /api/events branch historically sent "fps" instead
			// of "framerate"; preserve that legacy by checking the
			// raw body when Framerate is unset.
			if req.Framerate == 0 {
				var legacy struct {
					FPS int `json:"fps"`
				}
				_ = json.Unmarshal(body, &legacy)
				req.Framerate = legacy.FPS
			}
			eventID, err := s.createExternalSource(req)
			if err != nil {
				s.writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
				return
			}
			s.writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "event_id": eventID, "message": fmt.Sprintf("Started processing external source: %s", req.Name), "status": "processing"})
			return
		}
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "unsupported event type"})
	default:
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
	}
}
