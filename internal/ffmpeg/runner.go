package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// Job represents a one-shot FFmpeg invocation (upload conversion, external
// source ingest, anything that runs to completion rather than under the
// crash-restart supervisor). Live streams use Supervisor; everything else
// uses Job.
//
// The job captures stderr to a logFile (with credential redaction), parses
// -progress stdout for the UI, and updates a status callback as it runs.
type Job struct {
	cfg       Config
	logFile   *os.File
	logPath   string
	streamKey string

	mu       sync.Mutex
	pid      int
	finished bool
	err      error
	progress Progress
	startedAt time.Time
	stoppedAt time.Time

	cancel  context.CancelFunc
	done    chan struct{}

	onProgress func(Progress)
	onComplete func(err error)
}

// JobOptions configures behaviour beyond the Config itself.
type JobOptions struct {
	// LogPath is the file where stderr is written (with redaction).
	// Empty disables file logging (stderr still gets routed to os.Stderr).
	LogPath string
	// StreamKey is redacted from any line we write to LogPath.
	StreamKey string
	// OnProgress fires for every -progress block. Optional.
	OnProgress func(Progress)
	// OnComplete fires once with the exit error (nil on success). Optional.
	// Invoked from the runner goroutine after cleanup.
	OnComplete func(err error)
}

// StartJob spawns ffmpeg under the given config + options. The returned
// Job has its goroutine running; call Wait to block on completion, or
// Cancel to send SIGTERM.
func StartJob(cfg Config, opts JobOptions) (*Job, error) {
	args, err := cfg.Args()
	if err != nil {
		return nil, err
	}

	j := &Job{
		cfg:        cfg,
		logPath:    opts.LogPath,
		streamKey:  opts.StreamKey,
		onProgress: opts.OnProgress,
		onComplete: opts.OnComplete,
		done:       make(chan struct{}),
	}

	if opts.LogPath != "" {
		if err := os.MkdirAll(filepath.Dir(opts.LogPath), 0755); err != nil {
			return nil, err
		}
		f, err := os.Create(opts.LogPath)
		if err != nil {
			return nil, err
		}
		j.logFile = f
	}

	ctx, cancel := context.WithCancel(context.Background())
	j.cancel = cancel

	cmd := exec.Command(cfg.Binary, args...)
	setSysProcAttr(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		j.closeLog()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		j.closeLog()
		return nil, err
	}

	j.startedAt = time.Now()
	if err := cmd.Start(); err != nil {
		cancel()
		j.closeLog()
		return nil, err
	}
	j.mu.Lock()
	j.pid = cmd.Process.Pid
	j.mu.Unlock()

	go j.run(ctx, cmd, stdout, stderr)
	return j, nil
}

func (j *Job) run(ctx context.Context, cmd *exec.Cmd, stdout, stderr io.ReadCloser) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = ParseProgress(stdout, func(p Progress) {
			j.mu.Lock()
			j.progress = p
			j.mu.Unlock()
			if j.onProgress != nil {
				j.onProgress(p)
			}
		})
	}()
	go func() {
		defer wg.Done()
		j.writeLog(stderr)
	}()

	// Watch ctx for cancellation while waiting on the process.
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	var exitErr error
	select {
	case exitErr = <-waitCh:
	case <-ctx.Done():
		exitErr = terminateCommand(cmd, waitCh, 5*time.Second)
		if exitErr == nil {
			exitErr = errors.New("job cancelled")
		}
	}
	wg.Wait()
	j.closeLog()

	j.mu.Lock()
	j.finished = true
	j.err = exitErr
	j.stoppedAt = time.Now()
	j.mu.Unlock()

	if j.onComplete != nil {
		j.onComplete(exitErr)
	}
	close(j.done)
}

// writeLog reads ffmpeg's stderr, redacts URL credentials and stream key,
// and writes each line to logFile + os.Stderr.
func (j *Job) writeLog(r io.Reader) {
	buf := make([]byte, 4096)
	carry := ""
	for {
		n, err := r.Read(buf)
		if n > 0 {
			text := carry + string(buf[:n])
			carry = ""
			for {
				idx := indexByte(text, '\n')
				if idx < 0 {
					carry = text
					break
				}
				line := text[:idx]
				text = text[idx+1:]
				redacted := redactStreamKey(RedactURLsInLog(line), j.streamKey)
				if j.logFile != nil {
					_, _ = j.logFile.WriteString(redacted + "\n")
				}
				_, _ = os.Stderr.WriteString("ffmpeg: " + redacted + "\n")
			}
		}
		if err != nil {
			if carry != "" {
				redacted := redactStreamKey(RedactURLsInLog(carry), j.streamKey)
				if j.logFile != nil {
					_, _ = j.logFile.WriteString(redacted + "\n")
				}
				_, _ = os.Stderr.WriteString("ffmpeg: " + redacted + "\n")
			}
			return
		}
	}
}

// indexByte avoids strings.IndexByte to keep the file dep-free for tests.
func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func (j *Job) closeLog() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.logFile != nil {
		_ = j.logFile.Close()
		j.logFile = nil
	}
}

// PID returns the FFmpeg process ID. Zero before Start or after the
// process is reaped.
func (j *Job) PID() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.pid
}

// Done returns a channel closed when the job exits.
func (j *Job) Done() <-chan struct{} { return j.done }

// Wait blocks until the job completes and returns the exit error (nil
// on success).
func (j *Job) Wait() error {
	<-j.done
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.err
}

// Cancel sends SIGTERM to the process group. Returns immediately; use
// Wait to block until the job has fully exited.
func (j *Job) Cancel() {
	j.cancel()
}

// Progress returns the most recent -progress sample.
func (j *Job) Progress() Progress {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.progress
}

// IsFinished reports whether the job has exited.
func (j *Job) IsFinished() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.finished
}

// ExitError returns the exit error (nil if still running or successful).
func (j *Job) ExitError() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.err
}

// Elapsed returns how long the job has been running (or ran if finished).
func (j *Job) Elapsed() time.Duration {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.startedAt.IsZero() {
		return 0
	}
	if j.finished && !j.stoppedAt.IsZero() {
		return j.stoppedAt.Sub(j.startedAt)
	}
	return time.Since(j.startedAt)
}

// RunOnce is a convenience helper for "fire and wait" callers (tests
// mostly). Spawns a Job, waits, and returns the exit error.
func RunOnce(cfg Config, opts JobOptions) error {
	j, err := StartJob(cfg, opts)
	if err != nil {
		return fmt.Errorf("start: %w", err)
	}
	return j.Wait()
}
