// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/containerd/containerd/v2/core/content"

	"github.com/Azure/unbounded/internal/gantry/cdsub"
)

// dialBackoff is how long the provider waits before dialing containerd again
// after a failure.
const dialBackoff = 5 * time.Second

// lazyProvider reads layer blobs from containerd's content store, dialing on
// first use.
//
// Dialing eagerly at startup would be a deadlock in the making. containerd
// starts this daemon's peer, the proxy plugin connection, before it is
// necessarily ready to serve gRPC itself, and a snapshotter that refuses to
// start because containerd is not up yet is a snapshotter containerd cannot
// start with. Ingest is off the container start path, so a provider that is
// unavailable for the first few seconds costs nothing: the queue retries.
type lazyProvider struct {
	socket    string
	namespace string
	log       *slog.Logger

	// dial opens the connection. It is a field so tests can exercise the
	// retry behaviour without a containerd to fail against.
	dial func() (content.Store, io.Closer, error)

	mu     sync.Mutex
	store  content.Store
	closer io.Closer
	next   time.Time
}

func newLazyProvider(socket, namespace string, log *slog.Logger) *lazyProvider {
	p := &lazyProvider{socket: socket, namespace: namespace, log: log}
	p.dial = p.dialContainerd

	return p
}

// dialContainerd is the production dialer.
func (p *lazyProvider) dialContainerd() (content.Store, io.Closer, error) {
	src, err := cdsub.NewContainerdSource(p.socket, p.namespace, cdsub.WithContainerdLogger(p.log))
	if err != nil {
		return nil, nil, err
	}

	store := src.ContentStore()
	if store == nil {
		_ = src.Close() //nolint:errcheck // best effort on a useless connection

		return nil, nil, errors.New("containerd connection has no content store")
	}

	return store, src, nil
}

// ReaderAt implements snapshotter.Provider.
func (p *lazyProvider) ReaderAt(ctx context.Context, desc ocispec.Descriptor) (content.ReaderAt, error) {
	store, err := p.contentStore()
	if err != nil {
		return nil, err
	}

	return store.ReaderAt(ctx, desc)
}

// contentStore returns the content store, dialing if needed. Failures are rate
// limited so a queue full of retries does not turn into a dial storm against a
// containerd that is down.
func (p *lazyProvider) contentStore() (content.Store, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.store != nil {
		return p.store, nil
	}

	now := time.Now()
	if now.Before(p.next) {
		return nil, fmt.Errorf("containerd at %s is not available", p.socket)
	}

	p.next = now.Add(dialBackoff)

	store, closer, err := p.dial()
	if err != nil {
		return nil, err
	}

	p.store, p.closer = store, closer

	return store, nil
}

// Close releases the containerd connection.
func (p *lazyProvider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	closer := p.closer
	p.store, p.closer = nil, nil

	if closer == nil {
		return nil
	}

	return closer.Close()
}
