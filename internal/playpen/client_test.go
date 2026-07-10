// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package playpen

import (
	"strings"
	"testing"
)

func TestNormalizeClientConfig(t *testing.T) {
	cfg, err := NormalizeClientConfig(ClientConfig{
		Namespace:    "pxe-a",
		EndpointCIDR: "172.30.1.2/30",
		GatewayIP:    "172.30.1.1",
		RemoteIP:     "10.244.0.8",
		Command:      []string{"dnsmasq", "--no-daemon"},
	})
	if err != nil {
		t.Fatalf("NormalizeClientConfig() error = %v", err)
	}

	if cfg.BridgeCIDR != defaultClientBridgeCIDR || cfg.VXLANVNI != defaultVXLANVNI || cfg.VXLANPort != defaultVXLANPort {
		t.Fatalf("defaults not applied: %#v", cfg)
	}

	if cfg.MTU != defaultMTU {
		t.Fatalf("MTU = %d, want %d", cfg.MTU, defaultMTU)
	}

	if got := clientHostLinkName(cfg.Namespace); len(got) > maxInterfaceNameLen {
		t.Fatalf("host link name %q exceeds Linux limit", got)
	}

	if got := clientPeerLinkName(cfg.Namespace); len(got) > maxInterfaceNameLen {
		t.Fatalf("peer link name %q exceeds Linux limit", got)
	}
}

func TestNormalizeClientConfigUsesTunnelMTU(t *testing.T) {
	cfg := DefaultClientConfig()
	cfg.EndpointCIDR = "172.30.1.2/30"
	cfg.GatewayIP = "172.30.1.1"
	cfg.RemoteIP = "10.244.0.8"
	cfg.NodeIP = "172.31.1.2"
	cfg.NodeCIDR = "10.250.1.0/24"
	cfg.Site = "external"
	cfg.GatewayPool = "public"
	cfg.Command = []string{"dnsmasq"}

	cfg, err := NormalizeClientConfig(cfg)
	if err != nil {
		t.Fatalf("NormalizeClientConfig() error = %v", err)
	}

	if cfg.MTU != defaultClientTunnelMTU {
		t.Fatalf("MTU = %d, want %d", cfg.MTU, defaultClientTunnelMTU)
	}
}

func TestNormalizeClientConfigRejectsInvalid(t *testing.T) {
	tests := []struct {
		name string
		edit func(*ClientConfig)
		want string
	}{
		{name: "namespace", edit: func(c *ClientConfig) { c.Namespace = "bad/name" }, want: "namespace"},
		{name: "endpoint", edit: func(c *ClientConfig) { c.EndpointCIDR = "bad" }, want: "endpoint-cidr"},
		{name: "gateway outside prefix", edit: func(c *ClientConfig) { c.GatewayIP = "172.31.1.1" }, want: "outside endpoint network"},
		{name: "remote family", edit: func(c *ClientConfig) { c.RemoteIP = "2001:db8::1" }, want: "same IP family"},
		{name: "incomplete wireguard", edit: func(c *ClientConfig) { c.Site = "external" }, want: "gateway-pool is required"},
		{name: "missing node IP", edit: func(c *ClientConfig) {
			c.Site = "external"
			c.GatewayPool = "public"
			c.NodeCIDR = "10.250.1.0/24"
		}, want: "node-ip must be an IP address"},
		{name: "node IP on underlay", edit: func(c *ClientConfig) {
			c.Site = "external"
			c.GatewayPool = "public"
			c.NodeIP = "172.30.1.3"
			c.NodeCIDR = "10.250.1.0/24"
		}, want: "outside the endpoint-cidr"},
		{name: "overlapping wireguard", edit: func(c *ClientConfig) {
			c.Site = "external"
			c.GatewayPool = "public"
			c.NodeIP = "172.31.1.2"
			c.NodeCIDR = "172.30.1.0/29"
		}, want: "must not overlap"},
		{name: "wireguard mtu", edit: func(c *ClientConfig) {
			c.Site = "external"
			c.GatewayPool = "public"
			c.NodeCIDR = "10.250.1.0/24"
			c.NodeIP = "172.31.1.2"
			c.MTU = 1360
		}, want: "mtu must not exceed"},
		{name: "missing command", edit: func(c *ClientConfig) { c.Command = nil }, want: "command"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ClientConfig{
				Namespace: "pxe-a", EndpointCIDR: "172.30.1.2/30", GatewayIP: "172.30.1.1",
				RemoteIP: "10.244.0.8", Command: []string{"dnsmasq"},
			}
			tt.edit(&cfg)

			_, err := NormalizeClientConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestClientHostLinkNamesAreIsolated(t *testing.T) {
	a := clientHostLinkName("pxe-a")

	b := clientHostLinkName("pxe-b")
	if a == b {
		t.Fatalf("different namespaces produced the same host link %q", a)
	}

	if a == clientPeerLinkName("pxe-a") {
		t.Fatalf("host and peer links have the same name %q", a)
	}
}
