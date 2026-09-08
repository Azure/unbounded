//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestMain centralizes the suite-level boot/teardown so each test
// function gets a ready cluster and the cluster is torn down exactly
// once even if multiple tests are added later.
func TestMain(m *testing.M) {
	if err := guardAssumptions(); err != nil {
		// Fail loud rather than silently skip.
		os.Stderr.WriteString("e2e: " + err.Error() + "\n")
		os.Exit(2)
	}

	os.Exit(m.Run())
}

// TestSmoke_DaemonSetBecomesReadyAndPullThrough is the canary test. It proves the
// end-to-end pipeline works:
//
//   - kind cluster boots,
//   - the gantry image builds and side-loads,
//   - the deploy manifests apply cleanly,
//   - the DaemonSet rolls out to every node,
//   - each pod's /readyz turns green within the timeout,
//   - node-local containerd can pull through Gantry's mirror,
//   - a second node can reuse the warmed content through peer fetch.
//
// This is still a smoke-sized scenario; deeper auth, eviction, and chaos
// cases should be added as separate tests.
func TestSmoke_DaemonSetBecomesReadyAndPullThrough(t *testing.T) {
	h := newHarness(t)
	h.checkPrereqs()

	// 15-minute overall budget - kind boot can take 90 s on a cold
	// docker pull, then image build ~30 s, rollout ~30 s, with
	// generous slack.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	h.bootCluster(ctx)
	t.Cleanup(func() {
		// Best-effort teardown; use a fresh ctx since the test's
		// may already be canceled by a Fatal.
		tdCtx, tdCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer tdCancel()

		h.teardown(tdCtx)
	})

	h.buildAndLoadImage(ctx)
	h.applyManifests(ctx)
	h.waitForRollout(ctx)
	h.checkReadyz(ctx)

	h.installMirrorHosts(ctx)
	h.removePullImageFromNodes(ctx)
	workers := h.workerNodes(ctx)

	advertiseBefore := h.metricSum(ctx, "gantry_advertise_total")
	peerHitBefore := h.metricSum(ctx, "p2p_peer_fetch_total", `outcome="hit"`)

	h.deletePod(ctx, "gantry-e2e-first")
	h.applyPullPod(ctx, "gantry-e2e-first", workers[0])
	h.waitForPodReady(ctx, "gantry-e2e-first")
	h.waitForMetricIncrease(ctx, "gantry_advertise_total", advertiseBefore)

	h.deletePod(ctx, "gantry-e2e-second")
	h.applyPullPod(ctx, "gantry-e2e-second", workers[1])
	h.waitForPodReady(ctx, "gantry-e2e-second")
	h.waitForMetricIncrease(ctx, "p2p_peer_fetch_total", peerHitBefore, `outcome="hit"`)
}

// TestE2E_ColdStartDesignatedOriginPuller proves the per-digest cold-start
// invariant: when multiple nodes request the same image concurrently, every
// blob in that image is origin-pulled by exactly one node (its HRW rank-0
// owner) - not by every requester. HRW assignment is per-digest, so with N
// workers each blob lands on one of the N nodes; the work distributes across
// nodes but no digest is fetched twice. The thundering-herd hazard we're
// guarding against is N nodes all pulling the *same* blob, not the fact
// that different blobs go to different nodes.
//
//   - All nodes start with empty containerd (test image purged).
//   - Multiple nodes request the same content simultaneously.
//   - Across all pods, every "please_pull served" digest appears at most
//     once - no per-digest double-pull.
//   - Aggregate origin-pull count is bounded by the per-image blob count
//     (single image, single fetch per blob), so a runaway thundering herd
//     would inflate it well past the sanity ceiling.
//   - Sum of "please_pull served" log lines across pods is non-zero (proves
//     the designated-puller path actually fired rather than some bypass).
func TestE2E_ColdStartDesignatedOriginPuller(t *testing.T) {
	h := newHarness(t)
	h.checkPrereqs()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	h.bootCluster(ctx)
	t.Cleanup(func() {
		tdCtx, tdCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer tdCancel()

		h.teardown(tdCtx)
	})

	h.buildAndLoadImage(ctx)
	h.applyManifests(ctx)
	h.waitForRollout(ctx)
	h.checkReadyz(ctx)

	h.installMirrorHosts(ctx)
	h.removePullImageFromNodes(ctx) // Ensure all nodes start with empty cache
	workers := h.workerNodes(ctx)

	gantryA := h.gantryPodOnNode(ctx, workers[0])
	gantryB := h.gantryPodOnNode(ctx, workers[1])

	originPullABefore := h.metricSumOnPod(ctx, gantryA, "p2p_origin_pull_total")
	originPullBBefore := h.metricSumOnPod(ctx, gantryB, "p2p_origin_pull_total")

	// Schedule concurrent pulls on two worker nodes. HRW election
	// designates per-digest ownership; both nodes may take origin
	// pulls for different blobs of the same image.
	h.deletePod(ctx, "gantry-e2e-cold-1")
	h.applyPullPod(ctx, "gantry-e2e-cold-1", workers[0])

	h.deletePod(ctx, "gantry-e2e-cold-2")
	h.applyPullPod(ctx, "gantry-e2e-cold-2", workers[1])

	h.waitForPodReady(ctx, "gantry-e2e-cold-1")
	h.waitForPodReady(ctx, "gantry-e2e-cold-2")

	originPullAAfter := h.metricSumOnPod(ctx, gantryA, "p2p_origin_pull_total")
	originPullBAfter := h.metricSumOnPod(ctx, gantryB, "p2p_origin_pull_total")
	deltaA := originPullAAfter - originPullABefore
	deltaB := originPullBAfter - originPullBBefore
	totalPulls := deltaA + deltaB

	// At least one pod must have pulled. If both are zero, the
	// please_pull dispatch never reached the puller pump - either
	// coord broke or the test image was somehow already cached.
	if totalPulls == 0 {
		h.dumpDiagnostics(ctx)
		t.Fatalf("no origin pulls observed across either pod (A=%s, B=%s); designated-puller path never fired",
			gantryA, gantryB)
	}

	// Sanity ceiling: agnhost has ~13 blobs. A single cluster-wide
	// pull-each-blob-once should land around that number - generous
	// upper bound of 20 absorbs retries. Anything above means we
	// double-pulled at least one digest, which is the exact failure
	// mode HRW per-digest is supposed to prevent.
	if totalPulls > 20 {
		h.dumpDiagnostics(ctx)
		t.Fatalf("aggregate origin pulls (%.0f) exceeded the per-image sanity ceiling of 20: A=%.0f B=%.0f. Suggests a digest was pulled twice (thundering herd).",
			totalPulls, deltaA, deltaB)
	}

	// Cross-pod per-digest uniqueness - the real invariant. Extract
	// every "please_pull served" log digest from both pods and assert
	// no digest appears in both pods' logs. If HRW is honored each
	// digest will appear in exactly one pod's log; if HRW failed for
	// any digest, two pods will both have served the same digest and
	// the intersection is non-empty.
	servedA := h.pleasePullServedDigests(ctx, gantryA, 500)

	servedB := h.pleasePullServedDigests(ctx, gantryB, 500)
	if total := len(servedA) + len(servedB); total == 0 {
		h.dumpDiagnostics(ctx)
		t.Fatalf("origin pulls observed (A=%.0f B=%.0f) but no 'please_pull served' log lines on either pod - metric and log disagree",
			deltaA, deltaB)
	}

	var duplicated []string

	for d := range servedA {
		if _, ok := servedB[d]; ok {
			duplicated = append(duplicated, d)
		}
	}

	if len(duplicated) > 0 {
		h.dumpDiagnostics(ctx)
		t.Fatalf("HRW per-digest invariant violated: digests served by both pods (A=%s, B=%s): %v",
			gantryA, gantryB, duplicated)
	}
}

// TestE2E_EvictionRecovery proves that when containerd evicts content
// and a DHT provider record becomes stale (can no longer serve), the
// system correctly falls back to cold-start/origin without hanging
// or causing recursive peer failures:
//
//   - Pull content on node A to warm containerd.
//   - Advertise the content via DHT on node A.
//   - Delete the content from node A's containerd (simulate kubelet eviction).
//   - Trigger a pull on node B that would normally get from node A.
//   - Verify the peer fetch observes the new "notfound" outcome (the
//     classified-stale path, not the generic "error" bucket that no
//     longer carries meaning post-classification).
//   - Verify gantry_stale_provider_filtered_total increased (the
//     stale-provider memoization fired on at least one digest).
//   - Verify p2p_origin_pull_total increments (fallback succeeded).
//   - Verify the pod becomes ready.
func TestE2E_EvictionRecovery(t *testing.T) {
	h := newHarness(t)
	h.checkPrereqs()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	h.bootCluster(ctx)
	t.Cleanup(func() {
		tdCtx, tdCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer tdCancel()

		h.teardown(tdCtx)
	})

	h.buildAndLoadImage(ctx)
	h.applyManifests(ctx)
	h.waitForRollout(ctx)
	h.checkReadyz(ctx)

	h.installMirrorHosts(ctx)
	h.removePullImageFromNodes(ctx)
	workers := h.workerNodes(ctx)

	// Step 1: Pull on worker 0 to warm its containerd and DHT.
	h.deletePod(ctx, "gantry-e2e-evict-warm")
	h.applyPullPod(ctx, "gantry-e2e-evict-warm", workers[0])
	h.waitForPodReady(ctx, "gantry-e2e-evict-warm")
	h.deletePod(ctx, "gantry-e2e-evict-warm") // Cleanup the pod but leave the cached image.

	time.Sleep(2 * time.Second) // Let advertiser settle.

	// Step 2: Simulate kubelet eviction by deleting the image from worker 0's containerd.
	h.evictImageFromNode(ctx, workers[0])
	time.Sleep(2 * time.Second) // Let DHT record become stale (or at least unreliable).

	// Step 3: Pull on worker 1. This will query DHT, find worker 0, attempt peer fetch,
	// get 404, classify the provider stale, and fall back to origin.
	originPullBefore := h.metricSum(ctx, "p2p_origin_pull_total")
	peerNotFoundBefore := h.metricSum(ctx, "p2p_peer_fetch_total", `outcome="notfound"`)
	staleFilteredBefore := h.metricSum(ctx, "gantry_stale_provider_filtered_total")

	h.deletePod(ctx, "gantry-e2e-evict-pull")
	h.applyPullPod(ctx, "gantry-e2e-evict-pull", workers[1])
	// The production config keeps rediscovering peer seeds for up to 5m before
	// returning round 0's origin-fallback decision. Wait beyond that budget.
	h.waitForPodReadyTimeout(ctx, "gantry-e2e-evict-pull", "600s")

	// Assert: origin fallback worked.
	originPullAfter := h.metricSum(ctx, "p2p_origin_pull_total")
	if originPullAfter <= originPullBefore {
		h.dumpDiagnostics(ctx)
		t.Fatalf("expected fallback to origin after eviction, but p2p_origin_pull_total did not increase (before=%.0f after=%.0f)",
			originPullBefore, originPullAfter)
	}

	// Assert: the evicted node's 404s were classified into the new
	// "notfound" outcome bucket, not silently dropped. Generic
	// outcome="error" is no longer the carrier - see internal/mirror
	// peerFetchOutcome split (notfound/unavailable/server_error/
	// protocol_error/digest_mismatch). At least one digest must have
	// taken the peer-then-stale path or the stale provider was never
	// even consulted, which means DHT advertisement failed upstream.
	peerNotFoundAfter := h.metricSum(ctx, "p2p_peer_fetch_total", `outcome="notfound"`)

	staleFilteredAfter := h.metricSum(ctx, "gantry_stale_provider_filtered_total")
	if peerNotFoundAfter == peerNotFoundBefore && staleFilteredAfter == staleFilteredBefore {
		h.dumpDiagnostics(ctx)
		t.Fatalf("eviction recovery did not exercise the stale-provider path: peer_fetch_total{outcome=\"notfound\"} unchanged (%.0f) AND stale_provider_filtered_total unchanged (%.0f). Either DHT never advertised or the evicted node still answered the fetch.",
			peerNotFoundAfter, staleFilteredAfter)
	}
}

// TestE2E_ContainerdSocketAccess verifies that gantry pods have correct
// read/write access to the node's containerd socket. This validates that
// the deployment (file permissions, DaemonSet security context, mounts)
// is correctly configured for real-world containerd integration:
//
//   - Gantry pod is scheduled on a node.
//   - Verify the pod can stat the containerd socket without permission errors.
//   - Verify the pod can issue basic containerd API calls (e.g., List content).
//   - Confirm socket is writable (needed for container operations).
func TestE2E_ContainerdSocketAccess(t *testing.T) {
	h := newHarness(t)
	h.checkPrereqs()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	h.bootCluster(ctx)
	t.Cleanup(func() {
		tdCtx, tdCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer tdCancel()

		h.teardown(tdCtx)
	})

	h.buildAndLoadImage(ctx)
	h.applyManifests(ctx)
	h.waitForRollout(ctx)
	h.checkReadyz(ctx)

	// Verify gantry pods can access the containerd socket.
	for _, pod := range h.gantryPods(ctx) {
		h.verifyContainerdSocketAccess(ctx, pod)
	}
}

// TestE2E_ContainerdRestartRecovery verifies that replacing containerd's Unix
// socket does not strand the long-lived Gantry pod on the old socket inode.
func TestE2E_ContainerdRestartRecovery(t *testing.T) {
	h := newHarness(t)
	h.checkPrereqs()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	h.bootCluster(ctx)
	t.Cleanup(func() {
		tdCtx, tdCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer tdCancel()

		h.teardown(tdCtx)
	})

	h.buildAndLoadImage(ctx)
	h.applyManifests(ctx)
	h.waitForRollout(ctx)

	node := h.workerNodes(ctx)[0]
	pod := h.gantryPodOnNode(ctx, node)
	podUID := h.podUID(ctx, pod)
	restarts := h.podRestartCount(ctx, pod)
	oldSocketID := h.containerdSocketID(ctx, node)
	reconnects := h.metricSumOnPod(ctx, pod, "p2p_cdsub_reconnect_total")

	h.restartContainerd(ctx, node)
	h.waitForContainerdSocketReplacement(ctx, node, oldSocketID)
	h.waitForPodReadyByName(ctx, pod)
	h.waitForMetricIncreaseOnPod(ctx, pod, "p2p_cdsub_reconnect_total", reconnects)

	if got := h.podUID(ctx, pod); got != podUID {
		t.Fatalf("gantry pod UID changed across containerd restart: got %s, want %s", got, podUID)
	}

	if got := h.podRestartCount(ctx, pod); got != restarts {
		t.Fatalf("gantry container restart count changed across containerd restart: got %d, want %d", got, restarts)
	}

	h.installMirrorHosts(ctx)
	h.evictImageFromNode(ctx, node)
	h.deletePod(ctx, "gantry-e2e-after-containerd-restart")
	h.applyPullPod(ctx, "gantry-e2e-after-containerd-restart", node)
	h.waitForPodReady(ctx, "gantry-e2e-after-containerd-restart")
}
