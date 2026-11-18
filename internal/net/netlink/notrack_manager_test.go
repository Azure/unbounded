// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netlink

import (
	"errors"
	"fmt"
	"testing"
)

type fakeNativeNotrackIPTables struct {
	*fakeForwardIPTables
	stage    string
	failure  error
	deleted  int
	inserted int
}

func (f *fakeNativeNotrackIPTables) List(_, _ string) ([]string, error) {
	if f.stage == "list" {
		return nil, f.failure
	}

	return []string{fmt.Sprintf("-A PREROUTING -i oldeth0 -m comment --comment %q -j %s", notrackComment, notrackChain)}, nil
}

func (f *fakeNativeNotrackIPTables) DeleteIfExists(_, _ string, _ ...string) error {
	f.deleted++
	if f.stage == "delete" {
		return f.failure
	}

	return nil
}

func (f *fakeNativeNotrackIPTables) Exists(_, _ string, _ ...string) (bool, error) {
	if f.stage == "exists" {
		return false, f.failure
	}

	return false, nil
}

func (f *fakeNativeNotrackIPTables) Insert(_, _ string, _ int, _ ...string) error {
	f.inserted++
	if f.stage == "insert" {
		return f.failure
	}

	return nil
}

func TestNativeNotrackReconciliationFailures(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		stage    string
		deleted  int
		inserted int
	}{
		{"list", 0, 0}, {"delete", 1, 0}, {"exists", 1, 0}, {"insert", 1, 1}, {"", 1, 1},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			failure := errors.New("iptables failure")
			ipv4 := &fakeNativeNotrackIPTables{
				fakeForwardIPTables: &fakeForwardIPTables{fakeIPTables: &fakeIPTables{}}, stage: tc.stage, failure: failure,
			}
			ipv6 := &fakeNativeNotrackIPTables{stage: "list", failure: failure}
			manager := &NotrackManager{ipt4: ipv4, ipt6: ipv6, ctSupported4: true}

			err := manager.ReconcileNativeInterface("eth1", true)
			if (tc.stage == "" && err != nil) || (tc.stage != "" && !errors.Is(err, failure)) {
				t.Fatalf("required failure hidden or unsupported IPv6 blocked IPv4: %v", err)
			}

			if ipv4.deleted != tc.deleted || ipv4.inserted != tc.inserted {
				t.Fatalf("unexpected downstream operations after %q: deleted=%d inserted=%d", tc.stage, ipv4.deleted, ipv4.inserted)
			}
		})
	}
}

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
