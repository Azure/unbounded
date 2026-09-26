// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/wire"
)

func capacityCaches(count int) []racerv1.ClusterCache {
	caches := make([]racerv1.ClusterCache, count)
	for i := range caches {
		caches[i] = catalogCache(fmt.Sprintf("capacity-%d", i), types.UID(fmt.Sprintf("%08x-0000-0000-0000-000000000000", i)), nil)
	}

	return caches
}

func TestCatalogCapacityBoundary(t *testing.T) {
	r, _ := testKeyring(t)
	runKeys(t, r)
	_, b, _, _ := keyState(t, r)

	capacity, err := catalogCapacity(r.Config, b)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("default admitted maximum: %d", capacity)
	// Check the conservative byte envelope independently: max generation, four
	// generations of both purposes, longest state, and maximum reserved roots.
	for _, count := range []int{capacity, capacity + 1} {
		keys := []map[string]any{}

		for _, cache := range capacityCaches(count) {
			for range 4 {
				for _, purpose := range []wire.KeyPurpose{wire.PageKey, wire.OriginCredentialsKey} {
					keys = append(keys, map[string]any{"cache": cache.UID, "id": make([]byte, 16), "purpose": purpose, "state": wire.RetiringKey, "material": make([]byte, 32)})
				}
			}
		}

		var out bytes.Buffer

		err := json.NewEncoder(&out).Encode(map[string]any{"schema_version": wire.SchemaVersion, "cluster": r.Config.Cluster, "generation": fmt.Sprint(uint64(math.MaxUint64)), "peer_trust_roots": [][]byte{make([]byte, reservedRootBytes), make([]byte, reservedRootBytes), make([]byte, reservedRootBytes), make([]byte, reservedRootBytes)}, "cache_keys": keys})
		if err != nil || (out.Len() <= wire.MaxBundleBytes) != (count == capacity) {
			t.Fatalf("capacity=%d count=%d bytes=%d: %v", capacity, count, out.Len(), err)
		}
	}
	// Even an empty catalog cannot make an unbounded number of roots fit. Reject
	// pathological policy before consuming the one-way initialization claim.
	r.Config.Rotation.Interval, r.Config.Rotation.PrepareFor = time.Nanosecond, time.Nanosecond
	if _, err := catalogCapacity(r.Config, b); !errors.Is(err, wire.TooLarge) {
		t.Fatalf("unbounded trust reserve: %v", err)
	}
}

func TestCatalogAdmissionRotationCycles(t *testing.T) {
	for _, policy := range []RotationPolicy{
		{Interval: 24 * time.Hour, PrepareFor: time.Hour, RetainFor: 48 * time.Hour},
		{Interval: 6 * time.Hour, PrepareFor: time.Hour, RetainFor: 48 * time.Hour},
		{Interval: 12 * time.Hour, PrepareFor: 12 * time.Hour, RetainFor: 48 * time.Hour},
		{Interval: 7 * 24 * time.Hour, PrepareFor: time.Hour, RetainFor: 24 * time.Hour},
	} {
		t.Run(fmt.Sprint(policy), func(t *testing.T) {
			r, now := testKeyring(t)
			r.Config.Rotation = policy
			runKeys(t, r)
			shared, b, _, _ := keyState(t, r)

			capacity, err := catalogCapacity(r.Config, b)
			if err != nil {
				t.Fatal(err)
			}

			for _, cache := range capacityCaches(capacity - 1) {
				if err := r.Create(t.Context(), &cache); err != nil {
					t.Fatal(err)
				}
			}
			// Cross a decimal-width boundary immediately and continue through
			// enough cycles to reach the steady-state retirement high watermark.
			b.Generation = 99

			shared.Data["bundle.json"], _ = wire.EncodeBundle(b)
			if err := r.Update(t.Context(), shared); err != nil {
				t.Fatal(err)
			}

			runKeys(t, r)

			maxRoots := 0

			for range 30 {
				_, before, previous, _ := keyState(t, r)
				*now = previous.NextTransition

				runKeys(t, r)
				_, after, state, _ := keyState(t, r)

				maxRoots = max(maxRoots, len(after.PeerTrustRoots))
				if len(keyedCaches(after)) != capacity || !r.Lifecycle.issuer || after.Generation <= before.Generation {
					t.Fatal("rotation at capacity lost admission, readiness, or progress")
				}

				for id, deadline := range previous.Retiring {
					if now.Before(deadline) && !state.Retiring[id].Equal(deadline) {
						t.Fatal("capacity shortened retirement")
					}
				}
			}

			if policy.Interval == 6*time.Hour && maxRoots < 8 {
				t.Fatalf("did not exercise multiple retiring generations: %d", maxRoots)
			}
		})
	}
}

func TestCatalogAdmissionGrowthRemovalAndRestart(t *testing.T) {
	r, _ := testKeyring(t)
	runKeys(t, r)
	a := Assemble(r.Config, r.Client, r.APIReader)
	first := reconcileTopology(t, a.Topology, t.Context())
	_, initial, _, _ := keyState(t, r)

	capacity, err := catalogCapacity(r.Config, initial)
	if err != nil {
		t.Fatal(err)
	}

	caches := capacityCaches(capacity + 1)
	for _, cache := range caches {
		if err := r.Create(t.Context(), &cache); err != nil {
			t.Fatal(err)
		}
	}

	if got := reconcileTopology(t, a.Topology, t.Context()); got != first {
		t.Fatal("published growth before its keys were committed")
	}

	var logs strings.Builder

	logger := funcr.New(func(_, msg string) { logs.WriteString(msg) }, funcr.Options{})

	ctx := ctrl.LoggerInto(t.Context(), logger)
	if _, err := r.Reconcile(ctx, ctrl.Request{}); err != nil || !r.Lifecycle.issuer {
		t.Fatalf("growth disabled healthy service: %v", err)
	}

	if !strings.Contains(logs.String(), "rotation_capacity") || !strings.Contains(logs.String(), caches[capacity].Name) {
		t.Fatal("capacity rejection was not observable")
	}

	_, admitted, _, _ := keyState(t, r)

	ids := keyedCaches(admitted)
	if !ids[wire.CacheID(testNodeUID)] || len(ids) != capacity || ids[wire.CacheID(caches[capacity-1].UID)] {
		t.Fatal("growth displaced established UID or ignored sorted free-slot order")
	}

	assertPublishedKeys(t, a.Topology, capacity)
	// Removing a rejected candidate must neither change keys nor consume a
	// publication sequence. A restart retains the admitted set from the Secret.
	before := reconcileTopology(t, a.Topology, t.Context())
	if err := r.Delete(t.Context(), &caches[capacity]); err != nil {
		t.Fatal(err)
	}

	r = Assemble(r.Config, r.Client, r.APIReader).Keyring
	runKeys(t, r)

	if after := reconcileTopology(t, a.Topology, t.Context()); after != before {
		t.Fatal("rejected deletion changed publication")
	}

	if err := r.Delete(t.Context(), &caches[0]); err != nil {
		t.Fatal(err)
	}

	assertPublishedKeys(t, a.Topology, capacity-1)
	runKeys(t, r)
	assertPublishedKeys(t, a.Topology, capacity)

	_, replaced, _, _ := keyState(t, r)
	if keyedCaches(replaced)[wire.CacheID(caches[0].UID)] || !keyedCaches(replaced)[wire.CacheID(caches[capacity-1].UID)] {
		t.Fatal("deletion did not admit next waiting UID")
	}
	// Missing/corrupt durable credentials remain fail-closed in both controllers.
	shared := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: r.Config.Namespace, Name: r.Config.KeyringSecretName}}
	if err := r.Delete(t.Context(), shared); err != nil {
		t.Fatal(err)
	}

	if _, err := a.Topology.Reconcile(t.Context(), ctrl.Request{}); err == nil {
		t.Fatal("missing credentials accepted by topology")
	}

	if _, err := a.Topology.Publications.Current(); !errors.Is(err, wire.Unavailable) {
		t.Fatalf("missing credentials did not suspend publication: %v", err)
	}
}

func assertPublishedKeys(t *testing.T, r *TopologyReconciler, count int) {
	t.Helper()
	p := reconcileTopology(t, r, t.Context())

	v, err := wire.DecodePublication(strings.NewReader(p.Encoding()))
	if err != nil || len(v.Caches) != count {
		t.Fatalf("published caches=%d, want %d: %v", len(v.Caches), count, err)
	}

	_, b, _, _ := keyState(t, Assemble(r.Config, r.Client, r.APIReader).Keyring)
	for _, cache := range v.Caches {
		if !keyedCaches(b)[cache.ID] {
			t.Fatal("published cache without both active keys")
		}
	}
}

func TestCatalogAdmissionDeterministicColdStart(t *testing.T) {
	r, _ := testKeyring(t)
	runKeys(t, r)
	_, b, _, _ := keyState(t, r)
	b.CacheKeys = nil

	capacity, err := catalogCapacity(r.Config, b)
	if err != nil {
		t.Fatal(err)
	}

	input := capacityCaches(capacity + 2)
	slices.Reverse(input)

	catalog, err := BuildCatalog(input)
	if err != nil {
		t.Fatal(err)
	}

	got, err := admitCatalog(context.Background(), r.Config, catalog, b)
	if err != nil || !slices.Equal(got, catalog[:capacity]) {
		t.Fatalf("cold admission is not a sorted UID prefix: %v", err)
	}
}

func TestCatalogCapacityRejectsBeforeInitializationClaim(t *testing.T) {
	r, _ := testKeyring(t)

	r.Config.Rotation.Interval, r.Config.Rotation.PrepareFor = time.Nanosecond, time.Nanosecond
	if _, err := r.Reconcile(t.Context(), ctrl.Request{}); !errors.Is(err, wire.TooLarge) {
		t.Fatalf("impossible root reserve: %v", err)
	}

	cm, _, err := (&TopologyReconciler{APIReader: r.APIReader, Config: r.Config}).readVersion(t.Context())
	if err != nil || cm.Annotations[credentialClaim] != "" {
		t.Fatalf("impossible policy consumed credential claim: %v", err)
	}
}

func TestCatalogAdmissionLegacyOvercommitDoesNotEvict(t *testing.T) {
	r, now := testKeyring(t)
	runKeys(t, r)
	shared, b, state, _ := keyState(t, r)

	capacity, err := catalogCapacity(r.Config, b)
	if err != nil {
		t.Fatal(err)
	}

	caches := capacityCaches(capacity)
	for _, cache := range caches {
		if err := r.Create(t.Context(), &cache); err != nil {
			t.Fatal(err)
		}
	}

	caches = append(caches, catalogCache("cache", testNodeUID, nil))

	catalog, err := BuildCatalog(caches)
	if err != nil {
		t.Fatal(err)
	}
	// Model the older controller's active-only admission without removing the
	// planner's independent final wire-size check.
	b, state, err = r.PlanRotation(b, state, catalog, *now)
	if err != nil {
		t.Fatal(err)
	}

	shared.Data["bundle.json"], _ = wire.EncodeBundle(b)

	shared.Data["rotation.json"], _ = json.Marshal(state)
	if err := r.Update(t.Context(), shared); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(t.Context(), ctrl.Request{}); !errors.Is(err, wire.TooLarge) || r.Lifecycle.issuer {
		t.Fatalf("legacy overcommit silently accepted: %v", err)
	}

	after, preserved, _, _ := keyState(t, r)
	if after.ResourceVersion != shared.ResourceVersion || len(keyedCaches(preserved)) != capacity+1 {
		t.Fatal("legacy overcommit evicted durable credentials")
	}
}

func TestCatalogAdmissionSerializesPublicationAndPruning(t *testing.T) {
	r, _ := testKeyring(t)
	runKeys(t, r)
	a := Assemble(r.Config, r.Client, r.APIReader)
	read := make(chan struct{})
	proceed := make(chan struct{})
	a.Topology.APIReader = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			err := c.Get(ctx, key, obj, opts...)
			if key.Name == r.Config.KeyringSecretName {
				close(read)
				<-proceed
			}

			return err
		},
	})
	done := make(chan error, 1)

	go func() {
		_, err := a.Topology.Reconcile(t.Context(), ctrl.Request{})
		done <- err
	}()

	<-read
	// The keyring must be excluded for the entire read/commit/install window.
	if a.Keyring.CatalogMu.TryLock() {
		a.Keyring.CatalogMu.Unlock()
		close(proceed)
		<-done
		t.Fatal("keyring can prune an in-progress topology candidate")
	}

	close(proceed)

	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if !a.Keyring.CatalogMu.TryLock() {
		t.Fatal("publication did not release admission gate")
	}
	a.Keyring.CatalogMu.Unlock()
}

func integrationCatalogCapacity(t *testing.T, c client.Client) {
	a := integrationInstallation(t, c, "catalog-capacity")
	// Use a root-heavy valid policy to exercise real admission with a small
	// catalog. Unit tests above run the default maximum through repeated cycles.
	a.Keyring.Config.Rotation = RotationPolicy{Interval: time.Hour, PrepareFor: time.Hour, RetainFor: 600 * time.Hour}

	a.Topology.Config = a.Keyring.Config
	if err := a.Topology.InitializeVersion(t.Context()); err != nil {
		t.Fatal(err)
	}

	runKeys(t, a.Keyring)
	_, b, _, _ := keyState(t, a.Keyring)

	capacity, err := catalogCapacity(a.Keyring.Config, b)
	if err != nil || capacity < 1 || capacity > 5 {
		t.Fatalf("integration capacity: %d %v", capacity, err)
	}

	for i := range capacity + 1 {
		cache := &racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("capacity-real-%d", i)}}
		if err := c.Create(t.Context(), cache); err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() {
			if err := c.Delete(context.Background(), cache); err != nil {
				t.Error(err)
			}
		})
	}

	assertPublishedKeys(t, a.Topology, 0)
	runKeys(t, a.Keyring)
	assertPublishedKeys(t, a.Topology, capacity)

	if !a.Lifecycle.issuer {
		t.Fatal("API-backed capacity rejection withdrew readiness")
	}
}
