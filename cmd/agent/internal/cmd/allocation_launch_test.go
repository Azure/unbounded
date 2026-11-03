// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/internal/provision"
)

func allocationTestConfig(name string) *provision.AgentConfig {
	return &provision.AgentConfig{
		MachineName: name,
		Kubelet: provision.AgentKubeletConfig{
			Configuration: map[string]any{"providerID": v1alpha3.AllocationProviderIDPrefix + "6d3f0d6e-8d1a-4c1e-9f61-0d7b8f0b2a10"},
		},
	}
}

func plainTestConfig(name string) *provision.AgentConfig {
	return &provision.AgentConfig{MachineName: name}
}

// recordingVerify returns a verify function that records the config it was
// asked about and answers with err.
func recordingVerify(verified **provision.AgentConfig, err error) func(context.Context, *slog.Logger, *provision.AgentConfig) error {
	return func(_ context.Context, _ *slog.Logger, cfg *provision.AgentConfig) error {
		*verified = cfg

		return err
	}
}

func TestAuthorizeNSpawnStart(t *testing.T) {
	t.Parallel()

	applied := allocationTestConfig("applied")
	appliedPlain := plainTestConfig("applied-plain")
	record := allocationTestConfig("record")
	refusal := fmt.Errorf("%w: machine is fenced", daemon.ErrAllocationLaunchRefused)

	type loadResult struct {
		cfg   *provision.AgentConfig
		found bool
		err   error
	}

	tests := []struct {
		name         string
		applied      loadResult
		record       loadResult
		verifyErr    error
		wantVerified *provision.AgentConfig
		wantRefused  bool
		wantErr      error
	}{
		{name: "applied allocation config", applied: loadResult{cfg: applied, found: true}, record: loadResult{cfg: record, found: true}, wantVerified: applied},
		{name: "applied non-allocation config", applied: loadResult{cfg: appliedPlain, found: true}, wantVerified: appliedPlain},
		// A fence that lands after bootstrap's own check must still stop the
		// first nspawn start, which happens before the applied config exists.
		{name: "no applied config, record present", record: loadResult{cfg: record, found: true}, wantVerified: record},
		{name: "no applied config, no record"},
		{
			name:         "applied config fails to load, record present",
			applied:      loadResult{err: errors.New("checksum mismatch")},
			record:       loadResult{cfg: record, found: true},
			wantVerified: record,
		},
		{
			name:        "record cannot be read",
			record:      loadResult{err: errors.New("decode allocation launch config: unexpected EOF")},
			wantRefused: true,
		},
		{
			name:         "verify refuses",
			applied:      loadResult{cfg: applied, found: true},
			verifyErr:    refusal,
			wantVerified: applied,
			wantRefused:  true,
			wantErr:      refusal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var verified *provision.AgentConfig

			err := authorizeNSpawnStart(context.Background(), testLogger(), "kube1",
				func(machineName string) (*provision.AgentConfig, bool, error) {
					require.Equal(t, "kube1", machineName)

					return tt.applied.cfg, tt.applied.found, tt.applied.err
				},
				func() (*provision.AgentConfig, bool, error) {
					return tt.record.cfg, tt.record.found, tt.record.err
				},
				recordingVerify(&verified, tt.verifyErr))

			require.Same(t, tt.wantVerified, verified)

			if !tt.wantRefused {
				require.NoError(t, err)

				return
			}

			require.ErrorIs(t, err, daemon.ErrAllocationLaunchRefused)
			require.True(t, daemon.IsFinalAllocationLaunchRefusal(err), "final: %v", err)

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
			}
		})
	}

	// The real guard passes a host that is not allocation-backed without
	// reading anything.
	t.Run("applied non-allocation config with the real guard", func(t *testing.T) {
		t.Parallel()

		err := authorizeNSpawnStart(context.Background(), testLogger(), "kube1",
			func(string) (*provision.AgentConfig, bool, error) { return appliedPlain, true, nil },
			func() (*provision.AgentConfig, bool, error) {
				t.Error("unexpected record load")

				return nil, false, nil
			},
			daemon.VerifyAllocationLaunch)
		require.NoError(t, err)
	})
}

func TestGuardNodeStart(t *testing.T) {
	t.Parallel()

	refusal := fmt.Errorf("%w: machine is fenced", daemon.ErrAllocationLaunchRefused)

	tests := []struct {
		name       string
		cfg        *provision.AgentConfig
		persistErr error
		verifyErr  error
		wantCalls  []string
		wantErr    error
	}{
		{name: "allocation host allowed", cfg: allocationTestConfig("m"), wantCalls: []string{"persist", "verify", "start"}},
		{name: "allocation host refused", cfg: allocationTestConfig("m"), verifyErr: refusal, wantCalls: []string{"persist", "verify"}, wantErr: refusal},
		{name: "persist fails", cfg: allocationTestConfig("m"), persistErr: errors.New("disk full"), wantCalls: []string{"persist"}},
		{name: "non-allocation host", cfg: plainTestConfig("m"), wantCalls: []string{"start"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var calls []string

			err := guardNodeStart(context.Background(), testLogger(), tt.cfg,
				func(cfg *provision.AgentConfig) error {
					require.Same(t, tt.cfg, cfg)

					calls = append(calls, "persist")

					return tt.persistErr
				},
				func(_ context.Context, _ *slog.Logger, cfg *provision.AgentConfig) error {
					require.Same(t, tt.cfg, cfg)

					calls = append(calls, "verify")

					return tt.verifyErr
				},
				func(context.Context) error {
					calls = append(calls, "start")

					return nil
				})

			require.Equal(t, tt.wantCalls, calls)

			switch {
			case tt.persistErr != nil:
				require.ErrorIs(t, err, tt.persistErr)
			case tt.wantErr != nil:
				require.ErrorIs(t, err, daemon.ErrAllocationLaunchRefused)
				require.ErrorIs(t, err, tt.wantErr)
			default:
				require.NoError(t, err)
			}
		})
	}
}

// TestGuardNodeStartFenceAfterCheck replays the interleaving that makes the
// launch record necessary: bootstrap's check passes, the allocation is fenced,
// and only then does the nspawn machine start. Its pre-start finds no applied
// config, so it must find the record and refuse.
func TestGuardNodeStartFenceAfterCheck(t *testing.T) {
	t.Parallel()

	cfg := allocationTestConfig("m")
	fenced := false

	var stored *provision.AgentConfig

	verify := func(context.Context, *slog.Logger, *provision.AgentConfig) error {
		if fenced {
			return fmt.Errorf("%w: machine is fenced", daemon.ErrAllocationLaunchRefused)
		}

		return nil
	}

	var preStartErr error

	err := guardNodeStart(context.Background(), testLogger(), cfg,
		func(cfg *provision.AgentConfig) error {
			stored = cfg

			return nil
		},
		verify,
		func(ctx context.Context) error {
			fenced = true

			preStartErr = authorizeNSpawnStart(ctx, testLogger(), "kube1",
				func(string) (*provision.AgentConfig, bool, error) { return nil, false, nil },
				func() (*provision.AgentConfig, bool, error) { return stored, stored != nil, nil },
				verify)

			return preStartErr
		})

	require.ErrorIs(t, preStartErr, daemon.ErrAllocationLaunchRefused)
	require.ErrorIs(t, err, daemon.ErrAllocationLaunchRefused)
	require.Equal(t, daemon.AllocationLaunchRefusedReason, classifyNodeStartFailure(err))
}
