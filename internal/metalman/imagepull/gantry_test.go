// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package imagepull_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	"github.com/Azure/unbounded/internal/gantry/origin"
	"github.com/Azure/unbounded/internal/metalman/imagepull"
	ipt "github.com/Azure/unbounded/internal/metalman/imagepull/imagepulltest"
)

type gantryRequest struct {
	path, ns, rng string
	status        int
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// startGantry starts an in-process Gantry mirror in front of upstream and
// records the requests it receives.
func startGantry(t *testing.T, upstream *ipt.Registry) (*httptest.Server, func() []gantryRequest) {
	t.Helper()

	cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{{
		Name:     upstream.Host(),
		Endpoint: upstream.URL(),
	}}}

	oc, err := origin.New(cfg)
	if err != nil {
		t.Fatalf("origin.New: %v", err)
	}

	h := mirror.New(cfg, fakes.NewCache(), oc).Handler()

	var (
		mu   sync.Mutex
		reqs []gantryRequest
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)

		mu.Lock()

		reqs = append(reqs, gantryRequest{path: r.URL.Path, ns: r.URL.Query().Get("ns"), rng: r.Header.Get("Range"), status: rec.status})
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)

	return srv, func() []gantryRequest {
		mu.Lock()
		defer mu.Unlock()

		return append([]gantryRequest(nil), reqs...)
	}
}

func TestGantryMirror(t *testing.T) {
	upstream := ipt.NewRegistry(t)
	content := randomBytes(20*1024, 11)
	ref, indexDigest := upstream.Push(t, "netboot", "v1", ipt.ImageSpec{Platforms: map[string][]ipt.Layer{
		"amd64": {{Files: map[string][]byte{"disk/vmlinuz": content}}},
		"arm64": {{Files: map[string][]byte{"disk/vmlinuz": []byte("arm")}}},
	}})

	gantry, gantryReqs := startGantry(t, upstream)
	p := &imagepull.Puller{Mirror: gantry.URL, ChunkSize: 1024, Concurrency: 4}

	// Tag ref: Gantry answers 503 for tags, so the tag resolves upstream.
	img := resolve(t, p, imagepull.Source{Ref: ref})
	layer := upstream.LayerDigests(img.Digest())[0]

	data, _ := readFile(t, img, "disk/vmlinuz")
	if !bytes.Equal(data, content) {
		t.Fatal("content mismatch via Gantry")
	}

	var tagMiss, blobHits int

	for _, r := range gantryReqs() {
		if r.ns != upstream.Host() {
			t.Errorf("Gantry request %s has ns=%q, want %q", r.path, r.ns, upstream.Host())
		}

		if strings.HasSuffix(r.path, "/manifests/v1") && r.status == http.StatusServiceUnavailable {
			tagMiss++
		}

		if strings.HasSuffix(r.path, "/blobs/"+layer) && r.status == http.StatusOK {
			blobHits++
		}
	}

	if tagMiss != 1 {
		t.Errorf("Gantry tag 503s = %d, want 1", tagMiss)
	}

	// Gantry ignores bounded ranges and answers 200, so the blob is
	// streamed with a single request.
	if blobHits != 1 {
		t.Errorf("Gantry blob 200s = %d, want 1", blobHits)
	}

	// The tag was resolved upstream; manifests by digest and the blob went
	// through Gantry, which fetched them from upstream without ns.
	for _, r := range upstream.Requests() {
		if r.Kind == ipt.KindManifest && r.Ref != "v1" && r.Query.Get("ns") != "" {
			t.Errorf("unexpected direct upstream request %+v", r)
		}
	}

	upstreamBlobReqs := len(upstream.BlobRequests(layer))
	if upstreamBlobReqs == 0 {
		t.Fatal("Gantry never fetched the layer from upstream")
	}

	// Digest ref: everything is served by Gantry, the blob from its cache.
	upstream.ResetRequests()

	img2 := resolve(t, p, imagepull.Source{Ref: upstream.Host() + "/netboot@" + indexDigest})
	if img2.Digest() != img.Digest() {
		t.Fatalf("digest ref resolved to %s, want %s", img2.Digest(), img.Digest())
	}

	if data, _ := readFile(t, img2, "disk/vmlinuz"); !bytes.Equal(data, content) {
		t.Fatal("content mismatch on cached pull")
	}

	if n := len(upstream.BlobRequests(layer)); n != 0 {
		t.Errorf("upstream blob requests on cached pull = %d, want 0", n)
	}

	// The index was only ever fetched by tag from upstream, so Gantry pulls
	// it by digest now; the platform manifest comes from Gantry's cache.
	for _, r := range upstream.Requests() {
		if r.Kind == ipt.KindManifest && r.Ref != indexDigest {
			t.Errorf("unexpected upstream manifest request on cached pull: %+v", r)
		}
	}
}

func TestMirrorDownFallsBackToUpstream(t *testing.T) {
	upstream := ipt.NewRegistry(t)
	content := randomBytes(4*1024, 13)
	ref, dgst := upstream.PushFiles(t, "netboot", "v1", map[string][]byte{"disk/vmlinuz": content})

	gantry, _ := startGantry(t, upstream)
	mirrorURL := gantry.URL
	gantry.Close()

	p := &imagepull.Puller{Mirror: mirrorURL, ChunkSize: 1024}

	img := resolve(t, p, imagepull.Source{Ref: upstream.Host() + "/netboot@" + dgst})
	if data, _ := readFile(t, img, "disk/vmlinuz"); !bytes.Equal(data, content) {
		t.Fatal("content mismatch")
	}

	img = resolve(t, p, imagepull.Source{Ref: ref})
	if data, _ := readFile(t, img, "disk/vmlinuz"); !bytes.Equal(data, content) {
		t.Fatal("content mismatch")
	}

	if n := len(upstream.BlobRequests(upstream.LayerDigests(dgst)[0])); n < 4 {
		t.Fatalf("upstream blob requests = %d, want ranged chunks from upstream", n)
	}
}

func TestMirrorUnknownNamespaceFallsBack(t *testing.T) {
	upstream := ipt.NewRegistry(t)
	other := ipt.NewRegistry(t)
	ref, _ := upstream.PushFiles(t, "netboot", "v1", map[string][]byte{"disk/a": []byte("a")})

	// Gantry configured for a different registry 404s ns=upstream.
	gantry, gantryReqs := startGantry(t, other)
	p := &imagepull.Puller{Mirror: gantry.URL}

	img := resolve(t, p, imagepull.Source{Ref: ref})
	if data, _ := readFile(t, img, "disk/a"); string(data) != "a" {
		t.Fatalf("data = %q", data)
	}

	if len(gantryReqs()) == 0 {
		t.Fatal("mirror was not tried")
	}
}
