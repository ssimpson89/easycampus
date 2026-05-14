package quality

import (
	"strings"
	"testing"
)

// TestAsFFmpegPresetAllTiers verifies that wrapping a Preset of each
// tier into an FFmpegPreset yields non-empty / sensible values for
// every method of the ffmpeg.QualityPreset interface.
//
// We can't import internal/ffmpeg here (would create a cycle), but
// the interface is structural — checking each method's return value
// directly is equivalent to the type-assertion the supervisor performs.
func TestAsFFmpegPresetAllTiers(t *testing.T) {
	ids := []string{
		"consumer-480p30", "consumer-720p30", "consumer-1080p30",
		"prosumer-720p60", "prosumer-1080p30-high", "prosumer-1080p60",
		"broadcast-1080p30", "broadcast-1080p60", "broadcast-2160p30",
		"cinema-1080p2398", "cinema-1080p24",
		"cinema-2160p2398", "cinema-2160p24",
	}
	for _, id := range ids {
		t.Run(id, func(t *testing.T) {
			p, ok := ResolveFFmpegPreset(id)
			if !ok {
				t.Fatalf("ResolveFFmpegPreset(%q) returned ok=false", id)
			}
			if got := p.ID(); got == "" {
				t.Error("ID() returned empty")
			}
			if got := p.Label(); got == "" {
				t.Error("Label() returned empty")
			}
			if got := p.Resolution(); got == "" || !strings.Contains(got, "x") {
				t.Errorf("Resolution() = %q, want WxH", got)
			}
			if p.Width() <= 0 {
				t.Errorf("Width() = %d, want > 0", p.Width())
			}
			if p.Height() <= 0 {
				t.Errorf("Height() = %d, want > 0", p.Height())
			}
			if p.FPS() <= 0 {
				t.Errorf("FPS() = %d, want > 0", p.FPS())
			}
			if got := p.FPSExpr(); got == "" {
				t.Error("FPSExpr() returned empty")
			}
			if p.GOP() <= 0 {
				t.Errorf("GOP() = %d, want > 0", p.GOP())
			}
			if got := p.VideoBitrate(); !strings.HasSuffix(got, "k") {
				t.Errorf("VideoBitrate() = %q, want Nk suffix", got)
			}
			if got := p.AudioBitrate(); !strings.HasSuffix(got, "k") {
				t.Errorf("AudioBitrate() = %q, want Nk suffix", got)
			}
			if got := p.PixelFormat(); got == "" {
				t.Error("PixelFormat() returned empty")
			}
			if got := p.Profile(); got == "" {
				t.Error("Profile() returned empty")
			}
			if got := p.Level(); got == "" {
				t.Error("Level() returned empty")
			}
			// VideoEncoderPreset is allowed to be empty (the supervisor
			// substitutes per-encoder defaults).
			_ = p.VideoEncoderPreset()
			// Tune() returns empty for tier presets (the supervisor
			// overrides on the live path).
			_ = p.Tune()
			// WantsZeroLatency / WantsBFrames are typed booleans —
			// we just exercise them and verify the tier alignment.
			_ = p.WantsZeroLatency()
			_ = p.WantsBFrames()
		})
	}
}

// TestFFmpegPresetCinemaCadence verifies cinema presets emit the
// rational FPS form. 24000/1001 vs 24.000 is load-bearing for
// service-length cinema streams.
func TestFFmpegPresetCinemaCadence(t *testing.T) {
	cases := map[string]string{
		"cinema-1080p2398": "24000/1001",
		"cinema-2160p2398": "24000/1001",
		"cinema-1080p24":   "24",
		"cinema-2160p24":   "24",
	}
	for id, want := range cases {
		p, ok := ResolveFFmpegPreset(id)
		if !ok {
			t.Fatalf("ResolveFFmpegPreset(%q) missing", id)
		}
		if got := p.FPSExpr(); got != want {
			t.Errorf("preset %q FPSExpr=%q, want %q", id, got, want)
		}
	}
}

// TestFFmpegPresetColorAware verifies that the adapter satisfies the
// ColorPresetAware capability and returns the table's primaries.
func TestFFmpegPresetColorAware(t *testing.T) {
	// 1080p broadcast stays on Rec.709
	p, _ := ResolveFFmpegPreset("broadcast-1080p30")
	if got := p.ColorPrimaries(); got != "bt709" {
		t.Errorf("broadcast-1080p30 primaries=%q, want bt709", got)
	}
	if got := p.ColorRange(); got != "tv" {
		t.Errorf("broadcast-1080p30 range=%q, want tv", got)
	}

	// 2160p broadcast crosses to BT.2020
	p, _ = ResolveFFmpegPreset("broadcast-2160p30")
	if got := p.ColorPrimaries(); got != "bt2020" {
		t.Errorf("broadcast-2160p30 primaries=%q, want bt2020", got)
	}
	if got := p.ColorSpace(); got != "bt2020nc" {
		t.Errorf("broadcast-2160p30 space=%q, want bt2020nc", got)
	}
}

// TestFFmpegPresetRateControlAware verifies the adapter exposes the
// rate-control mode through the optional interface so the supervisor
// can wire CBR with 1-second VBV on the libx264 broadcast path.
func TestFFmpegPresetRateControlAware(t *testing.T) {
	p, _ := ResolveFFmpegPreset("broadcast-1080p30")
	if got := p.RateControlMode(); got != "cbr" {
		t.Errorf("broadcast-1080p30 rate=%q, want cbr", got)
	}
	if got := p.VideoMaxBitrate(); got != "8000k" {
		t.Errorf("broadcast-1080p30 maxrate=%q, want 8000k", got)
	}
	if got := p.VideoBufSize(); got != "8000k" {
		t.Errorf("broadcast-1080p30 bufsize=%q, want 8000k (1-second VBV)", got)
	}
	p, _ = ResolveFFmpegPreset("consumer-1080p30")
	if got := p.RateControlMode(); got != "vbr" {
		t.Errorf("consumer-1080p30 rate=%q, want vbr", got)
	}
}

// TestFFmpegPresetBroadcastSafeAware verifies the broadcast tier
// reports BroadcastSafe=true and other tiers don't.
func TestFFmpegPresetBroadcastSafeAware(t *testing.T) {
	p, _ := ResolveFFmpegPreset("broadcast-1080p30")
	if !p.BroadcastSafe() {
		t.Error("broadcast preset BroadcastSafe()=false, want true")
	}
	p, _ = ResolveFFmpegPreset("consumer-1080p30")
	if p.BroadcastSafe() {
		t.Error("consumer preset BroadcastSafe()=true, want false")
	}
}

// TestFFmpegPresetTierWalker verifies StepLower/StepHigher walks
// within the consumer tier and stops at the boundaries.
func TestFFmpegPresetTierWalker(t *testing.T) {
	// 1080p30 is the top of the consumer tier — StepHigher capped at
	// the same ID must return false.
	if _, ok := StepHigher("consumer-1080p30", "consumer-1080p30"); ok {
		t.Error("StepHigher at target ceiling should return false")
	}
	// 480p30 is the floor of the consumer tier — StepLower must
	// return false (no lower preset within the tier).
	if _, ok := StepLower("consumer-480p30"); ok {
		t.Error("StepLower at tier floor should return false")
	}
	// Standard step-down within consumer.
	p, ok := StepLower("consumer-1080p30")
	if !ok {
		t.Fatal("StepLower from consumer-1080p30 returned ok=false")
	}
	if p.ID != "consumer-720p30" {
		t.Errorf("StepLower(consumer-1080p30) = %q, want consumer-720p30", p.ID)
	}
	// Standard step-up — capped at the target.
	p, ok = StepHigher("consumer-720p30", "consumer-1080p30")
	if !ok {
		t.Fatal("StepHigher returned ok=false")
	}
	if p.ID != "consumer-1080p30" {
		t.Errorf("StepHigher(720p30 -> 1080p30) = %q, want consumer-1080p30", p.ID)
	}
}

// TestFFmpegPresetTierWalkerCrossTier verifies the walker refuses to
// hop tiers — broadcast tier presets share envelope properties (CBR,
// BT.2020) that consumer tiers don't.
func TestFFmpegPresetTierWalkerCrossTier(t *testing.T) {
	// StepLower from broadcast-1080p30 stops at the broadcast floor
	// (the table only has one preset below 1080p30 in broadcast tier:
	// none, since 1080p30 IS the floor). So it should return false.
	if _, ok := StepLower("broadcast-1080p30"); ok {
		t.Error("StepLower at broadcast tier floor should return false")
	}
}

// TestSummarisePopulated guards against an empty /api/quality
// response payload. The UI uses fallback when the catalog is empty
// but we'd rather catch the regression in test.
func TestSummarisePopulated(t *testing.T) {
	tiers := Summarise()
	if len(tiers) < 4 {
		t.Fatalf("Summarise() returned %d tiers, want >= 4", len(tiers))
	}
	seen := map[Tier]bool{}
	for _, ts := range tiers {
		seen[ts.Tier] = true
		if len(ts.Presets) == 0 {
			t.Errorf("tier %q has no presets in summary", ts.Tier)
		}
		if ts.Label == "" {
			t.Errorf("tier %q has empty label", ts.Tier)
		}
		for _, p := range ts.Presets {
			if p.ID == "" || p.Label == "" {
				t.Errorf("preset summary missing ID/Label: %+v", p)
			}
			if p.VideoBitrateKbps <= 0 {
				t.Errorf("preset %q has non-positive bitrate", p.ID)
			}
			if p.MinUploadKbps <= 0 {
				t.Errorf("preset %q has non-positive min upload", p.ID)
			}
		}
	}
	for _, want := range []Tier{TierConsumer, TierProsumer, TierBroadcast, TierCinema} {
		if !seen[want] {
			t.Errorf("Summarise() missing tier %q", want)
		}
	}
}

// TestAssertSummariesValid verifies the catalog passes the startup
// validator. A regression here means a malformed preset shipped.
func TestAssertSummariesValid(t *testing.T) {
	if err := AssertSummariesValid(); err != nil {
		t.Errorf("catalog failed validation: %v", err)
	}
}

// TestResolveFFmpegPresetUnknown returns ok=false for unknown IDs.
// Callers fall back to the supervisor's DefaultPreset on miss.
func TestResolveFFmpegPresetUnknown(t *testing.T) {
	if _, ok := ResolveFFmpegPreset("not-a-real-preset"); ok {
		t.Error("ResolveFFmpegPreset(unknown) = ok=true")
	}
	if _, ok := ResolveFFmpegPreset(""); ok {
		t.Error("ResolveFFmpegPreset(empty) = ok=true")
	}
}

// TestFFmpegPresetTuneOverride verifies WantsZeroLatency tracks the
// tier — consumer-tier presets opt into zerolatency on the live
// path, broadcast/cinema/prosumer keep B-frames for compression
// efficiency.
func TestFFmpegPresetTuneOverride(t *testing.T) {
	p, _ := ResolveFFmpegPreset("consumer-720p30")
	if !p.WantsZeroLatency() {
		t.Error("consumer preset WantsZeroLatency()=false, want true")
	}
	if p.WantsBFrames() {
		t.Error("consumer preset WantsBFrames()=true, want false")
	}
	p, _ = ResolveFFmpegPreset("broadcast-1080p30")
	if p.WantsZeroLatency() {
		t.Error("broadcast preset WantsZeroLatency()=true, want false")
	}
	if !p.WantsBFrames() {
		t.Error("broadcast preset WantsBFrames()=false, want true")
	}
}
