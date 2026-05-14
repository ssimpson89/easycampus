//go:build windows

package ffmpeg

import (
	"os/exec"
)

// setSysProcAttr is a no-op on Windows. ffmpeg on Windows runs without a
// dedicated process group; SIGTERM doesn't exist there. Termination
// happens via Process.Kill() which sends TerminateProcess.
func setSysProcAttr(cmd *exec.Cmd) {}

// sendSIGTERM on Windows can't actually deliver SIGTERM. The graceful
// path is to spawn `taskkill` without /F, but that races badly with the
// supervisor's wait channel and ffmpeg drops the trailing seconds of
// output. We return nil here and rely on the SIGKILL fallback after the
// grace window to actually terminate.
func sendSIGTERM(pid int) error {
	return nil
}

// sendSIGKILL terminates the process via the Windows TerminateProcess API.
// We use the *exec.Cmd's Process.Kill() because it carries the handle
// FFmpeg was spawned with; just looking up the PID would force a separate
// OpenProcess call that may race the process exiting.
func sendSIGKILL(pid int, cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
