// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestKeyringRotationCrashRecovery(t *testing.T) {
	for _, phase := range []string{"stage", "activate", "prune"} {
		for _, failure := range []string{"before issuer", "after issuer", "before bundle", "after bundle"} {
			t.Run(phase+"/"+failure, func(t *testing.T) {
				r, now := testKeyring(t)
				runKeys(t, r)
				_, _, initial, _ := keyState(t, r)
				*now = initial.NextRotation

				if phase != "stage" {
					runKeys(t, r)
					_, _, staged, _ := keyState(t, r)
					*now = staged.ActivateAt

					if phase == "prune" {
						runKeys(t, r)
						*now = now.Add(r.Config.Rotation.RetainFor)
					}
				}

				base := r.Client.(client.WithWatch)
				boom := errors.New("lost response")
				failed := false
				original, _, _, _ := keyState(t, r)
				r.Client = interceptor.NewClient(base, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if obj.GetName() != r.Config.CredentialsSecretName {
						t.Fatal("rotation wrote outside the credentials CAS")
					}

					if strings.HasPrefix(failure, "before ") && !failed {
						failed = true
						return boom
					}

					if err := c.Update(ctx, obj, opts...); err != nil {
						return err
					}

					if strings.HasPrefix(failure, "after ") && !failed {
						failed = true
						return boom
					}

					return nil
				}})

				_, err := r.Reconcile(context.Background(), ctrl.Request{})
				if !failed || !errors.Is(err, boom) || trustReady(r.Trust) {
					t.Fatalf("write failure accepted: %v", err)
				}
				// Former two-Secret boundaries now fail one coherent atomic version.
				committed, before, beforeState, _ := keyState(t, r)
				if strings.HasPrefix(failure, "before ") && !reflect.DeepEqual(original.Data, committed.Data) {
					t.Fatal("failed CAS partially changed credentials")
				}

				recovered := Assemble(r.Config, base, base).Keyring
				recovered.Now = r.Now
				runKeys(t, recovered)

				_, after, afterState, _ := keyState(t, recovered)
				if after.Generation < before.Generation {
					t.Fatal("generation reset")
				}

				if strings.HasPrefix(failure, "after ") && (after.Generation != before.Generation || !reflect.DeepEqual(afterState, beforeState)) {
					t.Fatal("committed atomic publication replaced on recovery")
				}

				if !beforeState.ActivateAt.IsZero() && !afterState.ActivateAt.IsZero() && !beforeState.ActivateAt.Equal(afterState.ActivateAt) {
					t.Fatal("committed preparation restarted")
				}
			})
		}
	}
}

func TestKeyringPrivatePruneRecovery(t *testing.T) {
	for _, afterWrite := range []bool{false, true} {
		t.Run(fmt.Sprintf("response-lost=%t", afterWrite), func(t *testing.T) {
			r, now := testKeyring(t)
			r.Config.Rotation.Interval = 7 * 24 * time.Hour
			runKeys(t, r)
			_, _, initial, _ := keyState(t, r)
			*now = initial.NextRotation

			runKeys(t, r)
			_, _, staged, _ := keyState(t, r)
			*now = staged.ActivateAt

			runKeys(t, r)
			_, _, active, _ := keyState(t, r)
			*now = active.Retiring[initial.ActiveIssuer]
			base := r.Client.(client.WithWatch)
			boom := errors.New("private prune interrupted")

			r.Client = interceptor.NewClient(base, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if obj.GetName() != r.Config.CredentialsSecretName {
					return c.Update(ctx, obj, opts...)
				}

				_, b, _, _ := keyState(t, r)
				if !containsRoot(b, initial.ActiveIssuer) {
					t.Fatal("root changed before atomic pruning")
				}

				if afterWrite {
					if err := c.Update(ctx, obj, opts...); err != nil {
						return err
					}
				}

				return boom
			}})
			if _, err := r.Reconcile(context.Background(), ctrl.Request{}); !errors.Is(err, boom) {
				t.Fatalf("prune interruption: %v", err)
			}

			_, before, _, beforeMaterial := keyState(t, r)
			if containsRoot(before, initial.ActiveIssuer) == afterWrite || (len(beforeMaterial.Keys) == 2) == afterWrite {
				t.Fatal("root and private key cleanup were not atomic")
			}

			r.Client = base
			runKeys(t, r)

			_, after, _, material := keyState(t, r)

			wantGeneration := before.Generation
			if !afterWrite {
				wantGeneration++
			}

			if wantGeneration != after.Generation || len(material.Keys) != 1 || containsRoot(after, initial.ActiveIssuer) {
				t.Fatal("prune recovery changed publication or retained private material")
			}
		})
	}
}

func TestKeyringBundleConflictKeepsPendingIssuer(t *testing.T) {
	r, now := testKeyring(t)
	runKeys(t, r)
	_, _, initial, _ := keyState(t, r)
	*now = initial.NextRotation
	base := r.Client.(client.WithWatch)
	r.Client = interceptor.NewClient(base, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if obj.GetName() == r.Config.CredentialsSecretName {
			return apierrors.NewConflict(corev1.Resource("secrets"), obj.GetName(), wire.Conflict)
		}

		return c.Update(ctx, obj, opts...)
	}})

	result, err := r.Reconcile(context.Background(), ctrl.Request{})
	if err != nil || result.RequeueAfter != retryConflictDelay {
		t.Fatalf("bundle conflict: %v, %v", result, err)
	}

	_, b, _, material := keyState(t, r)
	if b.Generation != 1 || len(material.Keys) != 1 || !containsRoot(b, initial.ActiveIssuer) {
		t.Fatal("conflicting bundle became authoritative")
	}

	r.Client = base
	runKeys(t, r)

	_, b, state, _ := keyState(t, r)
	if state.PreparedIssuer == "" || state.PreparedIssuer == initial.ActiveIssuer || b.Generation != 2 {
		t.Fatal("conflict recovery did not publish a coherent preparation")
	}
}

func TestKeyringInitializationNeverResurrects(t *testing.T) {
	for _, failure := range []string{"before claim", "after claim", "before issuer", "after issuer", "before bundle", "after bundle"} {
		t.Run(failure, func(t *testing.T) {
			r, _ := testKeyring(t)
			base := r.Client.(client.WithWatch)
			boom := errors.New("crash")

			r.Client = interceptor.NewClient(base, interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if failure == "before claim" {
						return boom
					}

					if err := c.Update(ctx, obj, opts...); err != nil {
						return err
					}

					if failure == "after claim" {
						return boom
					}

					return nil
				},
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if failure == "before issuer" || failure == "before bundle" {
						return boom
					}

					if err := c.Create(ctx, obj, opts...); err != nil {
						return err
					}

					if failure == "after issuer" || failure == "after bundle" {
						return boom
					}

					return nil
				},
			})
			if _, err := r.Reconcile(context.Background(), ctrl.Request{}); !errors.Is(err, boom) {
				t.Fatalf("crash not injected: %v", err)
			}

			recovered := Assemble(r.Config, base, base).Keyring
			recovered.Now = r.Now

			_, err := recovered.Reconcile(context.Background(), ctrl.Request{})
			if failure == "before claim" || failure == "after issuer" || failure == "after bundle" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("incomplete initialization resurrected")
			}
		})
	}

	for _, lost := range []string{"issuer", "bundle", "both", "version", "marker"} {
		t.Run("lost "+lost, func(t *testing.T) {
			r, _ := testKeyring(t)
			issuer := testIssuer(r)
			runKeys(t, r)

			for _, name := range []string{r.Config.CredentialsSecretName} {
				if lost == "both" || lost == "issuer" || lost == "bundle" {
					if err := r.Delete(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: r.Config.Namespace, Name: name}}); err != nil {
						t.Fatal(err)
					}
				}
			}

			if lost == "version" || lost == "marker" {
				name := r.Config.VersionConfigMapName
				if lost == "marker" {
					name = r.Config.InstallationConfigMapName
				}

				if err := r.Delete(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: r.Config.Namespace, Name: name}}); err != nil {
					t.Fatal(err)
				}
			}

			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				t.Fatal("recreated established state")
				return nil
			}})
			if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err == nil || trustReady(r.Trust) {
				t.Fatal("lost state accepted")
			}

			if _, err := issuer.TrustRoots(context.Background()); err == nil {
				t.Fatal("lost state still trusted")
			}
		})
	}
}

func TestKeyringConflictCancellationAndAuthoritativeReads(t *testing.T) {
	for _, cancelAt := range []string{"none", "before", "issuer", "bundle"} {
		t.Run(cancelAt, func(t *testing.T) {
			r, now := testKeyring(t)
			runKeys(t, r)
			_, _, s, _ := keyState(t, r)
			*now = s.NextRotation

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			base := r.Client.(client.WithWatch)
			writes := 0
			r.Client = interceptor.NewClient(base, interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					t.Fatal("cached credential read")
					return nil
				},
				List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					t.Fatal("cached catalog read")
					return nil
				},
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					writes++

					if (cancelAt == "issuer" || cancelAt == "bundle") && obj.GetName() == r.Config.CredentialsSecretName {
						cancel()
					}

					if cancelAt == "none" {
						return apierrors.NewConflict(corev1.Resource("secrets"), obj.GetName(), wire.Conflict)
					}

					return c.Update(ctx, obj, opts...)
				},
			})

			if cancelAt == "before" {
				cancel()
			}

			result, err := r.Reconcile(ctx, ctrl.Request{})
			if cancelAt == "none" {
				if err != nil || result.RequeueAfter != retryConflictDelay {
					t.Fatalf("conflict not requeued: %v %v", result, err)
				}
			} else if !errors.Is(err, context.Canceled) || !errors.Is(err, reconcile.TerminalError(nil)) || result.RequeueAfter != 0 {
				t.Fatalf("cancellation retried: %v %v", result, err)
			}

			if cancelAt == "before" && writes != 0 || cancelAt == "issuer" && writes != 1 {
				t.Fatal("write after cancellation")
			}

			// Cancellation before admission observes no authority failure.
			if trustReady(r.Trust) != (cancelAt == "before") {
				t.Fatal("readiness did not reflect whether admission observed a failure")
			}

			r.Client = base
			runKeys(t, r)
		})
	}
}
