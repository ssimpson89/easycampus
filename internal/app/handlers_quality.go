// handlers_quality.go exposes the broadcast preset catalog over HTTP
// so the encoder UI can render a tier-grouped quality picker rather
// than the legacy low/standard/high/ultra dropdown.
//
// Route:  GET /api/quality   (alias: GET /quality)
// Response:
//
//	{
//	  "tiers": [
//	    {
//	      "tier": "consumer",
//	      "label": "Consumer",
//	      "presets": [
//	        {
//	          "id":   "consumer-1080p30",
//	          "label": "Consumer 1080p30",
//	          "width": 1920, "height": 1080,
//	          "fps": "30", "video_bitrate_kbps": 6000,
//	          "min_upload_kbps": 8500,
//	          "hw_encoders": ["h264_videotoolbox", "h264_nvenc", ...]
//	        }, ...
//	      ]
//	    }, ...
//	  ]
//	}
//
// The shape is intentionally a subset of internal/quality.Preset —
// the UI doesn't need HDR / x264-params / etc., and we don't want to
// leak internal knobs through an operator-facing endpoint.
package app

import (
	"net/http"

	ocquality "easycampus/internal/quality"
)

// handleQualityCatalog serves the tier-grouped preset list. Method
// must be GET. Always returns 200 with the catalog — even if empty,
// the UI can fall back to its baked-in defaults.
func (s *Server) handleQualityCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"tiers": ocquality.Summarise(),
	})
}
