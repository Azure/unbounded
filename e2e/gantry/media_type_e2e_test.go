//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"mime"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/unbounded/e2e/gantry/fixture"
	"github.com/Azure/unbounded/internal/gantry/transfer"
)

// TestE2E_MultiArchIndexDigestPeerWithoutMediaTypeCache reproduces the
// containerd 2.1 "expected manifest but found index" failure against real
// containerd stores:
//
//  1. Build a deterministic scratch-image index for amd64 and arm64 using a
//     local pause executable. Purge its graph before seeding.
//  2. Import the OCI archive on one node, protect the graph from GC, remove
//     its image record, and restart the seed's Gantry pod. Periodic inventory
//     advertises bare content without rebuilding the media-type cache.
//  3. Verify the peer transfer endpoint labels the index bytes as an index.
//  4. Pull the same digest on another node through a local HTTPS registry
//     that answers auth probes but returns 404 for all content. Readiness can
//     only come from peers serving every descriptor with correct media types.
//
// No registry.k8s.io request is involved in fixture setup or the pull.
func TestE2E_MultiArchIndexDigestPeerWithoutMediaTypeCache(t *testing.T) {
	h := newHarness(t)
	h.checkPrereqs()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	image := h.buildPauseFixture(ctx)
	index := image.Descriptor

	h.bootCluster(ctx)
	t.Cleanup(func() {
		tdCtx, tdCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer tdCancel()

		h.teardown(tdCtx)
	})

	registryHost, certPEM := h.deployPeerOnlyRegistry(ctx)
	missingRepository := registryHost + "/gantry-e2e/multiarch-cache-miss-" + index.Digest.Encoded()[:12]
	peerOnlyRef := missingRepository + "@" + index.Digest.String()
	h.purgePauseFixture(ctx, image, peerOnlyRef)

	h.buildAndLoadImage(ctx)
	h.applyManifests(ctx)
	h.configurePeerOnlyRegistry(ctx, registryHost)
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

	h.seedBareImageContent(ctx, seedNode, image)

	// `ctr images import` creates an image event before seedBareImageContent
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
		"/v2/gantry-e2e/multiarch-cache-miss/manifests/"+index.Digest.String(),
		headers,
	)
	if err != nil {
		t.Fatalf("fetch index from seed transfer endpoint: %v", err)
	}

	responseMediaType, _, err := mime.ParseMediaType(peerResponse.header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse peer Content-Type %q: %v", peerResponse.header.Get("Content-Type"), err)
	}

	if responseMediaType != index.MediaType {
		t.Fatalf("peer Content-Type = %q, want %q", responseMediaType, index.MediaType)
	}

	indexBody := image.Blobs[index.Digest]
	if !bytes.Equal(peerResponse.body, indexBody) {
		t.Fatalf("peer index body differs from fixture index: got %d bytes, want %d", len(peerResponse.body), len(indexBody))
	}

	// Wait for a complete inventory reconciliation after seeding. Bare content
	// has no image-create event, so this periodic pass is what publishes the
	// provider records used by the requester.
	reconcileBefore := h.metricSumOnPod(ctx, seedPod, "gantry_advertise_reconcile_duration_seconds_count")
	h.waitForMetricIncreaseOnPod(ctx, seedPod, "gantry_advertise_reconcile_duration_seconds_count", reconcileBefore)

	h.installPeerOnlyMirrorHosts(ctx, registryHost, certPEM)

	requesterPod := h.gantryPodOnNode(ctx, requesterNode)
	peerHitsBefore := h.metricSumOnPod(ctx, requesterPod, "p2p_peer_fetch_total", `outcome="hit"`)

	h.deletePod(ctx, "gantry-e2e-multiarch-cache-miss")
	h.applyPullPodWithImage(ctx, "gantry-e2e-multiarch-cache-miss", requesterNode, peerOnlyRef, "/pause")
	h.waitForPodReadyTimeout(ctx, "gantry-e2e-multiarch-cache-miss", "600s")
	h.waitForMetricIncreaseOnPod(ctx, requesterPod, "p2p_peer_fetch_total", peerHitsBefore, `outcome="hit"`)
}

func (h *harness) seedBareImageContent(ctx context.Context, node string, image *fixture.Image) {
	h.t.Helper()

	archive, err := image.Archive(pauseFixtureImage)
	if err != nil {
		h.t.Fatalf("archive pause fixture: %v", err)
	}

	commands := []string{
		"set -eu",
		"ctr -n k8s.io images import --local --all-platforms --no-unpack --base-name " +
			shellQuote(pauseFixtureImage) + " - >/dev/null",
	}

	gcRootLabel := "containerd.io/gc.root=gantry-e2e-multiarch-cache-miss"
	for _, digest := range image.Digests() {
		commands = append(commands, "ctr -n k8s.io content label "+shellQuote(digest.String())+
			" "+shellQuote(gcRootLabel)+" >/dev/null")
	}

	commands = append(commands,
		"ctr -n k8s.io images rm "+shellQuote(pauseFixtureImage)+" >/dev/null",
		"if ctr -n k8s.io images ls -q | grep -Fx "+shellQuote(pauseFixtureImage)+" >/dev/null; then "+
			"echo 'fixture image metadata still exists' >&2; exit 1; fi",
	)
	for _, digest := range image.Digests() {
		commands = append(commands, "ctr -n k8s.io content get "+shellQuote(digest.String())+" >/dev/null")
	}

	if err := h.runWithInput(ctx, string(archive), h.containerEngine, "exec", "-i", node, "sh", "-c",
		strings.Join(commands, "; ")); err != nil {
		h.t.Fatalf("seed bare image content on %s: %v", node, err)
	}
}
