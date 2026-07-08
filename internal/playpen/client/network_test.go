// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package client

import (
	"strings"
	"testing"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/Azure/unbounded/internal/playpen/operator"
)

func TestTunnelSetupCommands(t *testing.T) {
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}

	fake := &fakeCommander{}
	metadata := testAllocResponse()
	tunnel := newTunnel(fake, key.String(), metadata, TunnelConfig{
		NetworkNamespace:             "ns-playpen",
		WireGuardInterface:           "wg-playpen",
		VXLANInterface:               "vx-playpen",
		ManagementHostInterface:      "mh-playpen",
		ManagementNamespaceInterface: "mn-playpen",
		ManagementHostAddress:        "169.254.10.1/30",
		ManagementNamespaceAddress:   "169.254.10.2/30",
		PersistentKeepalive:          25,
	})

	if err := tunnel.Setup(t.Context()); err != nil {
		t.Fatalf("setup: %v", err)
	}

	commands := strings.Join(fake.commands, "\n")
	for _, want := range []string{
		"iptables -t nat -D POSTROUTING -s 169.254.10.2/32 -j MASQUERADE",
		"iptables -D FORWARD -o mh-playpen -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT",
		"iptables -D FORWARD -i mh-playpen -j ACCEPT",
		"ip link delete mh-playpen",
		"ip netns delete ns-playpen",
		"ip link delete wg-playpen",
		"ip netns add ns-playpen",
		"ip link add mh-playpen type veth peer name mn-playpen",
		"ip link set mn-playpen netns ns-playpen",
		"ip addr add 169.254.10.1/30 dev mh-playpen",
		"ip -n ns-playpen addr add 169.254.10.2/30 dev mn-playpen",
		"ip -n ns-playpen route add default via 169.254.10.1 dev mn-playpen",
		"sysctl -w net.ipv4.ip_forward=1",
		"iptables -A FORWARD -i mh-playpen -j ACCEPT",
		"iptables -A FORWARD -o mh-playpen -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT",
		"iptables -t nat -A POSTROUTING -s 169.254.10.2/32 -j MASQUERADE",
		"ip link add wg-playpen type wireguard",
		"ip link set wg-playpen netns ns-playpen",
		"ip netns exec ns-playpen wg set wg-playpen private-key",
		"peer " + metadata.WireGuard.ServerPublicKey,
		"endpoint 20.30.40.50:32000",
		"allowed-ips 10.88.0.1/32",
		"persistent-keepalive 25",
		"ip -n ns-playpen addr add 10.88.0.2/32 dev wg-playpen",
		"ip -n ns-playpen route add 10.88.0.1/32 dev wg-playpen",
		"ip -n ns-playpen link add vx-playpen type vxlan id 12001 dev wg-playpen local 10.88.0.2 remote 10.88.0.1 dstport 4789 nolearning",
		"ip netns exec ns-playpen bridge fdb append 00:00:00:00:00:00 dev vx-playpen dst 10.88.0.1",
		"ip -n ns-playpen addr add 192.168.200.1/24 dev vx-playpen",
		"ip -n ns-playpen link set vx-playpen up",
		"ip netns exec ns-playpen sysctl -w net.ipv4.ip_forward=1",
		"ip netns exec ns-playpen iptables -A FORWARD -i vx-playpen -o mn-playpen -j ACCEPT",
		"ip netns exec ns-playpen iptables -A FORWARD -i mn-playpen -o vx-playpen -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT",
	} {
		if !strings.Contains(commands, want) {
			t.Fatalf("commands missing %q:\n%s", want, commands)
		}
	}
}

func TestGuestGatewayPrefix(t *testing.T) {
	metadata := testAllocResponse()

	gatewayPrefix, err := guestGatewayPrefix(metadata.Network)
	if err != nil {
		t.Fatalf("guest gateway prefix: %v", err)
	}

	if gatewayPrefix != "192.168.200.1/24" {
		t.Fatalf("gateway prefix = %q, want 192.168.200.1/24", gatewayPrefix)
	}
}

func TestGuestGatewayPrefixRejectsInvalidMetadata(t *testing.T) {
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

			if _, err := guestGatewayPrefix(metadata.Network); err == nil {
				t.Fatal("guest gateway prefix succeeded, want error")
			}
		})
	}
}

func TestDefaultTunnelInterfaceNamesAreShort(t *testing.T) {
	cfg := tunnelConfigWithDefaults(TunnelConfig{}, "claim-key")
	if !strings.HasPrefix(cfg.NetworkNamespace, "ppns") || len(cfg.NetworkNamespace) > 15 {
		t.Fatalf("network namespace = %q", cfg.NetworkNamespace)
	}

	if !strings.HasPrefix(cfg.WireGuardInterface, "ppwg") || len(cfg.WireGuardInterface) > 15 {
		t.Fatalf("wireguard interface = %q", cfg.WireGuardInterface)
	}

	if !strings.HasPrefix(cfg.VXLANInterface, "ppvx") || len(cfg.VXLANInterface) > 15 {
		t.Fatalf("vxlan interface = %q", cfg.VXLANInterface)
	}

	if !strings.HasPrefix(cfg.ManagementHostInterface, "ppmh") || len(cfg.ManagementHostInterface) > 15 {
		t.Fatalf("management host interface = %q", cfg.ManagementHostInterface)
	}

	if !strings.HasPrefix(cfg.ManagementNamespaceInterface, "ppmn") || len(cfg.ManagementNamespaceInterface) > 15 {
		t.Fatalf("management namespace interface = %q", cfg.ManagementNamespaceInterface)
	}

	if cfg.ManagementHostAddress == "" || cfg.ManagementNamespaceAddress == "" {
		t.Fatalf("management addresses were not defaulted: %#v", cfg)
	}
}
