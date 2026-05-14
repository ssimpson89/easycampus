# HTTP Routes

Source of truth: `internal/app/server.go` `routes()`.

Both the bare and `/api/`-prefixed forms work for every route below; the mirror column lists the canonical pair. The mirroring is done in code via a `register()` closure, so changing a handler in one place updates both forms — there's no copy-paste duplication to drift.

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
| POST | `/stream/start` | `/api/stream/start` | `handleStreamStart` | Start the FFmpeg encoder with the supplied config. |
| POST | `/stream/stop` | `/api/stream/stop` | `handleStreamStop` | Graceful stop of the current encode. |
| POST | `/stream/force_stop` | `/api/stream/force_stop` | `handleStreamForceStop` | SIGKILL the encoder when graceful stop hangs. |
| POST | `/api/start` | — | `handleStreamStart` | Legacy alias for `/stream/start` (Python-port compat). |
| POST | `/api/stop` | — | `handleStreamStop` | Legacy alias for `/stream/stop` (Python-port compat). |

## Events

| Method | Path | Mirror | Handler | Purpose |
|---|---|---|---|---|
| GET | `/events` | — | `handleEvents` | List recorded events with size and duration. |
| GET/POST | `/api/events` | — | `handleAPIEvents` | List or create events via the API surface used by the dashboards. |
| GET | `/events/{id}/...` | `/api/events/{id}/...` | `eventsCatchAll` → file server | Serve event HLS segments and `info.json`. |
| GET/POST | `/events/{id}/bookmarks` | `/api/events/{id}/bookmarks` | `handleEventBookmarks` | List or create bookmarks on an event. |
| PUT/DELETE | `/events/{id}/bookmarks/{bid}` | `/api/events/{id}/bookmarks/{bid}` | `handleEventBookmarks` | Update or delete a specific bookmark. |
| DELETE | `/events/{id}` | `/api/events/{id}` | `handleEventDelete` | Delete an event and its recording directory. |
| POST | `/events/upload` | `/api/events/upload` | `handleUpload` | Upload a local file and convert into an event. |
| POST | `/api/upload` | — | `handleUpload` | Legacy alias for `/events/upload`. |
| GET | `/events/upload/{id}` | `/api/events/upload/{id}` | `handleUploadStatus` | Poll FFmpeg conversion progress for an upload. |

## Output (MPV)

| Method | Path | Mirror | Handler | Purpose |
|---|---|---|---|---|
| GET | `/output/capabilities` | `/api/output/capabilities` | `handleOutputCapabilities` | Report MPV availability, screens, Decklink presence. |
| POST | `/output/start` | `/api/output/start` | `handleOutputStart` | Start MPV against the encoder's HLS URL (or a local file). |
| POST | `/output/stop` | `/api/output/stop` | `handleOutputStop` | Stop the MPV player. |
| GET | `/output/status` | `/api/output/status` | `handleOutputStatus` | Read current playback state (position, paused, source). |
| POST | `/output/update` | `/api/output/update` | `handleOutputUpdate` | Swap the playing source without restarting MPV. |
| POST | `/output/sync` | `/api/output/sync` | `handleOutputSync` | Re-sync to the live edge after a scrub. |
| POST | `/output/mute` | `/api/output/mute` | `handleOutputMute` | Toggle MPV audio mute. |
| GET | `/output/position` | `/api/output/position` | `handleOutputPosition` | Current playback position in seconds. |
| GET | `/output/frame-position` | `/api/output/frame-position` | `handleOutputFramePosition` | Current playback position as a frame index. |
| GET | `/output/diagnostics` | `/api/output/diagnostics` | `handleOutputDiagnostics` | MPV property dump (seekable, demuxer cache, etc.). |

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
| DELETE | `/cache/{id}` | `/api/cache/{id}` | `handleCacheDelete` | Delete a cached event. |

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
| GET | `/health` | `/api/health` | `handleHealth` | Liveness probe with FFmpeg / MPV running flags. |
| GET | `/status` | `/api/status` | `handleStatus` | Current encoder status (event id, started-at, last error). |
| GET | `/config` | `/api/config` | `handleConfig` | Read the loaded `AppSettings` for the dashboard to render. |
