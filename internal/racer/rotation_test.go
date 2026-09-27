// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/wire"
)

func testKeyring(t *testing.T) (*KeyringReconciler, *time.Time) {
	t.Helper()

	cache := &racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: "cache", UID: types.UID(testNodeUID)}}
	topology := initializedTopology(t, cache)
	a := Assemble(topology.Config, topology.Client, topology.APIReader)
	now := time.Now().UTC().Truncate(time.Second)
	a.Keyring.Now = func() time.Time { return now }

	return a.Keyring, &now
}

func runKeys(t *testing.T, r *KeyringReconciler) ctrl.Result {
	t.Helper()

	result, err := r.Reconcile(context.Background(), ctrl.Request{})
	if err != nil || result.RequeueAfter <= 0 {
		t.Fatalf("reconcile: %v, %v", result, err)
	}

	return result
}

func keyState(t *testing.T, r *KeyringReconciler) (*corev1.Secret, wire.KeyringBundle, RotationState, issuerMaterial) {
	t.Helper()

	version, _, err := readVersion(context.Background(), r.APIReader, r.Config)
	if err != nil {
		t.Fatal(err)
	}

	credentials, err := readCredentials(context.Background(), r.APIReader, r.Config, version.Annotations[credentialClaim])
	if err != nil {
		t.Fatal(err)
	}

	return credentials.shared, credentials.bundle, credentials.rotation, credentials.material
}

func TestKeyringRotationLifecycle(t *testing.T) {
	r, now := testKeyring(t)
	result := runKeys(t, r)

	shared, initial, state, _ := keyState(t, r)
	if initial.Generation != 1 || len(initial.CacheKeys) != 2 || len(initial.PeerTrustRoots) != 1 || result.RequeueAfter != r.Config.Rotation.Interval || !r.Lifecycle.issuer {
		t.Fatal("initial credentials or readiness")
	}

	for _, k := range initial.CacheKeys {
		if k.State != wire.ActiveKey {
			t.Fatal("initial key not active")
		}

		if string(k.Key.ID[:4]) != "RKG1" || binary.BigEndian.Uint64(k.Key.ID[4:12]) != 1 {
			t.Fatal("initial key must bind the first publication generation")
		}
	}
	// Repeated reconciliation must neither write nor consume a generation.
	runKeys(t, r)

	unchanged, _, _, _ := keyState(t, r)
	if shared.ResourceVersion != unchanged.ResourceVersion {
		t.Fatal("unchanged credentials written")
	}

	*now = state.NextRotation
	result = runKeys(t, r)

	_, staged, prepared, _ := keyState(t, r)
	if staged.Generation != 2 || len(staged.CacheKeys) != 4 || len(staged.PeerTrustRoots) != 2 || prepared.ActiveIssuer != state.ActiveIssuer || prepared.PreparedIssuer == "" || result.RequeueAfter != r.Config.Rotation.PrepareFor {
		t.Fatal("replacement not staged")
	}

	for _, k := range staged.CacheKeys {
		want := uint64(1)
		if k.State == wire.PreparedKey {
			want = 2
		}

		if string(k.Key.ID[:4]) != "RKG1" || binary.BigEndian.Uint64(k.Key.ID[4:12]) != want {
			t.Fatal("rotation changed an existing epoch or failed to bind the next generation")
		}
	}

	if !reflect.DeepEqual(staged.CacheKeys[:len(initial.CacheKeys)], initial.CacheKeys) {
		t.Fatal("staging changed retained keys")
	}
	// A fresh process resumes the persisted deadline, not a new delay.
	restarted := Assemble(r.Config, r.Client, r.APIReader).Keyring
	restarted.Now = r.Now

	*now = prepared.ActivateAt.Add(-time.Second)
	if got := runKeys(t, restarted).RequeueAfter; got != time.Second {
		t.Fatalf("lost activation deadline: %v", got)
	}

	*now = prepared.ActivateAt

	runKeys(t, restarted)

	_, activated, active, _ := keyState(t, r)
	if activated.Generation != 3 || active.ActiveIssuer != prepared.PreparedIssuer || active.PreparedIssuer != "" || len(active.Retiring) != 3 {
		t.Fatal("activation/overlap incorrect")
	}

	for i, k := range activated.CacheKeys {
		if k.State == wire.PreparedKey {
			t.Fatal("prepared key not activated")
		}

		if !reflect.DeepEqual(k.Key, staged.CacheKeys[i].Key) || !k.EqualMaterial(staged.CacheKeys[i]) {
			t.Fatal("activation changed key identity or material")
		}
	}
	// Further cycles overlap without evicting an earlier retirement prematurely.
	*now = active.NextRotation

	runKeys(t, restarted)
	_, _, nextStage, _ := keyState(t, r)
	*now = nextStage.ActivateAt

	runKeys(t, restarted)

	_, overlap, overlapping, _ := keyState(t, r)
	if len(overlap.PeerTrustRoots) != 3 || len(overlap.CacheKeys) != 6 {
		t.Fatal("multiple retiring generations lost")
	}

	*now = active.Retiring[state.ActiveIssuer]

	runKeys(t, restarted)

	_, pruned, prunedState, material := keyState(t, r)
	if containsRoot(pruned, state.ActiveIssuer) || len(pruned.CacheKeys) != 4 || len(material.Keys) != 2 || !prunedState.Retiring[active.ActiveIssuer].Equal(overlapping.Retiring[active.ActiveIssuer]) {
		t.Fatal("retirement pruning/reset")
	}
	// Topology CAS preserves the one-way initialization claim.
	topology := &TopologyReconciler{Client: r.Client, APIReader: r.APIReader, Config: r.Config, Publications: NewPublications(), Accepted: make(AcceptedMembers)}
	reconcileTopology(t, topology, context.Background())
	keyState(t, r)
}

func TestGenerationBoundCacheKey(t *testing.T) {
	if _, err := newCacheKey(wire.CacheID(testNodeUID), wire.PageKey, wire.ActiveKey, 0); err == nil {
		t.Fatal("zero or wrapped generation accepted")
	}

	for _, generation := range []wire.Generation{1, 256, math.MaxUint64} {
		key, err := newCacheKey(wire.CacheID(testNodeUID), wire.PageKey, wire.PreparedKey, generation)
		if err != nil {
			t.Fatal(err)
		}

		if len(key.Key.ID) != 16 || string(key.Key.ID[:4]) != "RKG1" || binary.BigEndian.Uint64(key.Key.ID[4:12]) != uint64(generation) {
			t.Fatalf("creation generation not preserved: %x", key.Key.ID)
		}
	}
}

func TestPlanRotationExhaustedKeyCreation(t *testing.T) {
	r, now := testKeyring(t)
	runKeys(t, r)
	_, b, state, _ := keyState(t, r)
	b.Generation = math.MaxUint64
	catalog := []wire.CacheDefinition{{ID: wire.CacheID(testNodeUID)}}

	next, nextState, err := r.PlanRotation(b, state, catalog, *now)
	if err != nil || !reflect.DeepEqual(next, b) || !reflect.DeepEqual(nextState, state) {
		t.Fatalf("exhausted idle plan: %v", err)
	}

	catalog = append(catalog, wire.CacheDefinition{ID: wire.CacheID(testOtherUID)})
	if _, _, err := r.PlanRotation(b, state, catalog, *now); !errors.Is(err, wire.Unavailable) {
		t.Fatalf("exhausted admission plan: %v", err)
	}

	state.PreparedIssuer = state.ActiveIssuer
	if _, _, err := r.PlanRotation(b, state, catalog[:1], state.NextRotation); !errors.Is(err, wire.Unavailable) {
		t.Fatalf("exhausted staging plan: %v", err)
	}
}

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
				r.Client = interceptor.NewClient(base, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					kind := "issuer"
					if obj.GetName() == r.Config.KeyringSecretName {
						kind = "bundle"
					}

					if failure == "before "+kind && !failed {
						failed = true
						return boom
					}

					if err := c.Update(ctx, obj, opts...); err != nil {
						return err
					}

					if failure == "after "+kind && !failed {
						failed = true
						return boom
					}

					return nil
				}})

				_, err := r.Reconcile(context.Background(), ctrl.Request{})
				if failed && (!errors.Is(err, boom) || r.Lifecycle.issuer) {
					t.Fatalf("write failure accepted: %v", err)
				}
				// Every intermediate durable pair must still be structurally readable.
				_, before, beforeState, beforeMaterial := keyState(t, r)
				recovered := Assemble(r.Config, base, base).Keyring
				recovered.Now = r.Now
				runKeys(t, recovered)

				_, after, afterState, _ := keyState(t, recovered)
				if after.Generation < before.Generation {
					t.Fatal("generation reset")
				}

				if phase == "stage" && beforeMaterial.Pending != "" && afterState.PreparedIssuer != beforeMaterial.Pending {
					t.Fatal("pending private material replaced on recovery")
				}

				if !beforeState.ActivateAt.IsZero() && !afterState.ActivateAt.IsZero() && !beforeState.ActivateAt.Equal(afterState.ActivateAt) {
					t.Fatal("committed preparation restarted")
				}
			})
		}
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
					kind := "issuer"
					if obj.GetName() == r.Config.KeyringSecretName {
						kind = "bundle"
					}

					if failure == "before "+kind {
						return boom
					}

					if err := c.Create(ctx, obj, opts...); err != nil {
						return err
					}

					if failure == "after "+kind {
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
			if failure == "before claim" || failure == "after bundle" {
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

			for _, name := range []string{r.Config.IssuerSecretName, r.Config.KeyringSecretName} {
				if lost == "both" || lost == "issuer" && name == r.Config.IssuerSecretName || lost == "bundle" && name == r.Config.KeyringSecretName {
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
			if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err == nil || r.Lifecycle.issuer {
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

					if cancelAt == "issuer" && obj.GetName() == r.Config.IssuerSecretName || cancelAt == "bundle" && obj.GetName() == r.Config.KeyringSecretName {
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
			if r.Lifecycle.issuer != (cancelAt == "before") {
				t.Fatal("readiness did not reflect whether admission observed a failure")
			}

			r.Client = base
			runKeys(t, r)
		})
	}
}

func TestKeyringCatalogAndBounds(t *testing.T) {
	r, now := testKeyring(t)
	runKeys(t, r)
	shared, original, s, _ := keyState(t, r)
	// Active-only fits, but staging the same catalog must account for overlap.
	var catalog []wire.CacheDefinition
	for n := range 800 {
		catalog = append(catalog, wire.CacheDefinition{ID: wire.CacheID(fmt.Sprintf("%08x-0000-0000-0000-000000000000", n+1))})
	}

	b, state, err := r.PlanRotation(original, s, catalog, *now)
	if err != nil {
		t.Fatal(err)
	}

	state.PreparedIssuer = state.ActiveIssuer
	if _, _, err := r.PlanRotation(b, state, catalog, state.NextRotation); !errors.Is(err, wire.TooLarge) {
		t.Fatalf("overlap bound not enforced: %v", err)
	}

	encoded, err := wire.EncodeBundle(original)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(encoded, shared.Data["bundle.json"]) {
		t.Fatal("planner mutated input")
	}

	cache := &racerv1.ClusterCache{}
	if err := r.APIReader.Get(context.Background(), client.ObjectKey{Name: "cache"}, cache); err != nil {
		t.Fatal(err)
	}

	if err := r.Delete(context.Background(), cache); err != nil {
		t.Fatal(err)
	}

	runKeys(t, r)

	_, removed, _, _ := keyState(t, r)
	if len(removed.CacheKeys) != 0 || removed.Generation != 2 {
		t.Fatal("removed cache keys retained")
	}

	cache.ResourceVersion = ""

	cache.UID = types.UID(testOtherUID)
	if err := r.Create(context.Background(), cache); err != nil {
		t.Fatal(err)
	}

	runKeys(t, r)

	_, recreated, _, _ := keyState(t, r)
	for _, key := range recreated.CacheKeys {
		if key.Key.Cache != wire.CacheID(testOtherUID) || key.EqualMaterial(original.CacheKeys[0]) {
			t.Fatal("recreated cache inherited keys")
		}
	}
}

func TestPlanRotationOwnsOutput(t *testing.T) {
	r, now := testKeyring(t)
	runKeys(t, r)
	_, _, initial, _ := keyState(t, r)
	*now = initial.NextRotation

	runKeys(t, r)
	_, _, prepared, _ := keyState(t, r)
	*now = prepared.ActivateAt

	runKeys(t, r)
	_, original, state, _ := keyState(t, r)
	catalog := []wire.CacheDefinition{{ID: wire.CacheID(testNodeUID)}}
	// The old codec round trip preserves order, even when it is not sorted.
	original.PeerTrustRoots[0], original.PeerTrustRoots[1] = original.PeerTrustRoots[1], original.PeerTrustRoots[0]
	original.CacheKeys[0], original.CacheKeys[1] = original.CacheKeys[1], original.CacheKeys[0]

	before, err := wire.EncodeBundle(original)
	if err != nil {
		t.Fatal(err)
	}

	beforeState, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}

	want, err := wire.DecodeBundle(bytes.NewReader(before))
	if err != nil {
		t.Fatal(err)
	}

	next, nextState, err := r.PlanRotation(original, state, catalog, *now)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(next, want) || !reflect.DeepEqual(nextState, state) {
		t.Fatal("idle planner changed bundle representation or rotation state")
	}

	// Exercise both outer collections and nested bytes, plus the retirement map.
	next.PeerTrustRoots[0][0] ^= 0xff
	next.PeerTrustRoots[1] = nil
	next.CacheKeys[0].Key.ID[0] ^= 0xff
	next.CacheKeys[1].State = wire.PreparedKey

	for id := range nextState.Retiring {
		delete(nextState.Retiring, id)
	}

	after, err := wire.EncodeBundle(original)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("output aliases input bundle: %v", err)
	}

	afterState, err := json.Marshal(state)
	if err != nil || !bytes.Equal(beforeState, afterState) {
		t.Fatalf("output aliases input retirement map: %v", err)
	}
}

func TestPlanRotationInputValidation(t *testing.T) {
	r, now := testKeyring(t)
	runKeys(t, r)

	_, original, state, _ := keyState(t, r)
	for _, tc := range []struct {
		name string
		edit func(*wire.KeyringBundle)
		want error
	}{
		{"schema", func(b *wire.KeyringBundle) { b.SchemaVersion++ }, wire.UnsupportedVersion},
		{"generation", func(b *wire.KeyringBundle) { b.Generation = 0 }, wire.InvalidRequest},
		{"duplicate root", func(b *wire.KeyringBundle) { b.PeerTrustRoots = append(b.PeerTrustRoots, b.PeerTrustRoots[0]) }, wire.InvalidRequest},
		{"invalid key", func(b *wire.KeyringBundle) {
			b.CacheKeys = append([]wire.CacheKey(nil), b.CacheKeys...)
			b.CacheKeys[0].Key.ID = nil
		}, wire.InvalidRequest},
		{"encoded size", func(b *wire.KeyringBundle) {
			key := b.CacheKeys[0]

			b.CacheKeys = make([]wire.CacheKey, 4000)
			for i := range b.CacheKeys {
				b.CacheKeys[i] = key
				b.CacheKeys[i].Key.Cache = wire.CacheID(fmt.Sprintf("%08x-0000-0000-0000-000000000000", i+1))
			}
		}, wire.TooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := original
			tc.edit(&b)
			// An empty catalog would discard all keys; invalid/oversized input
			// must still fail before the planner can shrink it into a valid output.
			if _, _, err := r.PlanRotation(b, state, nil, *now); !errors.Is(err, tc.want) {
				t.Fatalf("input validation: got %v, want %v", err, tc.want)
			}
		})
	}

	original.CacheKeys = nil
	state.Retiring = nil

	next, nextState, err := r.PlanRotation(original, state, nil, *now)
	if err != nil || next.CacheKeys == nil || nextState.Retiring == nil {
		t.Fatalf("empty collection normalization: %v", err)
	}
}

func TestKeyringCorruptionAndGenerationExhaustion(t *testing.T) {
	for _, corrupt := range []string{"timestamp", "transition mismatch", "zero root retirement", "zero key retirement", "unknown retirement", "active issuer", "bundle", "private key", "generation", "binding"} {
		t.Run(corrupt, func(t *testing.T) {
			r, now := testKeyring(t)
			runKeys(t, r)

			switch corrupt {
			case "zero root retirement", "zero key retirement", "unknown retirement":
				_, _, initial, _ := keyState(t, r)
				*now = initial.NextRotation

				runKeys(t, r)
				_, _, prepared, _ := keyState(t, r)
				*now = prepared.ActivateAt

				runKeys(t, r)
			}

			shared, b, s, _ := keyState(t, r)

			switch corrupt {
			case "timestamp":
				s.NextTransition = time.Time{}
				shared.Data["rotation.json"], _ = json.Marshal(s)
			case "transition mismatch":
				s.NextTransition = s.NextTransition.Add(time.Second)
				shared.Data["rotation.json"], _ = json.Marshal(s)
			case "zero root retirement":
				for _, root := range b.PeerTrustRoots {
					if id := rootID(root); id != s.ActiveIssuer {
						s.Retiring[id] = time.Time{}
					}
				}

				shared.Data["rotation.json"], _ = json.Marshal(s)
			case "zero key retirement":
				for _, key := range b.CacheKeys {
					if key.State == wire.RetiringKey {
						s.Retiring[keyID(key)] = time.Time{}
						break
					}
				}

				shared.Data["rotation.json"], _ = json.Marshal(s)
			case "unknown retirement":
				s.Retiring["unknown"] = s.NextTransition.Add(time.Hour)
				shared.Data["rotation.json"], _ = json.Marshal(s)
			case "active issuer":
				s.ActiveIssuer = "missing"
				shared.Data["rotation.json"], _ = json.Marshal(s)
			case "bundle":
				shared.Data["bundle.json"] = []byte("{}")
			case "generation":
				b.Generation = math.MaxUint64
				shared.Data["bundle.json"], _ = wire.EncodeBundle(b)
				*now = s.NextRotation
			case "binding":
				shared.Annotations[credentialClaim] = "foreign"
			case "private key":
				issuer := &corev1.Secret{}
				if err := r.APIReader.Get(context.Background(), client.ObjectKey{Namespace: r.Config.Namespace, Name: r.Config.IssuerSecretName}, issuer); err != nil {
					t.Fatal(err)
				}

				issuer.Data["issuer.json"] = []byte("{}")
				if err := r.Update(context.Background(), issuer); err != nil {
					t.Fatal(err)
				}
			}

			if err := r.Update(context.Background(), shared); err != nil {
				t.Fatal(err)
			}

			before := shared.DeepCopy()

			if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err == nil || r.Lifecycle.issuer {
				t.Fatal("corrupt state accepted")
			}

			after := &corev1.Secret{}
			if err := r.APIReader.Get(context.Background(), client.ObjectKeyFromObject(shared), after); err != nil {
				t.Fatal(err)
			}

			if !reflect.DeepEqual(before.Data, after.Data) {
				t.Fatal("corrupt state rewritten")
			}
		})
	}
}

func TestKeyringExhaustedGenerationTransitions(t *testing.T) {
	for _, transition := range []string{"idle", "admission", "stage", "empty stage", "activate", "remove", "prune"} {
		t.Run(transition, func(t *testing.T) {
			r, now := testKeyring(t)

			r.Config.Rotation.Interval = 7 * 24 * time.Hour
			if transition == "empty stage" {
				if err := r.Delete(t.Context(), &racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: "cache"}}); err != nil {
					t.Fatal(err)
				}
			}

			runKeys(t, r)

			_, _, initial, _ := keyState(t, r)
			if transition == "activate" || transition == "prune" {
				*now = initial.NextRotation

				runKeys(t, r)
				_, _, staged, _ := keyState(t, r)
				*now = staged.ActivateAt

				if transition == "prune" {
					runKeys(t, r)
					_, _, active, _ := keyState(t, r)
					*now = active.Retiring[initial.ActiveIssuer]
				}
			}

			shared, b, _, _ := keyState(t, r)
			b.Generation = math.MaxUint64

			var err error

			shared.Data["bundle.json"], err = wire.EncodeBundle(b)
			if err != nil {
				t.Fatal(err)
			}

			if err := r.Update(t.Context(), shared); err != nil {
				t.Fatal(err)
			}

			switch transition {
			case "admission":
				cache := &racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: "added", UID: types.UID(testOtherUID)}}
				if err := r.Create(t.Context(), cache); err != nil {
					t.Fatal(err)
				}
			case "stage", "empty stage":
				*now = initial.NextRotation
			case "remove":
				if err := r.Delete(t.Context(), &racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: "cache"}}); err != nil {
					t.Fatal(err)
				}
			}

			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
				t.Fatal("exhausted generation wrote durable state")
				return nil
			}})
			if transition == "idle" {
				runKeys(t, r)
			} else if _, err := r.Reconcile(t.Context(), ctrl.Request{}); !errors.Is(err, wire.Unavailable) {
				t.Fatalf("exhausted generation transition: %v", err)
			}

			after, preserved, _, _ := keyState(t, r)
			if after.ResourceVersion != shared.ResourceVersion || !reflect.DeepEqual(preserved, b) {
				t.Fatal("exhausted generation changed the published bundle")
			}
		})
	}
}

func TestKeyringAdmissionAtLastGeneration(t *testing.T) {
	r, _ := testKeyring(t)
	runKeys(t, r)
	shared, b, _, _ := keyState(t, r)
	b.Generation = math.MaxUint64 - 1

	var err error

	shared.Data["bundle.json"], err = wire.EncodeBundle(b)
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Update(t.Context(), shared); err != nil {
		t.Fatal(err)
	}

	cache := &racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: "added", UID: types.UID(testOtherUID)}}
	if err := r.Create(t.Context(), cache); err != nil {
		t.Fatal(err)
	}

	runKeys(t, r)

	_, admitted, _, _ := keyState(t, r)
	if admitted.Generation != math.MaxUint64 || len(admitted.CacheKeys) != 4 || !reflect.DeepEqual(admitted.CacheKeys[:2], b.CacheKeys) {
		t.Fatal("last generation admission lost existing keys or new scopes")
	}

	for _, key := range admitted.CacheKeys[2:] {
		if key.Key.Cache != wire.CacheID(testOtherUID) || key.State != wire.ActiveKey || binary.BigEndian.Uint64(key.Key.ID[4:12]) != math.MaxUint64 {
			t.Fatal("admitted key did not bind the last publication generation")
		}
	}

	runKeys(t, r)
}

func TestKeyringOversizedOverlapDoesNotWrite(t *testing.T) {
	r, now := testKeyring(t)
	runKeys(t, r)
	_, initial, _, _ := keyState(t, r)

	for n := range 800 {
		cache := &racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("cache-%d", n), UID: types.UID(fmt.Sprintf("%08x-0000-0000-0000-000000000000", n+1))}}
		if err := r.Create(context.Background(), cache); err != nil {
			t.Fatal(err)
		}
	}

	runKeys(t, r)
	shared, admitted, state, _ := keyState(t, r)

	capacity, err := catalogCapacity(r.Config, admitted)
	if err != nil || len(admitted.CacheKeys) != 2*capacity || capacity >= 801 || !r.Lifecycle.issuer {
		t.Fatalf("rotation capacity not enforced: %d, %v", capacity, err)
	}

	for _, key := range initial.CacheKeys {
		found := false
		for _, accepted := range admitted.CacheKeys {
			found = found || key.EqualMaterial(accepted)
		}

		if !found {
			t.Fatal("growth evicted an existing cache key")
		}
	}

	base := r.Client
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
		t.Fatal("rejected growth wrote durable state")
		return nil
	}})
	runKeys(t, r)

	unchanged, _, _, _ := keyState(t, r)
	if unchanged.ResourceVersion != shared.ResourceVersion {
		t.Fatal("rejected growth consumed generation")
	}

	r.Client = base
	*now = state.NextRotation

	runKeys(t, r)

	_, staged, _, _ := keyState(t, r)
	if len(staged.CacheKeys) != 4*capacity || !r.Lifecycle.issuer {
		t.Fatal("admitted catalog could not rotate")
	}
}

func TestKeyringEmptyCatalogAndCacheAddedDuringPreparation(t *testing.T) {
	r, now := testKeyring(t)

	cache := &racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: "cache"}}
	if err := r.Delete(context.Background(), cache); err != nil {
		t.Fatal(err)
	}

	runKeys(t, r)

	_, b, s, _ := keyState(t, r)
	if b.Generation != 1 || len(b.CacheKeys) != 0 {
		t.Fatal("empty catalog initialization has wrong generation or keys")
	}

	*now = s.NextRotation

	runKeys(t, r)
	_, _, staged, _ := keyState(t, r)

	cache.UID = types.UID(testNodeUID)
	if err := r.Create(context.Background(), cache); err != nil {
		t.Fatal(err)
	}

	runKeys(t, r)

	_, added, _, _ := keyState(t, r)
	if added.Generation != 3 || len(added.CacheKeys) != 2 {
		t.Fatal("new cache missing initial keys")
	}

	for _, key := range added.CacheKeys {
		if binary.BigEndian.Uint64(key.Key.ID[4:12]) != uint64(added.Generation) {
			t.Fatal("new cache key did not bind its admission generation")
		}
	}

	*now = staged.ActivateAt

	runKeys(t, r)

	_, activated, _, _ := keyState(t, r)
	for n, key := range activated.CacheKeys {
		if key.State != wire.ActiveKey || !reflect.DeepEqual(key.Key, added.CacheKeys[n].Key) || !key.EqualMaterial(added.CacheKeys[n]) {
			t.Fatal("new cache key retired without replacement")
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
				if obj.GetName() != r.Config.IssuerSecretName {
					return c.Update(ctx, obj, opts...)
				}

				_, b, _, _ := keyState(t, r)
				if containsRoot(b, initial.ActiveIssuer) {
					t.Fatal("private pruning preceded trust removal")
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

			_, before, _, _ := keyState(t, r)
			r.Client = base
			runKeys(t, r)

			_, after, _, material := keyState(t, r)
			if before.Generation != after.Generation || len(material.Keys) != 1 {
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
		if obj.GetName() == r.Config.KeyringSecretName {
			return apierrors.NewConflict(corev1.Resource("secrets"), obj.GetName(), wire.Conflict)
		}

		return c.Update(ctx, obj, opts...)
	}})

	result, err := r.Reconcile(context.Background(), ctrl.Request{})
	if err != nil || result.RequeueAfter != retryConflictDelay {
		t.Fatalf("bundle conflict: %v, %v", result, err)
	}

	_, b, _, material := keyState(t, r)
	if b.Generation != 1 || material.Pending == "" || containsRoot(b, material.Pending) {
		t.Fatal("conflicting bundle became authoritative")
	}

	r.Client = base
	runKeys(t, r)

	_, b, state, _ := keyState(t, r)
	if state.PreparedIssuer != material.Pending || b.Generation != 2 {
		t.Fatal("conflict regenerated pending issuer")
	}
}
