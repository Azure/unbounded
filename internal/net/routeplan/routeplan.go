// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package routeplan

import (
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/klog/v2"
)

// Peer captures the route-relevant view of a WireGuard peer.
type Peer struct {
	Name              string
	PeerType          string // site or gateway
	SiteName          string
	SitePeered        bool
	SkipPodCIDRRoutes bool
	Interface         string
	Endpoint          string
	PodCIDRGateways   []string
	AllowedIPs        []string
	RouteDistances    map[string]int
}

// Node captures route-relevant node identity and addressing.
type Node struct {
	Name        string
	SiteName    string
	PodCIDRs    []string
	InternalIPs []string
	ExternalIPs []string
}

// PathHop describes one hop in a learned route path.
type PathHop struct {
	Type string
	Name string
}

// LearnedRoute describes one destination and its learned path alternatives.
type LearnedRoute struct {
	Paths [][]PathHop
}

// GatewayRouteAdvertisement captures a gateway's advertised routed CIDRs.
type GatewayRouteAdvertisement struct {
	Name        string
	LastUpdated time.Time
	Routes      map[string]LearnedRoute
}

// RouteInfo identifies the object and route type associated with a destination.
type RouteInfo struct {
	ObjectName string
	ObjectType string
	RouteType  string
}

type routeInfoCandidate struct {
	Prefix string
	Info   RouteInfo
}

// ExpectedRoute describes one expected kernel route entry.
type ExpectedRoute struct {
	Destination string
	Gateway     string
	Device      string
	Distance    int
	Weight      int
	Family      int
}

// NormalizeCIDR canonicalizes CIDRs and returns empty string for invalid input.
func NormalizeCIDR(cidr string) string {
	trimmed := strings.TrimSpace(cidr)
	if trimmed == "" {
		return ""
	}

	_, ipNet, err := net.ParseCIDR(trimmed)
	if err != nil {
		return ""
	}

	return ipNet.String()
}

// NormalizeRouteDestination converts CIDR or host IP inputs to canonical CIDR form.
func NormalizeRouteDestination(destination string) (string, int) {
	trimmed := strings.TrimSpace(destination)
	if trimmed == "" {
		return "", 0
	}

	if strings.Contains(trimmed, "/") {
		_, ipNet, err := net.ParseCIDR(trimmed)
		if err != nil {
			return "", 0
		}

		if ipNet.IP.To4() != nil {
			return ipNet.String(), 4
		}

		return ipNet.String(), 6
	}

	ip := net.ParseIP(trimmed)
	if ip == nil {
		return "", 0
	}

	if ip.To4() != nil {
		return ip.String() + "/32", 4
	}

	return ip.String() + "/128", 6
}

// HostCIDRForIP converts an IP string to a host CIDR form.
func HostCIDRForIP(address string) string {
	ip := net.ParseIP(strings.TrimSpace(address))
	if ip == nil {
		return ""
	}

	if ip.To4() != nil {
		return ip.String() + "/32"
	}

	return ip.String() + "/128"
}

// NormalizeIP canonicalizes an IP string and returns empty when invalid.
func NormalizeIP(address string) string {
	trimmed := strings.TrimSpace(address)
	if trimmed == "" {
		return ""
	}

	ip := net.ParseIP(trimmed)
	if ip == nil {
		return ""
	}

	return ip.String()
}

// FirstUsableHostCIDRFromCIDR returns the first usable host route for a CIDR.
func FirstUsableHostCIDRFromCIDR(cidr string) string {
	_, ipNet, err := net.ParseCIDR(strings.TrimSpace(cidr))
	if err != nil || ipNet == nil {
		return ""
	}

	ones, bits := ipNet.Mask.Size()
	if bits == 0 {
		return ""
	}

	if ones == bits {
		normalized, _ := NormalizeRouteDestination(ipNet.String())
		return normalized
	}

	ip := append(net.IP(nil), ipNet.IP...)
	if ip4 := ip.To4(); ip4 != nil {
		ip = append(net.IP(nil), ip4...)
		if ones <= 30 {
			incrementIP(ip)
		}

		return ip.String() + "/32"
	}

	incrementIP(ip)

	return ip.String() + "/128"
}

// BuildNormalizedCIDRSet creates a canonical set of CIDRs.
func BuildNormalizedCIDRSet(cidrs []string) map[string]struct{} {
	set := make(map[string]struct{}, len(cidrs))
	for _, cidr := range cidrs {
		normalized := NormalizeCIDR(cidr)
		if normalized == "" {
			continue
		}

		set[normalized] = struct{}{}
	}

	return set
}

// BuildLocalGatewayHostCIDRSetFromPodCIDRs returns cbr0-style gateway host CIDRs from pod CIDRs.
func BuildLocalGatewayHostCIDRSetFromPodCIDRs(podCIDRs []string) map[string]struct{} {
	set := make(map[string]struct{}, len(podCIDRs))
	for _, podCIDR := range podCIDRs {
		gatewayIP := gatewayIPFromCIDR(podCIDR)
		if gatewayIP == nil {
			continue
		}

		if gatewayIP.To4() != nil {
			set[gatewayIP.String()+"/32"] = struct{}{}
			continue
		}

		set[gatewayIP.String()+"/128"] = struct{}{}
	}

	return set
}

// BuildSitePodCIDRSetBySite builds normalized pod CIDR destinations grouped by site.
func BuildSitePodCIDRSetBySite(nodesByName map[string]Node) map[string]map[string]struct{} {
	bySite := make(map[string]map[string]struct{})

	for _, node := range nodesByName {
		siteName := strings.TrimSpace(node.SiteName)
		if siteName == "" {
			continue
		}

		if _, ok := bySite[siteName]; !ok {
			bySite[siteName] = make(map[string]struct{})
		}

		for _, podCIDR := range node.PodCIDRs {
			normalizedDestination, _ := NormalizeRouteDestination(podCIDR)
			if normalizedDestination == "" {
				continue
			}

			bySite[siteName][normalizedDestination] = struct{}{}
			if hostRoute := FirstUsableHostCIDRFromCIDR(normalizedDestination); hostRoute != "" {
				bySite[siteName][hostRoute] = struct{}{}
			}
		}
	}

	return bySite
}

// BuildSiteNodeCIDRSetBySite builds normalized site node CIDR destinations grouped by site.
func BuildSiteNodeCIDRSetBySite(siteNodeCIDRs map[string][]string) map[string]map[string]struct{} {
	bySite := make(map[string]map[string]struct{})

	for rawSiteName, cidrs := range siteNodeCIDRs {
		siteName := strings.TrimSpace(rawSiteName)
		if siteName == "" {
			continue
		}

		if _, ok := bySite[siteName]; !ok {
			bySite[siteName] = make(map[string]struct{})
		}

		for _, cidr := range cidrs {
			normalizedDestination, _ := NormalizeRouteDestination(cidr)
			if normalizedDestination == "" {
				continue
			}

			bySite[siteName][normalizedDestination] = struct{}{}
		}
	}

	return bySite
}

// BuildGatewayPoolRoutedCIDRSetByPool builds normalized routed CIDR destinations grouped by gateway pool.
func BuildGatewayPoolRoutedCIDRSetByPool(poolRoutedCIDRs map[string][]string) map[string]map[string]struct{} {
	byPool := make(map[string]map[string]struct{})

	for rawPoolName, cidrs := range poolRoutedCIDRs {
		poolName := strings.TrimSpace(rawPoolName)
		if poolName == "" {
			continue
		}

		if _, ok := byPool[poolName]; !ok {
			byPool[poolName] = make(map[string]struct{})
		}

		for _, cidr := range cidrs {
			normalizedDestination, _ := NormalizeRouteDestination(cidr)
			if normalizedDestination == "" {
				continue
			}

			byPool[poolName][normalizedDestination] = struct{}{}
		}
	}

	return byPool
}

func keysContainingDestination(normalizedDestination string, byKey map[string]map[string]struct{}) []string {
	if normalizedDestination == "" || len(byKey) == 0 {
		return nil
	}

	destinationIP, destinationNet, err := net.ParseCIDR(normalizedDestination)
	if err != nil || destinationNet == nil || destinationIP == nil {
		return nil
	}

	destinationOnes, destinationBits := destinationNet.Mask.Size()

	matchingKeys := make([]string, 0)

	for key, destinations := range byKey {
		matched := false

		for candidate := range destinations {
			_, candidateNet, candidateErr := net.ParseCIDR(candidate)
			if candidateErr != nil || candidateNet == nil {
				continue
			}

			candidateOnes, candidateBits := candidateNet.Mask.Size()
			if candidateBits != destinationBits {
				continue
			}

			if candidateOnes > destinationOnes {
				continue
			}

			if candidateNet.Contains(destinationIP) {
				matched = true
				break
			}
		}

		if matched {
			matchingKeys = append(matchingKeys, key)
		}
	}

	sort.Strings(matchingKeys)

	return matchingKeys
}

// keysWithCIDRsContainedByDestination returns keys where at least one CIDR in
// the key's set is contained by (is a subnet of) the destination. This is the
// reverse of keysContainingDestination -- used when the destination is a
// supernet (e.g., /16) and the tracked CIDRs are subnets (e.g., /24).
func keysWithCIDRsContainedByDestination(normalizedDestination string, byKey map[string]map[string]struct{}) []string {
	if normalizedDestination == "" || len(byKey) == 0 {
		return nil
	}

	_, destinationNet, err := net.ParseCIDR(normalizedDestination)
	if err != nil || destinationNet == nil {
		return nil
	}

	destinationOnes, destinationBits := destinationNet.Mask.Size()

	matchingKeys := make([]string, 0)

	for key, destinations := range byKey {
		matched := false

		for candidate := range destinations {
			candidateIP, candidateNet, candidateErr := net.ParseCIDR(candidate)
			if candidateErr != nil || candidateNet == nil || candidateIP == nil {
				continue
			}

			candidateOnes, candidateBits := candidateNet.Mask.Size()
			if candidateBits != destinationBits {
				continue
			}
			// Candidate must be narrower (more specific) than destination
			if candidateOnes <= destinationOnes {
				continue
			}

			if destinationNet.Contains(candidateIP) {
				matched = true
				break
			}
		}

		if matched {
			matchingKeys = append(matchingKeys, key)
		}
	}

	sort.Strings(matchingKeys)

	return matchingKeys
}

// ExpectedDestinationsForPeer computes expected route destinations for a peer.
func ExpectedDestinationsForPeer(peer Peer, nodesByName map[string]Node) []string {
	destinations := make([]string, 0)
	seen := make(map[string]struct{})
	allowedDestinations := make(map[string]struct{})

	addDestination := func(destination string) {
		normalized, _ := NormalizeRouteDestination(destination)
		if normalized == "" {
			return
		}

		if _, exists := seen[normalized]; exists {
			return
		}

		seen[normalized] = struct{}{}
		destinations = append(destinations, normalized)
	}

	peerName := strings.TrimSpace(peer.Name)
	peerPodCIDRSet := make(map[string]struct{})
	peerPodHostSet := make(map[string]struct{})

	if peerName != "" {
		if peerNode, found := nodesByName[peerName]; found {
			for _, podCIDR := range peerNode.PodCIDRs {
				normalizedPodCIDR, _ := NormalizeRouteDestination(podCIDR)
				if normalizedPodCIDR == "" {
					continue
				}

				peerPodCIDRSet[normalizedPodCIDR] = struct{}{}
				if hostRoute := FirstUsableHostCIDRFromCIDR(normalizedPodCIDR); hostRoute != "" {
					peerPodHostSet[hostRoute] = struct{}{}
				}
			}
		}
	}

	if strings.EqualFold(peer.PeerType, "site") && peerName != "" && !peer.SkipPodCIDRRoutes {
		if peerNode, found := nodesByName[peerName]; found {
			for _, podCIDR := range peerNode.PodCIDRs {
				addDestination(podCIDR)

				if hostRoute := FirstUsableHostCIDRFromCIDR(podCIDR); hostRoute != "" {
					addDestination(hostRoute)
				}
			}
		}
	}

	for _, allowedIP := range peer.AllowedIPs {
		normalized, _ := NormalizeRouteDestination(allowedIP)
		if normalized == "" || normalized == "0.0.0.0/0" || normalized == "::/0" {
			continue
		}

		allowedDestinations[normalized] = struct{}{}

		if peerName != "" {
			if peerNode, found := nodesByName[peerName]; found {
				if isPeerExternalIPHostRoute(peerNode, normalized) {
					continue
				}

				if isPeerInternalIPHostRoute(peerNode, normalized) && !shouldIncludePeerInternalIPRoute(peer, normalized, peerNode) {
					continue
				}
			}
		}

		if strings.EqualFold(peer.PeerType, "site") && peerName != "" {
			if peerNode, found := nodesByName[peerName]; found && len(peerNode.PodCIDRs) > 0 {
				ones, bits, isCIDR := cidrMaskSize(normalized)
				if !isCIDR || ones != bits {
					continue
				}
			}
		}

		if peer.SkipPodCIDRRoutes {
			if _, isPeerPodCIDR := peerPodCIDRSet[normalized]; isPeerPodCIDR {
				continue
			}
		}

		addDestination(normalized)

		if strings.EqualFold(peer.PeerType, "site") {
			if hostRoute := FirstUsableHostCIDRFromCIDR(normalized); hostRoute != "" {
				addDestination(hostRoute)
			}
		}
	}

	if strings.EqualFold(peer.PeerType, "gateway") {
		if peerName != "" {
			if peerNode, found := nodesByName[peerName]; found && !peer.SkipPodCIDRRoutes {
				for _, podCIDR := range peerNode.PodCIDRs {
					normalizedPodCIDR, _ := NormalizeRouteDestination(podCIDR)
					if normalizedPodCIDR == "" {
						continue
					}

					addDestination(normalizedPodCIDR)

					if _, includePodCIDR := allowedDestinations[normalizedPodCIDR]; !includePodCIDR {
						continue
					}

					if hostRoute := FirstUsableHostCIDRFromCIDR(normalizedPodCIDR); hostRoute != "" {
						addDestination(hostRoute)
					}
				}
			}
		}

		for _, gateway := range peer.PodCIDRGateways {
			if gatewayCIDR := HostCIDRForIP(gateway); gatewayCIDR != "" {
				addDestination(gatewayCIDR)
			}
		}
	}

	if peer.SkipPodCIDRRoutes {
		for hostRoute := range peerPodHostSet {
			addDestination(hostRoute)
		}
	}

	return destinations
}

func isPeerInternalIPHostRoute(node Node, normalizedDestination string) bool {
	for _, ip := range node.InternalIPs {
		if HostCIDRForIP(ip) == normalizedDestination {
			return true
		}
	}

	return false
}

func isPeerExternalIPHostRoute(node Node, normalizedDestination string) bool {
	for _, ip := range node.ExternalIPs {
		if HostCIDRForIP(ip) == normalizedDestination {
			return true
		}
	}

	return false
}

func shouldIncludePeerInternalIPRoute(peer Peer, normalizedDestination string, node Node) bool {
	if !isPeerInternalIPHostRoute(node, normalizedDestination) {
		return false
	}

	endpointHost := normalizeEndpointHost(peer.Endpoint)

	if strings.EqualFold(peer.PeerType, "site") {
		if peer.SitePeered {
			return false
		}

		if endpointHost == "" {
			return true
		}

		for _, internalIP := range node.InternalIPs {
			if NormalizeIP(internalIP) == endpointHost {
				return false
			}
		}

		return true
	}

	if !strings.EqualFold(peer.PeerType, "gateway") {
		return false
	}

	if endpointHost == "" {
		return false
	}

	for _, externalIP := range node.ExternalIPs {
		if NormalizeIP(externalIP) == endpointHost {
			return true
		}
	}

	return false
}

func normalizeEndpointHost(endpoint string) string {
	trimmed := strings.TrimSpace(endpoint)
	if trimmed == "" {
		return ""
	}

	if strings.HasPrefix(trimmed, "[") {
		end := strings.Index(trimmed, "]")
		if end <= 0 {
			return ""
		}

		return NormalizeIP(trimmed[1:end])
	}

	lastColon := strings.LastIndex(trimmed, ":")

	host := trimmed
	if lastColon > 0 {
		candidate := trimmed[:lastColon]
		if !strings.Contains(candidate, ":") {
			host = candidate
		}
	}

	return NormalizeIP(host)
}

// BuildExpectedWireGuardRoutes builds expected IPv4 and IPv6 WireGuard routes.
func BuildExpectedWireGuardRoutes(peers []Peer, nodesByName map[string]Node) ([]ExpectedRoute, []ExpectedRoute) {
	ipv4Routes := make([]ExpectedRoute, 0)
	ipv6Routes := make([]ExpectedRoute, 0)
	seen := make(map[string]struct{})

	for _, peer := range peers {
		iface := strings.TrimSpace(peer.Interface)
		if !isWireGuardInterfaceName(iface) {
			continue
		}

		rawGatewayByFamily := podCIDRGatewayByFamily(peer.PodCIDRGateways)

		fallbackGatewayByFamily := fallbackGatewayByFamilyFromAllowedCIDRs(peer.AllowedIPs)
		for _, destination := range ExpectedDestinationsForPeer(peer, nodesByName) {
			normalizedDestination, family := NormalizeRouteDestination(destination)
			if normalizedDestination == "" {
				continue
			}

			distance := 0
			if peer.RouteDistances != nil {
				distance = peer.RouteDistances[normalizedDestination]
			}

			gateway := ""
			if ones, bits, ok := cidrMaskSize(normalizedDestination); ok && ones == bits {
				gateway = ""
			} else if rawGatewayByFamily[family] != "" {
				gateway = rawGatewayByFamily[family]
			} else if fallbackGateway := fallbackGatewayByFamily[family]; fallbackGateway != "" {
				gateway = fallbackGateway
			} else if fallbackGateway := gatewayIPFromCIDR(normalizedDestination); fallbackGateway != nil {
				if (family == 4 && fallbackGateway.To4() != nil) || (family == 6 && fallbackGateway.To4() == nil) {
					gateway = fallbackGateway.String()
				}
			}

			identity := strings.Join([]string{normalizedDestination, gateway, iface, strconv.Itoa(distance), "0", strconv.Itoa(family)}, "|")
			if _, exists := seen[identity]; exists {
				continue
			}

			seen[identity] = struct{}{}

			route := ExpectedRoute{
				Destination: normalizedDestination,
				Gateway:     gateway,
				Device:      iface,
				Distance:    distance,
				Weight:      0,
				Family:      family,
			}
			switch family {
			case 4:
				ipv4Routes = append(ipv4Routes, route)
			case 6:
				ipv6Routes = append(ipv6Routes, route)
			}
		}
	}

	return ipv4Routes, ipv6Routes
}

func fallbackGatewayByFamilyFromAllowedCIDRs(allowedIPs []string) map[int]string {
	fallbackByFamily := map[int]string{}
	bestMaskByFamily := map[int]int{}

	for _, allowedIP := range allowedIPs {
		normalizedAllowed, family := NormalizeRouteDestination(allowedIP)
		if normalizedAllowed == "" || (normalizedAllowed == "0.0.0.0/0" || normalizedAllowed == "::/0") {
			continue
		}

		ones, bits, ok := cidrMaskSize(normalizedAllowed)
		if !ok || ones == bits {
			continue
		}

		fallbackGateway := gatewayIPFromCIDR(normalizedAllowed)
		if fallbackGateway == nil {
			continue
		}

		if (family == 4 && fallbackGateway.To4() == nil) || (family == 6 && fallbackGateway.To4() != nil) {
			continue
		}

		if previousMask, exists := bestMaskByFamily[family]; exists && previousMask >= ones {
			continue
		}

		bestMaskByFamily[family] = ones
		fallbackByFamily[family] = fallbackGateway.String()
	}

	return fallbackByFamily
}

func podCIDRGatewayByFamily(gateways []string) map[int]string {
	byFamily := map[int]string{}

	for _, gateway := range gateways {
		normalized := NormalizeIP(gateway)
		if normalized == "" {
			continue
		}

		ip := net.ParseIP(normalized)
		if ip == nil {
			continue
		}

		family := 6
		if ip.To4() != nil {
			family = 4
		}

		if byFamily[family] == "" {
			byFamily[family] = normalized
		}
	}

	return byFamily
}

// CIDRMaskSize returns mask size details for a normalized CIDR.
func CIDRMaskSize(destination string) (int, int, bool) {
	return cidrMaskSize(destination)
}

// ClassifyRouteInfoForPeerDestination classifies a destination for UI/status output.
func ClassifyRouteInfoForPeerDestination(
	normalizedDestination string,
	peerNames []string,
	peerByName map[string]Peer,
	nodeByName map[string]Node,
	sitePodCIDRs map[string]map[string]struct{},
	siteNodeCIDRs map[string]map[string]struct{},
	gatewayPoolRoutedCIDRs map[string]map[string]struct{},
) *RouteInfo {
	if normalizedDestination == "" || len(peerNames) == 0 {
		return nil
	}

	ones, bits, isCIDR := CIDRMaskSize(normalizedDestination)
	isHostRoute := isCIDR && ones == bits

	if !isHostRoute {
		matchingPools := keysContainingDestination(normalizedDestination, gatewayPoolRoutedCIDRs)
		if len(matchingPools) == 1 {
			return &RouteInfo{ObjectName: matchingPools[0], ObjectType: "gatewayPool", RouteType: "routedCidr"}
		}
	}

	if len(peerNames) > 1 {
		podSites := keysContainingDestination(normalizedDestination, sitePodCIDRs)
		if len(podSites) == 1 {
			return &RouteInfo{ObjectName: podSites[0], ObjectType: "site", RouteType: "podCidr"}
		}

		nodeSites := keysContainingDestination(normalizedDestination, siteNodeCIDRs)
		if len(nodeSites) == 1 {
			return &RouteInfo{ObjectName: nodeSites[0], ObjectType: "site", RouteType: "nodeCidr"}
		}

		// Check if the destination is a supernet containing a site's pod CIDRs
		// (e.g., /16 supernet for a site whose nodes have /24 allocations).
		podSupernetSites := keysWithCIDRsContainedByDestination(normalizedDestination, sitePodCIDRs)
		if len(podSupernetSites) == 1 {
			return &RouteInfo{ObjectName: podSupernetSites[0], ObjectType: "site", RouteType: "podCidr"}
		}

		routeType := "podCidr"
		routeTypeSet := make(map[string]struct{})
		uniqueCandidates := make([]RouteInfo, 0, len(peerNames))

		candidateSeen := make(map[string]struct{}, len(peerNames))
		for _, peerName := range peerNames {
			classified := classifyRouteInfoForSinglePeer(normalizedDestination, peerName, peerByName, nodeByName, sitePodCIDRs, siteNodeCIDRs, gatewayPoolRoutedCIDRs)
			if classified == nil {
				continue
			}

			if classified.RouteType != "" {
				routeTypeSet[classified.RouteType] = struct{}{}
			}

			candidateKey := strings.ToLower(strings.TrimSpace(classified.ObjectType)) + "|" + strings.TrimSpace(classified.ObjectName) + "|" + strings.ToLower(strings.TrimSpace(classified.RouteType))
			if _, exists := candidateSeen[candidateKey]; exists {
				continue
			}

			candidateSeen[candidateKey] = struct{}{}

			uniqueCandidates = append(uniqueCandidates, *classified)
		}

		if len(routeTypeSet) > 0 {
			types := make([]string, 0, len(routeTypeSet))
			for classifiedType := range routeTypeSet {
				types = append(types, classifiedType)
			}

			sort.Slice(types, func(i, j int) bool {
				leftPriority := routeTypePriority(types[i])

				rightPriority := routeTypePriority(types[j])
				if leftPriority != rightPriority {
					return leftPriority > rightPriority
				}

				return types[i] < types[j]
			})
			routeType = types[0]
		}

		if sharedSiteName, ok := sharedSiteNameForPeers(peerNames, peerByName); ok {
			if routeType == "podCidr" || routeType == "nodeCidr" {
				return &RouteInfo{ObjectName: sharedSiteName, ObjectType: "site", RouteType: routeType}
			}
		}

		if len(uniqueCandidates) == 0 {
			return nil
		}

		sort.Slice(uniqueCandidates, func(i, j int) bool {
			leftRouteTypePriority := routeTypePriority(uniqueCandidates[i].RouteType)

			rightRouteTypePriority := routeTypePriority(uniqueCandidates[j].RouteType)
			if leftRouteTypePriority != rightRouteTypePriority {
				return leftRouteTypePriority > rightRouteTypePriority
			}

			leftObjectTypePriority := objectTypePriority(uniqueCandidates[i].ObjectType)

			rightObjectTypePriority := objectTypePriority(uniqueCandidates[j].ObjectType)
			if leftObjectTypePriority != rightObjectTypePriority {
				return leftObjectTypePriority > rightObjectTypePriority
			}

			leftName := strings.TrimSpace(uniqueCandidates[i].ObjectName)

			rightName := strings.TrimSpace(uniqueCandidates[j].ObjectName)
			if leftName != rightName {
				return leftName < rightName
			}

			leftType := strings.TrimSpace(uniqueCandidates[i].ObjectType)

			rightType := strings.TrimSpace(uniqueCandidates[j].ObjectType)
			if leftType != rightType {
				return leftType < rightType
			}

			return strings.TrimSpace(uniqueCandidates[i].RouteType) < strings.TrimSpace(uniqueCandidates[j].RouteType)
		})

		selected := uniqueCandidates[0]
		if strings.TrimSpace(selected.RouteType) == "" {
			selected.RouteType = routeType
		}

		return &selected
	}

	return classifyRouteInfoForSinglePeer(normalizedDestination, peerNames[0], peerByName, nodeByName, sitePodCIDRs, siteNodeCIDRs, gatewayPoolRoutedCIDRs)
}

// classifyRouteInfoForSinglePeer classifies a destination for a single peer.
func classifyRouteInfoForSinglePeer(
	normalizedDestination string,
	peerName string,
	peerByName map[string]Peer,
	nodeByName map[string]Node,
	sitePodCIDRs map[string]map[string]struct{},
	siteNodeCIDRs map[string]map[string]struct{},
	gatewayPoolRoutedCIDRs map[string]map[string]struct{},
) *RouteInfo {
	peer, ok := peerByName[peerName]
	if !ok {
		return nil
	}

	candidates := make([]routeInfoCandidate, 0)
	addCandidate := func(prefix string, info RouteInfo) {
		normalizedPrefix, _ := NormalizeRouteDestination(prefix)
		if normalizedPrefix == "" {
			return
		}

		candidates = append(candidates, routeInfoCandidate{Prefix: normalizedPrefix, Info: info})
	}

	if strings.EqualFold(peer.PeerType, "site") {
		if peerNode, found := nodeByName[peerName]; found {
			for _, podCIDR := range peerNode.PodCIDRs {
				normalizedPodCIDR, _ := NormalizeRouteDestination(podCIDR)
				addCandidate(normalizedPodCIDR, RouteInfo{ObjectName: peerName, ObjectType: "node", RouteType: "podCidr"})

				if hostRoute := FirstUsableHostCIDRFromCIDR(normalizedPodCIDR); hostRoute != "" {
					addCandidate(hostRoute, RouteInfo{ObjectName: peerName, ObjectType: "node", RouteType: "podCidr"})
				}
			}
		}

		for _, gateway := range peer.PodCIDRGateways {
			if overlayHost := HostCIDRForIP(gateway); overlayHost != "" {
				addCandidate(overlayHost, RouteInfo{ObjectName: peerName, ObjectType: "node", RouteType: "podCidr"})
			}
		}

		if best := pickLongestPrefixMatchRouteInfo(normalizedDestination, candidates); best != nil {
			return best
		}

		return &RouteInfo{ObjectName: peerName, ObjectType: "node", RouteType: "podCidr"}
	}

	if strings.EqualFold(peer.PeerType, "gateway") {
		for _, gateway := range peer.PodCIDRGateways {
			if overlayHost := HostCIDRForIP(gateway); overlayHost != "" {
				addCandidate(overlayHost, RouteInfo{ObjectName: peerName, ObjectType: "gateway", RouteType: "nodeCidr"})
			}
		}

		if peerNode, found := nodeByName[peerName]; found {
			siteName := strings.TrimSpace(peer.SiteName)

			for _, podCIDR := range peerNode.PodCIDRs {
				normalizedPodCIDR, _ := NormalizeRouteDestination(podCIDR)
				if normalizedPodCIDR == "" {
					continue
				}

				if siteName != "" && isSharedSitePodCIDR(siteName, normalizedPodCIDR, nodeByName) {
					continue
				}

				addCandidate(normalizedPodCIDR, RouteInfo{ObjectName: peerName, ObjectType: "gateway", RouteType: "podCidr"})

				if hostRoute := FirstUsableHostCIDRFromCIDR(normalizedPodCIDR); hostRoute != "" {
					addCandidate(hostRoute, RouteInfo{ObjectName: peerName, ObjectType: "gateway", RouteType: "podCidr"})
				}
			}

			for _, internalIP := range peerNode.InternalIPs {
				hostCIDR := HostCIDRForIP(internalIP)
				if hostCIDR == "" {
					continue
				}

				if shouldIncludePeerInternalIPRoute(peer, hostCIDR, peerNode) {
					addCandidate(hostCIDR, RouteInfo{ObjectName: peerName, ObjectType: "gateway", RouteType: "nodeCidr"})
				}
			}
		}

		siteName := strings.TrimSpace(peer.SiteName)
		if siteName != "" {
			if sharedSiteName, ok := sharedGatewaySiteCIDR(peer, normalizedDestination, peerByName, siteNodeCIDRs, sitePodCIDRs); ok {
				routeType := "nodeCidr"
				if destinationContainsSitePodCIDR(sharedSiteName, normalizedDestination, nodeByName, peerByName) {
					routeType = "podCidr"
				}

				addCandidate(normalizedDestination, RouteInfo{ObjectName: sharedSiteName, ObjectType: "site", RouteType: routeType})
			}

			if siteRoutes, ok := sitePodCIDRs[siteName]; ok {
				for siteRoute := range siteRoutes {
					addCandidate(siteRoute, RouteInfo{ObjectName: siteName, ObjectType: "site", RouteType: "podCidr"})
				}
			}
		}

		for _, allowedIP := range peer.AllowedIPs {
			normalizedAllowed, _ := NormalizeRouteDestination(allowedIP)
			if normalizedAllowed == "" || normalizedAllowed == "0.0.0.0/0" || normalizedAllowed == "::/0" {
				continue
			}

			addCandidate(normalizedAllowed, RouteInfo{ObjectName: peerName, ObjectType: "gateway", RouteType: "routedCidr"})
		}

		if best := pickLongestPrefixMatchRouteInfo(normalizedDestination, candidates); best != nil {
			return best
		}

		return &RouteInfo{ObjectName: peerName, ObjectType: "gateway", RouteType: "routedCidr"}
	}

	if best := pickLongestPrefixMatchRouteInfo(normalizedDestination, candidates); best != nil {
		return best
	}

	return &RouteInfo{ObjectName: peerName, ObjectType: strings.ToLower(strings.TrimSpace(peer.PeerType)), RouteType: "podCidr"}
}

func isSharedSitePodCIDR(siteName, normalizedPodCIDR string, nodeByName map[string]Node) bool {
	if strings.TrimSpace(siteName) == "" || normalizedPodCIDR == "" {
		return false
	}

	matches := 0

	for _, node := range nodeByName {
		if !strings.EqualFold(strings.TrimSpace(node.SiteName), siteName) {
			continue
		}

		for _, podCIDR := range node.PodCIDRs {
			normalized, _ := NormalizeRouteDestination(podCIDR)
			if normalized != normalizedPodCIDR {
				continue
			}

			matches++
			if matches > 1 {
				return true
			}

			break
		}
	}

	return false
}

func sharedGatewaySiteCIDR(peer Peer, normalizedDestination string, peerByName map[string]Peer, siteNodeCIDRs, sitePodCIDRs map[string]map[string]struct{}) (string, bool) {
	siteName := strings.TrimSpace(peer.SiteName)
	if siteName == "" || normalizedDestination == "" {
		return "", false
	}

	// Verify the destination is actually a CIDR belonging to this site
	// (either a node CIDR or a supernet containing pod CIDRs). If the
	// destination is from a different site routed through our gateways,
	// it should not be classified as belonging to our site.
	// Only check when siteNodeCIDRs is available (authoritative source).
	if len(siteNodeCIDRs) > 0 {
		belongsToSite := false

		if nodeCIDRs, ok := siteNodeCIDRs[siteName]; ok {
			for cidr := range nodeCIDRs {
				_, cidrNet, cidrErr := net.ParseCIDR(cidr)
				if cidrErr != nil || cidrNet == nil {
					continue
				}

				destIP, destNet, destErr := net.ParseCIDR(normalizedDestination)
				if destErr != nil || destNet == nil || destIP == nil {
					continue
				}
				// Destination is or overlaps with a node CIDR
				if cidrNet.Contains(destIP) || destNet.Contains(cidrNet.IP) {
					belongsToSite = true
					break
				}
			}
		}

		if !belongsToSite {
			if podCIDRs, ok := sitePodCIDRs[siteName]; ok {
				destIP, destNet, err := net.ParseCIDR(normalizedDestination)
				if err == nil && destNet != nil && destIP != nil {
					for podCIDR := range podCIDRs {
						_, podNet, podErr := net.ParseCIDR(podCIDR)
						if podErr != nil || podNet == nil {
							continue
						}

						if destNet.Contains(podNet.IP) {
							belongsToSite = true
							break
						}
					}
				}
			}
		}

		if !belongsToSite {
			return "", false
		}
	}

	matches := 0

	for _, candidate := range peerByName {
		if !strings.EqualFold(strings.TrimSpace(candidate.PeerType), "gateway") {
			continue
		}

		if !strings.EqualFold(strings.TrimSpace(candidate.SiteName), siteName) {
			continue
		}

		for _, allowedIP := range candidate.AllowedIPs {
			normalizedAllowed, _ := NormalizeRouteDestination(allowedIP)
			if normalizedAllowed == "" || normalizedAllowed == "0.0.0.0/0" || normalizedAllowed == "::/0" {
				continue
			}

			if normalizedAllowed != normalizedDestination {
				continue
			}

			matches++

			break
		}
	}

	return siteName, matches > 1
}

func destinationContainsSitePodCIDR(siteName, normalizedDestination string, nodeByName map[string]Node, peerByName map[string]Peer) bool {
	if strings.TrimSpace(siteName) == "" || normalizedDestination == "" {
		return false
	}

	_, destinationNet, err := net.ParseCIDR(normalizedDestination)
	if err != nil || destinationNet == nil {
		return false
	}

	for _, node := range nodeByName {
		if !strings.EqualFold(strings.TrimSpace(node.SiteName), siteName) {
			continue
		}

		for _, podCIDR := range node.PodCIDRs {
			normalizedPodCIDR, _ := NormalizeRouteDestination(podCIDR)
			if normalizedPodCIDR == "" {
				continue
			}

			podIP, _, podErr := net.ParseCIDR(normalizedPodCIDR)
			if podErr != nil || podIP == nil {
				continue
			}

			if destinationNet.Contains(podIP) {
				return true
			}
		}
	}

	// Fallback signal: if gateway peers in the same site have pod CIDR gateway
	// host IPs that fall within destination, this is a pod supernet route.
	for _, peer := range peerByName {
		if !strings.EqualFold(strings.TrimSpace(peer.PeerType), "gateway") {
			continue
		}

		if !strings.EqualFold(strings.TrimSpace(peer.SiteName), siteName) {
			continue
		}

		for _, gatewayIP := range peer.PodCIDRGateways {
			normalizedGatewayIP := NormalizeIP(gatewayIP)
			if normalizedGatewayIP == "" {
				continue
			}

			parsedGatewayIP := net.ParseIP(normalizedGatewayIP)
			if parsedGatewayIP == nil {
				continue
			}

			if destinationNet.Contains(parsedGatewayIP) {
				return true
			}
		}
	}

	return false
}

func pickLongestPrefixMatchRouteInfo(normalizedDestination string, candidates []routeInfoCandidate) *RouteInfo {
	_, destinationNet, err := net.ParseCIDR(normalizedDestination)
	if err != nil || destinationNet == nil {
		return nil
	}

	destinationOnes, destinationBits := destinationNet.Mask.Size()

	bestIndex := -1
	bestOnes := -1
	bestRouteTypePriority := -1
	bestObjectTypePriority := -1
	bestObjectName := ""

	for index, candidate := range candidates {
		_, candidateNet, candidateErr := net.ParseCIDR(candidate.Prefix)
		if candidateErr != nil || candidateNet == nil {
			continue
		}

		candidateOnes, candidateBits := candidateNet.Mask.Size()
		if candidateBits != destinationBits {
			continue
		}

		if candidateOnes > destinationOnes {
			continue
		}

		if !candidateNet.Contains(destinationNet.IP) {
			continue
		}

		candidateRouteTypePriority := routeTypePriority(candidate.Info.RouteType)

		candidateObjectTypePriority := objectTypePriority(candidate.Info.ObjectType)
		if candidateOnes > bestOnes ||
			(candidateOnes == bestOnes && candidateRouteTypePriority > bestRouteTypePriority) ||
			(candidateOnes == bestOnes && candidateRouteTypePriority == bestRouteTypePriority && candidateObjectTypePriority > bestObjectTypePriority) ||
			(candidateOnes == bestOnes && candidateRouteTypePriority == bestRouteTypePriority && candidateObjectTypePriority == bestObjectTypePriority && (bestIndex == -1 || candidate.Info.ObjectName < bestObjectName)) {
			bestIndex = index
			bestOnes = candidateOnes
			bestRouteTypePriority = candidateRouteTypePriority
			bestObjectTypePriority = candidateObjectTypePriority
			bestObjectName = candidate.Info.ObjectName
		}
	}

	if bestIndex == -1 {
		return nil
	}

	best := candidates[bestIndex].Info

	return &RouteInfo{ObjectName: best.ObjectName, ObjectType: best.ObjectType, RouteType: best.RouteType}
}

func routeTypePriority(routeType string) int {
	switch strings.ToLower(strings.TrimSpace(routeType)) {
	case "podcidr":
		return 3
	case "nodecidr":
		return 2
	case "routedcidr":
		return 1
	default:
		return 0
	}
}

func objectTypePriority(objectType string) int {
	switch strings.ToLower(strings.TrimSpace(objectType)) {
	case "gateway":
		return 4
	case "node":
		return 3
	case "site":
		return 2
	default:
		return 0
	}
}

func sharedSiteNameForPeers(peerNames []string, peerByName map[string]Peer) (string, bool) {
	sharedSiteName := ""

	for _, peerName := range peerNames {
		peer, ok := peerByName[peerName]
		if !ok {
			return "", false
		}

		siteName := strings.TrimSpace(peer.SiteName)
		if siteName == "" {
			return "", false
		}

		if sharedSiteName == "" {
			sharedSiteName = siteName
			continue
		}

		if !strings.EqualFold(sharedSiteName, siteName) {
			return "", false
		}
	}

	if sharedSiteName == "" {
		return "", false
	}

	return sharedSiteName, true
}

// FilterGatewayAdvertisedRoutes filters stale or inapplicable gateway-advertised routes.
func FilterGatewayAdvertisedRoutes(
	advertisement GatewayRouteAdvertisement,
	fallback []string,
	mySiteName string,
	localGatewayPools []string,
	now time.Time,
	staleAfter time.Duration,
) ([]string, map[string]int, map[string]LearnedRoute) {
	if len(advertisement.Routes) == 0 {
		return fallback, map[string]int{}, map[string]LearnedRoute{}
	}

	if advertisement.LastUpdated.IsZero() || advertisement.LastUpdated.Add(staleAfter).Before(now) {
		return fallback, map[string]int{}, map[string]LearnedRoute{}
	}

	routes := make([]string, 0, len(advertisement.Routes))
	distances := make(map[string]int, len(advertisement.Routes))
	selected := make(map[string]LearnedRoute, len(advertisement.Routes))

	for cidr, route := range advertisement.Routes {
		if cidr == "" {
			continue
		}

		if len(route.Paths) > 0 {
			filteredPaths := make([][]PathHop, 0, len(route.Paths))
			for _, path := range route.Paths {
				if len(path) == 0 {
					continue
				}

				if mySiteName != "" && PathHasHop(path, "Site", mySiteName) {
					continue
				}

				skipByPool := false

				for _, poolName := range localGatewayPools {
					if pathHasIntermediatePoolHop(path, poolName) {
						// Path includes our pool as an intermediate hop --
						// accepting it would create a loop (traffic departs
						// from us, passes through other pools, then returns).
						// Paths where our pool is the terminal hop are valid
						// (they represent routes from our own assigned sites).
						skipByPool = true
						break
					}
				}

				if skipByPool {
					continue
				}

				copied := make([]PathHop, len(path))
				copy(copied, path)
				filteredPaths = append(filteredPaths, copied)
			}

			if len(filteredPaths) == 0 {
				continue
			}

			route.Paths = DedupePathHops(filteredPaths)
		}

		routes = append(routes, cidr)
		pathLength := RoutePathLength(route)

		const maxRouteDistance = 20
		if pathLength <= 1 {
			distances[cidr] = 1
		} else if pathLength-1 > maxRouteDistance {
			klog.Warningf("Route path length %d for CIDR %s exceeds maximum %d, capping distance", pathLength, cidr, maxRouteDistance)
			distances[cidr] = maxRouteDistance
		} else {
			// Keep the base route level at distance 1 and increment for longer paths.
			distances[cidr] = pathLength - 1
		}

		selected[cidr] = route
	}

	if len(routes) == 0 {
		return []string{}, distances, selected
	}

	return dedupeStrings(routes), distances, selected
}

func cidrMaskSize(destination string) (int, int, bool) {
	_, ipNet, err := net.ParseCIDR(destination)
	if err != nil {
		return 0, 0, false
	}

	ones, bits := ipNet.Mask.Size()

	return ones, bits, true
}

func gatewayIPFromCIDR(cidr string) net.IP {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil || ipNet == nil {
		return nil
	}

	result := make(net.IP, len(ipNet.IP))
	copy(result, ipNet.IP)

	if ip4 := result.To4(); ip4 != nil {
		ip4[3]++
		return ip4
	}

	result[15]++

	return result
}

func incrementIP(ip net.IP) {
	for index := len(ip) - 1; index >= 0; index-- {
		ip[index]++
		if ip[index] != 0 {
			break
		}
	}
}

// PathHasHop reports whether a route path includes the given hop.
func PathHasHop(path []PathHop, hopType, hopName string) bool {
	for _, hop := range path {
		if hop.Type == hopType && hop.Name == hopName {
			return true
		}
	}

	return false
}

// pathHasIntermediatePoolHop returns true if the named gateway pool appears
// anywhere in the path as a loop. A loop is detected when:
//   - The pool appears as a non-terminal hop (traffic leaves us and comes back)
//   - The pool is the ONLY hop (self-referential, no useful routing info)
//
// A path where our pool is the terminal hop with preceding hops (e.g.,
// [site2, pool-local]) is valid -- it means site2 reaches us via our pool.
func pathHasIntermediatePoolHop(path []PathHop, poolName string) bool {
	for i, hop := range path {
		if hop.Type != "GatewayPool" || hop.Name != poolName {
			continue
		}
		// Non-terminal: loop (traffic departs and returns)
		if i < len(path)-1 {
			return true
		}
		// Terminal and only hop: self-referential, filter it
		if len(path) == 1 {
			return true
		}
	}

	return false
}

// RoutePathLength returns the shortest non-empty path length for a route.
func RoutePathLength(route LearnedRoute) int {
	if len(route.Paths) == 0 {
		return 1
	}

	shortest := 0

	for _, path := range route.Paths {
		if len(path) == 0 {
			continue
		}

		if shortest == 0 || len(path) < shortest {
			shortest = len(path)
		}
	}

	if shortest == 0 {
		return 1
	}

	return shortest
}

// DedupePathHops removes duplicate paths while preserving deterministic ordering.
func DedupePathHops(paths [][]PathHop) [][]PathHop {
	if len(paths) == 0 {
		return nil
	}

	seen := make(map[string][]PathHop, len(paths))
	for _, path := range paths {
		if len(path) == 0 {
			continue
		}

		parts := make([]string, 0, len(path))

		copied := make([]PathHop, len(path))
		for i, hop := range path {
			copied[i] = hop
			parts = append(parts, hop.Type+"/"+hop.Name)
		}

		seen[strings.Join(parts, "->")] = copied
	}

	if len(seen) == 0 {
		return nil
	}

	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	result := make([][]PathHop, 0, len(keys))
	for _, key := range keys {
		result = append(result, seen[key])
	}

	return result
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}

		seen[value] = struct{}{}
	}

	if len(seen) == 0 {
		return []string{}
	}

	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}

	sort.Strings(out)

	return out
}

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
