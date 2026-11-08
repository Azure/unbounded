// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package dst

import "strings"

// dataPlaneConfig picks the data-plane configuration of a suite case: the
// health variant rotates with the seed (every fourth seed makes panic
// reachable, every fourth hedges with a large budget), and large clusters
// run at a reduced rate.
func dataPlaneConfig(c dstCase) DataPlaneConfig {
	cfg := DataPlaneConfig{}

	switch c.sc.Seed % 4 {
	case 2:
		cfg.Health = HealthPanic
	case 3:
		cfg.Health = HealthHedge
	}

	if c.sc.Members >= 1000 {
		cfg.QPS = 100
		cfg.RouteSamples = 1
		cfg.BurstSize = 320
	}

	return cfg
}

// dataPlaneReport returns the data-plane lines of a coverage report.
func dataPlaneReport(c *Coverage) string {
	var b strings.Builder

	for line := range strings.SplitSeq(c.Report(), "\n") {
		if strings.HasPrefix(line, "dp-") {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}

	return b.String()
}

// dataPlaneCases are targeted data-plane cases added to the default set: a
// 40-member cluster whose only data-plane fault is two slow members (30x,
// for 150s, long enough to judge tail-latency protection after the
// detection window), alongside clock drift and a keyspace invalidation.
func dataPlaneCases() []dstCase {
	sc := Generate(7, SizeTiny)
	sc.Members, sc.Capacity, sc.PartitionBits = 40, 40, 16
	sc.CellSize, sc.MaxNeighbors = 4, 256
	sc.Warmup = 40 * sec
	sc.Faults = []Fault{
		{Kind: ClockDrift, Start: sc.Warmup + 10*sec, Duration: 200 * sec, Count: 1, Value: 200_000, Seed: 1},
		{Kind: SlowMembers, Start: sc.Warmup + 20*sec, Duration: 150 * sec, Count: 2, Value: 30_000, Seed: 2},
		{Kind: KeyspaceInvalidation, Start: sc.Warmup + 60*sec, Seed: 3},
	}
	sc.Settle = 120 * sec
	sc.Duration = sc.Warmup + 210*sec + sc.Settle

	return []dstCase{{name: "slow", sc: sc, twice: true}}
}
