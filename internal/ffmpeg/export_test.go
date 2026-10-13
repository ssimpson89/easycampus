package ffmpeg

// Test-only hooks. Compiled into package ffmpeg during `go test`, so
// the external ffmpeg_test package can use them too.

// StubEncoderProbe replaces the trial-encode probe with fn (codec name
// -> usable) and clears every detection cache. Returns a restore func.
func StubEncoderProbe(fn func(codec string) bool) (restore func()) {
	detectMu.Lock()
	prev := probeEncoderFunc
	probeEncoderFunc = func(_ string, codec string) bool { return fn(codec) }
	detectMu.Unlock()
	resetDetectionCache()
	return func() {
		detectMu.Lock()
		probeEncoderFunc = prev
		detectMu.Unlock()
		resetDetectionCache()
	}
}
