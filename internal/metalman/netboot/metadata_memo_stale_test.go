// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/metalman/imagepull/imagepulltest"
)

const (
	registryOK = iota
	registryFailing
	registrySlow
)

type staleFixture struct {
	images   *testImages
	resolver *FileResolver
	mode     atomic.Int32
	release  chan struct{}

	mu  sync.Mutex
	now time.Time
}

func newStaleFixture(t *testing.T, staleTTL time.Duration) *staleFixture {
	t.Helper()

	f := &staleFixture{release: make(chan struct{}), now: time.Unix(1000, 0)}
	f.images = newTestImages(t, imagepulltest.WithMiddleware(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/manifests/") {
				switch f.mode.Load() {
				case registryFailing:
					http.Error(w, "down", http.StatusServiceUnavailable)
					return
				case registrySlow:
					select {
					case <-f.release:
					case <-r.Context().Done():
						return
					}
				}
			}

			next.ServeHTTP(w, r)
		})
	}))
	f.resolver = &FileResolver{Puller: f.images.Puller, Metadata: &MetadataMemo{
		TTL:          time.Minute,
		ErrorTTL:     -1,
		StaleTTL:     staleTTL,
		FetchTimeout: 10 * time.Second,
		Now:          f.clock,
	}}

	return f
}

func (f *staleFixture) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.now
}

func (f *staleFixture) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.now = f.now.Add(d)
}

func (f *staleFixture) push(t *testing.T, boot string) string {
	t.Helper()

	return f.images.Push(t, "ghcr.io/test/netboot:v1", map[string][]byte{"metadata.yaml": []byte("dhcpBootImageName: " + boot + "\n")})
}

func TestMetadataStaleOnFailingRefresh(t *testing.T) {
	f := newStaleFixture(t, time.Hour)
	src := ImageSource{Ref: f.push(t, "a.efi")}

	meta, stale, err := f.resolver.ImageMetadataOrStale(t.Context(), src)
	require.NoError(t, err)
	require.False(t, stale)
	require.Equal(t, "a.efi", meta.DHCPBootImageName)

	f.advance(2 * time.Minute)
	f.mode.Store(registryFailing)

	meta, stale, err = f.resolver.ImageMetadataOrStale(t.Context(), src)
	require.Error(t, err)
	require.True(t, stale)
	require.Equal(t, "a.efi", meta.DHCPBootImageName)

	// Non-DHCP lookups never get stale metadata.
	_, err = f.resolver.ImageMetadata(t.Context(), src)
	require.Error(t, err)

	// Past StaleTTL the fallback is gone.
	f.advance(time.Hour)

	meta, stale, err = f.resolver.ImageMetadataOrStale(t.Context(), src)
	require.Error(t, err)
	require.False(t, stale)
	require.Nil(t, meta)
}

func TestMetadataStaleOnSlowRefresh(t *testing.T) {
	f := newStaleFixture(t, 0)
	f.push(t, "a.efi")

	src := ImageSource{Ref: "ghcr.io/test/netboot:v1"}

	_, _, err := f.resolver.ImageMetadataOrStale(t.Context(), src)
	require.NoError(t, err)

	f.push(t, "b.efi")
	f.advance(2 * time.Minute)
	f.mode.Store(registrySlow)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	meta, stale, err := f.resolver.ImageMetadataOrStale(ctx, src)

	cancel()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.True(t, stale)
	require.Equal(t, "a.efi", meta.DHCPBootImageName)

	// The refresh keeps running and updates the entry.
	close(f.release)
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()

		meta, stale, err := f.resolver.ImageMetadataOrStale(ctx, src)

		return err == nil && !stale && meta.DHCPBootImageName == "b.efi"
	}, 5*time.Second, 20*time.Millisecond)
}

func TestMetadataStaleDisabledAndColdMiss(t *testing.T) {
	f := newStaleFixture(t, -1)
	src := ImageSource{Ref: f.push(t, "a.efi")}

	// Cold miss with a failing registry: plain error, as before.
	f.mode.Store(registryFailing)

	meta, stale, err := f.resolver.ImageMetadataOrStale(t.Context(), src)
	require.Error(t, err)
	require.False(t, stale)
	require.Nil(t, meta)

	// Negative StaleTTL disables the fallback.
	f.mode.Store(registryOK)

	_, _, err = f.resolver.ImageMetadataOrStale(t.Context(), src)
	require.NoError(t, err)

	f.advance(2 * time.Minute)
	f.mode.Store(registryFailing)

	meta, stale, err = f.resolver.ImageMetadataOrStale(t.Context(), src)
	require.Error(t, err)
	require.False(t, stale)
	require.Nil(t, meta)
}
