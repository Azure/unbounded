// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Azure/unbounded/internal/racer"
)

const (
	annotationPrefix   = racer.MetadataPrefix
	universeAnnotation = racer.UniverseKey
	dataplaneLabel     = racer.DataplaneLabelKey
)

func universe(annotations map[string]string) string { return annotations[universeAnnotation] }

// resolveOrigin resolves the control-plane bootstrap Service's virtual address.
// Cache origins are local Unix sockets and never use this resolver.
func resolveOrigin(s *corev1.Service, port string) (originSpec, error) {
	bad := func(reason string) (originSpec, error) {
		return originSpec{}, fmt.Errorf("bootstrap Service %s/%s: %s", s.Namespace, s.Name, reason)
	}
	if s.DeletionTimestamp != nil || s.Spec.Type == corev1.ServiceTypeExternalName || s.Spec.ClusterIP == "" || s.Spec.ClusterIP == corev1.ClusterIPNone {
		return bad("requires a live, non-headless ClusterIP Service")
	}

	var selected *corev1.ServicePort

	for i := range s.Spec.Ports {
		p := &s.Spec.Ports[i]

		number, err := strconv.ParseUint(port, 10, 16)
		if p.Name == port || err == nil && int32(number) == p.Port {
			if selected != nil {
				return bad("ambiguous Service port")
			}

			selected = p
		}
	}

	if selected == nil || selected.Port < 1 || selected.Port > 65535 || (selected.Protocol != "" && selected.Protocol != corev1.ProtocolTCP) {
		return bad("port must select a TCP Service port")
	}

	result := originSpec{}

	ips := s.Spec.ClusterIPs
	if len(ips) == 0 {
		ips = []string{s.Spec.ClusterIP}
	}

	for _, raw := range ips {
		ip, err := netip.ParseAddr(raw)
		if err != nil || ip.Zone() != "" || ip.Is4In6() || !ip.IsGlobalUnicast() {
			return bad("invalid ClusterIP")
		}

		address := netip.AddrPortFrom(ip, uint16(selected.Port)).String()
		if ip.Is4() {
			result.IPv4 = address
		} else {
			result.IPv6 = address
		}
	}

	return result, nil
}

func nodeReady(n *corev1.Node) bool {
	if n.DeletionTimestamp != nil {
		return false
	}

	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}

	return false
}

// A starting Pod needs configuration before its readiness probe can succeed.
// P2PCache status separately requires readiness and fresh worker acknowledgments.
func podAvailable(p *corev1.Pod) bool {
	ip := net.ParseIP(p.Status.PodIP)
	return p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning && ip != nil && !ip.IsUnspecified() && !ip.IsMulticast() && !ip.IsLoopback() && !ip.Equal(net.IPv4bcast)
}

func buildInventory(name string, previous *generation, nodes []corev1.Node, pods []corev1.Pod) (*generation, error) {
	g := &generation{Format: generationFormat, Universe: name, Nodes: map[string]member{}, SlotHistory: map[string]uint32{}}

	if previous != nil {
		podKeys := map[string]types.NamespacedName{}

		for _, p := range pods {
			if p.UID != "" {
				podKeys[string(p.UID)] = types.NamespacedName{Namespace: p.Namespace, Name: p.Name}
			}
		}

		g.Revision = previous.Revision
		for key, n := range previous.Nodes {
			n.IP = ""
			// Only the exact retained UID may complete historical removal identity.
			if p, ok := podKeys[n.PodUID]; ok && (n.PodNamespace == "" || n.PodName == "") {
				n.PodNamespace, n.PodName = p.Namespace, p.Name
			}

			g.Nodes[key] = n
		}

		for key, n := range previous.SlotHistory {
			g.SlotHistory[key] = n
		}
	}

	ready := map[string]bool{}

	for _, n := range nodes {
		if !racer.NodeEligible(&n) || n.Labels[corev1.LabelOSStable] != "linux" || racer.NodeUniverse(&n) != name {
			continue
		}

		if n.UID == "" {
			return nil, fmt.Errorf("node %s has no UID", n.Name)
		}

		id := identity("node", string(n.UID))
		if old, ok := g.Nodes[n.Name]; ok && old.ID != id {
			old.IP = ""
			g.Nodes["deleted/"+old.ID] = old
			delete(g.Nodes, n.Name)
		}

		fabric := n.Annotations[annotationPrefix+"fabric"]
		if len(fabric) > 256 || strings.IndexFunc(fabric, func(r rune) bool { return r < 0x21 || r > 0x7e }) >= 0 {
			return nil, fmt.Errorf("node %s has invalid fabric", n.Name)
		}

		m := g.Nodes[n.Name]
		m.ID, m.Fabric = id, fabric
		g.Nodes[n.Name] = m
		ready[n.Name] = nodeReady(&n)
	}

	selected := map[string][]corev1.Pod{}
	podIdentities := map[string]string{}

	for _, old := range g.Nodes {
		if old.PodUID != "" {
			podIdentities[old.PodUID] = old.ID
		}
	}

	for _, p := range pods {
		if !ready[p.Spec.NodeName] || !podAvailable(&p) || p.Labels[dataplaneLabel] != "true" || p.Labels[universeAnnotation] != name || p.Spec.ServiceAccountName != "racer-dataplane" || p.UID == "" {
			continue
		}
		// A recreated Node cannot adopt the former Node's still-running Pod.
		if oldID := podIdentities[string(p.UID)]; oldID != "" && oldID != g.Nodes[p.Spec.NodeName].ID {
			continue
		}

		owned := false

		for _, o := range p.OwnerReferences {
			if o.APIVersion == "apps/v1" && o.Kind == "DaemonSet" && o.Controller != nil && *o.Controller {
				owned = true
			}
		}

		if owned {
			selected[p.Spec.NodeName] = append(selected[p.Spec.NodeName], p)
		}
	}

	if len(selected) > 100000 {
		return nil, fmt.Errorf("universe exceeds 100000 participants")
	}

	for node, candidates := range selected {
		// Prefer a ready Pod, then the oldest, then its name during overlap.
		sort.Slice(candidates, func(i, j int) bool {
			ri, rj := podReady(&candidates[i]), podReady(&candidates[j])
			if ri != rj {
				return ri
			}

			if !candidates[i].CreationTimestamp.Equal(&candidates[j].CreationTimestamp) {
				return candidates[i].CreationTimestamp.Before(&candidates[j].CreationTimestamp)
			}

			return candidates[i].Name < candidates[j].Name
		})

		n := g.Nodes[node]
		n.IP, n.PodUID = candidates[0].Status.PodIP, string(candidates[0].UID)
		n.PodNamespace, n.PodName = candidates[0].Namespace, candidates[0].Name
		g.Nodes[node] = n
	}

	return g, nil
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}

	return false
}
