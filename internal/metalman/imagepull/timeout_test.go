// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package imagepull_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/metalman/imagepull"
	ipt "github.com/Azure/unbounded/internal/metalman/imagepull/imagepulltest"
)

type stallMode int

const (
	stallNone stallMode = iota
	stallManifestHeaders
	stallBlobHeaders
	stallBlobBody
)

// stallingMirror proxies to upstream and stalls according to mode until
// the client gives up. It counts stalled and served requests.
func stallingMirror(t *testing.T, upstream *ipt.Registry, mode stallMode) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()

	var stalls, served atomic.Int32

	done := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isBlob := strings.Contains(r.URL.Path, "/blobs/")
		isManifest := strings.Contains(r.URL.Path, "/manifests/")

		wait := func() {
			stalls.Add(1)

			select {
			case <-r.Context().Done():
			case <-done:
			}
		}

		if (mode == stallManifestHeaders && isManifest) || (mode == stallBlobHeaders && isBlob) {
			wait()
			return
		}

		req, err := http.NewRequestWithContext(r.Context(), r.Method, upstream.URL()+r.URL.Path, nil)
		if err != nil {
			t.Error(err)
			return
		}

		for _, h := range []string{"Accept", "Range"} {
			if v := r.Header.Get(h); v != "" {
				req.Header.Set(h, v)
			}
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close() //nolint:errcheck // test

		body, _ := io.ReadAll(resp.Body)

		for k, v := range resp.Header {
			w.Header()[k] = v
		}

		w.WriteHeader(resp.StatusCode)
		served.Add(1)

		if mode == stallBlobBody && isBlob && len(body) > 1 {
			_, _ = w.Write(body[:len(body)/2])
			w.(http.Flusher).Flush()
			wait()

			return
		}

		_, _ = w.Write(body)
	}))

	t.Cleanup(func() {
		close(done)
		srv.Close()
	})

	return srv, &stalls, &served
}

// digestRef turns host/repo:tag into host/repo@dgst.
func digestRef(ref, dgst string) string {
	return ref[:strings.LastIndex(ref, ":")] + "@" + dgst
}

func TestMirrorTimeouts(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode stallMode
	}{
		{"manifest headers stall", stallManifestHeaders},
		{"blob headers stall", stallBlobHeaders},
		{"blob body stall", stallBlobBody},
		{"healthy mirror", stallNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := ipt.NewRegistry(t)
			ref, content, layer := pushBigUncompressed(t, upstream, 16*1024)
			mirror, stalls, served := stallingMirror(t, upstream, tc.mode)

			p := &imagepull.Puller{
				Mirror:                  mirror.URL,
				ChunkSize:               4096,
				Concurrency:             1,
				MirrorManifestTimeout:   100 * time.Millisecond,
				MirrorBlobHeaderTimeout: 100 * time.Millisecond,
				MirrorIdleTimeout:       100 * time.Millisecond,
			}

			// Resolve the tag upstream to learn the digest, then resolve
			// by digest so the manifest is requested from the mirror.
			tagged := resolve(t, &imagepull.Puller{}, imagepull.Source{Ref: ref})

			// Bound the whole operation well under the stall durations a
			// broken implementation would hit, but well above the bounds.
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

			start := time.Now()

			img, err := p.Resolve(ctx, imagepull.Source{Ref: digestRef(ref, tagged.Digest())})
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}

			data, _, err := img.ReadFile(ctx, "disk/big")
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}

			if !bytes.Equal(data, content) {
				t.Fatal("content mismatch")
			}

			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Fatalf("took %s, want fallback within the mirror bounds", elapsed)
			}

			upstreamBlobs := 0

			for _, r := range upstream.BlobRequests(layer) {
				if strings.Contains(r.UserAgent, "unbounded-metalman-imagepull") {
					upstreamBlobs++
				}
			}

			switch tc.mode {
			case stallNone:
				if stalls.Load() != 0 || upstreamBlobs != 0 {
					t.Fatalf("healthy mirror: stalls=%d upstream blob requests=%d", stalls.Load(), upstreamBlobs)
				}

				if served.Load() == 0 {
					t.Fatal("mirror served nothing")
				}
			case stallManifestHeaders:
				if stalls.Load() == 0 {
					t.Fatal("mirror never stalled")
				}
			default:
				if stalls.Load() == 0 || upstreamBlobs == 0 {
					t.Fatalf("stalls=%d upstream blob requests=%d", stalls.Load(), upstreamBlobs)
				}
			}
		})
	}
}

// A slow consumer must not trip the idle timeout: it only measures time
// spent blocked in Read.
func TestMirrorIdleTimeoutIgnoresSlowConsumer(t *testing.T) {
	upstream := ipt.NewRegistry(t)
	ref, content, layer := pushBigUncompressed(t, upstream, 16*1024)
	mirror, _, _ := stallingMirror(t, upstream, stallNone)

	p := &imagepull.Puller{
		Mirror:            mirror.URL,
		Concurrency:       1,
		MirrorIdleTimeout: 50 * time.Millisecond,
	}

	tagged := resolve(t, &imagepull.Puller{}, imagepull.Source{Ref: ref})
	img := resolve(t, p, imagepull.Source{Ref: digestRef(ref, tagged.Digest())})

	f, err := img.Open(t.Context(), "disk/big")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // test

	var got bytes.Buffer

	buf := make([]byte, 4096)

	for {
		n, err := f.Read(buf)
		got.Write(buf[:n])

		if err == io.EOF {
			break
		}

		if err != nil {
			t.Fatal(err)
		}

		time.Sleep(120 * time.Millisecond)
	}

	if !bytes.Equal(got.Bytes(), content) {
		t.Fatal("content mismatch")
	}

	for _, r := range upstream.BlobRequests(layer) {
		if strings.Contains(r.UserAgent, "unbounded-metalman-imagepull") {
			t.Fatalf("unexpected upstream blob request: %+v", r)
		}
	}
}
