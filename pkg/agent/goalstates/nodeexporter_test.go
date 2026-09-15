// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"net"
	"testing"
)

func TestNodeServiceListenersConflict(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		first  string
		second string
		want   bool
	}{
		"same":            {first: "10.0.0.4:9100", second: "10.0.0.4:9100", want: true},
		"wildcard":        {first: "10.0.0.4:9100", second: "0.0.0.0:9100", want: true},
		"different IP":    {first: "10.0.0.4:9100", second: "10.0.0.5:9100"},
		"different port":  {first: "10.0.0.4:9100", second: "10.0.0.4:9253"},
		"invalid address": {first: "invalid", second: "10.0.0.4:9100"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := nodeServiceListenersConflict(test.first, test.second); got != test.want {
				t.Fatalf("nodeServiceListenersConflict() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestResolveNodeExporterAddress(t *testing.T) {
	t.Parallel()

	deps := localDNSMetricsDeps{
		interfaceAddrs: func() ([]net.Addr, error) {
			return []net.Addr{&net.IPNet{IP: net.ParseIP("10.0.0.4"), Mask: net.CIDRMask(24, 32)}}, nil
		},
		lookupIP: func(string) ([]net.IP, error) { return nil, nil },
		resolveBindAddress: func(net.IP) (net.IP, error) {
			return net.ParseIP("10.0.0.4"), nil
		},
	}

	tests := map[string]struct {
		configured string
		nodeIP     string
		want       string
	}{
		"configured": {configured: "10.0.0.4:19100", want: "10.0.0.4:19100"},
		"node IP":    {nodeIP: "10.0.0.4", want: "10.0.0.4:9100"},
		"default":    {want: "10.0.0.4:9100"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveNodeExporterAddress(test.configured, test.nodeIP, "node.test", deps)
			if err != nil {
				t.Fatalf("resolveNodeExporterAddress() error = %v", err)
			}

			if got != test.want {
				t.Fatalf("resolveNodeExporterAddress() = %q, want %q", got, test.want)
			}
		})
	}
}
