// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestRecoveryHintRetriesWithoutPublication(t *testing.T) {
	for _, change := range []string{"unrelated", "replacement", "excluded", "inputs", "spoof"} {
		t.Run(change, func(t *testing.T) {
			f := newServingFixture(t)
			r := f.a.Topology

			var node corev1.Node
			require.NoError(t, r.Get(f.ctx, client.ObjectKey{Name: "worker"}, &node))
			node.Annotations[wire.SharesAnnotation] = "7"
			require.NoError(t, r.Update(f.ctx, &node))
			base := r.Client.(client.WithWatch)
			fail := true
			r.Client = interceptor.NewClient(base, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if fail {
					return apierrors.NewConflict(corev1.Resource("nodes"), obj.GetName(), errors.New("concurrent update"))
				}

				return c.Patch(ctx, obj, patch, opts...)
			}})

			var queued reconcile.Request

			r.enqueueHint = func(request reconcile.Request) { queued = request }
			result, err := r.Reconcile(f.ctx, ctrl.Request{})
			require.NoError(t, err)
			require.Equal(t, ctrl.Result{}, result)
			require.Equal(t, "worker", queued.Name)
			require.Equal(t, "hints", queued.Namespace)
			published := capturePublication(t, r.authority)
			result, err = r.Reconcile(f.ctx, queued)
			require.True(t, apierrors.IsConflict(err))
			require.Equal(t, ctrl.Result{}, result)
			require.NotEmpty(t, r.hints)
			require.NoError(t, base.Get(f.ctx, client.ObjectKeyFromObject(&node), &node))

			switch change {
			case "unrelated":
				node.Annotations["other"] = "preserved"
			case "replacement":
				require.NoError(t, base.Delete(f.ctx, &node))
				node.UID = types.UID(testOtherUID)
				node.ResourceVersion = ""
				node.Annotations = nil
				require.NoError(t, base.Create(f.ctx, &node))
			case "excluded":
				node.Labels = map[string]string{wire.ExclusionLabel: ""}
			case "inputs":
				node.Annotations[wire.SharesAnnotation] = "9"
			case "spoof":
				node.Annotations[admittedMemberAnnotation] = "spoof"
			}

			if change != "replacement" {
				require.NoError(t, base.Update(f.ctx, &node))
			}

			fail = false
			// Any topology or durable publication call here fails the test.
			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
				List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					t.Fatal("hint retry listed topology")
					return nil
				},
				Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
					t.Fatal("hint retry republished")
					return nil
				},
			})
			result, err = r.Reconcile(f.ctx, queued)
			require.NoError(t, err)
			require.Equal(t, ctrl.Result{}, result)
			require.Empty(t, r.hints)
			require.Equal(t, published.encoded, capturePublication(t, r.authority).encoded)
			require.NoError(t, base.Get(f.ctx, client.ObjectKeyFromObject(&node), &node))

			if change == "excluded" || change == "replacement" {
				require.Empty(t, node.Annotations[admittedMemberAnnotation])
				return
			}

			member, err := wire.DecodeAdmittedMember(strings.NewReader(node.Annotations[admittedMemberAnnotation]))
			require.NoError(t, err)

			if change == "inputs" {
				require.NotEqualValues(t, 7, member.Shares)
			} else {
				require.EqualValues(t, 7, member.Shares)
			}

			if change == "unrelated" {
				require.Equal(t, "preserved", node.Annotations["other"])
			}
		})
	}
}

func TestHandshakeTimeoutConfiguration(t *testing.T) {
	values := map[string]string{"RACER_CLUSTER_ID": testOtherUID}
	lookup := func(key string) (string, bool) { value, ok := values[key]; return value, ok }
	cfg, err := ConfigFromLookup(lookup)
	require.NoError(t, err)
	require.Equal(t, 5*time.Second, cfg.Limits.HandshakeTimeout)

	values["RACER_HANDSHAKE_TIMEOUT"] = "2s"
	cfg, err = ConfigFromLookup(lookup)
	require.NoError(t, err)
	require.Equal(t, 2*time.Second, cfg.serverConfig().Limits.HandshakeTimeout)
	require.Equal(t, 30*time.Second, cfg.Limits.WriteTimeout)

	for _, value := range []string{"", "bad", "0s", "-1s", "500ms"} {
		values["RACER_HANDSHAKE_TIMEOUT"] = value
		_, err := ConfigFromLookup(lookup)
		require.ErrorContains(t, err, "RACER_HANDSHAKE_TIMEOUT")
		require.NotErrorIs(t, err, wire.InvalidRequest)
	}
}

func TestNamedConfigMapCache(t *testing.T) {
	cfg := testConfig(t)
	options := managerOptions(cfg, runtime.NewScheme())
	require.Empty(t, options.LeaderElectionID)
	require.Empty(t, options.LeaderElectionNamespace)

	for obj, config := range options.Cache.ByObject {
		if _, ok := obj.(*corev1.ConfigMap); !ok {
			continue
		}

		require.Len(t, config.Namespaces, 1)
		require.Contains(t, config.Namespaces, cfg.Namespace)
		require.True(t, config.Field.Matches(fields.Set{"metadata.name": cfg.VersionConfigMapName}))
		require.False(t, config.Field.Matches(fields.Set{"metadata.name": "unrelated"}))
		require.False(t, config.Field.Matches(fields.Set{"metadata.name": cfg.InstallationConfigMapName}), "installation uses a separate exact-name source")

		return
	}

	t.Fatal("ConfigMap cache missing")
}

func TestControllerConflictUsesErrorBackoff(t *testing.T) {
	for _, component := range []string{"topology", "keyring"} {
		t.Run(component, func(t *testing.T) {
			r, now := testKeyring(t)
			runKeys(t, r)
			_, _, state, _ := keyState(t, r)
			*now = state.NextRotation
			d := fixtureDependencies[r.authority]
			conflict := apierrors.NewConflict(corev1.Resource("configmaps"), "version", errors.New("concurrent update"))
			wrapped := interceptor.NewClient(d.Client.(client.WithWatch), interceptor.Funcs{Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error { return conflict }})

			var target reconcile.Reconciler = r
			if component == "topology" {
				target = Assemble(r.config, wrapped, d.reader).Topology
			} else {
				d.Client = wrapped
			}

			result, err := target.Reconcile(t.Context(), ctrl.Request{})
			require.ErrorIs(t, err, conflict)
			require.Equal(t, ctrl.Result{}, result)
			require.NotErrorIs(t, err, reconcile.TerminalError(nil))
		})
	}
}
