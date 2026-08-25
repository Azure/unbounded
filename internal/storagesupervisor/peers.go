// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package storagesupervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
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

// placeholderRdmaSelfAddr is only used before a node's daemon has published
// live RDMA inventory. Config validation requires each peer to carry a valid
// address, while the daemon needs self set on the first render because peer
// identity is startup-fixed. This native address is never dialed: runtime
// projection removes the self peer from the remote peer set, and a later render
// replaces it with the daemon's real inventory address.
const placeholderRdmaSelfAddr = "hex:00"

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
	// selfName is this node's stable peer name, written to self.
	selfName string
	// selfListenAddr is this node's own routable fabric bind, "<internalIP>:
	// <port>", overriding the ConfigMap's fabric addr so the tcp provider
	// binds an address peers can actually dial.
	selfListenAddr string
	// peers is the ring roster as daemon PeerSpecs, including self, sorted by name.
	peers []*storageconfig.PeerSpec
}

// peerWatcher watches cluster Nodes and turns the self node's annotations plus
// storage-ring membership view into renderState on demand. It signals the run
// loop on every node change so the config is re-rendered.
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

	factory := informers.NewSharedInformerFactory(cs, nodeInformerResync)

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
// sync completes or ctx is canceled.
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

// snapshot computes the current render state from the informer's node store and
// the fabric port. A zero or unparseable port yields an inactive ring: without a
// fixed port the daemon's fabric addr stays ephemeral and peer addresses are
// unreachable, so emitting peers would be misleading. Node annotations are still
// returned when the ring is inactive.
func (w *peerWatcher) snapshot(port int, portOK bool) renderState {
	objs := w.informer.GetStore().List()
	nodes := nodesFromInformerObjects(objs)

	state := renderState{annotations: selfAnnotations(nodes, w.selfName)}
	if !portOK || port == 0 {
		slog.Warn("storage ring inactive: no fixed fabric port set in startup.fabric.tcp.addr; "+
			"set a non-zero port (e.g. 0.0.0.0:9000) to enable peering", "node", w.selfName)

		return state
	}

	state.ring = computeRing(nodes, w.selfName, w.ringLabel, port)

	return state
}

// snapshotRdma computes render state for RDMA fabrics. Local bind addresses are
// owned by the daemon's auto-RDMA/explicit-RDMA startup config, so unlike TCP
// this does not override startup.fabric; it only injects self and peer RDMA
// addresses published by other supervisors.
func (w *peerWatcher) snapshotRdma() renderState {
	objs := w.informer.GetStore().List()
	nodes := nodesFromInformerObjects(objs)

	return renderState{
		annotations: selfAnnotations(nodes, w.selfName),
		ring:        computeRDMARing(nodes, w.selfName, w.ringLabel),
	}
}

func nodesFromInformerObjects(objs []any) []*corev1.Node {
	nodes := make([]*corev1.Node, 0, len(objs))

	for _, obj := range objs {
		if n, ok := obj.(*corev1.Node); ok {
			nodes = append(nodes, n)
		}
	}

	return nodes
}

// computeRing is the pure core of peer discovery: given the ring-labeled
// nodes, this node's name, the ring label key, and the shared fabric port, it
// produces the ringState to inject into the config. It is separated from the
// informer plumbing so the membership logic is unit-testable.
//
// Membership is by equal label value: every node whose ring label matches this
// node's becomes a named peer, including this node. A peer with no usable
// InternalIP is skipped and logged rather than corrupting the set.
func computeRing(nodes []*corev1.Node, selfName, ringLabel string, port int) ringState {
	var self *corev1.Node

	for _, n := range nodes {
		if n.Name == selfName {
			self = n

			break
		}
	}

	if self == nil {
		slog.Warn("storage ring inactive: this node not found among watched nodes", "node", selfName)

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

	ring := ringState{
		active:         true,
		selfName:       selfName,
		selfListenAddr: net.JoinHostPort(selfIP, strconv.Itoa(port)),
	}

	seen := map[string]struct{}{}

	for _, n := range nodes {
		if n.Labels[ringLabel] != ringValue {
			continue
		}

		if _, dup := seen[n.Name]; dup {
			continue
		}

		ip := internalIP(n)
		if ip == "" {
			slog.Warn("skipping storage ring peer with no InternalIP", "peer", n.Name, "ring", ringValue)

			continue
		}

		seen[n.Name] = struct{}{}

		ring.peers = append(ring.peers, &storageconfig.PeerSpec{
			Name: n.Name,
			Config: &storageconfig.PeerSpec_Tcp{
				Tcp: &storageconfig.TcpPeerConfig{Addr: net.JoinHostPort(ip, strconv.Itoa(port))},
			},
		})
	}

	sort.Slice(ring.peers, func(i, j int) bool { return ring.peers[i].Name < ring.peers[j].Name })

	return ring
}

// computeRDMARing mirrors computeRing's label-membership rules, but peers are
// dialed through their published RDMA HCA inventory annotation. The daemon
// config currently models one RDMA address per peer name, so discovery chooses
// the first address from each peer's full HCA inventory.
func computeRDMARing(nodes []*corev1.Node, selfName, ringLabel string) ringState {
	var self *corev1.Node

	for _, n := range nodes {
		if n.Name == selfName {
			self = n

			break
		}
	}

	if self == nil {
		slog.Warn("storage ring inactive: this node not found among watched nodes", "node", selfName)

		return ringState{}
	}

	ringValue, ok := self.Labels[ringLabel]
	if !ok || ringValue == "" {
		return ringState{}
	}

	ring := ringState{
		active:   true,
		selfName: selfName,
	}
	seen := map[string]struct{}{}

	for _, n := range nodes {
		if n.Labels[ringLabel] != ringValue {
			continue
		}

		if _, dup := seen[n.Name]; dup {
			continue
		}

		addrs, err := rdmaInventoryAddrs(n.Annotations[storageRdmaHcasAnnotation])
		if err != nil {
			if n.Name == selfName {
				slog.Warn("storage ring inactive: this node has invalid rdma inventory", "node", selfName, "ring", ringValue, "error", err)

				return ringState{}
			}

			slog.Warn("skipping storage ring peer with invalid rdma inventory", "peer", n.Name, "ring", ringValue, "error", err)

			continue
		}

		addr := ""
		if len(addrs) > 0 {
			addr = addrs[0]
		}

		if addr == "" {
			if n.Name != selfName {
				slog.Warn("skipping storage ring peer with no rdma address", "peer", n.Name, "ring", ringValue)

				continue
			}

			// The daemon's local peer identity is startup-fixed, but RDMA
			// inventory is only available after the daemon has started. Emit a
			// valid placeholder for self so the first render locks in the peer
			// name; later renders replace it with the daemon-published address.
			addr = placeholderRdmaSelfAddr
		}

		addr, ok = rdmaPeerDialAddr(addr, n)
		if !ok {
			if n.Name == selfName {
				slog.Warn("storage ring inactive: this node has wildcard rdma address but no InternalIP", "node", selfName, "ring", ringValue)

				return ringState{}
			}

			slog.Warn("skipping storage ring peer with wildcard rdma address but no InternalIP", "peer", n.Name, "ring", ringValue)

			continue
		}

		if len(addrs) == 0 {
			addrs = []string{addr}
		} else {
			addrs = rewriteRdmaPeerDialAddrs(addrs, n)
			addrs[0] = addr
		}

		seen[n.Name] = struct{}{}
		ring.peers = append(ring.peers, &storageconfig.PeerSpec{
			Name: n.Name,
			Config: &storageconfig.PeerSpec_Rdma{
				Rdma: &storageconfig.RdmaPeerConfig{Addr: addr, Addrs: addrs},
			},
		})
	}

	sort.Slice(ring.peers, func(i, j int) bool { return ring.peers[i].Name < ring.peers[j].Name })

	return ring
}

func rewriteRdmaPeerDialAddrs(addrs []string, node *corev1.Node) []string {
	rewritten := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		if addr == "" {
			continue
		}

		if dialAddr, ok := rdmaPeerDialAddr(addr, node); ok {
			rewritten = append(rewritten, dialAddr)
		}
	}

	return rewritten
}

func rdmaPeerDialAddr(addr string, node *corev1.Node) (string, bool) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, true
	}

	ip := net.ParseIP(host)
	if ip == nil || !ip.IsUnspecified() {
		return addr, true
	}

	internal := internalIP(node)
	if internal == "" {
		return "", false
	}

	return net.JoinHostPort(internal, port), true
}

func selfAnnotations(nodes []*corev1.Node, selfName string) map[string]string {
	for _, n := range nodes {
		if n.Name != selfName {
			continue
		}

		if len(n.Annotations) == 0 {
			return nil
		}

		annotations := make(map[string]string, len(n.Annotations))
		for k, v := range n.Annotations {
			annotations[k] = v
		}

		return annotations
	}

	return nil
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
