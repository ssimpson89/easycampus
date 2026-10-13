// Package quality — ffmpeg adapter.
//
// This file is the glue between the broadcast preset matrix in quality.go
// and the supervisor-facing QualityPreset interface declared in
// internal/ffmpeg/config.go. The two packages were built in parallel
// (preset table vs supervisor / args builder) and meet here so neither
// has to import the other's heavyweight types directly.
//
// Design contract:
//
//   - quality.Preset (a richly-typed struct) is wrapped in a
//     FFmpegPreset value type whose methods satisfy the narrow
//     ffmpeg.QualityPreset interface. The wrapper carries no
//     state of its own; every accessor is a pure projection of the
//     underlying Preset struct.
//
//   - Optional richer interfaces (ColorPresetAware, RateControlAware,
//     BroadcastSafeAware) let the supervisor opt-in to broadcast-tier
//     features via runtime type assertions. The supervisor's
//     encoderArgs() path checks for ColorPresetAware first and emits
//     the Rec.709 / BT.2020 VUI metadata when present.
//
//   - The integration is one-directional: this package depends on
//     nothing in internal/ffmpeg. The ffmpeg package keeps a narrow
//     QualityPreset interface and runtime-asserts to richer
//     capabilities — that keeps the package import graph acyclic
//     and lets either side evolve without breaking the other.
package quality

import (
	"fmt"
	"strconv"
)

// FFmpegPreset is the bridge type. It wraps a quality.Preset and
// exposes the narrow ffmpeg.QualityPreset surface (ID, Resolution,
// Width/Height, FPS/FPSExpr, GOP, VideoBitrate, AudioBitrate, etc.)
// as well as richer optional interfaces (ColorPresetAware,
// RateControlAware, BroadcastSafeAware) that the supervisor's args
// builder can type-assert to without widening the core interface.
//
// Construct via AsFFmpegPreset(p) or ResolveFFmpegPreset(id). The
// zero value carries an empty preset and is not safe to use as a
// QualityPreset — Validate() in the supervisor catches that.
type FFmpegPreset struct {
	Preset Preset
}

// AsFFmpegPreset wraps a quality.Preset so it can be passed to
// internal/ffmpeg as a QualityPreset.
func AsFFmpegPreset(p Preset) FFmpegPreset { return FFmpegPreset{Preset: p} }

// ResolveFFmpegPreset looks up a preset by ID and returns it wrapped
// for the ffmpeg package. Returns (zero, false) when the ID is
// unknown; callers should fall back to the supervisor's
// DefaultPreset in that case so the stream still starts.
func ResolveFFmpegPreset(id string) (FFmpegPreset, bool) {
	p, ok := ByID(id)
	if !ok {
		return FFmpegPreset{}, false
	}
	return FFmpegPreset{Preset: p}, true
}

// -----------------------------------------------------------------------------
// QualityPreset interface (ffmpeg.QualityPreset)
// -----------------------------------------------------------------------------

// ID returns the stable preset identifier ("broadcast-1080p60").
func (f FFmpegPreset) ID() string { return f.Preset.ID }

// Label returns the human-facing name.
func (f FFmpegPreset) Label() string { return f.Preset.Label }

// Resolution returns "WxH".
func (f FFmpegPreset) Resolution() string { return f.Preset.Resolution() }

// Width returns the horizontal pixel count.
func (f FFmpegPreset) Width() int { return f.Preset.Width }

// Height returns the vertical pixel count.
func (f FFmpegPreset) Height() int { return f.Preset.Height }

// FPS returns the rounded-integer frame rate. Use FPSExpr() to emit
// the exact rational form on the FFmpeg command line.
func (f FFmpegPreset) FPS() int { return f.Preset.FPS.Int() }

// FPSExpr returns the wire form for `-r`. Cinema 23.976 presets
// return "24000/1001" so the muxer keeps exact cadence over long
// streams; integer rates return a plain decimal.
func (f FFmpegPreset) FPSExpr() string { return f.Preset.FPS.Expr() }

// GOP returns the keyframe interval in frames. Sized for 2-second
// HLS segments by Preset.GOP().
func (f FFmpegPreset) GOP() int { return f.Preset.GOP() }

// VideoBitrate returns the target bitrate in ffmpeg's "Nk" form
// (e.g. "8000k"). The supervisor uses this for -b:v.
func (f FFmpegPreset) VideoBitrate() string { return f.Preset.VideoBitrateStr() }

// AudioBitrate returns the audio target in "Nk" form.
func (f FFmpegPreset) AudioBitrate() string { return f.Preset.AudioBitrateStr() }

// PixelFormat returns the pixel format passed to `-pix_fmt`.
// yuv420p for SDR HLS, yuv420p10le on broadcast 2160p / cinema HEVC.
func (f FFmpegPreset) PixelFormat() string {
	if f.Preset.PixFmt == "" {
		return "yuv420p"
	}
	return f.Preset.PixFmt
}

// Profile returns the codec profile name. The preset table does not
// declare per-row profile values; we infer from PixFmt (10-bit implies
// HEVC "main10", 8-bit yuv420p uses H.264 "high"). The ffmpeg args
// builder uses PrefersHEVC() to pick an HEVC encoder for main10 presets
// and maps the profile back to "high" if it has to fall back to H.264.
func (f FFmpegPreset) Profile() string {
	if f.Preset.PixFmt == "yuv420p10le" {
		return "main10"
	}
	return "high"
}

// Level returns the H.264 SPS level (e.g. "4.1", "4.2", "5.1").
func (f FFmpegPreset) Level() string { return f.Preset.Level() }

// VideoEncoderPreset returns the per-encoder speed preset name.
// Empty defers to the encoder's default — the supervisor maps "" to
// "veryfast" / "p4" / etc. for the specific encoder.
func (f FFmpegPreset) VideoEncoderPreset() string {
	// quality.Preset does not declare per-encoder speed presets in
	// the table. ffmpeg.Config applies per-encoder defaults when this
	// returns "" (veryfast for x264/x265/QSV, p4 for NVENC) and
	// translates x264-style names for NVENC/QSV.
	return ""
}

// Tune returns the `-tune` value. Broadcast/Cinema both benefit
// from B-frames, so the table does not emit zerolatency for any
// preset. The supervisor's live path overrides this to zerolatency
// when WantsZeroLatency() is true.
func (f FFmpegPreset) Tune() string { return "" }

// WantsZeroLatency reports whether the live path should force
// `-tune zerolatency` for this preset. Only consumer tiers benefit
// from this — broadcast/cinema lose too much quality at the same
// bitrate when B-frames are disabled.
func (f FFmpegPreset) WantsZeroLatency() bool {
	return f.Preset.Tier == TierConsumer
}

// WantsBFrames reports whether the encoder should emit B-frames.
// Broadcast + cinema both want them on for compression efficiency.
// Consumer tiers leave them off so the live latency stays minimal.
func (f FFmpegPreset) WantsBFrames() bool {
	switch f.Preset.Tier {
	case TierBroadcast, TierCinema, TierProsumer:
		return true
	}
	return false
}

// -----------------------------------------------------------------------------
// Optional richer interfaces (type-asserted by internal/ffmpeg)
// -----------------------------------------------------------------------------

// ColorPresetAware exposes broadcast-grade color hints. The ffmpeg
// package type-asserts to this interface and (a) tags frames with a
// `setparams` filter in the -vf chain and (b) emits `-color_primaries /
// -color_trc / -colorspace / -color_range` before -c:v.
//
// The narrow QualityPreset interface deliberately omits these — most
// callers (DefaultPreset, SimplePreset) don't carry color metadata
// and forcing them to return "" or "unspecified" would invite drift
// between the consumer simple-bucket path and the broadcast tier path.
type ColorPresetAware interface {
	ColorPrimaries() string // "bt709" / "bt2020"
	ColorTransfer() string  // "bt709" / "smpte2084" / "arib-std-b67"
	ColorSpace() string     // "bt709" / "bt2020nc"
	ColorRange() string     // "tv" / "pc"
}

// RateControlAware exposes the rate-control mode (CBR/VBR/CRF/CQP)
// and the VBV envelope. The ffmpeg package uses this to wire
// `nal-hrd=cbr filler=1` and 1-second VBV (bufsize == maxrate) on
// the broadcast x264 path. Falls back to a single -b:v target when
// the preset doesn't satisfy the interface (DefaultPreset /
// SimplePreset path).
type RateControlAware interface {
	RateControlMode() string // "cbr" / "vbr" / "crf" / "cqp"
	VideoMaxBitrate() string // "8000k" (CBR: == target; VBR: ceiling)
	VideoBufSize() string    // "8000k" (CBR: == target; VBR: typ 2x target)
}

// BroadcastSafeAware reports whether the preset wants the
// `limiter`/`broadcast safe` video filter applied to clamp luma to
// 16-235 and chroma to 16-240. Set on broadcast-tier presets.
type BroadcastSafeAware interface {
	BroadcastSafe() bool
}

// AudioFormatAware exposes the audio sample rate and channel count.
// Used by the supervisor to lock the audio encoder to the preset's
// 48 kHz / stereo broadcast baseline rather than relying on FFmpeg's
// input-side default.
type AudioFormatAware interface {
	AudioSampleRate() int
	AudioChannels() int
}

// HEVCAware reports whether the preset's recommended encoder list is
// HEVC-only. Mirrors ffmpeg.HEVCAware: the args builder promotes the
// encoder family to hevc_videotoolbox / hevc_nvenc / libx265, falling
// back to 8-bit H.264 when no HEVC encoder is usable.
type HEVCAware interface {
	PrefersHEVC() bool
}

// ColorPrimaries returns the BT.709 / BT.2020 primaries name.
func (f FFmpegPreset) ColorPrimaries() string {
	if f.Preset.ColorPrimaries == "" {
		return "bt709"
	}
	return f.Preset.ColorPrimaries
}

// ColorTransfer returns the OETF / EOTF identifier.
func (f FFmpegPreset) ColorTransfer() string {
	if f.Preset.ColorTransfer == "" {
		return "bt709"
	}
	return f.Preset.ColorTransfer
}

// ColorSpace returns the matrix coefficients identifier.
func (f FFmpegPreset) ColorSpace() string {
	if f.Preset.ColorSpace == "" {
		return "bt709"
	}
	return f.Preset.ColorSpace
}

// ColorRange returns "tv" or "pc". Broadcast is always tv.
func (f FFmpegPreset) ColorRange() string {
	if f.Preset.ColorRange == "" {
		return "tv"
	}
	return f.Preset.ColorRange
}

// RateControlMode returns the configured rate control as a string.
func (f FFmpegPreset) RateControlMode() string {
	if f.Preset.RateControl == "" {
		return string(RateControlVBR)
	}
	return string(f.Preset.RateControl)
}

// VideoMaxBitrate returns the encoder ceiling in "Nk" form.
func (f FFmpegPreset) VideoMaxBitrate() string { return f.Preset.VideoMaxBitrateStr() }

// VideoBufSize returns the VBV buffer size in "Nk" form.
func (f FFmpegPreset) VideoBufSize() string { return f.Preset.VideoBufSizeStr() }

// BroadcastSafe reports whether to apply the broadcast-safe luma
// limiter. Set on broadcast-tier presets.
func (f FFmpegPreset) BroadcastSafe() bool { return f.Preset.BroadcastSafe }

// AudioSampleRate returns the preset audio sample rate.
func (f FFmpegPreset) AudioSampleRate() int { return f.Preset.AudioSampleRate }

// AudioChannels returns the preset audio channel count.
func (f FFmpegPreset) AudioChannels() int { return f.Preset.AudioChannels }

// PrefersHEVC reports whether the preset's recommended encoder list
// is HEVC-only.
func (f FFmpegPreset) PrefersHEVC() bool { return isHEVCPreset(f.Preset) }

// -----------------------------------------------------------------------------
// Catalog accessors used by the HTTP layer
// -----------------------------------------------------------------------------

// All returns every preset in catalog order. The HTTP /api/quality
// handler walks this to build the tier-grouped picker payload. Returns
// a fresh copy so callers can sort / filter without mutating the
// package-level catalog.
func All() []Preset {
	out := make([]Preset, len(Presets))
	copy(out, Presets)
	return out
}

// TierSummary is one row of the JSON returned by GET /api/quality:
// a tier identifier plus the ordered list of presets within it.
type TierSummary struct {
	Tier    Tier            `json:"tier"`
	Label   string          `json:"label"`
	Presets []PresetSummary `json:"presets"`
}

// PresetSummary is the per-preset payload surfaced to the UI. It's
// deliberately a subset of Preset — the encoder picker doesn't need
// HDR luminance / x264-params / etc. and we don't want to leak
// internal knobs through an operator-facing endpoint.
type PresetSummary struct {
	ID               string    `json:"id"`
	Label            string    `json:"label"`
	Tier             Tier      `json:"tier"`
	Width            int       `json:"width"`
	Height           int       `json:"height"`
	Resolution       string    `json:"resolution"`
	FPS              string    `json:"fps"`
	FPSInt           int       `json:"fps_int"`
	VideoBitrateKbps int       `json:"video_bitrate_kbps"`
	AudioBitrateKbps int       `json:"audio_bitrate_kbps"`
	MinUploadKbps    int       `json:"min_upload_kbps"`
	RateControl      string    `json:"rate_control"`
	ColorPrimaries   string    `json:"color_primaries"`
	HWEncoders       []Encoder `json:"hw_encoders"`
	Notes            string    `json:"notes,omitempty"`
}

// TierLabels returns the human-friendly label for each tier. The UI
// renders this as an <optgroup> heading.
var TierLabels = map[Tier]string{
	TierConsumer:  "Consumer",
	TierProsumer:  "Prosumer",
	TierBroadcast: "Broadcast",
	TierCinema:    "Cinema",
}

// Summarise returns the tier-grouped catalog summary suitable for
// JSON marshalling. Order matches the operator-friendly progression
// consumer → prosumer → broadcast → cinema regardless of the
// internal catalog order.
func Summarise() []TierSummary {
	order := []Tier{TierConsumer, TierProsumer, TierBroadcast, TierCinema}
	out := make([]TierSummary, 0, len(order))
	for _, tier := range order {
		presets := ByTier(tier)
		if len(presets) == 0 {
			continue
		}
		summaries := make([]PresetSummary, 0, len(presets))
		for _, p := range presets {
			summaries = append(summaries, presetSummaryOf(p))
		}
		label := TierLabels[tier]
		if label == "" {
			label = string(tier)
		}
		out = append(out, TierSummary{
			Tier:    tier,
			Label:   label,
			Presets: summaries,
		})
	}
	return out
}

func presetSummaryOf(p Preset) PresetSummary {
	return PresetSummary{
		ID:               p.ID,
		Label:            p.Label,
		Tier:             p.Tier,
		Width:            p.Width,
		Height:           p.Height,
		Resolution:       p.Resolution(),
		FPS:              p.FPS.Expr(),
		FPSInt:           p.FPS.Int(),
		VideoBitrateKbps: p.VideoBitrate,
		AudioBitrateKbps: p.AudioBitrate,
		MinUploadKbps:    p.MinUploadKbps,
		RateControl:      string(p.RateControl),
		ColorPrimaries:   p.ColorPrimaries,
		HWEncoders:       append([]Encoder(nil), p.RecommendedHW...),
		Notes:            p.Notes,
	}
}

// -----------------------------------------------------------------------------
// Tier walker — for the adaptive controller's PresetTier interface
// -----------------------------------------------------------------------------
//
// internal/ffmpeg.AdaptiveController calls PresetTier.Lower / Higher
// to walk the catalog when bandwidth drops or recovers. We expose
// pure-data Step{Lower,Higher} helpers here; the root package wraps
// them into the ffmpeg.PresetTier interface so we avoid an
// ffmpeg <-> quality import cycle.

// StepLower returns the next-lower preset within the current
// preset's tier (smaller bitrate). Returns (zero, false) at the tier
// floor. Cross-tier hops are deliberately disallowed — stepping from
// broadcast 1080p60 down to consumer 1080p30 would also drop the
// color signalling and CBR contract, which is not what "automatic
// quality fallback" means.
func StepLower(currentID string) (Preset, bool) {
	current, ok := ByID(currentID)
	if !ok {
		return Preset{}, false
	}
	siblings := ByTier(current.Tier)
	var best Preset
	found := false
	for _, p := range siblings {
		if p.VideoBitrate >= current.VideoBitrate {
			continue
		}
		if !found || p.VideoBitrate > best.VideoBitrate {
			best = p
			found = true
		}
	}
	return best, found
}

// StepHigher returns the next-higher preset within the tier, capped
// at targetID's bitrate (we never exceed the operator's chosen
// target). The current preset must share a tier with target — a
// cross-tier walk would mean the user chose, say, broadcast and the
// controller had previously stepped down out of broadcast into a
// different tier (impossible by StepLower's contract).
func StepHigher(currentID, targetID string) (Preset, bool) {
	current, ok := ByID(currentID)
	if !ok {
		return Preset{}, false
	}
	target, ok := ByID(targetID)
	if !ok {
		return Preset{}, false
	}
	if current.Tier != target.Tier {
		return Preset{}, false
	}
	siblings := ByTier(current.Tier)
	var best Preset
	found := false
	for _, p := range siblings {
		if p.VideoBitrate <= current.VideoBitrate {
			continue
		}
		if p.VideoBitrate > target.VideoBitrate {
			continue
		}
		if !found || p.VideoBitrate < best.VideoBitrate {
			best = p
			found = true
		}
	}
	return best, found
}

// -----------------------------------------------------------------------------
// Validation helper
// -----------------------------------------------------------------------------

// AssertSummariesValid is a compile-time-style sanity check the
// http handler can call at startup. Returns nil iff every preset
// in the catalog produces a non-empty Resolution / FPS / bitrate.
// Used by main.go before installing the route so a malformed
// preset doesn't ship with the binary.
func AssertSummariesValid() error {
	for _, p := range Presets {
		if p.ID == "" {
			return fmt.Errorf("preset has empty ID at index %d", presetIndex(p))
		}
		if p.Width <= 0 || p.Height <= 0 {
			return fmt.Errorf("preset %q has invalid dimensions %dx%d", p.ID, p.Width, p.Height)
		}
		if p.VideoBitrate <= 0 {
			return fmt.Errorf("preset %q has non-positive video bitrate %d", p.ID, p.VideoBitrate)
		}
		if _, err := strconv.Atoi(p.FPS.Expr()); err != nil {
			// Non-integer — must be the rational form. Den must be > 1.
			if p.FPS.Den <= 1 {
				return fmt.Errorf("preset %q non-integer FPS but Den=%d", p.ID, p.FPS.Den)
			}
		}
	}
	return nil
}

func presetIndex(p Preset) int {
	for i, q := range Presets {
		if q.ID == p.ID {
			return i
		}
	}
	return -1
}
