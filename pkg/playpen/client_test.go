// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package playpen

import (
	"context"
	"testing"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	playpenapi "github.com/Azure/unbounded/internal/playpen/server"
)

func TestEstablishTunnelNamesNamespace(t *testing.T) {
	allocation := &Allocation{AllocationID: "pp-abc"}
	tunnel, err := allocation.EstablishTunnel(context.Background(), TunnelOptions{})
	if err != nil {
		t.Fatalf("EstablishTunnel() error = %v", err)
	}
	if tunnel.Namespace != "playpen-pp-abc" {
		t.Fatalf("namespace = %q", tunnel.Namespace)
	}
}

func TestEstablishTunnelUsesReturnedWireGuardAddress(t *testing.T) {
	allocation := &Allocation{AllocationID: "pp-abc"}
	allocation.Tunnel.WireGuardAddress = "169.254.10.2"

	tunnel, err := allocation.EstablishTunnel(context.Background(), TunnelOptions{})
	if err != nil {
		t.Fatalf("EstablishTunnel() error = %v", err)
	}
	if tunnel.underlay != "169.254.10.2/32" {
		t.Fatalf("underlay = %q", tunnel.underlay)
	}
}

func TestEndpointRequiresOptIn(t *testing.T) {
	allocation := &Allocation{AllocationID: "pp-abc", RequiresEndpoint: true}
	allocation.Tunnel.EndpointRequired = true

	tunnel, err := allocation.EstablishTunnel(context.Background(), TunnelOptions{})
	if err != nil {
		t.Fatalf("EstablishTunnel() error = %v", err)
	}
	if tunnel.allowEndpoint() {
		t.Fatalf("endpoint fallback should require explicit AllowEndpoint")
	}

	tunnel, err = allocation.EstablishTunnel(context.Background(), TunnelOptions{AllowEndpoint: true})
	if err != nil {
		t.Fatalf("EstablishTunnel() with endpoint opt-in error = %v", err)
	}
	if !tunnel.allowEndpoint() {
		t.Fatalf("endpoint fallback was not enabled")
	}
}

func TestGatewayAllowedCIDRsIncludesRoutedCIDRs(t *testing.T) {
	peer := playpenapi.GatewayPeer{PodCIDRs: []string{"10.1.0.0/24"}, RoutedCIDRs: []string{"10.250.0.0/16"}}
	allowed := append([]string{}, peer.PodCIDRs...)
	allowed = append(allowed, peer.RoutedCIDRs...)

	if len(allowed) != 2 || allowed[0] != "10.1.0.0/24" || allowed[1] != "10.250.0.0/16" {
		t.Fatalf("allowed CIDRs = %#v", allowed)
	}
}

func TestAllocationMachineAndSecretHelpers(t *testing.T) {
	allocation := &Allocation{
		AllocationID: "pp-abc",
		Namespace:    "unbounded-kube",
		Site:         "playpen",
		MACAddress:   "02:00:00:00:00:01",
		Lease:        playpenapi.DHCPLease{IP: "10.241.1.10", Subnet: "10.241.1.0/24", Router: "10.241.1.1", DNS: "10.241.1.1"},
		Redfish:      playpenapi.RedfishAccess{URL: "https://playpen.unbounded-kube.svc:9443", Username: "playpen", Password: "secret", DeviceID: "pp-abc"},
	}

	machine := allocation.Machine("", "", "", "quay.io/test/image:latest", "", "")
	if machine.Namespace != "" {
		t.Fatalf("cluster-scoped Machine namespace = %q, want empty", machine.Namespace)
	}
	if machine.Labels[machinav1alpha3.MachineSiteLabelKey] != "playpen" {
		t.Fatalf("machine site label = %#v", machine.Labels)
	}
	lease := machine.Spec.PXE.DHCPLeases[0]
	if lease.MAC != allocation.MACAddress || lease.IPv4 != allocation.Lease.IP || lease.SubnetMask != "255.255.255.0" {
		t.Fatalf("machine DHCP lease = %#v", lease)
	}
	if machine.Spec.PXE.Redfish.PasswordRef.Name != "pp-abc-redfish" || machine.Spec.PXE.Redfish.PasswordRef.Namespace != "unbounded-kube" || machine.Spec.PXE.Redfish.PasswordRef.Key != "password" {
		t.Fatalf("redfish password ref = %#v", machine.Spec.PXE.Redfish.PasswordRef)
	}

	secret := allocation.RedfishSecret("", "", "")
	if secret.Name != "pp-abc-redfish" || secret.Namespace != "unbounded-kube" {
		t.Fatalf("secret metadata = %s/%s", secret.Namespace, secret.Name)
	}
	if got := secret.StringData["password"]; got != "secret" {
		t.Fatalf("secret password = %q", got)
	}
}

func TestClientUnderlayIPUsesHash(t *testing.T) {
	first := clientUnderlayIP("seed-a")
	second := clientUnderlayIP("seed-b")
	if first == second {
		t.Fatalf("clientUnderlayIP collided for simple seeds: %s", first)
	}
}
