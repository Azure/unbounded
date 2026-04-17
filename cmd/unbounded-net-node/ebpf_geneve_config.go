// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"k8s.io/klog/v2"

	unboundednetv1alpha1 "github.com/Azure/unbounded-kube/internal/net/apis/unboundednet/v1alpha1"
	ebpfpkg "github.com/Azure/unbounded-kube/internal/net/ebpf"
	unboundednetnetlink "github.com/Azure/unbounded-kube/internal/net/netlink"
)

// unbounded0DeviceName is the dedicated dummy interface for the eBPF tunnel
// dataplane. It has NOARP set so the kernel sends packets directly without
// neighbor resolution. TC egress BPF intercepts and redirects to the tunnel.
const unbounded0DeviceName = "unbounded0"

// ensureEBPFTunnelMap initializes the shared eBPF tunnel map and sets up the
// unbounded0 dummy device with TC egress for tunnel encapsulation.
func ensureEBPFTunnelMap(cfg *config, state *wireGuardState) (*ebpfpkg.TunnelMap, error) {
	if state.tunnelMaps == nil {
		state.tunnelMaps = make(map[string]*ebpfpkg.TunnelMap)
	}

	const mapKey = "ebpf"

	tm := state.tunnelMaps[mapKey]

	if tm == nil {
		var err error

		tm, err = ebpfpkg.NewTunnelMap(ebpfpkg.TunnelMapOptions{
			MaxEntries: uint32(cfg.TunnelDataplaneMapSize),
		})
		if err != nil {
			return nil, fmt.Errorf("failed to load eBPF tunnel program -- set tunnelDataplane=netlink to use netlink dataplane instead: %w", err)
		}

		state.tunnelMaps[mapKey] = tm
	}

	if !tm.Attached() {
		// Create unbounded0 dummy interface with NOARP.
		// NOARP means the kernel sends packets directly without ARP/neighbor
		// resolution -- no static neighbor entries needed.
		devLM := unboundednetnetlink.NewLinkManager(unbounded0DeviceName)
		if err := devLM.EnsureDummyInterface(); err != nil {
			klog.V(2).Infof("eBPF: could not create %s (will retry): %v", unbounded0DeviceName, err)
			return tm, nil
		}

		// Load TC egress (unbounded_encap) on unbounded0.
		// No ARP responder needed -- NOARP bypasses neighbor resolution.
		if attachErr := tm.AttachToInterface(unbounded0DeviceName); attachErr != nil {
			klog.V(2).Infof("eBPF TC not yet on %s (will retry): %v", unbounded0DeviceName, attachErr)
		}
	}

	// Assign podCIDR gateway IPs to unbounded0 as /32 addresses.
	// These serve as source IPs for overlay-destined traffic.
	// This runs on every reconcile (not just first attach) so that
	// transient address sync failures are retried.
	{
		devLM := unboundednetnetlink.NewLinkManager(unbounded0DeviceName)

		var devAddrs []string

		for _, cidr := range state.nodePodCIDRs {
			gwIP := getGatewayIPFromCIDR(cidr)
			if gwIP == nil {
				continue
			}

			if gwIP.To4() != nil {
				devAddrs = append(devAddrs, gwIP.String()+"/32")
			} else {
				devAddrs = append(devAddrs, gwIP.String()+"/128")
			}
		}

		if len(devAddrs) > 0 {
			if _, _, err := devLM.SyncAddresses(devAddrs, false); err != nil {
				klog.Warningf("eBPF: failed to sync addresses on %s: %v", unbounded0DeviceName, err)
			}
		}
	}

	return tm, nil
}

// neededTunnelProtocols scans mesh and gateway peers to determine which
// tunnel interfaces are required. GENEVE, VXLAN, Auto, and empty protocol
// values all map to the geneve0 interface in the eBPF path. IPIP maps to
// ipip0. None and WireGuard do not need either interface.
func neededTunnelProtocols(meshPeers []meshPeerInfo, gatewayPeers []gatewayPeerInfo) (needsGeneve, needsIPIP bool) {
	for _, p := range meshPeers {
		switch unboundednetv1alpha1.TunnelProtocol(p.TunnelProtocol) {
		case unboundednetv1alpha1.TunnelProtocolIPIP:
			needsIPIP = true
		case unboundednetv1alpha1.TunnelProtocolNone:
			// no interface needed
		default:
			// GENEVE, VXLAN, Auto, empty -- all use geneve0
			needsGeneve = true
		}
	}

	for _, p := range gatewayPeers {
		switch unboundednetv1alpha1.TunnelProtocol(p.TunnelProtocol) {
		case unboundednetv1alpha1.TunnelProtocolIPIP:
			needsIPIP = true
		case unboundednetv1alpha1.TunnelProtocolNone:
			// no interface needed
		default:
			needsGeneve = true
		}
	}

	return needsGeneve, needsIPIP
}

// configureEBPFTunnelPeers sets up a single flow-based GENEVE interface and
// programs BPF map entries so the TC classifier on the underlay interface
// redirects overlay traffic to geneve0 with the correct tunnel key.
//
// No kernel routes are needed -- the BPF program on the underlay egress
// intercepts overlay-destined packets (matched by LPM trie) before they
// exit via the default gateway and redirects them to geneve0 with tunnel
// metadata set.
//
// Tunnel interfaces (geneve0, ipip0) are only created when at least one
// peer uses the corresponding protocol. Unneeded interfaces are removed
// immediately so that cleanupUnusedTunnelDevices becomes a no-op.
func configureEBPFTunnelPeers(
	ctx context.Context,
	cfg *config,
	meshPeers []meshPeerInfo,
	gatewayPeers []gatewayPeerInfo,
	mySiteName string,
	peeredSites map[string]bool,
	networkPeeredSites map[string]bool,
	siteHealthCheckProfileNames map[string]string,
	peeringSiteHealthCheckProfileNames map[string]string,
	assignmentSiteHealthCheckProfileNames map[string]string,
	assignmentPoolHealthCheckProfileNames map[string]string,
	poolHealthCheckProfileNames map[string]string,
	siteTunnelMTUs map[string]int,
	peeringSiteTunnelMTUs map[string]int,
	assignmentSiteTunnelMTUs map[string]int,
	assignmentPoolTunnelMTUs map[string]int,
	poolTunnelMTUs map[string]int,
	state *wireGuardState,
) ([]unboundednetnetlink.DesiredRoute, map[string]bool, error) {
	// Do NOT early-return when both peer lists are empty. Even when
	// there are no GENEVE/IPIP/None tunnel-protocol peers (e.g.
	// WG-only gateway assignment), this function must still:
	//   1. Create the unbounded0 dummy interface + load the TC BPF program
	//   2. Build supernet routes on unbounded0 (from allGatewayPeersForSupernets)
	//   3. Clean up stale tunnel interfaces from previous runs
	// Without this, WG-only remote nodes never get the eBPF
	// infrastructure and the BPF map entries added later by
	// addWireGuardPeersToBPFMap are silently dropped.
	nlCache := state.netlinkCache
	ifName := cfg.GeneveInterfaceName
	ipipIfName := "ipip0"

	// Determine which tunnel interfaces are actually needed by scanning
	// all peers before creating any interfaces. This avoids unnecessary
	// interface churn and rp_filter resets.
	needsGeneve, needsIPIP := neededTunnelProtocols(meshPeers, gatewayPeers)

	// geneveMTU is used for supernet route MTU below. Initialize to the
	// configured MTU and refine if geneve0 is created.
	geneveMTU := cfg.MTU

	if defaultMTU := unboundednetnetlink.DetectDefaultRouteMTUFromCache(nlCache); defaultMTU > 0 {
		detected := defaultMTU - unboundednetnetlink.GeneveMTUOverhead
		if detected < geneveMTU {
			geneveMTU = detected
		}
	}

	var geneveIfIndex uint32

	if needsGeneve {
		// Ensure single flow-based GENEVE interface exists (external, no VNI).
		lm := unboundednetnetlink.NewLinkManager(ifName)
		if err := lm.EnsureGeneveInterface(uint32(cfg.GeneveVNI), cfg.GenevePort); err != nil {
			return nil, nil, fmt.Errorf("create flow-based GENEVE interface %s: %w", ifName, err)
		}

		if err := lm.SetLinkUpWithCache(nlCache); err != nil {
			return nil, nil, fmt.Errorf("bring up %s: %w", ifName, err)
		}

		// Strip any addresses from geneve0 -- with eBPF dataplane, overlay IPs
		// live on unbounded0 only. Stale addresses from a previous netlink
		// dataplane can interfere with decapsulated packet delivery.
		if _, _, err := lm.SyncAddresses(nil, true); err != nil {
			klog.V(2).Infof("eBPF: failed to clear addresses on %s: %v", ifName, err)
		}

		// Set the geneve0 MAC to a deterministic value derived from our underlay
		// IP. Remote nodes' BPF programs derive the inner dst MAC from the remote
		// underlay IP using the same formula, so the MACs must match.
		localUnderlayIP := selectUnderlayIP(state.nodeInternalIPs, cfg.TunnelIPFamily)
		if localUnderlayIP != nil {
			mac := ebpfpkg.TunnelMACFromIP(localUnderlayIP)
			if err := lm.SetLinkAddress(mac); err != nil {
				klog.Warningf("eBPF: failed to set MAC on %s: %v", ifName, err)
			} else {
				klog.V(2).Infof("eBPF: set %s MAC to %s (from underlay %s)", ifName, mac, localUnderlayIP)
			}
		}

		if err := lm.EnsureMTUWithCache(nlCache, geneveMTU); err != nil {
			klog.Warningf("eBPF tunnel: failed to set MTU on %s: %v", ifName, err)
		}

		// Disable rp_filter on geneve0 AFTER all link modifications (SetLinkUp,
		// SyncAddresses, SetLinkAddress, EnsureMTU). Some netlink operations
		// can cause the kernel to reset interface sysctls.
		disableRPFilter(ifName)
		ensureTunnelForwardAccept(ifName)

		// Get tunnel interface index for BPF map entries (redirect target).
		geneveIface, err := net.InterfaceByName(ifName)
		if err != nil {
			return nil, nil, fmt.Errorf("interface %s not found after creation: %w", ifName, err)
		}

		geneveIfIndex = uint32(geneveIface.Index)
	} else {
		// No peers need geneve0 -- remove it if it exists.
		lm := unboundednetnetlink.NewLinkManager(ifName)
		if lm.Exists() {
			klog.Infof("eBPF: removing %s (no peers use GENEVE)", ifName)

			if err := lm.DeleteLink(); err != nil {
				klog.V(2).Infof("eBPF: failed to remove %s: %v", ifName, err)
			}

			removeTunnelForwardAccept(ifName)
		}
	}

	var ipipIfIndex uint32

	if needsIPIP {
		// Ensure IPIP external interface for IPIP peers.
		ipipLM := unboundednetnetlink.NewLinkManager(ipipIfName)
		if err := ipipLM.EnsureIPIPExternalInterface(); err != nil {
			klog.Warningf("eBPF: failed to create IPIP external interface: %v", err)
		}

		if ipipIface, err := net.InterfaceByName(ipipIfName); err == nil {
			ipipIfIndex = uint32(ipipIface.Index)

			if err := ipipLM.SetLinkUpWithCache(nlCache); err != nil {
				klog.Warningf("eBPF: failed to bring up %s: %v", ipipIfName, err)
			}

			ipipMTU := cfg.MTU

			if defaultMTU := unboundednetnetlink.DetectDefaultRouteMTUFromCache(nlCache); defaultMTU > 0 {
				detected := defaultMTU - unboundednetnetlink.IPIPMTUOverhead
				if detected < ipipMTU {
					ipipMTU = detected
				}
			}

			if err := ipipLM.EnsureMTUWithCache(nlCache, ipipMTU); err != nil {
				klog.Warningf("eBPF: failed to set MTU on %s: %v", ipipIfName, err)
			}

			disableRPFilter(ipipIfName)
			ensureTunnelForwardAccept(ipipIfName)
			// IPIP is layer 3 and does not support MAC addresses; skip
			// SetLinkAddress (the kernel returns ENOTSUP).
		}
	} else {
		// No peers need ipip0 -- remove it if it exists.
		ipipLM := unboundednetnetlink.NewLinkManager(ipipIfName)
		if ipipLM.Exists() {
			klog.Infof("eBPF: removing %s (no peers use IPIP)", ipipIfName)

			if err := ipipLM.DeleteLink(); err != nil {
				klog.V(2).Infof("eBPF: failed to remove %s: %v", ipipIfName, err)
			}

			removeTunnelForwardAccept(ipipIfName)
		}
	}

	// Ensure shared eBPF tunnel map + TC on unbounded0.
	_, err := ensureEBPFTunnelMap(cfg, state)
	if err != nil {
		return nil, nil, err
	}

	// Build BPF map entries. Peers are routed to the appropriate tunnel
	// interface based on their protocol.
	bpfEntries := make(map[string]ebpfpkg.TunnelEndpoint)

	// Detect default route interface for TunnelProtocolNone (direct routing).
	_, defaultLinkIdx, _ := unboundednetnetlink.DetectDefaultRouteInterfaceFromCache(nlCache) //nolint:errcheck
	defaultIfIdx := uint32(defaultLinkIdx)

	for _, peer := range meshPeers {
		if len(peer.InternalIPs) == 0 || len(peer.PodCIDRs) == 0 {
			continue
		}

		underlayIP := selectUnderlayIP(peer.InternalIPs, cfg.TunnelIPFamily)
		if underlayIP == nil {
			continue
		}

		peerIfIdx, peerFlags, peerProto := resolveEBPFPeerTarget(peer.TunnelProtocol, geneveIfIndex, ipipIfIndex, defaultIfIdx, cfg)
		addPeerBPFEntries(bpfEntries, peer.PodCIDRs, underlayIP, uint32(cfg.GeneveVNI), peerIfIdx, peerFlags, peerProto, peer.Name)
	}

	for _, gwPeer := range gatewayPeers {
		if len(gwPeer.InternalIPs) == 0 || len(gwPeer.PodCIDRs) == 0 {
			continue
		}

		underlayIP := selectUnderlayIP(gwPeer.InternalIPs, cfg.TunnelIPFamily)
		if underlayIP == nil {
			continue
		}

		peerIfIdx, peerFlags, peerProto := resolveEBPFPeerTarget(gwPeer.TunnelProtocol, geneveIfIndex, ipipIfIndex, defaultIfIdx, cfg)
		addPeerBPFEntries(bpfEntries, gwPeer.PodCIDRs, underlayIP, uint32(cfg.GeneveVNI), peerIfIdx, peerFlags, peerProto, gwPeer.Name)
		addPeerBPFEntries(bpfEntries, gwPeer.RoutedCidrs, underlayIP, uint32(cfg.GeneveVNI), peerIfIdx, peerFlags, peerProto, gwPeer.Name)
	}

	// Store entries for deferred reconcile (after VXLAN and WG entries are added).
	state.mu.Lock()
	if state.pendingBPFEntries == nil {
		state.pendingBPFEntries = make(map[string]ebpfpkg.TunnelEndpoint)
	}

	for k, v := range bpfEntries {
		state.pendingBPFEntries[k] = v
	}
	state.mu.Unlock()

	// Remove stale per-peer GENEVE interfaces from previous netlink dataplane runs.
	removeStaleGeneveInterfaces(state, map[string]bool{ifName: true, ipipIfName: true})

	// Build supernet routes on unbounded0 to attract overlay traffic.
	// NOARP on unbounded0 means no neighbor resolution needed.
	// TC egress BPF intercepts and redirects to geneve0.
	devIface, devErr := net.InterfaceByName(unbounded0DeviceName)

	var routes []unboundednetnetlink.DesiredRoute

	if devErr == nil {
		supernets := collectSupernets(state, meshPeers, gatewayPeers, state.allGatewayPeersForSupernets)
		for cidrStr := range supernets {
			_, cidr, err := net.ParseCIDR(cidrStr)
			if err != nil {
				continue
			}

			routes = append(routes, unboundednetnetlink.DesiredRoute{
				Prefix: *cidr,
				Nexthops: []unboundednetnetlink.DesiredNexthop{{
					PeerID:    "ebpf/" + unbounded0DeviceName,
					LinkIndex: devIface.Index,
				}},
				Metric:            cfg.BaseMetric,
				MTU:               geneveMTU,
				Table:             0,
				HealthCheckImmune: true,
				ScopeGlobal:       true,
			})
		}
	}

	hcPeers := registerPeersWithHealthCheck(meshPeers, gatewayPeers, mySiteName, false,
		siteHealthCheckProfileNames, peeringSiteHealthCheckProfileNames,
		assignmentSiteHealthCheckProfileNames, assignmentPoolHealthCheckProfileNames,
		poolHealthCheckProfileNames, state, ebpfPeerIfaceName, true)

	klog.V(2).Infof("eBPF tunnel: configured %d mesh + %d gateway peers, %d BPF entries, %d supernet routes on %s",
		len(meshPeers), len(gatewayPeers), len(bpfEntries), len(routes), ifName)

	return routes, hcPeers, nil
}

// configureEBPFVXLANPeers sets up the shared vxlan0 interface and programs
// BPF map entries for VXLAN tunnel redirection.
func configureEBPFVXLANPeers(
	ctx context.Context,
	cfg *config,
	meshPeers []meshPeerInfo,
	gatewayPeers []gatewayPeerInfo,
	mySiteName string,
	peeredSites map[string]bool,
	siteHealthCheckProfileNames map[string]string,
	peeringSiteHealthCheckProfileNames map[string]string,
	assignmentSiteHealthCheckProfileNames map[string]string,
	assignmentPoolHealthCheckProfileNames map[string]string,
	poolHealthCheckProfileNames map[string]string,
	siteTunnelMTUs map[string]int,
	peeringSiteTunnelMTUs map[string]int,
	assignmentSiteTunnelMTUs map[string]int,
	assignmentPoolTunnelMTUs map[string]int,
	poolTunnelMTUs map[string]int,
	state *wireGuardState,
) ([]unboundednetnetlink.DesiredRoute, map[string]bool, error) {
	_ = ctx

	if len(meshPeers) == 0 && len(gatewayPeers) == 0 {
		return nil, nil, nil
	}

	nlCache := state.netlinkCache
	ifName := vxlanInterfaceName

	lm := unboundednetnetlink.NewLinkManager(ifName)
	if err := lm.EnsureVXLANInterface(cfg.VXLANPort, cfg.VXLANSrcPortLow, cfg.VXLANSrcPortHigh); err != nil {
		return nil, nil, err
	}

	if err := lm.SetLinkUp(); err != nil {
		return nil, nil, err
	}

	// Strip stale addresses from vxlan0 -- eBPF dataplane keeps overlay IPs
	// on unbounded0 only.
	if _, _, err := lm.SyncAddresses(nil, true); err != nil {
		klog.V(2).Infof("eBPF VXLAN: failed to clear addresses on %s: %v", ifName, err)
	}

	// Set deterministic MAC -- same rationale as geneve0.
	vxlanUnderlayIP := selectUnderlayIP(state.nodeInternalIPs, cfg.TunnelIPFamily)
	if vxlanUnderlayIP != nil {
		if err := lm.SetLinkAddress(ebpfpkg.TunnelMACFromIP(vxlanUnderlayIP)); err != nil {
			klog.Warningf("eBPF VXLAN: failed to set MAC on %s: %v", ifName, err)
		}
	}

	vxlanMTU := cfg.MTU

	if defaultMTU := unboundednetnetlink.DetectDefaultRouteMTUFromCache(nlCache); defaultMTU > 0 {
		detected := defaultMTU - unboundednetnetlink.VXLANMTUOverhead
		if detected < vxlanMTU {
			vxlanMTU = detected
		}
	}

	if err := lm.EnsureMTU(vxlanMTU); err != nil {
		klog.Warningf("eBPF VXLAN: failed to set MTU on %s: %v", ifName, err)
	}

	// Disable rp_filter on vxlan0 AFTER all link modifications (SetLinkUp,
	// SyncAddresses, SetLinkAddress, EnsureMTU). Some netlink operations
	// can cause the kernel to reset interface sysctls.
	disableRPFilter(ifName)
	ensureTunnelForwardAccept(ifName)

	iface, err := net.InterfaceByName(ifName)
	if err != nil {
		return nil, nil, err
	}

	tunnelIfIndex := uint32(iface.Index)
	vni := uint32(cfg.GeneveVNI)

	_, err = ensureEBPFTunnelMap(cfg, state)
	if err != nil {
		return nil, nil, err
	}

	bpfEntries := make(map[string]ebpfpkg.TunnelEndpoint)

	for _, peer := range meshPeers {
		if len(peer.InternalIPs) == 0 || len(peer.PodCIDRs) == 0 {
			continue
		}

		underlayIP := selectUnderlayIP(peer.InternalIPs, cfg.TunnelIPFamily)
		if underlayIP == nil {
			continue
		}

		addPeerBPFEntries(bpfEntries, peer.PodCIDRs, underlayIP, vni, tunnelIfIndex, ebpfpkg.TunnelFlagSetKey, ebpfpkg.TunnelProtoVXLAN, peer.Name)
	}

	for _, gwPeer := range gatewayPeers {
		if len(gwPeer.InternalIPs) == 0 || len(gwPeer.PodCIDRs) == 0 {
			continue
		}

		underlayIP := selectUnderlayIP(gwPeer.InternalIPs, cfg.TunnelIPFamily)
		if underlayIP == nil {
			continue
		}

		addPeerBPFEntries(bpfEntries, gwPeer.PodCIDRs, underlayIP, vni, tunnelIfIndex, ebpfpkg.TunnelFlagSetKey, ebpfpkg.TunnelProtoVXLAN, gwPeer.Name)
		addPeerBPFEntries(bpfEntries, gwPeer.RoutedCidrs, underlayIP, vni, tunnelIfIndex, ebpfpkg.TunnelFlagSetKey, ebpfpkg.TunnelProtoVXLAN, gwPeer.Name)
	}

	// Store entries for deferred reconcile.
	state.mu.Lock()
	if state.pendingBPFEntries == nil {
		state.pendingBPFEntries = make(map[string]ebpfpkg.TunnelEndpoint)
	}

	for k, v := range bpfEntries {
		state.pendingBPFEntries[k] = v
	}
	state.mu.Unlock()

	// Build supernet routes on unbounded0 for VXLAN overlay traffic.
	devIface, devErr := net.InterfaceByName(unbounded0DeviceName)

	var routes []unboundednetnetlink.DesiredRoute

	if devErr == nil {
		supernets := collectSupernets(state, meshPeers, gatewayPeers, state.allGatewayPeersForSupernets)
		for cidrStr := range supernets {
			_, cidr, err := net.ParseCIDR(cidrStr)
			if err != nil {
				continue
			}

			routes = append(routes, unboundednetnetlink.DesiredRoute{
				Prefix: *cidr,
				Nexthops: []unboundednetnetlink.DesiredNexthop{{
					PeerID:    "ebpf/" + unbounded0DeviceName,
					LinkIndex: devIface.Index,
				}},
				Metric:            cfg.BaseMetric,
				MTU:               vxlanMTU,
				Table:             0,
				HealthCheckImmune: true,
				ScopeGlobal:       true,
			})
		}
	}

	vxlanHCPeers := registerPeersWithHealthCheck(meshPeers, gatewayPeers, mySiteName, false,
		siteHealthCheckProfileNames, peeringSiteHealthCheckProfileNames,
		assignmentSiteHealthCheckProfileNames, assignmentPoolHealthCheckProfileNames,
		poolHealthCheckProfileNames, state, ebpfPeerIfaceName, true)

	klog.V(2).Infof("eBPF VXLAN: configured %d mesh + %d gateway peers, %d BPF entries, %d supernet routes on %s",
		len(meshPeers), len(gatewayPeers), len(bpfEntries), len(routes), ifName)

	return routes, vxlanHCPeers, nil
}

// addPeerBPFEntries adds LPM trie entries for a set of CIDRs pointing to a
// tunnel endpoint. The TUNNEL_F_HEALTHY flag is set automatically so new
// entries are forwarded immediately; the healthcheck callback clears it
// if the peer goes down. Supports both IPv4 and IPv6 CIDRs.
func addPeerBPFEntries(entries map[string]ebpfpkg.TunnelEndpoint, cidrs []string, underlayIP net.IP, vni, ifindex, flags, protocol uint32, peerName string) {
	for _, cidrStr := range cidrs {
		_, cidr, err := net.ParseCIDR(cidrStr)
		if err != nil {
			continue
		}

		key := cidr.String()
		ep := entries[key] // get existing (may be zero value)
		ep.Nexthops = append(ep.Nexthops, ebpfpkg.TunnelNexthop{
			RemoteIP: underlayIP,
			VNI:      vni,
			IfIndex:  ifindex,
			Flags:    flags | ebpfpkg.TunnelFlagHealthy,
			Protocol: protocol,
			PeerName: peerName,
		})
		entries[key] = ep
	}
}

// selectUnderlayIP picks the peer's underlay IP matching the configured tunnel IP family.
func selectUnderlayIP(internalIPs []string, ipFamily string) net.IP {
	for _, ipStr := range internalIPs {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			continue
		}

		if ipFamily == "IPv6" && ip.To4() == nil {
			return ip
		}

		if ipFamily == "IPv4" && ip.To4() != nil {
			return ip
		}
	}

	return nil
}

// resolveEBPFPeerTarget returns the tunnel ifindex, BPF flags, and protocol
// constant for a peer based on its tunnel protocol. GENEVE/VXLAN get
// set_tunnel_key; IPIP gets set_tunnel_key only; None redirects to the
// default route interface with no tunnel key.
func resolveEBPFPeerTarget(protocol string, geneveIfIdx, ipipIfIdx, defaultIfIdx uint32, cfg *config) (uint32, uint32, uint32) {
	switch unboundednetv1alpha1.TunnelProtocol(protocol) {
	case unboundednetv1alpha1.TunnelProtocolIPIP:
		return ipipIfIdx, ebpfpkg.TunnelFlagSetKey, ebpfpkg.TunnelProtoIPIP
	case unboundednetv1alpha1.TunnelProtocolVXLAN:
		return geneveIfIdx, ebpfpkg.TunnelFlagSetKey, ebpfpkg.TunnelProtoVXLAN
	case unboundednetv1alpha1.TunnelProtocolNone:
		// Direct routing: redirect to default route interface, no tunnel key.
		return defaultIfIdx, 0, ebpfpkg.TunnelProtoNone
	default:
		// GENEVE or unset -- use GENEVE interface
		return geneveIfIdx, ebpfpkg.TunnelFlagSetKey, ebpfpkg.TunnelProtoGENEVE
	}
}

// collectSupernets gathers the CIDRs that need routes on unbounded0 to attract
// overlay traffic through the bridge for BPF interception.
// Includes site pod CIDR pools (supernets) and gateway routedCIDRs.
// Individual peer podCIDRs are only added when no pool supernet covers them.
// allGatewayPeers includes ALL gateway peers (GENEVE, VXLAN, and WG) so that
// cross-site supernet routes are created regardless of the tunnel protocol
// used for the gateway link.
func collectSupernets(state *wireGuardState, meshPeers []meshPeerInfo, gatewayPeers, allGatewayPeers []gatewayPeerInfo) map[string]bool {
	supernets := make(map[string]bool)
	// Site pod CIDR assignment pools (supernets covering all node podCIDRs).
	for _, cidr := range state.sitePodCIDRPools {
		supernets[cidr] = true
	}
	// Gateway pool RoutedCidrs (cross-site supernets).
	for _, cidr := range state.sitePodCIDRs {
		supernets[cidr] = true
	}
	// Gateway peer routedCIDRs (remote site supernets advertised via gateways).
	// Use allGatewayPeers (all protocols) so cross-site routes are created
	// on unbounded0 even when the gateway link uses WireGuard.
	for _, gwPeer := range allGatewayPeers {
		for _, cidr := range gwPeer.RoutedCidrs {
			supernets[cidr] = true
		}
	}
	// Remove the local node's own podCIDRs (those are handled by the bridge directly).
	for _, cidr := range state.nodePodCIDRs {
		delete(supernets, cidr)
	}
	// Only add individual peer podCIDRs if no existing supernet covers them.
	addIfUncovered := func(cidrStr string) {
		_, cidr, err := net.ParseCIDR(cidrStr)
		if err != nil {
			return
		}

		for sn := range supernets {
			_, snNet, err := net.ParseCIDR(sn)
			if err != nil {
				continue
			}

			if snNet.Contains(cidr.IP) {
				snOnes, _ := snNet.Mask.Size()

				cOnes, _ := cidr.Mask.Size()
				if snOnes <= cOnes {
					return // covered by a broader supernet
				}
			}
		}

		supernets[cidrStr] = true
	}

	for _, peer := range meshPeers {
		for _, cidr := range peer.PodCIDRs {
			addIfUncovered(cidr)
		}
	}

	for _, gwPeer := range gatewayPeers {
		for _, cidr := range gwPeer.PodCIDRs {
			addIfUncovered(cidr)
		}
	}

	return supernets
}

// addWireGuardPeersToBPFMap adds WireGuard peer CIDRs to the pending BPF
// entries so that traffic routed via unbounded0 is redirected to the WireGuard
// interface. WG peers use redirect-only (no set_tunnel_key) -- the WireGuard
// driver handles crypto routing via its own AllowedIPs.
func addWireGuardPeersToBPFMap(cfg *config, state *wireGuardState, wgMeshPeers []meshPeerInfo, wgGatewayPeers []gatewayPeerInfo) {
	if state.tunnelMaps == nil || state.tunnelMaps["ebpf"] == nil {
		return
	}

	// Determine WG interface index. The mesh WG interface is wg<port>.
	wgIfName := fmt.Sprintf("wg%d", cfg.WireGuardPort)

	wgIface, err := net.InterfaceByName(wgIfName)
	if err != nil {
		klog.V(2).Infof("eBPF: WG interface %s not found, skipping BPF map entries", wgIfName)
		return
	}

	wgIfIndex := uint32(wgIface.Index)

	state.mu.Lock()
	if state.pendingBPFEntries == nil {
		state.pendingBPFEntries = make(map[string]ebpfpkg.TunnelEndpoint)
	}

	// Add mesh WG peers -- redirect only, no tunnel key.
	for _, peer := range wgMeshPeers {
		if len(peer.InternalIPs) == 0 || len(peer.PodCIDRs) == 0 {
			continue
		}

		underlayIP := selectUnderlayIP(peer.InternalIPs, cfg.TunnelIPFamily)
		if underlayIP == nil {
			continue
		}

		addPeerBPFEntries(state.pendingBPFEntries, peer.PodCIDRs, underlayIP, 0, wgIfIndex, 0, ebpfpkg.TunnelProtoWireGuard, peer.Name)
	}

	// Add gateway WG peers.
	for _, gwPeer := range wgGatewayPeers {
		if len(gwPeer.InternalIPs) == 0 {
			continue
		}

		underlayIP := selectUnderlayIP(gwPeer.InternalIPs, cfg.TunnelIPFamily)
		if underlayIP == nil {
			continue
		}

		if gwPeer.GatewayWireguardPort == 0 {
			continue
		}

		gwWgIfName := fmt.Sprintf("wg%d", gwPeer.GatewayWireguardPort)

		gwIface, gwErr := net.InterfaceByName(gwWgIfName)
		if gwErr != nil {
			continue
		}

		gwIfIdx := uint32(gwIface.Index)

		allCIDRs := append(gwPeer.PodCIDRs, gwPeer.RoutedCidrs...)
		addPeerBPFEntries(state.pendingBPFEntries, allCIDRs, underlayIP, 0, gwIfIdx, 0, ebpfpkg.TunnelProtoWireGuard, gwPeer.Name)
	}
	state.mu.Unlock()

	klog.V(2).Infof("eBPF: added %d WG mesh + %d WG gateway peers to BPF map (pending total: %d)",
		len(wgMeshPeers), len(wgGatewayPeers), len(state.pendingBPFEntries))
}

// reconcilePendingBPFEntries performs a single BPF map reconcile with all
// accumulated entries from GENEVE, VXLAN, and WireGuard config phases.
func reconcilePendingBPFEntries(state *wireGuardState) {
	if state.tunnelMaps == nil {
		return
	}

	tm := state.tunnelMaps["ebpf"]
	if tm == nil {
		return
	}

	state.mu.Lock()
	entries := state.pendingBPFEntries
	state.pendingBPFEntries = nil
	state.mu.Unlock()

	if entries == nil {
		entries = make(map[string]ebpfpkg.TunnelEndpoint)
	}

	if err := tm.Reconcile(entries); err != nil {
		klog.Warningf("eBPF: BPF map reconciliation error: %v", err)
	} else {
		klog.V(2).Infof("eBPF: reconciled BPF map with %d total entries", len(entries))
	}
}

// ebpfPeerIfaceName returns the shared tunnel interface name for any gateway
// peer in eBPF mode. Unlike the netlink dataplane which creates per-peer
// interfaces (gn<decimal>, vx<decimal>), eBPF uses a single shared interface.
func ebpfPeerIfaceName(gwPeer gatewayPeerInfo) string {
	switch gwPeer.TunnelProtocol {
	case "VXLAN":
		return vxlanInterfaceName
	case "IPIP":
		return "ipip0"
	default:
		return "geneve0"
	}
}

// disableRPFilter sets rp_filter=0 on the given interface and on the "all"
// pseudo-interface. Decapsulated inbound packets arrive on the tunnel
// interface (geneve0, vxlan0, ipip0) with overlay source IPs, but overlay
// routes point to unbounded0. Both strict (1) and loose (2) rp_filter would
// drop these packets because the reverse path does not match the arrival
// interface. The kernel uses max(all, iface) for the effective rp_filter
// value, so both must be set to 0.
//
// The container's /proc/sys may be an overlay mount that accepts writes but
// does not propagate them to the real kernel sysctl. We write through
// /proc/1/root/proc/sys/ which accesses PID 1's root filesystem and the
// real procfs (requires hostPID: true on the pod).
func disableRPFilter(ifName string) {
	for _, iface := range []string{ifName, "all"} {
		path := filepath.Join("/proc/1/root/proc/sys/net/ipv4/conf", iface, "rp_filter")
		if err := os.WriteFile(path, []byte("0\n"), 0o644); err != nil {
			klog.Warningf("failed to disable rp_filter on %s: %v", iface, err)
			continue
		}
		// Verify the write took effect via the same path.
		if val, err := os.ReadFile(path); err == nil {
			v := strings.TrimSpace(string(val))
			if v != "0" {
				klog.Warningf("rp_filter on %s is %s after writing 0 -- write did not take effect", iface, v)
			} else {
				klog.V(2).Infof("disabled rp_filter on %s", iface)
			}
		}
	}
}

// ensureTunnelForwardAccept adds an iptables FORWARD chain rule that accepts
// forwarded traffic arriving on the specified tunnel interface. This must be
// before KUBE-FORWARD (which drops ctstate INVALID packets) so that transit
// overlay traffic through gateway nodes is not dropped.
func ensureTunnelForwardAccept(ifName string) {
	check := exec.Command("nsenter", "-t", "1", "-n", "--",
		"iptables", "-C", "FORWARD",
		"-i", ifName, "-j", "ACCEPT",
		"-m", "comment", "--comment", "unbounded-net: accept tunnel traffic")
	if check.Run() == nil {
		return
	}

	out, err := exec.Command("nsenter", "-t", "1", "-n", "--",
		"iptables", "-I", "FORWARD", "1",
		"-i", ifName, "-j", "ACCEPT",
		"-m", "comment", "--comment", "unbounded-net: accept tunnel traffic").CombinedOutput()
	if err != nil {
		klog.Warningf("failed to add FORWARD accept rule for %s: %v (%s)", ifName, err, strings.TrimSpace(string(out)))
	} else {
		klog.V(2).Infof("added FORWARD accept rule for %s", ifName)
	}
}

// removeTunnelForwardAccept removes the FORWARD chain ACCEPT rule for
// the specified tunnel interface. Called when an interface is deleted.
func removeTunnelForwardAccept(ifName string) {
	out, err := exec.Command("nsenter", "-t", "1", "-n", "--",
		"iptables", "-D", "FORWARD",
		"-i", ifName, "-j", "ACCEPT",
		"-m", "comment", "--comment", "unbounded-net: accept tunnel traffic").CombinedOutput()
	if err != nil {
		// Rule may not exist if the interface was never set up -- ignore.
		klog.V(4).Infof("removeTunnelForwardAccept %s: %v (%s)", ifName, err, strings.TrimSpace(string(out)))
	} else {
		klog.V(2).Infof("removed FORWARD accept rule for %s", ifName)
	}
}

// using. This avoids leaving stale geneve0, vxlan0, or ipip0 interfaces
// when the tunnel protocol has been changed or when all peers use WireGuard.
func cleanupUnusedTunnelDevices(meshPeers []meshPeerInfo, gatewayPeers, wgGatewayPeers []gatewayPeerInfo) {
	usesGeneve := false
	usesVXLAN := false
	usesIPIP := false

	for _, p := range meshPeers {
		switch p.TunnelProtocol {
		case "GENEVE":
			usesGeneve = true
		case "VXLAN":
			usesVXLAN = true
		case "IPIP":
			usesIPIP = true
		}
	}

	for _, p := range gatewayPeers {
		switch p.TunnelProtocol {
		case "GENEVE":
			usesGeneve = true
		case "VXLAN":
			usesVXLAN = true
		case "IPIP":
			usesIPIP = true
		}
	}

	for _, p := range wgGatewayPeers {
		switch p.TunnelProtocol {
		case "GENEVE":
			usesGeneve = true
		case "VXLAN":
			usesVXLAN = true
		case "IPIP":
			usesIPIP = true
		}
	}

	type devInfo struct {
		name string
		used bool
	}

	devices := []devInfo{
		{"geneve0", usesGeneve},
		{vxlanInterfaceName, usesVXLAN},
		{"ipip0", usesIPIP},
	}
	for _, d := range devices {
		if d.used {
			continue
		}

		lm := unboundednetnetlink.NewLinkManager(d.name)
		if lm.Exists() {
			klog.Infof("eBPF: removing unused tunnel device %s (no peers use this protocol)", d.name)

			if err := lm.DeleteLink(); err != nil {
				klog.V(2).Infof("eBPF: failed to remove unused device %s: %v", d.name, err)
			}

			removeTunnelForwardAccept(d.name)
		}
	}
}

// reapplyRPFilterOnActiveTunnels sets rp_filter=0 on tunnel interfaces that
// are still in use. This must be called after cleanupUnusedTunnelDevices
// because deleting interfaces can cause the kernel to reset rp_filter on
// remaining interfaces.
func reapplyRPFilterOnActiveTunnels(meshPeers []meshPeerInfo, gatewayPeers, wgGatewayPeers []gatewayPeerInfo) {
	usesGeneve := false
	usesVXLAN := false
	usesIPIP := false

	for _, p := range meshPeers {
		switch p.TunnelProtocol {
		case "GENEVE":
			usesGeneve = true
		case "VXLAN":
			usesVXLAN = true
		case "IPIP":
			usesIPIP = true
		}
	}

	for _, p := range gatewayPeers {
		switch p.TunnelProtocol {
		case "GENEVE":
			usesGeneve = true
		case "VXLAN":
			usesVXLAN = true
		case "IPIP":
			usesIPIP = true
		}
	}

	for _, p := range wgGatewayPeers {
		switch p.TunnelProtocol {
		case "GENEVE":
			usesGeneve = true
		case "VXLAN":
			usesVXLAN = true
		case "IPIP":
			usesIPIP = true
		}
	}

	type devInfo struct {
		name string
		used bool
	}
	for _, d := range []devInfo{
		{"geneve0", usesGeneve},
		{vxlanInterfaceName, usesVXLAN},
		{"ipip0", usesIPIP},
	} {
		if d.used {
			disableRPFilter(d.name)
			ensureTunnelForwardAccept(d.name)
		}
	}
}
