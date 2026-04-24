// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netlink

import (
	"fmt"
	"net"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"
	"k8s.io/klog/v2"
)

const (
	// masqueradeChain is the name of the custom chain for masquerade rules
	masqueradeChain = "UNBOUNDED-MASQUERADE"
	// commentPrefix is used to identify rules created by this manager
	commentPrefix = "unbounded-net"
	// maxConsecutiveParseFailures is the threshold before flushing and rebuilding the chain
	maxConsecutiveParseFailures = 3
)

// MasqueradeManager manages iptables masquerade rules for pod egress traffic.
// It supports both IPv4 and IPv6 with separate rule sets.
type MasqueradeManager struct {
	ipt4 *iptables.IPTables // IPv4 iptables handle
	ipt6 *iptables.IPTables // IPv6 ip6tables handle
	mu   sync.Mutex

	// Tracked state for differential updates
	currentDefaultIfaceV4 string
	currentDefaultIfaceV6 string
	currentNonMasqCIDRs   []string
}

// NewMasqueradeManager creates a new MasqueradeManager with IPv4 and IPv6 support.
// Returns an error if iptables is not available.
func NewMasqueradeManager() (*MasqueradeManager, error) {
	// Initialize IPv4 iptables
	ipt4, err := iptables.New()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize IPv4 iptables: %w", err)
	}

	// Initialize IPv6 iptables
	ipt6, err := iptables.NewWithProtocol(iptables.ProtocolIPv6)
	if err != nil {
		// IPv6 may not be available, log warning but continue
		klog.Warningf("Failed to initialize IPv6 iptables (IPv6 masquerade will be disabled): %v", err)

		ipt6 = nil
	}

	mm := &MasqueradeManager{
		ipt4: ipt4,
		ipt6: ipt6,
	}

	// Initialize chains for both families
	if err := mm.ensureChainExists(ipt4, "IPv4"); err != nil {
		return nil, fmt.Errorf("failed to create IPv4 masquerade chain: %w", err)
	}

	if ipt6 != nil {
		if err := mm.ensureChainExists(ipt6, "IPv6"); err != nil {
			klog.Warningf("Failed to create IPv6 masquerade chain: %v", err)
			// Continue without IPv6
		}
	}

	return mm, nil
}

// ensureChainExists creates the UNBOUNDED-MASQUERADE chain and adds a jump from POSTROUTING
func (mm *MasqueradeManager) ensureChainExists(ipt *iptables.IPTables, family string) error {
	// Create the chain if it doesn't exist
	exists, err := ipt.ChainExists("nat", masqueradeChain)
	if err != nil {
		return fmt.Errorf("failed to check if chain exists: %w", err)
	}

	if !exists {
		if err := ipt.NewChain("nat", masqueradeChain); err != nil {
			return fmt.Errorf("failed to create chain: %w", err)
		}

		klog.V(2).Infof("Created %s chain %s in nat table", family, masqueradeChain)
	}

	// Add jump from POSTROUTING to our chain if not already present
	jumpRule := []string{"-m", "comment", "--comment", commentPrefix + ": masquerade egress traffic", "-j", masqueradeChain}

	exists, err = ipt.Exists("nat", "POSTROUTING", jumpRule...)
	if err != nil {
		return fmt.Errorf("failed to check if jump rule exists: %w", err)
	}

	if !exists {
		if err := ipt.Append("nat", "POSTROUTING", jumpRule...); err != nil {
			return fmt.Errorf("failed to add jump rule: %w", err)
		}

		klog.V(2).Infof("Added %s jump from POSTROUTING to %s", family, masqueradeChain)
	}

	return nil
}

// SyncRules synchronizes the masquerade rules with the desired state.
// It handles both IPv4 and IPv6 CIDRs, automatically classifying them by family.
func (mm *MasqueradeManager) SyncRules(defaultIfaceV4, defaultIfaceV6 string, nonMasqCIDRs []string) error {
	start := time.Now()

	mm.mu.Lock()
	defer mm.mu.Unlock()
	defer func() {
		MasqueradeSyncDuration.Observe(time.Since(start).Seconds())
	}()

	// Check if anything changed
	if mm.currentDefaultIfaceV4 == defaultIfaceV4 &&
		mm.currentDefaultIfaceV6 == defaultIfaceV6 &&
		stringSlicesEqual(mm.currentNonMasqCIDRs, nonMasqCIDRs) {
		klog.V(4).Info("Masquerade rules unchanged, skipping sync")
		return nil
	}

	// Classify CIDRs by IP family
	nonMasqCIDRsV4, nonMasqCIDRsV6 := classifyCIDRsByFamily(nonMasqCIDRs)

	// Sync IPv4 rules
	if err := mm.syncRulesForFamily(mm.ipt4, "IPv4", defaultIfaceV4, nonMasqCIDRsV4); err != nil {
		return fmt.Errorf("failed to sync IPv4 masquerade rules: %w", err)
	}

	// Sync IPv6 rules
	if mm.ipt6 != nil {
		if err := mm.syncRulesForFamily(mm.ipt6, "IPv6", defaultIfaceV6, nonMasqCIDRsV6); err != nil {
			klog.Warningf("Failed to sync IPv6 masquerade rules: %v", err)
			// Continue - IPv4 rules are already applied
		}
	}

	// Update tracked state
	mm.currentDefaultIfaceV4 = defaultIfaceV4
	mm.currentDefaultIfaceV6 = defaultIfaceV6

	mm.currentNonMasqCIDRs = append([]string{}, nonMasqCIDRs...)

	klog.V(2).Infof("Synced masquerade rules (IPv4 default: %s, IPv6 default: %s)", defaultIfaceV4, defaultIfaceV6)

	return nil
}

// syncRulesForFamily synchronizes the masquerade rules for a specific IP family using delta updates.
// Instead of flushing and rebuilding all rules, it calculates what needs to be added or removed.
// If too many consecutive parse failures occur, it flushes the chain and rebuilds from scratch.
func (mm *MasqueradeManager) syncRulesForFamily(ipt *iptables.IPTables, family, defaultIface string, nonMasqCIDRs []string) error {
	if ipt == nil {
		return nil
	}

	// Get current rules in the chain
	currentRules, err := ipt.List("nat", masqueradeChain)
	if err != nil {
		// Chain might not exist, try to create it
		if err := mm.ensureChainExists(ipt, family); err != nil {
			return fmt.Errorf("failed to ensure chain exists: %w", err)
		}

		currentRules = []string{}
	}

	// Build the desired rules
	desiredRules := mm.buildDesiredRules(defaultIface, nonMasqCIDRs)

	// Parse current rules into a set (skip the chain header line)
	currentRuleSet := make(map[string]bool)

	for _, rule := range currentRules {
		// Skip the chain policy line (e.g., "-N UNBOUNDED-MASQUERADE")
		if len(rule) > 0 && rule[0] != '-' {
			continue
		}
		// Skip if it's a chain definition
		if len(rule) >= 2 && rule[0:2] == "-N" {
			continue
		}

		currentRuleSet[normalizeRuleString(rule)] = true
	}

	// Build desired rule set
	desiredRuleSet := make(map[string]bool)

	for _, rule := range desiredRules {
		// Convert rule args to iptables list format for comparison
		ruleStr := mm.ruleArgsToListFormat(rule)
		desiredRuleSet[normalizeRuleString(ruleStr)] = true
	}

	// Calculate delta
	var added, removed int

	consecutiveParseFailures := 0

	// Remove rules that should not exist
	for _, rule := range currentRules {
		// Skip non-rule lines
		if len(rule) == 0 || rule[0] != '-' || (len(rule) >= 2 && rule[0:2] == "-N") {
			continue
		}

		if !desiredRuleSet[normalizeRuleString(rule)] {
			// This rule should not exist - remove it
			ruleArgs := mm.parseRuleFromListFormat(rule)
			if len(ruleArgs) > 0 {
				consecutiveParseFailures = 0

				if err := ipt.Delete("nat", masqueradeChain, ruleArgs...); err != nil {
					klog.V(3).Infof("Failed to remove %s rule: %v (rule: %s)", family, err, rule)
				} else {
					removed++
				}
			} else {
				consecutiveParseFailures++
				if consecutiveParseFailures >= maxConsecutiveParseFailures {
					klog.Warningf("Too many consecutive parse failures (%d) in %s %s chain; flushing and rebuilding",
						consecutiveParseFailures, family, masqueradeChain)

					return mm.flushAndRebuildChain(ipt, family, desiredRules)
				}
			}
		}
	}

	// Add rules that should exist
	for _, rule := range desiredRules {
		ruleStr := mm.ruleArgsToListFormat(rule)
		if !currentRuleSet[normalizeRuleString(ruleStr)] {
			// This rule should exist but doesn't - add it
			if err := ipt.Append("nat", masqueradeChain, rule...); err != nil {
				klog.V(3).Infof("Failed to add %s rule: %v", family, err)
			} else {
				added++
			}
		}
	}

	if added > 0 || removed > 0 {
		klog.V(3).Infof("Synced %s masquerade rules: %d added, %d removed", family, added, removed)
	}

	return nil
}

// flushAndRebuildChain clears the chain and re-adds all desired rules from scratch.
func (mm *MasqueradeManager) flushAndRebuildChain(ipt *iptables.IPTables, family string, desiredRules [][]string) error {
	MasqueradeChainRebuilds.Inc()

	if err := ipt.ClearChain("nat", masqueradeChain); err != nil {
		return fmt.Errorf("failed to flush %s chain %s: %w", family, masqueradeChain, err)
	}

	for _, rule := range desiredRules {
		if err := ipt.Append("nat", masqueradeChain, rule...); err != nil {
			return fmt.Errorf("failed to re-add %s rule after flush: %w", family, err)
		}
	}

	klog.V(2).Infof("Rebuilt %s %s chain with %d rules after flush", family, masqueradeChain, len(desiredRules))

	return nil
}

// buildDesiredRules constructs the list of desired iptables rules as argument slices
func (mm *MasqueradeManager) buildDesiredRules(defaultIface string, nonMasqCIDRs []string) [][]string {
	var rules [][]string

	// Rule 1: Skip traffic to local destinations
	rules = append(rules, []string{"-m", "addrtype", "--dst-type", "LOCAL", "-m", "comment", "--comment", commentPrefix + ": skip local destinations", "-j", "RETURN"})

	// Rule 2: Skip traffic to non-masquerade CIDRs
	for _, cidr := range nonMasqCIDRs {
		rules = append(rules, []string{"-d", cidr, "-m", "comment", "--comment", commentPrefix + ": skip nonMasqueradeCIDRs", "-j", "RETURN"})
	}

	// Rule 3: Masquerade traffic leaving via default gateway interface
	if defaultIface != "" {
		rules = append(rules, []string{"-o", defaultIface, "-m", "comment", "--comment", commentPrefix + ": masquerade default gateway egress", "-j", "MASQUERADE"})
	}

	return rules
}

// ruleArgsToListFormat converts rule arguments to the format returned by iptables.List()
// This is an approximation - iptables list format includes "-A CHAIN" prefix
func (mm *MasqueradeManager) ruleArgsToListFormat(args []string) string {
	// iptables.List() returns rules in format: "-A CHAIN <rule args>"
	result := "-A " + masqueradeChain
	for _, arg := range args {
		result += " " + arg
	}

	return result
}

// normalizeRuleString trims and collapses whitespace in an iptables rule string
// so that minor spacing differences do not cause false mismatches.
func normalizeRuleString(rule string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(rule)), " ")
}

// parseRuleFromListFormat extracts rule arguments from the iptables list format
// Input: "-A UNBOUNDED-MASQUERADE -m addrtype --dst-type LOCAL ..."
// Output: ["-m", "addrtype", "--dst-type", "LOCAL", ...]
func (mm *MasqueradeManager) parseRuleFromListFormat(rule string) []string {
	// Skip the "-A CHAIN" prefix
	prefix := "-A " + masqueradeChain + " "
	if len(rule) <= len(prefix) {
		return nil
	}

	if rule[:len(prefix)] != prefix {
		return nil
	}

	ruleBody := rule[len(prefix):]

	// Split by spaces, handling quoted strings
	// For simplicity, we'll use a basic split since our rules don't have complex quoting
	var args []string

	current := ""
	inQuote := false

	for _, c := range ruleBody {
		switch c {
		case '"':
			inQuote = !inQuote
		case ' ':
			if inQuote {
				current += string(c)
			} else if current != "" {
				args = append(args, current)
				current = ""
			}
		default:
			current += string(c)
		}
	}

	if current != "" {
		args = append(args, current)
	}

	return args
}

// Cleanup removes all masquerade rules installed by this manager
func (mm *MasqueradeManager) Cleanup() error {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	var errs []error

	// Cleanup IPv4 rules
	if err := mm.cleanupFamily(mm.ipt4, "IPv4"); err != nil {
		errs = append(errs, err)
	}

	// Cleanup IPv6 rules
	if mm.ipt6 != nil {
		if err := mm.cleanupFamily(mm.ipt6, "IPv6"); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("cleanup errors: %v", errs)
	}

	klog.V(2).Info("Cleaned up masquerade rules")

	return nil
}

// cleanupFamily removes rules for a specific IP family
func (mm *MasqueradeManager) cleanupFamily(ipt *iptables.IPTables, family string) error {
	if ipt == nil {
		return nil
	}

	// Remove jump from POSTROUTING
	jumpRule := []string{"-m", "comment", "--comment", commentPrefix + ": masquerade egress traffic", "-j", masqueradeChain}
	if err := ipt.DeleteIfExists("nat", "POSTROUTING", jumpRule...); err != nil {
		klog.Warningf("Failed to remove %s jump rule: %v", family, err)
	}

	// Check if chain exists before trying to clear/delete it
	exists, err := ipt.ChainExists("nat", masqueradeChain)
	if err != nil {
		return fmt.Errorf("failed to check if %s chain exists: %w", family, err)
	}

	if exists {
		// Flush the chain
		if err := ipt.ClearChain("nat", masqueradeChain); err != nil {
			klog.Warningf("Failed to flush %s chain: %v", family, err)
		}

		// Delete the chain
		if err := ipt.DeleteChain("nat", masqueradeChain); err != nil {
			klog.Warningf("Failed to delete %s chain: %v", family, err)
		}
	}

	return nil
}

// DetectDefaultGateway returns the default gateway interface name for the given IP family.
// family should be netlink.FAMILY_V4 or netlink.FAMILY_V6.
// Returns empty string if no default route is found.
func DetectDefaultGateway(family int) string {
	routes, err := netlink.RouteList(nil, family)
	if err != nil {
		klog.V(3).Infof("Failed to list routes for family %d: %v", family, err)
		return ""
	}

	for _, route := range routes {
		// Default route either has Dst == nil or Dst is 0.0.0.0/0 (IPv4) or ::/0 (IPv6)
		isDefault := false
		if route.Dst == nil {
			isDefault = true
		} else if ones, bits := route.Dst.Mask.Size(); ones == 0 && bits > 0 {
			// Mask is /0 (e.g., 0.0.0.0/0 or ::/0)
			isDefault = true
		}

		if isDefault {
			link, err := netlink.LinkByIndex(route.LinkIndex)
			if err != nil {
				klog.V(3).Infof("Failed to get link for default route: %v", err)
				continue
			}

			klog.V(4).Infof("Found default gateway interface: %s (family %d)", link.Attrs().Name, family)

			return link.Attrs().Name
		}
	}

	return ""
}

// classifyCIDRsByFamily separates CIDRs into IPv4 and IPv6 slices
func classifyCIDRsByFamily(cidrs []string) (ipv4, ipv6 []string) {
	for _, cidr := range cidrs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			klog.V(3).Infof("Invalid CIDR %s: %v", cidr, err)
			continue
		}

		if ipNet.IP.To4() != nil {
			ipv4 = append(ipv4, cidr)
		} else {
			ipv6 = append(ipv6, cidr)
		}
	}

	return ipv4, ipv6
}

// stringSlicesEqual compares two string slices for equality (order-independent)
func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	// Make copies to avoid modifying the originals
	aCopy := make([]string, len(a))
	bCopy := make([]string, len(b))

	copy(aCopy, a)
	copy(bCopy, b)
	sort.Strings(aCopy)
	sort.Strings(bCopy)

	return reflect.DeepEqual(aCopy, bCopy)
}
