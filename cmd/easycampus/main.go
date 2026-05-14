// Command easycampus is the daemon entry point. It parses flags +
// env, builds a slog logger, constructs an *app.Server, then runs it
// until SIGINT/SIGTERM lands. Shutdown sequencing is delegated to
// app.Server.Run.
//
// Layout mirrors easystream's cmd/easystream/main.go: keep the binary
// thin (flag parsing, signal handling, DI), keep the wiring in
// internal/app. This file should not grow beyond ~200 lines.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"mime"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"easycampus/internal/app"
	"easycampus/internal/version"
)

func main() {
	// Register HLS MIME types — Go's default detection may not know .m3u8.
	mime.AddExtensionType(".m3u8", "application/vnd.apple.mpegurl")
	mime.AddExtensionType(".m4s", "video/iso.segment")
	mime.AddExtensionType(".ts", "video/mp2t")

	setType := flag.String("set-type", "",
		"Set node type to 'encoder' or 'decoder' in the on-disk config and exit.")
	flag.Parse()

	logger := buildLogger()

	settings := app.LoadConfig(logger)

	// --set-type writes the chosen value to ~/.easycampus/config.yaml
	// and exits without starting the daemon. Matches the pre-refactor
	// behaviour the deployment scripts depend on.
	if *setType != "" {
		nodeType := strings.ToLower(*setType)
		if nodeType != "encoder" && nodeType != "decoder" {
			logger.Error("invalid --set-type value (expected encoder|decoder)",
				"received", *setType)
			os.Exit(2)
		}
		settings.NodeType = nodeType
		if err := app.SaveConfig(settings); err != nil {
			logger.Error("save config failed", "err", err)
			os.Exit(1)
		}
		logger.Info("config updated", "node_type", nodeType)
		return
	}

	if err := app.EnsureDirs(settings); err != nil {
		logger.Error("init dirs failed", "err", err)
		os.Exit(1)
	}

	server, err := app.NewServer(app.ServerConfig{
		Settings: settings,
		Logger:   logger,
	})
	if err != nil {
		logger.Error("server construction failed", "err", err)
		os.Exit(1)
	}

	// Trap SIGINT/SIGTERM. Cancelling the context unblocks the run
	// loop's signal channel, kicking off the graceful shutdown sequence.
	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	v := version.Current()
	logger.Info("easycampus starting",
		"addr", server.Addr(),
		"node_type", settings.NodeType,
		"version", v.Version,
		"commit", v.Commit,
		"date", v.Date)
	if err := server.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("server exited with error", "err", err)
		os.Exit(1)
	}
	logger.Info("easycampus stopped cleanly")
}

// buildLogger returns the application-wide slog.Logger. Output format
// defaults to JSON for systemd/journald consumers; LOG_FORMAT=text
// switches to the more readable TextHandler for `go run` workflows.
// Level comes from LOG_LEVEL (debug|info|warn|error), default info.
func buildLogger() *slog.Logger {
	level := parseLogLevel(os.Getenv("LOG_LEVEL"))
	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	switch strings.ToLower(os.Getenv("LOG_FORMAT")) {
	case "text", "console", "dev":
		handler = slog.NewTextHandler(os.Stderr, opts)
	default:
		handler = slog.NewJSONHandler(os.Stderr, opts)
	}
	logger := slog.New(handler).With(
		"app", "easycampus",
		"pid", os.Getpid(),
	)
	// Install as the package default so any third-party library that
	// calls slog.Info() lands in the same stream.
	slog.SetDefault(logger)
	return logger
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	case "", "info":
		return slog.LevelInfo
	}
	// Unknown value: return info but mention via stderr so the operator
	// notices on first boot. Once slog is initialized we'll never come
	// back through this path.
	fmt.Fprintf(os.Stderr, "warning: unknown LOG_LEVEL %q, defaulting to info\n", s)
	return slog.LevelInfo
}

