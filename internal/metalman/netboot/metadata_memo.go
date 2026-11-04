// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	// DefaultMetadataTTL is how long parsed image metadata is reused.
	DefaultMetadataTTL = time.Minute
	// DefaultMetadataErrorTTL is how long a failed metadata lookup is reused.
	DefaultMetadataErrorTTL = 5 * time.Second
	// DefaultMetadataFetchTimeout bounds one shared metadata fetch.
	DefaultMetadataFetchTimeout = time.Minute
)

// MetadataMemo memoizes parsed disk/metadata.yaml for a short time.
//
// Metadata is read on hot paths: every DHCP reply, every HTTP and TFTP
// request, and every MachineOperation poll. The memo keeps those paths from
// pulling the image for each packet, and deduplicates concurrent lookups of
// the same image so that a DHCP storm causes one registry request. Failures
// are memoized for ErrorTTL to avoid hammering an unavailable registry.
//
// Lookups by ImageSource (DHCP replies, boot media) are keyed by reference,
// so after a mutable tag moves they may return the old image's metadata for
// up to TTL. Lookups for an already resolved image (HTTP and TFTP file
// requests, which pin one image per request) are keyed by manifest digest,
// so the metadata always matches the content served from that image.
//
// Only the parsed metadata is memoized. Image content is never cached:
// every file request pulls the file from the registry or mirror again.
//
// The zero value is ready to use. A MetadataMemo is safe for concurrent use.
type MetadataMemo struct {
	// TTL is how long a successful lookup is reused. Zero means
	// DefaultMetadataTTL.
	TTL time.Duration
	// ErrorTTL is how long a failed lookup is reused. Zero means
	// DefaultMetadataErrorTTL; a negative value disables error memoization.
	ErrorTTL time.Duration
	// FetchTimeout bounds one shared metadata fetch. Zero or less means
	// DefaultMetadataFetchTimeout. An imagepull.Puller with a mirror caps
	// each mirror attempt at half of the remaining budget, so a stalled
	// mirror still leaves time for the upstream registry.
	FetchTimeout time.Duration
	// Now returns the current time. Nil means time.Now.
	Now func() time.Time

	mu      sync.Mutex
	entries map[string]metadataEntry
	group   singleflight.Group
}

type metadataKey struct {
	ref, architecture, secretNamespace, secretName string
	hasSecret                                      bool
}

type metadataEntry struct {
	meta    *ImageMetadata
	err     error
	expires time.Time
}

func newMetadataKey(src ImageSource) metadataKey {
	k := metadataKey{ref: src.Ref, architecture: src.architecture()}
	if src.PullSecretRef != nil {
		k.hasSecret = true
		k.secretNamespace = src.PullSecretRef.Namespace
		k.secretName = src.PullSecretRef.Name
	}

	return k
}

// flightKey encodes k as a singleflight key. Each field is quoted, so
// distinct keys never encode to the same string.
func (k metadataKey) flightKey() string {
	return fmt.Sprintf("%q %q %q %q %t", k.ref, k.architecture, k.secretNamespace, k.secretName, k.hasSecret)
}

func (m *MetadataMemo) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}

	return time.Now()
}

func (m *MetadataMemo) fetchTimeout() time.Duration {
	if m.FetchTimeout > 0 {
		return m.FetchTimeout
	}

	return DefaultMetadataFetchTimeout
}

// ttl returns how long a lookup result is reused. Failures and images without
// metadata.yaml use ErrorTTL, so a newly pushed metadata.yaml is seen quickly.
func (m *MetadataMemo) ttl(meta *ImageMetadata, err error) time.Duration {
	if err != nil || meta == nil || meta.missing {
		if m.ErrorTTL == 0 {
			return DefaultMetadataErrorTTL
		}

		return m.ErrorTTL
	}

	if m.TTL == 0 {
		return DefaultMetadataTTL
	}

	return m.TTL
}

// get returns the memoized metadata of src, keyed by reference.
func (m *MetadataMemo) get(ctx context.Context, src ImageSource, fetch func(context.Context, ImageSource) (*ImageMetadata, error)) (*ImageMetadata, error) {
	return m.lookup(ctx, "ref "+newMetadataKey(src).flightKey(), func(ctx context.Context) (*ImageMetadata, error) {
		return fetch(ctx, src)
	})
}

// getPinned returns the memoized metadata of the image with manifest digest
// digest. A digest names immutable content, so the entry cannot go stale.
func (m *MetadataMemo) getPinned(ctx context.Context, digest string, fetch func(context.Context) (*ImageMetadata, error)) (*ImageMetadata, error) {
	return m.lookup(ctx, "digest "+digest, fetch)
}

// cached returns the unexpired entry for key, if any.
func (m *MetadataMemo) cached(key string) (metadataEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.entries[key]
	if !ok || !m.now().Before(e.expires) {
		return metadataEntry{}, false
	}

	return e, true
}

func (m *MetadataMemo) lookup(ctx context.Context, key string, fetch func(context.Context) (*ImageMetadata, error)) (*ImageMetadata, error) {
	if e, ok := m.cached(key); ok {
		return e.meta, e.err
	}

	// The shared fetch must not be canceled when the first caller goes away,
	// since other callers may be waiting on it.
	ch := m.group.DoChan(key, func() (any, error) {
		// A flight for key may have completed and cached its result
		// between the lookup above and this flight starting.
		if e, ok := m.cached(key); ok {
			return e.meta, e.err
		}

		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.fetchTimeout())
		defer cancel()

		meta, err := fetch(fetchCtx)

		if ttl := m.ttl(meta, err); ttl > 0 {
			m.mu.Lock()
			if m.entries == nil {
				m.entries = map[string]metadataEntry{}
			}

			now := m.now()
			for k, e := range m.entries {
				if !now.Before(e.expires) {
					delete(m.entries, k)
				}
			}

			m.entries[key] = metadataEntry{meta: meta, err: err, expires: now.Add(ttl)}
			m.mu.Unlock()
		}

		return meta, err
	})

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}

		meta, _ := res.Val.(*ImageMetadata) //nolint:errcheck // the flight only returns *ImageMetadata

		return meta, nil
	}
}
