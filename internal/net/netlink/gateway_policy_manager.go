// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netlink

import (
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"
	"k8s.io/klog/v2"
)

// validIfaceNameRe matches safe interface names for rt_tables entries.
var validIfaceNameRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// validateInterfaceName checks that name contains only safe characters
// before it is written to rt_tables.
func validateInterfaceName(name string) error {
	if !validIfaceNameRe.MatchString(name) {
		return fmt.Errorf("invalid interface name %q: must match %s", name, validIfaceNameRe.String())
	}

	return nil
}

// Deprecated: GatewayPolicyManager is deprecated. Policy-based routing (PBR) via
// connmark/fwmark/ip-rule is replaced by per-interface iptables FORWARD ACCEPT
// rules. This type is retained for backward compatibility when
// enablePolicyRouting is explicitly set to true.
//
// GatewayPolicyManager manages policy routing rules to ensure return traffic
// from gateway interfaces leaves via the same interface it arrived on.
// This is necessary for proper bidirectional communication through WireGuard tunnels.
type GatewayPolicyManager struct {
	ipt4 *iptables.IPTables
	ipt6 *iptables.IPTables

	wireguardBasePort int
	gatewayTableBase  int

	// Track which interfaces have been configured
	configuredIfaces map[string]int // interface name -> table number

	// Track if the global OUTPUT rule has been added
	outputRuleAdded bool

	mu sync.Mutex
}

// rtTablesPath is the path to the iproute2 rt_tables file. It is a var
// (rather than const) so that tests can override it with a temp file.
var rtTablesPath = "/etc/iproute2/rt_tables"

const (
	mangleTable = "mangle"

	// Gateway marks/rule preferences are normalized as (<port> - <wireguardBasePort> + offset).
	gatewayPolicyPortOffset int = 100

	// kube-proxy's KUBE-MARK-MASQ path toggles bit 0x4000 via xor.
	// Keep gateway policy marks out of that bit entirely to avoid accidental SNAT behavior.
	kubeProxyMasqMarkBit uint32 = 0x4000

	gatewayPolicyPreroutingChain = "UNBOUNDED-GW-PRE"
	gatewayPolicyOutputChain     = "UNBOUNDED-GW-OUT"
)

func (m *GatewayPolicyManager) gatewayPolicyIngressMark(tableNum int) (uint32, error) {
	if tableNum <= 0 {
		return 0, fmt.Errorf("invalid table number %d", tableNum)
	}

	if tableNum < m.gatewayTableBase {
		return 0, fmt.Errorf("table number %d is below gateway table base %d", tableNum, m.gatewayTableBase)
	}

	normalized := tableNum - m.gatewayTableBase
	if normalized <= 0 {
		return 0, fmt.Errorf("table number %d must be greater than base %d", tableNum, m.gatewayTableBase)
	}

	if normalized >= int(kubeProxyMasqMarkBit) {
		return 0, fmt.Errorf("table number %d exceeds supported gateway policy mark range for base %d", tableNum, m.gatewayTableBase)
	}

	ingressMark := uint32(normalized)

	if ingressMark&kubeProxyMasqMarkBit != 0 {
		return 0, fmt.Errorf("derived ingress mark %#x for table %d overlaps kube-proxy MASQ bit %#x", ingressMark, tableNum, kubeProxyMasqMarkBit)
	}

	return ingressMark, nil
}

func (m *GatewayPolicyManager) gatewayPolicyRulePriority(tableNum int) (int, error) {
	// Keep fwmark rule preference low enough to run before main/default.
	// Requested mapping: <port> - <wgBasePort> + 100.
	priority := tableNum - m.gatewayTableBase
	if priority <= 0 {
		return 0, fmt.Errorf("invalid gateway policy rule priority %d for table %d", priority, tableNum)
	}

	return priority, nil
}

func (m *GatewayPolicyManager) gatewayPolicyTableFromIngressMark(mark int) (int, bool) {
	if mark <= 0 {
		return 0, false
	}

	u := uint32(mark)
	if u >= kubeProxyMasqMarkBit {
		return 0, false
	}

	return m.gatewayTableBase + int(u), true
}

func (m *GatewayPolicyManager) gatewayPolicyLegacyMarks(tableNum int) map[uint32]struct{} {
	marks := make(map[uint32]struct{})

	if tableNum <= 0 {
		return marks
	}

	if ingressMark, err := m.gatewayPolicyIngressMark(tableNum); err == nil {
		marks[ingressMark] = struct{}{}
	}

	legacyDirect := uint32(tableNum)
	marks[legacyDirect] = struct{}{}

	if tableNum <= 0xffff {
		legacyHigh16 := uint32(tableNum) << 16
		marks[legacyHigh16] = struct{}{}
		marks[legacyHigh16|(1<<31)] = struct{}{}
	}

	if normalized := tableNum - m.gatewayTableBase; normalized > 0 && normalized < int(kubeProxyMasqMarkBit) {
		legacyOutbound := uint32(normalized) | 0x2000
		marks[legacyOutbound] = struct{}{}
	}

	return marks
}

func (m *GatewayPolicyManager) deleteIPRulesByMarkSet(tableNum int, marks map[uint32]struct{}, family int, familyName string) error {
	rules, err := netlinkRuleList(family)
	if err != nil {
		if family == netlink.FAMILY_V4 {
			return fmt.Errorf("failed to list %s rules: %w", familyName, err)
		}

		klog.V(3).Infof("Failed to list %s rules: %v", familyName, err)

		return nil
	}

	for _, rule := range rules {
		if rule.Table != tableNum {
			continue
		}

		if _, keep := marks[rule.Mark]; !keep {
			continue
		}

		r := rule
		if err := netlinkRuleDel(&r); err != nil {
			klog.V(4).Infof("Failed deleting %s ip rule mark=%d table=%d: %v", familyName, rule.Mark, rule.Table, err)
			continue
		}

		klog.V(2).Infof("Deleted %s ip rule mark=%d table=%d", familyName, rule.Mark, rule.Table)
	}

	return nil
}

// NewGatewayPolicyManager creates a new GatewayPolicyManager.
func NewGatewayPolicyManager(wireguardBasePort int) (*GatewayPolicyManager, error) {
	ipt4, err := iptables.New()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize IPv4 iptables: %w", err)
	}

	ipt6, err := iptables.NewWithProtocol(iptables.ProtocolIPv6)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize IPv6 iptables: %w", err)
	}

	m := &GatewayPolicyManager{
		ipt4:              ipt4,
		ipt6:              ipt6,
		wireguardBasePort: wireguardBasePort,
		gatewayTableBase:  wireguardBasePort - gatewayPolicyPortOffset,
		configuredIfaces:  make(map[string]int),
	}
	if m.gatewayTableBase <= 0 {
		return nil, fmt.Errorf("invalid WireGuard base port %d for gateway policy table base", wireguardBasePort)
	}

	if err := m.ensureChainsAndJumps(m.ipt4, "IPv4"); err != nil {
		return nil, fmt.Errorf("failed to initialize IPv4 gateway policy chains: %w", err)
	}

	if err := m.ensureChainsAndJumps(m.ipt6, "IPv6"); err != nil {
		return nil, fmt.Errorf("failed to initialize IPv6 gateway policy chains: %w", err)
	}

	return m, nil
}

func (m *GatewayPolicyManager) ensureChainsAndJumps(ipt *iptables.IPTables, family string) error {
	if ipt == nil {
		return nil
	}

	for _, chain := range []string{gatewayPolicyPreroutingChain, gatewayPolicyOutputChain} {
		exists, err := ipt.ChainExists(mangleTable, chain)
		if err != nil {
			return fmt.Errorf("failed to check %s chain %s: %w", family, chain, err)
		}

		if !exists {
			if err := ipt.NewChain(mangleTable, chain); err != nil {
				return fmt.Errorf("failed to create %s chain %s: %w", family, chain, err)
			}
		}
	}

	preroutingJump := []string{"-m", "comment", "--comment", commentPrefix + ": gateway policy prerouting jump", "-j", gatewayPolicyPreroutingChain}

	exists, err := ipt.Exists(mangleTable, "PREROUTING", preroutingJump...)
	if err != nil {
		return fmt.Errorf("failed checking %s prerouting jump: %w", family, err)
	}

	if !exists {
		if err := ipt.Append(mangleTable, "PREROUTING", preroutingJump...); err != nil {
			return fmt.Errorf("failed adding %s prerouting jump: %w", family, err)
		}
	}

	outputJump := []string{"-m", "comment", "--comment", commentPrefix + ": gateway policy output jump", "-j", gatewayPolicyOutputChain}

	exists, err = ipt.Exists(mangleTable, "OUTPUT", outputJump...)
	if err != nil {
		return fmt.Errorf("failed checking %s output jump: %w", family, err)
	}

	if !exists {
		if err := ipt.Append(mangleTable, "OUTPUT", outputJump...); err != nil {
			return fmt.Errorf("failed adding %s output jump: %w", family, err)
		}
	}

	return nil
}

// ConfigureInterface sets up policy routing for a gateway interface.
// This ensures return traffic leaves via the same interface it arrived on.
// The tableNum is the routing table number to use (typically the WireGuard listen port).
func (m *GatewayPolicyManager) ConfigureInterface(ifaceName string, tableNum int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.pruneStaleInterfacesLocked(); err != nil {
		klog.Warningf("Failed pruning stale gateway policy interfaces: %v", err)
	}

	if err := m.cleanupLegacyOutputSourceRulesLocked(); err != nil {
		klog.Warningf("Failed cleaning legacy gateway output source rules: %v", err)
	}

	if err := m.cleanupOutboundPinningRulesLocked(); err != nil {
		klog.Warningf("Failed cleaning legacy gateway outbound pinning rules: %v", err)
	}

	// Check if already configured with the same table number
	if existingTable, exists := m.configuredIfaces[ifaceName]; exists {
		if existingTable == tableNum {
			klog.V(3).Infof("Gateway policy routing already configured for %s (table %d)", ifaceName, tableNum)
			return nil
		}
		// Table number changed, need to reconfigure
		klog.V(2).Infof("Gateway %s table number changed from %d to %d, reconfiguring", ifaceName, existingTable, tableNum)
		m.removeInterfaceRulesLocked(ifaceName, existingTable)
	}

	klog.V(2).Infof("Configuring gateway policy routing for %s (table %d)", ifaceName, tableNum)

	// 1. Add route table entry if not exists
	if err := EnsureRtTablesEntry(tableNum, ifaceName); err != nil {
		return fmt.Errorf("failed to ensure route table for %s: %w", ifaceName, err)
	}

	// 2. Add default route in the interface's table
	if err := m.ensureDefaultRoute(ifaceName, tableNum); err != nil {
		return fmt.Errorf("failed to add default route for %s: %w", ifaceName, err)
	}

	// 3. Add iptables rules for marking packets
	if err := m.ensureIptablesRules(ifaceName, tableNum); err != nil {
		return fmt.Errorf("failed to add iptables rules for %s: %w", ifaceName, err)
	}

	// 4. Add ip rule for policy routing
	if err := m.ensureIPRule(tableNum); err != nil {
		return fmt.Errorf("failed to add ip rule for %s: %w", ifaceName, err)
	}

	// 5. Add global OUTPUT rule (only once)
	if !m.outputRuleAdded {
		if err := m.ensureOutputRule(); err != nil {
			return fmt.Errorf("failed to add OUTPUT rule: %w", err)
		}

		m.outputRuleAdded = true
	}

	m.configuredIfaces[ifaceName] = tableNum
	klog.Infof("Gateway policy routing configured for %s (table %d)", ifaceName, tableNum)

	return nil
}

// ensureDefaultRoute ensures a default route exists in the interface's table
func (m *GatewayPolicyManager) ensureDefaultRoute(ifaceName string, tableNum int) error {
	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return fmt.Errorf("failed to get link %s: %w", ifaceName, err)
	}

	// Parse default route destinations
	_, defaultV4, _ := net.ParseCIDR("0.0.0.0/0") //nolint:errcheck
	_, defaultV6, _ := net.ParseCIDR("::/0")      //nolint:errcheck

	// IPv4 default route
	route4 := &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Table:     tableNum,
		Dst:       defaultV4,
	}

	// Check if route exists
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, route4, netlink.RT_FILTER_TABLE|netlink.RT_FILTER_OIF)
	if err != nil {
		klog.V(3).Infof("Failed to list routes for table %d: %v", tableNum, err)
	}

	routeExists := false

	for _, r := range routes {
		if r.Dst == nil || r.Dst.String() == "0.0.0.0/0" {
			routeExists = true
			break
		}
	}

	if !routeExists {
		if err := netlink.RouteAdd(route4); err != nil {
			// Ignore "file exists" error
			if !strings.Contains(err.Error(), "file exists") {
				return fmt.Errorf("failed to add IPv4 default route: %w", err)
			}
		} else {
			klog.V(2).Infof("Added IPv4 default route via %s in table %d", ifaceName, tableNum)
		}
	}

	// IPv6 default route
	route6 := &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Table:     tableNum,
		Dst:       defaultV6,
	}

	routes6, err := netlink.RouteListFiltered(netlink.FAMILY_V6, route6, netlink.RT_FILTER_TABLE|netlink.RT_FILTER_OIF)
	if err != nil {
		klog.V(3).Infof("Failed to list IPv6 routes for table %d: %v", tableNum, err)
	}

	route6Exists := false

	for _, r := range routes6 {
		if r.Dst == nil || r.Dst.String() == "::/0" {
			route6Exists = true
			break
		}
	}

	if !route6Exists {
		if err := netlink.RouteAdd(route6); err != nil {
			// Ignore "file exists" error
			if !strings.Contains(err.Error(), "file exists") {
				klog.V(3).Infof("Failed to add IPv6 default route (may not be needed): %v", err)
			}
		} else {
			klog.V(2).Infof("Added IPv6 default route via %s in table %d", ifaceName, tableNum)
		}
	}

	return nil
}

// ensureIptablesRules ensures the PREROUTING rules for marking incoming packets
func (m *GatewayPolicyManager) ensureIptablesRules(ifaceName string, tableNum int) error {
	ingressMark, err := m.gatewayPolicyIngressMark(tableNum)
	if err != nil {
		return err
	}

	ingressMarkStr := strconv.FormatUint(uint64(ingressMark), 10)

	// Ensure incoming packets can recover previously saved connection marks.
	if err := m.ensurePreroutingRestoreRule(); err != nil {
		return fmt.Errorf("failed to add PREROUTING restore rule: %w", err)
	}

	// Set connection mark on ORIGINAL direction NEW packets that ingress via the gateway interface.
	// We intentionally do not set packet mark here, so the initial upstream->downstream packet
	// follows normal routing. Reply packets restore this connmark and are policy-routed back.
	setConnmarkRule := []string{"-m", "conntrack", "--ctstate", "NEW", "--ctdir", "ORIGINAL", "-i", ifaceName, "-m", "comment", "--comment", commentPrefix + ": gateway policy set ingress connmark", "-j", "CONNMARK", "--set-mark", ingressMarkStr}

	if err := m.ensureRule(m.ipt4, mangleTable, gatewayPolicyPreroutingChain, setConnmarkRule); err != nil {
		return fmt.Errorf("failed to add IPv4 ingress connmark set rule: %w", err)
	}

	if err := m.ensureRule(m.ipt6, mangleTable, gatewayPolicyPreroutingChain, setConnmarkRule); err != nil {
		return fmt.Errorf("failed to add IPv6 ingress connmark set rule: %w", err)
	}

	klog.V(2).Infof("Added iptables mangle PREROUTING rules for %s (ingress mark %d)", ifaceName, ingressMark)

	return nil
}

func (m *GatewayPolicyManager) ensurePreroutingRestoreRule() error {
	restoreRule := []string{"-m", "conntrack", "--ctdir", "REPLY", "-m", "comment", "--comment", commentPrefix + ": gateway policy prerouting restore connmark", "-j", "CONNMARK", "--restore-mark"}

	if err := m.ensureRule(m.ipt4, mangleTable, gatewayPolicyPreroutingChain, restoreRule); err != nil {
		return fmt.Errorf("failed to add IPv4 PREROUTING restore rule: %w", err)
	}

	if err := m.ensureRule(m.ipt6, mangleTable, gatewayPolicyPreroutingChain, restoreRule); err != nil {
		return fmt.Errorf("failed to add IPv6 PREROUTING restore rule: %w", err)
	}

	klog.V(3).Infof("Ensured iptables mangle PREROUTING CONNMARK restore rule")

	return nil
}

// ensureOutputRule ensures the global OUTPUT rule for restoring connmark
func (m *GatewayPolicyManager) ensureOutputRule() error {
	restoreRule := []string{"-m", "comment", "--comment", commentPrefix + ": gateway policy restore connmark", "-j", "CONNMARK", "--restore-mark"}

	// IPv4
	if err := m.ensureRule(m.ipt4, mangleTable, gatewayPolicyOutputChain, restoreRule); err != nil {
		return fmt.Errorf("failed to add IPv4 OUTPUT rule: %w", err)
	}

	// IPv6
	if err := m.ensureRule(m.ipt6, mangleTable, gatewayPolicyOutputChain, restoreRule); err != nil {
		return fmt.Errorf("failed to add IPv6 OUTPUT rule: %w", err)
	}

	klog.V(2).Infof("Added iptables mangle OUTPUT CONNMARK restore rule")

	return nil
}

// ensureRule adds an iptables rule if it doesn't exist
func (m *GatewayPolicyManager) ensureRule(ipt *iptables.IPTables, table, chain string, rule []string) error {
	exists, err := ipt.Exists(table, chain, rule...)
	if err != nil {
		return fmt.Errorf("failed to check rule existence: %w", err)
	}

	if exists {
		return nil
	}

	return ipt.Append(table, chain, rule...)
}

// ensureIPRule ensures the ip rule for policy routing exists
func (m *GatewayPolicyManager) ensureIPRule(tableNum int) error {
	ingressMark, err := m.gatewayPolicyIngressMark(tableNum)
	if err != nil {
		return err
	}

	rulePriority, err := m.gatewayPolicyRulePriority(tableNum)
	if err != nil {
		return err
	}

	if err := EnsureIPRule(tableNum, rulePriority, ingressMark, 0xffffffff); err != nil {
		return err
	}

	// Clean up legacy marks that no longer match the current ingress mark.
	legacyMarks := m.gatewayPolicyLegacyMarks(tableNum)
	delete(legacyMarks, ingressMark)

	if len(legacyMarks) > 0 {
		if err := m.deleteIPRulesByMarkSet(tableNum, legacyMarks, netlink.FAMILY_V4, "IPv4"); err != nil {
			return err
		}

		if err := m.deleteIPRulesByMarkSet(tableNum, legacyMarks, netlink.FAMILY_V6, "IPv6"); err != nil {
			return err
		}
	}

	return nil
}

// RemoveInterface removes policy routing configuration for an interface
func (m *GatewayPolicyManager) RemoveInterface(ifaceName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.cleanupLegacyOutputSourceRulesLocked(); err != nil {
		klog.Warningf("Failed cleaning legacy gateway output source rules: %v", err)
	}

	if err := m.cleanupOutboundPinningRulesLocked(); err != nil {
		klog.Warningf("Failed cleaning legacy gateway outbound pinning rules: %v", err)
	}

	tableNum, exists := m.configuredIfaces[ifaceName]
	if !exists {
		var discovered bool

		tableNum, discovered = m.findInterfaceTableNumLocked(ifaceName)
		if !discovered {
			return nil
		}
	}

	m.removeInterfaceRulesLocked(ifaceName, tableNum)
	delete(m.configuredIfaces, ifaceName)

	return nil
}

// ReconcileExpectedInterfaces ensures managed UNBOUNDED-* chain rules only contain
// expected gateway interface entries and removes stale policy rules/tables.
func (m *GatewayPolicyManager) ReconcileExpectedInterfaces(expected map[string]int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	klog.V(4).Infof("Reconciling gateway policy expected interfaces: %s", formatExpectedGatewayPolicyTables(expected))

	if err := m.cleanupLegacyOutputSourceRulesLocked(); err != nil {
		return err
	}

	if err := m.cleanupOutboundPinningRulesLocked(); err != nil {
		return err
	}

	if err := m.reconcileManagedChainsLocked(expected); err != nil {
		return err
	}

	if err := m.cleanupStaleIPRulesLocked(expected); err != nil {
		return err
	}

	nextConfigured := make(map[string]int, len(expected))
	for iface, tableNum := range expected {
		nextConfigured[iface] = tableNum
	}

	m.configuredIfaces = nextConfigured

	return nil
}

// removeInterfaceRulesLocked removes rules for an interface (must hold lock)
func (m *GatewayPolicyManager) removeInterfaceRulesLocked(ifaceName string, tableNum int) {
	ingressMark, err := m.gatewayPolicyIngressMark(tableNum)
	if err != nil {
		klog.V(3).Infof("Continuing gateway policy cleanup for %s despite mark derivation failure for table %d: %v", ifaceName, tableNum, err)
	}

	ingressMarkStr := strconv.FormatUint(uint64(ingressMark), 10)

	// Remove iptables rules
	setConnmarkRule := []string{"-m", "conntrack", "--ctstate", "NEW", "--ctdir", "ORIGINAL", "-i", ifaceName, "-m", "comment", "--comment", commentPrefix + ": gateway policy set ingress connmark", "-j", "CONNMARK", "--set-mark", ingressMarkStr}
	legacyMarkRule := []string{"-i", ifaceName, "-m", "comment", "--comment", commentPrefix + ": gateway policy mark ingress", "-j", "MARK", "--set-mark", ingressMarkStr}
	legacySaveRule := []string{"-i", ifaceName, "-m", "comment", "--comment", commentPrefix + ": gateway policy save connmark", "-j", "CONNMARK", "--save-mark"}

	if err := m.ipt4.Delete(mangleTable, gatewayPolicyPreroutingChain, setConnmarkRule...); err != nil {
		klog.V(4).Infof("Failed to delete IPv4 ingress connmark set rule for %s: %v", ifaceName, err)
	}

	if err := m.ipt4.Delete(mangleTable, gatewayPolicyPreroutingChain, legacyMarkRule...); err != nil {
		klog.V(4).Infof("Failed to delete legacy IPv4 mark rule for %s: %v", ifaceName, err)
	}

	if err := m.ipt4.Delete(mangleTable, gatewayPolicyPreroutingChain, legacySaveRule...); err != nil {
		klog.V(4).Infof("Failed to delete legacy IPv4 save connmark rule for %s: %v", ifaceName, err)
	}

	if err := m.ipt6.Delete(mangleTable, gatewayPolicyPreroutingChain, setConnmarkRule...); err != nil {
		klog.V(4).Infof("Failed to delete IPv6 ingress connmark set rule for %s: %v", ifaceName, err)
	}

	if err := m.ipt6.Delete(mangleTable, gatewayPolicyPreroutingChain, legacyMarkRule...); err != nil {
		klog.V(4).Infof("Failed to delete legacy IPv6 mark rule for %s: %v", ifaceName, err)
	}

	if err := m.ipt6.Delete(mangleTable, gatewayPolicyPreroutingChain, legacySaveRule...); err != nil {
		klog.V(4).Infof("Failed to delete legacy IPv6 save connmark rule for %s: %v", ifaceName, err)
	}

	// Remove ip rules (current and legacy marks) by matching against actual listed rules.
	marks := m.gatewayPolicyLegacyMarks(tableNum)
	if ingressMark != 0 {
		marks[ingressMark] = struct{}{}
	}

	if err := m.deleteIPRulesByMarkSet(tableNum, marks, netlink.FAMILY_V4, "IPv4"); err != nil {
		klog.V(4).Infof("Failed deleting IPv4 ip rules for %s table=%d: %v", ifaceName, tableNum, err)
	}

	if err := m.deleteIPRulesByMarkSet(tableNum, marks, netlink.FAMILY_V6, "IPv6"); err != nil {
		klog.V(4).Infof("Failed deleting IPv6 ip rules for %s table=%d: %v", ifaceName, tableNum, err)
	}

	if err := FlushRouteTable(tableNum); err != nil {
		klog.V(3).Infof("Failed to remove routes for table %d (%s): %v", tableNum, ifaceName, err)
	}

	if err := m.removeRouteTableEntry(tableNum); err != nil {
		klog.V(3).Infof("Failed to remove rt_tables entry for table %d (%s): %v", tableNum, ifaceName, err)
	}

	klog.V(2).Infof("Removed policy routing rules for %s (table %d)", ifaceName, tableNum)
}

// Cleanup removes all gateway policy iptables rules/chains managed by this manager.
func (m *GatewayPolicyManager) Cleanup() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.cleanupFamily(m.ipt4, "IPv4"); err != nil {
		return err
	}

	if err := m.cleanupFamily(m.ipt6, "IPv6"); err != nil {
		return err
	}

	m.outputRuleAdded = false
	m.configuredIfaces = make(map[string]int)

	return nil
}

func (m *GatewayPolicyManager) pruneStaleInterfacesLocked() error {
	seen := make(map[string]int)

	collect := func(ipt *iptables.IPTables, family string) error {
		if ipt == nil {
			return nil
		}

		rules, err := ipt.List(mangleTable, gatewayPolicyPreroutingChain)
		if err != nil {
			return fmt.Errorf("failed to list %s prerouting chain: %w", family, err)
		}

		for _, line := range rules {
			iface, tableNum, ok := m.parseGatewayIngressRule(line)
			if !ok {
				continue
			}

			if iface == "" || tableNum == 0 {
				continue
			}

			if _, exists := seen[iface]; !exists {
				seen[iface] = tableNum
			}
		}

		return nil
	}

	if err := collect(m.ipt4, "IPv4"); err != nil {
		return err
	}

	if err := collect(m.ipt6, "IPv6"); err != nil {
		return err
	}

	for iface, tableNum := range seen {
		if _, err := netlink.LinkByName(iface); err == nil {
			continue
		}

		klog.V(2).Infof("Removing stale gateway policy rules for missing interface %s (table %d)", iface, tableNum)
		m.removeInterfaceRulesLocked(iface, tableNum)
		delete(m.configuredIfaces, iface)
	}

	return nil
}

func (m *GatewayPolicyManager) findInterfaceTableNumLocked(ifaceName string) (int, bool) {
	readFrom := func(ipt *iptables.IPTables) (int, bool) {
		if ipt == nil {
			return 0, false
		}

		rules, err := ipt.List(mangleTable, gatewayPolicyPreroutingChain)
		if err != nil {
			return 0, false
		}

		for _, line := range rules {
			iface, tableNum, ok := m.parseGatewayIngressRule(line)
			if !ok {
				continue
			}

			if iface == ifaceName {
				return tableNum, true
			}
		}

		return 0, false
	}

	if tableNum, ok := readFrom(m.ipt4); ok {
		return tableNum, true
	}

	return readFrom(m.ipt6)
}

func (m *GatewayPolicyManager) parseGatewayIngressRule(line string) (string, int, bool) {
	if !strings.Contains(line, commentPrefix+": gateway policy mark ingress") && !strings.Contains(line, commentPrefix+": gateway policy set ingress connmark") {
		return "", 0, false
	}

	fields := strings.Fields(line)
	iface := ""
	markValue := ""

	for i := 0; i < len(fields); i++ {
		switch fields[i] {
		case "-i":
			if i+1 < len(fields) {
				iface = fields[i+1]
			}
		case "--set-mark", "--set-xmark":
			if i+1 < len(fields) {
				markValue = fields[i+1]
			}
		}
	}

	if iface == "" || markValue == "" {
		return "", 0, false
	}

	mark, ok := parseMarkValue(markValue)
	if !ok {
		return "", 0, false
	}

	tableNum, ok := m.gatewayPolicyTableFromIngressMark(mark)
	if !ok {
		return "", 0, false
	}

	return iface, tableNum, true
}

func parseMarkValue(value string) (int, bool) {
	base := value
	if slash := strings.Index(base, "/"); slash >= 0 {
		base = base[:slash]
	}

	parsed, err := strconv.ParseInt(base, 0, 64)
	if err != nil {
		return 0, false
	}

	if parsed <= 0 || parsed > math.MaxInt32 {
		return 0, false
	}

	return int(parsed), true
}

func (m *GatewayPolicyManager) cleanupLegacyOutputSourceRulesLocked() error {
	cleanup := func(ipt *iptables.IPTables, family string) error {
		if ipt == nil {
			return nil
		}

		rules, err := ipt.List(mangleTable, gatewayPolicyOutputChain)
		if err != nil {
			return fmt.Errorf("failed to list %s output chain: %w", family, err)
		}

		for _, line := range rules {
			if !strings.Contains(line, commentPrefix+": gateway policy mark outbound") && !strings.Contains(line, commentPrefix+": gateway policy save outbound connmark") {
				continue
			}

			if !strings.Contains(line, " -s ") {
				continue
			}

			args := strings.Fields(line)
			if len(args) < 3 || args[0] != "-A" {
				continue
			}

			if err := ipt.Delete(mangleTable, gatewayPolicyOutputChain, args[2:]...); err != nil {
				klog.V(4).Infof("Failed to remove legacy %s output rule %q: %v", family, line, err)
				continue
			}

			klog.V(2).Infof("Removed legacy %s gateway output source-matching rule: %s", family, line)
		}

		return nil
	}

	if err := cleanup(m.ipt4, "IPv4"); err != nil {
		return err
	}

	return cleanup(m.ipt6, "IPv6")
}

func (m *GatewayPolicyManager) cleanupOutboundPinningRulesLocked() error {
	cleanup := func(ipt *iptables.IPTables, family string) error {
		if ipt == nil {
			return nil
		}

		rules, err := ipt.List(mangleTable, gatewayPolicyOutputChain)
		if err != nil {
			return fmt.Errorf("failed to list %s output chain: %w", family, err)
		}

		for _, line := range rules {
			if !strings.Contains(line, commentPrefix+": gateway policy mark outbound") && !strings.Contains(line, commentPrefix+": gateway policy save outbound connmark") {
				continue
			}

			args := strings.Fields(line)
			if len(args) < 3 || args[0] != "-A" {
				continue
			}

			if err := ipt.Delete(mangleTable, gatewayPolicyOutputChain, args[2:]...); err != nil {
				klog.V(4).Infof("Failed to remove %s outbound pinning rule %q: %v", family, line, err)
				continue
			}

			klog.V(2).Infof("Removed %s legacy gateway outbound pinning rule: %s", family, line)
		}

		return nil
	}

	if err := cleanup(m.ipt4, "IPv4"); err != nil {
		return err
	}

	return cleanup(m.ipt6, "IPv6")
}

func (m *GatewayPolicyManager) removeRouteTableEntry(tableNum int) error {
	content, err := os.ReadFile(rtTablesPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return err
	}

	lines := strings.Split(string(content), "\n")
	filtered := make([]string, 0, len(lines))
	changed := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			filtered = append(filtered, line)
			continue
		}

		fields := strings.Fields(trimmed)
		if len(fields) >= 2 {
			num, convErr := strconv.Atoi(fields[0])
			if convErr == nil && num == tableNum {
				changed = true
				continue
			}
		}

		filtered = append(filtered, line)
	}

	if !changed {
		return nil
	}

	updated := strings.Join(filtered, "\n")
	if !strings.HasSuffix(updated, "\n") {
		updated += "\n"
	}

	return atomicWriteFile(rtTablesPath, []byte(updated), 0o644)
}

func (m *GatewayPolicyManager) cleanupFamily(ipt *iptables.IPTables, family string) error {
	if ipt == nil {
		return nil
	}

	preroutingJump := []string{"-m", "comment", "--comment", commentPrefix + ": gateway policy prerouting jump", "-j", gatewayPolicyPreroutingChain}
	if err := ipt.DeleteIfExists(mangleTable, "PREROUTING", preroutingJump...); err != nil {
		klog.Warningf("Failed to delete %s gateway policy prerouting jump: %v", family, err)
	}

	outputJump := []string{"-m", "comment", "--comment", commentPrefix + ": gateway policy output jump", "-j", gatewayPolicyOutputChain}
	if err := ipt.DeleteIfExists(mangleTable, "OUTPUT", outputJump...); err != nil {
		klog.Warningf("Failed to delete %s gateway policy output jump: %v", family, err)
	}

	for _, chain := range []string{gatewayPolicyPreroutingChain, gatewayPolicyOutputChain} {
		exists, err := ipt.ChainExists(mangleTable, chain)
		if err != nil {
			return fmt.Errorf("failed to check %s chain %s: %w", family, chain, err)
		}

		if !exists {
			continue
		}

		if err := ipt.ClearChain(mangleTable, chain); err != nil {
			klog.Warningf("Failed to clear %s chain %s: %v", family, chain, err)
		}

		if err := ipt.DeleteChain(mangleTable, chain); err != nil {
			klog.Warningf("Failed to delete %s chain %s: %v", family, chain, err)
		}
	}

	return nil
}

func (m *GatewayPolicyManager) reconcileManagedChainsLocked(expected map[string]int) error {
	if err := m.reconcileFamilyChainRulesLocked(m.ipt4, "IPv4", expected); err != nil {
		return err
	}

	if err := m.reconcileFamilyChainRulesLocked(m.ipt6, "IPv6", expected); err != nil {
		return err
	}

	return nil
}

func (m *GatewayPolicyManager) reconcileFamilyChainRulesLocked(ipt *iptables.IPTables, family string, expected map[string]int) error {
	if ipt == nil {
		return nil
	}

	if err := ipt.ClearChain(mangleTable, gatewayPolicyPreroutingChain); err != nil {
		return fmt.Errorf("failed to clear %s %s chain: %w", family, gatewayPolicyPreroutingChain, err)
	}

	if err := ipt.ClearChain(mangleTable, gatewayPolicyOutputChain); err != nil {
		return fmt.Errorf("failed to clear %s %s chain: %w", family, gatewayPolicyOutputChain, err)
	}

	preRestoreRule := []string{"-m", "conntrack", "--ctdir", "REPLY", "-m", "comment", "--comment", commentPrefix + ": gateway policy prerouting restore connmark", "-j", "CONNMARK", "--restore-mark"}
	if err := ipt.Append(mangleTable, gatewayPolicyPreroutingChain, preRestoreRule...); err != nil {
		return fmt.Errorf("failed to add %s prerouting restore rule: %w", family, err)
	}

	ifaces := sortedExpectedIfaces(expected)
	for _, iface := range ifaces {
		tableNum := expected[iface]

		ingressMark, err := m.gatewayPolicyIngressMark(tableNum)
		if err != nil {
			return fmt.Errorf("failed to derive marks for %s table %d: %w", iface, tableNum, err)
		}

		ingressMarkStr := strconv.FormatUint(uint64(ingressMark), 10)

		setConnmarkRule := []string{"-m", "conntrack", "--ctstate", "NEW", "--ctdir", "ORIGINAL", "-i", iface, "-m", "comment", "--comment", commentPrefix + ": gateway policy set ingress connmark", "-j", "CONNMARK", "--set-mark", ingressMarkStr}
		if err := ipt.Append(mangleTable, gatewayPolicyPreroutingChain, setConnmarkRule...); err != nil {
			return fmt.Errorf("failed to add %s prerouting set connmark rule for %s: %w", family, iface, err)
		}
	}

	restoreRule := []string{"-m", "comment", "--comment", commentPrefix + ": gateway policy restore connmark", "-j", "CONNMARK", "--restore-mark"}
	if err := ipt.Append(mangleTable, gatewayPolicyOutputChain, restoreRule...); err != nil {
		return fmt.Errorf("failed to add %s output restore rule: %w", family, err)
	}

	return nil
}

func sortedExpectedIfaces(expected map[string]int) []string {
	ifaces := make([]string, 0, len(expected))
	for iface := range expected {
		ifaces = append(ifaces, iface)
	}

	sort.Strings(ifaces)

	return ifaces
}

func formatExpectedGatewayPolicyTables(expected map[string]int) string {
	if len(expected) == 0 {
		return "<none>"
	}

	ifaces := sortedExpectedIfaces(expected)

	parts := make([]string, 0, len(ifaces))
	for _, iface := range ifaces {
		parts = append(parts, fmt.Sprintf("%s->%d", iface, expected[iface]))
	}

	return strings.Join(parts, ",")
}

func (m *GatewayPolicyManager) cleanupStaleIPRulesLocked(expected map[string]int) error {
	expectedMarks := make(map[uint32]struct{}, len(expected))

	expectedTables := make(map[int]struct{}, len(expected))
	for _, table := range expected {
		ingressMark, err := m.gatewayPolicyIngressMark(table)
		if err != nil {
			continue
		}

		expectedMarks[ingressMark] = struct{}{}
		expectedTables[table] = struct{}{}
	}

	cleanupFamily := func(family int, familyName string) error {
		rules, err := netlinkRuleList(family)
		if err != nil {
			return fmt.Errorf("failed to list %s ip rules: %w", familyName, err)
		}

		for _, rule := range rules {
			if rule.Mark == 0 {
				continue
			}

			tableNum := rule.Table
			if tableNum <= 0 {
				continue
			}

			legacyMarks := m.gatewayPolicyLegacyMarks(tableNum)
			if len(legacyMarks) == 0 {
				continue
			}

			ingressMark, hasIngress := m.gatewayPolicyIngressMark(tableNum)
			if hasIngress != nil {
				ingressMark = 0
			}

			if _, owned := legacyMarks[rule.Mark]; !owned {
				continue
			}

			if _, keepTable := expectedTables[tableNum]; keepTable && ingressMark != 0 && rule.Mark == ingressMark {
				continue
			}

			if _, keepMark := expectedMarks[rule.Mark]; keepMark && rule.Mark == ingressMark {
				continue
			}

			r := rule
			if err := netlinkRuleDel(&r); err != nil {
				klog.V(4).Infof("Failed deleting stale %s ip rule mark=%d table=%d: %v", familyName, rule.Mark, rule.Table, err)
				continue
			}

			_ = FlushRouteTable(tableNum)         //nolint:errcheck
			_ = m.removeRouteTableEntry(tableNum) //nolint:errcheck

			klog.V(2).Infof("Removed stale %s ip rule mark=%d table=%d", familyName, rule.Mark, rule.Table)
		}

		return nil
	}

	if err := cleanupFamily(netlink.FAMILY_V4, "IPv4"); err != nil {
		return err
	}

	if err := cleanupFamily(netlink.FAMILY_V6, "IPv6"); err != nil {
		return err
	}

	return nil
}

// atomicWriteFile writes data to a temp file in the same directory as path,
// then renames the temp file to path. This avoids partial writes on crash.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("failed to create temp file for atomic write: %w", err)
	}

	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()        //nolint:errcheck
		_ = os.Remove(tmpName) //nolint:errcheck

		return fmt.Errorf("failed to write temp file: %w", err)
	}

	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()        //nolint:errcheck
		_ = os.Remove(tmpName) //nolint:errcheck

		return fmt.Errorf("failed to set permissions on temp file: %w", err)
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName) //nolint:errcheck
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName) //nolint:errcheck
		return fmt.Errorf("failed to rename temp file to %s: %w", path, err)
	}

	return nil
}
