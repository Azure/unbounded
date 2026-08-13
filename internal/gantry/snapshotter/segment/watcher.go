// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package segment

import (
	"context"
	"errors"
	"io/fs"
	"sync/atomic"
	"time"
)

// Watcher holds the current published mapping and refreshes it in the
// background.
//
// The mapping changes only when the operator adds or retires a segment, which
// is rare, so this polls rather than taking an inotify dependency. The read
// path never blocks on it: Current returns whatever was last successfully
// loaded, and a nil result means "the image volume is not usable here yet",
// which the snapshotter treats as a catalog miss and falls back to local
// unpack.
type Watcher struct {
	path     string
	interval time.Duration
	onChange func(*Set)

	current atomic.Pointer[Set]
	lastErr atomic.Pointer[error]
}

// WatcherOptions configures a Watcher.
type WatcherOptions struct {
	// Path to the published mapping. Empty means DefaultPath.
	Path string

	// Interval between reloads. Zero means DefaultWatchInterval.
	Interval time.Duration

	// OnChange, if set, is called whenever the generation advances. It runs
	// on the watcher's goroutine, so it must not block for long.
	OnChange func(*Set)
}

// DefaultWatchInterval is how often the mapping is reloaded when the caller
// does not say otherwise.
const DefaultWatchInterval = 5 * time.Second

// NewWatcher builds a Watcher. It does not perform any I/O; call Refresh or
// Run to load the mapping.
func NewWatcher(opts WatcherOptions) *Watcher {
	w := &Watcher{
		path:     opts.Path,
		interval: opts.Interval,
		onChange: opts.OnChange,
	}

	if w.path == "" {
		w.path = DefaultPath
	}

	if w.interval <= 0 {
		w.interval = DefaultWatchInterval
	}

	return w
}

// Current returns the last successfully loaded mapping, or nil if none has
// loaded yet.
func (w *Watcher) Current() *Set { return w.current.Load() }

// Err returns the error from the most recent load attempt, or nil if it
// succeeded. A missing file is reported as fs.ErrNotExist and is the normal
// state on a node where the operator has not yet placed an image device.
func (w *Watcher) Err() error {
	if e := w.lastErr.Load(); e != nil {
		return *e
	}

	return nil
}

// Refresh loads the mapping once.
//
// A generation that does not advance is not an error and does not re-publish:
// racer-ctrl rewrites the file by rename, so a reader can see the same content
// repeatedly, and re-publishing would make every consumer redo work for
// nothing. A generation that goes backwards is refused, since that is a stale
// file and adopting it would make blob addresses resolve to the wrong devices.
func (w *Watcher) Refresh() error {
	set, err := Load(w.path)
	if err != nil {
		w.lastErr.Store(&err)

		return err
	}

	var noErr error

	w.lastErr.Store(&noErr)

	previous := w.current.Load()
	if previous != nil && set.Generation <= previous.Generation {
		return nil
	}

	w.current.Store(set)

	if w.onChange != nil {
		w.onChange(set)
	}

	return nil
}

// Run refreshes until the context is cancelled. Load failures are recorded on
// the watcher and do not stop it: the mapping file legitimately does not exist
// until the operator places an image device on this node.
func (w *Watcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	_ = w.Refresh() //nolint:errcheck

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = w.Refresh() //nolint:errcheck
		}
	}
}

// Missing reports whether err means the mapping file is simply not there,
// which is the expected state on a node with no image device rather than a
// fault worth alerting on.
func Missing(err error) bool { return errors.Is(err, fs.ErrNotExist) }
