// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	registryorigin "github.com/Azure/unbounded/internal/gantry/origin"
	gantryracer "github.com/Azure/unbounded/internal/gantry/racer"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
	"github.com/Azure/unbounded/pkg/racersdk"
)

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

	client, cleanup, err := racersdk.NewFakeClient(origin)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(cleanup)

	return client
}

func racerServer(t *testing.T, cfg *config.Config, client mirror.RacerClient, trap *racerLegacyTrap, challengers ...mirror.AuthenticationChallenger) *httptest.Server {
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

	server := httptest.NewServer(mirror.New(cfg, trap, origin, mirror.WithRacer(client)).Handler())
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

	req, err := http.NewRequest(method, server.URL+"/v2/library/image/"+route+"/"+d.String(), nil)
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Range", rangeHeader)
	req.Header.Set("Authorization", authorization)

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { resp.Body.Close() })

	return resp
}

func racerMetadata(t *testing.T, d digest.Digest, size int) racersdk.Metadata {
	t.Helper()

	tag, err := racersdk.ParseETag(`"` + d.String() + `"`)
	if err != nil {
		t.Fatal(err)
	}

	return racersdk.Metadata{Size: racersdk.ByteLength(size), ETag: tag, ExpiresAt: time.Unix(2000000000, 0)}
}

func racerPageOrigin(t *testing.T, d digest.Digest, data []byte) racersdk.Origin {
	t.Helper()
	metadata := racerMetadata(t, d, len(data))

	return func(_ context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if len(data) == 0 {
			return metadata, nil, nil
		}

		page, _ := req.Range()

		first, last, err := page.Resolve(metadata.Size)
		if err != nil {
			return metadata, nil, err
		}

		return metadata, io.NopCloser(bytes.NewReader(data[first : last+1])), nil
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

func (u *racerRegistry) Pull(ctx context.Context, ref ifaces.OriginRef) (io.ReadCloser, int64, error) {
	u.check(ctx, ref)
	u.pulls.Add(1)

	if ref.Offset < 0 || ref.Offset >= int64(len(u.data)) {
		return nil, 0, errors.New("invalid registry offset")
	}

	return io.NopCloser(bytes.NewReader(u.data[ref.Offset:])), int64(len(u.data)), nil
}

func TestRacerGETAndHEAD(t *testing.T) {
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
				client := racerFakeClient(t, racerPageOrigin(t, d, tc.data))
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

				got, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}

				want := tc.data
				if method == http.MethodHead {
					want = nil
				}

				if !bytes.Equal(got, want) {
					t.Fatalf("response differs: got %d bytes; want %d", len(got), len(want))
				}
			})
		}
	}
}

func TestRacerResumeAndInvalidRange(t *testing.T) {
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

			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}

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
	client := racerFakeClient(t, racerPageOrigin(t, d, data))
	server := racerServer(t, racerConfig(), client, &racerLegacyTrap{})
	offset := int(racersdk.PageSize) + 7
	resp := racerRequest(t, server, http.MethodGet, "blobs", d, fmt.Sprintf("bytes=%d-", offset), "")

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	wantRange := fmt.Sprintf("bytes %d-%d/%d", offset, len(data)-1, len(data))
	if resp.StatusCode != http.StatusPartialContent || resp.Header.Get("Content-Range") != wantRange || !bytes.Equal(got, data[offset:]) {
		t.Fatalf("cross-page resume failed: status=%d range=%q bytes=%d", resp.StatusCode, resp.Header.Get("Content-Range"), len(got))
	}
}

func TestRacerErrorsNeverUseLegacyContent(t *testing.T) {
	for _, tc := range []struct {
		kind   racersdk.ErrorKind
		status int
	}{
		{racersdk.ErrorNotFound, http.StatusNotFound},
		{racersdk.ErrorUnauthorized, http.StatusUnauthorized},
		{racersdk.ErrorForbidden, http.StatusForbidden},
		{racersdk.ErrorUnavailable, http.StatusServiceUnavailable},
		{racersdk.ErrorInternal, http.StatusBadGateway},
		{racersdk.ErrorBadGateway, http.StatusBadGateway},
		{racersdk.ErrorVersionUnavailable, http.StatusBadGateway},
	} {
		for _, mode := range []string{"GET", "HEAD", "resume"} {
			t.Run(tc.kind.String()+"/"+mode, func(t *testing.T) {
				var calls atomic.Int32

				client := racerFakeClient(t, func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
					calls.Add(1)
					return racersdk.Metadata{}, nil, racersdk.NewOriginError(tc.kind, errors.New("private upstream detail"))
				})
				server := racerServer(t, racerConfig(), client, &racerLegacyTrap{})

				method, rangeHeader := http.MethodGet, ""

				switch mode {
				case "HEAD":
					method = http.MethodHead
				case "resume":
					rangeHeader = "bytes=4-"
				}

				resp := racerRequest(t, server, method, "blobs", racerDigest([]byte("missing")), rangeHeader, "")

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
	for _, state := range []string{"nil", "closed"} {
		for _, mode := range []string{"GET", "HEAD", "resume"} {
			t.Run(state+"/"+mode, func(t *testing.T) {
				cfg := racerConfig()
				cfg.RacerEnabled = true

				var client mirror.RacerClient

				if state == "closed" {
					fake := racerFakeClient(t, func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
						t.Error("closed client reached origin")
						return racersdk.Metadata{}, nil, errors.New("unexpected origin request")
					})
					fake.Close()
					client = fake
				}

				server := racerServer(t, cfg, client, &racerLegacyTrap{})

				method, rangeHeader := http.MethodGet, ""

				switch mode {
				case "HEAD":
					method = http.MethodHead
				case "resume":
					rangeHeader = "bytes=4-"
				}

				resp := racerRequest(t, server, method, "blobs", racerDigest(nil), rangeHeader, "")
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
			available, status := 128, http.StatusServiceUnavailable
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

			client := racerFakeClient(t, func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				return metadata, io.NopCloser(bytes.NewReader(data[:available])), nil
			})
			server := racerServer(t, racerConfig(), client, &racerLegacyTrap{})
			resp := racerRequest(t, server, method, "blobs", d, rangeHeader, "")

			_, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != status {
				t.Fatalf("response = %d, %v; want complete %d", resp.StatusCode, err, status)
			}

			if resp.Header.Get("Docker-Content-Digest") != "" || resp.Header.Get("Gantry-Mirrored") != "" || resp.Header.Get("Content-Range") != "" {
				t.Fatalf("failure committed content headers: %v", resp.Header)
			}
		})
	}
}

type racerReadError struct{}

func (racerReadError) Read([]byte) (int, error) { return 0, errors.New("injected stream failure") }

func TestRacerStreamFailureAbortsHTTP(t *testing.T) {
	for _, mode := range []string{"read error", "truncation", "digest mismatch", "continuation", "resume read error", "resume truncation", "resume digest mismatch", "resume continuation"} {
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
				// Corrupt only skipped bytes on resume: a suffix-only verifier
				// would incorrectly accept the returned bytes as valid.
				data[0] ^= 0xff
			}

			if strings.HasPrefix(mode, "resume ") {
				rangeHeader, offset = "bytes=65536-", 65536
			}

			var calls atomic.Int32

			client := racerFakeClient(t, func(_ context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				calls.Add(1)

				if failure == "continuation" && req.Operation() == racersdk.OperationPinned {
					return racersdk.Metadata{}, nil, racersdk.NewOriginError(racersdk.ErrorUnavailable, nil)
				}

				var reader io.Reader = bytes.NewReader(data)

				switch failure {
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
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("client read error = %v; want unexpected EOF", err)
			}

			if len(got) == 0 || len(got) >= len(data)-offset || !bytes.Equal(got, data[offset:offset+len(got)]) {
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
			callback := gantryracer.Origin(cfg, upstream)

			var calls atomic.Int32

			client := racerFakeClient(t, func(ctx context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				calls.Add(1)

				if req.Context().Authorization().ForOrigin() != authorization || strings.Contains(req.Context().Metadata().ForOrigin(), strings.Fields(authorization)[1]) {
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

			if trap.probes.Load() != 1 || calls.Load() != 2 || upstream.heads.Load() != 2 || upstream.pulls.Load() != 2 {
				t.Fatalf("probe/page counts = %d/%d/%d/%d; want 1/2/2/2", trap.probes.Load(), calls.Load(), upstream.heads.Load(), upstream.pulls.Load())
			}
		})
	}
}

func TestRacerRejectedCredentialPreservesRememberedChallenge(t *testing.T) {
	// origin.Client intentionally has no transport injection option. Isolate the
	// test TLS trust store in a subprocess instead of changing process-wide roots
	// for other tests or adding a production-only test hook.
	const child = "GANTRY_RACER_CHALLENGE_TEST_CHILD"
	if os.Getenv(child) != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRacerRejectedCredentialPreservesRememberedChallenge$", "-test.timeout=25s")

		cmd.Env = append(os.Environ(), child+"=1", "GODEBUG="+os.Getenv("GODEBUG")+",x509usefallbackroots=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("TLS integration subprocess: %v\n%s", err, output)
		}

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

		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("Content-Type", "application/octet-stream")

		if r.Method == http.MethodGet {
			w.Write(data)
		}
	}))
	t.Cleanup(registry.Close)

	roots := x509.NewCertPool()
	roots.AddCert(registry.Certificate())
	x509.SetFallbackRoots(roots)

	for _, rejected := range []string{http.MethodHead, http.MethodGet} {
		for _, mode := range []string{"GET", "HEAD", "resume"} {
			t.Run(rejected+" rejection/"+mode, func(t *testing.T) {
				probes.Store(0)
				heads.Store(0)
				gets.Store(0)
				rejectMethod.Store(rejected)

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

				callback := gantryracer.Origin(cfg, upstream)

				var calls atomic.Int32

				client := racerFakeClient(t, func(ctx context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
					calls.Add(1)
					return callback(ctx, req)
				})
				trap := &racerLegacyTrap{}
				server := racerServer(t, cfg, client, trap, upstream)

				method, rangeHeader, status, want := http.MethodGet, "", http.StatusOK, data

				switch mode {
				case "HEAD":
					method, want = http.MethodHead, nil
				case "resume":
					rangeHeader, status, want = "bytes=4-", http.StatusPartialContent, data[4:]
				}

				resp := racerRequest(t, server, method, "blobs", d, rangeHeader, "Bearer expired")
				if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != challenge {
					t.Fatalf("rejected credential: status=%d challenge=%q; want 401 with remembered repository challenge", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
				}

				resp.Body.Close()

				wantGets := int32(0)
				if rejected == http.MethodGet {
					wantGets = 1
				}

				if calls.Load() != 1 || heads.Load() != 1 || gets.Load() != wantGets || probes.Load() != 1 || trap.probes.Load() != 1 {
					t.Fatalf("rejection retried content or lost cached challenge: sdk=%d heads=%d gets=%d v2=%d challenges=%d", calls.Load(), heads.Load(), gets.Load(), probes.Load(), trap.probes.Load())
				}

				refreshed := racerRequest(t, server, method, "blobs", d, rangeHeader, "Bearer refreshed")

				got, err := io.ReadAll(refreshed.Body)
				if err != nil || refreshed.StatusCode != status || !bytes.Equal(got, want) || refreshed.Header.Get("WWW-Authenticate") != "" {
					t.Fatalf("refreshed credential: status=%d body=%q err=%v", refreshed.StatusCode, got, err)
				}

				if calls.Load() != 2 || heads.Load() != 2 || gets.Load() != wantGets+1 || trap.probes.Load() != 1 {
					t.Fatalf("refresh counts: sdk=%d heads=%d gets=%d challenges=%d", calls.Load(), heads.Load(), gets.Load(), trap.probes.Load())
				}
			})
		}
	}
}

func TestRacerRejectedCredentialProbeFailureKeepsUnauthorized(t *testing.T) {
	for _, outcome := range []string{"error", "not required", "empty", "timeout"} {
		t.Run(outcome, func(t *testing.T) {
			var calls atomic.Int32

			client := racerFakeClient(t, func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				calls.Add(1)
				return racersdk.Metadata{}, nil, racersdk.NewOriginError(racersdk.ErrorUnauthorized, nil)
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

func TestRacerCancellationClosesStream(t *testing.T) {
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
			client := racerFakeClient(t, func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				return metadata, body, nil
			})
			server := racerServer(t, racerConfig(), client, &racerLegacyTrap{})

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v2/library/image/blobs/"+d.String(), nil)
			if err != nil {
				t.Fatal(err)
			}

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

			select {
			case <-body.blocked:
			case <-time.After(5 * time.Second):
				t.Fatal("origin never blocked in Read")
			}

			if mode == "streaming" {
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					t.Fatal("mirror never started the response")
				}
			}

			cancel()

			select {
			case <-body.closed:
			case <-time.After(5 * time.Second):
				t.Fatal("request cancellation did not close the origin stream")
			}

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
