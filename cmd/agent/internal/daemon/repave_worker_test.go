// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/installstate"
)

func TestRepaveReportingSurvivesAPIFailureAndDesiredChange(t *testing.T) {
	t.Parallel()
	store := repaveStore{dir: t.TempDir()}
	cfg := baseConfig()
	s := &repaveState{
		Version: 2, TransitionID: "transition", Phase: "reporting", Source: "kube1", Target: "kube2", SourceConfig: *cfg,
		TargetConfig: provision.UnboundedAgentConfig{AgentConfig: *cfg}, TargetRef: &v1alpha3.MachineConfigurationRefStatus{Name: "config", Version: 2, VersionName: "config-v2"},
		TargetBootID: "boot", TargetNodeUID: "node", VerifiedAt: ptr.To(time.Now()),
	}
	require.NoError(t, store.Save(s))

	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: cfg.MachineName}, Spec: v1alpha3.MachineSpec{ConfigurationRef: &v1alpha3.MachineConfigurationRef{Name: "config", Version: ptr.To(int32(3))}}}
	fail := true
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(machine).WithObjects(machine).WithInterceptorFuncs(interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if fail {
				return errors.New("status unavailable")
			}

			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	}).Build()
	w := &repaveWorker{client: c, reader: c, store: store, log: discardLogger()}
	require.Error(t, w.reportCompletion(t.Context(), s))

	loaded, err := store.Load()
	require.NoError(t, err)
	require.Equal(t, s.TargetRef, loaded.TargetRef)

	fail = false
	// A fresh worker resumes the durable reporting obligation without a Node event.
	w = &repaveWorker{client: c, reader: c, store: store, log: discardLogger()}
	require.NoError(t, w.reportCompletion(t.Context(), loaded))
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(machine), machine))
	require.Equal(t, int32(2), machine.Status.Configuration.Version)

	for _, condition := range machine.Status.Conditions {
		if condition.Type == v1alpha3.MachineConditionRepavePending {
			require.Equal(t, metav1.ConditionTrue, condition.Status)
		}
	}

	loaded, err = store.Load()
	require.NoError(t, err)
	require.Nil(t, loaded)

	_, err = os.Stat(filepath.Join(store.dir, "repave-applied.json"))
	require.NoError(t, err)
}

func TestBlockedRepaveRetainsStateAndReleasesMutationLock(t *testing.T) {
	original := installstate.LockPathForTest
	installstate.LockPathForTest = filepath.Join(t.TempDir(), "install.lock")
	t.Cleanup(func() { installstate.LockPathForTest = original })

	for _, phase := range []string{"preparing", "starting", "verifying"} {
		t.Run(phase, func(t *testing.T) {
			store := repaveStore{dir: t.TempDir()}
			cfg := baseConfig()
			s := &repaveState{Version: 2, TransitionID: "t", Phase: phase, Source: "kube1", Target: "kube2", SourceConfig: *cfg, TargetConfig: provision.UnboundedAgentConfig{AgentConfig: *cfg}}
			require.NoError(t, store.Save(s))

			c := fakeStatusClient(&v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: cfg.MachineName}})
			w := &repaveWorker{
				client: c, reader: c, log: discardLogger(), store: store,
				loadOwner: func() (installstate.Record, error) { return installstate.Record{}, installstate.ErrNotFound },
				advance:   func(context.Context, *slog.Logger, *repaveState) error { return errors.New("artifact unavailable") },
				verify: func(context.Context, *slog.Logger, client.Reader, *repaveState) error {
					return errors.New("target API unavailable")
				},
			}
			_, err := w.step(t.Context())
			require.Error(t, err)
			loaded, err := store.Load()
			require.NoError(t, err)
			require.Equal(t, phase, loaded.Phase)

			lock, err := installstate.AcquireLock()
			require.NoError(t, err)
			require.NoError(t, lock.Release())

			var machine v1alpha3.Machine
			require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: cfg.MachineName}, &machine))
			require.NotEmpty(t, machine.Status.Conditions)

			for _, condition := range machine.Status.Conditions {
				require.Equal(t, "Blocked", condition.Reason)
			}
		})
	}
}

func TestWorkerShutdownCancelsAttemptAndReleasesLock(t *testing.T) {
	original := installstate.LockPathForTest
	installstate.LockPathForTest = filepath.Join(t.TempDir(), "install.lock")
	t.Cleanup(func() { installstate.LockPathForTest = original })
	store := repaveStore{dir: t.TempDir()}
	cfg := baseConfig()
	require.NoError(t, store.Save(&repaveState{Version: 2, TransitionID: "t", Phase: "preparing", Source: "kube1", Target: "kube2", SourceConfig: *cfg, TargetConfig: provision.UnboundedAgentConfig{AgentConfig: *cfg}}))
	c := fakeStatusClient(&v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: cfg.MachineName}})
	entered := make(chan struct{})
	w := &repaveWorker{
		client: c, reader: c, store: store, log: discardLogger(), wake: make(chan struct{}, 1),
		loadOwner: func() (installstate.Record, error) { return installstate.Record{}, installstate.ErrNotFound },
		advance: func(ctx context.Context, _ *slog.Logger, _ *repaveState) error {
			close(entered)
			<-ctx.Done()

			return ctx.Err()
		},
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- w.Start(ctx) }()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not join canceled attempt")
	}

	lock, err := installstate.AcquireLock()
	require.NoError(t, err)
	require.NoError(t, lock.Release())
}
