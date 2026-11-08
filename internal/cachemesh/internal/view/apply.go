// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package view

import (
	"errors"
	"fmt"
	"slices"

	"github.com/Azure/unbounded/internal/cachemesh/internal/hashing"
	"github.com/Azure/unbounded/internal/cachemesh/internal/placement"
	"github.com/Azure/unbounded/internal/cachemesh/internal/topology"
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

	if w > placement.MaxWeight {
		return fmt.Errorf("weight %d above %d", w, placement.MaxWeight)
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
	case m.Key != hashing.MemberKey(m.ID, m.Incarnation):
		return fmt.Errorf("member %q: key does not match ID and incarnation", m.ID)
	}

	return checkStateWeight(m.State, m.Weight)
}

func (a *applier) config(c Config) error {
	v := a.v
	if v.hasConfig {
		return errors.New("config already set")
	}

	if !placement.ValidBits(c.PartitionBits) {
		return fmt.Errorf("partition bits %d", c.PartitionBits)
	}

	if c.InitialOwner != NoSlot && c.InitialOwner >= MaxSlots {
		return fmt.Errorf("initial owner %d", c.InitialOwner)
	}

	sh, err := topology.NewShape(1)
	if err != nil {
		return err
	}

	v.config, v.hasConfig, v.shape = c, true, sh
	v.table = placement.NewTable(c.PartitionBits, c.InitialOwner)
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

func (a *applier) reshape(sh *topology.Shape, cells []uint32) error {
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
		} else if c != topology.NoCell {
			return fmt.Errorf("slot %d: free slot with cell %d", s, c)
		}
	}

	v.shape = *sh

	for s, c := range cells {
		if c != topology.NoCell && v.mem[s>>memShift].mem[s&memMask].Cell != c {
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
