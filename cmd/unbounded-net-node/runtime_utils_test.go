// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"slices"
	"testing"
)

// TestGatewayAndHealthIPHelpers tests GatewayAndHealthIPHelpers.
func TestGatewayAndHealthIPHelpers(t *testing.T) {
	if got := getGatewayIPFromCIDR("10.10.0.0/24"); got == nil || got.String() != "10.10.0.1" {
		t.Fatalf("unexpected IPv4 gateway IP: %v", got)
	}

	if got := getGatewayIPFromCIDR("fd00::/64"); got == nil || got.String() != "fd00::1" {
		t.Fatalf("unexpected IPv6 gateway IP: %v", got)
	}

	if got := getGatewayIPFromCIDR("bad-cidr"); got != nil {
		t.Fatalf("expected nil gateway IP for invalid CIDR, got %v", got)
	}

	if got := getHealthIPFromPodCIDRs([]string{"bad", "10.20.0.0/24"}); got != "10.20.0.1" {
		t.Fatalf("unexpected health IPv4: %q", got)
	}

	if got := getHealthIPFromPodCIDRs([]string{"fd12::/64"}); got != "fd12::1" {
		t.Fatalf("unexpected health IPv6: %q", got)
	}

	if got := getHealthIPFromPodCIDRs([]string{"bad"}); got != "" {
		t.Fatalf("expected empty health IP for invalid CIDRs, got %q", got)
	}

	healthIPs := getHealthIPsFromPodCIDRs([]string{"10.20.0.0/24", "fd12::/64", "10.20.0.0/24", "bad"})

	wantHealthIPs := []string{"10.20.0.1", "fd12::1"}
	if !slices.Equal(healthIPs, wantHealthIPs) {
		t.Fatalf("unexpected health IP candidates: got %#v want %#v", healthIPs, wantHealthIPs)
	}
}

// TestHostCIDRHelpers tests HostCIDRHelpers.
func TestHostCIDRHelpers(t *testing.T) {
	if got := ipToHostCIDR("10.0.0.5"); got != "10.0.0.5/32" {
		t.Fatalf("unexpected IPv4 host CIDR: %q", got)
	}

	if got := ipToHostCIDR("fd00::5"); got != "fd00::5/128" {
		t.Fatalf("unexpected IPv6 host CIDR: %q", got)
	}

	if got := ipToHostCIDR("not-an-ip"); got != "" {
		t.Fatalf("expected empty host CIDR for invalid IP, got %q", got)
	}

	got := ipsToHostCIDRs([]string{"10.0.0.5", "invalid", "fd00::5"})

	want := []string{"10.0.0.5/32", "fd00::5/128"}
	if !slices.Equal(got, want) {
		t.Fatalf("unexpected host CIDRs: got %#v want %#v", got, want)
	}
}

// TestNormalizeCIDRAndLocalSets tests NormalizeCIDRAndLocalSets.
func TestNormalizeCIDRAndLocalSets(t *testing.T) {
	if got := normalizeCIDR("10.124.1.0/24"); got != "10.124.1.0/24" {
		t.Fatalf("unexpected normalized IPv4 CIDR: %q", got)
	}

	if got := normalizeCIDR("fdde::/48"); got != "fdde::/48" {
		t.Fatalf("unexpected normalized IPv6 CIDR: %q", got)
	}

	if got := normalizeCIDR("not-a-cidr"); got != "" {
		t.Fatalf("expected empty normalized CIDR for invalid value, got %q", got)
	}

	localSet := buildNormalizedCIDRSet([]string{"10.124.1.0/24", "fdde::/48", "bad"})
	if _, ok := localSet["10.124.1.0/24"]; !ok {
		t.Fatalf("expected local set to include IPv4 CIDR")
	}

	if _, ok := localSet["fdde::/48"]; !ok {
		t.Fatalf("expected local set to include IPv6 CIDR")
	}

	if _, ok := localSet["bad"]; ok {
		t.Fatalf("did not expect invalid CIDR to be included in local set")
	}

	localHostSet := buildLocalGatewayHostCIDRSet([]string{"10.124.1.0/24", "fdde::/48", "bad"})
	if _, ok := localHostSet["10.124.1.1/32"]; !ok {
		t.Fatalf("expected local gateway host set to include IPv4 host route")
	}

	if _, ok := localHostSet["fdde::1/128"]; !ok {
		t.Fatalf("expected local gateway host set to include IPv6 host route")
	}

	if len(localHostSet) != 2 {
		t.Fatalf("expected exactly two host routes in local gateway host set, got %d", len(localHostSet))
	}
}

// TestPruneCoveredAllowedCIDRs tests PruneCoveredAllowedCIDRs.
func TestPruneCoveredAllowedCIDRs(t *testing.T) {
	t.Run("removes subnet covered by supernet", func(t *testing.T) {
		got := pruneCoveredAllowedCIDRs([]string{"100.125.0.0/16", "100.125.5.0/24"})

		want := []string{"100.125.0.0/16"}
		if !slices.Equal(got, want) {
			t.Fatalf("unexpected pruned allowed CIDRs: got %#v want %#v", got, want)
		}
	})

	t.Run("keeps longest-prefix peers for different branches", func(t *testing.T) {
		got := pruneCoveredAllowedCIDRs([]string{"100.125.5.0/24", "100.126.5.0/24"})

		want := []string{"100.125.5.0/24", "100.126.5.0/24"}
		if !slices.Equal(got, want) {
			t.Fatalf("unexpected allowed CIDRs for disjoint prefixes: got %#v want %#v", got, want)
		}
	})

	t.Run("keeps different IP families", func(t *testing.T) {
		got := pruneCoveredAllowedCIDRs([]string{"100.125.0.0/16", "fdde::/48", "fdde::1:0:0:0/64"})

		want := []string{"100.125.0.0/16", "fdde::/48"}
		if !slices.Equal(got, want) {
			t.Fatalf("unexpected family-aware pruning result: got %#v want %#v", got, want)
		}
	})

	t.Run("preserves invalid entries after valid CIDRs", func(t *testing.T) {
		got := pruneCoveredAllowedCIDRs([]string{"100.125.0.0/16", "bad-cidr", "100.125.5.0/24"})

		want := []string{"100.125.0.0/16", "bad-cidr"}
		if !slices.Equal(got, want) {
			t.Fatalf("unexpected pruning with invalid entries: got %#v want %#v", got, want)
		}
	})
}
