// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package storagesupervisor

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// renderDebounce is the quiet window the run loop waits for after a source
// change before re-rendering, so a burst of ConfigMap projection events
// (Kubernetes rewrites every key file and swaps the `..data` symlink) folds
// into a single render.
const renderDebounce = 200 * time.Millisecond

// Run is the runtime-container entrypoint. It renders the projected ConfigMap
// under cfg.SourceDir into the daemon's binary protobuf config at
// cfg.ConfigPath, then watches the source directory and re-renders on change.
// The daemon owns reacting to the rewritten file (it has its own watcher and
// manages its own restarts), so this loop is render-only. It blocks until ctx
// is cancelled.
func Run(ctx context.Context, cfg Config) error {
	slog.Info("rendering initial config", "source", cfg.SourceDir, "dest", cfg.ConfigPath)

	if err := reconcile(cfg); err != nil {
		return fmt.Errorf("initial config render: %w", err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create config source watcher: %w", err)
	}

	defer func() { _ = watcher.Close() }() //nolint:errcheck

	// Watch the source directory rather than the individual key files:
	// Kubernetes projects a ConfigMap by writing a timestamped directory and
	// atomically swapping a `..data` symlink, so per-file watches miss updates.
	if err := watcher.Add(cfg.SourceDir); err != nil {
		return fmt.Errorf("watch config source %q: %w", cfg.SourceDir, err)
	}

	slog.Info("watching config source for changes", "source", cfg.SourceDir)

	return watchLoop(ctx, cfg, watcher)
}

// watchLoop drives the debounced render loop until ctx is cancelled. It is
// split out from Run so the watcher wiring stays separate from the event
// handling.
func watchLoop(ctx context.Context, cfg Config, watcher *fsnotify.Watcher) error {
	// A stopped timer with a drained channel; Reset arms the debounce on the
	// first event of a burst.
	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}

	defer timer.Stop()

	pending := false

	for {
		select {
		case <-ctx.Done():
			slog.Info("shutting down config render loop")

			return nil
		case event, ok := <-watcher.Events:
			if !ok {
				return fmt.Errorf("config source watcher closed unexpectedly")
			}

			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}

			if !pending {
				pending = true

				timer.Reset(renderDebounce)
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return fmt.Errorf("config source watcher error channel closed")
			}

			slog.Warn("config source watcher error", "error", err)
		case <-timer.C:
			pending = false

			if err := reconcile(cfg); err != nil {
				// Keep the previously rendered config in place; an operator
				// typo should not blow away a working config.
				slog.Error("config re-render failed; keeping previous config", "error", err)

				continue
			}

			slog.Info("re-rendered config", "dest", cfg.ConfigPath)
		}
	}
}

// reconcile renders the current ConfigMap source into the daemon config file.
func reconcile(cfg Config) error {
	data, err := RenderConfig(cfg.SourceDir)
	if err != nil {
		return err
	}

	if err := WriteConfigAtomic(cfg.ConfigPath, data); err != nil {
		return fmt.Errorf("write config %q: %w", filepath.Clean(cfg.ConfigPath), err)
	}

	return nil
}
