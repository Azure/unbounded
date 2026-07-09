// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package network

import (
	"context"
	"fmt"
	"hash/fnv"
	"net"
	"sort"
	"time"

	netv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/playpen/labels"
)

const (
	DefaultWireGuardPort = 51820
	DefaultVXLANPort     = 4789
	DefaultVXLANVNI      = 1
)

type Config struct {
	DefaultSite        string
	DefaultGatewayPool string
	DefaultPodCIDRBase string
	VXLANPort          int
}

type Manager struct {
	ctrl client.Client
	cfg  Config
}

type AllocateRequest struct {
	Site                     string
	GatewayPool              string
	ClientWireGuardPublicKey string
	ClientInternalIP         string
}

type Allocation struct {
	NodeName         string
	PodCIDR          string
	GatewayPool      string
	RequiresEndpoint bool
	GatewayPeers     []GatewayPeer
	Tunnel           TunnelInfo
}

type TunnelInfo struct {
	Mode               string
	WireGuardAddress   string
	WireGuardPublicKey string
	VXLANVNI           int
	VXLANPort          int
	EndpointRequired   bool
}

type GatewayPeer struct {
	Name               string
	Site               string
	WireGuardPublicKey string
	InternalIPs        []string
	Endpoints          []string
	PodCIDRs           []string
	RoutedCIDRs        []string
}

type gatewaySelection struct {
	Name             string
	Peers            []GatewayPeer
	RequiresEndpoint bool
}

func NewManager(ctrlClient client.Client, cfg Config) *Manager {
	if cfg.DefaultSite == "" {
		cfg.DefaultSite = "playpen"
	}
	if cfg.DefaultPodCIDRBase == "" {
		cfg.DefaultPodCIDRBase = "10.241.0.0/16"
	}
	if cfg.VXLANPort == 0 {
		cfg.VXLANPort = DefaultVXLANPort
	}

	return &Manager{ctrl: ctrlClient, cfg: cfg}
}

func (m *Manager) Allocate(ctx context.Context, allocationID string, expiresAt time.Time, req AllocateRequest, podCIDR string) (*Allocation, error) {
	site := firstNonEmpty(req.Site, m.cfg.DefaultSite)
	if podCIDR == "" {
		podCIDR = podCIDRForAllocation(m.cfg.DefaultPodCIDRBase, allocationID)
	}
	if err := m.ensureSite(ctx, site, podCIDR, expiresAt); err != nil {
		return nil, err
	}

	nodeName := "playpen-" + allocationID
	internalIP := firstNonEmpty(req.ClientInternalIP, routerIP(podCIDR))

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Labels: map[string]string{
				labels.ManagedByLabel:    labels.AppName,
				labels.ComponentLabel:    "fake-node",
				labels.AllocationIDLabel: allocationID,
				labels.OwnedLabel:        "true",
				labels.NetSiteLabel:      site,
			},
			Annotations: map[string]string{
				labels.ExpiresAtAnnotation: expiresAt.Format(time.RFC3339),
			},
		},
		Spec: corev1.NodeSpec{
			PodCIDR:  podCIDR,
			PodCIDRs: []string{podCIDR},
		},
	}
	if req.ClientWireGuardPublicKey != "" {
		node.Annotations[labels.WireGuardPubKeyAnnotation] = req.ClientWireGuardPublicKey
	}

	if err := m.ctrl.Create(ctx, node); err != nil {
		return nil, fmt.Errorf("create fake node: %w", err)
	}

	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: internalIP}}
	if err := m.ctrl.Status().Update(ctx, node); err != nil {
		_ = m.ctrl.Delete(ctx, node)
		return nil, fmt.Errorf("patch fake node status: %w", err)
	}

	gatewayPool := firstNonEmpty(req.GatewayPool, m.cfg.DefaultGatewayPool)
	selection, err := m.gatewayPeers(ctx, gatewayPool)
	if err != nil {
		_ = m.ctrl.Delete(ctx, node)
		return nil, err
	}
	if selection.Name == "" || len(selection.Peers) == 0 {
		_ = m.ctrl.Delete(ctx, node)
		return nil, fmt.Errorf("no usable unbounded-net gateway pool peers found")
	}
	if selection.Name != "" {
		if err := m.ensureVXLANAssignment(ctx, allocationID, site, selection.Name, expiresAt); err != nil {
			_ = m.ctrl.Delete(ctx, node)
			return nil, err
		}
	}

	return &Allocation{
		NodeName:         nodeName,
		PodCIDR:          podCIDR,
		GatewayPool:      selection.Name,
		RequiresEndpoint: selection.RequiresEndpoint,
		GatewayPeers:     selection.Peers,
		Tunnel: TunnelInfo{
			Mode:               tunnelMode(selection.RequiresEndpoint),
			WireGuardAddress:   internalIP,
			WireGuardPublicKey: req.ClientWireGuardPublicKey,
			VXLANVNI:           DefaultVXLANVNI,
			VXLANPort:          m.cfg.VXLANPort,
			EndpointRequired:   selection.RequiresEndpoint,
		},
	}, nil
}

func (m *Manager) Delete(ctx context.Context, allocationID string) (bool, error) {
	deleted := false
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "playpen-" + allocationID}}
	if err := m.ctrl.Delete(ctx, node); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("delete fake node: %w", err)
		}
	} else {
		deleted = true
	}

	assignment := &netv1alpha1.SiteGatewayPoolAssignment{ObjectMeta: metav1.ObjectMeta{Name: "playpen-" + allocationID}}
	if err := m.ctrl.Delete(ctx, assignment); err != nil {
		if !apierrors.IsNotFound(err) {
			return deleted, fmt.Errorf("delete gateway assignment: %w", err)
		}
	} else {
		deleted = true
	}

	return deleted, nil
}

func (m *Manager) DeleteExpired(ctx context.Context, now time.Time) error {
	nodes := &corev1.NodeList{}
	if err := m.ctrl.List(ctx, nodes, client.MatchingLabels{labels.OwnedLabel: "true"}); err != nil {
		return fmt.Errorf("list playpen nodes: %w", err)
	}

	for i := range nodes.Items {
		node := &nodes.Items[i]
		expiresRaw := node.Annotations[labels.ExpiresAtAnnotation]
		expiresAt, err := time.Parse(time.RFC3339, expiresRaw)
		if err != nil || now.Before(expiresAt) {
			continue
		}

		allocationID := node.Labels[labels.AllocationIDLabel]
		if allocationID == "" {
			continue
		}
		if _, err := m.Delete(ctx, allocationID); err != nil {
			return err
		}
	}

	return nil
}

func (m *Manager) gatewayPeers(ctx context.Context, gatewayPool string) (gatewaySelection, error) {
	if gatewayPool != "" {
		pool := &netv1alpha1.GatewayPool{}
		if err := m.ctrl.Get(ctx, client.ObjectKey{Name: gatewayPool}, pool); err != nil {
			return gatewaySelection{}, fmt.Errorf("get gateway pool %q: %w", gatewayPool, err)
		}

		return gatewaySelection{Name: gatewayPool, Peers: peersFromPool(pool), RequiresEndpoint: requiresEndpoint(pool)}, nil
	}

	list := &netv1alpha1.GatewayPoolList{}
	err := m.ctrl.List(ctx, list)
	if apierrors.IsNotFound(err) {
		return gatewaySelection{}, fmt.Errorf("unbounded-net GatewayPool CRD is not installed")
	}
	if err != nil {
		return gatewaySelection{}, fmt.Errorf("list gateway pools: %w", err)
	}

	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	for i := range list.Items {
		pool := &list.Items[i]
		peers := peersFromPool(pool)
		if len(peers) == 0 {
			continue
		}

		return gatewaySelection{Name: pool.Name, Peers: peers, RequiresEndpoint: requiresEndpoint(pool)}, nil
	}

	return gatewaySelection{}, nil
}

func peersFromPool(pool *netv1alpha1.GatewayPool) []GatewayPeer {
	peers := make([]GatewayPeer, 0, len(pool.Status.Nodes))

	for _, node := range pool.Status.Nodes {
		name := node.Name
		pubKey := node.WireGuardPublicKey
		if name == "" || pubKey == "" {
			continue
		}

		endpoints := endpointsFromIPs(node.ExternalIPs, DefaultWireGuardPort)
		if len(endpoints) == 0 {
			endpoints = endpointsFromIPs(node.InternalIPs, DefaultWireGuardPort)
		}

		peers = append(peers, GatewayPeer{
			Name:               name,
			Site:               node.SiteName,
			WireGuardPublicKey: pubKey,
			InternalIPs:        node.InternalIPs,
			Endpoints:          endpoints,
			PodCIDRs:           node.PodCIDRs,
			RoutedCIDRs:        pool.Spec.RoutedCidrs,
		})
	}

	return peers
}

func requiresEndpoint(pool *netv1alpha1.GatewayPool) bool {
	if pool.Spec.Type == "Internal" {
		return true
	}

	for _, peer := range peersFromPool(pool) {
		if len(peer.Endpoints) > 0 {
			return false
		}
	}

	return true
}

func (m *Manager) ensureVXLANAssignment(ctx context.Context, allocationID, site, gatewayPool string, expiresAt time.Time) error {
	name := "playpen-" + allocationID
	tunnelProtocol := netv1alpha1.TunnelProtocolVXLAN
	assignment := &netv1alpha1.SiteGatewayPoolAssignment{}
	if err := m.ctrl.Get(ctx, client.ObjectKey{Name: name}, assignment); apierrors.IsNotFound(err) {
		assignment = &netv1alpha1.SiteGatewayPoolAssignment{
			TypeMeta: metav1.TypeMeta{APIVersion: netv1alpha1.GroupVersion.String(), Kind: "SiteGatewayPoolAssignment"},
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Labels:      ownedLabels(allocationID, "gateway-assignment"),
				Annotations: map[string]string{labels.ExpiresAtAnnotation: expiresAt.Format(time.RFC3339)},
			},
			Spec: netv1alpha1.SiteGatewayPoolAssignmentSpec{
				Enabled:        ptr.To(true),
				Sites:          []string{site},
				GatewayPools:   []string{gatewayPool},
				TunnelProtocol: &tunnelProtocol,
			},
		}
		if err := m.ctrl.Create(ctx, assignment); err != nil {
			return fmt.Errorf("ensure VXLAN gateway assignment: %w", err)
		}

		return nil
	} else if err != nil {
		return fmt.Errorf("ensure VXLAN gateway assignment: %w", err)
	}

	before := assignment.DeepCopy()
	assignment.Labels = mergeMap(assignment.Labels, ownedLabels(allocationID, "gateway-assignment"))
	assignment.Annotations = mergeMap(assignment.Annotations, map[string]string{labels.ExpiresAtAnnotation: expiresAt.Format(time.RFC3339)})
	assignment.Spec.Enabled = ptr.To(true)
	assignment.Spec.Sites = []string{site}
	assignment.Spec.GatewayPools = []string{gatewayPool}
	assignment.Spec.TunnelProtocol = &tunnelProtocol
	if err := m.ctrl.Patch(ctx, assignment, client.MergeFrom(before)); err != nil {
		return fmt.Errorf("ensure VXLAN gateway assignment: %w", err)
	}

	return nil
}

func (m *Manager) ensureSite(ctx context.Context, site, podCIDR string, expiresAt time.Time) error {
	nodeCIDRs := desiredNodeCIDRs(m.cfg.DefaultPodCIDRBase, podCIDR)
	tunnelProtocol := netv1alpha1.TunnelProtocolVXLAN
	siteObj := &netv1alpha1.Site{}
	if err := m.ctrl.Get(ctx, client.ObjectKey{Name: site}, siteObj); apierrors.IsNotFound(err) {
		siteObj = &netv1alpha1.Site{
			TypeMeta: metav1.TypeMeta{APIVersion: netv1alpha1.GroupVersion.String(), Kind: "Site"},
			ObjectMeta: metav1.ObjectMeta{
				Name: site,
				Labels: map[string]string{
					labels.ManagedByLabel: labels.AppName,
					labels.ComponentLabel: "site",
					labels.OwnedLabel:     "true",
				},
				Annotations: map[string]string{labels.ExpiresAtAnnotation: expiresAt.Format(time.RFC3339)},
			},
			Spec: siteSpec(nodeCIDRs, tunnelProtocol),
		}
		if err := m.ctrl.Create(ctx, siteObj); err != nil {
			return fmt.Errorf("ensure playpen site: %w", err)
		}

		return nil
	} else if err != nil {
		return fmt.Errorf("ensure playpen site: %w", err)
	}

	before := siteObj.DeepCopy()
	mergedCIDRs := mergeStrings(siteObj.Spec.NodeCidrs, nodeCIDRs...)
	siteObj.Labels = mergeMap(siteObj.Labels, map[string]string{
		labels.ManagedByLabel: labels.AppName,
		labels.ComponentLabel: "site",
		labels.OwnedLabel:     "true",
	})
	siteObj.Annotations = mergeMap(siteObj.Annotations, map[string]string{labels.ExpiresAtAnnotation: expiresAt.Format(time.RFC3339)})
	siteObj.Spec = siteSpec(mergedCIDRs, tunnelProtocol)
	if err := m.ctrl.Patch(ctx, siteObj, client.MergeFrom(before)); err != nil {
		return fmt.Errorf("ensure playpen site: %w", err)
	}

	return nil
}

func siteSpec(cidrs []string, tunnelProtocol netv1alpha1.TunnelProtocol) netv1alpha1.SiteSpec {
	return netv1alpha1.SiteSpec{
		NodeCidrs:       cidrs,
		ManageCniPlugin: ptr.To(false),
		PodCidrAssignments: []netv1alpha1.PodCidrAssignment{{
			AssignmentEnabled: ptr.To(false),
			CidrBlocks:        cidrs,
		}},
		TunnelProtocol: &tunnelProtocol,
	}
}

func desiredNodeCIDRs(base, podCIDR string) []string {
	return mergeStrings(nil, base, podCIDR)
}

func mergeStrings(existing []string, values ...string) []string {
	seen := make(map[string]bool, len(existing)+len(values))
	out := make([]string, 0, len(existing)+len(values))
	for _, value := range append(append([]string{}, existing...), values...) {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}

	return out
}

func endpointsFromIPs(ips []string, port int) []string {
	endpoints := make([]string, 0, len(ips))
	for _, ip := range ips {
		if ip != "" {
			endpoints = append(endpoints, fmt.Sprintf("%s:%d", ip, port))
		}
	}

	return endpoints
}

func tunnelMode(requiresEndpoint bool) string {
	if requiresEndpoint {
		return "endpoint"
	}

	return "direct-gateway"
}

func podCIDRForAllocation(base string, allocationID string) string {
	_, ipnet, err := net.ParseCIDR(base)
	if err != nil || ipnet == nil || ipnet.IP.To4() == nil {
		base = "10.241.0.0/16"
		_, ipnet, _ = net.ParseCIDR(base)
	}

	h := fnv.New32a()
	_, _ = h.Write([]byte(allocationID)) //nolint:errcheck
	third := byte(h.Sum32() % 250)
	ip := ipnet.IP.To4()

	return fmt.Sprintf("%d.%d.%d.0/24", ip[0], ip[1], third)
}

func routerIP(podCIDR string) string {
	ip, _, err := net.ParseCIDR(podCIDR)
	if err != nil || ip.To4() == nil {
		return ""
	}

	addr := ip.To4()
	addr[3] = 1

	return addr.String()
}

func ownedLabels(allocationID, component string) map[string]string {
	return map[string]string{
		labels.ManagedByLabel:    labels.AppName,
		labels.ComponentLabel:    component,
		labels.AllocationIDLabel: allocationID,
		labels.OwnedLabel:        "true",
	}
}

func mergeMap(existing map[string]string, values map[string]string) map[string]string {
	out := make(map[string]string, len(existing)+len(values))
	for key, value := range existing {
		out[key] = value
	}
	for key, value := range values {
		out[key] = value
	}

	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}
