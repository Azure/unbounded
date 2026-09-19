// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package installstate

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/unbounded/internal/fsutil"
)

const (
	DefaultDirectory = "/var/lib/unbounded/agent"
	defaultLockPath  = "/run/unbounded-agent-install.lock"
	schemaVersion    = 1
)

// Phase is what an installation is currently doing, not how far it has got.
//
// It deliberately records no progress. A record that claimed a stage was
// finished would be a statement about the host that could stop being true
// without anyone noticing, and a retry that trusted it would skip work the host
// no longer has. The stages instead decide what to do by looking at the host,
// so the only thing worth persisting is which mode we are in.
type Phase string

const (
	// Installing means an installation is under way. It says nothing about what
	// has been done, so there is nothing in it that can go stale.
	Installing Phase = "installing"
	// Complete means the installation finished. A later start verifies and
	// repairs from the applied config rather than reapplying bootstrap inputs,
	// which after an ordinary repave describe a retired slot.
	Complete Phase = "complete"
	// Resetting means a teardown started and may not have finished. This is the
	// one thing the host cannot be asked: a half-removed installation and a
	// half-built one look the same, because direction of travel is not visible.
	Resetting Phase = "resetting"
)

type Record struct {
	SchemaVersion     int    `json:"schemaVersion"`
	InstallID         string `json:"installID"`
	MachineName       string `json:"machineName"`
	ConfigFingerprint string `json:"configFingerprint"`
	Phase             Phase  `json:"phase"`

	// HostPrefix is the resolved installation prefix, recorded so teardown can
	// find the agent's own files without being told where they are.
	//
	// It is written before the first host mutation, which makes it the only
	// source that survives a bootstrap that failed before the node started. The
	// applied config carries the same prefix but does not exist until then, so
	// reset on a half-built host has nothing else to go on.
	//
	// Optional, and absent means the default. The schema version does not move
	// for it: a record written by an agent that knows about the prefix stays
	// readable by one that does not, because unknown fields are ignored, and a
	// record written before it existed is read here as the default, which is
	// what such a host actually has on disk.
	HostPrefix string `json:"hostPrefix,omitempty"`
}

func (r Record) Validate() error {
	if r.SchemaVersion != schemaVersion || strings.TrimSpace(r.InstallID) == "" || strings.TrimSpace(r.MachineName) == "" || strings.TrimSpace(r.ConfigFingerprint) == "" {
		return fmt.Errorf("invalid installation record identity or schema")
	}

	switch r.Phase {
	case Installing, Complete, Resetting:
		return nil
	default:
		return fmt.Errorf("unknown installation phase %q", r.Phase)
	}
}

var ErrNotFound = errors.New("installation record not found")

type Store struct {
	root, lockPath string
	// syncDir is a seam for exercising the window where the record is unlinked
	// but the directory entry has not reached disk.
	syncDir func(string) error
}

func NewStore(root, lockPath string) *Store {
	return &Store{root: root, lockPath: lockPath, syncDir: fsutil.SyncDir}
}

func DefaultStore() *Store                   { return NewStore(DefaultDirectory, defaultLockPath) }
func (s *Store) Root() string                { return s.root }
func (s *Store) statePath() string           { return filepath.Join(s.root, "install-state.json") }
func (s *Store) AcquireLock() (*Lock, error) { return acquireLockAt(s.lockPath) }

func (s *Store) Load() (Record, error) {
	var r Record

	data, err := os.ReadFile(s.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return r, ErrNotFound
	}

	if err != nil {
		return r, err
	}

	if err := json.Unmarshal(data, &r); err != nil {
		return r, err
	}

	return r, r.Validate()
}

func (s *Store) Save(r Record) error {
	if err := r.Validate(); err != nil {
		return err
	}

	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}

	return fsutil.WriteFileDurable(s.statePath(), append(data, '\n'), 0o600)
}

// MarkComplete commits completion. The durable record is the only completion
// signal; no separate marker file is maintained.
func (s *Store) MarkComplete(r Record) error {
	r.Phase = Complete
	return s.Save(r)
}

// Remove is called only after teardown's filesystem barriers succeed.
//
// Dropping ownership is itself a durable step. If the unlink cannot be flushed,
// the record is put back, because a reset that reports failure must leave the
// host visibly owned. Otherwise the removal survives in page cache only, the
// error sends the operator away, and the next start is admitted as a fresh
// install onto a half-torn-down host.
func (s *Store) Remove() error {
	if _, err := os.Stat(s.root); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}

	// Read what is about to be dropped so ownership can be restored below. A
	// record that does not load is not restored: it granted no usable ownership
	// and admission rejects it either way.
	previous, loadErr := s.Load()

	if err := os.Remove(s.statePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	err := s.syncDir(s.root)
	if err != nil && loadErr == nil {
		if saveErr := s.Save(previous); saveErr != nil {
			return errors.Join(err, saveErr)
		}
	}

	return err
}

// NewRecord returns a record for a fresh installation.
//
// hostPrefix is a parameter rather than a field callers set afterwards because
// forgetting it is silent and only surfaces at teardown, on a host whose files
// are somewhere reset would not look. An empty prefix means the default.
//
// The value is stored as given and not validated here. This package deals in
// stdlib and durability only, and pulling in config validation to re-check a
// string this agent wrote from an already validated config would buy little.
func NewRecord(machine, fingerprint, hostPrefix string) (Record, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return Record{}, err
	}

	return Record{
		SchemaVersion: schemaVersion, InstallID: hex.EncodeToString(id), MachineName: machine,
		ConfigFingerprint: fingerprint, Phase: Installing, HostPrefix: hostPrefix,
	}, nil
}

// Fingerprint hashes canonical JSON supplied before ephemeral credentials are
// resolved.
//
// The caller decides what canonical means, and the hash is over exactly the
// bytes it is given. A release that adds a field to the fingerprinted struct
// changes the hash of every host that did not have it, and each of those reads
// as a different installation demanding an explicit reset. An optional field
// therefore has to carry omitempty and be absent at its default, so records
// written before it existed keep hashing the same way.
//
// TestBootstrapV1CompatibilityFixtures enforces this: its fixtures carry a
// literal fingerprint, so any change to what is hashed fails there rather than
// on upgraded hosts.
func Fingerprint(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

type Disposition int

const (
	Fresh Disposition = iota
	Resume
	AlreadyComplete
)

func decide(r Record, loadErr error, machine, fingerprint string) (Disposition, error) {
	if strings.TrimSpace(machine) == "" || strings.TrimSpace(fingerprint) == "" {
		return Fresh, fmt.Errorf("bootstrap identity is required")
	}

	if errors.Is(loadErr, ErrNotFound) {
		return Fresh, nil
	}

	if loadErr != nil {
		return Fresh, fmt.Errorf("cannot read installation ownership: %w", loadErr)
	}

	if err := r.Validate(); err != nil {
		return Fresh, err
	}

	if r.Phase == Resetting {
		return Fresh, fmt.Errorf("reset is incomplete; run unbounded-agent reset again")
	}

	if r.MachineName != machine || r.ConfigFingerprint != fingerprint {
		return Fresh, fmt.Errorf("installation intent differs; explicit reset is required")
	}

	if r.Phase == Complete {
		return AlreadyComplete, nil
	}

	return Resume, nil
}

// Admit reads ownership and classifies a bootstrap attempt. Callers that mutate
// the host must hold the installation lock around this call.
func Admit(store *Store, machine, fingerprint string) (Record, Disposition, error) {
	r, loadErr := store.Load()

	disposition, err := decide(r, loadErr, machine, fingerprint)
	if err != nil {
		return Record{}, Fresh, err
	}

	return r, disposition, nil
}
