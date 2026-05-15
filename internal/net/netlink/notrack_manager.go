// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netlink

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"

	"github.com/coreos/go-iptables/iptables"
	"k8s.io/klog/v2"
)

const (
	// notrackChain is the custom chain in the raw table for NOTRACK rules.
	notrackChain = "UNBOUNDED-NOTRACK"
	// notrackComment identifies rules created by this manager.
	notrackComment = "unbounded-net: skip conntrack for tunnel transit"
)

// NotrackManager installs iptables raw-table rules on gateway nodes that
// skip connection tracking for transit tunnel traffic. This avoids
// unnecessary conntrack overhead and conntrack table exhaustion for packets
// that are simply forwarded between tunnel interfaces.
//
// The UNBOUNDED-NOTRACK chain is structured as:
//
//	-m addrtype --dst-type LOCAL -j RETURN   (keep conntrack for node itself)
//	-d <podCIDR> -j RETURN                   (keep conntrack for local pods)
//	-d <supernet> -j CT --notrack            (skip conntrack for transit)
//	(implicit RETURN)                         (anything else: normal conntrack)
//
// Jump rules in raw/PREROUTING are per-interface, matching the ForwardManager
// pattern.
type NotrackManager struct {
	ipt4 *iptables.IPTables
	ipt6 *iptables.IPTables
	mu   sync.Mutex

	// ctSupported tracks whether the CT target is available.
	ctSupported4 bool
	ctSupported6 bool

	// Track current CIDR sets to avoid unnecessary rebuilds.
	currentPodCIDRs    string
	currentReturnCIDRs string
	currentSupernets   string
}

// NewNotrackManager creates a NotrackManager with IPv4/IPv6 handles.
// It does NOT install any rules; call EnsureInterface and ReconcileCIDRs
// to install rules on gateway nodes.
func NewNotrackManager() (*NotrackManager, error) {
	ipt4, err := iptables.New()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize IPv4 iptables: %w", err)
	}

	ipt6, err := iptables.NewWithProtocol(iptables.ProtocolIPv6)
	if err != nil {
		klog.Warningf("Failed to initialize IPv6 iptables (IPv6 notrack rules will be disabled): %v", err)

		ipt6 = nil
	}

	m := &NotrackManager{
		ipt4:         ipt4,
		ipt6:         ipt6,
		ctSupported4: detectCTSupport(ipt4, "IPv4"),
		ctSupported6: ipt6 != nil && detectCTSupport(ipt6, "IPv6"),
	}

	if !m.ctSupported4 {
		klog.Warningf("NotrackManager: CT target not supported for IPv4, notrack rules will be disabled")
	}

	return m, nil
}

// detectCTSupport checks whether the CT target is available by attempting
// to create and immediately delete a test rule.
func detectCTSupport(ipt *iptables.IPTables, family string) bool {
	// Ensure the chain exists for the probe.
	_ = ipt.NewChain("raw", notrackChain) //nolint:errcheck // best-effort; may already exist

	testRule := []string{"-d", "192.0.2.0/32", "-j", "CT", "--notrack"}
	if family == "IPv6" {
		testRule = []string{"-d", "2001:db8::/128", "-j", "CT", "--notrack"}
	}

	if err := ipt.Append("raw", notrackChain, testRule...); err != nil {
		klog.V(4).Infof("NotrackManager: CT target probe failed for %s: %v", family, err)

		// Clean up probe chain if we created it.
		_ = ipt.ClearChain("raw", notrackChain)  //nolint:errcheck // best-effort cleanup
		_ = ipt.DeleteChain("raw", notrackChain) //nolint:errcheck // best-effort cleanup

		return false
	}

	// Clean up test rule.
	_ = ipt.Delete("raw", notrackChain, testRule...) //nolint:errcheck // best-effort cleanup
	// Don't delete the chain - we'll use it.

	return true
}

// notrackJumpRule returns the PREROUTING jump rule spec for a source interface.
func notrackJumpRule(ifName string) []string {
	return []string{"-i", ifName, "-m", "comment", "--comment", notrackComment, "-j", notrackChain}
}

// EnsureInterface adds a raw/PREROUTING jump rule for the given interface
// so that traffic arriving on it enters the UNBOUNDED-NOTRACK chain.
// The jump rule is inserted at position 1.
func (m *NotrackManager) EnsureInterface(ifName string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.ensureInterfaceForFamily(m.ipt4, "IPv4", m.ctSupported4, ifName)

	if m.ipt6 != nil {
		m.ensureInterfaceForFamily(m.ipt6, "IPv6", m.ctSupported6, ifName)
	}
}

func (m *NotrackManager) ensureInterfaceForFamily(ipt *iptables.IPTables, family string, ctSupported bool, ifName string) {
	if !ctSupported {
		return
	}

	// Ensure chain exists (self-healing).
	if err := m.ensureChain(ipt, family); err != nil {
		klog.Warningf("NotrackManager: failed to ensure %s chain for %s: %v", family, ifName, err)

		return
	}

	jRule := notrackJumpRule(ifName)

	exists, err := ipt.Exists("raw", "PREROUTING", jRule...)
	if err != nil {
		klog.Warningf("NotrackManager: failed to check %s PREROUTING jump for %s: %v", family, ifName, err)
	} else if !exists {
		if err := ipt.Insert("raw", "PREROUTING", 1, jRule...); err != nil {
			klog.Warningf("NotrackManager: failed to insert %s PREROUTING jump for %s: %v", family, ifName, err)
		} else {
			klog.V(2).Infof("NotrackManager: added %s PREROUTING jump for %s", family, ifName)
		}
	}
}

// RemoveInterface removes the raw/PREROUTING jump rule for the given interface.
func (m *NotrackManager) RemoveInterface(ifName string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.removeInterfaceForFamily(m.ipt4, "IPv4", ifName)

	if m.ipt6 != nil {
		m.removeInterfaceForFamily(m.ipt6, "IPv6", ifName)
	}
}

func (m *NotrackManager) removeInterfaceForFamily(ipt *iptables.IPTables, family, ifName string) {
	jRule := notrackJumpRule(ifName)
	if err := ipt.DeleteIfExists("raw", "PREROUTING", jRule...); err != nil {
		klog.V(4).Infof("NotrackManager: failed to remove %s PREROUTING jump for %s: %v", family, ifName, err)
	} else {
		klog.V(2).Infof("NotrackManager: removed %s PREROUTING jump for %s", family, ifName)
	}
}

// ReconcileCIDRs rebuilds the UNBOUNDED-NOTRACK chain contents if any of
// podCIDRs, returnCIDRs, or supernets have changed since the last call.
// It installs (in order):
//   - addrtype LOCAL RETURN (always first)
//   - per-podCIDR RETURN rules (this node's own pod CIDRs)
//   - per-returnCIDR RETURN rules (e.g. the gateway's own site NodeCidr,
//     where transit packets must remain conntrack-tracked so MASQUERADE
//     can SNAT overlay sources to the underlay-facing eth0 address)
//   - per-supernet CT --notrack rules
//
// CIDRs are automatically sorted into IPv4 and IPv6 buckets.
func (m *NotrackManager) ReconcileCIDRs(podCIDRs, returnCIDRs, supernets []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Deduplicate and sort for stable comparison.
	podCIDRs = dedupeAndSort(podCIDRs)
	returnCIDRs = dedupeAndSort(returnCIDRs)
	supernets = dedupeAndSort(supernets)

	podKey := strings.Join(podCIDRs, ",")
	returnKey := strings.Join(returnCIDRs, ",")
	superKey := strings.Join(supernets, ",")

	if podKey == m.currentPodCIDRs && returnKey == m.currentReturnCIDRs && superKey == m.currentSupernets {
		return nil
	}

	klog.V(2).Infof("NotrackManager: CIDRs changed, rebuilding chain (podCIDRs=%d, returnCIDRs=%d, supernets=%d)", len(podCIDRs), len(returnCIDRs), len(supernets))

	v4Pods, v6Pods := splitByFamily(podCIDRs)
	v4Returns, v6Returns := splitByFamily(returnCIDRs)
	v4Supers, v6Supers := splitByFamily(supernets)

	if err := m.reconcileFamily(m.ipt4, "IPv4", m.ctSupported4, v4Pods, v4Returns, v4Supers); err != nil {
		return fmt.Errorf("failed to reconcile IPv4 notrack rules: %w", err)
	}

	if m.ipt6 != nil {
		if err := m.reconcileFamily(m.ipt6, "IPv6", m.ctSupported6, v6Pods, v6Returns, v6Supers); err != nil {
			klog.Warningf("NotrackManager: failed to reconcile IPv6 notrack rules: %v", err)
		}
	}

	m.currentPodCIDRs = podKey
	m.currentReturnCIDRs = returnKey
	m.currentSupernets = superKey

	return nil
}

func (m *NotrackManager) reconcileFamily(ipt *iptables.IPTables, family string, ctSupported bool, podCIDRs, returnCIDRs, supernets []string) error {
	if !ctSupported {
		return nil
	}

	if err := m.ensureChain(ipt, family); err != nil {
		return err
	}

	// Flush and rebuild.
	if err := ipt.ClearChain("raw", notrackChain); err != nil {
		return fmt.Errorf("failed to flush %s notrack chain: %w", family, err)
	}

	// Rule 1: RETURN for traffic destined to local addresses.
	localRule := []string{
		"-m", "addrtype", "--dst-type", "LOCAL",
		"-m", "comment", "--comment", notrackComment, "-j", "RETURN",
	}
	if err := ipt.Append("raw", notrackChain, localRule...); err != nil {
		return fmt.Errorf("failed to add %s addrtype LOCAL rule: %w", family, err)
	}

	// RETURN for each local podCIDR.
	for _, cidr := range podCIDRs {
		rule := []string{
			"-d", cidr,
			"-m", "comment", "--comment", notrackComment, "-j", "RETURN",
		}
		if err := ipt.Append("raw", notrackChain, rule...); err != nil {
			return fmt.Errorf("failed to add %s podCIDR RETURN for %s: %w", family, cidr, err)
		}
	}

	// RETURN for each caller-supplied returnCIDR. These are typically the
	// gateway's own site NodeCidr: packets destined here exit via eth0
	// after MASQUERADE and so must keep conntrack tracking enabled.
	for _, cidr := range returnCIDRs {
		rule := []string{
			"-d", cidr,
			"-m", "comment", "--comment", notrackComment, "-j", "RETURN",
		}
		if err := ipt.Append("raw", notrackChain, rule...); err != nil {
			return fmt.Errorf("failed to add %s returnCIDR RETURN for %s: %w", family, cidr, err)
		}
	}

	// CT --notrack for each supernet.
	for _, cidr := range supernets {
		rule := []string{
			"-d", cidr,
			"-m", "comment", "--comment", notrackComment, "-j", "CT", "--notrack",
		}
		if err := ipt.Append("raw", notrackChain, rule...); err != nil {
			return fmt.Errorf("failed to add %s notrack rule for %s: %w", family, cidr, err)
		}
	}

	klog.V(2).Infof("NotrackManager: rebuilt %s chain (%d podCIDR RETURNs, %d returnCIDR RETURNs, %d supernet NOTRACKs)",
		family, len(podCIDRs), len(returnCIDRs), len(supernets))

	return nil
}

// ensureChain creates the UNBOUNDED-NOTRACK chain if it does not exist.
func (m *NotrackManager) ensureChain(ipt *iptables.IPTables, family string) error {
	exists, err := ipt.ChainExists("raw", notrackChain)
	if err != nil {
		return fmt.Errorf("failed to check if %s chain exists: %w", family, err)
	}

	if !exists {
		if err := ipt.NewChain("raw", notrackChain); err != nil {
			return fmt.Errorf("failed to create %s chain: %w", family, err)
		}

		klog.V(2).Infof("NotrackManager: created %s chain %s in raw table", family, notrackChain)
	}

	return nil
}

// Cleanup removes all PREROUTING jump rules, flushes and deletes the
// UNBOUNDED-NOTRACK chain. Safe to call even if rules were never installed.
func (m *NotrackManager) Cleanup() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.cleanupFamily(m.ipt4, "IPv4")

	if m.ipt6 != nil {
		m.cleanupFamily(m.ipt6, "IPv6")
	}

	m.currentPodCIDRs = ""
	m.currentReturnCIDRs = ""
	m.currentSupernets = ""

	klog.V(2).Info("NotrackManager: cleaned up notrack chain and rules")
}

func (m *NotrackManager) cleanupFamily(ipt *iptables.IPTables, family string) {
	// Remove all PREROUTING jump rules referencing our chain.
	rules, err := ipt.List("raw", "PREROUTING")
	if err != nil {
		klog.V(4).Infof("NotrackManager: failed to list %s PREROUTING rules for cleanup: %v", family, err)
	} else {
		for _, rule := range rules {
			if !strings.Contains(rule, notrackChain) || !strings.Contains(rule, notrackComment) {
				continue
			}

			ifName := parseInterfaceFromRule(rule, "-i")
			if ifName != "" {
				jRule := notrackJumpRule(ifName)
				if delErr := ipt.DeleteIfExists("raw", "PREROUTING", jRule...); delErr != nil {
					klog.V(4).Infof("NotrackManager: failed to remove %s PREROUTING jump for %s during cleanup: %v", family, ifName, delErr)
				}
			}
		}
	}

	// Flush and delete the chain.
	exists, err := ipt.ChainExists("raw", notrackChain)
	if err != nil {
		klog.Warningf("NotrackManager: failed to check if %s chain exists during cleanup: %v", family, err)

		return
	}

	if exists {
		if err := ipt.ClearChain("raw", notrackChain); err != nil {
			klog.Warningf("NotrackManager: failed to flush %s chain during cleanup: %v", family, err)
		}

		if err := ipt.DeleteChain("raw", notrackChain); err != nil {
			klog.Warningf("NotrackManager: failed to delete %s chain during cleanup: %v", family, err)
		}
	}
}

// splitByFamily separates a list of CIDRs into IPv4 and IPv6 slices.
func splitByFamily(cidrs []string) (v4, v6 []string) {
	for _, cidr := range cidrs {
		ip, _, err := net.ParseCIDR(cidr)
		if err != nil {
			klog.V(4).Infof("NotrackManager: skipping unparseable CIDR %q: %v", cidr, err)

			continue
		}

		if ip.To4() != nil {
			v4 = append(v4, cidr)
		} else {
			v6 = append(v6, cidr)
		}
	}

	return v4, v6
}

// dedupeAndSort returns a sorted, deduplicated copy of the input slice.
func dedupeAndSort(items []string) []string {
	if len(items) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(items))
	result := make([]string, 0, len(items))

	for _, item := range items {
		if _, ok := seen[item]; ok {
			continue
		}

		seen[item] = struct{}{}
		result = append(result, item)
	}

	sort.Strings(result)

	return result
}
