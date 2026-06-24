// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package storagesupervisor

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	storageconfig "github.com/Azure/unbounded/api/unbounded-storage"
)

// nodeInformerResync is the periodic relist interval for the node informer. A
// non-zero resync is cheap insurance against a missed watch event silently
// leaving the peer set stale.
const nodeInformerResync = 30 * time.Second

// ringState is a pure snapshot of the storage ring this node belongs to,
// computed from the watched Node set and the fabric port. It is the seam
// between the Kubernetes node watch (peerWatcher) and the pure renderer
// (RenderConfig): the watcher produces it, the renderer consumes it.
type ringState struct {
	// active is true when this node participates in a ring and the resulting
	// peers/fabric addr should be injected into the rendered config. When false
	// the renderer leaves the per-node sections untouched and passes the
	// ConfigMap's fabric addr through verbatim.
	active bool
	// localNodeID is this node's stable id (hash of its node name), written to
	// neighborhoods[].local_node_id.
	localNodeID uint64
	// selfListenAddr is this node's own routable fabric bind, "<internalIP>:
	// <port>", overriding the ConfigMap's fabric addr so the tcp provider
	// binds an address peers can actually dial.
	selfListenAddr string
	// peers is the other ring members as daemon PeerSpecs, sorted by id.
	peers []*storageconfig.PeerSpec
}

// nodeSnapshot is the node-derived supervisor overlay. The TCP storage ring
// needs a fixed port from source config; TCP benchmark annotations reuse it as
// their default port.
type nodeSnapshot struct {
	ring       ringState
	benchmarks benchmarkState
}

// peerWatcher watches cluster Nodes carrying the storage-ring label and turns
// the membership view into node-derived config overlays on demand. It signals
// the run loop on every membership change so the config is re-rendered.
type peerWatcher struct {
	selfName  string
	ringLabel string

	clientset kubernetes.Interface
	factory   informers.SharedInformerFactory
	informer  cache.SharedIndexInformer

	// events coalesces node-change notifications. It is buffered with capacity
	// 1 and written non-blockingly so a burst of events collapses into a single
	// pending signal the run loop drains.
	events chan struct{}

	stopCh chan struct{}
	once   sync.Once
}

// newPeerWatcher builds a watcher for the ring this node belongs to. cs may be
// a pre-built clientset (the fake clientset in tests); when nil an in-cluster
// (or kubeconfig) clientset is constructed. It returns (nil, nil) when peer
// discovery is disabled because no node name was provided, so the run loop can
// fall back to rendering startup settings only.
func newPeerWatcher(cfg Config, cs kubernetes.Interface) (*peerWatcher, error) {
	if cfg.NodeName == "" {
		return nil, nil
	}

	if cfg.StorageRingLabel == "" {
		return nil, errors.New("peers: StorageRingLabel required when NodeName is set")
	}

	if cs == nil {
		rc, err := loadRESTConfig(cfg.Kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("peers: load kube config: %w", err)
		}

		cs, err = kubernetes.NewForConfig(rc)
		if err != nil {
			return nil, fmt.Errorf("peers: build clientset: %w", err)
		}
	}

	// Scope the node watch to nodes that carry the ring label at all (an
	// Exists selector); membership by value is resolved in computeRing.
	factory := informers.NewSharedInformerFactoryWithOptions(cs, nodeInformerResync,
		informers.WithTweakListOptions(func(lo *metav1.ListOptions) {
			lo.LabelSelector = cfg.StorageRingLabel
		}),
	)

	w := &peerWatcher{
		selfName:  cfg.NodeName,
		ringLabel: cfg.StorageRingLabel,
		clientset: cs,
		factory:   factory,
		informer:  factory.Core().V1().Nodes().Informer(),
		events:    make(chan struct{}, 1),
		stopCh:    make(chan struct{}),
	}

	if _, err := w.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { w.signal() },
		UpdateFunc: func(any, any) { w.signal() },
		DeleteFunc: func(any) { w.signal() },
	}); err != nil {
		return nil, fmt.Errorf("peers: add node event handler: %w", err)
	}

	return w, nil
}

// Start begins the informer's list-and-watch and blocks until the initial
// sync completes or ctx is cancelled.
func (w *peerWatcher) Start(ctx context.Context) error {
	w.factory.Start(w.stopCh)

	if !cache.WaitForCacheSync(ctx.Done(), w.informer.HasSynced) {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("peers: wait for node sync: %w", err)
		}

		return errors.New("peers: node informer sync failed")
	}

	return nil
}

// Stop tears down the informer. Safe to call multiple times.
func (w *peerWatcher) Stop() {
	w.once.Do(func() { close(w.stopCh) })
}

// Events returns the change-notification channel the run loop selects on.
func (w *peerWatcher) Events() <-chan struct{} { return w.events }

// signal posts a coalesced change notification without blocking the informer
// callback.
func (w *peerWatcher) signal() {
	select {
	case w.events <- struct{}{}:
	default:
	}
}

// snapshot computes the current node-derived overlay from the informer's node
// store and the TCP fabric port. A zero or unparseable port yields an inactive
// TCP ring, but benchmark annotations are still evaluated because RDMA
// benchmarks use RDMA peer addresses carried in Node annotations.
func (w *peerWatcher) snapshot(port int, portOK bool) nodeSnapshot {
	objs := w.informer.GetStore().List()

	nodes := make([]*corev1.Node, 0, len(objs))

	for _, obj := range objs {
		if n, ok := obj.(*corev1.Node); ok {
			nodes = append(nodes, n)
		}
	}

	snapshot := nodeSnapshot{benchmarks: computeBenchmarks(nodes, w.selfName, port)}
	if !portOK || port == 0 {
		slog.Warn("storage ring inactive: no fixed fabric port set in startup.fabric.tcp.addr; "+
			"set a non-zero port (e.g. 0.0.0.0:9000) to enable peering", "node", w.selfName)

		return snapshot
	}

	snapshot.ring = computeRing(nodes, w.selfName, w.ringLabel, port)

	return snapshot
}

// computeRing is the pure core of peer discovery: given the ring-labelled
// nodes, this node's name, the ring label key, and the shared fabric port, it
// produces the ringState to inject into the config. It is separated from the
// informer plumbing so the membership logic is unit-testable.
//
// Membership is by equal label value: every node whose ring label matches this
// node's becomes a peer, this node excluded. Ids are a stable hash of the node
// name so a node keeps the same id across membership churn. A peer with no
// usable InternalIP, or whose id collides with this node or an already-emitted
// peer, is skipped and logged rather than corrupting the set.
func computeRing(nodes []*corev1.Node, selfName, ringLabel string, port int) ringState {
	var self *corev1.Node

	for _, n := range nodes {
		if n.Name == selfName {
			self = n

			break
		}
	}

	if self == nil {
		slog.Warn("storage ring inactive: this node not found among ring-labelled nodes", "node", selfName)

		return ringState{}
	}

	ringValue, ok := self.Labels[ringLabel]
	if !ok || ringValue == "" {
		// Node carries no ring membership; leave per-node config untouched.
		return ringState{}
	}

	selfIP := internalIP(self)
	if selfIP == "" {
		slog.Warn("storage ring inactive: this node has no InternalIP", "node", selfName)

		return ringState{}
	}

	localID := nodeID(selfName)

	ring := ringState{
		active:         true,
		localNodeID:    localID,
		selfListenAddr: net.JoinHostPort(selfIP, strconv.Itoa(port)),
	}

	seen := map[uint64]string{localID: selfName}

	for _, n := range nodes {
		if n.Name == selfName {
			continue
		}

		if n.Labels[ringLabel] != ringValue {
			continue
		}

		ip := internalIP(n)
		if ip == "" {
			slog.Warn("skipping storage ring peer with no InternalIP", "peer", n.Name, "ring", ringValue)

			continue
		}

		id := nodeID(n.Name)
		if other, dup := seen[id]; dup {
			slog.Warn("skipping storage ring peer: node id collision",
				"peer", n.Name, "collides_with", other, "id", id)

			continue
		}

		seen[id] = n.Name

		ring.peers = append(ring.peers, &storageconfig.PeerSpec{
			Id: id,
			Config: &storageconfig.PeerSpec_Tcp{
				Tcp: &storageconfig.TcpPeerConfig{Addr: net.JoinHostPort(ip, strconv.Itoa(port))},
			},
		})
	}

	sort.Slice(ring.peers, func(i, j int) bool { return ring.peers[i].Id < ring.peers[j].Id })

	return ring
}

// nodeID derives a node's stable storage id from its name via 64-bit FNV-1a.
// The id doubles as the daemon's NodeId and PeerId, so it must be stable
// across membership changes (a hash of the immutable node name is) and
// non-zero (zero is the daemon's "no local id / single peerless node"
// sentinel), which the +1 guard ensures without weakening uniqueness in
// practice.
func nodeID(name string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name)) //nolint:errcheck // hash.Write never errors.

	id := h.Sum64()
	if id == 0 {
		id = 1
	}

	return id
}

// internalIP returns the node's first InternalIP address, or "" when none is
// reported. InternalIP is the routable address peers dial on a flat cluster
// network.
func internalIP(node *corev1.Node) string {
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP && addr.Address != "" {
			return addr.Address
		}
	}

	return ""
}

// parseFabricPort extracts the port from a "host:port" fabric address. It
// returns ok=false when the address is empty, malformed, or has no numeric
// port, which the caller treats as "no fixed port" (ring inactive).
func parseFabricPort(addr string) (int, bool) {
	if addr == "" {
		return 0, false
	}

	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, false
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0, false
	}

	return port, true
}

// loadRESTConfig prefers in-cluster discovery and falls back to an explicit
// kubeconfig path. An empty path with no in-cluster environment is an error.
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

	return nil, errors.New("peers: not in cluster and no kubeconfig supplied")
}
