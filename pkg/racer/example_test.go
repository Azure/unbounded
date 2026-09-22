// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"time"

	racer "github.com/Azure/unbounded/pkg/racer"
)

// An immutable in-memory example. A production store can return a pinned file
// descriptor or storage version and release it with Source.Close.
type blobStore struct {
	blobs    map[string][]byte
	metadata map[string]racer.Metadata
}

func newBlobStore(blobs map[string][]byte) *blobStore {
	s := &blobStore{blobs: blobs, metadata: make(map[string]racer.Metadata)}
	for target, data := range blobs {
		// Compute a strong content validator once at publication, never per HEAD.
		ttl := time.Minute
		s.metadata[target] = racer.Metadata{Size: int64(len(data)), ETag: fmt.Sprintf(`"%x"`, sha256.Sum256(data)), TTL: &ttl}
	}

	return s
}

func (s *blobStore) Stat(ctx context.Context, target string) (racer.Metadata, error) {
	if err := ctx.Err(); err != nil {
		return racer.Metadata{}, err
	}

	m, ok := s.metadata[target]
	if !ok {
		return racer.Metadata{}, fs.ErrNotExist
	}

	return m, nil
}

func (s *blobStore) Open(ctx context.Context, target, etag string) (racer.Source, error) {
	m, err := s.Stat(ctx, target)
	if err != nil {
		return nil, err
	}

	if m.ETag != etag {
		return nil, racer.ErrVersionChanged
	}

	return blobSource{bytes.NewReader(s.blobs[target])}, nil
}

type blobSource struct{ *bytes.Reader }

func (blobSource) Close() error { return nil }

func ExampleNewOrigin() { //nolint:testableexamples // Illustrates a long-running server, not a finite output example.
	store := newBlobStore(map[string][]byte{"/hello": []byte("hello, Racer")})

	origin, err := racer.NewOrigin(store)
	if err != nil {
		log.Fatal(err)
	}

	server := &http.Server{Addr: ":8081", Handler: origin, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 90 * time.Second}
	log.Fatal(server.ListenAndServe())
}

func ExampleClient_Open() { //nolint:testableexamples // Requires an external cache serving application data.
	client, err := racer.NewClient("http://cache:8080", racer.ClientOptions{Concurrency: 8})
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

	p := make([]byte, 8192)
	n, err := object.ReadAt(ctx, p, 4096)
	fmt.Println(object.Metadata().Size, n, err)
}
