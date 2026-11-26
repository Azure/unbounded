// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"net"
	"slices"
	"testing"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	unboundednetnetlink "github.com/Azure/unbounded/internal/net/netlink"
)

type reconcileWireGuardClient struct {
	device    *wgtypes.Device
	deviceErr error
	writes    int
}

func (c *reconcileWireGuardClient) Device(string) (*wgtypes.Device, error) {
	if c.deviceErr != nil {
		return nil, c.deviceErr
	}

	return c.device, nil
}

func (c *reconcileWireGuardClient) ConfigureDevice(_ string, cfg wgtypes.Config) error {
	c.writes++
	if cfg.ListenPort != nil {
		c.device.ListenPort = *cfg.ListenPort
	}

	if cfg.PrivateKey != nil {
		c.device.PrivateKey = *cfg.PrivateKey
	}

	for _, change := range cfg.Peers {
		index := slices.IndexFunc(c.device.Peers, func(peer wgtypes.Peer) bool {
			return peer.PublicKey == change.PublicKey
		})
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
	}

	return nil
}

func (c *reconcileWireGuardClient) Close() error { return nil }

func TestUnchangedReconcileRepairsWireGuardDrift(t *testing.T) {
	fixture := newPoolPeeringProtocolFixture(t, false)

	privateKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}

	peerKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}

	client := &reconcileWireGuardClient{device: &wgtypes.Device{Name: "wg51821"}}

	manager := unboundednetnetlink.NewWireGuardManagerWithClient("wg51821", client)
	if err := manager.Configure(privateKey.String(), 51821, []unboundednetnetlink.WireGuardPeer{{
		PublicKey: peerKey.PublicKey().String(), Endpoint: "192.0.2.10:51821", AllowedIPs: []string{"10.244.1.0/24"},
	}}); err != nil {
		t.Fatal(err)
	}

	client.writes = 0
	fixture.state.gatewayWireguardManagers = map[string]*unboundednetnetlink.WireGuardManager{"wg51821": manager}

	if err := fixture.update(); err != nil {
		t.Fatal(err)
	}

	if fixture.configureCalls != 1 {
		t.Fatalf("initial full configure calls = %d, want 1", fixture.configureCalls)
	}

	client.device.Peers[0].Endpoint = &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 51820}

	if err := fixture.update(); err != nil {
		t.Fatal(err)
	}

	if fixture.configureCalls != 1 {
		t.Fatalf("drift repair ran full topology configuration: calls=%d", fixture.configureCalls)
	}

	if got := client.device.Peers[0].Endpoint.String(); got != "192.0.2.10:51821" {
		t.Fatalf("endpoint after unchanged reconcile = %s", got)
	}

	if client.writes != 1 {
		t.Fatalf("drift repair writes = %d, want 1", client.writes)
	}

	client.deviceErr = errors.New("device unavailable")

	if err := fixture.update(); err == nil {
		t.Fatal("expected device read failure")
	}

	if !fixture.state.dataplaneDirty {
		t.Fatal("repair failure did not mark dataplane dirty")
	}

	client.deviceErr = nil

	if err := fixture.update(); err != nil {
		t.Fatal(err)
	}

	if fixture.configureCalls != 2 {
		t.Fatalf("dirty retry did not run full configuration: calls=%d", fixture.configureCalls)
	}

	if fixture.state.dataplaneDirty {
		t.Fatal("successful retry did not clear dataplane dirty state")
	}
}
