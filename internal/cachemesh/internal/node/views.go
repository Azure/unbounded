// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"bytes"
	"errors"
	"slices"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/elect"
	"github.com/Azure/unbounded/internal/cachemesh/internal/hashing"
	"github.com/Azure/unbounded/internal/cachemesh/internal/health"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
	"github.com/Azure/unbounded/internal/cachemesh/internal/wire"
)

// --- Shared caches (simulation) ---

// Shared deduplicates immutable data across many Nodes in one process. It
// never changes what a Node computes, only how much memory and time it
// takes. It is not safe for concurrent use.
type Shared struct {
	// Interner shares views and decoded deltas.
	Interner *view.Interner

	enc   map[view.Digest][]byte
	encQ  []view.Digest
	snap  map[view.Digest][]byte
	snapQ []view.Digest
	cand  map[candKey][]uint64
	candQ []candKey
	size  int
}

type candKey struct {
	digest view.Digest
	epoch  uint64
}

// NewShared returns a Shared cache holding about size entries of each kind.
func NewShared(size int) *Shared {
	if size <= 0 {
		size = view.DefaultInternerSize
	}

	return &Shared{
		Interner: view.NewInterner(size),
		enc:      map[view.Digest][]byte{},
		snap:     map[view.Digest][]byte{},
		cand:     map[candKey][]uint64{},
		size:     size,
	}
}

func fifoPut[K comparable, V any](m map[K]V, q *[]K, limit int, k K, v V) {
	if _, ok := m[k]; !ok {
		if len(*q) >= limit {
			delete(m, (*q)[0])
			*q = (*q)[1:]
		}

		*q = append(*q, k)
	}

	m[k] = v
}

// encoded returns a canonical copy of the encoded delta.
func (n *Node) encoded(d *view.Delta, raw []byte) []byte {
	if s := n.shared; s != nil {
		if b, ok := s.enc[d.Digest]; ok {
			return b
		}

		b := bytes.Clone(raw)
		fifoPut(s.enc, &s.encQ, s.size, d.Digest, b)

		return b
	}

	return bytes.Clone(raw)
}

func (n *Node) decodeDelta(raw []byte) (*view.Delta, error) {
	if n.shared != nil {
		return n.shared.Interner.Delta(raw)
	}

	return view.DecodeDelta(raw)
}

func (n *Node) applyDelta(d *view.Delta) (*view.View, error) {
	if n.shared != nil {
		return n.shared.Interner.Apply(n.v, d)
	}

	return view.Apply(n.v, d)
}

func (n *Node) snapshot() []byte {
	d := n.v.Digest()
	if n.snapBytes != nil && n.snapDigest == d {
		return n.snapBytes
	}

	var b []byte

	if s := n.shared; s != nil {
		if c, ok := s.snap[d]; ok {
			b = c
		} else {
			b = view.EncodeSnapshot(n.v)
			fifoPut(s.snap, &s.snapQ, 64, d, b)
		}
	} else {
		b = view.EncodeSnapshot(n.v)
	}

	n.snapDigest, n.snapBytes = d, b

	return b
}

// --- Views ---

// setView installs nv (already validated by Apply or DecodeSnapshot) and
// updates everything derived from it.
func (n *Node) setView(now int64, nv *view.View) {
	old := n.v
	n.v = nv
	n.snapBytes = nil

	n.slot = view.NoSlot
	if s, ok := nv.Lookup(n.cfg.ID); ok && nv.Key(s) == n.key {
		n.slot = s
	}

	n.self.Slot = n.slot

	switch {
	case n.slot != view.NoSlot:
		n.joined = true
		n.seeds = n.seeds[:0] // no longer needed
		n.coldStart, n.bootstrapper = false, false
	case n.joined && n.leaving:
		n.left = true
	default:
		// Not a member: join (or rejoin after a removal, for example after
		// a partition) through the leader and members of the newest view,
		// since configured seeds may have gone away.
		if n.joined && len(n.seeds) == 0 {
			n.event("rejoin")
			n.nextJoin = n.now
		}

		n.seedsFromView()
	}

	if nv.Term() > 0 && (old.Term() < nv.Term() || old.Version() != nv.Version()) {
		n.heardAt = now
	}

	if n.leading && nv.Term() > n.term {
		// Another leader published a later term: stop at once, and give
		// the Lease back in case this member still holds it (a lost or
		// corrupt term annotation can let the lower lineage win it).
		n.resign(now, "fenced")
	}

	n.updateNeighbors(now)
	n.updateSubjects()

	if n.cfg.Hooks.OnView != nil {
		n.cfg.Hooks.OnView(nv)
	}
}

// applyOne applies d (encoded as raw) on top of the current view.
func (n *Node) applyOne(now int64, d *view.Delta, raw []byte) error {
	nv, err := n.applyDelta(d)
	if err != nil {
		return err
	}

	// A delta chained on top of the view confirms its digest.
	n.unconfirmed, n.badBase = false, false

	if err := n.log.Append(d, n.encoded(d, raw)); err != nil {
		// Unreachable: the log head tracks the view.
		n.log.Reset(nv)
	}

	for i := range d.Ops {
		if d.Ops[i].Kind == view.KindEpoch {
			n.ep.Anchor(d.Ops[i].Epoch, d.Ops[i].Period, now)
		}
	}

	n.setView(now, nv)

	return nil
}

func (n *Node) adoptSnapshot(now int64, nv *view.View) {
	n.adoptSnapshotFrom(now, nv, "")
}

// adoptSnapshotFrom installs a snapshot served by the peer at from ("" when
// the source is trusted: the leader of the snapshot, or the local config).
func (n *Node) adoptSnapshotFrom(now int64, nv *view.View, from string) {
	n.snapFrom = from
	n.unconfirmed, n.unconfirmedAt, n.badBase = from != "", now, false

	if n.shared != nil {
		nv = n.shared.Interner.View(nv)
	}

	n.log.Reset(nv)

	if nv.Period() > 0 {
		n.ep.Anchor(nv.Epoch(), nv.Period(), now)
	}

	n.ahead = n.ahead[:0]
	n.setView(now, nv)
}

func (n *Node) onBroadcast(now int64, from string, m *wire.Message) {
	n.addSeed(from)

	d, err := n.decodeDelta(m.Delta)
	if err != nil {
		n.event("bad-delta")

		return
	}

	n.observeTerm(now, d.Version.Term, from)

	cur := n.v.Version()

	switch {
	case d.Version.Term < cur.Term:
		n.event("fenced-delta")

		return
	case d.Base == cur:
		enc := n.encoded(d, m.Delta)
		if err := n.applyOne(now, d, enc); err != nil {
			if errors.Is(err, view.ErrDigest) {
				n.event("fork")
				n.recoverBase(now, from)
			}

			return
		}

		n.forward(d, enc, m.Level)
		n.drainAhead(now)
	case !d.Version.Less(cur) && d.Version != cur:
		// Ahead (or a new term on another base): queue and catch up.
		if len(n.ahead) < n.cfg.MaxAhead && !slices.ContainsFunc(n.ahead, func(a aheadDelta) bool { return a.d.Digest == d.Digest }) {
			n.ahead = append(n.ahead, aheadDelta{d: d, enc: n.encoded(d, m.Delta), level: m.Level})
			n.event("ahead-queued")
		}

		n.catchUp(now, from, false)
	default:
		// Already have it. Forward once if we got it another way first.
		if d.Version.Term == cur.Term && n.forwarded.Less(d.Version) && d.Version == cur && d.Digest == n.v.Digest() {
			n.forward(d, n.encoded(d, m.Delta), m.Level)
		}
	}
}

// drainAhead applies queued deltas that now fit, and drops stale ones.
func (n *Node) drainAhead(now int64) {
	for progress := true; progress; {
		progress = false
		cur := n.v.Version()

		n.ahead = slices.DeleteFunc(n.ahead, func(a aheadDelta) bool {
			return !cur.Less(a.d.Version) && a.d.Base != cur
		})

		for i, a := range n.ahead {
			if a.d.Base != cur {
				continue
			}

			n.ahead = slices.Delete(n.ahead, i, i+1)

			if err := n.applyOne(now, a.d, a.enc); err == nil {
				n.forward(a.d, a.enc, a.level)

				progress = true
			} else if errors.Is(err, view.ErrDigest) {
				n.recoverBase(now, "")
			}

			break
		}
	}
}

// forward relays delta d, encoded as enc, received at level l.
func (n *Node) forward(d *view.Delta, enc []byte, l int8) {
	if !n.isMember() {
		return
	}

	n.forwarded = d.Version
	n.fanOut(enc, d.Version.Seq, l)
}

// fanOut sends enc to one member of every adjacent cell in each dimension
// above l and, unless l is LevelLocal, to every cell-mate.
func (n *Node) fanOut(enc []byte, seq uint64, l int8) {
	if l == wire.LevelLocal {
		return
	}

	v := n.v
	sh := v.Shape()
	mine := v.Cell(n.slot)

	for j := int(l) + 1; j < int(sh.Dims); j++ {
		myc := sh.Coord(mine, j)

		for c := range sh.Size[j] {
			if c == myc {
				continue
			}

			n.forwardCell(enc, seq, int8(j), sh.Replace(mine, j, c), 0, view.NoSlot)
		}
	}

	for _, s := range v.CellMembers(mine) {
		if s != n.slot {
			n.sendBroadcast(enc, seq, wire.LevelLocal, mine, s, 0)
		}
	}
}

// forwardCell sends enc at level l to the best usable member of cell,
// skipping skip.
func (n *Node) forwardCell(enc []byte, seq uint64, l int8, cell uint32, attempt uint8, skip view.Slot) {
	v := n.v
	if cell >= v.Shape().Cells() {
		return
	}

	best, bestUsable := view.NoSlot, false

	var bestScore uint64

	for _, s := range v.CellMembers(cell) {
		if s == skip || s == n.slot {
			continue
		}

		k := v.Key(s)
		sc := hashing.Score(k, seq)
		usable := n.hl.Usable(s) != health.Unusable

		if best == view.NoSlot || (usable && !bestUsable) ||
			(usable == bestUsable && hashing.Better(sc, k, bestScore, v.Key(best))) {
			best, bestScore, bestUsable = s, sc, usable
		}
	}

	if best != view.NoSlot {
		n.sendBroadcast(enc, seq, l, cell, best, attempt)
	}
}

func (n *Node) sendBroadcast(enc []byte, seq uint64, l int8, cell uint32, to view.Slot, attempt uint8) {
	var m wire.Message
	n.header(&m, wire.KindBroadcast)
	m.Level = l
	m.Delta = enc
	n.send(n.addrOf(to), &m, &pending{kind: callBroadcast, slot: to, level: l, cell: cell, attempt: attempt, enc: enc, seq: seq}, n.cfg.CallTimeout)
}

func (n *Node) serveCatchUp(m *wire.Message) []byte {
	var r wire.Message
	n.header(&r, wire.KindCatchUpResponse)

	mine := n.v.Version()

	switch {
	case n.leading && m.Have.Term == n.term && mine.Less(m.Have):
		// Only this member publishes this term, so a requester ahead of it
		// holds a forged or corrupt view: send ours so it can repair.
		r.Snapshot = n.snapshot()
	case n.unconfirmed || n.badBase:
		// Do not spread a view that may be corrupt.
	case mine == m.Have && n.v.Digest() == m.HaveDigest, mine.Less(m.Have), mine.IsZero():
		// Nothing newer.
	default:
		limit := int(m.MaxDeltas)
		if limit <= 0 || limit > n.cfg.CatchUpMaxDeltas {
			limit = n.cfg.CatchUpMaxDeltas
		}

		if es, ok := n.log.Since(m.Have, m.HaveDigest, limit); ok && len(es) > 0 {
			for _, e := range es {
				r.Deltas = append(r.Deltas, e.Encoded)
			}
		} else if mine != m.Have || n.v.Digest() != m.HaveDigest {
			r.Snapshot = n.snapshot()
		}
	}

	return wire.Append(nil, &r)
}

func (n *Node) onCatchUp(now int64, m *wire.Message, from string) {
	switch {
	case len(m.Deltas) > 0:
		applied := 0

		for _, raw := range m.Deltas {
			d, err := n.decodeDelta(raw)
			if err != nil || d.Base != n.v.Version() {
				continue
			}

			if err := n.applyOne(now, d, raw); err != nil {
				if errors.Is(err, view.ErrDigest) {
					n.recoverBase(now, from)
				}

				break
			}

			applied++
		}

		if applied > 0 {
			if n.cfg.Hooks.OnCatchUp != nil {
				n.cfg.Hooks.OnCatchUp(CatchUpDeltas)
			}

			n.drainAhead(now)
		}

		if m.Version.Less(n.v.Version()) || m.Version == n.v.Version() {
			return
		}

		// Still behind the responder: continue.
		if applied > 0 {
			n.catchUp(now, n.respAddr(m), false)
		}
	case len(m.Snapshot) > 0:
		nv, err := view.DecodeSnapshot(m.Snapshot)
		if err != nil {
			n.event("bad-snapshot")

			return
		}

		cur := n.v.Version()
		fromLeader := nv.Leader() == m.From.Slot && nv.Version() == m.Version
		// A repair source is any peer other than the one that served the
		// current (suspect) view.
		repair := (n.badBase || n.unconfirmed) && from != n.snapFrom
		differs := nv.Digest() != n.v.Digest()

		switch {
		case cur.Less(nv.Version()), cur == nv.Version() && differs && (fromLeader || repair):
		case n.unconfirmed && fromLeader && from != n.snapFrom && differs && nv.Term() >= cur.Term:
			// The leader of the term is behind a snapshot nobody has
			// built on: that snapshot claimed a version that was never
			// published. Roll back to the leader's view.
			n.event("snapshot-rollback")
		default:
			return
		}

		src := from
		if fromLeader {
			src = ""
		}

		n.adoptSnapshotFrom(now, nv, src)

		if n.cfg.Hooks.OnCatchUp != nil {
			n.cfg.Hooks.OnCatchUp(CatchUpSnapshot)
		}

		n.drainAhead(now)
	default:
		if n.cfg.Hooks.OnCatchUp != nil {
			n.cfg.Hooks.OnCatchUp(CatchUpEmpty)
		}
	}
}

func (n *Node) respAddr(m *wire.Message) string {
	if m.From.Slot != view.NoSlot {
		if mm, ok := n.v.Member(m.From.Slot); ok && mm.ID == m.From.ID {
			return mm.Addr
		}
	}

	return ""
}

// --- Neighbors and monitoring ---

func (n *Node) updateNeighbors(now int64) {
	var cur []view.Slot
	if n.isMember() {
		cur = view.Neighbors(n.v, n.slot, n.buf[:0])
	}

	n.buf = cur[:0]

	var added []view.Slot

	changed := false
	next := make([]neighbor, 0, len(cur)+len(n.neighbors))
	i := 0

	for _, s := range cur {
		key, cell := n.v.Key(s), n.v.Cell(s)

		for i < len(n.neighbors) && n.neighbors[i].slot < s {
			nb := n.lingering(now, n.neighbors[i])
			changed = changed || nb != n.neighbors[i]
			next = append(next, nb)
			i++
		}

		if i < len(n.neighbors) && n.neighbors[i].slot == s {
			old := n.neighbors[i]
			i++

			if old.key == key {
				nb := neighbor{slot: s, key: key, cell: cell}
				changed = changed || nb != old
				next = append(next, nb)

				continue
			}
		}

		// New, or the slot was reused by another member (the old one is
		// gone at once).
		changed = true

		next = append(next, neighbor{slot: s, key: key, cell: cell})
		added = append(added, s)
	}

	for ; i < len(n.neighbors); i++ {
		nb := n.lingering(now, n.neighbors[i])
		changed = changed || nb != n.neighbors[i]
		next = append(next, nb)
	}

	n.neighbors = next

	if changed {
		n.syncHealth()
	}

	if len(added) > 0 && n.cfg.Hooks.OnNeighbors != nil {
		n.cfg.Hooks.OnNeighbors(added, nil)
	}
}

// syncHealth makes the health tracker track exactly the current and
// lingering neighbors.
func (n *Node) syncHealth() {
	ps := make([]health.Ref, len(n.neighbors))
	for i, nb := range n.neighbors {
		ps[i] = health.Ref{Slot: nb.slot, Key: nb.key, Cell: nb.cell}
	}

	n.hl.Sync(ps)
}

func (n *Node) lingering(now int64, nb neighbor) neighbor {
	if nb.lingers == 0 {
		nb.lingers = now + int64(n.cfg.NeighborLinger)
	}

	if n.lingerAt == 0 || nb.lingers < n.lingerAt {
		n.lingerAt = nb.lingers
	}

	return nb
}

func (n *Node) expireNeighbors(now int64) {
	if n.lingerAt == 0 || now < n.lingerAt {
		return
	}

	n.lingerAt = 0

	var removed []view.Slot

	n.neighbors = slices.DeleteFunc(n.neighbors, func(nb neighbor) bool {
		if nb.lingers != 0 && now >= nb.lingers {
			removed = append(removed, nb.slot)

			return true
		}

		if nb.lingers != 0 && (n.lingerAt == 0 || nb.lingers < n.lingerAt) {
			n.lingerAt = nb.lingers
		}

		return false
	})

	if len(removed) == 0 {
		return
	}

	n.syncHealth()

	if n.cfg.Hooks.OnNeighbors != nil {
		n.cfg.Hooks.OnNeighbors(nil, removed)
	}
}

func (n *Node) updateSubjects() {
	var cur []view.Slot
	if n.isMember() {
		cur = view.Subjects(n.v, n.slot, nil)
	}

	slices.Sort(cur)
	cur = slices.Compact(cur)

	next := make([]subject, 0, len(cur))

	for _, s := range cur {
		key := n.v.Key(s)
		if i := slices.IndexFunc(n.subjects, func(o subject) bool { return o.slot == s && o.key == key }); i >= 0 {
			next = append(next, n.subjects[i])

			continue
		}

		next = append(next, subject{slot: s, key: key})
	}

	n.subjects = next
}

func (n *Node) pingSubjects(_ int64) {
	for i := range n.subjects {
		sb := &n.subjects[i]
		if sb.inflight {
			continue
		}

		var m wire.Message
		n.header(&m, wire.KindPing)

		sb.inflight = n.send(n.addrOf(sb.slot), &m, &pending{kind: callPing, slot: sb.slot, key: sb.key}, n.cfg.PingTimeout)
	}
}

func (n *Node) pingDone(now int64, p *pending, ok bool, rtt time.Duration) {
	i := slices.IndexFunc(n.subjects, func(s subject) bool { return s.slot == p.slot && s.key == p.key })

	if n.hl != nil && n.v.Key(p.slot) == p.key {
		if ok {
			n.hl.Observe(p.slot, health.Success, rtt)
		} else {
			n.hl.Observe(p.slot, health.Timeout, 0)
		}
	}

	if i < 0 {
		return
	}

	sb := &n.subjects[i]
	sb.inflight = false

	if ok {
		sb.fails = 0

		return
	}

	sb.fails++
	if sb.fails < n.cfg.SuspectAfter || (sb.lastSuspect != 0 && now-sb.lastSuspect < int64(n.cfg.SuspectRefresh)) {
		return
	}

	addr, known := n.leaderTarget()
	if !known {
		return
	}

	sb.lastSuspect = now
	m, _ := n.v.Member(sb.slot)

	var s wire.Message
	n.header(&s, wire.KindSuspect)
	s.Target = wire.MemberRef{ID: m.ID, Incarnation: m.Incarnation, Slot: sb.slot}
	s.Observer = n.self
	n.toLeader(now, addr, &s)
	n.event("suspect-sent")
}

// --- Election and leadership ---

func (n *Node) electInput(now int64, wall time.Time) elect.Input {
	_, has := n.v.Config()
	in := elect.Input{
		Now: now, Wall: wall,
		ViewTerm:      n.v.Term(),
		HaveView:      has,
		ClusterSize:   n.v.Len(),
		IsCandidate:   n.isCandidate() || (n.bootstrapper && !has),
		LeaderHeardAt: n.heardAt,
		Seeds:         n.leaseSeeds,
	}

	// A joiner that has seeds is in contact with the cluster and needs no
	// Lease reads: keep it a follower. Otherwise a joiner without a view
	// would sit in the lottery and, with a skewed wall clock, take a live
	// leader's Lease only to give it back.
	if !n.isMember() && len(n.seeds) > 0 && !n.bootstrapper && !n.coldStart && !n.leading {
		in.HaveView, in.LeaderHeardAt = true, now
	}

	// Likewise while this member's own Lease read is outstanding: its
	// result decides whether to join, bootstrap, or cold start, and a
	// lottery read in the meantime would only duplicate it.
	if !n.isMember() && n.ownReadSeq != 0 && !n.ownReadDone && !n.bootstrapper && !n.coldStart && !n.leading {
		in.HaveView, in.LeaderHeardAt = true, now
	}

	return in
}

func (n *Node) isCandidate() bool {
	if !n.isMember() || n.leaving {
		return false
	}

	ep := n.ep.Current()
	if n.candKey == n.v.Digest() && n.candEpoch == ep {
		return n.candidate
	}

	n.candKey, n.candEpoch = n.v.Digest(), ep
	top := n.topCandidates(ep)
	n.candidate = slices.Contains(top, n.key)

	return n.candidate
}

func (n *Node) topCandidates(ep uint64) []uint64 {
	k := candKey{digest: n.v.Digest(), epoch: ep}
	if s := n.shared; s != nil {
		if t, ok := s.cand[k]; ok {
			return t
		}
	}

	per := n.v.Period()
	if per <= 0 {
		per = n.cfg.EpochPeriod
	}

	minAge := uint64(n.cfg.Elect.MinCandidateAge / per)

	var keys []uint64

	n.v.ForEachMember(func(_ view.Slot, m *view.Member) bool {
		if m.State == view.Active && m.Joined+minAge <= ep {
			keys = append(keys, m.Key)
		}

		return true
	})

	top := elect.TopCandidates(LeaseSeed, keys, n.cfg.Elect.Candidates)

	if s := n.shared; s != nil {
		fifoPut(s.cand, &s.candQ, s.size, k, top)
	}

	return top
}

func (n *Node) tickElector(now int64, wall time.Time) {
	if n.leading && now >= n.nextSeedsAt {
		n.nextSeedsAt = now + int64(DefaultSeedRefresh)
		n.pickSeeds()
	}

	n.leaseOps = append(n.leaseOps, n.el.Tick(n.electInput(now, wall))...)
	n.electEvents(now)

	// The Lease names a leader of a later term than our view (for example
	// after a partition healed into two lineages): catch up from it. This
	// costs no API calls; the elector already read the Lease.
	if li, ok := n.el.Leader(); ok && !n.leading && li.Term > n.v.Term() && li.Addr != "" && li.Addr != n.cfg.Addr {
		n.event("lease-newer-term")
		n.catchUp(now, li.Addr, false)
	}
}

// seedsFromView uses the leader and a few random members of the current
// view as join seeds.
func (n *Node) seedsFromView() {
	n.addSeed(n.addrOf(n.v.Leader()))

	if c := n.v.SlotCap(); c > 0 {
		for range 4 {
			n.addSeed(n.addrOf(view.Slot(n.rng.Uint32N(c))))
		}
	}
}

// maxSeeds bounds the join seed list.
const maxSeeds = 32

// pickSeeds chooses up to elect.MaxSeeds random member addresses for the
// Lease.
func (n *Node) pickSeeds() {
	n.leaseSeeds = n.leaseSeeds[:0]

	cap := n.v.SlotCap()
	if cap == 0 {
		return
	}

	for range 4 * elect.MaxSeeds {
		if len(n.leaseSeeds) == elect.MaxSeeds {
			break
		}

		s := view.Slot(n.rng.Uint32N(cap))
		if m, ok := n.v.Member(s); ok && m.State == view.Active && !slices.Contains(n.leaseSeeds, m.Addr) {
			n.leaseSeeds = append(n.leaseSeeds, m.Addr)
		}
	}
}

// OnLease reports the result of a Lease operation from LeaseOps.
func (n *Node) OnLease(now int64, wall time.Time, r elect.Result) {
	n.now = max(n.now, now)
	now = n.now

	if r.Seq&ownLeaseBit != 0 {
		if r.Seq != n.ownReadSeq || n.ownReadDone {
			return
		}

		n.ownReadDone = true
		defer n.noteColdRead(now, wall, r)

		// Any record that names a holder or seeds means a cluster exists
		// (or existed): join through it rather than judging liveness with
		// a possibly skewed wall clock. Only a missing Lease, or a released
		// one without seeds, makes this member bootstrap a cluster.
		if r.Err == nil && (r.Record.Holder != "" || len(r.Record.Seeds) > 0) {
			n.learnSeeds(r.Record.Addr, r.Record.Seeds)
		}

		switch {
		case len(n.seeds) > 0:
		case r.Err == nil || errors.Is(r.Err, elect.ErrNotFound):
			if _, has := n.v.Config(); !has && len(n.cfg.Seeds) == 0 && !n.joined {
				n.bootstrapper = true
				n.event("bootstrap-candidate")
			}
		}

		return
	}

	n.el.OnResult(n.electInput(now, wall), r)
	n.electEvents(now)
}

func (n *Node) learnSeeds(addr string, seeds []string) {
	if addr != "" && addr != n.cfg.Addr {
		n.leaderAddr = addr
	}

	if n.isMember() {
		return
	}

	for _, s := range append([]string{addr}, seeds...) {
		if s != "" && s != n.cfg.Addr && !slices.Contains(n.seeds, s) && len(n.seeds) < 4*elect.MaxSeeds {
			n.seeds = append(n.seeds, s)
		}
	}

	n.bootstrapper = false
}

func (n *Node) electEvents(now int64) {
	for _, e := range n.el.Events() {
		switch e.Kind {
		case elect.Acquired:
			n.startLeading(now, e.Term)
		case elect.Lost:
			n.stopLeading("lost")
		case elect.Discovered:
			// A live holder: join it rather than cold start.
			n.coldStart = false
			n.learnSeeds(e.Record.Addr, e.Record.Seeds)

			if n.isMember() && n.v.Term() < e.Record.Term {
				n.catchUp(now, e.Record.Addr, false)
			}
		}
	}
}

func (n *Node) startLeading(now int64, term uint64) {
	_, has := n.v.Config()
	if n.leaving {
		// The acquire was in flight when Leave was called: a leaving
		// member never starts a term. Give the Lease straight back.
		n.event("acquire-declined-leaving")
		n.leaseOps = append(n.leaseOps, n.el.Release(n.electInput(now, n.wall))...)

		return
	}

	if term <= n.v.Term() || (!has && !n.bootstrapper && !n.coldStart) {
		// Cannot start this term from our view (or must not bootstrap a
		// second cluster): give the Lease back.
		n.event("acquire-declined")
		n.leaseOps = append(n.leaseOps, n.el.Release(n.electInput(now, n.wall))...)

		return
	}

	n.leading, n.term, n.startTerm = true, term, true
	n.releasing = false
	n.ld.Reset()
	n.nextLeader = now
	n.nextSeedsAt = now
	n.event("term-start")
}

func (n *Node) stopLeading(reason string) {
	if !n.leading {
		return
	}

	n.leading, n.startTerm = false, false
	n.event("leader-stop-" + reason)
}

func (n *Node) leaderTick(now int64) {
	if !n.el.CanPublish(now) {
		n.stopLeading("deadline")

		return
	}

	var newTerm uint64

	if n.startTerm {
		newTerm = n.term
	} else if n.v.Term() != n.term {
		n.resign(now, "fenced")

		return
	}

	ops := n.ld.Tick(now, n.ep.Advance(now), n.v, newTerm)
	if ops == nil {
		return
	}

	d := view.NewDelta(n.v, ops...)
	raw := view.EncodeDelta(d)

	if err := n.applyOne(now, d, raw); err != nil {
		// A bug in the leader logic; never publish an invalid delta.
		n.event("leader-invalid-delta")

		return
	}

	n.startTerm = false

	if n.cfg.Hooks.OnPublish != nil {
		n.cfg.Hooks.OnPublish(d)
	}

	n.forward(d, n.encoded(d, raw), wire.LevelLeader)
}
