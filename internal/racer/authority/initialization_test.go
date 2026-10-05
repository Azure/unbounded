// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func stagedTopology(t *testing.T) *TopologyReconciler {
	t.Helper()
	r := testTopology(t)

	marker, err := readInstallation(t.Context(), r.APIReader, r.Config, true)
	if err != nil {
		t.Fatal(err)
	}

	marker.Data[markerInitializationProtocol] = stagedInitialization
	if err := r.Update(t.Context(), marker); err != nil {
		t.Fatal(err)
	}
	// The fake client does not assign server UIDs. Model the API's identity and
	// immutable ConfigMap data rules, including metadata updates remaining legal.
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			obj.SetUID(types.UID(uuid.NewString()))
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if cm, ok := obj.(*corev1.ConfigMap); ok {
				old := &corev1.ConfigMap{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(cm), old); err != nil {
					return err
				}

				if old.Immutable != nil && *old.Immutable && (!reflect.DeepEqual(old.Data, cm.Data) || cm.Immutable == nil || !*cm.Immutable) {
					return errors.New("immutable data changed")
				}
			}

			return c.Update(ctx, obj, opts...)
		},
	})

	return r
}

// Each write boundary is tested both before persistence and with an uncertain
// successful response, including cancellation immediately after persistence.
func interruptInitialization(base client.WithWatch, boundary string, cancel context.CancelFunc) client.WithWatch {
	boom := errors.New("interrupted initialization")

	return interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if boundary == "before create" {
				return boom
			}

			if err := c.Create(ctx, obj, opts...); err != nil {
				return err
			}

			if boundary == "after create" {
				return boom
			}

			if boundary == "cancel create" {
				cancel()
			}

			return nil
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if boundary == "before commit" {
				return boom
			}

			if err := c.Update(ctx, obj, opts...); err != nil {
				return err
			}

			if boundary == "after commit" {
				return boom
			}

			if boundary == "cancel commit" {
				cancel()
			}

			return nil
		},
	})
}

func TestStagedInitializationEveryBoundary(t *testing.T) {
	for _, credentials := range []bool{false, true} {
		for _, boundary := range []string{"before create", "after create", "cancel create", "before commit", "after commit", "cancel commit"} {
			t.Run(fmt.Sprintf("credentials=%t/%s", credentials, boundary), func(t *testing.T) {
				r := stagedTopology(t)

				base := r.Client.(client.WithWatch)
				if credentials {
					if err := ensureInstalled(t.Context(), base, r.APIReader, r.Config); err != nil {
						t.Fatal(err)
					}
				}

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				writer := interruptInitialization(base, boundary, cancel)

				run := func(ctx context.Context, c client.WithWatch) error {
					if !credentials {
						return ensureInstalled(ctx, c, r.APIReader, r.Config)
					}

					_, err := Assemble(r.Config, c, r.APIReader).Keyring.Reconcile(ctx, ctrl.Request{})

					return err
				}
				if err := run(ctx, writer); err == nil {
					t.Fatal("interruption not observed")
				}

				var before client.Object = &corev1.ConfigMap{}

				name := r.Config.VersionConfigMapName
				if credentials {
					before, name = &corev1.Secret{}, r.Config.CredentialsSecretName
				}

				err := base.Get(t.Context(), client.ObjectKey{Namespace: r.Config.Namespace, Name: name}, before)
				if err != nil && !apierrors.IsNotFound(err) {
					t.Fatal(err)
				}

				if boundary != "after commit" && boundary != "cancel commit" {
					if credentials {
						if _, err := loadSigning(t.Context(), base, r.Config, Assemble(r.Config, base, base).Keyring.now()); err == nil {
							t.Fatal("uncommitted credentials usable")
						}
					} else if _, _, err := readVersion(t.Context(), base, r.Config); err == nil {
						t.Fatal("uncommitted version usable")
					}
				}

				if err := run(t.Context(), base); err != nil {
					t.Fatalf("restart: %v", err)
				}

				after := before.DeepCopyObject().(client.Object)
				if err := base.Get(t.Context(), client.ObjectKey{Namespace: r.Config.Namespace, Name: name}, after); err != nil {
					t.Fatal(err)
				}

				if before.GetUID() != "" && before.GetUID() != after.GetUID() {
					t.Fatal("recovery replaced staged material")
				}

				if secret, ok := before.(*corev1.Secret); ok && secret.UID != "" && !reflect.DeepEqual(secret.Data, after.(*corev1.Secret).Data) {
					t.Fatal("recovery changed exact secret material")
				}
			})
		}
	}
}

func TestStagedCommittedDeletionAndReplacementFailClosed(t *testing.T) {
	for _, credentials := range []bool{false, true} {
		for _, replace := range []bool{false, true} {
			t.Run(fmt.Sprintf("credentials=%t/replace=%t", credentials, replace), func(t *testing.T) {
				r := stagedTopology(t)

				base := r.Client.(client.WithWatch)
				if err := ensureInstalled(t.Context(), base, base, r.Config); err != nil {
					t.Fatal(err)
				}

				runKeys(t, Assemble(r.Config, base, base).Keyring)

				var obj client.Object = &corev1.ConfigMap{}

				name := r.Config.VersionConfigMapName
				if credentials {
					obj, name = &corev1.Secret{}, r.Config.CredentialsSecretName
				}

				if err := base.Get(t.Context(), client.ObjectKey{Namespace: r.Config.Namespace, Name: name}, obj); err != nil {
					t.Fatal(err)
				}

				if err := base.Delete(t.Context(), obj); err != nil {
					t.Fatal(err)
				}

				if replace {
					obj.SetResourceVersion("")

					if err := base.Create(t.Context(), obj); err != nil {
						t.Fatal(err)
					}
				}

				noWrites := interceptor.NewClient(base, interceptor.Funcs{
					Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
						t.Fatal("recreated committed state")
						return nil
					},
					Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
						t.Fatal("rewrote committed state")
						return nil
					},
				})
				if credentials {
					if _, err := Assemble(r.Config, noWrites, base).Keyring.Reconcile(t.Context(), ctrl.Request{}); err == nil {
						t.Fatal("lost credentials accepted")
					}
				} else if err := ensureInstalled(t.Context(), noWrites, base, r.Config); err == nil {
					t.Fatal("lost version accepted")
				}
			})
		}
	}
}

func TestStagedCompetingInstallers(t *testing.T) {
	r := stagedTopology(t)
	base := r.Client.(client.WithWatch)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := ensureInstalled(t.Context(), base, base, r.Config); err != nil {
				t.Error(err)
			}
		})
	}

	wg.Wait()

	for range 8 {
		wg.Go(func() {
			if _, err := Assemble(r.Config, base, base).Keyring.Reconcile(t.Context(), ctrl.Request{}); err != nil {
				t.Error(err)
			}
		})
	}

	wg.Wait()
	runKeys(t, Assemble(r.Config, base, base).Keyring)
}

func TestStagedDelayedCreateCannotResurrectAuthority(t *testing.T) {
	for _, credentials := range []bool{false, true} {
		t.Run(fmt.Sprint(credentials), func(t *testing.T) {
			r := stagedTopology(t)

			base := r.Client.(client.WithWatch)
			if credentials {
				if err := ensureInstalled(t.Context(), base, base, r.Config); err != nil {
					t.Fatal(err)
				}
			}

			writer := interceptor.NewClient(base, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				// Let a competitor fully commit, then lose its resource, while this
				// installer is paused after its NotFound but before its Create.
				if credentials {
					runKeys(t, Assemble(r.Config, base, base).Keyring)
				} else if err := ensureInstalled(ctx, base, base, r.Config); err != nil {
					t.Fatal(err)
				}

				if err := base.Delete(ctx, obj); err != nil {
					t.Fatal(err)
				}

				return c.Create(ctx, obj, opts...)
			}})
			if credentials {
				_, _ = Assemble(r.Config, writer, base).Keyring.Reconcile(t.Context(), ctrl.Request{})
				if _, err := Assemble(r.Config, base, base).Keyring.Reconcile(t.Context(), ctrl.Request{}); err == nil {
					t.Fatal("delayed Create restored credentials authority")
				}
			} else {
				if err := ensureInstalled(t.Context(), writer, base, r.Config); err == nil {
					t.Fatal("delayed Create restored version authority")
				}

				if _, _, err := readVersion(t.Context(), base, r.Config); err == nil {
					t.Fatal("orphan version accepted")
				}
			}
		})
	}
}

func TestStagedRejectsUnboundOrCorruptCandidates(t *testing.T) {
	for _, credentials := range []bool{false, true} {
		for _, corruption := range []string{"binding", "protocol", "immutable", "data"} {
			t.Run(fmt.Sprintf("credentials=%t/%s", credentials, corruption), func(t *testing.T) {
				r := stagedTopology(t)

				base := r.Client.(client.WithWatch)
				if credentials {
					if err := ensureInstalled(t.Context(), base, base, r.Config); err != nil {
						t.Fatal(err)
					}
				}

				writer := interruptInitialization(base, "after create", func() {})

				var obj client.Object = &corev1.ConfigMap{}

				name := r.Config.VersionConfigMapName
				if credentials {
					_, _ = Assemble(r.Config, writer, base).Keyring.Reconcile(t.Context(), ctrl.Request{})
					obj, name = &corev1.Secret{}, r.Config.CredentialsSecretName
				} else {
					_ = ensureInstalled(t.Context(), writer, base, r.Config)
				}

				if err := base.Get(t.Context(), client.ObjectKey{Namespace: r.Config.Namespace, Name: name}, obj); err != nil {
					t.Fatal(err)
				}

				switch corruption {
				case "binding":
					obj.GetAnnotations()[installationUIDAnnotation] = "foreign"
				case "protocol":
					delete(obj.GetAnnotations(), initializationProtocol)
				case "immutable":
					immutable := true
					if credentials {
						obj.(*corev1.Secret).Immutable = &immutable
					} else {
						obj.(*corev1.ConfigMap).Immutable = &immutable
					}
				case "data":
					if credentials {
						delete(obj.(*corev1.Secret).Data, "issuer.json")
					} else {
						obj.(*corev1.ConfigMap).Data["sequence"] = "2"
					}
				}

				if err := base.Update(t.Context(), obj); err != nil {
					t.Fatal(err)
				}

				if credentials {
					if _, err := Assemble(r.Config, base, base).Keyring.Reconcile(t.Context(), ctrl.Request{}); err == nil {
						t.Fatal("invalid candidate committed")
					}

					version, _, err := readVersion(t.Context(), base, r.Config)
					if err != nil || version.Annotations[credentialClaim] != "" {
						t.Fatal("invalid candidate consumed claim")
					}
				} else {
					if err := ensureInstalled(t.Context(), base, base, r.Config); err == nil {
						t.Fatal("invalid candidate committed")
					}

					if _, err := readInstallation(t.Context(), base, r.Config, true); err != nil {
						t.Fatal("invalid candidate consumed marker")
					}
				}
			})
		}
	}
}

func integrationStagedInitialization(t *testing.T, c client.WithWatch) {
	for _, credentials := range []bool{false, true} {
		for i, boundary := range []string{"before create", "after create", "cancel create", "before commit", "after commit", "cancel commit"} {
			t.Run(fmt.Sprintf("credentials=%t/%s", credentials, boundary), func(t *testing.T) {
				a := integrationInstallation(t, c, fmt.Sprintf("staged-%t-%d", credentials, i))
				cfg := a.Topology.Config

				marker, err := readInstallation(t.Context(), c, cfg, true)
				if err != nil {
					t.Fatal(err)
				}

				marker.Data[markerInitializationProtocol] = stagedInitialization
				if err := c.Update(t.Context(), marker); err != nil {
					t.Fatal(err)
				}

				if credentials {
					if err := ensureInstalled(t.Context(), c, c, cfg); err != nil {
						t.Fatal(err)
					}
				}

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				writer := interruptInitialization(c, boundary, cancel)
				if credentials {
					if _, err := Assemble(cfg, writer, c).Keyring.Reconcile(ctx, ctrl.Request{}); err == nil {
						t.Fatal("interruption not injected")
					}

					runKeys(t, Assemble(cfg, c, c).Keyring)
				} else {
					if err := ensureInstalled(ctx, writer, c, cfg); err == nil {
						t.Fatal("interruption not injected")
					}

					if err := ensureInstalled(t.Context(), c, c, cfg); err != nil {
						t.Fatal(err)
					}
				}

				marker, err = readInstallation(t.Context(), c, cfg, false)
				if err != nil {
					t.Fatal(err)
				}

				marker.Data[versionUID] = "replacement"
				if err := c.Update(t.Context(), marker); err == nil {
					t.Fatal("API allowed rewriting immutable binding")
				}
			})
		}
	}
}
