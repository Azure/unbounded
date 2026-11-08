// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package dst

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
)

// FaultKind is a kind of injected fault.
type FaultKind uint8

// Fault kinds. The suite decides how each maps onto the system under test;
// the parameters each kind uses are documented on Fault.
const (
	// Crash kills Count members without cleanup; they stay down.
	Crash FaultKind = iota + 1
	// Restart crashes Count members and restarts them after Duration.
	Restart
	// MassJoin starts Count new members at once.
	MassJoin
	// ScaleDown gracefully removes Count members.
	ScaleDown
	// RollingRestart restarts Count members one at a time, Value apart.
	RollingRestart
	// NetworkPartition splits members into Groups groups for Duration, the
	// minority side holding PPM of members, then heals.
	NetworkPartition
	// LeaderCrash crashes the current leader and restarts it after Duration.
	LeaderCrash
	// APIServerOutage fails every Lease operation for Duration.
	APIServerOutage
	// SlowMembers multiplies latency to and from Count members by
	// Value/1000 for Duration.
	SlowMembers
	// ClockSkew steps the wall clock of Count members by Value nanoseconds.
	ClockSkew
	// ClockDrift sets the drift of Count members to Value ppb for Duration.
	ClockDrift
	// MessageLoss drops PPM of messages for Duration.
	MessageLoss
	// MessageDuplication duplicates PPM of messages for Duration.
	MessageDuplication
	// GracefulDrain drains Count members, which then leave.
	GracefulDrain
	// KeyspaceInvalidation invalidates a metadata keyspace.
	KeyspaceInvalidation
	// LeaseConflicts fails PPM of Lease writes with a conflict for Duration.
	LeaseConflicts
	// Pause freezes Count members for Duration.
	Pause

	numFaultKinds = int(Pause)
)

var faultNames = [...]string{
	"", "crash", "restart", "mass-join", "scale-down", "rolling-restart",
	"network-partition", "leader-crash", "apiserver-outage", "slow-members",
	"clock-skew", "clock-drift", "message-loss", "message-duplication",
	"graceful-drain", "keyspace-invalidation", "lease-conflicts", "pause",
}

// String returns the kind's name.
func (k FaultKind) String() string {
	if int(k) < len(faultNames) && k != 0 {
		return faultNames[k]
	}

	return "unknown"
}

// AllFaultKinds returns every fault kind in order.
func AllFaultKinds() []FaultKind {
	out := make([]FaultKind, 0, numFaultKinds)
	for k := 1; k <= numFaultKinds; k++ {
		out = append(out, FaultKind(k))
	}

	return out
}

// Fault is one scheduled fault. It is pure data; the suite applies it.
type Fault struct {
	Kind FaultKind
	// Start is the simulated time the fault begins.
	Start int64
	// Duration is how long the fault lasts or, for Restart and LeaderCrash,
	// how long members stay down. Zero for instantaneous faults.
	Duration int64
	// Count is the number of members affected.
	Count int
	// PPM is a probability or fraction in parts per million.
	PPM uint32
	// Value is a kind-specific magnitude (see FaultKind).
	Value int64
	// Groups is the number of partition groups.
	Groups int
	// Seed selects affected members deterministically.
	Seed uint64
}

// End returns the time the fault stops having effect.
func (f Fault) End() int64 { return f.Start + f.Duration }

// String formats the fault for logs.
func (f Fault) String() string {
	return fmt.Sprintf("%s@%.1fs dur=%.1fs count=%d ppm=%d value=%d groups=%d",
		f.Kind, float64(f.Start)/1e9, float64(f.Duration)/1e9, f.Count, f.PPM, f.Value, f.Groups)
}

// SizeClass selects the cluster size range of a generated scenario.
type SizeClass uint8

const (
	// SizeTiny is 8 to 64 members.
	SizeTiny SizeClass = iota
	// SizeSmall is 64 to 512 members.
	SizeSmall
	// SizeMedium is 1000 to 3000 members.
	SizeMedium
	// SizeLarge is 10,000 members.
	SizeLarge
	// SizeHuge is 100,000 members.
	SizeHuge
)

// Scenario is a generated simulation run.
type Scenario struct {
	Seed uint64
	// Members is the initial cluster size.
	Members int
	// Capacity is Members plus every member a MassJoin adds.
	Capacity      int
	CellSize      int
	MaxNeighbors  int
	PartitionBits uint8
	// Warmup is how long the cluster runs before the first fault.
	Warmup int64
	// Duration is the total simulated time; the last Settle of it has no
	// faults, for liveness checks.
	Duration int64
	Settle   int64
	// Faults are sorted by Start.
	Faults []Fault
}

// Kinds returns the distinct fault kinds in the scenario, in order.
func (s *Scenario) Kinds() []FaultKind {
	var out []FaultKind

	for _, f := range s.Faults {
		if !slices.Contains(out, f.Kind) {
			out = append(out, f.Kind)
		}
	}

	slices.Sort(out)

	return out
}

// String formats the scenario for logs.
func (s *Scenario) String() string {
	var b strings.Builder

	fmt.Fprintf(&b, "seed=%d members=%d capacity=%d cell=%d maxNeighbors=%d bits=%d duration=%.0fs settle=%.0fs\n",
		s.Seed, s.Members, s.Capacity, s.CellSize, s.MaxNeighbors, s.PartitionBits,
		float64(s.Duration)/1e9, float64(s.Settle)/1e9)

	for _, f := range s.Faults {
		fmt.Fprintf(&b, "  %s\n", f)
	}

	return b.String()
}

const sec = int64(nsPerSec)

func between(r *rand.Rand, lo, hi int64) int64 {
	if hi <= lo {
		return lo
	}

	return lo + r.Int64N(hi-lo+1)
}

func pick[T any](r *rand.Rand, xs ...T) T { return xs[r.IntN(len(xs))] }

// Generate returns a swarm scenario: a random subset of fault kinds, each
// scheduled one or more times with random parameters, followed by a quiet
// settle period.
func Generate(seed uint64, size SizeClass) Scenario {
	r := NewRand(seed, "scenario")

	var kinds []FaultKind

	for _, k := range AllFaultKinds() {
		if r.IntN(2) == 0 {
			kinds = append(kinds, k)
		}
	}

	if len(kinds) == 0 {
		kinds = append(kinds, pick(r, AllFaultKinds()...))
	}

	return generate(seed, r, size, kinds)
}

// GenerateKinds is Generate restricted to the given fault kinds, each
// scheduled at least once.
func GenerateKinds(seed uint64, size SizeClass, kinds ...FaultKind) Scenario {
	return generate(seed, NewRand(seed, "scenario"), size, kinds)
}

func generate(seed uint64, r *rand.Rand, size SizeClass, kinds []FaultKind) Scenario {
	s := Scenario{Seed: seed}

	switch size {
	case SizeTiny:
		s.Members = int(between(r, 8, 64))
	case SizeSmall:
		s.Members = int(between(r, 64, 512))
	case SizeMedium:
		s.Members = int(between(r, 1000, 3000))
	case SizeLarge:
		s.Members = 10_000
	default:
		s.Members = 100_000
	}

	s.CellSize = pick(r, 2, 3, 4, 4, 4, 5, 8)
	s.MaxNeighbors = pick(r, 256, 256, 256, 128, 64, 32)
	s.PartitionBits = pick[uint8](r, 20, 20, 16, 12)
	s.Capacity = s.Members
	s.Warmup = between(r, 30, 90) * sec

	window := between(r, 120, 600) * sec
	n := s.Members
	frac := func(lo, hi int) int { return max(1, int(between(r, int64(n*lo/1000), int64(n*hi/1000)))) }

	for _, k := range kinds {
		reps := 1 + r.IntN(2)
		for range reps {
			f := Fault{Kind: k, Start: s.Warmup + r.Int64N(window), Seed: r.Uint64()}

			switch k {
			case Crash, GracefulDrain, ScaleDown:
				f.Count = frac(1, 100)
			case Restart:
				f.Count = frac(1, 50)
				f.Duration = between(r, 1, 60) * sec
			case MassJoin:
				f.Count = frac(100, 500)
				s.Capacity += f.Count
			case RollingRestart:
				f.Count = frac(50, 1000)
				f.Value = between(r, 100, 5000) * 1_000_000
				f.Duration = int64(f.Count) * f.Value
			case NetworkPartition:
				f.Groups = int(between(r, 2, 3))
				f.PPM = uint32(between(r, 1000, 500_000))
				f.Duration = between(r, 5, 120) * sec
			case LeaderCrash:
				f.Duration = between(r, 0, 60) * sec
			case APIServerOutage:
				f.Duration = between(r, 5, 120) * sec
			case SlowMembers:
				f.Count = frac(1, 100)
				f.Value = between(r, 2000, 50_000)
				f.Duration = between(r, 10, 180) * sec
			case ClockSkew:
				f.Count = frac(1, 100)
				f.Value = between(r, -60, 60) * sec
			case ClockDrift:
				f.Count = frac(1, 100)
				f.Value = pick[int64](r, -1, 1) * between(r, 1_000, 500_000)
				f.Duration = between(r, 30, 300) * sec
			case MessageLoss:
				f.PPM = uint32(between(r, 1000, 200_000))
				f.Duration = between(r, 5, 120) * sec
			case MessageDuplication:
				f.PPM = uint32(between(r, 1000, 200_000))
				f.Duration = between(r, 5, 120) * sec
			case LeaseConflicts:
				f.PPM = uint32(between(r, 10_000, 500_000))
				f.Duration = between(r, 5, 120) * sec
			case Pause:
				f.Count = frac(1, 50)
				f.Duration = between(r, 1, 60) * sec
			case KeyspaceInvalidation:
			}

			s.Faults = append(s.Faults, f)
		}
	}

	slices.SortStableFunc(s.Faults, func(a, b Fault) int {
		switch {
		case a.Start < b.Start:
			return -1
		case a.Start > b.Start:
			return 1
		default:
			return 0
		}
	})

	end := s.Warmup
	for _, f := range s.Faults {
		end = max(end, f.End())
	}

	s.Settle = between(r, 120, 240) * sec
	s.Duration = end + s.Settle

	return s
}
