// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"context"
	"errors"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func bootstrapClient(t *testing.T) client.WithWatch {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	return fake.NewClientBuilder().WithScheme(scheme).Build()
}

func bootstrapManager(t *testing.T, c client.Client) *Manager {
	t.Helper()

	m, err := New(c, "racer", Options{})
	if err != nil {
		t.Fatal(err)
	}

	return m
}

func TestBootstrapWriteFailuresRecoverOnRestart(t *testing.T) {
	for _, stage := range []string{"private-state", "public-bundle", "commit-initialization"} {
		for _, applied := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/applied=%t", stage, applied), func(t *testing.T) {
				c := bootstrapClient(t)
				failure := errors.New("interrupted API response")
				injected := false
				write := func(ctx context.Context, obj client.Object, at string, apply func() error) error {
					// Public trust must never precede durable private state.
					if obj.GetName() == ConfigMapName {
						if err := c.Get(ctx, client.ObjectKey{Namespace: "racer", Name: SecretName}, &corev1.Secret{}); err != nil {
							t.Fatalf("publication preceded private state: %v", err)
						}
					}

					if at != stage {
						return apply()
					}

					injected = true

					if applied {
						if err := apply(); err != nil {
							return err
						}
					}

					return failure
				}
				broken := interceptor.NewClient(c, interceptor.Funcs{
					Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
						at := "private-state"
						if obj.GetName() == ConfigMapName {
							at = "public-bundle"
						}

						return write(ctx, obj, at, func() error { return c.Create(ctx, obj, opts...) })
					},
					Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
						return write(ctx, obj, "commit-initialization", func() error { return c.Update(ctx, obj, opts...) })
					},
				})

				m := bootstrapManager(t, broken)
				if err := m.AcquireLeadership(t.Context(), "interrupted"); !errors.Is(err, failure) || !injected {
					t.Fatalf("failure was not injected: %v", err)
				}

				csr, _ := csrKey(t)
				if _, err := m.Issue(t.Context(), csr, node("pod", "boot")); !errors.Is(err, ErrNotLeader) {
					t.Fatalf("failed initializer issued: %v", err)
				}

				var original string
				if _, s, err := m.readMetadata(t.Context()); err == nil {
					original = s.Active
				} else if !apierrors.IsNotFound(err) || stage != "private-state" || applied {
					t.Fatalf("unexpected private state after interruption: %v", err)
				}

				restarted := bootstrapManager(t, c)
				// Once the bundle is public, deployment state may appear before
				// the pending marker is cleared. Matching trust still recovers.
				if stage == "commit-initialization" {
					marker := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "racer-state", Namespace: "racer", Labels: map[string]string{"racer.unbounded-cloud.io/state": "commit"}}}
					if err := c.Create(t.Context(), marker); err != nil {
						t.Fatal(err)
					}
				}

				if err := restarted.AcquireLeadership(t.Context(), "restarted"); err != nil {
					t.Fatal(err)
				}

				if err := restarted.Load(t.Context()); err != nil {
					t.Fatal(err)
				}

				secret, s, err := restarted.readMetadata(t.Context())
				if err != nil {
					t.Fatal(err)
				}

				if original != "" && s.Active != original {
					t.Fatal("restart replaced the persisted CA")
				}

				if _, pending := secret.Annotations[bootstrapPending]; pending {
					t.Fatal("initialization was not committed")
				}

				if _, err := restarted.Issue(t.Context(), csr, node("pod", "boot")); err != nil {
					t.Fatalf("recovered leader cannot issue: %v", err)
				}
			})
		}
	}
}

func TestBootstrapConcurrentInitializersAndTakeover(t *testing.T) {
	for _, stage := range []string{"private-state", "public-bundle", "commit-initialization"} {
		t.Run(stage, func(t *testing.T) {
			c := bootstrapClient(t)
			next := bootstrapManager(t, c)
			interrupted := false
			takeover := func(at string) {
				if at != stage || interrupted {
					return
				}

				interrupted = true

				if err := next.AcquireLeadership(t.Context(), "next"); err != nil {
					t.Fatal(err)
				}
			}
			paused := interceptor.NewClient(c, interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if obj.GetName() == SecretName {
						takeover("private-state")
					} else {
						takeover("public-bundle")
					}

					return c.Create(ctx, obj, opts...)
				},
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if obj.GetName() == SecretName {
						takeover("commit-initialization")
					}

					return c.Update(ctx, obj, opts...)
				},
			})

			old := bootstrapManager(t, paused)
			if err := old.AcquireLeadership(t.Context(), "old"); err == nil || !interrupted {
				t.Fatalf("delayed initializer survived takeover: %v", err)
			}

			if err := next.Load(t.Context()); err != nil {
				t.Fatal(err)
			}

			if _, s, err := next.readMetadata(t.Context()); err != nil || s.Fence != "next" {
				t.Fatalf("takeover fence was overwritten: %v", err)
			}

			if err := old.Publish(t.Context()); !errors.Is(err, ErrNotLeader) {
				t.Fatalf("delayed initializer can publish: %v", err)
			}
		})
	}
}

func TestBootstrapDelayedInitializerCannotReplaceLostCA(t *testing.T) {
	for _, evidence := range []string{"public-bundle", "empty-tombstone", "commit", "replica"} {
		t.Run(evidence, func(t *testing.T) {
			c := bootstrapClient(t)
			paused := interceptor.NewClient(c, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetName() == SecretName {
					next := bootstrapManager(t, c)
					if err := next.AcquireLeadership(ctx, "next"); err != nil {
						t.Fatal(err)
					}

					if err := c.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: SecretName, Namespace: "racer"}}); err != nil {
						t.Fatal(err)
					}

					cm := &corev1.ConfigMap{}
					if err := c.Get(ctx, next.objectKey(ConfigMapName), cm); err != nil {
						t.Fatal(err)
					}

					switch evidence {
					case "empty-tombstone":
						cm.Data = nil
						if err := c.Update(ctx, cm); err != nil {
							t.Fatal(err)
						}
					case "commit", "replica":
						if err := c.Delete(ctx, cm); err != nil {
							t.Fatal(err)
						}

						marker := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "racer-state", Namespace: "racer", Labels: map[string]string{"racer.unbounded-cloud.io/state": "commit"}}}
						if evidence == "replica" {
							marker = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "racer-replica-old", Namespace: "racer"}, Data: map[string]string{"certificate": "old-cert"}}
						}

						if err := c.Create(ctx, marker); err != nil {
							t.Fatal(err)
						}
					}
				}

				return c.Create(ctx, obj, opts...)
			}})

			old := bootstrapManager(t, paused)
			if err := old.AcquireLeadership(t.Context(), "old"); !errors.Is(err, ErrLostState) {
				t.Fatalf("delayed initializer ignored lost CA: %v", err)
			}

			// The pending marker must retain this refusal across process restart.
			restarted := bootstrapManager(t, c)
			if err := restarted.AcquireLeadership(t.Context(), "restart"); !errors.Is(err, ErrLostState) {
				t.Fatalf("restart adopted a replacement trust domain: %v", err)
			}

			if err := old.Publish(t.Context()); !errors.Is(err, ErrNotLeader) {
				t.Fatalf("failed initializer published: %v", err)
			}
		})
	}
}

func TestBootstrapPendingPublicationStillDetectsCALoss(t *testing.T) {
	c := bootstrapClient(t)
	broken := interceptor.NewClient(c, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		return errors.New("crash before committing initialization")
	}})

	m := bootstrapManager(t, broken)
	if err := m.AcquireLeadership(t.Context(), "initial"); err == nil {
		t.Fatal("expected interrupted initialization")
	}

	if err := c.Delete(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: SecretName, Namespace: "racer"}}); err != nil {
		t.Fatal(err)
	}

	if err := bootstrapManager(t, c).AcquireLeadership(t.Context(), "restart"); !errors.Is(err, ErrLostState) {
		t.Fatalf("regenerated CA after initial publication: %v", err)
	}
}
