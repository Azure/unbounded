// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
	"github.com/Azure/unbounded/pkg/racersdk"
)

const accessTestChallenge = `Bearer realm="https://auth.example/token",scope="repository:private/image:pull",service="registry.example"`

// fakeRegistryHead models an upstream registry's HEAD authorization policy.
type fakeRegistryHead struct {
	mu    sync.Mutex
	calls int
	auths []string
	gate  chan struct{} // when non-nil, Head blocks until closed
	fn    func(auth string, ref ifaces.OriginRef) error
}

func (f *fakeRegistryHead) Head(ctx context.Context, ref ifaces.OriginRef) (int64, string, error) {
	auth := registryauth.Authorization(ctx)

	f.mu.Lock()
	f.calls++
	f.auths = append(f.auths, auth)
	gate := f.gate
	f.mu.Unlock()

	if gate != nil {
		<-gate
	}

	if err := f.fn(auth, ref); err != nil {
		return 0, "", err
	}

	return 1, "application/octet-stream", nil
}

func (f *fakeRegistryHead) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls
}

// policy: "private/image" requires "Bearer good"; "Bearer forbidden" gets 403;
// "broken/image" fails with 5xx; "net/image" fails at the transport.
func accessPolicy(auth string, ref ifaces.OriginRef) error {
	oe := func(class ifaces.FailureClass, status int, challenge string) error {
		return &ifaces.OriginError{Ref: ref, Class: class, StatusCode: status, Challenge: challenge, Err: errors.New("upstream")}
	}

	switch ref.Repository {
	case "broken/image":
		return oe(ifaces.FailureTransient, http.StatusBadGateway, "")
	case "limited/image":
		return oe(ifaces.FailureRateLimited, http.StatusTooManyRequests, "")
	case "net/image":
		return errors.New("dial tcp: connection refused")
	case "missing/image":
		return oe(ifaces.FailureNotFound, http.StatusNotFound, "")
	case "private/image":
		switch auth {
		case "Bearer good":
			return nil
		case "Bearer forbidden":
			return oe(ifaces.FailureAuth, http.StatusForbidden, "")
		default:
			return oe(ifaces.FailureAuth, http.StatusUnauthorized, accessTestChallenge)
		}
	default:
		return oe(ifaces.FailureAuth, http.StatusUnauthorized, accessTestChallenge)
	}
}

// countingClient records whether Racer was consulted.
type countingClient struct {
	Client
	calls atomic.Int32
}

func (c *countingClient) Get(ctx context.Context, r racersdk.Request, o ...racersdk.ReadOptions) (*racersdk.Object, error) {
	c.calls.Add(1)
	return c.Client.Get(ctx, r, o...)
}

func (c *countingClient) Stat(ctx context.Context, r racersdk.Request) (racersdk.Metadata, error) {
	c.calls.Add(1)
	return c.Client.Stat(ctx, r)
}

func accessServer(t *testing.T, head AccessHeader, data []byte) (*httptest.Server, *countingClient) {
	t.Helper()

	d := racerDigest(data)
	client := &countingClient{Client: racerFakeClient(t, racerPageOrigin(t, d, data))}
	handler := NewHandler(client, nil, nil, WithAccessCheck(head))
	server := httptest.NewServer(WrapHTTP(mirror.New(racerConfig(), nil, nil, mirror.WithContentBackend(handler)).Handler(), 0, nil))
	t.Cleanup(server.Close)

	return server, client
}

func accessRequest(t *testing.T, server *httptest.Server, method, repo string, d digest.Digest, authorization string) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, server.URL+"/v2/"+repo+"/blobs/"+d.String(), nil)
	if err != nil {
		t.Fatal(err)
	}

	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck // test drain
	resp.Body.Close()

	return resp
}

func TestRacerAccessCheckOutcomes(t *testing.T) {
	data := []byte("private layer bytes")
	d := racerDigest(data)

	for _, tc := range []struct {
		name, method, repo, auth string
		status                   int
		challenge                string
		racer                    bool
	}{
		{"allowed GET", http.MethodGet, "private/image", "Bearer good", http.StatusOK, "", true},
		{"allowed HEAD", http.MethodHead, "private/image", "Bearer good", http.StatusOK, "", true},
		{"invalid token", http.MethodGet, "private/image", "Bearer bogus", http.StatusUnauthorized, accessTestChallenge, false},
		{"invalid token HEAD", http.MethodHead, "private/image", "Bearer bogus", http.StatusUnauthorized, accessTestChallenge, false},
		{"anonymous on public root", http.MethodGet, "private/image", "", http.StatusUnauthorized, accessTestChallenge, false},
		{"forbidden", http.MethodGet, "private/image", "Bearer forbidden", http.StatusForbidden, "", false},
		{"other repository", http.MethodGet, "other/image", "Bearer good", http.StatusUnauthorized, accessTestChallenge, false},
		{"not found", http.MethodGet, "missing/image", "Bearer good", http.StatusNotFound, "", false},
		{"upstream 5xx", http.MethodGet, "broken/image", "Bearer good", http.StatusServiceUnavailable, "", false},
		{"upstream 429", http.MethodGet, "limited/image", "Bearer good", http.StatusServiceUnavailable, "", false},
		{"network error", http.MethodGet, "net/image", "Bearer good", http.StatusServiceUnavailable, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head := &fakeRegistryHead{fn: accessPolicy}
			server, client := accessServer(t, head, data)

			resp := accessRequest(t, server, tc.method, tc.repo, d, tc.auth)
			if resp.StatusCode != tc.status || resp.Header.Get("WWW-Authenticate") != tc.challenge {
				t.Fatalf("status=%d challenge=%q; want %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"), tc.status, tc.challenge)
			}

			if got := client.calls.Load() != 0; got != tc.racer {
				t.Fatalf("Racer consulted=%v; want %v", got, tc.racer)
			}

			if head.count() != 1 || head.auths[0] != tc.auth {
				t.Fatalf("upstream HEADs=%d auths=%q; want one with %q", head.count(), head.auths, tc.auth)
			}
		})
	}
}

func TestRacerAccessCheckRepositoryScopedAfterCacheFill(t *testing.T) {
	data := []byte("shared digest")
	d := racerDigest(data)
	head := &fakeRegistryHead{fn: accessPolicy}
	server, client := accessServer(t, head, data)

	if resp := accessRequest(t, server, http.MethodGet, "private/image", d, "Bearer good"); resp.StatusCode != http.StatusOK {
		t.Fatalf("repo A status=%d", resp.StatusCode)
	}

	before := client.calls.Load()

	if resp := accessRequest(t, server, http.MethodGet, "other/image", d, "Bearer good"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("repo B status=%d; want 401", resp.StatusCode)
	}

	if client.calls.Load() != before {
		t.Fatal("Racer consulted for unauthorized repository")
	}
}

func TestRacerAccessCheckCaching(t *testing.T) {
	data := []byte("cached decision")
	d := racerDigest(data)
	head := &fakeRegistryHead{fn: accessPolicy}
	server, _ := accessServer(t, head, data)

	for range 3 {
		if resp := accessRequest(t, server, http.MethodGet, "private/image", d, "Bearer good"); resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d", resp.StatusCode)
		}
	}

	if head.count() != 1 {
		t.Fatalf("upstream HEADs=%d; want 1 within TTL", head.count())
	}

	// A distinct credential must not reuse the allowed decision.
	if resp := accessRequest(t, server, http.MethodGet, "private/image", d, "Bearer bogus"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("distinct credential status=%d; want 401", resp.StatusCode)
	}

	if resp := accessRequest(t, server, http.MethodGet, "private/image", d, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d; want 401", resp.StatusCode)
	}

	if head.count() != 3 {
		t.Fatalf("upstream HEADs=%d; want 3", head.count())
	}

	// Transient failures are not cached.
	for range 2 {
		if resp := accessRequest(t, server, http.MethodGet, "broken/image", d, "Bearer good"); resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status=%d", resp.StatusCode)
		}
	}

	if head.count() != 5 {
		t.Fatalf("upstream HEADs=%d; want 5 (5xx not cached)", head.count())
	}
}

func TestAccessCheckerTTLAndBound(t *testing.T) {
	now := time.Unix(1000, 0)
	head := &fakeRegistryHead{fn: accessPolicy}
	c := newAccessChecker(head)
	c.now = func() time.Time { return now }
	ref := ifaces.OriginRef{Registry: "registry.example", Repository: "private/image", Digest: racerDigest([]byte("x"))}
	good := registryauth.WithAuthorization(context.Background(), "Bearer good")
	bad := registryauth.WithAuthorization(context.Background(), "Bearer bogus")

	for _, step := range []struct {
		ctx     context.Context
		advance time.Duration
		status  int
		calls   int
	}{
		{good, 0, http.StatusOK, 1},
		{good, defaultAccessAllowTTL - time.Second, http.StatusOK, 1},
		{good, time.Second, http.StatusOK, 2},
		{bad, 0, http.StatusUnauthorized, 3},
		{bad, defaultAccessDenyTTL - time.Second, http.StatusUnauthorized, 3},
		{bad, time.Second, http.StatusUnauthorized, 4},
	} {
		now = now.Add(step.advance)
		if d := c.check(step.ctx, ref); d.status != step.status || head.count() != step.calls {
			t.Fatalf("status=%d calls=%d; want %d %d", d.status, head.count(), step.status, step.calls)
		}
	}

	c.maxEntries = 4

	for i := range 10 {
		r := ref
		r.Digest = racerDigest([]byte{byte(i)})
		c.check(good, r)
	}

	if n := len(c.entries); n > 4 {
		t.Fatalf("entries=%d; want <= 4", n)
	}
}

func TestAccessCheckerSingleflight(t *testing.T) {
	gate := make(chan struct{})
	head := &fakeRegistryHead{fn: accessPolicy, gate: gate}
	c := newAccessChecker(head)
	ref := ifaces.OriginRef{Registry: "registry.example", Repository: "private/image", Digest: racerDigest([]byte("x"))}
	ctx := registryauth.WithAuthorization(context.Background(), "Bearer good")

	var wg sync.WaitGroup

	results := make(chan int, 16)

	for range 16 {
		wg.Go(func() { results <- c.check(ctx, ref).status })
	}

	// Let every caller join the flight before releasing the HEAD.
	deadline := time.Now().Add(5 * time.Second)
	for head.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()
	close(results)

	for status := range results {
		if status != http.StatusOK {
			t.Fatalf("status=%d", status)
		}
	}

	if head.count() != 1 {
		t.Fatalf("upstream HEADs=%d; want 1", head.count())
	}
}

func TestAccessKeyNeverContainsCredential(t *testing.T) {
	ref := ifaces.OriginRef{Registry: "registry.example", Repository: "private/image", Digest: racerDigest([]byte("x"))}
	secret := "Bearer super-secret-token"
	key := accessKey(secret, ref)

	if strings.Contains(key, "super-secret-token") || strings.Contains(key, secret) || len(key) != 64 {
		t.Fatalf("key %q leaks credential or is not a sha256 hex digest", key)
	}

	other := ref
	other.Repository = "other/image"

	manifest := ref
	manifest.Kind = ifaces.KindManifest

	for _, k := range []string{accessKey("Bearer other", ref), accessKey("", ref), accessKey(secret, other), accessKey(secret, manifest)} {
		if k == key {
			t.Fatal("distinct credential or resource shares a cache key")
		}
	}

	c := newAccessChecker(&fakeRegistryHead{fn: accessPolicy})
	c.check(registryauth.WithAuthorization(context.Background(), secret), ref)

	for k, e := range c.entries {
		if strings.Contains(k, "secret") || strings.Contains(e.decision.challenge, "secret") {
			t.Fatal("cache retains credential")
		}
	}
}
