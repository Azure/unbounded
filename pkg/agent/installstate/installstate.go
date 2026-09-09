// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package installstate records what the agent has installed on a host, so that
// an interrupted bootstrap can be told apart from a foreign deployment and from
// a finished one.
//
// Three questions cannot be answered from the host's files alone:
//
//   - Is a half-provisioned host the wreckage of *this* installation, which may
//     be resumed, or someone else's, which must not be touched?
//   - Did bootstrap actually finish? The daemon unit file is written several
//     fallible steps before the daemon is enabled and running, so its presence
//     does not mean the install completed.
//   - Which prefix did the install use? Teardown needs it, but the applied
//     config that used to carry it is only written once the node is up, and is
//     itself deleted by teardown.
//
// The record is deliberately kept outside both the node rootfs and
// /etc/unbounded/agent: it has to survive the removal of everything bootstrap
// created, right up until teardown has finished using it.
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
	"time"

	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
)

// Dir is the host directory holding agent installation state.
//
// A variable rather than a constant so tests can redirect it. The absolute
// paths elsewhere in the agent cannot be redirected, which leaves their
// behavior around real files untested; this record carries the decision of
// whether to resume or refuse a bootstrap, so it needs to be exercised
// directly.
var Dir = "/var/lib/unbounded/agent"

const (
	// stateFileName holds the installation record.
	stateFileName = "install-state.json"

	// completeFileName is the durable marker that bootstrap finished. It is a
	// separate empty file rather than a field of the record because systemd's
	// ConditionPathExists is what consumes it, and that cannot read JSON.
	completeFileName = "bootstrap-complete"
)

// SchemaVersion is the version of the on-disk record. A record written by a
// newer agent is refused rather than guessed at.
const SchemaVersion = 1

// Stage is how far an installation has progressed.
type Stage string

const (
	// StageInstalling is set before the first agent-side mutation and stays
	// until the install either completes or is torn down.
	StageInstalling Stage = "installing"

	// StageComplete is set only after the daemon is enabled and running.
	StageComplete Stage = "complete"

	// StageResetting is set before teardown removes anything, so an interrupted
	// teardown is never mistaken for a resumable install.
	StageResetting Stage = "resetting"
)

// Record is the persisted description of an installation.
type Record struct {
	SchemaVersion int    `json:"schemaVersion"`
	InstallID     string `json:"installID"`
	MachineName   string `json:"machineName"`
	// HostPrefix is the resolved prefix, not the configured one, so teardown
	// does not have to repeat the defaulting rules.
	HostPrefix string `json:"hostPrefix"`
	// ConfigFingerprint identifies the configuration this install was started
	// for. A retry carrying different configuration is a new intent, not a
	// resume, and is refused rather than half-applied.
	ConfigFingerprint string    `json:"configFingerprint"`
	Stage             Stage     `json:"stage"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

// StatePath returns the path of the installation record.
func StatePath() string { return filepath.Join(Dir, stateFileName) }

// CompletePath returns the path of the durable bootstrap completion marker.
func CompletePath() string { return filepath.Join(Dir, completeFileName) }

// ErrNotFound is returned by Load when no record exists.
var ErrNotFound = errors.New("no installation record")

// Fingerprint returns a stable identifier for the configuration an install was
// started for.
func Fingerprint(config []byte) string {
	sum := sha256.Sum256(config)
	return hex.EncodeToString(sum[:])
}

// NewInstallID returns a random identifier for a fresh installation.
func NewInstallID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate install id: %w", err)
	}

	return hex.EncodeToString(buf), nil
}

// Load returns the installation record, or ErrNotFound when the host has none.
//
// A record that cannot be parsed is an error rather than an absent record: it
// may describe an install whose files are still on the host, and treating it as
// absent would let bootstrap run over the top of them.
func Load() (Record, error) {
	data, err := os.ReadFile(StatePath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Record{}, ErrNotFound
		}

		return Record{}, fmt.Errorf("read %s: %w", StatePath(), err)
	}

	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, fmt.Errorf("parse %s: %w", StatePath(), err)
	}

	if rec.SchemaVersion > SchemaVersion {
		return Record{}, fmt.Errorf(
			"%s was written by a newer agent (schema %d, supported %d)",
			StatePath(), rec.SchemaVersion, SchemaVersion,
		)
	}

	if err := rec.validate(); err != nil {
		return Record{}, fmt.Errorf("%s is not a usable installation record: %w", StatePath(), err)
	}

	return rec, nil
}

// validate rejects a record that decoded but does not describe an installation.
//
// Decoding proves only that the bytes were JSON. A record missing its identity
// or stage cannot answer the question the caller is asking, and guessing at the
// missing part is how a foreign or half-erased host gets adopted.
func (r Record) validate() error {
	var missing []string

	if strings.TrimSpace(r.InstallID) == "" {
		missing = append(missing, "installID")
	}

	if strings.TrimSpace(r.MachineName) == "" {
		missing = append(missing, "machineName")
	}

	if strings.TrimSpace(r.HostPrefix) == "" {
		missing = append(missing, "hostPrefix")
	}

	if strings.TrimSpace(r.ConfigFingerprint) == "" {
		missing = append(missing, "configFingerprint")
	}

	if len(missing) > 0 {
		return fmt.Errorf("missing %s", strings.Join(missing, ", "))
	}

	switch r.Stage {
	case StageInstalling, StageComplete, StageResetting:
		return nil
	case "":
		return errors.New("missing stage")
	default:
		return fmt.Errorf("unrecognized stage %q", r.Stage)
	}
}

// Save writes the installation record atomically and durably.
func Save(rec Record) error {
	rec.SchemaVersion = SchemaVersion
	rec.UpdatedAt = time.Now().UTC()

	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("encode installation record: %w", err)
	}

	// 0o600: the record names host paths and a config fingerprint, and nothing
	// unprivileged needs to read it.
	//
	// Durable rather than merely atomic: this record is what tells the next
	// boot whether a half-built host is ours to finish. A rename that has not
	// reached disk is exactly the case it has to survive.
	if err := utilio.WriteFileDurable(StatePath(), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", StatePath(), err)
	}

	return nil
}

// SetStage persists a new stage for an existing record.
func SetStage(rec Record, stage Stage) (Record, error) {
	rec.Stage = stage
	if err := Save(rec); err != nil {
		return Record{}, err
	}

	return rec, nil
}

// MarkComplete records that bootstrap finished.
//
// The record is written before the marker so that a crash between the two
// leaves the install looking unfinished rather than finished: resuming a
// complete install is recoverable, skipping an incomplete one is not.
//
// The marker carries the install ID so that a marker left behind by an earlier
// installation cannot vouch for a later one.
func MarkComplete(rec Record) error {
	if _, err := SetStage(rec, StageComplete); err != nil {
		return err
	}

	if err := utilio.WriteFileDurable(CompletePath(), []byte(rec.InstallID+"\n"), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", CompletePath(), err)
	}

	return nil
}

// IsComplete reports whether the durable completion marker is present.
func IsComplete() bool {
	_, err := os.Stat(CompletePath())
	return err == nil
}

// CompletionMatches reports whether the completion marker was written by this
// installation.
//
// A marker naming a different install belongs to a host that was torn down
// incompletely, and must not be read as this installation having finished.
func CompletionMatches(rec Record) (bool, error) {
	data, err := os.ReadFile(CompletePath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}

		return false, fmt.Errorf("read %s: %w", CompletePath(), err)
	}

	// Markers written before the ID was recorded are empty. Those can only be
	// judged by the record beside them, which Decide has already matched.
	recorded := strings.TrimSpace(string(data))
	if recorded == "" {
		return true, nil
	}

	return recorded == rec.InstallID, nil
}

// Remove deletes the completion marker and the installation record.
//
// The marker goes first: if only one of the two survives an interrupted
// teardown, it must be the record, because that is what a later teardown needs
// to find the prefix it was still cleaning up.
func Remove() error {
	if err := os.Remove(CompletePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", CompletePath(), err)
	}

	if err := os.Remove(StatePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", StatePath(), err)
	}

	return nil
}
