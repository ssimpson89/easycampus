# Quality Presets

EasyCampus organises encoder quality into four tiers, each aimed at a
specific operator profile, infrastructure, and audience expectation.
Pick the tier that matches the install; pick the preset inside the tier
that matches the available upload bandwidth.

The catalog lives in `internal/quality/quality.go`. Each preset's
`MinUploadKbps` is the honest floor including audio and ~20% protocol
overhead — the recommender uses that number to filter the catalog so
operators never land on a preset their network can't sustain.

## Tier 1 — Consumer (church on a budget)

**Target operator:** volunteer, no broadcast training, follows a printed
checklist.

**Hardware:** Mac mini iGPU or low-end Windows laptop, USB webcam, single
HDMI capture, on-board audio.

**Network:** residential cable or DSL, 5–10 Mbps sustained upload.

**Goal:** stream survives a Sunday service without operator intervention.

| ID                     | Resolution  | FPS | Bitrate | Min upload  |
|------------------------|-------------|-----|---------|-------------|
| `consumer-480p30`      | 854×480     | 30  | 1.5 Mbps | 2.5 Mbps   |
| `consumer-720p30`      | 1280×720    | 30  | 3.5 Mbps | 5.5 Mbps   |
| `consumer-1080p30`     | 1920×1080   | 30  | 6.0 Mbps | 8.5 Mbps   |

**Rate control:** VBR with a 33% headroom ceiling and a 2-second VBV.
VBR lets the encoder spend bits where the picture needs them and saves
bits during static scenes — important on a residential uplink that
fluctuates with the rest of the household.

**Color:** Rec.709 SDR, `color_range=tv`. Hardware encoders bake the
VUI hints into the SPS at init.

**Audio:** 96–128 kbps AAC stereo at 48 kHz.

**Encoder preference:** VideoToolbox → NVENC → QuickSync → x264.
VideoToolbox first because the Mac mini is the modal consumer rig.

### Caveats

- `consumer-480p30` is below most live distributors' published minimums.
  Use as a fallback when the upload is actively failing — not as a
  default.
- Volunteers should set the QualityPreset once and leave it alone. The
  recommender takes upload bandwidth and picks for them.

## Tier 2 — Prosumer

**Target operator:** semi-technical, comfortable with OBS / vMix.

**Hardware:** Decklink Mini Recorder or Elgato Cam Link, discrete GPU
(NVIDIA preferred for NVENC), discrete audio interface.

**Network:** business fiber or cable, 15–25 Mbps committed upload.

**Goal:** 1080p60 reliably with hardware encoding and no surprises.

| ID                       | Resolution | FPS | Bitrate  | Min upload |
|--------------------------|------------|-----|----------|------------|
| `prosumer-720p60`        | 1280×720   | 60  | 6 Mbps   | 8 Mbps     |
| `prosumer-1080p30-high`  | 1920×1080  | 30  | 9 Mbps   | 11 Mbps    |
| `prosumer-1080p60`       | 1920×1080  | 60  | 12 Mbps  | 14.5 Mbps  |

**Rate control:** VBR, same envelope shape as consumer but at higher
target rates. NVENC defaults to `-rc vbr` on this tier.

**Color:** Rec.709 SDR. Same as consumer — prosumer doesn't need BT.2020
unless they're stepping into broadcast tier.

**Audio:** 160 kbps AAC stereo. Headroom for music.

**Encoder preference:** NVENC → VideoToolbox → QuickSync → x264.

### Caveats

- `prosumer-1080p60` uses H.264 Level 4.2 — Level 4.1's macroblock-rate
  cap (245.76 k MB/s) can't carry 1080p60. Strict hardware decoders
  reject SPS mismatch.
- Capture cards exposing PC-range (0–255) data need a `format=yuv420p`
  filter before encode or the highlights clip on Rec.709 decoders. The
  ffmpeg supervisor handles this via its input filter chain.

## Tier 3 — Broadcast

**Target operator:** technical, broadcast-aware, runs QC on the egress.

**Hardware:** Blackmagic SDI ingest (Decklink Duo / Quad), redundant
network path, sample-rate-locked audio, reference monitors.

**Network:** committed-rate fiber, 25–50 Mbps sustained, ideally with
SLA.

**Goal:** True CBR with 1-second VBV, Rec.709 (or BT.2020 at 2160p) VUI
baked into the SPS, broadcast-safe color limiter applied at the filter
stage.

| ID                     | Resolution | FPS | Bitrate  | Min upload |
|------------------------|------------|-----|----------|------------|
| `broadcast-1080p30`    | 1920×1080  | 30  | 8 Mbps   | 11 Mbps    |
| `broadcast-1080p60`    | 1920×1080  | 60  | 15 Mbps  | 18 Mbps    |
| `broadcast-2160p30`    | 3840×2160  | 30  | 35 Mbps  | 42 Mbps    |

**Rate control:** True CBR. `VideoBitrate == VideoMaxBitrate ==
VideoBufSize` — 1-second VBV. libx264 declares `nal-hrd=cbr:filler=1`
in `-x264-params` so the HRD parameters in the SPS VUI describe a
constant rate, and NAL filler is inserted to pad under-budget frames
up to the rate.

**Color:**

- HD (1080p): Rec.709 primaries / transfer / matrix, 8-bit chroma.
- UHD (2160p): BT.2020 primaries, Rec.709 transfer (SDR-2020), 10-bit
  chroma (`yuv420p10le` — BT.2020 mandates 10-bit per ITU-R BT.2020-2).
  HDR variants (PQ / HLG) would flip `ColorTransfer` to `smpte2084` or
  `arib-std-b67`; that work is gated behind explicit HDR support in
  the supervisor and is not in the current preset list.

**Audio:** 192 kbps AAC stereo at 48 kHz. Headroom for music and speech
without dipping into AAC artifacts.

**Encoder preference:** x264 → NVENC → QuickSync → VideoToolbox. x264
is preferred because VideoToolbox's "soft CBR" historically undershoots
on low-motion content — the broadcast tier wants the encoder that hits
the declared rate to the bit.

**Broadcast-safe limiter:** the `BroadcastSafe` flag on every broadcast
preset tells the supervisor's filter chain to insert a `limiter=16:235`
(luma) and `16:240` (chroma) clamp before encode. A source with
out-of-range whites (a confidence monitor passing through PC-range
graphics, a Decklink card pumping PC range) won't trip downstream QC
systems.

### Caveats

- `broadcast-2160p30` is HEVC only. The H.264 level for 2160p30 would
  be 5.1; we list 5.1 in the preset for argv consistency but the
  recommended encoders are `hevc_nvenc`, `hevc_videotoolbox`, and
  `libx265`. Most live destinations now accept HEVC for 2160p.
- 1-second VBV is unforgiving. Network drops manifest as visible
  blocking — the encoder has no buffer to spend through congestion.
  Production engineers run a hot-standby uplink for this tier.

## Tier 4 — Cinema

**Target operator:** cinema-camera owner who cares about cadence.

**Hardware:** Sony FX3 / Blackmagic Pocket 6K / similar cinema camera,
calibrated monitors, capture-card-clean signal path.

**Network:** any tier with the bandwidth — cinema's defining constraint
is cadence, not bitrate.

**Goal:** preserve 23.976 or 24.000 cadence end-to-end. Run with closed
GOP, no scenecut keyframes, and `-fps_mode cfr` so duplicate-PTS frames
never sneak into the muxer.

| ID                      | Resolution | FPS    | Bitrate | Min upload |
|-------------------------|------------|--------|---------|------------|
| `cinema-1080p2398`      | 1920×1080  | 23.976 | 12 Mbps | 16 Mbps    |
| `cinema-1080p24`        | 1920×1080  | 24.000 | 12 Mbps | 16 Mbps    |
| `cinema-2160p2398`      | 3840×2160  | 23.976 | 40 Mbps | 55 Mbps    |
| `cinema-2160p24`        | 3840×2160  | 24.000 | 40 Mbps | 55 Mbps    |

**Rate control:** VBR with generous headroom (2-second VBV). The
24p cadence is forgiving on burst tolerance — there are half as many
frames per second to cushion any one hard scene.

**Color:** HD cinema stays on Rec.709; UHD cinema uses BT.2020 primaries
with Rec.709 transfer (SDR-2020). HDR cinema would require expanding
the preset matrix with PQ / HLG transfer entries and is intentionally
out of scope for this revision.

**Audio:** 192 kbps AAC stereo at 48 kHz.

**Encoder preference:** x264 / x265 → NVENC → VideoToolbox. Software
encoders are preferred at this tier because film grain and shallow
depth-of-field benefit from x264's psychovisual rate-control over
hardware encoders' simpler RC models.

### Caveats

- **NTSC-cinema 23.976 must be encoded as `24000/1001`, not as the
  rounded `23.976`.** The exact ratio survives service-length streams
  without drift; 23.976 (truncated to a float) drifts approximately
  3.6 seconds per hour and the muxer eventually drops a frame to
  compensate. The Preset's `FPS.Expr()` emits `24000/1001` for the
  `2398` variants — do not bypass this with a manual `-r 23.976`.
- **`-fps_mode cfr` is mandatory.** Source must already be at the
  target rate. A 30 fps source piped into a 24p preset will produce
  judder no rate-control flag can hide. If you need to convert 30 fps
  to 24 fps, do it with `fps=24000/1001` in the input filter chain
  before the encoder — and accept that the result will show telecine
  artifacts.
- **23.976 and 24.000 are not interchangeable.** Cinema cameras
  shooting integer-24 (some Pocket 6K modes, all Cinema EOS C300
  modes) will refuse to match the playback rate of a 23.976 stream
  on frame-accurate playback. Pick the one your source emits.
- The recommender never silently hands a cinema preset to a generic
  operator — `CinemaCadence` must be set on `Constraints` explicitly.

## Recommender behaviour

`quality.Recommend(Constraints)` returns the best-fitting preset for an
operator's constraints. The algorithm:

1. Filter by `UploadKbps` (presets with `MinUploadKbps > available`
   drop out).
2. Filter by `AvailableEncoders` (presets whose `RecommendedHW` list
   doesn't intersect the available list drop out).
3. Filter by `PreferredTier` when set, or filter out cinema when the
   operator hasn't asked for cinema cadence.
4. If `TargetFPS` is set, prefer presets at that exact rate.
5. Return the highest-bitrate preset in the remaining set — bitrate
   correlates with resolution × framerate × intended quality within
   a tier.

Returns the default preset (`consumer-1080p30`) and `false` when
nothing matches. Callers should treat the `false` case as "the
operator can't actually run any preset; either upgrade the network
or accept the fallback below the safety floor".

## Integration notes

The preset's `BuildFFmpegArgs(p, encoder)` produces the encoder-side
fragment of the ffmpeg argv. The supervisor (in `internal/ffmpeg/`,
owned by a parallel agent) splices this between the input flags and
the output muxer flags. The current `ffmpeg.go` carries a
`// TODO(integration)` comment pointing at the exact splice site.

`BuildFFmpegArgs` does **not** emit:

- Input flags (`-f`, `-i`, capture format, audio device).
- Audio encoder selection (`-c:a` — platform-dependent codec pick
  belongs in the supervisor).
- Output muxer flags (HLS segment options, RTMP / SRT URL).

It **does** emit:

- Color signalling (`-color_primaries`, `-color_trc`, `-colorspace`,
  `-color_range`) **before** `-c:v` so hardware encoders bake VUI
  hints into the SPS.
- `-c:v` plus the encoder's bitrate / profile / level / rate-control
  flags.
- `-fps_mode cfr` and `-r preset.FPS.Expr()` to lock cadence.
- `-pix_fmt`, `-g`, `-keyint_min` (or encoder-native equivalents).
- `-force_key_frames` for VideoToolbox (the only reliable IDR
  cadence on that encoder).
- Audio rate envelope (`-b:a`, `-ar`, `-ac`).
