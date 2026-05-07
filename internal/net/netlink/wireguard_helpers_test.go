// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netlink

import (
	"net"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// TestWireGuardHelperComparisons tests wire guard helper comparisons.
func TestWireGuardHelperComparisons(t *testing.T) {
	wm := &WireGuardManager{}

	currentAllowed := []net.IPNet{
		{IP: net.ParseIP("10.0.0.1"), Mask: net.CIDRMask(32, 32)},
		{IP: net.ParseIP("fd00::1"), Mask: net.CIDRMask(128, 128)},
	}
	if !wm.allowedIPsEqual(currentAllowed, []string{"fd00::1", "10.0.0.1"}) {
		t.Fatalf("expected allowedIPsEqual to normalize IP and CIDR forms")
	}

	if wm.allowedIPsEqual(currentAllowed, []string{"10.0.0.0/24", "fd00::1"}) {
		t.Fatalf("expected different allowed IPs to mismatch")
	}

	a := &net.UDPAddr{IP: net.ParseIP("10.0.0.1"), Port: 51820}
	b := &net.UDPAddr{IP: net.ParseIP("10.0.0.1"), Port: 51820}

	c := &net.UDPAddr{IP: net.ParseIP("10.0.0.2"), Port: 51820}
	if !udpAddrsEqual(nil, nil) || !udpAddrsEqual(a, b) || udpAddrsEqual(a, c) {
		t.Fatalf("udpAddrsEqual behavior mismatch")
	}
}

// TestWireGuardKeepaliveAndPeerNeedsUpdate tests wire guard keepalive and peer needs update.
func TestWireGuardKeepaliveAndPeerNeedsUpdate(t *testing.T) {
	wm := &WireGuardManager{}
	if wm.keepaliveDuration(0) != nil {
		t.Fatalf("expected keepaliveDuration(0) to return nil")
	}

	if d := wm.keepaliveDuration(25); d == nil || *d != 25*time.Second {
		t.Fatalf("unexpected keepaliveDuration(25) result: %#v", d)
	}

	current := wgtypes.Peer{
		Endpoint:                    &net.UDPAddr{IP: net.ParseIP("10.0.0.1"), Port: 51820},
		PersistentKeepaliveInterval: 10 * time.Second,
		AllowedIPs: []net.IPNet{
			{IP: net.ParseIP("10.0.0.1"), Mask: net.CIDRMask(32, 32)},
		},
	}

	desired := WireGuardPeer{
		Endpoint:            "10.0.0.1:51820",
		PersistentKeepalive: 10,
		AllowedIPs:          []string{"10.0.0.1"},
	}
	if wm.peerNeedsUpdate(current, desired) {
		t.Fatalf("expected matching peer config to require no update")
	}

	desired.Endpoint = "10.0.0.2:51820"
	if !wm.peerNeedsUpdate(current, desired) {
		t.Fatalf("expected endpoint change to require update")
	}

	desired.Endpoint = "10.0.0.1:51820"

	desired.PersistentKeepalive = 20
	if !wm.peerNeedsUpdate(current, desired) {
		t.Fatalf("expected keepalive change to require update")
	}
}

// TestBuildPeerConfig tests build peer config.
func TestBuildPeerConfig(t *testing.T) {
	wm := &WireGuardManager{}

	priv, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey() error = %v", err)
	}

	pub := priv.PublicKey().String()

	cfg, err := wm.buildPeerConfig(WireGuardPeer{
		PublicKey:           pub,
		Endpoint:            "10.0.0.1:51820",
		AllowedIPs:          []string{"10.0.0.1", "fd00::1/128", "invalid"},
		PersistentKeepalive: 15,
	})
	if err != nil {
		t.Fatalf("buildPeerConfig() error = %v", err)
	}

	if cfg.PublicKey.String() != pub {
		t.Fatalf("unexpected peer public key")
	}

	if cfg.Endpoint == nil || cfg.Endpoint.String() != "10.0.0.1:51820" {
		t.Fatalf("unexpected endpoint in peer config: %#v", cfg.Endpoint)
	}

	if len(cfg.AllowedIPs) != 2 {
		t.Fatalf("expected 2 valid allowed IPs, got %d", len(cfg.AllowedIPs))
	}

	if cfg.PersistentKeepaliveInterval == nil || *cfg.PersistentKeepaliveInterval != 15*time.Second {
		t.Fatalf("unexpected keepalive interval: %#v", cfg.PersistentKeepaliveInterval)
	}

	if _, err := wm.buildPeerConfig(WireGuardPeer{PublicKey: "bad-key"}); err == nil {
		t.Fatalf("expected invalid public key to fail")
	}

	if _, err := wm.buildPeerConfig(WireGuardPeer{PublicKey: pub, Endpoint: "bad-endpoint"}); err == nil {
		t.Fatalf("expected invalid endpoint to fail")
	}
}
