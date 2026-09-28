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
	"net"
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

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/plugins/content/local"
	ocidigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

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

	server := httptest.NewServer(mirror.RacerHTTPHandler(mirror.New(cfg, trap, origin, mirror.WithRacer(client)).Handler(), 0, nil))
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

	return racersdk.Metadata{Size: racersdk.ByteLength(size), ETag: tag, ContentType: "application/octet-stream", ExpiresAt: time.Unix(2000000000, 0)}
}

func racerPageOrigin(t *testing.T, d digest.Digest, data []byte) racersdk.Origin {
	t.Helper()
	metadata := racerMetadata(t, d, len(data))

	return func(_ context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if req.Operation() == racersdk.OperationHead || len(data) == 0 {
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
	// Keep the historical final-chunk boundary cases even though the mirror no
	// longer withholds a final chunk or examines manifest payloads.
	for _, tc := range []struct {
		name, route, mediaType string
		data                   []byte
	}{
		{"blob", "blobs", "application/octet-stream", []byte("layer bytes")},
		{"empty", "blobs", "application/octet-stream", nil},
		{"below final chunk", "blobs", "application/octet-stream", bytes.Repeat([]byte{0}, 32*1024-1)},
		{"exact final chunk", "blobs", "application/octet-stream", bytes.Repeat([]byte{0}, 32*1024)},
		{"above final chunk", "blobs", "application/octet-stream", bytes.Repeat([]byte{0}, 32*1024+1)},
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
					if req.Operation() == racersdk.OperationHead {
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

				if method == http.MethodHead && (heads.Load() != 1 || gets.Load() != 0) {
					t.Fatalf("HEAD fetched object bytes: heads=%d gets=%d", heads.Load(), gets.Load())
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
	origin := racerPageOrigin(t, d, data)

	var heads, gets atomic.Int32

	client := racerFakeClient(t, func(ctx context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if req.Operation() == racersdk.OperationHead {
			heads.Add(1)
		} else {
			gets.Add(1)

			page, _ := req.Range()

			first, _, err := page.Resolve(racersdk.ByteLength(len(data)))
			if err != nil || first != racersdk.ByteOffset(racersdk.PageSize) {
				t.Errorf("resume fetched skipped page: first=%d err=%v", first, err)
			}
		}

		return origin(ctx, req)
	})
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

	if heads.Load() != 1 || gets.Load() != 1 {
		t.Fatalf("resume transcript: heads=%d gets=%d; want 1/1", heads.Load(), gets.Load())
	}
}

type racerMetadataClient struct {
	*racersdk.Client
	metadata racersdk.Metadata
	returned *racersdk.Metadata
}

func (c racerMetadataClient) Get(ctx context.Context, req racersdk.Request, options ...racersdk.ReadOptions) (*racersdk.Value, error) {
	if c.returned != nil && len(options) == 1 {
		selected := options[0]
		selected.Metadata = c.returned
		options = []racersdk.ReadOptions{selected}
	}

	return c.Client.Get(ctx, req, options...)
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

func (c *racerTranscriptClient) Get(ctx context.Context, req racersdk.Request, options ...racersdk.ReadOptions) (*racersdk.Value, error) {
	c.gets.Add(1)

	c.options <- options

	return c.Client.Get(ctx, req, options...)
}

func TestRacerSDKRequestTranscript(t *testing.T) {
	for _, mode := range []string{"GET", "HEAD", "resume", "unsatisfiable", "manifest"} {
		t.Run(mode, func(t *testing.T) {
			data := bytes.Repeat([]byte("x"), int(racersdk.PageSize)+99)
			if mode == "manifest" {
				data = []byte(`{"schemaVersion":2}`)
			}

			d := racerDigest(data)
			client := &racerTranscriptClient{Client: racerFakeClient(t, racerPageOrigin(t, d, data)), options: make(chan []racersdk.ReadOptions, 1)}
			server := racerServer(t, racerConfig(), client, &racerLegacyTrap{})
			method, rangeHeader, route := http.MethodGet, "", "blobs"
			wantStats, wantGets := int32(0), int32(1)

			switch mode {
			case "manifest":
				route = "manifests"
			case "HEAD":
				method, wantStats, wantGets = http.MethodHead, 1, 0
			case "resume":
				rangeHeader, wantStats = fmt.Sprintf("bytes=%d-", racersdk.PageSize+7), 1
			case "unsatisfiable":
				rangeHeader, wantStats, wantGets = fmt.Sprintf("bytes=%d-", len(data)), 1, 0
			}

			resp := racerRequest(t, server, method, route, d, rangeHeader, "")
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				t.Fatal(err)
			}

			if client.stats.Load() != wantStats || client.gets.Load() != wantGets {
				t.Fatalf("SDK transcript: Stat=%d Get=%d; want %d/%d", client.stats.Load(), client.gets.Load(), wantStats, wantGets)
			}

			if wantGets == 0 {
				return
			}

			options := <-client.options
			if mode == "manifest" {
				if len(options) != 1 || !options[0].SmallObject || options[0].Metadata != nil || options[0].Offset != 0 || options[0].Length != 0 || options[0].Pin != (racersdk.ETag{}) {
					t.Fatal("manifest must use the small-object bootstrap without metadata preflight")
				}

				return
			}

			if mode == "GET" {
				if len(options) != 0 {
					t.Fatal("full GET must use bootstrap, without a redundant metadata read")
				}

				return
			}

			if len(options) != 1 || options[0].Offset != racersdk.ByteOffset(racersdk.PageSize+7) || options[0].Length != 0 || options[0].Pin.String() != `"`+d.String()+`"` || options[0].Metadata == nil || options[0].Metadata.Size != racersdk.ByteLength(len(data)) {
				t.Fatal("resume must use exact offset, through-EOF length, and selected digest pin")
			}
		})
	}
}

func (c racerMetadataClient) Stat(context.Context, racersdk.Request) (racersdk.Metadata, error) {
	return c.metadata, nil
}

func TestRacerRejectsMixedMetadata(t *testing.T) {
	for _, mode := range []string{"size", "content type", "version", "overflow"} {
		t.Run(mode, func(t *testing.T) {
			data := []byte("0123456789")
			d := racerDigest(data)
			metadata := racerMetadata(t, d, len(data))

			switch mode {
			case "size":
				metadata.Size++
			case "content type":
				metadata.ContentType = "application/vnd.oci.image.index.v1+json"
			case "version":
				metadata = racerMetadata(t, racerDigest(nil), len(data))
			case "overflow":
				metadata.Size = racersdk.ByteLength(1) << 63
			}

			client := racerMetadataClient{Client: racerFakeClient(t, racerPageOrigin(t, d, data)), metadata: metadata}
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
		for _, source := range []string{"SDK snapshot", "independent returned metadata"} {
			t.Run(tc.name+"/"+source, func(t *testing.T) {
				data := []byte("0123456789")
				d := racerDigest(data)
				initial, actual := racerMetadata(t, d, len(data)), racerMetadata(t, d, len(data))
				initial.ContentType, actual.ContentType = tc.initial, tc.returned
				origin := racerPageOrigin(t, d, data)

				client := racerMetadataClient{metadata: initial, Client: racerFakeClient(t, func(ctx context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
					_, body, err := origin(ctx, req)
					return actual, body, err
				})}
				if source == "independent returned metadata" {
					client.returned = &actual
				}

				server := racerServer(t, racerConfig(), client, &racerLegacyTrap{})

				resp := racerRequest(t, server, http.MethodGet, "blobs", d, "bytes=4-", "")
				if tc.initial != "" && tc.returned != "" && tc.initial != tc.returned {
					if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Gantry-Mirrored") != "" {
						t.Fatal("conflicting present media types were accepted")
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
				if req.Operation() == racersdk.OperationHead {
					return metadata, nil, nil
				}

				return metadata, io.NopCloser(bytes.NewReader(data[:available])), nil
			})

			server := racerServer(t, racerConfig(), client, &racerLegacyTrap{})
			if mode == "prefix" || mode == "resume skip" {
				// Superseded: the mirror no longer peeks or reads a skipped
				// prefix before committing headers. Truncation aborts the stream.
				req, err := http.NewRequest(http.MethodGet, server.URL+"/v2/library/image/blobs/"+d.String(), nil)
				if err != nil {
					t.Fatal(err)
				}

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
				if req.Operation() == racersdk.OperationHead {
					return metadata, nil, nil
				}

				calls.Add(1)

				page, _ := req.Range()

				first, _, rangeErr := page.Resolve(metadata.Size)
				if rangeErr != nil {
					return metadata, nil, rangeErr
				}

				if failure == "continuation" && first != 0 {
					return racersdk.Metadata{}, nil, racersdk.NewOriginError(racersdk.ErrorUnavailable, nil)
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

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Docker-Content-Digest", d.String())
		http.ServeContent(w, r, "blob", time.Time{}, bytes.NewReader(data))
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
				// HEAD never opens a body; full GET obtains metadata from its
				// bounded response. Preserve both superseded rejection cases as
				// proof that the unnecessary request no longer occurs.
				rejectedRequest := mode == "resume" || rejected == method

				wantStatus, wantChallenge := status, ""
				if rejectedRequest {
					wantStatus, wantChallenge = http.StatusUnauthorized, challenge
				}

				if resp.StatusCode != wantStatus || resp.Header.Get("WWW-Authenticate") != wantChallenge {
					t.Fatalf("rejected credential: status=%d challenge=%q; want 401 with remembered repository challenge", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
				}

				resp.Body.Close()

				wantHeads, wantGets, wantProbes := int32(0), int32(0), int32(0)
				if mode != "GET" {
					wantHeads = 1
				}

				if mode == "GET" || (mode == "resume" && rejected == http.MethodGet) {
					wantGets = 1
				}

				if rejectedRequest {
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

				if mode != "GET" {
					wantHeads++
				}

				if mode != "HEAD" {
					wantGets++
				}

				if calls.Load() != wantHeads+wantGets || heads.Load() != wantHeads || gets.Load() != wantGets || trap.probes.Load() != wantProbes {
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

func TestRacerHEADDuringBulkStream(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 256*1024)
	d := racerDigest(data)
	metadata := racerMetadata(t, d, len(data))
	body := &racerBlockingBody{prefix: bytes.NewReader(data[:64*1024]), blocked: make(chan struct{}), closed: make(chan struct{})}

	var heads, gets atomic.Int32

	client := racerFakeClient(t, func(_ context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if req.Operation() == racersdk.OperationHead {
			heads.Add(1)
			return metadata, nil, nil
		}

		gets.Add(1)

		return metadata, body, nil
	})
	server := racerServer(t, racerConfig(), client, &racerLegacyTrap{})

	bulk := racerRequest(t, server, http.MethodGet, "blobs", d, "", "")
	defer bulk.Body.Close()

	select {
	case <-body.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("bulk body never blocked")
	}

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
		if req.Operation() == racersdk.OperationHead {
			heads.Add(1)
		} else {
			gets.Add(1)
		}

		metadata, body, err := origin(ctx, req)
		metadata.ContentType = "application/vnd.oci.image.index.v1+json"

		return metadata, body, err
	})

	request, err := gantryracer.Request(ifaces.OriginRef{Registry: "registry.example", Repository: "library/image", Digest: d, Kind: ifaces.KindBlob}, "")
	if err != nil {
		t.Fatal(err)
	}
	// Hold actual SDK Values rather than a mock semaphore: all default bulk
	// admission slots remain occupied until the returned bodies are consumed.
	for range 64 {
		value, err := client.Get(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		defer value.Close()
	}

	if stats := client.Stats(); stats.ActiveBulk != 64 {
		t.Fatalf("bulk pool is not saturated: %+v", stats)
	}

	server := racerServer(t, racerConfig(), client, &racerLegacyTrap{})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	queued, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v2/library/image/blobs/"+d.String(), nil)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() {
		resp, err := server.Client().Do(queued)
		if err == nil {
			resp.Body.Close()
		}

		done <- err
	}()

	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(time.Second)
	for client.Stats().QueueDepth == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	if client.Stats().QueueDepth != 1 {
		t.Fatal("extra blob request did not queue behind saturated bulk pool")
	}

	manifestCtx, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()

	req, err := http.NewRequestWithContext(manifestCtx, http.MethodGet, server.URL+"/v2/library/image/manifests/"+d.String(), nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("manifest waited for bulk capacity: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(body, data) || resp.Header.Get("Content-Type") != "application/vnd.oci.image.index.v1+json" {
		t.Fatalf("isolated manifest: status=%d bytes=%d err=%v", resp.StatusCode, len(body), err)
	}

	if heads.Load() != 0 || gets.Load() != 65 || client.Stats().ActiveBulk != 64 {
		t.Fatalf("manifest used HEAD or bulk capacity: heads=%d gets=%d stats=%+v", heads.Load(), gets.Load(), client.Stats())
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

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}

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
	observed := make(chan mirror.RacerHTTPObservation, 1)
	server := httptest.NewServer(mirror.RacerHTTPHandler(
		mirror.New(racerConfig(), &racerLegacyTrap{}, &racerLegacyTrap{}, mirror.WithRacer(client)).Handler(),
		100*time.Millisecond, func(observation mirror.RacerHTTPObservation) { observed <- observation }))
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

	select {
	case result := <-observed:
		if !result.Aborted || result.Bytes == 0 || result.Bytes >= int64(len(data)) || result.Duration < 100*time.Millisecond || result.Duration > 5*time.Second {
			t.Fatalf("stalled downstream observation: %+v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stalled downstream did not release the handler")
	}

	if stats := client.Stats(); stats.ActiveBulk != 0 {
		t.Fatalf("stalled downstream retained SDK bulk capacity: %+v", stats)
	}
	// The aborted bulk stream must not prevent a new metadata request.
	request, err := gantryracer.Request(ifaces.OriginRef{Registry: "registry.example", Repository: "library/image", Digest: d, Kind: ifaces.KindBlob}, "")
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
				if req.Operation() == racersdk.OperationHead {
					return metadata, nil, nil
				}

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
