// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

// resolveSingleScope returns the tunnel protocol from a single governing CRD
// object. If the value is nil or Auto, returns Auto so the caller falls back
// to the ConfigMap default.
func resolveSingleScope(scope *unboundednetv1alpha1.TunnelProtocol) unboundednetv1alpha1.TunnelProtocol {
	if scope == nil {
		return unboundednetv1alpha1.TunnelProtocolAuto
	}

	switch *scope {
	case unboundednetv1alpha1.TunnelProtocolAuto:
		return unboundednetv1alpha1.TunnelProtocolAuto
	case unboundednetv1alpha1.TunnelProtocolNone:
		return unboundednetv1alpha1.TunnelProtocolNone
	case unboundednetv1alpha1.TunnelProtocolVXLAN:
		return unboundednetv1alpha1.TunnelProtocolVXLAN
	default:
		return *scope
	}
}

// resolveAutoTunnelProtocol picks the tunnel protocol when the governing
// CRD object is Auto (or unset). Uses the preferred settings from the
// ConfigMap, with security-wins: links using external/public IPs always
// resolve to WireGuard unless explicitly overridden by the CRD object.
func resolveAutoTunnelProtocol(usesExternalIP bool, preferredPrivate, preferredPublic string) unboundednetv1alpha1.TunnelProtocol {
	if usesExternalIP {
		preferred := parsePreferredTunnelProtocol(preferredPublic, unboundednetv1alpha1.TunnelProtocolWireGuard)
		// Security-wins: if the link uses public IPs, force WireGuard
		// unless the ConfigMap explicitly chose something else.
		if preferred != unboundednetv1alpha1.TunnelProtocolWireGuard {
			// The ConfigMap explicitly opted out of encryption for public
			// links -- this is allowed but a startup warning is already
			// printed. Honor it.
			return preferred
		}

		return unboundednetv1alpha1.TunnelProtocolWireGuard
	}

	return parsePreferredTunnelProtocol(preferredPrivate, unboundednetv1alpha1.TunnelProtocolGENEVE)
}

// parsePreferredTunnelProtocol converts a config string to a TunnelProtocol,
// returning the fallback if the string is empty or unrecognized.
func parsePreferredTunnelProtocol(value string, fallback unboundednetv1alpha1.TunnelProtocol) unboundednetv1alpha1.TunnelProtocol {
	switch unboundednetv1alpha1.TunnelProtocol(value) {
	case unboundednetv1alpha1.TunnelProtocolWireGuard,
		unboundednetv1alpha1.TunnelProtocolIPIP,
		unboundednetv1alpha1.TunnelProtocolGENEVE,
		unboundednetv1alpha1.TunnelProtocolVXLAN,
		unboundednetv1alpha1.TunnelProtocolNone:
		return unboundednetv1alpha1.TunnelProtocol(value)
	default:
		return fallback
	}
}

// effectiveTunnelProtocol returns the final tunnel protocol for a link.
// The governing scope is the single CRD object that controls this link type.
// If it is nil/Auto, the ConfigMap default is used (with security-wins for
// public IPs).
func effectiveTunnelProtocol(usesExternalIP bool, preferredPrivate, preferredPublic string, governingScope *unboundednetv1alpha1.TunnelProtocol) unboundednetv1alpha1.TunnelProtocol {
	resolved := resolveSingleScope(governingScope)
	if resolved == unboundednetv1alpha1.TunnelProtocolAuto {
		return resolveAutoTunnelProtocol(usesExternalIP, preferredPrivate, preferredPublic)
	}

	return resolved
}

// tunnelProtoPtr converts a string map value to a TunnelProtocol pointer.
// Returns nil when the key is absent (treated as Auto).
func tunnelProtoPtr(m map[string]string, key string) *unboundednetv1alpha1.TunnelProtocol {
	v, ok := m[key]
	if !ok || v == "" {
		return nil
	}

	et := unboundednetv1alpha1.TunnelProtocol(v)

	return &et
}

// resolveTunnelProtocolsOnPeers sets the TunnelProtocol field on each peer
// using the single governing CRD object for that link type:
//
//   - Same-site mesh peers:       Site
//   - Peered nodes (diff sites):  SitePeering
//   - Site to gateway pool:       SiteGatewayPoolAssignment (keyed by mySiteName|poolName)
//   - Same-pool gateways:         GatewayPool
//   - Pool-to-pool gateways:      GatewayPoolPeering (via peer's HealthCheckProfileName)
//
// Gateway nodes with same-site mesh peers use the SiteGatewayPoolAssignment
// scope instead of the Site scope, because the non-gateway peer on the other
// end resolves the link as a "site to gateway pool" connection. Both ends must
// agree on the tunnel protocol.
//
// localPoolIsExternal indicates whether the local node belongs to an External
// gateway pool. When true and the peer is cross-site, WireGuard is selected
// even if the remote pool is Internal -- because one end of the link uses
// external/public IPs.
func resolveTunnelProtocolsOnPeers(
	meshPeers []meshPeerInfo,
	gatewayPeers []gatewayPeerInfo,
	mySiteName string,
	peeredSites map[string]bool,
	networkPeeredSites map[string]bool,
	isGatewayNode bool,
	localPoolIsExternal bool,
	localGatewayPools []string,
	preferredPrivate, preferredPublic string,
	siteTunnelProtos map[string]string,
	peeringSiteTunnelProtos map[string]string,
	assignmentPoolTunnelProtos map[string]string,
	poolTunnelProtos map[string]string,
) {
	for i := range meshPeers {
		peer := &meshPeers[i]
		usesExternal := !peeredSites[peer.SiteName] && !networkPeeredSites[peer.SiteName] && peer.SiteName != mySiteName

		var scope *unboundednetv1alpha1.TunnelProtocol

		if peer.SiteName == mySiteName || peeredSites[peer.SiteName] {
			if peer.SiteName != mySiteName {
				// Peered nodes in different sites -> SitePeering
				scope = tunnelProtoPtr(peeringSiteTunnelProtos, peer.SiteName)
			} else if isGatewayNode && len(localGatewayPools) > 0 {
				// Gateway node with same-site mesh peers: use the SGPA
				// scope so both ends agree. The non-gateway peer resolves
				// this link as "site to gateway pool" via the SGPA, so the
				// gateway end must use the same scope.
				scope = tunnelProtoPtr(assignmentPoolTunnelProtos, mySiteName+"|"+localGatewayPools[0])
				// Fall back to Site if SGPA is nil/Auto.
				if scope == nil || *scope == unboundednetv1alpha1.TunnelProtocolAuto {
					siteScope := tunnelProtoPtr(siteTunnelProtos, mySiteName)
					if siteScope != nil && *siteScope != unboundednetv1alpha1.TunnelProtocolAuto {
						scope = siteScope
					}
				}
			} else {
				// Same-site peers -> Site
				scope = tunnelProtoPtr(siteTunnelProtos, mySiteName)
			}
		}
		// Non-peered, non-same-site peers: scope stays nil -> Auto
		peer.TunnelProtocol = string(effectiveTunnelProtocol(usesExternal, preferredPrivate, preferredPublic, scope))
	}

	for i := range gatewayPeers {
		gw := &gatewayPeers[i]
		// WireGuard is required when either end of the peering uses external
		// IPs: the remote pool is External, or the local node's pool is
		// External.  Both conditions require a cross-site, non-network-peered
		// link.
		usesExternal := (gw.PoolType == "External" || (isGatewayNode && localPoolIsExternal)) &&
			gw.SiteName != mySiteName &&
			!networkPeeredSites[gw.SiteName]

		var scope *unboundednetv1alpha1.TunnelProtocol
		if isGatewayNode {
			// Same-pool gateways -> GatewayPool
			scope = tunnelProtoPtr(poolTunnelProtos, gw.PoolName)
		} else {
			// Site to gateway pool -> SiteGatewayPoolAssignment
			scope = tunnelProtoPtr(assignmentPoolTunnelProtos, mySiteName+"|"+gw.PoolName)
		}
		// If the SGPA/pool scope is nil or Auto and the gateway is in the
		// same site, fall back to the Site tunnelProtocol setting.
		if (scope == nil || *scope == unboundednetv1alpha1.TunnelProtocolAuto) && gw.SiteName == mySiteName {
			siteScope := tunnelProtoPtr(siteTunnelProtos, mySiteName)
			if siteScope != nil && *siteScope != unboundednetv1alpha1.TunnelProtocolAuto {
				scope = siteScope
			}
		}

		gw.TunnelProtocol = string(effectiveTunnelProtocol(usesExternal, preferredPrivate, preferredPublic, scope))
	}
}
