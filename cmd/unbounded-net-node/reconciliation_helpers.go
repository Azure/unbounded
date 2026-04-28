// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"

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

func effectiveSiteHealthCheckSettings(mySiteName string, siteMap map[string]*unboundednetv1alpha1.Site) (bool, healthcheck.HealthCheckSettings) {
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
	return a.DetectMultiplier == b.DetectMultiplier &&
		a.ReceiveInterval == b.ReceiveInterval &&
		a.TransmitInterval == b.TransmitInterval
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
	assignmentHealthCheckProfileName := ""

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

	if assignmentHealthCheckProfileName == "" {
		return
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

// resolveTunnelMTU returns the effective tunnel MTU for a given scope,
// taking the minimum of the global MTU and any CRD-specified MTU values.
// Zero values in mtuValues are ignored (meaning "no override").
func resolveTunnelMTU(globalMTU int, mtuValues ...int) int {
	result := globalMTU
	for _, v := range mtuValues {
		if v > 0 && v < result {
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
) int {
	mtu := globalMTU
	// Site MTU (from my site)
	if v := siteTunnelMTUs[mySiteName]; v > 0 && v < mtu {
		mtu = v
	}
	// If peer is in a different site, also consider the peering and assignment MTU
	if peer.SiteName != mySiteName {
		if v := peeringSiteTunnelMTUs[peer.SiteName]; v > 0 && v < mtu {
			mtu = v
		}

		if v := assignmentSiteTunnelMTUs[peer.SiteName]; v > 0 && v < mtu {
			mtu = v
		}
	}

	return mtu
}

// resolveGatewayPeerTunnelMTU resolves the effective tunnel MTU for a gateway peer.
// The resolution considers: global MTU, local site MTU, assignment pool MTU,
// and gateway pool MTU.
func resolveGatewayPeerTunnelMTU(globalMTU int, mySiteName string, peer gatewayPeerInfo,
	siteTunnelMTUs, assignmentPoolTunnelMTUs map[string]int,
	poolTunnelMTUs map[string]int,
) int {
	mtu := globalMTU
	if v := siteTunnelMTUs[mySiteName]; v > 0 && v < mtu {
		mtu = v
	}

	if v := assignmentPoolTunnelMTUs[peer.PoolName]; v > 0 && v < mtu {
		mtu = v
	}

	if v := poolTunnelMTUs[peer.PoolName]; v > 0 && v < mtu {
		mtu = v
	}

	return mtu
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
			a[i].SkipPodCIDRRoutes != b[i].SkipPodCIDRRoutes {
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
			a[i].SkipPodCIDRRoutes != b[i].SkipPodCIDRRoutes {
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

func removeUnmanagedWireGuardInterfaces(cfg *config, state *wireGuardState, desiredGatewayIfaces map[string]bool, meshIface string) {
	desired := make(map[string]bool, len(desiredGatewayIfaces)+1)

	desired[meshIface] = true
	for name := range desiredGatewayIfaces {
		desired[name] = true
	}

	links, err := netlink.LinkList()
	if err != nil {
		klog.Warningf("Failed to list links for WireGuard ownership reconciliation: %v", err)
		return
	}

	for _, link := range links {
		name := link.Attrs().Name
		if !strings.HasPrefix(name, "wg") {
			continue
		}

		if desired[name] {
			continue
		}

		if cfg.EnablePolicyRouting && state.gatewayPolicyManager != nil {
			if err := state.gatewayPolicyManager.RemoveInterface(name); err != nil {
				klog.V(3).Infof("Policy cleanup skipped for unmanaged interface %s: %v", name, err)
			}
		}

		if err := netlink.LinkDel(link); err != nil {
			klog.Warningf("Failed to delete unmanaged WireGuard interface %s: %v", name, err)
			continue
		}

		klog.Infof("Removed unmanaged WireGuard interface %s", name)

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
}
