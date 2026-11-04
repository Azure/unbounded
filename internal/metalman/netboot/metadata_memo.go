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
	// metadataFetchTimeout bounds one shared metadata fetch.
	metadataFetchTimeout = time.Minute
)

// MetadataMemo memoizes parsed disk/metadata.yaml for a short time.
//
// Metadata is read on hot paths: every DHCP reply, every HTTP and TFTP
// request, and every MachineOperation poll. The memo keeps those paths from
// pulling the image for each packet, and deduplicates concurrent lookups of
// the same image so that a DHCP storm causes one registry request. Failures
// are memoized for ErrorTTL to avoid hammering an unavailable registry.
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
	// Now returns the current time. Nil means time.Now.
	Now func() time.Time

	mu      sync.Mutex
	entries map[metadataKey]metadataEntry
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

func (m *MetadataMemo) get(ctx context.Context, src ImageSource, fetch func(context.Context, ImageSource) (*ImageMetadata, error)) (*ImageMetadata, error) {
	key := newMetadataKey(src)

	m.mu.Lock()

	if e, ok := m.entries[key]; ok && m.now().Before(e.expires) {
		m.mu.Unlock()
		return e.meta, e.err
	}

	m.mu.Unlock()

	// The shared fetch must not be canceled when the first caller goes away,
	// since other callers may be waiting on it.
	ch := m.group.DoChan(key.flightKey(), func() (any, error) {
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), metadataFetchTimeout)
		defer cancel()

		meta, err := fetch(fetchCtx, src)

		if ttl := m.ttl(meta, err); ttl > 0 {
			m.mu.Lock()
			if m.entries == nil {
				m.entries = map[metadataKey]metadataEntry{}
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
