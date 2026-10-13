# HTTP Routes

Source of truth: `internal/app/server.go` `routes()`.

Most routes are registered under both a bare form and an `/api/`-prefixed mirror via the `register()` closure, so changing a handler in one place updates both forms. The Mirror column shows the pair; `—` means there is no second form. Exceptions to the mirroring:

- `/`, `/encoder`, `/decoder` have no `/api` form.
- `/api/start`, `/api/stop`, `/api/upload`, `/api/cache/events` are legacy aliases with no bare form.
- `/events` and `/api/events` are **not** mirrors: `/events` is GET-only, `/api/events` also accepts POST (external-source creation).

Errors are JSON `{"detail": "..."}` unless noted. Every handler returns 405 for the wrong method.

## Pages

| Method | Path | Mirror | Handler | Purpose |
|---|---|---|---|---|
| GET | `/` | — | `handleRoot` | Redirect to `/encoder` or `/decoder` based on `NodeType`. |
| GET | `/encoder` | — | `handleEncoderPage` | Embedded encoder dashboard HTML. |
| GET | `/decoder` | — | `handleDecoderPage` | Embedded decoder dashboard HTML. |
| GET | `/assets/*` | `/api/assets/*` | `ui.AssetsFS` | Static JS/CSS for the dashboards (embedded via `//go:embed`). |

## Stream

| Method | Path | Mirror | Handler | Purpose |
|---|---|---|---|---|
| POST | `/stream/start` | `/api/stream/start` | `handleStreamStart` | Start the FFmpeg encoder with the supplied config. Returns the stream status; **409** if a stream is already running. |
| POST | `/stream/stop` | `/api/stream/stop` | `handleStreamStop` | Graceful stop of the current encode. `{success}`. |
| POST | `/stream/force_stop` | `/api/stream/force_stop` | `handleStreamForceStop` | Body `{pid, event_id}`. SIGKILL the encoder when graceful stop hangs. Only PIDs the supervisor is tracking are accepted; any other PID is refused (403). 400 if `pid` is missing. Returns `{killed, killed_pid, message, stream}`. |
| POST | `/api/start` | — | `handleStreamStart` | Legacy alias for `/stream/start` (Python-port compat). |
| POST | `/api/stop` | — | `handleStreamStop` | Legacy alias for `/stream/stop` (Python-port compat). |

## Events

| Method | Path | Mirror | Handler | Purpose |
|---|---|---|---|---|
| GET | `/events` | — | `handleEvents` | List recorded events with size and duration. GET only. |
| GET/POST | `/api/events` | — | `handleAPIEvents` | GET is the same list as `/events`. POST only supports `{"type": "external", ...}` (same body as `/external-sources`, legacy `fps` accepted for `framerate`) and returns `{success, event_id, message, status: "processing"}`; any other `type` is 400. |
| GET | `/events/{id}/...` | `/api/events/{id}/...` | `eventsCatchAll` → file server | Serve event HLS segments and `info.json`. |
| GET/POST | `/events/{id}/bookmarks` | `/api/events/{id}/bookmarks` | `handleEventBookmarks` | List or create bookmarks on an event. |
| PUT/DELETE | `/events/{id}/bookmarks/{bid}` | `/api/events/{id}/bookmarks/{bid}` | `handleEventBookmarks` | Update or delete a specific bookmark. |
| DELETE | `/events/{id}` | `/api/events/{id}` | `handleEventDelete` | Delete an event and its recording directory. 204 on success, **404** for an unknown id, **409** while the event is live (currently recording). |
| POST | `/events/upload` | `/api/events/upload` | `handleUpload` | Upload a local file and convert into an event. |
| POST | `/api/upload` | — | `handleUpload` | Legacy alias for `/events/upload`. |
| GET | `/events/upload/{id}` | `/api/events/upload/{id}` | `handleUploadStatus` | Poll FFmpeg conversion progress for an upload. |

## Output (MPV)

| Method | Path | Mirror | Handler | Purpose |
|---|---|---|---|---|
| GET | `/output/capabilities` | `/api/output/capabilities` | `handleOutputCapabilities` | `{displays, decklink}`: detected displays and Decklink availability. |
| POST | `/output/start` | `/api/output/start` | `handleOutputStart` | Start MPV against the encoder's HLS URL (or a local file). |
| POST | `/output/stop` | `/api/output/stop` | `handleOutputStop` | Stop the MPV player. |
| GET | `/output/status` | `/api/output/status` | `handleOutputStatus` | `{is_running, message}`. Use `/output/position` for position/paused. |
| POST | `/output/update` | `/api/output/update` | `handleOutputUpdate` | Swap the playing source without restarting MPV. |
| POST | `/output/sync` | `/api/output/sync` | `handleOutputSync` | Command dispatcher. Body `{command, timestamp}` where `command` is `play`, `pause` or `seek`; `play`/`pause` also seek when `timestamp > 0`. Returns `{success, message}`; **400** on an unknown command, 500 if a seek fails. |
| POST | `/output/mute` | `/api/output/mute` | `handleOutputMute` | Set mute explicitly (not a toggle). Body `{"muted": true\|false}`. Returns `{success, message}`. |
| GET | `/output/position` | `/api/output/position` | `handleOutputPosition` | Current playback position in seconds. |
| GET | `/output/frame-position` | `/api/output/frame-position` | `handleOutputFramePosition` | Current playback position as a frame index. |
| GET | `/output/diagnostics` | `/api/output/diagnostics` | `handleOutputDiagnostics` | MPV property dump (seekable, demuxer cache, etc.). |
| GET | `/playback-profiles` | `/api/playback-profiles` | `handlePlaybackProfiles` | `{profiles, default}`: the internal/playback catalog for the decoder's profile picker. `default` is the stored `playback_profile` when it is a known id, else `"auto"`. |

## Cache

| Method | Path | Mirror | Handler | Purpose |
|---|---|---|---|---|
| GET | `/cache` | `/api/cache` | `handleCacheList` | List events currently being DVR-cached and finished caches. |
| GET | `/api/cache/events` | — | `handleCacheList` | Legacy alias for `/cache` (Python-port compat). |
| POST | `/cache/start` | `/api/cache/start` | `handleCacheStart` | Start pulling an HLS stream from the encoder into the local cache. |
| POST | `/cache/stop/{id}` | `/api/cache/stop/{id}` | `handleCacheStop` | Stop a specific cache loop. |
| POST | `/cache/upload` | `/api/cache/upload` | `handleCacheUpload` | Upload a file into the decoder's cache directory. |
| GET | `/cache/upload/{id}` | `/api/cache/upload/{id}` | `handleCacheUploadStatus` | Poll cache-side upload conversion progress. |
| GET | `/cache/uploads` | `/api/cache/uploads` | `handleCacheUploads` | List cache-side uploaded items. |
| GET | `/cache/{id}/...` | `/api/cache/{id}/...` | `cacheCatchAll` → file server | Serve cached HLS segments and `info.json`. |
| GET | `/cache/{id}/status` | `/api/cache/{id}/status` | `handleCacheStatus` | Read cache-loop progress for an event. |
| DELETE | `/cache/{id}` | `/api/cache/{id}` | `handleCacheDelete` | Delete a cached event. `{success, message}`; **400** on an empty or invalid id. |

## External

| Method | Path | Mirror | Handler | Purpose |
|---|---|---|---|---|
| POST | `/external-sources` | `/api/external-sources` | `handleExternalSourceAdd` | Add and start an external (RTSP / RTMP / SRT) ingest source. |
| GET | `/external-sources/{id}` | `/api/external-sources/{id}` | `handleExternalSourceStatus` | Status for a specific external source. |

## Quality

| Method | Path | Mirror | Handler | Purpose |
|---|---|---|---|---|
| GET | `/quality` | `/api/quality` | `handleQualityCatalog` | Return the four-tier preset catalog for the dashboard picker. |

## System

| Method | Path | Mirror | Handler | Purpose |
|---|---|---|---|---|
| GET | `/inputs` | `/api/inputs` | `handleInputs` | Enumerate cameras, capture cards, and audio devices (cached). |
| GET | `/health` | `/api/health` | `handleHealth` | `{ok, version, ffmpeg_available, input_format, input_device, events_dir, events_dir_writable, stream}`; `stream` is the same object as `/status`. No MPV fields. |
| GET | `/status` | `/api/status` | `handleStatus` | Encoder stream status: `{is_running, event_id, uptime_seconds, started_at, ffmpeg_pid, last_error}` (optional fields omitted when empty). |
| GET | `/config` | `/api/config` | `handleConfig` | `{DEFAULT_ENCODER_URL, NODE_TYPE}` only; not the full settings. `DEFAULT_ENCODER_URL` is built from `encoder_host` (this node's configured port is appended when the host has none) unless that is `localhost`, else `default_encoder_url`. |
