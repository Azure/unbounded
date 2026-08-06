// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
)

func mustDigest(b []byte) digest.Digest {
	sum := sha256.Sum256(b)
	return digest.MustParse("sha256:" + hex.EncodeToString(sum[:]))
}

func newTestServer(t *testing.T) (*httptest.Server, *fakes.Cache, *int) {
	t.Helper()

	cache := fakes.NewCache()
	served := 0
	s := New(cache, WithMetrics(
		func() { served++ },
		nil,
	))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	return ts, cache, &served
}

func TestV2Root(t *testing.T) {
	ts, _, _ := newTestServer(t)

	resp, err := http.Get(ts.URL + "/v2/")
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	if got := resp.Header.Get("Docker-Distribution-API-Version"); got != "registry/2.0" {
		t.Errorf("API-Version header = %q, want registry/2.0", got)
	}
}

func TestRequiresMirroredHeader(t *testing.T) {
	ts, cache, _ := newTestServer(t)
	body := []byte("hello")
	d := mustDigest(body)
	cache.Put(d, body)

	resp, err := http.Get(ts.URL + "/v2/myrepo/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (missing %s header)", resp.StatusCode, MirroredHeader)
	}
}

func TestServeFromCache(t *testing.T) {
	ts, cache, served := newTestServer(t)
	body := []byte("hello world, this is a peer-served blob")
	d := mustDigest(body)
	cache.Put(d, body)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v2/myrepo/blobs/"+d.String(), nil)
	req.Header.Set(MirroredHeader, "1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	if got := resp.Header.Get("Docker-Content-Digest"); got != d.String() {
		t.Errorf("Docker-Content-Digest = %q, want %q", got, d)
	}

	if got := resp.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}

	got, _ := io.ReadAll(resp.Body)
	if string(got) != string(body) {
		t.Errorf("body mismatch: got %q, want %q", got, body)
	}

	if *served != 1 {
		t.Errorf("served count = %d, want 1", *served)
	}
}

func TestPeerServeByteMetrics(t *testing.T) {
	body := []byte("0123456789abcdef")
	d := mustDigest(body)
	cache := fakes.NewCache()
	cache.Put(d, body)

	type observation struct {
		kind  string
		bytes int64
	}

	var observations []observation

	s := New(cache, WithByteMetrics(func(kind string, bytes int64) {
		observations = append(observations, observation{kind: kind, bytes: bytes})
	}))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	request := func(method, byteRange string) {
		t.Helper()

		req, err := http.NewRequest(method, ts.URL+"/v2/r/blobs/"+d.String(), nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}

		req.Header.Set(MirroredHeader, "1")

		if byteRange != "" {
			req.Header.Set("Range", byteRange)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}

		_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck // drain response
		_ = resp.Body.Close()                 //nolint:errcheck // best-effort close
	}

	request(http.MethodGet, "")
	request(http.MethodGet, "bytes=2-5")
	request(http.MethodHead, "")

	want := []observation{
		{kind: "layer", bytes: int64(len(body))},
		{kind: "layer", bytes: 4},
	}

	if !reflect.DeepEqual(observations, want) {
		t.Fatalf("byte observations = %+v, want %+v", observations, want)
	}
}

func TestMiss404(t *testing.T) {
	ts, _, _ := newTestServer(t)
	d := digest.MustParse("sha256:" + strings.Repeat("a", 64))
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v2/r/blobs/"+d.String(), nil)
	req.Header.Set(MirroredHeader, "1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestTagAlways404(t *testing.T) {
	ts, _, _ := newTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v2/r/manifests/latest", nil)
	req.Header.Set(MirroredHeader, "1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (tags banned at peer endpoint)", resp.StatusCode)
	}
}

func TestServeCapShedsBlobGetWith429(t *testing.T) {
	cache := fakes.NewCache()
	body := []byte("a blob served under a concurrency cap")
	d := mustDigest(body)
	cache.Put(d, body)

	s := New(cache, WithMaxConcurrentServes(1))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	get := func(method string) *http.Response {
		t.Helper()

		req, _ := http.NewRequest(method, ts.URL+"/v2/r/blobs/"+d.String(), nil)
		req.Header.Set(MirroredHeader, "1")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}

		return resp
	}

	// Occupy the single serve slot so the server is at capacity.
	release, ok := s.tryAcquireServe()
	if !ok {
		t.Fatal("expected to acquire the only serve slot")
	}

	// A blob GET while at capacity is shed with 429 + Retry-After.
	resp := get(http.MethodGet)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("blob GET at capacity: status = %d, want 429", resp.StatusCode)
	}

	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Error("429 response missing Retry-After header")
	}

	_ = resp.Body.Close() //nolint:errcheck // best-effort body close

	// A HEAD is never capped.
	hresp := get(http.MethodHead)
	if hresp.StatusCode != http.StatusOK {
		t.Errorf("blob HEAD at capacity: status = %d, want 200 (HEAD is never capped)", hresp.StatusCode)
	}

	_ = hresp.Body.Close() //nolint:errcheck // best-effort body close

	// After releasing the slot, the blob GET succeeds.
	release()

	resp2 := get(http.MethodGet)
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("blob GET after release: status = %d, want 200", resp2.StatusCode)
	}

	_ = resp2.Body.Close() //nolint:errcheck // best-effort body close
}

func TestRangeRequest(t *testing.T) {
	ts, cache, _ := newTestServer(t)
	body := []byte("0123456789ABCDEF")
	d := mustDigest(body)
	cache.Put(d, body)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v2/r/blobs/"+d.String(), nil)
	req.Header.Set(MirroredHeader, "1")
	req.Header.Set("Range", "bytes=2-5")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != http.StatusPartialContent {
		t.Errorf("status = %d, want 206", resp.StatusCode)
	}

	if got := resp.Header.Get("Content-Range"); got != "bytes 2-5/16" {
		t.Errorf("Content-Range = %q, want bytes 2-5/16", got)
	}

	got, _ := io.ReadAll(resp.Body)
	if string(got) != "2345" {
		t.Errorf("body = %q, want 2345", got)
	}
}

func TestSuffixRange(t *testing.T) {
	ts, cache, _ := newTestServer(t)
	body := []byte("abcdefgh")
	d := mustDigest(body)
	cache.Put(d, body)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v2/r/blobs/"+d.String(), nil)
	req.Header.Set(MirroredHeader, "1")
	req.Header.Set("Range", "bytes=-3")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != http.StatusPartialContent {
		t.Errorf("status = %d, want 206", resp.StatusCode)
	}

	got, _ := io.ReadAll(resp.Body)
	if string(got) != "fgh" {
		t.Errorf("body = %q, want fgh", got)
	}
}

func TestInvalidRange(t *testing.T) {
	ts, cache, _ := newTestServer(t)
	body := []byte("ab")
	d := mustDigest(body)
	cache.Put(d, body)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v2/r/blobs/"+d.String(), nil)
	req.Header.Set(MirroredHeader, "1")
	req.Header.Set("Range", "bytes=10-20")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("status = %d, want 416", resp.StatusCode)
	}

	if got := resp.Header.Get("Content-Range"); got != "bytes */2" {
		t.Errorf("Content-Range = %q, want bytes */2", got)
	}
}

func TestHeadServesHeadersOnly(t *testing.T) {
	ts, cache, _ := newTestServer(t)
	body := []byte("payload")
	d := mustDigest(body)
	cache.Put(d, body)

	req, _ := http.NewRequest(http.MethodHead, ts.URL+"/v2/r/blobs/"+d.String(), nil)
	req.Header.Set(MirroredHeader, "1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	if got := resp.Header.Get("Docker-Content-Digest"); got != d.String() {
		t.Errorf("Docker-Content-Digest = %q, want %q", got, d)
	}

	gotBody, _ := io.ReadAll(resp.Body)
	if len(gotBody) != 0 {
		t.Errorf("HEAD body len = %d, want 0", len(gotBody))
	}
}

func TestInvalidDigest400(t *testing.T) {
	ts, _, _ := newTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v2/r/blobs/sha256:not-hex", nil)
	req.Header.Set(MirroredHeader, "1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	ts, _, _ := newTestServer(t)
	d := digest.MustParse("sha256:" + strings.Repeat("0", 64))
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v2/r/blobs/"+d.String(), nil)
	req.Header.Set(MirroredHeader, "1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}

	if got := resp.Header.Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow header = %q, want GET, HEAD", got)
	}
}

// unavailableCache returns *ifaces.ErrUnavailable from Has/Open so we
// can verify the the contract: containerd-unreachable surfaces
// as 503, NOT 404. Embeds *fakes.Cache so unused methods inherit the
// fake's behavior.
type unavailableCache struct {
	*fakes.Cache
	op string
}

func (u *unavailableCache) Has(_ context.Context, _ digest.Digest) (bool, error) {
	return false, &ifaces.ErrUnavailable{Op: u.op}
}

func (u *unavailableCache) Open(_ context.Context, _ digest.Digest) (io.ReadCloser, int64, error) {
	return nil, 0, &ifaces.ErrUnavailable{Op: u.op}
}

// TestServeBlobUnavailableReturns503 - the transfer endpoint MUST NOT
// collapse ErrUnavailable to 404 (which kubelet treats as
// definitively absent and routes around). Per "503 is
// the only signal kubelet has to back off and let the operator
// repair containerd before kubelet floods origin".
func TestServeBlobUnavailableReturns503(t *testing.T) {
	cache := &unavailableCache{Cache: fakes.NewCache(), op: "Info"}
	s := New(cache)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	d := mustDigest([]byte("nonexistent-but-unavailable"))
	req, _ := http.NewRequest("GET", ts.URL+"/v2/library/foo/blobs/"+d.String(), nil)
	req.Header.Set("Gantry-Mirrored", "1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

// TestServeManifestUnavailableReturns503 mirrors the blob test for
// manifest GETs.
func TestServeManifestUnavailableReturns503(t *testing.T) {
	cache := &unavailableCache{Cache: fakes.NewCache(), op: "Info"}
	s := New(cache)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	d := mustDigest([]byte("manifest-unavailable"))
	req, _ := http.NewRequest("GET", ts.URL+"/v2/library/foo/manifests/"+d.String(), nil)
	req.Header.Set("Gantry-Mirrored", "1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

// stubDescriber is a tiny Describer used for the media-type tests.
type stubDescriber struct{ m map[string]string }

func (s *stubDescriber) LookupMediaType(d digest.Digest) string { return s.m[d.String()] }

// TestServeManifestDescriberHintWins verifies that when a Describer
// is registered AND it returns a non-empty media type for a digest,
// the response Content-Type uses that hint instead of the default
// OCI manifest fallback. "Descriptor and media-type handling":
// the descriptor index is the truth source when populated.
func TestServeManifestDescriberHintWins(t *testing.T) {
	cache := fakes.NewCache()
	body := []byte(`{"schemaVersion":2}`)
	d := mustDigest(body)
	cache.Put(d, body)
	desc := &stubDescriber{m: map[string]string{
		d.String(): "application/vnd.docker.distribution.manifest.v2+json",
	}}
	s := New(cache, WithDescriber(desc))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest("GET", ts.URL+"/v2/library/foo/manifests/"+d.String(), nil)
	req.Header.Set("Gantry-Mirrored", "1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "application/vnd.docker.distribution.manifest.v2+json" {
		t.Errorf("Content-Type = %q, want docker manifest media type", got)
	}
}

// TestServeManifestDefaultMediaType - when no Describer is registered
// (or it returns ""), manifest GETs default to the OCI manifest
// media type instead of the generic application/octet-stream that
// containerd hostsd refuses to parse.
func TestServeManifestDefaultMediaType(t *testing.T) {
	cache := fakes.NewCache()
	body := []byte(`{"schemaVersion":2}`)
	d := mustDigest(body)
	cache.Put(d, body)
	s := New(cache)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest("GET", ts.URL+"/v2/library/foo/manifests/"+d.String(), nil)
	req.Header.Set("Gantry-Mirrored", "1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "application/vnd.oci.image.manifest.v1+json" {
		t.Errorf("Content-Type = %q, want oci manifest media type", got)
	}
}

// TestServeBlobDefaultMediaType - blob GETs without a Describer hint
// still fall through to application/octet-stream (the appropriate
// content-type for opaque layer payloads).
func TestServeBlobDefaultMediaType(t *testing.T) {
	cache := fakes.NewCache()
	body := []byte("opaque-blob-bytes")
	d := mustDigest(body)
	cache.Put(d, body)
	s := New(cache)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest("GET", ts.URL+"/v2/library/foo/blobs/"+d.String(), nil)
	req.Header.Set("Gantry-Mirrored", "1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", got)
	}
}

// _ = strings.HasPrefix is here so the strings import stays load-
// bearing for other tests in this file.
var _ = strings.HasPrefix
