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
	"errors"
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

	secret := h.rotationSecret("racer-credentials")
	bundle, err := wire.DecodeBundle(bytes.NewReader(secret.Data["bundle.json"]))
	require.NoError(h.t, err)

	var state racer.RotationState
	require.NoError(h.t, json.Unmarshal(secret.Data["rotation.json"], &state))

	return bundle, state, secret
}

// Never pass Secret JSON through command(), which logs stdout on failure.
func (h *harness) rotationSecret(name string) corev1.Secret {
	h.t.Helper()

	ctx, cancel := context.WithTimeout(h.ctx, 15*time.Second)
	defer cancel()

	raw, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", h.kubeconfig, "--request-timeout=10s", "get", "secret/"+name, "-n", namespace, "-o", "json").Output()
	require.NoError(h.t, err, "read rotation state without logging secret material")

	var secret corev1.Secret
	require.NoError(h.t, json.Unmarshal(raw, &secret))

	return secret
}

func (h *harness) awaitRotationGeneration(endpoint string, generation wire.Generation, phase string) {
	h.t.Helper()
	require.Eventually(h.t, func() bool {
		metrics := h.peerMetrics(endpoint, phase)
		require.Contains(h.t, metrics, "racer_keyring_generation")

		return metrics["racer_keyring_generation"] == uint64(generation)
	}, 45*time.Second, time.Second, "%s: Rust did not install generation %d", phase, generation)
}

// Interrupt control HTTPS only in the delayed reader's network namespace. Block
// replies too, so a previously established long poll cannot deliver activation.
// Peer traffic, diagnostics, token projection, identity and storage stay intact.
func (h *harness) holdControlDelivery(node peerNode) func() {
	h.t.Helper()
	id := strings.TrimPrefix(strings.TrimSpace(h.kubectl("get", "pod", node.pod, "-n", namespace, "-o", "jsonpath={.status.containerStatuses[0].containerID}")), "containerd://")

	var inspected struct {
		Info struct {
			PID int `json:"pid"`
		} `json:"info"`
	}
	require.NoError(h.t, json.Unmarshal([]byte(h.run("docker", "exec", node.name, "crictl", "inspect", id)), &inspected))
	require.Positive(h.t, inspected.Info.PID)
	base := []string{"exec", node.name, "nsenter", "-t", fmt.Sprint(inspected.Info.PID), "-n", "--", "iptables"}
	port := strings.TrimSpace(h.kubectl("get", "service/racer-controller", "-n", namespace, "-o", "jsonpath={.spec.ports[0].port}"))
	require.Equal(h.t, "8443", port, "outage rules assume the deployed control HTTPS port")

	rules := controlDeliveryRules()

	var installed [][]string

	command := func(operation string, rule []string) []string {
		return append(append(append([]string{}, base...), operation), rule...)
	}
	release := func() {
		var err error

		installed, err = removeControlDeliveryRules(installed, func(rule []string) ([]byte, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			return exec.CommandContext(ctx, "docker", command("-D", rule)...).CombinedOutput()
		})
		if err != nil {
			h.t.Errorf("release control delivery interruption: %v", err)
		}
	}
	h.t.Cleanup(release)

	for _, rule := range rules {
		h.run("docker", command("-I", rule)...)
		installed = append(installed, rule)
		h.run("docker", command("-C", rule)...)
	}

	return release
}

// Try every installed rule even after a failure. Retain failures so the registered
// cleanup can retry them after an explicit release, without re-removing successes.
func removeControlDeliveryRules(installed [][]string, remove func([]string) ([]byte, error)) ([][]string, error) {
	var (
		remaining [][]string
		failures  []error
	)

	for i := len(installed) - 1; i >= 0; i-- {
		rule := installed[i]

		output, err := remove(rule)
		if err == nil || strings.Contains(string(output), "Bad rule (does a matching rule exist in that chain?).") {
			continue
		}

		remaining = append(remaining, rule)
		failures = append(failures, fmt.Errorf("remove %s rule: %w: %s", rule[0], err, output))
	}

	return remaining, errors.Join(failures...)
}

func TestRemoveControlDeliveryRules(t *testing.T) {
	for _, scenario := range []string{"success", "output-failure", "both-fail", "already-absent"} {
		t.Run(scenario, func(t *testing.T) {
			var attempted []string

			failure := errors.New("delete failed")
			remaining, err := removeControlDeliveryRules(controlDeliveryRules(), func(rule []string) ([]byte, error) {
				attempted = append(attempted, rule[0])
				switch {
				case scenario == "already-absent":
					return []byte("iptables: Bad rule (does a matching rule exist in that chain?)."), failure
				case scenario == "both-fail", scenario == "output-failure" && rule[0] == "OUTPUT":
					return []byte("permission denied"), failure
				default:
					return nil, nil
				}
			})

			require.Equal(t, []string{"OUTPUT", "INPUT"}, attempted, "a failed OUTPUT deletion must not suppress INPUT cleanup")

			if scenario == "output-failure" || scenario == "both-fail" {
				require.ErrorIs(t, err, failure)
				require.Contains(t, err.Error(), "OUTPUT")

				if scenario == "both-fail" {
					require.Len(t, remaining, 2)
					require.Contains(t, err.Error(), "INPUT")
				} else {
					require.Equal(t, [][]string{controlDeliveryRules()[1]}, remaining)
				}
			} else {
				require.NoError(t, err)
				require.Empty(t, remaining)
			}

			retried := 0
			left, err := removeControlDeliveryRules(remaining, func([]string) ([]byte, error) {
				retried++
				return nil, nil
			})
			require.NoError(t, err)
			require.Empty(t, left)
			require.Equal(t, len(remaining), retried)
		})
	}
}

func controlDeliveryRules() [][]string {
	return [][]string{
		{"INPUT", "-p", "tcp", "--sport", "8443", "-m", "comment", "--comment", "racer-e2e-control-outage", "-j", "DROP"},
		{"OUTPUT", "-p", "tcp", "--dport", "8443", "-m", "comment", "--comment", "racer-e2e-control-outage", "-j", "REJECT", "--reject-with", "tcp-reset"},
	}
}

func TestControlDeliveryRules(t *testing.T) {
	rules := controlDeliveryRules()
	require.Len(t, rules, 2)
	require.Equal(t, []string{"INPUT", "-p", "tcp", "--sport", "8443"}, rules[0][:5])
	require.Equal(t, "DROP", rules[0][len(rules[0])-1], "in-flight long-poll replies must be blocked")
	require.Equal(t, []string{"OUTPUT", "-p", "tcp", "--dport", "8443"}, rules[1][:5])
	require.Equal(t, "tcp-reset", rules[1][len(rules[1])-1])
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
		// Two minutes of forward lifetime plus one minute of NotBefore skew.
		require.Equal(h.t, 3*time.Minute, leaf.NotAfter.Sub(leaf.NotBefore))
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
	h.kubectl("patch", "secret/racer-credentials", "-n", namespace, "--type=merge", "-p", string(patch))

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

	for i := range nodes {
		h.awaitRotationGeneration(urls[i], prepared.Generation, fmt.Sprintf("prepared-%d", i))
	}

	require.True(h.t, time.Now().Before(preparedState.ActivateAt), "preparation window exhausted before delivery interruption")
	release := h.holdControlDelivery(nodes[1])
	require.True(h.t, time.Now().Before(preparedState.ActivateAt), "delivery interruption missed preparation window")

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
	require.Len(h.t, active.CacheKeys, len(initial.CacheKeys))
	require.Len(h.t, activeState.Retiring, 1)

	for _, key := range active.CacheKeys {
		require.NotEqual(h.t, wire.PreparedKey, key.State)
	}

	for _, old := range initial.CacheKeys {
		found := false

		for _, key := range active.CacheKeys {
			if key.Key.Cache == old.Key.Cache && key.Key.Purpose == old.Key.Purpose && bytes.Equal(key.Key.ID, old.Key.ID) {
				found = true
			}
		}

		require.False(h.t, found, "replaced symmetric key retained at activation")
	}

	h.awaitRotationGeneration(urls[0], active.Generation, "active-server")
	require.Equal(h.t, uint64(prepared.Generation), h.peerMetrics(urls[1], "staggered-reader")["racer_keyring_generation"], "interrupted reader must actually remain behind")

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
	require.Equal(h.t, beforeGets, gets, "staggered delivery must fetch only the requested origin page")
	require.Equal(h.t, uint64(prepared.Generation), after["racer_keyring_generation"], "reader must stay delayed throughout the read")
	release()
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
			require.Equal(h.t, 3*time.Minute, leaf.NotAfter.Sub(leaf.NotBefore))

			return h.peerMetrics(urls[i], fmt.Sprintf("renewed-%d", i))["racer_identity_expires_at_seconds"] == uint64(leaf.NotAfter.Unix())
		}, 100*time.Second, time.Second, "node %s did not install controller-issued renewal", node.name)
	}

	var (
		pruned      wire.KeyringBundle
		credentials corev1.Secret
	)

	lastRead := time.Time{}

	require.Eventually(h.t, func() bool {
		if time.Since(lastRead) >= 5*time.Second {
			h.readPeerPage(nodes[0], fixture, 0)

			lastRead = time.Now()
		}

		pruned, _, credentials = h.rotationState()

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

	var material struct {
		Keys map[string]json.RawMessage `json:"keys"`
	}
	require.NoError(h.t, json.Unmarshal(credentials.Data["issuer.json"], &material))
	_, retained := material.Keys[initialState.ActiveIssuer]
	require.False(h.t, retained, "root and private key must be pruned in the same version")
	require.Len(h.t, material.Keys, 1)

	for i := range nodes {
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
	h.t.Logf("live rotation, staggered delivery, renewal and prune completed in %s", time.Since(started))
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
