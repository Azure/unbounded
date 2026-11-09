// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package dst

import (
	"cmp"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"

	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
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
	// ColdRestart crashes every member and restarts all of them after
	// Duration with new incarnations. Value 0 restarts them without seeds
	// (they find only the dead leader and seeds in the Lease); 1 keeps the
	// usual seed assignment (seeds that are restarting too).
	ColdRestart
	// PartialRestart crashes the leader and every member except Count
	// survivors, and restarts them after Duration without seeds.
	PartialRestart
	// Checkpoint requires the cluster to have converged by Start+Duration.
	Checkpoint

	// numFaultKinds counts the kinds Generate draws from its main stream;
	// later kinds come from a separate stream so adding them did not
	// change existing scenarios.
	numFaultKinds = int(Pause)
)

var faultNames = [...]string{
	"", "crash", "restart", "mass-join", "scale-down", "rolling-restart",
	"network-partition", "leader-crash", "apiserver-outage", "slow-members",
	"clock-skew", "clock-drift", "message-loss", "message-duplication",
	"graceful-drain", "keyspace-invalidation", "lease-conflicts", "pause",
	"cold-restart", "partial-restart", "checkpoint",
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
	// DeltaLogSize is the members' delta log size; 0 selects the default.
	// Small logs make partitioned members catch up by snapshot.
	DeltaLogSize int
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

	fmt.Fprintf(&b, "seed=%d members=%d capacity=%d cell=%d maxNeighbors=%d bits=%d log=%d duration=%.0fs settle=%.0fs\n",
		s.Seed, s.Members, s.Capacity, s.CellSize, s.MaxNeighbors, s.PartitionBits, s.DeltaLogSize,
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

	s := generate(seed, r, size, kinds)
	extend(&s)

	return s
}

// extend adds the fault kinds after numFaultKinds, drawn from their own
// stream: each in about one scenario in eight.
func extend(s *Scenario) {
	r := NewRand(s.Seed, "scenario-ext")
	last := s.Warmup

	for _, f := range s.Faults {
		last = max(last, f.Start)
	}

	add := func(f Fault) {
		f.Start = s.Warmup + r.Int64N(last-s.Warmup+1)
		f.Seed = r.Uint64()
		s.Faults = append(s.Faults, f)
		s.Duration = max(s.Duration, f.End()+s.Settle)
	}

	s.DeltaLogSize = pick(r, 0, 0, 64, 32)

	if r.IntN(8) == 0 {
		add(Fault{Kind: ColdRestart, Duration: between(r, 1, 20) * sec, Value: int64(r.IntN(2))})
	}

	if r.IntN(8) == 0 {
		add(Fault{Kind: PartialRestart, Duration: between(r, 1, 20) * sec, Count: int(between(r, 1, 3))})
	}

	sortFaults(s.Faults)
}

func sortFaults(fs []Fault) {
	slices.SortStableFunc(fs, func(a, b Fault) int { return cmp.Compare(a.Start, b.Start) })
}

// longRunKinds are the churn faults of GenerateLongRun.
var longRunKinds = []FaultKind{
	Restart, LeaderCrash, RollingRestart, NetworkPartition, Pause, MessageLoss,
	MessageDuplication, APIServerOutage, LeaseConflicts, ClockDrift, SlowMembers,
	KeyspaceInvalidation, ColdRestart, PartialRestart,
}

// GenerateLongRun returns a long scenario on a small cluster: a churn
// fault every few minutes for duration, each followed by a Checkpoint that
// requires convergence before the next one.
func GenerateLongRun(seed uint64, members int, duration int64) Scenario {
	r := NewRand(seed, "longrun")
	s := Scenario{
		Seed: seed, Members: members, Capacity: members, CellSize: 4, MaxNeighbors: 256,
		PartitionBits: 12, Warmup: 60 * sec, Settle: 120 * sec,
	}

	for at := s.Warmup; at < duration; {
		k := pick(r, longRunKinds...)
		f := Fault{Kind: k, Start: at, Seed: r.Uint64(), Count: 1 + r.IntN(max(1, members/8))}

		switch k {
		case Restart, Pause, LeaderCrash, ColdRestart, PartialRestart:
			f.Duration = between(r, 1, 30) * sec
		case RollingRestart:
			f.Value = between(r, 500, 3000) * 1_000_000
			f.Count = members / 2
			f.Duration = int64(f.Count) * f.Value
		case NetworkPartition:
			f.Groups, f.PPM, f.Duration = 2, uint32(between(r, 100_000, 400_000)), between(r, 5, 60)*sec
		case MessageLoss, MessageDuplication, LeaseConflicts:
			f.PPM, f.Duration = uint32(between(r, 10_000, 200_000)), between(r, 5, 60)*sec
		case APIServerOutage:
			f.Duration = between(r, 5, 60) * sec
		case ClockDrift:
			f.Value = pick[int64](r, -1, 1) * between(r, 1_000_000, 100_000_000)
			f.Duration = between(r, 30, 120) * sec
		case SlowMembers:
			f.Value, f.Duration = between(r, 2000, 20_000), between(r, 10, 60)*sec
		default:
		}

		if k == ColdRestart {
			f.Value = int64(r.IntN(2))
		}

		if k == PartialRestart {
			f.Count = 1 + r.IntN(2)
		}

		s.Faults = append(s.Faults, f)
		end := f.End() + minSettle
		s.Faults = append(s.Faults, Fault{Kind: Checkpoint, Start: end, Duration: 180 * sec})
		at = end + 180*sec + between(r, 0, 60)*sec
	}

	sortFaults(s.Faults)

	s.Duration = s.Warmup
	for _, f := range s.Faults {
		s.Duration = max(s.Duration, f.End())
	}

	s.Duration += s.Settle

	return s
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
				// Up to +-10% (scaled from the original draw so the
				// random stream is unchanged), inside the +-20% that
				// election safety tolerates with the default durations.
				f.Value = pick[int64](r, -1, 1) * between(r, 1_000, 500_000) * 200
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

	sortFaults(s.Faults)

	end := s.Warmup
	for _, f := range s.Faults {
		end = max(end, f.End())
	}

	s.Settle = between(r, 120, 240) * sec
	s.Duration = end + s.Settle

	return s
}

// GenerateRestarts returns a scripted scenario on a small cluster: a
// partial restart (the leader and all but two members die), a whole-cluster
// restart with the usual seeds, and one without seeds, each followed by a
// Checkpoint.
func GenerateRestarts(seed uint64, members int) Scenario {
	r := NewRand(seed, "restarts")
	s := Scenario{
		Seed: seed, Members: members, Capacity: members, CellSize: 4, MaxNeighbors: 256,
		PartitionBits: 12, Warmup: 90 * sec, Settle: 120 * sec,
	}

	at := s.Warmup

	for _, f := range []Fault{
		{Kind: PartialRestart, Count: 2, Duration: between(r, 1, 20) * sec},
		{Kind: ColdRestart, Value: 1, Duration: between(r, 1, 20) * sec},
		{Kind: ColdRestart, Value: 0, Duration: between(r, 1, 20) * sec},
	} {
		f.Start, f.Seed = at, r.Uint64()
		s.Faults = append(s.Faults, f, Fault{Kind: Checkpoint, Start: f.End() + minSettle, Duration: 240 * sec})
		at = f.End() + minSettle + 240*sec
	}

	s.Duration = at + s.Settle

	return s
}

// KeyKind is the kind of a generated workload key.
type KeyKind uint8

const (
	// KeyBlob is a content-addressed blob: a 32-byte digest.
	KeyBlob KeyKind = iota
	// KeyStripe is a stripe of a layer: a 32-byte digest followed by a
	// 4-byte big-endian stripe index.
	KeyStripe
	// KeyMeta is a metadata key of the form "repo-N:tag-M".
	KeyMeta
)

// Workload key mix: a Zipf popularity exponent, stripes per layer, and the
// blob:stripe:meta weights.
const (
	keySkew      = 1.1
	keyStripes   = 16
	blobWeight   = 2
	stripeWeight = 5
	metaWeight   = 3
)

// KeyGen generates workload keys with Zipf-distributed popularity. The
// returned key slice is reused by the next call and must be copied if
// retained.
type KeyGen struct {
	rng  *rand.Rand
	zipf *rand.Zipf
	buf  []byte
}

// NewKeyGen returns a generator of keys over objects [0, keys) whose draws
// come from rng.
func NewKeyGen(rng *rand.Rand, keys uint64) *KeyGen {
	return &KeyGen{rng: rng, zipf: rand.NewZipf(rng, keySkew, 1, keys-1), buf: make([]byte, 0, 64)}
}

// Next returns a random key, its kind, and the object's popularity rank
// (0 is the hottest).
func (g *KeyGen) Next() ([]byte, KeyKind, uint64) {
	rank := g.zipf.Uint64()

	w := g.rng.Uint32N(blobWeight + stripeWeight + metaWeight)

	switch {
	case w < blobWeight:
		return g.Blob(rank), KeyBlob, rank
	case w < blobWeight+stripeWeight:
		return g.Stripe(rank, g.rng.Uint32N(keyStripes)), KeyStripe, rank
	default:
		return g.Meta(rank), KeyMeta, rank
	}
}

func (g *KeyGen) digest(rank uint64) {
	g.buf = g.buf[:32]

	x := view.Mix64(rank ^ 0x9e3779b97f4a7c15)
	for i := 0; i < 32; i += 8 {
		binary.BigEndian.PutUint64(g.buf[i:], x)
		x = view.Mix64(x)
	}
}

// Blob returns the digest of blob rank.
func (g *KeyGen) Blob(rank uint64) []byte {
	g.digest(rank)

	return g.buf
}

// Stripe returns the key of stripe idx of layer rank.
func (g *KeyGen) Stripe(rank uint64, idx uint32) []byte {
	g.digest(rank)
	g.buf = binary.BigEndian.AppendUint32(g.buf, idx)

	return g.buf
}

// Meta returns the metadata key of object rank.
func (g *KeyGen) Meta(rank uint64) []byte {
	g.buf = append(g.buf[:0], "repo-"...)
	g.buf = strconv.AppendUint(g.buf, rank/8, 10)
	g.buf = append(g.buf, ":tag-"...)
	g.buf = strconv.AppendUint(g.buf, rank%8, 10)

	return g.buf
}
