// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"errors"
	"net"
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

func TestParseLocalDNSUpstreams(t *testing.T) {
	t.Parallel()

	got, err := parseLocalDNSUpstreams([]byte(`search example.test
nameserver 10.0.0.5
nameserver 169.254.10.10
nameserver 10.0.0.4
nameserver 10.0.0.5
nameserver 2001:db8::1
`), netip.MustParseAddr("169.254.10.10"), netip.MustParseAddr("169.254.10.11"))
	if err != nil {
		t.Fatalf("parseLocalDNSUpstreams() error = %v", err)
	}

	want := []netip.Addr{netip.MustParseAddr("10.0.0.4"), netip.MustParseAddr("10.0.0.5")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseLocalDNSUpstreams() = %v, want %v", got, want)
	}
}

func TestParseLocalDNSUpstreamsRejectsEmpty(t *testing.T) {
	t.Parallel()

	_, err := parseLocalDNSUpstreams([]byte("nameserver 169.254.10.10\nnameserver 127.0.0.53\n"), netip.MustParseAddr("169.254.10.10"))
	if err == nil {
		t.Fatal("parseLocalDNSUpstreams() error = nil")
	}
}

func TestDiscoverLocalDNSUpstreamsFromSystemdResolved(t *testing.T) {
	t.Parallel()

	files := map[string][]byte{
		hostResolvConfPath:            []byte("search example.test\nnameserver 127.0.0.53\n"),
		systemdResolvedResolvConfPath: []byte("nameserver 10.0.0.5\nnameserver 10.0.0.4\n"),
	}
	deps := localDNSResolverDeps{
		readFile: func(path string) ([]byte, error) { return files[path], nil },
		resolvedDomains: func() (string, error) {
			return "Global:\nLink 2 (eth0): ~.\n", nil
		},
	}

	original, got, err := discoverLocalDNSUpstreams(deps)
	if err != nil {
		t.Fatalf("discoverLocalDNSUpstreams() error = %v", err)
	}

	if string(original) != string(files[hostResolvConfPath]) {
		t.Fatalf("original resolver = %q, want %q", original, files[hostResolvConfPath])
	}

	want := []netip.Addr{netip.MustParseAddr("10.0.0.4"), netip.MustParseAddr("10.0.0.5")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("discoverLocalDNSUpstreams() = %v, want %v", got, want)
	}
}

func TestDiscoverLocalDNSUpstreamsRejectsUnsupportedStub(t *testing.T) {
	t.Parallel()

	deps := localDNSResolverDeps{
		readFile: func(string) ([]byte, error) { return []byte("nameserver 127.0.0.1\n"), nil },
		resolvedDomains: func() (string, error) {
			t.Fatal("resolvedDomains() must not be called")

			return "", nil
		},
	}

	_, _, err := discoverLocalDNSUpstreams(deps)
	if err == nil || !strings.Contains(err.Error(), "unsupported local caching stub") {
		t.Fatalf("discoverLocalDNSUpstreams() error = %v", err)
	}
}

func TestDiscoverLocalDNSUpstreamsRejectsSplitDNS(t *testing.T) {
	t.Parallel()

	deps := localDNSResolverDeps{
		readFile: func(string) ([]byte, error) { return []byte("nameserver 127.0.0.53\n"), nil },
		resolvedDomains: func() (string, error) {
			return "Global:\nLink 2 (eth0): corp.example ~.\n", nil
		},
	}

	_, _, err := discoverLocalDNSUpstreams(deps)
	if err == nil || !strings.Contains(err.Error(), "split-DNS") {
		t.Fatalf("discoverLocalDNSUpstreams() error = %v", err)
	}
}

func TestLocalDNSMetricsAddress(t *testing.T) {
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
		name       string
		configured string
		nodeIPs    string
		nodeName   string
		deps       localDNSMetricsDeps
		want       string
		wantErr    string
	}{
		{
			name:       "explicit address",
			configured: "10.0.0.8:9353",
			deps: localDNSMetricsDeps{
				interfaceAddrs: func() ([]net.Addr, error) { panic("must not be called") },
				lookupIP:       func(string) ([]net.IP, error) { panic("must not be called") },
				resolveBindAddress: func(net.IP) (net.IP, error) {
					panic("must not be called")
				},
			},
			want: "10.0.0.8:9353",
		},
		{
			name:     "configured kubelet IPv4",
			nodeIPs:  "fd00::4,10.0.0.4",
			nodeName: "node.example",
			deps: localDNSMetricsDeps{
				interfaceAddrs: hostAddresses("10.0.0.4"),
			},
			want: "10.0.0.4:9253",
		},
		{
			name:     "node name is IPv4",
			nodeName: "10.0.0.5",
			deps: localDNSMetricsDeps{
				interfaceAddrs: hostAddresses("10.0.0.5"),
			},
			want: "10.0.0.5:9253",
		},
		{
			name:     "node name DNS",
			nodeName: "node.example",
			deps: localDNSMetricsDeps{
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
			deps: localDNSMetricsDeps{
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
			deps: localDNSMetricsDeps{
				interfaceAddrs: hostAddresses("10.0.0.7"),
			},
			wantErr: "not assigned",
		},
		{
			name:    "configured kubelet IP has no IPv4",
			nodeIPs: "fd00::8",
			deps:    localDNSMetricsDeps{},
			wantErr: "contains no IPv4",
		},
		{
			name:     "default route is IPv6",
			nodeName: "node.example",
			deps: localDNSMetricsDeps{
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

			got, err := localDNSMetricsAddress(test.configured, test.nodeIPs, test.nodeName, test.deps)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("localDNSMetricsAddress() error = %v, want containing %q", err, test.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("localDNSMetricsAddress() error = %v", err)
			}

			if got != test.want {
				t.Fatalf("localDNSMetricsAddress() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRenderLocalDNSCorefile(t *testing.T) {
	t.Parallel()

	got, err := renderLocalDNSCorefile(defaultLocalDNSCorefileTemplate, LocalDNSCorefileTemplateData{
		NodeListenerIP:        "169.254.10.10",
		ClusterListenerIP:     "169.254.10.11",
		NodeUpstreamIPs:       []string{"10.0.0.4", "10.0.0.5"},
		NodeUpstreamIPsJoined: "10.0.0.4 10.0.0.5",
		ClusterDNSServiceIP:   "10.1.0.10",
		MetricsAddress:        "10.0.0.7:9253",
	})
	if err != nil {
		t.Fatalf("renderLocalDNSCorefile() error = %v", err)
	}

	for _, want := range []string{
		"bind 169.254.10.10",
		"bind 169.254.10.11",
		"forward . 10.0.0.4 10.0.0.5",
		"forward . 10.1.0.10",
		"prometheus 10.0.0.7:9253",
	} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("rendered Corefile missing %q:\n%s", want, got)
		}
	}

	if count := strings.Count(string(got), "prometheus 10.0.0.7:9253"); count != 1 {
		t.Fatalf("rendered Corefile Prometheus directive count = %d, want 1:\n%s", count, got)
	}
}
