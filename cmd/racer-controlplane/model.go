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
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/Azure/unbounded/internal/racer"
)

// Service annotations and volume validation.

const (
	annotationPrefix          = racer.MetadataPrefix
	universeAnnotation        = racer.UniverseKey
	originServiceAnnotation   = racer.OriginServiceAnnotationKey
	originNamespaceAnnotation = racer.OriginNamespaceAnnotationKey
	originPortAnnotation      = racer.OriginPortAnnotationKey
	dataplaneLabel            = racer.DataplaneLabelKey
)

func universe(annotations map[string]string) string {
	return annotations[universeAnnotation]
}

func number(a map[string]string, key string, fallback, low, high uint64) (uint64, error) {
	text, exists := a[annotationPrefix+key]
	if !exists {
		return fallback, nil
	}

	n, err := strconv.ParseUint(text, 10, 64)
	if err != nil || n < low || n > high {
		return 0, fmt.Errorf("%s must be in %d..%d", annotationPrefix+key, low, high)
	}

	return n, nil
}

func parseVolume(s *corev1.Service, port int32) (*volumeSpec, error) {
	a := s.Annotations
	if universe(a) == "" || len(validation.IsValidLabelValue(universe(a))) != 0 {
		return nil, fmt.Errorf("volume Service requires an explicit %s annotation containing the mapped Site universe", universeAnnotation)
	}

	if len(s.Spec.Selector) == 0 || s.Spec.Selector[dataplaneLabel] != "true" {
		return nil, fmt.Errorf("service %s/%s must select %s=true dataplane Pods", s.Namespace, s.Name, dataplaneLabel)
	}

	if s.Spec.Selector[universeAnnotation] != universe(s.Annotations) {
		return nil, fmt.Errorf("service selector must include racer.unbounded-cloud.io/universe=%s to prevent cross-universe client traffic", universe(s.Annotations))
	}

	if s.Spec.Type == corev1.ServiceTypeExternalName || s.Spec.ClusterIP == corev1.ClusterIPNone || s.Spec.PublishNotReadyAddresses {
		return nil, fmt.Errorf("volume Service must be non-headless and must not publish unready addresses")
	}

	if len(s.Spec.Ports) != 1 || (s.Spec.Ports[0].Protocol != "" && s.Spec.Ports[0].Protocol != corev1.ProtocolTCP) {
		return nil, fmt.Errorf("volume Service must have exactly one TCP port")
	}

	if _, err := originReference(s); err != nil {
		return nil, err
	}

	p, err := number(a, "slot-count", uint64(defaultSlots), 1, 262144)
	if err != nil {
		return nil, err
	}

	cache, err := number(a, "cache-generation", 1, 0, ^uint64(0))
	if err != nil {
		return nil, err
	}

	algorithm, err := number(a, "routing-algorithm", 2, 2, 2)
	if err != nil {
		return nil, err
	}

	attempts, err := number(a, "max-candidate-attempts", 3, 1, 8)
	if err != nil {
		return nil, err
	}

	if _, ok := a[annotationPrefix+"legacy-peer-wire"]; ok {
		return nil, fmt.Errorf("legacy-peer-wire is no longer supported")
	}

	return &volumeSpec{ID: s.Namespace + "/" + s.Name, Port: port, Slots: uint32(p), Cache: cache, Algorithm: uint32(algorithm), Attempts: uint32(attempts)}, nil
}

func originReference(s *corev1.Service) (types.NamespacedName, error) {
	ref := types.NamespacedName{Namespace: s.Annotations[originNamespaceAnnotation], Name: s.Annotations[originServiceAnnotation]}
	if ref.Namespace == "" {
		ref.Namespace = s.Namespace
	}

	if len(validation.IsDNS1035Label(ref.Name)) != 0 || len(validation.IsDNS1123Label(ref.Namespace)) != 0 {
		return ref, fmt.Errorf("origin-service and origin-namespace must name a Kubernetes Service")
	}

	if ref.Namespace == s.Namespace && ref.Name == s.Name {
		return ref, fmt.Errorf("origin must be separate from the volume Service")
	}

	if s.Annotations[originPortAnnotation] == "" {
		return ref, fmt.Errorf("origin-port is required (Service port name or number)")
	}

	return ref, nil
}

// Resolve only Kubernetes Service virtual addresses, never endpoints or DNS.
func resolveOrigin(s *corev1.Service, port string) (originSpec, error) {
	bad := func(reason string) (originSpec, error) {
		return originSpec{}, fmt.Errorf("origin Service %s/%s: %s", s.Namespace, s.Name, reason)
	}
	if s.DeletionTimestamp != nil || s.Spec.Type == corev1.ServiceTypeExternalName || s.Spec.ClusterIP == "" || s.Spec.ClusterIP == corev1.ClusterIPNone {
		return bad("requires a live, non-headless ClusterIP Service")
	}

	if isVolume(s) {
		return bad("must not be a Racer volume Service")
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
		return bad("origin-port must select a TCP Service port")
	}

	result := originSpec{Identity: s.Namespace + "/" + s.Name + ":" + strconv.Itoa(int(selected.Port))}

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

func isVolume(s *corev1.Service) bool {
	_, ok := s.Annotations[originServiceAnnotation]
	return ok
}

// Node and Pod availability.

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

// Pod readiness is deliberately not a prerequisite for initial configuration:
// a listener readiness probe may need that configuration to become ready.
// Kubernetes EndpointSlices independently exclude unready Pods from clients.
func podAvailable(p *corev1.Pod) bool {
	ip := net.ParseIP(p.Status.PodIP)
	return p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning && ip != nil && !ip.IsUnspecified() && !ip.IsMulticast() && !ip.IsLoopback() && !ip.Equal(net.IPv4bcast)
}

// Generation assembly and persistent listener allocation.

func buildGenerationReserved(name string, previous *generation, nodes []corev1.Node, pods []corev1.Pod, services []corev1.Service, reserved reservedPorts) (*generation, *corev1.Service, error) {
	if name == "" {
		return nil, nil, fmt.Errorf("universe must not be empty")
	}

	services = append([]corev1.Service(nil), services...)
	sort.Slice(services, func(i, j int) bool {
		return services[i].Namespace+"/"+services[i].Name < services[j].Namespace+"/"+services[j].Name
	})

	var selected []corev1.Service

	for _, s := range services {
		if isVolume(&s) && universe(s.Annotations) == name && s.DeletionTimestamp == nil {
			selected = append(selected, s)
		}
	}

	ports, service, err := allocateListenerPorts(previous, selected, reserved)
	if err != nil {
		return nil, service, err
	}

	if len(selected) == 0 {
		return buildSingleGeneration(name, previous, nodes, pods, nil, ports)
	}

	var result *generation

	for i := range selected {
		var prior *generation

		if previous != nil {
			copy := *previous
			copy.Volume, copy.Owners, copy.Additional = nil, nil, nil
			prior = &copy
		}

		if result != nil {
			copy := *result
			copy.Volume, copy.Owners, copy.Additional = nil, nil, nil
			prior = &copy
		}

		if previous != nil {
			for _, v := range previous.volumes() {
				if v.Volume.ID == selected[i].Namespace+"/"+selected[i].Name {
					prior.Volume, prior.Owners = v.Volume, v.Owners
				}
			}
		}

		g, _, err := buildSingleGeneration(name, prior, nodes, pods, selected[i:i+1], ports)
		if err != nil {
			return nil, &selected[i], err
		}

		ref, err := originReference(&selected[i])
		if err != nil {
			return nil, &selected[i], err
		}

		var origin *corev1.Service

		for j := range services {
			if services[j].Namespace == ref.Namespace && services[j].Name == ref.Name {
				origin = &services[j]
				break
			}
		}

		if origin == nil {
			return nil, &selected[i], fmt.Errorf("origin Service %s not found", ref)
		}

		g.Volume.Origin, err = resolveOrigin(origin, selected[i].Annotations[originPortAnnotation])
		if err != nil {
			return nil, &selected[i], err
		}

		for _, n := range g.Nodes {
			if n.IP != "" && g.Volume.Origin.address(n.IP) == "" {
				return nil, &selected[i], fmt.Errorf("origin Service %s has no ClusterIP matching Pod IP %s", ref, n.IP)
			}
		}

		if result == nil {
			result = g
			continue
		}

		for name, node := range g.Nodes {
			old := result.Nodes[name]
			if old.IP != "" && node.IP != "" && (old.IP != node.IP || old.PodUID != node.PodUID) {
				return nil, &selected[i], fmt.Errorf("volume selectors choose different dataplane processes on node %s", name)
			}

			if node.IP != "" {
				result.Nodes[name] = node
			}
		}

		result.Ports, result.SlotHistory = g.Ports, g.SlotHistory
		result.Additional = append(result.Additional, volumeState{g.Volume, g.Owners})
	}

	return result, &selected[0], nil
}

// Reservations are deployment policy, not volume allocation history. Always
// retain the default during rolling changes; nil also reserves 9090.
type reservedPorts map[int32]bool

func (ports reservedPorts) contains(port int32) bool {
	return port == 9090 || ports[port]
}

func parseReservedPorts(text string) (reservedPorts, error) {
	ports := reservedPorts{}

	for _, field := range strings.Split(text, ",") {
		port, err := strconv.ParseUint(field, 10, 16)
		if err != nil || port == 0 || ports[int32(port)] {
			return nil, fmt.Errorf("reserved-management-ports must be distinct comma-separated ports in 1..65535")
		}

		ports[int32(port)] = true
	}

	return ports, nil
}

// selected is in namespace/name order. History (including removed Services) owns
// its ports permanently; new explicit requests take precedence over new automatic
// allocations, regardless of where those requests occur in the sorted batch.
func allocateListenerPorts(previous *generation, selected []corev1.Service, reserved reservedPorts) (map[string]int32, *corev1.Service, error) {
	ports := map[string]int32{}
	used := map[int32]bool{}

	if previous != nil {
		for id, port := range previous.Ports {
			ports[id] = port
			used[port] = true
		}
	}

	for i := range selected {
		service := &selected[i]
		id := service.Namespace + "/" + service.Name

		port := ports[id]
		if reserved.contains(port) {
			return nil, service, fmt.Errorf("historical listener port %d for %s is reserved for management; allocation is immutable", port, id)
		}

		requested, err := number(service.Annotations, "listener-port", 0, 1024, 65535)
		if err != nil {
			return nil, service, err
		}

		if requested != 0 && port != 0 && port != int32(requested) {
			return nil, service, fmt.Errorf("listener-port is immutable once allocated")
		}

		if reserved.contains(int32(requested)) {
			return nil, service, fmt.Errorf("listener port %d is reserved for management", requested)
		}

		if port == 0 && requested != 0 {
			port = int32(requested)
			if used[port] {
				return nil, service, fmt.Errorf("listener port %d already reserved", port)
			}

			ports[id], used[port] = port, true
		}
	}
	// Every explicit reservation is now known. Assign remaining Services the
	// lowest free automatic ports, retaining deterministic lexical priority.
	next := int32(10000)

	for i := range selected {
		service := &selected[i]

		id := service.Namespace + "/" + service.Name
		if ports[id] != 0 {
			continue
		}

		for next <= 29999 && (used[next] || reserved.contains(next)) {
			next++
		}

		if next > 29999 {
			return nil, service, fmt.Errorf("listener port range exhausted")
		}

		ports[id], used[next] = next, true
		next++
	}

	return ports, nil, nil
}

func buildSingleGeneration(name string, previous *generation, nodes []corev1.Node, pods []corev1.Pod, services []corev1.Service, ports map[string]int32) (*generation, *corev1.Service, error) {
	g := &generation{Format: generationFormat, Universe: name, Nodes: map[string]member{}, SlotHistory: map[string]uint32{}, Ports: map[string]int32{}}
	for id, port := range ports {
		g.Ports[id] = port
	}

	if previous != nil {
		g.Revision = previous.Revision
		for key, n := range previous.Nodes {
			n.IP = ""
			g.Nodes[key] = n
		}

		for key, n := range previous.SlotHistory {
			g.SlotHistory[key] = n
		}
	}

	ready := map[string]bool{}
	ineligible := map[string]bool{}

	for _, n := range nodes {
		if !racer.NodeEligible(&n) {
			ineligible[n.Name] = true
			continue
		}

		if racer.NodeUniverse(&n) != name {
			continue
		}

		id := identity("node", string(n.UID))
		if n.UID == "" {
			return nil, nil, fmt.Errorf("node %s has no UID", n.Name)
		}
		// Retain a tombstone under a synthetic key when a Kubernetes Node is
		// recreated with the same name but a new bootstrap identity.
		if old, ok := g.Nodes[n.Name]; ok && old.ID != id {
			old.IP = ""
			g.Nodes["deleted/"+old.ID] = old
			// The old Pod's bootstrap identity belongs only to its tombstone.
			// A replacement Node gains authority from actual Pod selection below.
			delete(g.Nodes, n.Name)
		}

		fabric := n.Annotations[annotationPrefix+"fabric"]
		if len(fabric) > 256 || strings.IndexFunc(fabric, func(r rune) bool { return r < 0x21 || r > 0x7e }) >= 0 {
			return nil, nil, fmt.Errorf("node %s has invalid fabric", n.Name)
		}

		g.Nodes[n.Name] = member{ID: id, Fabric: fabric, PodUID: g.Nodes[n.Name].PodUID}
		ready[n.Name] = nodeReady(&n)
	}

	var service *corev1.Service

	for i := range services {
		s := &services[i]
		if !isVolume(s) || universe(s.Annotations) != name || s.DeletionTimestamp != nil {
			continue
		}

		if service != nil {
			return nil, nil, fmt.Errorf("internal single-volume builder received multiple Services")
		}

		service = s
	}

	var selector labels.Selector
	if service != nil {
		selector = labels.SelectorFromSet(service.Spec.Selector)
	}

	matches := func(p *corev1.Pod) bool {
		if service == nil {
			return p.Labels[dataplaneLabel] == "true" && p.Labels[universeAnnotation] == name && p.Spec.ServiceAccountName == "racer-dataplane" && p.UID != ""
		}

		return p.Namespace == service.Namespace && selector.Matches(labels.Set(p.Labels))
	}
	selected := map[string][]corev1.Pod{}
	podIdentities := map[string]string{}

	for _, old := range g.Nodes {
		if old.PodUID != "" {
			podIdentities[old.PodUID] = old.ID
		}
	}

	for _, p := range pods {
		// A departing process keeps its previous PodUID authority solely to
		// receive removal commands. It must not block the old Site's drain.
		if ineligible[p.Spec.NodeName] || !podAvailable(&p) {
			continue
		}

		// A recreated Node cannot adopt the former Node's still-running Pod.
		// Its token belongs to the tombstone until actual Pod deletion.
		if oldID := podIdentities[string(p.UID)]; oldID != "" && oldID != g.Nodes[p.Spec.NodeName].ID {
			continue
		}

		if service != nil && matches(&p) {
			if _, ok := ready[p.Spec.NodeName]; !ok && p.Spec.NodeName != "" {
				if old, exists := g.Nodes[p.Spec.NodeName]; exists && old.PodUID != "" && old.PodUID == string(p.UID) {
					continue
				}

				return nil, service, fmt.Errorf("selected Pod %s/%s is on a Node outside universe %s", p.Namespace, p.Name, name)
			}
		}

		if !matches(&p) || !ready[p.Spec.NodeName] {
			continue
		}
		// The Pod must actually be controlled by a DaemonSet, rather than an
		// arbitrary application that happens to share the Service's labels.
		owned := false

		for _, o := range p.OwnerReferences {
			if o.APIVersion == "apps/v1" && o.Kind == "DaemonSet" && o.Controller != nil && *o.Controller {
				owned = true
			}
		}

		if !owned {
			if service == nil {
				continue
			}

			return nil, service, fmt.Errorf("selected Pod %s/%s is not DaemonSet-controlled", p.Namespace, p.Name)
		}

		selected[p.Spec.NodeName] = append(selected[p.Spec.NodeName], p)
	}

	active := make([]string, 0, len(selected))
	for node, candidates := range selected {
		// Overlapping rollout Pods share RACER_NODE. Prefer a ready Pod, then
		// the oldest, keeping the endpoint stable until replacement is ready.
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
		n.IP = candidates[0].Status.PodIP
		n.PodUID = string(candidates[0].UID)
		g.Nodes[node] = n
		active = append(active, node)
	}

	if len(active) > 100000 {
		return nil, service, fmt.Errorf("universe exceeds 100000 participants")
	}

	if service == nil {
		return g, nil, nil
	}

	id := service.Namespace + "/" + service.Name

	v, err := parseVolume(service, g.Ports[id])
	if err != nil {
		return nil, service, err
	}

	if old := g.SlotHistory[id]; old != 0 && old != v.Slots {
		return nil, service, fmt.Errorf("slot-count for %s is immutable; use a new Service name", id)
	}

	g.SlotHistory[id] = v.Slots
	g.Volume = v

	if len(active) != 0 {
		var old []string
		if previous != nil && previous.Volume != nil && previous.Volume.ID == v.ID {
			old = previous.Owners
		}

		g.Owners, err = place(v.Slots, active, old)
		if err != nil {
			return nil, service, err
		}
	}

	return g, service, nil
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}

	return false
}
