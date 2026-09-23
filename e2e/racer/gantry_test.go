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
	"sync"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/pkg/namespaces"
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

	gantryAwait(t, "verified splice", func() bool { return f.metric(0, `gantry_racer_stream_total{outcome="verified"}`) == 1 })

	if f.metric(0, "gantry_racer_splice_calls_total") == 0 || f.metric(0, "gantry_racer_tee_calls_total") == 0 || f.metric(0, "gantry_racer_splice_bytes_total") < float64(len(data)-8192) || f.metric(0, "gantry_racer_tee_bytes_total") < float64(len(data)-8192) {
		t.Fatal("real splice/tee counters did not cover the payload")
	}

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

	t.Logf("verified %d bytes, four exact origin pages, owners=%v, offline warm reuse, boundary/final ranges, real splice+tee", len(data), owners)
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

	gantryAwait(t, "verified after restart", func() bool { return f.metric(0, `gantry_racer_stream_total{outcome="verified"}`) > 0 })
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
	f := newGantryFixture(t, 1, map[string]gantryObject{path: {data: data, mediaType: "application/octet-stream", corrupt: true}})

	resp, body, err := f.request(0, "GET", path, "", "")
	if err == nil || resp == nil || resp.StatusCode != 200 || len(body) >= len(data) {
		t.Fatalf("corrupt full response was not truncated: bytes=%d err=%v", len(body), err)
	}

	gantryAwait(t, "digest mismatch metric", func() bool { return f.metric(0, `gantry_racer_stream_total{outcome="digest_mismatch"}`) == 1 })

	if f.metric(0, `gantry_racer_stream_total{outcome="verified"}`) != 0 {
		t.Fatal("corrupt response counted verified")
	}

	if f.metric(0, "gantry_racer_tee_calls_total") == 0 {
		t.Fatal("corruption did not use kernel tee verification")
	}
	// The actual containerd content store must reject the quarantined fallback.
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

	commitErr := content.WriteBlob(ctx, f.containerd.ContentStore(), "corrupt-downstream", resp.Body, desc)
	resp.Body.Close()

	if commitErr == nil {
		t.Fatal("containerd accepted corrupt content")
	}

	if _, err := f.containerd.ContentStore().Info(ctx, desc.Digest); err == nil {
		t.Fatal("corrupt digest exists in containerd")
	}

	resp, body, err = f.request(0, "GET", path, "", "")
	if err == nil || resp == nil || len(body) >= len(data) {
		t.Fatalf("corrupt quarantine fallback was accepted: bytes=%d err=%v", len(body), err)
	}

	gantryAwait(t, "quarantine fallback", func() bool { return f.metric(0, "gantry_racer_fallback_total") == 2 })
	t.Log("Racer full-hash mismatch and quarantined ordinary fallback both truncate before complete framing")
}
