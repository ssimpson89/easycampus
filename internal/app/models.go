package app

import (
	"time"

	"easycampus/internal/version"
)

type StreamConfig struct {
	EventName         string `json:"event_name"`
	VideoBitrate      string `json:"video_bitrate"`
	VideoFramerate    int    `json:"video_framerate"`
	VideoDeviceIndex  string `json:"video_device_index"`
	VideoDeviceName   string `json:"video_device_name"`
	VideoDeviceFormat string `json:"video_device_format"`
	VideoDeviceType   string `json:"video_device_type"`
	AudioDeviceIndex  string `json:"audio_device_index"`
	AudioDeviceName   string `json:"audio_device_name"`
	// QualityPreset is the stable identifier of an internal/quality
	// Preset to use for this stream (e.g. "broadcast-1080p60").
	// When set, the supervisor uses the preset's resolution/bitrate
	// envelope instead of VideoBitrate / VideoFramerate. Empty falls
	// back to the per-stream legacy fields (which in turn fall back
	// to AppSettings.QualityPreset and then to DefaultBitrate /
	// DefaultFramerate).
	QualityPreset string `json:"quality_preset,omitempty"`
}

// ExternalSourceRequest is the JSON body for POST /external-sources and
// the {"type":"external", ...} branch of POST /api/events.
//
// Field aliases (kept for one release per ARCHITECTURE_REVIEW.md item 9):
//   - Type identifies the protocol ("rtsp", "srt", "hls") for callers
//     that want explicit selection. Empty falls through to URL-based
//     auto-detection inside the supervisor.
//   - Bitrate/Framerate are easycampus-go-specific encode parameters
//     that don't appear in the architect's typed-DTO sketch but are
//     load-bearing for the existing UI; they remain as optional fields.
type ExternalSourceRequest struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	Type      string `json:"type,omitempty"`
	Bitrate   int    `json:"bitrate,omitempty"`
	Framerate int    `json:"framerate,omitempty"`
}

// StreamStartRequest is the typed JSON body for POST /stream/start.
//
// Field aliases (kept for one release per ARCHITECTURE_REVIEW.md item 9):
//   - "event_name" is canonical; "name" is the python-port legacy.
//   - "video_framerate" is canonical; "framerate" is legacy.
//   - "video_bitrate" is canonical (string like "4000k"); "bitrate" is
//     legacy (integer kbps). When both are set the canonical wins.
//   - "video_device_index" is canonical; "input_id" is legacy.
//
// When the canonical field is empty the legacy alias is used. Both are
// decoded by encoding/json — no map[string]interface{} dredging.
type StreamStartRequest struct {
	EventName         string `json:"event_name"`
	VideoFramerate    int    `json:"video_framerate"`
	VideoBitrate      string `json:"video_bitrate"`
	BitrateKbps       int    `json:"bitrate"`
	Name              string `json:"name"`
	Framerate         int    `json:"framerate"`
	VideoDeviceIndex  string `json:"video_device_index"`
	InputID           string `json:"input_id"`
	VideoDeviceFormat string `json:"video_device_format"`
	VideoDeviceType   string `json:"video_device_type"`
	QualityPreset     string `json:"quality_preset"`
}

// BookmarkCreateRequest is the typed JSON body for
// POST /events/{id}/bookmarks. The legacy handler accepted "time_pos" as
// the seconds-into-stream field; we keep that JSON tag for compatibility
// while exposing it as TimeSecs in Go.
type BookmarkCreateRequest struct {
	// ID is optional. When set it must pass validID and be unique within
	// the event; otherwise the server allocates the next "bm_NNN".
	ID       string  `json:"id,omitempty"`
	Name     string  `json:"name"`
	TimeSecs float64 `json:"time_pos"`
	Frame    int64   `json:"frame"`
	FPS      float64 `json:"fps"`
	Notes    string  `json:"notes,omitempty"`
}

// BookmarkUpdateRequest is the typed JSON body for
// PUT /events/{id}/bookmarks/{bm}. All fields are optional; only
// non-nil pointers overwrite the stored bookmark.
type BookmarkUpdateRequest struct {
	Name  *string `json:"name,omitempty"`
	Color *string `json:"color,omitempty"`
	Notes *string `json:"notes,omitempty"`
}

// OutputStartRequest is the architect's forward-looking DTO for
// POST /output/start. Pointer-typed ScreenIndex distinguishes "user
// picked screen 0" from "user didn't pick" — see ARCHITECTURE_REVIEW.md
// item on models.go. The existing UI continues to use
// PlayerStartRequest (url, fullscreen, decklink_format); this DTO is
// kept available for the upcoming event-driven decoder flow.
//
// PlaybackProfile is the stable identifier of an internal/playback
// profile (e.g. "projector-30") that drives the mpv --video-sync /
// --interpolation / --display-fps-override / --hwdec / --vo flags.
// Empty falls back to AppSettings.PlaybackProfile and then to
// playback.Default() ("auto") so existing clients see no behavior
// change.
type OutputStartRequest struct {
	EventID         string `json:"event_id"`
	ScreenIndex     *int   `json:"screen_index,omitempty"`
	Decklink        bool   `json:"decklink,omitempty"`
	Locked          bool   `json:"locked,omitempty"`
	PlaybackProfile string `json:"playback_profile,omitempty"`
}

// OutputUpdateRequest is the architect's forward-looking DTO for
// POST /output/update. All fields are pointer-typed so the handler can
// distinguish "set mute to false" from "don't touch mute". The current
// UI POSTs PlayerStartRequest (url) to /output/update; this DTO is
// available for future structured player control.
//
// PlaybackProfile lets the operator change the playback profile
// mid-stream. Empty leaves the active profile untouched.
type OutputUpdateRequest struct {
	Position        *float64 `json:"position,omitempty"`
	Mute            *bool    `json:"mute,omitempty"`
	Paused          *bool    `json:"paused,omitempty"`
	PlaybackRate    *float64 `json:"playback_rate,omitempty"`
	PlaybackProfile string   `json:"playback_profile,omitempty"`
}

type DeviceInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Format     string `json:"format"`
	DeviceType string `json:"device_type"`
}

type StreamStatus struct {
	IsRunning     bool      `json:"is_running"`
	EventID       string    `json:"event_id,omitempty"`
	UptimeSeconds float64   `json:"uptime_seconds"`
	StartedAt     time.Time `json:"started_at,omitempty"`
	FFMpegPID     int       `json:"ffmpeg_pid,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	// QualityPreset is the ID of the preset the encoder is running
	// (e.g. "broadcast-1080p60" or a synthesized "live-1080p30").
	QualityPreset string `json:"quality_preset,omitempty"`
}

// MPVStatus is the snapshot returned by MPVService.Status. Kept tiny on
// purpose — handlers compose their own response objects from this plus
// per-property reads.
type MPVStatus struct {
	IsRunning  bool   `json:"is_running"`
	CurrentURL string `json:"current_url,omitempty"`
	SocketPath string `json:"socket_path,omitempty"`
}

type EventInfo struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Status          string     `json:"status"`
	Type            string     `json:"type"`
	CreatedAt       time.Time  `json:"created_at"`
	StoppedAt       *time.Time `json:"stopped_at,omitempty"`
	DurationSeconds int        `json:"duration_seconds"`
	// FPS is the recording's frame rate (from the quality preset at
	// stream start). Zero/omitted for events that predate the field or
	// whose rate isn't known (uploads, external sources).
	FPS       float64 `json:"fps,omitempty"`
	SizeBytes int64   `json:"size_bytes"`
	StreamURL string  `json:"stream_url,omitempty"`
	LogURL    string  `json:"log_url,omitempty"`
}

type HealthStatus struct {
	OK                bool         `json:"ok"`
	Version           version.Info `json:"version"`
	FFMpegAvailable   bool         `json:"ffmpeg_available"`
	InputFormat       string       `json:"input_format"`
	InputDevice       string       `json:"input_device"`
	EventsDir         string       `json:"events_dir"`
	EventsDirWritable bool         `json:"events_dir_writable"`
	Stream            StreamStatus `json:"stream"`
}

type ForceStopRequest struct {
	PID     int    `json:"pid"`
	EventID string `json:"event_id"`
}

type ForceStopResult struct {
	Killed    bool         `json:"killed"`
	KilledPID int          `json:"killed_pid,omitempty"`
	Message   string       `json:"message"`
	Stream    StreamStatus `json:"stream"`
}

type PlayerStartRequest struct {
	URL string `json:"url"`
	// ScreenIndex is the 0-based display index. Pointer so an explicit
	// 0 (first display) is distinguishable from "not provided".
	ScreenIndex    *int   `json:"screen_index,omitempty"`
	OutputMode     string `json:"output_mode"`
	Fullscreen     bool   `json:"fullscreen"`
	DecklinkFormat string `json:"decklink_format"`
	// PlaybackProfile is the stable identifier of an internal/playback
	// profile (e.g. "projector-30"). Empty falls back to
	// AppSettings.PlaybackProfile, then playback.Default(). The
	// profile's OutputMode wins over the request-level OutputMode for
	// new clients; legacy clients that send only OutputMode continue
	// to work via the auto-profile fallback path in handleOutputStart.
	PlaybackProfile string `json:"playback_profile,omitempty"`
}

type PlayerControlRequest struct {
	Command   string  `json:"command"`
	Timestamp float64 `json:"timestamp"`
}

type MuteRequest struct {
	Muted bool `json:"muted"`
}

type CacheStartRequest struct {
	EventID   string `json:"event_id"`
	RemoteURL string `json:"remote_url"`
}

type CacheStatus struct {
	Cached    bool  `json:"cached"`
	Active    bool  `json:"active"`
	Segments  int   `json:"segments"`
	SizeBytes int64 `json:"size_bytes"`
}

type CachedEventInfo struct {
	EventID    string `json:"event_id"`
	Segments   int    `json:"segments"`
	SizeBytes  int64  `json:"size_bytes"`
	IsComplete bool   `json:"is_complete"`
}

type Bookmark struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Frame     int64   `json:"frame"`
	FPS       float64 `json:"fps"`
	TimePos   float64 `json:"time_pos"`
	CreatedAt string  `json:"created_at"`
	Source    string  `json:"source"`
	Color     string  `json:"color"`
	Notes     string  `json:"notes,omitempty"`
}
