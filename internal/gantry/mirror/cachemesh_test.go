// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	"github.com/Azure/unbounded/internal/gantry/origin"
)

// fakeFetcher serves blobs it holds from ref.Offset, optionally failing
// after failAfter bytes, and declines everything else.
type fakeFetcher struct {
	mu        sync.Mutex
	blobs     map[digest.Digest][]byte
	refs      []ifaces.OriginRef
	failAfter int
}

func (f *fakeFetcher) FetchBlob(_ context.Context, w io.Writer, ref ifaces.OriginRef, start func(int64, string)) (bool, int64, error) {
	f.mu.Lock()
	f.refs = append(f.refs, ref)
	body, ok := f.blobs[ref.Digest]
	f.mu.Unlock()

	if !ok {
		return false, 0, nil
	}

	start(int64(len(body)), "")

	part := body[ref.Offset:]
	if f.failAfter > 0 && f.failAfter < len(part) {
		n, _ := w.Write(part[:f.failAfter])
		return true, int64(n), errors.New("mesh broke")
	}

	n, err := w.Write(part)

	return true, int64(n), err
}

type fakeTags struct {
	calls atomic.Int32
	body  []byte
	err   error
}

func (f *fakeTags) ResolveTag(context.Context, string, string, string) ([]byte, string, digest.Digest, error) {
	f.calls.Add(1)

	if f.err != nil {
		return nil, "", digest.Digest{}, f.err
	}

	return f.body, "application/vnd.oci.image.manifest.v1+json", digestOf(f.body), nil
}

type sourceBytes struct {
	mu sync.Mutex
	m  map[string]int64
}

func (s *sourceBytes) add(_, source string, n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.m[source] += n
}

func (s *sourceBytes) get(source string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.m[source]
}

// cachemeshFixture is newFixture with optional cachemesh sources.
func cachemeshFixture(t *testing.T, blobs map[digest.Digest][]byte, opts ...mirror.Option) (*httptest.Server, *int32, *sourceBytes) {
	t.Helper()

	var hits int32

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)

		for d, body := range blobs {
			if len(r.URL.Path) > 71 && r.URL.Path[len(r.URL.Path)-71:] == d.String() {
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				_, _ = w.Write(body)

				return
			}
		}

		http.NotFound(w, r)
	}))
	t.Cleanup(up.Close)

	cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "reg.example.com", Endpoint: up.URL}}}

	oc, err := origin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	bytesBySource := &sourceBytes{m: map[string]int64{}}
	opts = append(opts, mirror.WithByteMetrics(bytesBySource.add))

	srv := httptest.NewServer(mirror.New(cfg, fakes.NewCache(), oc, opts...).Handler())
	t.Cleanup(srv.Close)

	return srv, &hits, bytesBySource
}

func get(t *testing.T, url string, header map[string]string) (*http.Response, []byte, error) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}

	for k, v := range header {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)

	return resp, body, err
}

func TestMirrorBlobFetcherServesBeforeOrigin(t *testing.T) {
	body := []byte("layer bytes served through the mesh")
	d := digestOf(body)
	fetcher := &fakeFetcher{blobs: map[digest.Digest][]byte{d: body}}

	srv, hits, bySource := cachemeshFixture(t, map[digest.Digest][]byte{d: body}, mirror.WithBlobFetcher(fetcher))

	resp, got, err := get(t, srv.URL+"/v2/library/app/blobs/"+d.String(), nil)
	if err != nil || resp.StatusCode != http.StatusOK || string(got) != string(body) {
		t.Fatalf("GET: status=%v err=%v body=%q", resp, err, got)
	}

	if resp.Header.Get("Docker-Content-Digest") != d.String() || resp.Header.Get("Content-Length") != strconv.Itoa(len(body)) {
		t.Fatalf("headers: %v", resp.Header)
	}

	if atomic.LoadInt32(hits) != 0 {
		t.Fatalf("origin hit %d times", *hits)
	}

	if bySource.get("cachemesh") != int64(len(body)) {
		t.Fatalf("cachemesh bytes = %d", bySource.get("cachemesh"))
	}

	// Open-ended Range (containerd resume) also goes through the fetcher.
	resp, got, err = get(t, srv.URL+"/v2/library/app/blobs/"+d.String(), map[string]string{"Range": "bytes=6-"})
	if err != nil || resp.StatusCode != http.StatusPartialContent || string(got) != string(body[6:]) {
		t.Fatalf("Range: resp=%v err=%v body=%q", resp, err, got)
	}

	want := "bytes 6-" + strconv.Itoa(len(body)-1) + "/" + strconv.Itoa(len(body))
	if resp.Header.Get("Content-Range") != want {
		t.Fatalf("Content-Range %q, want %q", resp.Header.Get("Content-Range"), want)
	}

	if last := fetcher.refs[len(fetcher.refs)-1]; last.Offset != 6 || last.Registry != "reg.example.com" || last.Repository != "library/app" {
		t.Fatalf("fetcher ref %+v", last)
	}
}

func TestMirrorBlobFetcherDeclineFallsBackToOrigin(t *testing.T) {
	body := []byte("only the origin has these bytes")
	d := digestOf(body)
	fetcher := &fakeFetcher{blobs: map[digest.Digest][]byte{}}

	srv, hits, bySource := cachemeshFixture(t, map[digest.Digest][]byte{d: body}, mirror.WithBlobFetcher(fetcher))

	resp, got, err := get(t, srv.URL+"/v2/library/app/blobs/"+d.String(), nil)
	if err != nil || resp.StatusCode != http.StatusOK || string(got) != string(body) {
		t.Fatalf("GET: resp=%v err=%v", resp, err)
	}

	if atomic.LoadInt32(hits) == 0 || len(fetcher.refs) != 1 || bySource.get("cachemesh") != 0 {
		t.Fatalf("hits=%d fetcher calls=%d cachemesh bytes=%d", *hits, len(fetcher.refs), bySource.get("cachemesh"))
	}
}

func TestMirrorBlobFetcherAbortsTruncatedStream(t *testing.T) {
	body := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	d := digestOf(body)
	fetcher := &fakeFetcher{blobs: map[digest.Digest][]byte{d: body}, failAfter: 10}

	srv, hits, _ := cachemeshFixture(t, map[digest.Digest][]byte{d: body}, mirror.WithBlobFetcher(fetcher))

	_, got, err := get(t, srv.URL+"/v2/library/app/blobs/"+d.String(), nil)
	if err == nil {
		t.Fatalf("want a broken body, got %d bytes and no error", len(got))
	}

	if atomic.LoadInt32(hits) != 0 {
		t.Fatal("aborted cachemesh stream must not fall back to origin mid-response")
	}
}

func TestMirrorWithoutCachemeshKeepsDefaults(t *testing.T) {
	body := []byte("default path")
	d := digestOf(body)

	srv, hits, bySource := cachemeshFixture(t, map[digest.Digest][]byte{d: body})

	resp, got, err := get(t, srv.URL+"/v2/library/app/blobs/"+d.String(), nil)
	if err != nil || resp.StatusCode != http.StatusOK || string(got) != string(body) || atomic.LoadInt32(hits) == 0 {
		t.Fatalf("blob GET: resp=%v err=%v hits=%d", resp, err, *hits)
	}

	if bySource.get("cachemesh") != 0 {
		t.Fatal("cachemesh bytes without a fetcher")
	}

	resp, _, err = get(t, srv.URL+"/v2/library/app/manifests/latest", nil)
	if err != nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("tag GET without resolver: resp=%v err=%v", resp, err)
	}
}

func TestMirrorTagResolver(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2}`)
	tags := &fakeTags{body: manifest}

	srv, _, bySource := cachemeshFixture(t, nil, mirror.WithTagResolver(tags))

	resp, got, err := get(t, srv.URL+"/v2/library/app/manifests/v1", nil)
	if err != nil || resp.StatusCode != http.StatusOK || string(got) != string(manifest) {
		t.Fatalf("tag GET: resp=%v err=%v", resp, err)
	}

	if resp.Header.Get("Docker-Content-Digest") != digestOf(manifest).String() ||
		resp.Header.Get("Content-Type") != "application/vnd.oci.image.manifest.v1+json" {
		t.Fatalf("headers: %v", resp.Header)
	}

	if bySource.get("cachemesh") != int64(len(manifest)) {
		t.Fatalf("cachemesh bytes = %d", bySource.get("cachemesh"))
	}

	// HEAD answers without a body.
	req, _ := http.NewRequest(http.MethodHead, srv.URL+"/v2/library/app/manifests/v1", nil)

	head, err := http.DefaultClient.Do(req)
	if err != nil || head.StatusCode != http.StatusOK || head.Header.Get("Docker-Content-Digest") == "" {
		t.Fatalf("HEAD: resp=%v err=%v", head, err)
	}

	_ = head.Body.Close()

	// Requests with credentials never use the shared cache.
	calls := tags.calls.Load()

	resp, _, err = get(t, srv.URL+"/v2/library/app/manifests/v1", map[string]string{"Authorization": "Bearer abc"})
	if err != nil || resp.StatusCode != http.StatusServiceUnavailable || tags.calls.Load() != calls {
		t.Fatalf("credentialed tag GET: resp=%v err=%v calls=%d", resp, err, tags.calls.Load()-calls)
	}

	// A failing resolver keeps the default 503.
	tags.err = errors.New("no route")

	resp, _, err = get(t, srv.URL+"/v2/library/app/manifests/v1", nil)
	if err != nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("failing resolver: resp=%v err=%v", resp, err)
	}
}
