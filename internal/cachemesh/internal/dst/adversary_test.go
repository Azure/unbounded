// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package dst

import (
	"fmt"
	"testing"
)

// forgeryScenario is a tiny cluster attacked by a forging peer for two
// minutes after warmup. With crash set, the leader also crashes in the
// middle of the attack, so a real failover must win over the forgeries.
func forgeryScenario(seed uint64, members int, crash bool) Scenario {
	sc := quietScenario(seed, members)
	sc.Warmup = 120 * sec

	f := Fault{Kind: Forgery, Start: sc.Warmup, Duration: 120 * sec, Count: 2, Value: 500_000_000, Seed: seed}
	sc.Faults = []Fault{f}

	if crash {
		sc.Faults = append(sc.Faults, Fault{Kind: LeaderCrash, Start: f.Start + 30*sec, Duration: 20 * sec, Seed: seed + 1})
	}

	sc.Faults = append(sc.Faults, Fault{Kind: Checkpoint, Start: f.End() + minSettle, Duration: 240 * sec})
	sc.Duration = f.End() + minSettle + 240*sec + sc.Settle

	return sc
}

// TestDSTForgery runs clusters against a peer that forges later terms,
// NewTerm deltas, and snapshots led by members the Lease does not name.
// Without a crash the leader and its term must survive the attack (the
// fault checks this); with one, the cluster must still fail over. Every
// run must converge, so every forged view a member adopted was rolled back.
func TestDSTForgery(t *testing.T) {
	skipSlow(t)

	total := NewCoverage()
	want := []string{
		"forgery-ping", "forgery-broadcast", "forgery-snapshot", "forgery-adopted", "forgery-leader-kept",
		"node-term-unverified", "node-lease-verify", "node-absurd-term",
	}
	total.Register(want...)

	for _, seed := range []uint64{1, 2, 3} {
		for _, crash := range []bool{false, true} {
			sc := forgeryScenario(seed, 16+int(seed)*4, crash)

			t.Run(fmt.Sprintf("seed=%d/crash=%v", seed, crash), func(t *testing.T) {
				c := dstCase{name: "forgery", sc: sc, twice: seed == 1}
				r := runCase(c)

				t.Logf("%s", r.stats)

				if r.err != nil {
					t.Fatalf("%v\nscenario: %s", r.err, sc.String())
				}

				if c.twice {
					if r2 := runCase(c); r2.trace != r.trace || r2.steps != r.steps {
						t.Fatalf("nondeterministic: trace %x/%x steps %d/%d", r.trace, r2.trace, r.steps, r2.steps)
					}
				}

				total.Merge(r.cov)
			})
		}
	}

	if !t.Failed() {
		if missing := total.Missing(); len(missing) > 0 {
			t.Fatalf("coverage: never reached %v\n%s", missing, total.Report())
		}

		t.Logf("coverage:\n%s", total.Report())
	}
}
