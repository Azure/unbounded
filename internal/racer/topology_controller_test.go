// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestTopologyIndexedPodGroups(t *testing.T) {
	for _, stage := range []string{"success", "list error", "canceled list"} {
		t.Run(stage, func(t *testing.T) {
			nodeA, nodeB := memberNode(), memberNode()
			nodeB.Name, nodeB.UID = "node-b", testOtherUID
			podA, podB := memberPod("a", 1, "192.0.2.1"), memberPod("b", 1, "192.0.2.2")
			podB.Spec.NodeName = nodeB.Name
			r := initializedTopology(t, &nodeA, &nodeB, &podA, &podB, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
				Name: "racer-dataplane", Namespace: "racer", UID: testDaemonSetUID,
			}})
			r.Config.PeerPort = 7443

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			boom := errors.New("pod list failed")
			queries := map[string]int{}
			writes := 0
			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*corev1.PodList); ok {
						options := (&client.ListOptions{}).ApplyOptions(opts)
						if options.Namespace != r.Config.Namespace || options.FieldSelector == nil {
							t.Fatalf("Pod query lacks namespace or node index: %+v", options)
						}

						name, exact := options.FieldSelector.RequiresExactMatch(podNodeIndex)
						if !exact || (name != nodeA.Name && name != nodeB.Name) {
							t.Fatalf("unexpected Pod node selector: %v", options.FieldSelector)
						}

						queries[name]++

						if stage == "list error" {
							return boom
						}

						if stage == "canceled list" {
							defer cancel()
						}
					}

					return c.List(ctx, list, opts...)
				},
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					writes++
					return c.Update(ctx, obj, opts...)
				},
			})

			result, err := r.Reconcile(ctx, ctrl.Request{})
			if stage == "success" {
				if err != nil || result.RequeueAfter != 0 || queries[nodeA.Name] != 1 || queries[nodeB.Name] != 1 || len(r.Accepted) != 2 || r.Accepted[testNodeUID].PeerEndpoint != "192.0.2.1:7443" || r.Accepted[testOtherUID].PeerEndpoint != "192.0.2.2:7443" {
					t.Fatalf("indexed groups: queries=%v members=%v result=%v err=%v", queries, r.Accepted, result, err)
				}

				return
			}

			wantErr := boom
			if stage == "canceled list" {
				wantErr = context.Canceled

				if !errors.Is(err, reconcile.TerminalError(nil)) {
					t.Fatalf("canceled list is not terminal: %v", err)
				}
			}

			if !errors.Is(err, wantErr) || result.RequeueAfter != 0 || len(queries) != 1 || writes != 0 || len(r.Accepted) != 0 {
				t.Fatalf("failed listing changed state or continued: queries=%v writes=%d members=%v result=%v err=%v", queries, writes, r.Accepted, result, err)
			}

			if _, err := r.Publications.Current(); err == nil {
				t.Fatal("installed publication after failed listing")
			}
		})
	}
}

// Exercise the same ownership snapshot through discovery, durable publication,
// key admission, and restart rather than injecting an endpoint callback.
func TestTopologyOwnershipHistoryAndCatalogRestart(t *testing.T) {
	for _, workload := range []string{DataplaneDaemonSetName, PodNetworkDaemonSetName, "standalone-racer"} {
		t.Run(workload, func(t *testing.T) {
			node := memberNode()
			node.Annotations = map[string]string{wire.SharesAnnotation: "8"}
			pod := memberPod("current", 1, "192.0.2.1")
			pod.OwnerReferences[0].Name = workload
			ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: workload, UID: testDaemonSetUID}}

			r := initializedTopology(t, &node, &pod, ds)
			if workload == "standalone-racer" {
				r.Config.DaemonSetName = workload
			}

			r.Config.PeerPort = 7443
			a := Assemble(r.Config, r.Client, r.APIReader)
			r = a.Topology
			runKeys(t, a.Keyring)
			first := reconcileTopology(t, r, t.Context())
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(&node), &node))
			history := node.Annotations[admittedMemberAnnotation]
			require.NotEmpty(t, history)

			cache := catalogCache("cache-a", testOtherUID)
			require.NoError(t, r.Create(t.Context(), &cache))
			require.Same(t, first, reconcileTopology(t, r, t.Context()), "cache waits for committed keys")
			runKeys(t, a.Keyring)
			withCache := reconcileTopology(t, r, t.Context())
			published, err := wire.DecodePublication(strings.NewReader(withCache.encoded))
			require.NoError(t, err)
			require.Equal(t, []wire.CacheDefinition{{ID: testOtherUID, Name: "cache-a", ClientSocket: "/run/racer/cache-a/client/socket", OriginSocket: "/run/racer/cache-a/origin/socket"}}, published.Caches)
			require.Len(t, published.Members, 1)
			require.Equal(t, first.record.MembershipVersion, withCache.record.MembershipVersion)

			// The workload was recreated while the old Pod still exists. A fresh
			// process must recover history, not admit that Pod under its stale UID.
			require.NoError(t, r.Delete(t.Context(), ds))
			ds.UID, ds.ResourceVersion = "replacement", ""
			require.NoError(t, r.Create(t.Context(), ds))

			node.Annotations[wire.SharesAnnotation] = "malformed"
			require.NoError(t, r.Update(t.Context(), &node))
			a = Assemble(r.Config, r.Client, r.APIReader)
			r = a.Topology
			restarted := reconcileTopology(t, r, t.Context())
			require.Equal(t, withCache.encoded, restarted.encoded)
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(&node), &node))
			require.Equal(t, history, node.Annotations[admittedMemberAnnotation])

			// A new owned endpoint recovers independently of malformed attributes.
			replacement := memberPod("replacement", 2, "2001:db8::2")
			replacement.OwnerReferences[0] = *metav1.NewControllerRef(ds, appsv1.SchemeGroupVersion.WithKind("DaemonSet"))
			require.NoError(t, r.Create(t.Context(), &replacement))
			recovered := reconcileTopology(t, r, t.Context())
			published, err = wire.DecodePublication(strings.NewReader(recovered.encoded))
			require.NoError(t, err)
			require.Equal(t, "[2001:db8::2]:7443", published.Members[0].PeerEndpoint)
			require.Equal(t, uint32(8), published.Members[0].Shares)
			require.Len(t, published.Caches, 1)

			// Whole-candidate rejection leaves both publication and Node history
			// unchanged even when there is a valid membership update to publish.
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(&node), &node))
			history = node.Annotations[admittedMemberAnnotation]
			node.Annotations[wire.SharesAnnotation] = "16"
			require.NoError(t, r.Update(t.Context(), &node))

			invalid := catalogCache("cache-b", "not-a-uuid")
			require.NoError(t, r.Create(t.Context(), &invalid))
			_, err = r.Reconcile(t.Context(), ctrl.Request{})
			require.ErrorIs(t, err, wire.InvalidRequest)
			current, err := r.Publications.Current()
			require.NoError(t, err)
			require.Same(t, recovered, current)
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(&node), &node))
			require.Equal(t, history, node.Annotations[admittedMemberAnnotation])

			require.NoError(t, r.Delete(t.Context(), &invalid))
			require.NoError(t, r.Delete(t.Context(), &cache))

			node.Labels = map[string]string{wire.ExclusionLabel: ""}
			require.NoError(t, r.Update(t.Context(), &node))
			excluded := reconcileTopology(t, r, t.Context())
			published, err = wire.DecodePublication(strings.NewReader(excluded.encoded))
			require.NoError(t, err)
			require.Empty(t, published.Members)
			require.Empty(t, published.Caches)
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(&node), &node))
			require.Empty(t, node.Annotations[admittedMemberAnnotation])
		})
	}
}

func TestTopologyCustomWorkloadOwnership(t *testing.T) {
	for _, scenario := range []string{"current", "wrong name", "stale UID", "wrong namespace", "wrong kind", "wrong API version", "not controller", "labels only", "missing workload", "deleting workload"} {
		t.Run(scenario, func(t *testing.T) {
			node := memberNode()
			pod := memberPod("custom", 1, "192.0.2.1")
			pod.OwnerReferences[0].Name = "standalone-racer"
			ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: "standalone-racer", UID: testDaemonSetUID}}

			switch scenario {
			case "wrong name":
				pod.OwnerReferences[0].Name = DataplaneDaemonSetName
			case "stale UID":
				pod.OwnerReferences[0].UID = "stale"
			case "wrong namespace":
				pod.Namespace = "other"
			case "wrong kind":
				pod.OwnerReferences[0].Kind = "Deployment"
			case "wrong API version":
				pod.OwnerReferences[0].APIVersion = "apps/v2"
			case "not controller":
				pod.OwnerReferences[0].Controller = nil
			case "labels only":
				pod.OwnerReferences = nil
				pod.Labels = map[string]string{"app.kubernetes.io/name": ds.Name}
			case "deleting workload":
				ds.Finalizers = []string{"test/hold"}
			}

			r := initializedTopology(t, &node, &pod, ds)

			r.Config.DaemonSetName = ds.Name
			if scenario == "missing workload" || scenario == "deleting workload" {
				require.NoError(t, r.Delete(t.Context(), ds))
			}

			committed := reconcileTopology(t, r, t.Context())
			published, err := wire.DecodePublication(strings.NewReader(committed.encoded))
			require.NoError(t, err)
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(&node), &node))

			if scenario == "current" {
				require.Len(t, published.Members, 1)
				require.NotEmpty(t, node.Annotations[admittedMemberAnnotation])
			} else {
				require.Empty(t, published.Members)
				require.Empty(t, node.Annotations[admittedMemberAnnotation])
			}
		})
	}
}
