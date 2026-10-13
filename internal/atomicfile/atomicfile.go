// Package atomicfile writes files atomically: write to a temp file in the
// same directory, fsync, then rename over the target. A crash or power loss
// can never leave a half-written file at the target path.
//
// Used for every persisted-state file (event info.json, PID files). A
// truncated info.json silently parses as an empty struct and erases a
// recording's metadata — atomic writes prevent that.
package atomicfile

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
)

// Write writes data to path atomically with the given mode.
//
// MkdirAll on the parent is performed if needed (dir mode 0755). The temp
// file is created in the same directory as the target so the final rename
// is a same-filesystem operation (atomic on POSIX).
func Write(path string, data []byte, mode os.FileMode) error {
	return WriteFrom(path, bytes.NewReader(data), mode)
}

// WriteFrom streams r into path atomically with the given mode. Same
// guarantees as Write, without buffering the whole payload in memory —
// used for downloaded media segments.
func WriteFrom(path string, r io.Reader, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmpPath := f.Name()
	// On any error path below, remove the temp file so we don't litter.
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return err
	}
	return nil
}
