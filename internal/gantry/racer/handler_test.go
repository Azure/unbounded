// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/plugins/content/local"
	ocidigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	registryorigin "github.com/Azure/unbounded/internal/gantry/origin"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
	"github.com/Azure/unbounded/pkg/racersdk"
	"github.com/Azure/unbounded/pkg/racersdk/racersdktest"
)

type racerRepositoryChallengeOrigin struct {
	racerChallengeOrigin
	repositoryAuthenticationChallenger
}

func TestRacerRemoteRejectedCredentialRepositoryChallenge(t *testing.T) {
	if handlerTLSSubprocess(t) {
		return
	}

	data := []byte("private remote content")
	d := racerDigest(data)

	const challenge = `Bearer realm="https://registry.example/token",scope="repository:library/image:pull",service="registry.example"`

	var (
		roots, probes, pulls atomic.Int32
		route, outcome       atomic.Value
	)

	route.Store("blobs")
	outcome.Store("valid")

	registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			roots.Add(1)
			w.WriteHeader(http.StatusOK)

			return
		}

		if r.URL.Path != "/v2/library/image/"+route.Load().(string)+"/"+d.String() {
			t.Errorf("probe escaped caller path: %s", r.URL)
			http.NotFound(w, r)

			return
		}

		switch r.Header.Get("Authorization") {
		case "":
			probes.Add(1)

			if r.Method != http.MethodHead || r.Header.Get("Range") != "" {
				t.Error("recovery fetched content or forwarded range")
			}

			switch outcome.Load() {
			case "missing":
				w.WriteHeader(http.StatusUnauthorized)
				return
			case "invalid":
				w.Header().Set("WWW-Authenticate", `Bearer realm="http://insecure.example/token"`)
				w.WriteHeader(http.StatusUnauthorized)

				return
			case "redirect":
				w.Header().Set("Location", "/v2/other/repo/blobs/"+d.String())
				w.WriteHeader(http.StatusTemporaryRedirect)

				return
			}
		case "Bearer expired":
			pulls.Add(1)
		case "Bearer refreshed":
			pulls.Add(1)
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Docker-Content-Digest", d.String())
			http.ServeContent(w, r, "content", time.Time{}, bytes.NewReader(data))

			return
		default:
			t.Error("unexpected credential")
		}

		w.Header().Set("WWW-Authenticate", challenge)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer registry.Close()

	pool := x509.NewCertPool()
	pool.AddCert(registry.Certificate())
	x509.SetFallbackRoots(pool)

	for _, resource := range []string{"blobs", "manifests"} {
		for _, mode := range []string{"GET", "HEAD", "resume", "missing", "invalid", "redirect"} {
			t.Run(resource+"/"+mode, func(t *testing.T) {
				roots.Store(0)
				probes.Store(0)
				pulls.Store(0)
				route.Store(resource)
				outcome.Store(mode)

				cfg := racerConfig()
				cfg.UpstreamRegistries[0].Endpoint = registry.URL

				requesterA, err := registryorigin.New(cfg)
				if err != nil {
					t.Fatal(err)
				}

				originB, err := registryorigin.New(cfg)
				if err != nil {
					t.Fatal(err)
				}

				if got, required, err := requesterA.AuthenticationChallenge(context.Background(), "registry.example"); err != nil || required || got != "" {
					t.Fatalf("root: %q %v %v", got, required, err)
				}

				callback := Origin(cfg, originB)
				client := racerFakeClient(t, callback)
				trap := &racerLegacyTrap{}
				origin := racerRepositoryChallengeOrigin{racerChallengeOrigin{trap, requesterA}, requesterA}

				server := httptest.NewServer(WrapHTTP(mirror.New(cfg, trap, origin, mirror.WithContentBackend(NewHandler(client, origin, nil))).Handler(), 0, nil))
				defer server.Close()

				server.Client().Timeout = 5 * time.Second

				method, rangeHeader, status, want := http.MethodGet, "", http.StatusOK, data
				if mode == "HEAD" {
					method, want = http.MethodHead, nil
				}

				if mode == "resume" && resource == "blobs" {
					rangeHeader, status, want = "bytes=4-", http.StatusPartialContent, data[4:]
				}

				resp := racerRequest(t, server, method, resource, d, rangeHeader, "Bearer expired")

				wantChallenge := challenge
				if mode == "missing" || mode == "invalid" || mode == "redirect" {
					wantChallenge = ""
				}

				if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != wantChallenge {
					t.Fatalf("status=%d challenge=%q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
				}

				resp.Body.Close()

				if probes.Load() != 1 || pulls.Load() != 1 || roots.Load() != 1 {
					t.Fatalf("probes=%d pulls=%d roots=%d", probes.Load(), pulls.Load(), roots.Load())
				}
				// B learned the repository challenge; A still has only its public
				// root cache. Recovery must not depend on or poison either cache.
				if got, required, err := originB.AuthenticationChallenge(context.Background(), "registry.example"); err != nil || !required || got != challenge {
					t.Fatalf("B cache: %q %v %v", got, required, err)
				}

				if got, required, err := requesterA.AuthenticationChallenge(context.Background(), "registry.example"); err != nil || required || got != "" {
					t.Fatalf("A cache: %q %v %v", got, required, err)
				}

				refreshed := racerRequest(t, server, method, resource, d, rangeHeader, "Bearer refreshed")
				got, err := io.ReadAll(refreshed.Body)
				refreshed.Body.Close()

				if err != nil || refreshed.StatusCode != status || !bytes.Equal(got, want) || refreshed.Header.Get("WWW-Authenticate") != "" {
					t.Fatalf("refresh status=%d body=%q err=%v", refreshed.StatusCode, got, err)
				}

				if probes.Load() != 1 || roots.Load() != 1 || trap.originCalls.Load() != 0 || trap.storeCalls.Load() != 0 || trap.probes.Load() != 0 {
					t.Fatal("unexpected content fallback or root/recovery probe")
				}
			})
		}
	}
}

var (
	_ ifaces.OriginPuller                = racerRepositoryChallengeOrigin{}
	_ repositoryAuthenticationChallenger = racerRepositoryChallengeOrigin{}
)

type racerRepositoryChallengeFunc func(context.Context, ifaces.OriginRef) (string, bool, error)

func (f racerRepositoryChallengeFunc) RepositoryAuthenticationChallenge(ctx context.Context, ref ifaces.OriginRef) (string, bool, error) {
	return f(ctx, ref)
}

func TestRacerRepositoryChallengeOnlyOnUnauthorized(t *testing.T) {
	for _, kind := range []error{racersdk.ErrForbidden, racersdk.ErrNotFound, racersdk.ErrUnavailable} {
		t.Run(kind.Error(), func(t *testing.T) {
			client := racerFakeClient(t, func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				return racersdk.Metadata{}, nil, kind
			})
			trap := &racerLegacyTrap{}

			var probes atomic.Int32

			probe := racerRepositoryChallengeFunc(func(context.Context, ifaces.OriginRef) (string, bool, error) {
				probes.Add(1)
				return `Basic realm="unused"`, true, nil
			})
			origin := racerRepositoryChallengeOrigin{racerChallengeOrigin{trap, trap}, probe}

			server := httptest.NewServer(mirror.New(racerConfig(), trap, origin, mirror.WithContentBackend(NewHandler(client, origin, nil))).Handler())
			defer server.Close()

			server.Client().Timeout = 5 * time.Second
			resp := racerRequest(t, server, http.MethodGet, "blobs", racerDigest(nil), "", "Bearer expired")
			resp.Body.Close()

			if probes.Load() != 0 || trap.probes.Load() != 0 || resp.Header.Get("WWW-Authenticate") != "" {
				t.Fatal("non-401 failure triggered challenge recovery")
			}
		})
	}
}

// Count before panicking: net/http recovers handler panics, so an accidental
// legacy call must also fail the test even when a transport error is expected.
type racerLegacyTrap struct {
	storeCalls  atomic.Int32
	originCalls atomic.Int32
	probes      atomic.Int32
	challenge   string
}

func (s *racerLegacyTrap) Has(context.Context, digest.Digest) (bool, error) {
	s.storeCalls.Add(1)
	panic("Racer request reached legacy Has")
}

func (s *racerLegacyTrap) Open(context.Context, digest.Digest) (io.ReadCloser, int64, error) {
	s.storeCalls.Add(1)
	panic("Racer request reached legacy Open")
}

func (s *racerLegacyTrap) Writer(context.Context, digest.Digest) (ifaces.ContentWriter, error) {
	s.storeCalls.Add(1)
	panic("Racer request reached legacy Writer")
}

func (s *racerLegacyTrap) Pull(context.Context, ifaces.OriginRef) (io.ReadCloser, int64, error) {
	s.originCalls.Add(1)
	panic("Racer request retried direct origin Pull")
}

func (s *racerLegacyTrap) Head(context.Context, ifaces.OriginRef) (int64, string, error) {
	s.originCalls.Add(1)
	panic("Racer request reached direct origin Head")
}

func (s *racerLegacyTrap) AuthenticationChallenge(context.Context, string) (string, bool, error) {
	s.probes.Add(1)
	return s.challenge, s.challenge != "", nil
}

func racerConfig() *config.Config {
	return &config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "registry.example"}}}
}

func racerDigest(data []byte) digest.Digest {
	return digest.MustParse(fmt.Sprintf("sha256:%x", sha256.Sum256(data)))
}

func racerFakeClient(t *testing.T, origin racersdk.Origin) *racersdk.Client {
	t.Helper()

	return racersdktest.NewClient(t, origin)
}

func racerServer(t *testing.T, cfg *config.Config, client Client, trap *racerLegacyTrap, challengers ...mirror.AuthenticationChallenger) *httptest.Server {
	t.Helper()
	t.Cleanup(func() {
		if store, origin := trap.storeCalls.Load(), trap.originCalls.Load(); store != 0 || origin != 0 {
			t.Errorf("legacy content calls: store=%d origin=%d; want zero", store, origin)
		}
	})

	var origin ifaces.OriginPuller = trap
	if len(challengers) != 0 {
		origin = racerChallengeOrigin{racerLegacyTrap: trap, AuthenticationChallenger: challengers[0]}
	}

	server := httptest.NewServer(WrapHTTP(mirror.New(cfg, trap, origin, mirror.WithContentBackend(NewHandler(client, origin.(authenticationChallenger), nil))).Handler(), 0, nil))
	server.Client().Timeout = 10 * time.Second
	t.Cleanup(server.Close)

	return server
}

type racerChallengeOrigin struct {
	*racerLegacyTrap
	mirror.AuthenticationChallenger
}

func (o racerChallengeOrigin) AuthenticationChallenge(ctx context.Context, registry string) (string, bool, error) {
	o.probes.Add(1)
	return o.AuthenticationChallenger.AuthenticationChallenge(ctx, registry)
}

type racerChallengeFunc func(context.Context, string) (string, bool, error)

func (f racerChallengeFunc) AuthenticationChallenge(ctx context.Context, registry string) (string, bool, error) {
	return f(ctx, registry)
}

func racerRequest(t *testing.T, server *httptest.Server, method, route string, d digest.Digest, rangeHeader, authorization string) *http.Response {
	t.Helper()

	req := handlerRequest(t, t.Context(), server, method, route, d)
	req.Header.Set("Range", rangeHeader)
	req.Header.Set("Authorization", authorization)

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { resp.Body.Close() })

	return resp
}

func handlerRequest(t *testing.T, ctx context.Context, server *httptest.Server, method, route string, d digest.Digest) *http.Request {
	t.Helper()

	req, err := http.NewRequestWithContext(ctx, method, server.URL+"/v2/library/image/"+route+"/"+d.String(), nil)
	if err != nil {
		t.Fatal(err)
	}

	return req
}

func handlerReadBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body (status %d): %v", resp.StatusCode, err)
	}

	return body
}

// Run TLS trust changes in a child so they cannot affect other package tests.
func handlerTLSSubprocess(t *testing.T) bool {
	t.Helper()

	const child = "GANTRY_RACER_HANDLER_TLS_TEST_CHILD"
	if os.Getenv(child) == t.Name() {
		return false
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+t.Name()+"$", "-test.timeout=25s")

	cmd.Env = append(os.Environ(), child+"="+t.Name(), "GODEBUG="+os.Getenv("GODEBUG")+",x509usefallbackroots=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("TLS integration subprocess: %v\n%s", err, output)
	}

	return true
}

func handlerRequestModes() []struct{ name, method, rangeHeader string } {
	return []struct{ name, method, rangeHeader string }{
		{"GET", http.MethodGet, ""},
		{"HEAD", http.MethodHead, ""},
		{"resume", http.MethodGet, "bytes=4-"},
	}
}

func racerMetadata(t *testing.T, d digest.Digest, size int) racersdk.Metadata {
	t.Helper()

	return racersdk.Metadata{Size: int64(size), ETag: `"` + d.String() + `"`, ContentType: "application/octet-stream", ExpiresAt: time.Unix(2000000000, 0)}
}

func containerdRegistryHost(t *testing.T, server *httptest.Server) docker.RegistryHost {
	t.Helper()

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}

	return docker.RegistryHost{
		Client:       server.Client(),
		Host:         u.Host,
		Scheme:       u.Scheme,
		Path:         "/v2",
		Capabilities: docker.HostCapabilityPull,
	}
}

func racerPageOrigin(t *testing.T, d digest.Digest, data []byte) racersdk.Origin {
	t.Helper()
	metadata := racerMetadata(t, d, len(data))

	return func(_ context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if req.Head || req.Offset >= metadata.Size {
			return metadata, nil, nil
		}

		return metadata, io.NopCloser(bytes.NewReader(data[req.Offset:min(req.Offset+req.Length, metadata.Size)])), nil
	}
}

// This upstream is reachable only through gantry/racer.Origin. The separate
// racerLegacyTrap passed to mirror.New catches direct registry fallback.
type racerRegistry struct {
	t             *testing.T
	data          []byte
	digest        digest.Digest
	kind          ifaces.OriginRefKind
	contentType   string
	authorization string
	heads         atomic.Int32
	pulls         atomic.Int32
}

func (u *racerRegistry) check(ctx context.Context, ref ifaces.OriginRef) {
	u.t.Helper()

	if ref.Registry != "registry.example" || ref.Repository != "library/image" || ref.Digest != u.digest || ref.Kind != u.kind {
		u.t.Errorf("incorrect registry reference: %+v", ref)
	}

	if registryauth.Authorization(ctx) != u.authorization {
		u.t.Error("registry did not receive the delegated credential")
	}
}

func (u *racerRegistry) Head(ctx context.Context, ref ifaces.OriginRef) (int64, string, error) {
	u.check(ctx, ref)
	u.heads.Add(1)

	return int64(len(u.data)), u.contentType, nil
}

func (u *racerRegistry) PullRange(ctx context.Context, ref ifaces.OriginRef, length int64) (io.ReadCloser, int64, string, error) {
	u.check(ctx, ref)
	u.pulls.Add(1)

	if ref.Offset < 0 || ref.Offset >= int64(len(u.data)) || length <= 0 {
		return nil, 0, "", errors.New("invalid registry range")
	}

	end := ref.Offset + min(length, int64(len(u.data))-ref.Offset)

	return io.NopCloser(bytes.NewReader(u.data[ref.Offset:end])), int64(len(u.data)), u.contentType, nil
}

func TestRacerGETAndHEAD(t *testing.T) {
	t.Run("header defaults by kind", func(t *testing.T) {
		for _, tc := range []struct {
			kind        ifaces.OriginRefKind
			contentType string
		}{
			{ifaces.KindBlob, "application/octet-stream"},
			{ifaces.KindManifest, ""},
			{ifaces.KindConfig, ""},
		} {
			for _, supplied := range []string{"", "application/custom"} {
				w := httptest.NewRecorder()
				d := racerDigest(nil)
				metadata := racerMetadata(t, d, 0)
				metadata.ContentType = supplied
				writeRacerHeaders(w, d, metadata, tc.kind)

				want := supplied
				if want == "" {
					want = tc.contentType
				}

				if w.Header().Get("Content-Type") != want || w.Header().Get("Content-Length") != "0" || w.Header().Get("Docker-Content-Digest") != d.String() {
					t.Fatalf("kind=%v supplied=%q headers=%v", tc.kind, supplied, w.Header())
				}
			}
		}
	})

	for _, tc := range []struct {
		name, route, mediaType string
		data                   []byte
	}{
		{"blob", "blobs", "application/octet-stream", []byte("layer bytes")},
		{"empty", "blobs", "application/octet-stream", nil},
		{"manifest", "manifests", "application/vnd.oci.image.manifest.v1+json", []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}`)},
		{"index", "manifests", "application/vnd.oci.image.index.v1+json", []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)},
		{"docker list", "manifests", "application/vnd.docker.distribution.manifest.list.v2+json", []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.list.v2+json","manifests":[]}`)},
		{"index via blobs", "blobs", "application/vnd.oci.image.index.v1+json", []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)},
		{"multiple pages", "blobs", "application/octet-stream", bytes.Repeat([]byte("page bytes!"), int(2*racersdk.PageSize)/11+17)},
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(tc.name+"/"+method, func(t *testing.T) {
				d := racerDigest(tc.data)
				origin := racerPageOrigin(t, d, tc.data)

				var heads, gets atomic.Int32

				client := racerFakeClient(t, func(ctx context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
					if req.Head {
						heads.Add(1)
					} else {
						gets.Add(1)
					}

					metadata, body, err := origin(ctx, req)
					metadata.ContentType = tc.mediaType

					return metadata, body, err
				})
				server := racerServer(t, racerConfig(), client, &racerLegacyTrap{})

				resp := racerRequest(t, server, method, tc.route, d, "", "")
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status = %d; want 200", resp.StatusCode)
				}

				for name, want := range map[string]string{
					"Content-Type": tc.mediaType, "Content-Length": strconv.Itoa(len(tc.data)),
					"Docker-Content-Digest": d.String(), "Gantry-Mirrored": "1",
					"Docker-Distribution-API-Version": "registry/2.0",
				} {
					if got := resp.Header.Get(name); got != want {
						t.Errorf("%s = %q; want %q", name, got, want)
					}
				}

				got := handlerReadBody(t, resp)

				want := tc.data
				if method == http.MethodHead {
					want = nil
				}

				if !bytes.Equal(got, want) {
					t.Fatalf("response differs: got %d bytes; want %d", len(got), len(want))
				}

				if method == http.MethodHead && (heads.Load() != 1 || gets.Load() != 0) {
					t.Fatalf("HEAD fetched object bytes: heads=%d gets=%d", heads.Load(), gets.Load())
				}
			})
		}
	}
}

type racerHeaderOrigin struct {
	*racerLegacyTrap
	metadata racersdk.Metadata
}

func (o racerHeaderOrigin) Head(context.Context, ifaces.OriginRef) (int64, string, error) {
	return int64(o.metadata.Size), o.metadata.ContentType, nil
}

// Exercise mirror's non-sniffing HEAD path rather than copy its policy here.
func TestRacerMirrorPolicyParity(t *testing.T) {
	t.Run("content headers", func(t *testing.T) {
		for _, kind := range []ifaces.OriginRefKind{ifaces.KindBlob, ifaces.KindManifest} {
			for _, contentType := range []string{"", "application/custom"} {
				for _, size := range []int{0, 17} {
					d := racerDigest(nil)
					metadata := racerMetadata(t, d, size)
					metadata.ContentType = contentType
					origin := racerHeaderOrigin{racerLegacyTrap: &racerLegacyTrap{}, metadata: metadata}
					legacy := mirror.New(racerConfig(), fakes.NewCache(), origin)

					route := "blobs"
					if kind == ifaces.KindManifest {
						route = "manifests"
					}

					w := httptest.NewRecorder()
					legacy.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodHead, "/v2/library/image/"+route+"/"+d.String(), nil))

					racerResponse := httptest.NewRecorder()
					client := racerMetadataClient{metadata: metadata}
					NewHandler(client, nil, nil).ServeContent(racerResponse, httptest.NewRequest(http.MethodHead, "/", nil), ifaces.OriginRef{
						Registry: "registry.example", Repository: "library/image", Digest: d, Kind: kind,
					})

					if w.Code != http.StatusOK {
						t.Fatalf("mirror HEAD failed: %d", w.Code)
					}

					// Racer rejects unknown manifest types instead of using the legacy default.
					if kind == ifaces.KindManifest && contentType == "" {
						if racerResponse.Code != http.StatusBadGateway || racerResponse.Header().Get("Gantry-Mirrored") != "" {
							t.Fatalf("unknown manifest type accepted: status=%d headers=%v", racerResponse.Code, racerResponse.Header())
						}

						continue
					}

					if racerResponse.Code != http.StatusOK {
						t.Fatalf("Racer HEAD failed: %d", racerResponse.Code)
					}

					for _, header := range []string{"Content-Type", "Content-Length", "Docker-Content-Digest"} {
						if got, want := racerResponse.Header().Get(header), w.Header().Get(header); got != want {
							t.Fatalf("kind=%v type=%q size=%d: %s=%q; mirror=%q", kind, contentType, size, header, got, want)
						}
					}
				}
			}
		}
	})
	t.Run("authentication deadline", func(t *testing.T) {
		for _, backend := range []string{"mirror", "racer"} {
			t.Run(backend, func(t *testing.T) {
				started := time.Now()
				calls := 0
				probe := racerChallengeFunc(func(ctx context.Context, _ string) (string, bool, error) {
					calls++

					deadline, ok := ctx.Deadline()
					if !ok || deadline.Before(started.Add(authenticationChallengeTimeout)) || deadline.After(time.Now().Add(authenticationChallengeTimeout)) {
						t.Fatalf("%s challenge deadline differs from Racer's %s: %v", backend, authenticationChallengeTimeout, deadline)
					}

					return `Basic realm="registry"`, true, nil
				})
				w := httptest.NewRecorder()
				d := racerDigest(nil)
				r := httptest.NewRequest(http.MethodHead, "/v2/library/image/blobs/"+d.String(), nil)

				if backend == "mirror" {
					origin := racerChallengeOrigin{racerLegacyTrap: &racerLegacyTrap{}, AuthenticationChallenger: probe}
					mirror.New(racerConfig(), nil, origin).Handler().ServeHTTP(w, r)
				} else {
					NewHandler(nil, probe, nil).racerError(w, r, ifaces.OriginRef{Registry: "registry.example"}, racersdk.ErrUnauthorized)
				}

				if calls != 1 || w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != `Basic realm="registry"` {
					t.Fatalf("challenge calls=%d status=%d headers=%v", calls, w.Code, w.Header())
				}
			})
		}
	})
}

func TestRacerResumeAndInvalidRange(t *testing.T) {
	t.Run("direct non-blob offsets are ignored", func(t *testing.T) {
		for _, kind := range []ifaces.OriginRefKind{ifaces.KindManifest, ifaces.KindConfig} {
			for _, offset := range []int64{4, 100} {
				data := []byte(`{"schemaVersion":2}`)
				d := racerDigest(data)
				client := &racerTranscriptClient{Client: racerFakeClient(t, racerPageOrigin(t, d, data)), options: make(chan []racersdk.ReadOptions, 1)}
				ref := ifaces.OriginRef{Registry: "registry.example", Repository: "library/image", Digest: d, Kind: kind, Offset: offset}
				w := httptest.NewRecorder()
				NewHandler(client, nil, nil).ServeContent(w, httptest.NewRequest(http.MethodGet, "/", nil), ref)

				if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), data) || w.Header().Get("Content-Length") != strconv.Itoa(len(data)) || w.Header().Get("Content-Range") != "" || w.Header().Get("Accept-Ranges") != "" {
					t.Fatalf("kind=%v offset=%d: status=%d headers=%v body=%q", kind, offset, w.Code, w.Header(), w.Body.String())
				}

				if client.stats.Load() != 0 || client.gets.Load() != 1 {
					t.Fatalf("non-blob resume selected: Stat=%d Get=%d", client.stats.Load(), client.gets.Load())
				}

				options := <-client.options
				if kind == ifaces.KindManifest {
					if len(options) != 1 || options[0] != (racersdk.ReadOptions{SmallObject: true}) {
						t.Fatalf("manifest options = %+v", options)
					}
				} else if len(options) != 0 {
					t.Fatalf("config options = %+v", options)
				}
			}
		}
	})

	t.Run("generic backend dispatch", func(t *testing.T) {
		for _, tc := range []struct {
			path, rangeHeader, challenge string
			offset                       int64
			status                       int
		}{
			{"blobs/", "bytes=4-", "", 4, 502},
			{"blobs/", "bytes=0-", "", 0, 502},
			{"blobs/", "bytes=2-4", "", 0, 502},
			{"manifests/", "bytes=4-", "", 0, 502},
			{"blobs/", "bytes=4-", `Basic realm="registry"`, 0, 401},
		} {
			trap := &racerLegacyTrap{challenge: tc.challenge}
			calls := 0
			d := racerDigest(nil)
			backend := contentBackendFunc(func(w http.ResponseWriter, _ *http.Request, ref ifaces.OriginRef) {
				calls++

				if ref.Offset != tc.offset || ref.Registry != "registry.example" || ref.Repository != "library/image" || ref.Digest != d || trap.probes.Load() != 1 {
					t.Fatalf("backend called before resolution/preflight: %+v", ref)
				}

				w.WriteHeader(http.StatusBadGateway)
			})
			server := mirror.New(racerConfig(), trap, trap, mirror.WithContentBackend(backend))
			request := httptest.NewRequest(http.MethodGet, "/v2/library/image/"+tc.path+d.String(), nil)
			request.Header.Set("Range", tc.rangeHeader)

			w := httptest.NewRecorder()
			server.Handler().ServeHTTP(w, request)

			wantCalls := 1
			if tc.challenge != "" {
				wantCalls = 0
			}

			if w.Code != tc.status || calls != wantCalls || trap.storeCalls.Load() != 0 || trap.originCalls.Load() != 0 {
				t.Fatalf("status=%d calls=%d legacy=%d/%d", w.Code, calls, trap.storeCalls.Load(), trap.originCalls.Load())
			}
		}
	})

	data := []byte("0123456789")
	for _, tc := range []struct {
		rangeHeader, contentRange, body string
		status                          int
	}{
		{"bytes=4-", "bytes 4-9/10", "456789", http.StatusPartialContent},
		{"bytes=9-", "bytes 9-9/10", "9", http.StatusPartialContent},
		{"bytes=10-", "bytes */10", "", http.StatusRequestedRangeNotSatisfiable},
		{"bytes=100-", "bytes */10", "", http.StatusRequestedRangeNotSatisfiable},
		{"invalid", "", string(data), http.StatusOK},
		{"bytes=-3", "", string(data), http.StatusOK},
		{"bytes=2-4", "", string(data), http.StatusOK},
		{"bytes=1-,4-", "", string(data), http.StatusOK},
	} {
		t.Run(tc.rangeHeader, func(t *testing.T) {
			d := racerDigest(data)
			client := racerFakeClient(t, racerPageOrigin(t, d, data))
			server := racerServer(t, racerConfig(), client, &racerLegacyTrap{})

			resp := racerRequest(t, server, http.MethodGet, "blobs", d, tc.rangeHeader, "")
			if resp.StatusCode != tc.status || resp.Header.Get("Content-Range") != tc.contentRange {
				t.Fatalf("status/range = %d/%q; want %d/%q", resp.StatusCode, resp.Header.Get("Content-Range"), tc.status, tc.contentRange)
			}

			got := handlerReadBody(t, resp)

			if tc.status != http.StatusRequestedRangeNotSatisfiable && (string(got) != tc.body || resp.ContentLength != int64(len(tc.body))) {
				t.Fatalf("body/length = %q/%d; want %q/%d", got, resp.ContentLength, tc.body, len(tc.body))
			}

			if tc.status == http.StatusPartialContent && resp.Header.Get("Accept-Ranges") != "bytes" {
				t.Fatal("resume did not advertise byte ranges")
			}
		})
	}
}

func TestRacerResumeAcrossPages(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcdef"), int(racersdk.PageSize)/16+4096)
	d := racerDigest(data)
	origin := racerPageOrigin(t, d, data)

	var heads, gets atomic.Int32

	client := racerFakeClient(t, func(ctx context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if req.Head {
			heads.Add(1)
		} else {
			gets.Add(1)

			if req.Offset != racersdk.PageSize {
				t.Errorf("resume fetched skipped page: first=%d", req.Offset)
			}
		}

		return origin(ctx, req)
	})
	server := racerServer(t, racerConfig(), client, &racerLegacyTrap{})
	offset := int(racersdk.PageSize) + 7
	resp := racerRequest(t, server, http.MethodGet, "blobs", d, fmt.Sprintf("bytes=%d-", offset), "")

	got := handlerReadBody(t, resp)

	wantRange := fmt.Sprintf("bytes %d-%d/%d", offset, len(data)-1, len(data))
	if resp.StatusCode != http.StatusPartialContent || resp.Header.Get("Content-Range") != wantRange || !bytes.Equal(got, data[offset:]) {
		t.Fatalf("cross-page resume failed: status=%d range=%q bytes=%d", resp.StatusCode, resp.Header.Get("Content-Range"), len(got))
	}

	if heads.Load() != 2 || gets.Load() != 1 {
		t.Fatalf("resume transcript: heads=%d gets=%d; want 2/1 (Stat and fake pin validation)", heads.Load(), gets.Load())
	}
}

type racerMetadataClient struct {
	*racersdk.Client
	metadata racersdk.Metadata
}

type racerTranscriptClient struct {
	*racersdk.Client
	stats   atomic.Int32
	gets    atomic.Int32
	options chan []racersdk.ReadOptions
}

func (c *racerTranscriptClient) Stat(ctx context.Context, req racersdk.Request) (racersdk.Metadata, error) {
	c.stats.Add(1)
	return c.Client.Stat(ctx, req)
}

func (c *racerTranscriptClient) Get(ctx context.Context, req racersdk.Request, options ...racersdk.ReadOptions) (*racersdk.Object, error) {
	c.gets.Add(1)

	c.options <- options

	return c.Client.Get(ctx, req, options...)
}

func TestRacerSDKRequestTranscript(t *testing.T) {
	blob := bytes.Repeat([]byte("x"), int(racersdk.PageSize)+99)
	for _, tc := range []struct {
		name, method, route, rangeHeader string
		data                             []byte
		stats, gets                      int32
	}{
		{"GET", http.MethodGet, "blobs", "", blob, 0, 1},
		{"HEAD", http.MethodHead, "blobs", "", blob, 1, 0},
		{"resume", http.MethodGet, "blobs", fmt.Sprintf("bytes=%d-", racersdk.PageSize+7), blob, 1, 1},
		{"unsatisfiable", http.MethodGet, "blobs", fmt.Sprintf("bytes=%d-", len(blob)), blob, 1, 0},
		{"manifest", http.MethodGet, "manifests", "", []byte(`{"schemaVersion":2}`), 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := tc.data
			d := racerDigest(data)
			client := &racerTranscriptClient{Client: racerFakeClient(t, racerPageOrigin(t, d, data)), options: make(chan []racersdk.ReadOptions, 1)}
			server := racerServer(t, racerConfig(), client, &racerLegacyTrap{})

			resp := racerRequest(t, server, tc.method, tc.route, d, tc.rangeHeader, "")
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				t.Fatal(err)
			}

			if client.stats.Load() != tc.stats || client.gets.Load() != tc.gets {
				t.Fatalf("SDK transcript: Stat=%d Get=%d; want %d/%d", client.stats.Load(), client.gets.Load(), tc.stats, tc.gets)
			}

			if tc.gets == 0 {
				return
			}

			options := <-client.options
			if tc.name == "manifest" {
				if len(options) != 1 || options[0] != (racersdk.ReadOptions{SmallObject: true}) {
					t.Fatal("manifest must use the small-object bootstrap without metadata preflight")
				}

				return
			}

			if tc.name == "GET" {
				if len(options) != 0 {
					t.Fatal("full GET must use bootstrap, without a redundant metadata read")
				}

				return
			}

			if len(options) != 1 || options[0].Offset != racersdk.PageSize+7 || options[0].Length != 0 || options[0].ETag != `"`+d.String()+`"` {
				t.Fatal("resume must use exact offset, through-EOF length, and selected digest pin")
			}
		})
	}
}

func (c racerMetadataClient) Stat(context.Context, racersdk.Request) (racersdk.Metadata, error) {
	return c.metadata, nil
}

func TestRacerRejectsMixedMetadata(t *testing.T) {
	data := []byte("0123456789")
	d := racerDigest(data)
	metadata := racerMetadata(t, d, len(data))
	size, contentType, overflow := metadata, metadata, metadata
	version := racerMetadata(t, racerDigest(nil), len(data))
	size.Size++
	contentType.ContentType = "application/vnd.oci.image.index.v1+json"
	overflow.Size = -1

	for _, tc := range []struct {
		name     string
		metadata racersdk.Metadata
	}{
		{"size", size},
		{"content type", contentType},
		{"version", version},
		{"overflow", overflow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := racerMetadataClient{Client: racerFakeClient(t, racerPageOrigin(t, d, data)), metadata: tc.metadata}
			server := racerServer(t, racerConfig(), client, &racerLegacyTrap{})

			resp := racerRequest(t, server, http.MethodGet, "blobs", d, "bytes=4-", "")
			if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Gantry-Mirrored") != "" {
				t.Fatalf("mixed metadata accepted: status=%d headers=%v", resp.StatusCode, resp.Header)
			}
		})
	}
}

func TestRacerOptionalContentTypeKeepsSnapshot(t *testing.T) {
	for _, tc := range []struct{ name, initial, returned string }{
		{"absent to present", "", "application/vnd.oci.image.index.v1+json"},
		{"present to absent", "application/vnd.oci.image.index.v1+json", ""},
		{"both absent", "", ""},
		{"both present same", "application/vnd.oci.image.index.v1+json", "application/vnd.oci.image.index.v1+json"},
		{"both present different", "application/vnd.oci.image.index.v1+json", "application/vnd.oci.image.manifest.v1+json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte("0123456789")
			d := racerDigest(data)
			initial, actual := racerMetadata(t, d, len(data)), racerMetadata(t, d, len(data))
			initial.ContentType, actual.ContentType = tc.initial, tc.returned
			origin := racerPageOrigin(t, d, data)

			client := racerMetadataClient{metadata: initial, Client: racerFakeClient(t, func(ctx context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				_, body, err := origin(ctx, req)
				return actual, body, err
			})}

			server := racerServer(t, racerConfig(), client, &racerLegacyTrap{})

			resp := racerRequest(t, server, http.MethodGet, "blobs", d, "bytes=4-", "")
			if tc.initial != tc.returned {
				if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Gantry-Mirrored") != "" {
					t.Fatal("changed media type snapshot was accepted")
				}

				return
			}

			body, err := io.ReadAll(resp.Body)

			wantType := tc.initial
			if wantType == "" {
				wantType = "application/octet-stream"
			}

			if err != nil || resp.StatusCode != http.StatusPartialContent || string(body) != "456789" || resp.Header.Get("Content-Type") != wantType {
				t.Fatalf("snapshot changed: status=%d type=%q body=%q err=%v", resp.StatusCode, resp.Header.Get("Content-Type"), body, err)
			}
		})
	}
}

func TestRacerContainerdRejectsCorruptAssembly(t *testing.T) {
	for _, mode := range []string{"full", "resumed corrupt prefix", "resumed corrupt suffix"} {
		t.Run(mode, func(t *testing.T) {
			data := bytes.Repeat([]byte("0123456789abcdef"), int(racersdk.PageSize)/16+4096)

			d := racerDigest(data)
			if mode == "resumed corrupt suffix" {
				data[len(data)-1] ^= 0xff
			} else {
				data[0] ^= 0xff
			}

			server := racerServer(t, racerConfig(), racerFakeClient(t, racerPageOrigin(t, d, data)), &racerLegacyTrap{})

			var resumes atomic.Int32

			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				req, err := http.NewRequestWithContext(r.Context(), r.Method, server.URL+r.URL.RequestURI(), nil)
				if err != nil {
					panic(err)
				}

				req.Header = r.Header.Clone()

				resp, err := server.Client().Do(req)
				if err != nil {
					panic(http.ErrAbortHandler)
				}
				defer resp.Body.Close()

				for key, values := range resp.Header {
					w.Header()[key] = values
				}

				w.WriteHeader(resp.StatusCode)

				if r.Header.Get("Range") != "" {
					resumes.Add(1)
				}

				if mode != "full" && r.Method == http.MethodGet && r.Header.Get("Range") == "" {
					if _, err := io.CopyN(w, resp.Body, int64(racersdk.PageSize)+7); err != nil {
						panic(http.ErrAbortHandler)
					}

					return
				}

				if _, err := io.Copy(w, resp.Body); err != nil {
					panic(http.ErrAbortHandler)
				}
			}))
			t.Cleanup(proxy.Close)

			resolver := docker.NewResolver(docker.ResolverOptions{Hosts: func(string) ([]docker.RegistryHost, error) {
				return []docker.RegistryHost{containerdRegistryHost(t, proxy)}, nil
			}})

			fetcher, err := resolver.Fetcher(t.Context(), "registry.example/library/image:latest")
			if err != nil {
				t.Fatal(err)
			}

			desc := ocispec.Descriptor{Digest: ocidigest.Digest(d.String()), Size: int64(len(data)), MediaType: ocispec.MediaTypeImageLayer}

			body, err := fetcher.Fetch(t.Context(), desc)
			if err != nil {
				t.Fatal(err)
			}
			defer body.Close()

			store, err := local.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}

			err = content.WriteBlob(t.Context(), store, "corrupt", body, desc)
			if err == nil || !strings.Contains(err.Error(), "unexpected commit digest") {
				t.Fatalf("containerd must reject at digest commit: %v", err)
			}

			if _, err := store.Info(t.Context(), desc.Digest); err == nil {
				t.Fatal("corrupt content committed")
			}

			if mode != "full" && resumes.Load() != 1 {
				t.Fatalf("containerd resume requests=%d; want one", resumes.Load())
			}
		})
	}
}

func TestRacerErrorsNeverUseLegacyContent(t *testing.T) {
	for _, tc := range []struct {
		kind   error
		status int
	}{
		{racersdk.ErrNotFound, http.StatusNotFound},
		{racersdk.ErrUnauthorized, http.StatusUnauthorized},
		{racersdk.ErrForbidden, http.StatusForbidden},
		{racersdk.ErrUnavailable, http.StatusServiceUnavailable},
		{errors.New("internal error"), http.StatusBadGateway},
		{racersdk.ErrVersionMismatch, http.StatusBadGateway},
	} {
		for _, mode := range handlerRequestModes() {
			t.Run(tc.kind.Error()+"/"+mode.name, func(t *testing.T) {
				var calls atomic.Int32

				client := racerFakeClient(t, func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
					calls.Add(1)
					return racersdk.Metadata{}, nil, fmt.Errorf("%w: private upstream detail", tc.kind)
				})
				server := racerServer(t, racerConfig(), client, &racerLegacyTrap{})

				resp := racerRequest(t, server, mode.method, "blobs", racerDigest([]byte("missing")), mode.rangeHeader, "")

				body, err := io.ReadAll(resp.Body)
				if err != nil || resp.StatusCode != tc.status {
					t.Fatalf("response = %d, %v; want complete %d", resp.StatusCode, err, tc.status)
				}

				if calls.Load() != 1 || resp.Header.Get("Gantry-Mirrored") != "" || strings.Contains(string(body), "private upstream detail") {
					t.Fatalf("failure retried, claimed success, or exposed upstream details: calls=%d headers=%v", calls.Load(), resp.Header)
				}
			})
		}
	}
}

func TestRacerUnavailableFailsClosed(t *testing.T) {
	t.Run("missing backend", func(t *testing.T) {
		var logs bytes.Buffer

		logger := slog.New(slog.NewJSONHandler(&logs, nil))
		cfg := racerConfig()
		cfg.RacerEnabled = true
		trap := &racerLegacyTrap{}
		server := mirror.New(cfg, trap, trap, mirror.WithContentBackend(nil), mirror.WithLogger(logger))
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/library/image/blobs/"+racerDigest(nil).String(), nil))

		if w.Code != http.StatusServiceUnavailable || w.Body.String() != "Racer unavailable\n" || trap.storeCalls.Load() != 0 || trap.originCalls.Load() != 0 {
			t.Fatalf("missing backend did not fail closed: %d %q", w.Code, w.Body.String())
		}

		if !strings.Contains(logs.String(), "Racer enabled without a content backend") {
			t.Fatalf("missing backend was not logged: %s", &logs)
		}
	})

	t.Run("nil client through mirror retains bounded diagnostics", func(t *testing.T) {
		var logs bytes.Buffer

		logger := slog.New(slog.NewJSONHandler(&logs, nil))
		cfg := racerConfig()
		cfg.RacerEnabled = true
		trap := &racerLegacyTrap{}
		server := mirror.New(cfg, trap, trap, mirror.WithContentBackend(NewHandler(nil, trap, logger)))
		ids := make(map[string]bool)

		for i := range racerFailureLogBurst + 2 {
			logs.Reset()

			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/v2/library/image/blobs/"+racerDigest(nil).String(), nil)
			r.Header.Set("Gantry-Racer-Request-ID", "caller-controlled")
			server.Handler().ServeHTTP(w, r)

			id := w.Header().Get("Gantry-Racer-Request-ID")
			if w.Code != http.StatusServiceUnavailable || w.Body.String() != "Racer unavailable\n" || len(id) < 26 || ids[id] || id == "caller-controlled" {
				t.Fatalf("unavailable response=%d %q id=%q", w.Code, w.Body.String(), id)
			}

			ids[id] = true

			if i < racerFailureLogBurst {
				entry := decodeRacerDiagnostic(t, &logs)
				if entry["request_id"] != id || entry["stage"] != "client" || entry["subsystem"] != "mirror" {
					t.Fatalf("lost client diagnostic: %v", entry)
				}
			} else if logs.Len() != 0 {
				t.Fatalf("failure diagnostic exceeded burst: %s", &logs)
			}
		}

		if trap.storeCalls.Load() != 0 || trap.originCalls.Load() != 0 {
			t.Fatal("nil client reached legacy content")
		}
	})

	for _, state := range []string{"nil", "closed"} {
		for _, mode := range handlerRequestModes() {
			t.Run(state+"/"+mode.name, func(t *testing.T) {
				cfg := racerConfig()
				cfg.RacerEnabled = true

				var client Client

				if state == "closed" {
					fake := racerFakeClient(t, func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
						t.Error("closed client reached origin")
						return racersdk.Metadata{}, nil, errors.New("unexpected origin request")
					})
					fake.Close()
					client = fake
				}

				server := racerServer(t, cfg, client, &racerLegacyTrap{})

				resp := racerRequest(t, server, mode.method, "blobs", racerDigest(nil), mode.rangeHeader, "")
				if resp.StatusCode != http.StatusServiceUnavailable {
					t.Fatalf("status = %d; want 503", resp.StatusCode)
				}
			})
		}
	}
}

func TestRacerFailureBeforeContentHeaders(t *testing.T) {
	for _, mode := range []string{"version", "prefix", "HEAD prefix", "resume skip"} {
		t.Run(mode, func(t *testing.T) {
			data := bytes.Repeat([]byte("x"), 128*1024)
			d := racerDigest(data)
			metadata := racerMetadata(t, d, len(data))
			available, status := 128, http.StatusOK
			method, rangeHeader := http.MethodGet, ""

			switch mode {
			case "version":
				metadata = racerMetadata(t, racerDigest([]byte("other version")), len(data))
				status = http.StatusBadGateway
			case "HEAD prefix":
				method = http.MethodHead
			case "resume skip":
				available = 16 * 1024
				rangeHeader = "bytes=65536-"
			}

			client := racerFakeClient(t, func(_ context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				if req.Head {
					return metadata, nil, nil
				}

				return metadata, io.NopCloser(bytes.NewReader(data[:available])), nil
			})

			server := racerServer(t, racerConfig(), client, &racerLegacyTrap{})
			if mode == "prefix" || mode == "resume skip" {
				// Superseded: the mirror no longer peeks or reads a skipped
				// prefix before committing headers. Truncation aborts the stream.
				req := handlerRequest(t, t.Context(), server, http.MethodGet, "blobs", d)
				req.Header.Set("Range", rangeHeader)

				resp, err := server.Client().Do(req)
				if err == nil {
					defer resp.Body.Close()

					_, err = io.ReadAll(resp.Body)
				}

				if err == nil {
					t.Fatal("truncated stream completed")
				}

				return
			}

			resp := racerRequest(t, server, method, "blobs", d, rangeHeader, "")

			_, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != status {
				t.Fatalf("response = %d, %v; want complete %d", resp.StatusCode, err, status)
			}

			if mode == "HEAD prefix" {
				// Superseded: HEAD needs metadata only, so a broken body cannot
				// affect it and is never opened.
				if resp.Header.Get("Gantry-Mirrored") != "1" {
					t.Fatal("metadata-only HEAD failed")
				}
			} else if resp.Header.Get("Docker-Content-Digest") != "" || resp.Header.Get("Gantry-Mirrored") != "" || resp.Header.Get("Content-Range") != "" {
				t.Fatalf("failure committed content headers: %v", resp.Header)
			}
		})
	}
}

type contentBackendFunc func(http.ResponseWriter, *http.Request, ifaces.OriginRef)

func (f contentBackendFunc) ServeContent(w http.ResponseWriter, r *http.Request, ref ifaces.OriginRef) {
	f(w, r, ref)
}

type racerReadError struct{}

func (racerReadError) Read([]byte) (int, error) { return 0, errors.New("injected stream failure") }

func TestRacerStreamFailureAbortsHTTP(t *testing.T) {
	for _, mode := range []string{"read error", "truncation", "digest mismatch", "continuation", "terminal framing", "resume read error", "resume truncation", "resume digest mismatch", "resume continuation", "resume terminal framing"} {
		t.Run(mode, func(t *testing.T) {
			failure := strings.TrimPrefix(mode, "resume ")

			data := bytes.Repeat([]byte("0123456789abcdef"), 16*1024)
			if failure == "continuation" {
				data = bytes.Repeat([]byte("0123456789abcdef"), int(racersdk.PageSize)/16+4096)
			}

			d := racerDigest(data)
			metadata := racerMetadata(t, d, len(data))
			rangeHeader, offset := "", 0

			if failure == "digest mismatch" {
				// Superseded integrity expectation: the mirror transports bytes;
				// the consumer verifies the final assembled OCI object.
				data[0] ^= 0xff
				data[len(data)-1] ^= 0xff
			}

			if strings.HasPrefix(mode, "resume ") {
				rangeHeader, offset = "bytes=65536-", 65536
			}

			var calls atomic.Int32

			client := racerFakeClient(t, func(_ context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				if req.Head {
					return metadata, nil, nil
				}

				calls.Add(1)

				if failure == "continuation" && req.Offset != 0 {
					return racersdk.Metadata{}, nil, racersdk.ErrUnavailable
				}

				var reader io.Reader = bytes.NewReader(data)

				switch failure {
				case "terminal framing":
					reader = io.MultiReader(bytes.NewReader(data), racerReadError{})
				case "read error":
					reader = io.MultiReader(bytes.NewReader(data[:128*1024]), racerReadError{})
				case "truncation":
					reader = bytes.NewReader(data[:128*1024])
				case "continuation":
					reader = bytes.NewReader(data[:int(racersdk.PageSize)])
				}

				return metadata, io.NopCloser(reader), nil
			})
			server := racerServer(t, racerConfig(), client, &racerLegacyTrap{})
			resp := racerRequest(t, server, http.MethodGet, "blobs", d, rangeHeader, "")

			wantStatus := http.StatusOK
			if offset != 0 {
				wantStatus = http.StatusPartialContent
			}

			if resp.StatusCode != wantStatus || resp.ContentLength != int64(len(data)-offset) {
				t.Fatalf("stream never started: status=%d length=%d", resp.StatusCode, resp.ContentLength)
			}

			got, err := io.ReadAll(resp.Body)
			if failure == "digest mismatch" {
				if err != nil || !bytes.Equal(got, data[offset:]) {
					t.Fatalf("corrupt bytes must reach verifying consumer: bytes=%d err=%v", len(got), err)
				}

				return
			}

			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("client read error = %v; want unexpected EOF", err)
			}

			if len(got) >= len(data)-offset || !bytes.Equal(got, data[offset:offset+len(got)]) {
				t.Fatalf("failed stream was completed, replaced, or corrupted: got %d of %d bytes", len(got), len(data)-offset)
			}

			wantCalls := int32(1)
			if failure == "continuation" {
				wantCalls = 2
			}

			if calls.Load() != wantCalls {
				t.Fatalf("origin calls = %d; want %d (no retry)", calls.Load(), wantCalls)
			}
		})
	}
}

func TestRacerAuthenticationProbeAndDelegatedCredentials(t *testing.T) {
	for _, authorization := range []string{"Bearer requester-token", "Basic dXNlcjpwYXNz"} {
		t.Run(strings.Fields(authorization)[0], func(t *testing.T) {
			data := bytes.Repeat([]byte("credential scoped bytes"), int(racersdk.PageSize)/23+100)
			d := racerDigest(data)
			upstream := &racerRegistry{t: t, data: data, digest: d, kind: ifaces.KindBlob, contentType: "application/octet-stream", authorization: authorization}
			cfg := racerConfig()
			callback := Origin(cfg, upstream)

			var calls atomic.Int32

			client := racerFakeClient(t, func(ctx context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				calls.Add(1)

				if req.Authorization != authorization || strings.Contains(req.Metadata, strings.Fields(authorization)[1]) {
					t.Error("delegated authorization missing or embedded in adapter metadata")
				}

				return callback(ctx, req)
			})
			trap := &racerLegacyTrap{challenge: `Bearer realm="https://registry.example/token",service="registry.example"`}
			server := racerServer(t, cfg, client, trap)

			probe := racerRequest(t, server, http.MethodHead, "blobs", d, "", "")
			if probe.StatusCode != http.StatusUnauthorized || probe.Header.Get("WWW-Authenticate") != trap.challenge || calls.Load() != 0 {
				t.Fatalf("authentication probe lost or fetched content: status=%d calls=%d", probe.StatusCode, calls.Load())
			}

			resp := racerRequest(t, server, http.MethodGet, "blobs", d, "", authorization)

			got, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(got, data) {
				t.Fatalf("authenticated read: status=%d bytes=%d err=%v", resp.StatusCode, len(got), err)
			}

			if trap.probes.Load() != 1 || calls.Load() != 2 || upstream.heads.Load() != 0 || upstream.pulls.Load() != 2 {
				t.Fatalf("probe/page counts = %d/%d/%d/%d; want 1/2/0/2", trap.probes.Load(), calls.Load(), upstream.heads.Load(), upstream.pulls.Load())
			}
		})
	}
}

func TestRacerRejectedCredentialPreservesRememberedChallenge(t *testing.T) {
	if handlerTLSSubprocess(t) {
		return
	}

	data := []byte("protected registry blob")
	d := racerDigest(data)

	const challenge = `Bearer realm="https://registry.example/token",scope="repository:library/image:pull",service="registry.example"`

	var (
		probes, heads, gets atomic.Int32
		rejectMethod        atomic.Value
	)

	rejectMethod.Store(http.MethodHead)

	registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			probes.Add(1)
			w.WriteHeader(http.StatusOK)

			return
		}

		if r.URL.Path != "/v2/library/image/blobs/"+d.String() {
			t.Errorf("unexpected registry request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)

			return
		}

		switch r.Method {
		case http.MethodHead:
			heads.Add(1)
		case http.MethodGet:
			gets.Add(1)
		default:
			t.Errorf("unexpected registry method %s", r.Method)
		}

		auth := r.Header.Get("Authorization")
		if auth != "Bearer refreshed" && (r.Method == rejectMethod.Load() || auth != "Bearer expired") {
			w.Header().Set("WWW-Authenticate", challenge)
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Docker-Content-Digest", d.String())
		http.ServeContent(w, r, "blob", time.Time{}, bytes.NewReader(data))
	}))
	t.Cleanup(registry.Close)

	roots := x509.NewCertPool()
	roots.AddCert(registry.Certificate())
	x509.SetFallbackRoots(roots)

	for _, tc := range []struct {
		rejected, mode string
		heads, gets    int32
		unauthorized   bool
	}{
		{http.MethodHead, "GET", 0, 1, false},
		{http.MethodHead, "HEAD", 1, 0, true},
		{http.MethodHead, "resume", 1, 0, true},
		{http.MethodGet, "GET", 0, 1, true},
		{http.MethodGet, "HEAD", 1, 0, false},
		{http.MethodGet, "resume", 2, 1, true},
	} {
		t.Run(tc.rejected+" rejection/"+tc.mode, func(t *testing.T) {
			probes.Store(0)
			heads.Store(0)
			gets.Store(0)
			rejectMethod.Store(tc.rejected)

			cfg := racerConfig()
			cfg.UpstreamRegistries[0].Endpoint = registry.URL

			upstream, err := registryorigin.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			// Seed the anonymous /v2/ result. A later repository rejection
			// must replace it in this same origin client's challenge cache.
			initial, required, err := upstream.AuthenticationChallenge(context.Background(), "registry.example")
			if err != nil || required || initial != "" || probes.Load() != 1 {
				t.Fatalf("public /v2/ probe: challenge=%q required=%v err=%v probes=%d", initial, required, err, probes.Load())
			}

			callback := Origin(cfg, upstream)

			var calls atomic.Int32

			client := racerFakeClient(t, func(ctx context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				calls.Add(1)
				return callback(ctx, req)
			})
			trap := &racerLegacyTrap{}
			server := racerServer(t, cfg, client, trap, upstream)

			method, rangeHeader, status, want := http.MethodGet, "", http.StatusOK, data

			switch tc.mode {
			case "HEAD":
				method, want = http.MethodHead, nil
			case "resume":
				rangeHeader, status, want = "bytes=4-", http.StatusPartialContent, data[4:]
			}

			resp := racerRequest(t, server, method, "blobs", d, rangeHeader, "Bearer expired")
			// HEAD never opens a body; full GET obtains metadata from its
			// bounded response. Preserve both superseded rejection cases as
			// proof that the unnecessary request no longer occurs.
			wantStatus, wantChallenge := status, ""
			if tc.unauthorized {
				wantStatus, wantChallenge = http.StatusUnauthorized, challenge
			}

			if resp.StatusCode != wantStatus || resp.Header.Get("WWW-Authenticate") != wantChallenge {
				t.Fatalf("rejected credential: status=%d challenge=%q; want 401 with remembered repository challenge", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
			}

			resp.Body.Close()

			wantHeads, wantGets, wantProbes := tc.heads, tc.gets, int32(0)
			if tc.unauthorized {
				wantProbes = 1
			}

			if calls.Load() != wantHeads+wantGets || heads.Load() != wantHeads || gets.Load() != wantGets || probes.Load() != 1 || trap.probes.Load() != wantProbes {
				t.Fatalf("rejection retried content or lost cached challenge: sdk=%d heads=%d gets=%d v2=%d challenges=%d", calls.Load(), heads.Load(), gets.Load(), probes.Load(), trap.probes.Load())
			}

			refreshed := racerRequest(t, server, method, "blobs", d, rangeHeader, "Bearer refreshed")

			got, err := io.ReadAll(refreshed.Body)
			if err != nil || refreshed.StatusCode != status || !bytes.Equal(got, want) || refreshed.Header.Get("WWW-Authenticate") != "" {
				t.Fatalf("refreshed credential: status=%d body=%q err=%v", refreshed.StatusCode, got, err)
			}

			if tc.mode != "GET" {
				wantHeads++
			}

			if tc.mode == "resume" {
				wantHeads++
			}

			if tc.mode != "HEAD" {
				wantGets++
			}

			if calls.Load() != wantHeads+wantGets || heads.Load() != wantHeads || gets.Load() != wantGets || trap.probes.Load() != wantProbes {
				t.Fatalf("refresh counts: sdk=%d heads=%d gets=%d challenges=%d", calls.Load(), heads.Load(), gets.Load(), trap.probes.Load())
			}
		})
	}
}

func TestRacerRejectedCredentialProbeFailureKeepsUnauthorized(t *testing.T) {
	for _, outcome := range []string{"error", "not required", "empty", "timeout"} {
		t.Run(outcome, func(t *testing.T) {
			var calls atomic.Int32

			client := racerFakeClient(t, func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				calls.Add(1)
				return racersdk.Metadata{}, nil, racersdk.ErrUnauthorized
			})
			probe := racerChallengeFunc(func(ctx context.Context, registry string) (string, bool, error) {
				if registry != "registry.example" {
					t.Errorf("challenge registry = %q", registry)
				}

				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 2*time.Second {
					t.Error("challenge probe lacks the bounded authentication deadline")
					return "", false, errors.New("unbounded probe")
				}

				switch outcome {
				case "error":
					return `Basic realm="unusable"`, true, errors.New("probe failed")
				case "not required":
					return `Basic realm="unused"`, false, nil
				case "timeout":
					<-ctx.Done()
					return "", false, ctx.Err()
				default:
					return "", true, nil
				}
			})
			trap := &racerLegacyTrap{}
			server := racerServer(t, racerConfig(), client, trap, probe)

			resp := racerRequest(t, server, http.MethodGet, "blobs", racerDigest(nil), "", "Bearer expired")
			if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != "" {
				t.Fatalf("failed probe: status=%d challenge=%q; want 401 without challenge", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
			}

			if calls.Load() != 1 || trap.probes.Load() != 1 {
				t.Fatalf("failed probe retried content: sdk=%d challenges=%d", calls.Load(), trap.probes.Load())
			}
		})
	}
}

type racerBlockingBody struct {
	prefix  *bytes.Reader
	blocked chan struct{}
	closed  chan struct{}
	once    sync.Once
	closes  atomic.Int32
}

func (b *racerBlockingBody) Read(p []byte) (int, error) {
	if b.prefix.Len() != 0 {
		return b.prefix.Read(p)
	}

	b.once.Do(func() { close(b.blocked) })
	<-b.closed

	return 0, io.ErrClosedPipe
}

func (b *racerBlockingBody) Close() error {
	if b.closes.Add(1) == 1 {
		close(b.closed)
	}

	return nil
}

func TestRacerHEADDuringBulkStream(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 256*1024)
	d := racerDigest(data)
	metadata := racerMetadata(t, d, len(data))
	body := &racerBlockingBody{prefix: bytes.NewReader(data[:64*1024]), blocked: make(chan struct{}), closed: make(chan struct{})}

	var heads, gets atomic.Int32

	client := racerFakeClient(t, func(_ context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if req.Head {
			heads.Add(1)
			return metadata, nil, nil
		}

		gets.Add(1)

		return metadata, body, nil
	})
	server := racerServer(t, racerConfig(), client, &racerLegacyTrap{})

	bulk := racerRequest(t, server, http.MethodGet, "blobs", d, "", "")
	defer bulk.Body.Close()

	racerAwaitStream(t, body.blocked, "bulk body blocked in Read")

	head := racerRequest(t, server, http.MethodHead, "blobs", d, "", "")
	if head.StatusCode != http.StatusOK || head.ContentLength != int64(len(data)) || heads.Load() != 1 || gets.Load() != 1 {
		t.Fatalf("HEAD mixed metadata and bulk: status=%d length=%d heads=%d gets=%d", head.StatusCode, head.ContentLength, heads.Load(), gets.Load())
	}
}

func TestRacerManifestGETWithSaturatedBulkPool(t *testing.T) {
	data := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	d := racerDigest(data)
	origin := racerPageOrigin(t, d, data)

	var heads, gets atomic.Int32

	client := racerFakeClient(t, func(ctx context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if req.Head {
			heads.Add(1)
		} else {
			gets.Add(1)
		}

		metadata, body, err := origin(ctx, req)
		metadata.ContentType = "application/vnd.oci.image.index.v1+json"

		return metadata, body, err
	})

	request, err := Request(ifaces.OriginRef{Registry: "registry.example", Repository: "library/image", Digest: d, Kind: ifaces.KindBlob}, "")
	if err != nil {
		t.Fatal(err)
	}
	// Hold actual SDK Objects rather than a mock semaphore: all default bulk
	// admission slots remain occupied until the returned bodies are consumed.
	for range 64 {
		value, err := client.Get(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		defer value.Close()
	}

	server := racerServer(t, racerConfig(), client, &racerLegacyTrap{})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	queued := handlerRequest(t, ctx, server, http.MethodGet, "blobs", d)

	done := make(chan error, 1)
	finished := make(chan struct{})

	go func() {
		defer close(finished)

		resp, err := server.Client().Do(queued)
		if err == nil {
			resp.Body.Close()
		}

		done <- err
	}()

	defer func() { cancel(); <-finished }()

	select {
	case err := <-done:
		t.Fatalf("extra blob request bypassed saturated bulk pool: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	manifestCtx, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()

	req := handlerRequest(t, manifestCtx, server, http.MethodGet, "manifests", d)

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("manifest waited for bulk capacity: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(body, data) || resp.Header.Get("Content-Type") != "application/vnd.oci.image.index.v1+json" {
		t.Fatalf("isolated manifest: status=%d bytes=%d err=%v", resp.StatusCode, len(body), err)
	}

	if heads.Load() != 0 || gets.Load() != 65 {
		t.Fatalf("manifest used HEAD or bulk capacity: heads=%d gets=%d", heads.Load(), gets.Load())
	}
}

func TestRacerManifestSizeLimitBeforeHeaders(t *testing.T) {
	for _, size := range []int{int(racersdk.PageSize), int(racersdk.PageSize) + 1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			data := bytes.Repeat([]byte("x"), size)
			d := racerDigest(data)
			client := racerFakeClient(t, racerPageOrigin(t, d, data))
			server := racerServer(t, racerConfig(), client, &racerLegacyTrap{})
			resp := racerRequest(t, server, http.MethodGet, "manifests", d, "", "")

			body := handlerReadBody(t, resp)

			if size == int(racersdk.PageSize) {
				if resp.StatusCode != http.StatusOK || !bytes.Equal(body, data) {
					t.Fatal("manifest at the size limit was rejected")
				}

				return
			}

			if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Gantry-Mirrored") != "" || resp.Header.Get("Docker-Content-Digest") != "" || resp.ContentLength == int64(size) {
				t.Fatalf("oversize manifest committed content headers: status=%d headers=%v", resp.StatusCode, resp.Header)
			}
		})
	}
}

func TestRacerStalledDownstreamClosesSDKStream(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 8<<20)
	d := racerDigest(data)
	client := racerFakeClient(t, racerPageOrigin(t, d, data))
	observed := make(chan HTTPObservation, 1)
	server := httptest.NewServer(handlerStreamHandler(t, client, 100*time.Millisecond, func(observation HTTPObservation) { observed <- observation }))
	t.Cleanup(server.Close)

	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if tcp, ok := conn.(*net.TCPConn); ok {
		if err := tcp.SetReadBuffer(1024); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := fmt.Fprintf(conn, "GET /v2/library/image/blobs/%s HTTP/1.1\r\nHost: registry.example\r\n\r\n", d); err != nil {
		t.Fatal(err)
	}

	if result := handlerAwaitObservation(t, observed); !result.Aborted || result.Bytes == 0 || result.Bytes >= int64(len(data)) || result.Duration < 100*time.Millisecond || result.Duration > 5*time.Second {
		t.Fatalf("stalled downstream observation: %+v", result)
	}

	// The aborted bulk stream must not prevent a new metadata request.
	request, err := Request(ifaces.OriginRef{Registry: "registry.example", Repository: "library/image", Digest: d, Kind: ifaces.KindBlob}, "")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.Stat(t.Context(), request); err != nil {
		t.Fatal(err)
	}
}

func TestRacerCancellationClosesStream(t *testing.T) {
	// "resume skip" is retained as the historical case name. Cancellation now
	// interrupts the requested suffix directly; the mirror never skips bytes.
	for _, mode := range []string{"before headers", "streaming", "resume skip"} {
		t.Run(mode, func(t *testing.T) {
			data := bytes.Repeat([]byte("x"), 256*1024)
			d := racerDigest(data)
			metadata := racerMetadata(t, d, len(data))

			prefixSize := 64 * 1024
			if mode == "before headers" {
				prefixSize = 0
			}

			body := &racerBlockingBody{prefix: bytes.NewReader(data[:prefixSize]), blocked: make(chan struct{}), closed: make(chan struct{})}
			client := racerFakeClient(t, func(_ context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				if req.Head {
					return metadata, nil, nil
				}

				return metadata, body, nil
			})
			server := racerServer(t, racerConfig(), client, &racerLegacyTrap{})

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			req := handlerRequest(t, ctx, server, http.MethodGet, "blobs", d)

			if mode == "resume skip" {
				req.Header.Set("Range", "bytes=131072-")
			}

			done := make(chan error, 1)
			started := make(chan struct{})

			go func() {
				resp, err := server.Client().Do(req)
				if err == nil {
					close(started)

					_, err = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}

				done <- err
			}()

			racerAwaitStream(t, body.blocked, "origin blocked in Read")

			if mode == "streaming" {
				racerAwaitStream(t, started, "mirror response headers")
			}

			cancel()

			racerAwaitStream(t, body.closed, "origin stream closed after cancellation")

			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("client error = %v; want cancellation", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("client read did not stop after cancellation")
			}

			if body.closes.Load() != 1 {
				t.Fatalf("origin stream closed %d times; want once", body.closes.Load())
			}
		})
	}
}
