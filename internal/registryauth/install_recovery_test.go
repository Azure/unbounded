// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInstallerRetriesConfigChangeAfterFailedRestartWithActiveFlags(t *testing.T) {
	i, _, restarts := installerFixture(t)
	cfg := testManagedConfig(t)
	require.NoError(t, i.Reconcile(t.Context(), cfg))
	run := i.HostCommand
	i.HostCommand = func(context.Context, ...string) error { return errors.New("restart failed") }

	cfg.Registry = "other.azurecr.io"
	require.ErrorContains(t, i.Reconcile(t.Context(), cfg), "restart failed")
	require.FileExists(t, i.path(restartPath))
	i.HostCommand = run
	require.NoError(t, i.Reconcile(t.Context(), cfg))
	require.Equal(t, 2, *restarts, "must reload kubelet config even though its provider flags never changed")
	require.NoFileExists(t, i.path(restartPath))
}

func TestOwnedInstallerRecoversStoppedKubelet(t *testing.T) {
	for _, mode := range []string{IdentityBindingMode, ExternalMode} {
		t.Run(mode, func(t *testing.T) {
			i, _, restarts := installerFixture(t)
			cfg := testManagedConfig(t)
			require.NoError(t, i.Reconcile(t.Context(), cfg))
			readArgs, run := i.KubeletArgs, i.HostCommand
			stopped := true
			i.KubeletArgs = func() ([]string, error) {
				if stopped {
					return nil, ErrKubeletNotRunning
				}

				return readArgs()
			}
			i.HostCommand = func(ctx context.Context, args ...string) error {
				if args[0] == "restart" {
					stopped = false
				}

				return run(ctx, args...)
			}
			cfg.Mode = mode
			require.NoError(t, i.Reconcile(t.Context(), cfg))
			require.Equal(t, 2, *restarts)
		})
	}
}

func TestUnownedInstallerWaitsForKubeletBeforeTakingOwnership(t *testing.T) {
	i, _, restarts := installerFixture(t)
	i.KubeletArgs = func() ([]string, error) { return nil, ErrKubeletNotRunning }
	require.ErrorIs(t, i.Reconcile(t.Context(), testManagedConfig(t)), ErrKubeletNotRunning)
	require.Zero(t, *restarts)
	require.NoFileExists(t, i.path(ownerPath))
}

func TestInstallerRefusesExternalProviderWithSeparateFlagValue(t *testing.T) {
	i, args, restarts := installerFixture(t)
	*args = append(*args, "--image-credential-provider-config", "/etc/kubernetes/external.json")

	require.ErrorContains(t, i.Reconcile(t.Context(), testManagedConfig(t)), "external credential provider")
	require.Zero(t, *restarts)
	require.NoFileExists(t, i.path(ownerPath))
}
