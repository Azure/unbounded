// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package client

import (
	"strings"
	"testing"

	"github.com/Azure/unbounded/internal/playpen/operator"
)

func TestTunnelSetupCommands(t *testing.T) {
	fake := &fakeCommander{}
	metadata := testAllocResponse()
	tunnel := newTunnel(fake, metadata, TunnelConfig{
		VXLANInterface: "vx-playpen",
	})

	if err := tunnel.Setup(t.Context()); err != nil {
		t.Fatalf("setup: %v", err)
	}

	commands := strings.Join(fake.commands, "\n")
	for _, want := range []string{
		"ip link delete vx-playpen",
		"ip link add vx-playpen type vxlan id 12001 dev unbounded0 local 10.88.0.2 remote 10.88.0.1 dstport 4789 nolearning",
		"bridge fdb append 00:00:00:00:00:00 dev vx-playpen dst 10.88.0.1",
		"ip link set vx-playpen up",
		"ip addr add 192.168.200.1/24 dev vx-playpen",
	} {
		if !strings.Contains(commands, want) {
			t.Fatalf("commands missing %q:\n%s", want, commands)
		}
	}
}

func TestGuestNetworkPrefixes(t *testing.T) {
	metadata := testAllocResponse()

	gatewayPrefix, guestSubnet, err := guestNetworkPrefixes(metadata.Network)
	if err != nil {
		t.Fatalf("guest network prefixes: %v", err)
	}

	if gatewayPrefix != "192.168.200.1/24" {
		t.Fatalf("gateway prefix = %q, want 192.168.200.1/24", gatewayPrefix)
	}

	if guestSubnet != "192.168.200.0/24" {
		t.Fatalf("guest subnet = %q, want 192.168.200.0/24", guestSubnet)
	}
}

func TestGuestNetworkPrefixesRejectsInvalidMetadata(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*operator.NetworkResponse)
	}{
		{
			name: "guest IP",
			mutate: func(network *operator.NetworkResponse) {
				network.GuestIPv4 = "not-an-ip"
			},
		},
		{
			name: "gateway IP",
			mutate: func(network *operator.NetworkResponse) {
				network.GatewayIPv4 = "not-an-ip"
			},
		},
		{
			name: "subnet mask",
			mutate: func(network *operator.NetworkResponse) {
				network.SubnetMask = "255.0.255.0"
			},
		},
		{
			name: "gateway outside subnet",
			mutate: func(network *operator.NetworkResponse) {
				network.GatewayIPv4 = "192.168.201.1"
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			metadata := testAllocResponse()
			tt.mutate(&metadata.Network)

			if _, _, err := guestNetworkPrefixes(metadata.Network); err == nil {
				t.Fatal("guest network prefixes succeeded, want error")
			}
		})
	}
}

func TestDefaultTunnelInterfaceNamesAreShort(t *testing.T) {
	cfg := tunnelConfigWithDefaults(TunnelConfig{}, "claim-key")
	if !strings.HasPrefix(cfg.VXLANInterface, "ppvx") || len(cfg.VXLANInterface) > 15 {
		t.Fatalf("vxlan interface = %q", cfg.VXLANInterface)
	}
}
