// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netlink

import (
	"reflect"
	"testing"
)

// TestMasqueradeRuleNormalizationQuotedComments verifies that the locally rendered
// form of a rule compares equal to the quoted form printed by iptables -S.
func TestMasqueradeRuleNormalizationQuotedComments(t *testing.T) {
	mm := &MasqueradeManager{}

	desiredArgs := []string{
		"-m", "addrtype", "--dst-type", "LOCAL",
		"-m", "comment", "--comment", commentPrefix + ": skip local destinations",
		"-j", "RETURN",
	}
	desired := mm.ruleArgsToListFormat(desiredArgs)

	// Real iptables -S output; the comment value contains spaces, so it is quoted.
	live := `-A UNBOUNDED-MASQUERADE -m addrtype --dst-type LOCAL -m comment --comment "unbounded-net: skip local destinations" -j RETURN`

	if normalizeRuleString(live) != normalizeRuleString(desired) {
		t.Fatalf("quoted -S rule should match desired rule: live=%q desired=%q",
			normalizeRuleString(live), normalizeRuleString(desired))
	}

	if parsedLive := mm.parseRuleFromListFormat(live); !reflect.DeepEqual(parsedLive, desiredArgs) {
		t.Fatalf("parsed argv should match desired args: parsed=%#v desired=%#v", parsedLive, desiredArgs)
	}

	// A genuinely different rule must still be seen as different.
	different := `-A UNBOUNDED-MASQUERADE -d 10.0.0.0/8 -m comment --comment "unbounded-net: skip nonMasqueradeCIDRs" -j RETURN`

	if normalizeRuleString(live) == normalizeRuleString(different) {
		t.Fatalf("different rule should not match: live=%q different=%q",
			normalizeRuleString(live), normalizeRuleString(different))
	}

	if normalizeRuleString(different) != normalizeRuleString(mm.ruleArgsToListFormat([]string{
		"-d", "10.0.0.0/8",
		"-m", "comment", "--comment", commentPrefix + ": skip nonMasqueradeCIDRs",
		"-j", "RETURN",
	})) {
		t.Fatalf("quoted nonMasqueradeCIDRs rule should match desired rule")
	}
}
