// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/containerd/containerd/v2/core/content"
)

// nopCloser counts closes so the test can prove the connection is released.
type nopCloser struct{ closed int }

func (c *nopCloser) Close() error {
	c.closed++

	return nil
}

// stubStore is a content.Store that only answers ReaderAt.
type stubStore struct {
	content.Store

	reads int
}

func (s *stubStore) ReaderAt(_ context.Context, _ ocispec.Descriptor) (content.ReaderAt, error) {
	s.reads++

	return nil, errors.New("not implemented")
}

func newTestProvider(dial func() (content.Store, io.Closer, error)) *lazyProvider {
	p := newLazyProvider("/nonexistent.sock", "testing", slog.New(slog.DiscardHandler))
	p.dial = dial

	return p
}

func TestLazyProviderDialsOnce(t *testing.T) {
	var (
		dials  int
		closer nopCloser
		store  stubStore
	)

	p := newTestProvider(func() (content.Store, io.Closer, error) {
		dials++

		return &store, &closer, nil
	})

	for range 5 {
		if _, err := p.contentStore(); err != nil {
			t.Fatalf("contentStore: %v", err)
		}
	}

	if dials != 1 {
		t.Errorf("dialed %d times, want 1", dials)
	}

	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if closer.closed != 1 {
		t.Errorf("closed %d times, want 1", closer.closed)
	}
}

// A containerd that is down must not be dialed once per queued ingest.
func TestLazyProviderRateLimitsFailedDials(t *testing.T) {
	dials := 0
	p := newTestProvider(func() (content.Store, io.Closer, error) {
		dials++

		return nil, nil, errors.New("connection refused")
	})

	if _, err := p.contentStore(); err == nil {
		t.Fatal("want the dial error")
	}

	for range 4 {
		_, err := p.contentStore()
		if err == nil {
			t.Fatal("want an error while the backoff holds")
		}

		if !strings.Contains(err.Error(), "not available") {
			t.Errorf("error = %v, want the rate limited message", err)
		}
	}

	if dials != 1 {
		t.Errorf("dialed %d times, want 1", dials)
	}
}

// Once the backoff expires the provider tries again, and a containerd that has
// come up is picked up without a restart.
func TestLazyProviderRecovers(t *testing.T) {
	var (
		dials int
		store stubStore
	)

	p := newTestProvider(func() (content.Store, io.Closer, error) {
		dials++

		if dials == 1 {
			return nil, nil, errors.New("connection refused")
		}

		return &store, &nopCloser{}, nil
	})

	if _, err := p.contentStore(); err == nil {
		t.Fatal("want the first dial to fail")
	}

	// Expire the backoff rather than sleeping through it.
	p.mu.Lock()
	p.next = time.Now().Add(-time.Second)
	p.mu.Unlock()

	if _, err := p.contentStore(); err != nil {
		t.Fatalf("contentStore: %v", err)
	}

	if dials != 2 {
		t.Errorf("dialed %d times, want 2", dials)
	}
}

func TestLazyProviderCloseWithoutADial(t *testing.T) {
	p := newTestProvider(func() (content.Store, io.Closer, error) {
		t.Fatal("Close must not dial")

		return nil, nil, nil
	})

	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// ReaderAt has to surface the dial failure rather than panicking on a nil
// store: the ingest queue turns it into a retry.
func TestLazyProviderReaderAtWithoutContainerd(t *testing.T) {
	p := newTestProvider(func() (content.Store, io.Closer, error) {
		return nil, nil, errors.New("connection refused")
	})

	if _, err := p.ReaderAt(t.Context(), ocispec.Descriptor{}); err == nil {
		t.Fatal("want an error")
	}
}
