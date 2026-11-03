// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNodeReconciliationDoesNotContactKubernetes(t *testing.T) {
	var requests atomic.Int64

	api := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer api.Close()

	t.Setenv("KUBERNETES_SERVICE_HOST", api.Listener.Addr().String())
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	require.NoError(t, RunNode(ctx, NodeOptions{
		Root: t.TempDir(), ConfigJSON: `{"mode":"external"}`, Listen: "127.0.0.1:0",
	}))
	require.Zero(t, requests.Load())
}

func TestManagedNodeHelpersRestartIndependentlyWithoutAPIRequests(t *testing.T) {
	var requests atomic.Int64

	api := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer api.Close()

	t.Setenv("KUBERNETES_SERVICE_HOST", api.Listener.Addr().String())
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")

	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	results := make(chan error, 2)
	installed := make(chan error, 2)
	restarts := make([]*int, 0, 2)

	cfg := testManagedConfig(t)
	for range 2 {
		installer, _, count := installerFixture(t)
		restarts = append(restarts, count)
		command := installer.HostCommand
		installer.HostCommand = func(ctx context.Context, args ...string) error {
			if args[0] == "restart" {
				entered <- struct{}{}

				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}

			return command(ctx, args...)
		}

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		go func() {
			results <- runNode(ctx, "127.0.0.1:0", IdentityBindingMode, func(reconcileCtx context.Context) error {
				if err := installer.Reconcile(reconcileCtx, cfg); err != nil {
					installed <- err

					cancel()

					return err
				}

				err := installer.Reconcile(reconcileCtx, cfg)
				installed <- err

				cancel()

				return err
			})
		}()
	}

	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("another node must not block this node's restart")
		}
	}

	close(release)

	for range 2 {
		require.NoError(t, <-results)
		require.NoError(t, <-installed)
	}

	for _, count := range restarts {
		require.Equal(t, 1, *count, "unchanged local repair must not restart again")
	}

	require.Zero(t, requests.Load())
}

func TestNodeCancellationDoesNotInvokeHostOperation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	called := false

	require.NoError(t, runNode(ctx, "127.0.0.1:0", IdentityBindingMode, func(context.Context) error {
		called = true
		return nil
	}))
	require.False(t, called)
}

func TestNodeRejectsInvalidInlineConfiguration(t *testing.T) {
	for _, config := range []string{"", "{", `{"mode":"unsupported"}`} {
		require.Error(t, RunNode(t.Context(), NodeOptions{ConfigJSON: config}))
	}
}
