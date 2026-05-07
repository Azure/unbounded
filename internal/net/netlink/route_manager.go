// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package netlink provides utilities for managing network configuration using netlink
package netlink

import (
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"slices"
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
	Prefix            net.IPNet        // destination prefix
	Nexthops          []DesiredNexthop // one or more nexthops (for ECMP)
	Metric            int              // route metric (lower = preferred)
	MTU               int              // per-route MTU (0 = no MTU set)
	Table             int              // routing table (0 = main)
	HealthCheckImmune bool             // if true, route is never withdrawn by healthcheck
	Encap             netlink.Encap    // lightweight tunnel encap (nil = none)
	Flags             int              // route flags (e.g. unix.RTNH_F_ONLINK)
	ScopeGlobal       bool             // if true, use scope global instead of link for gatewayless routes
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

	// Desired routes (kept so RestoreNexthopForPeer can rebuild routes)
	desiredRoutes []DesiredRoute

	// Peer health state (for fast removal/restoration)
	peerHealthy map[string]bool // peerID -> healthy

	// Preferred source IPs for routes (one per IP family)
	preferredSrcIPv4 net.IP
	preferredSrcIPv6 net.IP

	// netlinkCache provides cached route/link reads when available.
	netlinkCache *NetlinkCache
}

// NewUnifiedRouteManager creates a new route manager. linkName is the primary
// interface used for metrics labelling; actual route interfaces come from each
// DesiredNexthop.LinkIndex. defaultTable is the routing table ID used for
// routes whose Table field is 0; pass 0 to use the main table (254) for
// backward compatibility.
func NewUnifiedRouteManager(linkName string, defaultTable int) *UnifiedRouteManager {
	effectiveTable := defaultTable
	if effectiveTable == 0 {
		effectiveTable = unix.RT_TABLE_MAIN
	}

	m := &UnifiedRouteManager{
		linkName:        linkName,
		defaultTable:    effectiveTable,
		nexthops:        make(map[string]*nexthopState),
		nexthopIDs:      make(map[uint32]string),
		installedRoutes: make(map[string]*installedRouteState),
		peerHealthy:     make(map[string]bool),
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

		// Ensure all nexthops are tracked and default-healthy.
		for _, nh := range dr.Nexthops {
			m.ensureNexthop(nh)

			if _, tracked := m.peerHealthy[nh.PeerID]; !tracked {
				m.peerHealthy[nh.PeerID] = true
			}
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
			// If any contributor is immune, the merged route is immune
			if dr.HealthCheckImmune {
				existing.HealthCheckImmune = true
			}
			// Preserve encap if the new route has one and the existing does not.
			if dr.Encap != nil && existing.Encap == nil {
				existing.Encap = dr.Encap
			}

			desiredSet[key] = existing
		} else {
			desiredSet[key] = DesiredRoute{
				Prefix:            normalized,
				Nexthops:          dr.Nexthops,
				Metric:            dr.Metric,
				MTU:               dr.MTU,
				Table:             dr.Table,
				HealthCheckImmune: dr.HealthCheckImmune,
				Encap:             dr.Encap,
			}
		}
	}

	// Store the merged desired routes for use by RestoreNexthopForPeer.
	m.desiredRoutes = make([]DesiredRoute, 0, len(desiredSet))
	for _, dr := range desiredSet {
		m.desiredRoutes = append(m.desiredRoutes, dr)
	}

	// Add or update routes.
	for key, dr := range desiredSet {
		active := m.activeNexthops(dr)
		if len(active) == 0 {
			klog.V(2).Infof("No healthy nexthops for route %s, skipping", dr.Prefix.String())
			continue
		}

		route := m.buildKernelRoute(dr, active)
		if route == nil {
			klog.V(2).Infof("Could not build kernel route for %s, skipping", dr.Prefix.String())
			continue
		}

		existing, installed := m.installedRoutes[key]
		if installed && !m.routeNeedsUpdate(existing, dr, active) {
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

		m.installedRoutes[key] = m.buildInstalledState(dr, active)

		if !installed {
			added++

			klog.Infof("Added route for %s via %d nexthop(s)", dr.Prefix.String(), len(active))
		} else {
			klog.V(2).Infof("Updated route for %s via %d nexthop(s)", dr.Prefix.String(), len(active))
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

// RemoveNexthopForPeer removes a peer's nexthop from all groups and routes.
// This is called when a health-check session goes down.
func (m *UnifiedRouteManager) RemoveNexthopForPeer(peerID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	matched := m.setPeerHealthByPrefix(peerID, false)
	if matched == 0 {
		klog.V(4).Infof("No nexthops found for peer %s to mark unhealthy (may be eBPF-only)", peerID)
		return nil
	}

	klog.Infof("Marking peer %s as unhealthy (%d nexthop(s)), updating routes", peerID, matched)

	return m.rebuildRoutesForPeerPrefix(peerID)
}

// RestoreNexthopForPeer re-adds a peer's nexthop to all applicable routes.
// This is called when a health-check session comes back up.
func (m *UnifiedRouteManager) RestoreNexthopForPeer(peerID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	matched := m.setPeerHealthByPrefix(peerID, true)
	if matched == 0 {
		klog.V(4).Infof("No nexthops found for peer %s to mark healthy (may be eBPF-only)", peerID)
		return nil
	}

	klog.Infof("Marking peer %s as healthy (%d nexthop(s)), updating routes", peerID, matched)

	return m.rebuildRoutesForPeerPrefix(peerID)
}

// setPeerHealthByPrefix sets the health state for all peer IDs that match
// the given prefix. The healthcheck uses bare hostnames (e.g., "node-a") while
// nexthop peer IDs include the interface (e.g., "node-a/wg51820"). This method
// matches both exact IDs and hostname-prefixed IDs (hostname + "/").
func (m *UnifiedRouteManager) setPeerHealthByPrefix(prefix string, healthy bool) int {
	matched := 0

	for id := range m.peerHealthy {
		if id == prefix || strings.HasPrefix(id, prefix+"/") {
			m.peerHealthy[id] = healthy
			matched++
		}
	}

	return matched
}

// rebuildRoutesForPeerPrefix rebuilds routes for any peer ID matching the
// given prefix (hostname or exact ID).
func (m *UnifiedRouteManager) rebuildRoutesForPeerPrefix(prefix string) error {
	var lastErr error

	for _, dr := range m.desiredRoutes {
		if !routeReferencesPeerPrefix(dr, prefix) {
			continue
		}

		normalized := normalizePrefix(dr.Prefix)
		key := m.routeKey(dr.Table, normalized)
		active := m.activeNexthops(dr)

		if len(active) == 0 {
			if state, installed := m.installedRoutes[key]; installed {
				if err := m.deleteKernelRoute(state); err != nil {
					klog.Errorf("Failed to remove route %s after peer %s went down: %v", normalized.String(), prefix, err)
					lastErr = err

					continue
				}

				delete(m.installedRoutes, key)
				klog.Infof("Removed route %s (last nexthop peer %s went down)", normalized.String(), prefix)
			}

			continue
		}

		normalizedDR := DesiredRoute{
			Prefix:   normalized,
			Nexthops: dr.Nexthops,
			Metric:   dr.Metric,
			MTU:      dr.MTU,
			Table:    dr.Table,
			Encap:    dr.Encap,
		}

		route := m.buildKernelRoute(normalizedDR, active)
		if route == nil {
			continue
		}

		if err := netlink.RouteReplace(route); err != nil {
			klog.Errorf("Failed to update route %s for peer %s change: %v", normalized.String(), prefix, err)
			lastErr = err

			continue
		}

		m.installedRoutes[key] = m.buildInstalledState(normalizedDR, active)
		klog.V(2).Infof("Updated route %s (now %d nexthop(s) after peer %s change)", normalized.String(), len(active), prefix)
	}

	return lastErr
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
	m.desiredRoutes = nil

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

// ValidateRoutes checks the kernel routing table against the desired state and
// corrects any drift. Routes that are missing are re-added, routes that should
// not exist are removed, and routes whose nexthops have diverged from the
// expected healthy set are replaced. Returns the number of corrections made.
//
// When the manager uses a dedicated table (not the main table), validation
// scopes its kernel route listing to that table and treats every RTPROT_STATIC
// route in it as managed. When using the main table, only routes matching a
// desired prefix are considered.
func (m *UnifiedRouteManager) ValidateRoutes() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.desiredRoutes) == 0 {
		return 0
	}

	corrections := 0

	// Build the expected route set from desiredRoutes filtered by health.
	type expectedEntry struct {
		desired     DesiredRoute
		activeNH    []DesiredNexthop
		kernelRoute *netlink.Route
	}

	expected := make(map[string]*expectedEntry)

	for _, dr := range m.desiredRoutes {
		active := m.activeNexthops(dr)
		if len(active) == 0 {
			continue
		}

		kr := m.buildKernelRoute(dr, active)
		if kr == nil {
			continue
		}

		table := m.effectiveTable(dr.Table)
		key := fmt.Sprintf("%d:%s:%d", table, dr.Prefix.String(), dr.Metric)
		expected[key] = &expectedEntry{
			desired:     dr,
			activeNH:    active,
			kernelRoute: kr,
		}
	}

	// Read current kernel routes. When using a dedicated table, list only
	// routes from that table (every static route is ours). When using the
	// main table, list all routes and filter to desired prefixes.
	kernelRoutes := make(map[string]netlink.Route)

	if m.isDedicatedTable() {
		routes, err := ListRoutesInTable(m.defaultTable)
		if err != nil {
			klog.V(4).Infof("ValidateRoutes: failed to list routes in table %d: %v", m.defaultTable, err)
		} else {
			for _, r := range routes {
				if r.Dst == nil || r.Protocol != unix.RTPROT_STATIC {
					continue
				}

				table := r.Table
				if table == 0 {
					table = m.defaultTable
				}

				key := fmt.Sprintf("%d:%s:%d", table, r.Dst.String(), r.Priority)
				kernelRoutes[key] = r
			}
		}
	} else {
		desiredPrefixes := make(map[string]bool, len(m.desiredRoutes))
		for _, dr := range m.desiredRoutes {
			table := m.effectiveTable(dr.Table)
			desiredPrefixes[fmt.Sprintf("%d:%s", table, dr.Prefix.String())] = true
		}

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
				klog.V(4).Infof("ValidateRoutes: failed to list routes (family %d): %v", family, err)
				continue
			}

			for _, r := range routes {
				if r.Dst == nil || r.Protocol != unix.RTPROT_STATIC {
					continue
				}

				table := r.Table
				if table == 0 {
					table = unix.RT_TABLE_MAIN
				}

				pfxKey := fmt.Sprintf("%d:%s", table, r.Dst.String())
				if !desiredPrefixes[pfxKey] {
					continue
				}

				key := fmt.Sprintf("%d:%s:%d", table, r.Dst.String(), r.Priority)
				kernelRoutes[key] = r
			}
		}
	}

	// Add missing routes and correct routes with wrong nexthops.
	for key, exp := range expected {
		kr, exists := kernelRoutes[key]
		if !exists {
			if err := netlink.RouteReplace(exp.kernelRoute); err != nil {
				klog.Warningf("ValidateRoutes: failed to add missing route %s metric %d: %v",
					exp.desired.Prefix.String(), exp.desired.Metric, err)
			} else {
				klog.Warningf("ValidateRoutes: added missing route %s metric %d",
					exp.desired.Prefix.String(), exp.desired.Metric)

				corrections++
				rkey := m.routeKey(exp.desired.Table, exp.desired.Prefix)
				m.installedRoutes[rkey] = m.buildInstalledState(exp.desired, exp.activeNH)
			}

			continue
		}

		if !kernelRouteMatchesExpected(kr, exp.activeNH) {
			if err := netlink.RouteReplace(exp.kernelRoute); err != nil {
				klog.Warningf("ValidateRoutes: failed to correct route %s metric %d: %v",
					exp.desired.Prefix.String(), exp.desired.Metric, err)
			} else {
				klog.Warningf("ValidateRoutes: corrected route %s metric %d (nexthops drifted)",
					exp.desired.Prefix.String(), exp.desired.Metric)

				corrections++
				rkey := m.routeKey(exp.desired.Table, exp.desired.Prefix)
				m.installedRoutes[rkey] = m.buildInstalledState(exp.desired, exp.activeNH)
			}
		}
	}

	// Remove kernel routes that should not exist.
	for key, kr := range kernelRoutes {
		if _, wanted := expected[key]; !wanted {
			krCopy := kr
			if err := netlink.RouteDel(&krCopy); err != nil {
				if !errors.Is(err, syscall.ESRCH) {
					klog.Warningf("ValidateRoutes: failed to remove unexpected route %s metric %d: %v",
						kr.Dst.String(), kr.Priority, err)
				}
			} else {
				klog.Warningf("ValidateRoutes: removed unexpected route %s metric %d",
					kr.Dst.String(), kr.Priority)

				corrections++
			}
		}
	}

	if corrections > 0 {
		klog.Infof("ValidateRoutes: made %d correction(s)", corrections)
	} else {
		klog.V(4).Infof("ValidateRoutes: all routes consistent")
	}

	return corrections
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

// activeNexthops returns the subset of a route's nexthops whose peers are
// considered healthy (or not yet tracked, which defaults to healthy).
func (m *UnifiedRouteManager) activeNexthops(dr DesiredRoute) []DesiredNexthop {
	// Health-check-immune routes always return all nexthops regardless of health.
	if dr.HealthCheckImmune {
		return dr.Nexthops
	}

	var active []DesiredNexthop

	for _, nh := range dr.Nexthops {
		healthy, tracked := m.peerHealthy[nh.PeerID]
		if !tracked || healthy {
			active = append(active, nh)
		}
	}

	return active
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

				if !strings.HasPrefix(link.Attrs().Name, "wg") && link.Attrs().Name != "unbounded0" {
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

					if strings.HasPrefix(link.Attrs().Name, "wg") || link.Attrs().Name == "unbounded0" {
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

// routeReferencesPeer returns true if any nexthop in the route matches the
// given peer ID.
func routeReferencesPeer(dr DesiredRoute, peerID string) bool {
	for _, nh := range dr.Nexthops {
		if nh.PeerID == peerID {
			return true
		}
	}

	return false
}

// routeReferencesPeerPrefix returns true if any nexthop in the route has a
// peer ID that matches the prefix exactly or starts with prefix + "/".
func routeReferencesPeerPrefix(dr DesiredRoute, prefix string) bool {
	for _, nh := range dr.Nexthops {
		if nh.PeerID == prefix || strings.HasPrefix(nh.PeerID, prefix+"/") {
			return true
		}
	}

	return false
}

// kernelRouteMatchesExpected returns true if the kernel route's nexthops match
// the expected active nexthop set.
func kernelRouteMatchesExpected(kr netlink.Route, active []DesiredNexthop) bool {
	if isLinkScopeRoute(active) {
		// Link-scope: single nexthop, no gateway.
		if len(kr.MultiPath) > 0 {
			return false
		}

		return kr.LinkIndex == active[0].LinkIndex
	}

	// Multipath: compare the set of (linkIndex, gateway) pairs.
	if len(kr.MultiPath) != len(active) {
		return false
	}

	type nhKey struct {
		linkIndex int
		gw        string
	}

	expectedNH := make(map[nhKey]bool, len(active))
	for _, nh := range active {
		gwStr := ""
		if nh.Gateway != nil {
			gwStr = nh.Gateway.String()
		}

		expectedNH[nhKey{linkIndex: nh.LinkIndex, gw: gwStr}] = true
	}

	for _, mp := range kr.MultiPath {
		gwStr := ""
		if mp.Gw != nil {
			gwStr = mp.Gw.String()
		}

		if !expectedNH[nhKey{linkIndex: mp.LinkIndex, gw: gwStr}] {
			return false
		}
	}

	return true
}

// ---------------------------------------------------------------------------
// Utility functions
// ---------------------------------------------------------------------------

// intSlicesEqual compares two int slices for equality regardless of order.
// Copies are sorted to avoid mutating the originals.
func intSlicesEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}

	aCopy := make([]int, len(a))
	bCopy := make([]int, len(b))

	copy(aCopy, a)
	copy(bCopy, b)
	sort.Ints(aCopy)
	sort.Ints(bCopy)

	return slices.Equal(aCopy, bCopy)
}

// incrementIP returns a new IP that is one greater than the input.
// It handles carry propagation across bytes for both IPv4 and IPv6 addresses.
func incrementIP(ip net.IP) net.IP {
	result := make(net.IP, len(ip))
	copy(result, ip)

	for i := len(result) - 1; i >= 0; i-- {
		result[i]++
		if result[i] != 0 {
			break
		}
		// Byte overflowed to 0, carry to next byte
	}

	return result
}

// InterfaceHasRoutes checks if the given interface has any routes in the kernel
// routing table. This queries the actual kernel state, not in-memory tracking.
// Used to determine if a gateway was healthy before a pod restart (interface
// exists + has routes).
func InterfaceHasRoutes(ifaceName string) bool {
	return InterfaceHasRoutesWithCache(nil, ifaceName)
}

// InterfaceHasRoutesWithCache is like InterfaceHasRoutes but reads from the
// provided cache when available.
func InterfaceHasRoutesWithCache(cache *NetlinkCache, ifaceName string) bool {
	var (
		link netlink.Link
		err  error
	)

	if cache != nil {
		link, err = cache.LinkByName(ifaceName)
	} else {
		link, err = netlink.LinkByName(ifaceName)
	}

	if err != nil {
		return false
	}

	linkIndex := link.Attrs().Index

	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		var routes []netlink.Route
		if cache != nil {
			routes, err = cache.RouteList(nil, family)
		} else {
			routes, err = netlink.RouteList(nil, family)
		}

		if err != nil {
			continue
		}

		for _, route := range routes {
			if route.LinkIndex == linkIndex {
				return true
			}

			for _, nh := range route.MultiPath {
				if nh.LinkIndex == linkIndex {
					return true
				}
			}
		}
	}

	return false
}
