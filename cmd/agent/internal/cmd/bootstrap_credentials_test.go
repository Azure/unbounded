// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/cmd/agent/internal/hostnode"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// credentialSeams replaces the attestation and record steps of
// prepareCredentials with recorders and returns the calls they saw, in order.
type credentialSeams struct {
	calls      []string
	attested   *provision.AgentAttestConfig
	persisted  *provision.AgentAttestConfig
	attestErr  error
	persistErr error
	token      string
}

func useCredentialSeams(t *testing.T, seams *credentialSeams) {
	t.Helper()

	oldApply, oldPersist := applyAttestation, persistAttestConfig

	t.Cleanup(func() { applyAttestation, persistAttestConfig = oldApply, oldPersist })

	applyAttestation = func(_ *slog.Logger, attest *provision.AgentAttestConfig, _ string, gs *goalstates.NodeStart) phases.Task {
		return hostnode.Func("apply-attestation", func(context.Context) error {
			seams.calls = append(seams.calls, "attest")
			seams.attested = attest

			if seams.attestErr != nil {
				return seams.attestErr
			}

			// What a successful attestation does to the goal state.
			gs.Kubelet.BootstrapToken = seams.token

			return nil
		})
	}

	persistAttestConfig = func(_ *slog.Logger, attest *provision.AgentAttestConfig) phases.Task {
		return hostnode.Func("persist-attest-config", func(context.Context) error {
			seams.calls = append(seams.calls, "persist")
			seams.persisted = attest

			return seams.persistErr
		})
	}
}

func newCredentialStages() *agentStages {
	return &agentStages{
		log: slog.New(slog.DiscardHandler),
		cfg: &provision.UnboundedAgentConfig{AgentConfig: config.AgentConfig{MachineName: "m1"}},
		gs:  &goalstates.MachineGoalState{NodeStart: &goalstates.NodeStart{MachineName: goalstates.NSpawnMachineKube1}},
		// A reporter that is not ready, so no call leaves the process.
		reporter: &daemon.BootstrapStatusReporter{},
	}
}

// TestPrepareCredentialsRecordsTheAttestationSettingsAfterAttesting pins the
// order that matters to a reprovision: the settings are recorded for a host
// that attests, and only once attestation has produced its token.
func TestPrepareCredentialsRecordsTheAttestationSettingsAfterAttesting(t *testing.T) {
	seams := &credentialSeams{token: "attested-token"}
	useCredentialSeams(t, seams)

	attest := &provision.AgentAttestConfig{URL: "http://10.0.0.1:8880"}
	s := newCredentialStages()
	s.cfg.Attest = attest

	require.NoError(t, s.prepareCredentials(t.Context()))

	assert.Equal(t, []string{"attest", "persist"}, seams.calls)
	assert.Same(t, attest, seams.attested)
	assert.Same(t, attest, seams.persisted, "the record is of the settings the host attested with")
	assert.Equal(t, "attested-token", s.cfg.Kubelet.Auth.BootstrapToken, "the attested token reaches the agent config")
	assert.True(t, s.credentialsReady)
}

// A host that does not attest hands nil settings to the record step, which
// writes nothing. That is what keeps nspawn bootstrap unchanged.
func TestPrepareCredentialsPassesNoSettingsForAHostThatDoesNotAttest(t *testing.T) {
	seams := &credentialSeams{}
	useCredentialSeams(t, seams)

	s := newCredentialStages()

	require.NoError(t, s.prepareCredentials(t.Context()))

	assert.Equal(t, []string{"attest", "persist"}, seams.calls)
	assert.Nil(t, seams.persisted)
	assert.Empty(t, s.cfg.Kubelet.Auth.BootstrapToken)
	assert.True(t, s.credentialsReady)
}

// A record that cannot be written fails the stage of the modes that were built
// with the record, so a host never reaches the node work believing a
// reprovision can attest again when it cannot.
func TestPrepareCredentialsFailsWhenTheRecordCannotBeWritten(t *testing.T) {
	for _, mode := range []string{"none", "container"} {
		t.Run(mode, func(t *testing.T) {
			seams := &credentialSeams{persistErr: errors.New("read-only file system")}
			useCredentialSeams(t, seams)

			s := newCredentialStages()
			s.cfg.Isolation = mode
			s.cfg.Attest = &provision.AgentAttestConfig{URL: "http://10.0.0.1:8880"}

			err := s.prepareCredentials(t.Context())
			require.ErrorContains(t, err, "read-only file system")
			assert.False(t, s.credentialsReady, "the stage is retried on the next attempt")
		})
	}
}

// An nspawn host was bootstrapped before this record existed. A record that
// cannot be written there must not stop the bootstrap; a later reprovision
// falls back to the stored bootstrap token.
func TestPrepareCredentialsOnNSpawnToleratesARecordThatCannotBeWritten(t *testing.T) {
	for _, mode := range []string{"", "nspawn"} {
		t.Run("isolation "+mode, func(t *testing.T) {
			seams := &credentialSeams{persistErr: errors.New("read-only file system")}
			useCredentialSeams(t, seams)

			s := newCredentialStages()
			s.cfg.Isolation = mode
			s.cfg.Attest = &provision.AgentAttestConfig{URL: "http://10.0.0.1:8880"}

			require.NoError(t, s.prepareCredentials(t.Context()))
			assert.True(t, s.credentialsReady)
		})
	}
}

// A failed attestation records nothing: there is no token to refresh and the
// settings were never shown to work.
func TestPrepareCredentialsDoesNotRecordWhenAttestationFails(t *testing.T) {
	seams := &credentialSeams{attestErr: errors.New("tpm-attest: no device")}
	useCredentialSeams(t, seams)

	s := newCredentialStages()
	s.cfg.Attest = &provision.AgentAttestConfig{URL: "http://10.0.0.1:8880"}

	require.ErrorContains(t, s.prepareCredentials(t.Context()), "no device")
	assert.Equal(t, []string{"attest"}, seams.calls)
	assert.False(t, s.credentialsReady)
}

// Credentials are resolved once per process: a second call neither attests nor
// records again.
func TestPrepareCredentialsRunsOnce(t *testing.T) {
	seams := &credentialSeams{}
	useCredentialSeams(t, seams)

	s := newCredentialStages()

	require.NoError(t, s.prepareCredentials(t.Context()))
	require.NoError(t, s.prepareCredentials(t.Context()))

	assert.Equal(t, []string{"attest", "persist"}, seams.calls)
}
