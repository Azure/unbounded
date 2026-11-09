// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package control

import (
	"bytes"
	"errors"
	"slices"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/route"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
)

// --- Shared caches (simulation) ---

// Shared deduplicates immutable data across many Nodes in one process. It
// never changes what a Node computes, only how much memory and time it
// takes. It is not safe for concurrent use.
type Shared struct {
	// Interner shares views and decoded deltas.
	Interner *view.Interner

	enc  view.FIFO[view.Digest, []byte]
	snap view.FIFO[view.Digest, []byte]
	cand view.FIFO[candKey, []uint64]
}

// sharedSnapshots bounds the encoded snapshots a Shared cache holds.
const sharedSnapshots = 64

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
		enc:      view.NewFIFO[view.Digest, []byte](size),
		snap:     view.NewFIFO[view.Digest, []byte](sharedSnapshots),
		cand:     view.NewFIFO[candKey, []uint64](size),
	}
}

// encoded returns a canonical copy of the encoded delta.
func (n *Node) encoded(d *view.Delta, raw []byte) []byte {
	if s := n.shared; s != nil {
		if b, ok := s.enc.Get(d.Digest); ok {
			return b
		}

		b := bytes.Clone(raw)
		s.enc.Put(d.Digest, b)

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
		if c, ok := s.snap.Get(d); ok {
			b = c
		} else {
			b = view.EncodeSnapshot(n.v)
			s.snap.Put(d, b)
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

func (n *Node) onBroadcast(now int64, from string, m *Message) {
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
	if l == LevelLocal {
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
			n.sendBroadcast(enc, seq, LevelLocal, mine, s, 0)
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
		sc := view.Score(k, seq)
		usable := n.hl.Usable(s) != route.Unusable

		if best == view.NoSlot || (usable && !bestUsable) ||
			(usable == bestUsable && view.Better(sc, k, bestScore, v.Key(best))) {
			best, bestScore, bestUsable = s, sc, usable
		}
	}

	if best != view.NoSlot {
		n.sendBroadcast(enc, seq, l, cell, best, attempt)
	}
}

func (n *Node) sendBroadcast(enc []byte, seq uint64, l int8, cell uint32, to view.Slot, attempt uint8) {
	var m Message
	n.header(&m, KindBroadcast)
	m.Level = l
	m.Delta = enc
	n.send(n.addrOf(to), &m, &pending{kind: callBroadcast, slot: to, level: l, cell: cell, attempt: attempt, enc: enc, seq: seq}, n.cfg.CallTimeout)
}

func (n *Node) serveCatchUp(m *Message) []byte {
	var r Message
	n.header(&r, KindCatchUpResponse)

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

	return Append(nil, &r)
}

func (n *Node) onCatchUp(now int64, m *Message, from string) {
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

func (n *Node) respAddr(m *Message) string {
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
	ps := make([]route.TrackRef, len(n.neighbors))
	for i, nb := range n.neighbors {
		ps[i] = route.TrackRef{Slot: nb.slot, Key: nb.key, Cell: nb.cell}
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

		var m Message
		n.header(&m, KindPing)

		sb.inflight = n.send(n.addrOf(sb.slot), &m, &pending{kind: callPing, slot: sb.slot, key: sb.key}, n.cfg.PingTimeout)
	}
}

func (n *Node) pingDone(now int64, p *pending, ok bool, rtt time.Duration) {
	i := slices.IndexFunc(n.subjects, func(s subject) bool { return s.slot == p.slot && s.key == p.key })

	if n.v.Key(p.slot) == p.key {
		if ok {
			n.hl.Observe(p.slot, route.Success, rtt)
		} else {
			n.hl.Observe(p.slot, route.Timeout, 0)
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

	var s Message
	n.header(&s, KindSuspect)
	s.Target = MemberRef{ID: m.ID, Incarnation: m.Incarnation, Slot: sb.slot}
	s.Observer = n.self
	n.toLeader(now, addr, &s)
	n.event("suspect-sent")
}
