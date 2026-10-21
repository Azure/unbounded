// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"

	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
)

func neighborPressureTestDeps(t *testing.T, limit int, neighbors map[int][]netlink.Neigh) nodeConfigValidatorDeps {
	t.Helper()

	return nodeConfigValidatorDeps{
		readFile: func(path string) ([]byte, error) {
			if strings.HasSuffix(path, "/gc_thresh3") {
				return fmt.Appendf(nil, "%d\n", limit), nil
			}

			if path == "/proc/sys/net/ipv4/ip_forward" || path == "/proc/sys/net/ipv6/conf/all/forwarding" {
				return []byte("1\n"), nil
			}

			return nil, os.ErrNotExist
		},
		neighList: func(index, family int) ([]netlink.Neigh, error) {
			if index != 0 || (family != netlink.FAMILY_V4 && family != netlink.FAMILY_V6) {
				t.Fatalf("unexpected neighbor dump index=%d family=%d", index, family)
			}

			return neighbors[family], nil
		},
		linkByIndex: func(index int) (netlink.Link, error) {
			attrs := netlink.LinkAttrs{Index: index, Name: "eth0"}
			if index == 2 {
				attrs.Flags = net.FlagLoopback
			}

			return &netlink.Dummy{LinkAttrs: attrs}, nil
		},
	}
}

func TestNeighborTablePressure(t *testing.T) {
	for _, tc := range []struct {
		name              string
		ipv4, ipv6, limit int
		want              []string
	}{
		{name: "empty", limit: 1024},
		{name: "below limit", ipv4: 1023, ipv6: 1023, limit: 1024},
		{name: "IPv4 at limit", ipv4: 1024, limit: 1024, want: []string{nodeErrorTypeNeighborTableFullIPv4}},
		{name: "IPv6 above limit", ipv4: 1, ipv6: 1025, limit: 1024, want: []string{nodeErrorTypeNeighborTableFullIPv6}},
		{
			name: "both exhausted", ipv4: 2000, ipv6: 2000, limit: 1024,
			want: []string{nodeErrorTypeNeighborTableFullIPv4, nodeErrorTypeNeighborTableFullIPv6},
		},
		{name: "zero hard limit", want: []string{nodeErrorTypeNeighborTableFullIPv4, nodeErrorTypeNeighborTableFullIPv6}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tables := map[int][]netlink.Neigh{
				netlink.FAMILY_V4: make([]netlink.Neigh, tc.ipv4),
				netlink.FAMILY_V6: make([]netlink.Neigh, tc.ipv6),
			}
			for _, entries := range tables {
				for i := range entries {
					entries[i] = netlink.Neigh{LinkIndex: 1, State: netlink.NUD_REACHABLE}
				}
			}

			warnings := collectNeighborTableWarnings(neighborPressureTestDeps(t, tc.limit, tables))

			var types []string
			for _, warning := range warnings {
				types = append(types, warning.Type)
				if warning.Timestamp.IsZero() || !strings.Contains(warning.Message, "reached gc_thresh3") ||
					!strings.Contains(warning.Message, fmt.Sprintf("limit %d", tc.limit)) {
					t.Fatalf("incomplete warning: %#v", warning)
				}
			}

			if !slices.Equal(types, tc.want) {
				t.Fatalf("warning types=%v, want %v", types, tc.want)
			}
		})
	}
}

func TestNeighborTableGCExemptions(t *testing.T) {
	neighbors := []netlink.Neigh{
		{LinkIndex: 1, State: netlink.NUD_REACHABLE},
		{LinkIndex: 1, State: netlink.NUD_STALE},
		{LinkIndex: 1, State: netlink.NUD_INCOMPLETE},
		{LinkIndex: 1, State: netlink.NUD_FAILED},
		{LinkIndex: 1, State: netlink.NUD_NOARP},
		{LinkIndex: 1, State: netlink.NUD_REACHABLE, FlagsExt: 1},
		{LinkIndex: 1, State: netlink.NUD_PERMANENT},
		{LinkIndex: 1, Flags: netlink.NTF_EXT_LEARNED},
		{LinkIndex: 1, FlagsExt: neighborExtValidated},
		{LinkIndex: 2, State: netlink.NUD_NOARP},
	}
	deps := neighborPressureTestDeps(t, 7, map[int][]netlink.Neigh{netlink.FAMILY_V4: neighbors})
	linkByIndex := deps.linkByIndex
	calls := make(map[int]int)
	deps.linkByIndex = func(index int) (netlink.Link, error) {
		calls[index]++
		return linkByIndex(index)
	}

	usage, err := readNeighborTableUsage("ipv4", deps)
	if err != nil || usage == nil || usage.entries != 6 || usage.limit != 7 {
		t.Fatalf("usage=%+v error=%v, want 6 GC-managed entries", usage, err)
	}

	if calls[1] != 1 || calls[2] != 1 {
		t.Fatalf("interface reads were not cached: %v", calls)
	}

	if warnings := collectNeighborTableWarnings(deps); len(warnings) != 0 {
		t.Fatalf("GC-exempt entries caused false pressure warnings: %v", warnings)
	}
}

func TestNeighborTablePressureErrors(t *testing.T) {
	for _, tc := range []struct {
		name, family, value, wantError string
		readErr, dumpErr, linkErr      error
	}{
		{name: "missing IPv6", family: "ipv6", readErr: os.ErrNotExist},
		{name: "missing IPv4", family: "ipv4", readErr: os.ErrNotExist, wantError: "read /proc/sys/net/ipv4/neigh/default/gc_thresh3"},
		{name: "read permission", family: "ipv4", readErr: os.ErrPermission, wantError: "read /proc/sys/net/ipv4/neigh/default/gc_thresh3"},
		{name: "invalid value", family: "ipv4", value: "1024junk", wantError: "invalid nonnegative integer"},
		{name: "negative value", family: "ipv4", value: "-1", wantError: "invalid nonnegative integer"},
		{name: "overflow", family: "ipv4", value: "2147483648", wantError: "invalid nonnegative integer"},
		{name: "empty value", family: "ipv4", wantError: "invalid nonnegative integer"},
		{name: "neighbor dump failure", family: "ipv4", value: "1", dumpErr: os.ErrPermission, wantError: "list ipv4 neighbors"},
		{name: "incomplete neighbor dump", family: "ipv6", value: "1", dumpErr: netlink.ErrDumpInterrupted, wantError: "list ipv6 neighbors"},
		{name: "interface failure", family: "ipv4", value: "1", linkErr: os.ErrPermission, wantError: "inspect neighbor interface"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := neighborPressureTestDeps(t, 1, nil)
			deps.readFile = func(string) ([]byte, error) { return []byte(tc.value), tc.readErr }
			deps.neighList = func(int, int) ([]netlink.Neigh, error) {
				if tc.readErr != nil || tc.value == "" {
					t.Fatal("invalid threshold must not dump neighbors")
				}

				return []netlink.Neigh{{LinkIndex: 1, State: netlink.NUD_REACHABLE}}, tc.dumpErr
			}
			deps.linkByIndex = func(int) (netlink.Link, error) { return nil, tc.linkErr }

			usage, err := readNeighborTableUsage(tc.family, deps)
			if usage != nil || (err == nil) != (tc.wantError == "") {
				t.Fatalf("usage=%+v error=%v", usage, err)
			}

			if tc.wantError != "" && !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error=%v, want %q", err, tc.wantError)
			}

			if err != nil {
				for _, original := range []error{tc.readErr, tc.dumpErr, tc.linkErr} {
					if original != nil && !errors.Is(err, original) {
						t.Fatalf("cause not preserved: %v", err)
					}
				}
			}

			if tc.wantError != "" {
				warnings := collectNeighborTableWarnings(deps)
				for _, warning := range warnings {
					if warning.Type != nodeErrorTypeNeighborTableCheckFailed || !strings.Contains(warning.Message, "check failed") {
						t.Fatalf("failed observation reported as exhaustion: %+v", warning)
					}
				}

				want := 2
				if errors.Is(tc.readErr, os.ErrNotExist) {
					want = 1
				}

				if len(warnings) != want {
					t.Fatalf("check failures were not reported: %v", warnings)
				}
			}
		})
	}
}

func neighborStatsFixture(counters ...uint64) []byte {
	data := []byte("entries allocs destroys hash_grows lookups hits res_failed rcv_probes_mcast rcv_probes_ucast periodic_gc_runs forced_gc_runs unresolved_discards table_fulls\n")
	for _, counter := range counters {
		data = fmt.Appendf(data, "00001000 0 0 0 0 0 0 0 0 0 0 0 %08x\n", counter)
	}

	return data
}

func TestParseNeighborTableFulls(t *testing.T) {
	for _, tc := range []struct {
		name      string
		data      []byte
		want      uint64
		wantError bool
	}{
		{name: "per CPU hexadecimal counters", data: neighborStatsFixture(10, 32), want: 42},
		{name: "zero counters not repeated entries", data: neighborStatsFixture(0, 0)},
		{name: "empty", wantError: true},
		{name: "header only", data: neighborStatsFixture(), wantError: true},
		{name: "missing column", data: []byte("entries allocs\n0 0\n"), wantError: true},
		{name: "incomplete row", data: []byte("entries table_fulls\n0\n"), wantError: true},
		{name: "malformed counter", data: []byte("entries table_fulls\n0 bad-value\n"), wantError: true},
		{name: "overflow", data: neighborStatsFixture(^uint64(0), 1), wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseNeighborTableFulls(tc.data)
			if got != tc.want || (err != nil) != tc.wantError {
				t.Fatalf("total=%d error=%v, want %d error=%v", got, err, tc.want, tc.wantError)
			}
		})
	}
}

func TestNeighborOverflowWarnings(t *testing.T) {
	state := &wireGuardState{}
	counters := [2]uint64{5, 6}

	var readErr error

	read := func(path string) ([]byte, error) {
		if readErr != nil {
			return nil, readErr
		}

		switch path {
		case "/proc/net/stat/arp_cache":
			return neighborStatsFixture(counters[0]), nil
		case "/proc/net/stat/ndisc_cache":
			return neighborStatsFixture(counters[1]), nil
		default:
			t.Fatalf("unexpected counter path %s", path)
			return nil, nil
		}
	}
	if got := collectNeighborOverflowWarnings(state, read); len(got) != 0 {
		t.Fatalf("historical failures must establish a baseline, not warn: %v", got)
	}

	counters = [2]uint64{10, 7}

	warnings := collectNeighborOverflowWarnings(state, read)
	if len(warnings) != 2 || warnings[0].Type != nodeErrorTypeNeighborTableOverflowIPv4 ||
		!strings.Contains(warnings[0].Message, "5 new allocation failures") ||
		warnings[1].Type != nodeErrorTypeNeighborTableOverflowIPv6 ||
		!strings.Contains(warnings[1].Message, "1 new allocation failures") {
		t.Fatalf("new kernel allocation failures were not reported: %v", warnings)
	}

	if got := collectNeighborOverflowWarnings(state, read); len(got) != 0 {
		t.Fatalf("unchanged counters must not repeat an event: %v", got)
	}

	readErr = os.ErrPermission

	warnings = collectNeighborOverflowWarnings(state, read)
	if len(warnings) != 2 || warnings[0].Type != nodeErrorTypeNeighborTableCheckFailed ||
		!strings.Contains(warnings[0].Message, os.ErrPermission.Error()) {
		t.Fatalf("counter read failures were hidden: %v", warnings)
	}

	readErr = os.ErrNotExist

	if got := collectNeighborOverflowWarnings(state, read); len(got) != 0 {
		t.Fatalf("unsupported counter files must be skipped: %v", got)
	}

	readErr = nil
	counters = [2]uint64{12, 7}

	if got := collectNeighborOverflowWarnings(state, read); len(got) != 1 || !strings.Contains(got[0].Message, "2 new allocation failures") {
		t.Fatalf("failed samples lost the counter baseline: %v", got)
	}

	counters = [2]uint64{}

	if got := collectNeighborOverflowWarnings(state, read); len(got) != 0 {
		t.Fatalf("counter reset must not cause an unsigned-wrap warning: %v", got)
	}
}

func TestNeighborTableWarningSurvivesAutomaticGrowthAndReachesControllerStatus(t *testing.T) {
	tuner := newNeighborGCTuner()

	values := fakeNeighborSysctls(t, tuner)
	for path := range values {
		values[path] = 2
	}

	tables := map[int][]netlink.Neigh{
		netlink.FAMILY_V4: {{LinkIndex: 1, State: netlink.NUD_REACHABLE}, {LinkIndex: 1, State: netlink.NUD_REACHABLE}},
		netlink.FAMILY_V6: {{LinkIndex: 1, State: netlink.NUD_REACHABLE}, {LinkIndex: 1, State: netlink.NUD_REACHABLE}},
	}
	deps := neighborPressureTestDeps(t, 2, tables)
	readFile := deps.readFile
	fulls := uint64(5)
	deps.readFile = func(path string) ([]byte, error) {
		if _, ok := values[path]; ok {
			return tuner.readFile(path)
		}

		if strings.HasPrefix(path, "/proc/net/stat/") {
			return neighborStatsFixture(fulls), nil
		}

		return readFile(path)
	}
	original := defaultNodeConfigValidatorDeps
	defaultNodeConfigValidatorDeps = deps

	t.Cleanup(func() { defaultNodeConfigValidatorDeps = original })

	state := &wireGuardState{nodeErrors: []NodeError{{Type: "unrelated", Message: "preserve me"}}}
	refresh := func() { refreshNodeConfigurationProblems(nil, "", state, time.Minute) }
	refresh()
	refresh()

	if len(state.nodeErrors) != 3 {
		t.Fatalf("warnings missing or duplicated: %v", state.nodeErrors)
	}

	if err := applyNeighborGCThresholds([2]int{2, 2}, tuner.readFile, tuner.writeFile); err != nil {
		t.Fatal(err)
	}

	refresh()

	if len(state.nodeErrors) != 3 {
		t.Fatalf("automatic growth cleared warnings before publication: %v", state.nodeErrors)
	}

	fulls++

	refresh()

	if len(state.nodeErrors) != 5 {
		t.Fatalf("kernel overflow events were not retained alongside occupancy warnings: %v", state.nodeErrors)
	}

	server := &nodeStatusServer{
		cfg:   &config{NodeName: "node-a", WireGuardPort: 51820, WireGuardInterfacePrefix: "wg"},
		state: state,
	}
	for _, warnings := range [][]*statusproto.NodeError{
		nodeStatusToProto(server.getNodeStatus()).GetNodeErrors(),
		nodeSummaryToProto(server.getNodeSummary()).GetNodeErrors(),
	} {
		for _, kind := range []string{
			nodeErrorTypeNeighborTableFullIPv4, nodeErrorTypeNeighborTableFullIPv6,
			nodeErrorTypeNeighborTableOverflowIPv4, nodeErrorTypeNeighborTableOverflowIPv6,
		} {
			if !slices.ContainsFunc(warnings, func(warning *statusproto.NodeError) bool {
				return warning.GetType() == kind && strings.Contains(warning.GetMessage(), "gc_thresh3") && warning.GetTimestampNs() != 0
			}) {
				t.Fatalf("warning %s absent from controller status payload: %v", kind, warnings)
			}
		}
	}

	for i := range state.nodeErrors {
		if isNeighborTablePressureWarning(state.nodeErrors[i].Type) {
			state.nodeErrors[i].Timestamp = time.Now().Add(-2 * time.Minute)
		}
	}

	refresh()

	if len(state.nodeErrors) != 1 || state.nodeErrors[0].Type != "unrelated" {
		t.Fatalf("recovered warnings did not expire independently of other errors: %v", state.nodeErrors)
	}
}
