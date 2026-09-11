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
//   - How far did it get? Repeating host preparation under a running node, or
//     rebuilding a rootfs it is running from, is worse than not recovering.
//   - Which prefix did it use? Teardown needs it, but the applied config that
//     used to carry it is only written once the node is up, and is itself
//     deleted by teardown.
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
	"strings"
	"time"
)

// Dir is the default host directory holding agent installation state.
var Dir = "/var/lib/unbounded/agent"

const (
	// stateFileName holds the installation record.
	stateFileName = "install-state.json"

	// completeFileName is the durable marker that bootstrap finished. It is a
	// separate file rather than a field of the record because systemd's
	// ConditionPathExists is what consumes it, and that cannot read JSON.
	completeFileName = "bootstrap-complete"
)

// SchemaVersion is the version of the on-disk record. A record written by a
// newer agent is refused rather than guessed at.
const SchemaVersion = 1

// Record is the persisted description of an installation.
type Record struct {
	SchemaVersion int    `json:"schemaVersion"`
	InstallID     string `json:"installID"`
	MachineName   string `json:"machineName"`
	// HostPrefix is the resolved prefix, not the configured one, so teardown
	// does not have to repeat the defaulting rules.
	HostPrefix string `json:"hostPrefix"`
	// ConfigFingerprint identifies the bootstrap intent this install was
	// started for. A retry carrying different configuration is a new intent,
	// not a resume, and is refused rather than half-applied.
	ConfigFingerprint string `json:"configFingerprint"`
	// Checkpoint is the stage that still has to run.
	Checkpoint Checkpoint `json:"checkpoint"`
	UpdatedAt  time.Time  `json:"updatedAt"`
}

// ErrNotFound is returned when the host has no installation record.
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

func encodeRecord(rec Record) ([]byte, error) {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode installation record: %w", err)
	}

	return append(data, '\n'), nil
}

func decodeRecord(data []byte) (Record, error) {
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, fmt.Errorf("parse installation record: %w", err)
	}

	if rec.SchemaVersion > SchemaVersion {
		return Record{}, fmt.Errorf(
			"written by a newer agent (schema %d, supported %d)",
			rec.SchemaVersion, SchemaVersion,
		)
	}

	if err := rec.validate(); err != nil {
		return Record{}, fmt.Errorf("not a usable installation record: %w", err)
	}

	return rec, nil
}

// validate rejects a record that decoded but does not describe an installation.
//
// Decoding proves only that the bytes were JSON. A record missing its identity
// or checkpoint cannot answer the question the caller is asking, and guessing
// at the missing part is how a foreign or half-erased host gets adopted.
func (r Record) validate() error {
	var missing []string

	for _, field := range []struct {
		name  string
		value string
	}{
		{"installID", r.InstallID},
		{"machineName", r.MachineName},
		{"hostPrefix", r.HostPrefix},
		{"configFingerprint", r.ConfigFingerprint},
	} {
		if strings.TrimSpace(field.value) == "" {
			missing = append(missing, field.name)
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf("missing %s", strings.Join(missing, ", "))
	}

	if r.Checkpoint == "" {
		return errors.New("missing checkpoint")
	}

	if _, ok := validCheckpoints[r.Checkpoint]; !ok {
		return fmt.Errorf("unrecognized checkpoint %q", r.Checkpoint)
	}

	return nil
}
