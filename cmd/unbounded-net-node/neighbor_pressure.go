// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"k8s.io/klog/v2"
)

const (
	nodeErrorTypeNeighborTableFullIPv4     = "neighborTableFullIPv4"
	nodeErrorTypeNeighborTableFullIPv6     = "neighborTableFullIPv6"
	nodeErrorTypeNeighborTableOverflowIPv4 = "neighborTableOverflowIPv4"
	nodeErrorTypeNeighborTableOverflowIPv6 = "neighborTableOverflowIPv6"
	nodeErrorTypeNeighborTableCheckFailed  = "neighborTableCheckFailed"

	// NTF_EXT_EXT_VALIDATED in Linux's NDA_FLAGS_EXT exempts entries from GC.
	neighborExtValidated = 1 << 2
)

type neighborTableUsage struct {
	entries int
	limit   int
}

func isNeighborTablePressureWarning(kind string) bool {
	switch kind {
	case nodeErrorTypeNeighborTableFullIPv4, nodeErrorTypeNeighborTableFullIPv6,
		nodeErrorTypeNeighborTableOverflowIPv4, nodeErrorTypeNeighborTableOverflowIPv6:
		return true
	default:
		return false
	}
}

func collectNeighborOverflowWarnings(state *wireGuardState, readFile func(string) ([]byte, error)) []NodeError {
	var warnings []NodeError

	for i, table := range []string{"arp_cache", "ndisc_cache"} {
		path := "/proc/net/stat/" + table

		data, err := readFile(path)
		if errors.Is(err, os.ErrNotExist) {
			klog.V(4).Infof("Skipping unavailable neighbor overflow counter %s: %v", path, err)
			continue
		}

		var fulls uint64
		if err == nil {
			fulls, err = parseNeighborTableFulls(data)
		}

		if err != nil {
			warnings = append(warnings, NodeError{
				Type:    nodeErrorTypeNeighborTableCheckFailed,
				Message: fmt.Sprintf("read neighbor overflow counter %s: %v", path, err),
			})

			continue
		}

		state.mu.Lock()
		previous := state.neighborTableFulls[i]
		state.neighborTableFulls[i] = new(fulls)
		state.mu.Unlock()

		if previous != nil && fulls > *previous {
			warnings = append(warnings, NodeError{
				Type: []string{nodeErrorTypeNeighborTableOverflowIPv4, nodeErrorTypeNeighborTableOverflowIPv6}[i],
				Message: fmt.Sprintf("%s neighbor table exhausted gc_thresh3: %d new allocation failures (table_fulls %d -> %d)",
					[]string{"ipv4", "ipv6"}[i], fulls-*previous, *previous, fulls),
				Timestamp: time.Now(),
			})
		}
	}

	return warnings
}

func parseNeighborTableFulls(data []byte) (uint64, error) {
	var (
		header []string
		total  uint64
	)

	rows := 0
	column := -1

	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		if header == nil {
			header = fields

			column = slices.Index(header, "table_fulls")
			if column < 0 {
				return 0, errors.New("missing table_fulls column")
			}

			continue
		}

		if len(fields) != len(header) {
			return 0, fmt.Errorf("invalid neighbor statistics row: %q", line)
		}

		value, err := strconv.ParseUint(fields[column], 16, 64)
		if err != nil || value > math.MaxUint64-total {
			return 0, fmt.Errorf("invalid table_fulls counter: %q", fields[column])
		}

		total += value
		rows++
	}

	if rows == 0 {
		return 0, errors.New("neighbor statistics contain no CPU rows")
	}

	return total, nil
}

func collectNeighborTableWarnings(deps nodeConfigValidatorDeps) []NodeError {
	var warnings []NodeError

	for i, family := range []string{"ipv4", "ipv6"} {
		usage, err := readNeighborTableUsage(family, deps)
		if err != nil {
			warnings = append(warnings, NodeError{
				Type:    nodeErrorTypeNeighborTableCheckFailed,
				Message: fmt.Sprintf("%s neighbor table check failed: %v", family, err),
			})

			continue
		}

		if usage == nil || usage.entries < usage.limit {
			continue
		}

		warnings = append(warnings, NodeError{
			Type: []string{nodeErrorTypeNeighborTableFullIPv4, nodeErrorTypeNeighborTableFullIPv6}[i],
			Message: fmt.Sprintf("%s neighbor table reached gc_thresh3: %d GC-managed entries, limit %d (net.%s.neigh.default.gc_thresh3); new neighbor allocations may fail",
				family, usage.entries, usage.limit, family),
			Timestamp: time.Now(),
		})
	}

	return warnings
}

func readNeighborTableUsage(family string, deps nodeConfigValidatorDeps) (*neighborTableUsage, error) {
	if deps.readFile == nil || deps.neighList == nil || deps.linkByIndex == nil {
		return nil, errors.New("neighbor table check dependencies are unavailable")
	}

	path := "/proc/sys/net/" + family + "/neigh/default/gc_thresh3"

	data, err := deps.readFile(path)
	if err != nil {
		if family == "ipv6" && errors.Is(err, os.ErrNotExist) {
			klog.V(4).Infof("Skipping IPv6 neighbor table check: %s is unavailable", path)
			return nil, nil
		}

		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	limit, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 32)
	if err != nil || limit < 0 {
		return nil, fmt.Errorf("invalid nonnegative integer in %s: %q", path, strings.TrimSpace(string(data)))
	}

	addressFamily := netlink.FAMILY_V4
	if family == "ipv6" {
		addressFamily = netlink.FAMILY_V6
	}

	neighbors, err := deps.neighList(0, addressFamily)
	if err != nil {
		return nil, fmt.Errorf("list %s neighbors: %w", family, err)
	}

	usage := &neighborTableUsage{limit: int(limit)}
	loopback := make(map[int]bool)

	for _, neighbor := range neighbors {
		if neighbor.State&netlink.NUD_PERMANENT != 0 || neighbor.Flags&netlink.NTF_EXT_LEARNED != 0 ||
			neighbor.FlagsExt&neighborExtValidated != 0 {
			continue
		}

		isLoopback, known := loopback[neighbor.LinkIndex]
		if !known {
			link, err := deps.linkByIndex(neighbor.LinkIndex)
			if err != nil {
				return nil, fmt.Errorf("inspect neighbor interface %d: %w", neighbor.LinkIndex, err)
			}

			isLoopback = link.Attrs().Flags&net.FlagLoopback != 0
			loopback[neighbor.LinkIndex] = isLoopback
		}

		if !isLoopback {
			usage.entries++
		}
	}

	return usage, nil
}
