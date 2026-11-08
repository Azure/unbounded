// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package dst

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"

	"github.com/Azure/unbounded/internal/cachemesh/internal/elect"
)

// ErrUnavailable is the transient error returned during an API server
// outage.
var ErrUnavailable = errors.New("dst: lease server unavailable")

// NoMember attributes a Lease operation to no member.
const NoMember = ^uint32(0)

// LeaseConfig configures a LeaseServer. Zero values select defaults.
type LeaseConfig struct {
	// Latency is the base latency of an asynchronous operation. Default 5ms.
	Latency int64
	// Jitter is the maximum extra latency, drawn uniformly. Default
	// Latency.
	Jitter int64
	// CacheLag is how far behind the latest write GetCached may be. Each
	// cached read sees the state as of a uniformly random time in
	// [now-CacheLag, now]. Default 0 (cached reads are fresh).
	CacheLag int64
	// ConflictPPM is the probability, in parts per million, that a create or
	// update fails with an injected ErrConflict.
	ConflictPPM uint32
	// SeriesSeconds is how many recent per-second totals QPS keeps. Default
	// 600.
	SeriesSeconds int
}

// Window is a half-open simulated time interval [Start, End).
type Window struct{ Start, End int64 }

// Write is one successful Lease write, for invariant checks.
type Write struct {
	Time        int64
	Member      uint32
	Kind        elect.OpKind
	Holder      string
	Incarnation uint64
	Term        uint64
	RV          uint64
}

type leaseVersion struct {
	t      int64
	exists bool
	rec    elect.Record
}

// LeaseServer is an in-memory Lease with API server semantics: consistent
// and cached reads, create, compare-and-swap update, server-assigned
// resourceVersion and UID, latency, outages, injected conflicts, and QPS
// accounting.
type LeaseServer struct {
	sim     *Sim
	rng     *rand.Rand
	cfg     LeaseConfig
	hist    []leaseVersion // history[len-1] is current
	rv      uint64
	uids    uint64
	outages []Window
	audit   []Write
	qps     *QPS
}

// NewLeaseServer returns a server whose Lease does not exist yet.
func NewLeaseServer(sim *Sim, cfg LeaseConfig) *LeaseServer {
	if cfg.Latency <= 0 {
		cfg.Latency = 5_000_000
	}

	if cfg.Jitter <= 0 {
		cfg.Jitter = cfg.Latency
	}

	return &LeaseServer{
		sim:  sim,
		rng:  sim.Stream("lease"),
		cfg:  cfg,
		hist: []leaseVersion{{}},
		qps:  NewQPS(cfg.SeriesSeconds),
	}
}

// SetConflictPPM changes the injected conflict probability.
func (l *LeaseServer) SetConflictPPM(ppm uint32) { l.cfg.ConflictPPM = ppm }

// SetCacheLag changes the cached-read lag.
func (l *LeaseServer) SetCacheLag(d int64) { l.cfg.CacheLag = d }

// AddOutage makes every operation in [start, end) fail with ErrUnavailable.
func (l *LeaseServer) AddOutage(start, end int64) {
	l.outages = append(l.outages, Window{start, end})
}

func (l *LeaseServer) down(now int64) bool {
	for _, w := range l.outages {
		if now >= w.Start && now < w.End {
			return true
		}
	}

	return false
}

// Audit returns the successful writes in order.
func (l *LeaseServer) Audit() []Write { return l.audit }

// QPS returns the operation accounting.
func (l *LeaseServer) QPS() *QPS { return l.qps }

// Current returns the stored Lease and whether it exists.
func (l *LeaseServer) Current() (elect.Record, bool) {
	c := l.hist[len(l.hist)-1]

	return c.rec.Clone(), c.exists
}

// Do executes op at simulated time now, attributed to no member.
func (l *LeaseServer) Do(op elect.Op, now int64) elect.Result {
	return l.DoAs(NoMember, op, now)
}

// DoAs executes op at simulated time now on behalf of member.
func (l *LeaseServer) DoAs(member uint32, op elect.Op, now int64) elect.Result {
	l.qps.Record(now, op.Kind, member)

	res := elect.Result{Seq: op.Seq, Kind: op.Kind}

	if l.down(now) {
		res.Err = ErrUnavailable

		return res
	}

	cur := l.hist[len(l.hist)-1]

	switch op.Kind {
	case elect.OpGet:
		res.Record, res.Err = read(cur)
	case elect.OpGetCached:
		res.Record, res.Err = read(l.cached(now))
	case elect.OpCreate:
		if cur.exists || l.injectConflict() {
			res.Err = elect.ErrConflict

			return res
		}

		l.uids++
		r := op.Record.Clone()
		r.UID = "uid-" + strconv.FormatUint(l.uids, 10)
		res.Record = l.write(now, member, op.Kind, r)
	case elect.OpUpdate:
		if !cur.exists {
			res.Err = elect.ErrNotFound

			return res
		}

		if op.Record.ResourceVersion != cur.rec.ResourceVersion ||
			(op.Record.UID != "" && op.Record.UID != cur.rec.UID) || l.injectConflict() {
			res.Err = elect.ErrConflict

			return res
		}

		r := op.Record.Clone()
		r.UID = cur.rec.UID
		res.Record = l.write(now, member, op.Kind, r)
	default:
		res.Err = errors.New("dst: unknown lease op")
	}

	return res
}

func read(v leaseVersion) (elect.Record, error) {
	if !v.exists {
		return elect.Record{}, elect.ErrNotFound
	}

	return v.rec.Clone(), nil
}

func (l *LeaseServer) injectConflict() bool {
	return l.cfg.ConflictPPM > 0 && l.rng.Uint32N(1_000_000) < l.cfg.ConflictPPM
}

func (l *LeaseServer) write(now int64, member uint32, kind elect.OpKind, r elect.Record) elect.Record {
	l.rv++
	r.ResourceVersion = strconv.FormatUint(l.rv, 10)
	l.hist = append(l.hist, leaseVersion{t: now, exists: true, rec: r})
	l.audit = append(l.audit, Write{
		Time: now, Member: member, Kind: kind,
		Holder: r.Holder, Incarnation: r.Incarnation, Term: r.Term, RV: l.rv,
	})
	l.trim(now)

	return r.Clone()
}

// cached returns the version visible at a random time within CacheLag.
func (l *LeaseServer) cached(now int64) leaseVersion {
	at := now
	if l.cfg.CacheLag > 0 {
		at -= l.rng.Int64N(l.cfg.CacheLag + 1)
	}

	for i := len(l.hist) - 1; i > 0; i-- {
		if l.hist[i].t <= at {
			return l.hist[i]
		}
	}

	return l.hist[0]
}

// trim drops history no cached read can observe any more.
func (l *LeaseServer) trim(now int64) {
	cut := now - l.cfg.CacheLag

	i := 0
	for i+1 < len(l.hist) && l.hist[i+1].t <= cut {
		i++
	}

	if i > 0 {
		l.hist = slices.Delete(l.hist, 0, i)
	}
}

// Submit executes op after a random latency on behalf of member and passes
// the result to done. The operation takes effect at the server at the
// midpoint of the latency, and the response arrives at the end.
func (l *LeaseServer) Submit(member uint32, op elect.Op, done func(elect.Result)) {
	lat := l.cfg.Latency + l.rng.Int64N(l.cfg.Jitter+1)
	l.sim.After(lat/2, func() {
		res := l.DoAs(member, op, l.sim.Now())
		l.sim.After(lat-lat/2, func() { done(res) })
	})
}

// Store returns a synchronous elect.Store that executes each call at the
// current simulated time on behalf of member.
func (l *LeaseServer) Store(member uint32) elect.Store {
	return leaseStore{l: l, member: member}
}

type leaseStore struct {
	l      *LeaseServer
	member uint32
}

func (s leaseStore) do(ctx context.Context, op elect.Op) (elect.Record, error) {
	if err := ctx.Err(); err != nil {
		return elect.Record{}, err
	}

	res := s.l.DoAs(s.member, op, s.l.sim.Now())

	return res.Record, res.Err
}

func (s leaseStore) Get(ctx context.Context) (elect.Record, error) {
	return s.do(ctx, elect.Op{Kind: elect.OpGet})
}

func (s leaseStore) GetCached(ctx context.Context) (elect.Record, error) {
	return s.do(ctx, elect.Op{Kind: elect.OpGetCached})
}

func (s leaseStore) Create(ctx context.Context, r elect.Record) (elect.Record, error) {
	return s.do(ctx, elect.Op{Kind: elect.OpCreate, Record: r})
}

func (s leaseStore) Update(ctx context.Context, r elect.Record) (elect.Record, error) {
	return s.do(ctx, elect.Op{Kind: elect.OpUpdate, Record: r})
}

// CheckAudit verifies the write log: terms never decrease, a change of
// holder or incarnation strictly increases the term, and no term has two
// holders. Releases (empty Holder) are allowed at the current term.
func CheckAudit(ws []Write) error {
	type who struct {
		holder string
		inc    uint64
	}

	owner := make(map[uint64]who)

	var prev *Write

	for i := range ws {
		w := &ws[i]
		if w.Holder != "" {
			cur := who{w.Holder, w.Incarnation}
			if o, ok := owner[w.Term]; ok && o != cur {
				return fmt.Errorf("term %d held by %s/%d and %s/%d (rv %d)",
					w.Term, o.holder, o.inc, w.Holder, w.Incarnation, w.RV)
			}

			owner[w.Term] = cur
		}

		if prev != nil {
			if w.Term < prev.Term {
				return fmt.Errorf("term decreased from %d to %d at rv %d", prev.Term, w.Term, w.RV)
			}

			changed := w.Holder != "" && prev.Holder != "" &&
				(w.Holder != prev.Holder || w.Incarnation != prev.Incarnation)
			if changed && w.Term <= prev.Term {
				return fmt.Errorf("holder changed %s/%d -> %s/%d without a new term at rv %d",
					prev.Holder, prev.Incarnation, w.Holder, w.Incarnation, w.RV)
			}
		}

		prev = w
	}

	return nil
}
