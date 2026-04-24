// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"net"
	"sort"

	"github.com/vishvananda/netlink"
	"k8s.io/klog/v2"

	unboundednetnetlink "github.com/Azure/unbounded/internal/net/netlink"
	"github.com/Azure/unbounded/internal/net/routeplan"
)

// configureWireGuard sets up WireGuard interfaces and configures peers
// - wg<port>: Main mesh interface for all mesh peers (intra-site, remote, same-pool gateways)
// - wg<gwPort>: Separate interfaces for each gateway peer (for ECMP routing)
// Endpoint and routing decisions are driven by peer.SiteName and peeredSites membership.
func configureWireGuard(ctx context.Context, cfg *config, privKey string, peers []meshPeerInfo, gatewayPeers []gatewayPeerInfo, mySiteName string, peeredSites, networkPeeredSites, gatewayNodePubKeys map[string]bool, siteHealthCheckProfileNames, peeringSiteHealthCheckProfileNames, assignmentSiteHealthCheckProfileNames, assignmentPoolHealthCheckProfileNames, poolHealthCheckProfileNames map[string]string, siteTunnelMTUs, peeringSiteTunnelMTUs, assignmentSiteTunnelMTUs, assignmentPoolTunnelMTUs, poolTunnelMTUs map[string]int, additionalRoutes []unboundednetnetlink.DesiredRoute, geneveHCPeers map[string]bool, state *wireGuardState) error {
	nodePodCIDRs := state.nodePodCIDRs
	isGatewayNode := state.isGatewayNode
	myGatewayPort := state.myGatewayPort

	port := cfg.WireGuardPort
	iface := fmt.Sprintf("wg%d", port)
	localPodCIDRs := routeplan.BuildNormalizedCIDRSet(nodePodCIDRs)
	localGatewayHostCIDRs := routeplan.BuildLocalGatewayHostCIDRSetFromPodCIDRs(nodePodCIDRs)

	// Detect the tunnel MTU once for all interfaces in this reconciliation.
	// MTU = default-route interface MTU minus WireGuard encapsulation overhead.
	detectedMTU := 0
	if defaultMTU := unboundednetnetlink.DetectDefaultRouteMTUFromCache(state.netlinkCache); defaultMTU > 0 {
		detectedMTU = defaultMTU - unboundednetnetlink.WireGuardMTUOverhead
	}

	// The effective MTU is the lower of the configured value and the detected
	// maximum. This ensures we never exceed the physical link capacity while
	// still respecting an operator's deliberate lower setting.
	tunnelMTU := cfg.MTU
	if detectedMTU > 0 && detectedMTU < tunnelMTU {
		tunnelMTU = detectedMTU
	}

	// Warn if the configured MTU exceeds the detected maximum for this node.
	if cfg.MTU > 0 && detectedMTU > 0 && cfg.MTU > detectedMTU {
		klog.Errorf("Configured MTU %d exceeds this node's maximum tunnel MTU %d (default route MTU minus %d overhead); clamping to %d",
			cfg.MTU, detectedMTU, unboundednetnetlink.WireGuardMTUOverhead, tunnelMTU)
		mtuErrMsg := fmt.Sprintf("configured MTU %d exceeds node maximum tunnel MTU %d; clamped to %d", cfg.MTU, detectedMTU, tunnelMTU)
		hasMTUError := false

		for _, e := range state.nodeErrors {
			if e.Type == "mtuMismatch" {
				hasMTUError = true
				break
			}
		}

		if !hasMTUError {
			state.nodeErrors = append(state.nodeErrors, NodeError{Type: "mtuMismatch", Message: mtuErrMsg})
		}
	} else {
		// Clear any stale MTU mismatch error when the condition no longer applies.
		filtered := state.nodeErrors[:0]
		for _, e := range state.nodeErrors {
			if e.Type != "mtuMismatch" {
				filtered = append(filtered, e)
			}
		}

		state.nodeErrors = filtered
	}

	// === Configure main mesh interface ===

	// If there are no WireGuard peers, remove the mesh interface and flush
	// routes. The informer sync guard in reconcileUpdate ensures we only
	// reach here after all caches are populated, so an empty peer list is
	// genuine (not a transient startup state).
	if len(peers) == 0 && len(gatewayPeers) == 0 {
		if state.linkManager.Exists() {
			klog.Infof("No WireGuard peers -- removing mesh interface %s", iface)

			if err := state.linkManager.DeleteLink(); err != nil {
				klog.V(2).Infof("Failed to remove unused WireGuard interface %s: %v", iface, err)
			}
		}
		// Sync routes (clear WG routes, keep any GENEVE/IPIP additional routes)
		if state.routeManager != nil {
			if state.netlinkCache != nil {
				state.netlinkCache.BeginRouteBatch()
			}

			if err := state.routeManager.SyncRoutes(additionalRoutes); err != nil {
				klog.Errorf("Failed to sync routes: %v", err)
			}

			if state.netlinkCache != nil {
				state.netlinkCache.EndRouteBatch()
			}
		}
		// Clean up stale health check peers even when no WG peers exist.
		// GENEVE peers are still tracked via geneveHCPeers.
		if state.healthCheckManager != nil {
			for peerName := range state.healthCheckManager.GetAllPeerStatuses() {
				if !geneveHCPeers[peerName] {
					if err := state.healthCheckManager.RemovePeer(peerName); err != nil {
						klog.V(2).Infof("Healthcheck: failed to remove stale peer %s: %v", peerName, err)
					} else {
						klog.V(4).Infof("Healthcheck: removed stale peer %s", peerName)
					}
				}
			}
		}

		return nil
	}

	// Ensure WireGuard interface exists using netlink
	if err := state.linkManager.EnsureWireGuardInterface(); err != nil {
		return fmt.Errorf("failed to ensure WireGuard interface: %w", err)
	}

	// Set tunnel MTU so tunnel packets fit within the underlying network
	// MTU without fragmentation. Also corrects stale MTUs on startup.
	if tunnelMTU > 0 {
		if err := state.linkManager.EnsureMTU(tunnelMTU); err != nil {
			klog.Warningf("Failed to set MTU on %s: %v", iface, err)
		}
	}

	// Initialize WireGuard manager if not already done (after interface exists)
	if state.wireguardManager == nil {
		wm, err := unboundednetnetlink.NewWireGuardManager(iface)
		if err != nil {
			return fmt.Errorf("failed to create WireGuard manager: %w", err)
		}

		state.wireguardManager = wm
	}

	// Build peer configurations for mesh interface.
	// Endpoint selection is driven by SiteName:
	//   - Peered site (including same site): endpoint = InternalIPs[0]
	//   - Non-peered site: endpoint = "" (peer initiates connection to gateways)
	// Port selection: when this node is a gateway, non-gateway mesh peers have a
	// dedicated gateway interface (wg<gwPort>) listening on this gateway's assigned
	// port, so the endpoint port must be myGatewayPort for those peers. Other
	// gateway peers use the standard mesh port since they communicate via wg51820.
	wgPeerByPublicKey := make(map[string]unboundednetnetlink.WireGuardPeer, len(peers))

	for _, peer := range peers {
		if peer.WireGuardPublicKey == "" {
			continue
		}

		// Choose endpoint port: gateway nodes use their assigned port for
		// non-gateway peers; standard mesh port for other gateways.
		peerEndpointPort := port
		if isGatewayNode && myGatewayPort != 0 && !gatewayNodePubKeys[peer.WireGuardPublicKey] {
			peerEndpointPort = int(myGatewayPort)
		}

		var endpoint string
		if peeredSites[peer.SiteName] && len(peer.InternalIPs) > 0 {
			// Same site or directly peered site -- use internal IP as endpoint
			endpoint = net.JoinHostPort(peer.InternalIPs[0], fmt.Sprintf("%d", peerEndpointPort))
		}
		// Non-peered sites: no endpoint, WireGuard learns it from incoming packets

		normalizedPeerPodCIDRs := make([]string, 0, len(peer.PodCIDRs))
		for _, podCIDR := range peer.PodCIDRs {
			normalizedCIDR := routeplan.NormalizeCIDR(podCIDR)
			if normalizedCIDR == "" {
				continue
			}

			normalizedPeerPodCIDRs = append(normalizedPeerPodCIDRs, normalizedCIDR)
		}

		// AllowedIPs policy on mesh interface:
		// - Always include peer podCIDRs.
		// - Include peer internal IP host routes only for non-peered sites.
		//   Same-site and directly peered-site nodes are directly reachable, so
		//   internal host /32-/128 prefixes are intentionally excluded.
		allowedIPs := append([]string{}, normalizedPeerPodCIDRs...)
		if !peeredSites[peer.SiteName] {
			allowedIPs = append(allowedIPs, ipsToHostCIDRs(peer.InternalIPs)...)
		}

		desiredPeer := unboundednetnetlink.WireGuardPeer{
			PublicKey:  peer.WireGuardPublicKey,
			Endpoint:   endpoint,
			AllowedIPs: dedupeStrings(allowedIPs),
		}

		if existingPeer, exists := wgPeerByPublicKey[peer.WireGuardPublicKey]; exists {
			if existingPeer.Endpoint == "" && desiredPeer.Endpoint != "" {
				existingPeer.Endpoint = desiredPeer.Endpoint
			}

			existingPeer.AllowedIPs = dedupeStrings(append(existingPeer.AllowedIPs, desiredPeer.AllowedIPs...))
			wgPeerByPublicKey[peer.WireGuardPublicKey] = existingPeer

			continue
		}

		wgPeerByPublicKey[peer.WireGuardPublicKey] = desiredPeer
	}

	wgPeerKeys := make([]string, 0, len(wgPeerByPublicKey))
	for key := range wgPeerByPublicKey {
		wgPeerKeys = append(wgPeerKeys, key)
	}

	sort.Strings(wgPeerKeys)

	wgPeers := make([]unboundednetnetlink.WireGuardPeer, 0, len(wgPeerKeys))
	for _, key := range wgPeerKeys {
		wgPeers = append(wgPeers, wgPeerByPublicKey[key])
	}

	// Configure mesh interface with intra-site peers
	if err := state.wireguardManager.Configure(privKey, port, wgPeers); err != nil {
		return fmt.Errorf("failed to configure WireGuard: %w", err)
	}

	// Calculate cbr0 gateway IPs for WireGuard address assignment
	var wgAddresses []string

	for _, cidr := range nodePodCIDRs {
		gatewayIP := getGatewayIPFromCIDR(cidr)
		if gatewayIP == nil {
			continue
		}

		if gatewayIP.To4() != nil {
			wgAddresses = append(wgAddresses, gatewayIP.String()+"/32")
		} else {
			wgAddresses = append(wgAddresses, gatewayIP.String()+"/128")
		}
	}

	// Sync addresses on mesh interface
	if _, _, err := state.linkManager.SyncAddresses(wgAddresses, false); err != nil {
		return fmt.Errorf("failed to sync addresses on WireGuard interface: %w", err)
	}

	// Bring mesh interface up
	if err := state.linkManager.SetLinkUp(); err != nil {
		return fmt.Errorf("failed to bring up WireGuard interface: %w", err)
	}

	// Accept forwarded traffic arriving on the mesh WG interface.
	ensureTunnelForwardAccept(iface)

	// Build routes for mesh interface via shared route builder.
	// All routing is per-peer for healthcheck granularity: bootstrap host routes,
	// pod CIDR routes, and internal IP routes for non-peered sites.
	var allDesiredRoutes []unboundednetnetlink.DesiredRoute

	// Resolve the link index for the mesh interface
	meshLink, meshLinkErr := netlink.LinkByName(iface)

	meshLinkIndex := 0
	if meshLinkErr == nil && meshLink != nil {
		meshLinkIndex = meshLink.Attrs().Index
	} else {
		klog.Warningf("Failed to resolve link index for %s: %v", iface, meshLinkErr)
	}

	for _, peer := range peers {
		peerMTU := resolveMeshPeerTunnelMTU(tunnelMTU, peer, mySiteName, siteTunnelMTUs, peeringSiteTunnelMTUs, assignmentSiteTunnelMTUs)
		peerRoutes := buildMeshPeerRoutes(peer, meshLinkIndex, iface, mySiteName, peeredSites, cfg, peerMTU, localPodCIDRs, localGatewayHostCIDRs)
		allDesiredRoutes = append(allDesiredRoutes, peerRoutes...)
	}

	// === Configure separate gateway interfaces for gateway peers ===

	// Initialize gateway interface maps if needed
	if state.gatewayLinkManagers == nil {
		state.gatewayLinkManagers = make(map[string]*unboundednetnetlink.LinkManager)
	}

	if state.gatewayWireguardManagers == nil {
		state.gatewayWireguardManagers = make(map[string]*unboundednetnetlink.WireGuardManager)
	}

	if state.gatewayHealthEndpoints == nil {
		state.gatewayHealthEndpoints = make(map[string]string)
	}

	if state.gatewayRoutes == nil {
		state.gatewayRoutes = make(map[string][]string)
	}

	if state.gatewayRouteDistances == nil {
		state.gatewayRouteDistances = make(map[string]map[string]int)
	}

	if state.gatewaySiteCIDRs == nil {
		state.gatewaySiteCIDRs = make(map[string][]string)
	}

	if state.gatewayPodCIDRs == nil {
		state.gatewayPodCIDRs = make(map[string][]string)
	}

	// Initialize gateway policy manager if unavailable.
	if state.gatewayPolicyManager == nil {
		var err error

		state.gatewayPolicyManager, err = unboundednetnetlink.NewGatewayPolicyManager(cfg.WireGuardPort)
		if err != nil {
			klog.Errorf("Failed to create gateway policy manager: %v", err)
			// Continue without policy routing - it's not fatal
		} else {
			klog.V(2).Info("Initialized gateway policy manager for policy routing")
		}
	}

	// Track which gateway interfaces we're using this round
	desiredGatewayIfaces := make(map[string]bool)
	desiredGatewayPolicyTables := make(map[string]int)

	var gatewayCIDRs []string

	// Create/update a separate interface for each gateway peer
	for _, gwPeer := range gatewayPeers {
		// Determine the listen port for this gateway interface.
		// The port is encoded in the interface name (wg<port>).
		// Always use the peer's advertised gateway port for stable interface naming.
		// If missing, skip this peer to avoid creating an unstable interface.
		if gwPeer.GatewayWireguardPort == 0 {
			klog.Warningf("Skipping gateway peer %s: missing gatewayWireguardPort", gwPeer.Name)
			continue
		}

		gwPort := int(gwPeer.GatewayWireguardPort)

		gwIfaceName := fmt.Sprintf("wg%d", gwPort)
		desiredGatewayIfaces[gwIfaceName] = true
		desiredGatewayPolicyTables[gwIfaceName] = gwPort

		// Collect all routed CIDRs (they're the same for all gateways)
		if len(gatewayCIDRs) == 0 {
			gatewayCIDRs = gwPeer.RoutedCidrs
		}

		// Get or create link manager for this gateway interface
		gwLinkManager, exists := state.gatewayLinkManagers[gwIfaceName]
		if !exists {
			gwLinkManager = unboundednetnetlink.NewLinkManager(gwIfaceName)
			state.gatewayLinkManagers[gwIfaceName] = gwLinkManager
		}

		// Ensure gateway interface exists
		if err := gwLinkManager.EnsureWireGuardInterface(); err != nil {
			klog.Errorf("Failed to ensure gateway interface %s: %v", gwIfaceName, err)
			continue
		}

		// Apply the same tunnel MTU to gateway interfaces
		if tunnelMTU > 0 {
			if err := gwLinkManager.EnsureMTU(tunnelMTU); err != nil {
				klog.Warningf("Failed to set MTU on %s: %v", gwIfaceName, err)
			}
		}

		// Get or create WireGuard manager for this gateway interface
		gwWgManager, exists := state.gatewayWireguardManagers[gwIfaceName]
		if !exists {
			var err error

			gwWgManager, err = unboundednetnetlink.NewWireGuardManager(gwIfaceName)
			if err != nil {
				klog.Errorf("Failed to create WireGuard manager for %s: %v", gwIfaceName, err)
				continue
			}

			state.gatewayWireguardManagers[gwIfaceName] = gwWgManager
		}

		// Build endpoint using three-way site-based rule:
		//   1. Same site: use internal IP
		//   2. Different site, External pool: use public (external) IP
		//   3. Different site, Internal pool: no endpoint (WireGuard learns from incoming)
		//
		// Port selection:
		//   Gateway-to-gateway: crossover -- endpoint port is our own gateway port
		//     (the remote gateway's interface for us listens on our GatewayWireguardPort).
		//   Non-gateway to gateway: use the standard mesh port. The non-gateway node's
		//     gateway interface (wg<gwPort>) sends to the gateway's mesh interface,
		//     which has the non-gateway node as a peer.
		var (
			endpoint     string
			endpointPort int32
		)

		if isGatewayNode && myGatewayPort != 0 && gwPeer.GatewayWireguardPort != 0 {
			// Gateway-to-gateway: crossover -- endpoint port is our own gateway port
			endpointPort = myGatewayPort
		} else {
			// Non-gateway to gateway: connect to the gateway's mesh interface (default port)
			endpointPort = int32(port)
		}

		if gwPeer.SiteName == mySiteName && len(gwPeer.InternalIPs) > 0 {
			// Same site -- always use internal IP regardless of pool type
			endpoint = net.JoinHostPort(gwPeer.InternalIPs[0], fmt.Sprintf("%d", endpointPort))
			klog.V(3).Infof("Gateway %s is in same site, using internal IP %s:%d", gwPeer.Name, gwPeer.InternalIPs[0], endpointPort)
		} else if networkPeeredSites[gwPeer.SiteName] && len(gwPeer.InternalIPs) > 0 {
			// Different site but network-peered -- use internal IP since sites can reach each other
			endpoint = net.JoinHostPort(gwPeer.InternalIPs[0], fmt.Sprintf("%d", endpointPort))
			klog.V(3).Infof("Gateway %s is in peered site (%s), using internal IP %s:%d", gwPeer.Name, gwPeer.SiteName, gwPeer.InternalIPs[0], endpointPort)
		} else if gwPeer.PoolType == "External" && len(gwPeer.ExternalIPs) > 0 {
			// Different site, External pool, not peered -- use public IP
			endpoint = net.JoinHostPort(gwPeer.ExternalIPs[0], fmt.Sprintf("%d", endpointPort))
			klog.V(3).Infof("Gateway %s is in different site (%s), external pool, using public IP %s:%d", gwPeer.Name, gwPeer.SiteName, gwPeer.ExternalIPs[0], endpointPort)
		} else if gwPeer.PoolType == "Internal" {
			// Different site, Internal pool -- no endpoint, WireGuard learns it
			klog.V(3).Infof("Gateway %s is in different site (%s), internal pool, no endpoint (WireGuard learns)", gwPeer.Name, gwPeer.SiteName)
		}

		// Build allowed IPs from routed CIDRs plus the gateway node's own podCIDRs.
		// Including podCIDRs ensures packets destined to the gateway node's pod ranges
		// are accepted on this peer even when routed supernets are narrowed.
		allowedIPs := pruneCoveredAllowedCIDRs(append(append([]string{}, gwPeer.RoutedCidrs...), gwPeer.PodCIDRs...))

		// Configure this gateway interface with a single peer
		// No PersistentKeepalive needed - we rely on health checks for connectivity monitoring
		gwPeersCfg := []unboundednetnetlink.WireGuardPeer{
			{
				PublicKey:  gwPeer.WireGuardPublicKey,
				Endpoint:   endpoint,
				AllowedIPs: allowedIPs,
			},
		}

		if err := gwWgManager.Configure(privKey, gwPort, gwPeersCfg); err != nil {
			klog.Errorf("Failed to configure gateway interface %s: %v", gwIfaceName, err)
			continue
		}

		// Add the same addresses to gateway interfaces for source IP selection
		if _, _, err := gwLinkManager.SyncAddresses(wgAddresses, false); err != nil {
			klog.Warningf("Failed to sync addresses on gateway interface %s: %v", gwIfaceName, err)
		}

		// Bring gateway interface up
		if err := gwLinkManager.SetLinkUp(); err != nil {
			klog.Errorf("Failed to bring up gateway interface %s: %v", gwIfaceName, err)
			continue
		}

		// Set fwmark on gateway WG interface for transit traffic forwarding.
		ensureTunnelForwardAccept(gwIfaceName)

		// Configure policy routing for this gateway interface
		// This ensures return traffic leaves via the same interface it arrived on
		if cfg.EnablePolicyRouting && state.gatewayPolicyManager != nil {
			if err := state.gatewayPolicyManager.ConfigureInterface(gwIfaceName, gwPort); err != nil {
				klog.Warningf("Failed to configure policy routing for %s: %v", gwIfaceName, err)
				// Continue - policy routing is not critical
			}
		}

		// Build gateway peer routes via shared route builder.
		gwLink, gwLinkErr := netlink.LinkByName(gwIfaceName)

		gwLinkIdx := 0
		if gwLinkErr == nil && gwLink != nil {
			gwLinkIdx = gwLink.Attrs().Index
		}

		gwPeerMTU := resolveGatewayPeerTunnelMTU(tunnelMTU, mySiteName, gwPeer, siteTunnelMTUs, assignmentPoolTunnelMTUs, poolTunnelMTUs)
		// In eBPF mode, skip creating kernel routes for WG gateway peers --
		// the BPF program on unbounded0 handles routing to the correct WG
		// interface via the LPM trie. Creating WG-interface routes would
		// conflict with the unbounded0 supernet routes.
		if cfg.TunnelDataplane != "ebpf" {
			gwPeerRoutes := buildGatewayPeerRoutes(gwPeer, gwLinkIdx, gwIfaceName, mySiteName, cfg, gwPeerMTU, localPodCIDRs, localGatewayHostCIDRs)
			allDesiredRoutes = append(allDesiredRoutes, gwPeerRoutes...)
		}

		prevName, hadPrevName := state.gatewayNames[gwIfaceName]
		prevSiteName := state.gatewaySiteNames[gwIfaceName]
		prevRoutedCIDRs := state.gatewaySiteCIDRs[gwIfaceName]
		prevPodCIDRs := state.gatewayPodCIDRs[gwIfaceName]
		prevHealthEndpoint := state.gatewayHealthEndpoints[gwIfaceName]

		// Store health endpoint for this gateway (use first available)
		desiredHealthEndpoint := ""
		if len(gwPeer.HealthEndpoints) > 0 {
			desiredHealthEndpoint = gwPeer.HealthEndpoints[0]
			state.gatewayHealthEndpoints[gwIfaceName] = desiredHealthEndpoint
		} else {
			delete(state.gatewayHealthEndpoints, gwIfaceName)
		}

		gatewayIfaceChanged := !hadPrevName ||
			prevName != gwPeer.Name ||
			prevSiteName != gwPeer.SiteName ||
			!strSliceEqual(prevRoutedCIDRs, gwPeer.RoutedCidrs) ||
			!strSliceEqual(prevPodCIDRs, gwPeer.PodCIDRs) ||
			prevHealthEndpoint != desiredHealthEndpoint

		// Store gateway metadata for status endpoint
		state.gatewayNames[gwIfaceName] = gwPeer.Name
		state.gatewaySiteNames[gwIfaceName] = gwPeer.SiteName

		// Store routes for tracking
		state.gatewayRoutes[gwIfaceName] = gwPeer.PodCIDRs // Gateway-specific routes
		state.gatewayRouteDistances[gwIfaceName] = copyStringIntMap(gwPeer.RouteDistances)
		state.gatewaySiteCIDRs[gwIfaceName] = gwPeer.RoutedCidrs // Shared routed CIDRs
		state.gatewayPodCIDRs[gwIfaceName] = gwPeer.PodCIDRs     // Specific gateway podCIDRs

		if gatewayIfaceChanged {
			klog.V(4).Infof("Configured gateway interface %s with peer %s (%s)",
				gwIfaceName, gwPeer.Name, endpoint)
		}
	}

	// Store gateway CIDRs for health monitoring.
	// Collect stale interfaces and clean up state under lock, then
	// perform netlink cleanup outside the lock.
	state.gatewayCIDRs = gatewayCIDRs
	staleGatewayIfaces := make(map[string]*unboundednetnetlink.LinkManager)

	for ifaceName, linkManager := range state.gatewayLinkManagers {
		if !desiredGatewayIfaces[ifaceName] {
			staleGatewayIfaces[ifaceName] = linkManager
		}
	}

	for ifaceName := range staleGatewayIfaces {
		delete(state.gatewayLinkManagers, ifaceName)
		delete(state.gatewayWireguardManagers, ifaceName)
		delete(state.gatewayHealthEndpoints, ifaceName)
		delete(state.gatewayRoutes, ifaceName)
		delete(state.gatewayRouteDistances, ifaceName)
		delete(state.gatewaySiteCIDRs, ifaceName)
		delete(state.gatewayPodCIDRs, ifaceName)
	}

	// Remove old gateway interfaces that are no longer needed
	for ifaceName, linkManager := range staleGatewayIfaces {
		klog.Infof("Removing unused gateway interface %s", ifaceName)

		if cfg.EnablePolicyRouting && state.gatewayPolicyManager != nil {
			if err := state.gatewayPolicyManager.RemoveInterface(ifaceName); err != nil {
				klog.Warningf("Failed to remove policy routing for %s: %v", ifaceName, err)
			}
		}

		if err := linkManager.DeleteLink(); err != nil {
			klog.Warningf("Failed to delete gateway interface %s: %v", ifaceName, err)
		}

		removeTunnelForwardAccept(ifaceName)
	}

	if cfg.EnablePolicyRouting && state.gatewayPolicyManager != nil {
		if err := state.gatewayPolicyManager.ReconcileExpectedInterfaces(desiredGatewayPolicyTables); err != nil {
			klog.Warningf("Failed to reconcile gateway policy routing chains: %v", err)
		}
	}

	// Full ownership reconcile: remove any unmanaged wg* interfaces not present
	// in the current desired map (mesh interface + desired gateway interfaces).
	removeUnmanagedWireGuardInterfaces(cfg, state, desiredGatewayIfaces, iface)

	// === Sync all routes via unified route manager (netlink) ===
	// This handles intra-site routes on the mesh interface, gateway-specific routes on gateway interfaces,
	// shared RoutedCidrs ECMP routes (same prefix via multiple gateway interfaces), and GENEVE routes.
	// The unified route manager handles ECMP automatically when the same prefix has multiple nexthops.
	// Healthcheck handles health-based nexthop removal/restoration.
	allDesiredRoutes = append(allDesiredRoutes, additionalRoutes...)

	if state.routeManager != nil {
		if state.netlinkCache != nil {
			state.netlinkCache.BeginRouteBatch()
		}

		if err := state.routeManager.SyncRoutes(allDesiredRoutes); err != nil {
			klog.Errorf("Failed to sync routes via unified route manager: %v", err)
		} else {
			klog.V(4).Infof("Unified route sync complete (total desired: %d)", len(allDesiredRoutes))
		}

		if state.netlinkCache != nil {
			state.netlinkCache.EndRouteBatch()
		}
	} else {
		klog.Errorf("Unified route manager unavailable -- %d routes not programmed", len(allDesiredRoutes))
	}

	// Log summary
	klog.V(4).Infof("WireGuard configured: mesh interface with %d intra-site peers; %d gateway interfaces; %d total desired routes",
		len(peers), len(gatewayPeers), len(allDesiredRoutes))

	// === Register healthcheck peers via shared HC registration ===
	wgHCPeers := registerPeersWithHealthCheck(peers, gatewayPeers, mySiteName, isGatewayNode,
		siteHealthCheckProfileNames, peeringSiteHealthCheckProfileNames,
		assignmentSiteHealthCheckProfileNames, assignmentPoolHealthCheckProfileNames,
		poolHealthCheckProfileNames, state, peerIfaceNameWireGuard, false)

	// Remove peers that are no longer desired (preserve GENEVE HC peers)
	if state.healthCheckManager != nil {
		for peerName := range state.healthCheckManager.GetAllPeerStatuses() {
			if !wgHCPeers[peerName] && !geneveHCPeers[peerName] {
				if err := state.healthCheckManager.RemovePeer(peerName); err != nil {
					klog.V(2).Infof("Healthcheck: failed to remove stale peer %s: %v", peerName, err)
				} else {
					klog.V(4).Infof("Healthcheck: removed stale peer %s", peerName)
				}
			}
		}
	}

	// Update Prometheus collector with current set of WireGuard managers
	if state.wgCollector != nil {
		managers := make([]*unboundednetnetlink.WireGuardManager, 0, 1+len(state.gatewayWireguardManagers))
		if state.wireguardManager != nil {
			managers = append(managers, state.wireguardManager)
		}

		for _, gwm := range state.gatewayWireguardManagers {
			managers = append(managers, gwm)
		}

		state.wgCollector.SetManagers(managers)
	}

	return nil
}
