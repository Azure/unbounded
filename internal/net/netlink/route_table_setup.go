// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netlink

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
	"k8s.io/klog/v2"
)

// Package-level netlink rule function vars for testability.
var (
	netlinkRuleList = netlink.RuleList
	netlinkRuleAdd  = netlink.RuleAdd
	netlinkRuleDel  = netlink.RuleDel
)

// EnsureRtTablesEntry ensures a route table entry with the given tableID and
// name exists in /etc/iproute2/rt_tables. It is idempotent: if an entry for
// the tableID already exists (even under a different name), no changes are made.
func EnsureRtTablesEntry(tableID int, name string) error {
	if err := validateInterfaceName(name); err != nil {
		return fmt.Errorf("refusing to write rt_tables entry: %w", err)
	}

	dir := filepath.Dir(rtTablesPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	if _, err := os.Stat(rtTablesPath); os.IsNotExist(err) {
		defaultContent := "#\n# reserved values\n#\n255\tlocal\n254\tmain\n253\tdefault\n0\tunspec\n#\n# local\n#\n"
		if err := atomicWriteFile(rtTablesPath, []byte(defaultContent), 0o644); err != nil {
			return fmt.Errorf("failed to create %s: %w", rtTablesPath, err)
		}

		klog.V(2).Infof("Created %s with default content", rtTablesPath)
	}

	file, err := os.Open(rtTablesPath)
	if err != nil {
		return fmt.Errorf("failed to open %s: %w", rtTablesPath, err)
	}

	scanner := bufio.NewScanner(file)
	tableExists := false
	existingName := ""

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) >= 2 {
			if num, err := strconv.Atoi(fields[0]); err == nil && num == tableID {
				tableExists = true
				existingName = fields[1]

				break
			}
		}
	}

	_ = file.Close() //nolint:errcheck

	if tableExists {
		if existingName != name {
			klog.V(2).Infof("Route table %d exists with name %s (wanted %s), reusing", tableID, existingName, name)
		} else {
			klog.V(3).Infof("Route table %d already exists for %s", tableID, name)
		}

		return nil
	}

	existing, err := os.ReadFile(rtTablesPath)
	if err != nil {
		return fmt.Errorf("failed to read %s for append: %w", rtTablesPath, err)
	}

	entry := fmt.Sprintf("%d\t%s\n", tableID, name)

	updated := string(existing)
	if len(updated) > 0 && !strings.HasSuffix(updated, "\n") {
		updated += "\n"
	}

	updated += entry
	if err := atomicWriteFile(rtTablesPath, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("failed to write route table entry: %w", err)
	}

	klog.V(2).Infof("Added route table entry: %d %s", tableID, name)

	return nil
}

// EnsureIPRule ensures an ip rule exists for policy routing in both IPv4 and
// IPv6. When fwmark is 0, an unconditional lookup rule is created (no mark
// matching). When fwmark > 0, a fwmark-based rule is created with the given
// mask; a zero fwmask is treated as 0xffffffff. Stale rules with the same
// mark+table but mismatched mask or priority are removed automatically.
func EnsureIPRule(tableID, priority int, fwmark, fwmask uint32) error {
	effectiveMask := fwmask
	if fwmark > 0 && effectiveMask == 0 {
		effectiveMask = 0xffffffff
	}

	ensureForFamily := func(family int, familyName string) error {
		rules, err := netlinkRuleList(family)
		if err != nil {
			if family == netlink.FAMILY_V4 {
				return fmt.Errorf("failed to list %s rules: %w", familyName, err)
			}

			klog.V(3).Infof("Failed to list %s rules: %v", familyName, err)

			return nil
		}

		ruleExists := false
		staleRules := make([]netlink.Rule, 0)

		for _, r := range rules {
			if r.Table != tableID {
				continue
			}

			if fwmark > 0 {
				if r.Mark != fwmark {
					continue
				}

				maskMatches := (r.Mask == nil && effectiveMask == 0xffffffff) ||
					(r.Mask != nil && *r.Mask == effectiveMask)
				if maskMatches && r.Priority == priority {
					ruleExists = true
					continue
				}

				staleRules = append(staleRules, r)
			} else {
				// Unconditional rule: match by table + priority with no mark.
				if r.Mark != 0 {
					continue
				}

				if r.Priority == priority {
					ruleExists = true
					continue
				}
				// Do not treat other unconditional rules as stale -- they
				// may belong to a different subsystem.
			}
		}

		for _, stale := range staleRules {
			s := stale
			if delErr := netlinkRuleDel(&s); delErr != nil {
				klog.V(4).Infof("Failed deleting stale %s ip rule before ensure mark=%d table=%d priority=%d: %v",
					familyName, stale.Mark, stale.Table, stale.Priority, delErr)

				continue
			}

			klog.V(2).Infof("Deleted stale %s ip rule before ensure mark=%d table=%d priority=%d",
				familyName, stale.Mark, stale.Table, stale.Priority)
		}

		if ruleExists {
			return nil
		}

		rule := netlink.NewRule()
		rule.Table = tableID

		rule.Priority = priority
		if fwmark > 0 {
			rule.Mark = fwmark
			m := effectiveMask
			rule.Mask = &m
		}

		if family == netlink.FAMILY_V6 {
			rule.Family = netlink.FAMILY_V6
		}

		if addErr := netlinkRuleAdd(rule); addErr != nil {
			if !strings.Contains(addErr.Error(), "file exists") {
				if family == netlink.FAMILY_V4 {
					return fmt.Errorf("failed to add %s ip rule: %w", familyName, addErr)
				}

				klog.V(3).Infof("Failed to add %s ip rule (may not be needed): %v", familyName, addErr)
			}
		} else {
			if fwmark > 0 {
				klog.V(2).Infof("Added %s ip rule: fwmark %d table %d priority %d", familyName, fwmark, tableID, priority)
			} else {
				klog.V(2).Infof("Added %s ip rule: lookup table %d priority %d", familyName, tableID, priority)
			}
		}

		return nil
	}

	if err := ensureForFamily(netlink.FAMILY_V4, "IPv4"); err != nil {
		return err
	}

	return ensureForFamily(netlink.FAMILY_V6, "IPv6")
}

// RemoveIPRule removes ip rules matching the given table, priority, and fwmark
// for both IPv4 and IPv6. When fwmark is 0, only unconditional (no-mark)
// rules are matched. A zero fwmask is treated as 0xffffffff when fwmark > 0.
func RemoveIPRule(tableID, priority int, fwmark, fwmask uint32) error {
	effectiveMask := fwmask
	if fwmark > 0 && effectiveMask == 0 {
		effectiveMask = 0xffffffff
	}

	removeForFamily := func(family int, familyName string) error {
		rules, err := netlinkRuleList(family)
		if err != nil {
			if family == netlink.FAMILY_V4 {
				return fmt.Errorf("failed to list %s rules: %w", familyName, err)
			}

			klog.V(3).Infof("Failed to list %s rules: %v", familyName, err)

			return nil
		}

		for _, r := range rules {
			if r.Table != tableID || r.Priority != priority {
				continue
			}

			if fwmark > 0 {
				if r.Mark != fwmark {
					continue
				}

				maskOK := (r.Mask == nil && effectiveMask == 0xffffffff) ||
					(r.Mask != nil && *r.Mask == effectiveMask)
				if !maskOK {
					continue
				}
			} else {
				if r.Mark != 0 {
					continue
				}
			}

			toDelete := r
			if err := netlinkRuleDel(&toDelete); err != nil {
				klog.V(4).Infof("Failed deleting %s ip rule table=%d priority=%d: %v",
					familyName, r.Table, r.Priority, err)

				continue
			}

			klog.V(2).Infof("Removed %s ip rule table=%d priority=%d", familyName, r.Table, r.Priority)
		}

		return nil
	}

	if err := removeForFamily(netlink.FAMILY_V4, "IPv4"); err != nil {
		return err
	}

	return removeForFamily(netlink.FAMILY_V6, "IPv6")
}

// FlushRouteTable removes all routes in the specified routing table for both
// IPv4 and IPv6.
func FlushRouteTable(tableID int) error {
	flushFamily := func(family int) error {
		routes, err := netlink.RouteListFiltered(family, &netlink.Route{Table: tableID}, netlink.RT_FILTER_TABLE)
		if err != nil {
			return err
		}

		for _, route := range routes {
			r := route
			if err := netlink.RouteDel(&r); err != nil {
				klog.V(4).Infof("Failed removing route from table %d: %v", tableID, err)
			}
		}

		return nil
	}

	if err := flushFamily(netlink.FAMILY_V4); err != nil {
		return err
	}

	return flushFamily(netlink.FAMILY_V6)
}

// ListRoutesInTable returns all routes in the specified routing table for
// both IPv4 and IPv6.
func ListRoutesInTable(tableID int) ([]netlink.Route, error) {
	filter := &netlink.Route{Table: tableID}

	v4Routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, filter, netlink.RT_FILTER_TABLE)
	if err != nil {
		return nil, fmt.Errorf("failed to list IPv4 routes in table %d: %w", tableID, err)
	}

	v6Routes, err := netlink.RouteListFiltered(netlink.FAMILY_V6, filter, netlink.RT_FILTER_TABLE)
	if err != nil {
		return nil, fmt.Errorf("failed to list IPv6 routes in table %d: %w", tableID, err)
	}

	allRoutes := make([]netlink.Route, 0, len(v4Routes)+len(v6Routes))
	allRoutes = append(allRoutes, v4Routes...)
	allRoutes = append(allRoutes, v6Routes...)

	return allRoutes, nil
}
