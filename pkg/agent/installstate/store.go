// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package installstate records ownership before initial bootstrap mutates a host.
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

	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
)

const (
	DefaultDirectory  = "/var/lib/unbounded/agent"
	DefaultLockPath   = "/run/unbounded-agent-install.lock"
	DefaultHostPrefix = "/usr/local"
	SchemaVersion     = 1
)

type Checkpoint string

const (
	PreparingHost    Checkpoint = "preparing-host"
	PreparingRootFS  Checkpoint = "preparing-rootfs"
	StartingNode     Checkpoint = "starting-node"
	InstallingDaemon Checkpoint = "installing-daemon"
	RepairingDaemon  Checkpoint = "repairing-daemon"
	Complete         Checkpoint = "complete"
	Resetting        Checkpoint = "resetting"
)

func (c Checkpoint) NodeMayBeRunning() bool {
	return c == StartingNode || c == InstallingDaemon || c == RepairingDaemon || c == Complete
}

type Record struct {
	SchemaVersion int    `json:"schemaVersion"`
	InstallID     string `json:"installID"`
	MachineName   string `json:"machineName"`
	// Store the resolved default now, so later configurable-prefix support can
	// consume records from this release without changing their meaning.
	HostPrefix        string     `json:"hostPrefix"`
	ConfigFingerprint string     `json:"configFingerprint"`
	Checkpoint        Checkpoint `json:"checkpoint"`
}

func (r Record) Validate() error {
	if r.SchemaVersion != SchemaVersion || strings.TrimSpace(r.InstallID) == "" || strings.TrimSpace(r.MachineName) == "" || strings.TrimSpace(r.ConfigFingerprint) == "" {
		return fmt.Errorf("invalid installation record identity or schema")
	}
	// This release installs only at the default prefix. Unknown ownership must
	// not authorize cleanup at a guessed location.
	if r.HostPrefix != DefaultHostPrefix {
		return fmt.Errorf("unsupported recorded host prefix %q", r.HostPrefix)
	}

	switch r.Checkpoint {
	case PreparingHost, PreparingRootFS, StartingNode, InstallingDaemon, RepairingDaemon, Complete, Resetting:
		return nil
	default:
		return fmt.Errorf("unknown installation checkpoint %q", r.Checkpoint)
	}
}

var ErrNotFound = errors.New("installation record not found")

type Store struct{ root, lockPath string }

func NewStore(root, lockPath string) *Store  { return &Store{root: root, lockPath: lockPath} }
func DefaultStore() *Store                   { return NewStore(DefaultDirectory, DefaultLockPath) }
func (s *Store) Root() string                { return s.root }
func (s *Store) StatePath() string           { return filepath.Join(s.root, "install-state.json") }
func (s *Store) CompletePath() string        { return filepath.Join(s.root, "bootstrap-complete") }
func (s *Store) AcquireLock() (*Lock, error) { return AcquireLockAt(s.lockPath) }

func (s *Store) Load() (Record, error) {
	var r Record

	data, err := os.ReadFile(s.StatePath())
	if errors.Is(err, os.ErrNotExist) {
		if _, markerErr := os.Lstat(s.CompletePath()); markerErr == nil {
			return r, fmt.Errorf("completion marker exists without installation ownership")
		} else if !errors.Is(markerErr, os.ErrNotExist) {
			return r, markerErr
		}

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

	return utilio.WriteFileDurable(s.StatePath(), append(data, '\n'), 0o600)
}

func (s *Store) CheckMarker(r Record) (bool, error) {
	data, err := os.ReadFile(s.CompletePath())
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	if err != nil {
		return false, err
	}

	if strings.TrimSpace(string(data)) != r.InstallID {
		return false, fmt.Errorf("completion marker conflicts with installation identity")
	}

	return true, nil
}

func (s *Store) MarkComplete(r Record) error {
	r.Checkpoint = Complete
	if err := s.Save(r); err != nil {
		return err
	}

	return utilio.WriteFileDurable(s.CompletePath(), []byte(r.InstallID+"\n"), 0o644)
}

// Remove is called only after teardown's filesystem barriers succeed.
func (s *Store) Remove() error {
	if _, err := os.Stat(s.root); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}

	for _, path := range []string{s.CompletePath(), s.StatePath()} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		// Persist marker removal before deleting ownership. After interruption,
		// reset can resume from the record rather than encounter an orphan marker.
		if err := utilio.SyncDir(s.root); err != nil {
			return err
		}
	}

	return nil
}

func NewRecord(machine, fingerprint string) (Record, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return Record{}, err
	}

	return Record{
		SchemaVersion: SchemaVersion, InstallID: hex.EncodeToString(id), MachineName: machine,
		HostPrefix: DefaultHostPrefix, ConfigFingerprint: fingerprint, Checkpoint: PreparingHost,
	}, nil
}

// Fingerprint hashes canonical JSON supplied before ephemeral credentials are
// resolved. Omitted optional fields stay omitted across compatible releases.
func Fingerprint(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

type Disposition int

const (
	Fresh Disposition = iota
	Resume
	AlreadyComplete
)

func Decide(r Record, loadErr error, machine, fingerprint string) (Disposition, error) {
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

	if r.Checkpoint == Resetting {
		return Fresh, fmt.Errorf("reset is incomplete; run unbounded-agent reset again")
	}

	if r.MachineName != machine || r.ConfigFingerprint != fingerprint {
		return Fresh, fmt.Errorf("installation intent differs; explicit reset is required")
	}

	if r.Checkpoint == Complete {
		return AlreadyComplete, nil
	}

	return Resume, nil
}
