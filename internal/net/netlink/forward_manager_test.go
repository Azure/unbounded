// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netlink

import (
	"errors"
	"testing"
)

type fakeForwardIPTables struct {
	*fakeIPTables
	chainErr error
	listed   int
}

func (f *fakeForwardIPTables) ChainExists(_, _ string) (bool, error) {
	return true, f.chainErr
}

func (f *fakeForwardIPTables) List(_, _ string) ([]string, error) {
	f.listed++
	return nil, nil
}

func (f *fakeForwardIPTables) Insert(_, _ string, _ int, _ ...string) error {
	return nil
}

func TestNativeForwardingWithoutIPv6(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name         string
		prefixes     []string
		failedFamily int
		missing      bool
		wantError    bool
	}{
		{"IPv4 only", []string{"10.244.0.0/16"}, 6, false, false},
		{"withdrawal", nil, 6, false, false},
		{"required IPv6", []string{"fd44::/64"}, 6, false, true},
		{"required IPv4", []string{"10.244.0.0/16"}, 4, false, true},
		{"missing required IPv6", []string{"fd44::/64"}, 6, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ipv4 := &fakeForwardIPTables{fakeIPTables: &fakeIPTables{}}
			ipv6 := &fakeForwardIPTables{fakeIPTables: &fakeIPTables{}}

			failure := errors.New("filter table unavailable")
			if tc.failedFamily == 4 {
				ipv4.chainErr = failure
			} else {
				ipv6.chainErr = failure
			}

			manager := &ForwardManager{ipt4: ipv4}
			if !tc.missing {
				manager.ipt6 = ipv6
			}

			err := manager.ReconcileNativeInterface("eth0", tc.prefixes)
			if tc.wantError {
				if err == nil || (!tc.missing && !errors.Is(err, failure)) {
					t.Fatalf("required forwarding error was hidden: %v", err)
				}
			} else if err != nil || ipv4.listed == 0 {
				t.Fatalf("unused IPv6 family blocked IPv4 forwarding: %v, listed=%d", err, ipv4.listed)
			}
		})
	}
}

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
