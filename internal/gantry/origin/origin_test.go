// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package origin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func digestOf(b []byte) digest.Digest {
	sum := sha256.Sum256(b)

	d, err := digest.Parse("sha256:" + hex.EncodeToString(sum[:]))
	if err != nil {
		panic(err)
	}

	return d
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}

	return u
}

func newClient(t *testing.T, ur config.UpstreamRegistry) *Client {
	t.Helper()

	cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{ur}}

	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return c
}

func TestPullBlob_Success(t *testing.T) {
	body := []byte("layer-bytes")
	d := digestOf(body)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/library/nginx/blobs/"+d.String() {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
			_, _ = w.Write(body) //nolint:errcheck // best-effort write

			return
		}

		t.Errorf("unexpected request: %s", r.URL.Path)
		w.WriteHeader(404)
	}))
	defer srv.Close()

	c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL})

	rc, size, err := c.Pull(context.Background(), ifaces.OriginRef{
		Registry: "reg", Repository: "library/nginx", Digest: d, Kind: ifaces.KindBlob,
	})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	defer rc.Close()

	got, _ := io.ReadAll(rc)
	if string(got) != string(body) {
		t.Errorf("body = %q", got)
	}

	if size != int64(len(body)) {
		t.Errorf("size = %d, want %d", size, len(body))
	}
}

func TestPullManifest_AcceptHeaderAndPath(t *testing.T) {
	body := []byte(`{"schemaVersion":2}`)
	d := digestOf(body)

	var seenAccept atomic.Value

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAccept.Store(r.Header.Get("Accept"))

		if !strings.Contains(r.URL.Path, "/manifests/") {
			t.Errorf("manifest pull hit wrong path: %s", r.URL.Path)
		}

		_, _ = w.Write(body) //nolint:errcheck // best-effort write
	}))
	defer srv.Close()

	c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL})

	rc, _, err := c.Pull(context.Background(), ifaces.OriginRef{
		Registry: "reg", Repository: "library/nginx", Digest: d, Kind: ifaces.KindManifest,
	})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	rc.Close()

	got, _ := seenAccept.Load().(string)
	if !strings.Contains(got, "manifest.v1+json") {
		t.Errorf("Accept header missing manifest media types: %q", got)
	}
}

func TestPull_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()

	c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL})
	d := digestOf([]byte("x"))
	_, _, err := c.Pull(context.Background(), ifaces.OriginRef{
		Registry: "reg", Repository: "r", Digest: d,
	})

	var oe *ifaces.OriginError
	if !errors.As(err, &oe) || oe.Class != ifaces.FailureNotFound {
		t.Fatalf("want FailureNotFound, got %v", err)
	}
}

func TestPull_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
	}))
	defer srv.Close()

	c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL})
	d := digestOf([]byte("x"))
	_, _, err := c.Pull(context.Background(), ifaces.OriginRef{Registry: "reg", Repository: "r", Digest: d})

	var oe *ifaces.OriginError
	if !errors.As(err, &oe) || oe.Class != ifaces.FailureRateLimited {
		t.Fatalf("want FailureRateLimited, got %v", err)
	}
}

func TestPull_TransientOn5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()

	c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL})
	d := digestOf([]byte("x"))
	_, _, err := c.Pull(context.Background(), ifaces.OriginRef{Registry: "reg", Repository: "r", Digest: d})

	var oe *ifaces.OriginError
	if !errors.As(err, &oe) || oe.Class != ifaces.FailureTransient {
		t.Fatalf("want FailureTransient, got %v", err)
	}
}

func TestPull_UnknownRegistry(t *testing.T) {
	c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: "https://reg.example.com"})
	d := digestOf([]byte("x"))
	_, _, err := c.Pull(context.Background(), ifaces.OriginRef{Registry: "other", Repository: "r", Digest: d})

	var oe *ifaces.OriginError
	if !errors.As(err, &oe) || oe.Class != ifaces.FailureNotFound {
		t.Fatalf("want FailureNotFound for unknown registry, got %v", err)
	}
}

func TestPull_InvalidRepositoryRejectedBeforeRequest(t *testing.T) {
	var hits int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var failures int32

	c, err := New(&config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "reg", Endpoint: srv.URL}}}, WithMetrics(nil, func(_, _ string) {
		atomic.AddInt32(&failures, 1)
	}))
	if err != nil {
		t.Fatal(err)
	}

	d := digestOf([]byte("x"))

	_, _, err = c.Pull(context.Background(), ifaces.OriginRef{Registry: "reg", Repository: "../../etc", Digest: d})
	if err == nil {
		t.Fatal("Pull: expected invalid repository error")
	}

	var oe *ifaces.OriginError
	if !errors.As(err, &oe) || oe.Class != ifaces.FailureNotFound {
		t.Fatalf("err = %v, want OriginError{Class=not_found}", err)
	}

	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("origin requests = %d, want 0", got)
	}

	if got := atomic.LoadInt32(&failures); got != 1 {
		t.Fatalf("failure callbacks = %d, want 1", got)
	}
}

func TestHead_InvalidRepositoryRejectedBeforeRequest(t *testing.T) {
	var hits int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL})
	d := digestOf([]byte("x"))

	_, _, err := c.Head(context.Background(), ifaces.OriginRef{Registry: "reg", Repository: "foo?bar", Digest: d})
	if err == nil {
		t.Fatal("Head: expected invalid repository error")
	}

	var oe *ifaces.OriginError
	if !errors.As(err, &oe) || oe.Class != ifaces.FailureNotFound {
		t.Fatalf("err = %v, want OriginError{Class=not_found}", err)
	}

	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("origin requests = %d, want 0", got)
	}
}

func TestPull_BearerTokenFlow(t *testing.T) {
	body := []byte("token-protected")
	d := digestOf(body)

	var authReqs, tokenReqs, dataReqs int32

	tokenMux := http.NewServeMux()

	tokenMux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&tokenReqs, 1)

		user, pass, ok := r.BasicAuth()
		if !ok || user != "alice" || pass != "secret" {
			t.Errorf("token auth missing/wrong: ok=%v user=%q", ok, user)
		}

		_ = json.NewEncoder(w).Encode(map[string]any{"token": "deadbeef"}) //nolint:errcheck // best-effort
	})

	var srv *httptest.Server

	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokenMux.ServeHTTP(w, r)
			return
		}

		if r.Header.Get("Authorization") != "Bearer deadbeef" {
			atomic.AddInt32(&authReqs, 1)
			w.Header().Set("WWW-Authenticate",
				`Bearer realm="`+srv.URL+`/token",service="reg",scope="repository:library/nginx:pull"`)
			w.WriteHeader(401)

			return
		}

		atomic.AddInt32(&dataReqs, 1)

		_, _ = w.Write(body) //nolint:errcheck // best-effort write
	}))
	defer srv.Close()

	dir := t.TempDir()

	credsPath := filepath.Join(dir, "creds")
	if err := os.WriteFile(credsPath, []byte("alice:secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL, CredentialsPath: credsPath})
	c.registries["reg"].hc = srv.Client()

	rc, _, err := c.Pull(context.Background(), ifaces.OriginRef{
		Registry: "reg", Repository: "library/nginx", Digest: d,
	})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	got, _ := io.ReadAll(rc)
	rc.Close()

	if string(got) != string(body) {
		t.Errorf("body = %q", got)
	}

	if atomic.LoadInt32(&authReqs) == 0 || atomic.LoadInt32(&tokenReqs) == 0 || atomic.LoadInt32(&dataReqs) == 0 {
		t.Errorf("flow incomplete: auth=%d token=%d data=%d", authReqs, tokenReqs, dataReqs)
	}

	// Second pull should reuse the cached token (no extra token request).
	rc2, _, err := c.Pull(context.Background(), ifaces.OriginRef{
		Registry: "reg", Repository: "library/nginx", Digest: d,
	})
	if err != nil {
		t.Fatalf("Pull (2nd): %v", err)
	}

	io.Copy(io.Discard, rc2)
	rc2.Close()

	if atomic.LoadInt32(&tokenReqs) != 1 {
		t.Errorf("tokenReqs after 2nd pull = %d, want 1 (cached)", tokenReqs)
	}
}

func TestFetchBearerTokenRejectsHTTPRealm(t *testing.T) {
	var tokenReqs int32

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&tokenReqs, 1)

		_ = json.NewEncoder(w).Encode(map[string]any{"token": "deadbeef"}) //nolint:errcheck // best-effort
	}))
	defer tokenSrv.Close()

	r := &registry{
		base:     mustParseURL(t, "https://reg.example.com"),
		username: "alice",
		password: "secret",
		hc:       tokenSrv.Client(),
	}

	_, _, err := r.fetchBearerToken(context.Background(), `Bearer realm="`+tokenSrv.URL+`/token",service="reg"`)
	if err == nil {
		t.Fatal("expected non-https token realm to be rejected")
	}

	if !strings.Contains(err.Error(), "absolute https URL") {
		t.Fatalf("error = %v, want https URL rejection", err)
	}

	if got := atomic.LoadInt32(&tokenReqs); got != 0 {
		t.Fatalf("token endpoint was called %d times; want 0", got)
	}
}

func TestFetchBearerTokenAllowsCrossHostHTTPSRealm(t *testing.T) {
	var tokenReqs int32

	tokenSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&tokenReqs, 1)

		user, pass, ok := r.BasicAuth()
		if !ok || user != "alice" || pass != "secret" {
			t.Errorf("token auth missing/wrong: ok=%v user=%q", ok, user)
		}

		if r.URL.Query().Get("service") != "reg" {
			t.Errorf("service query = %q", r.URL.Query().Get("service"))
		}

		_ = json.NewEncoder(w).Encode(map[string]any{"token": "deadbeef"}) //nolint:errcheck // best-effort
	}))
	defer tokenSrv.Close()

	r := &registry{
		base:     mustParseURL(t, "https://reg.example.com"),
		username: "alice",
		password: "secret",
		hc:       tokenSrv.Client(),
	}

	tok, _, err := r.fetchBearerToken(context.Background(), `Bearer realm="`+tokenSrv.URL+`/token",service="reg"`)
	if err != nil {
		t.Fatalf("fetchBearerToken: %v", err)
	}

	if tok != "deadbeef" {
		t.Fatalf("token = %q, want deadbeef", tok)
	}

	if got := atomic.LoadInt32(&tokenReqs); got != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", got)
	}
}

func TestHTTPRegistryDoesNotSendBasicToTokenEndpoint(t *testing.T) {
	body := []byte("token-protected")
	d := digestOf(body)

	var tokenReqs int32

	tokenSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&tokenReqs, 1)

		if user, _, ok := r.BasicAuth(); ok {
			t.Errorf("token request unexpectedly included Basic auth for user %q", user)
		}

		_ = json.NewEncoder(w).Encode(map[string]any{"token": "deadbeef"}) //nolint:errcheck // best-effort
	}))
	defer tokenSrv.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer deadbeef" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+tokenSrv.URL+`/token",service="reg"`)
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		_, _ = w.Write(body) //nolint:errcheck // best-effort write
	}))
	defer srv.Close()

	dir := t.TempDir()

	credsPath := filepath.Join(dir, "creds")
	if err := os.WriteFile(credsPath, []byte("alice:secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL, CredentialsPath: credsPath})
	c.registries["reg"].hc = tokenSrv.Client()

	rc, _, err := c.Pull(context.Background(), ifaces.OriginRef{
		Registry: "reg", Repository: "library/nginx", Digest: d,
	})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	got, _ := io.ReadAll(rc)
	rc.Close()

	if string(got) != string(body) {
		t.Errorf("body = %q", got)
	}

	if got := atomic.LoadInt32(&tokenReqs); got != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", got)
	}
}

func TestHTTPRegistryRepeatWithoutTokenDoesNotSendBasic(t *testing.T) {
	var reqs, sawBasic int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqs, 1)

		if _, _, ok := r.BasicAuth(); ok {
			atomic.StoreInt32(&sawBasic, 1)
		}

		w.Header().Set("WWW-Authenticate", `Basic realm="reg"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	dir := t.TempDir()

	credsPath := filepath.Join(dir, "creds")
	if err := os.WriteFile(credsPath, []byte("alice:secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL, CredentialsPath: credsPath})
	d := digestOf([]byte("x"))

	_, _, err := c.Pull(context.Background(), ifaces.OriginRef{Registry: "reg", Repository: "library/nginx", Digest: d})
	if err == nil {
		t.Fatal("expected Pull to fail")
	}

	if got := atomic.LoadInt32(&reqs); got != 2 {
		t.Fatalf("requests = %d, want 2 (initial + repeatWithoutToken)", got)
	}

	if atomic.LoadInt32(&sawBasic) != 0 {
		t.Fatal("Basic auth was sent to an http:// registry endpoint")
	}
}

// TestPullStripsAuthOnHTTPSDowngradeRedirect proves the registry's HTTP client
// refuses to carry an Authorization header onto a plaintext hop when an HTTPS
// endpoint redirects to HTTP on the same hostname. net/http copies the header
// across same-hostname redirects (it only compares hostnames, not schemes), so
// without the scheme-aware CheckRedirect wired up by newRegistry the bearer
// token would arrive at the HTTP target in clear text.
func TestPullStripsAuthOnHTTPSDowngradeRedirect(t *testing.T) {
	body := []byte("downgrade-protected")
	d := digestOf(body)

	var leaked int32

	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			atomic.StoreInt32(&leaked, 1)
		}

		_, _ = w.Write(body) //nolint:errcheck // best-effort write
	}))
	defer httpSrv.Close()

	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, httpSrv.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer tlsSrv.Close()

	c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: tlsSrv.URL})
	reg := c.registries["reg"]

	// Trust the test TLS cert but keep the production redirect policy so the
	// credential-stripping behavior wired up by newRegistry is exercised. If
	// newRegistry stopped setting CheckRedirect this copies nil and the test
	// fails when net/http leaks the header to the plaintext target.
	tlsClient := tlsSrv.Client()
	tlsClient.CheckRedirect = reg.hc.CheckRedirect
	reg.hc = tlsClient

	// Seed a cached bearer token so do() attaches an Authorization header to
	// the first request against the HTTPS endpoint.
	reg.setToken("deadbeef", time.Hour)

	rc, _, err := c.Pull(context.Background(), ifaces.OriginRef{
		Registry: "reg", Repository: "library/nginx", Digest: d,
	})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	got, _ := io.ReadAll(rc)
	rc.Close()

	if string(got) != string(body) {
		t.Errorf("body = %q", got)
	}

	if atomic.LoadInt32(&leaked) != 0 {
		t.Fatal("Authorization header leaked onto plain-HTTP redirect target")
	}
}

func TestCheckRedirect(t *testing.T) {
	t.Run("strips Authorization on http downgrade", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://reg.example.com/v2/", nil)
		req.Header.Set("Authorization", "Bearer secret")

		if err := checkRedirect(req, nil); err != nil {
			t.Fatalf("checkRedirect: %v", err)
		}

		if got := req.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization preserved on http downgrade: %q", got)
		}
	})

	t.Run("keeps Authorization on https redirect", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "https://reg.example.com/v2/", nil)
		req.Header.Set("Authorization", "Bearer secret")

		if err := checkRedirect(req, nil); err != nil {
			t.Fatalf("checkRedirect: %v", err)
		}

		if got := req.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("Authorization stripped on https redirect: %q", got)
		}
	})

	t.Run("stops after 10 redirects", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "https://reg.example.com/v2/", nil)

		if err := checkRedirect(req, make([]*http.Request, 10)); err == nil {
			t.Fatal("expected error after 10 redirects")
		}
	})
}

// TestFetchBearerTokenPreservesRealmQueryAndFragment verifies the token URL is
// assembled from the parsed realm: a pre-existing realm query survives, the
// service/scope params are added, and a realm fragment does not swallow them.
// Raw string concatenation would have appended "&service=...&scope=..." after
// the "#frag", which url.Parse folds into the fragment so those params never
// reach the wire.
func TestFetchBearerTokenPreservesRealmQueryAndFragment(t *testing.T) {
	var gotService, gotScope, gotExtra string

	tokenSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		gotService = q.Get("service")
		gotScope = q.Get("scope")
		gotExtra = q.Get("extra")

		_ = json.NewEncoder(w).Encode(map[string]any{"token": "deadbeef"}) //nolint:errcheck // best-effort
	}))
	defer tokenSrv.Close()

	r := &registry{
		base: mustParseURL(t, "https://reg.example.com"),
		hc:   tokenSrv.Client(),
	}

	realm := tokenSrv.URL + "/token?extra=keep#frag"
	challenge := `Bearer realm="` + realm + `",service="reg",scope="repository:library/nginx:pull"`

	tok, _, err := r.fetchBearerToken(context.Background(), challenge)
	if err != nil {
		t.Fatalf("fetchBearerToken: %v", err)
	}

	if tok != "deadbeef" {
		t.Fatalf("token = %q, want deadbeef", tok)
	}

	if gotService != "reg" {
		t.Errorf("service = %q, want reg", gotService)
	}

	if gotScope != "repository:library/nginx:pull" {
		t.Errorf("scope = %q, want repository:library/nginx:pull", gotScope)
	}

	if gotExtra != "keep" {
		t.Errorf("extra = %q, want keep (pre-existing realm query dropped)", gotExtra)
	}
}

func TestNSAliasResolves(t *testing.T) {
	body := []byte("aliased")
	d := digestOf(body)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body) //nolint:errcheck // best-effort write
	}))
	defer srv.Close()

	cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{
		{Name: "ghcr.io", Endpoint: srv.URL, NSAlias: "github"},
	}}

	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Pull by alias.
	rc, _, err := c.Pull(context.Background(), ifaces.OriginRef{
		Registry: "github", Repository: "owner/repo", Digest: d,
	})
	if err != nil {
		t.Fatalf("Pull(alias): %v", err)
	}

	rc.Close()
}

func TestNewRejectsBadCredentialsFile(t *testing.T) {
	dir := t.TempDir()

	credsPath := filepath.Join(dir, "creds")
	if err := os.WriteFile(credsPath, []byte("no-colon-here\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{
		{Name: "reg", Endpoint: "https://reg.example.com", CredentialsPath: credsPath},
	}}
	if _, err := New(cfg); err == nil {
		t.Fatal("expected New() to reject malformed credentials")
	}
}

func TestParseChallenge(t *testing.T) {
	got := parseChallenge(`Bearer realm="https://auth.example.com/token",service="reg.example.com",scope="repository:lib/n:pull"`)
	if got["realm"] != "https://auth.example.com/token" {
		t.Errorf("realm = %q", got["realm"])
	}

	if got["service"] != "reg.example.com" {
		t.Errorf("service = %q", got["service"])
	}

	if got["scope"] != "repository:lib/n:pull" {
		t.Errorf("scope = %q", got["scope"])
	}
}

// TestPull_StartCallbackFiresOnceBeforeOutcome pins the contract
// that originated in : p2p_origin_pull_total must
// be incremented exactly once per Pull invocation, regardless of
// the terminal outcome (success, registry-not-found, 4xx, 5xx,
// transport error). This is the started == success + failure +
// in-flight arithmetic identity that the wiring in cmd/gantry
// relies on so 'origin failure rate' alerts can be computed
// against a coherent denominator.
//
// The mirror direct-origin path and the coordinated please_pull /
// runOriginPull path both call origin.Client.Pull; counting at
// Pull's entry means both paths share one source of truth and the
// counter cannot silently undercount please_pull-coordinated
// pulls (which used to be the case when the started hook lived on
// the mirror's WithMetrics).
func TestPull_StartCallbackFiresOnceBeforeOutcome(t *testing.T) {
	body := []byte("payload")
	d := digestOf(body)

	mux := http.NewServeMux()
	mux.HandleFunc("/v2/r/blobs/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body) //nolint:errcheck // best-effort write
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	var (
		startKinds       []string
		failureKindClass [][2]string
	)

	cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "reg", Endpoint: srv.URL}}}

	c, err := New(cfg, WithMetrics(
		func(kind string) { startKinds = append(startKinds, kind) },
		func(kind, class string) { failureKindClass = append(failureKindClass, [2]string{kind, class}) },
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Run("success path increments started exactly once with the kind label", func(t *testing.T) {
		startKinds, failureKindClass = nil, nil

		rc, _, err := c.Pull(context.Background(), ifaces.OriginRef{Registry: "reg", Repository: "r", Digest: d, Kind: ifaces.KindBlob})
		if err != nil {
			t.Fatalf("Pull: %v", err)
		}

		_, _ = io.Copy(io.Discard, rc) //nolint:errcheck // best-effort
		_ = rc.Close()                 //nolint:errcheck // best-effort close
		// KindBlob maps to the metric label "layer" (the design
		// vocabulary), NOT "blob" (the OCI URL family). See
		// ifaces.OriginRefKind.MetricLabel for the rationale.
		if len(startKinds) != 1 || startKinds[0] != "layer" {
			t.Fatalf("startKinds = %v, want [layer]", startKinds)
		}

		if len(failureKindClass) != 0 {
			t.Fatalf("failureKindClass = %v, want empty", failureKindClass)
		}
		// Origin no longer reports SUCCESS itself - that hook
		// was lifted out in a prior review because Close
		// fires on HEAD, on io.Copy interruption, and on
		// cache-commit failure, all of which would falsely
		// inflate the success counter. The mirror's serveDigest
		// and the puller pump's runOriginPull now own success
		// reporting after their respective verify/commit step
		// passes. See ifaces.OriginRefKind.MetricLabel and
		// mirror.WithOriginSuccessMetric for the contract.
	})

	t.Run("unknown registry increments started before the failure", func(t *testing.T) {
		startKinds, failureKindClass = nil, nil

		_, _, err := c.Pull(context.Background(), ifaces.OriginRef{Registry: "other", Repository: "r", Digest: d, Kind: ifaces.KindManifest})
		if err == nil {
			t.Fatalf("Pull: want error, got nil")
		}

		if len(startKinds) != 1 || startKinds[0] != "manifest" {
			t.Fatalf("startKinds = %v, want [manifest] (started must fire even when the registry lookup fails - this is the 'started' chokepoint please_pull relies on)", startKinds)
		}

		if len(failureKindClass) != 1 || failureKindClass[0][0] != "manifest" {
			t.Fatalf("failureKindClass = %v, want one entry with kind=manifest", failureKindClass)
		}
	})

	t.Run("config kind label passes through", func(t *testing.T) {
		startKinds, failureKindClass = nil, nil

		rc, _, err := c.Pull(context.Background(), ifaces.OriginRef{Registry: "reg", Repository: "r", Digest: d, Kind: ifaces.KindConfig})
		if err != nil {
			t.Fatalf("Pull: %v", err)
		}

		_, _ = io.Copy(io.Discard, rc) //nolint:errcheck // best-effort
		_ = rc.Close()                 //nolint:errcheck // best-effort close

		if len(startKinds) != 1 || startKinds[0] != "config" {
			t.Fatalf("startKinds = %v, want [config] (KindConfig must surface as a distinct 'kind' label all the way through origin.WithMetrics so the started counter agrees with the per-kind success/failure breakdown)", startKinds)
		}
	})
}

// TestOriginMetricKind_MapsToDesignVocabulary locks in the
// design-doc label set:
//
//	p2p_origin_pull_total{kind="manifest|config|layer"}
//
// In the in-process enum KindBlob covers everything under /blobs/
// (both config blobs and layer blobs), and KindBlob.String returns
// "blob" - the OCI URL-family term, correct for logs but wrong as a
// Prometheus label because the design vocabulary commits to "layer".
// OriginRefKind.MetricLabel is the seam where the in-process kind
// becomes the observability label; this test pins both halves
// (manifest/config pass through unchanged, KindBlob is rewritten to
// "layer") so a later refactor cannot reintroduce a `kind="blob"`
// series that dashboards built against the design spec would not
// pick up.
func TestOriginMetricKind_MapsToDesignVocabulary(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   ifaces.OriginRefKind
		want string
	}{
		{ifaces.KindManifest, "manifest"},
		{ifaces.KindConfig, "config"},
		{ifaces.KindBlob, "layer"}, // <- the load-bearing rewrite
	}
	for _, tc := range cases {
		if got := tc.in.MetricLabel(); got != tc.want {
			t.Errorf("OriginRefKind(%v).MetricLabel() = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestHead_DoesNotFirePullMetrics pins contract:
// origin.Client.Head must NOT invoke onPullStart or onPullFailure,
// regardless of outcome. HEAD is a metadata-only operation; folding
// it into p2p_origin_pull_total broke the per-pull arithmetic
// (started == success + failure + in_flight) because HEAD never
// produces bytes and therefore can fire neither success (no commit)
// nor downstream-failure (no body copy).
func TestHead_DoesNotFirePullMetrics(t *testing.T) {
	body := []byte("head-metadata-only")
	d := digestOf(body)

	t.Run("success path", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodHead {
				t.Errorf("origin received method %q, want HEAD", r.Method)
			}

			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		var starts, failures int32

		cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{
			{Name: "reg", Endpoint: srv.URL},
		}}

		c, err := New(cfg, WithMetrics(
			func(_ string) { atomic.AddInt32(&starts, 1) },
			func(_, _ string) { atomic.AddInt32(&failures, 1) },
		))
		if err != nil {
			t.Fatal(err)
		}

		size, _, err := c.Head(context.Background(), ifaces.OriginRef{
			Registry: "reg", Repository: "lib/n", Digest: d, Kind: ifaces.KindBlob,
		})
		if err != nil {
			t.Fatalf("Head: %v", err)
		}

		if size != int64(len(body)) {
			t.Errorf("size = %d, want %d", size, len(body))
		}

		if n := atomic.LoadInt32(&starts); n != 0 {
			t.Errorf("starts = %d, want 0 (Head must NOT bump p2p_origin_pull_total)", n)
		}

		if n := atomic.LoadInt32(&failures); n != 0 {
			t.Errorf("failures = %d, want 0", n)
		}
	})

	t.Run("404 failure path", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		var starts, failures int32

		cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{
			{Name: "reg", Endpoint: srv.URL},
		}}

		c, err := New(cfg, WithMetrics(
			func(_ string) { atomic.AddInt32(&starts, 1) },
			func(_, _ string) { atomic.AddInt32(&failures, 1) },
		))
		if err != nil {
			t.Fatal(err)
		}

		_, _, err = c.Head(context.Background(), ifaces.OriginRef{
			Registry: "reg", Repository: "lib/n", Digest: d, Kind: ifaces.KindBlob,
		})
		if err == nil {
			t.Fatal("Head: expected error on 404")
		}

		var oe *ifaces.OriginError
		if !errors.As(err, &oe) || oe.Class != ifaces.FailureNotFound {
			t.Errorf("err = %v, want OriginError{Class=not_found}", err)
		}

		if n := atomic.LoadInt32(&starts); n != 0 {
			t.Errorf("starts = %d, want 0 (Head must NOT bump p2p_origin_pull_total)", n)
		}
		// HEAD failures also stay out of the pull-failure family
		// for now - operators see HEAD failures via the mirror's
		// HTTP response code. A future batch can add a dedicated
		// HEAD failure counter if needed.
		if n := atomic.LoadInt32(&failures); n != 0 {
			t.Errorf("failures = %d, want 0 (Head must NOT bump p2p_origin_pull_failure_total either)", n)
		}
	})

	t.Run("unknown registry", func(t *testing.T) {
		var starts, failures int32

		c, err := New(&config.Config{UpstreamRegistries: []config.UpstreamRegistry{
			{Name: "known", Endpoint: "http://localhost"},
		}}, WithMetrics(
			func(_ string) { atomic.AddInt32(&starts, 1) },
			func(_, _ string) { atomic.AddInt32(&failures, 1) },
		))
		if err != nil {
			t.Fatal(err)
		}

		_, _, err = c.Head(context.Background(), ifaces.OriginRef{
			Registry: "absent", Repository: "lib/n", Digest: d, Kind: ifaces.KindBlob,
		})
		if err == nil {
			t.Fatal("Head: expected error for unknown registry")
		}

		if n := atomic.LoadInt32(&starts); n != 0 {
			t.Errorf("starts = %d, want 0", n)
		}

		if n := atomic.LoadInt32(&failures); n != 0 {
			t.Errorf("failures = %d, want 0", n)
		}
	})
}
