// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

// guardConfig returns an allocation-backed config the guard can build a
// bootstrap client for. Nothing listens at its API server.
func guardConfig(t *testing.T, uid string) *provision.AgentConfig {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	return &provision.AgentConfig{
		MachineName: "test-machine",
		NodeName:    "test-node",
		Cluster: provision.AgentClusterConfig{
			CaCertBase64: base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		},
		Kubelet: provision.AgentKubeletConfig{
			ApiServer:     "https://127.0.0.1:1",
			Auth:          provision.KubeletAuthInfo{BootstrapToken: "abc123.xyz789"},
			Configuration: map[string]any{"providerID": v1alpha3.AllocationProviderIDPrefix + uid},
		},
	}
}

// unconfirmedRefusal returns the guard's refusal when no read can be made
// before ctx ends. The context is over before the first read, so no request
// is sent.
func unconfirmedRefusal(t *testing.T, ctx context.Context) error {
	t.Helper()

	err := daemon.VerifyAllocationLaunch(ctx, testLogger(), guardConfig(t, "6d3f0d6e-8d1a-4c1e-9f61-0d7b8f0b2a10"))
	require.ErrorIs(t, err, daemon.ErrAllocationLaunchRefused)
	require.False(t, daemon.IsFinalAllocationLaunchRefusal(err), "refusal must not be final: %v", err)

	return err
}

func TestNSpawnPreStart(t *testing.T) {
	t.Parallel()

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	// The budget is the read context's deadline, so a deadline already past
	// takes the same path as a budget spent on transient failures.
	expiredCtx, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	budgetSpent := unconfirmedRefusal(t, expiredCtx)
	require.ErrorIs(t, budgetSpent, context.DeadlineExceeded)

	canceled := unconfirmedRefusal(t, canceledCtx)
	require.ErrorIs(t, canceled, context.Canceled)

	fenced := fmt.Errorf("%w for allocation %q, machine test-machine: machine is fenced", daemon.ErrAllocationLaunchRefused, "6d3f0d6e")

	malformed := daemon.VerifyAllocationLaunch(context.Background(), testLogger(), guardConfig(t, ""))
	require.True(t, daemon.IsFinalAllocationLaunchRefusal(malformed), "malformed identity must be final: %v", malformed)

	unreadableRecord := authorizeNSpawnStart(context.Background(), testLogger(), "kube1",
		func(string) (*provision.AgentConfig, bool, error) { return nil, false, nil },
		func() (*provision.AgentConfig, bool, error) {
			return nil, false, errors.New("decode allocation launch config: unexpected EOF")
		},
		func(context.Context, *slog.Logger, *provision.AgentConfig) error {
			t.Error("unexpected verify")

			return nil
		})

	regenerateErr := errors.New("render nspawn config")

	tests := []struct {
		name           string
		authorizeErr   error
		regenerateErr  error
		wantCode       int
		wantRegenerate bool
		wantRefused    bool
	}{
		{name: "fenced", authorizeErr: fenced, wantCode: goalstates.NSpawnPreStartRefusedExitCode, wantRefused: true},
		{name: "malformed identity", authorizeErr: malformed, wantCode: goalstates.NSpawnPreStartRefusedExitCode, wantRefused: true},
		{name: "unreadable record", authorizeErr: unreadableRecord, wantCode: goalstates.NSpawnPreStartRefusedExitCode, wantRefused: true},
		{name: "budget spent", authorizeErr: budgetSpent, wantCode: 1, wantRefused: true},
		{name: "canceled context", authorizeErr: canceled, wantCode: 1, wantRefused: true},
		{name: "authorized", wantRegenerate: true},
		{name: "authorized, regeneration fails", regenerateErr: regenerateErr, wantCode: 1, wantRegenerate: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			regenerated := false
			err := nspawnPreStart(
				func() error { return tt.authorizeErr },
				func() error {
					regenerated = true

					return tt.regenerateErr
				})

			require.Equal(t, tt.wantRegenerate, regenerated)

			switch {
			case tt.wantRefused:
				require.ErrorIs(t, err, daemon.ErrAllocationLaunchRefused)
				require.ErrorIs(t, err, tt.authorizeErr)
				require.Equal(t, tt.authorizeErr.Error(), err.Error())
			case tt.regenerateErr != nil:
				require.Same(t, tt.regenerateErr, err)
			default:
				require.NoError(t, err)

				return
			}

			require.Equal(t, tt.wantCode, commandExitCode(err))
		})
	}
}

func TestCommandExitCode(t *testing.T) {
	t.Parallel()

	finalRefusal := fmt.Errorf("starting-node: %w: machine is fenced", daemon.ErrAllocationLaunchRefused)
	require.True(t, daemon.IsFinalAllocationLaunchRefusal(finalRefusal))

	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "deferred daemon", err: fmt.Errorf("run daemon: %w", daemon.ErrDeferred), want: daemon.DeferredExitCode},
		{name: "plain error", err: errors.New("boom"), want: 1},
		// unbounded-agent start reports a final refusal like any failure.
		{name: "final refusal outside the pre-start", err: finalRefusal, want: 1},
		{
			name: "coded error",
			err:  fmt.Errorf("pre-start: %w", &exitCodeError{code: goalstates.NSpawnPreStartRefusedExitCode, err: finalRefusal}),
			want: goalstates.NSpawnPreStartRefusedExitCode,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tt.want, commandExitCode(tt.err))
		})
	}

	// The code must not collide with an exit status the agent already uses.
	require.NotContains(t, []int{0, 1, daemon.DeferredExitCode}, goalstates.NSpawnPreStartRefusedExitCode)
}

func TestNSpawnLifecycleSilencesUsage(t *testing.T) {
	t.Parallel()

	cmd := newCmdNSpawnLifecycle(&CommandContext{LogFormat: "text"})
	for _, sub := range cmd.Commands() {
		require.True(t, sub.SilenceUsage, "%s must silence usage", sub.Name())
	}

	runErr := fmt.Errorf("%w: machine is fenced", daemon.ErrAllocationLaunchRefused)
	phase := newCmdNSpawnLifecyclePhase(&CommandContext{LogFormat: "text"}, "pre-start", "test",
		func(context.Context, *slog.Logger, string) error { return runErr })

	var out bytes.Buffer
	phase.SetOut(&out)
	phase.SetErr(&out)
	phase.SetArgs([]string{"kube1"})

	err := phase.ExecuteContext(context.Background())
	require.ErrorIs(t, err, runErr)
	require.NotContains(t, out.String(), "Usage:")
}
