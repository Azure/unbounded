// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/manifest"
)

// delayedManifestStore returns ErrNotFound until availableAfter opens, which
// models containerd committing the streamed manifest a moment after the mirror
// finishes serving it.
type delayedManifestStore struct {
	mu    sync.Mutex
	opens int
	body  string
	ready bool
}

func (s *delayedManifestStore) markReady() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ready = true
}

func (s *delayedManifestStore) Open(_ context.Context, _ digest.Digest) (io.ReadCloser, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.opens++

	if !s.ready {
		return nil, 0, errors.New("content not found")
	}

	return io.NopCloser(strings.NewReader(s.body)), int64(len(s.body)), nil
}

func (s *delayedManifestStore) openCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.opens
}

func (s *delayedManifestStore) Has(context.Context, digest.Digest) (bool, error) {
	return false, nil
}

func (s *delayedManifestStore) Writer(context.Context, digest.Digest) (ifaces.ContentWriter, error) {
	return nil, errors.New("not implemented")
}

func (s *delayedManifestStore) Delete(context.Context, digest.Digest) error {
	return errors.New("not implemented")
}

func testDigest(t *testing.T, fill string) digest.Digest {
	t.Helper()

	d, err := digest.Parse("sha256:" + strings.Repeat(fill, 64))
	if err != nil {
		t.Fatalf("digest.Parse: %v", err)
	}

	return d
}

func TestOpenManifestRetriesUntilContainerdCommits(t *testing.T) {
	store := &delayedManifestStore{body: `{"schemaVersion":2}`}
	adapter := &layerPrefetchAdapter{cache: store, logger: slog.Default()}

	go func() {
		time.Sleep(250 * time.Millisecond)
		store.markReady()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rc, err := adapter.openManifest(ctx, testDigest(t, "a"))
	if err != nil {
		t.Fatalf("openManifest: %v", err)
	}

	defer func() { _ = rc.Close() }() //nolint:errcheck // best-effort close

	if store.openCount() < 2 {
		t.Fatalf("open attempts = %d, want the retry path exercised", store.openCount())
	}
}

func TestOpenManifestGivesUpWhenNeverCommitted(t *testing.T) {
	store := &delayedManifestStore{body: "unused"}
	adapter := &layerPrefetchAdapter{cache: store, logger: slog.Default()}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := adapter.openManifest(ctx, testDigest(t, "b")); err == nil {
		t.Fatal("openManifest succeeded, want failure when the manifest never lands")
	}

	if store.openCount() < 2 {
		t.Fatalf("open attempts = %d, want more than one before giving up", store.openCount())
	}
}

func TestLayerPrefetchAdapterReportsManifestChildrenWithoutResolver(t *testing.T) {
	manifestDigest := testDigest(t, "a")
	configDigest := testDigest(t, "b")
	layer0 := testDigest(t, "c")
	layer1 := testDigest(t, "d")
	body := `{"schemaVersion":2,"config":{"digest":"` + configDigest.String() + `"},"layers":[{"digest":"` + layer0.String() + `"},{"digest":"` + layer1.String() + `"}]}`
	store := &delayedManifestStore{body: body, ready: true}

	var (
		gotManifest digest.Digest
		gotChildren int
	)

	adapter := &layerPrefetchAdapter{
		cache:  store,
		logger: slog.Default(),
		onManifest: func(observed digest.Digest, children []manifest.TypedChild) {
			gotManifest = observed
			gotChildren = len(children)
		},
	}

	adapter.OnManifestServed(context.Background(), "registry.example", "pull", manifestDigest)

	if gotManifest != manifestDigest || gotChildren != 3 {
		t.Fatalf("manifest callback = %s with %d children, want %s with 3", gotManifest, gotChildren, manifestDigest)
	}
}
