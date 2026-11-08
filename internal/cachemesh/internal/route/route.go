// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package route is the cachemesh request hot path: given a key, or the
// header of a request received from another member, it decides where the
// request goes next.
//
// # Contract
//
// A State is an immutable snapshot of everything routing needs from the
// control plane: one *view.View and the slot of this member in it. It also
// points at the long-lived, concurrently updated pieces shared by all states
// of one member, bundled in an Env: the health.Tracker (atomics), the
// epoch.Tracker, the gateway list used while this member is not in the view,
// and the Notes that collect view versions seen in incoming headers.
//
// The runtime keeps the current *State in an atomic.Pointer and replaces it
// whenever the view or this member's slot changes. NewState is O(1): it reads
// a handful of fields from the view and never scans members or partitions,
// because simulations rebuild states on thousands of members per delta.
//
// Every State method is safe for concurrent use, never blocks, never reads
// the clock, and the routing methods (Route, RouteHash, RoutePartition,
// RouteMany, Forward) never allocate on success. Results depend only on the
// view, the partition, and the health and epoch trackers; nothing iterates
// a map or uses randomness.
//
// # Routing
//
// A key hashes (hashing.KeyHash with the keyspace seed) to a partition (the
// top PartitionBits bits); the partition table names its owner. With self
// the routing member and its cell cx, owner o in cell co:
//
//   - o == self: KindSelf.
//   - co == cx or co adjacent to cx (one coordinate differs): the owner is a
//     neighbor and the hop is final. Backups are o's cell-mates ranked by
//     usability (Healthy before Degraded, Unusable excluded) and then by
//     hashing.Score(member.Key, partition) with hashing.Better tie-breaks.
//     If o is Healthy, or Degraded with no Healthy backup, or no backup is
//     usable at all: KindOwner with Next = o and Alternate = best backup.
//     Otherwise the best backup takes the request: KindBackup with
//     FlagBackup, Alternate = the next backup or, if o is Degraded, o. When
//     self is itself the best backup it serves the request: KindSelf with
//     Owner != self and FlagBackup set in the header.
//   - otherwise relay: for each dimension where cx and co differ, lowest
//     first, the candidate cell is cx with that coordinate replaced by co's.
//     Next is the best usable member of the first candidate cell that has
//     one (same ranking as backups); Alternate is the second best of that
//     cell or the best of the next candidate cell. If no candidate cell has
//     a usable member, health is ignored for this decision. If every
//     candidate cell is empty: KindDirect to o with FlagDirect, a connection
//     outside the neighbor budget.
//
// Each relay hop fixes one coordinate and the final hop is direct, so on a
// consistent view a request takes at most D hops (D grid dimensions). Next
// is never self. Forward never sends back to the member the header came from
// unless that member is the owner.
//
// Member state does not matter for routing: Joining, Draining, and weight-0
// members own whatever the table says until the leader moves it, and every
// member in a cell (Joining and Draining included) is a valid relay and
// backup.
//
// Handoff: when the owner is the final hop (KindSelf as the owner,
// KindOwner, KindDirect) and the partition moved less than HandoffEpochs
// epochs ago (uint32(current) - Entry.Moved < HandoffEpochs, wrapping like
// the 32-bit Moved field; a Moved up to HandoffEpochs ahead of a lagging
// current epoch also counts), PrevOwner names the previous owner if it is still
// a member, so the new owner can read through from it on a miss.
//
// A member that is not in its own view (joining, or removed) routes through
// a gateway: KindGateway to the gateway with the highest
// hashing.Score(Mix64(KeyHashString(0, addr)), partition), Alternate = the
// runner-up. Without gateways it falls back to KindDirect to the owner when
// it has a configured view, and to KindNoRoute otherwise.
//
// # Headers
//
// The origin stamps Header{Hops: 0, TTL: D + 2 (D + 3 when sending through
// a gateway), Partition, Sender: self, Term, Seq, Epoch}. Forward rejects
// malformed headers and partitions outside this member's table
// (ErrBadHeader), records the sender's view version in Notes when it is
// ahead of this member's view, recomputes the decision from this member's
// own view, and returns ErrTTLExhausted (with the decision) when the next
// hop would make Hops reach TTL. Serving locally is never refused for TTL.
package route

import (
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/epoch"
	"github.com/Azure/unbounded/internal/cachemesh/internal/hashing"
	"github.com/Azure/unbounded/internal/cachemesh/internal/health"
	"github.com/Azure/unbounded/internal/cachemesh/internal/placement"
	"github.com/Azure/unbounded/internal/cachemesh/internal/topology"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
)

// Kind classifies a Decision.
type Kind uint8

// Kinds.
const (
	// KindNoRoute means there is no view to route with and no gateway.
	KindNoRoute Kind = iota
	// KindSelf means serve locally: this member owns the partition, or
	// (Owner != self, FlagBackup set) it stands in for an unusable owner.
	KindSelf
	// KindRelay sends to an intermediate member closer to the owner.
	KindRelay
	// KindOwner sends to the owner, a neighbor.
	KindOwner
	// KindBackup sends to a cell-mate of an unusable owner.
	KindBackup
	// KindGateway sends to a gateway because this member is not in the view.
	KindGateway
	// KindDirect sends to the owner outside the neighbor budget because no
	// relay cell has members.
	KindDirect
)

var kindNames = [...]string{"NoRoute", "Self", "Relay", "Owner", "Backup", "Gateway", "Direct"}

// String returns the kind name.
func (k Kind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}

	return "Kind(" + strconv.Itoa(int(k)) + ")"
}

// PeerRef identifies a peer. ID and Addr share storage with the view member
// record. The zero value means no peer.
type PeerRef struct {
	// Slot is the member slot, or view.NoSlot for a gateway that is not
	// identified by slot.
	Slot view.Slot
	// Key is the member's rendezvous key (0 for gateways).
	Key uint64
	// ID is the member ID.
	ID string
	// Addr is the member's address.
	Addr string
}

// IsZero reports whether p names no peer.
func (p PeerRef) IsZero() bool { return p.ID == "" }

// Decision is the result of routing one request.
type Decision struct {
	// Kind classifies the decision.
	Kind Kind
	// Next is the peer to send to; zero for KindSelf and KindNoRoute.
	Next PeerRef
	// Owner is the owner of the partition in this member's view; zero for
	// KindNoRoute and KindGateway.
	Owner PeerRef
	// Alternate is a hedge or retry target; may be zero.
	Alternate PeerRef
	// PrevOwner is set during the handoff window when the owner is the
	// final hop.
	PrevOwner PeerRef
	// HedgeAfter is how long to wait before hedging to Alternate; zero
	// when Next is zero.
	HedgeAfter time.Duration
	// Header is the header to send with the request.
	Header Header
}

// Config configures an Env.
type Config struct {
	// Health tracks neighbor health. Nil means a private default tracker
	// with nothing tracked, so every peer is Healthy.
	Health *health.Tracker
	// Epoch is the member's epoch tracker. Nil means a private tracker
	// fixed at epoch 0.
	Epoch *epoch.Tracker
	// Notes receives versions seen in headers. Nil means a private Notes.
	Notes *Notes
	// Gateways are used while this member is not in its view. Entries
	// with an empty ID are ignored.
	Gateways []PeerRef
	// SelfID, when set, must match the member record at the self slot
	// for this member to count as a member, guarding against a reused
	// slot. SelfIncarnation, when nonzero, must match as well.
	SelfID          string
	SelfIncarnation uint64
	// PartitionBits is used to compute partitions while there is no
	// configured view. Default placement.DefaultBits.
	PartitionBits uint8
}

type gateway struct {
	ref PeerRef
	key uint64
}

// Env holds the long-lived routing dependencies shared by every State of
// one member. Build it once with NewEnv.
type Env struct {
	health  *health.Tracker
	epoch   *epoch.Tracker
	notes   *Notes
	gws     []gateway
	selfID  string
	selfInc uint64
	bits    uint8
}

// NewEnv returns an Env for cfg. It copies the gateway list.
func NewEnv(cfg Config) *Env {
	e := &Env{
		health:  cfg.Health,
		epoch:   cfg.Epoch,
		notes:   cfg.Notes,
		selfID:  cfg.SelfID,
		selfInc: cfg.SelfIncarnation,
		bits:    cfg.PartitionBits,
	}

	if e.health == nil {
		e.health = health.NewTracker(health.Config{}, rand.New(rand.NewPCG(0, 0)))
	}

	if e.epoch == nil {
		e.epoch = &epoch.Tracker{}
	}

	if e.notes == nil {
		e.notes = &Notes{}
	}

	if !placement.ValidBits(e.bits) {
		e.bits = placement.DefaultBits
	}

	for _, g := range cfg.Gateways {
		if g.ID != "" {
			e.gws = append(e.gws, gateway{ref: g, key: hashing.Mix64(hashing.KeyHashString(0, g.Addr))})
		}
	}

	return e
}

// Health returns the health tracker.
func (e *Env) Health() *health.Tracker { return e.health }

// Epoch returns the epoch tracker.
func (e *Env) Epoch() *epoch.Tracker { return e.epoch }

// Notes returns the notes.
func (e *Env) Notes() *Notes { return e.notes }

// State is an immutable routing snapshot. See the package documentation.
type State struct {
	v        *view.View
	env      *Env
	h        *health.Tracker
	e        *epoch.Tracker
	shape    *topology.Shape
	table    *placement.Table
	self     view.Slot
	selfCell uint32
	// selfCoords are the coordinates of selfCell, so that decide divides
	// only the owner's cell.
	selfCoords [topology.MaxDims]uint32
	member     bool
	bits       uint8
	ttl        uint8
	handoff    uint32
	term       uint32
	seq        uint64
}

// NewState returns the routing state of the member at slot self in v. A nil
// v means the genesis view. It is O(1).
func NewState(v *view.View, self view.Slot, env *Env) *State {
	if v == nil {
		v = view.Genesis()
	}

	if env == nil {
		env = NewEnv(Config{})
	}

	cfg, ok := v.Config()
	s := &State{
		v:        v,
		env:      env,
		h:        env.health,
		e:        env.epoch,
		shape:    v.Shape(),
		self:     self,
		selfCell: topology.NoCell,
		bits:     env.bits,
		term:     uint32(v.Term()),
		seq:      v.Version().Seq,
	}

	if ok && v.Table() != nil {
		s.table = v.Table()
		s.bits = cfg.PartitionBits
		s.handoff = min(cfg.HandoffEpochs, 1<<30) // keep 2*handoff in range
	}

	if m, ok := v.Member(self); ok && s.table != nil &&
		(env.selfID == "" || (m.ID == env.selfID && (env.selfInc == 0 || m.Incarnation == env.selfInc))) {
		s.member = true
		s.selfCell = m.Cell
		s.shape.Coords(&s.selfCoords, m.Cell)
	}

	switch {
	case s.member:
		s.ttl = s.shape.Dims + 2
	case s.table != nil:
		s.ttl = s.shape.Dims + 3
	default:
		s.ttl = topology.MaxDims + 3
	}

	if !s.member {
		s.self = view.NoSlot
	}

	return s
}

// View returns the view.
func (s *State) View() *view.View { return s.v }

// Self returns this member's slot, or view.NoSlot when it is not a member.
func (s *State) Self() view.Slot { return s.self }

// Member reports whether this member is in the view.
func (s *State) Member() bool { return s.member }

// Env returns the shared environment.
func (s *State) Env() *Env { return s.env }

// PartitionBits returns the partition bits used for routing.
func (s *State) PartitionBits() uint8 { return s.bits }

// Partition returns the partition of key in keyspace seed.
func (s *State) Partition(seed uint64, key []byte) uint32 {
	return hashing.Partition(hashing.KeyHash(seed, key), s.bits)
}

// Peer returns the PeerRef of the member at slot.
func (s *State) Peer(slot view.Slot) (PeerRef, bool) {
	m, ok := s.v.Member(slot)
	if !ok {
		return PeerRef{}, false
	}

	return PeerRef{Slot: slot, Key: m.Key, ID: m.ID, Addr: m.Addr}, true
}

// setRef fills *dst with the member at slot, or leaves it unchanged if
// slot is not a member. dst is filled field by field rather than by
// assigning a PeerRef value: a composite value is built on the stack with
// 8-byte stores and then copied with 16-byte loads, which defeats
// store-to-load forwarding and stalls the hot path.
func (s *State) setRef(dst *PeerRef, slot view.Slot) {
	m, ok := s.v.Member(slot)
	if !ok {
		return
	}

	fillRef(dst, slot, m)
}

func fillRef(dst *PeerRef, slot view.Slot, m *view.Member) {
	dst.Slot = slot
	dst.Key = m.Key
	dst.ID = m.ID
	dst.Addr = m.Addr
}

// Stamp returns the expiry stamp for a value of keyspace seed with ttl,
// issued now.
func (s *State) Stamp(seed uint64, ttl time.Duration) epoch.Stamp {
	period := s.v.Period()
	if period <= 0 {
		period = s.e.Period()
	}

	return epoch.StampFor(s.e.Current(), ttl, period, s.v.Gen(seed))
}

// Fresh reports whether a value of keyspace seed with stamp st is fresh.
func (s *State) Fresh(seed uint64, st epoch.Stamp) bool {
	return epoch.Fresh(st, s.e.Current(), s.v.Gen(seed))
}

// TryHedge admits a hedge of the request carrying h within the hedge
// budget.
func (s *State) TryHedge(h Header) bool {
	return s.h.TryHedge(uint64(h.Partition))
}

// Route routes key in keyspace seed from this member.
func (s *State) Route(seed uint64, key []byte) (d Decision) {
	s.decide(&d, hashing.Partition(hashing.KeyHash(seed, key), s.bits), view.NoSlot, 0)

	return d
}

// RouteHash routes a key whose hashing.KeyHash is h.
func (s *State) RouteHash(h uint64) (d Decision) {
	s.decide(&d, hashing.Partition(h, s.bits), view.NoSlot, 0)

	return d
}

// RoutePartition routes partition p. A p outside the table gives
// KindNoRoute.
func (s *State) RoutePartition(p uint32) Decision {
	var d Decision

	if s.bits < 32 && p>>s.bits != 0 {
		return d
	}

	s.decide(&d, p, view.NoSlot, 0)

	return d
}

// RouteMany routes keys into out[:len(keys)]. out must be at least as long
// as keys.
func (s *State) RouteMany(seed uint64, keys [][]byte, out []Decision) {
	out = out[:len(keys)]
	for i, k := range keys {
		out[i] = Decision{}
		s.decide(&out[i], hashing.Partition(hashing.KeyHash(seed, k), s.bits), view.NoSlot, 0)
	}
}

// Forward routes a request received with header h. On ErrTTLExhausted the
// decision is still returned for diagnostics but must not be sent.
func (s *State) Forward(h Header) (d Decision, err error) {
	// Fast validity test; check builds the descriptive error.
	if h.Format != HeaderFormat || h.Flags&^flagsKnown != 0 || h.Hops > h.TTL {
		return d, h.check()
	}

	if s.bits < 32 && h.Partition>>s.bits != 0 {
		return d, ErrBadHeader
	}

	// Inline the common case (sender not ahead) before calling note.
	if h.Term > s.term || (h.Term == s.term && h.Seq > s.seq) {
		s.env.notes.note(&h, s.term, s.seq)
	}

	avoid := h.Sender
	if avoid == s.self {
		avoid = view.NoSlot
	}

	s.decide(&d, h.Partition, avoid, h.Flags)

	d.Header.TTL = h.TTL
	d.Header.Hops = h.Hops + 1

	if !d.Next.IsZero() && int(h.Hops)+1 >= int(h.TTL) {
		return d, ErrTTLExhausted
	}

	return d, nil
}

// cand is a ranked candidate. class is a health.Usability; the zero value
// (class Unusable) is an empty candidate that every usable one beats.
type cand struct {
	score uint64
	key   uint64
	slot  view.Slot
	class health.Usability
}

func (c *cand) ok() bool { return c.class != health.Unusable }

func (a *cand) better(b *cand) bool {
	if a.class != b.class {
		return a.class > b.class
	}

	return hashing.Better(a.score, a.key, b.score, b.key)
}

// top3 keeps the three best candidates.
type top3 [3]cand

func (t *top3) add(c cand) {
	switch {
	case c.better(&t[0]):
		t[2], t[1], t[0] = t[1], t[0], c
	case c.better(&t[1]):
		t[2], t[1] = t[1], c
	case c.better(&t[2]):
		t[2] = c
	}
}

func (s *State) usable(slot view.Slot, ignore bool) health.Usability {
	if ignore || slot == s.self {
		return health.Healthy
	}

	return s.h.Usable(slot)
}

// best2 ranks the usable members of cell, skipping avoid.
func (s *State) best2(cell uint32, pm uint64, avoid view.Slot, ignore bool) (cand, cand) {
	var a, b cand

	for _, m := range s.v.CellMembers(cell) {
		if m == avoid {
			continue
		}

		u := s.usable(m, ignore)
		if u == health.Unusable {
			continue
		}

		k := s.v.Key(m)
		c := cand{slot: m, score: hashing.Mix64(k ^ pm), key: k, class: u}

		switch {
		case c.better(&a):
			a, b = c, a
		case c.better(&b):
			b = c
		}
	}

	return a, b
}

// decide fills *d, which must be the zero Decision, with the route of
// partition p. Fields are stored one by one; see setRef.
func (s *State) decide(d *Decision, p uint32, avoid view.Slot, inFlags uint8) {
	d.Header.Format = HeaderFormat
	d.Header.Flags = inFlags & FlagHedge
	d.Header.TTL = s.ttl
	d.Header.Partition = p
	d.Header.Sender = s.self
	d.Header.Term = s.term
	d.Header.Seq = s.seq
	d.Header.Epoch = s.e.Current()

	if !s.member {
		s.gateway(d, p)
		return
	}

	// Only the owner column is read here; prev reads the rest of the
	// entry for the decisions that need it.
	owner := s.table.Owner(p)

	om, ok := s.v.Member(owner)
	if !ok {
		return
	}

	fillRef(&d.Owner, owner, om)

	s.h.NoteRouted(uint64(p))

	if owner == s.self {
		d.Kind = KindSelf
		s.prev(d, p)

		return
	}

	var oc [topology.MaxDims]uint32

	mask := s.diffMask(&oc, om.Cell)
	if mask&(mask-1) == 0 {
		s.final(d, p, owner, om.Cell, avoid, inFlags&FlagBackup != 0)
	} else {
		s.relay(d, p, owner, &oc, mask, avoid)
	}

	if !d.Next.IsZero() {
		d.HedgeAfter = s.h.HedgeAfter(d.Next.Slot)
	}
}

// diffMask writes the coordinates of cell into oc and returns the mask of
// dimensions where they differ from selfCoords, as Shape.DiffMask.
func (s *State) diffMask(oc *[topology.MaxDims]uint32, cell uint32) uint8 {
	var m uint8

	for i, x := range s.shape.Coords(oc, cell) {
		if x != s.selfCoords[i] {
			m |= 1 << i
		}
	}

	return m
}

// prev sets d.PrevOwner if partition p is in its handoff window.
func (s *State) prev(d *Decision, p uint32) {
	ent := s.table.Get(p)

	// Window: the 32-bit epoch distance since the move, taken modulo
	// 2^32, is in [-handoff, handoff). The negative half covers a member
	// whose epoch tracker lags the leader's that stamped Moved (it applied
	// the move before re-anchoring); without it such a member would drop
	// PrevOwner for the freshest moves.
	if ent.Prev == ent.Owner || ent.Prev == placement.NoMember ||
		uint32(s.e.Current())-ent.Moved+s.handoff >= 2*s.handoff {
		return
	}

	s.setRef(&d.PrevOwner, ent.Prev)
}

// final handles an owner that is a neighbor.
func (s *State) final(d *Decision, p uint32, owner view.Slot, oc uint32, avoid view.Slot, standin bool) {
	all := s.h.AllHealthy()

	ou := health.Healthy
	if !all {
		ou = s.h.Usable(owner)
	}

	pm := hashing.Mix64(uint64(p))

	var t top3

	for _, m := range s.v.CellMembers(oc) {
		if m == owner || m == avoid {
			continue
		}

		u := s.usable(m, all)
		if u == health.Unusable {
			continue
		}

		k := s.v.Key(m)
		t.add(cand{slot: m, score: hashing.Mix64(k ^ pm), key: k, class: u})
	}

	// Backups other than self, best first.
	var bk [2]view.Slot

	nb := 0

	for i := range t {
		if t[i].ok() && t[i].slot != s.self && nb < len(bk) {
			bk[nb] = t[i].slot
			nb++
		}
	}

	toOwner := ou == health.Healthy || !t[0].ok() || (ou == health.Degraded && t[0].class < health.Healthy)
	if standin && s.selfCell == oc && ou != health.Unusable {
		toOwner = true
	}

	switch {
	case toOwner:
		d.Kind = KindOwner
		s.setRef(&d.Next, owner)

		if nb > 0 {
			s.setRef(&d.Alternate, bk[0])
		}

		s.prev(d, p)
	case t[0].slot == s.self || (standin && s.selfCell == oc):
		d.Kind = KindSelf
		d.Header.Flags |= FlagBackup
	default:
		d.Kind = KindBackup
		d.Header.Flags |= FlagBackup
		s.setRef(&d.Next, bk[0])

		switch {
		case nb > 1:
			s.setRef(&d.Alternate, bk[1])
		case ou != health.Unusable:
			s.setRef(&d.Alternate, owner)
		}
	}
}

// relay handles an owner more than one dimension away.
func (s *State) relay(d *Decision, p uint32, owner view.Slot, oc *[topology.MaxDims]uint32, mask uint8, avoid view.Slot) {
	pm := hashing.Mix64(uint64(p))

	for pass := range 2 {
		// AllHealthy covers Panic: every peer is Healthy either way.
		ignore := pass == 1 || s.h.AllHealthy()

		var first, alt cand

		for m := mask; m != 0; m &= m - 1 {
			dim := topology.FirstDiff(m)
			c := s.selfCell + (oc[dim]-s.selfCoords[dim])*s.shape.Stride(dim)
			a, b := s.best2(c, pm, avoid, ignore)

			if !first.ok() {
				first, alt = a, b
			} else if !alt.ok() {
				alt = a
			}

			if first.ok() && alt.ok() {
				break
			}
		}

		if first.ok() {
			d.Kind = KindRelay
			s.setRef(&d.Next, first.slot)

			if alt.ok() {
				s.setRef(&d.Alternate, alt.slot)
			}

			return
		}

		if ignore {
			break
		}
	}

	d.Kind = KindDirect
	d.Header.Flags |= FlagDirect
	s.setRef(&d.Next, owner)
	s.prev(d, p)
}

// gateway routes from a member that is not in its view.
func (s *State) gateway(d *Decision, p uint32) {
	d.Header.Sender = view.NoSlot

	if len(s.env.gws) == 0 {
		if s.table == nil {
			return
		}

		owner := s.table.Owner(p)

		om, ok := s.v.Member(owner)
		if !ok {
			return
		}

		d.Kind = KindDirect
		d.Header.Flags |= FlagDirect
		fillRef(&d.Owner, owner, om)
		fillRef(&d.Next, owner, om)
		s.prev(d, p)
		d.HedgeAfter = s.h.HedgeAfter(d.Next.Slot)

		return
	}

	b0, b1 := -1, -1

	var s0, s1 uint64

	for i := range s.env.gws {
		g := &s.env.gws[i]
		sc := hashing.Score(g.key, uint64(p))

		switch {
		case b0 < 0 || hashing.Better(sc, g.key, s0, s.env.gws[b0].key):
			b1, s1 = b0, s0
			b0, s0 = i, sc
		case b1 < 0 || hashing.Better(sc, g.key, s1, s.env.gws[b1].key):
			b1, s1 = i, sc
		}
	}

	d.Kind = KindGateway
	d.Header.Flags |= FlagGateway
	d.Next = s.env.gws[b0].ref

	if b1 >= 0 {
		d.Alternate = s.env.gws[b1].ref
	}

	d.HedgeAfter = s.h.HedgeAfter(view.NoSlot)
}
