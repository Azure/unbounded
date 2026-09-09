// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package installstate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
)

// Checkpoint names the stage of bootstrap that still has to run.
//
// A checkpoint is recorded before entering the stage it names, so a recorded
// checkpoint means "this stage may have run partially". Recovery therefore
// re-enters the recorded stage rather than the one after it, and every stage
// has to tolerate being repeated.
type Checkpoint string

const (
	// CheckpointPreparingHost is the first stage: package and OS level
	// preparation, before anything about a node exists.
	CheckpointPreparingHost Checkpoint = "preparing-host"

	// CheckpointPreparingRootFS covers building the machine rootfs and
	// downloading the binaries that go into it.
	CheckpointPreparingRootFS Checkpoint = "preparing-rootfs"

	// CheckpointStartingNode covers starting the machine and waiting for the
	// kubelet. Once this is recorded, recovery must never rebuild the rootfs or
	// re-run host preparation: a node may be running.
	CheckpointStartingNode Checkpoint = "starting-node"

	// CheckpointInstallingDaemon covers persisting the applied config and
	// installing, enabling and starting the daemon.
	CheckpointInstallingDaemon Checkpoint = "installing-daemon"

	// CheckpointComplete means bootstrap finished.
	CheckpointComplete Checkpoint = "complete"

	// CheckpointResetting means teardown began. Bootstrap must refuse.
	CheckpointResetting Checkpoint = "resetting"
)

// validCheckpoints is the set a record may carry. An unrecognized value is
// refused rather than guessed at, because guessing decides whether a running
// node gets rebuilt.
var validCheckpoints = map[Checkpoint]struct{}{
	CheckpointPreparingHost:    {},
	CheckpointPreparingRootFS:  {},
	CheckpointStartingNode:     {},
	CheckpointInstallingDaemon: {},
	CheckpointComplete:         {},
	CheckpointResetting:        {},
}

// NodeMayBeRunning reports whether a node may have been started by the time
// this checkpoint was recorded.
//
// This is the safety question recovery asks before touching the rootfs or the
// host firewall. It is deliberately conservative: it answers "may", not "is".
func (c Checkpoint) NodeMayBeRunning() bool {
	switch c {
	case CheckpointStartingNode, CheckpointInstallingDaemon, CheckpointComplete:
		return true
	default:
		return false
	}
}

// Store reads and writes installation state under a root directory.
//
// A value rather than package functions so that a test can point one at a
// temporary directory and exercise the real file behavior, including partial
// writes and interrupted transitions. The recovery decisions this state drives
// are worth testing against real files rather than a mock.
type Store struct {
	root string
}

// NewStore returns a Store rooted at dir.
func NewStore(dir string) *Store {
	return &Store{root: dir}
}

// DefaultStore returns a Store at the standard host location.
func DefaultStore() *Store { return NewStore(Dir) }

// Root returns the directory the store writes to.
func (s *Store) Root() string { return s.root }

// StatePath returns the path of the installation record.
func (s *Store) StatePath() string { return filepath.Join(s.root, stateFileName) }

// CompletePath returns the path of the completion marker.
func (s *Store) CompletePath() string { return filepath.Join(s.root, completeFileName) }

// Load returns the installation record, or ErrNotFound when there is none.
//
// A record that cannot be read or does not describe an installation is an
// error rather than an absent record: it may name files that are still on this
// host, and reading it as absent would let bootstrap run over them.
func (s *Store) Load() (Record, error) {
	data, err := os.ReadFile(s.StatePath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Record{}, ErrNotFound
		}

		return Record{}, fmt.Errorf("read %s: %w", s.StatePath(), err)
	}

	rec, err := decodeRecord(data)
	if err != nil {
		return Record{}, fmt.Errorf("%s: %w", s.StatePath(), err)
	}

	return rec, nil
}

// Save writes the record atomically and durably.
func (s *Store) Save(rec Record) error {
	rec.SchemaVersion = SchemaVersion
	rec.UpdatedAt = time.Now().UTC()

	data, err := encodeRecord(rec)
	if err != nil {
		return err
	}

	// Durable rather than merely atomic: this record is what tells the next
	// boot whether a half-built host is ours to finish, so a rename that has
	// not reached disk is exactly the case it exists to survive.
	if err := utilio.WriteFileDurable(s.StatePath(), data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", s.StatePath(), err)
	}

	return nil
}

// Advance records that bootstrap is moving to the next checkpoint.
//
// Returns the updated record so callers cannot carry on with a stale copy,
// which is how a later write would silently roll the checkpoint back.
func (s *Store) Advance(rec Record, next Checkpoint) (Record, error) {
	if _, ok := validCheckpoints[next]; !ok {
		return Record{}, fmt.Errorf("refusing to record unknown checkpoint %q", next)
	}

	// Going backwards would re-enter a stage that has already been passed, and
	// after StartingNode that means rebuilding under a running node.
	if rec.Checkpoint.NodeMayBeRunning() && !next.NodeMayBeRunning() {
		return Record{}, fmt.Errorf(
			"refusing to move installation from %q back to %q: a node may already be running",
			rec.Checkpoint, next,
		)
	}

	rec.Checkpoint = next
	if err := s.Save(rec); err != nil {
		return Record{}, err
	}

	return rec, nil
}

// MarkComplete records completion and writes the durable marker.
//
// The record is written first so that a crash between the two leaves the
// install looking unfinished rather than finished: finishing an install that
// was already complete is recoverable, skipping one that was not is not.
func (s *Store) MarkComplete(rec Record) (Record, error) {
	updated, err := s.Advance(rec, CheckpointComplete)
	if err != nil {
		return Record{}, err
	}

	if err := utilio.WriteFileDurable(s.CompletePath(), []byte(updated.InstallID+"\n"), 0o644); err != nil {
		return Record{}, fmt.Errorf("write %s: %w", s.CompletePath(), err)
	}

	return updated, nil
}

// CompletionMatches reports whether the completion marker was written by this
// installation.
//
// A marker naming a different install is left over from a host that was torn
// down incompletely, and must not vouch for the current one.
func (s *Store) CompletionMatches(rec Record) (bool, error) {
	data, err := os.ReadFile(s.CompletePath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}

		return false, fmt.Errorf("read %s: %w", s.CompletePath(), err)
	}

	// Markers written before the ID was recorded are empty; those can only be
	// judged by the record beside them, which the caller has already matched.
	recorded := strings.TrimSpace(string(data))
	if recorded == "" {
		return true, nil
	}

	return recorded == rec.InstallID, nil
}

// Remove deletes the completion marker and the record.
//
// The marker goes first: if only one survives an interrupted teardown it must
// be the record, because that is what the next teardown needs in order to know
// what it was still cleaning up.
func (s *Store) Remove() error {
	for _, path := range []string{s.CompletePath(), s.StatePath()} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}

	return utilio.SyncDir(s.root)
}
