// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	registryorigin "github.com/Azure/unbounded/internal/gantry/origin"
	gantryracer "github.com/Azure/unbounded/internal/gantry/racer"
	"github.com/Azure/unbounded/pkg/racersdk"
)

type racerRepositoryChallengeOrigin struct {
	racerChallengeOrigin
	mirror.RepositoryAuthenticationChallenger
}

func TestRacerRemoteRejectedCredentialRepositoryChallenge(t *testing.T) {
	// Keep test-only TLS roots isolated, just as in the same-node regression.
	const child = "GANTRY_RACER_REMOTE_CHALLENGE_TEST_CHILD"
	if os.Getenv(child) != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRacerRemoteRejectedCredentialRepositoryChallenge$", "-test.timeout=25s")

		cmd.Env = append(os.Environ(), child+"=1", "GODEBUG="+os.Getenv("GODEBUG")+",x509usefallbackroots=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("TLS integration subprocess: %v\n%s", err, output)
		}

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

				callback := gantryracer.Origin(cfg, originB)
				client := racerFakeClient(t, callback)
				trap := &racerLegacyTrap{}
				origin := racerRepositoryChallengeOrigin{racerChallengeOrigin{trap, requesterA}, requesterA}

				server := httptest.NewServer(mirror.RacerHTTPHandler(mirror.New(cfg, trap, origin, mirror.WithRacer(client)).Handler(), 0, nil))
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
	_ ifaces.OriginPuller                       = racerRepositoryChallengeOrigin{}
	_ mirror.RepositoryAuthenticationChallenger = racerRepositoryChallengeOrigin{}
)

type racerRepositoryChallengeFunc func(context.Context, ifaces.OriginRef) (string, bool, error)

func (f racerRepositoryChallengeFunc) RepositoryAuthenticationChallenge(ctx context.Context, ref ifaces.OriginRef) (string, bool, error) {
	return f(ctx, ref)
}

func TestRacerRepositoryChallengeOnlyOnUnauthorized(t *testing.T) {
	for _, kind := range []racersdk.ErrorKind{racersdk.ErrorForbidden, racersdk.ErrorNotFound, racersdk.ErrorUnavailable} {
		t.Run(kind.String(), func(t *testing.T) {
			client := racerFakeClient(t, func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				return racersdk.Metadata{}, nil, racersdk.NewOriginError(kind, nil)
			})
			trap := &racerLegacyTrap{}

			var probes atomic.Int32

			probe := racerRepositoryChallengeFunc(func(context.Context, ifaces.OriginRef) (string, bool, error) {
				probes.Add(1)
				return `Basic realm="unused"`, true, nil
			})
			origin := racerRepositoryChallengeOrigin{racerChallengeOrigin{trap, trap}, probe}

			server := httptest.NewServer(mirror.New(racerConfig(), trap, origin, mirror.WithRacer(client)).Handler())
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
