// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package client

import (
	"strings"
	"testing"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestTunnelSetupCommands(t *testing.T) {
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeCommander{}
	metadata := testClaimResponse()
	tunnel := NewTunnel(fake, key.String(), metadata, TunnelConfig{
		WireGuardInterface:  "wg-playpen",
		VXLANInterface:      "vx-playpen",
		PersistentKeepalive: 25,
	})

	if err := tunnel.Setup(t.Context()); err != nil {
		t.Fatalf("setup: %v", err)
	}

	commands := strings.Join(fake.commands, "\n")
	for _, want := range []string{
		"ip link delete vx-playpen",
		"ip link delete wg-playpen",
		"ip link add wg-playpen type wireguard",
		"wg set wg-playpen private-key",
		"peer " + metadata.WireGuard.ServerPublicKey,
		"endpoint 20.30.40.50:32000",
		"allowed-ips 10.88.0.1/32",
		"persistent-keepalive 25",
		"ip addr add 10.88.0.2/32 dev wg-playpen",
		"ip route add 10.88.0.1/32 dev wg-playpen",
		"ip link add vx-playpen type vxlan id 12001 dev wg-playpen local 10.88.0.2 remote 10.88.0.1 dstport 4789 nolearning",
		"bridge fdb append 00:00:00:00:00:00 dev vx-playpen dst 10.88.0.1",
	} {
		if !strings.Contains(commands, want) {
			t.Fatalf("commands missing %q:\n%s", want, commands)
		}
	}
}

func TestDefaultTunnelInterfaceNamesAreShort(t *testing.T) {
	cfg := tunnelConfigWithDefaults(TunnelConfig{}, "claim-key")
	if !strings.HasPrefix(cfg.WireGuardInterface, "ppwg") || len(cfg.WireGuardInterface) > 15 {
		t.Fatalf("wireguard interface = %q", cfg.WireGuardInterface)
	}
	if !strings.HasPrefix(cfg.VXLANInterface, "ppvx") || len(cfg.VXLANInterface) > 15 {
		t.Fatalf("vxlan interface = %q", cfg.VXLANInterface)
	}
}
