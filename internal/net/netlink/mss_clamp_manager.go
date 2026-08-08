// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netlink

import (
	"fmt"
	"strconv"
	"sync"

	"github.com/coreos/go-iptables/iptables"
	"k8s.io/klog/v2"
)

const (
	// mssClampChain is the custom chain for MSS clamping rules.
	mssClampChain = "UNBOUNDED-MSS-CLAMP"
	// mssClampComment identifies rules created by this manager.
	mssClampComment = "unbounded-net: clamp TCP MSS to fabric MTU"
	// legacyMSSClampComment identifies the previous WireGuard-only rules.
	legacyMSSClampComment = "unbounded-net: clamp TCP MSS to PMTU on WireGuard interfaces"
)

// MSSClampManager installs iptables mangle rules that clamp TCP MSS on SYN
// packets forwarded through the node. The explicit MSS is derived from the
// lowest calculated MTU for the unbounded fabric.
type MSSClampManager struct {
	ipt4 iptablesClient
	ipt6 iptablesClient
	mu   sync.Mutex
	// legacyWGPrefix is retained to remove rules created by older versions.
	legacyWGPrefix string
	// installedMTU is the fabric MTU represented by the current rules.
	installedMTU int
}

type iptablesClient interface {
	Append(table, chain string, rulespec ...string) error
	ChainExists(table, chain string) (bool, error)
	ClearChain(table, chain string) error
	DeleteChain(table, chain string) error
	DeleteIfExists(table, chain string, rulespec ...string) error
	Exists(table, chain string, rulespec ...string) (bool, error)
	NewChain(table, chain string) error
}

// NewMSSClampManager creates a manager and ensures the mangle chain exists.
// wgPrefix is the configured WireGuard interface prefix (typically
// cfg.WireGuardInterfacePrefix); it must be non-empty.
func NewMSSClampManager(wgPrefix string) (*MSSClampManager, error) {
	ipt4, err := iptables.New()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize IPv4 iptables: %w", err)
	}

	ipt6, err := iptables.NewWithProtocol(iptables.ProtocolIPv6)
	if err != nil {
		klog.Warningf("Failed to initialize IPv6 iptables (IPv6 MSS clamping will be disabled): %v", err)

		return newMSSClampManager(wgPrefix, ipt4, nil)
	}

	return newMSSClampManager(wgPrefix, ipt4, ipt6)
}

func newMSSClampManager(wgPrefix string, ipt4, ipt6 iptablesClient) (*MSSClampManager, error) {
	m := &MSSClampManager{ipt4: ipt4, ipt6: ipt6, legacyWGPrefix: wgPrefix}

	if err := m.ensureChain(ipt4, "IPv4"); err != nil {
		return nil, fmt.Errorf("failed to create IPv4 MSS clamp chain: %w", err)
	}

	if err := ipt4.ClearChain("mangle", mssClampChain); err != nil {
		return nil, fmt.Errorf("failed to clear stale IPv4 MSS clamp rules: %w", err)
	}

	if ipt6 != nil {
		if err := m.ensureChain(ipt6, "IPv6"); err != nil {
			klog.Warningf("Failed to create IPv6 MSS clamp chain: %v", err)

			ipt6 = nil
			m.ipt6 = nil
		} else if err := ipt6.ClearChain("mangle", mssClampChain); err != nil {
			klog.Warningf("Failed to clear stale IPv6 MSS clamp rules: %v", err)

			if detachErr := m.detachChain(ipt6, "IPv6"); detachErr != nil {
				return nil, fmt.Errorf(
					"failed to disable IPv6 MSS clamping after clearing stale rules: %w",
					detachErr,
				)
			}

			m.ipt6 = nil
		}
	}

	return m, nil
}

// ensureChain creates the custom chain and adds a jump from FORWARD.
func (m *MSSClampManager) ensureChain(ipt iptablesClient, family string) error {
	exists, err := ipt.ChainExists("mangle", mssClampChain)
	if err != nil {
		return fmt.Errorf("failed to check if chain exists: %w", err)
	}

	if !exists {
		if err := ipt.NewChain("mangle", mssClampChain); err != nil {
			return fmt.Errorf("failed to create chain: %w", err)
		}

		klog.V(2).Infof("Created %s chain %s in mangle table", family, mssClampChain)
	}

	legacyJumpRule := []string{"-m", "comment", "--comment", legacyMSSClampComment, "-j", mssClampChain}
	if err := ipt.DeleteIfExists("mangle", "FORWARD", legacyJumpRule...); err != nil {
		return fmt.Errorf("failed to remove legacy jump rule: %w", err)
	}

	jumpRule := []string{"-m", "comment", "--comment", mssClampComment, "-j", mssClampChain}

	exists, err = ipt.Exists("mangle", "FORWARD", jumpRule...)
	if err != nil {
		return fmt.Errorf("failed to check if jump rule exists: %w", err)
	}

	if !exists {
		if err := ipt.Append("mangle", "FORWARD", jumpRule...); err != nil {
			return fmt.Errorf("failed to add jump rule: %w", err)
		}

		klog.V(2).Infof("Added %s jump from FORWARD to %s", family, mssClampChain)
	}

	return nil
}

func (m *MSSClampManager) detachChain(ipt iptablesClient, family string) error {
	jumpRule := []string{"-m", "comment", "--comment", mssClampComment, "-j", mssClampChain}
	if err := ipt.DeleteIfExists("mangle", "FORWARD", jumpRule...); err != nil {
		return fmt.Errorf("failed to remove %s MSS clamp jump rule: %w", family, err)
	}

	legacyJumpRule := []string{"-m", "comment", "--comment", legacyMSSClampComment, "-j", mssClampChain}
	if err := ipt.DeleteIfExists("mangle", "FORWARD", legacyJumpRule...); err != nil {
		return fmt.Errorf("failed to remove legacy %s MSS clamp jump rule: %w", family, err)
	}

	return nil
}

// EnsureRules reconciles TCPMSS rules to the current fabric MTU.
func (m *MSSClampManager) EnsureRules(fabricMTU int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if fabricMTU <= 0 {
		return nil
	}

	if fabricMTU <= 60 {
		return fmt.Errorf("fabric MTU %d is too small for TCP MSS clamping", fabricMTU)
	}

	if m.installedMTU == fabricMTU {
		return nil
	}

	if err := m.ensureRulesForFamily(m.ipt4, "IPv4", fabricMTU, m.installedMTU, false); err != nil {
		return fmt.Errorf("failed to ensure IPv4 MSS clamp rules: %w", err)
	}

	if m.ipt6 != nil {
		if err := m.ensureRulesForFamily(m.ipt6, "IPv6", fabricMTU, m.installedMTU, true); err != nil {
			return fmt.Errorf("failed to ensure IPv6 MSS clamp rules: %w", err)
		}
	}

	m.installedMTU = fabricMTU

	klog.V(2).Infof("MSS clamp rules reconciled for fabric MTU %d", fabricMTU)

	return nil
}

func (m *MSSClampManager) ensureRulesForFamily(
	ipt iptablesClient,
	family string,
	fabricMTU, previousMTU int,
	ipv6 bool,
) error {
	if ipt == nil {
		return nil
	}

	rule := mssClampRule(fabricMTU, ipv6)

	exists, err := ipt.Exists("mangle", mssClampChain, rule...)
	if err != nil {
		return fmt.Errorf("failed to check %s MSS clamp rule: %w", family, err)
	}

	if !exists {
		if err := ipt.Append("mangle", mssClampChain, rule...); err != nil {
			return fmt.Errorf("failed to add %s MSS clamp rule: %w", family, err)
		}
	}

	if previousMTU > 0 && previousMTU != fabricMTU {
		if err := ipt.DeleteIfExists("mangle", mssClampChain, mssClampRule(previousMTU, ipv6)...); err != nil {
			return fmt.Errorf("failed to remove previous %s MSS clamp rule: %w", family, err)
		}
	}

	if m.legacyWGPrefix != "" {
		if err := ipt.DeleteIfExists("mangle", mssClampChain, legacyMSSClampRule(m.legacyWGPrefix)...); err != nil {
			return fmt.Errorf("failed to remove legacy %s MSS clamp rule: %w", family, err)
		}
	}

	return nil
}

func mssClampRule(fabricMTU int, ipv6 bool) []string {
	headerSize := 40
	if ipv6 {
		headerSize = 60
	}

	mss := fabricMTU - headerSize

	return []string{
		"-p", "tcp", "--tcp-flags", "SYN,RST", "SYN",
		"-m", "tcpmss", "--mss", strconv.Itoa(mss+1) + ":65535",
		"-m", "comment", "--comment", mssClampComment,
		"-j", "TCPMSS", "--set-mss", strconv.Itoa(mss),
	}
}

func legacyMSSClampRule(wgPrefix string) []string {
	return []string{
		"-o", wgPrefix + "+",
		"-p", "tcp", "--tcp-flags", "SYN,RST", "SYN",
		"-m", "comment", "--comment", legacyMSSClampComment,
		"-j", "TCPMSS", "--clamp-mss-to-pmtu",
	}
}

// Cleanup removes all MSS clamping rules installed by this manager.
func (m *MSSClampManager) Cleanup() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var errs []error
	if err := m.cleanupFamily(m.ipt4, "IPv4"); err != nil {
		errs = append(errs, err)
	}

	if m.ipt6 != nil {
		if err := m.cleanupFamily(m.ipt6, "IPv6"); err != nil {
			errs = append(errs, err)
		}
	}

	m.installedMTU = 0

	if len(errs) > 0 {
		return fmt.Errorf("MSS clamp cleanup errors: %v", errs)
	}

	klog.V(2).Info("Cleaned up MSS clamp rules")

	return nil
}

// cleanupFamily removes the jump and chain for one address family.
func (m *MSSClampManager) cleanupFamily(ipt iptablesClient, family string) error {
	if ipt == nil {
		return nil
	}

	jumpRule := []string{"-m", "comment", "--comment", mssClampComment, "-j", mssClampChain}
	if err := ipt.DeleteIfExists("mangle", "FORWARD", jumpRule...); err != nil {
		klog.Warningf("Failed to remove %s MSS clamp jump rule: %v", family, err)
	}

	legacyJumpRule := []string{"-m", "comment", "--comment", legacyMSSClampComment, "-j", mssClampChain}
	if err := ipt.DeleteIfExists("mangle", "FORWARD", legacyJumpRule...); err != nil {
		klog.Warningf("Failed to remove legacy %s MSS clamp jump rule: %v", family, err)
	}

	exists, err := ipt.ChainExists("mangle", mssClampChain)
	if err != nil {
		return fmt.Errorf("failed to check if %s MSS clamp chain exists: %w", family, err)
	}

	if exists {
		if err := ipt.ClearChain("mangle", mssClampChain); err != nil {
			klog.Warningf("Failed to flush %s MSS clamp chain: %v", family, err)
		}

		if err := ipt.DeleteChain("mangle", mssClampChain); err != nil {
			klog.Warningf("Failed to delete %s MSS clamp chain: %v", family, err)
		}
	}

	return nil
}
