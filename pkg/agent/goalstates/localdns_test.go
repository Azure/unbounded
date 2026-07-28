// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
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

	_, err := parseLocalDNSUpstreams([]byte("nameserver 169.254.10.10\n"), netip.MustParseAddr("169.254.10.10"))
	if err == nil {
		t.Fatal("parseLocalDNSUpstreams() error = nil")
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
}
