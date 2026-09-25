// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/coldstart"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/manifest"
	"github.com/Azure/unbounded/internal/gantry/metrics"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
)

// delayedManifestStore returns an error until markReady is called, which
// models containerd committing the streamed manifest a moment after the mirror
// finishes serving it.
type delayedManifestStore struct {
	mu    sync.Mutex
	opens int
	body  string
	ready bool
	has   func(digest.Digest) (bool, error)
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

func (s *delayedManifestStore) Has(_ context.Context, d digest.Digest) (bool, error) {
	if s.has != nil {
		return s.has(d)
	}

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
	adapter := &committedManifestHandler{cache: store, logger: slog.Default()}

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
	adapter := &committedManifestHandler{cache: store, logger: slog.Default()}

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
	store.has = func(digest.Digest) (bool, error) {
		t.Fatal("observation must not probe child presence")
		return false, nil
	}

	var (
		gotManifest digest.Digest
		gotChildren int
	)

	adapter := newManifestObserver(store, slog.Default(),
		func(observed digest.Digest, children []manifest.TypedChild) {
			gotManifest = observed
			gotChildren = len(children)
		},
	)

	adapter.OnManifestServed(context.Background(), "registry.example", "pull", manifestDigest)

	if gotManifest != manifestDigest || gotChildren != 3 {
		t.Fatalf("manifest callback = %s with %d children, want %s with 3", gotManifest, gotChildren, manifestDigest)
	}
}

type manifestPrefetchFunc func(context.Context, digest.Digest, []coldstart.ChildDigest, string, string) error

func (f manifestPrefetchFunc) PrefetchManifestChildren(ctx context.Context, d digest.Digest, children []coldstart.ChildDigest, registry, repository string) error {
	return f(ctx, d, children, registry, repository)
}

func TestManifestConsumersShareParsingAndFiltering(t *testing.T) {
	d := testDigest(t, "a")
	config := testDigest(t, "b")
	local := testDigest(t, "c")
	unknown := testDigest(t, "d")
	foreign := testDigest(t, "e")
	body := `{"config":{"digest":"` + config.String() + `"},"layers":[{"digest":"` + local.String() + `"},{"digest":"` + unknown.String() + `"},{"digest":"` + foreign.String() + `","urls":["https://foreign.example/layer"]},{"digest":"invalid"}]}`
	wantObserved := []manifest.TypedChild{{Digest: config, Kind: ifaces.KindConfig}, {Digest: local, Kind: ifaces.KindBlob}, {Digest: unknown, Kind: ifaces.KindBlob}}
	wantPending := []coldstart.ChildDigest{{Digest: config, Kind: ifaces.KindConfig}, {Digest: unknown, Kind: ifaces.KindBlob}}

	for _, direct := range []bool{false, true} {
		t.Run(map[bool]string{false: "observation", true: "direct"}[direct], func(t *testing.T) {
			store := &delayedManifestStore{body: body, ready: true}

			var checked []digest.Digest

			store.has = func(child digest.Digest) (bool, error) {
				checked = append(checked, child)
				if child == unknown {
					return false, errors.New("presence unavailable")
				}

				return child == local, nil
			}

			observed, prefetched := false, false
			observe := func(got digest.Digest, children []manifest.TypedChild) {
				observed = true

				if got != d || !reflect.DeepEqual(children, wantObserved) {
					t.Fatalf("observed %s %v, want %s %v", got, children, d, wantObserved)
				}
			}
			consumer := newManifestObserver(store, slog.Default(), observe)

			if direct {
				resolver := manifestPrefetchFunc(func(ctx context.Context, got digest.Digest, children []coldstart.ChildDigest, registry, repository string) error {
					prefetched = true

					if !observed || got != d || registry != "registry.example" || repository != "pull" || !reflect.DeepEqual(children, wantPending) {
						t.Fatalf("prefetch before observation or incorrect arguments: %s %v %s %s", got, children, registry, repository)
					}

					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 30*time.Second || registryauth.Authorization(ctx) != "Bearer delegated" {
						t.Fatal("prefetch lost deadline or delegated authorization")
					}

					return errors.New("prefetch failure is best effort")
				})
				consumer = newLayerPrefetcher(resolver, store, slog.Default(), observe)
			}

			consumer.OnManifestServed(registryauth.WithAuthorization(context.Background(), "Bearer delegated"), "registry.example", "pull", d)

			if !observed || prefetched != direct || store.openCount() != 1 {
				t.Fatalf("observed=%v prefetched=%v opens=%d", observed, prefetched, store.openCount())
			}

			if direct && !reflect.DeepEqual(checked, []digest.Digest{config, local, unknown}) || !direct && len(checked) != 0 {
				t.Fatalf("child presence checks = %v", checked)
			}
		})
	}
}

func TestManifestConsumersSkipInvalidAndUnneededPrefetch(t *testing.T) {
	child := testDigest(t, "b")
	for _, tc := range []struct {
		name    string
		body    string
		observe bool
	}{
		{name: "malformed", body: `{"layers":`},
		{name: "size cap", body: `{}` + strings.Repeat(" ", int(maxManifestBytes)-2)},
		{name: "image index", body: `{"manifests":[{"digest":"` + child.String() + `"}]}`, observe: true},
		{name: "empty", body: `{}`, observe: true},
		{name: "already local", body: `{"layers":[{"digest":"` + child.String() + `"}]}`, observe: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, direct := range []bool{false, true} {
				store := &delayedManifestStore{body: tc.body, ready: true, has: func(digest.Digest) (bool, error) { return true, nil }}
				observed := false
				observe := func(digest.Digest, []manifest.TypedChild) { observed = true }
				consumer := newManifestObserver(store, slog.Default(), observe)

				if direct {
					resolver := manifestPrefetchFunc(func(context.Context, digest.Digest, []coldstart.ChildDigest, string, string) error {
						t.Fatal("unexpected prefetch")
						return nil
					})
					consumer = newLayerPrefetcher(resolver, store, slog.Default(), observe)
				}

				consumer.OnManifestServed(context.Background(), "registry.example", "pull", testDigest(t, "a"))

				if observed != tc.observe {
					t.Fatalf("direct=%v observed=%v, want %v", direct, observed, tc.observe)
				}
			}
		})
	}
}

func TestManifestObservationReportsProgressAfterCommit(t *testing.T) {
	reg := metrics.New()
	phase := newPhase2Metrics(reg)
	tracker := newLayerProgressTracker(phase.layerCompletedAt, "node-a", func() time.Time { return time.Unix(123, 0) })
	child := testDigest(t, "b")
	store := &delayedManifestStore{body: `{"layers":[{"digest":"` + child.String() + `"}]}`}
	tracker.completed(child)

	go func() {
		time.Sleep(150 * time.Millisecond)
		store.markReady()
	}()

	observer := newManifestObserver(store, slog.Default(), tracker.observeManifest)
	observer.OnManifestServed(context.Background(), "registry.example", "pull", testDigest(t, "a"))

	series := layerProgressSeries(t, reg)
	if store.openCount() < 2 || len(series) != 1 || series[child.String()] != 123 {
		t.Fatalf("opens=%d progress=%v", store.openCount(), series)
	}
}
