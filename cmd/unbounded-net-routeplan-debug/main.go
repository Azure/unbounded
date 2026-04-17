// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"

	"github.com/Azure/unbounded-kube/internal/net/routeplan"
	statusv1alpha1 "github.com/Azure/unbounded-kube/internal/net/status/v1alpha1"
)

type clusterStatus struct {
	Nodes []statusv1alpha1.NodeStatusResponse `json:"nodes"`
}

type peerDebugOutput struct {
	Name                      string                `json:"name"`
	PeerType                  string                `json:"peerType"`
	SiteName                  string                `json:"siteName,omitempty"`
	Interface                 string                `json:"interface,omitempty"`
	Endpoint                  string                `json:"endpoint,omitempty"`
	PodCIDRGateways           []string              `json:"podCidrGateways"`
	HealthCheckEnabled        bool                  `json:"healthCheckEnabled"`
	ObservedHealthCheckStatus string                `json:"observedHealthCheckStatus,omitempty"`
	AllowedIPs                []string              `json:"allowedIPs,omitempty"`
	ExpectedDestinations      []expectedEntry       `json:"expectedDestinations"`
	Decisions                 []routeDecision       `json:"decisions,omitempty"`
	HealthCheckDecisions      []healthCheckDecision `json:"healthCheckDecisions,omitempty"`
}

type expectedEntry struct {
	Destination string `json:"destination"`
	Type        string `json:"type"`
	Source      string `json:"source"`
}

type routeDecision struct {
	Destination string `json:"destination"`
	Included    bool   `json:"included"`
	Reason      string `json:"reason"`
}

type healthCheckDecision struct {
	Destination string `json:"destination"`
	Expected    bool   `json:"expected"`
	Reason      string `json:"reason"`
}

type debugOutput struct {
	Node                 string            `json:"node"`
	PeerCount            int               `json:"peerCount"`
	Peers                []peerDebugOutput `json:"peers"`
	ExpectedIPv4Routes   []expectedRoute   `json:"expectedIPv4Routes"`
	ExpectedIPv6Routes   []expectedRoute   `json:"expectedIPv6Routes"`
	UnexpectedIPv4Routes []expectedRoute   `json:"unexpectedIPv4Routes,omitempty"`
	UnexpectedIPv6Routes []expectedRoute   `json:"unexpectedIPv6Routes,omitempty"`
}

type expectedRoute struct {
	Destination string `json:"destination"`
	Gateway     string `json:"gateway"`
	Device      string `json:"device"`
	Weight      int    `json:"weight"`
	Family      int    `json:"family"`
	Type        string `json:"type"`
	Source      string `json:"source"`
}

func main() {
	statusFile := flag.String("status-file", "", "Path to cluster status JSON file (for example: controller /status/json output)")
	nodeName := flag.String("node", "", "Node name to analyze")
	peerFilter := flag.String("peer", "", "Optional peer name filter; when set, only that peer is included")
	pretty := flag.Bool("pretty", true, "Pretty-print JSON output")

	flag.Parse()

	if strings.TrimSpace(*statusFile) == "" {
		fail("--status-file is required")
	}

	if strings.TrimSpace(*nodeName) == "" {
		fail("--node is required")
	}

	content, err := os.ReadFile(*statusFile)
	if err != nil {
		fail("read status file: %v", err)
	}

	var cluster clusterStatus
	if err := json.Unmarshal(content, &cluster); err != nil {
		fail("parse status file as cluster status: %v", err)
	}

	if len(cluster.Nodes) == 0 {
		fail("status file has no nodes")
	}

	nodesByName := make(map[string]statusv1alpha1.NodeStatusResponse, len(cluster.Nodes))
	for _, node := range cluster.Nodes {
		name := strings.TrimSpace(node.NodeInfo.Name)
		if name == "" {
			continue
		}

		nodesByName[name] = node
	}

	targetNodeName := strings.TrimSpace(*nodeName)

	target, ok := nodesByName[targetNodeName]
	if !ok {
		fail("node %q not found in status file", targetNodeName)
	}

	routeNodes := make(map[string]routeplan.Node, len(nodesByName))
	for name, node := range nodesByName {
		routeNodes[name] = routeplan.Node{
			Name:        node.NodeInfo.Name,
			SiteName:    node.NodeInfo.SiteName,
			PodCIDRs:    node.NodeInfo.PodCIDRs,
			InternalIPs: node.NodeInfo.InternalIPs,
			ExternalIPs: node.NodeInfo.ExternalIPs,
		}
	}

	requestedPeer := strings.TrimSpace(*peerFilter)

	selectedPeers := make([]statusv1alpha1.WireGuardPeerStatus, 0, len(target.Peers))
	for _, peer := range target.Peers {
		if requestedPeer != "" && !strings.EqualFold(strings.TrimSpace(peer.Name), requestedPeer) {
			continue
		}

		selectedPeers = append(selectedPeers, peer)
	}

	peerFound := requestedPeer == "" || len(selectedPeers) > 0

	routePeers := make([]routeplan.Peer, 0, len(selectedPeers))
	for _, peer := range selectedPeers {
		routePeers = append(routePeers, routeplan.Peer{
			Name:            peer.Name,
			PeerType:        peer.PeerType,
			SiteName:        peer.SiteName,
			Interface:       peer.Tunnel.Interface,
			Endpoint:        peer.Tunnel.Endpoint,
			PodCIDRGateways: peer.PodCIDRGateways,
			AllowedIPs:      append([]string(nil), peer.Tunnel.AllowedIPs...),
		})
	}

	peerOutput := make([]peerDebugOutput, 0, len(selectedPeers))
	for _, peer := range selectedPeers {
		rp := routeplan.Peer{
			Name:            peer.Name,
			PeerType:        peer.PeerType,
			SiteName:        peer.SiteName,
			Interface:       peer.Tunnel.Interface,
			Endpoint:        peer.Tunnel.Endpoint,
			PodCIDRGateways: peer.PodCIDRGateways,
			AllowedIPs:      append([]string(nil), peer.Tunnel.AllowedIPs...),
		}

		destinationEntries := expectedDestinationsForPeer(rp, routeNodes, routePeers)

		allowedIPs := append([]string(nil), peer.Tunnel.AllowedIPs...)
		sort.Strings(allowedIPs)

		destinations := make([]string, 0, len(destinationEntries))
		for _, destination := range destinationEntries {
			destinations = append(destinations, destination.Destination)
		}

		observedStatus := ""
		if peer.HealthCheck != nil {
			observedStatus = strings.TrimSpace(peer.HealthCheck.Status)
		}

		peerOutput = append(peerOutput, peerDebugOutput{
			Name:                      peer.Name,
			PeerType:                  peer.PeerType,
			SiteName:                  peer.SiteName,
			Interface:                 peer.Tunnel.Interface,
			Endpoint:                  peer.Tunnel.Endpoint,
			PodCIDRGateways:           append([]string{}, peer.PodCIDRGateways...),
			HealthCheckEnabled:        peer.HealthCheck != nil && peer.HealthCheck.Enabled,
			ObservedHealthCheckStatus: observedStatus,
			AllowedIPs:                allowedIPs,
			ExpectedDestinations:      destinationEntries,
			Decisions:                 explainPeerDecisions(rp, routeNodes),
			HealthCheckDecisions:      explainHealthCheckDecisions(peer.HealthCheck != nil && peer.HealthCheck.Enabled, destinations),
		})
	}

	if !peerFound {
		fail("peer %q not found on node %q", requestedPeer, targetNodeName)
	}

	sort.SliceStable(peerOutput, func(i, j int) bool {
		if peerOutput[i].Name != peerOutput[j].Name {
			return peerOutput[i].Name < peerOutput[j].Name
		}

		if peerOutput[i].PeerType != peerOutput[j].PeerType {
			return peerOutput[i].PeerType < peerOutput[j].PeerType
		}

		return peerOutput[i].Interface < peerOutput[j].Interface
	})

	expectedIPv4, expectedIPv6 := routeplan.BuildExpectedWireGuardRoutes(routePeers, routeNodes)
	expectedIPv4Routes := buildExpectedRouteOutput(expectedIPv4, routePeers, routeNodes)
	expectedIPv6Routes := buildExpectedRouteOutput(expectedIPv6, routePeers, routeNodes)
	unexpectedIPv4Routes, unexpectedIPv6Routes := buildUnexpectedRouteOutput(target, expectedIPv4Routes, expectedIPv6Routes)

	out := debugOutput{
		Node:                 targetNodeName,
		PeerCount:            len(peerOutput),
		Peers:                peerOutput,
		ExpectedIPv4Routes:   expectedIPv4Routes,
		ExpectedIPv6Routes:   expectedIPv6Routes,
		UnexpectedIPv4Routes: unexpectedIPv4Routes,
		UnexpectedIPv6Routes: unexpectedIPv6Routes,
	}

	encoder := json.NewEncoder(os.Stdout)
	if *pretty {
		encoder.SetIndent("", "  ")
	}

	if err := encoder.Encode(out); err != nil {
		fail("write output: %v", err)
	}
}

func sortExpectedRoutes(routes []expectedRoute) {
	sort.SliceStable(routes, func(i, j int) bool {
		if routes[i].Destination != routes[j].Destination {
			return routes[i].Destination < routes[j].Destination
		}

		if routes[i].Gateway != routes[j].Gateway {
			return routes[i].Gateway < routes[j].Gateway
		}

		if routes[i].Device != routes[j].Device {
			return routes[i].Device < routes[j].Device
		}

		return routes[i].Weight < routes[j].Weight
	})
}

func expectedDestinationsForPeer(peer routeplan.Peer, nodesByName map[string]routeplan.Node, peers []routeplan.Peer) []expectedEntry {
	sitePodCIDRs := routeplan.BuildSitePodCIDRSetBySite(nodesByName)

	peerByName := make(map[string]routeplan.Peer, len(peers))
	for _, routePeer := range peers {
		name := strings.TrimSpace(routePeer.Name)
		if name == "" {
			continue
		}

		peerByName[name] = routePeer
	}

	destinations := routeplan.ExpectedDestinationsForPeer(peer, nodesByName)

	entries := make([]expectedEntry, 0, len(destinations))
	for _, destination := range destinations {
		normalizedDestination, _ := routeplan.NormalizeRouteDestination(destination)
		entryType := destinationType(destination)
		entries = append(entries, expectedEntry{
			Destination: destination,
			Type:        entryType,
			Source:      sourceForPeerDestination(peer, normalizedDestination, peerByName, nodesByName, sitePodCIDRs, nil),
		})
	}

	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Destination != entries[j].Destination {
			return entries[i].Destination < entries[j].Destination
		}

		if entries[i].Type != entries[j].Type {
			return entries[i].Type < entries[j].Type
		}

		return entries[i].Source < entries[j].Source
	})

	return entries
}

func buildExpectedRouteOutput(routes []routeplan.ExpectedRoute, peers []routeplan.Peer, nodesByName map[string]routeplan.Node) []expectedRoute {
	peerByName := make(map[string]routeplan.Peer, len(peers))
	for _, peer := range peers {
		peerByName[strings.TrimSpace(peer.Name)] = peer
	}

	sitePodCIDRs := routeplan.BuildSitePodCIDRSetBySite(nodesByName)
	peerNamesByRouteKey := make(map[string][]string)
	buildRouteKey := func(destination, gateway, device string, family int) string {
		normalizedDestination, _ := routeplan.NormalizeRouteDestination(destination)
		normalizedGateway := routeplan.NormalizeIP(gateway)

		return strings.Join([]string{
			normalizedDestination,
			normalizedGateway,
			strings.TrimSpace(device),
			fmt.Sprintf("%d", family),
		}, "|")
	}

	for _, peer := range peers {
		name := strings.TrimSpace(peer.Name)
		if name == "" {
			continue
		}

		peerExpectedIPv4, peerExpectedIPv6 := routeplan.BuildExpectedWireGuardRoutes([]routeplan.Peer{peer}, nodesByName)
		for _, peerRoute := range append(peerExpectedIPv4, peerExpectedIPv6...) {
			key := buildRouteKey(peerRoute.Destination, peerRoute.Gateway, peerRoute.Device, peerRoute.Family)
			peerNamesByRouteKey[key] = append(peerNamesByRouteKey[key], name)
		}
	}

	output := make([]expectedRoute, 0, len(routes))
	for _, route := range routes {
		routeType := destinationType(route.Destination)
		source := "derived"

		normalizedDestination, _ := routeplan.NormalizeRouteDestination(route.Destination)
		if normalizedDestination != "" {
			key := buildRouteKey(route.Destination, route.Gateway, route.Device, route.Family)
			peerNames := peerNamesByRouteKey[key]
			sort.Strings(peerNames)

			if len(peerNames) > 1 {
				source = "multiple:" + strings.Join(peerNames, ",") + "/podCidr"
			} else if len(peerNames) == 1 {
				if peer, ok := peerByName[peerNames[0]]; ok {
					source = sourceForPeerDestination(peer, normalizedDestination, peerByName, nodesByName, sitePodCIDRs, nil)
				}
			}
		}

		output = append(output, expectedRoute{
			Destination: route.Destination,
			Gateway:     route.Gateway,
			Device:      route.Device,
			Weight:      route.Weight,
			Family:      route.Family,
			Type:        routeType,
			Source:      source,
		})
	}

	sortExpectedRoutes(output)

	return output
}

func destinationType(destination string) string {
	ones, bits, isCIDR := routeplan.CIDRMaskSize(destination)
	if isCIDR && ones == bits {
		return "host"
	}

	return "network"
}

func buildUnexpectedRouteOutput(
	target statusv1alpha1.NodeStatusResponse,
	expectedIPv4 []expectedRoute,
	expectedIPv6 []expectedRoute,
) ([]expectedRoute, []expectedRoute) {
	expectedByFamily := map[int]map[string]struct{}{
		4: make(map[string]struct{}, len(expectedIPv4)),
		6: make(map[string]struct{}, len(expectedIPv6)),
	}
	expectedByFamilyIgnoringWeight := map[int]map[string]struct{}{
		4: make(map[string]struct{}, len(expectedIPv4)),
		6: make(map[string]struct{}, len(expectedIPv6)),
	}

	buildKey := func(destination, gateway, device string, weight, family int) string {
		normalizedDestination, _ := routeplan.NormalizeRouteDestination(destination)
		normalizedGateway := routeplan.NormalizeIP(gateway)

		return strings.Join([]string{
			normalizedDestination,
			normalizedGateway,
			strings.TrimSpace(device),
			fmt.Sprintf("%d", weight),
			fmt.Sprintf("%d", family),
		}, "|")
	}
	buildKeyIgnoringWeight := func(destination, gateway, device string, family int) string {
		normalizedDestination, _ := routeplan.NormalizeRouteDestination(destination)
		normalizedGateway := routeplan.NormalizeIP(gateway)

		return strings.Join([]string{
			normalizedDestination,
			normalizedGateway,
			strings.TrimSpace(device),
			fmt.Sprintf("%d", family),
		}, "|")
	}

	for _, route := range expectedIPv4 {
		expectedByFamily[4][buildKey(route.Destination, route.Gateway, route.Device, route.Weight, 4)] = struct{}{}
		expectedByFamilyIgnoringWeight[4][buildKeyIgnoringWeight(route.Destination, route.Gateway, route.Device, 4)] = struct{}{}
	}

	for _, route := range expectedIPv6 {
		expectedByFamily[6][buildKey(route.Destination, route.Gateway, route.Device, route.Weight, 6)] = struct{}{}
		expectedByFamilyIgnoringWeight[6][buildKeyIgnoringWeight(route.Destination, route.Gateway, route.Device, 6)] = struct{}{}
	}

	isConnectedSelfWireGuardHostRoute := func(destination string, hop statusv1alpha1.NextHop) bool {
		if !isWireGuardInterfaceName(hop.Device) {
			return false
		}

		if routeplan.NormalizeIP(hop.Gateway) != "" {
			return false
		}

		ones, bits, ok := routeplan.CIDRMaskSize(destination)
		if !ok || ones != bits {
			return false
		}

		for _, routeType := range hop.RouteTypes {
			typeName := strings.ToLower(strings.TrimSpace(routeType.Type))
			if typeName == "connected" || typeName == "local" {
				return true
			}
		}

		return false
	}

	buildUnexpected := func(routes []statusv1alpha1.RouteEntry, family int) []expectedRoute {
		unexpected := make([]expectedRoute, 0)
		seen := make(map[string]struct{})

		for _, route := range routes {
			normalizedDestination, normalizedFamily := routeplan.NormalizeRouteDestination(route.Destination)
			if normalizedDestination == "" || normalizedFamily != family {
				continue
			}

			for _, hop := range route.NextHops {
				if !isWireGuardInterfaceName(hop.Device) {
					continue
				}

				if isConnectedSelfWireGuardHostRoute(normalizedDestination, hop) {
					continue
				}

				key := buildKey(normalizedDestination, hop.Gateway, hop.Device, hop.Weight, family)
				if _, ok := expectedByFamily[family][key]; ok {
					continue
				}

				keyIgnoringWeight := buildKeyIgnoringWeight(normalizedDestination, hop.Gateway, hop.Device, family)
				if _, ok := expectedByFamilyIgnoringWeight[family][keyIgnoringWeight]; ok {
					continue
				}

				if _, exists := seen[key]; exists {
					continue
				}

				seen[key] = struct{}{}
				source := "observed"

				if hop.Info != nil {
					parts := []string{strings.TrimSpace(hop.Info.ObjectType), strings.TrimSpace(hop.Info.ObjectName)}
					head := strings.Join(parts[:], ":")
					head = strings.Trim(head, ":")

					routeType := strings.TrimSpace(hop.Info.RouteType)
					if head != "" && routeType != "" {
						source = head + "/" + routeType
					} else if head != "" {
						source = head
					} else if routeType != "" {
						source = routeType
					}
				}

				unexpected = append(unexpected, expectedRoute{
					Destination: normalizedDestination,
					Gateway:     routeplan.NormalizeIP(hop.Gateway),
					Device:      strings.TrimSpace(hop.Device),
					Weight:      hop.Weight,
					Family:      family,
					Type:        destinationType(normalizedDestination),
					Source:      source,
				})
			}
		}

		sortExpectedRoutes(unexpected)

		return unexpected
	}

	return buildUnexpected(target.RoutingTable.Routes, 4), buildUnexpected(target.RoutingTable.Routes, 6)
}

func isWireGuardInterfaceName(device string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(device)), "wg")
}

func sourceForPeerDestination(
	peer routeplan.Peer,
	normalizedDestination string,
	peerByName map[string]routeplan.Peer,
	nodesByName map[string]routeplan.Node,
	sitePodCIDRs map[string]map[string]struct{},
	siteNodeCIDRs map[string]map[string]struct{},
) string {
	if normalizedDestination == "" {
		return "derived"
	}

	peerName := strings.TrimSpace(peer.Name)
	if peerName == "" {
		return "derived"
	}

	info := routeplan.ClassifyRouteInfoForPeerDestination(
		normalizedDestination,
		[]string{peerName},
		peerByName,
		nodesByName,
		sitePodCIDRs,
		siteNodeCIDRs,
		nil,
	)
	if info == nil {
		return "derived"
	}

	objectType := strings.ToLower(strings.TrimSpace(info.ObjectType))
	if objectType == "" {
		objectType = "peer"
	}

	objectName := strings.TrimSpace(info.ObjectName)
	if objectName == "" {
		return "derived"
	}

	routeType := strings.TrimSpace(info.RouteType)
	if routeType == "" {
		routeType = "podCidr"
	}

	return objectType + ":" + objectName + "/" + routeType
}

func fail(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)

	os.Exit(1)
}

func explainPeerDecisions(peer routeplan.Peer, nodesByName map[string]routeplan.Node) []routeDecision {
	expected := routeplan.ExpectedDestinationsForPeer(peer, nodesByName)

	expectedSet := make(map[string]struct{}, len(expected))
	for _, destination := range expected {
		expectedSet[destination] = struct{}{}
	}

	addDecision := func(out *[]routeDecision, destination, reason string) {
		normalized, _ := routeplan.NormalizeRouteDestination(destination)
		if normalized == "" {
			return
		}

		_, included := expectedSet[normalized]
		*out = append(*out, routeDecision{
			Destination: normalized,
			Included:    included,
			Reason:      reason,
		})
	}

	decisions := make([]routeDecision, 0)
	peerName := strings.TrimSpace(peer.Name)
	peerNode, hasPeerNode := nodesByName[peerName]

	if hasPeerNode {
		for _, podCIDR := range peerNode.PodCIDRs {
			addDecision(&decisions, podCIDR, "peer podCIDR")

			if host := routeplan.FirstUsableHostCIDRFromCIDR(podCIDR); host != "" {
				addDecision(&decisions, host, "first usable host in peer podCIDR")
			}
		}

		endpointHost := endpointHost(peer.Endpoint)
		reachesViaExternalIP := false

		for _, ip := range peerNode.ExternalIPs {
			if routeplan.NormalizeIP(ip) == endpointHost && endpointHost != "" {
				reachesViaExternalIP = true
				break
			}
		}

		for _, ip := range peerNode.InternalIPs {
			hostCIDR := routeplan.HostCIDRForIP(ip)

			reason := "internal IP host route excluded by policy"
			if strings.EqualFold(peer.PeerType, "gateway") && reachesViaExternalIP {
				reason = "gateway peer reached via external endpoint: internal IP host route allowed"
			}

			addDecision(&decisions, hostCIDR, reason)
		}

		for _, ip := range peerNode.ExternalIPs {
			addDecision(&decisions, routeplan.HostCIDRForIP(ip), "external IP host routes are never expected")
		}
	}

	for _, allowedIP := range peer.AllowedIPs {
		normalized, _ := routeplan.NormalizeRouteDestination(allowedIP)
		if normalized == "" {
			continue
		}

		reason := "allowed IP candidate"
		if normalized == "0.0.0.0/0" || normalized == "::/0" {
			reason = "default route excluded"
		}

		addDecision(&decisions, normalized, reason)
	}

	if strings.EqualFold(peer.PeerType, "gateway") {
		for _, gateway := range peer.PodCIDRGateways {
			if host := routeplan.HostCIDRForIP(gateway); host != "" {
				addDecision(&decisions, host, "gateway overlay host route")
			}
		}
	}

	seen := make(map[string]struct{}, len(decisions))

	unique := make([]routeDecision, 0, len(decisions))
	for _, decision := range decisions {
		key := decision.Destination + "|" + decision.Reason
		if _, exists := seen[key]; exists {
			continue
		}

		seen[key] = struct{}{}

		unique = append(unique, decision)
	}

	sort.SliceStable(unique, func(i, j int) bool {
		if unique[i].Destination != unique[j].Destination {
			return unique[i].Destination < unique[j].Destination
		}

		if unique[i].Included != unique[j].Included {
			return unique[i].Included
		}

		return unique[i].Reason < unique[j].Reason
	})

	return unique
}

func endpointHost(endpoint string) string {
	trimmed := strings.TrimSpace(endpoint)
	if trimmed == "" {
		return ""
	}

	if strings.HasPrefix(trimmed, "[") {
		end := strings.Index(trimmed, "]")
		if end <= 0 {
			return ""
		}

		return routeplan.NormalizeIP(trimmed[1:end])
	}

	host := trimmed
	if strings.Count(trimmed, ":") == 1 {
		parts := strings.SplitN(trimmed, ":", 2)
		host = parts[0]
	} else if strings.Count(trimmed, ":") > 1 {
		if _, _, err := net.SplitHostPort(trimmed); err == nil {
			h, _, splitErr := net.SplitHostPort(trimmed)
			if splitErr == nil {
				host = h
			}
		}
	}

	return routeplan.NormalizeIP(host)
}

func explainHealthCheckDecisions(healthCheckEnabled bool, expectedDestinations []string) []healthCheckDecision {
	decisions := make([]healthCheckDecision, 0, len(expectedDestinations))
	for _, destination := range expectedDestinations {
		ones, bits, isCIDR := routeplan.CIDRMaskSize(destination)
		isHostRoute := isCIDR && ones == bits

		reason := "peer health check disabled"
		expected := false

		if isHostRoute {
			reason = "host route bootstrap: health check not expected"
		} else if healthCheckEnabled {
			expected = true
			reason = "non-host route for health-check-enabled peer"
		}

		decisions = append(decisions, healthCheckDecision{
			Destination: destination,
			Expected:    expected,
			Reason:      reason,
		})
	}

	sort.SliceStable(decisions, func(i, j int) bool {
		if decisions[i].Destination != decisions[j].Destination {
			return decisions[i].Destination < decisions[j].Destination
		}

		if decisions[i].Expected != decisions[j].Expected {
			return decisions[i].Expected
		}

		return decisions[i].Reason < decisions[j].Reason
	})

	return decisions
}
