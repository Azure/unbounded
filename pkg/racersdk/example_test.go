// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk"
)

// An immutable in-memory example. A production store can return a pinned file
// descriptor or storage version and release it with ResolvedRange.Close.
type blobStore struct {
	blobs    map[string][]byte
	metadata map[string]racersdk.Metadata
}

func newBlobStore(blobs map[string][]byte) *blobStore {
	s := &blobStore{blobs: blobs, metadata: make(map[string]racersdk.Metadata)}
	for target, data := range blobs {
		// Compute a strong content validator once at publication, never per HEAD.
		ttl := time.Minute
		s.metadata[target] = racersdk.Metadata{Size: int64(len(data)), ETag: fmt.Sprintf(`"%x"`, sha256.Sum256(data)), TTL: &ttl}
	}

	return s
}

func (s *blobStore) Stat(ctx context.Context, target string, _ []byte) (racersdk.Metadata, error) {
	if err := ctx.Err(); err != nil {
		return racersdk.Metadata{}, err
	}

	m, ok := s.metadata[target]
	if !ok {
		return racersdk.Metadata{}, fs.ErrNotExist
	}

	return m, nil
}

func (s *blobStore) ResolveRange(ctx context.Context, target string, _ []byte) (racersdk.ResolvedRange, error) {
	m, err := s.Stat(ctx, target, nil)
	if err != nil {
		return nil, err
	}

	return &blobRange{meta: m, data: s.blobs[target]}, nil
}

type blobRange struct {
	meta racersdk.Metadata
	data []byte
}

func (b *blobRange) Metadata() racersdk.Metadata { return b.meta }
func (b *blobRange) Close() error                { return nil }
func (b *blobRange) OpenRange(ctx context.Context, off, length int64) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return io.NopCloser(io.NewSectionReader(bytes.NewReader(b.data), off, length)), nil
}

func ExampleNewRangeOrigin() { //nolint:testableexamples // Illustrates a long-running server, not a finite output example.
	store := newBlobStore(map[string][]byte{"/hello": []byte("hello, Racer")})

	origin, err := racersdk.NewRangeOrigin(store)
	if err != nil {
		log.Fatal(err)
	}

	// Supply ClusterCache.status.originSocket, for example /run/racer/<uid>/origin.
	originSocket := os.Getenv("RACER_ORIGIN_SOCKET")

	listener, err := net.Listen("unix", originSocket)
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()

	if err := os.Chmod(originSocket, 0o660); err != nil {
		log.Fatal(err)
	}

	server := &http.Server{Handler: origin, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 90 * time.Second}
	log.Fatal(server.Serve(listener))
}

func ExampleClient_Open() { //nolint:testableexamples // Requires an external cache serving application data.
	// Supply ClusterCache.status.cacheSocket, for example /run/racer/<uid>/cache.
	client, err := racersdk.NewClient(os.Getenv("RACER_CACHE_SOCKET"), racersdk.ClientOptions{})
	if err != nil {
		log.Fatal(err)
	}
	defer client.CloseIdleConnections()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	object, err := client.Open(ctx, "/models/weights?version=7")
	if err != nil {
		log.Fatal(err)
	}

	stream, err := object.ReadRange(ctx, 4096, 8192)
	if err != nil {
		log.Fatal(err)
	}
	defer stream.Close()

	n, err := stream.WriteTo(io.Discard)
	fmt.Println(object.Metadata().Size, n, err)
}
