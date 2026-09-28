// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"

	"k8s.io/klog/v2"

	"github.com/Azure/unbounded/internal/net/routeplan"
)

type routeSummaryFamily struct {
	expected     map[routeKey]expectedRoute
	lowest       map[string]int
	destinations map[string]bool
	hasUnbounded bool
}

func newRouteSummaryFamily(plan []routeplan.ExpectedRoute) *routeSummaryFamily {
	family := &routeSummaryFamily{
		expected:     make(map[routeKey]expectedRoute),
		lowest:       make(map[string]int),
		destinations: make(map[string]bool),
	}

	for _, route := range plan {
		distance := effectiveRouteDistance(route.Distance)
		key := routeKey{destination: route.Destination, gateway: route.Gateway, device: route.Device, distance: distance, weight: route.Weight}

		family.expected[key] = expectedRoute{
			destination: route.Destination,
			nextHop:     NextHop{Gateway: route.Gateway, Device: route.Device, Distance: distance, Weight: route.Weight},
		}
		if previous, ok := family.lowest[route.Destination]; !ok || distance < previous {
			family.lowest[route.Destination] = distance
		}
	}

	return family
}

// collectRouteSummary preserves annotation counts, including synthetic missing
// routes and the per-family unbounded0 suppression of missing tunnel hops.
// It never creates status route arrays or fills the full-detail route cache.
func (s *nodeStatusServer) collectRouteSummary(peers []routeplan.Peer, localSite string) (count, mismatchCount int) {
	defer func() {
		if r := recover(); r != nil {
			klog.Warningf("route summary recovered from panic: %v", r)
		}
	}()

	actx := buildAnnotationContext(s.siteInformer, s.sliceInformer, s.gatewayPoolInformer, s.sitePeeringInformer)
	for i := range peers {
		peers[i].SitePeered = sitesAreDirectlyPeered(strings.TrimSpace(localSite), peers[i].SiteName, actx.directSitePeerings)
	}

	ipv4, ipv6 := routeplan.BuildExpectedWireGuardRoutes(peers, actx.routeNodes, routeplan.InterfaceNames{
		WireGuardPrefix: s.cfg.WireGuardInterfacePrefix,
		Geneve:          s.cfg.GeneveInterfaceName,
		VXLAN:           s.cfg.VXLANInterfaceName,
		IPIP:            s.cfg.IPIPInterfaceName,
	})
	families := map[string]*routeSummaryFamily{
		"IPv4": newRouteSummaryFamily(ipv4),
		"IPv6": newRouteSummaryFamily(ipv6),
	}

	s.inspectKernelRoutes(func(familyName, destination string, _ int, hops []observedNextHop) {
		family := families[familyName]
		count++
		family.destinations[destination] = true
		normalized, _ := normalizeRouteDestination(destination)

		for _, observed := range hops {
			if observed.device == unbounded0DeviceName {
				family.hasUnbounded = true
			}

			if !isPeerRoutingInterface(s.cfg, observed.device) {
				continue
			}

			hop := NextHop{Gateway: observed.gateway, Device: observed.device, Distance: observed.distance}

			key, matched := findExpectedWireGuardMatch(family.expected, normalized, &hop)
			if matched {
				delete(family.expected, key)
			} else {
				// Kernel inspection only emits "kernel" route types, so the
				// connected/local host-route exception cannot apply here.
				mismatchCount++
			}
		}
	})

	// The legacy collector does not annotate an entirely empty kernel result.
	if count == 0 {
		return count, mismatchCount
	}

	for _, family := range families {
		for _, expected := range family.expected {
			if effectiveRouteDistance(expected.nextHop.Distance) > family.lowest[expected.destination] {
				continue
			}

			if family.hasUnbounded {
				continue
			}

			mismatchCount++

			if !family.destinations[expected.destination] {
				family.destinations[expected.destination] = true
				count++
			}
		}
	}

	return count, mismatchCount
}
