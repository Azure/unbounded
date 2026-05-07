// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
	"context"
	"flag"
	"math"
	"path/filepath"
	"strconv"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
	"k8s.io/klog/v2"
)

// WatchConfigLogLevel watches the runtime config file for changes and
// dynamically updates the klog verbosity when the common.logLevel field
// changes. Kubernetes ConfigMap volume mounts use symlink swaps, so we
// watch the parent directory for reliable notification. The function
// blocks until ctx is cancelled.
func WatchConfigLogLevel(ctx context.Context, configPath string) {
	dir := filepath.Dir(configPath)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		klog.Errorf("Failed to create config file watcher: %v", err)
		return
	}

	defer func() { _ = watcher.Close() }() //nolint:errcheck

	if err := watcher.Add(dir); err != nil {
		klog.Errorf("Failed to watch config directory %s: %v", dir, err)
		return
	}

	klog.V(2).Infof("Watching %s for log level changes", configPath)

	var currentLevel atomic.Int32

	if v := flag.Lookup("v"); v != nil {
		if n, err := strconv.Atoi(v.Value.String()); err == nil && n >= 0 && n <= math.MaxInt32 {
			currentLevel.Store(int32(n))
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			// Kubernetes ConfigMap mounts fire Create on the ..data symlink
			// when the content is updated. Also handle Write for direct edits.
			if event.Op&(fsnotify.Create|fsnotify.Write) == 0 {
				continue
			}

			applyLogLevelFromConfig(configPath, &currentLevel)
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}

			klog.V(4).Infof("Config watcher error: %v", err)
		}
	}
}

// applyLogLevelFromConfig reads the config file and updates klog verbosity
// if the log level has changed.
func applyLogLevelFromConfig(configPath string, currentLevel *atomic.Int32) {
	cfg, err := LoadRuntimeConfig(configPath)
	if err != nil {
		klog.V(4).Infof("Failed to reload config for log level: %v", err)
		return
	}

	if cfg.Common.LogLevel == nil {
		return
	}

	newLevel := *cfg.Common.LogLevel
	if newLevel < 0 || newLevel > math.MaxInt32 {
		return
	}

	if int32(newLevel) == currentLevel.Load() {
		return
	}

	v := flag.Lookup("v")
	if v == nil {
		return
	}

	levelStr := strconv.Itoa(newLevel)
	if err := v.Value.Set(levelStr); err != nil {
		klog.Errorf("Failed to set log level to %d: %v", newLevel, err)
		return
	}

	klog.Infof("Log level changed from %d to %d", currentLevel.Load(), newLevel)
	currentLevel.Store(int32(newLevel))
}
