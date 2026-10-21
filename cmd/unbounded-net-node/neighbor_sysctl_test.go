// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestNeighborGCThresholds(t *testing.T) {
	for _, tc := range []struct {
		nodes int
		want  [3]int
	}{
		{0, [3]int{}},
		{1, [3]int{2, 2, 2}},
		{9, [3]int{10, 11, 12}},
		{10, [3]int{11, 12, 13}},
		{11, [3]int{13, 14, 15}},
		{2000, [3]int{2200, 2400, 2600}},
		{2001, [3]int{2202, 2402, 2602}},
		{maxExpectedNeighborNodes, [3]int{1817101547, 1982292597, 2147483647}},
	} {
		t.Run(strconv.Itoa(tc.nodes), func(t *testing.T) {
			got, err := neighborGCThresholds(tc.nodes)
			if err != nil || got != tc.want {
				t.Fatalf("neighborGCThresholds(%d) = %v, %v; want %v", tc.nodes, got, err, tc.want)
			}
		})
	}
}

func TestApplyNeighborGCThresholds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		nodes   int
		current [3]int
		want    [3]int
		writes  []string
	}{
		{"disabled", 0, [3]int{128, 512, 1024}, [3]int{128, 512, 1024}, nil},
		{"raise", 2000, [3]int{128, 512, 1024}, [3]int{2200, 2400, 2600}, []string{"gc_thresh3", "gc_thresh2", "gc_thresh1"}},
		{"odd", 2001, [3]int{128, 512, 1024}, [3]int{2202, 2402, 2602}, []string{"gc_thresh3", "gc_thresh2", "gc_thresh1"}},
		{"already higher", 2000, [3]int{4096, 8192, 16384}, [3]int{4096, 8192, 16384}, nil},
		{"already equal", 2000, [3]int{2200, 2400, 2600}, [3]int{2200, 2400, 2600}, nil},
		{"mixed", 2000, [3]int{128, 4096, 8192}, [3]int{2200, 4096, 8192}, []string{"gc_thresh1"}},
		{"preserve ordering", 2000, [3]int{6000, 7000, 1024}, [3]int{6000, 7000, 7000}, []string{"gc_thresh3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := [6]int{tc.current[0], tc.current[1], tc.current[2], tc.current[0], tc.current[1], tc.current[2]}
			want := [6]int{tc.want[0], tc.want[1], tc.want[2], tc.want[0], tc.want[1], tc.want[2]}
			wantWrites := append(append([]string(nil), tc.writes...), tc.writes...)

			var writes []string

			readFile := func(path string) ([]byte, error) {
				if tc.nodes == 0 {
					t.Fatal("disabled configuration must not read sysctls")
				}

				i := neighborThresholdIndex(t, path)

				return []byte(fmt.Sprintf("%d\n", values[i])), nil
			}
			writeFile := func(path string, data []byte, _ os.FileMode) error {
				i := neighborThresholdIndex(t, path)

				value, err := strconv.Atoi(strings.TrimSpace(string(data)))
				if err != nil {
					t.Fatal(err)
				}

				if value <= values[i] {
					t.Fatalf("unexpected non-increase of %s: %d -> %d", path, values[i], value)
				}

				values[i] = value

				writes = append(writes, filepath.Base(path))

				return nil
			}

			for range 2 {
				if err := applyNeighborGCThresholds(tc.nodes, readFile, writeFile); err != nil {
					t.Fatal(err)
				}
			}

			if values != want || !reflect.DeepEqual(writes, wantWrites) {
				t.Fatalf("values=%v writes=%v; want values=%v writes=%v", values, writes, want, wantWrites)
			}
		})
	}
}

func neighborThresholdIndex(t *testing.T, path string) int {
	t.Helper()

	for familyIndex, family := range []string{"ipv4", "ipv6"} {
		for i := range 3 {
			if path == "/proc/sys/net/"+family+"/neigh/default/gc_thresh"+strconv.Itoa(i+1) {
				return familyIndex*3 + i
			}
		}
	}

	t.Fatalf("unexpected sysctl path %q", path)

	return 0
}

func TestApplyNeighborGCThresholdsIPv6(t *testing.T) {
	for _, tc := range []struct {
		name       string
		value      string
		readIndex  int
		readErr    error
		writeErr   error
		wantError  string
		wantWrites int
	}{
		{name: "raise IPv6 independently", value: "128", wantWrites: 3},
		{name: "preserve higher IPv6 values", value: "16384"},
		{name: "IPv6 unavailable", readErr: os.ErrNotExist},
		{name: "IPv6 permission denied", readErr: os.ErrPermission, wantError: "read /proc/sys/net/ipv6/neigh/default/gc_thresh1"},
		{name: "partial IPv6 table", value: "128", readIndex: 1, readErr: os.ErrNotExist, wantError: "read /proc/sys/net/ipv6/neigh/default/gc_thresh2"},
		{name: "malformed IPv6", value: "bad", wantError: "invalid nonnegative integer in /proc/sys/net/ipv6/neigh/default/gc_thresh1"},
		{name: "IPv6 write denied", value: "128", writeErr: os.ErrPermission, wantError: "raise /proc/sys/net/ipv6/neigh/default/gc_thresh3", wantWrites: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			readFile := func(path string) ([]byte, error) {
				i := neighborThresholdIndex(t, path)
				if i < 3 {
					return []byte("8192\n"), nil
				}

				if i-3 == tc.readIndex && tc.readErr != nil {
					return nil, tc.readErr
				}

				return []byte(tc.value), nil
			}
			writeFile := func(path string, data []byte, _ os.FileMode) error {
				i := neighborThresholdIndex(t, path)
				if i != 5-writes {
					t.Fatalf("unexpected write to %s; IPv4 must stay unchanged and IPv6 must be raised in reverse order", path)
				}

				want := []string{"2200\n", "2400\n", "2600\n"}[i-3]
				if string(data) != want {
					t.Fatalf("%s: wrote %q, want %q", path, data, want)
				}

				writes++

				return tc.writeErr
			}

			err := applyNeighborGCThresholds(2000, readFile, writeFile)
			if tc.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %v; want %q", err, tc.wantError)
			}

			if tc.wantError != "" && tc.readErr != nil && !errors.Is(err, tc.readErr) {
				t.Fatalf("read error not preserved: %v", err)
			}

			if tc.writeErr != nil && !errors.Is(err, tc.writeErr) {
				t.Fatalf("write error not preserved: %v", err)
			}

			if writes != tc.wantWrites {
				t.Fatalf("writes = %d, want %d", writes, tc.wantWrites)
			}
		})
	}
}

func TestApplyNeighborGCThresholdsErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		nodes     int
		value     string
		readErr   error
		writeErr  error
		wantError string
	}{
		{name: "negative count", nodes: -1, wantError: "node.expectedNodes"},
		{name: "overflow", nodes: maxExpectedNeighborNodes + 1, wantError: "node.expectedNodes"},
		{name: "missing sysctl", nodes: 2000, readErr: os.ErrNotExist, wantError: "read /proc/sys/net/ipv4/neigh/default/gc_thresh1"},
		{name: "read denied", nodes: 2000, readErr: os.ErrPermission, wantError: "read /proc/sys/net/ipv4/neigh/default/gc_thresh1"},
		{name: "malformed", nodes: 2000, value: "128junk", wantError: "invalid nonnegative integer"},
		{name: "negative sysctl", nodes: 2000, value: "-1", wantError: "invalid nonnegative integer"},
		{name: "oversized sysctl", nodes: 2000, value: "2147483648", wantError: "invalid nonnegative integer"},
		{name: "empty sysctl", nodes: 2000, wantError: "invalid nonnegative integer"},
		{name: "write denied", nodes: 2000, value: "128", writeErr: os.ErrPermission, wantError: "raise /proc/sys/net/ipv4/neigh/default/gc_thresh3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			readFile := func(path string) ([]byte, error) {
				neighborThresholdIndex(t, path)

				if tc.nodes < 0 || tc.nodes > maxExpectedNeighborNodes {
					t.Fatal("invalid count must not read sysctls")
				}

				return []byte(tc.value), tc.readErr
			}
			writeFile := func(string, []byte, os.FileMode) error {
				writes++
				return tc.writeErr
			}

			err := applyNeighborGCThresholds(tc.nodes, readFile, writeFile)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %v; want %q", err, tc.wantError)
			}

			if tc.readErr != nil && !errors.Is(err, tc.readErr) {
				t.Fatalf("read error not preserved: %v", err)
			}

			if tc.writeErr != nil {
				if !errors.Is(err, tc.writeErr) || writes != 1 {
					t.Fatalf("write failure not propagated immediately: err=%v writes=%d", err, writes)
				}
			} else if writes != 0 {
				t.Fatalf("wrote %d sysctls before validating inputs", writes)
			}
		})
	}
}

func TestNodeExpectedNodesConfig(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, flag string
		want             int
		invalid          bool
	}{
		{name: "unset", yaml: "node: {}"},
		{name: "disabled", yaml: "node:\n  expectedNodes: 0"},
		{name: "negative", yaml: "node:\n  expectedNodes: -1", invalid: true},
		{name: "overflow", yaml: "node:\n  expectedNodes: 1651910498", invalid: true},
		{name: "noninteger", yaml: "node:\n  expectedNodes: many", invalid: true},
		{name: "invalid flag", yaml: "node: {}", flag: "-1", invalid: true},
		{name: "flag disables", yaml: "node:\n  expectedNodes: 2000", flag: "0"},
		{name: "flag overrides invalid config", yaml: "node:\n  expectedNodes: -1", flag: "2000", want: 2000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}

			cfg := &config{
				ConfigFile: path, GeneveInterfaceName: "geneve0", VXLANInterfaceName: "vxlan0",
				IPIPInterfaceName: "ipip0", WireGuardInterfacePrefix: "wg",
			}

			cmd := newNodeConfigTestCommand(cfg)
			if tc.flag != "" {
				if err := cmd.Flags().Set("expected-nodes", tc.flag); err != nil {
					t.Fatal(err)
				}
			}

			err := applyNodeRuntimeConfig(cmd, cfg)
			if (err != nil) != tc.invalid {
				t.Fatalf("applyNodeRuntimeConfig() = %v", err)
			}

			if !tc.invalid && cfg.ExpectedNodes != tc.want {
				t.Fatalf("expectedNodes=%d, want %d", cfg.ExpectedNodes, tc.want)
			}
		})
	}
}
