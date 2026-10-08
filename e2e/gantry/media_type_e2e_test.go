//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	godigest "github.com/opencontainers/go-digest"

	"github.com/Azure/unbounded/internal/gantry/transfer"
)

const (
	ociIndexMediaType           = "application/vnd.oci.image.index.v1+json"
	dockerManifestListMediaType = "application/vnd.docker.distribution.manifest.list.v2+json"
	maxE2EManifestBytes         = 5 * 1024 * 1024
)

type resolvedImageIndex struct {
	digest    godigest.Digest
	mediaType string
	body      []byte
}

// TestE2E_MultiArchIndexDigestPeerWithoutMediaTypeCache reproduces the
// containerd 2.1 "expected manifest but found index" failure against real
// containerd stores:
//
//  1. Purge the source image before Gantry starts so no descriptor walk can
//     populate its in-memory media-type cache.
//  2. Use `ctr content fetch` on one node to store a real multi-platform index
//     and its node-platform graph, protect the graph from GC, then remove the
//     image record and restart the seed's Gantry pod. Gantry advertises the
//     remaining bare content through periodic inventory, while the restarted
//     process has no image descriptor from which to rebuild its media-type
//     cache.
//  3. Verify the peer transfer endpoint labels the index bytes as an index.
//  4. Pull the same digest on another node under a deliberately nonexistent
//     repository. Origin fallback can only return 404, so the workload can
//     become Ready only if Gantry serves the index, manifest, config, and
//     layers from the peer with media types containerd accepts.
func TestE2E_MultiArchIndexDigestPeerWithoutMediaTypeCache(t *testing.T) {
	h := newHarness(t)
	h.checkPrereqs()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	index := h.resolveE2EImageIndex(ctx)
	nodePlatform := "linux/" + runtime.GOARCH
	missingRepository := e2eRegistry + "/gantry-e2e/multiarch-cache-miss-" + index.digest.Encoded()[:12]
	peerOnlyRef := missingRepository + "@" + index.digest.String()

	h.bootCluster(ctx)
	t.Cleanup(func() {
		tdCtx, tdCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer tdCancel()

		h.teardown(tdCtx)
	})

	// Remove both the real source name and this test's peer-only name before
	// Gantry starts. This also makes E2E_KEEP cluster reuse deterministic.
	h.removePullImageFromNodes(ctx)

	for _, node := range h.kindNodes(ctx) {
		h.evictImageFromNode(ctx, node, peerOnlyRef)
	}

	h.buildAndLoadImage(ctx)
	h.applyManifests(ctx)
	h.waitForRollout(ctx)

	// A reused cluster may already have Gantry pods whose descriptor cache saw
	// an earlier image record. Restart after eviction, then seed only bare
	// content so the cache-miss precondition is deterministic.
	if err := h.run(ctx, "kubectl", "-n", namespace, "rollout", "restart", "daemonset/"+dsName); err != nil {
		t.Fatalf("restart Gantry after image eviction: %v", err)
	}

	h.waitForRollout(ctx)
	h.checkReadyz(ctx)

	workers := h.workerNodes(ctx)
	seedNode := workers[0]
	requesterNode := workers[1]
	seedPod := h.gantryPodOnNode(ctx, seedNode)

	version, err := h.runOut(ctx, h.containerEngine, "exec", seedNode, "ctr", "version")
	if err != nil {
		t.Fatalf("read containerd version on %s: %v", seedNode, err)
	}

	if !strings.Contains(version, "v2.1.") {
		t.Fatalf("containerd version on %s is not v2.1.x:\n%s", seedNode, version)
	}

	h.seedBareImageContent(ctx, seedNode, index.digest, nodePlatform)

	// `ctr content fetch` creates an image event before seedBareImageContent
	// removes the resulting image record. Restart this one Gantry process after
	// removal so any descriptor metadata learned from that event is gone. Its
	// startup image walk cannot relearn the index because only bare content
	// remains.
	if err := h.run(ctx, "kubectl", "-n", namespace, "delete", "pod/"+seedPod,
		"--wait=true", "--timeout=60s"); err != nil {
		t.Fatalf("restart seed Gantry pod %s: %v", seedPod, err)
	}

	h.waitForRollout(ctx)
	seedPod = h.gantryPodOnNode(ctx, seedNode)
	h.verifyContainerdSocketAccess(ctx, seedPod)

	// Verify the exact transfer response before involving the requester. The
	// body comparison proves the response came from the raw containerd content
	// seeded above, and the header is what containerd's resolver trusts.
	headers := make(http.Header)
	headers.Set(transfer.MirroredHeader, "1")

	peerResponse, err := h.fetchPodHTTP(
		ctx,
		seedPod,
		5001,
		"/v2/gantry-e2e/multiarch-cache-miss/manifests/"+index.digest.String(),
		headers,
	)
	if err != nil {
		t.Fatalf("fetch index from seed transfer endpoint: %v", err)
	}

	responseMediaType, _, err := mime.ParseMediaType(peerResponse.header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse peer Content-Type %q: %v", peerResponse.header.Get("Content-Type"), err)
	}

	if responseMediaType != index.mediaType {
		t.Fatalf("peer Content-Type = %q, want %q", responseMediaType, index.mediaType)
	}

	if !bytes.Equal(peerResponse.body, index.body) {
		t.Fatalf("peer index body differs from registry index: got %d bytes, want %d", len(peerResponse.body), len(index.body))
	}

	// Wait for a complete inventory reconciliation after seeding. Bare content
	// has no image-create event, so this periodic pass is what publishes the
	// provider records used by the requester.
	reconcileBefore := h.metricSumOnPod(ctx, seedPod, "gantry_advertise_reconcile_duration_seconds_count")
	h.waitForMetricIncreaseOnPod(ctx, seedPod, "gantry_advertise_reconcile_duration_seconds_count", reconcileBefore)

	h.installMirrorHosts(ctx)

	requesterPod := h.gantryPodOnNode(ctx, requesterNode)
	peerHitsBefore := h.metricSumOnPod(ctx, requesterPod, "p2p_peer_fetch_total", `outcome="hit"`)

	h.deletePod(ctx, "gantry-e2e-multiarch-cache-miss")
	h.applyPullPodWithImage(ctx, "gantry-e2e-multiarch-cache-miss", requesterNode, peerOnlyRef)
	h.waitForPodReadyTimeout(ctx, "gantry-e2e-multiarch-cache-miss", "600s")
	h.waitForMetricIncreaseOnPod(ctx, requesterPod, "p2p_peer_fetch_total", peerHitsBefore, `outcome="hit"`)
}

func (h *harness) resolveE2EImageIndex(ctx context.Context) resolvedImageIndex {
	h.t.Helper()

	url := e2eRegistryServer + "/v2/e2e-test-images/agnhost/manifests/2.39"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		h.t.Fatalf("build index request: %v", err)
	}

	req.Header.Set("Accept", strings.Join([]string{
		ociIndexMediaType,
		dockerManifestListMediaType,
	}, ", "))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("resolve %s: %v", e2ePullImage, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		h.t.Fatalf("resolve %s: status %d: %s", e2ePullImage, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxE2EManifestBytes+1))
	if err != nil {
		h.t.Fatalf("read %s index: %v", e2ePullImage, err)
	}

	if len(body) > maxE2EManifestBytes {
		h.t.Fatalf("%s index is larger than %d bytes", e2ePullImage, maxE2EManifestBytes)
	}

	var envelope struct {
		MediaType string `json:"mediaType"`
		Manifests []struct {
			Platform struct {
				Architecture string `json:"architecture"`
				OS           string `json:"os"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		h.t.Fatalf("parse %s index: %v", e2ePullImage, err)
	}

	if len(envelope.Manifests) == 0 {
		h.t.Fatalf("%s did not resolve to a multi-platform index", e2ePullImage)
	}

	hasNodeArchitecture := false

	for _, manifest := range envelope.Manifests {
		if manifest.Platform.OS == "linux" && manifest.Platform.Architecture == runtime.GOARCH {
			hasNodeArchitecture = true
			break
		}
	}

	if !hasNodeArchitecture {
		h.t.Fatalf("%s index has no linux/%s manifest", e2ePullImage, runtime.GOARCH)
	}

	mediaType := envelope.MediaType
	if mediaType == "" {
		mediaType = ociIndexMediaType
	}

	if mediaType != ociIndexMediaType && mediaType != dockerManifestListMediaType {
		h.t.Fatalf("%s media type = %q, want an OCI index or Docker manifest list", e2ePullImage, mediaType)
	}

	digest := godigest.FromBytes(body)
	if headerDigest := strings.TrimSpace(resp.Header.Get("Docker-Content-Digest")); headerDigest != "" && headerDigest != digest.String() {
		h.t.Fatalf("%s Docker-Content-Digest = %q, computed %q", e2ePullImage, headerDigest, digest)
	}

	return resolvedImageIndex{
		digest:    digest,
		mediaType: mediaType,
		body:      body,
	}
}

func (h *harness) seedBareImageContent(
	ctx context.Context,
	node string,
	indexDigest godigest.Digest,
	platform string,
) {
	h.t.Helper()

	gcRootLabel := "containerd.io/gc.root=gantry-e2e-multiarch-cache-miss"
	cmd := strings.Join([]string{
		"set -eu",
		"ctr -n k8s.io content fetch --platform " + shellQuote(platform) +
			" --label " + shellQuote(gcRootLabel) + " " + shellQuote(e2ePullImage) + " >/dev/null",
		"ctr -n k8s.io content label " + shellQuote(indexDigest.String()) + " " + shellQuote(gcRootLabel) + " >/dev/null",
		"ctr -n k8s.io images rm " + shellQuote(e2ePullImage) + " >/dev/null 2>&1 || true",
		"ctr -n k8s.io content get " + shellQuote(indexDigest.String()) + " >/dev/null",
		"if ctr -n k8s.io images ls -q | grep -Fx " + shellQuote(e2ePullImage) + " >/dev/null; then " +
			"echo " + shellQuote("image metadata still exists after removing "+e2ePullImage) + " >&2; exit 1; fi",
	}, "; ")

	if err := h.run(ctx, h.containerEngine, "exec", node, "sh", "-c", cmd); err != nil {
		h.t.Fatalf("seed bare image content on %s: %v", node, err)
	}
}
