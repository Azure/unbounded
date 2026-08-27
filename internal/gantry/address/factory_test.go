// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package address

import (
	"testing"

	"github.com/multiformats/go-multiaddr"
)

func TestRewrite(t *testing.T) {
	tests := []struct {
		name  string
		addr  string
		podIP string
		want  string
	}{
		{"IPv4 wildcard", "/ip4/0.0.0.0/tcp/4001", "10.42.0.7", "/ip4/10.42.0.7/tcp/4001"},
		{"IPv6 wildcard", "/ip6/::/tcp/4001", "fd00:10:244::7", "/ip6/fd00:10:244::7/tcp/4001"},
		{"IPv4 cross family", "/ip4/0.0.0.0/tcp/4001", "fd00:10:244::7", ""},
		{"IPv6 cross family", "/ip6/::/tcp/4001", "10.42.0.7", ""},
		{"missing Pod IP", "/ip4/0.0.0.0/tcp/4001", "", ""},
		{"malformed Pod IP", "/ip4/0.0.0.0/tcp/4001", "bad", ""},
		{"loopback", "/ip4/127.0.0.1/tcp/4001", "10.42.0.7", ""},
		{"link local", "/ip4/169.254.1.1/tcp/4001", "10.42.0.7", ""},
		{"IPv6 link local", "/ip6/fe80::1/tcp/4001", "fd00:10:244::7", ""},
		{"concrete", "/ip4/10.42.0.8/tcp/4001", "10.42.0.7", "/ip4/10.42.0.8/tcp/4001"},
		{"DNS rejected", "/dns4/peer.example/tcp/4001", "10.42.0.7", ""},
		{"malformed", "not-a-multiaddr", "10.42.0.7", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Rewrite(tt.addr, tt.podIP); got != tt.want {
				t.Fatalf("Rewrite(%q, %q) = %q, want %q", tt.addr, tt.podIP, got, tt.want)
			}
		})
	}
}

func TestFactoryDeduplicates(t *testing.T) {
	input := []multiaddr.Multiaddr{
		multiaddr.StringCast("/ip4/0.0.0.0/tcp/4001"),
		multiaddr.StringCast("/ip4/10.42.0.7/tcp/4001"),
		multiaddr.StringCast("/ip4/127.0.0.1/tcp/4001"),
	}

	got := Factory("10.42.0.7")(input)
	if len(got) != 1 || got[0].String() != "/ip4/10.42.0.7/tcp/4001" {
		t.Fatalf("Factory() = %v, want one Pod-IP address", got)
	}
}
