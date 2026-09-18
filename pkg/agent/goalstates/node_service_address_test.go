// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"errors"
	"net"
	"strings"
	"testing"
)

func TestResolveNodeServiceAddress(t *testing.T) {
	t.Parallel()

	hostAddresses := func(values ...string) func() ([]net.Addr, error) {
		return func() ([]net.Addr, error) {
			addresses := make([]net.Addr, 0, len(values))
			for _, value := range values {
				ip := net.ParseIP(value)
				addresses = append(addresses, &net.IPNet{IP: ip, Mask: net.CIDRMask(24, 32)})
			}

			return addresses, nil
		}
	}

	tests := []struct {
		name     string
		nodeIPs  string
		nodeName string
		resolver nodeServiceAddressResolver
		want     string
		wantErr  string
	}{
		{
			name:     "configured kubelet IPv4",
			nodeIPs:  "fd00::4,10.0.0.4",
			nodeName: "node.example",
			resolver: nodeServiceAddressResolver{
				interfaceAddrs: hostAddresses("10.0.0.4"),
			},
			want: "10.0.0.4:9253",
		},
		{
			name:     "node name is IPv4",
			nodeName: "10.0.0.5",
			resolver: nodeServiceAddressResolver{
				interfaceAddrs: hostAddresses("10.0.0.5"),
			},
			want: "10.0.0.5:9253",
		},
		{
			name:     "node name DNS",
			nodeName: "node.example",
			resolver: nodeServiceAddressResolver{
				interfaceAddrs: hostAddresses("10.0.0.6"),
				lookupIP: func(string) ([]net.IP, error) {
					return []net.IP{net.ParseIP("fd00::6"), net.ParseIP("10.0.0.99"), net.ParseIP("10.0.0.6")}, nil
				},
			},
			want: "10.0.0.6:9253",
		},
		{
			name:     "default route fallback",
			nodeName: "node.example",
			resolver: nodeServiceAddressResolver{
				interfaceAddrs: hostAddresses("10.0.0.7"),
				lookupIP: func(string) ([]net.IP, error) {
					return nil, errors.New("not found")
				},
				resolveBindAddress: func(ip net.IP) (net.IP, error) {
					if ip != nil {
						t.Fatalf("resolveBindAddress() input = %v, want nil", ip)
					}

					return net.ParseIP("10.0.0.7"), nil
				},
			},
			want: "10.0.0.7:9253",
		},
		{
			name:    "configured kubelet IP is not assigned",
			nodeIPs: "10.0.0.8",
			resolver: nodeServiceAddressResolver{
				interfaceAddrs: hostAddresses("10.0.0.7"),
			},
			wantErr: "not assigned",
		},
		{
			name:     "configured kubelet IP has no IPv4",
			nodeIPs:  "fd00::8",
			resolver: nodeServiceAddressResolver{},
			wantErr:  "contains no IPv4",
		},
		{
			name:     "default route is IPv6",
			nodeName: "node.example",
			resolver: nodeServiceAddressResolver{
				interfaceAddrs: hostAddresses(),
				lookupIP:       func(string) ([]net.IP, error) { return nil, nil },
				resolveBindAddress: func(net.IP) (net.IP, error) {
					return net.ParseIP("fd00::9"), nil
				},
			},
			wantErr: "not IPv4",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := test.resolver.resolve(nodeServiceAddressParams{
				nodeIPs:     test.nodeIPs,
				nodeName:    test.nodeName,
				description: "test service",
				port:        LocalDNSMetricsPort,
			})
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("resolveNodeServiceAddress() error = %v, want containing %q", err, test.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("resolveNodeServiceAddress() error = %v", err)
			}

			if got != test.want {
				t.Fatalf("resolveNodeServiceAddress() = %q, want %q", got, test.want)
			}
		})
	}
}
