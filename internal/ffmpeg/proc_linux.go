//go:build linux

package ffmpeg

import (
	"os/exec"
	"syscall"
)

// setSysProcAttr puts ffmpeg into its own process group so we can signal
// the group (and reap helper children) deterministically, and asks the
// kernel to SIGTERM it if easycampus dies (Pdeathsig) so a crashed
// parent doesn't leave an orphan encoder holding the capture device.
//
// Caveat: Pdeathsig fires when the OS *thread* that forked the child
// exits, not the process. The Go runtime doesn't retire threads in
// normal operation (only LockOSThread goroutines that exit locked), and
// the pid-file reaper remains the backstop.
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
}
