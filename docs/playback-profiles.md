# Playback Profiles

EasyCampus decoder-side playback is configured via *profiles*: named
bundles of mpv flags that together produce one consistent display
behaviour. Profiles are the symmetric counterpart to the encoder-side
quality presets in `docs/quality-presets.md` — the encoder side picks a
bitrate/resolution/cadence envelope; the decoder side picks how to
deliver those frames to the local display or SDI card.

The catalog lives in `internal/playback/profile.go`. Each profile
contributes a fixed set of mpv flags via `Profile.MPVArgs()`; the
decoder service (`internal/app/mpv.go`) merges that fragment with the
common base flags (IPC socket, cache, idle/pause).

## Why profiles exist

The default mpv argv that shipped before this system was
`--vo=gpu --hwdec=auto`, no `--video-sync`, no `--interpolation`,
no `--display-fps-override`. That's fine when the source rate evenly
divides the display refresh — 30 fps on a 60 Hz monitor is a clean 1:2
duplication, no judder. It breaks down in two scenarios:

1. **Cinema content (24 fps) on a non-multiple refresh rate.** A 30 Hz
   projector receiving 24 fps frame-doubles unevenly: out of every five
   source frames, one gets shown for 2 display frames and four get
   shown for 1 display frame each (the 1:2-1:1-1:1-1:1 pattern). The
   eye sees this as judder during slow pans.
2. **SDI broadcast output.** Decklink cards lock to a SMPTE-derived
   clock and want frames delivered at the card's rate, not the
   display's. Software interpolation on top would introduce phase
   noise relative to the SMPTE reference.

Profiles solve both: cinema-to-30Hz judder by enabling
`--interpolation=yes`; SDI by switching the output stage and dropping
hardware decode.

## The seven profiles

| ID                     | --vo      | --video-sync     | Interp | --display-fps-override | --hwdec | Use case                              |
|------------------------|-----------|------------------|--------|------------------------|---------|---------------------------------------|
| `auto`                 | gpu       | *(not set)*      | no     | *(not set)*            | auto    | Safe fallback; pre-existing behavior. |
| `projector-24`         | gpu       | display-resample | no     | 24                     | auto    | True 24Hz cinema projector.           |
| `projector-30`         | gpu       | display-resample | **yes**| 30                     | auto    | 30Hz projector + 24fps cinema.        |
| `projector-60`         | gpu       | display-resample | no     | 60                     | auto    | 60Hz projector.                       |
| `tv-60`                | gpu       | display-resample | no     | 60                     | auto    | US/JP consumer TVs.                   |
| `tv-50-pal`            | gpu       | display-resample | **yes**| 50                     | auto    | UK/EU 50Hz TVs.                       |
| `decklink-broadcast`   | decklink  | display-vdrop    | no     | *(not set)*            | **no**  | SDI / broadcast.                      |

### `auto`

The starting point. Emits no `--video-sync`, no `--interpolation`, no
`--display-fps-override`; mpv applies its own defaults. Functionally
identical to the pre-profile decoder behaviour, so existing deployments
see no change unless an operator explicitly switches.

Pick `auto` when you don't yet know what the display does. Pick a
specific profile only when you've measured judder or the deployment
matches one of the use cases below.

### `projector-24`

For a true-24Hz projector receiving 23.976 / 24 fps cinema content
(rare in church / venue installs, common in screening rooms and
post-production review). `--video-sync=display-resample` keeps the
audio-video sync stable across mpv's hour-plus playback; the override
to 24 is defensive in case EDID lies about the panel rate.

No interpolation — source and display are the same rate; interpolation
would only add latency without changing the visible image.

**CPU impact:** minimal. Resampling at 24Hz is cheaper than at 60Hz
and there's no motion-compensation work.

**Known gotchas:**
- `--display-fps-override=24` lies if your projector's EDID reports
  60Hz (some 4K projectors expose only 60Hz at the EDID level even
  when driven from a 24Hz source). When this happens, set the actual
  projector mode in the OS display settings before launching mpv.

### `projector-30`

**The user's exact case.** 30Hz projector receiving 24 fps cinema
content. Naive 24→30 frame-doubling produces the 1-2-1-2-1-3 pattern
(four out of five source frames shown for one display frame, one shown
for two display frames) and the resulting cadence judders visibly on
slow pans and dolly shots.

`--interpolation=yes` is the fix: mpv synthesizes intermediate frames
so the on-screen motion is smooth at 30Hz even though the source is
24fps. The combination of `display-resample` plus `interpolation` plus
the explicit `display-fps-override=30` is what makes this work.

**CPU impact:** noticeable. Frame interpolation runs a motion-estimate
algorithm per output frame. On a Mac mini M1 this consumes ~30-40% of
one performance core during 1080p playback; on older Intel mini
hardware, plan for ~70% of one core. Hardware-decode (`--hwdec=auto`)
keeps the decode itself cheap.

**Known gotchas:**
- Interpolation works best on continuous motion. Hard cuts (scene
  changes) can produce one-frame artifacts where the interpolator
  guesses motion between unrelated frames; mpv handles this by
  detecting the cut and skipping interpolation for that frame.
- If the projector is actually 30Hz but advertises 60Hz over EDID,
  `--display-fps-override=30` is load-bearing — without it mpv will
  resample to 60 and you'll still see the 24→60 cadence (which 60Hz
  handles cleanly, so it's not a disaster, but the override matches
  the actual panel).

### `projector-60`

60Hz projector receiving 24 fps cinema. Unlike 24→30, the 24→60
relationship is a clean 2.5× resample that mpv handles smoothly via
`--video-sync=display-resample` alone — no interpolation needed.
60Hz panels also handle 30→60 (clean 2×) and 60→60 (identity) without
trouble.

**CPU impact:** very low. No interpolation; resampling at 60Hz is the
common path and mpv's optimisation focus.

**Known gotchas:**
- If the source is 50 fps (PAL broadcast), 50→60 needs interpolation
  to avoid judder — use `tv-50-pal` instead, or move the display to
  a refresh rate that divides 50 evenly.

### `tv-60`

For US / Japan consumer TVs and standard PC monitors at 60Hz. Identical
flag set to `projector-60` but separately named so the operator's
intent is clear in the UI and logs ("TV" implies a small or medium
panel viewed at TV distance; "projector" implies a large image viewed
from further back, where judder is more visible).

**CPU impact:** very low.

**Known gotchas:**
- Modern TVs (especially 120Hz "motion smoothing" panels) sometimes
  insert their own frame interpolation on top of mpv's output. The
  result is double-interpolation and a noticeable "soap opera effect".
  Disable the TV's motion-smoothing setting when using any profile
  that enables `--interpolation`.

### `tv-50-pal`

UK / EU consumer TVs at 50Hz. The defining problem is that almost no
source rate evenly divides 50:

- 24 → 50 is 1:2-1:2-1:2-1:2-1:3 repeating (one frame in five shown
  for 3 display frames; the rest for 2). Judder is unavoidable
  without interpolation.
- 30 → 50 is 3:5 — every two source frames take five display frames,
  producing a similar cadence problem.
- 25 → 50 is the only clean case (1:2), but 25 fps sources are rare
  outside legacy PAL broadcast.

Interpolation makes the cadence smooth at the cost of CPU.

**CPU impact:** same as `projector-30` — interpolation is the dominant
cost.

**Known gotchas:**
- Some EU streaming services intentionally encode at 25 fps for PAL
  compatibility. With 25 fps source on a 50Hz TV, `tv-50-pal` still
  works but interpolation is wasted CPU — pick `auto` or build a
  custom 25-on-50 profile if this becomes a frequent deployment.

### `decklink-broadcast`

For SDI output to a Blackmagic Decklink card. Three things change vs
the desktop profiles:

1. `--vo=decklink` routes frames to the SDI board instead of the
   compositor / GPU surface.
2. `--video-sync=display-vdrop` drops frames to align with the card's
   SMPTE clock (the card has its own clock; mpv yields to it).
3. `--hwdec=no` forces software decode. Hardware decoders have
   non-deterministic timing on macOS especially (VideoToolbox can
   reorder by tens of milliseconds); broadcast wants every frame
   delivered to the card at a known time relative to the SMPTE
   reference.

Interpolation is **off**. Broadcast cards emit frames at the SDI rate
they're configured for (set via `--decklink-format=...` from the
existing UI control); inserting interpolated frames upstream of the
card adds phase noise without changing the SMPTE-locked output rate.

**CPU impact:** moderate. Software decode at 1080p / 1080i is fine on
any current CPU; 2160p software decode is harder and may require
manually picking a smaller `--decklink-format` if the host can't
sustain the decode.

**Known gotchas:**
- `--display-fps-override` is intentionally unset. Decklink cards
  ignore display-related flags; the SDI clock comes from the card's
  format setting.
- `--decklink-format` is supplied at runtime by the existing UI
  control. The profile's `DecklinkFormat` field is empty in the
  catalog; the handler splices the per-request value into the
  resolved profile before calling `MPVService.Start`.
- Software decode does NOT mean software-only mpv. The output stage
  is still GPU-aware for chroma conversion; only the H.264 / HEVC
  decode itself runs on the CPU.

## Selection guidance

Pick the profile that matches the display's refresh rate and the
source content's frame rate.

| Display | Source 24 fps | Source 30 fps | Source 60 fps |
|---------|---------------|---------------|---------------|
| 24Hz    | `projector-24` | (not recommended; pick a 60Hz display) | (not recommended) |
| 30Hz    | `projector-30` (interp ON) | `auto` | (not recommended) |
| 50Hz    | `tv-50-pal` (interp ON) | `tv-50-pal` (interp ON) | (not recommended; pick 60Hz) |
| 60Hz    | `projector-60` or `tv-60` | `tv-60` | `tv-60` |
| SDI     | `decklink-broadcast` | `decklink-broadcast` | `decklink-broadcast` |

When unsure, leave the profile on `auto` — it preserves the prior
behaviour and you can switch once you've observed actual playback.

## API and persistence

### HTTP endpoint

```
GET /api/playback-profiles

{
  "profiles": [
    {"id":"auto","label":"Auto","description":"...",
     "video_sync":"audio","interpolation":false,
     "display_fps_override":0,"hwdec":"auto",
     "output_mode":"gpu"},
    ...
  ],
  "default": "auto"
}
```

The decoder UI fetches this on load and renders a `<select>` beside
the existing screen/hardware-output controls.

### Request body

`POST /api/output/start` accepts an optional `playback_profile` field:

```json
{
  "url": "http://encoder.example/master.m3u8",
  "fullscreen": true,
  "playback_profile": "projector-30"
}
```

Resolution order:

1. Explicit `playback_profile` in the request.
2. `AppSettings.PlaybackProfile` (config file / env var).
3. Legacy `output_mode == "decklink"` → `decklink-broadcast`.
4. `playback.Default()` (`auto`).

### Configuration

In `~/.easycampus/config.yaml`:

```yaml
playback_profile: "projector-30"
```

Or via environment variable:

```sh
EASYCAMPUS_PLAYBACK_PROFILE=projector-30 easycampus
```

The operator's last UI choice is persisted in browser `localStorage`
(key `oc_dec_playback_profile`) so it survives a page reload without
requiring a server-side write.

## Integration notes

`Profile.MPVArgs()` is a pure function. It returns the flag fragment
this profile contributes; the decoder service in
`internal/app/mpv.go` assembles the full argv by combining:

- The URL and `--input-ipc-server=...` (always).
- `profile.MPVArgs()` (the variable part).
- Window geometry / fullscreen flags (GPU output stage only).
- Common base flags (`--idle=yes`, `--pause`, `--mute=yes`, `--no-osc`,
  `--osd-level=0`, cache and demuxer tuning).

The profile does **not** emit:

- The URL.
- The IPC socket path.
- Window geometry, fullscreen, screen-index flags.
- Cache / demuxer / OSD flags.

It **does** emit:

- `--vo=<gpu|decklink>` (always; defaults to gpu).
- `--hwdec=<auto|no|...>` (always; defaults to auto).
- `--video-sync=<audio|display-resample|display-vdrop>` (only when
  the profile explicitly sets one — `auto` leaves this off).
- `--interpolation=yes` (only when enabled; mpv's default is no).
- `--display-fps-override=<N>` (only when non-zero).
- `--decklink-format=<...>` (only when output_mode is decklink AND a
  format was supplied; the catalog leaves this empty, the handler
  splices the UI-supplied value in).
