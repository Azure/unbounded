// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netlink

import (
	"testing"
)

// TestMasqueradeRuleHelpers tests masquerade rule helpers.
func TestMasqueradeRuleHelpers(t *testing.T) {
	mm := &MasqueradeManager{}

	rules := mm.buildDesiredRules("eth0", []string{"10.0.0.0/8", "fd00::/64"})
	if len(rules) != 4 {
		t.Fatalf("expected 4 rules (local + 2 bypass + masq), got %d", len(rules))
	}

	formatted := mm.ruleArgsToListFormat(rules[0])

	parsed := mm.parseRuleFromListFormat(formatted)
	if len(parsed) == 0 {
		t.Fatalf("expected parsed args from formatted rule")
	}

	if parsed[0] != "-m" {
		t.Fatalf("unexpected parsed args: %#v", parsed)
	}

	quoted := `-A UNBOUNDED-MASQUERADE -m comment --comment "hello world" -j RETURN`

	quotedParsed := mm.parseRuleFromListFormat(quoted)
	if len(quotedParsed) == 0 || quotedParsed[3] != "hello world" {
		t.Fatalf("expected quoted comment to parse, got %#v", quotedParsed)
	}

	if got := mm.parseRuleFromListFormat("-A OTHER-CHAIN -j RETURN"); got != nil {
		t.Fatalf("expected non-matching chain to return nil, got %#v", got)
	}
}

// TestCIDRClassificationAndStringSliceEquality tests cidrclassification and string slice equality.
func TestCIDRClassificationAndStringSliceEquality(t *testing.T) {
	v4, v6 := classifyCIDRsByFamily([]string{"10.0.0.0/8", "fd00::/64", "invalid"})
	if len(v4) != 1 || len(v6) != 1 {
		t.Fatalf("unexpected classification v4=%#v v6=%#v", v4, v6)
	}

	if !stringSlicesEqual([]string{"b", "a"}, []string{"a", "b"}) {
		t.Fatalf("expected order-independent equality")
	}

	if stringSlicesEqual([]string{"a"}, []string{"a", "b"}) {
		t.Fatalf("expected different lengths to be unequal")
	}
}

// TestIntSliceEqualityFromHelpers tests int slice equality (previously covered by ECMP helper test).
func TestIntSliceEqualityFromHelpers(t *testing.T) {
	if !intSlicesEqual([]int{1, 2, 3}, []int{1, 2, 3}) {
		t.Fatalf("expected identical int slices to be equal")
	}

	if !intSlicesEqual([]int{1, 2}, []int{2, 1}) {
		t.Fatalf("expected reordered int slices to be equal (order-insensitive)")
	}

	if intSlicesEqual([]int{1, 2}, []int{1, 3}) {
		t.Fatalf("expected different int slices to be unequal")
	}
}
