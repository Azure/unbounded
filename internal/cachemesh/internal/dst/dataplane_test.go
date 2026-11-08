// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package dst

import (
	"strings"
	"testing"

	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
)

// quietScenario is a small fault-free scenario for data-plane unit tests.
func quietScenario(seed uint64, members int) Scenario {
	sc := Generate(seed, SizeTiny)
	sc.Members, sc.Capacity = members, members
	sc.Faults = nil
	sc.PartitionBits = 12
	sc.Warmup = 30 * sec
	sc.Settle = 120 * sec
	sc.Duration = sc.Warmup + sc.Settle

	return sc
}

func runDataPlane(t *testing.T, sc Scenario, cfg DataPlaneConfig) (*Cluster, *DataPlane) {
	t.Helper()

	dp := NewDataPlane(cfg)
	c := NewCluster(ClusterConfig{Scenario: sc, Workload: dp, Node: dp.ConfigureNode})

	if err := c.Run(); err != nil {
		t.Fatalf("%v\n%s", err, dp.Summary())
	}

	return c, dp
}

func TestDataPlaneQuiet(t *testing.T) {
	_, dp := runDataPlane(t, quietScenario(3, 24), DataPlaneConfig{})

	req, served := dp.Counts.Get("requests"), dp.Counts.Get("result-served")
	if req == 0 || served == 0 {
		t.Fatalf("no traffic: %s", dp.Summary())
	}

	// Without faults only herd overloads and TTL truncation may fail a
	// request, and nothing may miss its deadline.
	if d := dp.Counts.Get("result-deadline"); d != 0 {
		t.Fatalf("%d deadline misses: %s", d, dp.Summary())
	}

	if served*100 < req*95 {
		t.Fatalf("served %d of %d: %s", served, req, dp.Summary())
	}

	if dp.Steady.Count() == 0 || dp.Steady.Quantile(0.5) <= 0 {
		t.Fatalf("no steady latency: %s", dp.Summary())
	}

	if len(dp.open) > 0 {
		for _, r := range dp.open {
			if r.done && r.live == 0 {
				continue
			}

			if r.start < dp.c.Sim.Now()-dp.hangBound() {
				t.Fatalf("request %d still open", r.id)
			}
		}
	}
}

func TestDataPlaneDeterministic(t *testing.T) {
	sc := quietScenario(5, 16)
	a, _ := runDataPlane(t, sc, DataPlaneConfig{Health: HealthHedge})
	b, _ := runDataPlane(t, sc, DataPlaneConfig{Health: HealthHedge})

	if a.Trace.Sum() != b.Trace.Sum() || a.Sim.Steps() != b.Sim.Steps() {
		t.Fatalf("trace %x/%x steps %d/%d", a.Trace.Sum(), b.Trace.Sum(), a.Sim.Steps(), b.Sim.Steps())
	}

	// The data plane must feed the trace: without it the hash differs.
	c := NewCluster(ClusterConfig{Scenario: sc})
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}

	if c.Trace.Sum() == a.Trace.Sum() {
		t.Fatal("data-plane work is not in the trace")
	}
}

func TestDataPlaneNoHedge(t *testing.T) {
	_, dp := runDataPlane(t, quietScenario(7, 16), DataPlaneConfig{NoHedge: true, Health: HealthHedge})
	if h := dp.Counts.Get("hedges"); h != 0 {
		t.Fatalf("%d hedges with hedging disabled", h)
	}
}

func TestCheckPathCatchesBadPaths(t *testing.T) {
	sc := quietScenario(9, 16)
	dp := NewDataPlane(DataPlaneConfig{})
	c := NewCluster(ClusterConfig{Scenario: sc, Workload: dp, Node: dp.ConfigureNode})

	if err := c.Run(); err != nil {
		t.Fatal(err)
	}

	// Find a member and a partition it serves itself, then claim the
	// request visited a member twice.
	st := dp.state(0)
	v := st.View()

	for p := range uint32(1) << st.PartitionBits() {
		d := st.RoutePartition(p)
		if d.Kind.String() != "Self" {
			continue
		}

		r := &dpReq{id: 1, live: 1}
		cp := &dpCopy{req: r, path: []uint32{0, 0}, vers: []view.Version{v.Version(), v.Version()}, members: true}
		dp.checkPath(cp, st, &d)

		if err := c.Err(); err == nil || !strings.Contains(err.Error(), "visited twice") {
			t.Fatalf("duplicate visit not caught: %v", err)
		}

		return
	}

	t.Skip("member 0 owns no partition")
}

func TestNeighborBound(t *testing.T) {
	sc := quietScenario(11, 40)
	c, dp := runDataPlane(t, sc, DataPlaneConfig{})

	v := dp.state(0).View()
	b := neighborBound(v, sc.CellSize, sc.MaxNeighbors)

	if b[1] != 1 {
		t.Fatalf("settled view not balanced: %v", b)
	}

	if b[0] < sc.MaxNeighbors {
		t.Fatalf("bound %d below MaxNeighbors %d", b[0], sc.MaxNeighbors)
	}

	// Every member is within it.
	for i := range uint32(c.Members()) {
		if n := c.Node(i); n != nil {
			if k := len(n.Neighbors(nil)); k > b[0] {
				t.Fatalf("member %d: %d neighbors above %d", i, k, b[0])
			}
		}
	}

	// A tight bound with an infeasible configuration falls back to the
	// best shape's estimate rather than reporting every member.
	if b := neighborBound(v, sc.CellSize, 1); b[0] <= 1 {
		t.Fatalf("infeasible bound %v", b)
	}
}

func TestHealthVariants(t *testing.T) {
	if HealthConfig(HealthDefault).MaxEjectionPercent != 0 {
		t.Fatal("default variant must use library defaults")
	}

	if c := HealthConfig(HealthPanic); c.MaxEjectionPercent <= c.PanicThreshold {
		t.Fatalf("panic variant cannot reach panic: %+v", c)
	}

	if HealthConfig(HealthHedge).HedgeRatio <= 0.05 {
		t.Fatal("hedge variant must raise the budget")
	}

	for _, v := range []HealthVariant{HealthDefault, HealthPanic, HealthHedge} {
		if v.String() == "" {
			t.Fatal("empty name")
		}
	}
}
