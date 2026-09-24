// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"os"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestClusterCacheAPISchema(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS for API schema validation")
	}

	server := &envtest.Environment{CRDDirectoryPaths: []string{"../../../deploy/racer/crd"}, ErrorIfCRDPathMissing: true}

	config, err := server.Start()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Error(err)
		}
	})

	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	kube, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": GroupVersion.String(), "kind": "ClusterCache",
		"metadata": map[string]any{"name": "defaults"},
	}}
	if err := kube.Create(ctx, obj); err != nil {
		t.Fatal(err)
	}

	var cache ClusterCache
	if err := kube.Get(ctx, client.ObjectKey{Name: "defaults"}, &cache); err != nil {
		t.Fatal(err)
	}

	if cache.Spec.CacheGeneration != 1 || cache.Spec.MaxCandidateAttempts != 3 {
		t.Fatalf("missing defaults: %+v", cache.Spec)
	}

	cache.Spec.SiteSelector.MatchLabels = map[string]string{"region": "west"}

	cache.Spec.CacheGeneration = 2
	if err := kube.Update(ctx, &cache); err != nil {
		t.Fatalf("mutable selector and increasing generation: %v", err)
	}

	cache.Spec.CacheGeneration = 1
	if err := kube.Update(ctx, &cache); !apierrors.IsInvalid(err) {
		t.Fatalf("decreasing generation accepted: %v", err)
	}

	for _, tc := range []struct {
		name       string
		generation int64
		attempts   int32
	}{
		{"invalid.name", 1, 3},
		{strings.Repeat("x", 64), 1, 3},
		{"negative-generation", -1, 3},
		{"zero-attempts", 1, 0},
		{"excess-attempts", 1, 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := &ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: tc.name}, Spec: ClusterCacheSpec{CacheGeneration: tc.generation, MaxCandidateAttempts: tc.attempts}}
			if err := kube.Create(ctx, invalid); !apierrors.IsInvalid(err) {
				t.Fatalf("invalid spec accepted: %v", err)
			}
		})
	}

	if err := kube.Create(ctx, &ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("x", 63)}, Spec: ClusterCacheSpec{CacheGeneration: 0, MaxCandidateAttempts: 8}}); err != nil {
		t.Fatalf("boundary values rejected: %v", err)
	}

	if err := kube.Get(ctx, client.ObjectKey{Name: "defaults"}, &cache); err != nil {
		t.Fatal(err)
	}

	cache.Status.ObservedGeneration = cache.Generation
	cache.Status.CacheSocket = "/run/racer/" + string(cache.UID) + "/cache"

	cache.Status.OriginSocket = "/run/racer/" + string(cache.UID) + "/origin"
	if err := kube.Status().Update(ctx, &cache); err != nil {
		t.Fatalf("status subresource: %v", err)
	}

	cache = ClusterCache{}
	if err := kube.Get(ctx, client.ObjectKey{Name: "defaults"}, &cache); err != nil {
		t.Fatal(err)
	}

	if cache.Status.ObservedGeneration != cache.Generation {
		t.Fatal("status was not persisted")
	}

	if cache.Status.CacheSocket != "/run/racer/"+string(cache.UID)+"/cache" || cache.Status.OriginSocket != "/run/racer/"+string(cache.UID)+"/origin" {
		t.Fatalf("socket status was not persisted: %+v", cache.Status)
	}
}
