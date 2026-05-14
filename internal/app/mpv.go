package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"easycampus/internal/playback"
)

// MPVService owns the lifecycle of a single child mpv process for the
// decoder node. Methods are safe under concurrent calls — the mutex
// covers process + currentURL + socketPath bookkeeping. IPC calls dial
// a fresh unix socket on each request (mpv accepts that trivially) so
// concurrent property reads don't serialize through one connection.
type MPVService struct {
	mu         sync.Mutex
	process    *exec.Cmd
	socketPath string
	currentURL string
	logger     *slog.Logger
}

// NewMPVService scopes the IPC socket under the per-node data dir so
// two easycampus-go processes on the same host don't collide. Fall
// back to /tmp when no BaseDir is configured so tests still work.
func NewMPVService(settings AppSettings, logger *slog.Logger) *MPVService {
	if logger == nil {
		logger = slog.Default()
	}
	socketDir := filepath.Join(settings.BaseDir, settings.NodeType)
	if settings.BaseDir == "" {
		socketDir = os.TempDir()
	}
	return &MPVService{
		socketPath: filepath.Join(socketDir, "mpv.sock"),
		logger:     logger,
	}
}

// Start launches mpv playing the supplied URL with the given profile.
// The profile drives the output stage (--vo), video-sync mode,
// interpolation, hardware decode, and optional --display-fps-override —
// see internal/playback for the catalog. Base flags (IPC socket, cache,
// idle/pause, OSD) are merged in here regardless of profile.
//
// ctx bounds the *startup* path: any existing mpv is stopped first (Stop
// honours ctx), then the new child is spawned and we poll its IPC socket
// for up to min(5s, ctx deadline) so a client disconnect or shutdown
// signal cuts the wait short. Once mpv is up, ctx is no longer
// referenced — the child runs until Stop is called.
//
// screenIndex / fullscreen only apply to the GPU output path; the
// decklink path ignores them (the SDI board owns the output geometry).
func (m *MPVService) Start(ctx context.Context, url string, screenIndex int, profile playback.Profile, fullscreen bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_ = m.Stop(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()

	// Ensure the socket directory exists; otherwise mpv silently
	// refuses to start the IPC server and every subsequent property
	// read fails with "connection refused".
	if dir := filepath.Dir(m.socketPath); dir != "" {
		_ = os.MkdirAll(dir, 0755)
	}
	if m.socketPath != "" {
		_ = os.Remove(m.socketPath)
	}

	outputMode := profile.OutputMode
	if outputMode == "" {
		outputMode = playback.OutputModeGPU
	}

	// Profile flags first (after URL + socket): --vo, --hwdec,
	// --video-sync, --interpolation, --display-fps-override,
	// --decklink-format (when decklink + format set). Pure projection
	// of the profile via internal/playback.
	args := []string{
		url,
		"--input-ipc-server=" + m.socketPath,
	}
	args = append(args, profile.MPVArgs()...)

	if outputMode == playback.OutputModeDecklink {
		// Decklink path: no window geometry — the SDI board owns the
		// output frame. Common base flags only.
		args = append(args,
			"--pause",
			"--mute=yes",
			"--no-osc",
			"--osd-level=0",
			"--cache=yes",
			"--cache-pause=no",
			"--demuxer-max-bytes=500M",
			"--demuxer-max-back-bytes=300M",
			"--force-seekable=yes",
			"--hr-seek=yes",
			"--hr-seek-framedrop=no",
			"--demuxer-readahead-secs=99999",
			"--demuxer-lavf-o=live_start_index=0",
		)
	} else {
		// GPU desktop path: window geometry + idle/keep-open semantics.
		if fullscreen {
			args = append(args,
				"--fs-screen="+itoa(screenIndex),
				"--fs",
				"--force-window=immediate",
				"--keep-open=yes",
				"--force-window-position",
				"--geometry=100%:100%",
				"--no-border",
				"--ontop",
			)
		} else {
			args = append(args,
				"--geometry=50%",
				"--autofit=50%",
			)
		}

		args = append(args,
			"--idle=yes",
			"--pause",
			"--mute=yes",
			"--no-osc",
			"--osd-level=0",
			"--cache=yes",
			"--cache-pause=no",
			"--demuxer-max-bytes=500M",
			"--demuxer-max-back-bytes=300M",
			"--force-seekable=yes",
			"--hr-seek=yes",
			"--hr-seek-framedrop=no",
			"--demuxer-readahead-secs=99999",
			"--demuxer-lavf-o=live_start_index=0",
		)
	}

	// exec.CommandContext lets a parent ctx cancellation reap the
	// child. Note: a nil ctx would panic, so fall back to Background.
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, "mpv", args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	m.logger.Info("mpv: starting",
		"component", "mpv",
		"url", url,
		"mode", outputMode,
		"profile", profile.ID,
		"fullscreen", fullscreen,
		"screen", screenIndex)
	m.logger.Debug("mpv: argv", "args", args)

	if err := cmd.Start(); err != nil {
		return err
	}

	m.process = cmd
	m.currentURL = url
	// Poll the IPC socket until mpv is ready (or min(5s, ctx deadline)
	// elapses). The previous implementation slept a hard 1s regardless
	// of how fast mpv came up — on a healthy box that's an unnecessary
	// 900ms of dead time per start; on a slow box 1s wasn't enough and
	// the first property read failed with "connection refused".
	//
	// The poll exits early when mpv exits without ever creating the
	// socket (most commonly: bad URL, missing decklink plugin) or when
	// ctx is cancelled (client disconnect / shutdown). Per
	// ARCHITECTURE_REVIEW.md items 4 (mpv.go) and 7 (ctx plumbing).
	timeout := mpvStartTimeout(ctx, 5*time.Second)
	if err := waitForMPVSocket(ctx, cmd, m.socketPath, timeout); err != nil {
		// mpv hasn't come up — kill the child if it's still running
		// so we don't leave an orphan, then surface the error.
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		m.process = nil
		m.currentURL = ""
		return err
	}
	return nil
}

// mpvStartTimeout returns the smaller of `max` and the time remaining
// in ctx. If ctx has no deadline (or is already past it), max wins.
// Centralizing this keeps Start and any future callers in agreement.
func mpvStartTimeout(ctx context.Context, max time.Duration) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem > 0 && rem < max {
			return rem
		}
	}
	return max
}

// waitForMPVSocket polls socketPath every 50ms for up to timeout. Returns
// nil as soon as a connection succeeds (mpv's IPC server is ready), or
// an error when the timeout elapses, the mpv child exits prematurely,
// or ctx is cancelled.
//
// The poll cadence is chosen so a fast-startup mpv (typically 50-150ms
// on an SSD) is unblocked within one tick of being ready, but the loop
// doesn't spin on the CPU for slower startups.
func waitForMPVSocket(ctx context.Context, cmd *exec.Cmd, socketPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	dialer := net.Dialer{}
	for time.Now().Before(deadline) {
		// Respect ctx between polls so a shutdown signal lands within
		// one 50ms tick instead of waiting the full timeout.
		if err := ctx.Err(); err != nil {
			return err
		}
		dialCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		conn, err := dialer.DialContext(dialCtx, "unix", socketPath)
		cancel()
		if err == nil {
			_ = conn.Close()
			return nil
		}
		// If mpv has already exited, no amount of waiting will help.
		// ProcessState becomes non-nil once Wait() (or the
		// internal reaper) has observed the exit; we rely on the
		// caller having NOT called Wait, so this catches the
		// case where the process died on its own.
		if cmd.ProcessState != nil {
			return fmt.Errorf("mpv exited before IPC came up: %s", cmd.ProcessState)
		}
		// Ctx-aware sleep — same justification as the dial-context
		// above: cancellation propagates within a single tick.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("mpv IPC did not come up within %s", timeout)
}

// Stop kills the child mpv (if any) and waits for it to exit. ctx is
// accepted for API symmetry; the Wait is bounded by the kernel's
// SIGKILL semantics rather than by ctx so we never leak a child even if
// ctx is already expired.
func (m *MPVService) Stop(ctx context.Context) error {
	_ = ctx
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.process != nil && m.process.Process != nil {
		_ = m.process.Process.Kill()
		_, _ = m.process.Process.Wait()
		m.process = nil
	}
	if m.socketPath != "" {
		_ = os.Remove(m.socketPath)
	}
	m.currentURL = ""
	return nil
}

// IsRunning reports whether the child mpv process is alive. ctx is
// accepted for API symmetry — the probe is sync (signal-0 on Unix,
// ProcessState elsewhere) so cancellation is informational.
func (m *MPVService) IsRunning(ctx context.Context) bool {
	_ = ctx
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.process == nil || m.process.Process == nil {
		return false
	}
	if m.process.ProcessState != nil && m.process.ProcessState.Exited() {
		return false
	}
	if runtime.GOOS != "windows" {
		if err := m.process.Process.Signal(syscall.Signal(0)); err != nil {
			return false
		}
	}
	// TODO(windows): proper liveness probe. The Unix branch uses
	// signal-0 to ask the kernel if the PID is still alive; Windows
	// has no equivalent in os.Process and we don't pull in
	// golang.org/x/sys/windows just for this. The ProcessState check
	// above catches the case where we've already Wait()'d the child;
	// a hung Windows mpv that didn't get Wait()'d slips through.
	// Per ARCHITECTURE_REVIEW.md item 4 on mpv.go IsRunning.
	return true
}

// sendIPC dials the mpv IPC socket, writes the JSON command, optionally
// reads the matching response, and returns it. ctx bounds the dial and
// the read deadline: an already-cancelled ctx fails fast, and an
// in-flight read is cut short when ctx expires.
//
// The previous implementation used net.Dial (no cancellation) plus a
// hard 3s read deadline. The new version uses net.Dialer.DialContext
// for the connect and either ctx's remaining deadline (capped at 3s)
// or a flat 3s for the read deadline. So:
//
//   - request ctx with 50ms left -> dial + read both bounded to 50ms
//   - request ctx with no deadline -> 3s read deadline, ctx-free dial
//   - cancelled ctx -> immediate ctx.Err() without touching the socket
func (m *MPVService) sendIPC(ctx context.Context, command map[string]interface{}, waitForResponse bool) (map[string]interface{}, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !m.IsRunning(ctx) {
		return nil, errors.New("mpv not running")
	}

	m.mu.Lock()
	socketPath := m.socketPath
	m.mu.Unlock()

	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// Assign a request_id so we can match the response (MPV sends async events too)
	command["request_id"] = 1

	payload, err := json.Marshal(command)
	if err != nil {
		return nil, err
	}
	payload = append(payload, '\n')

	if _, err := conn.Write(payload); err != nil {
		return nil, err
	}

	if !waitForResponse {
		return nil, nil
	}

	// Read lines from the socket until we find the command response.
	// MPV sends asynchronous event lines (with "event" key) that we skip.
	// Deadline is min(ctx remaining, 3s) — see function doc.
	readDeadline := time.Now().Add(3 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(readDeadline) {
		readDeadline = dl
	}
	_ = conn.SetReadDeadline(readDeadline)

	// Also wire ctx cancellation directly into the conn so a cancel
	// that fires before the deadline still preempts the read.
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetReadDeadline(time.Now())
		case <-stopWatch:
		}
	}()

	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg map[string]interface{}
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		// Skip asynchronous event messages — they have an "event" key
		if _, isEvent := msg["event"]; isEvent {
			continue
		}
		// This is a command response (has "error" and optionally "request_id")
		return msg, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("no response from mpv")
}

// getProperty is the internal helper around get_property; every public
// caller now passes ctx through.
func (m *MPVService) getProperty(ctx context.Context, name string) (interface{}, error) {
	resp, err := m.sendIPC(ctx, map[string]interface{}{"command": []interface{}{"get_property", name}}, true)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errors.New("no response")
	}
	return resp["data"], nil
}

// GetProperty exposes the IPC get_property primitive for handlers /
// tests that want a single typed read without going through the
// convenience helpers below.
func (m *MPVService) GetProperty(ctx context.Context, name string) (interface{}, error) {
	return m.getProperty(ctx, name)
}

// SetProperty exposes the IPC set_property primitive. waitForResponse
// is false because mpv acks set_property via the same channel as the
// async event stream and we don't currently care about the ack body.
func (m *MPVService) SetProperty(ctx context.Context, name string, value interface{}) error {
	_, err := m.sendIPC(ctx, map[string]interface{}{"command": []interface{}{"set_property", name, value}}, false)
	return err
}

// SendCommand is the lowest-level IPC primitive — handlers / tests that
// need to talk to mpv directly use this rather than dialing the socket
// themselves. ctx bounds dial + read; see sendIPC doc.
func (m *MPVService) SendCommand(ctx context.Context, cmd []interface{}, waitForResponse bool) (map[string]interface{}, error) {
	return m.sendIPC(ctx, map[string]interface{}{"command": cmd}, waitForResponse)
}

// Status is the typed snapshot exposed to handlers. ctx is accepted for
// API symmetry; the local lookup is sync and ctx-cancellation just
// short-circuits the response with an empty struct.
func (m *MPVService) Status(ctx context.Context) MPVStatus {
	if err := ctx.Err(); err != nil {
		return MPVStatus{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	running := m.process != nil && m.process.Process != nil
	if running && m.process.ProcessState != nil && m.process.ProcessState.Exited() {
		running = false
	}
	return MPVStatus{
		IsRunning:  running,
		CurrentURL: m.currentURL,
		SocketPath: m.socketPath,
	}
}

func (m *MPVService) GetPosition(ctx context.Context) (float64, error) {
	val, err := m.getProperty(ctx, "time-pos")
	if err != nil {
		return 0, err
	}
	if num, ok := val.(float64); ok {
		return num, nil
	}
	return 0, errors.New("invalid time-pos")
}

func (m *MPVService) GetFrameInfo(ctx context.Context) (map[string]interface{}, error) {
	frameVal, err := m.getProperty(ctx, "estimated-frame-number")
	if err != nil {
		return nil, err
	}
	fpsVal, err := m.getProperty(ctx, "container-fps")
	if err != nil {
		return nil, err
	}
	timeVal, err := m.getProperty(ctx, "time-pos")
	if err != nil {
		return nil, err
	}

	frame, _ := frameVal.(float64)
	fps, _ := fpsVal.(float64)
	timePos, _ := timeVal.(float64)

	return map[string]interface{}{
		"frame":    int(frame),
		"fps":      fps,
		"time_pos": timePos,
	}, nil
}

func (m *MPVService) IsPaused(ctx context.Context) (bool, error) {
	val, err := m.getProperty(ctx, "pause")
	if err != nil {
		return false, err
	}
	if paused, ok := val.(bool); ok {
		return paused, nil
	}
	return false, errors.New("invalid pause state")
}

func (m *MPVService) Seek(ctx context.Context, timestamp float64) error {
	log := m.logger.With("component", "mpv", "op", "seek", "timestamp", timestamp)
	log.Info("mpv: seek requested")

	// Diagnostic context before issuing the seek.
	if dur, err := m.getProperty(ctx, "duration"); err == nil {
		log.Debug("mpv: duration", "value", dur)
	}
	if pos, err := m.getProperty(ctx, "time-pos"); err == nil {
		log.Debug("mpv: time-pos before seek", "value", pos)
	}
	if seekable, err := m.getProperty(ctx, "seekable"); err == nil {
		log.Debug("mpv: seekable", "value", seekable)
	}

	// Use "absolute" seek — "exact" flag requires decoding to the exact frame
	// which may fail on live/growing HLS playlists.
	resp, err := m.sendIPC(ctx, map[string]interface{}{"command": []interface{}{"seek", timestamp, "absolute"}}, true)
	if err != nil {
		log.Warn("mpv: seek failed", "err", err)
		return err
	}
	log.Debug("mpv: seek response", "response", resp)

	// Give mpv a moment to process the seek, then check position.
	// Ctx-aware sleep so a client disconnect during the post-seek
	// settle window doesn't strand the handler.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(200 * time.Millisecond):
	}
	if pos, err := m.getProperty(ctx, "time-pos"); err == nil {
		log.Debug("mpv: time-pos after seek", "value", pos)
	}
	return nil
}

func (m *MPVService) Play(ctx context.Context) error {
	_, err := m.sendIPC(ctx, map[string]interface{}{"command": []interface{}{"set_property", "pause", false}}, false)
	return err
}

func (m *MPVService) Pause(ctx context.Context) error {
	_, err := m.sendIPC(ctx, map[string]interface{}{"command": []interface{}{"set_property", "pause", true}}, false)
	return err
}

func (m *MPVService) Load(ctx context.Context, url string) error {
	_, err := m.sendIPC(ctx, map[string]interface{}{"command": []interface{}{"loadfile", url}}, false)
	if err == nil {
		m.mu.Lock()
		m.currentURL = url
		m.mu.Unlock()
	}
	return err
}

func (m *MPVService) SetMute(ctx context.Context, muted bool) error {
	_, err := m.sendIPC(ctx, map[string]interface{}{"command": []interface{}{"set_property", "mute", muted}}, false)
	return err
}

func (m *MPVService) GetCurrentURL() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentURL
}

// GetDiagnostics probes a fixed list of player properties. ctx bounds
// the entire batch — if the first read times out, the rest are skipped
// rather than each independently waiting the full 3s.
func (m *MPVService) GetDiagnostics(ctx context.Context) map[string]interface{} {
	diag := map[string]interface{}{}
	props := []string{
		"time-pos", "duration", "seekable", "playback-time",
		"demuxer-cache-duration", "demuxer-cache-state",
		"paused-for-cache", "cache-buffering-state",
		"file-format", "file-size",
	}
	for _, prop := range props {
		if err := ctx.Err(); err != nil {
			diag[prop] = "error: " + err.Error()
			continue
		}
		val, err := m.getProperty(ctx, prop)
		if err != nil {
			diag[prop] = "error: " + err.Error()
		} else {
			diag[prop] = val
		}
	}
	return diag
}

func itoa(val int) string {
	return strconv.Itoa(val)
}
