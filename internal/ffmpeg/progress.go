package ffmpeg

import (
	"bufio"
	"io"
	"strconv"
	"strings"
	"time"
)

// Progress represents one snapshot of FFmpeg's `-progress` stdout output.
//
// FFmpeg emits a block of key=value lines roughly every second, terminated
// with `progress=continue` (or `progress=end` on clean exit). ParseProgress
// consumes the pipe and calls emit() at every block boundary.
type Progress struct {
	Frame       int       `json:"frame"`
	FPS         float64   `json:"fps"`
	BitrateKbps float64   `json:"bitrateKbps"`
	Speed       string    `json:"speed"`
	Dropped     int       `json:"dropped"`
	OutTimeUS   int64     `json:"outTimeUs"`
	UpdatedAt   time.Time `json:"updatedAt"`
	RawStatus   string    `json:"rawStatus"`
}

// ParseProgress reads `-progress pipe:1` output and calls emit once per
// `progress=...` terminator. It returns when the reader closes (FFmpeg exits)
// or scanner.Err if the scan fails. Safe to read a closed pipe.
func ParseProgress(r io.Reader, emit func(Progress)) error {
	scanner := bufio.NewScanner(r)
	current := Progress{}
	for scanner.Scan() {
		line := scanner.Text()
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "frame":
			current.Frame, _ = strconv.Atoi(value)
		case "fps":
			current.FPS, _ = strconv.ParseFloat(value, 64)
		case "bitrate":
			current.BitrateKbps = parseKbits(value)
		case "speed":
			current.Speed = value
		case "drop_frames":
			current.Dropped, _ = strconv.Atoi(value)
		case "out_time_us", "out_time_ms":
			// out_time_us is the modern key (microseconds); out_time_ms
			// is the legacy alias in some builds. Both report micro-
			// seconds. We don't try to be clever — last one wins.
			current.OutTimeUS, _ = strconv.ParseInt(value, 10, 64)
		case "progress":
			current.RawStatus = value
			current.UpdatedAt = time.Now().UTC()
			emit(current)
			if value == "end" {
				current = Progress{}
			}
		}
	}
	return scanner.Err()
}

func parseKbits(value string) float64 {
	trimmed := strings.TrimSpace(strings.TrimSuffix(value, "kbits/s"))
	trimmed = strings.TrimSpace(strings.TrimSuffix(trimmed, "kbit/s"))
	out, _ := strconv.ParseFloat(trimmed, 64)
	return out
}
