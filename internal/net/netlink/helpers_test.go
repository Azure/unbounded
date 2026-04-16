// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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

// TestGatewayPolicyPrimitiveHelpers tests gateway policy primitive helpers.
func TestGatewayPolicyPrimitiveHelpers(t *testing.T) {
	m := &GatewayPolicyManager{gatewayTableBase: 1000}

	mark, err := m.gatewayPolicyIngressMark(1001)
	if err != nil || mark != 1 {
		t.Fatalf("unexpected ingress mark result mark=%d err=%v", mark, err)
	}

	if _, err := m.gatewayPolicyIngressMark(1000); err == nil {
		t.Fatalf("expected invalid table number error")
	}

	if _, err := m.gatewayPolicyIngressMark(1000 + int(kubeProxyMasqMarkBit)); err == nil {
		t.Fatalf("expected ingress mark range error")
	}

	priority, err := m.gatewayPolicyRulePriority(1005)
	if err != nil || priority != 5 {
		t.Fatalf("unexpected rule priority=%d err=%v", priority, err)
	}

	if _, err := m.gatewayPolicyRulePriority(999); err == nil {
		t.Fatalf("expected invalid priority error")
	}

	if table, ok := m.gatewayPolicyTableFromIngressMark(1); !ok || table != 1001 {
		t.Fatalf("unexpected table from mark result: table=%d ok=%v", table, ok)
	}

	if _, ok := m.gatewayPolicyTableFromIngressMark(int(kubeProxyMasqMarkBit)); ok {
		t.Fatalf("expected kube-proxy mark-bit overlap to be rejected")
	}

	legacy := m.gatewayPolicyLegacyMarks(1001)
	if _, ok := legacy[1]; !ok {
		t.Fatalf("expected ingress mark in legacy set")
	}

	if _, ok := legacy[1001]; !ok {
		t.Fatalf("expected direct legacy mark in set")
	}

	if mark, ok := parseMarkValue("0x11/0xffffffff"); !ok || mark != 17 {
		t.Fatalf("unexpected parseMarkValue hex result mark=%d ok=%v", mark, ok)
	}

	if _, ok := parseMarkValue("-1"); ok {
		t.Fatalf("expected negative mark parsing to fail")
	}

	line := "-A UNBOUNDED-GW-PRE -i wg0 -m comment --comment unbounded-net: gateway policy set ingress connmark -j CONNMARK --set-xmark 0x1/0xffffffff"

	iface, tableNum, ok := m.parseGatewayIngressRule(line)
	if !ok || iface != "wg0" || tableNum != 1001 {
		t.Fatalf("unexpected parseGatewayIngressRule result iface=%s table=%d ok=%v", iface, tableNum, ok)
	}

	if _, _, ok := m.parseGatewayIngressRule("-A UNBOUNDED-GW-PRE -j ACCEPT"); ok {
		t.Fatalf("expected non-gateway-policy line to be ignored")
	}

	if _, _, ok := m.parseGatewayIngressRule("-A UNBOUNDED-GW-PRE -i wg0 -m comment --comment unbounded-net: gateway policy set ingress connmark -j CONNMARK --set-mark not-a-mark"); ok {
		t.Fatalf("expected invalid mark value to be ignored")
	}
}

// TestGatewayPolicyFormattingHelpers tests gateway policy formatting helpers.
func TestGatewayPolicyFormattingHelpers(t *testing.T) {
	expected := map[string]int{"wg10": 110, "wg2": 102}

	ifaces := sortedExpectedIfaces(expected)
	if len(ifaces) != 2 || ifaces[0] != "wg10" || ifaces[1] != "wg2" {
		t.Fatalf("unexpected sorted ifaces: %#v", ifaces)
	}

	formatted := formatExpectedGatewayPolicyTables(expected)
	if formatted != "wg10->110,wg2->102" {
		t.Fatalf("unexpected formatted gateway policy tables: %s", formatted)
	}

	if formatExpectedGatewayPolicyTables(map[string]int{}) != "<none>" {
		t.Fatalf("expected empty format marker <none>")
	}
}

// TestUnifiedRouteManagerPeerHealthTracking tests peer health state tracking.
func TestUnifiedRouteManagerPeerHealthTracking(t *testing.T) {
	m := NewUnifiedRouteManager("test-iface", 0)

	// Initially no peers tracked.
	if len(m.peerHealthy) != 0 {
		t.Fatalf("expected empty peer health map, got %d entries", len(m.peerHealthy))
	}

	// Ensure nexthops registers peers.
	m.ensureNexthop(DesiredNexthop{PeerID: "peer-a", LinkIndex: 1})
	m.peerHealthy["peer-a"] = true
	m.ensureNexthop(DesiredNexthop{PeerID: "peer-b", LinkIndex: 2})
	m.peerHealthy["peer-b"] = true

	if !m.peerHealthy["peer-a"] || !m.peerHealthy["peer-b"] {
		t.Fatalf("expected both peers healthy")
	}

	// Mark peer-b unhealthy.
	m.peerHealthy["peer-b"] = false

	dr := DesiredRoute{
		Nexthops: []DesiredNexthop{
			{PeerID: "peer-a", LinkIndex: 1},
			{PeerID: "peer-b", LinkIndex: 2},
		},
	}

	active := m.activeNexthops(dr)
	if len(active) != 1 || active[0].PeerID != "peer-a" {
		t.Fatalf("expected only peer-a active, got %v", active)
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
