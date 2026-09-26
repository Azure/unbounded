// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestNodeAuthorizationUsesOnlyLiveStateAfterIndexedHint(t *testing.T) {
	for _, scenario := range []string{"success", "excluded", "terminating", "deleted", "recreated", "stale exclusion", "missing hint", "ambiguous hint", "hint error", "API error", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			a, _, _ := authFixture(t)
			base := a.Topology.Client.(client.WithWatch)

			node := &corev1.Node{}
			if err := base.Get(t.Context(), client.ObjectKey{Name: "worker"}, node); err != nil {
				t.Fatal(err)
			}

			hint := node.DeepCopy()
			want := error(nil)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			switch scenario {
			case "excluded":
				node.Labels = map[string]string{wire.ExclusionLabel: ""}
				want = wire.Forbidden
			case "terminating":
				node.Finalizers = []string{"test/retain"}
				want = wire.Forbidden
			case "deleted", "recreated":
				want = wire.Forbidden
			case "stale exclusion":
				hint.Labels = map[string]string{wire.ExclusionLabel: ""}
				now := metav1.Now()
				hint.DeletionTimestamp = &now
				hint.Finalizers = []string{"test/retain"}
			case "missing hint", "ambiguous hint", "hint error", "API error":
				want = wire.Unavailable
			case "canceled":
				cancel()

				want = wire.Unavailable
			}

			if err := base.Update(t.Context(), node); err != nil {
				t.Fatal(err)
			}

			if scenario == "deleted" || scenario == "recreated" || scenario == "terminating" {
				if err := base.Delete(t.Context(), node); err != nil {
					t.Fatal(err)
				}
			}

			if scenario == "recreated" {
				node.UID = types.UID(testOtherUID)

				node.ResourceVersion = ""
				if err := base.Create(t.Context(), node); err != nil {
					t.Fatal(err)
				}
			}

			objects := []client.Object{hint}
			if scenario == "missing hint" {
				objects = nil
			}

			if scenario == "ambiguous hint" {
				other := hint.DeepCopy()
				other.Name = "other"
				objects = append(objects, other)
			}

			hints := fake.NewClientBuilder().WithScheme(base.Scheme()).WithObjects(objects...).WithIndex(&corev1.Node{}, nodeUIDIndex, nodeUIDKeys).Build()
			hintReader := interceptor.NewClient(hints, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				options := (&client.ListOptions{}).ApplyOptions(opts)
				if options.FieldSelector == nil || options.FieldSelector.String() != nodeUIDIndex+"="+testNodeUID {
					t.Fatal("Node hint query must use the UID index")
				}

				if scenario == "hint error" || ctx.Err() != nil {
					return wire.Unavailable
				}

				return c.List(ctx, list, opts...)
			}})
			gets := 0
			reader := interceptor.NewClient(base, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*corev1.Node); ok {
						gets++

						if key.Name != "worker" || key.Namespace != "" {
							t.Fatalf("unexpected live Node lookup: %v", key)
						}

						if scenario == "API error" {
							return wire.Unavailable
						}
					}

					return c.Get(ctx, key, obj, opts...)
				},
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*corev1.NodeList); ok {
						t.Fatal("authorization must never list live Nodes")
					}

					return c.List(ctx, list, opts...)
				},
			})

			err := authorizeNode(ctx, reader, hintReader, a.Server.Config, wire.NodeID(testNodeUID))
			if !errors.Is(err, want) {
				t.Fatalf("authorization = %v, want %v", err, want)
			}

			wantGets := 1
			if scenario == "missing hint" || scenario == "ambiguous hint" || scenario == "hint error" || scenario == "canceled" {
				wantGets = 0
			}

			if gets != wantGets {
				t.Fatalf("live Node GETs = %d, want %d", gets, wantGets)
			}
		})
	}
}
