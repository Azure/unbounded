// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package mirror_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	"github.com/Azure/unbounded/internal/gantry/origin"
)

// prefetchSpy records OnManifestServed calls and lets tests block
// until a given call count is reached.
type prefetchSpy struct {
	mu    sync.Mutex
	calls []prefetchCall
	cond  *sync.Cond
}

type prefetchCall struct {
	registry   string
	repository string
	digest     digest.Digest
}

func newPrefetchSpy() *prefetchSpy {
	s := &prefetchSpy{}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *prefetchSpy) OnManifestServed(_ context.Context, reg, repo string, d digest.Digest) {
	s.mu.Lock()
	s.calls = append(s.calls, prefetchCall{registry: reg, repository: repo, digest: d})
	s.cond.Broadcast()
	s.mu.Unlock()
}

func (s *prefetchSpy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *prefetchSpy) snapshot() []prefetchCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]prefetchCall, len(s.calls))
	copy(out, s.calls)
	return out
}

// waitForCount waits up to d for the spy to reach at least n calls.
// Returns the count seen at return time.
func (s *prefetchSpy) waitForCount(n int, d time.Duration) int {
	deadline := time.Now().Add(d)
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.calls) < n {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		// sync.Cond can't take a timeout directly; spawn a timer
		// that wakes us up if no Broadcast arrives in time.
		stopped := make(chan struct{})
		timer := time.AfterFunc(remaining, func() {
			s.mu.Lock()
			s.cond.Broadcast()
			s.mu.Unlock()
			close(stopped)
		})
		s.cond.Wait()
		timer.Stop()
		select {
		case <-stopped:
		default:
		}
	}
	return len(s.calls)
}

// newPrefetchFixture builds a self-contained mirror httptest setup
// wired to a prefetchSpy. Mirrors newFixture but uses
// WithLayerPrefetcher.
func newPrefetchFixture(t *testing.T, blobs map[digest.Digest][]byte, spy *prefetchSpy) *fixture {
	t.Helper()
	var originHits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&originHits, 1)
		path := r.URL.Path
		var refStart int
		switch {
		case strings.Contains(path, "/blobs/"):
			refStart = strings.LastIndex(path, "/blobs/") + len("/blobs/")
		case strings.Contains(path, "/manifests/"):
			refStart = strings.LastIndex(path, "/manifests/") + len("/manifests/")
		default:
			w.WriteHeader(404)
			return
		}
		ref := path[refStart:]
		d, err := digest.Parse(ref)
		if err != nil {
			w.WriteHeader(404)
			return
		}
		body, ok := blobs[d]
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		_, _ = w.Write(body) //nolint:errcheck // best-effort write
	}))
	t.Cleanup(up.Close)

	cfg := &config.Config{
		UpstreamRegistries: []config.UpstreamRegistry{
			{Name: "reg.example.com", Endpoint: up.URL},
		},
	}
	c := fakes.NewCache()

	oc, err := origin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	m := mirror.New(cfg, c, oc, mirror.WithLayerPrefetcher(spy))
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	return &fixture{cfg: cfg, cache: c, upstream: up, server: srv, originHits: &originHits}
}

// hashSum is a local digestOf clone to avoid touching the existing
// helper.
func hashSum(b []byte) digest.Digest {
	h := sha256.Sum256(b)
	d, err := digest.Parse("sha256:" + hex.EncodeToString(h[:]))
	if err != nil {
		panic(err)
	}
	return d
}

func TestMirror_Prefetch_FiresOnManifestOriginServe(t *testing.T) {
	body := []byte(`{"schemaVersion":2}`)
	d := hashSum(body)
	spy := newPrefetchSpy()
	f := newPrefetchFixture(t, map[digest.Digest][]byte{d: body}, spy)

	url := f.server.URL + "/v2/library/nginx/manifests/" + d.String() + "?ns=reg.example.com"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(got) != string(body) {
		t.Fatalf("manifest serve: status=%d body=%q", resp.StatusCode, got)
	}
	if n := spy.waitForCount(1, 2*time.Second); n != 1 {
		t.Fatalf("OnManifestServed calls after origin manifest serve: got %d want 1", n)
	}
	call := spy.snapshot()[0]
	if call.registry != "reg.example.com" {
		t.Errorf("registry: got %q want reg.example.com", call.registry)
	}
	if call.repository != "library/nginx" {
		t.Errorf("repository: got %q want library/nginx", call.repository)
	}
	if call.digest.String() != d.String() {
		t.Errorf("digest: got %s want %s", call.digest, d)
	}
}

func TestMirror_Prefetch_FiresAgainOnCacheHit(t *testing.T) {
	body := []byte(`{"schemaVersion":2,"layers":[]}`)
	d := hashSum(body)
	spy := newPrefetchSpy()
	f := newPrefetchFixture(t, map[digest.Digest][]byte{d: body}, spy)

	url := f.server.URL + "/v2/library/nginx/manifests/" + d.String() + "?ns=reg.example.com"

	// First serve: origin path fires once.
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body) //nolint:errcheck // best-effort
	resp.Body.Close()
	if n := spy.waitForCount(1, 2*time.Second); n != 1 {
		t.Fatalf("first serve: got %d calls want 1", n)
	}

	// Second serve: cache hit must fire again.
	resp2, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp2.Body) //nolint:errcheck // best-effort
	resp2.Body.Close()
	if n := spy.waitForCount(2, 2*time.Second); n != 2 {
		t.Fatalf("cache-hit serve: got %d calls want 2", n)
	}
	if atomic.LoadInt32(f.originHits) != 1 {
		t.Errorf("origin hits: got %d want 1 (second serve was cache hit)", *f.originHits)
	}
}

func TestMirror_Prefetch_DoesNotFireOnBlobServe(t *testing.T) {
	body := []byte("layer-bytes")
	d := hashSum(body)
	spy := newPrefetchSpy()
	f := newPrefetchFixture(t, map[digest.Digest][]byte{d: body}, spy)

	url := f.server.URL + "/v2/library/nginx/blobs/" + d.String() + "?ns=reg.example.com"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body) //nolint:errcheck // best-effort
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("blob GET: got %d want 200", resp.StatusCode)
	}
	// Give the goroutine ample chance to (incorrectly) fire.
	time.Sleep(100 * time.Millisecond)
	if n := spy.count(); n != 0 {
		t.Fatalf("blob serve must NOT invoke prefetch; got %d calls", n)
	}
}

func TestMirror_Prefetch_DoesNotFireOnTagFallthrough(t *testing.T) {
	spy := newPrefetchSpy()
	f := newPrefetchFixture(t, nil, spy)

	resp, err := http.Get(f.server.URL + "/v2/library/nginx/manifests/latest?ns=reg.example.com")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("tag fallthrough: got %d want 503", resp.StatusCode)
	}
	time.Sleep(100 * time.Millisecond)
	if n := spy.count(); n != 0 {
		t.Fatalf("tag fallthrough must NOT invoke prefetch; got %d calls", n)
	}
}

func TestMirror_Prefetch_FiresOnHeadCacheHit(t *testing.T) {
	body := []byte(`{"schemaVersion":2}`)
	d := hashSum(body)
	spy := newPrefetchSpy()
	f := newPrefetchFixture(t, map[digest.Digest][]byte{d: body}, spy)

	url := f.server.URL + "/v2/library/nginx/manifests/" + d.String() + "?ns=reg.example.com"
	// Warm cache via GET.
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body) //nolint:errcheck // best-effort
	resp.Body.Close()
	if n := spy.waitForCount(1, 2*time.Second); n != 1 {
		t.Fatalf("warm-up GET: got %d calls want 1", n)
	}

	// HEAD hits cache; must fire prefetch.
	headResp, err := http.Head(url)
	if err != nil {
		t.Fatal(err)
	}
	headResp.Body.Close()
	if headResp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD: got %d want 200", headResp.StatusCode)
	}
	if n := spy.waitForCount(2, 2*time.Second); n != 2 {
		t.Fatalf("HEAD cache-hit: got %d calls want 2", n)
	}
}
