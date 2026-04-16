// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/Azure/unbounded-kube/internal/net/routeplan"
)

// routeKey uniquely identifies an expected route by destination, gateway,
// device, distance, and weight.
type routeKey struct {
	destination string
	gateway     string
	device      string
	distance    int
	weight      int
}

// expectedRoute pairs a destination with its expected next-hop, classification
// info, and the peer names that contribute to it.
type expectedRoute struct {
	destination string
	nextHop     NextHop
	info        *NextHopInfo
	peerNames   []string
}

// peerRouteExpectation describes the destinations a single peer is expected to
// contribute, along with gateway and interface matching criteria.
type peerRouteExpectation struct {
	peerName            string
	interfaceName       string
	overlayGateways     map[string]struct{}
	endpointGateway     string
	destinations        map[string]struct{}
	matchAnyDestination bool
}

// annotationContext holds precomputed maps derived from informers that are
// needed by route annotation logic.
type annotationContext struct {
	routeNodes             map[string]routeplan.Node
	directSitePeerings     map[string]map[string]struct{}
	siteNodeCIDRs          map[string]map[string]struct{}
	gatewayPoolRoutedCIDRs map[string]map[string]struct{}
	sitePodCIDRs           map[string]map[string]struct{}
}

// buildAnnotationContext builds the annotation context maps from informers.
// It is best-effort -- if any informer is nil or data is missing, the
// corresponding map is returned empty rather than causing a failure.
func buildAnnotationContext(
	siteInformer, sliceInformer, gatewayPoolInformer, sitePeeringInformer cache.SharedIndexInformer,
) *annotationContext {
	ctx := &annotationContext{
		routeNodes:             make(map[string]routeplan.Node),
		directSitePeerings:     make(map[string]map[string]struct{}),
		siteNodeCIDRs:          make(map[string]map[string]struct{}),
		gatewayPoolRoutedCIDRs: make(map[string]map[string]struct{}),
	}

	// Build routeNodes from SiteNodeSlice informer (nodes in slices)
	if sliceInformer != nil {
		for _, item := range sliceInformer.GetStore().List() {
			unstr, ok := item.(*unstructured.Unstructured)
			if !ok {
				continue
			}

			specObj, ok := unstr.Object["spec"].(map[string]interface{})
			if !ok {
				continue
			}

			siteName, _ := specObj["siteName"].(string) //nolint:errcheck

			nodesObj, ok := specObj["nodes"].([]interface{})
			if !ok {
				continue
			}

			for _, nodeObj := range nodesObj {
				nodeMap, ok := nodeObj.(map[string]interface{})
				if !ok {
					continue
				}

				nodeName, _ := nodeMap["name"].(string) //nolint:errcheck

				nodeName = strings.TrimSpace(nodeName)
				if nodeName == "" {
					continue
				}

				node := routeplan.Node{
					Name:     nodeName,
					SiteName: siteName,
				}

				if podCIDRs, ok := nodeMap["podCIDRs"].([]interface{}); ok {
					for _, cidr := range podCIDRs {
						if s, ok := cidr.(string); ok {
							node.PodCIDRs = append(node.PodCIDRs, s)
						}
					}
				}

				if ips, ok := nodeMap["internalIPs"].([]interface{}); ok {
					for _, ip := range ips {
						if s, ok := ip.(string); ok {
							node.InternalIPs = append(node.InternalIPs, s)
						}
					}
				}

				if ips, ok := nodeMap["externalIPs"].([]interface{}); ok {
					for _, ip := range ips {
						if s, ok := ip.(string); ok {
							node.ExternalIPs = append(node.ExternalIPs, s)
						}
					}
				}

				ctx.routeNodes[nodeName] = node
			}
		}
	}

	// Also extract gateway nodes from GatewayPool informer
	if gatewayPoolInformer != nil {
		for _, item := range gatewayPoolInformer.GetStore().List() {
			unstr, ok := item.(*unstructured.Unstructured)
			if !ok {
				continue
			}

			specObj, ok := unstr.Object["spec"].(map[string]interface{})
			if !ok {
				continue
			}

			poolName := unstr.GetName()

			// Build gatewayPoolRoutedCIDRs
			if cidrs, okCidrs := specObj["routedCidrs"].([]interface{}); okCidrs {
				cidrList := make([]string, 0, len(cidrs))
				for _, cidr := range cidrs {
					if s, okStr := cidr.(string); okStr {
						cidrList = append(cidrList, s)
					}
				}

				if len(cidrList) > 0 {
					normalized := routeplan.BuildGatewayPoolRoutedCIDRSetByPool(map[string][]string{poolName: cidrList})
					for k, v := range normalized {
						ctx.gatewayPoolRoutedCIDRs[k] = v
					}
				}
			}

			// Extract gateway node identities from the pool's status.nodes
			statusObj, ok := unstr.Object["status"].(map[string]interface{})
			if !ok {
				continue
			}

			nodesObj, ok := statusObj["nodes"].([]interface{})
			if !ok {
				continue
			}

			for _, nodeObj := range nodesObj {
				nodeMap, ok := nodeObj.(map[string]interface{})
				if !ok {
					continue
				}

				nodeName, _ := nodeMap["name"].(string) //nolint:errcheck

				if nodeName == "" {
					continue
				}

				// Skip if we already have this node from Site informer (which has richer data).
				if _, exists := ctx.routeNodes[nodeName]; exists {
					continue
				}

				siteName, _ := nodeMap["siteName"].(string) //nolint:errcheck
				node := routeplan.Node{
					Name:     nodeName,
					SiteName: siteName,
				}

				if podCIDRs, ok := nodeMap["podCIDRs"].([]interface{}); ok {
					for _, cidr := range podCIDRs {
						if s, ok := cidr.(string); ok {
							node.PodCIDRs = append(node.PodCIDRs, s)
						}
					}
				}

				if ips, ok := nodeMap["internalIPs"].([]interface{}); ok {
					for _, ip := range ips {
						if s, ok := ip.(string); ok {
							node.InternalIPs = append(node.InternalIPs, s)
						}
					}
				}

				if ips, ok := nodeMap["externalIPs"].([]interface{}); ok {
					for _, ip := range ips {
						if s, ok := ip.(string); ok {
							node.ExternalIPs = append(node.ExternalIPs, s)
						}
					}
				}

				ctx.routeNodes[nodeName] = node
			}
		}
	}

	// Build siteNodeCIDRs from Site informer
	if siteInformer != nil {
		siteNodeCIDRInputs := make(map[string][]string)

		for _, item := range siteInformer.GetStore().List() {
			unstr, ok := item.(*unstructured.Unstructured)
			if !ok {
				continue
			}

			if specObj, ok := unstr.Object["spec"].(map[string]interface{}); ok {
				if cidrs, ok := specObj["nodeCidrs"].([]interface{}); ok {
					for _, cidr := range cidrs {
						if s, ok := cidr.(string); ok {
							siteNodeCIDRInputs[unstr.GetName()] = append(siteNodeCIDRInputs[unstr.GetName()], s)
						}
					}
				}
			}
		}

		ctx.siteNodeCIDRs = routeplan.BuildSiteNodeCIDRSetBySite(siteNodeCIDRInputs)
	}

	// Build directSitePeerings from SitePeering informer
	ctx.directSitePeerings = buildDirectSitePeeringSet(sitePeeringInformer)

	// Build sitePodCIDRs from the routeNodes we've collected
	ctx.sitePodCIDRs = routeplan.BuildSitePodCIDRSetBySite(ctx.routeNodes)

	return ctx
}

// buildDirectSitePeeringSet builds a bidirectional mapping of directly peered
// sites from the SitePeering informer.
func buildDirectSitePeeringSet(sitePeeringInformer cache.SharedIndexInformer) map[string]map[string]struct{} {
	result := make(map[string]map[string]struct{})
	if sitePeeringInformer == nil {
		return result
	}

	for _, item := range sitePeeringInformer.GetStore().List() {
		unstr, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		specObj, ok := unstr.Object["spec"].(map[string]interface{})
		if !ok {
			continue
		}

		sitesObj, ok := specObj["sites"].([]interface{})
		if !ok || len(sitesObj) < 2 {
			continue
		}

		sites := make([]string, 0, len(sitesObj))
		for _, siteObj := range sitesObj {
			siteName, ok := siteObj.(string)
			if !ok {
				continue
			}

			siteName = strings.TrimSpace(siteName)
			if siteName == "" {
				continue
			}

			sites = append(sites, siteName)
		}

		for i := 0; i < len(sites); i++ {
			for j := i + 1; j < len(sites); j++ {
				a := sites[i]

				b := sites[j]
				if a == b {
					continue
				}

				if result[a] == nil {
					result[a] = make(map[string]struct{})
				}

				if result[b] == nil {
					result[b] = make(map[string]struct{})
				}

				result[a][b] = struct{}{}
				result[b][a] = struct{}{}
			}
		}
	}

	return result
}

// annotateNodeRoutes annotates routes in the status response with Expected,
// Present, PeerDestinations, and Info fields. The function is best-effort --
// if informers are nil or not synced, annotation is skipped gracefully.
func annotateNodeRoutes(
	status *NodeStatusResponse,
	siteInformer, sliceInformer, gatewayPoolInformer, sitePeeringInformer cache.SharedIndexInformer,
) {
	if status == nil || len(status.RoutingTable.Routes) == 0 {
		return
	}

	defer func() {
		if r := recover(); r != nil {
			klog.Warningf("route annotation recovered from panic: %v", r)
		}
	}()

	actx := buildAnnotationContext(siteInformer, sliceInformer, gatewayPoolInformer, sitePeeringInformer)

	localSiteName := strings.TrimSpace(status.NodeInfo.SiteName)

	// Annotate peer-level RouteDestinations
	annotatePeerRouteDestinations(status, actx.routeNodes, actx.sitePodCIDRs, localSiteName, actx.directSitePeerings)

	// Build expected routes and annotate
	expectedIPv4, expectedIPv6 := buildExpectedRoutes(status, actx, localSiteName)
	ipv4Routes, ipv6Routes := splitRoutesByFamily(status.RoutingTable.Routes)
	ipv4Routes = annotateFamilyRoutes(ipv4Routes, expectedIPv4)
	ipv6Routes = annotateFamilyRoutes(ipv6Routes, expectedIPv6)
	ipv4Routes = annotateRoutePeerDestinationsForFamily(ipv4Routes, status.Peers, actx, localSiteName)
	ipv6Routes = annotateRoutePeerDestinationsForFamily(ipv6Routes, status.Peers, actx, localSiteName)

	// Mark eBPF supernet routes on unbounded0 as expected. These are managed
	// routes that don't correspond to individual peers but exist in the FIB
	// because the eBPF dataplane programs them as aggregate routes.
	markEBPFSupernetRoutesExpected(ipv4Routes)
	markEBPFSupernetRoutesExpected(ipv6Routes)

	// Remove empty routes (all nexthops filtered).
	ipv4Routes = filterEmptyRoutes(ipv4Routes)
	ipv6Routes = filterEmptyRoutes(ipv6Routes)

	status.RoutingTable.Routes = append(ipv4Routes, ipv6Routes...)
}

// markEBPFSupernetRoutesExpected adjusts route annotations for the eBPF
// dataplane. In eBPF mode:
//   - FIB-present routes on unbounded0 with no Expected annotation are marked
//     as expected (these are supernet routes the per-peer logic doesn't know).
//   - Nexthops on tunnel and WG interfaces that are not present in the FIB
//     are removed entirely, since the eBPF dataplane uses unbounded0 + BPF
//     maps instead of per-interface kernel routes.
func markEBPFSupernetRoutesExpected(routes []RouteEntry) {
	// Check if unbounded0 appears in any route (indicates eBPF mode).
	hasUnbounded0 := false

	for _, r := range routes {
		for _, hop := range r.NextHops {
			if hop.Device == "unbounded0" {
				hasUnbounded0 = true
				break
			}
		}

		if hasUnbounded0 {
			break
		}
	}

	if !hasUnbounded0 {
		return
	}

	for i := range routes {
		filtered := routes[i].NextHops[:0]
		for _, hop := range routes[i].NextHops {
			// Mark unbounded0 FIB routes as expected and present. These
			// routes are in the kernel (collected by collectRoutingTable)
			// but the per-peer annotation logic doesn't set Present/Expected
			// because unbounded0 isn't a traditional tunnel interface.
			if hop.Device == "unbounded0" {
				if hop.Expected == nil {
					hop.Expected = boolPtr(true)
				}

				if hop.Present == nil {
					hop.Present = boolPtr(true)
				}
			}
			// Remove phantom nexthops on tunnel/WG interfaces that are not
			// present in the FIB -- eBPF handles routing via the BPF map.
			if (hop.Device == "geneve0" || hop.Device == "vxlan0" || hop.Device == "ipip0" ||
				strings.HasPrefix(hop.Device, "wg")) &&
				(hop.Present == nil || !*hop.Present) {
				continue // drop this nexthop
			}

			filtered = append(filtered, hop)
		}

		routes[i].NextHops = filtered
	}
}

// filterEmptyRoutes removes routes that have no nexthops remaining after
// phantom nexthop cleanup.
func filterEmptyRoutes(routes []RouteEntry) []RouteEntry {
	filtered := routes[:0]
	for _, r := range routes {
		if len(r.NextHops) > 0 {
			filtered = append(filtered, r)
		}
	}

	return filtered
}

// sitesAreDirectlyPeered returns true if two sites are directly peered
// according to the directSitePeerings map.
func sitesAreDirectlyPeered(localSiteName, remoteSiteName string, directSitePeerings map[string]map[string]struct{}) bool {
	localSiteName = strings.TrimSpace(localSiteName)

	remoteSiteName = strings.TrimSpace(remoteSiteName)
	if localSiteName == "" || remoteSiteName == "" || localSiteName == remoteSiteName {
		return false
	}

	peersForLocal, ok := directSitePeerings[localSiteName]
	if !ok {
		return false
	}

	_, ok = peersForLocal[remoteSiteName]

	return ok
}

// annotatePeerRouteDestinations sets RouteDestinations on each peer status
// entry by matching kernel routes against expected destinations.
func annotatePeerRouteDestinations(
	nodeStatus *NodeStatusResponse,
	routeNodes map[string]routeplan.Node,
	sitePodCIDRs map[string]map[string]struct{},
	localSiteName string,
	directSitePeerings map[string]map[string]struct{},
) {
	if nodeStatus == nil || len(nodeStatus.Peers) == 0 {
		return
	}

	allRoutes := nodeStatus.RoutingTable.Routes

	for peerIndex := range nodeStatus.Peers {
		peer := &nodeStatus.Peers[peerIndex]
		interfaceName := strings.TrimSpace(peer.Tunnel.Interface)
		overlayGateways := normalizedGatewaySet(peer.PodCIDRGateways)
		endpointGateway := normalizeIPAddress(parseEndpointHost(peer.Tunnel.Endpoint))
		expectedDestinations := expectedDestinationsForPeer(*peer, peer.PodCIDRGateways, routeNodes, localSiteName, directSitePeerings)

		expectedDestinationSet := make(map[string]struct{}, len(expectedDestinations))
		for _, destination := range expectedDestinations {
			normalizedDestination, _ := normalizeRouteDestination(destination)
			if normalizedDestination == "" {
				continue
			}

			expectedDestinationSet[normalizedDestination] = struct{}{}
		}

		destinationsSet := make(map[string]struct{})

		for _, route := range allRoutes {
			normalizedDestination, _ := normalizeRouteDestination(route.Destination)
			if normalizedDestination == "" {
				continue
			}

			if !strings.EqualFold(peer.PeerType, "gateway") && len(expectedDestinationSet) > 0 {
				if _, ok := expectedDestinationSet[normalizedDestination]; !ok {
					continue
				}
			}

			for _, hop := range route.NextHops {
				if interfaceName != "" && !strings.EqualFold(strings.TrimSpace(hop.Device), interfaceName) {
					continue
				}

				hopGateway := normalizeIPAddress(hop.Gateway)
				_, overlayGatewayMatch := overlayGateways[hopGateway]

				gatewayMatch := hopGateway != "" && (overlayGatewayMatch || (endpointGateway != "" && hopGateway == endpointGateway))
				if gatewayMatch || len(expectedDestinationSet) > 0 {
					destinationsSet[normalizedDestination] = struct{}{}
				}
			}
		}

		if len(destinationsSet) == 0 {
			peer.RouteDestinations = nil
			continue
		}

		destinations := make([]string, 0, len(destinationsSet))
		for destination := range destinationsSet {
			destinations = append(destinations, destination)
		}

		sort.Strings(destinations)
		peer.RouteDestinations = destinations
	}
}

// expectedDestinationsForPeer computes the expected route destinations for a
// peer using routeplan.ExpectedDestinationsForPeer.
func expectedDestinationsForPeer(
	peer WireGuardPeerStatus,
	gateways []string,
	routeNodes map[string]routeplan.Node,
	localSiteName string,
	directSitePeerings map[string]map[string]struct{},
) []string {
	peerGateways := peer.PodCIDRGateways
	if len(gateways) > 0 {
		peerGateways = gateways
	}

	planPeer := routeplan.Peer{
		Name:              peer.Name,
		PeerType:          peer.PeerType,
		SiteName:          peer.SiteName,
		SitePeered:        sitesAreDirectlyPeered(localSiteName, peer.SiteName, directSitePeerings),
		SkipPodCIDRRoutes: peer.SkipPodCIDRRoutes,
		Endpoint:          peer.Tunnel.Endpoint,
		PodCIDRGateways:   peerGateways,
		AllowedIPs:        peer.Tunnel.AllowedIPs,
		RouteDistances:    peer.RouteDistances,
	}

	return routeplan.ExpectedDestinationsForPeer(planPeer, routeNodes)
}

// buildExpectedRoutes builds expected IPv4 and IPv6 route maps from the
// node's peer status and annotation context.
func buildExpectedRoutes(
	nodeStatus *NodeStatusResponse,
	actx *annotationContext,
	localSiteName string,
) (map[routeKey]expectedRoute, map[routeKey]expectedRoute) {
	expectedIPv4 := make(map[routeKey]expectedRoute)

	expectedIPv6 := make(map[routeKey]expectedRoute)
	if nodeStatus == nil {
		return expectedIPv4, expectedIPv6
	}

	routePeers := make([]routeplan.Peer, 0, len(nodeStatus.Peers))

	peerByName := make(map[string]routeplan.Peer, len(nodeStatus.Peers))
	for _, peer := range nodeStatus.Peers {
		routePeer := routeplan.Peer{
			Name:              peer.Name,
			PeerType:          peer.PeerType,
			SiteName:          peer.SiteName,
			SitePeered:        sitesAreDirectlyPeered(localSiteName, peer.SiteName, actx.directSitePeerings),
			SkipPodCIDRRoutes: peer.SkipPodCIDRRoutes,
			Interface:         peer.Tunnel.Interface,
			Endpoint:          peer.Tunnel.Endpoint,
			PodCIDRGateways:   peer.PodCIDRGateways,
			AllowedIPs:        peer.Tunnel.AllowedIPs,
			RouteDistances:    peer.RouteDistances,
		}
		routePeers = append(routePeers, routePeer)

		peerName := strings.TrimSpace(routePeer.Name)
		if peerName != "" {
			peerByName[peerName] = routePeer
		}
	}

	peerNamesByDestination := make(map[string]map[string]struct{})

	for _, peer := range routePeers {
		peerName := strings.TrimSpace(peer.Name)
		if peerName == "" {
			continue
		}

		for _, destination := range routeplan.ExpectedDestinationsForPeer(peer, actx.routeNodes) {
			normalizedDestination, _ := routeplan.NormalizeRouteDestination(destination)
			if normalizedDestination == "" {
				continue
			}

			if _, ok := peerNamesByDestination[normalizedDestination]; !ok {
				peerNamesByDestination[normalizedDestination] = make(map[string]struct{})
			}

			peerNamesByDestination[normalizedDestination][peerName] = struct{}{}
		}
	}

	buildNextHopInfo := func(destination string) (*NextHopInfo, []string) {
		normalizedDestination, _ := routeplan.NormalizeRouteDestination(destination)
		if normalizedDestination == "" {
			return nil, nil
		}

		peerNameSet := peerNamesByDestination[normalizedDestination]
		if len(peerNameSet) == 0 {
			return nil, nil
		}

		peerNames := make([]string, 0, len(peerNameSet))
		for peerName := range peerNameSet {
			peerNames = append(peerNames, peerName)
		}

		sort.Strings(peerNames)

		sharedInfo := routeplan.ClassifyRouteInfoForPeerDestination(normalizedDestination, peerNames, peerByName, actx.routeNodes, actx.sitePodCIDRs, actx.siteNodeCIDRs, actx.gatewayPoolRoutedCIDRs)
		if sharedInfo == nil {
			return nil, peerNames
		}

		return &NextHopInfo{
			ObjectName: sharedInfo.ObjectName,
			ObjectType: sharedInfo.ObjectType,
			RouteType:  sharedInfo.RouteType,
		}, peerNames
	}

	ipv4Plan, ipv6Plan := routeplan.BuildExpectedWireGuardRoutes(routePeers, actx.routeNodes)

	for _, route := range ipv4Plan {
		routeDistance := effectiveRouteDistance(route.Distance)
		key := routeKey{destination: route.Destination, gateway: route.Gateway, device: route.Device, distance: routeDistance, weight: route.Weight}
		info, peerNames := buildNextHopInfo(route.Destination)
		expectedIPv4[key] = expectedRoute{
			destination: route.Destination,
			nextHop:     NextHop{Gateway: route.Gateway, Device: route.Device, Distance: routeDistance, Weight: route.Weight},
			info:        info,
			peerNames:   peerNames,
		}
	}

	for _, route := range ipv6Plan {
		routeDistance := effectiveRouteDistance(route.Distance)
		key := routeKey{destination: route.Destination, gateway: route.Gateway, device: route.Device, distance: routeDistance, weight: route.Weight}
		info, peerNames := buildNextHopInfo(route.Destination)
		expectedIPv6[key] = expectedRoute{
			destination: route.Destination,
			nextHop:     NextHop{Gateway: route.Gateway, Device: route.Device, Distance: routeDistance, Weight: route.Weight},
			info:        info,
			peerNames:   peerNames,
		}
	}

	return expectedIPv4, expectedIPv6
}

// annotateFamilyRoutes annotates a slice of route entries for a single IP
// family (IPv4 or IPv6) with Expected and Present flags. Routes that are
// expected but missing from the kernel are appended as synthetic entries.
func annotateFamilyRoutes(routes []RouteEntry, expected map[routeKey]expectedRoute) []RouteEntry {
	annotated := make([]RouteEntry, len(routes))
	copy(annotated, routes)

	remainingExpected := cloneExpectedWireGuardRoutes(expected)
	lowestExpectedDistanceByDestination := make(map[string]int)

	for _, expectedRoute := range expected {
		distance := effectiveRouteDistance(expectedRoute.nextHop.Distance)
		if current, exists := lowestExpectedDistanceByDestination[expectedRoute.destination]; !exists || distance < current {
			lowestExpectedDistanceByDestination[expectedRoute.destination] = distance
		}
	}

	for routeIndex := range annotated {
		normalizedDestination, _ := normalizeRouteDestination(annotated[routeIndex].Destination)
		for hopIndex := range annotated[routeIndex].NextHops {
			hop := &annotated[routeIndex].NextHops[hopIndex]
			if !isWireGuardInterfaceName(hop.Device) {
				continue
			}

			hop.Present = boolPtr(true)

			matchKey, matched := findExpectedWireGuardMatch(remainingExpected, normalizedDestination, hop)
			if matched {
				hop.Expected = boolPtr(true)

				matchedRoute := remainingExpected[matchKey]
				if len(matchedRoute.peerNames) > 0 {
					hop.PeerDestinations = append([]string(nil), matchedRoute.peerNames...)
				}

				if matchedRoute.info != nil {
					hop.Info = &NextHopInfo{
						ObjectName: matchedRoute.info.ObjectName,
						ObjectType: matchedRoute.info.ObjectType,
						RouteType:  matchedRoute.info.RouteType,
					}
				}
			} else if isConnectedSelfWireGuardHostRoute(normalizedDestination, hop) {
				hop.Expected = boolPtr(true)
			} else {
				hop.Expected = boolPtr(false)
			}

			if matched {
				delete(remainingExpected, matchKey)
			}
		}
	}

	if len(remainingExpected) == 0 {
		return annotated
	}

	routeIndexByDestination := make(map[string]int)

	for routeIndex := range annotated {
		destination := strings.TrimSpace(annotated[routeIndex].Destination)
		if destination == "" {
			continue
		}

		routeIndexByDestination[destination] = routeIndex
	}

	for _, expectedRoute := range remainingExpected {
		if lowestDistance, exists := lowestExpectedDistanceByDestination[expectedRoute.destination]; exists {
			if effectiveRouteDistance(expectedRoute.nextHop.Distance) > lowestDistance {
				continue
			}
		}

		nextHop := expectedRoute.nextHop
		nextHop.Expected = boolPtr(true)

		nextHop.Present = boolPtr(false)
		if len(expectedRoute.peerNames) > 0 {
			nextHop.PeerDestinations = append([]string(nil), expectedRoute.peerNames...)
		}

		if expectedRoute.info != nil {
			nextHop.Info = &NextHopInfo{
				ObjectName: expectedRoute.info.ObjectName,
				ObjectType: expectedRoute.info.ObjectType,
				RouteType:  expectedRoute.info.RouteType,
			}
		}

		if existingRouteIndex, ok := routeIndexByDestination[expectedRoute.destination]; ok {
			annotated[existingRouteIndex].NextHops = append(annotated[existingRouteIndex].NextHops, nextHop)
			continue
		}

		annotated = append(annotated, RouteEntry{
			Destination: expectedRoute.destination,
			NextHops:    []NextHop{nextHop},
		})
		routeIndexByDestination[expectedRoute.destination] = len(annotated) - 1
	}

	sort.SliceStable(annotated, func(firstIndex, secondIndex int) bool {
		return annotated[firstIndex].Destination < annotated[secondIndex].Destination
	})

	for routeIndex := range annotated {
		sort.SliceStable(annotated[routeIndex].NextHops, func(firstIndex, secondIndex int) bool {
			firstHop := annotated[routeIndex].NextHops[firstIndex]

			secondHop := annotated[routeIndex].NextHops[secondIndex]
			if firstHop.Device != secondHop.Device {
				return firstHop.Device < secondHop.Device
			}

			if firstHop.Gateway != secondHop.Gateway {
				return firstHop.Gateway < secondHop.Gateway
			}

			if effectiveRouteDistance(firstHop.Distance) != effectiveRouteDistance(secondHop.Distance) {
				return effectiveRouteDistance(firstHop.Distance) < effectiveRouteDistance(secondHop.Distance)
			}

			return firstHop.Weight < secondHop.Weight
		})
	}

	return annotated
}

// annotateRoutePeerDestinationsForFamily sets PeerDestinations and Info on
// each next-hop of the provided routes by matching against peer expectations.
func annotateRoutePeerDestinationsForFamily(
	routes []RouteEntry,
	peers []WireGuardPeerStatus,
	actx *annotationContext,
	localSiteName string,
) []RouteEntry {
	if len(routes) == 0 || len(peers) == 0 {
		return routes
	}

	peerByName := make(map[string]WireGuardPeerStatus, len(peers))
	for _, peer := range peers {
		peerName := strings.TrimSpace(peer.Name)
		if peerName == "" {
			continue
		}

		peerByName[peerName] = peer
	}

	expectations := make([]peerRouteExpectation, 0, len(peers))
	for _, peer := range peers {
		peerName := strings.TrimSpace(peer.Name)

		interfaceName := strings.TrimSpace(peer.Tunnel.Interface)
		if peerName == "" || !isWireGuardInterfaceName(interfaceName) {
			continue
		}

		destinationSet := make(map[string]struct{})

		for _, destination := range expectedDestinationsForPeer(peer, peer.PodCIDRGateways, actx.routeNodes, localSiteName, actx.directSitePeerings) {
			normalizedDestination, _ := normalizeRouteDestination(destination)
			if normalizedDestination == "" {
				continue
			}

			destinationSet[normalizedDestination] = struct{}{}
		}

		expectations = append(expectations, peerRouteExpectation{
			peerName:            peerName,
			interfaceName:       interfaceName,
			overlayGateways:     normalizedGatewaySet(peer.PodCIDRGateways),
			endpointGateway:     normalizeIPAddress(parseEndpointHost(peer.Tunnel.Endpoint)),
			destinations:        destinationSet,
			matchAnyDestination: strings.EqualFold(peer.PeerType, "gateway"),
		})
	}

	if len(expectations) == 0 {
		return routes
	}

	for routeIndex := range routes {
		normalizedDestination, _ := normalizeRouteDestination(routes[routeIndex].Destination)
		if normalizedDestination == "" {
			for hopIndex := range routes[routeIndex].NextHops {
				routes[routeIndex].NextHops[hopIndex].PeerDestinations = nil
				routes[routeIndex].NextHops[hopIndex].Info = nil
			}

			continue
		}

		for hopIndex := range routes[routeIndex].NextHops {
			hop := &routes[routeIndex].NextHops[hopIndex]

			interfaceName := strings.TrimSpace(hop.Device)
			if !isWireGuardInterfaceName(interfaceName) {
				hop.PeerDestinations = nil
				hop.Info = nil

				continue
			}

			hopGateway := normalizeIPAddress(hop.Gateway)
			peerNameSet := make(map[string]struct{})

			for _, existingPeerName := range hop.PeerDestinations {
				normalizedPeerName := strings.TrimSpace(existingPeerName)
				if normalizedPeerName == "" {
					continue
				}

				peerNameSet[normalizedPeerName] = struct{}{}
			}

			for _, expectation := range expectations {
				if !strings.EqualFold(expectation.interfaceName, interfaceName) {
					continue
				}

				if !expectation.matchAnyDestination {
					if _, ok := expectation.destinations[normalizedDestination]; !ok {
						continue
					}
				}

				if hopGateway != "" && len(expectation.overlayGateways) > 0 {
					if _, ok := expectation.overlayGateways[hopGateway]; !ok {
						if expectation.endpointGateway == "" || hopGateway != expectation.endpointGateway {
							continue
						}
					}
				}

				peerNameSet[expectation.peerName] = struct{}{}
			}

			if len(peerNameSet) == 0 {
				hop.PeerDestinations = nil
				hop.Info = nil

				continue
			}

			peerNames := make([]string, 0, len(peerNameSet))
			for peerName := range peerNameSet {
				peerNames = append(peerNames, peerName)
			}

			sort.Strings(peerNames)
			hop.PeerDestinations = peerNames
			hop.Info = routeInfoForNextHop(normalizedDestination, peerNames, peerByName, actx)
		}
	}

	return routes
}

// routeInfoForNextHop classifies a next-hop destination to determine its
// object name, object type, and route type for display purposes.
func routeInfoForNextHop(
	normalizedDestination string,
	peerNames []string,
	peerByName map[string]WireGuardPeerStatus,
	actx *annotationContext,
) *NextHopInfo {
	routePeers := make(map[string]routeplan.Peer, len(peerByName))
	for peerName, peer := range peerByName {
		routePeers[peerName] = routeplan.Peer{
			Name:              peer.Name,
			PeerType:          peer.PeerType,
			SiteName:          peer.SiteName,
			SkipPodCIDRRoutes: peer.SkipPodCIDRRoutes,
			Endpoint:          peer.Tunnel.Endpoint,
			PodCIDRGateways:   peer.PodCIDRGateways,
			AllowedIPs:        peer.Tunnel.AllowedIPs,
		}
	}

	sharedInfo := routeplan.ClassifyRouteInfoForPeerDestination(normalizedDestination, peerNames, routePeers, actx.routeNodes, actx.sitePodCIDRs, actx.siteNodeCIDRs, actx.gatewayPoolRoutedCIDRs)
	if sharedInfo == nil {
		return nil
	}

	return &NextHopInfo{
		ObjectName: sharedInfo.ObjectName,
		ObjectType: sharedInfo.ObjectType,
		RouteType:  sharedInfo.RouteType,
	}
}

// findExpectedWireGuardMatch finds the expected route that best matches the
// given kernel route hop. It first tries an exact match, then falls back to a
// relaxed match on destination and device only.
func findExpectedWireGuardMatch(expected map[routeKey]expectedRoute, normalizedDestination string, hop *NextHop) (routeKey, bool) {
	if hop == nil {
		return routeKey{}, false
	}

	normalizedGateway := normalizeIPAddress(hop.Gateway)

	key := routeKey{
		destination: normalizedDestination,
		gateway:     normalizedGateway,
		device:      strings.TrimSpace(hop.Device),
		distance:    hop.Distance,
		weight:      hop.Weight,
	}
	if _, exists := expected[key]; exists {
		return key, true
	}

	for candidate := range expected {
		if candidate.destination != normalizedDestination {
			continue
		}

		if candidate.device != strings.TrimSpace(hop.Device) {
			continue
		}

		if hop.Distance > 0 && candidate.distance != hop.Distance {
			continue
		}

		return candidate, true
	}

	return routeKey{}, false
}

// cloneExpectedWireGuardRoutes creates a shallow copy of the expected routes map.
func cloneExpectedWireGuardRoutes(input map[routeKey]expectedRoute) map[routeKey]expectedRoute {
	copyMap := make(map[routeKey]expectedRoute, len(input))
	for key, value := range input {
		copyMap[key] = value
	}

	return copyMap
}

// boolPtr returns a pointer to the given bool value.
func boolPtr(value bool) *bool {
	b := value
	return &b
}

// isWireGuardInterfaceName returns true if the device name matches a managed
// tunnel interface pattern.
func isWireGuardInterfaceName(device string) bool {
	return isTunnelInterfaceName(device)
}

// isTunnelInterfaceName returns true for any managed tunnel interface:
// WireGuard (wg*), GENEVE (gn*), VXLAN (vxlan*), and IPIP (ipip*).
func isTunnelInterfaceName(device string) bool {
	dev := strings.ToLower(strings.TrimSpace(device))

	return strings.HasPrefix(dev, "wg") ||
		strings.HasPrefix(dev, "gn") ||
		strings.HasPrefix(dev, "vxlan") ||
		strings.HasPrefix(dev, "ipip")
}

// isConnectedSelfWireGuardHostRoute returns true if the hop is a connected
// or local host route (/32 or /128) on a tunnel interface with no gateway.
func isConnectedSelfWireGuardHostRoute(normalizedDestination string, hop *NextHop) bool {
	if hop == nil {
		return false
	}

	if !isWireGuardInterfaceName(hop.Device) {
		return false
	}

	if normalizeIPAddress(hop.Gateway) != "" {
		return false
	}

	ones, bits, ok := cidrMaskSize(normalizedDestination)
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

// normalizeRouteDestination normalizes a route destination CIDR string.
func normalizeRouteDestination(destination string) (string, int) {
	return routeplan.NormalizeRouteDestination(destination)
}

// normalizeIPAddress normalizes an IP address string.
func normalizeIPAddress(address string) string {
	return routeplan.NormalizeIP(address)
}

// effectiveRouteDistance returns the route distance, defaulting to 1 if zero.
func effectiveRouteDistance(distance int) int {
	if distance > 0 {
		return distance
	}

	return 1
}

// normalizedGatewaySet normalizes a list of gateway IPs into a set.
func normalizedGatewaySet(gateways []string) map[string]struct{} {
	set := make(map[string]struct{}, len(gateways))
	for _, gateway := range gateways {
		normalized := normalizeIPAddress(gateway)
		if normalized == "" {
			continue
		}

		set[normalized] = struct{}{}
	}

	return set
}

// parseEndpointHost extracts the host part from a WireGuard endpoint address,
// stripping the port suffix.
func parseEndpointHost(endpoint string) string {
	trimmed := strings.TrimSpace(endpoint)
	if trimmed == "" {
		return ""
	}

	if strings.HasPrefix(trimmed, "[") {
		end := strings.Index(trimmed, "]")
		if end > 0 {
			return trimmed[1:end]
		}

		return ""
	}

	lastColon := strings.LastIndex(trimmed, ":")
	if lastColon <= 0 {
		return trimmed
	}

	maybeHost := trimmed[:lastColon]
	if strings.Contains(maybeHost, ":") {
		return trimmed
	}

	return maybeHost
}

// cidrMaskSize returns the mask prefix length and total bits for a CIDR.
func cidrMaskSize(destination string) (int, int, bool) {
	return routeplan.CIDRMaskSize(destination)
}

// splitRoutesByFamily partitions a unified route slice into IPv4 and IPv6
// groups based on each entry's Family field.
func splitRoutesByFamily(routes []RouteEntry) (ipv4, ipv6 []RouteEntry) {
	for _, r := range routes {
		if strings.EqualFold(r.Family, "IPv6") {
			ipv6 = append(ipv6, r)
		} else {
			ipv4 = append(ipv4, r)
		}
	}

	return ipv4, ipv6
}
