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

// nodeSyncTimeout bounds the initial node-informer sync so a missing RBAC
// grant fails fast instead of blocking the supervisor forever.
const nodeSyncTimeout = 30 * time.Second

// Run is the runtime-container entrypoint. It renders the projected ConfigMap
// under cfg.SourceDir plus the per-node peer set (discovered from the
// Kubernetes node watch) into the daemon's binary protobuf config at
// cfg.ConfigPath, then watches both the source directory and cluster nodes and
// re-renders on change. The daemon owns reacting to the rewritten file (it has
// its own watcher and manages its own restarts), so this loop is render-only.
// It blocks until ctx is cancelled.
func Run(ctx context.Context, cfg Config) error {
	watcher, err := newPeerWatcher(cfg, nil)
	if err != nil {
		return fmt.Errorf("init peer watcher: %w", err)
	}

	if watcher != nil {
		defer watcher.Stop()

		syncCtx, cancel := context.WithTimeout(ctx, nodeSyncTimeout)

		err := watcher.Start(syncCtx)

		cancel()

		if err != nil {
			return fmt.Errorf("start peer watcher: %w", err)
		}

		slog.Info("watching nodes for storage ring peers", "node", cfg.NodeName, "label", cfg.StorageRingLabel)
	}

	slog.Info("rendering initial config", "source", cfg.SourceDir, "dest", cfg.ConfigPath)

	if err := reconcile(cfg, watcher); err != nil {
		return fmt.Errorf("initial config render: %w", err)
	}

	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create config source watcher: %w", err)
	}

	defer func() { _ = fsWatcher.Close() }() //nolint:errcheck

	// Watch the source directory rather than the individual key files:
	// Kubernetes projects a ConfigMap by writing a timestamped directory and
	// atomically swapping a `..data` symlink, so per-file watches miss updates.
	if err := fsWatcher.Add(cfg.SourceDir); err != nil {
		return fmt.Errorf("watch config source %q: %w", cfg.SourceDir, err)
	}

	slog.Info("watching config source for changes", "source", cfg.SourceDir)

	return watchLoop(ctx, cfg, fsWatcher, watcher)
}

// watchLoop drives the debounced render loop until ctx is cancelled. It folds
// two change sources, ConfigMap projection events (fsWatcher) and node
// membership events (peerWatcher), into a single debounced reconcile. It is
// split out from Run so the watcher wiring stays separate from the event
// handling.
func watchLoop(ctx context.Context, cfg Config, fsWatcher *fsnotify.Watcher, peers *peerWatcher) error {
	// A stopped timer with a drained channel; Reset arms the debounce on the
	// first event of a burst.
	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}

	defer timer.Stop()

	// A nil channel blocks forever in select, so peer-less runs simply never
	// take the node-event branch.
	var nodeEvents <-chan struct{}
	if peers != nil {
		nodeEvents = peers.Events()
	}

	pending := false

	arm := func() {
		if !pending {
			pending = true

			timer.Reset(renderDebounce)
		}
	}

	for {
		select {
		case <-ctx.Done():
			slog.Info("shutting down config render loop")

			return nil
		case event, ok := <-fsWatcher.Events:
			if !ok {
				return fmt.Errorf("config source watcher closed unexpectedly")
			}

			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}

			arm()
		case _, ok := <-nodeEvents:
			if !ok {
				return fmt.Errorf("node watcher event channel closed unexpectedly")
			}

			arm()
		case err, ok := <-fsWatcher.Errors:
			if !ok {
				return fmt.Errorf("config source watcher error channel closed")
			}

			slog.Warn("config source watcher error", "error", err)
		case <-timer.C:
			pending = false

			if err := reconcile(cfg, peers); err != nil {
				// Keep the previously rendered config in place; an operator
				// typo should not blow away a working config.
				slog.Error("config re-render failed; keeping previous config", "error", err)

				continue
			}

			slog.Info("re-rendered config", "dest", cfg.ConfigPath)
		}
	}
}

// reconcile renders the current ConfigMap source plus the latest node-derived
// overlays into the daemon config file. peers may be nil (node discovery
// disabled), in which case only the source config is rendered.
func reconcile(cfg Config, peers *peerWatcher) error {
	snapshot := snapshotNodes(cfg, peers)

	data, err := RenderConfigWithBenchmarks(cfg.SourceDir, snapshot.ring, snapshot.benchmarks)
	if err != nil {
		return err
	}

	if err := WriteConfigAtomic(cfg.ConfigPath, data); err != nil {
		return fmt.Errorf("write config %q: %w", filepath.Clean(cfg.ConfigPath), err)
	}

	return nil
}

// snapshotNodes resolves current node-derived overlays from the node watch,
// reading the shared TCP fabric port from the ConfigMap. It returns zero-value
// overlays when peer discovery is disabled. A source that cannot be loaded
// yields an inactive ring; the subsequent render surfaces the same error in
// reconcile, which keeps the previously rendered config in place.
func snapshotNodes(cfg Config, peers *peerWatcher) nodeSnapshot {
	if peers == nil {
		return nodeSnapshot{}
	}

	sc, err := loadSourceConfig(cfg.SourceDir)
	if err != nil {
		return peers.snapshot(0, false)
	}

	port, ok := parseFabricPort(sc.GetStartup().GetFabric().GetTcp().GetAddr())

	return peers.snapshot(port, ok)
}
