// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"strconv"
	"testing"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk"
)

func TestServeDocument(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset int64
		length int64
		head   bool
		want   int64
	}{
		{name: "first page without metadata", length: racersdk.PageSize, want: racersdk.PageSize},
		{name: "middle page", offset: racersdk.PageSize, length: racersdk.PageSize, want: racersdk.PageSize},
		{name: "final page", offset: 2 * racersdk.PageSize, length: racersdk.PageSize, want: docSize - 2*racersdk.PageSize},
		{name: "head", head: true, length: racersdk.PageSize},
		{name: "zero length", offset: racersdk.PageSize},
		{name: "at end", offset: docSize, length: racersdk.PageSize},
		{name: "past end", offset: docSize + racersdk.PageSize, length: racersdk.PageSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := time.Now()

			metadata, body, err := serveDocument(t.Context(), racersdk.OriginRequest{
				Request: racersdk.Request{Key: sha256.Sum256([]byte("0"))},
				Offset:  tc.offset,
				Length:  tc.length,
				Head:    tc.head,
			})
			if err != nil {
				t.Fatal(err)
			}

			if metadata.Size != docSize || metadata.ETag != `"v1"` || metadata.ContentType != "text/plain; charset=utf-8" {
				t.Fatalf("unexpected metadata: %+v", metadata)
			}

			if metadata.ExpiresAt.Before(before.Add(time.Hour)) || metadata.ExpiresAt.After(time.Now().Add(time.Hour)) {
				t.Fatalf("unexpected expiry: %v", metadata.ExpiresAt)
			}

			if tc.want == 0 {
				if body != nil {
					t.Fatal("HEAD or empty range returned a body")
				}

				return
			}

			if body == nil {
				t.Fatal("nonempty range returned no body")
			}

			defer func() {
				if err := body.Close(); err != nil {
					t.Error(err)
				}
			}()

			n, err := io.Copy(io.Discard, body)
			if err != nil || n != tc.want {
				t.Fatalf("read %d bytes, err=%v; want %d bytes", n, err, tc.want)
			}

			if n, err := body.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
				t.Fatalf("read after end = (%d, %v), want (0, EOF)", n, err)
			}
		})
	}
}

func TestServeDocumentUnknownKey(t *testing.T) {
	for _, key := range []string{"unknown", "racer-demo/doc/0"} {
		t.Run(key, func(t *testing.T) {
			metadata, body, err := serveDocument(t.Context(), racersdk.OriginRequest{
				Request: racersdk.Request{Key: sha256.Sum256([]byte(key)), Metadata: "0"},
				Length:  racersdk.PageSize,
			})
			if !errors.Is(err, racersdk.ErrNotFound) || body != nil || metadata != (racersdk.Metadata{}) {
				t.Fatalf("got (%+v, %v, %v), want empty metadata, nil body, ErrNotFound", metadata, body, err)
			}
		})
	}
}

func TestDocReaderDeterministicSplitReads(t *testing.T) {
	const size = 257

	want, err := io.ReadAll(&docReader{ctx: t.Context(), end: size})
	if err != nil || len(want) != size {
		t.Fatalf("reference read: length=%d err=%v", len(want), err)
	}

	for _, chunk := range []int{1, 7, 63, 64, 65, size} {
		t.Run(strconv.Itoa(chunk), func(t *testing.T) {
			r := &docReader{ctx: t.Context(), end: size}

			var got []byte

			buf := make([]byte, chunk)
			for {
				n, err := r.Read(buf)
				got = append(got, buf[:n]...)

				if errors.Is(err, io.EOF) {
					break
				}

				if err != nil || n == 0 {
					t.Fatalf("read made no progress: n=%d err=%v", n, err)
				}
			}

			if !bytes.Equal(got, want) {
				t.Fatal("content changed with read buffer size")
			}
		})
	}

	got, err := io.ReadAll(&docReader{ctx: t.Context(), offset: 61, end: 193})
	if err != nil || !bytes.Equal(got, want[61:193]) {
		t.Fatalf("range did not match full read: err=%v", err)
	}
}

func TestDocReaderCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	r := &docReader{ctx: ctx, end: docSize}

	buf := make([]byte, 7)
	if n, err := r.Read(buf); n != len(buf) || err != nil {
		t.Fatalf("initial read = (%d, %v)", n, err)
	}

	cancel()

	if n, err := r.Read(buf); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read = (%d, %v), want (0, context.Canceled)", n, err)
	}

	if r.offset != int64(len(buf)) {
		t.Fatalf("canceled read advanced offset to %d", r.offset)
	}
}
