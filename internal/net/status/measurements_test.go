// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package status

import (
	"bytes"
	"testing"
	"time"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func TestPeerIdentityDigest(t *testing.T) {
	peer := statusv1alpha1.PeerStatus{Name: "a", Tunnel: statusv1alpha1.PeerTunnelStatus{Protocol: "bc", Interface: "d", PublicKey: "e"}}

	original, err := PeerIdentityDigest([]statusv1alpha1.PeerStatus{peer})
	if err != nil {
		t.Fatal(err)
	}

	ambiguous := peer
	ambiguous.Name, ambiguous.Tunnel.Protocol = "ab", "c"

	digest, err := PeerIdentityDigest([]statusv1alpha1.PeerStatus{ambiguous})
	if err != nil || bytes.Equal(original, digest) {
		t.Fatal("identity fields require unambiguous length prefixes")
	}

	another := peer

	another.Tunnel.Interface = "other"
	if _, err := PeerIdentityDigest([]statusv1alpha1.PeerStatus{peer, another}); err != nil {
		t.Fatal("different links to the same named peer must remain distinguishable")
	}

	if _, err := PeerIdentityDigest([]statusv1alpha1.PeerStatus{peer, peer}); err == nil {
		t.Fatal("duplicate identity accepted")
	}

	peer.Name = ""
	if _, err := PeerMeasurementsToProto([]statusv1alpha1.PeerStatus{peer}); err == nil {
		t.Fatal("unnamed identity accepted")
	}

	empty, err := PeerMeasurementsToProto(nil)
	if err != nil || empty.PeerCount != 0 || len(empty.IdentityDigest) != 32 {
		t.Fatal("empty peer set must have a valid digest")
	}
}

func TestPeerMetadataEqualAllFields(t *testing.T) {
	base := statusv1alpha1.PeerStatus{Name: "peer"}

	for field, change := range map[string]func(*statusv1alpha1.PeerStatus){
		"name":            func(p *statusv1alpha1.PeerStatus) { p.Name = "other" },
		"type":            func(p *statusv1alpha1.PeerStatus) { p.PeerType = "site" },
		"site":            func(p *statusv1alpha1.PeerStatus) { p.SiteName = "site-a" },
		"gateway":         func(p *statusv1alpha1.PeerStatus) { p.PodCIDRGateways = []string{"10.0.0.1"} },
		"skip":            func(p *statusv1alpha1.PeerStatus) { p.SkipPodCIDRRoutes = true },
		"distances":       func(p *statusv1alpha1.PeerStatus) { p.RouteDistances = map[string]int{"10.0.0.0/24": 1} },
		"destinations":    func(p *statusv1alpha1.PeerStatus) { p.RouteDestinations = []string{"10.0.0.0/24"} },
		"protocol":        func(p *statusv1alpha1.PeerStatus) { p.Tunnel.Protocol = "GENEVE" },
		"interface":       func(p *statusv1alpha1.PeerStatus) { p.Tunnel.Interface = "geneve0" },
		"key":             func(p *statusv1alpha1.PeerStatus) { p.Tunnel.PublicKey = "key" },
		"endpoint":        func(p *statusv1alpha1.PeerStatus) { p.Tunnel.Endpoint = "10.0.0.1" },
		"allowedIPs":      func(p *statusv1alpha1.PeerStatus) { p.Tunnel.AllowedIPs = []string{"10.0.0.0/24"} },
		"health presence": func(p *statusv1alpha1.PeerStatus) { p.HealthCheck = &statusv1alpha1.HealthCheckPeerStatus{} },
	} {
		t.Run(field, func(t *testing.T) {
			changed := base
			change(&changed)

			if PeerMetadataEqual(base, changed) || PeerMetadataEqual(changed, base) {
				t.Fatal("metadata change ignored")
			}
		})
	}

	changed := base
	changed.Tunnel.RxBytes, changed.Tunnel.TxBytes = 1, 2

	changed.Tunnel.LastHandshake = time.Unix(123, 0)
	if !PeerMetadataEqual(base, changed) {
		t.Fatal("measurements are not metadata")
	}

	base.HealthCheck = &statusv1alpha1.HealthCheckPeerStatus{Enabled: true, Status: "up"}

	changed.HealthCheck = &statusv1alpha1.HealthCheckPeerStatus{Enabled: true, Status: "up", Uptime: "1h", RTT: "5ms"}
	if !PeerMetadataEqual(base, changed) {
		t.Fatal("health measurements are not metadata")
	}
}
