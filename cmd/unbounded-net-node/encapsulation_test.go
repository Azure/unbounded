// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"testing"

	unboundednetv1alpha1 "github.com/Azure/unbounded-kube/api/net/v1alpha1"
)

func ptrTunnelProto(t unboundednetv1alpha1.TunnelProtocol) *unboundednetv1alpha1.TunnelProtocol {
	return &t
}

func TestResolveSingleScope(t *testing.T) {
	// nil -> Auto
	if got := resolveSingleScope(nil); got != unboundednetv1alpha1.TunnelProtocolAuto {
		t.Errorf("expected Auto for nil, got %q", got)
	}
	// Auto -> Auto
	if got := resolveSingleScope(ptrTunnelProto(unboundednetv1alpha1.TunnelProtocolAuto)); got != unboundednetv1alpha1.TunnelProtocolAuto {
		t.Errorf("expected Auto for explicit Auto, got %q", got)
	}
	// WireGuard -> WireGuard
	if got := resolveSingleScope(ptrTunnelProto(unboundednetv1alpha1.TunnelProtocolWireGuard)); got != unboundednetv1alpha1.TunnelProtocolWireGuard {
		t.Errorf("expected WireGuard, got %q", got)
	}
	// GENEVE -> GENEVE
	if got := resolveSingleScope(ptrTunnelProto(unboundednetv1alpha1.TunnelProtocolGENEVE)); got != unboundednetv1alpha1.TunnelProtocolGENEVE {
		t.Errorf("expected GENEVE, got %q", got)
	}
	// IPIP -> IPIP
	if got := resolveSingleScope(ptrTunnelProto(unboundednetv1alpha1.TunnelProtocolIPIP)); got != unboundednetv1alpha1.TunnelProtocolIPIP {
		t.Errorf("expected IPIP, got %q", got)
	}
}

func TestResolveAutoTunnelProtocol(t *testing.T) {
	if got := resolveAutoTunnelProtocol(true, "WireGuard", "WireGuard"); got != unboundednetv1alpha1.TunnelProtocolWireGuard {
		t.Errorf("expected WireGuard for external IP, got %q", got)
	}
	// Default private preference is GENEVE
	if got := resolveAutoTunnelProtocol(false, "GENEVE", "WireGuard"); got != unboundednetv1alpha1.TunnelProtocolGENEVE {
		t.Errorf("expected GENEVE for internal IP with IPIP preference, got %q", got)
	}
	// GENEVE preference
	if got := resolveAutoTunnelProtocol(false, "GENEVE", "WireGuard"); got != unboundednetv1alpha1.TunnelProtocolGENEVE {
		t.Errorf("expected GENEVE for internal IP with GENEVE preference, got %q", got)
	}
	// Empty preference falls back to IPIP
	if got := resolveAutoTunnelProtocol(false, "", ""); got != unboundednetv1alpha1.TunnelProtocolGENEVE {
		t.Errorf("expected GENEVE for empty preference, got %q", got)
	}
}

func TestEffectiveTunnelProtocol(t *testing.T) {
	// Auto with external IP -> WireGuard (from ConfigMap default)
	got := effectiveTunnelProtocol(true, "GENEVE", "WireGuard", nil)
	if got != unboundednetv1alpha1.TunnelProtocolWireGuard {
		t.Errorf("expected WireGuard for Auto+external, got %q", got)
	}

	// Auto with internal IP -> GENEVE (from ConfigMap default)
	got = effectiveTunnelProtocol(false, "GENEVE", "WireGuard", nil)
	if got != unboundednetv1alpha1.TunnelProtocolGENEVE {
		t.Errorf("expected GENEVE for Auto+internal, got %q", got)
	}

	// Explicit GENEVE on the governing CRD overrides Auto
	got = effectiveTunnelProtocol(true, "GENEVE", "WireGuard", ptrTunnelProto(unboundednetv1alpha1.TunnelProtocolGENEVE))
	if got != unboundednetv1alpha1.TunnelProtocolGENEVE {
		t.Errorf("expected GENEVE for explicit GENEVE scope, got %q", got)
	}

	// Explicit WireGuard on the governing CRD
	got = effectiveTunnelProtocol(false, "GENEVE", "WireGuard", ptrTunnelProto(unboundednetv1alpha1.TunnelProtocolWireGuard))
	if got != unboundednetv1alpha1.TunnelProtocolWireGuard {
		t.Errorf("expected WireGuard for explicit WireGuard scope, got %q", got)
	}

	// Explicit IPIP on the governing CRD
	got = effectiveTunnelProtocol(false, "GENEVE", "WireGuard", ptrTunnelProto(unboundednetv1alpha1.TunnelProtocolIPIP))
	if got != unboundednetv1alpha1.TunnelProtocolIPIP {
		t.Errorf("expected GENEVE for explicit IPIP scope, got %q", got)
	}

	// Auto scope (explicitly set) -> falls through to ConfigMap
	got = effectiveTunnelProtocol(false, "GENEVE", "WireGuard", ptrTunnelProto(unboundednetv1alpha1.TunnelProtocolAuto))
	if got != unboundednetv1alpha1.TunnelProtocolGENEVE {
		t.Errorf("expected GENEVE for explicit Auto scope, got %q", got)
	}
}

// TestResolveTunnelProtocolsOnPeers_GatewayMixedPoolTypes verifies that
// cross-site gateway peers use WireGuard when either the local or remote pool
// is External.
func TestResolveTunnelProtocolsOnPeers_GatewayMixedPoolTypes(t *testing.T) {
	tests := []struct {
		name                string
		localPoolIsExternal bool
		remotePoolType      string
		remoteSite          string
		networkPeered       bool
		wantProtocol        string
	}{
		{
			name:                "External-to-External cross-site uses WireGuard",
			localPoolIsExternal: true,
			remotePoolType:      "External",
			remoteSite:          "remote",
			wantProtocol:        "WireGuard",
		},
		{
			name:                "External-to-Internal cross-site uses WireGuard",
			localPoolIsExternal: true,
			remotePoolType:      "Internal",
			remoteSite:          "remote",
			wantProtocol:        "WireGuard",
		},
		{
			name:                "Internal-to-External cross-site uses WireGuard",
			localPoolIsExternal: false,
			remotePoolType:      "External",
			remoteSite:          "remote",
			wantProtocol:        "WireGuard",
		},
		{
			name:                "Internal-to-Internal cross-site uses GENEVE",
			localPoolIsExternal: false,
			remotePoolType:      "Internal",
			remoteSite:          "remote",
			wantProtocol:        "GENEVE",
		},
		{
			name:                "External-to-Internal same-site uses GENEVE",
			localPoolIsExternal: true,
			remotePoolType:      "Internal",
			remoteSite:          "local",
			wantProtocol:        "GENEVE",
		},
		{
			name:                "External-to-Internal network-peered uses GENEVE",
			localPoolIsExternal: true,
			remotePoolType:      "Internal",
			remoteSite:          "peered",
			networkPeered:       true,
			wantProtocol:        "GENEVE",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw := []gatewayPeerInfo{{
				Name:     "test-gw",
				SiteName: tt.remoteSite,
				PoolName: "test-pool",
				PoolType: tt.remotePoolType,
			}}

			networkPeeredSites := map[string]bool{}
			if tt.networkPeered {
				networkPeeredSites[tt.remoteSite] = true
			}

			resolveTunnelProtocolsOnPeers(
				nil, gw, "local",
				map[string]bool{}, networkPeeredSites,
				true, tt.localPoolIsExternal, nil,
				"GENEVE", "WireGuard",
				nil, nil, nil, nil,
			)

			if gw[0].TunnelProtocol != tt.wantProtocol {
				t.Errorf("got %q, want %q", gw[0].TunnelProtocol, tt.wantProtocol)
			}
		})
	}
}

// TestResolveTunnelProtocolsOnPeers_GatewayMeshSGPAScope verifies that gateway
// nodes use the SiteGatewayPoolAssignment scope (not the Site scope) for
// same-site mesh peers, matching the non-gateway peer's scope selection.
func TestResolveTunnelProtocolsOnPeers_GatewayMeshSGPAScope(t *testing.T) {
	tests := []struct {
		name         string
		siteTunnel   string // Site tunnelProtocol
		sgpaTunnel   string // SGPA tunnelProtocol (keyed as "s6|s6intgw1")
		wantProtocol string
	}{
		{
			name:         "SGPA overrides Site for gateway mesh peers",
			siteTunnel:   "VXLAN",
			sgpaTunnel:   "GENEVE",
			wantProtocol: "GENEVE",
		},
		{
			name:         "SGPA nil falls through to Site",
			siteTunnel:   "VXLAN",
			sgpaTunnel:   "",
			wantProtocol: "VXLAN",
		},
		{
			name:         "SGPA explicit WireGuard wins",
			siteTunnel:   "GENEVE",
			sgpaTunnel:   "WireGuard",
			wantProtocol: "WireGuard",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mesh := []meshPeerInfo{{
				Name:     "user-node",
				SiteName: "s6",
			}}
			siteTunnelProtos := map[string]string{"s6": tt.siteTunnel}

			assignmentPoolTunnelProtos := map[string]string{}
			if tt.sgpaTunnel != "" {
				assignmentPoolTunnelProtos["s6|s6intgw1"] = tt.sgpaTunnel
			}

			resolveTunnelProtocolsOnPeers(
				mesh, nil, "s6",
				map[string]bool{}, map[string]bool{},
				true, false, []string{"s6intgw1"},
				"GENEVE", "WireGuard",
				siteTunnelProtos, nil, assignmentPoolTunnelProtos, nil,
			)

			if mesh[0].TunnelProtocol != tt.wantProtocol {
				t.Errorf("got %q, want %q", mesh[0].TunnelProtocol, tt.wantProtocol)
			}
		})
	}

	// Non-gateway nodes still use the Site scope for same-site mesh peers
	t.Run("Non-gateway uses Site scope", func(t *testing.T) {
		mesh := []meshPeerInfo{{Name: "peer", SiteName: "s6"}}
		resolveTunnelProtocolsOnPeers(
			mesh, nil, "s6",
			map[string]bool{}, map[string]bool{},
			false, false, nil,
			"GENEVE", "WireGuard",
			map[string]string{"s6": "VXLAN"}, nil, nil, nil,
		)

		if mesh[0].TunnelProtocol != "VXLAN" {
			t.Errorf("non-gateway should use Site scope: got %q, want VXLAN", mesh[0].TunnelProtocol)
		}
	})
}
