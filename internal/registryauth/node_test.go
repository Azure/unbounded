// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestManagedNodeHelpersRestartIndependently(t *testing.T) {
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

func TestProbeNodeLayoutRejectsIsolatedKubelet(t *testing.T) {
	// A node root with no agent kubelet unit at all (the nspawn case, where the
	// unit lives inside the machine rootfs, not the host) is an unsupported
	// layout and must be a terminal failure, not an indefinite wait.
	root := t.TempDir()
	require.ErrorIs(t, probeNodeLayout(t.Context(), root), ErrUnsupportedKubeletLayout)
}

func TestProbeNodeLayoutToleratesMissingKubeletProcess(t *testing.T) {
	// A node root that has the agent kubelet unit but no running kubelet process
	// is a transient "wait for the kubelet", not an unsupported layout: the
	// probe must succeed so the reconcile loop keeps waiting.
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, filepath.Dir(unitPath)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, unitPath), []byte("[Service]\nExecStart=/usr/local/bin/kubelet $KUBELET_NODE_CONFIG_ARGS\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "proc"), 0o755))

	require.NoError(t, probeNodeLayout(t.Context(), root))
}

func TestProbeNodeLayoutAcceptsRunningKubelet(t *testing.T) {
	// A fully matching node (unit + running kubelet in the root's /proc) is
	// supported and must pass the probe.
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, filepath.Dir(unitPath)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, unitPath), []byte("[Service]\nExecStart=/usr/local/bin/kubelet $KUBELET_NODE_CONFIG_ARGS\n"), 0o644))

	proc := filepath.Join(root, "proc", "42")
	require.NoError(t, os.MkdirAll(proc, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(proc, "comm"), []byte("kubelet\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(proc, "cmdline"), []byte("/usr/local/bin/kubelet\x00--config=/x\x00"), 0o644))

	require.NoError(t, probeNodeLayout(t.Context(), root))
}

func TestRunNodeExternalModeIsNoOpOnUnsupportedLayout(t *testing.T) {
	// External (teardown) mode must not be blocked by the unsupported-layout
	// probe: on a node with no agent kubelet unit and no owned files, detach is
	// a no-op reconciliation, not ErrUnsupportedKubeletLayout. This is what lets
	// the supported detach converge on the very unsupported nodes the probe
	// introduces.
	root := t.TempDir()
	external := `{"mode":"external"}`
	// A bounded context so the loop would exit if it entered steady reconciliation.
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()

	// RunNode in external mode on an unsupported layout must not fail fast with
	// the unsupported-layout error; it reconciles (a no-op) until the context
	// expires, at which point runNode returns nil.
	err := RunNode(ctx, NodeOptions{Root: root, ConfigJSON: external, Listen: "127.0.0.1:0"})
	require.NoError(t, err, "external mode must reconcile to completion, not fail on an unsupported layout")
}

func TestProbeNodeLayoutReportsAmbiguousKubelet(t *testing.T) {
	// Two kubelet processes in the root is ambiguous and must be reported as an
	// error, not treated as a supported node.
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, filepath.Dir(unitPath)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, unitPath), []byte("[Service]\nExecStart=/usr/local/bin/kubelet $KUBELET_NODE_CONFIG_ARGS\n"), 0o644))

	for _, pid := range []string{"10", "20"} {
		proc := filepath.Join(root, "proc", pid)
		require.NoError(t, os.MkdirAll(proc, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(proc, "comm"), []byte("kubelet\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(proc, "cmdline"), []byte("/usr/local/bin/kubelet\x00\x00"), 0o644))
	}

	require.Error(t, probeNodeLayout(t.Context(), root))
}
