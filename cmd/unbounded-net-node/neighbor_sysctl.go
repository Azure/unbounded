// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"k8s.io/klog/v2"
)

const maxExpectedNeighborNodes = math.MaxInt32 * 5 / 6

func neighborGCThresholds(expectedNodes int) ([3]int, error) {
	if expectedNodes < 0 || expectedNodes > maxExpectedNeighborNodes {
		return [3]int{}, fmt.Errorf("node.expectedNodes must be between 0 and %d", maxExpectedNeighborNodes)
	}

	nodes := int64(expectedNodes)

	return [3]int{expectedNodes, int((nodes*11 + 9) / 10), int((nodes*6 + 4) / 5)}, nil
}

func applyNeighborGCThresholds(expectedNodes int, readFile func(string) ([]byte, error), writeFile func(string, []byte, os.FileMode) error) error {
	thresholds, err := neighborGCThresholds(expectedNodes)
	if err != nil {
		return err
	}

	if expectedNodes == 0 {
		return nil
	}

	// Linux exposes these table-wide limits only under default, not per interface.
	const prefix = "/proc/sys/net/ipv4/neigh/default/gc_thresh"

	var current [3]int

	for i := range thresholds {
		path := prefix + strconv.Itoa(i+1)

		data, err := readFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}

		value, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 32)
		if err != nil || value < 0 {
			return fmt.Errorf("invalid nonnegative integer in %s: %q", path, strings.TrimSpace(string(data)))
		}

		current[i] = int(value)

		thresholds[i] = max(thresholds[i], current[i])
		if i > 0 {
			thresholds[i] = max(thresholds[i], thresholds[i-1])
		}
	}

	// Raise the hard limit first so intermediate values remain ordered.
	for i := len(thresholds) - 1; i >= 0; i-- {
		if current[i] >= thresholds[i] {
			continue
		}

		path := prefix + strconv.Itoa(i+1)
		if err := writeFile(path, []byte(strconv.Itoa(thresholds[i])+"\n"), 0o644); err != nil {
			return fmt.Errorf("raise %s from %d to %d: %w", path, current[i], thresholds[i], err)
		}

		klog.Infof("Raised %s from %d to %d (expected nodes: %d)", path, current[i], thresholds[i], expectedNodes)
	}

	return nil
}
