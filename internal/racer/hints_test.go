// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/Azure/unbounded/internal/racer/authority"
	"github.com/Azure/unbounded/internal/racer/members"
	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestRecoveryHintsUnchangedLargeSnapshot(t *testing.T) {
	r := testTopology(t)
	gets := 0
	r.APIReader = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			gets++
			return errors.New("unexpected authoritative read")
		},
	})
	queued := 0
	r.enqueueHint = func(reconcile.Request) { queued++ }
	update := authority.TopologyHints{Members: make(members.History)}

	for i := range 100_000 {
		id := wire.NodeID(fmt.Sprintf("00000000-0000-4000-8000-%012d", i))
		member := wire.Member{Node: id, Shares: wire.DefaultShares, PeerEndpoint: "192.0.2.1:8082"}
		encoded, err := json.Marshal(member)
		require.NoError(t, err)

		update.Members[id] = member
		update.Nodes.Items = append(update.Nodes.Items, corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: string(id), UID: types.UID(id), Annotations: map[string]string{admittedMemberAnnotation: string(encoded)},
		}})
	}

	update.Nodes.Items = append(update.Nodes.Items,
		corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "excluded", Labels: map[string]string{wire.ExclusionLabel: ""}}},
		corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "unadmitted"}},
	)
	for range 2 {
		require.NoError(t, r.queueHints(t.Context(), update))
		require.Empty(t, r.hints)
	}

	require.Zero(t, gets)
	require.Zero(t, queued)
}

func TestRecoveryHintsQueueLatestSnapshot(t *testing.T) {
	for _, change := range []string{"desired", "satisfied", "absent", "unadmitted", "replacement", "excluded", "stale cache"} {
		t.Run(change, func(t *testing.T) {
			f := newServingFixture(t)
			r := f.a.Topology
			base := r.Client.(client.WithWatch)

			var node corev1.Node
			require.NoError(t, base.Get(f.ctx, client.ObjectKey{Name: "worker"}, &node))
			member := acceptedMembers(t, r)[testNodeUID]
			member.Shares = 7
			update := authority.TopologyHints{Nodes: corev1.NodeList{Items: []corev1.Node{*node.DeepCopy()}}, Members: members.History{testNodeUID: member}}

			queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
			defer queue.ShutDown()

			adds, gets, patches := 0, 0, 0
			r.enqueueHint = func(request reconcile.Request) { adds++; queue.Add(request) }
			r.APIReader = interceptor.NewClient(base, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					gets++
					return c.Get(ctx, key, obj, opts...)
				},
			})
			r.Client = interceptor.NewClient(base, interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					patches++
					return c.Patch(ctx, obj, patch, opts...)
				},
			})
			require.NoError(t, r.queueHints(f.ctx, update))
			require.Equal(t, 1, queue.Len())
			require.Zero(t, gets)
			require.Zero(t, patches)

			switch change {
			case "desired":
				member.Shares = 9
				update.Members[testNodeUID] = member
			case "satisfied":
				update.Members = acceptedMembers(t, r)
			case "absent":
				update.Nodes.Items = nil
			case "unadmitted":
				update.Members = nil
			case "replacement":
				require.NoError(t, base.Delete(f.ctx, &node))
				node.UID = types.UID(testOtherUID)
				node.ResourceVersion = ""
				node.Annotations = nil
				require.NoError(t, base.Create(f.ctx, &node))

				member.Node = testOtherUID
				update.Nodes.Items[0] = *node.DeepCopy()
				update.Members = members.History{testOtherUID: member}
			case "excluded":
				node.Labels = map[string]string{wire.ExclusionLabel: ""}
				require.NoError(t, base.Update(f.ctx, &node))
				update.Nodes.Items[0] = *node.DeepCopy()
				update.Members = nil
			case "stale cache":
				encoded, err := json.Marshal(member)
				require.NoError(t, err)

				node.Annotations[admittedMemberAnnotation] = string(encoded)
				require.NoError(t, base.Update(f.ctx, &node))
			}

			require.NoError(t, r.queueHints(f.ctx, update))
			require.Equal(t, 1, adds, "pending work must retain its retry delay")
			require.Equal(t, 1, queue.Len())
			require.Zero(t, gets)

			request, shutdown := queue.Get()
			require.False(t, shutdown)

			result, err := r.Reconcile(f.ctx, request)
			queue.Done(request)
			queue.Forget(request)
			require.NoError(t, err)
			require.Equal(t, ctrl.Result{}, result)
			require.Empty(t, r.hints)
			require.Zero(t, queue.Len())

			if change == "satisfied" || change == "absent" || change == "unadmitted" {
				require.Zero(t, gets, "obsolete requests must not read Nodes")
				require.Zero(t, patches)

				return
			}

			require.Equal(t, 1, gets)

			if change == "stale cache" {
				require.Zero(t, patches, "fresh read must suppress a redundant patch")
			} else {
				require.Equal(t, 1, patches)
			}

			require.NoError(t, base.Get(f.ctx, client.ObjectKeyFromObject(&node), &node))

			if change == "excluded" {
				require.Empty(t, node.Annotations[admittedMemberAnnotation])
				return
			}

			var saved wire.Member
			require.NoError(t, json.Unmarshal([]byte(node.Annotations[admittedMemberAnnotation]), &saved))
			require.Equal(t, member, saved)
		})
	}
}

func TestRecoveryHintRetriesWithoutPublication(t *testing.T) {
	for _, change := range []string{"unrelated", "replacement", "deleted", "excluded", "inputs", "spoof"} {
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
			case "deleted":
				require.NoError(t, base.Delete(f.ctx, &node))
			case "excluded":
				node.Labels = map[string]string{wire.ExclusionLabel: ""}
			case "inputs":
				node.Annotations[wire.SharesAnnotation] = "9"
			case "spoof":
				node.Annotations[admittedMemberAnnotation] = "spoof"
			}

			if change != "replacement" && change != "deleted" {
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

			if change == "deleted" {
				return
			}

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

func TestRecoveryHintCancellationOverridesConflict(t *testing.T) {
	for _, stage := range []string{"before", "read", "patch"} {
		t.Run(stage, func(t *testing.T) {
			f := newServingFixture(t)
			r := f.a.Topology

			var node corev1.Node
			require.NoError(t, r.Get(f.ctx, client.ObjectKey{Name: "worker"}, &node))
			node.Annotations[wire.SharesAnnotation] = "7"
			require.NoError(t, r.Update(f.ctx, &node))

			var queued reconcile.Request

			r.enqueueHint = func(request reconcile.Request) { queued = request }
			_, err := r.Reconcile(f.ctx, ctrl.Request{})
			require.NoError(t, err)
			require.Equal(t, "hints", queued.Namespace)
			published := capturePublication(t, r.authority)

			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()

			conflict := apierrors.NewConflict(corev1.Resource("nodes"), node.Name, errors.New("concurrent update"))
			gets, patches := 0, 0
			r.APIReader = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					gets++

					if stage == "read" {
						cancel()
						return conflict
					}

					return c.Get(ctx, key, obj, opts...)
				},
			})
			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
				Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
					patches++

					cancel()

					return conflict
				},
			})

			if stage == "before" {
				cancel()
			}

			result, err := r.Reconcile(ctx, queued)
			require.ErrorIs(t, err, context.Canceled)
			require.ErrorIs(t, err, reconcile.TerminalError(nil))
			require.Equal(t, ctrl.Result{}, result)
			require.Len(t, r.hints, 1)
			require.Equal(t, published.encoded, capturePublication(t, r.authority).encoded)

			if stage == "before" {
				require.Zero(t, gets)
			} else {
				require.Equal(t, 1, gets)
			}

			if stage == "patch" {
				require.Equal(t, 1, patches)
			} else {
				require.Zero(t, patches)
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
