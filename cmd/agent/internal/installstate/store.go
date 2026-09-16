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
	DefaultDirectory  = "/var/lib/unbounded/agent"
	defaultLockPath   = "/run/unbounded-agent-install.lock"
	defaultHostPrefix = "/usr/local"
	schemaVersion     = 1
)

type Checkpoint string

const (
	PreparingHost    Checkpoint = "preparing-host"
	PreparingRootFS  Checkpoint = "preparing-rootfs"
	StartingNode     Checkpoint = "starting-node"
	InstallingDaemon Checkpoint = "installing-daemon"
	Complete         Checkpoint = "complete"
	Resetting        Checkpoint = "resetting"
)

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
	if r.SchemaVersion != schemaVersion || strings.TrimSpace(r.InstallID) == "" || strings.TrimSpace(r.MachineName) == "" || strings.TrimSpace(r.ConfigFingerprint) == "" {
		return fmt.Errorf("invalid installation record identity or schema")
	}
	// This release installs only at the default prefix. Unknown ownership must
	// not authorize cleanup at a guessed location.
	if r.HostPrefix != defaultHostPrefix {
		return fmt.Errorf("unsupported recorded host prefix %q", r.HostPrefix)
	}

	switch r.Checkpoint {
	case PreparingHost, PreparingRootFS, StartingNode, InstallingDaemon, Complete, Resetting:
		return nil
	default:
		return fmt.Errorf("unknown installation checkpoint %q", r.Checkpoint)
	}
}

var ErrNotFound = errors.New("installation record not found")

type Store struct{ root, lockPath string }

func NewStore(root, lockPath string) *Store  { return &Store{root: root, lockPath: lockPath} }
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
	r.Checkpoint = Complete
	return s.Save(r)
}

// Remove is called only after teardown's filesystem barriers succeed.
func (s *Store) Remove() error {
	if _, err := os.Stat(s.root); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}

	if err := os.Remove(s.statePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return fsutil.SyncDir(s.root)
}

func NewRecord(machine, fingerprint string) (Record, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return Record{}, err
	}

	return Record{
		SchemaVersion: schemaVersion, InstallID: hex.EncodeToString(id), MachineName: machine,
		HostPrefix: defaultHostPrefix, ConfigFingerprint: fingerprint, Checkpoint: PreparingHost,
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
