// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netlink

import (
	"fmt"
	"testing"

	vishnetlink "github.com/vishvananda/netlink"
)

// TestDeleteIPRulesByMarkSetDeletesOnlyMatchingEntries tests delete iprules by mark set deletes only matching entries.
func TestDeleteIPRulesByMarkSetDeletesOnlyMatchingEntries(t *testing.T) {
	origList := netlinkRuleList
	origDel := netlinkRuleDel

	defer func() {
		netlinkRuleList = origList
		netlinkRuleDel = origDel
	}()

	rules := []vishnetlink.Rule{
		{Table: 1001, Mark: 1},
		{Table: 1001, Mark: 2},
		{Table: 1002, Mark: 1},
	}

	deleted := make([]vishnetlink.Rule, 0)
	netlinkRuleList = func(family int) ([]vishnetlink.Rule, error) {
		if family != vishnetlink.FAMILY_V4 {
			t.Fatalf("unexpected family: %d", family)
		}

		return rules, nil
	}
	netlinkRuleDel = func(rule *vishnetlink.Rule) error {
		deleted = append(deleted, *rule)
		return nil
	}

	m := &GatewayPolicyManager{}

	err := m.deleteIPRulesByMarkSet(1001, map[uint32]struct{}{1: {}}, vishnetlink.FAMILY_V4, "IPv4")
	if err != nil {
		t.Fatalf("deleteIPRulesByMarkSet returned error: %v", err)
	}

	if len(deleted) != 1 {
		t.Fatalf("expected 1 deleted rule, got %d", len(deleted))
	}

	if deleted[0].Table != 1001 || deleted[0].Mark != 1 {
		t.Fatalf("unexpected deleted rule: %+v", deleted[0])
	}
}

// TestDeleteIPRulesByMarkSetIgnoresIPv6ListError tests delete iprules by mark set ignores ipv6 list error.
func TestDeleteIPRulesByMarkSetIgnoresIPv6ListError(t *testing.T) {
	origList := netlinkRuleList

	defer func() {
		netlinkRuleList = origList
	}()

	netlinkRuleList = func(family int) ([]vishnetlink.Rule, error) {
		return nil, fmt.Errorf("list failed")
	}

	m := &GatewayPolicyManager{}

	err := m.deleteIPRulesByMarkSet(1001, map[uint32]struct{}{1: {}}, vishnetlink.FAMILY_V6, "IPv6")
	if err != nil {
		t.Fatalf("expected IPv6 list error to be ignored, got: %v", err)
	}
}

// TestEnsureIPRuleDeletesStaleRulesAndAddsNormalizedRule tests ensure iprule deletes stale rules and adds normalized rule.
func TestEnsureIPRuleDeletesStaleRulesAndAddsNormalizedRule(t *testing.T) {
	origList := netlinkRuleList
	origAdd := netlinkRuleAdd
	origDel := netlinkRuleDel

	defer func() {
		netlinkRuleList = origList
		netlinkRuleAdd = origAdd
		netlinkRuleDel = origDel
	}()

	fullMask := uint32(0xffffffff)
	call := 0
	netlinkRuleList = func(family int) ([]vishnetlink.Rule, error) {
		call++
		switch call {
		case 1:
			return []vishnetlink.Rule{{Table: 1001, Mark: 1, Priority: 99, Mask: &fullMask}}, nil
		case 2:
			return []vishnetlink.Rule{{Table: 1001, Mark: 1, Priority: 99, Mask: &fullMask}}, nil
		case 3:
			return []vishnetlink.Rule{{Table: 1001, Mark: 1001}}, nil
		case 4:
			return []vishnetlink.Rule{{Table: 1001, Mark: 1001}}, nil
		default:
			return nil, nil
		}
	}

	added := make([]vishnetlink.Rule, 0)
	deleted := make([]vishnetlink.Rule, 0)
	netlinkRuleAdd = func(rule *vishnetlink.Rule) error {
		added = append(added, *rule)
		return nil
	}
	netlinkRuleDel = func(rule *vishnetlink.Rule) error {
		deleted = append(deleted, *rule)
		return nil
	}

	m := &GatewayPolicyManager{gatewayTableBase: 1000}
	if err := m.ensureIPRule(1001); err != nil {
		t.Fatalf("ensureIPRule returned error: %v", err)
	}

	if len(added) != 2 {
		t.Fatalf("expected 2 added rules (IPv4+IPv6), got %d", len(added))
	}

	for _, rule := range added {
		if rule.Mark != 1 || rule.Table != 1001 || rule.Priority != 1 {
			t.Fatalf("unexpected added rule: %+v", rule)
		}
	}

	if len(deleted) != 4 {
		t.Fatalf("expected 4 deleted rules (2 stale + 2 legacy), got %d", len(deleted))
	}
}

// TestEnsureIPRuleReturnsErrorWhenIPv4RuleListFails tests ensure iprule returns error when ipv4 rule list fails.
func TestEnsureIPRuleReturnsErrorWhenIPv4RuleListFails(t *testing.T) {
	origList := netlinkRuleList

	defer func() {
		netlinkRuleList = origList
	}()

	netlinkRuleList = func(family int) ([]vishnetlink.Rule, error) {
		if family == vishnetlink.FAMILY_V4 {
			return nil, fmt.Errorf("v4 list failed")
		}

		return nil, nil
	}

	m := &GatewayPolicyManager{gatewayTableBase: 1000}
	if err := m.ensureIPRule(1001); err == nil {
		t.Fatalf("expected ensureIPRule to fail on IPv4 rule list error")
	}
}

// TestCleanupStaleIPRulesLockedDeletesUnexpectedOwnedMarks tests cleanup stale iprules locked deletes unexpected owned marks.
func TestCleanupStaleIPRulesLockedDeletesUnexpectedOwnedMarks(t *testing.T) {
	origList := netlinkRuleList
	origDel := netlinkRuleDel

	defer func() {
		netlinkRuleList = origList
		netlinkRuleDel = origDel
	}()

	netlinkRuleList = func(family int) ([]vishnetlink.Rule, error) {
		return []vishnetlink.Rule{
			{Table: 1001, Mark: 1},    // expected keep
			{Table: 1002, Mark: 1002}, // stale legacy mark should be removed
			{Table: 2000, Mark: 9999}, // unrelated mark should be ignored
		}, nil
	}

	deleted := make([]vishnetlink.Rule, 0)
	netlinkRuleDel = func(rule *vishnetlink.Rule) error {
		deleted = append(deleted, *rule)
		return nil
	}

	m := &GatewayPolicyManager{gatewayTableBase: 1000}
	if err := m.cleanupStaleIPRulesLocked(map[string]int{"wg1001": 1001}); err != nil {
		t.Fatalf("cleanupStaleIPRulesLocked returned error: %v", err)
	}

	if len(deleted) == 0 {
		t.Fatalf("expected stale legacy rules to be deleted")
	}

	foundStaleDeletion := false

	for _, rule := range deleted {
		if rule.Table == 1002 && rule.Mark == 1002 {
			foundStaleDeletion = true
		}
	}

	if !foundStaleDeletion {
		t.Fatalf("expected stale rule table=1002 mark=1002 to be deleted, got %#v", deleted)
	}
}

// TestCleanupStaleIPRulesLockedReturnsErrorOnIPv4ListFailure tests cleanup stale iprules locked returns error on ipv4 list failure.
func TestCleanupStaleIPRulesLockedReturnsErrorOnIPv4ListFailure(t *testing.T) {
	origList := netlinkRuleList

	defer func() {
		netlinkRuleList = origList
	}()

	netlinkRuleList = func(family int) ([]vishnetlink.Rule, error) {
		if family == vishnetlink.FAMILY_V4 {
			return nil, fmt.Errorf("v4 list failed")
		}

		return nil, nil
	}

	m := &GatewayPolicyManager{gatewayTableBase: 1000}
	if err := m.cleanupStaleIPRulesLocked(map[string]int{"wg1001": 1001}); err == nil {
		t.Fatalf("expected cleanupStaleIPRulesLocked to fail on IPv4 rule list error")
	}
}

// TestCleanupStaleIPRulesLockedKeepsExpectedIngressMark tests cleanup stale iprules locked keeps expected ingress mark.
func TestCleanupStaleIPRulesLockedKeepsExpectedIngressMark(t *testing.T) {
	origList := netlinkRuleList
	origDel := netlinkRuleDel

	defer func() {
		netlinkRuleList = origList
		netlinkRuleDel = origDel
	}()

	netlinkRuleList = func(family int) ([]vishnetlink.Rule, error) {
		return []vishnetlink.Rule{{Table: 1001, Mark: 1}}, nil
	}

	deletions := 0
	netlinkRuleDel = func(rule *vishnetlink.Rule) error {
		deletions++
		return nil
	}

	m := &GatewayPolicyManager{gatewayTableBase: 1000}
	if err := m.cleanupStaleIPRulesLocked(map[string]int{"wg1001": 1001}); err != nil {
		t.Fatalf("cleanupStaleIPRulesLocked returned error: %v", err)
	}

	if deletions != 0 {
		t.Fatalf("expected expected ingress mark rule to be kept, got deletions=%d", deletions)
	}
}
