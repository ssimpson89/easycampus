package ffmpeg

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"easycampus/internal/atomicfile"
)

// PidFile records the FFmpeg child PID on disk so we can detect
// easycampus-go orphans after a crash or kill -9. We do not adopt
// those processes: once the parent dies their progress/audio/error
// pipes are gone, so startup reaps the orphan and a fresh, fully
// observable FFmpeg is started by the supervisor when intent calls for it.
type PidFile struct {
	Path string
}

// Write atomically records the PID. Safe to call with Path="" (no-op).
func (p *PidFile) Write(pid int) error {
	if p == nil || p.Path == "" {
		return nil
	}
	return atomicfile.Write(p.Path, []byte(strconv.Itoa(pid)), 0644)
}

// Clear removes the PID file. Safe when no file exists.
func (p *PidFile) Clear() {
	if p == nil || p.Path == "" {
		return
	}
	_ = os.Remove(p.Path)
}

// ReapOrphan checks for a leftover FFmpeg process from a previous session.
// Returns the reaped PID (0 if nothing to reap) and any error.
//
// We verify the PID is actually ffmpeg before killing it — defensive in
// case PIDs got recycled to an unrelated process between runs.
func (p *PidFile) ReapOrphan() (int, error) {
	if p == nil || p.Path == "" {
		return 0, nil
	}
	data, err := os.ReadFile(p.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		p.Clear()
		return 0, nil
	}

	proc, err := os.FindProcess(pid)
	if err != nil || proc == nil {
		p.Clear()
		return 0, nil
	}
	// Signal 0 = liveness probe without affecting the process.
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		// Already dead.
		p.Clear()
		return 0, nil
	}

	// Confirm it's an FFmpeg process before killing. PIDs recycle, and the
	// host may have unrelated FFmpeg jobs running.
	if !isFFmpegProcess(pid) {
		p.Clear()
		return 0, nil
	}

	// Graceful shutdown first, then force kill.
	_ = proc.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if proc.Signal(syscall.Signal(0)) != nil {
			p.Clear()
			return pid, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = proc.Kill()
	p.Clear()
	return pid, nil
}

// isFFmpegProcess checks whether a PID is an ffmpeg process. We don't
// require easycampus-go-specific command-line markers because most
// deployments will have at most one ffmpeg owned by this process (live
// stream OR an upload conversion, not both with overlap on the same pid
// file). We only require argv[0] basename == "ffmpeg" so the reaper
// won't kill an unrelated `go run` or `ssh` that recycled this PID.
func isFFmpegProcess(pid int) bool {
	line, err := processCommand(pid)
	if err != nil {
		return false
	}
	return isFFmpegCommandLine(line)
}

// processCommand returns the full command line of pid.
//
// On Linux, read /proc/<pid>/cmdline directly: no subprocess, no PATH
// dependency, and no truncation. procps `ps -o command=` defaults to
// COLUMNS-truncated output, which silently breaks longer ffmpeg argvs.
//
// On macOS, fall back to BSD `ps -ww` which gives unlimited width.
func processCommand(pid int) (string, error) {
	if path := fmt.Sprintf("/proc/%d/cmdline", pid); fileExists(path) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		s := strings.ReplaceAll(strings.TrimRight(string(raw), "\x00"), "\x00", " ")
		return s, nil
	}
	out, err := exec.Command("ps", "-ww", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func isFFmpegCommandLine(line string) bool {
	if line == "" {
		return false
	}
	// `ps` returns the full command line; take the first whitespace-
	// separated field, which is argv[0], and check its basename.
	if idx := strings.IndexAny(line, " \t"); idx >= 0 {
		line = line[:idx]
	}
	base := line
	if idx := strings.LastIndex(line, "/"); idx >= 0 {
		base = line[idx+1:]
	}
	return strings.EqualFold(base, "ffmpeg")
}
