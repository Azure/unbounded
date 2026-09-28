// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package origin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
)

func TestPullRangeExactBounds(t *testing.T) {
	for _, tc := range []struct {
		name               string
		offset, length     int64
		status             int
		contentRange, data string
		wantErr            bool
	}{
		{"first", 0, 4, 206, "bytes 0-3/10", "0123", false},
		{"middle", 4, 3, 206, "bytes 4-6/10", "456", false},
		{"final", 8, 4, 206, "bytes 8-9/10", "89", false},
		{"ignored", 0, 4, 200, "", "0123456789", true},
		{"wrong end", 0, 4, 206, "bytes 0-4/10", "01234", true},
		{"short end", 0, 4, 206, "bytes 0-2/10", "012", true},
		{"wrong start", 4, 3, 206, "bytes 3-5/10", "345", true},
		{"wrong length", 0, 4, 206, "bytes 0-3/10", "012", true},
		{"unknown total", 0, 4, 206, "bytes 0-3/*", "0123", true},
		{"empty 416", 0, 4, 416, "bytes */0", "error payload", false},
		{"empty 200", 0, 4, 200, "", "", false},
		{"nonzero 416", 4, 4, 416, "bytes */0", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int64

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)

				if r.Method != http.MethodGet || r.Header.Get("Range") != fmt.Sprintf("bytes=%d-%d", tc.offset, tc.offset+tc.length-1) {
					t.Errorf("request = %s %q", r.Method, r.Header.Get("Range"))
				}

				w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
				w.Header().Set("Content-Range", tc.contentRange)
				w.Header().Set("Content-Length", fmt.Sprint(len(tc.data)))
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.data)
			}))
			defer srv.Close()

			c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL})
			ref := ifaces.OriginRef{Registry: "reg", Repository: "image", Digest: digestOf([]byte("0123456789")), Offset: tc.offset}

			body, size, contentType, err := c.PullRange(context.Background(), ref, tc.length)
			if tc.wantErr {
				if err == nil {
					_ = body.Close()

					t.Fatal("accepted invalid response")
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			got, err := io.ReadAll(body)
			_ = body.Close()

			want, total := tc.data, int64(10)
			if strings.HasPrefix(tc.name, "empty") {
				want, total = "", 0
			}

			if err != nil || string(got) != want || size != total || contentType != "application/vnd.oci.image.index.v1+json" || hits.Load() != 1 {
				t.Fatalf("body=%q size=%d type=%q hits=%d err=%v", got, size, contentType, hits.Load(), err)
			}
		})
	}
}

func TestPullRangeInvalidArgumentsBeforeIO(t *testing.T) {
	c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: "http://invalid.example"})
	for _, pair := range [][2]int64{{-1, 4}, {0, 0}, {0, -1}, {math.MaxInt64, 2}} {
		_, _, _, err := c.PullRange(context.Background(), ifaces.OriginRef{Registry: "reg", Repository: "image", Offset: pair[0]}, pair[1])
		if err == nil {
			t.Fatal("accepted invalid range")
		}
	}
}

func TestPullRangeRejectsDifferentDigest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Docker-Content-Digest", digestOf([]byte("wrong")).String())
		w.Header().Set("Content-Range", "bytes 0-3/10")
		w.WriteHeader(206)
		_, _ = io.WriteString(w, "0123")
	}))
	defer srv.Close()

	c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL})

	body, _, _, err := c.PullRange(context.Background(), ifaces.OriginRef{Registry: "reg", Repository: "image", Digest: digestOf([]byte("0123456789"))}, 4)
	if err == nil {
		_ = body.Close()

		t.Fatal("accepted contradictory digest metadata")
	}
}

type rangeRoundTripper func(*http.Request) (*http.Response, error)

func (f rangeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type unreadRangeBody struct{ reads, closes atomic.Int64 }

func (b *unreadRangeBody) Read([]byte) (int, error) {
	b.reads.Add(1)
	return 0, errors.New("unexpected response-body read")
}
func (b *unreadRangeBody) Close() error { b.closes.Add(1); return nil }

func TestPullRangeIgnoredFallbackNeverReadsEntireBody(t *testing.T) {
	c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: "http://registry.example"})
	blob, manifest := &unreadRangeBody{}, &unreadRangeBody{}

	var calls int

	c.registries["reg"].hc.Transport = rangeRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++

		if req.Method != http.MethodGet || req.Header.Get("Range") != "bytes=0-3" {
			t.Fatalf("unexpected request: %s %s", req.Method, req.Header.Get("Range"))
		}

		status, body := 404, blob
		if strings.Contains(req.URL.Path, "/manifests/") {
			status, body = 200, manifest
		}

		return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: make(http.Header), ContentLength: 1 << 30, Body: body, Request: req}, nil
	})

	body, _, _, err := c.PullRange(context.Background(), ifaces.OriginRef{Registry: "reg", Repository: "image"}, 4)
	if err == nil || body != nil {
		t.Fatalf("ignored range accepted: %v", err)
	}

	if calls != 2 || blob.reads.Load() != 0 || manifest.reads.Load() != 0 || blob.closes.Load() != 1 || manifest.closes.Load() != 1 {
		t.Fatal("fallback performed extra I/O or drained a rejected body")
	}
}

func TestPullRangeRejectsUnboundedCompleteResponsesWithoutReading(t *testing.T) {
	for _, tc := range []struct {
		name         string
		offset, size int64
	}{
		{"oversized", 0, 5},
		{"unknown length", 0, -1},
		{"nonzero offset", 4, 4},
		{"nonzero empty", 4, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: "http://registry.example"})
			upstream := &unreadRangeBody{}
			c.registries["reg"].hc.Transport = rangeRoundTripper(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), ContentLength: tc.size, Body: upstream, Request: req}, nil
			})

			body, _, _, err := c.PullRange(context.Background(), ifaces.OriginRef{Registry: "reg", Repository: "image", Offset: tc.offset}, 4)
			if err == nil || body != nil || upstream.reads.Load() != 0 || upstream.closes.Load() != 1 {
				t.Fatalf("ignored range: error=%v reads=%d closes=%d", err, upstream.reads.Load(), upstream.closes.Load())
			}
		})
	}
}

func TestPullRangeCompleteManifestResponse(t *testing.T) {
	const data = `{"schemaVersion":2}`

	for _, kind := range []ifaces.OriginRefKind{ifaces.KindManifest, ifaces.KindBlob} {
		for _, variant := range []string{"short final", "exact length", "truncated"} {
			t.Run(kind.String()+"/"+variant, func(t *testing.T) {
				var hits atomic.Int64

				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)

					if r.Method != http.MethodGet {
						t.Error("unexpected HEAD")
					}

					if strings.Contains(r.URL.Path, "/blobs/") {
						w.WriteHeader(404)
						return
					}

					if r.Header.Get("Accept") == "" {
						t.Error("missing manifest Accept")
					}
					// Like Distribution GetManifest, ignore Range and write complete 200.
					w.Header().Set("Content-Length", fmt.Sprint(len(data)))
					w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
					w.Header().Set("Docker-Content-Digest", digestOf([]byte(data)).String())
					w.WriteHeader(http.StatusOK)

					payload := data
					if variant == "truncated" {
						payload = data[:len(data)-1]
					}

					_, _ = io.WriteString(w, payload)
				}))
				defer srv.Close()

				c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL})

				length := int64(len(data) + 10)
				if variant == "exact length" {
					length = int64(len(data))
				}

				body, size, contentType, err := c.PullRange(context.Background(), ifaces.OriginRef{Registry: "reg", Repository: "image", Kind: kind, Digest: digestOf([]byte(data))}, length)
				if err != nil {
					t.Fatal(err)
				}

				got, err := io.ReadAll(body)
				_ = body.Close()

				if variant == "truncated" {
					if !errors.Is(err, io.ErrUnexpectedEOF) {
						t.Fatalf("truncated body error=%v", err)
					}
				} else if err != nil || string(got) != data {
					t.Fatalf("body=%q error=%v", got, err)
				}

				wantHits := int64(1)
				if kind == ifaces.KindBlob {
					wantHits++
				}

				if size != int64(len(data)) || contentType != "application/vnd.oci.image.manifest.v1+json" || hits.Load() != wantHits {
					t.Fatalf("size=%d type=%q requests=%d", size, contentType, hits.Load())
				}
			})
		}
	}
}

func TestPullRangeDelegatedAuthAndFallbackStatuses(t *testing.T) {
	for _, status := range []int{206, 401, 403, 404, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var hits atomic.Int64

			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)

				if r.Header.Get("Authorization") != "Bearer private-token" || r.Header.Get("Range") != "bytes=4-6" {
					t.Error("lost credential or bounded range")
				}

				if strings.Contains(r.URL.Path, "/blobs/") {
					w.WriteHeader(404)
					return
				}

				if r.Header.Get("Accept") == "" {
					t.Error("missing manifest Accept")
				}

				w.Header().Set("Content-Range", "bytes 4-6/10")
				w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
				w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "456")
			}))
			defer srv.Close()

			c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL})
			c.registries["reg"].hc = srv.Client()
			ctx := registryauth.WithAuthorization(context.Background(), "Bearer private-token")

			body, _, _, err := c.PullRange(ctx, ifaces.OriginRef{Registry: "reg", Repository: "image", Offset: 4}, 3)
			if status == 206 {
				if err != nil {
					t.Fatal(err)
				}

				_ = body.Close()
			} else {
				var oe *ifaces.OriginError
				if !errors.As(err, &oe) || oe.StatusCode != status || strings.Contains(err.Error(), "private-token") {
					t.Fatalf("status lost: %v", err)
				}
			}

			if hits.Load() != 2 {
				t.Fatalf("hits=%d", hits.Load())
			}
		})
	}
}

func TestPullRangeHTTP1PoolReuse(t *testing.T) {
	const parallel = 8

	var (
		connections atomic.Int64
		arrived     atomic.Int64
	)

	release := make(chan struct{})
	ready := make(chan struct{})
	releaseSecond := make(chan struct{})
	readySecond := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 {
			t.Error("expected HTTP/1")
		}

		n := arrived.Add(1)
		if n == parallel {
			close(ready)
		}

		if n <= parallel {
			<-release
		}

		if n == 2*parallel {
			close(readySecond)
		}

		if n > parallel && n <= 2*parallel {
			<-releaseSecond
		}

		w.Header().Set("Content-Range", "bytes 0-3/10")
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(206)
		_, _ = io.WriteString(w, "0123")
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}

	srv.Start()
	defer srv.Close()

	c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL})

	transport := c.registries["reg"].hc.Transport.(*http.Transport)
	defer transport.CloseIdleConnections()

	if !transport.ForceAttemptHTTP2 || transport.MaxConnsPerHost <= 0 || transport.MaxIdleConnsPerHost < parallel {
		t.Fatal("unbounded or undersized pool")
	}

	var wg sync.WaitGroup
	for range parallel {
		wg.Add(1)

		go func() {
			defer wg.Done()

			body, _, _, err := c.PullRange(context.Background(), ifaces.OriginRef{Registry: "reg", Repository: "image"}, 4)
			if err != nil {
				t.Error(err)
				return
			}

			_, err = io.Copy(io.Discard, body)
			_ = body.Close()

			if err != nil {
				t.Error(err)
			}
		}()
	}
	// All first-wave connections are opened before any is returned to the pool.
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("parallel requests did not arrive")
	}

	close(release)
	wg.Wait()

	before := connections.Load()

	for range parallel {
		wg.Add(1)

		go func() {
			defer wg.Done()

			body, _, _, err := c.PullRange(context.Background(), ifaces.OriginRef{Registry: "reg", Repository: "image"}, 4)
			if err != nil {
				t.Error(err)
				return
			}

			_, _ = io.Copy(io.Discard, body)
			_ = body.Close()
		}()
	}

	select {
	case <-readySecond:
	case <-time.After(5 * time.Second):
		close(releaseSecond)
		t.Fatal("second wave did not arrive")
	}

	close(releaseSecond)
	wg.Wait()

	if connections.Load() != before {
		t.Fatalf("connection churn: %d -> %d", before, connections.Load())
	}
}
