# easycampus-go Architecture Review

Branch: `easystream-alignment`. Reviewer's mandate: written advice only, no code changes. Compare against the sibling project `easystream` (Go 1.25, `cmd/` + `internal/` layout, ~10k lines, 12 test files) and recommend a path forward.

`IMPLEMENTATION_STATUS.md` is stale (it lists half the routes as "Not Yet Implemented" but they are wired in `main.go:74-187`). Delete or rewrite it; the routes table below replaces it.

---

## 1. File-by-file findings

### `main.go` (247 lines)
- Global mutable state: `appSettings`, `ffmpegService`, `mpvService`, `cacheService`, `detectionService` declared in `handlers_common.go:10-15` and assigned at `main.go:24,44-47`. This is the entire reason every handler is a free function rather than a method on a `Server` struct. It blocks tests, blocks running two instances in-process, and forces import-time singletons.
- `http.ListenAndServe` at `main.go:202` has no `ReadHeaderTimeout`, no `Shutdown`, no signal handling. A SIGTERM kills ffmpeg children mid-segment and leaves orphan processes — exactly the failure mode `easystream/internal/preview/preview.go:56` (`ReapOrphans`) exists to clean up.
- Route duplication `main.go:139-197`: every `/foo` is also registered as `/api/foo`. This is HTML/legacy compat from the Python port, but it doubles the surface area without any indirection — change one, forget the other.
- Catch-all closures `main.go:107-118, 127-137, 168-178, 187-197` inline path-prefix routing logic into anonymous handlers. They reimplement `pathSegmentAfter` (`handlers_system.go:80`) ad-hoc. Move to Go 1.22+ pattern-based mux (`mux.HandleFunc("DELETE /events/{id}", ...)` like easystream does at `server.go:425`).
- `serveHTML` reads `encoder.html`/`decoder.html` from disk on every request (`main.go:238-246`). easystream embeds with `//go:embed` (`internal/ui/ui.go`) — same approach should be used here so binaries are single-file deployable.
- CORS in `withMiddleware` is `*` for everything including credentials-bearing routes. There's also no panic-recovery — a panic in any handler crashes the whole process.

### `handlers_common.go` (33 lines)
- `writeJSON` discards encode errors (`handlers_common.go:20`). easystream's equivalent at `server.go:595-609` writes a fallback error body — adopt that pattern.
- Global singletons (see above). The fix here is `type Server struct { settings AppSettings; ffmpeg *FFmpegService; ... }` and methods on it.

### `handlers_stream.go` (135 lines)
- `handleStreamStop:52` reads `ffmpegService.currentEventID` without a lock — a concurrent `StartStream` call could race. There's no mutex on `FFmpegService` at all.
- Event-info write at `:36` is non-atomic `os.WriteFile` — a crash mid-write yields a truncated `info.json` that fails `json.Unmarshal` at `events.go:106`, silently dropping the event from the list (`events.go:51-53` `continue`). easystream solved this with `internal/atomicfile`.
- `parseStreamConfig:90-134` accepts two aliases for every field (`event_name`/`name`, `video_framerate`/`framerate`, `bitrate`/`video_bitrate`, `video_device_index`/`input_id`). This is python-port residue; nail the JSON contract down.
- No validation: a client can POST `framerate: -1` and FFmpegService.StartStream will reject only the negative case (`ffmpeg.go:167`). Resolution, bitrate range, device-id format — all unchecked.

### `ffmpeg.go` (268 lines)
- `IsRunning()` at `:27-29` checks `ProcessState == nil` to mean "still running." This is wrong as written: `ProcessState` is set only after `Wait()` returns, and nothing calls `Wait()` on the stream path (only `StopStream` does). So an ffmpeg that died on its own keeps reporting `IsRunning() == true` until someone stops it. Status reads `lastError` only after a stop call has already mutated state — see also `GetStatus:248-251` which checks `ProcessState != nil` for exit-code reporting, but again nobody reaps the child.
- No `context.Context`. `StopStream:209-235` does a 5-second hard timeout on a goroutine without selecting on a parent ctx.
- `buildHLSCmd` hardcodes `1280x720` (`:83,95,100`) regardless of preset. easystream's quality presets at `internal/quality/quality.go` are the right model.
- `cmd.Stdout = combined; cmd.Stderr = combined` (`:188-189`) writes to both a log file *and* `os.Stderr`. There's no log rotation; an 8-hour service writes hundreds of MB to the event dir. The log file `f.logHandle` is closed in `StopStream`, but if `cmd.Start()` succeeds and the process later exits independently, no one closes the handle — fd leak.
- `ForceKillPID:237-246` will call `terminateProcess` on any arbitrary PID the caller supplies (`handlers_stream.go:73`). The PID isn't checked against `os.Getppid()`, our own children list, or anything. A client can POST `pid=1` and we'd happily try.
- `currentEventID`, `startedAtMono`, `startedAtWall`, `logHandle`, `lastError` are all mutated without synchronization across `StartStream`/`StopStream`/`GetStatus`. Race-detector would flag this.

### `ffmpeg_devices.go` (196 lines)
- Spawns `ffmpeg -list_devices` on every `/inputs` request (`:35, :108, :144`) — that's a 200-500ms hit per call, and `ffmpeg_devices.go:31` returns *combined* results across all backends including the always-failing decklink/NDI probes on systems without those plugins. easystream's `internal/devices/devices.go:67-80` caches for 5s.
- `listV4L2Devices:74` shells out to `v4l2-ctl` per device serially — should be parallelized or cached.
- Linux DRM/screen-capture detection isn't even attempted; easystream's `devices.DeviceType` taxonomy is richer (`devices.go:20-27`).

### `mpv.go` (338 lines)
- Hardcoded `/tmp/easycampus_mpv.sock` (`:24`) — second instance on same host overwrites. Should be in the per-process data dir.
- `Start:113` blocks 1 second with `time.Sleep` to "let mpv come up". Replace with a connect-retry loop on the IPC socket.
- `IsRunning:130-143` calls `Signal(syscall.Signal(0))` on Unix to probe, but the Windows branch (`runtime.GOOS == "windows"`) skips this check entirely — a hung mpv on Windows would report running forever.
- `sendIPC:145-197` does a per-call Dial + scan + 3s deadline. Every property read = a new Unix socket connection. Cheap on the same box but wasteful. Hold one persistent conn behind a mutex.
- `getProperty` returns `interface{}` and callers do `.(float64)` (`:215, :235, :237`). Untyped, brittle, and the `Bookmark.Frame` int truncation at `:240` silently drops fractional frames.
- `GetDiagnostics:316-333` stuffs error strings into the same map as values (`"error: " + err.Error()`) — a client parsing the response can't distinguish a real "seekable" boolean from the string `"error: ..."`.
- No tests. The mpv IPC layer is the obvious unit-testable piece (stub the socket).

### `cache_service.go` (198 lines)
- Race window in `StartCaching:30-44`: between dropping the lock at `:38` and the goroutine starting at `:43`, a `StopCaching` arriving on a different goroutine cancels the context the goroutine hasn't observed yet — benign in practice but the pattern is fragile. Acquire ctx inside the lock.
- `downloadLoop:89` does a hardcoded `time.Sleep(2 * time.Second)` (`:134`) instead of `select { case <-ctx.Done(): case <-time.After(...) }`. So a `Stop` call waits up to 2 seconds for cancellation to land — and worse, the next iteration's `c.fetchText` (no per-request context) can hang for the client's 10s timeout even after ctx is cancelled.
- `fetchText:138`/`downloadFile:154` use `c.client.Get(url)`, not `NewRequestWithContext`. Cancellation can't preempt an in-flight HTTP fetch.
- `parseSegments:175-197` is a fragile m3u8 parser. It treats anything not starting with `#` as a segment URL — fine for the simple playlists this server emits, but `#EXT-X-BYTERANGE` followed by a URI on the *next* line is mis-parsed. easystream doesn't have this code because it doesn't pull from peers — but if you keep DVR caching, use a proper m3u8 lib (e.g. `github.com/grafov/m3u8`).
- `parseSegments` extracts the init segment from `#EXT-X-MAP:URI=` (`:184`) but does no path-relativity handling — an absolute URL there would be downloaded to `outputDir/<full-url>`.

### `detection_service.go` (172 lines)
- Display detection shells out to `system_profiler` / `xrandr` / `powershell` on every call. Cache it.
- Returns `[]map[string]interface{}` instead of typed structs (`:17`). Same shape as the rest of the file — define `Display` and `DecklinkInfo` types in `models.go`.
- `CheckDecklinkAvailable:30` runs `mpv --vo=help` just to grep `decklink` — needs an `mpv` binary check first. If mpv isn't installed the function returns `available:false` silently with no log line.

### `config.go` (287 lines)
- Hand-rolled YAML serialization via `fmt.Sprintf` (`:205-255`). If a user puts a `"` in their `EncoderHost`, the YAML breaks. Use `yaml.Marshal` and add comments via `yaml.Node`, or accept that round-tripping doesn't preserve comments and stop trying.
- `getConfigPath:264-275` *always* returns `~/.easycampus/config.yaml` if `UserHomeDir()` succeeds — the "fallback to local directory" branch at `:274` is unreachable. Dead code.
- `loadConfig` returns a value rather than `(AppSettings, error)`; parse errors are logged-and-ignored (`:81`). A typo in YAML silently reverts to defaults.
- No env-var precedence documentation; the file mixes "set on first run" with "override on every run." Document or rewrite as flag → env → file → default (easystream uses `envDefault` at `cmd/easystream/main.go:186-191` — simpler).
- `Debug: true` is the default (`:55`). Wrong sign for a production app.

### `models.go` (122 lines)
- `time.Time` in `EventInfo.StartedAt` is fine but `EventInfo.CreatedAt` is `time.Time` while `EventInfoFile.CreatedAt` (`events.go:14`) is `string`. Two parallel models for the same data, parsed manually at `events.go:55-57`. Define one and convert on the file boundary.
- `Bookmark.Frame int` truncates from MPV's `estimated-frame-number` float (`mpv.go:240`) — fine for SD/HD content but a 4-hour 60fps service overruns int32 only at 596 hours, so functionally fine. Type as int64 anyway.
- `PlayerStartRequest.ScreenIndex int` at `:77`: handler treats `0` as "unset, default to 1" (`handlers_output.go:28-30`). Use `*int` to disambiguate "user picked screen 0" from "user didn't pick."

### `events.go` (167 lines)
- `listEvents:35` walks every event dir and computes size on every call — no caching. For a server with hundreds of recordings this is an O(events × files) syscall storm per request.
- `deleteEvent:112` reads `info.json`, finds a PID, kills it (`:124`). The PID could now belong to a completely different process if the recording ended hours ago and the OS recycled the PID. Check `isProcessRunning` first AND verify it's actually ffmpeg (proc cmdline match) — easystream does this in `internal/ffmpeg/pidfile.go`.
- `EventInfoFile.PID` is a `string` (`:22`). Why? Either it's a number or it's `""`. Use `*int`.

### `bookmarks.go` (119 lines)
- All file IO is non-atomic — concurrent POST + DELETE to the same event's bookmarks would race (last writer wins, possibly with a partial write). Use atomicfile-style write + a per-event mutex.
- ID generation `"bm_" + padInt(len(bookmarks)+1, 3)` (`:49`) collides if you delete bookmark 2 of 3, then create a new one (now 3 of 3, but the original bookmark 3 still has id `bm_003`). Use a UUID or unix-nano.

### `util.go` (117 lines)
- `terminateProcess:22-39` does `Interrupt` → `Sleep(500ms)` → `Kill`. The sleep blocks the caller and ignores whether the process actually exited. Use `process.Wait()` with a timer.
- `isProcessRunning:41-55` on Windows sends `os.Interrupt` as the liveness probe (`:50`) — which on Windows is actually equivalent to "try to send Ctrl+C", a real signal that can shut things down. Should be a no-op probe (`OpenProcess` + `GetExitCodeProcess`).
- `valueString`/`valueInt`/`valueFloat` are reflection-style coercion over `map[string]interface{}`. Replace by decoding into typed structs (you already have `StreamConfig` — actually use it).

### Cross-cutting
- **Zero tests.** `find . -name '*_test.go'` returns nothing. easystream has 12 test files.
- **No `context.Context` plumbing** through handlers. The only `Context` use is in `cache_service.go`.
- **No graceful shutdown.** No signal trap, no `Server.Shutdown`, no ffmpeg/mpv child reap-on-exit. Compare `cmd/easystream/main.go:169-177`.
- **`h264_videotoolbox` hardcoded** at `handlers_upload.go:78, 206, 364`. Non-portable to Linux/Windows. easystream's encoder selection (`internal/ffmpeg/config.go` + `Encoder` field) is the right model.

---

## 2. Layout assessment

Everything in `package main`. Refactor target (mirroring easystream):

```
easycampus-go/
├── cmd/easycampus/main.go      # flag parsing, signal handling, DI
├── internal/
│   ├── app/                        # Server struct, routes, handlers
│   │   ├── server.go
│   │   ├── handlers_stream.go
│   │   ├── handlers_events.go
│   │   ├── handlers_output.go      # mpv-driven decoder
│   │   ├── handlers_cache.go
│   │   └── handlers_upload.go
│   ├── ffmpeg/                     # supervisor, cmd builder, devices
│   ├── mpv/                        # IPC client + supervisor
│   ├── store/                      # event metadata, bookmarks (atomic)
│   ├── cache/                      # HLS DVR puller
│   ├── config/                     # load/save settings
│   ├── detect/                     # displays, decklink
│   ├── atomicfile/                 # copied from easystream
│   └── ui/                         # embedded encoder.html / decoder.html
└── go.mod
```

The natural seams are visible in the current filenames — `ffmpeg.go`, `mpv.go`, `cache_service.go`, `detection_service.go`, `bookmarks.go` map almost 1:1 to packages. The main translation work is killing the global singletons in `handlers_common.go:10-15` and threading dependencies through a `Server` struct.

---

## 3. What's missing

Compared to a production Go HTTP service:

- **Tests.** All packages need at least table-driven unit tests. ffmpeg arg construction, m3u8 parsing, config load/save, MPV property decoding — all pure-input/pure-output, all trivially testable.
- **`context.Context`** on every handler and through to `FFmpegService.StartStream`, `MPVService.sendIPC`, `CacheService.fetchText`. Right now there's no way to abort a slow ffmpeg startup or a hung mpv socket read.
- **Graceful shutdown.** SIGTERM trap → `http.Server.Shutdown(ctx)` → `ffmpegService.StopStream()` → `mpvService.Stop()` → `cacheService.StopAll()`. Currently a `kill -TERM` leaves orphans.
- **Structured logging.** `log.Printf` everywhere. Switch to `log/slog` (Go 1.21+ stdlib) with at minimum `event_id`, `pid`, `phase` fields. Logs are operationally the only debug tool when the camera mysteriously won't start on Sunday.
- **Config validation.** `loadConfig` doesn't return an error. Add `Validate()` that rejects nonsense (negative bitrate, unknown node type, missing `EncoderHost` when type=decoder, etc.). Mirror `ffmpeg.Config.Validate()` in easystream at `internal/ffmpeg/config.go:Validate`.
- **Request validation.** Every handler does `readJSON(r, &body)` into `map[string]interface{}` (`handlers_stream.go:17`) and dredges fields with `valueString`. Define DTOs and decode into them.
- **CORS.** The current `*` policy with `Allow-Methods: ... DELETE` (`main.go:213`) means any web page on the internet can DELETE your events. Restrict to known origins, or require an auth token.
- **Panic recovery.** Any panic in a handler crashes the process. Wrap the mux as easystream does at `server.go:619-631`.
- **Metrics.** No `/metrics`, no Prometheus, nothing. For a single-host streaming box this is optional — but at minimum expose request counts and active ffmpeg PIDs as JSON on `/health`.
- **Build/release.** No `Makefile`, no `goreleaser`, no Dockerfile. easystream has `.goreleaser.yaml`.

---

## 4. The merge question

**Recommendation: (b) keep separate but extract a shared `livestream-core` library.**

Scoring:

| Dimension | Overlap |
|---|---|
| **Domain** | High. Both wrap FFmpeg to produce live video. |
| **Audience** | Low. easystream targets churches scheduling YouTube/RTMP broadcasts. easycampus-go targets a 2-node encoder/decoder pair on the same LAN for community meetings/local playback. Different deployment story, different operator. |
| **API surface** | Low. easystream has `/api/youtube/*`, `/api/schedules`, `/api/preview/webrtc/*`. easycampus-go has `/output/*` (mpv-driven local playback), `/cache/*` (DVR pull), `/events/*/bookmarks`. Only `POST /api/start`, `POST /api/stop`, `GET /api/devices` materially overlap. |
| **Lifecycle** | Different. easystream: scheduled cron → testing → live → complete, with a supervisor + adaptive bitrate over hours of unattended runtime. easycampus-go: ad-hoc start, optional DVR caching, local mpv playback. easystream has no playback path; easycampus-go has no scheduling, no destination broadcaster, no WebRTC preview. |
| **Code reuse** | High at the *primitive* layer (ffmpeg supervision, device enumeration, atomic file IO, HLS file serving, quality presets) — moderate-to-zero above it. |

A merge would force easycampus-go to inherit easystream's YouTube/OAuth/schedule code it doesn't want, and force easystream to inherit easycampus-go's mpv decoder and DVR cache it doesn't want. Both teams pay carrying cost for features they don't ship. Worse, the easystream UI design assumes one stream at a time with a single destination; easycampus-go's encoder/decoder pairing assumes two cooperating nodes.

A fully-separate path (c) is wasteful: easycampus-go's `FFmpegService` is a worse version of easystream's `Supervisor` (no restart logic, no progress parsing, no adaptive bitrate, no PID file reap, no audio fallback) and there's no reason to maintain two.

The library path (b) lets easycampus-go inherit easystream's mature primitives without taking on the broadcast/schedule baggage.

---

## 5. Extracting `livestream-core`

Reusable as-is (copy verbatim into `livestream-core/`):

- `internal/atomicfile/` (57 lines + tests). Zero dependencies, perfectly generic.
- `internal/hls/server.go` (72 lines). Trivial CORS-aware static HLS server. Generalize the URL prefix (currently `/hls/` is hardcoded at `:70`) via a constructor option.
- `internal/ffmpeg/pidfile.go`. The PID-file + orphan-reap pattern is something easycampus-go badly needs.
- `internal/ffmpeg/progress.go` (58 lines). FFmpeg `-progress` parser; useful for easycampus-go's upload conversion and live stream status.

Reusable with minor generalization:

- `internal/ffmpeg/supervisor.go` + `config.go`. The supervisor is the crown jewel — restart-with-backoff, stable-after, progress-stall detection. To use in easycampus-go: extract the `SupervisorConfig`/`Status`/`State` types and the run loop. The `quality.Preset` coupling at `config.go:17` should be parameterized; the YouTube ingest URL fields (`IngestURL`, `StreamName`) should be optional. The supervisor itself doesn't know about destinations — `Config.Args()` builds them.
- `internal/devices/devices.go` (452 lines). Caches device scans, categorizes by `DeviceType`. easycampus-go's `ffmpeg_devices.go` is a worse subset; replace it.
- `internal/quality/quality.go` (353 lines). Presets are generic. easycampus-go's `qualitySettings` map at `handlers_upload.go:16-22` is the strawman.
- `internal/preview/preview.go`. The WebRTC preview is genuinely useful for easycampus-go's encoder UI (currently has no preview at all). The dependency on pion/webrtc is heavy — keep it optional via a build tag or sub-module.

Stays in easystream only:
- `internal/youtube/`, `internal/schedule/`, `internal/ingest/srt/`, `internal/app/handlers_youtube.go`, `internal/app/handlers_schedule.go`.

Sketch of `livestream-core` API surface:

```go
package ffmpeg
// Supervisor, Config, Input, Status, State; Config.Args() builds argv;
// SupervisorConfig.PidFilePath enables orphan reap.

package devices
// Scanner.Scan() DeviceList (cached); Device.Type taxonomy.

package quality
// Preset; ByID(string); Presets list.

package hls
// Server (dir-backed, CORS+cache headers); Clean(); ServeHTTP.

package atomicfile
// Write(path, data, mode).

package preview // optional, build tag webrtc
// Server (FFmpeg RTP → pion WebRTC).
```

Module path: `github.com/ssimpson89/livestream-core`. Versioned separately. Both apps import it. Migration is incremental: replace `ffmpeg.go` first, then `ffmpeg_devices.go`, then add the supervisor's restart/progress features as they prove useful.

---

## 6. Refactor punch list

1. **Add a Server struct + DI.** Delete the package-level vars in `handlers_common.go:10-15`. Move every handler to a method on `*Server`. This is the prerequisite for everything else; do it first.
2. **Wire graceful shutdown.** In `main.go:202`, replace `http.ListenAndServe` with `http.Server{}` + `signal.Notify` + `Shutdown(ctx)` calling `ffmpegService.StopStream()`, `mpvService.Stop()`, `cacheService.StopAll()`. Mirror `cmd/easystream/main.go:169-177`.
3. **Embed HTML.** Move `encoder.html`/`decoder.html` into `internal/ui` with `//go:embed`; drop the per-request `os.ReadFile` at `main.go:238-246`.
4. **Fix the FFmpeg liveness bug.** `ffmpeg.go:27-29` IsRunning is broken when the child dies on its own. Add a goroutine that `Wait()`s on the child (mirroring `handlers_upload.go:450-460`) and updates a guarded `running bool` field, OR replace `FFmpegService` wholesale with `livestream-core/ffmpeg.Supervisor`.
5. **Atomic event/bookmark writes.** Copy `internal/atomicfile/atomicfile.go` from easystream. Replace `os.WriteFile(infoPath, ...)` in `events.go:138`, `handlers_upload.go:486`, `bookmarks.go:34` with `atomicfile.Write`.
6. **Lock `FFmpegService`.** Add `sync.Mutex` covering `process`, `currentEventID`, `logHandle`, `lastError`, `startedAt*`. `Start/Stop/Status/CurrentPID` all need it.
7. **`context.Context` everywhere.** Thread `r.Context()` through handlers into `FFmpegService.StartStream`, `MPVService.sendIPC`, `CacheService.StartCaching`, `client.Get`. Use `http.NewRequestWithContext` in `cache_service.go:139,155`.
8. **Cache device scans.** `ffmpegService.ListInputDevices()` is called per `/inputs` request and shells out 3-4 times. Add a 5s-TTL cache like `devices.Scanner` in easystream. File: `ffmpeg_devices.go:13`.
9. **Typed DTOs for requests.** Define `StreamStartRequest`, `UploadRequest`, `BookmarkCreateRequest` in `models.go`; decode directly; delete `valueString/valueInt/valueFloat` from `util.go:57-107`.
10. **Replace 1280x720 hardcode.** `ffmpeg.go:83,95,100` + `qualitySettings` in `handlers_upload.go:16-22` should consult a single preset table (move to `internal/quality`).
11. **Pluggable encoder.** `h264_videotoolbox` at `handlers_upload.go:78, 206, 364` breaks Linux. Detect once at startup (`exec.Command("ffmpeg", "-encoders")` + grep) and choose `h264_videotoolbox` / `h264_nvenc` / `h264_qsv` / `libx264`. Mirror easystream's encoder selection.
12. **Panic recovery + structured logs.** Wrap the mux with a panic-recovering middleware (see `easystream/internal/app/server.go:619-631`). Replace `log.Printf` with `log/slog` and structured fields.
13. **Drop the `/api/*` duplicate routes** — or keep them as a thin alias and source-of-truth lives in one place. Currently they have separate `mux.HandleFunc` lines (`main.go:139-197`) — collapse to a loop that registers both forms.
14. **Restrict CORS** at `main.go:212-214`. `*` plus `DELETE` is the wrong default for anything that isn't local-only. At minimum gate destructive methods on `Origin` header matching the configured `EncoderHost`.
15. **Delete or rewrite `IMPLEMENTATION_STATUS.md`.** It misleads anyone trying to onboard. Replace with a short `ROUTES.md` listing the live routes from `main.go:56-187`.

Items 1–5 are prerequisites for everything else and roughly mirror the order easystream's git log followed when it was at the same stage. The rest can be paralleled across multiple sessions.

---

*~1480 words.*
