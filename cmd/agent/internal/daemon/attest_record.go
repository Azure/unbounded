// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Azure/unbounded/cmd/agent/internal/hostnode"
	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// attestRecordName is the file in the agent configuration directory that keeps
// the attestation settings a host was bootstrapped with. The applied config
// does not carry them, and the bootstrap token it does carry is short lived.
// A reprovision needs the settings to attest again instead of reusing a token
// that has expired.
const attestRecordName = "attest.json"

// AttestRecordPath is where the attestation settings are recorded on the host.
func AttestRecordPath() string { return filepath.Join(goalstates.AgentConfigDir, attestRecordName) }

// PersistAttestConfig returns a task that records the attestation settings the
// host was bootstrapped with at AttestRecordPath. It does nothing when attest
// is nil or has no URL.
func PersistAttestConfig(log *slog.Logger, attest *provision.AgentAttestConfig) phases.Task {
	return persistAttestConfigAt(log, AttestRecordPath(), attest)
}

func persistAttestConfigAt(log *slog.Logger, path string, attest *provision.AgentAttestConfig) phases.Task {
	return hostnode.Func("persist-attest-config", func(context.Context) error {
		if attest == nil || attest.URL == "" {
			return nil
		}

		data, err := json.MarshalIndent(attest, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal attestation settings: %w", err)
		}

		if err := fsutil.WriteFileDurable(path, data, 0o600); err != nil {
			return fmt.Errorf("write attestation settings to %s: %w", path, err)
		}

		log.Info("attestation settings persisted", "path", path)

		return nil
	})
}

// readAttestRecord returns the attestation settings recorded at path, or nil
// when the host was not bootstrapped with attestation (or by an agent that did
// not record it).
func readAttestRecord(path string) (*provision.AgentAttestConfig, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil //nolint:nilnil // absence is the answer, not an error
	}

	if err != nil {
		return nil, fmt.Errorf("read attestation settings: %w", err)
	}

	var attest provision.AgentAttestConfig
	if err := json.Unmarshal(data, &attest); err != nil {
		return nil, fmt.Errorf("decode attestation settings %s: %w", path, err)
	}

	if attest.URL == "" {
		return nil, nil //nolint:nilnil // an empty record is no record
	}

	return &attest, nil
}
