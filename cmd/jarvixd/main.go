// jarvixd is the Jarvix daemon: it owns session state, audio, speech-to-text,
// the AI provider, text-to-speech, and the IPC socket. Run it as a user
// service (systemctl --user enable --now jarvixd) or in the foreground for
// debugging.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/rpickz/jarvix/internal/build"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/daemon"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "jarvixd:", err)
		os.Exit(1)
	}
}

func run() error {
	paths := config.DefaultPaths()
	configPath := flag.String("config", paths.ConfigFile(), "path to config.toml")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("jarvixd", build.Version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	logger := newLogger(cfg.Log.Level)
	slog.SetDefault(logger)

	d, err := daemon.New(cfg, paths, logger, daemon.Deps{})
	if err != nil {
		return err
	}

	// SIGINT/SIGTERM shut down cleanly: the socket is removed, any active
	// session is cancelled, and Run does not return until the work the daemon
	// still owes — the conversation-history write above all — has finished or
	// the shutdown grace period has run out. Restarting the service therefore
	// cannot lose the last exchange.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("starting", "component", "daemon", "version", build.Version,
		"socket", paths.Socket, "provider", cfg.AI.Provider, "model", cfg.AI.Model)
	return d.Run(ctx)
}

// newLogger builds the structured logger. Output goes to stderr in text form,
// which journald captures cleanly for journalctl --user -u jarvixd.
func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}
