// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package view

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"slices"
)

// Errors returned by Apply. Structural errors wrap ErrInvalid.
var (
	// ErrBase means the delta's Base is not the version of the view.
	ErrBase = errors.New("view: delta base does not match view version")
	// ErrVersion means the delta's Version does not follow its Base.
	ErrVersion = errors.New("view: invalid delta version")
	// ErrDigest means the delta's Digest does not match the view it
	// produces, for example because the base forked.
	ErrDigest = errors.New("view: delta digest mismatch")
	// ErrInvalid means the delta or view violates a structural rule.
	ErrInvalid = errors.New("view: invalid")
)

// Apply returns the view that results from applying d on base. base is not
// modified and the result shares every storage chunk d does not touch.
//
// Apply checks, in order: d.Base equals base.Version() (ErrBase); d.Version
// is NextVersion(d.Base, d.Ops) and a NewTerm term exceeds the base term
// (ErrVersion); d.Digest is DeltaDigest(base.Digest(), d) (ErrDigest); then
// every op in order and, at the end, that the leader is a member when the
// term is non-zero and that no partition is owned by a non-member while the
// view has members (ErrInvalid).
//
// The cost is proportional to the ops and the storage chunks they touch,
// except Config and Reshape, which rebuild the cell index.
func Apply(base *View, d *Delta) (*View, error) {
	if base.version != d.Base {
		return nil, fmt.Errorf("%w: base %v, delta base %v", ErrBase, base.version, d.Base)
	}

	if len(d.Ops) > 0 && d.Ops[0].Kind == KindNewTerm && d.Ops[0].Term <= base.version.Term {
		return nil, fmt.Errorf("%w: new term %d not above %d", ErrVersion, d.Ops[0].Term, base.version.Term)
	}

	if want := NextVersion(d.Base, d.Ops); d.Version != want {
		return nil, fmt.Errorf("%w: version %v, want %v", ErrVersion, d.Version, want)
	}

	if len(d.Ops) > MaxDeltaOps {
		return nil, fmt.Errorf("%w: %d ops", ErrInvalid, len(d.Ops))
	}

	if DeltaDigest(base.digest, d) != d.Digest {
		return nil, ErrDigest
	}

	nv := *base
	a := applier{v: &nv}

	for i := range d.Ops {
		op := &d.Ops[i]
		if err := a.op(op, i); err != nil {
			return nil, fmt.Errorf("%w: op %d (%v): %w", ErrInvalid, i, op.Kind, err)
		}
	}

	a.finish()

	if nv.version = d.Version; nv.version.Term > 0 && !nv.IsMember(nv.leader) {
		return nil, fmt.Errorf("%w: leader slot %d is not a member", ErrInvalid, nv.leader)
	}

	if nv.n > 0 && nv.orphans > 0 {
		return nil, fmt.Errorf("%w: %d partitions owned by non-members", ErrInvalid, nv.orphans)
	}

	nv.digest = d.Digest

	return &nv, nil
}

// applier mutates a shallow copy of a view, copying each storage chunk
// before its first write.
type applier struct {
	v *View

	memOwned  []bool // nil until the outer slice is cloned
	loadOwned []bool
	idOwned   []bool

	cellChunkOwned []bool
	cellsFresh     bool            // every cell slice was built by this applier
	cellOwned      map[uint32]bool // cell slices copied by this applier
	dirty          []uint32        // cells to sort in finish
}

var (
	errNoConfig = errors.New("no config")
	errNoMember = errors.New("no member at slot")
)

func (a *applier) op(op *Op, i int) error {
	v := a.v

	switch op.Kind {
	case KindNewTerm:
		if i != 0 {
			return errors.New("NewTerm must be the first op")
		}

		v.leader = op.Slot

		return nil
	case KindConfig:
		return a.config(op.Config)
	case KindEpoch:
		if op.Epoch < v.epoch {
			return fmt.Errorf("epoch %d below %d", op.Epoch, v.epoch)
		}

		if op.Period <= 0 {
			return fmt.Errorf("period %v not positive", op.Period)
		}

		v.epoch, v.period = op.Epoch, op.Period

		return nil
	case KindInvalidate:
		j, ok := findGen(v.gens, op.Seed)

		cur := uint32(0)
		if ok {
			cur = v.gens[j].Gen
		}

		if op.Gen <= cur {
			return fmt.Errorf("generation %d not above %d", op.Gen, cur)
		}

		if ok {
			v.gens = slices.Clone(v.gens)
			v.gens[j].Gen = op.Gen
		} else {
			if len(v.gens) >= MaxGens {
				return errors.New("too many keyspace generations")
			}

			v.gens = slices.Insert(slices.Clone(v.gens), j, KeyspaceGen{Seed: op.Seed, Gen: op.Gen})
		}

		return nil
	}

	if !v.hasConfig {
		return errNoConfig
	}

	switch op.Kind {
	case KindAddMember:
		return a.add(op.Slot, &op.Member)
	case KindRemoveMember:
		return a.remove(op.Slot)
	case KindSetState:
		m, ok := v.Member(op.Slot)
		if !ok {
			return errNoMember
		}

		if err := checkStateWeight(op.State, op.Weight); err != nil {
			return err
		}

		w := a.memW(op.Slot)
		if m.State != op.State {
			w.Since = v.epoch
		}

		w.State, w.Weight = op.State, op.Weight

		return nil
	case KindSetCell:
		m, ok := v.Member(op.Slot)
		if !ok {
			return errNoMember
		}

		if op.Cell >= v.shape.Cells() {
			return fmt.Errorf("cell %d out of range", op.Cell)
		}

		if m.Cell != op.Cell {
			a.cellRemove(m.Cell, op.Slot)
			a.memW(op.Slot).Cell = op.Cell
			a.cellAdd(op.Cell, op.Slot)
		}

		return nil
	case KindReshape:
		return a.reshape(&op.Shape, op.Cells)
	case KindAssign:
		return a.assign(op.Parts, op.Owners)
	default:
		return fmt.Errorf("unknown op kind %d", op.Kind)
	}
}

func checkStateWeight(s MemberState, w uint32) error {
	if !s.Valid() {
		return fmt.Errorf("invalid state %d", s)
	}

	if w > MaxWeight {
		return fmt.Errorf("weight %d above %d", w, MaxWeight)
	}

	if s != Active && w != 0 {
		return fmt.Errorf("weight %d in state %v", w, s)
	}

	return nil
}

func checkMember(m *Member) error {
	switch {
	case m.ID == "" || len(m.ID) > MaxIDLen:
		return fmt.Errorf("ID length %d", len(m.ID))
	case len(m.Addr) > MaxAddrLen:
		return fmt.Errorf("addr length %d", len(m.Addr))
	case len(m.Zone) > MaxZoneLen:
		return fmt.Errorf("zone length %d", len(m.Zone))
	case m.Key != MemberKey(m.ID, m.Incarnation):
		return fmt.Errorf("member %q: key does not match ID and incarnation", m.ID)
	}

	return checkStateWeight(m.State, m.Weight)
}

func (a *applier) config(c Config) error {
	v := a.v
	if v.hasConfig {
		return errors.New("config already set")
	}

	if !ValidPartitionBits(c.PartitionBits) {
		return fmt.Errorf("partition bits %d", c.PartitionBits)
	}

	if c.InitialOwner != NoSlot && c.InitialOwner >= MaxSlots {
		return fmt.Errorf("initial owner %d", c.InitialOwner)
	}

	sh, err := NewShape(1)
	if err != nil {
		return err
	}

	v.config, v.hasConfig, v.shape = c, true, sh
	v.table = NewTable(c.PartitionBits, c.InitialOwner)
	v.cells = make([][][]Slot, 1)
	a.cellsFresh = true

	if c.InitialOwner != NoSlot {
		p := uint32(1) << c.PartitionBits
		*a.loadW(c.InitialOwner) += p
		v.orphans += uint64(p)
	}

	return nil
}

func (a *applier) add(s Slot, in *Member) error {
	v := a.v

	if err := checkMember(in); err != nil {
		return err
	}

	if in.Cell >= v.shape.Cells() {
		return fmt.Errorf("cell %d out of range", in.Cell)
	}

	if v.lookupID(in.ID) != NoSlot {
		return fmt.Errorf("member %q already present", in.ID)
	}

	switch {
	case s == v.slotCap:
		if s >= MaxSlots {
			return errors.New("too many slots")
		}

		a.appendSlot()
	case s < v.slotCap:
		if v.IsMember(s) {
			return fmt.Errorf("slot %d in use", s)
		}

		if r := v.mem[s>>memShift].reuse[s&memMask]; r > v.epoch {
			return fmt.Errorf("slot %d reusable at epoch %d, now %d", s, r, v.epoch)
		}
	default:
		return fmt.Errorf("slot %d beyond next slot %d", s, v.slotCap)
	}

	w := a.memW(s)
	*w = *in
	w.Joined, w.Since = v.epoch, v.epoch

	ci := s >> memShift
	v.mem[ci].reuse[s&memMask] = 0
	v.minReuse[ci] = chunkMinReuse(v, ci)
	v.n++
	v.orphans -= uint64(v.Load(s))

	a.cellAdd(in.Cell, s)
	a.idInsert(s)

	return nil
}

func (a *applier) remove(s Slot) error {
	v := a.v

	m, ok := v.Member(s)
	if !ok {
		return errNoMember
	}

	a.idDelete(s, m.ID)
	a.cellRemove(m.Cell, s)

	w := a.memW(s)
	*w = Member{}

	ci := s >> memShift

	r := v.epoch + uint64(v.config.SlotReuseEpochs)
	if r < v.epoch {
		// Saturate rather than wrap, which would free the slot at once.
		r = math.MaxUint64
	}

	v.mem[ci].reuse[s&memMask] = r
	v.minReuse[ci] = min(v.minReuse[ci], r)
	v.n--
	v.orphans += uint64(v.Load(s))

	return nil
}

func chunkMinReuse(v *View, ci uint32) uint64 {
	c := v.mem[ci]
	m := uint64(noReuse)

	for j := range memChunk {
		if Slot(ci)<<memShift+Slot(j) >= v.slotCap {
			break
		}

		if c.mem[j].State == 0 {
			m = min(m, c.reuse[j])
		}
	}

	return m
}

func (a *applier) reshape(sh *Shape, cells []uint32) error {
	v := a.v

	if err := sh.Validate(); err != nil {
		return err
	}

	nc := sh.Cells()
	if nc > MaxCells {
		return fmt.Errorf("%d cells", nc)
	}

	if len(cells) != int(v.slotCap) {
		return fmt.Errorf("%d cells for %d slots", len(cells), v.slotCap)
	}

	for s, c := range cells {
		if v.IsMember(Slot(s)) {
			if c >= nc {
				return fmt.Errorf("slot %d: cell %d out of range", s, c)
			}
		} else if c != NoCell {
			return fmt.Errorf("slot %d: free slot with cell %d", s, c)
		}
	}

	v.shape = *sh

	for s, c := range cells {
		if c != NoCell && v.mem[s>>memShift].mem[s&memMask].Cell != c {
			a.memW(Slot(s)).Cell = c
		}
	}

	v.cells = buildCells(v)
	a.cellChunkOwned = nil
	a.cellsFresh = true
	a.cellOwned = nil
	a.dirty = a.dirty[:0]

	return nil
}

// buildCells returns a fresh, sorted cell index for v's members and shape.
func buildCells(v *View) [][][]Slot {
	nc := v.shape.Cells()
	out := make([][][]Slot, (nc+cellChunk-1)/cellChunk)

	occ := make([]uint32, nc)

	v.ForEachMember(func(_ Slot, m *Member) bool {
		occ[m.Cell]++

		return true
	})

	// One backing array for all cells, split by occupancy.
	backing := make([]Slot, v.n)
	off := 0

	for c, o := range occ {
		if o == 0 {
			continue
		}

		ci := c >> cellShift
		if out[ci] == nil {
			out[ci] = make([][]Slot, cellChunk)
		}

		out[ci][c&cellMask] = backing[off : off : off+int(o)]
		off += int(o)
	}

	v.ForEachMember(func(s Slot, m *Member) bool {
		p := &out[m.Cell>>cellShift][m.Cell&cellMask]
		*p = append(*p, s)

		return true
	})

	for _, ch := range out {
		for _, cs := range ch {
			sortCell(v, cs)
		}
	}

	return out
}

func sortCell(v *View, cs []Slot) {
	if len(cs) < 2 {
		return
	}

	slices.SortFunc(cs, func(x, y Slot) int {
		kx, ky := v.Key(x), v.Key(y)

		switch {
		case kx < ky:
			return -1
		case kx > ky:
			return 1
		case x < y:
			return -1
		case x > y:
			return 1
		}

		return 0
	})
}

func (a *applier) assign(parts, owners []uint32) error {
	v := a.v

	if len(parts) != len(owners) {
		return fmt.Errorf("%d parts, %d owners", len(parts), len(owners))
	}

	for _, o := range owners {
		if !v.IsMember(o) {
			return fmt.Errorf("owner %d is not a member", o)
		}
	}

	old := v.table

	nt, err := old.Assign(parts, owners, uint32(v.epoch))
	if err != nil {
		return err
	}

	for i, p := range parts {
		o, n := old.Owner(p), owners[i]
		if o == n {
			continue
		}

		if o != NoSlot {
			*a.loadW(o)--

			if !v.IsMember(o) {
				v.orphans--
			}
		}

		*a.loadW(n)++
	}

	v.table = nt

	return nil
}

// finish sorts the cells changed since the last rebuild.
func (a *applier) finish() {
	for _, c := range a.dirty {
		sortCell(a.v, a.v.cells[c>>cellShift][c&cellMask])
	}
}

// memW returns the member at slot s for writing.
func (a *applier) memW(s Slot) *Member {
	v := a.v
	ci := s >> memShift

	if a.memOwned == nil {
		v.mem = slices.Clone(v.mem)
		v.minReuse = slices.Clone(v.minReuse)
		a.memOwned = make([]bool, len(v.mem), len(v.mem)+1)
	}

	if !a.memOwned[ci] {
		c := *v.mem[ci]
		v.mem[ci] = &c
		a.memOwned[ci] = true
	}

	return &v.mem[ci].mem[s&memMask]
}

func (a *applier) appendSlot() {
	v := a.v
	if v.slotCap == uint32(len(v.mem))<<memShift {
		if a.memOwned == nil {
			v.mem = slices.Clone(v.mem)
			v.minReuse = slices.Clone(v.minReuse)
			a.memOwned = make([]bool, len(v.mem), len(v.mem)+1)
		}

		v.mem = append(v.mem, &memberChunk{})
		v.minReuse = append(v.minReuse, noReuse)
		a.memOwned = append(a.memOwned, true)
	}

	v.slotCap++
}

// loadW returns the load of slot s for writing.
func (a *applier) loadW(s Slot) *uint32 {
	v := a.v
	ci := int(s >> loadShift)

	if a.loadOwned == nil {
		v.loads = slices.Clone(v.loads)
		a.loadOwned = make([]bool, len(v.loads))
	}

	for len(v.loads) <= ci {
		v.loads = append(v.loads, nil)
		a.loadOwned = append(a.loadOwned, false)
	}

	if !a.loadOwned[ci] {
		if v.loads[ci] == nil {
			v.loads[ci] = make([]uint32, loadChunk)
		} else {
			v.loads[ci] = slices.Clone(v.loads[ci])
		}

		a.loadOwned[ci] = true
	}

	return &v.loads[ci][s&loadMask]
}

// cellW returns the slice of cell c for writing and marks it for sorting.
func (a *applier) cellW(c uint32) *[]Slot {
	v := a.v
	ci := c >> cellShift

	if !a.cellsFresh {
		if a.cellChunkOwned == nil {
			v.cells = slices.Clone(v.cells)
			a.cellChunkOwned = make([]bool, len(v.cells))
		}

		if !a.cellChunkOwned[ci] {
			if v.cells[ci] == nil {
				v.cells[ci] = make([][]Slot, cellChunk)
			} else {
				v.cells[ci] = slices.Clone(v.cells[ci])
			}

			a.cellChunkOwned[ci] = true
		}
	} else if v.cells[ci] == nil {
		v.cells[ci] = make([][]Slot, cellChunk)
	}

	p := &v.cells[ci][c&cellMask]

	if !a.cellsFresh && !a.cellOwned[c] {
		if a.cellOwned == nil {
			a.cellOwned = make(map[uint32]bool)
		}

		*p = slices.Grow(slices.Clone(*p), 1)
		a.cellOwned[c] = true
	}

	if len(a.dirty) == 0 || a.dirty[len(a.dirty)-1] != c {
		a.dirty = append(a.dirty, c)
	}

	return p
}

func (a *applier) cellAdd(c uint32, s Slot) {
	p := a.cellW(c)
	*p = append(*p, s)
}

func (a *applier) cellRemove(c uint32, s Slot) {
	p := a.cellW(c)
	if i := slices.Index(*p, s); i >= 0 {
		*p = slices.Delete(*p, i, i+1)
	}
}

// idW returns the ID index entry i for writing.
func (a *applier) idW(i uint64) *Slot {
	v := a.v
	ci := i >> idxShift

	if a.idOwned == nil {
		v.ids = slices.Clone(v.ids)
		a.idOwned = make([]bool, len(v.ids))
	}

	if !a.idOwned[ci] {
		v.ids[ci] = slices.Clone(v.ids[ci])
		a.idOwned[ci] = true
	}

	return &v.ids[ci][i&(idxChunk-1)]
}

// idInsert adds the member at s, already stored, to the ID index.
func (a *applier) idInsert(s Slot) {
	v := a.v
	if uint64(v.idCount+1)*2 > v.idSize() {
		v.ids = buildIDs(v, max(idxChunk, 2*v.idSize()))
		a.idOwned = make([]bool, len(v.ids))

		for i := range a.idOwned {
			a.idOwned[i] = true
		}

		return
	}

	mask := v.idSize() - 1

	i := idHash(v.mem[s>>memShift].mem[s&memMask].ID) & mask
	for v.idAt(i) != NoSlot {
		i = (i + 1) & mask
	}

	*a.idW(i) = s
	v.idCount++
}

// idDelete removes slot s with the given ID using backward-shift deletion.
func (a *applier) idDelete(s Slot, id string) {
	v := a.v
	mask := v.idSize() - 1

	i := idHash(id) & mask
	for v.idAt(i) != s {
		i = (i + 1) & mask
	}

	for j := (i + 1) & mask; ; j = (j + 1) & mask {
		t := v.idAt(j)
		if t == NoSlot {
			break
		}

		home := idHash(v.mem[t>>memShift].mem[t&memMask].ID) & mask
		// Move t back to i unless its home lies cyclically in (i, j].
		if (j-home)&mask >= (j-i)&mask {
			*a.idW(i) = t
			i = j
		}
	}

	*a.idW(i) = NoSlot
	v.idCount--
}

// buildIDs returns a fresh ID index of the given size (a power of two
// multiple of idxChunk) holding every member of v, and sets v.idCount.
func buildIDs(v *View, size uint64) [][]Slot {
	out := make([][]Slot, size>>idxShift)
	for i := range out {
		c := make([]Slot, idxChunk)
		for j := range c {
			c[j] = NoSlot
		}

		out[i] = c
	}

	mask := size - 1
	v.idCount = 0

	v.ForEachMember(func(s Slot, m *Member) bool {
		i := idHash(m.ID) & mask
		for out[i>>idxShift][i&(idxChunk-1)] != NoSlot {
			i = (i + 1) & mask
		}

		out[i>>idxShift][i&(idxChunk-1)] = s
		v.idCount++

		return true
	})

	return out
}

// idSizeFor returns the index size for n members.
func idSizeFor(n int) uint64 {
	size := uint64(idxChunk)
	for uint64(n)*2 > size {
		size *= 2
	}

	return size
}

// Validate checks every structural invariant of v in O(members +
// partitions + cells): member records (keys match IDs and incarnations,
// unique IDs, states, weights, cells in range), the ID index, the cell index
// (matches members, sorted by key then slot), free slots, per-slot loads
// against the table, that owners are members while the view has members,
// that the table matches the config, the shape, the leader, and the sorted
// keyspace generations. Errors wrap ErrInvalid.
func Validate(v *View) error {
	if err := validate(v); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	return nil
}

func validate(v *View) error {
	for i := 1; i < len(v.gens); i++ {
		if v.gens[i-1].Seed >= v.gens[i].Seed {
			return fmt.Errorf("generations not sorted at %d", i)
		}
	}

	for _, g := range v.gens {
		if g.Gen == 0 {
			return fmt.Errorf("zero generation for seed %x", g.Seed)
		}
	}

	if len(v.gens) > MaxGens {
		return fmt.Errorf("%d generations", len(v.gens))
	}

	if v.period < 0 {
		return fmt.Errorf("negative period")
	}

	if !v.hasConfig {
		if v.n != 0 || v.slotCap != 0 || v.table != nil || v.config != (Config{}) || v.shape != (Shape{}) {
			return fmt.Errorf("members, table, or shape without config")
		}

		if v.version.Term > 0 {
			return fmt.Errorf("term %d without a leader member", v.version.Term)
		}

		return nil
	}

	if err := validateLayout(v); err != nil {
		return err
	}

	if err := validateMembers(v); err != nil {
		return err
	}

	if err := validateCells(v); err != nil {
		return err
	}

	if err := validateLoads(v); err != nil {
		return err
	}

	if v.version.Term > 0 && !v.IsMember(v.leader) {
		return fmt.Errorf("leader slot %d is not a member", v.leader)
	}

	return nil
}

func validateLayout(v *View) error {
	c := v.config
	if !ValidPartitionBits(c.PartitionBits) {
		return fmt.Errorf("partition bits %d", c.PartitionBits)
	}

	if c.InitialOwner != NoSlot && c.InitialOwner >= MaxSlots {
		return fmt.Errorf("initial owner %d", c.InitialOwner)
	}

	if v.table == nil || v.table.Bits() != c.PartitionBits {
		return fmt.Errorf("table does not match %d partition bits", c.PartitionBits)
	}

	if err := v.shape.Validate(); err != nil {
		return err
	}

	if v.shape.Cells() > MaxCells {
		return fmt.Errorf("%d cells", v.shape.Cells())
	}

	if want := (v.shape.Cells() + cellChunk - 1) / cellChunk; len(v.cells) != int(want) {
		return fmt.Errorf("cell index has %d chunks, want %d", len(v.cells), want)
	}

	if v.slotCap > MaxSlots {
		return fmt.Errorf("slot cap %d", v.slotCap)
	}

	if want := (int(v.slotCap) + memChunk - 1) / memChunk; len(v.mem) != want || len(v.minReuse) != want {
		return fmt.Errorf("member storage has %d chunks, want %d", len(v.mem), want)
	}

	if len(v.ids) != 0 && v.idSize() < idxChunk || v.idSize()&(v.idSize()-1) != 0 {
		return fmt.Errorf("ID index size %d", v.idSize())
	}

	if uint64(v.n)*2 > v.idSize() && v.n > 0 {
		return fmt.Errorf("ID index too small")
	}

	return nil
}

func validateMembers(v *View) error {
	n := 0
	ids := make(map[string]Slot, v.n)

	for ci, c := range v.mem {
		minR := uint64(noReuse)

		for j := range memChunk {
			s := Slot(ci<<memShift + j)
			m := &c.mem[j]

			if s >= v.slotCap {
				if *m != (Member{}) || c.reuse[j] != 0 {
					return fmt.Errorf("slot %d beyond cap is not empty", s)
				}

				continue
			}

			if m.State == 0 {
				if *m != (Member{}) {
					return fmt.Errorf("free slot %d has member data", s)
				}

				minR = min(minR, c.reuse[j])

				continue
			}

			if c.reuse[j] != 0 {
				return fmt.Errorf("slot %d: member with reuse epoch", s)
			}

			if err := checkMember(m); err != nil {
				return fmt.Errorf("slot %d: %w", s, err)
			}

			if m.Cell >= v.shape.Cells() {
				return fmt.Errorf("slot %d: cell %d out of range", s, m.Cell)
			}

			if m.Joined > m.Since {
				return fmt.Errorf("slot %d: joined %d after since %d", s, m.Joined, m.Since)
			}

			if prev, dup := ids[m.ID]; dup {
				return fmt.Errorf("ID %q at slots %d and %d", m.ID, prev, s)
			}

			ids[m.ID] = s

			if got := v.lookupID(m.ID); got != s {
				return fmt.Errorf("ID index maps %q to %d, want %d", m.ID, got, s)
			}

			n++
		}

		if v.minReuse[ci] != minR {
			return fmt.Errorf("chunk %d: min reuse %d, want %d", ci, v.minReuse[ci], minR)
		}
	}

	if n != v.n || v.idCount != n {
		return fmt.Errorf("member count %d, recorded %d, indexed %d", n, v.n, v.idCount)
	}

	return nil
}

func validateCells(v *View) error {
	total := 0

	for ci, ch := range v.cells {
		if ch == nil {
			continue
		}

		if len(ch) != cellChunk {
			return fmt.Errorf("cell chunk %d has %d cells", ci, len(ch))
		}

		for j, cs := range ch {
			c := uint32(ci<<cellShift + j)
			if c >= v.shape.Cells() && len(cs) > 0 {
				return fmt.Errorf("members in cell %d beyond shape", c)
			}

			for i, s := range cs {
				if v.Cell(s) != c {
					return fmt.Errorf("cell %d lists slot %d in cell %d", c, s, v.Cell(s))
				}

				if i > 0 {
					p := cs[i-1]
					if kp, ks := v.Key(p), v.Key(s); kp > ks || kp == ks && p >= s {
						return fmt.Errorf("cell %d not sorted at %d", c, i)
					}
				}
			}

			total += len(cs)
		}
	}

	if total != v.n {
		return fmt.Errorf("cell index holds %d slots, want %d", total, v.n)
	}

	return nil
}

func validateLoads(v *View) error {
	loads := v.table.Loads(nil)
	orphans := uint64(0)

	for s, l := range loads {
		if l > 0 && !v.IsMember(Slot(s)) {
			orphans += uint64(l)
		}
	}

	for s := range max(len(loads), len(v.loads)<<loadShift) {
		want := uint32(0)
		if s < len(loads) {
			want = loads[s]
		}

		if got := v.Load(Slot(s)); got != want {
			return fmt.Errorf("slot %d: load %d, table has %d", s, got, want)
		}
	}

	if orphans != v.orphans {
		return fmt.Errorf("orphaned partitions %d, recorded %d", orphans, v.orphans)
	}

	if v.n > 0 && orphans > 0 {
		return fmt.Errorf("%d partitions owned by non-members", orphans)
	}

	return nil
}

// Equal reports whether a and b have identical content, including version
// and digest. Storage sharing is irrelevant.
func Equal(a, b *View) bool {
	if a == b {
		return true
	}

	if a.version != b.version || a.digest != b.digest || a.leader != b.leader ||
		a.config != b.config || a.hasConfig != b.hasConfig || a.shape != b.shape ||
		a.epoch != b.epoch || a.period != b.period || a.n != b.n || a.slotCap != b.slotCap ||
		!slices.Equal(a.gens, b.gens) {
		return false
	}

	if (a.table == nil) != (b.table == nil) || a.table != nil && !EqualTables(a.table, b.table) {
		return false
	}

	for ci := range a.mem {
		if a.mem[ci] != b.mem[ci] && *a.mem[ci] != *b.mem[ci] {
			return false
		}
	}

	return true
}

// DefaultDeltaLogSize is the default number of deltas a DeltaLog keeps.
const DefaultDeltaLogSize = 512

// LogEntry is a logged delta and its canonical encoding.
type LogEntry struct {
	Delta   *Delta
	Encoded []byte
}

type logEntry struct {
	LogEntry

	baseDigest Digest
}

// DeltaLog is a bounded chain of the most recent deltas a node applied, kept
// with their encodings so serving catch-up does not re-encode. It is not
// safe for concurrent use.
type DeltaLog struct {
	ring  []logEntry
	start int // index of the oldest entry
	n     int

	// head is the version and digest at the end of the chain (or the anchor
	// when the log is empty).
	head       Version
	headDigest Digest
}

// NewDeltaLog returns a log holding up to size deltas (DefaultDeltaLogSize
// if size <= 0), anchored at the genesis view.
func NewDeltaLog(size int) *DeltaLog {
	if size <= 0 {
		size = DefaultDeltaLogSize
	}

	l := &DeltaLog{ring: make([]logEntry, size)}
	l.Reset(Genesis())

	return l
}

// Reset empties the log and anchors it at v, for example after installing a
// snapshot.
func (l *DeltaLog) Reset(v *View) {
	clear(l.ring)
	l.start, l.n = 0, 0
	l.head, l.headDigest = v.Version(), v.Digest()
}

// Len returns the number of logged deltas.
func (l *DeltaLog) Len() int { return l.n }

// Head returns the version and digest at the end of the chain.
func (l *DeltaLog) Head() (Version, Digest) { return l.head, l.headDigest }

// Append adds d, which must have been applied to the log head (d.Base equals
// the head version), evicting the oldest delta when full. encoded is the
// canonical encoding of d; if nil it is computed. The log retains d and
// encoded.
func (l *DeltaLog) Append(d *Delta, encoded []byte) error {
	if d.Base != l.head {
		return fmt.Errorf("%w: log head %v, delta base %v", ErrBase, l.head, d.Base)
	}

	if encoded == nil {
		encoded = EncodeDelta(d)
	}

	i := (l.start + l.n) % len(l.ring)
	if l.n == len(l.ring) {
		l.start = (l.start + 1) % len(l.ring)
	} else {
		l.n++
	}

	l.ring[i] = logEntry{LogEntry: LogEntry{Delta: d, Encoded: encoded}, baseDigest: l.headDigest}
	l.head, l.headDigest = d.Version, d.Digest

	return nil
}

// Since returns up to limit deltas (all if max <= 0) that follow the view
// (have, haveDigest), oldest first, and true, when that view is the head or
// the base of a logged delta. It returns nil and true when have is the head
// and false when the log cannot serve the request (the caller sends a
// snapshot). The returned entries are shared.
func (l *DeltaLog) Since(have Version, haveDigest Digest, limit int) ([]LogEntry, bool) {
	if have == l.head {
		return nil, haveDigest == l.headDigest
	}

	for k := range l.n {
		e := &l.ring[(l.start+k)%len(l.ring)]
		if e.Delta.Base != have {
			continue
		}

		if e.baseDigest != haveDigest {
			return nil, false
		}

		cnt := l.n - k
		if limit > 0 {
			cnt = min(cnt, limit)
		}

		out := make([]LogEntry, cnt)
		for j := range out {
			out[j] = l.ring[(l.start+k+j)%len(l.ring)].LogEntry
		}

		return out, true
	}

	return nil, false
}

// DefaultInternerSize is the default number of entries per Interner table.
const DefaultInternerSize = 4096

type applyKey struct {
	base  Digest
	delta Digest
}

// Interner shares identical views and decoded deltas, for example across
// thousands of simulated members that apply the same deltas. It is only a
// cache: results equal those of Apply and DecodeDelta. Each table is bounded
// and evicts in insertion order. It is not safe for concurrent use.
// Production code does not use it.
type Interner struct {
	byID    FIFO[Digest, *View]
	deltas  FIFO[Digest, *Delta]
	applied FIFO[applyKey, *View]

	// Hits and Misses count Apply and Delta cache lookups.
	Hits, Misses uint64
}

// NewInterner returns an interner keeping up to size entries per table
// (DefaultInternerSize if size <= 0).
func NewInterner(size int) *Interner {
	if size <= 0 {
		size = DefaultInternerSize
	}

	return &Interner{
		byID:    NewFIFO[Digest, *View](size),
		deltas:  NewFIFO[Digest, *Delta](size),
		applied: NewFIFO[applyKey, *View](size),
	}
}

// View returns the interned view with v's digest, interning v if there is
// none. Views with equal digests are assumed equal.
func (in *Interner) View(v *View) *View {
	if w, ok := in.byID.Get(v.Digest()); ok && w.Version() == v.Version() {
		return w
	}

	in.byID.Put(v.Digest(), v)

	return v
}

// Apply is Apply(base, d) memoized on (base digest, delta digest). The
// result is interned.
func (in *Interner) Apply(base *View, d *Delta) (*View, error) {
	k := applyKey{base: base.Digest(), delta: d.Digest}
	if v, ok := in.applied.Get(k); ok && base.Version() == d.Base && v.Version() == d.Version {
		in.Hits++

		return v, nil
	}

	in.Misses++

	v, err := Apply(base, d)
	if err != nil {
		return nil, err
	}

	v = in.View(v)
	in.applied.Put(k, v)

	return v, nil
}

// Delta is DecodeDelta(encoded) memoized on the content of encoded. The
// returned delta is shared and must not be modified.
func (in *Interner) Delta(encoded []byte) (*Delta, error) {
	k := Digest(sha256.Sum256(encoded))
	if d, ok := in.deltas.Get(k); ok {
		in.Hits++

		return d, nil
	}

	in.Misses++

	d, err := DecodeDelta(encoded)
	if err != nil {
		return nil, err
	}

	in.deltas.Put(k, d)

	return d, nil
}

// FIFO is a bounded map that evicts the oldest insertion. It backs the
// simulation-wide caches (Interner and the node package's Shared). It is not
// safe for concurrent use.
type FIFO[K comparable, V any] struct {
	m     map[K]V
	order []K
	next  int
}

// NewFIFO returns a FIFO holding at most n entries.
func NewFIFO[K comparable, V any](n int) FIFO[K, V] {
	return FIFO[K, V]{m: make(map[K]V, n), order: make([]K, 0, n)}
}

// Get returns the value stored for k.
func (f *FIFO[K, V]) Get(k K) (V, bool) {
	v, ok := f.m[k]

	return v, ok
}

// Put stores v for k. Replacing an existing key keeps its eviction order;
// inserting a new key into a full FIFO evicts the oldest key.
func (f *FIFO[K, V]) Put(k K, v V) {
	if _, ok := f.m[k]; ok {
		f.m[k] = v

		return
	}

	if len(f.order) < cap(f.order) {
		f.order = append(f.order, k)
	} else {
		delete(f.m, f.order[f.next])
		f.order[f.next] = k
		f.next = (f.next + 1) % len(f.order)
	}

	f.m[k] = v
}
