// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package imagepull_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Azure/unbounded/internal/metalman/imagepull"
	ipt "github.com/Azure/unbounded/internal/metalman/imagepull/imagepulltest"
)

// cuttingMirror proxies to upstream and cuts blob responses: in full mode
// it answers 200 with the full Content-Length but sends only half of the
// blob; in ranged mode it serves the first chunk and then drops the
// connection for every later range.
func cuttingMirror(t *testing.T, upstream *ipt.Registry, ranged bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	var cuts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequestWithContext(r.Context(), r.Method, upstream.URL()+r.URL.Path, nil)
		if err != nil {
			t.Error(err)
			return
		}

		isBlob := strings.Contains(r.URL.Path, "/blobs/")
		rng := r.Header.Get("Range")

		if ranged && isBlob && rng != "" && !strings.HasPrefix(rng, "bytes=0-") {
			cuts.Add(1)

			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}

			return
		}

		if ranged && isBlob {
			req.Header.Set("Range", rng)
		}

		req.Header.Set("Accept", r.Header.Get("Accept"))

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		defer resp.Body.Close() //nolint:errcheck // test

		body, _ := io.ReadAll(resp.Body)

		for k, v := range resp.Header {
			w.Header()[k] = v
		}

		w.WriteHeader(resp.StatusCode)

		if !ranged && isBlob && resp.StatusCode == http.StatusOK {
			cuts.Add(1)

			_, _ = w.Write(body[:len(body)/2])

			return
		}

		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	return srv, &cuts
}

func TestMirrorMidStreamFailureResumesUpstream(t *testing.T) {
	for _, tc := range []struct {
		name          string
		ranged        bool
		upstreamRange bool
	}{
		{"mirror stream cut, upstream ranged", false, true},
		{"mirror stream cut, upstream without Range", false, false},
		{"mirror chunk cut, upstream ranged", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var opts []ipt.Option
			if !tc.upstreamRange {
				opts = append(opts, ipt.WithoutRange())
			}

			upstream := ipt.NewRegistry(t, opts...)
			ref, content, layer := pushBigUncompressed(t, upstream, 16*1024)
			mirror, cuts := cuttingMirror(t, upstream, tc.ranged)
			p := &imagepull.Puller{Mirror: mirror.URL, ChunkSize: 1024, Concurrency: 4}

			img := resolve(t, p, imagepull.Source{Ref: ref})
			if data, _ := readFile(t, img, "disk/big"); !bytes.Equal(data, content) {
				t.Fatal("content mismatch")
			}

			if cuts.Load() == 0 {
				t.Fatal("mirror never cut a response")
			}

			reqs := upstream.BlobRequests(layer)

			var resumed bool

			for _, r := range reqs {
				if !strings.Contains(r.UserAgent, "unbounded-metalman-imagepull") {
					continue
				}

				if !tc.upstreamRange || (r.Range != "" && !strings.HasPrefix(r.Range, "bytes=0-")) {
					resumed = true
				}
			}

			if !resumed {
				t.Fatalf("no resumed upstream request: %+v", reqs)
			}
		})
	}
}

func TestChunkRetry(t *testing.T) {
	var failed atomic.Bool

	reg := ipt.NewRegistry(t, ipt.WithMiddleware(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if rng := r.Header.Get("Range"); rng != "" && !strings.HasPrefix(rng, "bytes=0-") && failed.CompareAndSwap(false, true) {
				http.Error(w, "busy", http.StatusServiceUnavailable)
				return
			}

			next.ServeHTTP(w, r)
		})
	}))
	ref, content, _ := pushBigUncompressed(t, reg, 8*1024)

	img := resolve(t, &imagepull.Puller{ChunkSize: 1024}, imagepull.Source{Ref: ref})
	if data, _ := readFile(t, img, "disk/big"); !bytes.Equal(data, content) {
		t.Fatal("content mismatch")
	}

	if !failed.Load() {
		t.Fatal("no chunk request failed")
	}
}
