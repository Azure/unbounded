// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
	"github.com/Azure/unbounded/internal/gantry/members"
)

// rewriteWildcardMultiaddr substitutes a Pod IP into wildcard listen
// addresses so the agent publishes dialable p2p multiaddrs in its
// self-announce annotation. Without the substitution, peers receive
// /ip4/0.0.0.0/tcp/4001 and silently fail to connect, deadlocking
// libp2p bootstrap on a cold cluster.
func TestRewriteWildcardMultiaddr(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		podIP string
		want  string
	}{
		{
			name:  "ipv4 wildcard with pod IP",
			in:    "/ip4/0.0.0.0/tcp/4001",
			podIP: "10.42.0.7",
			want:  "/ip4/10.42.0.7/tcp/4001",
		},
		{
			name:  "ipv4 wildcard without pod IP returns empty (skip)",
			in:    "/ip4/0.0.0.0/tcp/4001",
			podIP: "",
			want:  "",
		},
		{
			name:  "ipv6 wildcard with v6 pod IP rewrites to /ip6/",
			in:    "/ip6/::/tcp/4001",
			podIP: "fd00:10:244::7",
			want:  "/ip6/fd00:10:244::7/tcp/4001",
		},
		{
			name:  "ipv4 wildcard with v6 pod IP skips (no v6 listener)",
			in:    "/ip4/0.0.0.0/tcp/4001",
			podIP: "fd00:10:244::7",
			want:  "",
		},
		{
			name:  "ipv6 wildcard with v4 pod IP skips (no v4 listener under /ip6/)",
			in:    "/ip6/::/tcp/4001",
			podIP: "10.42.0.7",
			want:  "",
		},
		{
			name:  "wildcard with unparseable pod IP returns empty",
			in:    "/ip4/0.0.0.0/tcp/4001",
			podIP: "not-an-ip",
			want:  "",
		},
		{
			name:  "concrete ipv4 passes through",
			in:    "/ip4/10.42.0.7/tcp/4001",
			podIP: "10.42.0.7",
			want:  "/ip4/10.42.0.7/tcp/4001",
		},
		{
			name:  "concrete ipv6 passes through",
			in:    "/ip6/2001:db8::1/tcp/4001",
			podIP: "10.42.0.7",
			want:  "/ip6/2001:db8::1/tcp/4001",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteWildcardMultiaddr(tc.in, tc.podIP)
			if got != tc.want {
				t.Errorf("rewriteWildcardMultiaddr(%q, %q) = %q, want %q",
					tc.in, tc.podIP, got, tc.want)
			}
		})
	}
}

// advertisedTransferAddr leaves empty when the listen host is a
// wildcard and no Pod IP is available, so members.Snapshot falls back
// to composing podIP:port from the pod's status.podIP. A non-empty
// 0.0.0.0:port published in the annotation would override that
// fallback and produce an unreachable advertised address.
//
// It also leaves empty for the cross-family case (e.g. v4 wildcard
// with a v6 Pod IP) so that an undialable annotation isn't published.
// In that case the readiness probe (see TestTransferAddrFamilyMismatch)
// is responsible for failing the rollout so the broken pod never goes
// Ready and never appears in peers' Snapshot views.
func TestAdvertisedTransferAddr(t *testing.T) {
	cases := []struct {
		name           string
		transferListen string
		podIP          string
		want           string
	}{
		{
			name:           "ipv4 wildcard + ipv4 pod IP composes",
			transferListen: "0.0.0.0:5001",
			podIP:          "10.42.0.7",
			want:           "10.42.0.7:5001",
		},
		{
			name:           "ipv4 wildcard + empty pod IP returns empty",
			transferListen: "0.0.0.0:5001",
			podIP:          "",
			want:           "",
		},
		{
			name:           "ipv4 wildcard + ipv6 pod IP returns empty (cross-family)",
			transferListen: "0.0.0.0:5001",
			podIP:          "fd00::1234",
			want:           "",
		},
		{
			name:           "ipv6 wildcard + ipv6 pod IP composes with brackets",
			transferListen: "[::]:5001",
			podIP:          "fd00::1234",
			want:           "[fd00::1234]:5001",
		},
		{
			name:           "ipv6 wildcard + ipv4 pod IP returns empty (cross-family)",
			transferListen: "[::]:5001",
			podIP:          "10.42.0.7",
			want:           "",
		},
		{
			name:           "ipv6 wildcard + empty pod IP returns empty",
			transferListen: "[::]:5001",
			podIP:          "",
			want:           "",
		},
		{
			name:           "explicit bind passes through",
			transferListen: "10.42.0.7:5001",
			podIP:          "10.42.0.7",
			want:           "10.42.0.7:5001",
		},
		{
			name:           "empty host (dual-stack) + ipv4 pod IP composes",
			transferListen: ":5001",
			podIP:          "10.42.0.7",
			want:           "10.42.0.7:5001",
		},
		{
			name:           "empty host (dual-stack) + ipv6 pod IP composes",
			transferListen: ":5001",
			podIP:          "fd00::1234",
			want:           "[fd00::1234]:5001",
		},
		{
			name:           "unparseable listen passes through verbatim",
			transferListen: "notahostport",
			podIP:          "10.42.0.7",
			want:           "notahostport",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := advertisedTransferAddr(tc.transferListen, tc.podIP)
			if got != tc.want {
				t.Errorf("advertisedTransferAddr(%q, %q) = %q, want %q",
					tc.transferListen, tc.podIP, got, tc.want)
			}
		})
	}
}

// transferAddrFamilyMismatch must return true exactly for the
// configurations where advertisedTransferAddr drops to "" because of
// a cross-family wildcard listener (not because of a missing Pod IP
// or a non-K8s deploy). The readiness probe uses this to fail
// /readyz with a targeted message when the transfer endpoint is
// misconfigured for the pod's IP family.
func TestTransferAddrFamilyMismatch(t *testing.T) {
	cases := []struct {
		name           string
		transferListen string
		podIP          string
		want           bool
	}{
		{"v4 wildcard + v4 pod -> OK", "0.0.0.0:5001", "10.42.0.7", false},
		{"v4 wildcard + v6 pod -> MISMATCH", "0.0.0.0:5001", "fd00::1234", true},
		{"v6 wildcard + v6 pod -> OK", "[::]:5001", "fd00::1234", false},
		{"v6 wildcard + v4 pod -> MISMATCH", "[::]:5001", "10.42.0.7", true},
		{"empty host (dual-stack) + v4 pod -> OK", ":5001", "10.42.0.7", false},
		{"empty host (dual-stack) + v6 pod -> OK", ":5001", "fd00::1234", false},
		{"explicit v4 bind -> not a wildcard -> OK", "10.42.0.7:5001", "10.42.0.7", false},
		{"explicit v6 bind -> not a wildcard -> OK", "[fd00::1234]:5001", "fd00::1234", false},
		{"empty pod IP (non-K8s) -> never a mismatch", "0.0.0.0:5001", "", false},
		{"unparseable listen -> never a mismatch (annotation passes verbatim)", "notahostport", "10.42.0.7", false},
		{"unparseable pod IP -> never a mismatch (treat as opaque)", "0.0.0.0:5001", "not-an-ip", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := transferAddrFamilyMismatch(tc.transferListen, tc.podIP); got != tc.want {
				t.Errorf("transferAddrFamilyMismatch(%q, %q) = %v; want %v",
					tc.transferListen, tc.podIP, got, tc.want)
			}
		})
	}
}

// hasMultiNodeMembership controls whether the cold-start orchestrator
// is wired at startup. On first-cluster boot the membership snapshot
// is empty (no peer is Ready yet) — but cold-start must still be
// enabled because that is exactly the situation it exists to handle.
// The gate therefore looks at whether the membership view is backed
// by the real K8s informer (cold-start ON) vs the dev-mode single-
// self fake (cold-start OFF, no peers to coordinate with).
func TestHasMultiNodeMembership(t *testing.T) {
	t.Run("real manager with empty snapshot still enables cold-start", func(t *testing.T) {
		// A *members.Manager with no Start() called yet has an empty
		// snapshot — the first-cluster-boot scenario. The previous
		// implementation returned false here and permanently disabled
		// cold-start; the new implementation returns true.
		var mgr *members.Manager // typed nil; only the dynamic type matters
		got := hasMultiNodeMembership(mgr)
		if !got {
			t.Errorf("hasMultiNodeMembership(*members.Manager) = false, want true (first-cluster boot must enable cold-start)")
		}
	})
	t.Run("single-self fake disables cold-start", func(t *testing.T) {
		f := fakes.NewMembers(ifaces.NodeID("self"), ifaces.Node{ID: "self"})
		if hasMultiNodeMembership(f) {
			t.Errorf("hasMultiNodeMembership(single-self fake) = true, want false (no peers to coordinate)")
		}
	})
}

// isProductionMode is the gate that decides whether a Kubernetes-
// membership setup failure crashes the agent or silently falls back
// to a single-self stub. Misclassifying this is the difference
// between a clear deploy-time error and an apparently-healthy agent
// running with no peer coordination.
func TestIsProductionMode(t *testing.T) {
	cases := []struct {
		name string
		mut  func(c *config.Config)
		want bool
	}{
		{
			name: "all empty is dev mode",
			mut:  func(*config.Config) {},
			want: false,
		},
		{
			name: "NodeName set is production",
			mut:  func(c *config.Config) { c.NodeName = "node-1" },
			want: true,
		},
		{
			name: "PodName set is production",
			mut:  func(c *config.Config) { c.PodName = "gantry-abc" },
			want: true,
		},
		{
			name: "MembersNamespace set is production",
			mut:  func(c *config.Config) { c.MembersNamespace = "gantry-system" },
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &config.Config{}
			tc.mut(c)
			if got := isProductionMode(c); got != tc.want {
				t.Errorf("isProductionMode = %v, want %v", got, tc.want)
			}
		})
	}
}

// selfAnnounceRequiredForReadiness must be true exactly when the
// agent is in production mode AND has a PodName (so AnnounceSelf
// has something to patch). Static bootstrap peers DO NOT bypass
// the gate: they solve DHT seeding, not the K8s-node-name → libp2p
// peer-ID mapping that ships through the gantry.io/peer-id pod
// annotation. A pod that boots, completes DHT bootstrap via static
// peers, but fails to publish its own peer-id annotation would be
// listed in HRW membership under a node name no other agent can
// translate to a dialable peer ID — every cold-start RPC to it 503s.
func TestSelfAnnounceRequiredForReadiness(t *testing.T) {
	cases := []struct {
		name string
		c    *config.Config
		want bool
	}{
		{
			name: "dev mode never requires self-announce",
			c:    &config.Config{},
			want: false,
		},
		{
			name: "prod mode with PodName requires it",
			c: &config.Config{
				NodeName: "node-1",
				PodName:  "gantry-abc",
			},
			want: true,
		},
		{
			name: "prod mode with static bootstrap peers still requires it (annotations are independent of DHT seeding)",
			c: &config.Config{
				NodeName:             "node-1",
				PodName:              "gantry-abc",
				Libp2pBootstrapPeers: []string{"/ip4/10.0.0.1/tcp/4001/p2p/Qm..."},
			},
			want: true,
		},
		{
			name: "prod mode without PodName cannot self-announce, so no gate",
			c: &config.Config{
				NodeName: "node-1",
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := selfAnnounceRequiredForReadiness(tc.c); got != tc.want {
				t.Errorf("selfAnnounceRequiredForReadiness = %v, want %v", got, tc.want)
			}
		})
	}
}

// bootstrapConvergenceTarget gates "bootstrap converged; ceasing
// periodic dials" on RoutingTableSize ≥ target. A fixed target of 5
// loops forever on small clusters (2-3 nodes) because the routing
// table can never grow that big; for single-node deploys we return
// 0 so the loop exits immediately on the first pass (no peers to
// dial; routing-table will stay empty by definition).
func TestBootstrapConvergenceTarget(t *testing.T) {
	cases := []struct {
		name         string
		snapshotSize int // includes self
		maxSize      int
		want         int
	}{
		{name: "single-node cluster targets 0", snapshotSize: 1, maxSize: 5, want: 0},
		{name: "empty snapshot defensively returns 0", snapshotSize: 0, maxSize: 5, want: 0},
		{name: "2-node cluster targets 1 peer", snapshotSize: 2, maxSize: 5, want: 1},
		{name: "3-node cluster targets 2 peers", snapshotSize: 3, maxSize: 5, want: 2},
		{name: "5-node cluster targets 4 peers", snapshotSize: 5, maxSize: 5, want: 4},
		{name: "6-node cluster caps at max=5", snapshotSize: 6, maxSize: 5, want: 5},
		{name: "100-node cluster caps at max=5", snapshotSize: 100, maxSize: 5, want: 5},
		{name: "custom max=3 caps at 3", snapshotSize: 10, maxSize: 3, want: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bootstrapConvergenceTarget(tc.snapshotSize, tc.maxSize); got != tc.want {
				t.Errorf("bootstrapConvergenceTarget(%d, %d) = %d, want %d",
					tc.snapshotSize, tc.maxSize, got, tc.want)
			}
		})
	}
}

// bootstrapPeerCount must consult SnapshotForBootstrap (Running +
// annotated, regardless of Ready) when the Members implementation
// exposes it, and fall back to Snapshot only for Members
// implementations that don't (the dev-mode single-self fake, test
// stubs). The bootstrap view is the correct denominator for the DHT
// routing-table target and the /readyz DHT-convergence gate during
// a cold rollout where no pod is Ready yet — using the serving view
// there is the readiness-bypass bug this helper exists to prevent.
func TestBootstrapPeerCount(t *testing.T) {
	t.Run("fallback to Snapshot when no SnapshotForBootstrap", func(t *testing.T) {
		// fakes.Members implements ifaces.Members but NOT the
		// bootstrapper extension; verify the fallback branch.
		f := fakes.NewMembers(
			ifaces.NodeID("self"),
			ifaces.Node{ID: "self"},
			ifaces.Node{ID: "peer-a"},
			ifaces.Node{ID: "peer-b"},
		)
		if got := bootstrapPeerCount(f); got != 3 {
			t.Errorf("bootstrapPeerCount(fakes) = %d, want 3 (fallback to Snapshot)", got)
		}
	})
	t.Run("uses SnapshotForBootstrap when available and prefers it over Snapshot", func(t *testing.T) {
		// During a cold rollout Snapshot() may report 0 while
		// SnapshotForBootstrap reports the full announced set.
		// bootstrapPeerCount MUST pick up the bootstrap value or
		// the /readyz DHT-convergence gate gets bypassed.
		m := &bootstrapStub{
			snapshot: nil, // simulate "no Ready peers yet"
			boot: []ifaces.Node{
				{ID: "self"},
				{ID: "peer-a"},
				{ID: "peer-b"},
				{ID: "peer-c"},
			},
		}
		if got := bootstrapPeerCount(m); got != 4 {
			t.Errorf("bootstrapPeerCount(stub) = %d, want 4 (must read SnapshotForBootstrap, not Snapshot)", got)
		}
	})
	t.Run("single-self bootstrap returns 1", func(t *testing.T) {
		m := &bootstrapStub{boot: []ifaces.Node{{ID: "self"}}}
		if got := bootstrapPeerCount(m); got != 1 {
			t.Errorf("bootstrapPeerCount(single-self stub) = %d, want 1", got)
		}
	})
}

// bootstrapStub is a minimal Members implementation that ALSO
// exposes SnapshotForBootstrap and RunningMatchingPodCount, so we
// can exercise the structural type assertions inside
// bootstrapPeerCount and runningMatchingPodCount without dragging
// the full k8s informer plumbing into a unit test.
type bootstrapStub struct {
	snapshot []ifaces.Node
	boot     []ifaces.Node
	running  int
}

func (s *bootstrapStub) Self() ifaces.NodeID                 { return "self" }
func (s *bootstrapStub) Snapshot() []ifaces.Node             { return s.snapshot }
func (s *bootstrapStub) SnapshotForBootstrap() []ifaces.Node { return s.boot }
func (s *bootstrapStub) RunningMatchingPodCount() int        { return s.running }
func (s *bootstrapStub) WaitForSync(_ context.Context) error { return nil }

// runningMatchingPodCount must consult RunningMatchingPodCount
// (Running + PodIP, regardless of Ready or annotation) when the
// Members implementation exposes it, and fall back to Snapshot
// only for implementations that don't (the dev-mode single-self
// fake, test stubs that predate this method). The new counter is
// the strict superset that the /readyz "peer self-announcements
// pending" branch relies on to distinguish "real single-node
// cluster" (count == 1) from "multi-node, peers haven't yet
// published p2p-addrs" (count > 1, bootstrap view ≤ 1). Without
// this superset the existing DHT-empty check would short-circuit
// during the first-rollout window and /readyz would race to green
// before any peer is dialable.
func TestRunningMatchingPodCount(t *testing.T) {
	t.Run("fallback to Snapshot when no RunningMatchingPodCount", func(t *testing.T) {
		// fakes.Members implements ifaces.Members but NOT the
		// runningCounter extension; verify the fallback branch.
		// On the dev-mode path this undercount is acceptable —
		// the readiness gate this helper feeds only triggers when
		// count > 1, and the dev-mode fake is always single-self.
		f := fakes.NewMembers(
			ifaces.NodeID("self"),
			ifaces.Node{ID: "self"},
			ifaces.Node{ID: "peer-a"},
			ifaces.Node{ID: "peer-b"},
		)
		if got := runningMatchingPodCount(f); got != 3 {
			t.Errorf("runningMatchingPodCount(fakes) = %d, want 3 (fallback to Snapshot)", got)
		}
	})
	t.Run("uses RunningMatchingPodCount when available, ignores Snapshot", func(t *testing.T) {
		// Multi-node fresh rollout: every Gantry pod is Running
		// but none has self-announced yet (Snapshot() and
		// SnapshotForBootstrap() both empty for peers).
		// RunningMatchingPodCount must still see all 4 pods so
		// /readyz can stay 503 with "peer self-announcements
		// pending" rather than racing to green.
		m := &bootstrapStub{
			snapshot: nil,
			boot:     nil,
			running:  4,
		}
		if got := runningMatchingPodCount(m); got != 4 {
			t.Errorf("runningMatchingPodCount(stub) = %d, want 4 (must read RunningMatchingPodCount, not Snapshot)", got)
		}
	})
	t.Run("single-node returns 1 (real single-node cluster, not a stuck rollout)", func(t *testing.T) {
		// On a legitimate single-node deploy the new readiness
		// branch must NOT fire — count == 1 falls below the
		// `count > 1` guard. Confirm the helper preserves that
		// invariant.
		m := &bootstrapStub{running: 1}
		if got := runningMatchingPodCount(m); got != 1 {
			t.Errorf("runningMatchingPodCount(single-node stub) = %d, want 1", got)
		}
	})
}

// TestRoutingTableTarget guards eighth-review #3: the kad-dht
// routing-table target is the count of *other* peers we expect to
// learn about — snapshotSize-1 — NOT the raw snapshot size. Passing
// snapshotSize as target meant a fully-converged N-node cluster
// could only ever reach (N-1)/N of the target, pegging the DHT
// health score at < 1.0 (0.5 in a 2-node deploy, 0.66 in a 3-node
// deploy, 0.75 in a 4-node deploy …) and tripping degraded-cluster
// alerts on healthy clusters.
//
// Single-node carve-out: snapshot ≤ 1 → 0. The lone-agent case
// must not produce a positive target (the routing table has nothing
// to learn) and the bootstrap loop already encodes that contract via
// bootstrapConvergenceTarget; routingTableTarget agrees.
func TestRoutingTableTarget(t *testing.T) {
	cases := []struct {
		name         string
		snapshotSize int
		maxSize      int
		want         int
	}{
		{"empty snapshot returns 0", 0, 256, 0},
		{"single-self snapshot returns 0 (lone-agent carve-out)", 1, 256, 0},
		{"2-node cluster expects 1 peer in routing table", 2, 256, 1},
		{"3-node cluster expects 2 peers in routing table", 3, 256, 2},
		{"10-node cluster expects 9 peers in routing table", 10, 256, 9},
		{"snapshot exactly at cap returns cap (snapshot-1 = max)", 257, 256, 256},
		{"snapshot above cap clamps to cap", 1000, 256, 256},
		{"small max applies (snapshot-1 > max)", 100, 4, 4},
		{"small max with snapshot just over: snapshot-1 ≤ max returns snapshot-1", 5, 4, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := routingTableTarget(tc.snapshotSize, tc.maxSize); got != tc.want {
				t.Errorf("routingTableTarget(snapshotSize=%d, maxSize=%d) = %d; want %d",
					tc.snapshotSize, tc.maxSize, got, tc.want)
			}
		})
	}
}
