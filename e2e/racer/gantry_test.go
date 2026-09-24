//go:build e2e && linux

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	ocidigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	sdk "github.com/Azure/unbounded/pkg/racer"
)

func TestGantryRacerStriped(t *testing.T) {
	if gantryNamespace(t) {
		return
	}

	data := make([]byte, 3*sdk.PageSize+123)
	for i := range data {
		data[i] = byte(i*31 + i/65537)
	}

	path := gantryPath("public", "blobs", data)
	f := newGantryFixture(t, 3, map[string]gantryObject{path: {data: data, mediaType: "application/octet-stream"}})

	resp, body, err := f.request(0, "HEAD", path, "", "")
	if err != nil || resp.StatusCode != 200 || resp.ContentLength != int64(len(data)) || len(body) != 0 {
		t.Fatalf("HEAD: %v %v", resp, err)
	}

	f.mu.Lock()
	for _, hit := range f.hits {
		if hit.path == path && hit.method != "HEAD" {
			t.Errorf("HEAD fetched payload: %+v", hit)
		}
	}
	f.mu.Unlock()

	resp, body, err = f.request(0, "GET", path, "", "")
	if err != nil || resp.StatusCode != 200 || sha256.Sum256(body) != sha256.Sum256(data) {
		t.Fatalf("cold object: status=%v bytes=%d err=%v", resp, len(body), err)
	}

	gantryAwait(t, "completed splice", func() bool { return f.metric(0, `gantry_racer_stream_total{outcome="completed"}`) == 1 })

	// Each page GET can read ahead one SDK header buffer before splicing.
	// Account for every page rather than depending on socket arrival timing.
	pages := (int64(len(data)) + sdk.PageSize - 1) / sdk.PageSize
	spliced := f.metric(0, "gantry_racer_splice_bytes_total")

	buffered := f.metric(0, "gantry_racer_buffered_bytes_total")
	if f.metric(0, "gantry_racer_splice_calls_total") == 0 || spliced <= 0 || buffered > float64(pages*8192) || spliced+buffered != float64(len(data)) {
		t.Fatalf("Racer payload accounting: splice=%g buffered=%g pages=%d payload=%d", spliced, buffered, pages, len(data))
	}

	t.Logf("Racer payload accounting: splice=%g buffered=%g pages=%d payload=%d", spliced, buffered, pages, len(data))

	gantryAssertNoTee(t, f, 0)

	if f.metric(0, "gantry_racer_fallback_total") != 0 {
		t.Fatal("cold read fell back outside Racer")
	}

	f.mu.Lock()
	owners := map[int]bool{}
	ranges := map[string]int{}

	for _, hit := range f.hits {
		if hit.path == path && hit.method == "GET" {
			owners[hit.node] = true
			ranges[hit.rangeValue]++
		}
	}

	before := len(f.hits)
	f.mu.Unlock()

	if len(owners) < 2 {
		t.Fatalf("pages did not reach multiple Gantry origins: %v", owners)
	}

	for i := int64(0); i < 4; i++ {
		value := fmt.Sprintf("bytes=%d-%d", i*sdk.PageSize, min((i+1)*sdk.PageSize, int64(len(data)))-1)
		if ranges[value] != 1 {
			t.Fatalf("page %s fetched %d times; all=%v", value, ranges[value], ranges)
		}
	}

	if len(ranges) != 4 {
		t.Fatalf("unexpected origin GETs: %v", ranges)
	}

	var peerPages, offloadedBytes float64
	for _, address := range f.racerMetrics {
		peerPages += f.metricAt(address, `racer_dataplane_upstream_requests_total{destination="peer",transport="http",kind="page"}`)
		offloadedBytes += f.metricAt(address, "racer_dataplane_tls_sendfile_bytes_total")
	}

	if peerPages < 1 {
		t.Fatal("striped placement did not transfer a peer page")
	}

	if os.Getenv("RACER_REQUIRE_KTLS") == "1" && offloadedBytes < float64(sdk.PageSize) {
		t.Fatal("required real kTLS sendfile page was not observed")
	}

	t.Logf("peer page requests=%g; confirmed kTLS sendfile bytes=%g", peerPages, offloadedBytes)
	f.offline.Store(true)

	resp, body, err = f.request(0, "GET", path, "", "")
	if err != nil || resp.StatusCode != 200 || sha256.Sum256(body) != sha256.Sum256(data) {
		t.Fatalf("warm offline: bytes=%d err=%v", len(body), err)
	}

	for _, tc := range []struct{ start, end int64 }{{sdk.PageSize - 5, sdk.PageSize + 11}, {3 * sdk.PageSize, int64(len(data)) - 1}} {
		resp, body, err = f.request(0, "GET", path, "", fmt.Sprintf("bytes=%d-%d", tc.start, tc.end))
		if err != nil || resp.StatusCode != 206 || resp.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", tc.start, tc.end, len(data)) || !bytes.Equal(body, data[tc.start:tc.end+1]) {
			t.Fatalf("range %+v: %v %v", tc, resp, err)
		}
	}

	resp, body, err = f.request(0, "GET", path, "", "bytes=-123")
	if err != nil || resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, data[len(data)-123:]) {
		t.Fatalf("suffix range: %v %v", resp, err)
	}

	resp, _, err = f.request(0, "GET", path, "", fmt.Sprintf("bytes=%d-", len(data)))
	if err != nil || resp.StatusCode != http.StatusRequestedRangeNotSatisfiable || resp.Header.Get("Content-Range") != fmt.Sprintf("bytes */%d", len(data)) {
		t.Fatalf("unsatisfiable range: %v %v", resp, err)
	}
	// Use containerd's actual content API as the downstream commit verifier.
	ctx := namespaces.WithNamespace(t.Context(), "node0")
	desc := ocispec.Descriptor{Digest: ocidigest.FromBytes(data), Size: int64(len(data))}

	r, err := http.NewRequestWithContext(ctx, "GET", "http://"+f.mirrors[0]+path+"?ns=fixture.test", nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err = f.client.Do(r)
	if err != nil {
		t.Fatal(err)
	}

	err = content.WriteBlob(ctx, f.containerd.ContentStore(), "striped-downstream", resp.Body, desc)
	resp.Body.Close()

	if err != nil {
		t.Fatal(err)
	}

	stored, err := content.ReadBlob(ctx, f.containerd.ContentStore(), desc)
	if err != nil || sha256.Sum256(stored) != sha256.Sum256(data) {
		t.Fatalf("containerd committed content: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.hits) != before {
		t.Fatalf("warm reads contacted offline origins: before=%d after=%d", before, len(f.hits))
	}

	t.Logf("verified %d downstream bytes, four exact origin pages, owners=%v, offline warm reuse, boundary/final ranges, real splice without tee", len(data), owners)
}

func TestGantryRacerAuthorization(t *testing.T) {
	if gantryNamespace(t) {
		return
	}

	data := bytes.Repeat([]byte("private-payload"), 4096)
	path := gantryPath("private", "blobs", data)
	pagePath := gantryPath("private/page", "blobs", data)
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","size":2,"digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"},"layers":[]}`)
	manifestPath := gantryPath("private", "manifests", manifest)
	mediaType := "application/vnd.oci.image.manifest.v1+json"
	f := newGantryFixture(t, 2, map[string]gantryObject{path: {data: data, mediaType: "application/octet-stream"}, pagePath: {data: data, mediaType: "application/octet-stream"}, manifestPath: {data: manifest, mediaType: mediaType}})
	f.privateRegistry.Store(true)

	challenge, _, err := f.request(0, "HEAD", path, "", "")
	if err != nil || challenge.StatusCode != http.StatusUnauthorized || challenge.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("initial registry challenge: %v %v", challenge, err)
	}

	var wg sync.WaitGroup
	for _, tc := range []struct {
		auth   string
		status int
	}{{"Bearer unauthorized", 401}, {"Bearer denied", 403}} {
		wg.Go(func() {
			resp, _, err := f.request(0, "HEAD", path, tc.auth, "")
			if err != nil || resp.StatusCode != tc.status || resp.Header.Get("WWW-Authenticate") == "" {
				t.Errorf("concurrent HEAD: status=%v err=%v", resp, err)
			}
		})
	}

	wg.Wait()

	resp, _, err := f.request(0, "GET", pagePath, "Bearer page-denied", "")
	if err != nil || resp.StatusCode != 403 || resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("page authorization: %v %v", resp, err)
	}

	for _, auth := range []string{"Bearer caller-a", "Bearer caller-b"} {
		wg.Go(func() {
			resp, body, err := f.request(0, "GET", manifestPath, auth, "")
			if err != nil || resp.StatusCode != 200 || resp.Header.Get("Content-Type") != mediaType || !bytes.Equal(body, manifest) {
				t.Errorf("manifest: %v %v", resp, err)
			}
		})
	}

	wg.Wait()

	if f.metric(0, "gantry_racer_fallback_total") != 0 {
		t.Fatal("authorization or manifest reads bypassed Racer")
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	seen := map[string]bool{}

	for _, hit := range f.hits {
		if hit.path == manifestPath {
			seen[hit.auth] = true
		}

		if hit.path == pagePath && hit.method == "GET" && hit.auth != "Bearer page-denied" {
			t.Errorf("page credential changed: %+v", hit)
		}
	}

	if !seen["Bearer caller-a"] || !seen["Bearer caller-b"] {
		t.Fatalf("concurrent credentials were not independently authorized: %v", seen)
	}
}

func TestGantryRacerRecovery(t *testing.T) {
	if gantryNamespace(t) {
		return
	}

	data := bytes.Repeat([]byte("recovery"), 1<<20)
	path := gantryPath("public", "blobs", data)
	unsupported := gantryPath("unsupported", "blobs", data)
	f := newGantryFixture(t, 1, map[string]gantryObject{path: {data: data, mediaType: "application/octet-stream"}, unsupported: {data: data, mediaType: "application/octet-stream", noRange: true}})

	resp, body, err := f.request(0, "GET", unsupported, "", "")
	if err != nil || resp.StatusCode != 200 || !bytes.Equal(body, data) {
		t.Fatalf("unsupported range fallback: %v %v", resp, err)
	}

	if f.metric(0, "gantry_racer_fallback_total") != 1 {
		t.Fatal("unsupported range did not take fallback")
	}

	f.racers[0].stop()

	resp, body, err = f.request(0, "GET", path, "", "")
	if err != nil || resp.StatusCode != 200 || !bytes.Equal(body, data) {
		t.Fatalf("stopped daemon fallback: %v %v", resp, err)
	}

	if f.metric(0, "gantry_racer_fallback_total") != 2 {
		t.Fatal("stopped daemon did not take fallback")
	}

	f.racers[0] = gantryStart(t, f.dir, "racer-restarted", f.racerEnvs[0], os.Getenv("RACER_DATAPLANE_BINARY"))
	gantryAwait(t, "restarted cache", func() bool {
		r, e := f.client.Get("http://" + f.racerMetrics[0] + "/readyz")
		if e != nil {
			return false
		}

		r.Body.Close()

		return r.StatusCode == 200
	})

	resp, body, err = f.request(0, "GET", path, "", "")
	if err != nil || resp.StatusCode != 200 || !bytes.Equal(body, data) {
		t.Fatalf("restarted daemon: %v %v", resp, err)
	}

	gantryAwait(t, "completed after restart", func() bool { return f.metric(0, `gantry_racer_stream_total{outcome="completed"}`) > 0 })
	// Abandon a warm response while Gantry is forwarding to a real TCP socket.
	r, err := http.NewRequestWithContext(t.Context(), "GET", "http://"+f.mirrors[0]+path+"?ns=fixture.test", nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err = f.client.Do(r)
	if err != nil {
		t.Fatal(err)
	}

	_, err = io.CopyN(io.Discard, resp.Body, 1024)
	if err != nil {
		t.Fatal(err)
	}

	resp.Body.Close()
	gantryAwait(t, "canceled stream reclaimed", func() bool { return f.metric(0, `gantry_racer_stream_total{outcome="aborted"}`) > 0 })
	f.agents[0].stop()
	f.agents[0] = gantryStart(t, f.dir, "gantry-restarted", []string{"SSL_CERT_FILE=" + filepath.Join(f.dir, "registry-0.pem")}, os.Getenv("GANTRY_BINARY"), "agent", "--config", f.agentConfigs[0])
	gantryAwait(t, "restarted Gantry", func() bool {
		r, e := f.client.Get("http://" + f.metrics[0] + "/readyz")
		if e != nil {
			return false
		}

		r.Body.Close()

		return r.StatusCode == 200
	})
	f.offline.Store(true)

	resp, body, err = f.request(0, "GET", path, "Bearer caller-a", "")
	if err != nil || resp.StatusCode != 200 || !bytes.Equal(body, data) {
		t.Fatalf("Gantry restart warm cache: %v %v", resp, err)
	}

	if f.metric(0, "gantry_racer_fallback_total") != 0 {
		t.Fatal("warm read after Gantry restart fell back")
	}
}

func TestGantryRacerCorruption(t *testing.T) {
	if gantryNamespace(t) {
		return
	}

	data := bytes.Repeat([]byte("integrity"), 1<<17)
	path := gantryPath("public", "blobs", data)
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayer, Digest: ocidigest.FromBytes(data), Size: int64(len(data))}
	config := fmt.Appendf(nil, `{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[%q]}}`, desc.Digest)
	manifest := fmt.Appendf(nil, `{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":%q,"size":%d,"digest":%q},"layers":[{"mediaType":%q,"size":%d,"digest":%q}]}`, ocispec.MediaTypeImageManifest, ocispec.MediaTypeImageConfig, len(config), ocidigest.FromBytes(config), desc.MediaType, desc.Size, desc.Digest)
	object := gantryObject{data: data, mediaType: desc.MediaType, corrupt: true}
	f := newGantryFixture(t, 2, map[string]gantryObject{
		path:                                  object,
		gantryPath("public", "blobs", config): {data: config, mediaType: ocispec.MediaTypeImageConfig},
		gantryPath("public", "manifests", manifest): {data: manifest, mediaType: ocispec.MediaTypeImageManifest},
	})
	bad := bytes.Clone(data)
	bad[len(bad)/2] ^= 1
	badDigest := ocidigest.FromBytes(bad)
	read := func(node int, want []byte) {
		t.Helper()

		completed := f.metric(node, `gantry_racer_stream_total{outcome="completed"}`)

		resp, body, err := f.request(node, "GET", path, "", "")
		if err != nil || resp.StatusCode != http.StatusOK || resp.ContentLength != desc.Size || resp.Header.Get("Docker-Content-Digest") != desc.Digest.String() || !bytes.Equal(body, want) {
			t.Fatalf("node%d complete HTTP forward: status=%v bytes=%d err=%v", node, resp, len(body), err)
		}

		gantryAwait(t, "Racer forward completed", func() bool { return f.metric(node, `gantry_racer_stream_total{outcome="completed"}`) == completed+1 })
	}
	// Corruption is at the origin, before Racer computes its admission CRC.
	// Reading through both nodes exercises peer admission of those same bad bytes.
	for node := range 2 {
		read(node, bad)
		gantryAssertNoTee(t, f, node)
	}

	var peerPages float64

	gantryAwait(t, "corrupt peer page metrics", func() bool {
		peerPages = 0
		for _, address := range f.racerMetrics {
			peerPages += f.metricAt(address, `racer_dataplane_upstream_requests_total{destination="peer",transport="http",kind="page"}`)
		}

		return peerPages > 0
	})

	// Keep the consumer namespace separate from Gantry's local content stores,
	// so later warm HTTP reads must still use Racer after the successful commit.
	ctx, release, err := f.containerd.WithLease(namespaces.WithNamespace(t.Context(), "corruption-pull"))
	if err != nil {
		t.Fatal(err)
	}
	defer release(ctx)

	var layerGETs atomic.Int64

	pullClient := &http.Client{Timeout: f.client.Timeout, Transport: gantryRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && r.URL.Path == path {
			layerGETs.Add(1)
		}

		return http.DefaultTransport.RoundTrip(r)
	})}
	resolver := docker.NewResolver(docker.ResolverOptions{Hosts: func(host string) ([]docker.RegistryHost, error) {
		if host != "fixture.test" {
			return nil, fmt.Errorf("unexpected registry %q", host)
		}

		return []docker.RegistryHost{{Client: pullClient, Host: f.mirrors[0], Scheme: "http", Path: "/v2", Capabilities: docker.HostCapabilityPull | docker.HostCapabilityResolve}}, nil
	}})
	imageRef := "fixture.test/public@" + ocidigest.FromBytes(manifest).String()
	store := f.containerd.ContentStore()
	// Fetch the valid config through the real resolver first. A failed layer
	// cancels sibling config requests during Pull, which can otherwise count an
	// unrelated pre-header fallback and obscure this layer-integrity campaign.
	fetcher, err := resolver.Fetcher(ctx, imageRef)
	if err != nil {
		t.Fatal(err)
	}

	if err := remotes.Fetch(ctx, store, fetcher, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: ocidigest.FromBytes(config), Size: int64(len(config))}); err != nil {
		t.Fatal(err)
	}

	pull := func() error {
		_, err := f.containerd.Pull(ctx, imageRef, containerd.WithResolver(resolver))
		return err
	}
	ingestRef := remotes.MakeRefKey(ctx, desc)
	assertRejected := func(stage string, err error) {
		t.Helper()

		want := fmt.Sprintf("unexpected commit digest %s, expected %s", badDigest, desc.Digest)
		if !errdefs.IsFailedPrecondition(err) || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: expected OCI digest mismatch, got %v", stage, err)
		}

		for _, digest := range []ocidigest.Digest{desc.Digest, badDigest} {
			if _, err := store.Info(ctx, digest); !errdefs.IsNotFound(err) {
				t.Fatalf("%s: content %s should be absent: %v", stage, digest, err)
			}
		}

		if _, err := f.containerd.GetImage(ctx, imageRef); !errdefs.IsNotFound(err) {
			t.Fatalf("%s: failed pull published an image: %v", stage, err)
		}

		status, err := store.Status(ctx, ingestRef)
		if err != nil || status.Ref != ingestRef || status.Offset != desc.Size || status.Total != desc.Size {
			t.Fatalf("%s: retained ingest: %+v err=%v", stage, status, err)
		}

		t.Logf("%s: digest mismatch; retained ref=%s offset=%d total=%d layer GETs=%d", stage, ingestRef, status.Offset, status.Total, layerGETs.Load())
	}
	assertRejected("initial normal pull", pull())

	if layerGETs.Load() != 1 {
		t.Fatalf("initial normal pull layer GETs=%d, want 1", layerGETs.Load())
	}

	before := f.originRequestCount("GET", path)

	object.corrupt = false
	if err := f.setOriginObject(path, object); err != nil {
		t.Fatal(err)
	}

	for node := range 2 {
		read(node, bad)
	}

	if f.originRequestCount("GET", path) != before {
		t.Fatal("origin repair alone refetched a warmed bad page")
	}

	revision := f.bumpCacheGeneration("gantry")
	// A normal retry can recommit its complete failed ingest without any GET,
	// even after all dataplanes activate the operator's generation bump.
	assertRejected("normal retry after generation activation", pull())

	if layerGETs.Load() != 1 || f.originRequestCount("GET", path) != before {
		t.Fatal("complete failed ingest retry unexpectedly fetched the repaired layer")
	}

	// Both synchronous pulls have returned. Acquire the exact failed writer to
	// prove it is idle and still complete, then close before abort. The commit
	// error above identifies its bad digest; the remote Writer.Digest may be empty.
	writer, err := store.Writer(ctx, content.WithRef(ingestRef), content.WithDescriptor(desc))
	if err != nil {
		t.Fatal(err)
	}

	retained, statusErr := writer.Status()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	if statusErr != nil || retained.Ref != ingestRef || retained.Offset != desc.Size {
		t.Fatalf("retained writer status=%+v err=%v", retained, statusErr)
	}

	if err := store.Abort(ctx, ingestRef); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Status(ctx, ingestRef); !errdefs.IsNotFound(err) {
		t.Fatalf("aborted ingest still present: %v", err)
	}
	// Reuse the identical image and ingest references in the same namespace.
	if err := pull(); err != nil {
		t.Fatalf("normal same-ref pull after explicit abort: %v", err)
	}

	if layerGETs.Load() != 2 || f.originRequestCount("GET", path) <= before {
		t.Fatal("recovery did not freshly fetch the repaired layer")
	}

	stored, err := content.ReadBlob(ctx, store, desc)
	if err != nil || !bytes.Equal(stored, data) || ocidigest.FromBytes(stored) != desc.Digest {
		t.Fatalf("recovered containerd content: bytes=%d err=%v", len(stored), err)
	}

	if _, err := store.Status(ctx, ingestRef); !errdefs.IsNotFound(err) {
		t.Fatalf("successful commit retained an ingest: %v", err)
	}

	before = f.originRequestCount("GET", path)
	f.offline.Store(true)

	for node := range 2 {
		read(node, data)
		gantryAssertNoTee(t, f, node)

		if f.metric(node, "gantry_racer_fallback_total") != 0 || f.metric(node, "gantry_racer_splice_calls_total") == 0 || f.metric(node, "gantry_racer_splice_bytes_total") < float64(len(data)-8192) {
			t.Fatalf("node%d Racer recovery: fallback=%g splice calls=%g bytes=%g", node, f.metric(node, "gantry_racer_fallback_total"), f.metric(node, "gantry_racer_splice_calls_total"), f.metric(node, "gantry_racer_splice_bytes_total"))
		}
	}

	if f.originRequestCount("GET", path) != before {
		t.Fatal("recovered warm reads contacted offline origins")
	}

	t.Logf("revision=%d; peer pages=%g; full corrupt HTTP forwarding, downstream digest rejection, explicit idle-ingest abort, same-ref commit, and offline warm reads verified", revision, peerPages)

	t.Run("RegistryFallback", func(t *testing.T) {
		gantryAssertFallbackCorruption(t, f, resolver)
	})
}

func gantryAssertFallbackCorruption(t *testing.T, f *gantryFixture, resolver remotes.Resolver) {
	t.Helper()

	// Keep the primary Racer recovery campaign intact. A distinct image prevents
	// its successful commit or retained ingest from satisfying this pull locally.
	data := bytes.Repeat([]byte("fallback-integrity"), 1<<16)
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayer, Digest: ocidigest.FromBytes(data), Size: int64(len(data))}
	config := fmt.Appendf(nil, `{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[%q]}}`, desc.Digest)
	manifest := fmt.Appendf(nil, `{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":%q,"size":%d,"digest":%q},"layers":[{"mediaType":%q,"size":%d,"digest":%q}]}`, ocispec.MediaTypeImageManifest, ocispec.MediaTypeImageConfig, len(config), ocidigest.FromBytes(config), desc.MediaType, desc.Size, desc.Digest)
	path := gantryPath("public", "blobs", data)
	objects := map[string]gantryObject{
		path:                                  {data: data, mediaType: desc.MediaType, corrupt: true},
		gantryPath("public", "blobs", config): {data: config, mediaType: ocispec.MediaTypeImageConfig},
		gantryPath("public", "manifests", manifest): {data: manifest, mediaType: ocispec.MediaTypeImageManifest},
	}

	f.mu.Lock()
	for path, object := range objects {
		f.objects[path] = object
	}
	f.mu.Unlock()

	f.offline.Store(false)
	// A real daemon outage forces pre-header fallback in the production Racer
	// mirror, without swapping its constructor or sending the pull to origin.
	f.racers[0].stop()
	streams := f.metric(0, `gantry_racer_stream_total{outcome="completed"}`)
	fallbacks := f.metric(0, "gantry_racer_fallback_total")
	completed := f.metric(0, `gantry_origin_stream_completed_total{kind="layer"}`)
	failed := f.metric(0, `gantry_origin_stream_failed_total{kind="layer"}`)
	bad := bytes.Clone(data)
	bad[len(bad)/2] ^= 1
	badDigest := ocidigest.FromBytes(bad)

	// Even a range request completes as a full, chunk-terminated 200 with the
	// registry's MIME type and delegated credentials, despite the wrong digest.
	resp, body, err := f.request(0, http.MethodGet, path, "Bearer fallback-caller", "bytes=1-2")
	if err != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(body, bad) || int64(len(body)) != desc.Size || ocidigest.FromBytes(body) == desc.Digest {
		t.Fatalf("complete corrupt fallback: status=%v bytes=%d err=%v", resp, len(body), err)
	}

	if resp.ContentLength != -1 || len(resp.TransferEncoding) != 1 || resp.TransferEncoding[0] != "chunked" || resp.Header.Get("Content-Range") != "" || resp.Header.Get("Content-Type") != desc.MediaType || resp.Header.Get("Docker-Content-Digest") != desc.Digest.String() {
		t.Fatalf("fallback framing/metadata: %+v", resp)
	}

	gantryAwait(t, "corrupt fallback HTTP completion", func() bool {
		return f.metric(0, "gantry_racer_fallback_total") == fallbacks+1 && f.metric(0, `gantry_origin_stream_completed_total{kind="layer"}`) == completed+1
	})

	ctx, release, err := f.containerd.WithLease(namespaces.WithNamespace(t.Context(), "fallback-corruption-pull"))
	if err != nil {
		t.Fatal(err)
	}
	defer release(ctx)

	imageRef := "fixture.test/public@" + ocidigest.FromBytes(manifest).String()
	store := f.containerd.ContentStore()

	fetcher, err := resolver.Fetcher(ctx, imageRef)
	if err != nil {
		t.Fatal(err)
	}
	// Avoid sibling config cancellation obscuring the corrupt layer outcome.
	if err := remotes.Fetch(ctx, store, fetcher, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: ocidigest.FromBytes(config), Size: int64(len(config))}); err != nil {
		t.Fatal(err)
	}

	gantryAwait(t, "fallback config completion", func() bool {
		return f.metric(0, `gantry_origin_stream_completed_total{kind="layer"}`) == completed+2
	})

	_, err = f.containerd.Pull(ctx, imageRef, containerd.WithResolver(resolver))

	want := fmt.Sprintf("unexpected commit digest %s, expected %s", badDigest, desc.Digest)
	if !errdefs.IsFailedPrecondition(err) || !strings.Contains(err.Error(), want) {
		t.Fatalf("fallback normal pull: expected OCI digest commit rejection, got %v", err)
	}

	for _, digest := range []ocidigest.Digest{desc.Digest, badDigest} {
		if _, err := store.Info(ctx, digest); !errdefs.IsNotFound(err) {
			t.Fatalf("fallback content %s should be absent: %v", digest, err)
		}
	}

	if _, err := f.containerd.GetImage(ctx, imageRef); !errdefs.IsNotFound(err) {
		t.Fatalf("fallback failed pull published an image: %v", err)
	}

	ingestRef := remotes.MakeRefKey(ctx, desc)

	status, err := store.Status(ctx, ingestRef)
	if err != nil || status.Ref != ingestRef || status.Offset != desc.Size || status.Total != desc.Size {
		t.Fatalf("fallback did not deliver a complete layer to containerd: %+v err=%v", status, err)
	}

	gantryAwait(t, "rejected layer forwarding completion", func() bool {
		return f.metric(0, `gantry_origin_stream_completed_total{kind="layer"}`) == completed+3
	})

	if f.metric(0, `gantry_origin_stream_failed_total{kind="layer"}`) != failed || f.metric(0, `gantry_racer_stream_total{outcome="completed"}`) != streams {
		t.Fatal("digest rejection was attributed to a forwarding failure or a Racer stream")
	}

	gantryAssertNoTee(t, f, 0)
	f.mu.Lock()
	defer f.mu.Unlock()

	var layerGETs int

	for _, hit := range f.hits {
		if hit.path != path || hit.method != http.MethodGet {
			continue
		}

		layerGETs++
		if hit.node != 0 || hit.rangeValue != "" || layerGETs == 1 && hit.auth != "Bearer fallback-caller" {
			t.Errorf("fallback registry request lost routing/auth or forwarded Range: %+v", hit)
		}
	}

	if layerGETs != 2 {
		t.Fatalf("fallback layer GETs=%d, want one HTTP probe and one containerd fetch", layerGETs)
	}

	t.Logf("ordinary registry fallback: complete corrupt HTTP response; containerd FailedPrecondition, retained offset=%d total=%d, neither digest nor image committed", status.Offset, status.Total)
}

type gantryRoundTripper func(*http.Request) (*http.Response, error)

func (fn gantryRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func gantryAssertNoTee(t *testing.T, f *gantryFixture, node int) {
	t.Helper()

	for _, metric := range []string{"gantry_racer_tee_calls_total", "gantry_racer_tee_bytes_total", `gantry_racer_stream_total{outcome="verified"}`, `gantry_racer_stream_total{outcome="digest_mismatch"}`} {
		if got := f.metric(node, metric); got != 0 {
			t.Fatalf("node%d %s=%g; Racer forwarding must not claim SHA verification", node, metric, got)
		}
	}
}
