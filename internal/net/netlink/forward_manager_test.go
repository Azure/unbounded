// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netlink

import (
	"testing"
)

func TestParseInterfaceFromRule(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rule string
		flag string
		want string
	}{
		{
			name: "input interface",
			rule: "-A FORWARD -i geneve0 -m comment --comment \"unbounded-net: forward between managed tunnels\" -j UNBOUNDED-FORWARD",
			flag: "-i",
			want: "geneve0",
		},
		{
			name: "output interface",
			rule: "-A UNBOUNDED-FORWARD -o wg0 -m comment --comment \"unbounded-net: forward between managed tunnels\" -j ACCEPT",
			flag: "-o",
			want: "wg0",
		},
		{
			name: "flag not present",
			rule: "-A FORWARD -j ACCEPT",
			flag: "-i",
			want: "",
		},
		{
			name: "flag at end of rule",
			rule: "-A FORWARD -i",
			flag: "-i",
			want: "",
		},
		{
			name: "empty rule",
			rule: "",
			flag: "-i",
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := parseInterfaceFromRule(tc.rule, tc.flag)
			if got != tc.want {
				t.Errorf("parseInterfaceFromRule(%q, %q) = %q, want %q", tc.rule, tc.flag, got, tc.want)
			}
		})
	}
}

func TestJumpRuleSpec(t *testing.T) {
	t.Parallel()

	rule := jumpRule("geneve0")
	expected := []string{"-i", "geneve0", "-m", "comment", "--comment", forwardComment, "-j", forwardChain}

	if len(rule) != len(expected) {
		t.Fatalf("jumpRule length = %d, want %d", len(rule), len(expected))
	}

	for i, v := range rule {
		if v != expected[i] {
			t.Errorf("jumpRule[%d] = %q, want %q", i, v, expected[i])
		}
	}
}

func TestAcceptRuleSpec(t *testing.T) {
	t.Parallel()

	rule := acceptRule("wg51820")
	expected := []string{"-o", "wg51820", "-m", "comment", "--comment", forwardComment, "-j", "ACCEPT"}

	if len(rule) != len(expected) {
		t.Fatalf("acceptRule length = %d, want %d", len(rule), len(expected))
	}

	for i, v := range rule {
		if v != expected[i] {
			t.Errorf("acceptRule[%d] = %q, want %q", i, v, expected[i])
		}
	}
}
