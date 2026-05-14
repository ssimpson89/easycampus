// ffmpeg_service.go is the adapter between the HTTP handlers and
// internal/ffmpeg. It exposes the legacy FFmpegService surface
// (StartStream / StopStream / GetStatus / ForceKillPID /
// ListInputDevices / FFMpegAvailable) while routing every actual
// ffmpeg invocation through internal/ffmpeg.Supervisor (live) or
// internal/ffmpeg.Job (uploads / external sources).
//
// NO exec.Command("ffmpeg", ...) lives in this package — all ffmpeg
// subprocesses go through internal/ffmpeg.
package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	ocdevices "easycampus/internal/devices"
	ocffmpeg "easycampus/internal/ffmpeg"
	ocquality "easycampus/internal/quality"
)

// FFmpegService is the legacy facade. The handlers expect this type by
// name; we keep the name so handler call sites don't churn. Underneath
// it delegates to a Supervisor (live encode) and a device Scanner.
type FFmpegService struct {
	settings AppSettings
	logger   *slog.Logger

	scanner *ocdevices.Scanner

	mu             sync.Mutex
	sup            *ocffmpeg.Supervisor
	adaptive       *ocffmpeg.AdaptiveController
	currentEventID string
	startedAtWall  time.Time
	lastError      string
	cancelKey      string // unused currently — placeholder for stream-key redaction wiring
}

// NewFFmpegService constructs the shim. Wires up the supervisor with
// easycampus-go's stall/restart defaults and points the pid file at
// <BaseDir>/<NodeType>/ffmpeg.pid so an orphan from a crash gets reaped
// at next launch.
//
// The internal supervisor still takes a stdlib *log.Logger — we bridge
// from slog via slogWriter so every line still ends up in the
// structured journal with the right component tag.
//
// When settings.AdaptiveBitrate is set, an AdaptiveController is also
// constructed and started — it watches the supervisor's progress and
// steps the preset down/up within the chosen tier. The controller is
// gated by the supervisor's OnStreamStart/OnStreamStop callbacks so it
// only acts while a live encode is active.
func NewFFmpegService(settings AppSettings, logger *slog.Logger) *FFmpegService {
	if logger == nil {
		logger = slog.Default()
	}
	supLogger := log.New(slogWriter(logger.With("subsystem", "ffmpeg.sup")), "", 0)
	pidPath := filepath.Join(settings.BaseDir, settings.NodeType, "ffmpeg.pid")
	if settings.BaseDir == "" {
		pidPath = ""
	}
	sup := ocffmpeg.NewSupervisor(supLogger, ocffmpeg.SupervisorConfig{
		RestartInitialDelay: time.Second,
		RestartMaxDelay:     30 * time.Second,
		StableAfter:         2 * time.Minute,
		ProgressStallAfter:  20 * time.Second,
		MaxRestarts:         20,
		PidFilePath:         pidPath,
	})
	svc := &FFmpegService{
		settings: settings,
		logger:   logger,
		scanner:  ocdevices.NewScanner("ffmpeg"),
		sup:      sup,
	}
	if settings.AdaptiveBitrate {
		cfg := ocffmpeg.DefaultAdaptiveConfig()
		cfg.Enabled = true
		adaptiveLogger := log.New(slogWriter(logger.With("subsystem", "ffmpeg.adaptive")), "", 0)
		svc.adaptive = ocffmpeg.NewAdaptiveController(sup, qualityPresetTier{}, cfg, adaptiveLogger)
		// Hook supervisor restart callbacks so the controller can
		// detect restart storms via OnRestart().
		sup.SetOnRestart(svc.adaptive.OnRestart)
		svc.adaptive.Start()
	}
	return svc
}

// AdaptiveController returns the live adaptive controller, or nil
// when AdaptiveBitrate is disabled. Exposed primarily for graceful
// shutdown — main.go calls Stop() on shutdown to drain the run
// goroutine cleanly.
func (f *FFmpegService) AdaptiveController() *ocffmpeg.AdaptiveController {
	return f.adaptive
}

// qualityPresetTier bridges quality.StepLower/StepHigher into the
// ffmpeg.PresetTier interface the AdaptiveController expects.
//
// Lives in the root package because the import chain is
// root -> ffmpeg + root -> quality, so root is the only place that
// can connect the two without cycles.
type qualityPresetTier struct{}

func (qualityPresetTier) Lower(currentID string) (ocffmpeg.QualityPreset, bool) {
	p, ok := ocquality.StepLower(currentID)
	if !ok {
		return nil, false
	}
	return ocquality.AsFFmpegPreset(p), true
}

func (qualityPresetTier) Higher(currentID, targetID string) (ocffmpeg.QualityPreset, bool) {
	p, ok := ocquality.StepHigher(currentID, targetID)
	if !ok {
		return nil, false
	}
	return ocquality.AsFFmpegPreset(p), true
}

// IsRunning reports whether the live encoder is active.
func (f *FFmpegService) IsRunning() bool {
	return f.sup.IsRunning()
}

// RequireFFmpeg returns an error if the ffmpeg binary isn't on PATH.
func (f *FFmpegService) RequireFFmpeg() error {
	if !commandExists("ffmpeg") {
		return errors.New("ffmpeg not found in PATH")
	}
	return nil
}

// FFMpegAvailable reports whether the ffmpeg binary is on PATH.
//
// ctx is accepted for symmetry with the rest of the service surface and
// for future use if probing becomes ctx-aware (e.g. exec.CommandContext
// against `ffmpeg -version`). The current LookPath is sync and
// stat-bound — fast enough that ctx cancellation is moot — so we just
// honour an already-cancelled ctx by returning false rather than
// reporting stale results.
func (f *FFmpegService) FFMpegAvailable(ctx context.Context) bool {
	if err := ctx.Err(); err != nil {
		return false
	}
	return commandExists("ffmpeg")
}

// CurrentPID returns the live ffmpeg PID, or 0 when not running.
func (f *FFmpegService) CurrentPID() int {
	return f.sup.Status().PID
}

// CurrentEventID returns the event ID associated with the running stream,
// or empty when nothing is live. Exposed because handlers_stream consults
// it before calling StopStream so it can mark the event "completed".
func (f *FFmpegService) CurrentEventID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.currentEventID
}

// StartStream is the legacy entry point used by handlers_stream. It maps
// the on-wire StreamConfig (per-call) plus AppSettings (per-deploy) onto
// the internal Config and hands off to the supervisor.
//
// ctx scopes the *startup* work only (validation, mkdir, supervisor
// preflight). Once sup.Start returns, the supervisor owns its own
// lifetime ctx internally — a client disconnecting cancels ctx, but the
// already-launched supervisor keeps running until StopStream(). This is
// intentional: the supervisor is a hours-long process and must outlive
// the HTTP request that kicked it off.
func (f *FFmpegService) StartStream(ctx context.Context, eventID string, config StreamConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.IsRunning() {
		return errors.New("stream already running")
	}
	if config.VideoFramerate <= 0 {
		return errors.New("framerate must be positive")
	}
	if err := f.RequireFFmpeg(); err != nil {
		return err
	}

	eventDir := filepath.Join(f.settings.EventsDir, eventID)
	if err := os.MkdirAll(eventDir, 0755); err != nil {
		return err
	}
	// Recheck ctx after potentially-slow disk work so a disconnect that
	// landed during mkdir doesn't push us into the supervisor.
	if err := ctx.Err(); err != nil {
		return err
	}

	preset := presetFromStreamConfig(config, f.settings)
	cfg := ocffmpeg.Config{
		Binary:            "ffmpeg",
		Mode:              ocffmpeg.ModeLiveHLS,
		Input:             inputFromStreamConfig(config, f.settings),
		Preset:            preset,
		Encoder:           ocffmpeg.EncoderAuto, // supervisor will pick the best available
		HWAccel:           ocffmpeg.HWAccelAuto,
		OutputDir:         eventDir,
		HLSSegmentSeconds: hlsSegSec(f.settings),
		HLSDeleteOld:      f.settings.HLSDeleteOld,
		HLSListSize:       f.settings.HLSListSize,
		LogLevel:          mapLogLevel(f.settings.LogLevel),
		DisableAudio:      f.settings.DisableAudio,
	}

	// NOTE: sup.Start spawns a long-running goroutine that owns its own
	// context.WithCancel(context.Background()). We deliberately do NOT
	// pass the request ctx down — a request cancellation must not kill
	// a multi-hour live encode. StopStream(ctx) is the only path that
	// tears the supervisor down.
	if err := f.sup.Start(cfg, eventID); err != nil {
		f.mu.Lock()
		f.lastError = err.Error()
		f.mu.Unlock()
		return err
	}

	f.mu.Lock()
	f.currentEventID = eventID
	f.startedAtWall = time.Now()
	f.lastError = ""
	adaptive := f.adaptive
	f.mu.Unlock()
	if adaptive != nil {
		adaptive.OnStreamStart(preset.ID())
	}
	return nil
}

// StopStream sends SIGTERM to the live ffmpeg and waits for it to exit.
//
// ctx is accepted for API symmetry; the underlying sup.Stop() waits for
// the supervisor's run goroutine which is bounded by the supervisor's
// own grace/kill timer rather than ctx. Cancelling ctx is informational
// only — we record it via the logger but do not abort the shutdown
// midway (an orphan ffmpeg is worse than a slow shutdown).
func (f *FFmpegService) StopStream(ctx context.Context) error {
	_ = ctx
	f.sup.Stop()
	f.mu.Lock()
	f.currentEventID = ""
	f.startedAtWall = time.Time{}
	adaptive := f.adaptive
	f.mu.Unlock()
	if adaptive != nil {
		adaptive.OnStreamStop()
	}
	return nil
}

// ForceKillPID terminates a specific PID. If it's the currently-supervised
// stream, route through StopStream so the supervisor cleans up correctly.
// Otherwise terminate the bare PID (used for upload-job cleanup paths).
//
// ctx scopes the supervisor-stop path only — same rationale as StopStream.
func (f *FFmpegService) ForceKillPID(ctx context.Context, pid int) bool {
	if pid <= 0 {
		return false
	}
	if f.sup.Status().PID == pid {
		_ = f.StopStream(ctx)
		return true
	}
	return terminateProcess(pid) == nil
}

// GetStatus returns the legacy StreamStatus shape that handlers_system /
// handlers_stream expect. We compose it from the supervisor's snapshot.
//
// ctx is accepted for API symmetry. The current implementation only
// reads in-memory state (no IO), so ctx cancellation is a no-op; the
// parameter is plumbed through so future health probes / disk stats can
// honour it.
func (f *FFmpegService) GetStatus(ctx context.Context) StreamStatus {
	_ = ctx
	st := f.sup.Status()
	f.mu.Lock()
	startedAtWall := f.startedAtWall
	lastError := f.lastError
	f.mu.Unlock()
	if lastError == "" {
		lastError = st.LastError
	}
	uptime := 0.0
	if !st.StartedAt.IsZero() {
		uptime = time.Since(st.StartedAt).Seconds()
	}
	return StreamStatus{
		IsRunning:     st.State == ocffmpeg.StateRunning || st.State == ocffmpeg.StateDegraded || st.State == ocffmpeg.StateStarting,
		EventID:       st.EventID,
		UptimeSeconds: uptime,
		StartedAt:     startedAtWall,
		FFMpegPID:     st.PID,
		LastError:     lastError,
	}
}

// ListInputDevices returns the legacy DeviceInfo shape that handlers_system
// expects. We translate from internal/devices.Device categories.
//
// ctx bounds the device scan. The underlying Scanner.Scan() may shell
// out to ffmpeg/v4l2-ctl which can hang on a wedged USB device — an
// already-cancelled ctx short-circuits the call before we issue any
// shell-out. The internal scanner doesn't yet expose a Context-aware
// API; the scan itself runs sync but is cheap enough that the pre-check
// is adequate for the current call frequency.
func (f *FFmpegService) ListInputDevices(ctx context.Context) ([]DeviceInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := f.RequireFFmpeg(); err != nil {
		return nil, err
	}
	list := f.scanner.Scan()
	out := make([]DeviceInfo, 0, len(list.Video))
	for _, d := range list.Video {
		out = append(out, deviceInfoFromDevice(d))
	}
	return out, nil
}

func deviceInfoFromDevice(d ocdevices.Device) DeviceInfo {
	return DeviceInfo{
		ID:         d.Index,
		Name:       d.Name,
		Format:     d.Backend,
		DeviceType: legacyDeviceType(d.Type, d.Backend),
	}
}

// legacyDeviceType maps the richer internal categories back to the
// 4-bucket strings the existing UI templates expect.
func legacyDeviceType(t ocdevices.DeviceType, backend string) string {
	switch t {
	case ocdevices.TypeSDI:
		return "sdi"
	case ocdevices.TypeNDI:
		return "ndi"
	case ocdevices.TypeCaptureCard:
		return "hdmi_capture"
	case ocdevices.TypeScreen:
		return "screen"
	}
	// Default to webcam — the legacy UI uses this for everything camera-
	// shaped including capture cards that fall through classification.
	_ = backend
	return "webcam"
}

// -----------------------------------------------------------------------------
// Config / input translation
// -----------------------------------------------------------------------------

// presetFromStreamConfig builds an internal QualityPreset from the
// on-wire StreamConfig + AppSettings defaults.
//
// Resolution order:
//
//  1. StreamConfig.QualityPreset (per-request override) — look up by
//     ID in the internal/quality catalog. If found, wrap as an
//     ocquality.FFmpegPreset so the supervisor sees color VUI / CBR
//     envelope / broadcast-safe limiter through the optional
//     interfaces (ColorPresetAware, RateControlAware,
//     BroadcastSafeAware).
//  2. AppSettings.QualityPreset (per-deploy default).
//  3. Legacy bitrate/framerate composition into a SimplePreset.
//
// We deliberately fall through on unknown IDs rather than erroring —
// a stale config shouldn't prevent a stream from starting; the
// supervisor's logs surface the chosen preset so the operator notices.
func presetFromStreamConfig(config StreamConfig, settings AppSettings) ocffmpeg.QualityPreset {
	if id := strings.TrimSpace(config.QualityPreset); id != "" {
		if p, ok := ocquality.ResolveFFmpegPreset(id); ok {
			return p
		}
	}
	if id := strings.TrimSpace(settings.QualityPreset); id != "" {
		if p, ok := ocquality.ResolveFFmpegPreset(id); ok {
			return p
		}
	}
	fps := config.VideoFramerate
	if fps <= 0 {
		fps = settings.DefaultFramerate
	}
	if fps <= 0 {
		fps = 30
	}
	bitrate := config.VideoBitrate
	if bitrate == "" {
		bitrate = fmt.Sprintf("%dk", settings.DefaultBitrate/1000)
	}
	w, h := 1920, 1080
	if fps >= 50 {
		// Capture-side defaults are 1280x720 for AVFoundation 60 fps; reflect
		// that here so the encoder output matches what the device delivers.
		w, h = 1280, 720
	}
	segSec := hlsSegSec(settings)
	gop := fps * segSec
	return ocffmpeg.SimplePreset{
		IDValue:       fmt.Sprintf("live-%dp%d", h, fps),
		LabelValue:    fmt.Sprintf("%dp%d", h, fps),
		W:             w,
		H:             h,
		FPSInt:        fps,
		GOPFrames:     gop,
		VBitrate:      bitrate,
		ABitrate:      "128k",
		PixFmt:        "yuv420p",
		ProfileValue:  "high",
		LevelValue:    "4.1",
		EncoderPreset: "veryfast",
		ZeroLatency:   true,
		BFrames:       false,
	}
}

// inputFromStreamConfig translates the on-wire device fields onto the
// internal Input. Falls back to platform/Settings defaults when a field
// is unset.
func inputFromStreamConfig(config StreamConfig, settings AppSettings) ocffmpeg.Input {
	format := config.VideoDeviceFormat
	if format == "" {
		format = settings.InputFormat
	}
	videoIdx := config.VideoDeviceIndex
	if videoIdx == "" {
		videoIdx = settings.InputDevice
	}
	audioIdx := config.AudioDeviceIndex
	switch format {
	case "avfoundation":
		if videoIdx == "" {
			videoIdx = "0"
		}
		if audioIdx == "" {
			audioIdx = "0"
		}
		size := "1280x720"
		if config.VideoFramerate < 50 {
			size = "1280x720"
		}
		return ocffmpeg.Input{
			Kind:        ocffmpeg.InputAVFoundation,
			VideoDevice: videoIdx,
			AudioDevice: audioIdx,
			Framerate:   config.VideoFramerate,
			VideoSize:   size,
		}
	case "v4l2":
		return ocffmpeg.Input{Kind: ocffmpeg.InputV4L2, VideoDevice: videoIdx, VideoSize: "1280x720"}
	case "dshow":
		return ocffmpeg.Input{Kind: ocffmpeg.InputDShow, VideoDevice: videoIdx, VideoSize: "1280x720"}
	case "decklink":
		return ocffmpeg.Input{Kind: ocffmpeg.InputDeckLink, VideoDevice: videoIdx}
	case "libndi_newtek":
		return ocffmpeg.Input{Kind: ocffmpeg.InputNDI, VideoDevice: videoIdx}
	default:
		// Best-effort fallback: treat unknown formats as the platform default.
		switch runtime.GOOS {
		case "darwin":
			return ocffmpeg.Input{Kind: ocffmpeg.InputAVFoundation, VideoDevice: "0", AudioDevice: "0", Framerate: config.VideoFramerate, VideoSize: "1280x720"}
		case "windows":
			return ocffmpeg.Input{Kind: ocffmpeg.InputDShow, VideoDevice: videoIdx, VideoSize: "1280x720"}
		default:
			return ocffmpeg.Input{Kind: ocffmpeg.InputV4L2, VideoDevice: videoIdx, VideoSize: "1280x720"}
		}
	}
}

func hlsSegSec(settings AppSettings) int {
	if settings.HLSSegmentDuration > 0 {
		return settings.HLSSegmentDuration
	}
	return 2
}

// mapLogLevel turns config.LogLevel ("INFO"/"DEBUG"/"WARNING") into an
// FFmpeg loglevel keyword. We default to "error" (matching the new
// internal/ffmpeg default) because the supervisor parses progress
// separately and we don't want stderr noise drowning the log.
func mapLogLevel(level string) string {
	switch strings.ToLower(level) {
	case "debug":
		return "debug"
	case "info":
		return "info"
	case "warn", "warning":
		return "warning"
	case "error":
		return "error"
	case "":
		return "error"
	default:
		return "error"
	}
}
