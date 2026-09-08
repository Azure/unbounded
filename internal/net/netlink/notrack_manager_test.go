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
