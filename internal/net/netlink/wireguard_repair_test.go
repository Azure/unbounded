// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netlink

import (
	"errors"
	"net"
	"slices"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

type repairWireGuardClient struct {
	device       *wgtypes.Device
	deviceErr    error
	configure    []wgtypes.Config
	configureErr error
}

func (c *repairWireGuardClient) Device(string) (*wgtypes.Device, error) {
	if c.deviceErr != nil {
		return nil, c.deviceErr
	}

	return c.device, nil
}

func (c *repairWireGuardClient) ConfigureDevice(_ string, cfg wgtypes.Config) error {
	if c.configureErr != nil {
		return c.configureErr
	}

	c.configure = append(c.configure, cfg)
	if cfg.PrivateKey != nil {
		c.device.PrivateKey = *cfg.PrivateKey
	}
	if cfg.ListenPort != nil {
		c.device.ListenPort = *cfg.ListenPort
	}

	for _, change := range cfg.Peers {
		index := slices.IndexFunc(c.device.Peers, func(peer wgtypes.Peer) bool {
			return peer.PublicKey == change.PublicKey
		})
		if change.Remove {
			if index >= 0 {
				c.device.Peers = slices.Delete(c.device.Peers, index, index+1)
			}

			continue
		}

		if index < 0 {
			c.device.Peers = append(c.device.Peers, wgtypes.Peer{PublicKey: change.PublicKey})
			index = len(c.device.Peers) - 1
		}

		if change.Endpoint != nil {
			c.device.Peers[index].Endpoint = change.Endpoint
		}
		if change.ReplaceAllowedIPs {
			c.device.Peers[index].AllowedIPs = append([]net.IPNet(nil), change.AllowedIPs...)
		}
		if change.PersistentKeepaliveInterval != nil {
			c.device.Peers[index].PersistentKeepaliveInterval = *change.PersistentKeepaliveInterval
		}
	}

	return nil
}

func (c *repairWireGuardClient) Close() error { return nil }

func repairTestKey(t *testing.T) wgtypes.Key {
	t.Helper()

	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}

	return key
}

func TestRepairDriftRestoresExplicitEndpointAndPreservesLearnedEndpoint(t *testing.T) {
	privateKey := repairTestKey(t)
	explicitKey := repairTestKey(t).PublicKey()
	learnedKey := repairTestKey(t).PublicKey()
	client := &repairWireGuardClient{device: &wgtypes.Device{Name: "wg51821"}}
	manager := NewWireGuardManagerWithClient("wg51821", client)

	peers := []WireGuardPeer{
		{PublicKey: explicitKey.String(), Endpoint: "192.0.2.10:51821", AllowedIPs: []string{"10.244.1.0/24"}},
		{PublicKey: learnedKey.String(), AllowedIPs: []string{"10.244.2.0/24"}, PersistentKeepalive: 0},
	}
	if err := manager.Configure(privateKey.String(), 51821, peers); err != nil {
		t.Fatal(err)
	}

	learned := &net.UDPAddr{IP: net.ParseIP("198.51.100.20"), Port: 40000}
	explicitIndex := slices.IndexFunc(client.device.Peers, func(peer wgtypes.Peer) bool { return peer.PublicKey == explicitKey })
	learnedIndex := slices.IndexFunc(client.device.Peers, func(peer wgtypes.Peer) bool { return peer.PublicKey == learnedKey })
	client.device.Peers[explicitIndex].Endpoint = &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 51820}
	client.device.Peers[learnedIndex].Endpoint = learned
	client.device.Peers[learnedIndex].PersistentKeepaliveInterval = 25 * time.Second
	client.configure = nil

	if err := manager.RepairDrift(); err != nil {
		t.Fatal(err)
	}

	if got := client.device.Peers[explicitIndex].Endpoint.String(); got != "192.0.2.10:51821" {
		t.Fatalf("explicit endpoint was not repaired: %s", got)
	}
	if got := client.device.Peers[learnedIndex].Endpoint; got != learned {
		t.Fatalf("learned endpoint changed: got %v, want %v", got, learned)
	}
	if got := client.device.Peers[learnedIndex].PersistentKeepaliveInterval; got != 0 {
		t.Fatalf("keepalive was not disabled: %v", got)
	}
	if len(client.configure) != 1 || len(client.configure[0].Peers) != 2 {
		t.Fatalf("repair wrote unexpected configuration: %+v", client.configure)
	}

	client.configure = nil
	if err := manager.RepairDrift(); err != nil {
		t.Fatal(err)
	}
	if len(client.configure) != 0 {
		t.Fatal("unchanged state caused another WireGuard write")
	}
}

func TestRepairDriftRestoresDeviceAndPeerMembership(t *testing.T) {
	privateKey := repairTestKey(t)
	peerKey := repairTestKey(t).PublicKey()
	strangerKey := repairTestKey(t).PublicKey()
	client := &repairWireGuardClient{device: &wgtypes.Device{Name: "wg51820"}}
	manager := NewWireGuardManagerWithClient("wg51820", client)

	if err := manager.Configure(privateKey.String(), 51820, []WireGuardPeer{{
		PublicKey: peerKey.String(), AllowedIPs: []string{"10.244.1.0/24"},
	}}); err != nil {
		t.Fatal(err)
	}

	client.device.PrivateKey = repairTestKey(t)
	client.device.ListenPort = 51822
	client.device.Peers = []wgtypes.Peer{{PublicKey: strangerKey}}
	client.configure = nil

	if err := manager.RepairDrift(); err != nil {
		t.Fatal(err)
	}
	if client.device.PrivateKey != privateKey || client.device.ListenPort != 51820 {
		t.Fatal("device configuration was not repaired")
	}
	if len(client.device.Peers) != 1 || client.device.Peers[0].PublicKey != peerKey {
		t.Fatalf("peer membership was not repaired: %+v", client.device.Peers)
	}
}

func TestRepairDriftReadFailure(t *testing.T) {
	manager := NewWireGuardManagerWithClient("wg51820", &repairWireGuardClient{device: &wgtypes.Device{}})
	client := &repairWireGuardClient{device: &wgtypes.Device{}, deviceErr: errors.New("read failed")}
	manager.client = client
	manager.desired = &wireGuardDesired{}

	if err := manager.RepairDrift(); err == nil {
		t.Fatal("expected repair read failure")
	}
}
