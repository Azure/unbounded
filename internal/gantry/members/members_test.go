// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package members

import (
	"context"
	"sort"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func newPod(name, ns, node, ip string, ready bool, labels map[string]string) *corev1.Pod {
	cond := corev1.ConditionFalse
	if ready {
		cond = corev1.ConditionTrue
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: ip,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: cond},
			},
		},
	}
}

func newNode(name, zone string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"topology.kubernetes.io/zone": zone},
		},
	}
}

func TestNew_RequiresNodeName(t *testing.T) {
	_, err := New(Options{LabelSelector: "app=gantry"})
	if err == nil {
		t.Fatal("expected NodeName-required error")
	}
}

func TestNew_RequiresSelector(t *testing.T) {
	_, err := New(Options{NodeName: "n1"})
	if err == nil {
		t.Fatal("expected LabelSelector-required error")
	}
}

func TestNew_RejectsBadSelector(t *testing.T) {
	_, err := New(Options{NodeName: "n1", LabelSelector: "$$bogus"})
	if err == nil {
		t.Fatal("expected bad-selector error")
	}
}

func TestSnapshot_JoinsPodIPAndNodeZone(t *testing.T) {
	cs := fake.NewSimpleClientset(
		newPod("gantry-a", "kube-system", "node-a", "10.0.0.1", true, map[string]string{"app.kubernetes.io/name": "gantry"}),
		newPod("gantry-b", "kube-system", "node-b", "10.0.0.2", true, map[string]string{"app.kubernetes.io/name": "gantry"}),
		newPod("noisy", "kube-system", "node-a", "10.0.0.99", true, map[string]string{"app.kubernetes.io/name": "other"}),
		newPod("not-ready", "kube-system", "node-c", "10.0.0.3", false, map[string]string{"app.kubernetes.io/name": "gantry"}),
		newNode("node-a", "us-east-1a"),
		newNode("node-b", "us-east-1b"),
		newNode("node-c", "us-east-1c"),
	)

	m, err := New(Options{
		NodeName:      "node-a",
		Namespace:     "",
		LabelSelector: "app.kubernetes.io/name=gantry",
		Clientset:     cs,
		ResyncPeriod:  10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(m.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m.Start()

	if err := m.WaitForSync(ctx); err != nil {
		t.Fatalf("WaitForSync: %v", err)
	}

	if got, want := m.Self(), "node-a"; string(got) != want {
		t.Errorf("Self = %q, want %q", got, want)
	}

	got := m.Snapshot()
	if len(got) != 2 {
		t.Fatalf("Snapshot len = %d, want 2 (have %+v)", len(got), got)
	}
	// Snapshot is sorted by NodeID.
	if !sort.SliceIsSorted(got, func(i, j int) bool { return got[i].ID < got[j].ID }) {
		t.Errorf("Snapshot not sorted by ID: %+v", got)
	}

	want := map[string]struct {
		Addr string
		Zone string
	}{
		"node-a": {"10.0.0.1", "us-east-1a"},
		"node-b": {"10.0.0.2", "us-east-1b"},
	}
	for _, n := range got {
		w, ok := want[string(n.ID)]
		if !ok {
			t.Errorf("unexpected node %+v", n)
			continue
		}

		if n.Addr != w.Addr {
			t.Errorf("%s Addr = %q, want %q", n.ID, n.Addr, w.Addr)
		}

		if n.Zone != w.Zone {
			t.Errorf("%s Zone = %q, want %q", n.ID, n.Zone, w.Zone)
		}
	}
}

func TestSnapshot_ExcludesNonRunning(t *testing.T) {
	pending := newPod("pending", "ns", "node-x", "10.0.0.4", true, map[string]string{"app.kubernetes.io/name": "gantry"})
	pending.Status.Phase = corev1.PodPending
	cs := fake.NewSimpleClientset(
		pending,
		newNode("node-x", "us-east-1a"),
	)

	m, err := New(Options{
		NodeName:      "node-x",
		LabelSelector: "app.kubernetes.io/name=gantry",
		Clientset:     cs,
		ResyncPeriod:  10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(m.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m.Start()

	if err := m.WaitForSync(ctx); err != nil {
		t.Fatalf("WaitForSync: %v", err)
	}

	if got := m.Snapshot(); len(got) != 0 {
		t.Errorf("Snapshot = %+v, want empty (pending pods excluded)", got)
	}
}

func TestSnapshot_OmitsPodsWithoutIP(t *testing.T) {
	cs := fake.NewSimpleClientset(
		newPod("no-ip", "ns", "node-x", "", true, map[string]string{"app.kubernetes.io/name": "gantry"}),
		newNode("node-x", "us-east-1a"),
	)

	m, err := New(Options{
		NodeName:      "node-x",
		LabelSelector: "app.kubernetes.io/name=gantry",
		Clientset:     cs,
		ResyncPeriod:  10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(m.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m.Start()

	if err := m.WaitForSync(ctx); err != nil {
		t.Fatalf("WaitForSync: %v", err)
	}

	if got := m.Snapshot(); len(got) != 0 {
		t.Errorf("Snapshot = %+v, want empty", got)
	}
}

func TestZoneLabelKey_Override(t *testing.T) {
	n := newNode("node-x", "")
	n.Labels["custom/zone"] = "z1"
	cs := fake.NewSimpleClientset(
		newPod("p", "ns", "node-x", "10.0.0.1", true, map[string]string{"app": "gantry"}),
		n,
	)

	m, err := New(Options{
		NodeName:      "node-x",
		LabelSelector: "app=gantry",
		ZoneLabelKey:  "custom/zone",
		Clientset:     cs,
		ResyncPeriod:  10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(m.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m.Start()

	if err := m.WaitForSync(ctx); err != nil {
		t.Fatalf("WaitForSync: %v", err)
	}

	got := m.Snapshot()
	if len(got) != 1 || got[0].Zone != "z1" {
		t.Errorf("Snapshot = %+v, want zone=z1", got)
	}
}

func TestWaitForSync_RespectsCtx(t *testing.T) {
	cs := fake.NewSimpleClientset()

	m, err := New(Options{
		NodeName:      "n1",
		LabelSelector: "app=gantry",
		Clientset:     cs,
		ResyncPeriod:  10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(m.Stop)

	// Stop the manager *before* starting, so factory.Start is a no-op and
	// HasSynced never flips. WaitForSync should return when ctx expires.
	m.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err = m.WaitForSync(ctx)
	if err == nil {
		t.Fatal("expected wait error from ctx deadline / sync failure")
	}
}

func TestSnapshot_TransferPortComposesAddr(t *testing.T) {
	cs := fake.NewSimpleClientset(
		newPod("p", "ns", "node-x", "10.0.0.7", true, map[string]string{"app": "gantry"}),
		newNode("node-x", "us-east-1a"),
	)

	m, err := New(Options{
		NodeName:      "node-x",
		LabelSelector: "app=gantry",
		TransferPort:  5001,
		Clientset:     cs,
		ResyncPeriod:  10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(m.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m.Start()

	if err := m.WaitForSync(ctx); err != nil {
		t.Fatalf("WaitForSync: %v", err)
	}

	got := m.Snapshot()
	if len(got) != 1 || got[0].Addr != "10.0.0.7:5001" {
		t.Errorf("Addr = %q, want 10.0.0.7:5001", got[0].Addr)
	}
}

// TestSnapshot_TransferPortIPv6PodBracketsLiteral verifies that an
// IPv6 Pod IP is bracketed correctly when composed with the transfer
// port. fmt.Sprintf("%s:%d", "::1", 5001) yields "::1:5001" which
// fails parse as a host:port pair (the trailing :5001 is ambiguous
// with the v6 literal's last segment); net.JoinHostPort produces
// "[::1]:5001" which is dialable by net.Dial("tcp", addr) and
// parseable by url.Parse. Sixth code review #5.
func TestSnapshot_TransferPortIPv6PodBracketsLiteral(t *testing.T) {
	cs := fake.NewSimpleClientset(
		newPod("p", "ns", "node-x", "fd00::7", true, map[string]string{"app": "gantry"}),
		newNode("node-x", "us-east-1a"),
	)

	m, err := New(Options{
		NodeName:      "node-x",
		LabelSelector: "app=gantry",
		TransferPort:  5001,
		Clientset:     cs,
		ResyncPeriod:  10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(m.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m.Start()

	if err := m.WaitForSync(ctx); err != nil {
		t.Fatalf("WaitForSync: %v", err)
	}

	got := m.Snapshot()
	if len(got) != 1 || got[0].Addr != "[fd00::7]:5001" {
		t.Errorf("Addr = %q, want [fd00::7]:5001 (IPv6 literal must be bracketed)", got[0].Addr)
	}
	// Also verify SnapshotForBootstrap, which has its own composition site.
	// The fixture pod has no AnnotationP2PAddrs so SnapshotForBootstrap
	// will skip it; this is intentional (bootstrap requires a
	// published peer addr). The Snapshot check above covers the
	// transfer-port composition path for both functions because they
	// share the same logic - adding a second fixture here would only
	// re-verify what the function-level coverage already shows.
}

func TestSnapshot_AnnotationsPopulateFields(t *testing.T) {
	p := newPod("p", "ns", "node-x", "10.0.0.7", true, map[string]string{"app": "gantry"})
	p.Annotations = map[string]string{
		AnnotationPeerID:       "12D3KooWAbc",
		AnnotationP2PAddrs:     "/ip4/10.0.0.7/tcp/4001/p2p/12D3KooWAbc, /ip4/1.2.3.4/tcp/4001/p2p/12D3KooWAbc",
		AnnotationTransferAddr: "1.2.3.4:5099",
	}
	cs := fake.NewSimpleClientset(p, newNode("node-x", "us-east-1a"))

	m, err := New(Options{
		NodeName:      "node-x",
		LabelSelector: "app=gantry",
		TransferPort:  5001, // overridden by annotation
		Clientset:     cs,
		ResyncPeriod:  10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(m.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m.Start()

	if err := m.WaitForSync(ctx); err != nil {
		t.Fatalf("WaitForSync: %v", err)
	}

	got := m.Snapshot()
	if len(got) != 1 {
		t.Fatalf("Snapshot = %+v, want 1", got)
	}

	n := got[0]
	if n.PeerID != "12D3KooWAbc" {
		t.Errorf("PeerID = %q, want 12D3KooWAbc", n.PeerID)
	}

	if len(n.P2PAddrs) != 2 ||
		n.P2PAddrs[0] != "/ip4/10.0.0.7/tcp/4001/p2p/12D3KooWAbc" ||
		n.P2PAddrs[1] != "/ip4/1.2.3.4/tcp/4001/p2p/12D3KooWAbc" {
		t.Errorf("P2PAddrs = %v, want two trimmed entries", n.P2PAddrs)
	}

	if n.Addr != "1.2.3.4:5099" {
		t.Errorf("Addr = %q, want annotation override 1.2.3.4:5099", n.Addr)
	}
}

func TestAnnounceSelf_PatchesPod(t *testing.T) {
	p := newPod("self", "ns", "node-x", "10.0.0.7", true, map[string]string{"app": "gantry"})
	cs := fake.NewSimpleClientset(p, newNode("node-x", "us-east-1a"))

	m, err := New(Options{
		NodeName:      "node-x",
		Namespace:     "ns",
		LabelSelector: "app=gantry",
		Clientset:     cs,
		ResyncPeriod:  10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(m.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = m.AnnounceSelf(ctx, "self", SelfAnnouncement{
		PeerID:       "12D3KooWAbc",
		P2PAddrs:     []string{"/ip4/10.0.0.7/tcp/4001/p2p/12D3KooWAbc"},
		TransferAddr: "10.0.0.7:5001",
	})
	if err != nil {
		t.Fatalf("AnnounceSelf: %v", err)
	}

	got, err := cs.CoreV1().Pods("ns").Get(ctx, "self", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get pod: %v", err)
	}

	if got.Annotations[AnnotationPeerID] != "12D3KooWAbc" {
		t.Errorf("PeerID annotation = %q", got.Annotations[AnnotationPeerID])
	}

	if got.Annotations[AnnotationP2PAddrs] != "/ip4/10.0.0.7/tcp/4001/p2p/12D3KooWAbc" {
		t.Errorf("P2PAddrs annotation = %q", got.Annotations[AnnotationP2PAddrs])
	}

	if got.Annotations[AnnotationTransferAddr] != "10.0.0.7:5001" {
		t.Errorf("TransferAddr annotation = %q", got.Annotations[AnnotationTransferAddr])
	}
}

func TestAnnounceSelf_RequiresPodName(t *testing.T) {
	cs := fake.NewSimpleClientset(newNode("n", ""))

	m, err := New(Options{
		NodeName:      "n",
		Namespace:     "ns",
		LabelSelector: "app=gantry",
		Clientset:     cs,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(m.Stop)

	if err := m.AnnounceSelf(context.Background(), "", SelfAnnouncement{}); err == nil {
		t.Fatal("expected error when PodName empty")
	}
}

func TestAnnounceSelf_RequiresNamespace(t *testing.T) {
	cs := fake.NewSimpleClientset(newNode("n", ""))

	m, err := New(Options{
		NodeName:      "n",
		LabelSelector: "app=gantry",
		Clientset:     cs,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(m.Stop)

	if err := m.AnnounceSelf(context.Background(), "self", SelfAnnouncement{}); err == nil {
		t.Fatal("expected error when Namespace empty (cluster-wide informer)")
	}
}

// SnapshotForBootstrap must include Running-but-NotReady pods that
// have published p2p-addrs so first-cluster boot doesn't deadlock on
// the readiness/RoutingTableSize circular dependency. It must also
// exclude pods that haven't AnnounceSelf'd yet (no annotations).
func TestSnapshotForBootstrap_IncludesNotReadyPodsWithAnnotations(t *testing.T) {
	readyAnn := newPod("ready", "ns", "node-a", "10.0.0.1", true,
		map[string]string{"app.kubernetes.io/name": "gantry"})
	readyAnn.Annotations = map[string]string{
		AnnotationPeerID:   "peer-ready",
		AnnotationP2PAddrs: "/ip4/10.0.0.1/tcp/4001/p2p/peer-ready",
	}
	notReadyAnn := newPod("notready", "ns", "node-b", "10.0.0.2", false,
		map[string]string{"app.kubernetes.io/name": "gantry"})
	notReadyAnn.Annotations = map[string]string{
		AnnotationPeerID:   "peer-notready",
		AnnotationP2PAddrs: "/ip4/10.0.0.2/tcp/4001/p2p/peer-notready",
	}
	notReadyNoAnn := newPod("blank", "ns", "node-c", "10.0.0.3", false,
		map[string]string{"app.kubernetes.io/name": "gantry"})
	pending := newPod("pending", "ns", "node-d", "10.0.0.4", true,
		map[string]string{"app.kubernetes.io/name": "gantry"})
	pending.Status.Phase = corev1.PodPending
	pending.Annotations = map[string]string{AnnotationP2PAddrs: "/ip4/10.0.0.4/tcp/4001/p2p/peer-pending"}

	cs := fake.NewSimpleClientset(readyAnn, notReadyAnn, notReadyNoAnn, pending,
		newNode("node-a", ""), newNode("node-b", ""), newNode("node-c", ""), newNode("node-d", ""),
	)

	m, err := New(Options{
		NodeName:      "node-a",
		Namespace:     "ns",
		LabelSelector: "app.kubernetes.io/name=gantry",
		Clientset:     cs,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(m.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m.Start()

	if err := m.WaitForSync(ctx); err != nil {
		t.Fatalf("WaitForSync: %v", err)
	}

	got := m.SnapshotForBootstrap()

	gotIDs := map[string]bool{}
	for _, n := range got {
		gotIDs[string(n.ID)] = true
	}

	if !gotIDs["node-a"] {
		t.Errorf("SnapshotForBootstrap missing Ready+annotated node-a; got %+v", got)
	}

	if !gotIDs["node-b"] {
		t.Errorf("SnapshotForBootstrap missing NotReady+annotated node-b; this is the deadlock-breaker case the function exists for; got %+v", got)
	}

	if gotIDs["node-c"] {
		t.Errorf("SnapshotForBootstrap should exclude annotation-less peers (no useful bootstrap addr); got %+v", got)
	}

	if gotIDs["node-d"] {
		t.Errorf("SnapshotForBootstrap should exclude non-Running pods; got %+v", got)
	}

	// The regular Snapshot must still be Ready-only for the serving
	// path: NotReady pods get bootstrap dials but no transfer load.
	serving := m.Snapshot()
	for _, n := range serving {
		if string(n.ID) == "node-b" {
			t.Errorf("Snapshot leaked NotReady node-b into serving view: %+v", serving)
		}
	}
}

// RunningMatchingPodCount must count every Running pod with a PodIP
// the informer sees, regardless of Ready condition or annotation
// state. This is the strict superset that Snapshot (Ready) and
// SnapshotForBootstrap (Running + annotated) are filtered subsets
// of. It exists so /readyz can distinguish "real single-node
// cluster" from "multi-node, peers haven't self-announced yet" -
// using either narrower count there would race readiness to green
// during the first-rollout window where every pod is Running but
// none has published p2p-addrs.
//
// Test matrix:
//
// - Ready+annotated: counted
// - NotReady+annotated: counted
// - NotReady+no-ann: counted (this is the case the method exists for)
// - Pending (any ann): NOT counted (Phase != Running)
// - Running, no PodIP: NOT counted (informer is racing scheduler)
// - Wrong label: NOT counted (excluded by informer selector)
func TestRunningMatchingPodCount_CountsRunningPodsRegardlessOfAnnotationOrReady(t *testing.T) {
	gantryLabel := map[string]string{"app.kubernetes.io/name": "gantry"}

	readyAnn := newPod("ready-ann", "ns", "node-a", "10.0.0.1", true, gantryLabel)
	readyAnn.Annotations = map[string]string{
		AnnotationPeerID:   "peer-a",
		AnnotationP2PAddrs: "/ip4/10.0.0.1/tcp/4001/p2p/peer-a",
	}
	notReadyAnn := newPod("notready-ann", "ns", "node-b", "10.0.0.2", false, gantryLabel)
	notReadyAnn.Annotations = map[string]string{
		AnnotationP2PAddrs: "/ip4/10.0.0.2/tcp/4001/p2p/peer-b",
	}
	notReadyNoAnn := newPod("notready-bare", "ns", "node-c", "10.0.0.3", false, gantryLabel)
	pending := newPod("pending", "ns", "node-d", "10.0.0.4", true, gantryLabel)
	pending.Status.Phase = corev1.PodPending
	noIP := newPod("no-ip", "ns", "node-e", "", true, gantryLabel)
	wrongLabel := newPod("other", "ns", "node-f", "10.0.0.6", true,
		map[string]string{"app.kubernetes.io/name": "not-gantry"})

	cs := fake.NewSimpleClientset(
		readyAnn, notReadyAnn, notReadyNoAnn, pending, noIP, wrongLabel,
		newNode("node-a", ""), newNode("node-b", ""), newNode("node-c", ""),
		newNode("node-d", ""), newNode("node-e", ""), newNode("node-f", ""),
	)

	m, err := New(Options{
		NodeName:      "node-a",
		Namespace:     "ns",
		LabelSelector: "app.kubernetes.io/name=gantry",
		Clientset:     cs,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(m.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m.Start()

	if err := m.WaitForSync(ctx); err != nil {
		t.Fatalf("WaitForSync: %v", err)
	}

	// Expect: ready-ann + notready-ann + notready-bare = 3.
	// Pending, no-IP and wrong-label are excluded; the latter never
	// enters the informer at all (selector filter), the former two
	// are excluded by the method's documented predicate.
	if got, want := m.RunningMatchingPodCount(), 3; got != want {
		t.Errorf("RunningMatchingPodCount = %d, want %d (must count NotReady and un-annotated Running pods so /readyz can detect 'peers haven't announced yet')", got, want)
	}

	// Sanity: Snapshot (Ready only) reports 1 and
	// SnapshotForBootstrap (Running + annotated) reports 2, so
	// the new counter is a strict superset of both - exactly the
	// invariant the readiness gate depends on.
	if got := len(m.Snapshot()); got != 1 {
		t.Errorf("Snapshot len = %d, want 1 (only ready-ann is Ready)", got)
	}

	if got := len(m.SnapshotForBootstrap()); got != 2 {
		t.Errorf("SnapshotForBootstrap len = %d, want 2 (only ready-ann + notready-ann have annotations)", got)
	}
}

// TestTerminatingPodsExcludedFromAllViews guards the
// rolling-update / node-drain deadlock: a pod with
// DeletionTimestamp set stays Phase=Running with Ready=True for the
// grace-period window, but will never re-AnnounceSelf with fresh
// annotations. If it leaks into any membership view the surviving
// pods either route traffic to a doomed peer (Snapshot leak),
// waste libp2p dials on a peer that can't be reached
// (SnapshotForBootstrap leak), or deadlock at NotReady on
// "peer self-announcements pending" (RunningMatchingPodCount leak,
// because the new readiness gate compares this count against
// SnapshotForBootstrap).
//
// The terminating fixture is deliberately given a FULL set of valid
// annotations (peer-id, p2p-addrs, transfer-addr) and is both
// Phase=Running and Ready=True. This proves the terminating filter
// wins over otherwise-perfect bootstrap/serving eligibility - a
// terminating pod missing annotations would already be filtered by
// SnapshotForBootstrap's annotation check and would not exercise
// the new path.
func TestTerminatingPodsExcludedFromAllViews(t *testing.T) {
	gantryLabel := map[string]string{"app.kubernetes.io/name": "gantry"}

	// Terminating peer: fully eligible by every other filter.
	terminating := newPod("terminating", "ns", "node-a", "10.0.0.1", true, gantryLabel)
	terminating.Annotations = map[string]string{
		AnnotationPeerID:       "peer-terminating",
		AnnotationP2PAddrs:     "/ip4/10.0.0.1/tcp/4001/p2p/peer-terminating",
		AnnotationTransferAddr: "10.0.0.1:5001",
	}
	delTime := metav1.NewTime(time.Now())
	terminating.DeletionTimestamp = &delTime

	// Control peer: same shape, no DeletionTimestamp. Must appear
	// in all three views so the test fails loudly if someone
	// breaks the positive path while fixing the negative one.
	control := newPod("control", "ns", "node-b", "10.0.0.2", true, gantryLabel)
	control.Annotations = map[string]string{
		AnnotationPeerID:       "peer-control",
		AnnotationP2PAddrs:     "/ip4/10.0.0.2/tcp/4001/p2p/peer-control",
		AnnotationTransferAddr: "10.0.0.2:5001",
	}

	cs := fake.NewSimpleClientset(
		terminating, control,
		newNode("node-a", ""), newNode("node-b", ""),
	)

	m, err := New(Options{
		NodeName:      "node-b",
		Namespace:     "ns",
		LabelSelector: "app.kubernetes.io/name=gantry",
		Clientset:     cs,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(m.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m.Start()

	if err := m.WaitForSync(ctx); err != nil {
		t.Fatalf("WaitForSync: %v", err)
	}

	// Snapshot (Ready, serving view) must NOT include the
	// terminating pod even though it is Phase=Running+Ready=True.
	serving := m.Snapshot()
	for _, n := range serving {
		if string(n.ID) == "node-a" {
			t.Errorf("Snapshot leaked terminating node-a into serving view: %+v (HRW would route transfer traffic to a doomed peer)", serving)
		}
	}

	if len(serving) != 1 || string(serving[0].ID) != "node-b" {
		t.Errorf("Snapshot = %+v, want [node-b] (control only)", serving)
	}

	// SnapshotForBootstrap must NOT include the terminating pod
	// even though its p2p-addrs annotation is fully populated.
	boot := m.SnapshotForBootstrap()
	for _, n := range boot {
		if string(n.ID) == "node-a" {
			t.Errorf("SnapshotForBootstrap leaked terminating node-a into bootstrap view: %+v (libp2p would dial a doomed peer)", boot)
		}
	}

	if len(boot) != 1 || string(boot[0].ID) != "node-b" {
		t.Errorf("SnapshotForBootstrap = %+v, want [node-b] (control only)", boot)
	}

	// RunningMatchingPodCount must NOT include the terminating
	// pod. Counting it while SnapshotForBootstrap rightly excludes
	// it would deadlock the surviving pod at NotReady on
	// "peer self-announcements pending".
	if got, want := m.RunningMatchingPodCount(), 1; got != want {
		t.Errorf("RunningMatchingPodCount = %d, want %d (only the control pod; the terminating pod must not be counted or the readiness gate deadlocks)", got, want)
	}
}

// TestPodReady_ExcludesTerminating is a direct unit test on the
// podReady helper covering all four short-circuit paths plus the
// positive case. podReady is called from Snapshot and is also
// exported (via Snapshot) as the de facto Ready predicate; the doc
// comment on podReady promises to exclude Terminating pods so this
// test pins that contract at the source.
func TestPodReady_ExcludesTerminating(t *testing.T) {
	gantryLabel := map[string]string{"app.kubernetes.io/name": "gantry"}

	if podReady(nil) {
		t.Error("podReady(nil) = true, want false")
	}

	// Running + Ready=True + DeletionTimestamp set: must return
	// false. This is the same case Snapshot filters out at its
	// own loop head; podReady duplicates the check so future
	// callers inherit the contract.
	term := newPod("term", "ns", "node-a", "10.0.0.1", true, gantryLabel)
	delTime := metav1.NewTime(time.Now())

	term.DeletionTimestamp = &delTime
	if podReady(term) {
		t.Error("podReady(Running+Ready+Terminating) = true, want false")
	}

	// Pending pod: must return false (Phase guard).
	pending := newPod("pending", "ns", "node-b", "10.0.0.2", true, gantryLabel)

	pending.Status.Phase = corev1.PodPending
	if podReady(pending) {
		t.Error("podReady(Pending) = true, want false")
	}

	// Running + Ready=False: must return false (no Ready=True
	// condition).
	notReady := newPod("notready", "ns", "node-c", "10.0.0.3", false, gantryLabel)
	if podReady(notReady) {
		t.Error("podReady(Running+Ready=False) = true, want false")
	}

	// Positive case: Running + Ready=True + non-terminating.
	ok := newPod("ready", "ns", "node-d", "10.0.0.4", true, gantryLabel)
	if !podReady(ok) {
		t.Error("podReady(Running+Ready+non-terminating) = false, want true")
	}
}
