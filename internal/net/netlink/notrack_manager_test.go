// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netlink

import (
	"testing"
)

func TestSplitByFamily(t *testing.T) {
	t.Parallel()

	v4, v6 := splitByFamily([]string{
		"10.244.0.0/16",
		"fd00::/48",
		"192.168.1.0/24",
		"2001:db8::/32",
	})

	if len(v4) != 2 {
		t.Fatalf("expected 2 IPv4 CIDRs, got %d: %v", len(v4), v4)
	}

	if len(v6) != 2 {
		t.Fatalf("expected 2 IPv6 CIDRs, got %d: %v", len(v6), v6)
	}

	if v4[0] != "10.244.0.0/16" || v4[1] != "192.168.1.0/24" {
		t.Errorf("unexpected IPv4 CIDRs: %v", v4)
	}

	if v6[0] != "fd00::/48" || v6[1] != "2001:db8::/32" {
		t.Errorf("unexpected IPv6 CIDRs: %v", v6)
	}
}

func TestSplitByFamilyInvalidCIDR(t *testing.T) {
	t.Parallel()

	v4, v6 := splitByFamily([]string{"not-a-cidr", "10.0.0.0/8"})

	if len(v4) != 1 || v4[0] != "10.0.0.0/8" {
		t.Errorf("expected [10.0.0.0/8], got %v", v4)
	}

	if len(v6) != 0 {
		t.Errorf("expected no IPv6, got %v", v6)
	}
}

func TestSplitByFamilyEmpty(t *testing.T) {
	t.Parallel()

	v4, v6 := splitByFamily(nil)

	if v4 != nil || v6 != nil {
		t.Errorf("expected nil slices, got v4=%v v6=%v", v4, v6)
	}
}

func TestDedupeAndSort(t *testing.T) {
	t.Parallel()

	result := dedupeAndSort([]string{"c", "a", "b", "a", "c"})
	expected := []string{"a", "b", "c"}

	if len(result) != len(expected) {
		t.Fatalf("expected %d items, got %d: %v", len(expected), len(result), result)
	}

	for i, v := range result {
		if v != expected[i] {
			t.Errorf("result[%d] = %q, want %q", i, v, expected[i])
		}
	}
}

func TestDedupeAndSortEmpty(t *testing.T) {
	t.Parallel()

	result := dedupeAndSort(nil)
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}

	result = dedupeAndSort([]string{})
	if result != nil {
		t.Errorf("expected nil for empty slice, got %v", result)
	}
}

func TestNotrackJumpRuleSpec(t *testing.T) {
	t.Parallel()

	rule := notrackJumpRule("wg0")
	expected := []string{"-i", "wg0", "-m", "comment", "--comment", notrackComment, "-j", notrackChain}

	if len(rule) != len(expected) {
		t.Fatalf("notrackJumpRule length = %d, want %d", len(rule), len(expected))
	}

	for i, v := range rule {
		if v != expected[i] {
			t.Errorf("notrackJumpRule[%d] = %q, want %q", i, v, expected[i])
		}
	}
}

// TestReconcileFamilyTracksRoutedSources covers a non-gateway node in the
// gateway's site reaching a remote site through the gateway. A request from
// the gateway's routedCidrs arrives on the tunnel with a local source and its
// reply comes back with a local destination; both must stay tracked so the
// reply is not INVALID. Node IPs are not routed through the gateway, so their
// replies may bypass it and they must stay untracked by source.
func TestReconcileFamilyTracksRoutedSources(t *testing.T) {
	t.Parallel()

	const (
		podCIDR    = "10.244.1.0/24"
		podSubnet  = "10.0.2.0/23"
		nodeSubnet = "10.0.0.0/27"
		remotePods = "192.168.20.0/24"
	)

	ipt := &fakeIPTables{}
	m := &NotrackManager{}

	err := m.reconcileFamily(ipt, "IPv4", true,
		[]string{podCIDR},
		[]string{podSubnet, nodeSubnet},
		[]string{podSubnet},
		[]string{remotePods},
	)
	if err != nil {
		t.Fatalf("reconcileFamily: %v", err)
	}

	index := func(match, cidr, target string) int {
		for i, rule := range ipt.appended {
			if len(rule) > 1 && rule[0] == match && rule[1] == cidr && rule[len(rule)-1] == target {
				return i
			}
		}

		return -1
	}

	notrack := index("-d", remotePods, "--notrack")
	if notrack < 0 {
		t.Fatalf("missing supernet NOTRACK rule: %v", ipt.appended)
	}

	for _, want := range []struct{ match, cidr string }{
		{"-d", podSubnet},
		{"-d", nodeSubnet},
		{"-s", podSubnet},
	} {
		i := index(want.match, want.cidr, "RETURN")
		if i < 0 {
			t.Fatalf("missing %s %s RETURN rule: %v", want.match, want.cidr, ipt.appended)
		}

		if i > notrack {
			t.Fatalf("%s %s RETURN at %d comes after NOTRACK at %d", want.match, want.cidr, i, notrack)
		}
	}

	for _, cidr := range []string{nodeSubnet, podCIDR} {
		if index("-s", cidr, "RETURN") >= 0 {
			t.Fatalf("%s must not be tracked by source: %v", cidr, ipt.appended)
		}
	}
}
