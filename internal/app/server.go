// Package app wires the easycampus HTTP API together. A single
// *Server holds every long-lived dependency (ffmpeg, mpv, cache,
// detection) and exposes the handlers as methods. There are no
// package-level globals — tests construct their own Server with a
// fake settings object; the binary in cmd/easycampus constructs
// one with the real settings.
//
// Layout mirrors easystream/internal/app: Server struct + lifecycle
// methods here, handlers_*.go for the HTTP surface, *.go for the
// services they call.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"easycampus/internal/ui"
)

// ServerConfig is the dependency bundle passed into NewServer. Everything
// the daemon needs lives here; main.go is responsible for populating it.
type ServerConfig struct {
	// Settings is the parsed AppSettings (config file + env). Required.
	Settings AppSettings
	// Logger is the slog logger to use for everything. Required.
	Logger *slog.Logger
	// ReadHeaderTimeout caps request header parsing. Defaults to 5s
	// when zero — long enough for slow LAN clients, short enough to
	// bound Slowloris-style abuse from a misconfigured operator
	// exposing the server to the open internet.
	ReadHeaderTimeout time.Duration
	// ShutdownTimeout bounds the whole Run shutdown sequence. Defaults
	// to 10s.
	ShutdownTimeout time.Duration
	// HTTPDrainTimeout caps how long Server.Shutdown waits for in-flight
	// HTTP requests before force-closing remaining connections, leaving
	// the rest of the shutdown budget for ffmpeg/mpv/cache teardown.
	// Defaults to 3s.
	HTTPDrainTimeout time.Duration
}

// Server is the singular owner of every long-lived dependency. Handler
// methods (handlers_*.go) read from this struct directly — no package
// globals, no init() side effects, no DI framework. Test code can
// construct one with fake settings and exercise individual handlers in
// isolation.
type Server struct {
	settings   AppSettings
	logger     *slog.Logger
	httpServer *http.Server

	ffmpeg *FFmpegService
	mpv    *MPVService
	cache  *CacheService
	detect *DetectionService

	// allowedOrigins is the CORS allow-list computed once at startup.
	// Always concrete origins — never "*". Preflight responses echo
	// the requested origin back only when it matches one of these.
	allowedOrigins []string

	// csrf rejects cross-origin state-changing requests (wired in
	// middleware()). CORS alone can't do this: a "simple" POST or a
	// form submission from any site is sent by the browser without a
	// preflight, so the handler would run before CORS headers matter.
	csrf *http.CrossOriginProtection

	// profileMu guards playbackProfile, the operator's last-chosen
	// playback profile (seeded from settings.PlaybackProfile). Handlers
	// read and write it concurrently.
	profileMu       sync.Mutex
	playbackProfile string

	// bookmarkLocks serializes bookmarks.json read-modify-write per event.
	bookmarkLocks keyedMutex

	shutdownTimeout  time.Duration
	httpDrainTimeout time.Duration
}

// NewServer wires the dependencies and registers routes. Returns a ready-
// to-run *Server; callers invoke Run(ctx) to block on signals or
// ListenAndServe/Shutdown for finer control (tests).
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.Logger == nil {
		return nil, errors.New("ServerConfig.Logger is required")
	}
	settings := cfg.Settings

	if cfg.ReadHeaderTimeout <= 0 {
		cfg.ReadHeaderTimeout = 5 * time.Second
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 10 * time.Second
	}
	if cfg.HTTPDrainTimeout <= 0 {
		cfg.HTTPDrainTimeout = 3 * time.Second
	}

	s := &Server{
		settings:         settings,
		logger:           cfg.Logger,
		shutdownTimeout:  cfg.ShutdownTimeout,
		httpDrainTimeout: cfg.HTTPDrainTimeout,
		allowedOrigins:   computeAllowedOrigins(settings),
		playbackProfile:  settings.PlaybackProfile,
	}
	s.csrf = http.NewCrossOriginProtection()
	for _, origin := range s.allowedOrigins {
		if err := s.csrf.AddTrustedOrigin(origin); err != nil {
			cfg.Logger.Warn("ignoring invalid allowed origin", "origin", origin, "err", err)
		}
	}

	// A previous process that died mid-stream leaves info.json saying
	// "running"; nothing can be live yet, so mark those stopped.
	if n := reconcileStaleEvents(settings); n > 0 {
		cfg.Logger.Info("marked stale running events as stopped", "count", n)
	}

	// Build the services in the order they're needed. The ffmpeg
	// supervisor performs a pid-file reap of orphaned children from
	// the previous session — do it before binding to the HTTP port so
	// a /api/start race during startup can't beat the reaper.
	s.ffmpeg = NewFFmpegService(settings, cfg.Logger.With("component", "ffmpeg"))
	s.mpv = NewMPVService(settings, cfg.Logger.With("component", "mpv"))
	s.cache = NewCacheService(settings, cfg.Logger.With("component", "cache"))
	s.detect = NewDetectionService(cfg.Logger.With("component", "detect"))

	addr := fmt.Sprintf(":%d", settings.Port)
	s.httpServer = &http.Server{
		Addr:              addr,
		Handler:           s.middleware(s.routes()),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
	}

	return s, nil
}

// Addr returns the listen address (":port").
func (s *Server) Addr() string {
	return s.httpServer.Addr
}

// ListenAndServe blocks serving HTTP. Returns http.ErrServerClosed on a
// clean Shutdown. Tests prefer ListenAndServe + Shutdown over Run.
func (s *Server) ListenAndServe() error {
	return s.httpServer.ListenAndServe()
}

// Run is the entry point used by the binary: starts the HTTP server,
// traps SIGINT/SIGTERM, and orchestrates the shutdown sequence. Returns
// only after every child (HTTP, cache loops, ffmpeg, mpv) has exited.
// Returns nil on a clean shutdown, or the first non-ErrServerClosed
// error from any child.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("http server starting",
			"addr", s.httpServer.Addr,
			"node_type", s.settings.NodeType)
		if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		s.logger.Info("shutdown signal received", "cause", context.Cause(ctx))
	case err := <-errCh:
		if err != nil {
			return err
		}
		// ListenAndServe returned ErrServerClosed without us triggering
		// a shutdown — something else (kernel-level error, missing port)
		// brought it down. Still run the cleanup path.
		s.logger.Warn("http server exited without shutdown signal")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer cancel()
	if err := s.Shutdown(shutdownCtx); err != nil {
		s.logger.Error("shutdown completed with errors", "err", err)
		return err
	}
	return nil
}

// Shutdown drains in-flight HTTP requests then stops every supervised
// child: cache loops first (so they don't observe a half-stopped
// ffmpeg/mpv), then the live encoder, then the local player. Each step
// is best-effort; errors are logged but do not abort subsequent steps.
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("shutdown: starting http drain", "timeout", s.httpDrainTimeout)
	// Dedicated, shorter budget for the HTTP drain so a stuck client
	// can't consume the time the child-process teardown needs.
	drainCtx, cancel := context.WithTimeout(ctx, s.httpDrainTimeout)
	httpErr := s.httpServer.Shutdown(drainCtx)
	cancel()
	if errors.Is(httpErr, context.DeadlineExceeded) {
		// http.Server.Shutdown never closes connections that are still
		// in StateNew (dialed, no request yet) until they're 5s old, and
		// waits forever on long-running handlers. Force-close whatever
		// is left; that's the documented follow-up to a timed-out drain.
		s.logger.Warn("shutdown: http drain timed out; force-closing connections")
		httpErr = s.httpServer.Close()
	}
	if httpErr != nil {
		s.logger.Error("shutdown: http drain failed", "err", httpErr)
	} else {
		s.logger.Info("shutdown: http drain complete")
	}

	// Stop cache loops first — they own goroutines that pull from
	// peers and write to disk. Stopping them before the live encoder
	// avoids a brief window where a cache loop is writing to a file
	// the encoder is about to truncate.
	s.cache.StopAll()
	s.logger.Info("shutdown: cache loops stopped")

	// Stop the adaptive controller BEFORE the supervisor it watches.
	// The controller's tick goroutine reads supervisor status and may
	// call sup.Stop/Start to step quality; if we tore the supervisor
	// down first, an in-flight tick could observe a half-stopped state
	// or attempt a restart against the dying supervisor.
	if adaptive := s.ffmpeg.AdaptiveController(); adaptive != nil {
		adaptive.Stop()
		s.logger.Info("shutdown: adaptive controller stopped")
	}

	if err := s.ffmpeg.StopStream(ctx); err != nil {
		s.logger.Error("shutdown: ffmpeg stop failed", "err", err)
	} else {
		s.logger.Info("shutdown: ffmpeg stopped")
	}

	if err := s.mpv.Stop(ctx); err != nil {
		s.logger.Error("shutdown: mpv stop failed", "err", err)
	} else {
		s.logger.Info("shutdown: mpv stopped")
	}

	return httpErr
}

// routes registers every HTTP endpoint. Each `/foo` is automatically
// mirrored at `/api/foo` so the existing HTML/JS that hard-codes the
// `/api/*` prefix keeps working alongside the canonical short paths.
// The mirror is achieved via the register() closure — single source of
// truth per route, no copy-paste duplication.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	register := func(path string, h http.HandlerFunc) {
		mux.HandleFunc(path, h)
		mux.HandleFunc("/api"+path, h)
	}

	// Static assets: embedded via internal/ui. The handler strips the
	// /assets/ prefix so the embed FS sees plain "encoder.js" etc.
	// ui.AssetsHandler adds Cache-Control and hides directory listings.
	mux.Handle("/assets/", http.StripPrefix("/assets/", ui.AssetsHandler()))
	// /api/assets/ mirror for completeness; some templates relative-path.
	mux.Handle("/api/assets/", http.StripPrefix("/api/assets/", ui.AssetsHandler()))

	// Pages.
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/encoder", s.handleEncoderPage)
	mux.HandleFunc("/decoder", s.handleDecoderPage)

	// Output (MPV).
	register("/output/capabilities", s.handleOutputCapabilities)
	register("/output/start", s.handleOutputStart)
	register("/output/stop", s.handleOutputStop)
	register("/output/status", s.handleOutputStatus)
	register("/output/update", s.handleOutputUpdate)
	register("/output/sync", s.handleOutputSync)
	register("/output/mute", s.handleOutputMute)
	register("/output/position", s.handleOutputPosition)
	register("/output/frame-position", s.handleOutputFramePosition)
	register("/output/diagnostics", s.handleOutputDiagnostics)
	// Decoder playback profile catalog. Symmetric to /api/quality on
	// the encoder side — the decoder UI fetches this to render its
	// profile picker. See internal/playback for the profile model.
	register("/playback-profiles", s.handlePlaybackProfiles)

	// System.
	register("/inputs", s.handleInputs)
	register("/health", s.handleHealth)
	register("/status", s.handleStatus)
	register("/config", s.handleConfig)

	// Stream control.
	register("/stream/start", s.handleStreamStart)
	register("/stream/stop", s.handleStreamStop)
	register("/stream/force_stop", s.handleStreamForceStop)
	// Legacy aliases: HTML compatibility — older UIs POST /api/start
	// and /api/stop without the /stream prefix.
	mux.HandleFunc("/api/start", s.handleStreamStart)
	mux.HandleFunc("/api/stop", s.handleStreamStop)

	// External sources.
	register("/external-sources", s.handleExternalSourceAdd)
	register("/external-sources/", s.handleExternalSourceStatus)

	// Events. Specific routes before catch-alls.
	mux.HandleFunc("/events", s.handleEvents)
	mux.HandleFunc("/api/events", s.handleAPIEvents)
	register("/events/upload", s.handleUpload)
	register("/events/upload/", s.handleUploadStatus)
	mux.HandleFunc("/api/upload", s.handleUpload)
	eventsFS := http.FileServer(http.Dir(s.settings.EventsDir))
	mux.HandleFunc("/events/", s.eventsCatchAll(http.StripPrefix("/events/", eventsFS)))
	mux.HandleFunc("/api/events/", s.eventsCatchAll(http.StripPrefix("/api/events/", eventsFS)))

	// Cache. Specific routes before catch-alls.
	register("/cache", s.handleCacheList)
	mux.HandleFunc("/api/cache/events", s.handleCacheList)
	register("/cache/start", s.handleCacheStart)
	register("/cache/stop/", s.handleCacheStop)
	register("/cache/upload", s.handleCacheUpload)
	register("/cache/upload/", s.handleCacheUploadStatus)
	register("/cache/uploads", s.handleCacheUploads)
	cacheFS := http.FileServer(http.Dir(s.settings.CacheDir))
	mux.HandleFunc("/cache/", s.cacheCatchAll(http.StripPrefix("/cache/", cacheFS)))
	mux.HandleFunc("/api/cache/", s.cacheCatchAll(http.StripPrefix("/api/cache/", cacheFS)))

	// registerQualityRoutes is an empty hook today — the parallel
	// quality+UI agent is expected to fill this in (e.g. GET
	// /api/quality, POST /api/adaptive). Leaving the call site here
	// minimizes merge surface.
	s.registerQualityRoutes(mux)

	return mux
}

// registerQualityRoutes wires the broadcast preset catalog exposed by
// internal/quality. Surfaced at /quality (canonical) and /api/quality
// (legacy mirror) so encoder.html can render a tier-grouped picker.
func (s *Server) registerQualityRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/quality", s.handleQualityCatalog)
	mux.HandleFunc("/api/quality", s.handleQualityCatalog)
}

// middleware wraps the mux with: panic recovery, restricted CORS,
// structured request logging, cross-origin (CSRF) protection. The order
// matters — recovery is outermost so a panic inside the CORS path still
// returns 500 + structured log, and CSRF sits inside logging so rejected
// requests still show up (as 403) in the journal.
func (s *Server) middleware(next http.Handler) http.Handler {
	return s.recoverMiddleware(s.corsMiddleware(s.logMiddleware(s.csrf.Handler(next))))
}

// recoverMiddleware catches handler panics and converts them into a
// generic 500 JSON response. The stack is logged at error level with
// the request path/method so the operator can grep their journald
// output without exposing implementation details to a remote client.
func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("handler panicked",
					"path", r.URL.Path,
					"method", r.Method,
					"err", fmt.Sprintf("%v", rec),
					"stack", string(debug.Stack()))
				// Best-effort write. If headers were already sent the
				// client gets a truncated response — there's nothing
				// we can do about that at this layer.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"detail":"internal error"}` + "\n"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// corsMiddleware applies a restricted, allow-list CORS policy: only
// origins in s.allowedOrigins (127.0.0.1/localhost on the listen port
// plus AppSettings.AllowedOrigins) get Access-Control-Allow-* headers,
// echoed back verbatim — never "*".
//
// CORS only controls whether a cross-origin page may READ a response
// (and whether non-simple requests pass preflight). It does NOT stop a
// "simple" cross-site request (form POST, text/plain fetch, or a
// DELETE issued via a top-level navigation trick) from reaching the
// handler and mutating state. That protection is s.csrf
// (http.CrossOriginProtection, trusting the same allow-list), which
// middleware() layers in: it rejects non-safe methods whose
// Sec-Fetch-Site / Origin say they came from another site, while
// same-origin requests and non-browser clients (no Origin) pass.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && s.originAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		}
		if r.Method == http.MethodOptions {
			// Preflight: respond 204 either way. Without a matching
			// origin the browser sees missing CORS headers and blocks
			// the request — correct behaviour.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originAllowed performs an exact case-insensitive match against the
// computed allow-list. No wildcards, no suffix matches; if an operator
// needs a wildcard they spell out every concrete origin.
func (s *Server) originAllowed(origin string) bool {
	for _, allowed := range s.allowedOrigins {
		if strings.EqualFold(origin, allowed) {
			return true
		}
	}
	return false
}

// logMiddleware logs one structured line per request after the handler
// returns. Status code, latency, path, method, remote IP. Skipped for
// /assets/* so embedded static asset traffic doesn't drown the journal.
func (s *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lrw := &loggingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(lrw, r)

		// Suppress per-asset noise; keep the API surface and pages.
		if strings.HasPrefix(r.URL.Path, "/assets/") || strings.HasPrefix(r.URL.Path, "/api/assets/") {
			return
		}
		s.logger.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", lrw.statusCode,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote", r.RemoteAddr)
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	lrw.statusCode = code
	lrw.ResponseWriter.WriteHeader(code)
}

// Unwrap exposes the underlying writer so http.ResponseController (and
// optional interfaces like http.Flusher / io.ReaderFrom discovered
// through it) keep working behind the logging middleware.
func (lrw *loggingResponseWriter) Unwrap() http.ResponseWriter {
	return lrw.ResponseWriter
}

// handleRoot redirects bare GET / to the page matching NodeType.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	target := "/decoder"
	if s.settings.NodeType == "encoder" {
		target = "/encoder"
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// handleEncoderPage serves the embedded encoder.html.
func (s *Server) handleEncoderPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(ui.EncoderHTML())
}

// handleDecoderPage serves the embedded decoder.html.
func (s *Server) handleDecoderPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(ui.DecoderHTML())
}

// eventsCatchAll preserves the original /events/<id>/{bookmarks|file}
// routing. The DELETE branch routes to handleEventDelete; everything
// containing /bookmarks routes to handleEventBookmarks; the rest is
// static HLS file serving via the supplied handler.
func (s *Server) eventsCatchAll(fileServer http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/bookmarks") {
			s.handleEventBookmarks(w, r)
			return
		}
		if r.Method == http.MethodDelete {
			s.handleEventDelete(w, r)
			return
		}
		fileServer.ServeHTTP(w, r)
	}
}

// cacheCatchAll preserves the original /cache/<id>/{status|file}
// routing. /status reads status; DELETE deletes; the rest is served
// statically from the cache directory.
func (s *Server) cacheCatchAll(fileServer http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			s.handleCacheStatus(w, r)
			return
		}
		if r.Method == http.MethodDelete {
			s.handleCacheDelete(w, r)
			return
		}
		fileServer.ServeHTTP(w, r)
	}
}

// --- Shared helpers ---

// writeJSON encodes payload as JSON. Encode failures are logged and a
// generic error body is written so the client never sees an empty
// successful response.
func (s *Server) writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	data, err := json.Marshal(payload)
	if err != nil {
		s.logger.Error("writeJSON: encode failed", "err", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"detail":"failed to encode response"}` + "\n"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n"))
}

// computeAllowedOrigins assembles the CORS allow-list once at startup.
// Defaults: http://127.0.0.1:<port> and http://localhost:<port>.
// Operator additions from AppSettings.AllowedOrigins are appended
// verbatim (trim only). Empty entries are skipped.
func computeAllowedOrigins(settings AppSettings) []string {
	defaultOrigins := []string{
		fmt.Sprintf("http://127.0.0.1:%d", settings.Port),
		fmt.Sprintf("http://localhost:%d", settings.Port),
	}
	out := make([]string, 0, len(defaultOrigins)+len(settings.AllowedOrigins))
	seen := make(map[string]bool, len(out))
	add := func(o string) {
		o = strings.TrimSpace(o)
		if o == "" {
			return
		}
		if seen[o] {
			return
		}
		seen[o] = true
		out = append(out, o)
	}
	for _, o := range defaultOrigins {
		add(o)
	}
	for _, o := range settings.AllowedOrigins {
		add(o)
	}
	return out
}

// Settings returns a read-only copy of the loaded AppSettings.
func (s *Server) Settings() AppSettings { return s.settings }

// currentPlaybackProfile returns the operator's last-chosen playback
// profile ID (may be empty).
func (s *Server) currentPlaybackProfile() string {
	s.profileMu.Lock()
	defer s.profileMu.Unlock()
	return s.playbackProfile
}

// setPlaybackProfile records the operator's playback profile choice.
func (s *Server) setPlaybackProfile(id string) {
	s.profileMu.Lock()
	s.playbackProfile = id
	s.profileMu.Unlock()
}
