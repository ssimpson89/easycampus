package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"easycampus/internal/atomicfile"
)

// CacheService pulls a remote HLS playlist (the encoder peer's stream)
// and stores segments locally so the decoder can DVR/replay even when
// the encoder is offline. One goroutine per active event ID.
type CacheService struct {
	client   *http.Client
	active   map[string]cacheLoop
	mu       sync.Mutex
	settings AppSettings
	logger   *slog.Logger
}

// cacheRequestTimeout caps a single playlist or segment fetch.
const cacheRequestTimeout = 30 * time.Second

// errInvalidCacheRequest is returned by StartCaching for an unsafe event
// ID or a non-http(s) remote URL.
var errInvalidCacheRequest = errors.New("invalid event_id or remote_url")

// cacheLoop tracks both the cancel-fn and the done-channel for a single
// in-flight cache goroutine. Shutdown waits on done after cancelling so
// every loop has time to flush and close its files cleanly.
type cacheLoop struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func NewCacheService(settings AppSettings, logger *slog.Logger) *CacheService {
	if logger == nil {
		logger = slog.Default()
	}
	// Every outbound fetch is bounded twice: by the loop ctx (so Stop
	// lands promptly) and by a per-request timeout (cacheRequestTimeout,
	// applied in fetchText / downloadFile) so a peer that accepts the
	// connection and then stalls can't wedge the loop forever.
	return &CacheService{
		client:   &http.Client{},
		active:   make(map[string]cacheLoop),
		settings: settings,
		logger:   logger,
	}
}

// StartCaching spawns a downloader goroutine for eventID. A second
// concurrent call with the same eventID is a no-op (the existing
// goroutine keeps running). Returns immediately; use StopAll or
// StopCaching to drain.
//
// parent is used as the root for the loop's context — typically the
// caller passes the request ctx, but per item #7 we derive a fresh
// child via context.WithCancel so the cache loop survives the HTTP
// request that started it. The child still observes parent
// cancellation if the daemon is shutting down. Explicit StopCaching
// invokes the locally-captured cancel-fn for fast teardown.
//
// eventID must pass validID and remoteURL must be an http(s) URL;
// otherwise errInvalidCacheRequest is returned and nothing is created.
func (c *CacheService) StartCaching(parent context.Context, eventID, remoteURL string) error {
	if !validID(eventID) || !validHTTPURL(remoteURL) {
		return errInvalidCacheRequest
	}
	if parent == nil {
		parent = context.Background()
	}
	c.mu.Lock()
	if _, ok := c.active[eventID]; ok {
		c.mu.Unlock()
		return nil
	}
	// Detach the request's lifetime so a client disconnect doesn't
	// kill the cache loop; keep the parent's *values* if any.
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(parent))
	done := make(chan struct{})
	c.active[eventID] = cacheLoop{cancel: cancel, done: done}
	c.mu.Unlock()

	eventDir := filepath.Join(c.settings.CacheDir, eventID)
	_ = os.MkdirAll(eventDir, 0755)

	c.logger.Info("cache loop start",
		"component", "cache.loop",
		"event_id", eventID,
		"remote_url", remoteURL)

	go func() {
		defer close(done)
		c.downloadLoop(loopCtx, eventID, remoteURL, eventDir)
		c.logger.Info("cache loop exit",
			"component", "cache.loop",
			"event_id", eventID)
	}()
	return nil
}

// StopCaching cancels a single in-flight loop and waits for it to exit.
// The wait is bounded indirectly by the loop's own ctx-aware sleeps and
// ctx-bound HTTP fetches; with the architect's #7 fix in place a Stop
// now lands within ~1s instead of up to a full 10s.
//
// ctx is accepted for API symmetry but cancellation does not affect the
// wait — once we've signalled the loop we always block until it exits,
// otherwise the next caller could observe a half-stopped state.
func (c *CacheService) StopCaching(ctx context.Context, eventID string) {
	_ = ctx
	c.mu.Lock()
	loop, ok := c.active[eventID]
	if ok {
		loop.cancel()
		delete(c.active, eventID)
	}
	c.mu.Unlock()
	if ok {
		<-loop.done
	}
}

// StopAll cancels every in-flight loop and waits for each to exit.
// Called from Server.Shutdown to drain before the daemon closes file
// handles or the process group.
func (c *CacheService) StopAll() {
	c.mu.Lock()
	loops := c.active
	c.active = make(map[string]cacheLoop)
	c.mu.Unlock()
	for id, loop := range loops {
		loop.cancel()
		<-loop.done
		c.logger.Info("cache loop drained on shutdown",
			"component", "cache.loop", "event_id", id)
	}
}

// DeleteCache stops any active loop for eventID then removes its on-disk
// segments. ctx is forwarded into StopCaching but the file removal is
// best-effort and unconditional.
//
// An invalid eventID (including "") is rejected so the removal can never
// target CacheDir itself or anything outside it.
func (c *CacheService) DeleteCache(ctx context.Context, eventID string) error {
	if !validID(eventID) {
		return errInvalidCacheRequest
	}
	c.StopCaching(ctx, eventID)
	return os.RemoveAll(filepath.Join(c.settings.CacheDir, eventID))
}

// Status reports cache state for eventID. ctx is accepted for API
// symmetry; the call only does local stat + WalkDir work which is fast
// enough that ctx cancellation is unnecessary — but plumbing it through
// makes future per-segment HEAD probes ctx-aware.
func (c *CacheService) Status(ctx context.Context, eventID string) CacheStatus {
	_ = ctx
	if !validID(eventID) {
		return CacheStatus{}
	}
	eventDir := filepath.Join(c.settings.CacheDir, eventID)
	if _, err := os.Stat(eventDir); err != nil {
		return CacheStatus{Cached: false, Active: false, Segments: 0, SizeBytes: 0}
	}

	segments := 0
	size := int64(0)
	_ = filepath.WalkDir(eventDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".m4s") || strings.HasSuffix(path, ".ts") {
			segments++
		}
		if info, err := d.Info(); err == nil {
			size += info.Size()
		}
		return nil
	})

	c.mu.Lock()
	_, active := c.active[eventID]
	c.mu.Unlock()

	return CacheStatus{Cached: true, Active: active, Segments: segments, SizeBytes: size}
}

// activeCount returns the number of in-flight loops. Used by tests
// (and could feed a /health metric later).
func (c *CacheService) activeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.active)
}

func (c *CacheService) downloadLoop(ctx context.Context, eventID, baseURL, outputDir string) {
	playlistURL := strings.TrimRight(baseURL, "/") + "/events/" + eventID + "/master.m3u8"
	downloaded := make(map[string]bool)
	isLive := true
	completeCycles := 0

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		content, err := c.fetchText(ctx, playlistURL)
		if err == nil {
			if strings.Contains(content, "#EXT-X-ENDLIST") {
				isLive = false
			}

			segments := parseSegments(content)
			newDownloads := 0
			// complete stays true only if every segment the playlist
			// references is a safe name and is on disk locally.
			complete := true
			for _, segment := range segments {
				if downloaded[segment] {
					continue
				}
				// Segment names come from an untrusted peer; anything
				// that isn't a plain file name could escape outputDir.
				if !safeSegmentName(segment) {
					c.logger.Warn("cache: skipping unsafe segment name",
						"event_id", eventID, "segment", segment)
					complete = false
					continue
				}
				// Cheap pre-check: if Stop landed mid-batch we'd
				// otherwise issue more downloads before observing
				// the next sleep's ctx.Done().
				if ctx.Err() != nil {
					return
				}
				url := strings.TrimRight(baseURL, "/") + "/events/" + eventID + "/" + segment
				if err := c.downloadFile(ctx, url, filepath.Join(outputDir, segment)); err == nil {
					downloaded[segment] = true
					newDownloads++
				} else {
					complete = false
				}
			}

			// Publish the playlist only when every segment it lists is
			// on disk, so a reader never requests a file that failed to
			// download or was rejected as unsafe. Otherwise keep the
			// previous playlist and retry on the next cycle. Atomic so
			// readers never see a half-written playlist.
			if complete {
				if werr := atomicfile.Write(
					filepath.Join(outputDir, "master.m3u8"),
					[]byte(content), 0644); werr != nil {
					c.logger.Warn("cache: playlist write failed",
						"event_id", eventID, "err", werr)
				}
			} else {
				c.logger.Warn("cache: playlist held back; some segments missing or unsafe",
					"event_id", eventID)
			}

			if !isLive && complete && newDownloads == 0 {
				completeCycles++
				if completeCycles >= 2 {
					// VOD source has stabilised: nothing new for two
					// cycles after the ENDLIST marker. Return without
					// calling StopCaching (would deadlock waiting on
					// our own done channel). The parent goroutine
					// scope closes done; StopCaching/StopAll handles
					// the active-map cleanup.
					c.mu.Lock()
					delete(c.active, eventID)
					c.mu.Unlock()
					return
				}
			} else {
				completeCycles = 0
			}
		} else {
			c.logger.Debug("cache: playlist fetch failed",
				"event_id", eventID, "url", playlistURL, "err", err)
		}

		// Ctx-aware sleep — a Stop call lands within 2s instead of
		// always waiting the full interval.
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// fetchText fetches url and returns its body as a string. ctx bounds
// both the request setup (DNS / dial / TLS) and the response read; a
// peer that opens the connection but stalls mid-body still gets ripped
// out by ctx cancellation. Per the architect's #7 concern.
func (c *CacheService) fetchText(ctx context.Context, url string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, cacheRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", errors.New("bad status")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// downloadFile fetches url and streams the body to path. ctx scopes the
// whole transfer so a Stop that arrives mid-segment cancels the copy.
// The body lands in a temp file that is renamed over path only once the
// download completes, so readers never serve a truncated segment.
func (c *CacheService) downloadFile(ctx context.Context, url, path string) error {
	ctx, cancel := context.WithTimeout(ctx, cacheRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.New("bad status")
	}
	return atomicfile.WriteFrom(path, resp.Body, 0644)
}

func parseSegments(content string) []string {
	lines := strings.Split(content, "\n")
	segments := make([]string, 0)
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-MAP:") {
			if uri := m3u8Attr(line, "URI"); uri != "" {
				segments = append(segments, uri)
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		segments = append(segments, line)
	}
	return segments
}

// m3u8Attr returns the value of a quoted or bare attribute in an HLS
// tag line, e.g. URI from `#EXT-X-MAP:URI="init.mp4",BYTERANGE="..."`.
func m3u8Attr(line, name string) string {
	i := strings.Index(line, ":")
	if i < 0 {
		return ""
	}
	rest := line[i+1:]
	for rest != "" {
		eq := strings.Index(rest, "=")
		if eq < 0 {
			return ""
		}
		key := strings.TrimSpace(rest[:eq])
		rest = rest[eq+1:]
		var val string
		if strings.HasPrefix(rest, "\"") {
			end := strings.Index(rest[1:], "\"")
			if end < 0 {
				return ""
			}
			val = rest[1 : end+1]
			rest = rest[end+2:]
		} else {
			end := strings.Index(rest, ",")
			if end < 0 {
				end = len(rest)
			}
			val = rest[:end]
			rest = rest[end:]
		}
		rest = strings.TrimPrefix(rest, ",")
		if key == name {
			return val
		}
	}
	return ""
}
