// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/Azure/unbounded/internal/version"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, err)
		}

		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "version", "-version", "--version":
			fmt.Fprintf(stdout, "gantry-snapshotter %s %s/%s (go %s, commit %s, built %s)\n", //nolint:errcheck
				version.Version, runtime.GOOS, runtime.GOARCH, runtime.Version(),
				version.GitCommit, version.BuildTime)

			return nil
		}
	}

	cfg, err := parseConfig(args, stderr)
	if err != nil {
		return err
	}

	log, err := newLogger(cfg, stderr)
	if err != nil {
		return err
	}

	// Signals are wired before anything is built so a daemon that is stuck
	// waiting for containerd or for its image devices still stops on the
	// first SIGTERM rather than needing to be killed.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("starting",
		slog.String("version", version.Version),
		slog.String("socket", cfg.Socket),
		slog.String("root", cfg.Root),
		slog.String("devices", cfg.Devices),
	)

	if err := serve(ctx, cfg, log); err != nil {
		return fmt.Errorf("gantry-snapshotter: %w", err)
	}

	log.Info("stopped")

	return nil
}

// newLogger builds the process logger. The handler writes to stderr because
// the daemon's stdout is reserved for the version subcommand.
func newLogger(cfg *Config, stderr io.Writer) (*slog.Logger, error) {
	level, err := parseLevel(cfg.LogLevel)
	if err != nil {
		return nil, err
	}

	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	if cfg.LogFormat == "json" {
		handler = slog.NewJSONHandler(stderr, opts)
	} else {
		handler = slog.NewTextHandler(stderr, opts)
	}

	return slog.New(handler).With(slog.String("component", "gantry-snapshotter")), nil
}

// parseLevel maps the configured level name onto a slog level. The comparison
// is case insensitive because "INFO" in a DaemonSet environment variable is a
// typo, not a configuration error worth refusing to start over.
func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}

	return 0, fmt.Errorf("log-level %q: want debug, info, warn or error", s)
}
