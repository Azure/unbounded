// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
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
