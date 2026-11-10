// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/cmd/agent/internal/noneops"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestAuthorizeNoneStart(t *testing.T) {
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
		wantContains string
	}{
		{name: "applied allocation config", applied: loadResult{cfg: applied, found: true}, record: loadResult{cfg: record, found: true}, wantVerified: applied},
		{name: "applied non-allocation config", applied: loadResult{cfg: appliedPlain, found: true}, wantVerified: appliedPlain},
		{name: "no applied config, record present", record: loadResult{cfg: record, found: true}, wantVerified: record},
		{
			name:         "applied config fails to load, record present",
			applied:      loadResult{err: errors.New("checksum mismatch")},
			record:       loadResult{cfg: record, found: true},
			wantVerified: record,
		},
		{
			name:         "record cannot be read",
			record:       loadResult{err: errors.New("decode allocation launch config: unexpected EOF")},
			wantRefused:  true,
			wantContains: "unexpected EOF",
		},
		// The check exists only on an allocation host, so finding neither
		// means the agent's configuration was removed under it.
		{name: "neither", wantRefused: true, wantContains: "no applied config and no allocation launch record"},
		{
			name:         "applied config fails to load, no record",
			applied:      loadResult{err: errors.New("checksum mismatch")},
			wantRefused:  true,
			wantContains: "no applied config and no allocation launch record",
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

			err := authorizeNoneStart(context.Background(), testLogger(),
				func() (*provision.AgentConfig, bool, error) {
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

			if tt.wantContains != "" {
				require.ErrorContains(t, err, tt.wantContains)
			}
		})
	}
}

func TestAllocationLaunchCheckCommand(t *testing.T) {
	t.Parallel()

	root := newRootCommand(&CommandContext{LogFormat: "text"})

	cmd, _, err := root.Find([]string{noneops.LaunchCheckCommand})
	require.NoError(t, err)
	require.Equal(t, "allocation-launch-check", cmd.Name())
	require.True(t, cmd.Hidden)
	require.True(t, cmd.SilenceUsage)
	require.Error(t, cmd.Args(cmd, []string{"kube1"}), "it takes no arguments")
	require.NoError(t, cmd.Args(cmd, nil))

	final := fmt.Errorf("%w: machine is fenced", daemon.ErrAllocationLaunchRefused)
	require.Equal(t, goalstates.AllocationLaunchRefusedExitCode, commandExitCode(finalRefusalExit(final)))
	require.ErrorIs(t, finalRefusalExit(final), daemon.ErrAllocationLaunchRefused)

	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	budgetSpent := unconfirmedRefusal(t, expired)
	require.Equal(t, 1, commandExitCode(finalRefusalExit(budgetSpent)))

	plain := errors.New("boom")
	require.Equal(t, 1, commandExitCode(finalRefusalExit(plain)))
	require.Same(t, plain, finalRefusalExit(plain))

	require.NoError(t, finalRefusalExit(nil))
}

// The command is wired to the none node's applied config and the launch record.
func TestAllocationLaunchCheckCommandRefusesWithNeitherConfig(t *testing.T) {
	t.Parallel()

	var logged []string

	log := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		logged = append(logged, string(p))

		return len(p), nil
	}), nil))

	err := finalRefusalExit(authorizeNoneStart(context.Background(), log,
		func() (*provision.AgentConfig, bool, error) { return nil, false, nil },
		func() (*provision.AgentConfig, bool, error) { return nil, false, nil },
		func(context.Context, *slog.Logger, *provision.AgentConfig) error {
			t.Error("verify must not run without a config")

			return nil
		}))
	require.Equal(t, goalstates.AllocationLaunchRefusedExitCode, commandExitCode(err))
	require.Len(t, logged, 1)
	require.Contains(t, logged[0], "allocation launch refused")
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
