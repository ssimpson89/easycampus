package quality

import (
	"strings"
	"testing"
)

// TestPresetCatalogIsPopulated guards against the catalog being
// accidentally emptied / shadowed. The tier matrix in the task spec
// requires consumer, prosumer, broadcast, and cinema coverage.
func TestPresetCatalogIsPopulated(t *testing.T) {
	if len(Presets) < 12 {
		t.Fatalf("expected at least 12 presets across 4 tiers, got %d", len(Presets))
	}
	tiers := map[Tier]int{}
	for _, p := range Presets {
		tiers[p.Tier]++
	}
	for _, want := range []Tier{TierConsumer, TierProsumer, TierBroadcast, TierCinema} {
		if tiers[want] == 0 {
			t.Errorf("tier %q has no presets — recommender will skip it entirely", want)
		}
	}
}

// TestByIDLookup exercises the basic stable-identifier contract.
// Callers persist preset IDs in config and need a deterministic lookup.
func TestByIDLookup(t *testing.T) {
	cases := []string{
		"consumer-480p30", "consumer-720p30", "consumer-1080p30",
		"prosumer-720p60", "prosumer-1080p30-high", "prosumer-1080p60",
		"broadcast-1080p30", "broadcast-1080p60", "broadcast-2160p30",
		"cinema-1080p2398", "cinema-1080p24",
		"cinema-2160p2398", "cinema-2160p24",
	}
	for _, id := range cases {
		if _, ok := ByID(id); !ok {
			t.Errorf("ByID(%q) missing — operator-facing presets must remain stable", id)
		}
	}
	if _, ok := ByID("does-not-exist"); ok {
		t.Error("ByID(unknown) should return false")
	}
}

// TestPresetGOPIs2Seconds verifies every preset emits a 2-second GOP
// (within ±1 frame for fractional rates). Receivers / HLS segmenters
// expect 2 s; longer cadences break HLS conformance.
func TestPresetGOPIs2Seconds(t *testing.T) {
	for _, p := range Presets {
		gop := p.GOP()
		secs := float64(gop) / p.FPS.Float()
		if secs < 1.9 || secs > 2.1 {
			t.Errorf("preset %q GOP=%d at %g fps = %.3fs, want ~2s",
				p.ID, gop, p.FPS.Float(), secs)
		}
	}
}

// TestCinemaPresetsUseExact23976 verifies the cinema 23.976 presets
// emit "24000/1001" on -r, not the rounded 24.000 — the latter
// drifts ~3.6 s per hour and the muxer drops frames.
func TestCinemaPresetsUseExact23976(t *testing.T) {
	for _, id := range []string{"cinema-1080p2398", "cinema-2160p2398"} {
		p, ok := ByID(id)
		if !ok {
			t.Fatalf("missing preset %q", id)
		}
		if p.FPS.Expr() != "24000/1001" {
			t.Errorf("preset %q FPS.Expr=%q, want \"24000/1001\"", id, p.FPS.Expr())
		}
		if p.GOP() != 48 {
			t.Errorf("preset %q GOP=%d, want 48 (≈2 s at 23.976)", id, p.GOP())
		}
	}
}

// TestCinemaTruePresetsUse24 confirms the integer-24 cinema presets
// emit a plain "24" — distinct from the 23.976 variants.
func TestCinemaTruePresetsUse24(t *testing.T) {
	for _, id := range []string{"cinema-1080p24", "cinema-2160p24"} {
		p, ok := ByID(id)
		if !ok {
			t.Fatalf("missing preset %q", id)
		}
		if p.FPS.Expr() != "24" {
			t.Errorf("preset %q FPS.Expr=%q, want \"24\"", id, p.FPS.Expr())
		}
		if p.GOP() != 48 {
			t.Errorf("preset %q GOP=%d, want 48", id, p.GOP())
		}
	}
}

// TestNonCinemaPresetsEmitIntegerFPS verifies non-cinema presets
// emit a plain integer on -r. A spurious slash means the wrong
// rational got plumbed through.
func TestNonCinemaPresetsEmitIntegerFPS(t *testing.T) {
	for _, p := range Presets {
		if p.Tier == TierCinema {
			continue
		}
		if strings.Contains(p.FPS.Expr(), "/") {
			t.Errorf("preset %q (non-cinema) FPS.Expr=%q should be a plain integer",
				p.ID, p.FPS.Expr())
		}
	}
}

// TestBroadcastPresetsAreTrueCBR enforces the broadcast envelope:
// CBR rate control with 1-second VBV (bufsize == target).
func TestBroadcastPresetsAreTrueCBR(t *testing.T) {
	for _, p := range Presets {
		if p.Tier != TierBroadcast {
			continue
		}
		if p.RateControl != RateControlCBR {
			t.Errorf("preset %q (broadcast) rate control = %q, want CBR",
				p.ID, p.RateControl)
		}
		if p.VideoMaxBitrate != p.VideoBitrate {
			t.Errorf("preset %q (broadcast) maxrate=%d != target=%d — broadcast must be true CBR",
				p.ID, p.VideoMaxBitrate, p.VideoBitrate)
		}
		if p.VideoBufSize != p.VideoBitrate {
			t.Errorf("preset %q (broadcast) bufsize=%d != target=%d — broadcast must be 1-second VBV",
				p.ID, p.VideoBufSize, p.VideoBitrate)
		}
	}
}

// TestBroadcast2160UsesBT2020 verifies the 2160p broadcast preset
// emits BT.2020 primaries / colorspace. The HD broadcast presets
// stay on Rec.709.
func TestBroadcast2160UsesBT2020(t *testing.T) {
	p, ok := ByID("broadcast-2160p30")
	if !ok {
		t.Fatal("missing preset broadcast-2160p30")
	}
	if p.ColorPrimaries != "bt2020" {
		t.Errorf("broadcast-2160p30 color_primaries=%q, want bt2020", p.ColorPrimaries)
	}
	if p.ColorSpace != "bt2020nc" {
		t.Errorf("broadcast-2160p30 colorspace=%q, want bt2020nc", p.ColorSpace)
	}
	if p.PixFmt != "yuv420p10le" {
		t.Errorf("broadcast-2160p30 pix_fmt=%q, want yuv420p10le (10-bit required for BT.2020)", p.PixFmt)
	}
}

// TestConsumerPresetsStayRec709 verifies consumer/prosumer/HD-broadcast
// don't speculatively flip to BT.2020 — mistagged SDR clips on
// HDR-aware decoders.
func TestConsumerPresetsStayRec709(t *testing.T) {
	for _, p := range Presets {
		if p.Tier == TierConsumer || p.Tier == TierProsumer {
			if p.ColorPrimaries != "bt709" {
				t.Errorf("preset %q tier=%s primaries=%q, want bt709",
					p.ID, p.Tier, p.ColorPrimaries)
			}
		}
	}
}

// TestRecommendByBandwidth picks the strongest preset that fits the
// uplink. A volunteer at 10 Mbps should land on consumer-1080p30
// (8.5 Mbps min) and never get pushed into a broadcast tier they
// can't sustain.
func TestRecommendByBandwidth(t *testing.T) {
	cases := []struct {
		name       string
		uploadKbps int
		wantID     string
	}{
		{"emergency-DSL", 3000, "consumer-480p30"},
		{"6-mbps-DSL", 6500, "consumer-720p30"},
		{"10-mbps-cable", 10000, "consumer-1080p30"},
		{"15-mbps-fiber", 15000, "prosumer-1080p60"},
		{"50-mbps-business", 50000, "broadcast-1080p60"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := Recommend(Constraints{
				UploadKbps:        tc.uploadKbps,
				AvailableEncoders: []Encoder{EncoderH264VideoToolbox, EncoderH264NVENC, EncoderX264},
			})
			if !ok {
				t.Fatalf("Recommend returned no match for %d kbps", tc.uploadKbps)
			}
			if p.ID != tc.wantID {
				t.Errorf("Recommend(%d kbps) = %q, want %q", tc.uploadKbps, p.ID, tc.wantID)
			}
		})
	}
}

// TestRecommendCinemaCadence verifies operators who specifically
// want cinema cadence get a cinema preset — and that a generic
// recommendation never silently hands them one.
func TestRecommendCinemaCadence(t *testing.T) {
	c := Constraints{
		UploadKbps:        20000,
		AvailableEncoders: []Encoder{EncoderX264, EncoderH264NVENC},
		CinemaCadence:     true,
	}
	p, ok := Recommend(c)
	if !ok {
		t.Fatal("Recommend(cinema) returned no match")
	}
	if p.Tier != TierCinema {
		t.Errorf("Recommend(cinema cadence) tier=%q, want cinema", p.Tier)
	}

	// Without CinemaCadence the recommender must never surprise the
	// operator with film cadence.
	p2, ok := Recommend(Constraints{
		UploadKbps:        50000,
		AvailableEncoders: []Encoder{EncoderX264, EncoderH264NVENC},
	})
	if !ok {
		t.Fatal("Recommend(generic) returned no match")
	}
	if p2.Tier == TierCinema {
		t.Errorf("Recommend(generic) handed cinema preset %q — must be opt-in", p2.ID)
	}
}

// TestRecommendByTier honors PreferredTier. An operator who picked
// "broadcast" in the UI should not get bumped down to prosumer even
// if a prosumer preset would also fit the bandwidth.
func TestRecommendByTier(t *testing.T) {
	p, ok := Recommend(Constraints{
		UploadKbps:        20000,
		AvailableEncoders: []Encoder{EncoderX264, EncoderH264NVENC},
		PreferredTier:     TierBroadcast,
	})
	if !ok {
		t.Fatal("Recommend(broadcast) returned no match")
	}
	if p.Tier != TierBroadcast {
		t.Errorf("Recommend(PreferredTier=broadcast) tier=%q, want broadcast", p.Tier)
	}
}

// TestRecommendNoBandwidthMatch returns Default and false when
// nothing fits — callers expect a sane fallback rather than a
// zero-value preset.
func TestRecommendNoBandwidthMatch(t *testing.T) {
	p, ok := Recommend(Constraints{UploadKbps: 100})
	if ok {
		t.Errorf("Recommend(100 kbps) returned ok=true — nothing fits at 100 kbps")
	}
	if p.ID == "" {
		t.Error("Recommend fallback returned zero-value preset; want Default()")
	}
}

// TestBuildFFmpegArgsCarriesColorSignaling verifies every encoder
// path emits the color metadata BEFORE -c:v. Hardware encoders bake
// the VUI hints into the SPS at init.
func TestBuildFFmpegArgsCarriesColorSignaling(t *testing.T) {
	p, _ := ByID("broadcast-1080p30")
	encoders := []Encoder{
		EncoderX264, EncoderH264VideoToolbox, EncoderH264NVENC,
		EncoderH264QSV, EncoderH264VAAPI,
	}
	for _, enc := range encoders {
		args := BuildFFmpegArgs(p, enc)
		// Color flags must appear before -c:v.
		colorIdx := indexOf(args, "-color_primaries")
		cvIdx := indexOf(args, "-c:v")
		if colorIdx < 0 || cvIdx < 0 {
			t.Errorf("encoder %q: missing -color_primaries (%d) or -c:v (%d)",
				enc, colorIdx, cvIdx)
			continue
		}
		if colorIdx > cvIdx {
			t.Errorf("encoder %q: -color_primaries (idx %d) must come before -c:v (idx %d)",
				enc, colorIdx, cvIdx)
		}
	}
}

// TestBuildFFmpegArgsHEVCPresets verifies the HEVC encoder paths
// emit hvc1 tagging — Apple decoders refuse HEVC without it.
func TestBuildFFmpegArgsHEVCPresets(t *testing.T) {
	p, _ := ByID("broadcast-2160p30")
	for _, enc := range []Encoder{EncoderHEVCNVENC, EncoderHEVCVideoToolbox, EncoderX265} {
		args := BuildFFmpegArgs(p, enc)
		if !contains(args, "-tag:v") || !contains(args, "hvc1") {
			t.Errorf("encoder %q HEVC args missing -tag:v hvc1: %v", enc, args)
		}
	}
}

// TestBuildFFmpegArgsCBRBroadcast verifies broadcast x264 path emits
// the SPS HRD parameters (nal-hrd=cbr) so strict CDNs treat the
// stream as CBR, not VBR-with-filler.
func TestBuildFFmpegArgsCBRBroadcast(t *testing.T) {
	p, _ := ByID("broadcast-1080p30")
	args := BuildFFmpegArgs(p, EncoderX264)
	idx := indexOf(args, "-x264-params")
	if idx < 0 || idx+1 >= len(args) {
		t.Fatal("broadcast x264 args missing -x264-params")
	}
	params := args[idx+1]
	if !strings.Contains(params, "nal-hrd=cbr") {
		t.Errorf("broadcast x264 -x264-params=%q missing nal-hrd=cbr", params)
	}
	if !strings.Contains(params, "filler=1") {
		t.Errorf("broadcast x264 -x264-params=%q missing filler=1", params)
	}
	if !strings.Contains(params, "scenecut=0") {
		t.Errorf("broadcast x264 -x264-params=%q missing scenecut=0", params)
	}
}

// TestBuildFFmpegArgsConsumerVBR verifies consumer presets DON'T
// declare HRD CBR — those defaults exclude SPS filler NALs which
// VBR streams shouldn't carry.
func TestBuildFFmpegArgsConsumerVBR(t *testing.T) {
	p, _ := ByID("consumer-1080p30")
	args := BuildFFmpegArgs(p, EncoderX264)
	idx := indexOf(args, "-x264-params")
	if idx < 0 || idx+1 >= len(args) {
		t.Fatal("consumer x264 args missing -x264-params")
	}
	params := args[idx+1]
	if strings.Contains(params, "nal-hrd=cbr") {
		t.Errorf("consumer x264 -x264-params=%q should NOT declare nal-hrd=cbr (VBR preset)", params)
	}
}

// TestBuildFFmpegArgsCadenceFlags verifies fps_mode + -r emit
// together so the muxer locks both the rate and CFR semantics.
// -r without fps_mode lets duplicate-PTS frames through.
func TestBuildFFmpegArgsCadenceFlags(t *testing.T) {
	for _, id := range []string{"consumer-1080p30", "cinema-1080p2398", "broadcast-1080p60"} {
		p, ok := ByID(id)
		if !ok {
			t.Fatalf("missing preset %q", id)
		}
		args := BuildFFmpegArgs(p, EncoderX264)
		fpsModeIdx := indexOf(args, "-fps_mode")
		rIdx := indexOf(args, "-r")
		if fpsModeIdx < 0 || rIdx < 0 {
			t.Errorf("preset %q missing -fps_mode (%d) or -r (%d)", id, fpsModeIdx, rIdx)
			continue
		}
		if args[fpsModeIdx+1] != "cfr" {
			t.Errorf("preset %q -fps_mode=%q, want cfr", id, args[fpsModeIdx+1])
		}
		// Cinema 23.976 must emit "24000/1001"; integer rates must
		// emit a plain decimal.
		gotR := args[rIdx+1]
		wantR := p.FPS.Expr()
		if gotR != wantR {
			t.Errorf("preset %q -r=%q, want %q", id, gotR, wantR)
		}
	}
}

// TestBuildFFmpegArgsVideoToolboxKeyframeExpr verifies VideoToolbox
// uses -force_key_frames for IDR cadence. -g alone is advisory on
// VideoToolbox — confirmed by Apple's WWDC sessions.
func TestBuildFFmpegArgsVideoToolboxKeyframeExpr(t *testing.T) {
	p, _ := ByID("broadcast-1080p30")
	args := BuildFFmpegArgs(p, EncoderH264VideoToolbox)
	idx := indexOf(args, "-force_key_frames")
	if idx < 0 {
		t.Fatal("VideoToolbox args missing -force_key_frames")
	}
	expr := args[idx+1]
	if !strings.HasPrefix(expr, "expr:") {
		t.Errorf("VideoToolbox -force_key_frames=%q should start with expr:", expr)
	}
}

// TestBuildFFmpegArgsNVENCStrictGOP verifies NVENC paths emit
// -strict_gop and -no-scenecut so receivers can count frames to
// the next IDR.
func TestBuildFFmpegArgsNVENCStrictGOP(t *testing.T) {
	p, _ := ByID("broadcast-1080p30")
	args := BuildFFmpegArgs(p, EncoderH264NVENC)
	for _, want := range []string{"-strict_gop", "-no-scenecut", "-forced-idr"} {
		if !contains(args, want) {
			t.Errorf("NVENC args missing %s: %v", want, args)
		}
	}
}

// TestPreferredEncoderFallback verifies the encoder selector falls
// back to the right software encoder when no hardware option fits.
func TestPreferredEncoderFallback(t *testing.T) {
	hd := mustByID(t, "broadcast-1080p30")
	if got := PreferredEncoder(hd, nil); got != EncoderX264 {
		t.Errorf("PreferredEncoder(HD, none) = %q, want %q", got, EncoderX264)
	}
	uhd := mustByID(t, "broadcast-2160p30")
	if got := PreferredEncoder(uhd, nil); got != EncoderX265 {
		t.Errorf("PreferredEncoder(UHD, none) = %q, want %q", got, EncoderX265)
	}
	// First match wins in preset's preference order.
	if got := PreferredEncoder(hd, []Encoder{EncoderH264NVENC, EncoderX264}); got != EncoderX264 {
		// broadcast-1080p30 recommends x264 first.
		t.Errorf("PreferredEncoder(HD, [nvenc,x264]) = %q, want %q (catalog order)", got, EncoderX264)
	}
}

// TestPresetMinUploadIncludesAudio guards against forgetting the
// audio bitrate in MinUploadKbps. The recommender uses this to
// gate the catalog — undercounting strands operators on a preset
// they can't actually stream.
func TestPresetMinUploadIncludesAudio(t *testing.T) {
	for _, p := range Presets {
		floor := p.VideoBitrate + p.AudioBitrate
		if p.MinUploadKbps < floor {
			t.Errorf("preset %q MinUploadKbps=%d < video+audio=%d — must include audio overhead",
				p.ID, p.MinUploadKbps, floor)
		}
	}
}

// --- helpers ---

func mustByID(t *testing.T, id string) Preset {
	t.Helper()
	p, ok := ByID(id)
	if !ok {
		t.Fatalf("missing preset %q", id)
	}
	return p
}

func indexOf(args []string, needle string) int {
	for i, a := range args {
		if a == needle {
			return i
		}
	}
	return -1
}

func contains(args []string, needle string) bool {
	return indexOf(args, needle) >= 0
}
