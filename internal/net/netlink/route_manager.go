// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package netlink provides utilities for managing network configuration using netlink
package netlink

import (
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"
)

// DesiredRoute describes a route to be programmed by the UnifiedRouteManager.
type DesiredRoute struct {
	Prefix      net.IPNet        // destination prefix
	Nexthops    []DesiredNexthop // one or more nexthops (for ECMP)
	Metric      int              // route metric (lower = preferred)
	MTU         int              // per-route MTU (0 = no MTU set)
	Table       int              // routing table (0 = main)
	Encap       netlink.Encap    // lightweight tunnel encap (nil = none)
	Flags       int              // route flags (e.g. unix.RTNH_F_ONLINK)
	ScopeGlobal bool             // if true, use scope global instead of link for gatewayless routes
}

// DesiredNexthop describes a single nexthop for a route.
type DesiredNexthop struct {
	PeerID    string // unique peer identifier (e.g., "node-foo/wg51820")
	LinkIndex int    // interface index
	Gateway   net.IP // gateway IP (nil for link-scope)
}

// InstalledRoute describes a route currently installed in the kernel, returned
// by GetInstalledRoutes for status reporting.
type InstalledRoute struct {
	Prefix    string   // CIDR string
	Nexthops  []string // peer IDs that are active in this route
	Metric    int
	MTU       int
	Table     int
	LinkScope bool // true if the route is link-scope (no gateway)
}

// nexthopState tracks the in-memory state of a single nexthop object.
type nexthopState struct {
	peerID    string
	linkIndex int
	gateway   net.IP
	nhID      uint32
}

// installedRouteState tracks the in-memory state of an installed route.
type installedRouteState struct {
	prefix       net.IPNet
	metric       int
	mtu          int
	table        int
	linkScope    bool
	hasEncap     bool                      // true when route has lightweight tunnel encap
	peerNexthops map[string]DesiredNexthop // peerID -> nexthop info
}

// UnifiedRouteManager manages all routes -- both simple link-scope and ECMP
// multipath -- using a single code path. It replaces both RouteManager and
// ECMPRouteManager with unified support for:
//   - Link-scope routes (no gateway, scope=link) for bootstrap routes
//   - ECMP multipath routes with gateways
//   - Per-route MTU
//   - Preferred source IPs
//   - Route table selection (main table, custom tables)
//   - Fast peer removal/restoration for health-check integration
//   - Differential sync (add missing, update changed, remove stale)
type UnifiedRouteManager struct {
	linkName  string
	linkIndex int

	// wgPrefix is the configured WireGuard interface prefix (e.g. "wg").
	// Used by the orphan-route cleanup scan to identify routes pointing at
	// per-port WireGuard devices vs other kernel routes. Must be non-empty;
	// the caller is required to supply it.
	wgPrefix string

	// dummyDeviceName is the name of the eBPF dummy device used by the
	// agent (currently always "unbounded0"). Routes via this device are
	// preserved by the orphan-route cleanup scan. Must be non-empty; the
	// caller is required to supply it.
	dummyDeviceName string

	// defaultTable is the routing table ID used for routes that do not
	// specify an explicit Table (i.e. Table==0 in DesiredRoute). When set
	// to a dedicated table (not 0 and not RT_TABLE_MAIN), cleanup and
	// validation can be simplified because every route in the table is ours.
	defaultTable int

	mu sync.Mutex

	// Nexthop tracking
	nexthops   map[string]*nexthopState // peerID -> nexthop state
	nexthopIDs map[uint32]string        // nexthop ID -> peerID (reverse lookup)

	// Route tracking
	installedRoutes map[string]*installedRouteState // routeKey -> route info

	// Preferred source IPs for routes (one per IP family)
	preferredSrcIPv4 net.IP
	preferredSrcIPv6 net.IP

	// netlinkCache provides cached route/link reads when available.
	netlinkCache *NetlinkCache
}

// NewUnifiedRouteManager creates a new route manager.
//
// linkName is the primary interface used for metrics labeling; actual
// route interfaces come from each DesiredNexthop.LinkIndex.
//
// defaultTable is the routing table ID used for routes whose Table field
// is 0; pass 0 to use the main table (254) for backward compatibility.
//
// wgPrefix is the per-port WireGuard interface name prefix (typically
// the operator-configurable cfg.WireGuardInterfacePrefix, e.g. "wg" by
// default). Used by the orphan-route cleanup scan to match kernel routes
// pointing at managed WireGuard devices.
//
// dummyDeviceName is the name of the agent's eBPF dummy device
// (typically the unbounded0DeviceName constant). Routes via this device
// are preserved by the orphan-route cleanup scan.
//
// Both wgPrefix and dummyDeviceName must be non-empty; the caller is
// responsible for supplying the values from the merged runtime config.
func NewUnifiedRouteManager(linkName string, defaultTable int, wgPrefix, dummyDeviceName string) *UnifiedRouteManager {
	effectiveTable := defaultTable
	if effectiveTable == 0 {
		effectiveTable = unix.RT_TABLE_MAIN
	}

	m := &UnifiedRouteManager{
		linkName:        linkName,
		wgPrefix:        wgPrefix,
		dummyDeviceName: dummyDeviceName,
		defaultTable:    effectiveTable,
		nexthops:        make(map[string]*nexthopState),
		nexthopIDs:      make(map[uint32]string),
		installedRoutes: make(map[string]*installedRouteState),
	}

	// Try to resolve link index; the interface may not exist yet.
	if link, err := netlink.LinkByName(linkName); err == nil {
		m.linkIndex = link.Attrs().Index
	}

	return m
}

// SetNetlinkCache sets the cache used for read-path operations (route listing,
// link lookups). Pass nil to revert to direct netlink syscalls.
func (m *UnifiedRouteManager) SetNetlinkCache(cache *NetlinkCache) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.netlinkCache = cache
}

// SetPreferredSourceIPs sets the preferred source IPs for routes.
// IPv4 routes will use ipv4 and IPv6 routes will use ipv6 as the preferred
// source for locally-originated packets. Pass nil to disable.
func (m *UnifiedRouteManager) SetPreferredSourceIPs(ipv4, ipv6 net.IP) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.preferredSrcIPv4 = ipv4
	m.preferredSrcIPv6 = ipv6

	if ipv4 != nil {
		klog.Infof("Set preferred IPv4 source for unified routes: %s", ipv4.String())
	}

	if ipv6 != nil {
		klog.Infof("Set preferred IPv6 source for unified routes: %s", ipv6.String())
	}
}

// SyncRoutes performs a differential sync of all routes. It ensures nexthop
// objects are tracked for all referenced peers, then adds/updates/removes
// kernel routes as needed.
func (m *UnifiedRouteManager) SyncRoutes(desired []DesiredRoute) error {
	start := time.Now()

	m.mu.Lock()
	defer m.mu.Unlock()

	var (
		syncErr        error
		added, removed int
	)

	defer func() {
		RouteSyncDuration.WithLabelValues("unified").Observe(time.Since(start).Seconds())

		if syncErr != nil {
			RouteSyncErrors.WithLabelValues("unified").Inc()
		}

		RoutesAdded.WithLabelValues("unified").Add(float64(added))
		RoutesRemoved.WithLabelValues("unified").Add(float64(removed))
		RoutesInstalled.WithLabelValues(m.linkName).Set(float64(len(m.installedRoutes)))
	}()

	// Build desired route set keyed by table:prefix.
	// Multiple DesiredRoute entries with the same prefix are merged into a single
	// route with combined nexthops (ECMP). For metric and MTU, the minimum
	// value across contributors is used.
	desiredSet := make(map[string]DesiredRoute, len(desired))
	for _, dr := range desired {
		normalized := normalizePrefix(dr.Prefix)

		for _, nh := range dr.Nexthops {
			m.ensureNexthop(nh)
		}

		key := m.routeKey(dr.Table, normalized)
		if existing, ok := desiredSet[key]; ok {
			// Merge nexthops from duplicate prefix entries (ECMP).
			// Only nexthops at the best (lowest) metric are kept. If a new
			// entry has a lower metric, it replaces all previous nexthops.
			if dr.Metric > 0 && existing.Metric > 0 && dr.Metric < existing.Metric {
				// New route has better metric -- replace nexthops entirely
				existing.Nexthops = dr.Nexthops
				existing.Metric = dr.Metric
			} else if dr.Metric == existing.Metric || existing.Metric == 0 || dr.Metric == 0 {
				// Same metric (or one is unset) -- merge nexthops
				existing.Nexthops = append(existing.Nexthops, dr.Nexthops...)
				if dr.Metric > 0 && existing.Metric == 0 {
					existing.Metric = dr.Metric
				}
			}
			// else: new route has worse metric, skip its nexthops
			if dr.MTU > 0 && (existing.MTU == 0 || dr.MTU < existing.MTU) {
				existing.MTU = dr.MTU
			}
			// Preserve encap if the new route has one and the existing does not.
			if dr.Encap != nil && existing.Encap == nil {
				existing.Encap = dr.Encap
			}

			desiredSet[key] = existing
		} else {
			desiredSet[key] = DesiredRoute{
				Prefix:   normalized,
				Nexthops: dr.Nexthops,
				Metric:   dr.Metric,
				MTU:      dr.MTU,
				Table:    dr.Table,
				Encap:    dr.Encap,
			}
		}
	}

	// Add or update routes.
	for key, dr := range desiredSet {
		if len(dr.Nexthops) == 0 {
			continue
		}

		route := m.buildKernelRoute(dr, dr.Nexthops)
		if route == nil {
			klog.V(2).Infof("Could not build kernel route for %s, skipping", dr.Prefix.String())
			continue
		}

		existing, installed := m.installedRoutes[key]
		if installed && !m.routeNeedsUpdate(existing, dr, dr.Nexthops) {
			continue
		}

		// If the metric changed, delete the old route first. In the kernel,
		// routes with different metrics are separate entries, so RouteReplace
		// at a new metric creates a second route instead of updating.
		if installed && existing.metric != dr.Metric {
			if err := m.deleteKernelRoute(existing); err != nil {
				klog.V(2).Infof("Failed to remove old-metric route for %s (metric %d): %v", dr.Prefix.String(), existing.metric, err)
			}
		}

		if err := netlink.RouteReplace(route); err != nil {
			klog.Errorf("Failed to install route for %s: %v", dr.Prefix.String(), err)
			syncErr = err

			continue
		}

		m.installedRoutes[key] = m.buildInstalledState(dr, dr.Nexthops)

		if !installed {
			added++

			klog.Infof("Added route for %s via %d nexthop(s)", dr.Prefix.String(), len(dr.Nexthops))
		} else {
			klog.V(2).Infof("Updated route for %s via %d nexthop(s)", dr.Prefix.String(), len(dr.Nexthops))
		}
	}

	// Remove stale routes.
	for key, state := range m.installedRoutes {
		if _, wanted := desiredSet[key]; !wanted {
			if err := m.deleteKernelRoute(state); err != nil {
				klog.Errorf("Failed to remove route for %s: %v", state.prefix.String(), err)
				syncErr = err

				continue
			}

			delete(m.installedRoutes, key)

			removed++

			klog.Infof("Removed route for %s", state.prefix.String())
		}
	}

	// Cleanup orphaned kernel routes: scan for proto-static routes on wg*
	// interfaces that we did not just install. This handles stale routes from
	// previous runs (e.g., different metric) that survive pod restarts.
	m.cleanupOrphanedKernelRoutes(desiredSet)

	return syncErr
}

// RemoveAllRoutes removes all managed routes and clears tracking state.
func (m *UnifiedRouteManager) RemoveAllRoutes() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var lastErr error

	for key, state := range m.installedRoutes {
		if err := m.deleteKernelRoute(state); err != nil {
			klog.Errorf("Failed to remove route %s: %v", state.prefix.String(), err)
			lastErr = err
		} else {
			delete(m.installedRoutes, key)
			klog.Infof("Removed route for %s", state.prefix.String())
		}
	}

	// Clear all tracking state.
	m.nexthops = make(map[string]*nexthopState)
	m.nexthopIDs = make(map[uint32]string)
	m.installedRoutes = make(map[string]*installedRouteState)

	return lastErr
}

// GetInstalledRoutes returns the current set of installed routes for status
// reporting.
func (m *UnifiedRouteManager) GetInstalledRoutes() []InstalledRoute {
	m.mu.Lock()
	defer m.mu.Unlock()

	routes := make([]InstalledRoute, 0, len(m.installedRoutes))
	for _, state := range m.installedRoutes {
		peers := make([]string, 0, len(state.peerNexthops))
		for peerID := range state.peerNexthops {
			peers = append(peers, peerID)
		}

		sort.Strings(peers)

		routes = append(routes, InstalledRoute{
			Prefix:    state.prefix.String(),
			Nexthops:  peers,
			Metric:    state.metric,
			MTU:       state.mtu,
			Table:     state.table,
			LinkScope: state.linkScope,
		})
	}

	return routes
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// routeKey generates a unique key for a route based on its table and prefix.
// When the route's table is 0, the manager's defaultTable is used.
func (m *UnifiedRouteManager) routeKey(table int, prefix net.IPNet) string {
	t := table
	if t == 0 {
		t = m.defaultTable
	}

	return fmt.Sprintf("%d:%s", t, prefix.String())
}

// effectiveTable returns the routing table to use for a DesiredRoute. Routes
// with an explicitly set Table (non-zero) keep their table; routes with
// Table==0 use the manager's defaultTable.
func (m *UnifiedRouteManager) effectiveTable(table int) int {
	if table != 0 {
		return table
	}

	return m.defaultTable
}

// isDedicatedTable returns true when the manager is configured to use a
// dedicated routing table (not the main table). When true, every route in
// the table is managed by us and cleanup/validation can skip interface checks.
func (m *UnifiedRouteManager) isDedicatedTable() bool {
	return m.defaultTable != 0 && m.defaultTable != unix.RT_TABLE_MAIN
}

// normalizePrefix returns a copy of the prefix with the IP masked to the
// network address.
func normalizePrefix(p net.IPNet) net.IPNet {
	return net.IPNet{
		IP:   p.IP.Mask(p.Mask),
		Mask: p.Mask,
	}
}

// peerNexthopID computes a nexthop ID from a peer ID using FNV-1a, handling
// collisions by incrementing.
func (m *UnifiedRouteManager) peerNexthopID(peerID string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(peerID))

	id := h.Sum32()
	if id == 0 {
		id = 1 // 0 is not a valid nexthop ID
	}

	for {
		existing, taken := m.nexthopIDs[id]
		if !taken || existing == peerID {
			return id
		}

		id++
		if id == 0 {
			id = 1
		}
	}
}

// ensureNexthop ensures a nexthop entry is tracked for the given peer.
func (m *UnifiedRouteManager) ensureNexthop(nh DesiredNexthop) uint32 {
	if existing, ok := m.nexthops[nh.PeerID]; ok {
		existing.linkIndex = nh.LinkIndex
		existing.gateway = nh.Gateway

		return existing.nhID
	}

	id := m.peerNexthopID(nh.PeerID)
	m.nexthops[nh.PeerID] = &nexthopState{
		peerID:    nh.PeerID,
		linkIndex: nh.LinkIndex,
		gateway:   nh.Gateway,
		nhID:      id,
	}
	m.nexthopIDs[id] = nh.PeerID

	return id
}

// preferredSrc returns the configured preferred source IP for the given prefix
// family, or nil if unset.
func (m *UnifiedRouteManager) preferredSrc(prefix net.IPNet) net.IP {
	if prefix.IP.To4() != nil {
		return m.preferredSrcIPv4
	}

	return m.preferredSrcIPv6
}

// isLinkScopeRoute returns true when a route has exactly one nexthop with no
// gateway, meaning it should be programmed as a direct/link-scope route.
func isLinkScopeRoute(nexthops []DesiredNexthop) bool {
	return len(nexthops) == 1 && nexthops[0].Gateway == nil
}

// buildKernelRoute translates a desired route plus its currently-active
// nexthops into a netlink.Route suitable for RouteReplace. Returns nil if the
// route cannot be built (e.g. no valid IPv6 nexthops).
func (m *UnifiedRouteManager) buildKernelRoute(desired DesiredRoute, activeNexthops []DesiredNexthop) *netlink.Route {
	table := m.effectiveTable(desired.Table)

	prefix := desired.Prefix
	isIPv6 := prefix.IP.To4() == nil

	route := &netlink.Route{
		Dst:      &prefix,
		Table:    table,
		Protocol: unix.RTPROT_STATIC,
		Type:     unix.RTN_UNICAST,
	}

	if desired.MTU > 0 {
		route.MTU = desired.MTU
	}

	if desired.Metric > 0 {
		route.Priority = desired.Metric
	}

	if src := m.preferredSrc(prefix); src != nil {
		route.Src = src
	}

	if desired.Encap != nil {
		route.Encap = desired.Encap
	}

	if desired.Flags != 0 {
		route.Flags = desired.Flags
	}

	if isLinkScopeRoute(activeNexthops) {
		// Single nexthop, no gateway.
		route.LinkIndex = activeNexthops[0].LinkIndex
		if desired.ScopeGlobal {
			route.Scope = netlink.SCOPE_UNIVERSE
		} else {
			route.Scope = netlink.SCOPE_LINK
		}
	} else if len(activeNexthops) == 1 && activeNexthops[0].Gateway != nil {
		// Single nexthop with gateway -- direct route (not multipath).
		route.LinkIndex = activeNexthops[0].LinkIndex
		route.Gw = activeNexthops[0].Gateway
		route.Scope = netlink.SCOPE_UNIVERSE
	} else {
		// Multipath route with (optional) gateways.
		var nhInfos []*netlink.NexthopInfo

		for _, nh := range activeNexthops {
			nhi := &netlink.NexthopInfo{
				LinkIndex: nh.LinkIndex,
			}
			if isIPv6 {
				if nh.Gateway != nil {
					nhi.Gw = nh.Gateway
				} else {
					klog.V(3).Infof("Skipping nexthop %s for IPv6 route %s: no gateway", nh.PeerID, prefix.String())
					continue
				}
			} else if nh.Gateway != nil {
				nhi.Gw = nh.Gateway
			}

			nhInfos = append(nhInfos, nhi)
		}

		if len(nhInfos) == 0 {
			return nil
		}

		route.MultiPath = nhInfos
		if !isIPv6 {
			route.Scope = netlink.SCOPE_UNIVERSE
		}
	}

	return route
}

// buildInstalledState creates an installedRouteState snapshot from a desired
// route and its active nexthops.
func (m *UnifiedRouteManager) buildInstalledState(dr DesiredRoute, active []DesiredNexthop) *installedRouteState {
	peerNH := make(map[string]DesiredNexthop, len(active))
	for _, nh := range active {
		peerNH[nh.PeerID] = nh
	}

	return &installedRouteState{
		prefix:       dr.Prefix,
		metric:       dr.Metric,
		mtu:          dr.MTU,
		table:        dr.Table,
		linkScope:    isLinkScopeRoute(active),
		hasEncap:     dr.Encap != nil,
		peerNexthops: peerNH,
	}
}

// routeNeedsUpdate returns true when the installed state differs from the
// desired state plus the set of currently-active nexthops.
func (m *UnifiedRouteManager) routeNeedsUpdate(installed *installedRouteState, desired DesiredRoute, activeNH []DesiredNexthop) bool {
	if installed.metric != desired.Metric || installed.mtu != desired.MTU {
		return true
	}

	// Detect encap changes (e.g. route switching from plain to VXLAN encap).
	if installed.hasEncap != (desired.Encap != nil) {
		return true
	}

	if len(installed.peerNexthops) != len(activeNH) {
		return true
	}

	for _, nh := range activeNH {
		existing, ok := installed.peerNexthops[nh.PeerID]
		if !ok {
			return true
		}

		if existing.LinkIndex != nh.LinkIndex {
			return true
		}

		if !ipEqual(existing.Gateway, nh.Gateway) {
			return true
		}
	}

	return false
}

// ipEqual compares two IPs, treating nil and nil as equal.
func ipEqual(a, b net.IP) bool {
	if a == nil && b == nil {
		return true
	}

	if a == nil || b == nil {
		return false
	}

	return a.Equal(b)
}

// deleteKernelRoute removes a single route from the kernel.
func (m *UnifiedRouteManager) deleteKernelRoute(state *installedRouteState) error {
	table := m.effectiveTable(state.table)

	prefix := state.prefix
	route := &netlink.Route{
		Dst:   &prefix,
		Table: table,
	}

	if state.metric > 0 {
		route.Priority = state.metric
	}

	if err := netlink.RouteDel(route); err != nil {
		// Ignore ESRCH ("no such process") -- route already gone.
		if !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("failed to delete route for %s: %w", prefix.String(), err)
		}
	}

	return nil
}

// cleanupOrphanedKernelRoutes removes proto-static routes from the kernel that
// do not match the desired route set. This handles stale routes from previous
// runs (e.g., a route programmed at metric 2 that should now be metric 3).
//
// When using a dedicated table, every static route in the table is considered
// ours and no interface-name filtering is needed. When using the main table,
// only routes on wg* interfaces are considered. Must be called with m.mu held.
func (m *UnifiedRouteManager) cleanupOrphanedKernelRoutes(desiredSet map[string]DesiredRoute) {
	// Build a set of desired prefix+metric for quick lookup.
	type desiredKey struct {
		prefix string
		metric int
	}

	desired := make(map[desiredKey]bool, len(desiredSet))
	for _, dr := range desiredSet {
		desired[desiredKey{prefix: dr.Prefix.String(), metric: dr.Metric}] = true
	}

	if m.isDedicatedTable() {
		// Dedicated table: list only routes in our table.
		routes, err := ListRoutesInTable(m.defaultTable)
		if err != nil {
			klog.V(4).Infof("Failed to list routes in table %d for orphan cleanup: %v", m.defaultTable, err)
			return
		}

		for _, r := range routes {
			if r.Dst == nil || r.Protocol != unix.RTPROT_STATIC {
				continue
			}

			dk := desiredKey{prefix: r.Dst.String(), metric: r.Priority}
			if !desired[dk] {
				rCopy := r
				if err := netlink.RouteDel(&rCopy); err != nil && !errors.Is(err, syscall.ESRCH) {
					klog.V(2).Infof("Failed to remove orphaned kernel route %s metric %d from table %d: %v", r.Dst.String(), r.Priority, m.defaultTable, err)
				} else {
					klog.Infof("Removed orphaned kernel route %s metric %d from table %d", r.Dst.String(), r.Priority, m.defaultTable)
				}
			}
		}

		return
	}

	// Main table: scan for proto-static routes on wg* interfaces.
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		var (
			routes []netlink.Route
			err    error
		)

		if m.netlinkCache != nil {
			routes, err = m.netlinkCache.RouteList(nil, family)
		} else {
			routes, err = netlink.RouteList(nil, family)
		}

		if err != nil {
			klog.V(4).Infof("Failed to list kernel routes for orphan cleanup (family %d): %v", family, err)
			continue
		}

		for _, r := range routes {
			if r.Dst == nil || r.Protocol != unix.RTPROT_STATIC {
				continue
			}
			// Check single-path routes
			if r.LinkIndex != 0 {
				var (
					link    netlink.Link
					linkErr error
				)

				if m.netlinkCache != nil {
					link, linkErr = m.netlinkCache.LinkByIndex(r.LinkIndex)
				} else {
					link, linkErr = netlink.LinkByIndex(r.LinkIndex)
				}

				if linkErr != nil || link == nil || link.Attrs() == nil {
					continue
				}

				if !strings.HasPrefix(link.Attrs().Name, m.wgPrefix) && link.Attrs().Name != m.dummyDeviceName {
					continue
				}
			} else if len(r.MultiPath) > 0 {
				hasWG := false

				for _, mp := range r.MultiPath {
					var (
						link    netlink.Link
						linkErr error
					)

					if m.netlinkCache != nil {
						link, linkErr = m.netlinkCache.LinkByIndex(mp.LinkIndex)
					} else {
						link, linkErr = netlink.LinkByIndex(mp.LinkIndex)
					}

					if linkErr != nil || link == nil || link.Attrs() == nil {
						continue
					}

					if strings.HasPrefix(link.Attrs().Name, m.wgPrefix) || link.Attrs().Name == m.dummyDeviceName {
						hasWG = true
						break
					}
				}

				if !hasWG {
					continue
				}
			} else {
				continue
			}

			dk := desiredKey{prefix: r.Dst.String(), metric: r.Priority}
			if !desired[dk] {
				if err := netlink.RouteDel(&r); err != nil && !errors.Is(err, syscall.ESRCH) {
					klog.V(2).Infof("Failed to remove orphaned kernel route %s metric %d: %v", r.Dst.String(), r.Priority, err)
				} else {
					klog.Infof("Removed orphaned kernel route %s metric %d", r.Dst.String(), r.Priority)
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Utility functions
// ---------------------------------------------------------------------------
