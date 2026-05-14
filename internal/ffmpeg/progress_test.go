package ffmpeg

import (
	"strings"
	"testing"
)

func TestParseProgressEmitsOnEachBlock(t *testing.T) {
	input := strings.NewReader(`frame=120
fps=30.00
bitrate=4000.0kbits/s
drop_frames=2
speed=1.00x
out_time_us=4000000
progress=continue
frame=240
fps=30.00
bitrate=4000.0kbits/s
drop_frames=5
speed=1.00x
out_time_us=8000000
progress=end
`)
	var samples []Progress
	err := ParseProgress(input, func(p Progress) {
		samples = append(samples, p)
	})
	if err != nil {
		t.Fatalf("ParseProgress: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("want 2 emit calls, got %d", len(samples))
	}
	if samples[0].Frame != 120 || samples[0].Dropped != 2 {
		t.Errorf("first sample: %+v", samples[0])
	}
	if samples[0].BitrateKbps != 4000.0 {
		t.Errorf("bitrate kbps: got %v", samples[0].BitrateKbps)
	}
	if samples[0].RawStatus != "continue" {
		t.Errorf("raw status: %q", samples[0].RawStatus)
	}
	if samples[1].RawStatus != "end" || samples[1].Frame != 240 {
		t.Errorf("end sample: %+v", samples[1])
	}
	if samples[1].OutTimeUS != 8000000 {
		t.Errorf("out_time_us: %d", samples[1].OutTimeUS)
	}
}

func TestParseProgressHandlesPartial(t *testing.T) {
	// No `progress=` terminator: emit should never be called.
	input := strings.NewReader("frame=10\nfps=30.0\n")
	called := false
	_ = ParseProgress(input, func(Progress) { called = true })
	if called {
		t.Error("emit should not be called without progress= terminator")
	}
}
