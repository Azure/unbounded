// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	machina "github.com/Azure/unbounded/api/machina/v1alpha3"
	netv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	"github.com/Azure/unbounded/internal/racer"
)

// Real resource-version CAS, ambiguous writes and history recovery.

func TestB14RealAPICAS(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real CAS")
	}

	environment := &envtest.Environment{}

	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	kube, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "state"}}); err != nil {
		t.Fatal(err)
	}

	for _, fault := range []string{"lost", "conflict", "no-commit", "lost-read", "create-lost"} {
		t.Run(fault, func(t *testing.T) {
			f := newCoordinationFixture(t, kube)
			if fault == "create-lost" {
				f.api.fault = "lost"
				f.call(t, 0, 503)
				f.call(t, 0, 200)
			} else {
				f.call(t, 0, 200)
				stale := f.durable(t)
				f.api.fault = fault
				f.call(t, 1, 503)

				if fault != "no-commit" {
					stale.Data["phase"] = "5"
					if err := kube.Update(ctx, stale); !apierrors.IsConflict(err) {
						t.Fatalf("real stale-RV CAS did not conflict: %v", err)
					}
				}

				if fault == "lost-read" {
					f.call(t, 1, 503)
					f.call(t, 1, 503)
				}

				got := f.call(t, 0, 200)
				if fault != "no-commit" && got.Phase != 2 {
					t.Fatal("durable receive not reloaded")
				}
			}

			f.call(t, 1, 200)

			if got := f.call(t, 2, 200); got.Phase != 3 {
				t.Fatal("receive did not progress")
			}

			if got := f.call(t, 3, 200); got.Phase != 4 {
				t.Fatal("activation did not progress")
			}

			f.call(t, 4, 200)

			if err := kube.Delete(ctx, f.durable(t)); err != nil {
				t.Fatal(err)
			}

			_, pointer, err := f.s.controlStore.load(ctx, "default")
			if err != nil {
				t.Fatal(err)
			}

			if err := kube.Delete(ctx, pointer); err != nil {
				t.Fatal(err)
			}
		})
	}

	for _, operation := range []string{"terminal", "gc"} {
		for _, fault := range []string{"lost", "lost-read", "no-commit", "history-conflict"} {
			t.Run("B13/"+operation+"/"+fault, func(t *testing.T) {
				historyFault(t, kube, operation, fault)

				for _, name := range []string{stateName("default"), stateName("default") + "-rollout"} {
					cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "state", Name: name}}
					if err := kube.Delete(ctx, cm); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}

	for _, test := range []struct {
		name string
		run  func(*testing.T, client.Client)
	}{
		{"review-repeated-boot", reviewRepeatedBoot}, {"review-capacity-reconcile", reviewCapacityReconcile},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.run(t, kube)

			for _, name := range []string{stateName("default"), stateName("default") + "-rollout"} {
				if err := kube.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "state", Name: name}}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// Informer-driven reconciliation and recovery.

// Exercise real API validation, informer events, optimistic Service patches,
// immutable chunks and recovery. Set KUBEBUILDER_ASSETS to run locally or in CI.
func TestControllerAPIIntegration(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS for Kubernetes API integration")
	}

	environment := &envtest.Environment{CRDDirectoryPaths: []string{"../../deploy/machina/crd"}, ErrorIfCRDPathMissing: true}

	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})

	scheme := runtime.NewScheme()

	_ = corev1.AddToScheme(scheme)
	if err := machina.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, name := range []string{"state", "ns"} {
		if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}

	n, p, s := fixtures()
	// Use an explicitly assigned, non-default Site and deliberately conflicting
	// legacy metadata. Neither enrollment nor event routing may use the mirror.
	const site = "rack-a"

	universe := racer.UniverseForSite(site)
	n.Labels[racer.SiteLabelKey] = site
	n.Labels[racer.UniverseKey] = "ignored-label"
	n.Annotations = map[string]string{racer.UniverseKey: "ignored-annotation"}
	p.Labels[racer.UniverseKey] = universe
	s.Annotations[racer.UniverseKey] = universe
	n.UID = ""
	desiredNodeStatus := n.Status

	n.Status = corev1.NodeStatus{}
	if err := c.Create(ctx, n); err != nil {
		t.Fatal(err)
	}

	n.Status = desiredNodeStatus
	if err := c.Status().Update(ctx, n); err != nil {
		t.Fatal(err)
	}

	p.Spec.Containers = []corev1.Container{{Name: "dataplane", Image: "example.invalid/racer:test"}}
	desiredPodStatus := p.Status

	p.Status = corev1.PodStatus{}
	if err := c.Create(ctx, p); err != nil {
		t.Fatal(err)
	}

	p.Status = desiredPodStatus
	if err := c.Status().Update(ctx, p); err != nil {
		t.Fatal(err)
	}

	s.Spec.ClusterIP = ""

	s.Annotations[originPortAnnotation] = "http"
	if err := c.Create(ctx, s); err != nil {
		t.Fatal(err)
	}

	origin := originFixture()
	origin.Spec.ClusterIP = ""
	origin.Spec.ClusterIPs = nil

	origin.Annotations = map[string]string{universeAnnotation: "separate-origin-universe"}
	if err := c.Create(ctx, origin); err != nil {
		t.Fatal(err)
	}

	server := &Server{}

	manager, err := ctrl.NewManager(config, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0"})
	if err != nil {
		t.Fatal(err)
	}

	if err := setupController(ctx, manager, server, "state", nil); err != nil {
		t.Fatal(err)
	}

	if err := setupStorageController(manager, server); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() { done <- manager.Start(ctx) }()

	defer func() {
		cancel()

		if err := <-done; err != nil {
			t.Error(err)
		}
	}()

	store := stateStore{client: c, namespace: "state"}
	await := func(check func(*generation) bool) *generation {
		t.Helper()

		var (
			last    *generation
			loadErr error
		)

		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			g, _, err := store.load(ctx, universe)

			last, loadErr = g, err
			if err == nil && g != nil && check(g) {
				return g
			}

			time.Sleep(50 * time.Millisecond)
		}

		t.Fatalf("controller failed to converge: generation=%+v load error=%v", last, loadErr)

		return nil
	}
	// Envtest has no dataplane to acknowledge prepare/receive/activate. Complete
	// each fixture rollout durably before requesting the next topology, as in the
	// origin reconciliation tests. Keep the real controller's barrier intact.
	completeRollout := func(g *generation) {
		t.Helper()

		index, err := indexGeneration(g)
		if err != nil {
			t.Fatal(err)
		}

		server.mu.Lock()
		defer server.mu.Unlock()

		rollout, err := server.rolloutFor(ctx, index)
		if err != nil {
			t.Fatal(err)
		}

		if err := server.persistPhase(ctx, universe, rollout, 4); err != nil {
			t.Fatal(err)
		}
	}
	g := await(func(g *generation) bool { return len(g.Owners) == 8 })
	// Drive the real Site informer and indexed Node fanout, not a manually
	// invoked storage reconciler. Convergence must precede the minute repair loop.
	awaitStorage := func(bytes int64) {
		t.Helper()

		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			server.mu.Lock()
			record := server.storagePolicies[identity("node", string(n.UID))]
			server.mu.Unlock()

			if record.DesiredBytes == bytes && record.ValidationError == "" {
				var cm corev1.ConfigMap
				if err := c.Get(ctx, client.ObjectKey{Namespace: "state", Name: "racer-storage-" + record.Node}, &cm); err != nil {
					t.Fatal(err)
				}

				var durable storagePolicyRecord
				if err := json.Unmarshal([]byte(cm.Data["policy"]), &durable); err != nil || durable != record {
					t.Fatalf("storage publication differs from durable policy: %+v %v", durable, err)
				}

				return
			}

			time.Sleep(20 * time.Millisecond)
		}

		t.Fatalf("storage informer did not publish %d bytes", bytes)
	}
	awaitStorage(racer.DefaultCacheSizeBytes)

	quantity := resource.MustParse("2Ti")

	storageSite := &machina.Site{ObjectMeta: metav1.ObjectMeta{Name: site}, Spec: machina.SiteSpec{
		NodeCidrs:          []string{"10.0.0.0/16"},
		PodCidrAssignments: []netv1alpha1.PodCidrAssignment{{CidrBlocks: []string{"10.244.0.0/16"}, NodeBlockSizes: &netv1alpha1.NodeBlockSizes{IPv4: 24, IPv6: 80}}},
		Components:         machina.SiteComponents{Racer: &machina.RacerComponentSpec{CacheSize: &quantity}},
	}}
	if err := c.Create(ctx, storageSite); err != nil {
		t.Fatal(err)
	}

	awaitStorage(2 << 40)

	quantity = resource.MustParse("4Ti")

	storageSite.Spec.Components.Racer.CacheSize = &quantity
	if err := c.Update(ctx, storageSite); err != nil {
		t.Fatal(err)
	}

	awaitStorage(4 << 40)

	n.Annotations[racer.CacheSizeAnnotationKey] = "32Mi"
	if err := c.Update(ctx, n); err != nil {
		t.Fatal(err)
	}

	awaitStorage(32 << 20)

	if err := c.Delete(ctx, storageSite); err != nil {
		t.Fatal(err)
	}

	delete(n.Annotations, racer.CacheSizeAnnotationKey)

	if err := c.Update(ctx, n); err != nil {
		t.Fatal(err)
	}

	awaitStorage(racer.DefaultCacheSizeBytes)

	unchanged, _, err := store.load(ctx, universe)
	if err != nil || !reflect.DeepEqual(unchanged, g) {
		t.Fatalf("storage edits changed topology: %+v %v", unchanged, err)
	}

	index, err := indexGeneration(g)
	if err != nil {
		t.Fatal(err)
	}

	want := index.snapshot(identity("node", string(n.UID)))
	deadline := time.Now().Add(10 * time.Second)

	for {
		response := get(handler(server), target(want), "", "")
		if response.Code == 200 {
			checkSnapshot(t, response, want)
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("committed generation not served")
		}

		time.Sleep(20 * time.Millisecond)
	}

	var actual corev1.Service
	for {
		if err := c.Get(ctx, client.ObjectKeyFromObject(s), &actual); err != nil {
			t.Fatal(err)
		}

		if actual.Annotations[annotationPrefix+"status"] == "Published; readiness follows dataplane activation" {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("Service patch missing")
		}

		time.Sleep(20 * time.Millisecond)
	}

	if actual.Spec.Ports[0].TargetPort.IntVal != 10000 || actual.Spec.InternalTrafficPolicy == nil || *actual.Spec.InternalTrafficPolicy != corev1.ServiceInternalTrafficPolicyLocal {
		t.Fatal("incorrect Service routing")
	}

	completeRollout(g)

	// Only an origin event changes: the reverse dependency must wake the volume's
	// universe even though the origin has a different universe annotation.
	origin.Spec.Ports[0].Port = 8083
	if err := c.Update(ctx, origin); err != nil {
		t.Fatal(err)
	}

	g = await(func(next *generation) bool {
		return next.Revision > g.Revision && next.Volume.Origin.Identity == "ns/origin:8083"
	})
	completeRollout(g)

	// Exclusion and re-enrollment must be driven by Node label events alone.
	n.Labels[racer.ExcludeLabelKey] = "true"
	if err := c.Update(ctx, n); err != nil {
		t.Fatal(err)
	}

	g = await(func(next *generation) bool { return next.Revision > g.Revision && len(next.Owners) == 0 })
	if g.Nodes[n.Name].ID != identity("node", string(n.UID)) {
		t.Fatal("Site exclusion lost the removal recipient")
	}

	delete(n.Labels, racer.ExcludeLabelKey)

	if err := c.Update(ctx, n); err != nil {
		t.Fatal(err)
	}

	g = await(func(next *generation) bool { return next.Revision > g.Revision && len(next.Owners) == 8 })
	completeRollout(g)

	if err := c.Delete(ctx, n); err != nil {
		t.Fatal(err)
	}

	removed := await(func(next *generation) bool { return next.Revision > g.Revision && len(next.Owners) == 0 })
	if removed.Nodes[n.Name].ID != identity("node", string(n.UID)) {
		t.Fatal("Node removal lost recipient")
	}
}
