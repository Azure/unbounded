// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func frozenAuthorizationHints(t *testing.T, a *Application) client.WithWatch {
	t.Helper()

	var (
		nodes corev1.NodeList
		pods  corev1.PodList
	)

	for _, list := range []client.ObjectList{&nodes, &pods} {
		if err := a.Topology.List(t.Context(), list); err != nil {
			t.Fatal(err)
		}
	}

	return fake.NewClientBuilder().WithScheme(a.Topology.Scheme()).WithLists(&nodes, &pods).
		WithIndex(&corev1.Node{}, nodeUIDIndex, nodeUIDKeys).
		WithIndex(&corev1.Pod{}, authorizationPodIndex, authorizationPodKeys(a.Server.Config)).Build()
}

func TestAuthorizationStaleHintsRequireLiveFacts(t *testing.T) {
	for _, scenario := range []string{"success", "node deleted", "node recreated", "node excluded", "node terminating", "pod deleted", "pod recreated", "pod moved", "pod terminal", "pod terminating", "pod sa", "pod owner", "ds deleted", "ds recreated", "ds terminating", "sa deleted", "sa empty uid", "sa terminating", "API failure", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			a, _, _ := authFixture(t)
			hints := frozenAuthorizationHints(t, a)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			if scenario == "canceled" {
				cancel()
			}

			reads := 0
			live := interceptor.NewClient(a.Topology.Client.(client.WithWatch), interceptor.Funcs{
				List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					t.Fatal("authorization attempted a live list")
					return wire.Unavailable
				},
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					reads++

					if scenario == "API failure" {
						return errors.New("unreachable API")
					}

					if err := c.Get(ctx, key, obj, opts...); err != nil {
						return err
					}

					now := metav1.Now()

					switch v := obj.(type) {
					case *corev1.Node:
						switch scenario {
						case "node deleted":
							return apierrors.NewNotFound(corev1.Resource("nodes"), key.Name)
						case "node recreated":
							v.UID = testOtherUID
						case "node excluded":
							v.Labels = map[string]string{wire.ExclusionLabel: ""}
						case "node terminating":
							v.DeletionTimestamp = &now
						}
					case *corev1.Pod:
						switch scenario {
						case "pod deleted":
							return apierrors.NewNotFound(corev1.Resource("pods"), key.Name)
						case "pod recreated":
							v.UID = "replacement"
						case "pod moved":
							v.Spec.NodeName = "other"
						case "pod terminal":
							v.Status.Phase = corev1.PodFailed
						case "pod terminating":
							v.DeletionTimestamp = &now
						case "pod sa":
							v.Spec.ServiceAccountName = "other"
						case "pod owner":
							v.OwnerReferences[0].UID = "other"
						}
					case *appsv1.DaemonSet:
						switch scenario {
						case "ds deleted":
							return apierrors.NewNotFound(appsv1.Resource("daemonsets"), key.Name)
						case "ds recreated":
							v.UID = "replacement"
						case "ds terminating":
							v.DeletionTimestamp = &now
						}
					case *corev1.ServiceAccount:
						switch scenario {
						case "sa deleted":
							return apierrors.NewNotFound(corev1.Resource("serviceaccounts"), key.Name)
						case "sa empty uid":
							v.UID = ""
						case "sa terminating":
							v.DeletionTimestamp = &now
						}
					}

					return ctx.Err()
				},
			})
			err := authorizeNode(ctx, live, hints, a.Server.Config, wire.NodeID(testNodeUID))
			want := error(wire.Forbidden)

			switch scenario {
			case "success":
				want = nil
			case "API failure", "canceled":
				want = wire.Unavailable
			}

			if !errors.Is(err, want) || reads > 4 {
				t.Fatalf("authorization=%v want=%v reads=%d", err, want, reads)
			}
		})
	}
}

func TestAuthorizationHintMissesAndCandidateBound(t *testing.T) {
	for _, scenario := range []string{"node miss", "pod miss", "cache failure", "too many pods", "stale pods", "live rejected pods", "replacement converges"} {
		t.Run(scenario, func(t *testing.T) {
			a, _, _ := authFixture(t)
			hints := frozenAuthorizationHints(t, a)
			node := &corev1.Node{}
			pod := &corev1.Pod{}

			if err := hints.Get(t.Context(), client.ObjectKey{Name: "worker"}, node); err != nil {
				t.Fatal(err)
			}

			if err := hints.Get(t.Context(), client.ObjectKey{Namespace: "racer", Name: "worker-pod"}, pod); err != nil {
				t.Fatal(err)
			}

			switch scenario {
			case "node miss":
				if err := hints.Delete(t.Context(), node); err != nil {
					t.Fatal(err)
				}
			case "pod miss":
				if err := hints.Delete(t.Context(), pod); err != nil {
					t.Fatal(err)
				}
			case "too many pods", "stale pods", "live rejected pods":
				if err := hints.Delete(t.Context(), pod); err != nil {
					t.Fatal(err)
				}

				count := maxAuthorizationPods
				if scenario == "too many pods" {
					count++
				}

				for i := range count {
					p := pod.DeepCopy()

					p.Name, p.ResourceVersion = fmt.Sprintf("stale-%d", i), ""
					if err := hints.Create(t.Context(), p); err != nil {
						t.Fatal(err)
					}

					if scenario == "live rejected pods" {
						p.ResourceVersion = ""
						if err := a.Topology.Create(t.Context(), p); err != nil {
							t.Fatal(err)
						}
					}
				}
			case "replacement converges":
				pod.UID = "replacement"

				pod.ResourceVersion = ""
				if err := a.Topology.Update(t.Context(), pod); err != nil {
					t.Fatal(err)
				}

				if err := authorizeNode(t.Context(), a.Server.APIReader, hints, a.Server.Config, wire.NodeID(testNodeUID)); err != wire.Forbidden {
					t.Fatalf("stale Pod UID accepted: %v", err)
				}

				hints = frozenAuthorizationHints(t, a)
			}

			if scenario == "cache failure" {
				hints = interceptor.NewClient(hints, interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					return errors.New("cache unavailable")
				}})
			}

			reads := 0
			live := interceptor.NewClient(a.Topology.Client.(client.WithWatch), interceptor.Funcs{
				List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					t.Fatal("live list fallback")
					return wire.Unavailable
				},
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					reads++

					if _, ok := obj.(*corev1.ServiceAccount); ok && scenario == "live rejected pods" {
						return apierrors.NewNotFound(corev1.Resource("serviceaccounts"), key.Name)
					}

					return c.Get(ctx, key, obj, opts...)
				},
			})
			err := authorizeNode(t.Context(), live, hints, a.Server.Config, wire.NodeID(testNodeUID))
			want, budget := error(wire.Forbidden), 1

			switch scenario {
			case "node miss":
				budget = 0
			case "cache failure":
				want, budget = wire.Unavailable, 0
			case "too many pods":
				want = wire.Unavailable
			case "stale pods":
				budget = 1 + maxAuthorizationPods
			case "live rejected pods":
				budget = 1 + 3*maxAuthorizationPods
			case "replacement converges":
				want, budget = nil, 4
			}

			if !errors.Is(err, want) || reads != budget {
				t.Fatalf("authorization=%v want=%v reads=%d want=%d", err, want, reads, budget)
			}
		})
	}
}
