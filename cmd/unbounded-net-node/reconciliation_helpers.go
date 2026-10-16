// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	"github.com/Azure/unbounded/internal/net/healthcheck"
)

func healthCheckLogScope(gvr schema.GroupVersionResource, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return gvr.String()
	}

	return fmt.Sprintf("%s, Name=%s", gvr.String(), name)
}

func effectiveSiteHealthCheckSettings(mySiteName string, siteMap map[string]*unboundedv1alpha3.Site) (bool, healthcheck.HealthCheckSettings) {
	if mySiteName == "" {
		return true, healthcheck.DefaultSettings()
	}

	site := siteMap[mySiteName]
	if site == nil {
		return true, healthcheck.DefaultSettings()
	}

	return healthCheckProfileFromSettings(site.Spec.HealthCheckSettings, healthCheckLogScope(siteGVR, site.Name))
}

// healthCheckProfileFromSettings converts CRD HealthCheckSettings into a healthcheck.HealthCheckSettings.
func healthCheckProfileFromSettings(settings *unboundednetv1alpha1.HealthCheckSettings, source string) (bool, healthcheck.HealthCheckSettings) {
	enabled := true
	profile := healthcheck.DefaultSettings()

	if settings == nil {
		klog.V(4).Infof("Parsed healthCheckSettings for %s: settings=nil, enabled=true, profile overrides empty", source)
		return enabled, profile
	}

	if settings.Enabled != nil {
		enabled = *settings.Enabled
	}

	if settings.DetectMultiplier != nil {
		profile.DetectMultiplier = int(*settings.DetectMultiplier)
	}

	if value, ok := intOrStringMilliseconds(settings.ReceiveInterval, source+".spec.healthCheckSettings.receiveInterval"); ok {
		profile.ReceiveInterval = time.Duration(value) * time.Millisecond
	}

	if value, ok := intOrStringMilliseconds(settings.TransmitInterval, source+".spec.healthCheckSettings.transmitInterval"); ok {
		profile.TransmitInterval = time.Duration(value) * time.Millisecond
	}

	klog.V(4).Infof(
		"Parsed healthCheckSettings for %s: enabled=%t detectMultiplier=%d receiveInterval=%v transmitInterval=%v",
		source,
		enabled,
		profile.DetectMultiplier,
		profile.ReceiveInterval,
		profile.TransmitInterval,
	)

	return enabled, profile
}

func healthCheckProfileNameForSite(siteName string) string {
	return "s-" + siteName
}

func healthCheckProfileNameForGatewayPool(poolName string) string {
	return "gp-" + poolName
}

func healthCheckProfileNameForSitePeering(peeringName string) string {
	return "sp-" + peeringName
}

func healthCheckProfileNameForSiteGatewayPoolAssignment(name string) string {
	return "sgpa-" + name
}

// healthCheckProfileNameForGatewayPoolPeering returns the profile name prefix for a GatewayPoolPeering.
func healthCheckProfileNameForGatewayPoolPeering(name string) string {
	return "gpp-" + name
}

func healthCheckProfilesEqual(a, b healthcheck.HealthCheckSettings) bool {
	return a == b
}

// mergeAssignmentHealthCheckState merges SiteGatewayPoolAssignment health check settings into active maps.
// includedPools, when non-empty, limits processing to assignment pools present in the set.
func mergeAssignmentHealthCheckState(
	assignment unboundednetv1alpha1.SiteGatewayPoolAssignment,
	mySiteName string,
	includedPools map[string]struct{},
	healthCheckProfiles map[string]healthcheck.HealthCheckSettings,
	healthCheckProfileSources map[string]string,
	assignmentPoolHealthCheckProfileNames map[string]string,
	assignmentPoolHealthCheckSourceAssignment map[string]string,
	assignmentSiteHealthCheckProfileNames map[string]string,
	assignmentSiteHealthCheckSourceAssignment map[string]string,
) {
	assignmentHealthCheckProfileName := disabledHealthCheckProfile

	assignmentScope := healthCheckLogScope(siteGatewayPoolAssignmentGVR, assignment.Name)
	if enabled, profile := healthCheckProfileFromSettings(assignment.Spec.HealthCheckSettings, assignmentScope); enabled {
		assignmentHealthCheckProfileName = healthCheckProfileNameForSiteGatewayPoolAssignment(assignment.Name)
		if existing, ok := healthCheckProfiles[assignmentHealthCheckProfileName]; ok && !healthCheckProfilesEqual(existing, profile) {
			klog.Warningf("Conflicting health check settings for SiteGatewayPoolAssignment %q profile %q; keeping first profile values", assignment.Name, assignmentHealthCheckProfileName)
		} else {
			healthCheckProfiles[assignmentHealthCheckProfileName] = profile
			if healthCheckProfileSources != nil {
				healthCheckProfileSources[assignmentHealthCheckProfileName] = assignmentScope
			}
		}
	}

	for _, poolName := range assignment.Spec.GatewayPools {
		poolName = strings.TrimSpace(poolName)
		if poolName == "" {
			continue
		}

		if len(includedPools) > 0 {
			if _, ok := includedPools[poolName]; !ok {
				continue
			}
		}

		for _, siteName := range assignment.Spec.Sites {
			siteName = strings.TrimSpace(siteName)
			if siteName == "" {
				continue
			}

			compositeKey := siteName + "|" + poolName
			if existing, ok := assignmentPoolHealthCheckProfileNames[compositeKey]; ok && existing != assignmentHealthCheckProfileName {
				keptFrom := assignmentPoolHealthCheckSourceAssignment[compositeKey]
				klog.V(3).Infof("Conflicting health check settings for site %q / gatewayPool %q: keeping profile %q from SiteGatewayPoolAssignment %q; ignoring profile %q from SiteGatewayPoolAssignment %q", siteName, poolName, existing, keptFrom, assignmentHealthCheckProfileName, assignment.Name)
			} else {
				assignmentPoolHealthCheckProfileNames[compositeKey] = assignmentHealthCheckProfileName
				assignmentPoolHealthCheckSourceAssignment[compositeKey] = assignment.Name
			}
		}
	}

	for _, siteName := range assignment.Spec.Sites {
		siteName = strings.TrimSpace(siteName)
		if siteName == "" || siteName == mySiteName {
			continue
		}

		if existing, ok := assignmentSiteHealthCheckProfileNames[siteName]; ok && existing != assignmentHealthCheckProfileName {
			keptFrom := assignmentSiteHealthCheckSourceAssignment[siteName]
			klog.Warningf("Conflicting health check settings for assigned site %q: keeping profile %q from SiteGatewayPoolAssignment %q; ignoring profile %q from SiteGatewayPoolAssignment %q", siteName, existing, keptFrom, assignmentHealthCheckProfileName, assignment.Name)
		} else {
			assignmentSiteHealthCheckProfileNames[siteName] = assignmentHealthCheckProfileName
			assignmentSiteHealthCheckSourceAssignment[siteName] = assignment.Name
		}
	}
}

// resolveMeshPeerHealthCheckProfileName resolves health check profile for mesh (node-to-node) links.
//
// Health check profile precedence (most specific association wins):
//  1. Explicit peer override (peer.HealthCheckProfileName): always wins if set.
//     Same-pool gateway peers have this set from GatewayPool settings.
//     Cross-pool gateway peers have this set from GatewayPoolPeering settings.
//  2. SiteGatewayPoolAssignment: connects a particular site to a particular pool
//  3. SitePeering: connects two specific sites
//  4. Site: least specific -- default for all peers in the site
//
// GatewayPool and GatewayPoolPeering settings are resolved at peer construction
// time and baked into peer.HealthCheckProfileName, so they always have highest priority.
// This ordering applies uniformly to all node types (gateway and non-gateway).
func resolveMeshPeerHealthCheckProfileName(isGatewayNode bool, peer meshPeerInfo, mySiteName string, siteHealthCheckProfileNames, peeringSiteHealthCheckProfileNames, assignmentSiteHealthCheckProfileNames map[string]string) string {
	if peer.HealthCheckProfileName != "" {
		return peer.HealthCheckProfileName
	}

	if profileName := assignmentSiteHealthCheckProfileNames[peer.SiteName]; profileName != "" {
		return profileName
	}

	if profileName := peeringSiteHealthCheckProfileNames[peer.SiteName]; profileName != "" {
		return profileName
	}

	return siteHealthCheckProfileNames[peer.SiteName]
}

// resolveGatewayPeerHealthCheckProfileName resolves health check profile for gateway links.
//
// Health check profile precedence for gateway peers (most specific wins):
//   - Explicit peer override (peer.HealthCheckProfileName): always wins if set.
//     For same-pool gateways this is set from GatewayPool settings.
//     For cross-pool gateways this is set from GatewayPoolPeering settings.
//   - Gateway node fallback: GatewayPool settings (poolHealthCheckProfileNames)
//   - Non-gateway node: SiteGatewayPoolAssignment settings
//     (assignmentPoolHealthCheckProfileNames, keyed by "siteName|poolName")
//
// Summary of governing relationship by link type:
//   - Gateway to gateway (same pool):      GatewayPool settings
//   - Gateway to gateway (different pool):  GatewayPoolPeering settings
//   - Node to gateway (associated pool):    SiteGatewayPoolAssignment settings
func resolveGatewayPeerHealthCheckProfileName(isGatewayNode bool, mySiteName string, peer gatewayPeerInfo, assignmentPoolHealthCheckProfileNames, poolHealthCheckProfileNames map[string]string) string {
	if peer.HealthCheckProfileName != "" {
		return peer.HealthCheckProfileName
	}

	if isGatewayNode {
		if profile := poolHealthCheckProfileNames[peer.PoolName]; profile != "" {
			return profile
		}

		return assignmentPoolHealthCheckProfileNames[peer.SiteName+"|"+peer.PoolName]
	}
	// Non-gateway nodes use the assignment profile keyed by their own site.
	return assignmentPoolHealthCheckProfileNames[mySiteName+"|"+peer.PoolName]
}

// tunnelMTUFromSpec extracts the tunnel MTU from a CRD spec field.
// Returns 0 if the pointer is nil (meaning "no override").
func tunnelMTUFromSpec(mtu *int32) int {
	if mtu == nil {
		return 0
	}

	return int(*mtu)
}

func mergeLowestTunnelMTU(tunnelMTUs map[string]int, scope string, mtu int) {
	scope = strings.TrimSpace(scope)
	if scope == "" || mtu <= 0 {
		return
	}

	if current := tunnelMTUs[scope]; current == 0 || mtu < current {
		tunnelMTUs[scope] = mtu
	}
}

type poolPairMTUKey struct {
	a string
	b string
}

func canonicalPoolPair(a, b string) poolPairMTUKey {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if b < a {
		a, b = b, a
	}

	return poolPairMTUKey{a: a, b: b}
}

func gatewayPoolPeeringEdgeTunnelMTUs(peerings []unboundednetv1alpha1.GatewayPoolPeering) map[poolPairMTUKey]int {
	result := make(map[poolPairMTUKey]int)

	for _, peering := range peerings {
		if !unboundednetv1alpha1.SpecEnabled(peering.Spec.Enabled) {
			continue
		}

		mtu := tunnelMTUFromSpec(peering.Spec.TunnelMTU)
		if mtu == 0 {
			continue
		}

		for i, a := range peering.Spec.GatewayPools {
			for _, b := range peering.Spec.GatewayPools[i+1:] {
				key := canonicalPoolPair(a, b)
				if key.a != "" && key.a != key.b {
					result[key] = resolveTunnelMTU(result[key], mtu)
				}
			}
		}
	}

	return result
}

func gatewayPoolPeeringTunnelMTU(localPools []string, remotePool string, edgeMTUs map[poolPairMTUKey]int) int {
	result := 0
	for _, pool := range localPools {
		result = resolveTunnelMTU(result, edgeMTUs[canonicalPoolPair(pool, remotePool)])
	}

	return result
}

type sitePoolMTUKey struct {
	site string
	pool string
}

func assignmentEdgeTunnelMTUs(assignments []unboundednetv1alpha1.SiteGatewayPoolAssignment) map[sitePoolMTUKey]int {
	result := make(map[sitePoolMTUKey]int)

	for _, assignment := range assignments {
		if !unboundednetv1alpha1.SpecEnabled(assignment.Spec.Enabled) {
			continue
		}

		mtu := tunnelMTUFromSpec(assignment.Spec.TunnelMTU)
		if mtu == 0 {
			continue
		}

		for _, site := range assignment.Spec.Sites {
			for _, pool := range assignment.Spec.GatewayPools {
				key := sitePoolMTUKey{site: site, pool: pool}
				result[key] = resolveTunnelMTU(result[key], mtu)
			}
		}
	}

	return result
}

func learnedGatewayTransitTunnelMTU(
	learnedRoutes map[string]unboundednetv1alpha1.GatewayNodeRoute,
	siteMTUs, poolMTUs map[string]int,
	assignmentMTUs map[sitePoolMTUKey]int,
	poolPeeringMTUs map[poolPairMTUKey]int,
) int {
	result := 0

	for _, route := range learnedRoutes {
		for _, path := range route.Paths {
			for index, hop := range path {
				switch hop.Type {
				case "Site":
					result = resolveTunnelMTU(result, siteMTUs[hop.Name])
				case "GatewayPool":
					result = resolveTunnelMTU(result, poolMTUs[hop.Name])
				}

				if index == 0 {
					continue
				}

				// Paths run from origin to advertiser. Only adjacent hops
				// identify a forwarding edge; co-occurrence is not enough.
				previous := path[index-1]
				if previous.Type == "GatewayPool" && hop.Type == "GatewayPool" {
					result = resolveTunnelMTU(result, poolPeeringMTUs[canonicalPoolPair(previous.Name, hop.Name)])

					continue
				}

				site, pool := previous, hop
				if site.Type == "GatewayPool" && pool.Type == "Site" {
					site, pool = pool, site
				}

				if site.Type != "Site" || pool.Type != "GatewayPool" {
					continue
				}

				result = resolveTunnelMTU(result, assignmentMTUs[sitePoolMTUKey{site: site.Name, pool: pool.Name}])
			}
		}
	}

	return result
}

type sitePairMTUKey struct {
	a string
	b string
}

func sitePeeringEdgeTunnelMTUs(peerings []unboundednetv1alpha1.SitePeering) map[sitePairMTUKey]int {
	result := make(map[sitePairMTUKey]int)

	for _, peering := range peerings {
		if !unboundednetv1alpha1.SpecEnabled(peering.Spec.Enabled) {
			continue
		}

		mtu := tunnelMTUFromSpec(peering.Spec.TunnelMTU)
		if mtu == 0 {
			continue
		}

		for _, a := range peering.Spec.Sites {
			for _, b := range peering.Spec.Sites {
				if a < b {
					key := sitePairMTUKey{a: a, b: b}
					result[key] = resolveTunnelMTU(result[key], mtu)
				}
			}
		}
	}

	return result
}

func applyPublishedGatewayMTUs(peers []gatewayPeerInfo, pools map[string]*unboundednetv1alpha1.GatewayPool, nodes map[string]*unboundednetv1alpha1.GatewayPoolNode) {
	nodeMTU := func(name string) int {
		if node := nodes[name]; node != nil && node.Status.ForwardingMTU != nil {
			return int(*node.Status.ForwardingMTU)
		}

		return 0
	}

	poolMTUs := make(map[string]int, len(pools))
	for name, pool := range pools {
		for _, node := range pool.Status.Nodes {
			poolMTUs[name] = resolveTunnelMTU(poolMTUs[name], nodeMTU(node.Name))
		}
	}

	for i := range peers {
		mtu := nodeMTU(peers[i].Name)
		for _, route := range peers[i].LearnedRoutes {
			for _, path := range route.Paths {
				for _, hop := range path {
					if hop.Type == "GatewayPool" && hop.Name != peers[i].PoolName {
						mtu = resolveTunnelMTU(mtu, poolMTUs[hop.Name])
					}
				}
			}
		}

		peers[i].TransitTunnelMTU = resolveTunnelMTU(peers[i].TransitTunnelMTU, mtu)
	}
}

func gatewayMeshTransitTunnelMTU(
	peerPool, peerSite string,
	routes map[string]unboundednetv1alpha1.GatewayNodeRoute,
	pools map[string]*unboundednetv1alpha1.GatewayPool,
	siteMTUs map[string]int,
	peeringMTUs map[sitePairMTUKey]int,
) int {
	result := 0

	for _, route := range routes {
		for _, path := range route.Paths {
			for _, hop := range path {
				if hop.Type != "GatewayPool" {
					continue
				}

				if hop.Name == peerPool {
					result = resolveTunnelMTU(result, siteMTUs[peerSite])
				} else if pool := pools[hop.Name]; pool != nil {
					for _, node := range pool.Status.Nodes {
						result = resolveTunnelMTU(result, siteMTUs[node.SiteName])
					}
				}
			}

			for i := 1; i < len(path); i++ {
				site, pool := path[i-1], path[i]
				if site.Type != "Site" || pool.Type != "GatewayPool" {
					continue
				}

				gatewaySites := []string{peerSite}
				if pool.Name != peerPool {
					// Intermediate pool paths identify a pool, not a specific
					// gateway. Honor every candidate mesh edge in that pool.
					gatewaySites = nil

					if downstream := pools[pool.Name]; downstream != nil {
						for _, node := range downstream.Status.Nodes {
							gatewaySites = append(gatewaySites, node.SiteName)
						}
					}
				}

				for _, gatewaySite := range gatewaySites {
					result = resolveTunnelMTU(result, siteMTUs[gatewaySite])
					if gatewaySite == site.Name || gatewaySite == "" {
						continue
					}

					a, b := gatewaySite, site.Name
					if a > b {
						a, b = b, a
					}

					result = resolveTunnelMTU(result, peeringMTUs[sitePairMTUKey{a: a, b: b}])
				}
			}
		}
	}

	return result
}

// resolveTunnelMTU returns the effective tunnel MTU for a given scope,
// taking the minimum of the global MTU and any CRD-specified MTU values.
// Zero values in mtuValues are ignored (meaning "no override").
func resolveTunnelMTU(globalMTU int, mtuValues ...int) int {
	result := 0
	if globalMTU > 0 {
		result = globalMTU
	}

	for _, v := range mtuValues {
		if v > 0 && (result == 0 || v < result) {
			result = v
		}
	}

	return result
}

// resolveMeshPeerTunnelMTU resolves the effective tunnel MTU for a mesh peer.
// The resolution considers: global MTU, local site MTU, and for cross-site peers,
// the peering MTU and assignment MTU for the remote site.
func resolveMeshPeerTunnelMTU(globalMTU int, peer meshPeerInfo, mySiteName string,
	siteTunnelMTUs, peeringSiteTunnelMTUs map[string]int,
	assignmentSiteTunnelMTUs map[string]int,
	includeAssignmentMTU bool,
) int {
	values := []int{siteTunnelMTUs[mySiteName], siteTunnelMTUs[peer.SiteName]}
	if peer.SiteName != mySiteName {
		values = append(values, peeringSiteTunnelMTUs[peer.SiteName])
	}

	if includeAssignmentMTU {
		values = append(values, assignmentSiteTunnelMTUs[peer.SiteName])
	}

	return resolveTunnelMTU(globalMTU, values...)
}

// resolveGatewayPeerTunnelMTU resolves the effective tunnel MTU for a gateway peer.
// The resolution considers: global MTU, local site MTU, assignment pool MTU,
// and gateway pool MTU.
func resolveGatewayPeerTunnelMTU(globalMTU int, mySiteName string, peer gatewayPeerInfo,
	siteTunnelMTUs, assignmentPoolTunnelMTUs map[string]int,
	poolTunnelMTUs map[string]int,
) int {
	return resolveTunnelMTU(globalMTU,
		siteTunnelMTUs[mySiteName],
		siteTunnelMTUs[peer.SiteName],
		assignmentPoolTunnelMTUs[peer.PoolName],
		poolTunnelMTUs[peer.PoolName],
		peer.TransitTunnelMTU,
	)
}

func intOrStringMilliseconds(value *intstr.IntOrString, fieldRef string) (int, bool) {
	if value == nil {
		klog.V(4).Infof("Parsing healthCheckSettings interval for %s: value=nil", fieldRef)
		return 0, false
	}

	raw := strings.TrimSpace(value.StrVal)
	klog.V(4).Infof("Parsing healthCheckSettings interval for %s: type=%d int=%d raw=%q", fieldRef, value.Type, value.IntValue(), raw)

	var (
		ms int
		ok bool
	)

	switch value.Type {
	case intstr.Int:
		if value.IntValue() > 0 {
			ms, ok = value.IntValue(), true
		} else if raw != "" {
			if milliseconds, err := strconv.Atoi(raw); err == nil {
				ms, ok = milliseconds, true
			} else {
				klog.Warningf("Ignoring invalid health check interval value %q for %s", raw, fieldRef)
				return 0, false
			}
		} else {
			return 0, false
		}
	case intstr.String:
		if raw == "" {
			return 0, false
		}

		if milliseconds, err := strconv.Atoi(raw); err == nil {
			ms, ok = milliseconds, true
		} else if parsed, err := time.ParseDuration(raw); err == nil {
			ms, ok = int(parsed/time.Millisecond), true
		} else {
			klog.Warningf("Ignoring invalid health check interval value %q for %s", raw, fieldRef)
			return 0, false
		}
	default:
		return 0, false
	}

	if ok {
		ms = clampHealthCheckIntervalMs(ms, fieldRef)
	}

	return ms, ok
}

const (
	healthCheckIntervalMinMs = 1
	healthCheckIntervalMaxMs = 60000
)

// clampHealthCheckIntervalMs restricts a health check interval to [1, 60000] ms to prevent
// integer overflow or nonsensical values. Logs a warning when clamping.
func clampHealthCheckIntervalMs(ms int, fieldRef string) int {
	if ms < healthCheckIntervalMinMs {
		klog.Warningf("Health check interval %d ms for %s is below minimum; clamping to %d ms", ms, fieldRef, healthCheckIntervalMinMs)
		return healthCheckIntervalMinMs
	}

	if ms > healthCheckIntervalMaxMs {
		klog.Warningf("Health check interval %d ms for %s exceeds maximum; clamping to %d ms", ms, fieldRef, healthCheckIntervalMaxMs)
		return healthCheckIntervalMaxMs
	}

	return ms
}

// shouldIncludeSliceForGatewayNode determines whether a gateway node should include
// a SiteNodeSlice as mesh peers.
//
// Gateway mesh peers are limited to sites explicitly present in SiteGatewayPoolAssignment.
// For internal pools, assigned sites must be local or directly connected.
// For external pools, assigned sites may be included without direct connectivity.
func shouldIncludeSliceForGatewayNode(siteName, mySiteName string, connectedSiteSet, assignedSiteSet map[string]bool, allowAssignedRemote bool) bool {
	if !assignedSiteSet[siteName] {
		return false
	}

	if allowAssignedRemote {
		return true
	}

	if siteName == mySiteName {
		return true
	}

	return connectedSiteSet[siteName]
}

// parseSitePeering converts an unstructured object to a SitePeering.
func parseSitePeering(obj *unstructured.Unstructured) (*unboundednetv1alpha1.SitePeering, error) {
	data, err := obj.MarshalJSON()
	if err != nil {
		return nil, err
	}

	var peering unboundednetv1alpha1.SitePeering
	if err := json.Unmarshal(data, &peering); err != nil {
		return nil, err
	}

	return &peering, nil
}

// parseSiteGatewayPoolAssignment converts an unstructured object to a SiteGatewayPoolAssignment.
func parseSiteGatewayPoolAssignment(obj *unstructured.Unstructured) (*unboundednetv1alpha1.SiteGatewayPoolAssignment, error) {
	data, err := obj.MarshalJSON()
	if err != nil {
		return nil, err
	}

	var assignment unboundednetv1alpha1.SiteGatewayPoolAssignment
	if err := json.Unmarshal(data, &assignment); err != nil {
		return nil, err
	}

	return &assignment, nil
}

// parseGatewayPoolPeering converts an unstructured object to a GatewayPoolPeering.
func parseGatewayPoolPeering(obj *unstructured.Unstructured) (*unboundednetv1alpha1.GatewayPoolPeering, error) {
	data, err := obj.MarshalJSON()
	if err != nil {
		return nil, err
	}

	var peering unboundednetv1alpha1.GatewayPoolPeering
	if err := json.Unmarshal(data, &peering); err != nil {
		return nil, err
	}

	return &peering, nil
}

// meshPeersEqual compares two meshPeerInfo slices for equality
func meshPeersEqual(a, b []meshPeerInfo) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i].Name != b[i].Name ||
			a[i].SiteName != b[i].SiteName ||
			a[i].HealthCheckProfileName != b[i].HealthCheckProfileName ||
			a[i].WireGuardPublicKey != b[i].WireGuardPublicKey ||
			!strSliceEqual(a[i].InternalIPs, b[i].InternalIPs) ||
			!strSliceEqual(a[i].PodCIDRs, b[i].PodCIDRs) ||
			a[i].SkipPodCIDRRoutes != b[i].SkipPodCIDRRoutes ||
			a[i].TunnelProtocol != b[i].TunnelProtocol ||
			a[i].TunnelMTU != b[i].TunnelMTU {
			return false
		}
	}

	return true
}

// gatewayPeersEqual compares two gatewayPeerInfo slices for equality
func gatewayPeersEqual(a, b []gatewayPeerInfo) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i].Name != b[i].Name ||
			a[i].SiteName != b[i].SiteName ||
			a[i].PoolName != b[i].PoolName ||
			a[i].PeeringTunnelProtocol != b[i].PeeringTunnelProtocol ||
			a[i].HealthCheckProfileName != b[i].HealthCheckProfileName ||
			a[i].PoolType != b[i].PoolType ||
			a[i].WireGuardPublicKey != b[i].WireGuardPublicKey ||
			a[i].GatewayWireguardPort != b[i].GatewayWireguardPort ||
			!strSliceEqual(a[i].InternalIPs, b[i].InternalIPs) ||
			!strSliceEqual(a[i].ExternalIPs, b[i].ExternalIPs) ||
			!strSliceEqual(a[i].HealthEndpoints, b[i].HealthEndpoints) ||
			!strSliceEqual(a[i].RoutedCidrs, b[i].RoutedCidrs) ||
			!intMapEqual(a[i].RouteDistances, b[i].RouteDistances) ||
			!strSliceEqual(a[i].PodCIDRs, b[i].PodCIDRs) ||
			a[i].SkipPodCIDRRoutes != b[i].SkipPodCIDRRoutes ||
			a[i].TunnelProtocol != b[i].TunnelProtocol ||
			a[i].TunnelMTU != b[i].TunnelMTU {
			return false
		}
	}

	return true
}

func intMapEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}

	for k, v := range a {
		if b[k] != v {
			return false
		}
	}

	return true
}

func copyStringIntMap(input map[string]int) map[string]int {
	if len(input) == 0 {
		return nil
	}

	output := make(map[string]int, len(input))
	for key, value := range input {
		output[key] = value
	}

	return output
}

func copyStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}

	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}

	return output
}

func stringMapEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}

	for key, value := range a {
		if b[key] != value {
			return false
		}
	}

	return true
}

func copyHealthCheckProfileMap(input map[string]healthcheck.HealthCheckSettings) map[string]healthcheck.HealthCheckSettings {
	if len(input) == 0 {
		return nil
	}

	output := make(map[string]healthcheck.HealthCheckSettings, len(input))
	for key, value := range input {
		output[key] = value
	}

	return output
}

func healthCheckProfileMapEqual(a, b map[string]healthcheck.HealthCheckSettings) bool {
	if len(a) != len(b) {
		return false
	}

	for key, value := range a {
		other, ok := b[key]
		if !ok || !healthCheckProfilesEqual(value, other) {
			return false
		}
	}

	return true
}

// strSliceEqual compares two string slices for equality
func strSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

var (
	listWireGuardLinks  = netlink.LinkList
	deleteWireGuardLink = netlink.LinkDel
)

func removeUnmanagedWireGuardInterfaces(cfg *config, state *wireGuardState, desiredGatewayIfaces map[string]bool, meshIface string) error {
	desired := make(map[string]bool, len(desiredGatewayIfaces)+1)

	desired[meshIface] = true
	for name := range desiredGatewayIfaces {
		desired[name] = true
	}

	links, err := listWireGuardLinks()
	if err != nil {
		return fmt.Errorf("list links for WireGuard ownership reconciliation: %w", err)
	}

	var cleanupErr error

	for _, link := range links {
		name := link.Attrs().Name

		// Only consider devices whose name matches our configured WG
		// prefix. Cross-prefix stale cleanup is handled by
		// removeStaleWireGuardInterfaces (which is much more careful
		// about not deleting devices owned by other tooling).
		if !isWireGuardInterface(cfg, name) {
			continue
		}

		if desired[name] {
			continue
		}

		if cfg.EnablePolicyRouting && state.gatewayPolicyManager != nil {
			if err := state.gatewayPolicyManager.RemoveInterface(name); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove policy for %s: %w", name, err))
				continue
			}
		}

		if err := deleteWireGuardLink(link); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete unmanaged WireGuard interface %s: %w", name, err))
			continue
		}

		klog.Infof("Removed unmanaged WireGuard interface %s", name)

		if state.forwardManager != nil {
			state.forwardManager.RemoveInterface(name)
		}

		if state.notrackManager != nil {
			state.notrackManager.RemoveInterface(name)
		}

		delete(state.gatewayLinkManagers, name)
		delete(state.gatewayWireguardManagers, name)
		delete(state.gatewayHealthEndpoints, name)
		delete(state.gatewayRoutes, name)
		delete(state.gatewayRouteDistances, name)
		delete(state.gatewaySiteCIDRs, name)
		delete(state.gatewayPodCIDRs, name)
		delete(state.gatewayNames, name)
		delete(state.gatewaySiteNames, name)
	}

	return cleanupErr
}

// removeStaleWireGuardInterfaces deletes kernel-resident WireGuard
// devices that look like they were created by a previous agent run with
// a different --wireguard-interface-prefix value but still own a UDP
// port we care about. A device is considered stale only when ALL of:
//
//   - link kind is "wireguard"
//   - its WireGuard ListenPort is queryable via wgctrl
//   - its current name does NOT match the name we would now compute for
//     a device on that port (cfg.WireGuardInterfacePrefix + ListenPort)
//   - its current name DOES match some other prefix followed by exactly
//     that ListenPort (i.e. the device was clearly named by an
//     unbounded-net agent under a different prefix)
//
// The last condition is what makes this safe to run on a host that may
// also be running an unrelated WireGuard daemon (Tailscale, Netbird, a
// user's own wg<n> device, etc.): we only delete devices whose name fits
// the unbounded-net agent's "<prefix><port>" pattern. Other WireGuard
// tooling typically names devices "wg0" or "tailscale0" or a UUID, which
// won't match the strict "<alpha-prefix><exact-listen-port>" pattern.
//
// If an operator's other tooling happens to use the same "<alpha><port>"
// pattern on the SAME UDP port, they cannot coexist on the same host
// regardless of this code (only one UDP listener per port).
func removeStaleWireGuardInterfaces(cfg *config) {
	wgClient, err := wgctrl.New()
	if err != nil {
		klog.Warningf("WireGuard: failed to open wgctrl for stale-prefix sweep; stale devices from a previous --wireguard-interface-prefix value will be left in place and may block port binding: %v", err)
		return
	}

	defer func() {
		if err := wgClient.Close(); err != nil {
			klog.V(4).Infof("WireGuard: failed to close wgctrl client after stale-prefix sweep: %v", err)
		}
	}()

	links, err := netlink.LinkList()
	if err != nil {
		klog.Warningf("WireGuard: failed to list links for stale-prefix sweep; stale devices from a previous --wireguard-interface-prefix value will be left in place and may block port binding: %v", err)
		return
	}

	for _, link := range links {
		if link.Type() != "wireguard" {
			continue
		}

		name := link.Attrs().Name

		dev, err := wgClient.Device(name)
		if err != nil {
			klog.Warningf("WireGuard: failed to query device %s via wgctrl; skipping for stale-prefix sweep (a stale device on this name may remain): %v", name, err)
			continue
		}

		port := dev.ListenPort
		if port <= 0 {
			continue
		}

		portSuffix := strconv.Itoa(port)
		if !strings.HasSuffix(name, portSuffix) {
			continue
		}

		// Confirm the part before the digits is alpha-only. This is the
		// "<prefix><port>" pattern that the unbounded-net agent uses; it
		// rejects names like "wg0" (port suffix would be "0" while
		// ListenPort might be 51820) and names like "myiface-13" (suffix
		// is non-port-decimal).
		oldPrefix := name[:len(name)-len(portSuffix)]
		if oldPrefix == "" || !isAlphaPrefix(oldPrefix) {
			continue
		}

		expected := wireGuardInterfaceName(cfg, port)
		if name == expected {
			continue
		}

		klog.Infof("WireGuard: removing stale device %s (matches the unbounded-net agent's <prefix><port> pattern for ListenPort=%d but does not match our configured prefix %q)", name, port, cfg.WireGuardInterfacePrefix)

		if err := netlink.LinkDel(link); err != nil {
			klog.Warningf("WireGuard: failed to delete stale device %s: %v", name, err)
		}
	}
}

// isAlphaPrefix reports whether s is non-empty and consists only of ASCII
// letters. Used by removeStaleWireGuardInterfaces to filter out names
// like "tailscale0" (which would otherwise look like prefix "tailscale"
// + port "0" -- safe to leave alone because wgctrl ListenPort would
// disagree, but this is a cheap extra defense).
func isAlphaPrefix(s string) bool {
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}

	return true
}
