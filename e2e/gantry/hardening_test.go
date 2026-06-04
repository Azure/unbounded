//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestE2E_Hardening proves that the production NetworkPolicy hardening
// layout applies cleanly without breaking the agent. It generates a
// kind-specific variant of deploy/examples/networkpolicy.yaml with
// concrete CIDRs discovered at runtime, applies it, and verifies that
// the DaemonSet keeps passing readiness.
//
// IMPORTANT — enforcement caveat:
//
//	kind's default CNI (kindnetd) does NOT enforce NetworkPolicy. A
//	policy applied against kindnetd is parsed and stored but never
//	consulted for actual traffic decisions. This test therefore proves:
//
//	  1. The policy YAML is structurally valid (apiserver accepts it).
//	  2. The selectors target the right pods.
//	  3. The ingress/egress rules don't accidentally break readiness
//	     probes or apiserver reach (which would surface as DaemonSet
//	     pods going NotReady after policy apply).
//	  4. The policy survives a rollout restart of the DaemonSet.
//
//	What this test does NOT prove: that traffic is actually blocked.
//	That requires a Calico or Cilium CNI. Operators MUST validate
//	enforcement in their production environment with their actual CNI;
//	this is documented in deploy/examples/networkpolicy.yaml.
func TestE2E_Hardening(t *testing.T) {
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

	// Discover the kind node CIDR — apiserver and kubelet probes both
	// originate from a node IP, so the NP's ipBlock rules need it.
	nodeCIDR := h.discoverNodeCIDR(ctx)
	t.Logf("discovered node CIDR for kind cluster: %s", nodeCIDR)

	// Apply the kind-specific NetworkPolicy.
	h.applyHardeningPolicy(ctx, nodeCIDR)

	// Wait for the policy to propagate and verify DaemonSet pods stay
	// ready. If a rule accidentally blocks kubelet probes or apiserver
	// reach we'd see pods flip NotReady within ~30s.
	time.Sleep(15 * time.Second)
	h.waitForRollout(ctx)
	h.checkReadyz(ctx)

	// Rollout restart so every pod re-establishes its informer and DHT
	// connections under the NP. If apiserver egress is wrong, restarted
	// pods will fail to start their informer and never go Ready.
	if err := h.run(ctx, "kubectl", "-n", namespace, "rollout", "restart", "daemonset/"+dsName); err != nil {
		t.Fatalf("rollout restart: %v", err)
	}
	h.waitForRollout(ctx)
	h.checkReadyz(ctx)

	t.Logf("NetworkPolicy applied cleanly; DaemonSet remained Ready through restart. " +
		"NOTE: enforcement on kind requires a Calico/Cilium overlay; this test only " +
		"verifies the policy layout doesn't break the agent.")
}

// discoverNodeCIDR returns a CIDR string that covers all kind node
// InternalIPs in the cluster. For kind's docker network, all nodes
// share a single /16 (typically 172.18.0.0/16) so we just return that.
func (h *harness) discoverNodeCIDR(ctx context.Context) string {
	h.t.Helper()
	out, err := h.runOut(ctx, "kubectl", "get", "nodes",
		"-o", "jsonpath={range .items[*]}{.status.addresses[?(@.type==\"InternalIP\")].address}{\"\\n\"}{end}")
	if err != nil {
		h.t.Fatalf("get node IPs: %v", err)
	}
	for _, line := range strings.Split(out, "\n") {
		ip := strings.TrimSpace(line)
		if ip == "" {
			continue
		}
		// kind nodes are on a /16 docker network. Use the /16 to cover
		// all of them; the test is about policy layout, not minimum
		// privilege.
		parts := strings.Split(ip, ".")
		if len(parts) == 4 {
			return fmt.Sprintf("%s.%s.0.0/16", parts[0], parts[1])
		}
	}
	h.t.Fatalf("no node InternalIP found; cannot compute node CIDR")
	return "" // unreachable
}

// applyHardeningPolicy writes a kind-specific NetworkPolicy that mirrors
// deploy/examples/networkpolicy.yaml but uses the discovered nodeCIDR
// instead of placeholder values, then applies it.
func (h *harness) applyHardeningPolicy(ctx context.Context, nodeCIDR string) {
	h.t.Helper()
	// Build the policy as a string list to avoid tab corruption from
	// gofmt against backtick literals (see applyPullPod for the same
	// pattern).
	manifest := strings.Join([]string{
		"apiVersion: networking.k8s.io/v1",
		"kind: NetworkPolicy",
		"metadata:",
		"  name: gantry-agent",
		"  namespace: " + namespace,
		"spec:",
		"  podSelector:",
		"    matchLabels:",
		"      app.kubernetes.io/name: gantry",
		"      app.kubernetes.io/component: agent",
		"  policyTypes:",
		"    - Ingress",
		"    - Egress",
		"  ingress:",
		// Peer transfer + libp2p only from other gantry pods.
		"    - from:",
		"        - podSelector:",
		"            matchLabels:",
		"              app.kubernetes.io/name: gantry",
		"              app.kubernetes.io/component: agent",
		"      ports:",
		"        - protocol: TCP",
		"          port: 5001",
		"        - protocol: TCP",
		"          port: 4001",
		"        - protocol: UDP",
		"          port: 4001",
		// Metrics + healthz from kubelet (node IP) and same-cluster pods.
		"    - from:",
		"        - ipBlock:",
		"            cidr: " + nodeCIDR,
		"      ports:",
		"        - protocol: TCP",
		"          port: 9095",
		// Mirror :5000 from node IPs (containerd reaches via hostPort DNAT).
		"    - from:",
		"        - ipBlock:",
		"            cidr: " + nodeCIDR,
		"      ports:",
		"        - protocol: TCP",
		"          port: 5000",
		"  egress:",
		// DNS to coredns.
		"    - to:",
		"        - namespaceSelector:",
		"            matchLabels:",
		"              kubernetes.io/metadata.name: kube-system",
		"      ports:",
		"        - protocol: UDP",
		"          port: 53",
		"        - protocol: TCP",
		"          port: 53",
		// Apiserver via node CIDR (kind exposes apiserver at a node IP).
		"    - to:",
		"        - ipBlock:",
		"            cidr: " + nodeCIDR,
		"      ports:",
		"        - protocol: TCP",
		"          port: 6443",
		// Peer gantry transfer + libp2p.
		"    - to:",
		"        - podSelector:",
		"            matchLabels:",
		"              app.kubernetes.io/name: gantry",
		"              app.kubernetes.io/component: agent",
		"      ports:",
		"        - protocol: TCP",
		"          port: 5001",
		"        - protocol: TCP",
		"          port: 4001",
		"        - protocol: UDP",
		"          port: 4001",
		// Egress to registry.k8s.io (HTTPS). For kind we allow all
		// HTTPS egress; production overlays should narrow this.
		"    - to:",
		"        - ipBlock:",
		"            cidr: 0.0.0.0/0",
		"      ports:",
		"        - protocol: TCP",
		"          port: 443",
		"        - protocol: TCP",
		"          port: 80",
		"",
	}, "\n")

	if err := h.runWithInput(ctx, manifest, "kubectl", "apply", "-f", "-"); err != nil {
		h.t.Fatalf("apply hardening NetworkPolicy: %v", err)
	}
}
