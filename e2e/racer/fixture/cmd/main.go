// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// The kind smoke pod serves an SDK origin and verifies reads through Racer.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk"
)

const pageSize = racersdk.PageSize

type store struct {
	data  []byte
	opens atomic.Int64
}

func (s *store) Stat(_ context.Context, target string, _ []byte) (racersdk.Metadata, error) {
	if target != "/smoke" {
		return racersdk.Metadata{}, fs.ErrNotExist
	}

	ttl := time.Hour

	return racersdk.Metadata{Size: int64(len(s.data)), ETag: fmt.Sprintf(`"%x"`, sha256.Sum256(s.data)), ContentType: "application/octet-stream", TTL: &ttl}, nil
}

func (s *store) ResolveRange(ctx context.Context, target string, data []byte) (racersdk.ResolvedRange, error) {
	m, err := s.Stat(ctx, target, data)
	if err != nil {
		return nil, err
	}

	return &resolved{store: s, meta: m}, nil
}

type resolved struct {
	store *store
	meta  racersdk.Metadata
}

func (r *resolved) Metadata() racersdk.Metadata { return r.meta }
func (r *resolved) Close() error                { return nil }
func (r *resolved) OpenRange(_ context.Context, offset, length int64) (io.ReadCloser, error) {
	r.store.opens.Add(1)
	return io.NopCloser(io.NewSectionReader(bytes.NewReader(r.store.data), offset, length)), nil
}

func run() error {
	s := &store{data: make([]byte, 2*pageSize+17)}
	for i := range s.data {
		s.data[i] = byte((i*31 + 17) % 251)
	}

	origin, err := racersdk.NewRangeOrigin(s)
	if err != nil {
		return err
	}

	if err := os.MkdirAll("/run/racer/smoke/origin", 0o770); err != nil {
		return err
	}

	listener, err := net.Listen("unix", "/run/racer/smoke/origin/socket")
	if err != nil {
		return err
	}

	server := &http.Server{Handler: origin, ReadHeaderTimeout: 5 * time.Second}

	defer func() {
		if err := server.Close(); err != nil {
			log.Printf("close origin: %v", err)
		}
	}()

	if err := os.Chmod(listener.Addr().String(), 0o660); err != nil {
		return err
	}

	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("serve origin: %v", err)
		}
	}()

	client, err := racersdk.NewClient("/run/racer/smoke/client/socket", racersdk.ClientOptions{})
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	for i, span := range [][2]int64{{0, int64(len(s.data))}, {0, int64(len(s.data))}, {pageSize - 5, 17}} {
		object, err := client.Open(ctx, "/smoke")
		if err != nil {
			return err
		}

		if object.Metadata().Size != int64(len(s.data)) {
			return fmt.Errorf("wrong object size: %+v", object.Metadata())
		}

		stream, err := object.ReadRange(ctx, span[0], span[1])
		if err != nil {
			return err
		}

		var out bytes.Buffer

		n, err := stream.WriteTo(&out)
		closeErr := stream.Close()

		if err != nil {
			return err
		}

		if closeErr != nil {
			return closeErr
		}

		if n != span[1] || !bytes.Equal(out.Bytes(), s.data[span[0]:span[0]+span[1]]) {
			return fmt.Errorf("read %d: incorrect bytes (length %d)", i, n)
		}

		if got := s.opens.Load(); got != 3 {
			return fmt.Errorf("read %d: origin payload fetches = %d, want 3", i, got)
		}
	}

	log.Print("verified SDK cold read, warm cache reuse, and cross-page range")

	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
