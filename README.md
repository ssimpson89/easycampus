# EasyCampus (Go)

EasyCampus is a self-hosted, two-node live video system for community meetings, classrooms, and small events — one machine encodes the local camera or capture card, a second machine on the same LAN decodes it on a TV or projector, and the encoder keeps a DVR cache so you can scroll back the moment somebody asks "what did she just say?" It's a sibling project to easystream (a separate repository): easystream schedules and broadcasts to cloud destinations like YouTube and RTMP, whereas easycampus-go pairs an encoder node with a decoder node on the same network and never has to talk to the public internet.

If your job is "Sunday 10am, push to YouTube, walk away," use easystream. If your job is "encode the camera in the back of the hall and put it on the projector at the front of the hall, with a DVR scrubber for the volunteer at the back," this is the tool.

## Why use it

- **Encoder/decoder pairing on the same LAN.** Run the encoder on the machine with the camera or capture card, run the decoder on the machine driving the projector or TV, and they find each other by hostname. No cloud, no account, no NAT traversal.
- **DVR cache for playback.** Every encoder session is segmented to disk on the encoder host and pulled by the decoder while the event runs. Late arrivals can join at the live edge or scrub back to the start of the meeting; recordings stick around after the stream ends.
- **MPV-driven local display output.** The decoder uses MPV to drive a real display — full-screen on the projector, audio routed to the room PA, fade-to-black between sermons — instead of pretending a browser tab is a media player.
- **Broadcast-grade quality tiers.** Four preset tiers cover consumer (church on a budget, 480p–1080p30), prosumer (1080p60 with hardware encoding), broadcast (true-CBR with Rec.709 / BT.2020 VUI baked into the SPS), and cinema (23.976 and 24.000 with closed GOP and `-fps_mode cfr`).

## Quick start

```bash
go build -o easycampus ./cmd/easycampus
./easycampus
```

Then open <http://127.0.0.1:8000> in your browser. The dashboard auto-redirects to `/encoder` or `/decoder` depending on which mode you've configured. The default port is `8000`; override with `EASYCAMPUS_PORT`. By default the server listens on all interfaces so the decoder on the other end of the LAN can reach it.

Two environment variables you'll actually use:

- `EASYCAMPUS_PORT=8000` — HTTP listen port.
- `EASYCAMPUS_ALLOWED_ORIGINS=http://encoder.local:8000,http://projector.local:8000` — comma-separated CORS allow-list. The defaults already cover `http://127.0.0.1:<port>` and `http://localhost:<port>`; add the actual hostnames the other node browses to.

Settings are layered: built-in defaults, then `~/.easycampus/config.yaml` (written with comments on first run), then `EASYCAMPUS_*` environment variables. See `internal/app/config.go` for the full env-var list (input device, HLS segment duration, adaptive bitrate, node type, etc.). Out-of-range values (port outside 1-65535, `hls_list_size` < 1, `hls_segment_duration` outside 1-10 s, `default_bitrate` below 100000 bits/s) are logged as warnings and replaced by the default. Unparseable env values (`EASYCAMPUS_PORT=eighty`) are ignored with a warning, so the file or default value stays in effect. `default_bitrate` is in bits per second (`4000000` = 4 Mbps).

`./easycampus --set-type encoder|decoder` rewrites `node_type` in the config file and exits. It only touches that one key; environment overrides and derived paths are never written back.

Logging has two independent knobs:

- `LOG_LEVEL` (`debug|info|warn|error`, default `info`) and `LOG_FORMAT` (`json` default, or `text`) control the daemon's own structured log.
- `EASYCAMPUS_LOG_LEVEL` / `log_level` in the config file (`debug|info|warning|error`, default `error`) is passed to FFmpeg as `-loglevel`. It does not affect the daemon log.

## What you'll need

- A small machine for the encoder (Mac mini, a Linux box, a NUC) on the same network as the camera or capture card. A second machine for the decoder if you want local playback — Mac mini, Linux box, or anything with HDMI out and the same network. Both ends can also be the same box for testing.
- **FFmpeg** installed and on `PATH`. `brew install ffmpeg` on macOS, `apt install ffmpeg` on Debian/Ubuntu, `dnf install ffmpeg` on Fedora.
- **MPV** on the decoder host, if you want it to drive a local display. `brew install mpv` / `apt install mpv` / `dnf install mpv`. The encoder host doesn't need it.
- **Go 1.25.6+** to build from source (see `go.mod`). The binary is single-file once compiled. Tagged releases publish macOS and Linux (amd64, arm64) `.tar.gz` archives. Windows is not supported for now.

## Architecture

The codebase follows the `cmd/` + `internal/` layout. Each package owns one concern:

- `internal/app` — `Server` struct, HTTP routes, handlers, middleware (panic recovery, restricted CORS, structured request logging), graceful shutdown.
- `internal/ffmpeg` — FFmpeg supervisor, argv builder, child-process lifecycle. Owns the live encode.
- `internal/devices` — device enumeration (USB cameras, capture cards, screens) with caching so the `/inputs` endpoint doesn't re-shell to `ffmpeg -list_devices` on every request.
- `internal/quality` — the four-tier preset catalog (consumer / prosumer / broadcast / cinema), the recommender that filters by upload bandwidth and available hardware encoders, and the argv builder that emits color signalling and rate-control flags.
- `internal/playback` — the decoder's mpv playback-profile catalog (`auto`, projector/TV cadences, Decklink) that drives `--video-sync`, `--interpolation`, `--display-fps-override`, `--hwdec` and `--vo`. See `docs/playback-profiles.md`.
- `internal/atomicfile` — write-temp-then-rename helper. Used for event metadata, bookmark JSON, PID files and `config.yaml` so a crash mid-write can't truncate the file.
- `internal/version` — build metadata (version, commit, date) injected via `-ldflags` by GoReleaser and reported at startup and in `/health`.
- `internal/ui` — embedded `encoder.html` and `decoder.html` plus their assets, served via `//go:embed`. The binary is single-file deployable.

For the architectural rationale and the refactor history that got us here, read `ARCHITECTURE_REVIEW.md`. Note it is a historical, pre-refactor review: the shared `livestream-core` library it proposes has not been extracted, and easycampus still carries its own `internal/ffmpeg`.

## Quality presets

EasyCampus exposes the same four-tier matrix easystream uses: **consumer** for volunteer-operated, residential-uplink installs; **prosumer** for 1080p60 with hardware encoders on a business connection; **broadcast** for true-CBR egress with broadcast-safe color limiters; **cinema** for 23.976 / 24.000 cadence preservation with closed GOP. Pick the tier that matches your install, pick the preset inside the tier that matches your upload bandwidth, and the recommender will tell you when you've picked something the network can't sustain. See `docs/quality-presets.md` for the full matrix, rate-control envelopes, color signalling, and per-tier caveats.

Adaptive bitrate (step down on congestion, step back up when it clears) is opt-in via `EASYCAMPUS_ADAPTIVE_BITRATE=true`. It's off by default because the consumer tier is already conservative; broadcasters who want to ride a flaky uplink turn it on.

## Routes

The HTTP API is documented in [`ROUTES.md`](ROUTES.md). Most routes are registered under both a bare form (`/stream/start`) and an `/api/`-prefixed mirror (`/api/stream/start`) for compatibility with the original Python port's frontend. The exceptions (pages with no `/api` form, legacy `/api`-only aliases, and `/events` vs `/api/events`) are listed at the top of `ROUTES.md`.

## Development

A `Makefile` carries the common targets. `make help` prints the list:

```bash
make build    # build ./bin/easycampus
make run      # go run ./cmd/easycampus, honouring environment
make test     # go test ./...
make race     # go test -race -count=1 ./...
make vet      # go vet ./...
make lint     # gofmt -l . (fails if gofmt finds anything)
make check    # lint + vet + race (the CI and release gate)
make clean    # rm -rf bin/
```

`make build` drops the binary into `./bin/easycampus`. The root-level `/easycampus` path in `.gitignore` is retained for the legacy `go build -o easycampus` invocation; `./bin/` is gitignored too.

## License

No `LICENSE` file is currently committed to the repository. Treat this as "all rights reserved" until that's resolved — open an issue if you need a license decision before adopting.
