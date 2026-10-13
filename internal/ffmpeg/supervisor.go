package ffmpeg

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// State is the lifecycle phase of the supervised FFmpeg process.
type State string

const (
	StateIdle       State = "idle"
	StateStarting   State = "starting"
	StateRunning    State = "running"
	StateDegraded   State = "degraded"
	StateRestarting State = "restarting"
	StateStopping   State = "stopping"
	StateFailed     State = "failed"
)

// Status is the public snapshot of supervisor state.
type Status struct {
	State        State     `json:"state"`
	EventID      string    `json:"eventId,omitempty"`
	StartedAt    time.Time `json:"startedAt,omitempty"`
	UpdatedAt    time.Time `json:"updatedAt"`
	RestartCount int       `json:"restartCount"`
	LastExit     string    `json:"lastExit,omitempty"`
	LastError    string    `json:"lastError,omitempty"`
	LastLogLine  string    `json:"lastLogLine,omitempty"`
	LastProgress Progress  `json:"lastProgress"`
	// LastAdvanceAt is when Frame or OutTimeUS last moved forward. The
	// stall watchdog keys off this, not LastProgress.UpdatedAt: FFmpeg
	// keeps emitting progress=continue blocks with a frozen frame count
	// when the capture device wedges.
	LastAdvanceAt time.Time `json:"lastAdvanceAt,omitempty"`
	PresetID      string    `json:"presetId,omitempty"`
	PID           int       `json:"pid,omitempty"`
}

// SupervisorConfig controls restart backoff + stall detection.
type SupervisorConfig struct {
	RestartInitialDelay time.Duration
	RestartMaxDelay     time.Duration
	StableAfter         time.Duration // process must run this long to reset the backoff
	ProgressStallAfter  time.Duration // no -progress for this long => declare stalled
	MaxRestarts         int           // crash-restart budget; reset after StableAfter of healthy running
	PidFilePath         string        // empty disables pid-file reaping
}

// Supervisor runs FFmpeg with crash-restart, stall detection, and graceful
// stop. The single instance is reused across multiple StartXxx calls (one
// active stream at a time) — call Stop before starting again.
type Supervisor struct {
	mu     sync.Mutex
	cfg    SupervisorConfig
	logger *log.Logger

	cancel  context.CancelFunc
	done    chan struct{}
	config  Config
	eventID string
	status  Status
	pidFile *PidFile
	restart chan string
	// generation increments on every Start so SwapPreset can refuse to
	// act on a stream that was stopped (or replaced) in the meantime.
	generation uint64

	onRestart     func()
	onStateChange func()
}

var (
	errRestartRequested = errors.New("supervisor restart requested")
	errProgressStalled  = errors.New("ffmpeg progress stalled")

	// ErrNotRunning is returned by SwapPreset when there is no active
	// stream (or the stream it targeted has since been stopped).
	ErrNotRunning = errors.New("stream is not running")
)

// NewSupervisor builds a supervisor with sensible defaults applied to any
// zero fields in cfg.
func NewSupervisor(logger *log.Logger, cfg SupervisorConfig) *Supervisor {
	if logger == nil {
		logger = log.Default()
	}
	if cfg.RestartInitialDelay <= 0 {
		cfg.RestartInitialDelay = time.Second
	}
	if cfg.RestartMaxDelay <= 0 {
		cfg.RestartMaxDelay = 30 * time.Second
	}
	if cfg.StableAfter <= 0 {
		cfg.StableAfter = 2 * time.Minute
	}
	if cfg.ProgressStallAfter <= 0 {
		cfg.ProgressStallAfter = 20 * time.Second
	}
	if cfg.MaxRestarts <= 0 {
		cfg.MaxRestarts = 20
	}
	s := &Supervisor{
		cfg:    cfg,
		logger: logger,
		status: Status{State: StateIdle, UpdatedAt: time.Now().UTC()},
	}
	if cfg.PidFilePath != "" {
		s.pidFile = &PidFile{Path: cfg.PidFilePath}
		if reaped, err := s.pidFile.ReapOrphan(); err != nil {
			logger.Printf("supervisor: pid file reap error: %v", err)
		} else if reaped > 0 {
			logger.Printf("supervisor: reaped orphan FFmpeg (pid %d) from previous session", reaped)
		}
	}
	return s
}

// SetOnRestart installs a callback invoked whenever FFmpeg exits non-cleanly
// and is about to be restarted. Invoked without the supervisor lock held —
// it's safe to call back into Status() or anything else that takes the
// supervisor lock. Used by the adaptive controller.
func (s *Supervisor) SetOnRestart(fn func()) {
	s.mu.Lock()
	s.onRestart = fn
	s.mu.Unlock()
}

// SetOnStateChange installs a callback invoked on every state transition.
// Same lock contract as SetOnRestart.
func (s *Supervisor) SetOnStateChange(fn func()) {
	s.mu.Lock()
	s.onStateChange = fn
	s.mu.Unlock()
}

func (s *Supervisor) emitStateChange() {
	s.mu.Lock()
	fn := s.onStateChange
	s.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// CurrentConfig returns a copy of the currently-configured FFmpeg config.
func (s *Supervisor) CurrentConfig() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.config
}

// IsRunning reports whether the supervisor has an active FFmpeg invocation.
func (s *Supervisor) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancel != nil
}

// Start launches FFmpeg under supervision with the given config and event ID.
// The event ID is logged and surfaced in Status; it has no effect on argv.
// Returns nil iff the supervisor goroutine has been spawned.
func (s *Supervisor) Start(config Config, eventID string) error {
	if err := config.Validate(); err != nil {
		return err
	}
	if _, err := config.Args(); err != nil {
		return err
	}

	s.mu.Lock()
	if s.cancel != nil {
		s.mu.Unlock()
		return errors.New("stream is already running")
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan struct{})
	s.restart = make(chan string, 1)
	s.config = config
	s.eventID = eventID
	s.generation++
	s.status = Status{
		State:     StateStarting,
		EventID:   eventID,
		StartedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		PresetID:  config.Preset.ID(),
	}
	s.mu.Unlock()

	s.emitStateChange()
	go s.run(ctx)
	return nil
}

// Restart asks the running supervisor loop to bounce FFmpeg while keeping
// the stream intent active. Returns false when no active stream.
//
// Requested restarts don't count against MaxRestarts and skip backoff.
// On live HLS the next run appends to the existing playlist.
func (s *Supervisor) Restart(reason string) bool {
	s.mu.Lock()
	if s.cancel == nil || s.restart == nil {
		s.mu.Unlock()
		return false
	}
	s.requestRestartLocked(reason)
	s.mu.Unlock()
	s.emitStateChange()
	return true
}

func (s *Supervisor) requestRestartLocked(reason string) {
	if reason == "" {
		reason = "restart requested"
	}
	s.status.State = StateDegraded
	s.status.LastError = reason
	s.status.UpdatedAt = time.Now().UTC()
	select {
	case s.restart <- reason:
	default:
	}
}

// Generation identifies the current stream: it increments on every
// Start. Pair with SwapPresetIfGeneration to make sure a decision made
// against one stream isn't applied to a later one.
func (s *Supervisor) Generation() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation
}

// SwapPreset atomically replaces the running stream's preset and
// restarts FFmpeg with it, keeping the event ID and (on live HLS)
// appending to the existing playlist. Returns ErrNotRunning when no
// stream is active; it never starts a stopped stream.
func (s *Supervisor) SwapPreset(p QualityPreset, reason string) error {
	return s.SwapPresetIfGeneration(p, reason, 0)
}

// SwapPresetIfGeneration is SwapPreset guarded by a generation check:
// when gen != 0 and the supervisor has been restarted via Start since
// gen was read, it returns ErrNotRunning and changes nothing.
func (s *Supervisor) SwapPresetIfGeneration(p QualityPreset, reason string, gen uint64) error {
	if p == nil {
		return errors.New("preset is required")
	}
	s.mu.Lock()
	if s.cancel == nil || (gen != 0 && s.generation != gen) {
		s.mu.Unlock()
		return ErrNotRunning
	}
	gen = s.generation
	cfg := s.config
	s.mu.Unlock()

	// Validate outside the lock: Args() may probe encoders.
	cfg.Preset = p
	if _, err := cfg.Args(); err != nil {
		return err
	}

	s.mu.Lock()
	if s.cancel == nil || s.restart == nil || s.generation != gen {
		s.mu.Unlock()
		return ErrNotRunning
	}
	s.config.Preset = p
	s.status.PresetID = p.ID()
	if reason == "" {
		reason = "quality preset changed to " + p.ID()
	}
	s.requestRestartLocked(reason)
	s.mu.Unlock()
	s.emitStateChange()
	return nil
}

// Stop signals the supervised FFmpeg to exit gracefully and waits for the
// run goroutine to return.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	cancel := s.cancel
	done := s.done
	changed := false
	if cancel != nil {
		s.status.State = StateStopping
		s.status.UpdatedAt = time.Now().UTC()
		changed = true
	}
	s.mu.Unlock()

	if changed {
		s.emitStateChange()
	}
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// Status returns the current public snapshot, applying stall detection.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.status
	if (status.State == StateRunning || status.State == StateDegraded) &&
		status.progressStalled(s.cfg.ProgressStallAfter, time.Now()) {
		status.State = StateDegraded
		if status.LastError == "" {
			status.LastError = "FFmpeg progress has stalled"
		}
	}
	return status
}

// progressStalled reports whether frame / out_time has failed to
// advance for longer than `after`. Before the first advance it measures
// from StartedAt (the run start).
func (s Status) progressStalled(after time.Duration, now time.Time) bool {
	if after <= 0 {
		return false
	}
	if !s.LastAdvanceAt.IsZero() {
		return now.Sub(s.LastAdvanceAt) > after
	}
	return !s.StartedAt.IsZero() && now.Sub(s.StartedAt) > after
}

func (s *Supervisor) progressStalledSince(started time.Time) (bool, string) {
	s.mu.Lock()
	status := s.status
	after := s.cfg.ProgressStallAfter
	s.mu.Unlock()
	now := time.Now()
	if !status.progressStalled(after, now) {
		return false, ""
	}
	if status.LastAdvanceAt.IsZero() {
		return true, fmt.Sprintf("FFmpeg produced no frames for %s", now.Sub(started).Round(time.Second))
	}
	return true, fmt.Sprintf("FFmpeg output frozen for %s (frame %d)",
		now.Sub(status.LastAdvanceAt).Round(time.Second), status.LastProgress.Frame)
}

func (s *Supervisor) markDegraded(reason string) {
	s.mu.Lock()
	s.status.State = StateDegraded
	s.status.LastError = reason
	s.status.UpdatedAt = time.Now().UTC()
	s.mu.Unlock()
	s.emitStateChange()
}

func (s *Supervisor) run(ctx context.Context) {
	defer func() {
		s.mu.Lock()
		done := s.done
		restart := s.restart
		s.cancel = nil
		s.done = nil
		s.restart = nil
		changed := false
		if s.status.State == StateStopping {
			s.status.State = StateIdle
			s.status.UpdatedAt = time.Now().UTC()
			changed = true
		}
		s.mu.Unlock()
		if changed {
			s.emitStateChange()
		}
		if done != nil {
			close(done)
		}
		if restart != nil {
			close(restart)
		}
	}()

	crashes := 0       // crash/stall restarts since the last stable run
	totalRestarts := 0 // every restart, surfaced in Status.RestartCount
	quickFailures := 0 // back-to-back failures inside quickFailWindow
	const quickFailWindow = 5 * time.Second
	const quickFailLimit = 2
	delay := s.cfg.RestartInitialDelay
	for run := 0; ; run++ {
		started := time.Now()
		err := s.runOnce(ctx, run)

		if ctx.Err() != nil {
			s.setExit(StateStopping, "stopped by user", "")
			return
		}

		elapsed := time.Since(started)
		lastExit := exitMessage(err)
		requested := errors.Is(err, errRestartRequested)
		totalRestarts++

		// A run that stayed up for StableAfter resets the crash budget
		// and the backoff; MaxRestarts bounds crash *storms*, not the
		// lifetime of a service-length stream.
		if elapsed >= s.cfg.StableAfter {
			crashes = 0
			quickFailures = 0
			delay = s.cfg.RestartInitialDelay
		}

		s.mu.Lock()
		s.status.RestartCount = totalRestarts
		s.status.LastExit = lastExit
		s.status.UpdatedAt = time.Now().UTC()
		onRestart := s.onRestart
		s.mu.Unlock()

		if requested {
			// Operator / adaptive-controller restart: not a failure.
			// No budget, no backoff, no onRestart (it feeds the
			// adaptive controller's instability detector).
			continue
		}

		crashes++
		if elapsed < quickFailWindow {
			quickFailures++
		} else {
			quickFailures = 0
		}
		if onRestart != nil {
			onRestart()
		}

		if quickFailures >= quickFailLimit {
			s.setExit(StateFailed, lastExit,
				"FFmpeg failed to start twice in a row. Check the capture device and output directory permissions.")
			return
		}
		if crashes > s.cfg.MaxRestarts {
			s.setExit(StateFailed, lastExit, "FFmpeg crashed too many times")
			return
		}
		wait := withJitter(delay)
		s.setRestarting(lastExit, wait)
		if !sleepContext(ctx, wait) {
			s.setExit(StateStopping, "stopped by user", "")
			return
		}
		delay *= 2
		if delay > s.cfg.RestartMaxDelay {
			delay = s.cfg.RestartMaxDelay
		}
	}
}

// runOnce runs one FFmpeg process. `run` is 0 for the first run of a
// stream; later runs on live HLS append to the existing playlist and use
// a fresh fMP4 init filename.
func (s *Supervisor) runOnce(ctx context.Context, run int) error {
	s.mu.Lock()
	// A restart requested while we were in backoff is satisfied by this
	// run (it reads the latest config below); drop it so the new process
	// isn't bounced immediately.
	select {
	case <-s.restart:
	default:
	}
	cfg := s.config
	s.mu.Unlock()
	if run > 0 && cfg.Mode == ModeLiveHLS {
		cfg.HLSAppend = true
		cfg.HLSInitFile = RunInitFilename(cfg.HLSInitFile, run)
	}
	args, err := cfg.Args()
	if err != nil {
		return err
	}

	cmd := exec.Command(cfg.Binary, args...)
	setSysProcAttr(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	processStarted := time.Now()
	if err := cmd.Start(); err != nil {
		// Not terminal: the run loop retries start failures under the
		// same quick-failure and crash budget as any other exit, and only
		// it publishes StateFailed once retries are exhausted. Publishing
		// it here made listeners tear down an event that a retry then
		// brought back up. Record the reason so Status shows it meanwhile.
		s.mu.Lock()
		s.status.LastError = err.Error()
		s.status.UpdatedAt = time.Now().UTC()
		s.mu.Unlock()
		return fmt.Errorf("start ffmpeg: %w", err)
	}

	if s.pidFile != nil {
		if err := s.pidFile.Write(cmd.Process.Pid); err != nil {
			s.logger.Printf("supervisor: failed to write pid file: %v", err)
		}
	}

	s.mu.Lock()
	s.status.State = StateRunning
	s.status.StartedAt = time.Now().UTC()
	s.status.LastProgress = Progress{}
	s.status.LastAdvanceAt = time.Time{}
	s.status.LastError = ""
	s.status.PID = cmd.Process.Pid
	s.status.UpdatedAt = time.Now().UTC()
	preset := s.status.PresetID
	s.mu.Unlock()

	s.emitStateChange()
	s.logger.Printf("stream-start: mode=%s preset=%s codec=%s run=%d pid=%d",
		cfg.Mode, preset, cfg.EffectiveVideoCodec(), run, cmd.Process.Pid)

	// Readers must drain to EOF before cmd.Wait (Wait closes the pipes
	// and would drop FFmpeg's final stderr lines — usually the error).
	// So Wait runs only after both readers finish; kill/terminate is
	// driven from the select below.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = ParseProgress(stdout, s.recordProgress)
	}()
	go func() {
		defer wg.Done()
		s.recordLogs(stderr, cfg.StreamKey)
	}()
	waitCh := make(chan error, 1)
	go func() {
		wg.Wait()
		waitCh <- cmd.Wait()
	}()

	stallTicker := time.NewTicker(time.Second)
	defer stallTicker.Stop()
	s.mu.Lock()
	restartCh := s.restart
	s.mu.Unlock()

	finish := func() {
		if s.pidFile != nil {
			s.pidFile.Clear()
		}
	}

	for {
		select {
		case err := <-waitCh:
			finish()
			s.logger.Printf("stream-end: ffmpeg exited after %s (%s)",
				time.Since(processStarted).Round(time.Second), exitMessage(err))
			return err

		case reason := <-restartCh:
			if reason == "" {
				reason = "restart requested"
			}
			s.logger.Printf("supervisor: restarting FFmpeg: %s", reason)
			err := terminateCommand(cmd, waitCh, 5*time.Second)
			finish()
			if err != nil {
				return fmt.Errorf("%w: %s (%v)", errRestartRequested, reason, err)
			}
			return fmt.Errorf("%w: %s", errRestartRequested, reason)

		case <-stallTicker.C:
			if stalled, reason := s.progressStalledSince(processStarted); stalled {
				s.logger.Printf("supervisor: restarting FFmpeg: %s", reason)
				s.markDegraded(reason)
				err := terminateCommand(cmd, waitCh, 5*time.Second)
				finish()
				if err != nil {
					return fmt.Errorf("%w: %s (%v)", errProgressStalled, reason, err)
				}
				return fmt.Errorf("%w: %s", errProgressStalled, reason)
			}

		case <-ctx.Done():
			s.logger.Printf("supervisor: stopping FFmpeg (pid %d)", cmd.Process.Pid)
			err := terminateCommand(cmd, waitCh, 5*time.Second)
			finish()
			return err
		}
	}
}

func (s *Supervisor) recordProgress(progress Progress) {
	s.mu.Lock()
	prev := s.status.LastProgress
	advanced := progress.Frame > prev.Frame || progress.OutTimeUS > prev.OutTimeUS
	s.status.LastProgress = progress
	s.status.UpdatedAt = time.Now().UTC()
	if advanced {
		s.status.LastAdvanceAt = progress.UpdatedAt
		if s.status.LastAdvanceAt.IsZero() {
			s.status.LastAdvanceAt = time.Now().UTC()
		}
	}
	// Only real forward progress clears a degraded state; a frozen
	// frame counter must not flip the stream back to "running".
	recovered := advanced && s.status.State == StateDegraded
	if recovered {
		s.status.State = StateRunning
		s.status.LastError = ""
	}
	s.mu.Unlock()
	if recovered {
		s.emitStateChange()
	}
}

func (s *Supervisor) recordLogs(r io.Reader, streamKey string) {
	scanner := bufio.NewScanner(r)
	// FFmpeg can emit very long lines in -loglevel debug; the default
	// 64k token is enough for our default loglevel error but we bump it
	// to 1MB for safety.
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		// Order matters: scrub URLs first (net/url-based), then stream key.
		redacted := redactStreamKey(RedactURLsInLog(line), streamKey)
		hint := classifyFFmpegError(redacted)
		s.mu.Lock()
		s.status.LastLogLine = redacted
		if hint != "" {
			s.status.LastError = hint
		}
		s.status.UpdatedAt = time.Now().UTC()
		s.mu.Unlock()
		if s.logger != nil {
			s.logger.Printf("ffmpeg: %s", redacted)
		}
	}
}

// classifyFFmpegError pattern-matches known fatal FFmpeg messages and
// returns a volunteer-friendly explanation. Empty string means "no
// useful hint".
func classifyFFmpegError(line string) string {
	l := strings.ToLower(line)
	switch {
	case strings.Contains(l, "no such file or directory") && strings.Contains(l, "input"):
		return "Capture device not found. Plug it in or pick a different source."
	case strings.Contains(l, "permission denied"),
		strings.Contains(l, "operation not permitted"):
		return "Permission denied. Grant camera/microphone access in OS privacy settings (macOS) or add the user to the video/audio groups (Linux)."
	case strings.Contains(l, "device or resource busy"),
		strings.Contains(l, "device i/o error"),
		strings.Contains(l, "no av capture device"):
		return "Capture device is busy or unavailable. Close other apps using it (FaceTime, Zoom, OBS)."
	case strings.Contains(l, "connection refused"):
		return "Could not reach the destination. Check the URL and your internet connection."
	case strings.Contains(l, "connection timed out"),
		strings.Contains(l, "timeout"):
		return "Connection timed out. Check your network."
	}
	return ""
}

func (s *Supervisor) setRestarting(lastExit string, delay time.Duration) {
	s.mu.Lock()
	s.status.State = StateRestarting
	s.status.LastExit = lastExit
	if s.status.LastError == "" {
		s.status.LastError = fmt.Sprintf("restarting FFmpeg in %s", delay.Round(time.Second))
	}
	s.status.UpdatedAt = time.Now().UTC()
	s.mu.Unlock()
	s.emitStateChange()
}

func (s *Supervisor) setExit(state State, lastExit, lastError string) {
	s.mu.Lock()
	s.status.State = state
	s.status.LastExit = lastExit
	s.status.LastError = lastError
	s.status.UpdatedAt = time.Now().UTC()
	s.mu.Unlock()
	s.emitStateChange()
}

func exitMessage(err error) string {
	if err == nil {
		return "FFmpeg exited cleanly"
	}
	return err.Error()
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func terminateCommand(cmd *exec.Cmd, waitCh <-chan error, grace time.Duration) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	_ = sendSIGTERM(pid)
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case err := <-waitCh:
		return err
	case <-timer.C:
		_ = sendSIGKILL(pid, cmd)
		return <-waitCh
	}
}

func withJitter(delay time.Duration) time.Duration {
	if delay <= 0 {
		return delay
	}
	spread := int64(delay / 4)
	if spread <= 0 {
		return delay
	}
	return delay + time.Duration(rand.Int63n(spread))
}
