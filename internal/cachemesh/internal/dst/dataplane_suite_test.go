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
