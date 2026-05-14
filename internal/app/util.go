package app

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

func parsePID(val string) (int, error) {
	pid, err := strconv.Atoi(val)
	if err != nil || pid <= 0 {
		return 0, errors.New("invalid pid")
	}
	return pid, nil
}

func terminateProcess(pid int) error {
	if pid <= 0 {
		return errors.New("invalid pid")
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}

	if runtime.GOOS == "windows" {
		return process.Kill()
	}

	_ = process.Signal(os.Interrupt)
	time.Sleep(500 * time.Millisecond)
	return process.Kill()
}

func isProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		err = process.Signal(os.Interrupt)
		return err == nil
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil
}

func padInt(value, width int) string {
	return fmt.Sprintf("%0*d", width, value)
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
