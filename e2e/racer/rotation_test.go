//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/Azure/unbounded/internal/racer"
	"github.com/Azure/unbounded/internal/racer/wire"
)

// Twenty full pages exceed the default node-wide 256 MiB plaintext budget.
// Spread them evenly across the default maximum of four workers. Each worker's
// memory budget is exceeded without filling its larger disk shard, including
// hosts restricted to fewer workers. Twelve buckets cover divisors 1 through 4.
func (h *harness) rotationDiskFixtures() []*peerOrigin {
	h.t.Helper()
	cache := strings.TrimSpace(h.kubectl("get", "clustercache", "gantry", "-o", "jsonpath={.metadata.uid}"))

	fixtures := make([]*peerOrigin, 0, 20)
	for nonce := uint64(0); len(fixtures) < cap(fixtures) && nonce < 512; nonce++ {
		body := bytes.Repeat([]byte{0x93}, peerPageSize)
		binary.BigEndian.PutUint64(body, nonce)

		key := sha256.Sum256(body)
		if localWorkerBucket(cache, key) != uint64(len(fixtures)%12) {
			continue
		}

		fixtures = append(fixtures, &peerOrigin{body: body, id: digest(body), gets: make(map[string]int)})
	}

	require.Len(h.t, fixtures, 20, "could not balance disk-pressure fixture across worker buckets")

	return fixtures
}

func localWorkerBucket(cache string, key [32]byte) uint64 {
	input := []byte("racer.local-worker.v1\x00")
	input = binary.BigEndian.AppendUint64(input, uint64(len(cache)))
	input = append(input, cache...)
	input = append(input, key[:]...)
	input = binary.BigEndian.AppendUint64(input, 0)
	sum := sha256.Sum256(input)

	return binary.BigEndian.Uint64(sum[:8]) % 12
}

func TestRotationLocalWorkerBucket(t *testing.T) {
	// Independent digest e5bf2f34c9eba92e8e3c08610501bfcc2ca7fc7aa3a5c27c032edfbdeb471f32
	// for the production WorkerMap page-zero encoding.
	var key [32]byte
	for i := range key {
		key[i] = 0xab
	}

	require.Equal(t, uint64(6), localWorkerBucket("cache-a", key))
}

// Set short leaf and retention lifetimes before the first enrollment. Keep the
// normal interval so earlier source-accounting tests cannot race a rotation.
const rotationOverrides = `apiVersion: v1
kind: ConfigMap
metadata:
  name: unbounded-component-overrides
  namespace: unbounded-system
data:
  rotation.yaml: |
    apiVersion: overrides.unbounded-cloud.io/v1alpha1
    overrides:
      - component: racer
        kind: Deployment
        patch:
          spec:
            template:
              spec:
                containers:
                  - name: controller
                    env:
                      - {name: RACER_CERTIFICATE_LIFETIME, value: "2m"}
                      - {name: RACER_ROTATION_PREPARE_FOR, value: "60s"}
                      - {name: RACER_ROTATION_RETAIN_FOR, value: "2m"}
`

// Public synthetic fixture value, never a registry credential. CopyOnly seals
// this with the active origin key but deliberately never decrypts it remotely.
const rotationAuthorization = "Bearer racer-e2e-public-rotation-fixture"

func (h *harness) rotationState() (wire.KeyringBundle, racer.RotationState, corev1.Secret) {
	h.t.Helper()

	var secret corev1.Secret
	require.NoError(h.t, json.Unmarshal([]byte(h.kubectl("get", "secret/racer-keyring", "-n", namespace, "-o", "json")), &secret))
	bundle, err := wire.DecodeBundle(bytes.NewReader(secret.Data["bundle.json"]))
	require.NoError(h.t, err)

	var state racer.RotationState
	require.NoError(h.t, json.Unmarshal(secret.Data["rotation.json"], &state))

	return bundle, state, secret
}

func (h *harness) awaitRotationGeneration(endpoint string, generation wire.Generation, phase string) {
	h.t.Helper()
	require.Eventually(h.t, func() bool {
		metrics := h.peerMetrics(endpoint, phase)
		require.Contains(h.t, metrics, "racer_keyring_generation")

		return metrics["racer_keyring_generation"] == uint64(generation)
	}, 45*time.Second, time.Second, "%s: Rust did not install generation %d", phase, generation)
}

// Pin a copy of a genuine kubelet projection in only this pod's mount namespace.
// Kubelet, token refresh, control polling, and the underlying Secret volume keep
// progressing. Unmount exposes the newest real Kubernetes projection again.
func (h *harness) holdKeyringProjection(node peerNode, expected wire.KeyringBundle) func() {
	h.t.Helper()
	id := strings.TrimPrefix(strings.TrimSpace(h.kubectl("get", "pod", node.pod, "-n", namespace, "-o", "jsonpath={.status.containerStatuses[0].containerID}")), "containerd://")

	var inspected struct {
		Info struct {
			PID int `json:"pid"`
		} `json:"info"`
	}
	require.NoError(h.t, json.Unmarshal([]byte(h.run("docker", "exec", node.name, "crictl", "inspect", id)), &inspected))
	require.Positive(h.t, inspected.Info.PID)
	base := []string{"exec", node.name, "nsenter", "-t", fmt.Sprint(inspected.Info.PID), "-m", "--root", "--"}
	held := false

	const copyRoot = "/var/lib/racer/identity/e2e-projection"

	release := func() {
		if held {
			h.command(context.Background(), "docker", append(base, "umount", "/etc/racer/keyring")...)

			held = false
		}

		h.command(context.Background(), "docker", append(base, "rm", "-rf", copyRoot)...)
	}
	h.t.Cleanup(release)
	// Output contains key material and must never pass through harness.command,
	// whose failure diagnostics include stdout. Decode/compare only in memory.
	readBundle := func(path string) (wire.KeyringBundle, bool) {
		ctx, cancel := context.WithTimeout(h.ctx, 2*time.Second)
		defer cancel()

		raw, err := exec.CommandContext(ctx, "docker", append(base, "cat", path+"/bundle.json")...).Output()
		if err != nil {
			return wire.KeyringBundle{}, false
		}

		bundle, err := wire.DecodeBundle(bytes.NewReader(raw))

		return bundle, err == nil
	}
	equal := func(bundle wire.KeyringBundle) bool {
		got, err := wire.EncodeBundle(bundle)
		want, wantErr := wire.EncodeBundle(expected)

		return err == nil && wantErr == nil && bytes.Equal(got, want)
	}

	require.Eventually(h.t, func() bool {
		before, ok := readBundle("/etc/racer/keyring")
		if !ok || !equal(before) {
			return false
		}

		ctx, cancel := context.WithTimeout(h.ctx, 3*time.Second)
		defer cancel()
		// Keep a private parent: cp -a may restore the source directory's mode.
		script := "umask 077; rm -rf " + copyRoot + "; mkdir -m 700 " + copyRoot + "; mkdir " + copyRoot + "/bundle; cp -a /etc/racer/keyring/. " + copyRoot + "/bundle/"
		if exec.CommandContext(ctx, "docker", append(base, "sh", "-ec", script)...).Run() != nil {
			return false
		}

		copied, ok := readBundle(copyRoot + "/bundle")
		after, afterOK := readBundle("/etc/racer/keyring")

		return ok && afterOK && equal(copied) && equal(after)
	}, 8*time.Second, 200*time.Millisecond, "could not capture coherent prepared projection")
	h.run("docker", append(base, "mount", "--bind", copyRoot+"/bundle", "/etc/racer/keyring")...)

	held = true
	installed, ok := readBundle("/etc/racer/keyring")
	require.True(h.t, ok && equal(installed), "mounted projection differs from validated prepared bundle")

	return release
}

func (h *harness) rotationIdentity(node peerNode) wire.BootstrapResponse {
	h.t.Helper()
	// Decode only the public enrollment response; never persist the identity file
	// in diagnostics or compare/print its node-private key.
	ctx, cancel := context.WithTimeout(h.ctx, 2*time.Second)
	defer cancel()

	raw, err := exec.CommandContext(ctx, "docker", "exec", node.name, "cat", "/var/lib/racer/identity/private/identity.json").Output()
	require.NoError(h.t, err, "read node identity without logging private material")

	var persisted struct {
		Response []byte `json:"response"`
	}
	require.NoError(h.t, json.Unmarshal(raw, &persisted))
	response, err := wire.DecodeBootstrapResponse(bytes.NewReader(persisted.Response))
	require.NoError(h.t, err)
	require.Equal(h.t, wire.NodeID(node.uid), response.Node)

	return response
}

func (h *harness) verifyLiveRotation(nodes [2]peerNode, fixture *peerOrigin, pressure []*peerOrigin) {
	h.t.Helper()

	started := time.Now()
	podState := func() string {
		return h.kubectl("get", "pods", "-n", namespace, "-l", "app.kubernetes.io/name=racer-dataplane", "--sort-by=.metadata.uid", "-o", `jsonpath={range .items[*]}{.metadata.uid}{":"}{.status.containerStatuses[*].restartCount}{"\n"}{end}`)
	}
	beforePods := podState()
	urls := [2]string{h.racerDiagnostics(nodes[0].pod), h.racerDiagnostics(nodes[1].pod)}
	initial, initialState, secret := h.rotationState()

	var oldLeaves [2]*x509.Certificate

	for i, node := range nodes {
		h.awaitRotationGeneration(urls[i], initial.Generation, fmt.Sprintf("initial-%d", i))
		identity := h.rotationIdentity(node)
		leaf, err := x509.ParseCertificate(identity.CertificateChain[0])
		require.NoError(h.t, err)
		require.Equal(h.t, 2*time.Minute, leaf.NotAfter.Sub(leaf.NotBefore))
		oldLeaves[i] = leaf
	}
	// Warm only the serving node, leaving reader pages cold for the rotation.
	persistedBefore := h.awaitDiskIdle(urls[0], "before-initial-target")
	h.readPeerPage(nodes[0], fixture, 0)
	h.awaitDiskPublication(urls[0], persistedBefore, "initial-target")

	for page := 1; page < 3; page++ {
		h.readPeerPage(nodes[0], fixture, page)
	}

	beforeWarm, _ := fixture.counts()
	h.readPeerPage(nodes[0], fixture, 0)
	gets, _ := fixture.counts()
	require.Equal(h.t, beforeWarm, gets, "pre-rotation cached bytes must be reusable")
	h.verifyRotationDisk(nodes[0], urls[0], fixture, pressure, "before-rotation")

	// Advance only the next rotation timestamp. The real Go reconciler
	// generates, stages, activates and prunes every key/root; no bundle is forged.
	initialState.NextRotation = time.Now().UTC().Truncate(time.Second)
	stateBytes, err := json.Marshal(initialState)
	require.NoError(h.t, err)
	patch, err := json.Marshal(map[string]any{"metadata": map[string]string{"resourceVersion": secret.ResourceVersion}, "data": map[string][]byte{"rotation.json": stateBytes}})
	require.NoError(h.t, err)
	h.kubectl("patch", "secret/racer-keyring", "-n", namespace, "--type=merge", "-p", string(patch))

	var (
		prepared      wire.KeyringBundle
		preparedState racer.RotationState
	)

	require.Eventually(h.t, func() bool {
		prepared, preparedState, _ = h.rotationState()
		return preparedState.PreparedIssuer != ""
	}, 10*time.Second, time.Second, "Go controller did not stage replacement credentials")
	require.Equal(h.t, initialState.ActiveIssuer, preparedState.ActiveIssuer)
	require.Len(h.t, prepared.PeerTrustRoots, 2)
	require.Len(h.t, prepared.CacheKeys, 2*len(initial.CacheKeys))

	preparedKeys := 0

	for _, key := range prepared.CacheKeys {
		if key.State == wire.PreparedKey {
			preparedKeys++
		}
	}

	require.Equal(h.t, len(initial.CacheKeys), preparedKeys)

	for _, node := range nodes {
		h.kubectl("annotate", "pod", node.pod, "-n", namespace, "e2e.racer/rotation=prepared", "--overwrite")
	}

	for i := range nodes {
		h.awaitRotationGeneration(urls[i], prepared.Generation, fmt.Sprintf("prepared-%d", i))
	}

	require.True(h.t, time.Now().Before(preparedState.ActivateAt), "preparation window exhausted before projection pin")
	release := h.holdKeyringProjection(nodes[1], prepared)
	require.True(h.t, time.Now().Before(preparedState.ActivateAt), "projection pin missed preparation window")

	var (
		active      wire.KeyringBundle
		activeState racer.RotationState
	)

	require.Eventually(h.t, func() bool {
		active, activeState, _ = h.rotationState()
		return activeState.ActiveIssuer != initialState.ActiveIssuer && activeState.PreparedIssuer == ""
	}, time.Until(preparedState.ActivateAt)+15*time.Second, time.Second, "Go controller did not activate its prepared issuer")
	require.Equal(h.t, preparedState.PreparedIssuer, activeState.ActiveIssuer)
	require.Len(h.t, active.PeerTrustRoots, 2)

	for _, key := range active.CacheKeys {
		require.NotEqual(h.t, wire.PreparedKey, key.State)
	}

	for _, old := range initial.CacheKeys {
		found := false

		for _, key := range active.CacheKeys {
			if key.Key.Cache == old.Key.Cache && key.Key.Purpose == old.Key.Purpose && bytes.Equal(key.Key.ID, old.Key.ID) {
				require.Equal(h.t, wire.RetiringKey, key.State)

				found = true
			}
		}

		require.True(h.t, found, "old opaque key reference lost before retention deadline")
	}
	// Nudge kubelet's ordinary projection sync, without touching credential data.
	h.kubectl("annotate", "pod", nodes[0].pod, "-n", namespace, "e2e.racer/rotation=active", "--overwrite")
	h.awaitRotationGeneration(urls[0], active.Generation, "active-server")
	require.Equal(h.t, uint64(prepared.Generation), h.peerMetrics(urls[1], "staggered-reader")["racer_keyring_generation"], "delayed projection must actually remain behind")

	// Retired-key admission closes immediately. The previously warm server must
	// refill, rather than misreport an old-key cache hit as rotation success.
	before := h.peerMetrics(urls[0], "before-active-refill")
	persistedBefore = h.awaitDiskIdle(urls[0], "before-active-target")
	beforeGets, _ := fixture.counts()
	h.readPeerPage(nodes[0], fixture, 0)
	h.awaitDiskPublication(urls[0], persistedBefore, "active-target")
	after := h.peerMetrics(urls[0], "after-active-refill")
	require.Equal(h.t, uint64(1), after["racer_origin_fills_total"]-before["racer_origin_fills_total"])

	gets, _ = fixture.counts()
	beforeGets["bytes=0-16777215"]++
	require.Equal(h.t, beforeGets, gets, "retired page must refill once through the deployed origin adapter")
	// Both nodes are candidates, so reader probes its predecessor with CopyOnly.
	// The server's retired page cannot be admitted and CopyOnly cannot start an
	// origin fill. Prepared keys could read a new copy, but none exists for page 1.
	serverBeforeProbe := h.peerMetrics(urls[0], "before-copy-only-probe")
	before = h.peerMetrics(urls[1], "before-staggered-read")
	readStarted := time.Now()

	h.readPeerPage(nodes[1], fixture, 1)
	require.Less(h.t, time.Since(readStarted), 30*time.Second)
	after = h.peerMetrics(urls[1], "after-staggered-read")
	require.Equal(h.t, uint64(1), after["racer_origin_fills_total"]-before["racer_origin_fills_total"])
	require.Equal(h.t, before["racer_peer_hits_total"], after["racer_peer_hits_total"])
	serverAfterProbe := h.peerMetrics(urls[0], "after-copy-only-probe")
	require.Equal(h.t, serverBeforeProbe["racer_origin_fills_total"], serverAfterProbe["racer_origin_fills_total"], "CopyOnly predecessor must not acquire from origin")

	gets, _ = fixture.counts()
	beforeGets["bytes=16777216-33554431"]++
	require.Equal(h.t, beforeGets, gets, "staggered projection must fetch only the requested origin page")
	release()
	h.kubectl("annotate", "pod", nodes[1].pod, "-n", namespace, "e2e.racer/rotation=active", "--overwrite")
	h.awaitRotationGeneration(urls[1], active.Generation, "active-reader")

	// Observe a new public leaf on each unchanged live process, verify its signer,
	// then require the running runtime's expiry gauge to match the persisted leaf.
	for i, node := range nodes {
		lastRead := time.Time{}

		require.Eventually(h.t, func() bool {
			if time.Since(lastRead) >= 5*time.Second {
				h.readPeerPage(nodes[0], fixture, 0)

				lastRead = time.Now()
			}

			identity := h.rotationIdentity(node)
			leaf, err := x509.ParseCertificate(identity.CertificateChain[0])
			require.NoError(h.t, err)
			root, err := x509.ParseCertificate(identity.CertificateChain[1])
			require.NoError(h.t, err)

			if fmt.Sprintf("%x", sha256.Sum256(root.Raw)) != activeState.ActiveIssuer || bytes.Equal(leaf.Raw, oldLeaves[i].Raw) {
				return false
			}

			require.NoError(h.t, leaf.CheckSignatureFrom(root))
			require.Equal(h.t, 2*time.Minute, leaf.NotAfter.Sub(leaf.NotBefore))

			return h.peerMetrics(urls[i], fmt.Sprintf("renewed-%d", i))["racer_identity_expires_at_seconds"] == uint64(leaf.NotAfter.Unix())
		}, 100*time.Second, time.Second, "node %s did not install controller-issued renewal", node.name)
	}

	var pruned wire.KeyringBundle

	lastRead := time.Time{}

	require.Eventually(h.t, func() bool {
		if time.Since(lastRead) >= 5*time.Second {
			h.readPeerPage(nodes[0], fixture, 0)

			lastRead = time.Now()
		}

		pruned, _, _ = h.rotationState()

		return len(pruned.PeerTrustRoots) == 1
	}, max(15*time.Second, time.Until(activeState.Retiring[initialState.ActiveIssuer])+15*time.Second), time.Second, "Go controller did not prune old root")
	require.Greater(h.t, pruned.Generation, active.Generation)
	require.Equal(h.t, activeState.ActiveIssuer, fmt.Sprintf("%x", sha256.Sum256(pruned.PeerTrustRoots[0])))
	require.Len(h.t, pruned.CacheKeys, len(initial.CacheKeys))

	for _, key := range pruned.CacheKeys {
		require.Equal(h.t, wire.ActiveKey, key.State)

		for _, old := range initial.CacheKeys {
			require.False(h.t, bytes.Equal(old.Key.ID, key.Key.ID), "old key remains after prune")
		}
	}

	var issuer corev1.Secret

	require.Eventually(h.t, func() bool {
		require.NoError(h.t, json.Unmarshal([]byte(h.kubectl("get", "secret/racer-issuer", "-n", namespace, "-o", "json")), &issuer))

		var material struct {
			Keys map[string]json.RawMessage `json:"keys"`
		}
		require.NoError(h.t, json.Unmarshal(issuer.Data["issuer.json"], &material))
		_, retained := material.Keys[initialState.ActiveIssuer]

		return !retained && len(material.Keys) == 1
	}, 10*time.Second, time.Second, "old issuer private material not pruned")

	for i, node := range nodes {
		h.kubectl("annotate", "pod", node.pod, "-n", namespace, "e2e.racer/rotation=pruned", "--overwrite")
		h.awaitRotationGeneration(urls[i], pruned.Generation, fmt.Sprintf("pruned-%d", i))
		require.True(h.t, time.Now().After(oldLeaves[i].NotAfter), "old certificate must really have expired")
		h.waitHTTP(urls[i] + "/readyz")
	}
	// Refill page two under the active key, then demand a real peer hit from the
	// still-cold reader, using the fixture's deterministic serving-node ranking.
	h.readPeerPage(nodes[0], fixture, 2)
	before = h.peerMetrics(urls[1], "before-renewed-peer")
	gets, _ = fixture.counts()
	h.readPeerPage(nodes[1], fixture, 2)
	after = h.peerMetrics(urls[1], "after-renewed-peer")
	require.Equal(h.t, uint64(1), after["racer_peer_hits_total"]-before["racer_peer_hits_total"])
	require.Equal(h.t, before["racer_origin_fills_total"], after["racer_origin_fills_total"])

	finalGets, _ := fixture.counts()
	require.Equal(h.t, gets, finalGets)
	h.readPeerPage(nodes[1], fixture, 2)
	finalGets, _ = fixture.counts()
	require.Equal(h.t, gets, finalGets, "refilled page not reusable after old-key prune")
	h.verifyRotationDisk(nodes[0], urls[0], fixture, pressure, "after-prune")
	require.Equal(h.t, beforePods, podState(), "restart cannot substitute for live rotation")
	h.t.Logf("live rotation, staggered projection, renewal and prune completed in %s", time.Since(started))
}

func (h *harness) verifyRotationDisk(node peerNode, endpoint string, fixture *peerOrigin, pressure []*peerOrigin, phase string) {
	h.t.Helper()

	for _, filler := range pressure {
		h.readPeerPage(node, filler, 0)
	}

	before := h.peerMetrics(endpoint, phase+"-disk-before")
	gets, _ := fixture.counts()
	h.readPeerPage(node, fixture, 0)
	after := h.peerMetrics(endpoint, phase+"-disk-after")
	require.Contains(h.t, after, "racer_disk_hits_total")
	require.Equal(h.t, uint64(1), after["racer_disk_hits_total"]-before["racer_disk_hits_total"], "memory pressure must force an authenticated disk read")
	require.Equal(h.t, before["racer_origin_fills_total"], after["racer_origin_fills_total"])

	finalGets, _ := fixture.counts()
	require.Equal(h.t, gets, finalGets)
}

func (h *harness) awaitDiskIdle(endpoint, phase string) uint64 {
	h.t.Helper()

	var metrics map[string]uint64

	require.Eventually(h.t, func() bool {
		metrics = h.peerMetrics(endpoint, phase)
		require.Contains(h.t, metrics, "racer_pending_disk_writes")
		require.Contains(h.t, metrics, "racer_disk_publications_total")

		return metrics["racer_pending_disk_writes"] == 0
	}, 10*time.Second, 200*time.Millisecond, "disk writes did not drain before isolated target fill")

	return metrics["racer_disk_publications_total"]
}

func (h *harness) awaitDiskPublication(endpoint string, before uint64, phase string) {
	h.t.Helper()
	require.Eventually(h.t, func() bool {
		metrics := h.peerMetrics(endpoint, phase)
		return metrics["racer_pending_disk_writes"] == 0 && metrics["racer_disk_publications_total"] == before+1
	}, 10*time.Second, 200*time.Millisecond, "isolated target was not published to disk before pressure")
}
