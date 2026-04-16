// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netlink

import (
	"fmt"
	"sync"

	"github.com/coreos/go-iptables/iptables"
	"k8s.io/klog/v2"
)

const (
	// mssClampChain is the custom chain for MSS clamping rules.
	mssClampChain = "UNBOUNDED-MSS-CLAMP"
	// mssClampComment identifies rules created by this manager.
	mssClampComment = "unbounded-net: clamp TCP MSS to PMTU on WireGuard interfaces"
)

// MSSClampManager installs iptables mangle rules that clamp TCP MSS on SYN
// packets transiting WireGuard interfaces. This prevents pods (whose network
// namespace sees a 1500-byte veth MTU) from advertising an MSS too large for
// the WireGuard tunnel, which would cause silent drops of large TLS responses
// at gateway forwarding hops.
type MSSClampManager struct {
	ipt4 *iptables.IPTables
	ipt6 *iptables.IPTables
	mu   sync.Mutex
	// installed tracks whether rules have been applied.
	installed bool
}

// NewMSSClampManager creates a manager and ensures the mangle chain exists.
func NewMSSClampManager() (*MSSClampManager, error) {
	ipt4, err := iptables.New()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize IPv4 iptables: %w", err)
	}

	ipt6, err := iptables.NewWithProtocol(iptables.ProtocolIPv6)
	if err != nil {
		klog.Warningf("Failed to initialize IPv6 iptables (IPv6 MSS clamping will be disabled): %v", err)

		ipt6 = nil
	}

	m := &MSSClampManager{ipt4: ipt4, ipt6: ipt6}

	if err := m.ensureChain(ipt4, "IPv4"); err != nil {
		return nil, fmt.Errorf("failed to create IPv4 MSS clamp chain: %w", err)
	}

	if ipt6 != nil {
		if err := m.ensureChain(ipt6, "IPv6"); err != nil {
			klog.Warningf("Failed to create IPv6 MSS clamp chain: %v", err)
		}
	}

	return m, nil
}

// ensureChain creates the custom chain and adds a jump from FORWARD.
func (m *MSSClampManager) ensureChain(ipt *iptables.IPTables, family string) error {
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

// EnsureRules installs the TCPMSS clamp rules if not already present.
// Safe to call on every reconciliation loop.
func (m *MSSClampManager) EnsureRules() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.installed {
		return nil
	}

	if err := m.ensureRulesForFamily(m.ipt4, "IPv4"); err != nil {
		return fmt.Errorf("failed to ensure IPv4 MSS clamp rules: %w", err)
	}

	if m.ipt6 != nil {
		if err := m.ensureRulesForFamily(m.ipt6, "IPv6"); err != nil {
			klog.Warningf("Failed to ensure IPv6 MSS clamp rules: %v", err)
		}
	}

	m.installed = true

	klog.V(2).Info("MSS clamp rules installed on WireGuard interfaces")

	return nil
}

// ensureRulesForFamily adds the TCPMSS --clamp-mss-to-pmtu rule for a single
// address family, matching SYN packets leaving via any wg+ interface.
func (m *MSSClampManager) ensureRulesForFamily(ipt *iptables.IPTables, family string) error {
	if ipt == nil {
		return nil
	}

	rule := []string{
		"-o", "wg+",
		"-p", "tcp", "--tcp-flags", "SYN,RST", "SYN",
		"-m", "comment", "--comment", mssClampComment,
		"-j", "TCPMSS", "--clamp-mss-to-pmtu",
	}

	exists, err := ipt.Exists("mangle", mssClampChain, rule...)
	if err != nil {
		return fmt.Errorf("failed to check %s MSS clamp rule: %w", family, err)
	}

	if !exists {
		if err := ipt.Append("mangle", mssClampChain, rule...); err != nil {
			return fmt.Errorf("failed to add %s MSS clamp rule: %w", family, err)
		}

		klog.V(2).Infof("Added %s MSS clamp rule for wg+ interfaces", family)
	}

	return nil
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

	m.installed = false

	if len(errs) > 0 {
		return fmt.Errorf("MSS clamp cleanup errors: %v", errs)
	}

	klog.V(2).Info("Cleaned up MSS clamp rules")

	return nil
}

// cleanupFamily removes the jump and chain for one address family.
func (m *MSSClampManager) cleanupFamily(ipt *iptables.IPTables, family string) error {
	if ipt == nil {
		return nil
	}

	jumpRule := []string{"-m", "comment", "--comment", mssClampComment, "-j", mssClampChain}
	if err := ipt.DeleteIfExists("mangle", "FORWARD", jumpRule...); err != nil {
		klog.Warningf("Failed to remove %s MSS clamp jump rule: %v", family, err)
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
