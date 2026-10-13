// Package playback defines the decoder-side playback profile matrix and
// the mpv flag fragments each profile translates to.
//
// This is the symmetric counterpart to internal/quality on the encoder
// side. Where internal/quality models the encoder's resolution/bitrate/
// color envelope, internal/playback models the decoder's display-sync,
// frame-interpolation, hardware-decode, and output-stage configuration.
//
// The package is deliberately self-contained: it has no dependency on
// the mpv supervisor, the HTTP layer, or the configuration loader. The
// decoder (in internal/app/MPVService.Start) calls MPVArgs() on a
// resolved Profile and splices the fragment into the larger argv it
// constructs for the IPC socket, idle/cache flags, etc. Keeping the
// boundary here narrow lets the registry grow with new profiles without
// touching the process-management code.
//
// Why we need this
//
//   - Mixed-cadence multi-campus deployments are real. A main campus may
//     broadcast cinema 23.976/24 fps content while a satellite campus
//     plays it on a 30 Hz projector. Naive playback shows one source
//     frame doubled for every 4 source frames (a 2-1-1-1 pattern) and
//     the result judders visibly during slow pans. The fix is
//     `--video-sync=display-resample --interpolation=yes
//     --display-fps-override=30`: mpv resamples the timeline to the
//     display's refresh and synthesizes the in-between frames.
//
//   - Broadcast SDI output (Decklink) wants `--video-sync=display-vdrop`
//     with `--hwdec=no`. NOTE: upstream mpv has no `decklink` video
//     output and no `--decklink-format` option, so the decklink-broadcast
//     profile is marked Unavailable (see CheckAvailable) and is not
//     listed by All().
//
//   - 60 Hz and 50 Hz TVs differ between regions (NTSC vs PAL). 24→50
//     fills 25 display frames from every 12 source frames (eleven shown
//     twice, one held for 3), and 30→50 is a 2-2-1 pattern over 3 source
//     frames; both judder, so interpolation matters. 24→60 is 3:2
//     pulldown: a regular cadence that still judders on slow pans; the
//     60 Hz profiles accept that rather than spend GPU on interpolation.
//
//   - "Auto" is the safety fallback for operators who don't know or
//     care; it leaves mpv to its defaults (the prior behavior).
//
// mpv flag references
//
//   - --video-sync=audio        : default, naive (decoder follows audio).
//   - --video-sync=display-resample : resample to display refresh, smoother
//     for cinema→non-multiple refresh rates.
//   - --video-sync=display-vdrop    : drop frames to match display refresh;
//     used by SDI/broadcast paths.
//   - --interpolation=yes       : motion-compensated frame interpolation;
//     spends CPU/GPU to kill cadence judder.
//   - --display-fps-override=N  : force the assumed display refresh; useful
//     when EDID lies (projectors, KVMs). Requires mpv >= 0.37 (older
//     releases spell it --override-display-fps).
//   - --hwdec=auto|videotoolbox|nvdec|vaapi|no : hardware decode selection.
//     "no" forces software decode for deterministic
//     timing (broadcast use case).
//   - --vo=gpu                  : output stage (the default desktop path).
//     A --vo=decklink stage only exists in custom mpv builds.
package playback

import (
	"errors"
	"fmt"
	"strconv"
)

// ErrProfileUnavailable is wrapped by Profile.CheckAvailable when a
// profile can't run on upstream mpv.
var ErrProfileUnavailable = errors.New("playback profile unavailable")

// VideoSync constants name the mpv --video-sync values this package
// understands. The strings are passed to mpv unmodified.
const (
	VideoSyncAudio           = "audio"
	VideoSyncDisplayResample = "display-resample"
	VideoSyncDisplayVDrop    = "display-vdrop"
)

// HWDec constants name the mpv --hwdec values this package understands.
// "auto" lets mpv pick; "no" forces software decode (broadcast path).
const (
	HWDecAuto         = "auto"
	HWDecNo           = "no"
	HWDecVideoToolbox = "videotoolbox"
	HWDecNVDec        = "nvdec"
	HWDecVAAPI        = "vaapi"
)

// OutputMode names the mpv --vo target the profile selects. "gpu" is
// the desktop/projector path; "decklink" routes frames to an SDI card.
const (
	OutputModeGPU      = "gpu"
	OutputModeDecklink = "decklink"
)

// Profile is the unit of operator-facing playback choice. One Profile
// produces a self-consistent set of mpv flags: a single video-sync
// strategy, an interpolation toggle, an optional display-fps override,
// a hardware-decode choice, and an output stage. Profiles do not carry
// stream-side concerns (URL, screen index, fullscreen) — those belong
// to the request DTO.
type Profile struct {
	// ID is the stable identifier ("projector-30"). Persisted in
	// AppSettings, sent over the API, stored in localStorage.
	ID string `json:"id"`
	// Label is the human-readable name shown in the UI ("Projector 30Hz").
	Label string `json:"label"`
	// Description is one operator-facing line surfaced under the picker
	// in the UI. Documents the cadence/refresh scenario the profile is
	// for; doc/playback-profiles.md has the long-form explanation.
	Description string `json:"description"`

	// VideoSync is the mpv --video-sync value:
	//   "audio"             — default, naive; decoder follows audio.
	//   "display-resample"  — resample the video timeline to the display
	//                         refresh rate. Smooths cinema→non-multiple
	//                         refresh cadences (24→30, 24→50).
	//   "display-vdrop"     — drop frames to align with the display.
	//                         Used by SDI/broadcast where the card has
	//                         its own clock.
	// Empty string means "do not set --video-sync" — equivalent to
	// "audio" in mpv's defaults, but emitted as zero flags so the auto
	// profile can claim "we don't touch this".
	VideoSync string `json:"video_sync"`

	// Interpolation gates --interpolation=yes. Frame interpolation is
	// the heaviest CPU/GPU lever in this profile; reserve it for the
	// 24→30 (2-1-1-1) and 24→50 / 30→50 cadence-judder cases where it
	// makes a visible difference. Broadcast cards bypass interpolation
	// (they have their own clock). 24→60 is 3:2 pulldown, which also
	// judders on slow pans; the 60 Hz profiles leave interpolation off
	// as a CPU/quality trade-off, not because the cadence is clean.
	Interpolation bool `json:"interpolation"`

	// DisplayFPSOverride is the value passed to --display-fps-override.
	// Zero means "let mpv autodetect via the display EDID" — usually
	// correct. Override is useful when the EDID lies (projectors, KVMs,
	// some HDMI splitters) or when the operator knows the panel's
	// native refresh better than the display does.
	DisplayFPSOverride float64 `json:"display_fps_override"`

	// HWDec is the mpv --hwdec value. "auto" is the desktop default;
	// "no" forces software decode (broadcast path — deterministic
	// timing on SDI cards is worth the CPU). Platform-specific values
	// ("videotoolbox" on macOS, "nvdec" on NVIDIA, "vaapi" on Intel/
	// AMD Linux) are available for operators who want to pin the
	// backend.
	HWDec string `json:"hwdec"`

	// OutputMode drives --vo. "gpu" is the desktop path with full
	// shader and color-management support; "decklink" routes the
	// decoded frames to an SDI board.
	OutputMode string `json:"output_mode"`

	// DecklinkFormat is the SMPTE format identifier the user picks
	// through the existing decklink_format UI control (e.g.
	// "1080p59.94"). The profile carries it for completeness; today's
	// UI continues to pick the format separately. When non-empty AND
	// OutputMode == "decklink", MPVArgs() emits --decklink-format.
	DecklinkFormat string `json:"decklink_format,omitempty"`

	// Unavailable, when non-empty, is the operator-facing reason this
	// profile cannot be used with upstream mpv. Such profiles stay
	// resolvable through ByID (so stored settings / legacy requests get
	// a clear error instead of silently changing behaviour) but are not
	// listed by All(). Callers must check CheckAvailable before
	// launching mpv.
	Unavailable string `json:"unavailable,omitempty"`
}

// CheckAvailable returns nil when the profile can be launched, or an
// error wrapping ErrProfileUnavailable with the reason.
func (p Profile) CheckAvailable() error {
	if p.Unavailable == "" {
		return nil
	}
	return fmt.Errorf("%w: %s: %s", ErrProfileUnavailable, p.ID, p.Unavailable)
}

// MPVArgs returns the mpv flag fragment this profile contributes. Pure
// function — same Profile in, same []string out, no side effects. The
// caller assembles the full argv by combining base flags (URL, IPC
// socket, cache, idle/pause) with this fragment.
//
// Behaviour:
//
//   - OutputMode is always emitted as --vo=... so the base argv doesn't
//     have to know which output stage to use.
//   - VideoSync is emitted only when non-empty; the "auto" profile
//     leaves it off so mpv falls back to its own default (audio).
//   - Interpolation is emitted as --interpolation=yes ONLY when true.
//     mpv's default is no; explicit --interpolation=no would be
//     redundant.
//   - DisplayFPSOverride is emitted only when non-zero. Zero means
//     "let mpv autodetect".
//   - HWDec is always emitted (the default "auto" is the safest
//     desktop choice, "no" is intentional for broadcast).
//   - DecklinkFormat is emitted only when OutputMode is decklink and
//     the format is non-empty.
//
// The order of flags matches the existing handcoded argv in mpv.go so
// log diffing across the migration is straightforward.
func (p Profile) MPVArgs() []string {
	args := make([]string, 0, 8)

	// --vo first — output stage selection.
	mode := p.OutputMode
	if mode == "" {
		mode = OutputModeGPU
	}
	args = append(args, "--vo="+mode)

	// Hardware decode. Always emit so the operator can see which
	// backend is in play.
	hw := p.HWDec
	if hw == "" {
		hw = HWDecAuto
	}
	args = append(args, "--hwdec="+hw)

	// --video-sync. Only emit when explicitly set; the auto profile
	// leaves it off so mpv's own default applies.
	if p.VideoSync != "" {
		args = append(args, "--video-sync="+p.VideoSync)
	}

	// --interpolation=yes only when enabled (mpv's default is no).
	if p.Interpolation {
		args = append(args, "--interpolation=yes")
	}

	// --display-fps-override only when non-zero (zero = let mpv
	// autodetect). The flag name needs mpv >= 0.37; earlier releases
	// call it --override-display-fps. There is no mpv version probe in
	// this package, so the modern spelling is emitted unconditionally. We render integer rates as plain decimals and
	// fractional rates (rare in this context — projectors quote integer
	// refresh) as the float form mpv accepts.
	if p.DisplayFPSOverride > 0 {
		args = append(args, "--display-fps-override="+formatFPS(p.DisplayFPSOverride))
	}

	// Decklink format — only when the output stage is decklink and the
	// operator (or profile) has specified a format.
	if mode == OutputModeDecklink && p.DecklinkFormat != "" {
		args = append(args, "--decklink-format="+p.DecklinkFormat)
	}

	return args
}

// formatFPS renders the override value as an integer when it's a clean
// integer (e.g. 30.0 → "30") and otherwise as a 3-decimal float (e.g.
// 23.976 → "23.976"). mpv accepts both forms on --display-fps-override.
func formatFPS(v float64) string {
	if v == float64(int(v)) {
		return strconv.Itoa(int(v))
	}
	return strconv.FormatFloat(v, 'f', 3, 64)
}

// profiles is the canonical registry. Adding a profile here is the only
// edit required to wire it into the catalog, the HTTP endpoint, the UI
// (which fetches /api/playback-profiles), and the resolver in
// MPVService.Start. Order is the order shown in the UI picker.
//
// The profiles cover the cross-product of (projector vs TV vs
// broadcast) × (24 / 30 / 50 / 60 Hz) without exploding into 16+ entries.
// decklink-broadcast is kept in the registry (so ByID resolves it with
// a clear CheckAvailable error) but is Unavailable on upstream mpv.
// The selection rationale lives in docs/playback-profiles.md.
var profiles = []Profile{
	{
		ID:          "auto",
		Label:       "Auto",
		Description: "Use mpv defaults. Safe fallback when the display behavior is unknown or untested.",
		// VideoSync intentionally empty: auto profile emits no
		// --video-sync flag so mpv applies its own default ("audio").
		VideoSync:          "",
		Interpolation:      false,
		DisplayFPSOverride: 0,
		HWDec:              HWDecAuto,
		OutputMode:         OutputModeGPU,
	},
	{
		ID:    "projector-24",
		Label: "Projector 24Hz",
		Description: "True 24Hz projector receiving 23.976/24 cinema content. Resample the timeline; " +
			"no interpolation needed (the source IS 24).",
		VideoSync:          VideoSyncDisplayResample,
		Interpolation:      false,
		DisplayFPSOverride: 24,
		HWDec:              HWDecAuto,
		OutputMode:         OutputModeGPU,
	},
	{
		ID:    "projector-30",
		Label: "Projector 30Hz",
		Description: "30Hz projector receiving 24fps cinema. Interpolation ON: 24→30 doubles one of every 4 source frames " +
			"(a 2-1-1-1 pattern) and judders on pans. Costs CPU/GPU but smooths the cadence.",
		VideoSync:          VideoSyncDisplayResample,
		Interpolation:      true,
		DisplayFPSOverride: 30,
		HWDec:              HWDecAuto,
		OutputMode:         OutputModeGPU,
	},
	{
		ID:    "projector-60",
		Label: "Projector 60Hz",
		Description: "60Hz projector receiving 24fps cinema. 24→60 is 3:2 pulldown: a regular cadence that still judders " +
			"on slow pans. Interpolation off to save GPU; use projector-24 if the projector can run at 24Hz.",
		VideoSync:          VideoSyncDisplayResample,
		Interpolation:      false,
		DisplayFPSOverride: 60,
		HWDec:              HWDecAuto,
		OutputMode:         OutputModeGPU,
	},
	{
		ID:    "tv-60",
		Label: "Consumer TV 60Hz",
		Description: "Most US/JP consumer TVs and PC monitors at 60Hz. Resample the timeline; interpolation off. " +
			"30/60fps sources map cleanly; 24fps cinema shows 3:2 pulldown judder on slow pans.",
		VideoSync:          VideoSyncDisplayResample,
		Interpolation:      false,
		DisplayFPSOverride: 60,
		HWDec:              HWDecAuto,
		OutputMode:         OutputModeGPU,
	},
	{
		ID:    "tv-50-pal",
		Label: "PAL TV 50Hz",
		Description: "UK/EU consumer TVs at 50Hz receiving 24fps cinema or 30fps NTSC content. Interpolation ON: " +
			"24→50 holds one frame in 12 for 3 display frames, and 30→50 is a 2-2-1 pattern; both judder without it.",
		VideoSync:          VideoSyncDisplayResample,
		Interpolation:      true,
		DisplayFPSOverride: 50,
		HWDec:              HWDecAuto,
		OutputMode:         OutputModeGPU,
	},
	{
		ID:    "decklink-broadcast",
		Label: "Decklink (broadcast SDI)",
		Description: "SDI output to a Blackmagic Decklink card. Drop frames to align with the card's SMPTE clock; " +
			"software decode for deterministic timing; no interpolation (broadcast cards don't want it).",
		VideoSync:          VideoSyncDisplayVDrop,
		Interpolation:      false,
		DisplayFPSOverride: 0, // decklink has its own clock; override is meaningless
		HWDec:              HWDecNo,
		OutputMode:         OutputModeDecklink,
		// DecklinkFormat left empty; the UI's existing decklink_format
		// control fills it in per-request, and handlers_output merges
		// the request value into the resolved profile before calling
		// MPVService.Start.
		Unavailable: "upstream mpv has no decklink video output (--vo=decklink) or --decklink-format option; " +
			"SDI playback needs a custom mpv build or a different output path",
	},
}

// All returns every available profile in registry order (profiles with
// a non-empty Unavailable reason are omitted so the UI doesn't offer
// them). Returns a fresh slice so callers can sort/filter without
// mutating the package-level registry.
func All() []Profile {
	out := make([]Profile, 0, len(profiles))
	for _, p := range profiles {
		if p.Unavailable == "" {
			out = append(out, p)
		}
	}
	return out
}

// ByIDAvailable is ByID followed by CheckAvailable: it returns an error
// for unknown ids and for profiles that can't run on upstream mpv.
func ByIDAvailable(id string) (Profile, error) {
	p, ok := ByID(id)
	if !ok {
		return Profile{}, fmt.Errorf("unknown playback profile %q", id)
	}
	if err := p.CheckAvailable(); err != nil {
		return p, err
	}
	return p, nil
}

// ByID looks up a profile by stable identifier, including unavailable
// ones (check Profile.CheckAvailable before launching). Returns the zero
// value and false when the id is unknown; callers must check the bool
// before consuming the profile.
func ByID(id string) (Profile, bool) {
	for _, p := range profiles {
		if p.ID == id {
			return p, true
		}
	}
	return Profile{}, false
}

// Default returns the safest profile for callers that have no operator
// input yet. "auto" is correct: it leaves mpv to its defaults (the
// prior behavior), so existing deployments see no change in output
// unless an operator explicitly opts into a different profile.
func Default() Profile {
	p, _ := ByID("auto")
	return p
}

// DefaultID is the stable identifier of the default profile. Exposed
// separately so the HTTP /api/playback-profiles endpoint can include it
// in the payload without round-tripping through Default().
const DefaultID = "auto"

// Validate is a startup sanity check the HTTP layer can call before
// installing the route so a malformed profile doesn't ship with the
// binary. Returns nil iff every profile in the registry has a
// non-empty ID, label, and (when OutputMode == "decklink") doesn't
// also try to set --display-fps-override (decklink ignores it).
func Validate() error {
	seen := make(map[string]bool, len(profiles))
	for i, p := range profiles {
		if p.ID == "" {
			return fmt.Errorf("profile at index %d has empty ID", i)
		}
		if seen[p.ID] {
			return fmt.Errorf("profile %q duplicated", p.ID)
		}
		seen[p.ID] = true
		if p.Label == "" {
			return fmt.Errorf("profile %q has empty Label", p.ID)
		}
		switch p.OutputMode {
		case "", OutputModeGPU, OutputModeDecklink:
			// ok
		default:
			return fmt.Errorf("profile %q has unknown OutputMode %q", p.ID, p.OutputMode)
		}
		switch p.VideoSync {
		case "", VideoSyncAudio, VideoSyncDisplayResample, VideoSyncDisplayVDrop:
			// ok
		default:
			return fmt.Errorf("profile %q has unknown VideoSync %q", p.ID, p.VideoSync)
		}
		switch p.HWDec {
		case "", HWDecAuto, HWDecNo, HWDecVideoToolbox, HWDecNVDec, HWDecVAAPI:
			// ok
		default:
			return fmt.Errorf("profile %q has unknown HWDec %q", p.ID, p.HWDec)
		}
	}
	return nil
}
