// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netlink

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"

	"github.com/coreos/go-iptables/iptables"
	"k8s.io/klog/v2"
)

const (
	// forwardChain is the custom chain for tunnel-to-tunnel forwarding rules.
	forwardChain = "UNBOUNDED-FORWARD"
	// forwardComment identifies rules created by this manager.
	forwardComment = "unbounded-net: forward between managed tunnels"
	// legacyForwardComment identifies old per-interface FORWARD ACCEPT rules
	// from previous agent versions that should be cleaned up.
	legacyForwardComment = "unbounded-net: accept tunnel traffic"
	nativeForwardComment = "unbounded-net: forward native cluster traffic"
)

// ForwardManager manages iptables FORWARD rules that restrict forwarded
// traffic to tunnel-to-tunnel paths. Instead of a blanket ACCEPT on each
// source interface, it installs:
//
//   - FORWARD: -i <ifName> -j UNBOUNDED-FORWARD (per source interface)
//   - UNBOUNDED-FORWARD: -o <ifName> -j ACCEPT (per destination interface)
//
// This gives 2*N rules instead of N^2 and ensures only traffic between
// managed tunnel interfaces bypasses KUBE-FORWARD's conntrack checks.
type ForwardManager struct {
	ipt4 *iptables.IPTables
	ipt6 *iptables.IPTables
	mu   sync.Mutex
}

// NewForwardManager creates a ForwardManager, ensures the UNBOUNDED-FORWARD
// chain exists, and cleans up any legacy per-interface FORWARD ACCEPT rules
// left by previous agent versions.
func NewForwardManager() (*ForwardManager, error) {
	ipt4, err := iptables.New()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize IPv4 iptables: %w", err)
	}

	ipt6, err := iptables.NewWithProtocol(iptables.ProtocolIPv6)
	if err != nil {
		klog.Warningf("Failed to initialize IPv6 iptables (IPv6 forwarding rules will be disabled): %v", err)

		ipt6 = nil
	}

	m := &ForwardManager{ipt4: ipt4, ipt6: ipt6}

	if err := m.ensureChain(ipt4, "IPv4"); err != nil {
		return nil, fmt.Errorf("failed to create IPv4 forward chain: %w", err)
	}

	if ipt6 != nil {
		if err := m.ensureChain(ipt6, "IPv6"); err != nil {
			klog.Warningf("Failed to create IPv6 forward chain: %v", err)
		}
	}

	m.cleanupLegacyRules(ipt4, "IPv4")

	if ipt6 != nil {
		m.cleanupLegacyRules(ipt6, "IPv6")
	}

	return m, nil
}

// ensureChain creates the UNBOUNDED-FORWARD chain if it does not exist.
// It does NOT insert a jump rule in FORWARD here; jump rules are per-interface
// and managed by EnsureInterface.
func (m *ForwardManager) ensureChain(ipt *iptables.IPTables, family string) error {
	exists, err := ipt.ChainExists("filter", forwardChain)
	if err != nil {
		return fmt.Errorf("failed to check if chain exists: %w", err)
	}

	if !exists {
		if err := ipt.NewChain("filter", forwardChain); err != nil {
			return fmt.Errorf("failed to create chain: %w", err)
		}

		klog.V(2).Infof("Created %s chain %s in filter table", family, forwardChain)
	}

	return nil
}

// jumpRule returns the FORWARD chain jump rule spec for a source interface.
func jumpRule(ifName string) []string {
	return []string{"-i", ifName, "-m", "comment", "--comment", forwardComment, "-j", forwardChain}
}

// acceptRule returns the UNBOUNDED-FORWARD chain accept rule spec for a destination interface.
func acceptRule(ifName string) []string {
	return []string{"-o", ifName, "-m", "comment", "--comment", forwardComment, "-j", "ACCEPT"}
}

func nativeForwardRule(ifName, prefix string, ingress bool) []string {
	if ingress {
		return []string{"-i", ifName, "-s", prefix, "-m", "comment", "--comment", nativeForwardComment, "-j", forwardChain}
	}

	return []string{"-o", ifName, "-d", prefix, "-m", "comment", "--comment", nativeForwardComment, "-j", "ACCEPT"}
}

// ReconcileNativeInterface trusts only cluster prefixes on a shared physical NIC.
func (m *ForwardManager) ReconcileNativeInterface(ifName string, prefixes []string) error {
	desired := make(map[int]map[string]bool)

	for _, value := range prefixes {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return fmt.Errorf("invalid native forwarding prefix %q: %w", value, err)
		}

		family := 6
		if prefix.Addr().Is4() {
			family = 4
		}

		if desired[family] == nil {
			desired[family] = make(map[string]bool)
		}

		desired[family][prefix.Masked().String()] = true
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, family := range []struct {
		ipt    *iptables.IPTables
		number int
	}{{m.ipt4, 4}, {m.ipt6, 6}} {
		if family.ipt == nil {
			continue
		}

		if err := m.ensureChain(family.ipt, fmt.Sprint(family.number)); err != nil {
			return err
		}

		// Remove broad rules installed by earlier native forwarding versions.
		if err := family.ipt.DeleteIfExists("filter", "FORWARD", jumpRule(ifName)...); err != nil {
			return err
		}

		if err := family.ipt.DeleteIfExists("filter", forwardChain, acceptRule(ifName)...); err != nil {
			return err
		}

		for _, ingress := range []bool{true, false} {
			chain, ifaceFlag, prefixFlag := forwardChain, "-o", "-d"
			if ingress {
				chain, ifaceFlag, prefixFlag = "FORWARD", "-i", "-s"
			}

			rules, err := family.ipt.List("filter", chain)
			if err != nil {
				return err
			}

			for _, rule := range rules {
				if !strings.Contains(rule, nativeForwardComment) {
					continue
				}

				existingName := parseInterfaceFromRule(rule, ifaceFlag)

				prefix := parseInterfaceFromRule(rule, prefixFlag)
				if prefix == "" {
					prefix = "0.0.0.0/0"
					if family.number == 6 {
						prefix = "::/0"
					}
				}

				if existingName == ifName && desired[family.number][prefix] {
					continue
				}

				if err := family.ipt.DeleteIfExists("filter", chain, nativeForwardRule(existingName, prefix, ingress)...); err != nil {
					return err
				}
			}

			var sorted []string
			for prefix := range desired[family.number] {
				sorted = append(sorted, prefix)
			}

			slices.Sort(sorted)

			for _, prefix := range sorted {
				rule := nativeForwardRule(ifName, prefix, ingress)

				exists, err := family.ipt.Exists("filter", chain, rule...)
				if err != nil {
					return err
				}

				if exists {
					continue
				}

				if ingress {
					err = family.ipt.Insert("filter", chain, 1, rule...)
				} else {
					err = family.ipt.Append("filter", chain, rule...)
				}

				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// EnsureInterface adds the jump rule in FORWARD (-i ifName) and the accept
// rule in UNBOUNDED-FORWARD (-o ifName) if they do not already exist. The
// jump rule is inserted at position 1 so it is evaluated before KUBE-FORWARD.
func (m *ForwardManager) EnsureInterface(ifName string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.ensureInterfaceForFamily(m.ipt4, "IPv4", ifName)

	if m.ipt6 != nil {
		m.ensureInterfaceForFamily(m.ipt6, "IPv6", ifName)
	}
}

func (m *ForwardManager) ensureInterfaceForFamily(ipt *iptables.IPTables, family, ifName string) {
	// Ensure the chain still exists (self-healing if someone flushed it).
	if err := m.ensureChain(ipt, family); err != nil {
		klog.Warningf("ForwardManager: failed to ensure %s chain for %s: %v", family, ifName, err)

		return
	}

	// Jump rule in FORWARD: -i ifName -j UNBOUNDED-FORWARD
	jRule := jumpRule(ifName)

	exists, err := ipt.Exists("filter", "FORWARD", jRule...)
	if err != nil {
		klog.Warningf("ForwardManager: failed to check %s FORWARD jump for %s: %v", family, ifName, err)
	} else if !exists {
		if err := ipt.Insert("filter", "FORWARD", 1, jRule...); err != nil {
			klog.Warningf("ForwardManager: failed to insert %s FORWARD jump for %s: %v", family, ifName, err)
		} else {
			klog.V(2).Infof("ForwardManager: added %s FORWARD jump for %s", family, ifName)
		}
	}

	// Accept rule in UNBOUNDED-FORWARD: -o ifName -j ACCEPT
	aRule := acceptRule(ifName)

	exists, err = ipt.Exists("filter", forwardChain, aRule...)
	if err != nil {
		klog.Warningf("ForwardManager: failed to check %s accept rule for %s: %v", family, ifName, err)
	} else if !exists {
		if err := ipt.Append("filter", forwardChain, aRule...); err != nil {
			klog.Warningf("ForwardManager: failed to add %s accept rule for %s: %v", family, ifName, err)
		} else {
			klog.V(2).Infof("ForwardManager: added %s accept rule for -o %s", family, ifName)
		}
	}
}

// RemoveInterface removes the jump rule from FORWARD and the accept rule
// from UNBOUNDED-FORWARD for the specified interface.
func (m *ForwardManager) RemoveInterface(ifName string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.removeInterfaceForFamily(m.ipt4, "IPv4", ifName)

	if m.ipt6 != nil {
		m.removeInterfaceForFamily(m.ipt6, "IPv6", ifName)
	}
}

func (m *ForwardManager) removeInterfaceForFamily(ipt *iptables.IPTables, family, ifName string) {
	jRule := jumpRule(ifName)
	if err := ipt.DeleteIfExists("filter", "FORWARD", jRule...); err != nil {
		klog.V(4).Infof("ForwardManager: failed to remove %s FORWARD jump for %s: %v", family, ifName, err)
	} else {
		klog.V(2).Infof("ForwardManager: removed %s FORWARD jump for %s", family, ifName)
	}

	aRule := acceptRule(ifName)
	if err := ipt.DeleteIfExists("filter", forwardChain, aRule...); err != nil {
		klog.V(4).Infof("ForwardManager: failed to remove %s accept rule for %s: %v", family, ifName, err)
	} else {
		klog.V(2).Infof("ForwardManager: removed %s accept rule for -o %s", family, ifName)
	}
}

// Cleanup removes all rules and the UNBOUNDED-FORWARD chain. Called on
// graceful shutdown.
func (m *ForwardManager) Cleanup() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.cleanupFamily(m.ipt4, "IPv4")

	if m.ipt6 != nil {
		m.cleanupFamily(m.ipt6, "IPv6")
	}

	klog.V(2).Info("ForwardManager: cleaned up forward chain and rules")
}

func (m *ForwardManager) cleanupFamily(ipt *iptables.IPTables, family string) {
	// Remove all jump rules from FORWARD that reference our chain.
	rules, err := ipt.List("filter", "FORWARD")
	if err != nil {
		klog.Warningf("ForwardManager: failed to list %s FORWARD rules for cleanup: %v", family, err)
	} else {
		for _, rule := range rules {
			if !strings.Contains(rule, forwardChain) || (!strings.Contains(rule, forwardComment) && !strings.Contains(rule, nativeForwardComment)) {
				continue
			}

			// Parse the -i interface name from the rule.
			ifName := parseInterfaceFromRule(rule, "-i")
			if ifName != "" {
				jRule := jumpRule(ifName)

				if strings.Contains(rule, nativeForwardComment) {
					prefix := parseInterfaceFromRule(rule, "-s")
					if prefix == "" {
						prefix = "0.0.0.0/0"
						if family == "IPv6" {
							prefix = "::/0"
						}
					}

					jRule = nativeForwardRule(ifName, prefix, true)
				}

				if delErr := ipt.DeleteIfExists("filter", "FORWARD", jRule...); delErr != nil {
					klog.V(4).Infof("ForwardManager: failed to remove %s FORWARD jump for %s during cleanup: %v", family, ifName, delErr)
				}
			}
		}
	}

	// Flush and delete the chain.
	exists, err := ipt.ChainExists("filter", forwardChain)
	if err != nil {
		klog.Warningf("ForwardManager: failed to check if %s chain exists during cleanup: %v", family, err)

		return
	}

	if exists {
		if err := ipt.ClearChain("filter", forwardChain); err != nil {
			klog.Warningf("ForwardManager: failed to flush %s chain during cleanup: %v", family, err)
		}

		if err := ipt.DeleteChain("filter", forwardChain); err != nil {
			klog.Warningf("ForwardManager: failed to delete %s chain during cleanup: %v", family, err)
		}
	}
}

// cleanupLegacyRules removes old-style per-interface FORWARD ACCEPT rules
// from previous agent versions. These rules have the comment
// "unbounded-net: accept tunnel traffic".
func (m *ForwardManager) cleanupLegacyRules(ipt *iptables.IPTables, family string) {
	rules, err := ipt.List("filter", "FORWARD")
	if err != nil {
		klog.V(4).Infof("ForwardManager: failed to list %s FORWARD rules for legacy cleanup: %v", family, err)

		return
	}

	for _, rule := range rules {
		if !strings.Contains(rule, legacyForwardComment) {
			continue
		}

		ifName := parseInterfaceFromRule(rule, "-i")
		if ifName == "" {
			continue
		}

		legacyRule := []string{"-i", ifName, "-j", "ACCEPT", "-m", "comment", "--comment", legacyForwardComment}
		if err := ipt.DeleteIfExists("filter", "FORWARD", legacyRule...); err != nil {
			klog.V(4).Infof("ForwardManager: failed to remove legacy %s FORWARD rule for %s: %v", family, ifName, err)
		} else {
			klog.V(2).Infof("ForwardManager: removed legacy %s FORWARD ACCEPT rule for %s", family, ifName)
		}
	}
}

// parseInterfaceFromRule extracts the interface name following the given flag
// (e.g. "-i" or "-o") from an iptables rule string.
func parseInterfaceFromRule(rule, flag string) string {
	fields := strings.Fields(rule)
	for i, f := range fields {
		if f == flag && i+1 < len(fields) {
			return fields[i+1]
		}
	}

	return ""
}
