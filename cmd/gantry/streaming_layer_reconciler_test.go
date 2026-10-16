// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/coldstart"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
)

type streamingReconcileResolverStub struct {
	mu            sync.Mutex
	calls         int
	children      []coldstart.ChildDigest
	completeAfter int
	store         *fakes.Cache
	invoked       chan struct{}
}

func (s *streamingReconcileResolverStub) PrefetchManifestChildren(_ context.Context, _ digest.Digest, children []coldstart.ChildDigest, _, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.calls++
	s.children = append([]coldstart.ChildDigest(nil), children...)
	if s.completeAfter > 0 && s.calls == s.completeAfter {
		for _, child := range children {
			s.store.Put(child.Digest, []byte("complete"))
		}
	}
	if s.invoked != nil {
		select {
		case s.invoked <- struct{}{}:
		default:
		}
	}

	return nil
}

func (s *streamingReconcileResolverStub) snapshot() (int, []coldstart.ChildDigest) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.calls, append([]coldstart.ChildDigest(nil), s.children...)
}

func waitStreamingReconciler(t *testing.T, reconciler *streamingLayerReconciler) {
	t.Helper()

	deadline := time.After(time.Second)
	for {
		reconciler.mu.Lock()
		active := len(reconciler.active)
		reconciler.mu.Unlock()
		if active == 0 {
			return
		}

		select {
		case <-deadline:
			t.Fatal("streaming reconciler did not finish")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestStreamingLayerReconcilerRetriesUntilCompleteAndDeduplicates(t *testing.T) {
	store := fakes.NewCache()
	resolver := &streamingReconcileResolverStub{store: store, completeAfter: 2}
	reconciler := newStreamingLayerReconciler(streamingLayerReconcilerOptions{
		Resolver:        resolver,
		Store:           store,
		Logger:          slog.Default(),
		Interval:        time.Millisecond,
		DispatchTimeout: 100 * time.Millisecond,
	})
	manifestDigest := testDigest(t, "a")
	children := []coldstart.ChildDigest{
		{Digest: testDigest(t, "b"), Kind: ifaces.KindBlob},
		{Digest: testDigest(t, "c"), Kind: ifaces.KindBlob},
	}

	reconciler.Start(t.Context(), "app.azurecr.io", "team/app", manifestDigest, children)
	reconciler.Start(t.Context(), "app.azurecr.io", "team/app", manifestDigest, children)
	waitStreamingReconciler(t, reconciler)

	calls, gotChildren := resolver.snapshot()
	if calls != 2 {
		t.Fatalf("resolver calls = %d, want 2 retries without duplicate reconcile", calls)
	}
	if len(gotChildren) != len(children) {
		t.Fatalf("last dispatch children = %d, want %d", len(gotChildren), len(children))
	}
	for _, child := range children {
		if present, err := store.Has(t.Context(), child.Digest); err != nil || !present {
			t.Fatalf("child %s present = %t, %v", child.Digest, present, err)
		}
	}
}

func TestLayerPrefetchAdapterRoutesOnlyOverlayBDManifestToReconciler(t *testing.T) {
	streamingBody, err := os.ReadFile("testdata/acr-streaming-manifest.json")
	if err != nil {
		t.Fatal(err)
	}

	store := fakes.NewCache()
	streamingResolver := &streamingReconcileResolverStub{store: store, invoked: make(chan struct{}, 1)}
	reconciler := newStreamingLayerReconciler(streamingLayerReconcilerOptions{
		Resolver:        streamingResolver,
		Store:           store,
		Logger:          slog.Default(),
		Interval:        time.Hour,
		DispatchTimeout: time.Second,
	})
	regularResolver := &manifestPrefetchResolverStub{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	streamingAdapter := &layerPrefetchAdapter{
		cache:               &delayedManifestStore{body: string(streamingBody), ready: true},
		logger:              slog.Default(),
		resolver:            regularResolver,
		streamingReconciler: reconciler,
	}
	streamingAdapter.OnManifestServed(ctx, "app.azurecr.io", "team/app", testDigest(t, "d"))

	select {
	case <-streamingResolver.invoked:
	case <-time.After(time.Second):
		t.Fatal("streaming reconciler was not invoked")
	}
	if len(regularResolver.children) != 0 {
		t.Fatalf("regular resolver received streaming children: %+v", regularResolver.children)
	}
	_, streamingChildren := streamingResolver.snapshot()
	if len(streamingChildren) != 2 {
		t.Fatalf("streaming children = %d, want only 2 layers", len(streamingChildren))
	}

	nonACRResolver := &manifestPrefetchResolverStub{}
	nonACRAdapter := &layerPrefetchAdapter{
		cache:               &delayedManifestStore{body: string(streamingBody), ready: true},
		logger:              slog.Default(),
		resolver:            nonACRResolver,
		streamingReconciler: reconciler,
	}
	nonACRAdapter.OnManifestServed(t.Context(), "registry.example.com", "team/app", testDigest(t, "f"))
	if len(nonACRResolver.children) != 3 {
		t.Fatalf("annotated non-ACR resolver children = %d, want existing config+layers path", len(nonACRResolver.children))
	}

	regularBody := `{"schemaVersion":2,"config":{"digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"layers":[{"digest":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}]}`
	regularAdapter := &layerPrefetchAdapter{
		cache:               &delayedManifestStore{body: regularBody, ready: true},
		logger:              slog.Default(),
		resolver:            regularResolver,
		streamingReconciler: reconciler,
	}
	regularAdapter.OnManifestServed(t.Context(), "app.azurecr.io", "team/app", testDigest(t, "e"))
	if len(regularResolver.children) != 2 {
		t.Fatalf("regular resolver children = %d, want existing config+layer path", len(regularResolver.children))
	}
}

func TestPumpedManifestActivatesOnlyStreamingReconciler(t *testing.T) {
	streamingBody, err := os.ReadFile("testdata/acr-streaming-manifest.json")
	if err != nil {
		t.Fatal(err)
	}

	store := fakes.NewCache()
	streamingResolver := &streamingReconcileResolverStub{store: store, invoked: make(chan struct{}, 1)}
	reconciler := newStreamingLayerReconciler(streamingLayerReconcilerOptions{
		Resolver:        streamingResolver,
		Store:           store,
		Logger:          slog.Default(),
		Interval:        time.Hour,
		DispatchTimeout: time.Second,
	})
	regularResolver := &manifestPrefetchResolverStub{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	streamingAdapter := &layerPrefetchAdapter{
		cache:               &delayedManifestStore{body: string(streamingBody), ready: true},
		logger:              slog.Default(),
		resolver:            regularResolver,
		streamingReconciler: reconciler,
	}
	streamingAdapter.OnStreamingManifestCommitted(ctx, "app.azurecr.io", "team/app", testDigest(t, "1"))

	select {
	case <-streamingResolver.invoked:
	case <-time.After(time.Second):
		t.Fatal("pumped streaming manifest did not activate the reconciler")
	}
	if len(regularResolver.children) != 0 {
		t.Fatalf("regular resolver received pumped streaming children: %+v", regularResolver.children)
	}

	regularBody := `{"schemaVersion":2,"config":{"digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"layers":[{"digest":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}]}`
	regularAdapter := &layerPrefetchAdapter{
		cache:               &delayedManifestStore{body: regularBody, ready: true},
		logger:              slog.Default(),
		resolver:            regularResolver,
		streamingReconciler: reconciler,
	}
	regularAdapter.OnStreamingManifestCommitted(ctx, "app.azurecr.io", "team/app", testDigest(t, "2"))
	if len(regularResolver.children) != 0 {
		t.Fatalf("regular resolver received pumped regular-manifest children: %+v", regularResolver.children)
	}
	if calls, _ := streamingResolver.snapshot(); calls != 1 {
		t.Fatalf("streaming resolver calls = %d, want 1 after regular ACR manifest", calls)
	}
}
