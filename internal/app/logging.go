package app

import (
	"io"
	"log/slog"
	"strings"
)

// slogWriter returns an io.Writer that splits its input on newlines and
// forwards each line to the supplied slog.Logger at info level. Used to
// bridge legacy *log.Logger consumers (notably internal/ffmpeg.Supervisor
// and Job) into the structured journal without rewriting their argv-
// surface APIs.
//
// The writer is unbuffered between writes: a single Write that contains
// multiple newlines emits multiple records; a Write without a trailing
// newline emits one record with whatever was given (matches stdlib
// log.Logger semantics, which always appends "\n" before calling Write).
func slogWriter(logger *slog.Logger) io.Writer {
	return &slogWriterImpl{logger: logger}
}

type slogWriterImpl struct {
	logger *slog.Logger
}

func (w *slogWriterImpl) Write(p []byte) (int, error) {
	s := strings.TrimRight(string(p), "\n")
	if s == "" {
		return len(p), nil
	}
	// Split multi-line writes into one record per line. Stdlib
	// log.Logger writes "<prefix><line>\n" as a single Write so this
	// loop is essentially a one-shot in practice, but a callee that
	// writes "a\nb" gets two records as expected.
	for _, line := range strings.Split(s, "\n") {
		if line != "" {
			w.logger.Info(line)
		}
	}
	return len(p), nil
}
