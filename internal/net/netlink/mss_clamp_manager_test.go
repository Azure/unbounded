// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netlink

import (
	"errors"
	"slices"
	"testing"
)

type fakeIPTables struct {
	clearChainErr      error
	deleteJumpErr      error
	deletedCurrentJump bool
	deletedLegacyJump  bool
}

func (f *fakeIPTables) Append(_, _ string, _ ...string) error {
	return nil
}

func (f *fakeIPTables) ChainExists(_, _ string) (bool, error) {
	return true, nil
}

func (f *fakeIPTables) ClearChain(_, _ string) error {
	return f.clearChainErr
}

func (f *fakeIPTables) DeleteChain(_, _ string) error {
	return nil
}

func (f *fakeIPTables) DeleteIfExists(_, _ string, rulespec ...string) error {
	if slices.Contains(rulespec, mssClampComment) {
		f.deletedCurrentJump = true

		return f.deleteJumpErr
	}

	if slices.Contains(rulespec, legacyMSSClampComment) {
		f.deletedLegacyJump = true
	}

	return nil
}

func (f *fakeIPTables) Exists(_, _ string, _ ...string) (bool, error) {
	return true, nil
}

func (f *fakeIPTables) NewChain(_, _ string) error {
	return nil
}

func TestMSSClampRule(t *testing.T) {
	tests := []struct {
		name      string
		fabricMTU int
		ipv6      bool
		wantMSS   string
		wantRange string
	}{
		{name: "IPv4", fabricMTU: 1280, wantMSS: "1240", wantRange: "1241:65535"},
		{name: "IPv6", fabricMTU: 1280, ipv6: true, wantMSS: "1220", wantRange: "1221:65535"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule := mssClampRule(tt.fabricMTU, tt.ipv6)

			if !slices.Contains(rule, tt.wantMSS) {
				t.Fatalf("rule %v does not set MSS %s", rule, tt.wantMSS)
			}

			if !slices.Contains(rule, tt.wantRange) {
				t.Fatalf("rule %v does not match MSS range %s", rule, tt.wantRange)
			}

			if slices.Contains(rule, "-i") || slices.Contains(rule, "-o") {
				t.Fatalf("rule %v should apply across all forwarded interfaces", rule)
			}
		})
	}
}

func TestMSSClampManagerIgnoresUnknownMTU(t *testing.T) {
	manager := &MSSClampManager{}

	if err := manager.EnsureRules(0); err != nil {
		t.Fatalf("EnsureRules(0) returned error: %v", err)
	}
}

func TestNewMSSClampManagerDetachesIPv6JumpWhenClearFails(t *testing.T) {
	ipt4 := &fakeIPTables{}
	ipt6 := &fakeIPTables{clearChainErr: errors.New("clear failed")}

	manager, err := newMSSClampManager("wg", ipt4, ipt6)
	if err != nil {
		t.Fatalf("newMSSClampManager() returned error: %v", err)
	}

	if manager.ipt6 != nil {
		t.Fatal("newMSSClampManager() left IPv6 reconciliation enabled")
	}

	if !ipt6.deletedCurrentJump || !ipt6.deletedLegacyJump {
		t.Fatal("newMSSClampManager() did not detach IPv6 MSS clamp jumps")
	}
}

func TestNewMSSClampManagerFailsIfIPv6JumpCannotBeDetached(t *testing.T) {
	ipt4 := &fakeIPTables{}
	ipt6 := &fakeIPTables{
		clearChainErr: errors.New("clear failed"),
		deleteJumpErr: errors.New("delete failed"),
	}

	if _, err := newMSSClampManager("wg", ipt4, ipt6); err == nil {
		t.Fatal("newMSSClampManager() returned nil error when IPv6 jump remained active")
	}
}
