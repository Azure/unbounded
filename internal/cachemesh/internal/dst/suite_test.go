// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package dst

import (
	"fmt"
	"os"
	"runtime"
	"slices"
	"testing"
	"time"
)

// shorten trims a scenario to faults starting before warmup+window and
// recomputes its duration.
func shorten(sc Scenario, window, settle int64) Scenario {
	limit := sc.Warmup + window
	sc.Faults = slices.DeleteFunc(slices.Clone(sc.Faults), func(f Fault) bool { return f.Start > limit })

	end := sc.Warmup
	for i := range sc.Faults {
		f := &sc.Faults[i]
		f.Duration = min(f.Duration, window)
		end = max(end, f.End())
	}

	sc.Settle = settle
	sc.Duration = end + settle

	return sc
}

type dstCase struct {
	name     string
	sc       Scenario
	prebuilt bool
	twice    bool
}

type dstResult struct {
	trace uint64
	steps uint64
	cov   *Coverage
	err   error
	stats string
}

func runCase(c dstCase) dstResult {
	cov := NewCoverage()
	dp := NewDataPlane(dataPlaneConfig(c))
	cl := NewCluster(ClusterConfig{Scenario: c.sc, Prebuilt: c.prebuilt, Coverage: cov, Workload: dp, Node: dp.ConfigureNode})
	err := cl.Run()

	return dstResult{
		trace: cl.Trace.Sum(), steps: cl.Sim.Steps(), cov: cov, err: err,
		stats: fmt.Sprintf("deltas=%d net=%+v %s\n%s", cl.Stats.Get("deltas"), cl.Net.Stats(), cl.ControlStats(), dp.Summary()),
	}
}

// requiredCoverage lists states the default seed set must reach.
var requiredCoverage = []string{
	"elect-candidate", "elect-leader", "elect-lottery", "elect-follower",
	"lease-op-get", "lease-op-get-cached", "lease-op-create", "lease-op-update",
	"msg-Ping", "msg-Broadcast", "msg-CatchUpRequest", "msg-Routed",
	"catchup-deltas", "catchup-snapshot", "catchup-empty",
	"node-leader-input-Join", "node-leader-input-Leave", "node-leader-input-Suspect", "node-leader-input-Invalidate",
	"node-join-sent", "node-suspect-sent", "node-term-start", "node-relay", "node-ahead-queued",
	"node-lease-seed-read", "node-bootstrap-candidate", "node-rejoin",
	"leader-bootstrap", "leader-admit", "leader-promote-Active", "leader-promote-Draining",
	"leader-remove-1", "leader-remove-2", "leader-remove-3",
	"leader-damped", "leader-reshape", "leader-cell-move",
	"leader-plan-mandatory", "leader-plan-optional",
	"new-term", "member-left", "converged",
	"fault-cold-restart", "fault-partial-restart", "checkpoint-converged", "node-cold-start",
	"node-leader-stop-higher-term",
}

// capBits keeps the default suite fast: 2^20 partitions make every leader
// tick and view validation scan a million entries, so only every fourth
// seed keeps 20 bits.
func capBits(sc Scenario) Scenario {
	if sc.PartitionBits > 16 && sc.Seed%4 != 1 {
		sc.PartitionBits = 16
	}

	return sc
}

func defaultCases(seeds []uint64, explicit bool) []dstCase {
	var cases []dstCase

	for _, s := range seeds {
		sc := shorten(Generate(s, SizeTiny), 240*sec, 180*sec)
		if !explicit {
			sc = capBits(sc)
		}

		cases = append(cases, dstCase{name: fmt.Sprintf("tiny/%d", s), sc: sc, twice: true})
	}

	if explicit {
		for _, s := range seeds {
			cases = append(cases, dstCase{name: fmt.Sprintf("small/%d", s), sc: shorten(Generate(s, SizeSmall), 180*sec, 180*sec), twice: true})
		}

		return cases
	}

	for _, s := range seeds[:min(4, len(seeds))] {
		sc := capBits(shorten(Generate(s, SizeSmall), 180*sec, 180*sec))
		cases = append(cases, dstCase{name: fmt.Sprintf("small/%d", s), sc: sc, twice: true})
	}

	// Pinned seeds from sweeps (run once; they are full-size explicit
	// scenarios): tiny/293 has a term per injected Lease conflict; tiny/75
	// has skewed lottery takeovers (excused overlaps); tiny/456 left only
	// non-members holding a view after a partial restart (cold start from
	// a stale view); in tiny/501 the only member was leaving and kept
	// declining the Lease. small/28 (a
	// bootstrap herd after a cold restart found a released Lease) is left
	// to sweeps for runtime.
	for _, p := range []struct {
		size string
		seed uint64
	}{{"tiny", 293}, {"tiny", 75}, {"tiny", 456}, {"tiny", 501}} {
		c, _ := sweepCase(p.size, p.seed)
		c.name = "pinned/" + c.name
		cases = append(cases, c)
	}

	// Whole-cluster and partial restarts.
	cases = append(cases, dstCase{name: "restarts", sc: GenerateRestarts(seeds[0], 16), twice: true})

	// Long-run stability: two simulated hours of churn on a small cluster,
	// converging between faults.
	cases = append(cases, dstCase{name: "longrun", sc: GenerateLongRun(seeds[0], 12, 2*3600*sec), twice: true})

	medium := shorten(GenerateKinds(seeds[0], SizeMedium, Crash, LeaderCrash, MassJoin, MessageLoss), 60*sec, 150*sec)
	medium.Members, medium.PartitionBits = 1000, 16
	medium.Capacity = 1000 + medium.Capacity - medium.Members
	cases = append(cases, dstCase{name: "medium", sc: medium, prebuilt: true})

	large := GenerateKinds(seeds[0], SizeLarge, Crash, LeaderCrash)
	large.Warmup = 30 * sec
	// Production defaults for the grid; they also bound the ping fan-out.
	large.CellSize, large.MaxNeighbors, large.PartitionBits = 4, 256, 16
	large = shorten(large, 20*sec, 75*sec)
	cases = append(cases, dstCase{name: "10k", sc: large, prebuilt: true})

	if Large() {
		huge := GenerateKinds(seeds[0], SizeHuge, Crash)
		huge.Warmup = 30 * sec
		huge.CellSize, huge.MaxNeighbors, huge.PartitionBits = 4, 256, 20
		huge = shorten(huge, 20*sec, 75*sec)
		cases = append(cases, dstCase{name: "100k", sc: huge, prebuilt: true})
	}

	return cases
}

func defaultSeeds() []uint64 {
	out := make([]uint64, 16)
	for i := range out {
		out[i] = uint64(i + 1)
	}

	return out
}

// TestControlPlaneDST is the control-plane simulation suite.
func TestDST(t *testing.T) {
	if testing.Short() {
		t.Skip("simulation suite")
	}

	explicit := os.Getenv(EnvSeed) != "" || os.Getenv(EnvSeeds) != ""

	seeds, err := SeedsFromEnv(defaultSeeds())
	if err != nil {
		t.Fatal(err)
	}

	total := NewCoverage()
	total.Register(requiredCoverage...)
	total.Register(DataPlaneCoverage...)

	cases := defaultCases(seeds, explicit)
	ran := 0

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start := time.Now()
			r := runCase(c)

			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			t.Logf("%s: %d members, %.0fs simulated, %d steps, %v wall, heap %d MiB, %s",
				c.name, c.sc.Members, float64(c.sc.Duration)/1e9, r.steps, time.Since(start).Round(time.Millisecond),
				ms.HeapAlloc>>20, r.stats)

			if r.err != nil {
				t.Fatalf("%v\nscenario: %s\nreplay: %s", r.err, c.sc.String(), ReplayCommand(c.sc.Seed))
			}

			total.Merge(r.cov)

			ran++

			if c.twice {
				if r2 := runCase(c); r2.trace != r.trace || r2.steps != r.steps {
					t.Fatalf("nondeterministic: trace %x/%x steps %d/%d\nreplay: %s", r.trace, r2.trace, r.steps, r2.steps, ReplayCommand(c.sc.Seed))
				}
			}
		})
	}

	// Coverage is judged only over the whole default set (not under -run).
	if !explicit && !t.Failed() && ran == len(cases) {
		if missing := total.Missing(); len(missing) > 0 {
			t.Fatalf("coverage: never reached %v\n%s", missing, total.Report())
		}

		t.Logf("data-plane coverage:\n%s", dataPlaneReport(total))
	}
}
