// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package members provides Gantry's cluster-membership view, sourced from a
// Kubernetes Pod informer.
//
// Design :
//
// - A label-selected Pod informer enumerates Gantry DaemonSet pods. The
// selector is operator-configurable (`members_label_selector`) so this
// package does not assume a particular DaemonSet manifest.
// - Each agent reads its own Node once when zone-scoped HRW is enabled and
// publishes the zone through its Pod announcement. This avoids a full
// cluster-scoped Node informer per agent.
// - `Self` is set from the Downward API (`spec.nodeName` ->
// `GANTRY_NODE_NAME`); this is the stable identifier HRW uses to score
// digests.
//
// Failure modes:
//
// - In-cluster: a missing/invalid service-account token fails fast at
// New; the agent cannot run without a Kubernetes API view.
// - Out-of-cluster: explicit `members_kubeconfig` is honored; this path
// is the developer/test path, not production.
//
// Tests use `client-go/kubernetes/fake` via the Options.Clientset escape
// hatch so the package does not require a real cluster.
package members

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

// Options configures a Manager.
type Options struct {
	// NodeName is the Kubernetes node this agent runs on. Required - used
	// as Self and for the optional one-time zone lookup.
	NodeName string

	// Namespace restricts the pod informer; empty means cluster-wide.
	Namespace string

	// LabelSelector is the K8s label selector identifying peer agents.
	// Required (empty selector would enrol every pod in the cluster).
	LabelSelector string

	// ZoneLabelKey is the node label that exposes the topology zone.
	// Defaults to `topology.kubernetes.io/zone` if empty.
	ZoneLabelKey string

	// Kubeconfig is the path to an out-of-cluster kubeconfig. Empty means
	// in-cluster service-account discovery.
	Kubeconfig string

	// Clientset is an optional pre-built clientset (typically the fake
	// clientset in tests). When non-nil, Kubeconfig is ignored.
	Clientset kubernetes.Interface

	// ResyncPeriod is the informer resync interval. Zero means "no resync"
	// (rely on watch events). Default 30s when zero.
	ResyncPeriod time.Duration

	// TransferPort is the TCP port each agent's transfer endpoint
	// listens on (the design doc). When non-zero, Snapshot fills Node.Addr as
	// "podIP:TransferPort"; when zero, Snapshot returns the bare pod
	// IP (back-compat). Production deployments MUST set this so
	// peer-fetch URLs are reachable.
	TransferPort int
}

// Manager owns the Pod informer and exposes ifaces.Members.
type Manager struct {
	self         ifaces.NodeID
	selfZone     string
	zoneLabelKey string
	selector     labels.Selector
	transferPort int
	clientset    kubernetes.Interface
	namespace    string

	podFactory informers.SharedInformerFactory
	podInf     cache.SharedIndexInformer

	stopCh chan struct{}
	once   sync.Once
}

// New builds a Manager. The factory is constructed but informers are not
// started until Start is called.
func New(opts Options) (*Manager, error) {
	if opts.NodeName == "" {
		return nil, errors.New("members: NodeName required (set via spec.nodeName / GANTRY_NODE_NAME)")
	}

	if opts.LabelSelector == "" {
		return nil, errors.New("members: LabelSelector required")
	}

	sel, err := labels.Parse(opts.LabelSelector)
	if err != nil {
		return nil, fmt.Errorf("members: parse selector %q: %w", opts.LabelSelector, err)
	}

	cs := opts.Clientset
	if cs == nil {
		rc, err := loadRESTConfig(opts.Kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("members: load kube config: %w", err)
		}

		cs, err = kubernetes.NewForConfig(rc)
		if err != nil {
			return nil, fmt.Errorf("members: build clientset: %w", err)
		}
	}

	resync := opts.ResyncPeriod
	if resync == 0 {
		resync = 30 * time.Second
	}

	zoneKey := opts.ZoneLabelKey
	if zoneKey == "" {
		zoneKey = "topology.kubernetes.io/zone"
	}

	// Pod factory: namespace-scoped + label-selected.
	podFactory := informers.NewSharedInformerFactoryWithOptions(cs, resync,
		informers.WithNamespace(opts.Namespace),
		informers.WithTweakListOptions(func(lo *metav1.ListOptions) {
			lo.LabelSelector = sel.String()
		}),
	)

	return &Manager{
		self:         ifaces.NodeID(opts.NodeName),
		zoneLabelKey: zoneKey,
		selector:     sel,
		transferPort: opts.TransferPort,
		clientset:    cs,
		namespace:    opts.Namespace,
		podFactory:   podFactory,
		podInf:       podFactory.Core().V1().Pods().Informer(),
		stopCh:       make(chan struct{}),
	}, nil
}

// Start begins the informers' list-and-watch in the background. It
// does NOT wait for initial sync - callers MUST follow Start with a
// WaitForSync(ctx) call under a bounded context so they own the
// sync-deadline policy (production mode treats a timeout as fatal,
// dev mode warns and continues; see cmd/gantry/main.go.buildMembers
// for the canonical use). Previously Start blocked on
// WaitForSync(ctx) using the long-lived app context, which made the
// 10s bounded WaitForSync in buildMembers dead code - an RBAC /
// permissions failure could pin startup indefinitely instead of
// reaching the deadline branch.
func (m *Manager) Start() {
	m.podFactory.Start(m.stopCh)
}

// Stop tears down the informers. Safe to call multiple times.
func (m *Manager) Stop() {
	m.once.Do(func() { close(m.stopCh) })
}

// Self implements ifaces.Members.
func (m *Manager) Self() ifaces.NodeID { return m.self }

// WaitForSync blocks until the Pod informer has completed its initial
// list+watch or ctx is cancelled.
func (m *Manager) WaitForSync(ctx context.Context) error {
	if !cache.WaitForCacheSync(ctx.Done(), m.podInf.HasSynced) {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("members: wait for sync: %w", err)
		}

		return errors.New("members: informer sync failed")
	}

	return nil
}

// LoadSelfZone reads this agent's own Node once and caches its topology zone.
// Callers only need this in zone-scoped HRW mode.
func (m *Manager) LoadSelfZone(ctx context.Context) error {
	node, err := m.clientset.CoreV1().Nodes().Get(ctx, string(m.self), metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("members: get self node %q: %w", m.self, err)
	}

	m.selfZone = node.Labels[m.zoneLabelKey]

	return nil
}

// SelfZone returns the zone loaded by LoadSelfZone.
func (m *Manager) SelfZone() string { return m.selfZone }

// Annotation keys agents publish on their own pod so peers can discover
// libp2p identity, transfer endpoint, and zone without operator-supplied
// bootstrap config.
const (
	AnnotationPeerID       = "gantry.io/peer-id"
	AnnotationP2PAddrs     = "gantry.io/p2p-addrs"     // comma-separated multiaddrs
	AnnotationTransferAddr = "gantry.io/transfer-addr" // host:port
	AnnotationZone         = "gantry.io/zone"
)

// Snapshot returns the current peer view: one Node per Ready pod matching the
// selector. The returned slice is sorted by NodeID for deterministic HRW input.
//
// PeerID, P2PAddrs, Zone, and a transfer-addr override are read from pod
// annotations populated by each agent's AnnounceSelf call at startup. Pods
// that have not yet published these annotations still appear in the snapshot -
// Addr falls back to podIP[:TransferPort], and the announced fields remain
// empty until the announcement arrives.
//
// Terminating pods (DeletionTimestamp set) are excluded even if they
// are still Phase=Running and Ready=True - kubelet leaves Ready=True
// for the duration of the grace period, and routing HRW/transfer
// traffic to a pod that is about to disappear is a
// connection-refused waiting to happen. The exclusion is enforced
// both directly (the explicit podTerminating check below) and via
// podReady, which short-circuits on the same predicate.
func (m *Manager) Snapshot() []ifaces.Node {
	out := []ifaces.Node{}

	for _, obj := range m.podInf.GetStore().List() {
		p, ok := obj.(*corev1.Pod)
		if !ok {
			continue
		}

		if podTerminating(p) {
			continue
		}

		if !podReady(p) {
			continue
		}

		if p.Spec.NodeName == "" || p.Status.PodIP == "" {
			continue
		}

		addr := p.Status.PodIP
		if m.transferPort > 0 {
			// net.JoinHostPort wraps IPv6 literals in brackets,
			// producing [::1]:5001 instead of the broken ::1:5001 a
			// raw fmt.Sprintf("%s:%d", ...) would emit. Required for
			// IPv6 or dual-stack clusters where PodIP is a v6
			// literal - without bracketing, url.Parse and net.Dial
			// both fail downstream.
			addr = net.JoinHostPort(p.Status.PodIP, strconv.Itoa(m.transferPort))
		}
		// Annotation override wins so operators can publish a
		// non-default transfer endpoint (NodePort, separate listener).
		if a := p.Annotations[AnnotationTransferAddr]; a != "" {
			addr = a
		}

		node := ifaces.Node{
			ID:       ifaces.NodeID(p.Spec.NodeName),
			Addr:     addr,
			Zone:     p.Annotations[AnnotationZone],
			PeerID:   p.Annotations[AnnotationPeerID],
			P2PAddrs: splitAnnotation(p.Annotations[AnnotationP2PAddrs]),
		}
		if node.ID == m.self && node.Zone == "" {
			node.Zone = m.selfZone
		}

		out = append(out, node)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

	return out
}

// splitAnnotation parses a comma-separated annotation value, trimming
// whitespace around each entry and dropping empty fields. Returns nil
// when no entries remain so callers can range over the result without
// special-casing the empty-annotation path.
func splitAnnotation(v string) []string {
	if v == "" {
		return nil
	}

	parts := strings.Split(v, ",")

	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// SnapshotForBootstrap returns the membership view used for libp2p
// bootstrap dialing: every Running pod matching the selector that has
// published a gantry.io/p2p-addrs annotation, regardless of Ready
// status. This intentionally diverges from Snapshot because the
// readiness probe depends on a populated DHT routing table, which
// depends on libp2p bootstrap, which (under Snapshot's filter) would
// depend on at least one peer already being Ready - a deadlock on
// first-cluster boot and full-cluster restart. Bootstrap-time peers
// are a strictly larger set than serving-time peers; we want any peer
// we can reach, even a NotReady one, just to seed the DHT.
//
// The serving path (HRW choice, transfer destinations) MUST keep
// using Snapshot so unready peers don't receive request traffic.
//
// Terminating pods (DeletionTimestamp set) are excluded even if they
// still have valid p2p-addrs annotations - they will never
// re-announce, so bootstrap dials to them are wasted at best and at
// worst keep the connection pool half-open until the termination
// grace period expires.
func (m *Manager) SnapshotForBootstrap() []ifaces.Node {
	out := []ifaces.Node{}

	for _, obj := range m.podInf.GetStore().List() {
		p, ok := obj.(*corev1.Pod)
		if !ok {
			continue
		}

		if podTerminating(p) {
			continue
		}

		if p.Status.Phase != corev1.PodRunning {
			continue
		}

		if p.Spec.NodeName == "" || p.Status.PodIP == "" {
			continue
		}
		// Bootstrap is only useful when the peer has published its
		// libp2p multiaddrs. Pods that haven't AnnounceSelf'd yet
		// contribute nothing to a dial seed.
		p2p := splitAnnotation(p.Annotations[AnnotationP2PAddrs])
		if len(p2p) == 0 {
			continue
		}

		addr := p.Status.PodIP
		if m.transferPort > 0 {
			// net.JoinHostPort: see Snapshot for the IPv6
			// bracketing rationale. SnapshotForBootstrap feeds
			// the bootstrap dial loop directly; an IPv6 literal
			// without brackets here would silently break libp2p
			// dialing on v6 clusters.
			addr = net.JoinHostPort(p.Status.PodIP, strconv.Itoa(m.transferPort))
		}

		if a := p.Annotations[AnnotationTransferAddr]; a != "" {
			addr = a
		}

		out = append(out, ifaces.Node{
			ID:       ifaces.NodeID(p.Spec.NodeName),
			Addr:     addr,
			PeerID:   p.Annotations[AnnotationPeerID],
			P2PAddrs: p2p,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

	return out
}

// RunningMatchingPodCount returns the number of pods the informer
// sees in Running phase with a populated PodIP, REGARDLESS of Ready
// condition or any annotation. The informer's label selector and
// namespace scope already constrain the set to Gantry peers; this
// method intentionally does not apply any further filter.
//
// Contract (do not tighten):
//
// - Requires Status.Phase == PodRunning AND Status.PodIP != "".
// - Requires NOT terminating (DeletionTimestamp == nil) - see
// below for the rolling-update deadlock this avoids.
// - Does NOT require: PodReady=True, gantry.io/p2p-addrs,
// gantry.io/peer-id, or any other readiness/announcement signal.
//
// This is the strict superset that Snapshot (Ready) and
// SnapshotForBootstrap (Running + p2p-addrs annotated) are filtered
// subsets of. It exists so /readyz can distinguish "this is a real
// single-node cluster" (count == 1) from "multi-node cluster, peers
// just haven't self-announced yet" (count > 1, bootstrap view ≤ 1).
// Surfacing the latter as "peer self-announcements pending" rather
// than "dht routing table empty" tells operators the real cause; it
// also keeps the readiness probe at 503 during the first-rollout
// window where every pod is Running but none has yet published its
// libp2p multiaddrs - without this gate, /readyz would race to green
// before any peer was actually reachable.
//
// Terminating-pod exclusion is the rolling-update / drain guard: a
// terminating pod stays Phase=Running for the grace period but will
// never re-AnnounceSelf with fresh annotations. Counting it here
// while SnapshotForBootstrap rightly excludes it (stale or absent
// p2p-addrs) would deadlock the surviving pods at
// "peer self-announcements pending" - they would wait forever for
// an announcement from a peer that has no opportunity to make one.
func (m *Manager) RunningMatchingPodCount() int {
	n := 0

	for _, obj := range m.podInf.GetStore().List() {
		p, ok := obj.(*corev1.Pod)
		if !ok {
			continue
		}

		if podTerminating(p) {
			continue
		}

		if p.Status.Phase != corev1.PodRunning {
			continue
		}

		if p.Status.PodIP == "" {
			continue
		}

		n++
	}

	return n
}

// podReady reports whether a pod is in Running phase with a Ready=True
// condition AND is not in the process of termination. Pending,
// Terminating, and not-Ready pods are excluded from the membership
// view so HRW does not score nodes that cannot serve traffic (or are
// about to disappear).
//
// The DeletionTimestamp check is the tail-of-rollout guard: kubelet
// can leave a pod at Phase=Running with Ready=True for the duration
// of the termination grace period, and routing traffic to it during
// that window is a connection-refused waiting to happen. The other
// three views (Snapshot, SnapshotForBootstrap, RunningMatchingPodCount)
// apply podTerminating at their own loop heads; this defensive
// re-check here means any future caller of podReady inherits the
// contract that the function's doc comment already claims.
func podReady(p *corev1.Pod) bool {
	if p == nil || podTerminating(p) || p.Status.Phase != corev1.PodRunning {
		return false
	}

	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}

	return false
}

// podTerminating reports whether a pod has been marked for deletion
// by the API server (DeletionTimestamp set). Such a pod is leaving
// the cluster, will never re-AnnounceSelf with fresh annotations, and
// must NOT contribute to any membership view:
//
// - Snapshot: terminating pods would receive HRW
// transfer traffic on the way out.
// - SnapshotForBootstrap: bootstrap dials to a doomed peer are
// wasted at best, and at worst keep the
// connection pool half-open until the
// grace period expires.
// - RunningMatchingPodCount: this count gates the new "peer
// self-announcements pending" /readyz
// branch. If a terminating pod is
// counted but never appears in the
// bootstrap view, the surviving pods
// deadlock at NotReady - they wait for
// an announcement from a peer that has
// no opportunity to make one.
//
// The nil guard means callers in tight loops can skip a separate
// nil-check before the type assertion. Implemented as a free function
// (not a method) so test fixtures can construct *corev1.Pod values
// with explicit DeletionTimestamps without going through the
// informer.
func podTerminating(p *corev1.Pod) bool {
	return p != nil && p.DeletionTimestamp != nil
}

// Compile-time check.
var _ ifaces.Members = (*Manager)(nil)

// loadRESTConfig prefers in-cluster discovery; falls back to an explicit
// kubeconfig path. Empty path + no in-cluster env returns an error.
func loadRESTConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}

	rc, err := rest.InClusterConfig()
	if err == nil {
		return rc, nil
	}

	if !errors.Is(err, rest.ErrNotInCluster) {
		return nil, err
	}

	return nil, errors.New("members: not in cluster and no kubeconfig supplied")
}
