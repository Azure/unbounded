// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package view holds the immutable membership view that every cachemesh
// member routes with: members by slot, the grid shape and cell index, the
// partition table, the epoch anchor, and keyspace generations. Views change
// only by applying leader-issued deltas, and each view is identified by a
// Version and a content Digest. See designs/cachemesh.md, "Membership view".
//
// The package also holds what a view is built from and what every member
// must agree on bit for bit: the grid arithmetic (Shape, Choose, cell
// assignment), the partition Table, the hash functions, and the canonical
// binary encoding (Reader and the Append functions) that the control
// package's wire messages reuse.
package view

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"slices"
	"strconv"
	"time"
)

const (
	memShift  = 8
	memChunk  = 1 << memShift
	memMask   = memChunk - 1
	loadShift = 10
	loadChunk = 1 << loadShift
	loadMask  = loadChunk - 1
	cellShift = 8
	cellChunk = 1 << cellShift
	cellMask  = cellChunk - 1
	idxShift  = 10
	idxChunk  = 1 << idxShift

	noReuse = math.MaxUint64
	idSeed  = 0x6361636865696473 // "cacheids"
)

// memberChunk holds memChunk slots. A slot without a member has State 0 and
// keeps its ReusableAt in reuse.
type memberChunk struct {
	mem   [memChunk]Member
	reuse [memChunk]uint64
}

// View is an immutable membership view. All methods are safe for concurrent
// use. Slices and pointers returned by a View are shared and must not be
// modified. Views share unchanged storage chunks with the views they were
// derived from.
type View struct {
	version Version
	digest  Digest
	leader  Slot

	config    Config
	hasConfig bool
	shape     Shape
	table     *Table

	epoch  uint64
	period time.Duration
	gens   []KeyspaceGen

	n       int    // members
	slotCap uint32 // slots in use or free; members are below slotCap
	orphans uint64 // partitions owned by non-member slots

	mem      []*memberChunk
	minReuse []uint64   // per member chunk: min ReusableAt of its free slots
	loads    [][]uint32 // per slot partition counts; nil chunk is zero
	cells    [][][]Slot // per cell sorted slots; nil chunk is all empty
	ids      [][]Slot   // open-addressing ID index chunks, NoSlot empty
	idCount  int        // entries in ids
}

var genesisDigest = Digest(sha256.Sum256([]byte("cachemesh/view/v1/genesis")))

var genesis = &View{digest: genesisDigest, leader: NoSlot}

// GenesisDigest is the digest of the genesis view.
func GenesisDigest() Digest { return genesisDigest }

// Genesis returns the empty view at the zero Version: no config, no members,
// no leader. Every cluster starts from it.
func Genesis() *View { return genesis }

// Version returns the view version.
func (v *View) Version() Version { return v.version }

// Digest returns the content digest.
func (v *View) Digest() Digest { return v.digest }

// Term returns the leader term, Version().Term.
func (v *View) Term() uint64 { return v.version.Term }

// Leader returns the slot of the term's leader, or NoSlot at term 0.
func (v *View) Leader() Slot { return v.leader }

// Config returns the cluster constants and whether they have been set.
func (v *View) Config() (Config, bool) { return v.config, v.hasConfig }

// PartitionBits returns log2 of the partition count, or 0 without config.
func (v *View) PartitionBits() uint8 { return v.config.PartitionBits }

// Shape returns the grid shape. It is the zero (invalid) shape without
// config.
func (v *View) Shape() *Shape { return &v.shape }

// Table returns the partition table, or nil without config.
func (v *View) Table() *Table { return v.table }

// Epoch returns the epoch anchor.
func (v *View) Epoch() uint64 { return v.epoch }

// Period returns the epoch period, or 0 before the first Epoch op.
func (v *View) Period() time.Duration { return v.period }

// Gen returns the generation of keyspace seed.
func (v *View) Gen(seed uint64) uint32 {
	i, ok := findGen(v.gens, seed)
	if !ok {
		return 0
	}

	return v.gens[i].Gen
}

// Gens returns every non-zero keyspace generation sorted by seed.
func (v *View) Gens() []KeyspaceGen { return v.gens[:len(v.gens):len(v.gens)] }

func findGen(g []KeyspaceGen, seed uint64) (int, bool) {
	lo, hi := 0, len(g)
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if g[m].Seed < seed {
			lo = m + 1
		} else {
			hi = m
		}
	}

	return lo, lo < len(g) && g[lo].Seed == seed
}

// Len returns the number of members.
func (v *View) Len() int { return v.n }

// SlotCap returns one past the highest slot ever used. Every slot below it
// is a member or a free slot.
func (v *View) SlotCap() uint32 { return v.slotCap }

// Member returns the member at slot. The pointer is shared and must not be
// modified.
func (v *View) Member(s Slot) (*Member, bool) {
	if s >= v.slotCap {
		return nil, false
	}

	m := &v.mem[s>>memShift].mem[s&memMask]

	return m, m.State != 0
}

// Key returns the key of the member at slot, or 0.
func (v *View) Key(s Slot) uint64 {
	if s >= v.slotCap {
		return 0
	}

	return v.mem[s>>memShift].mem[s&memMask].Key
}

// Cell returns the cell of the member at slot, or NoCell.
func (v *View) Cell(s Slot) uint32 {
	if s >= v.slotCap {
		return NoCell
	}

	m := &v.mem[s>>memShift].mem[s&memMask]
	if m.State == 0 {
		return NoCell
	}

	return m.Cell
}

// State returns the state of the member at slot, or 0 if there is none.
func (v *View) State(s Slot) MemberState {
	if s >= v.slotCap {
		return 0
	}

	return v.mem[s>>memShift].mem[s&memMask].State
}

// IsMember reports whether slot holds a member.
func (v *View) IsMember(s Slot) bool { return v.State(s) != 0 }

// CellMembers returns the members of cell sorted by Key ascending, ties by
// slot. A member's index in it is its rank. The slice is shared.
func (v *View) CellMembers(c uint32) []Slot {
	ci := c >> cellShift
	if int(ci) >= len(v.cells) || v.cells[ci] == nil {
		return nil
	}

	s := v.cells[ci][c&cellMask]

	return s[:len(s):len(s)]
}

// Occupancy returns the number of members in cell.
func (v *View) Occupancy(c uint32) int { return len(v.CellMembers(c)) }

// Load returns the number of partitions owned by slot.
func (v *View) Load(s Slot) uint32 {
	ci := s >> loadShift
	if int(ci) >= len(v.loads) || v.loads[ci] == nil {
		return 0
	}

	return v.loads[ci][s&loadMask]
}

// ForEachMember calls fn for every member in slot order until fn returns
// false.
func (v *View) ForEachMember(fn func(Slot, *Member) bool) {
	for s := range v.slotCap {
		m := &v.mem[s>>memShift].mem[s&memMask]
		if m.State != 0 && !fn(s, m) {
			return
		}
	}
}

// FreeSlots returns every free slot in slot order.
func (v *View) FreeSlots() []FreeSlot {
	var out []FreeSlot

	for s := range v.slotCap {
		c := v.mem[s>>memShift]
		if c.mem[s&memMask].State == 0 {
			out = append(out, FreeSlot{Slot: s, ReusableAt: c.reuse[s&memMask]})
		}
	}

	return out
}

// NextSlot returns the lowest free slot reusable at the view epoch, or
// SlotCap if there is none.
func (v *View) NextSlot() Slot {
	for ci, m := range v.minReuse {
		if m > v.epoch {
			continue
		}

		c := v.mem[ci]
		for j := range memChunk {
			s := Slot(ci<<memShift + j)
			if s >= v.slotCap {
				break
			}

			if c.mem[j].State == 0 && c.reuse[j] <= v.epoch {
				return s
			}
		}
	}

	return v.slotCap
}

// Lookup returns the slot of the member with the given ID.
func (v *View) Lookup(id string) (Slot, bool) {
	s := v.lookupID(id)

	return s, s != NoSlot
}

func idHash(id string) uint64 { return KeyHashString(idSeed, id) }

func (v *View) idSize() uint64 { return uint64(len(v.ids)) << idxShift }

func (v *View) idAt(i uint64) Slot { return v.ids[i>>idxShift][i&(idxChunk-1)] }

func (v *View) lookupID(id string) Slot {
	if len(v.ids) == 0 {
		return NoSlot
	}

	mask := v.idSize() - 1
	for i := idHash(id) & mask; ; i = (i + 1) & mask {
		s := v.idAt(i)
		if s == NoSlot {
			return NoSlot
		}

		if v.mem[s>>memShift].mem[s&memMask].ID == id {
			return s
		}
	}
}

// Slot is a member's index in a view. Slots are dense, reused only after
// Config.SlotReuseEpochs epochs, and stable for the lifetime of a member.
type Slot = uint32

// NoSlot means no member.
const NoSlot Slot = math.MaxUint32

// Version orders views. Versions compare by Term, then Seq. The zero Version
// is the genesis view, which has no members.
type Version struct {
	Term uint64
	Seq  uint64
}

// Less reports whether v orders before o.
func (v Version) Less(o Version) bool {
	if v.Term != o.Term {
		return v.Term < o.Term
	}

	return v.Seq < o.Seq
}

// IsZero reports whether v is the genesis version.
func (v Version) IsZero() bool { return v == Version{} }

// String formats v as "term.seq".
func (v Version) String() string {
	return strconv.FormatUint(v.Term, 10) + "." + strconv.FormatUint(v.Seq, 10)
}

// DigestSize is the size of a view digest in bytes.
const DigestSize = 32

// Digest identifies the content of a view. Two views with the same Version
// and a different Digest are a safety violation.
type Digest [DigestSize]byte

// String returns the first 8 bytes of d in hex, enough for logs.
func (d Digest) String() string { return hex.EncodeToString(d[:8]) }

// MemberState is a member's lifecycle state in a view.
type MemberState uint8

// Member states. The zero value is invalid so that a missing state is caught
// by Validate.
const (
	// Joining members are admitted but own no partitions (weight 0) until
	// they leave quarantine.
	Joining MemberState = 1
	// Active members own partitions according to their weight.
	Active MemberState = 2
	// Draining members are leaving: weight 0, removed after the drain period.
	Draining MemberState = 3
)

// Valid reports whether s is a defined state.
func (s MemberState) Valid() bool { return s >= Joining && s <= Draining }

// String returns the state name.
func (s MemberState) String() string {
	switch s {
	case Joining:
		return "Joining"
	case Active:
		return "Active"
	case Draining:
		return "Draining"
	default:
		return "MemberState(" + strconv.Itoa(int(s)) + ")"
	}
}

// Limits on view contents. Apply rejects values beyond them and the decoders
// use them to bound allocation on hostile input.
const (
	// MaxIDLen bounds Member.ID (a DNS subdomain name fits).
	MaxIDLen = 253
	// MaxAddrLen bounds Member.Addr.
	MaxAddrLen = 1024
	// MaxZoneLen bounds Member.Zone.
	MaxZoneLen = 128
	// MaxSlots bounds SlotCap and any slot number in a view.
	MaxSlots = 1 << 22
	// MaxCells bounds Shape().Cells() of a view. It equals MaxSlots (a view
	// never needs more cells than members) and is stricter than
	// MaxCells, which only bounds Shape arithmetic.
	MaxCells = MaxSlots
	// MaxDeltaOps bounds the number of ops in one delta.
	MaxDeltaOps = 1 << 16
	// MaxGens bounds the number of keyspace generations in a view.
	MaxGens = 1 << 16
)

// Member is one member record of a view. Members returned by a View are
// shared and must not be modified.
type Member struct {
	// ID is the application-supplied member name, unique in a view.
	ID string
	// Incarnation distinguishes restarts of the same ID.
	Incarnation uint64
	// Key is MemberKey(ID, Incarnation), the rendezvous key.
	Key uint64
	// Addr is the control and data endpoint chosen by the application.
	Addr string
	// Zone is an application-defined failure domain label.
	Zone string
	// State is the lifecycle state.
	State MemberState
	// Weight is 0..MaxWeight and 0 unless State is Active.
	Weight uint32
	// Cell is the member's grid cell, below Shape().Cells().
	Cell uint32
	// Joined is the view epoch at which the member was admitted.
	Joined uint64
	// Since is the view epoch of the member's last state change.
	Since uint64
}

// Config holds the cluster constants, set once by the Config op of the
// first delta of a cluster.
type Config struct {
	// PartitionBits is log2 of the partition count.
	PartitionBits uint8
	// InitialOwner owns every partition of the initial table, or NoSlot.
	InitialOwner Slot
	// SlotReuseEpochs is how many epochs a freed slot stays unused.
	SlotReuseEpochs uint32
	// HandoffEpochs is how long a moved partition reads through from Prev.
	HandoffEpochs uint32
}

// FreeSlot is a slot below SlotCap without a member, reusable once the view
// epoch reaches ReusableAt.
type FreeSlot struct {
	Slot       Slot
	ReusableAt uint64
}

// KeyspaceGen is the generation of the keyspace with the given seed.
// Keyspaces not listed have generation 0.
type KeyspaceGen struct {
	Seed uint64
	Gen  uint32
}

// OpKind identifies the kind of an Op. Its numeric value is part of the
// encoding.
type OpKind uint8

// Op kinds. See Op for the fields each kind uses.
const (
	// KindNewTerm starts a leader term: Term, Slot (the leader).
	KindNewTerm OpKind = 1
	// KindConfig sets the cluster constants: Config.
	KindConfig OpKind = 2
	// KindAddMember admits a member: Slot, Member.
	KindAddMember OpKind = 3
	// KindRemoveMember removes a member and frees its slot: Slot.
	KindRemoveMember OpKind = 4
	// KindSetState changes state and weight: Slot, State, Weight.
	KindSetState OpKind = 5
	// KindSetCell moves a member to another cell: Slot, Cell.
	KindSetCell OpKind = 6
	// KindReshape replaces the grid: Shape, Cells (indexed by slot).
	KindReshape OpKind = 7
	// KindAssign moves partitions: Parts, Owners.
	KindAssign OpKind = 8
	// KindEpoch sets the epoch anchor: Epoch, Period.
	KindEpoch OpKind = 9
	// KindInvalidate bumps a keyspace generation: Seed, Gen.
	KindInvalidate OpKind = 10
)

// String returns the kind name.
func (k OpKind) String() string {
	switch k {
	case KindNewTerm:
		return "NewTerm"
	case KindConfig:
		return "Config"
	case KindAddMember:
		return "AddMember"
	case KindRemoveMember:
		return "RemoveMember"
	case KindSetState:
		return "SetState"
	case KindSetCell:
		return "SetCell"
	case KindReshape:
		return "Reshape"
	case KindAssign:
		return "Assign"
	case KindEpoch:
		return "Epoch"
	case KindInvalidate:
		return "Invalidate"
	default:
		return "OpKind(" + strconv.Itoa(int(k)) + ")"
	}
}

// Op is one change in a Delta. It is a tagged struct: Kind selects which
// fields are meaningful, and the others are ignored by Apply and by the
// encoding. Build ops with the Op* constructors.
type Op struct {
	Kind OpKind
	// Slot is the leader (NewTerm) or the subject member (AddMember,
	// RemoveMember, SetState, SetCell).
	Slot Slot
	// Term is the new term (NewTerm).
	Term uint64
	// Member is the admitted member (AddMember). Joined and Since are
	// ignored and set from the view epoch.
	Member Member
	// State and Weight are the new state and weight (SetState).
	State  MemberState
	Weight uint32
	// Cell is the new cell (SetCell).
	Cell uint32
	// Config is the cluster constants (Config).
	Config Config
	// Shape is the new grid and Cells the new cell of every slot below
	// SlotCap, NoCell for slots without a member (Reshape).
	Shape Shape
	Cells []uint32
	// Parts[i] moves to Owners[i] (Assign).
	Parts, Owners []uint32
	// Epoch and Period are the new epoch anchor (Epoch).
	Epoch  uint64
	Period time.Duration
	// Seed and Gen are the keyspace and its new generation (Invalidate).
	Seed uint64
	Gen  uint32
}

// OpNewTerm starts term with the given leader slot.
func OpNewTerm(term uint64, leader Slot) Op { return Op{Kind: KindNewTerm, Term: term, Slot: leader} }

// OpConfig sets the cluster constants.
func OpConfig(c Config) Op { return Op{Kind: KindConfig, Config: c} }

// OpAddMember admits m at slot.
func OpAddMember(slot Slot, m Member) Op { return Op{Kind: KindAddMember, Slot: slot, Member: m} }

// OpRemoveMember removes the member at slot.
func OpRemoveMember(slot Slot) Op { return Op{Kind: KindRemoveMember, Slot: slot} }

// OpSetState sets the state and weight of the member at slot.
func OpSetState(slot Slot, s MemberState, weight uint32) Op {
	return Op{Kind: KindSetState, Slot: slot, State: s, Weight: weight}
}

// OpSetCell moves the member at slot to cell.
func OpSetCell(slot Slot, cell uint32) Op { return Op{Kind: KindSetCell, Slot: slot, Cell: cell} }

// OpReshape replaces the grid. cells is indexed by slot. The slice is
// retained.
func OpReshape(s Shape, cells []uint32) Op {
	return Op{Kind: KindReshape, Shape: s, Cells: cells}
}

// OpAssign moves parts[i] to owners[i]. The slices are retained.
func OpAssign(parts, owners []uint32) Op { return Op{Kind: KindAssign, Parts: parts, Owners: owners} }

// OpEpoch sets the epoch anchor.
func OpEpoch(epoch uint64, period time.Duration) Op {
	return Op{Kind: KindEpoch, Epoch: epoch, Period: period}
}

// OpInvalidate sets the generation of keyspace seed to gen.
func OpInvalidate(seed uint64, gen uint32) Op { return Op{Kind: KindInvalidate, Seed: seed, Gen: gen} }

// Delta changes a view from Base to Version. Digest is the digest of the
// resulting view, sha256(base digest || canonical encoding of the delta
// without its digest); fill it with Seal or NewDelta.
type Delta struct {
	Base, Version Version
	Digest        Digest
	Ops           []Op
}

// NextVersion returns the version of a delta on base: {Term, 1} if ops start
// with NewTerm, otherwise {base.Term, base.Seq + 1}.
func NextVersion(base Version, ops []Op) Version {
	if len(ops) > 0 && ops[0].Kind == KindNewTerm {
		return Version{Term: ops[0].Term, Seq: 1}
	}

	return Version{Term: base.Term, Seq: base.Seq + 1}
}

// NewDelta returns a sealed delta of ops on base with the next version.
// The ops slice is retained.
func NewDelta(base *View, ops ...Op) *Delta {
	d := &Delta{Base: base.Version(), Version: NextVersion(base.Version(), ops), Ops: ops}
	Seal(base, d)

	return d
}

// Seal sets d.Digest for applying d on base.
func Seal(base *View, d *Delta) { d.Digest = DeltaDigest(base.Digest(), d) }

// DeltaDigest returns the digest of the view that results from applying d on
// a view with digest base. It ignores d.Digest.
func DeltaDigest(base Digest, d *Delta) Digest {
	h := sha256.New()
	_, _ = h.Write(base[:])
	_, _ = h.Write(appendDeltaBody(make([]byte, 0, deltaSizeHint(d)), d))

	var out Digest

	h.Sum(out[:0])

	return out
}

// The monitoring relation (designs/cachemesh.md, "Monitoring and failure
// detection"). A member s in cell c with rank r (its index in
// CellMembers(c)) monitors:
//
//   - every other member of c, and
//   - for each dimension j with Size[j] > 1 and direction +1 and -1
//     (wrapping, counted once when both directions reach the same cell), the
//     member of the adjacent cell a with rank r mod Occupancy(a), if a is
//     not empty.
//
// Observers is the exact inverse of Subjects.

// rank returns the index of s in its cell, or -1.
func rank(cs []Slot, s Slot) int { return slices.Index(cs, s) }

// forEachAdjacent calls fn with each distinct cell adjacent to c along a
// dimension of size > 1, one per (dimension, direction) pair.
func forEachAdjacent(v *View, c uint32, fn func(a uint32)) {
	sh := &v.shape
	for j := range int(sh.Dims) {
		if sh.Size[j] < 2 {
			continue
		}

		up := sh.Step(c, j, 1)
		fn(up)

		if down := sh.Step(c, j, -1); down != up {
			fn(down)
		}
	}
}

// Subjects appends to dst the members slot monitors: its cell-mates in rank
// order, then one member per adjacent cell in dimension then direction (+1,
// -1) order. It appends nothing if slot is not a member.
func Subjects(v *View, slot Slot, dst []Slot) []Slot {
	c := v.Cell(slot)
	if c == NoCell {
		return dst
	}

	cs := v.CellMembers(c)
	r := rank(cs, slot)

	for _, s := range cs {
		if s != slot {
			dst = append(dst, s)
		}
	}

	forEachAdjacent(v, c, func(a uint32) {
		if as := v.CellMembers(a); len(as) > 0 {
			dst = append(dst, as[r%len(as)])
		}
	})

	return dst
}

// Observers appends to dst the members that monitor slot, the exact inverse
// of Subjects: its cell-mates, then for each adjacent cell a (in the order
// Subjects uses) the members of a whose rank mod Occupancy(cell of slot)
// equals slot's rank.
func Observers(v *View, slot Slot, dst []Slot) []Slot {
	c := v.Cell(slot)
	if c == NoCell {
		return dst
	}

	cs := v.CellMembers(c)
	r := rank(cs, slot)

	for _, s := range cs {
		if s != slot {
			dst = append(dst, s)
		}
	}

	occ := len(cs)

	// Members of a reach c in the opposite direction; the set of cells is
	// the same either way, and each is visited once.
	forEachAdjacent(v, c, func(a uint32) {
		as := v.CellMembers(a)
		for i := r; i < len(as); i += occ {
			dst = append(dst, as[i])
		}
	})

	return dst
}

// ObserverCells returns the number of distinct cells holding observers of
// slot, which bounds the ObserverCells suspicion quorum.
func ObserverCells(v *View, slot Slot) int {
	c := v.Cell(slot)
	if c == NoCell {
		return 0
	}

	cs := v.CellMembers(c)
	r := rank(cs, slot)

	n := 0
	if len(cs) > 1 {
		n++
	}

	forEachAdjacent(v, c, func(a uint32) {
		if r < v.Occupancy(a) {
			n++
		}
	})

	return n
}

// Neighbors appends to dst every other member in slot's cell or a cell
// adjacent to it, the data-plane neighbor set, in slot order. It appends
// nothing if slot is not a member.
func Neighbors(v *View, slot Slot, dst []Slot) []Slot {
	c := v.Cell(slot)
	if c == NoCell {
		return dst
	}

	start := len(dst)

	var buf [64]uint32

	for _, nc := range v.shape.AppendNeighborCells(buf[:0], c) {
		for _, s := range v.CellMembers(nc) {
			if s != slot {
				dst = append(dst, s)
			}
		}
	}

	slices.Sort(dst[start:])

	return dst
}
