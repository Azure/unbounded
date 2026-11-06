// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/httprange"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/manifest"
	"github.com/Azure/unbounded/internal/gantry/rangecache"
)

func TestIndexPrefetchDeduplicatesOverlappingCallbacks(t *testing.T) {
	cache, err := rangecache.New(t.TempDir()+"/ranges", 0)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = cache.Close() })

	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	done := make(chan struct{})
	origin := &prefetchOriginStub{beforePull: func(ctx context.Context, _ ifaces.OriginRef) error {
		select {
		case started <- struct{}{}:
		default:
		}

		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	p := newIndexPrefetcher(indexPrefetchOptions{Cache: cache, Origin: origin})
	d := prefetchDigest(t, "layer")
	image := prefetchDigest(t, "manifest")
	children := []manifest.TypedChild{{Digest: d, Kind: ifaces.KindBlob, Size: 4096}}

	go func() {
		defer close(done)

		p.Prefetch(ctx, "reg", "repo", image, children)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first prefetch did not reach origin")
	}

	var callbacks sync.WaitGroup
	for range 16 {
		callbacks.Go(func() { p.Prefetch(ctx, "reg", "repo", image, children) })
	}

	duplicatesDone := make(chan struct{})

	go func() {
		callbacks.Wait()
		close(duplicatesDone)
	}()

	select {
	case <-duplicatesDone:
	case <-time.After(2 * time.Second):
		t.Fatal("duplicate callbacks did not return while the first job was active")
	}

	if got := origin.calls.Load(); got != 1 {
		t.Fatalf("overlapping origin pulls = %d, want 1", got)
	}

	close(release)
	<-done

	p.Prefetch(ctx, "reg", "repo", image, children)

	if got := origin.calls.Load(); got != 1 {
		t.Fatalf("warm callback origin pulls = %d, want 1", got)
	}

	if !cache.Has(d, httprange.Range{Start: 0, End: 4095}) {
		t.Fatal("deduplicated job did not finish warming metadata")
	}
}

func TestIndexPrefetchKeepsManifestsAndOriginsIndependent(t *testing.T) {
	for _, scope := range []string{"registry", "repository", "manifest"} {
		t.Run(scope, func(t *testing.T) {
			cache, err := rangecache.New(t.TempDir()+"/ranges", 0)
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() { _ = cache.Close() })

			ctx, cancel := context.WithCancel(t.Context())
			started := make(chan struct{}, 2)
			release := make(chan struct{})
			origin := &prefetchOriginStub{beforePull: func(ctx context.Context, _ ifaces.OriginRef) error {
				started <- struct{}{}

				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}}
			p := newIndexPrefetcher(indexPrefetchOptions{Cache: cache, Origin: origin})
			image := prefetchDigest(t, "manifest")
			children := []manifest.TypedChild{{Digest: prefetchDigest(t, "layer"), Kind: ifaces.KindBlob, Size: 4096}}

			var jobs sync.WaitGroup

			t.Cleanup(func() {
				cancel()
				jobs.Wait()
			})
			jobs.Go(func() { p.Prefetch(ctx, "reg", "repo", image, children) })

			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("first prefetch did not start")
			}

			registry, repository, otherImage := "reg", "repo", image

			switch scope {
			case "registry":
				registry = "other"
			case "repository":
				repository = "other"
			case "manifest":
				otherImage = prefetchDigest(t, "other")
			}

			jobs.Go(func() { p.Prefetch(ctx, registry, repository, otherImage, children) })

			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatalf("independent %s prefetch was incorrectly deduplicated", scope)
			}

			if got := origin.calls.Load(); got != 2 {
				t.Fatalf("independent origin pulls = %d, want 2", got)
			}

			close(release)
			jobs.Wait()
		})
	}
}

func TestIndexPrefetchBudgetReleasesClaimForRetry(t *testing.T) {
	cache, err := rangecache.New(t.TempDir()+"/ranges", 0)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = cache.Close() })

	origin := &prefetchOriginStub{beforePull: func(ctx context.Context, _ ifaces.OriginRef) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	p := newIndexPrefetcher(indexPrefetchOptions{
		Cache: cache, Origin: origin, Budget: 50 * time.Millisecond,
	})
	d := prefetchDigest(t, "layer")
	image := prefetchDigest(t, "manifest")
	children := []manifest.TypedChild{{Digest: d, Kind: ifaces.KindBlob, Size: 4096}}

	done := make(chan struct{})
	go func() {
		defer close(done)

		p.Prefetch(t.Context(), "reg", "repo", image, children)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("prefetch exceeded its deadline")
	}

	if got := origin.calls.Load(); got != 1 {
		t.Fatalf("timed-out origin pulls = %d, want 1", got)
	}

	origin.beforePull = nil

	p.Prefetch(t.Context(), "reg", "repo", image, children)

	if got := origin.calls.Load(); got != 2 {
		t.Fatalf("retry origin pulls = %d, want 2", got)
	}

	if !cache.Has(d, httprange.Range{Start: 0, End: 4095}) {
		t.Fatal("retry did not warm metadata after the timed-out job released its claim")
	}
}
