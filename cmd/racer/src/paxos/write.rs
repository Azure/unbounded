use super::*;

// --- client side ---

impl Paxos {
    /// The originating node's write path. `guard` is the version the caller expects to
    /// replace and is every type check at once. Returns the new version, always `guard + 1`, which
    /// is why an `ACCEPT` needs no reply body.
    pub(super) async fn propose(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        guard: Option<u64>,
        page: Page<'_>,
    ) -> Result<u64, Status> {
        // Homed in another zone: not in our slot table, so no group here to resolve. The
        // gateway resolves it and the member it reaches proposes.
        if let Some(z) = self.away(&worker, addr)? {
            self.via(worker.clone(), z, addr, false, |r| {
                self.send_accept(&worker, r, addr, guard, Ballot::ZERO, page)
            })
            .await?;
            return Ok(guard.map_or(0, |g| g + 1));
        }
        let group = self.group(&worker, addr);
        let replaying = match self.gate(worker.clone(), addr, group).await? {
            Gate::Serve { replaying } => replaying,
            // Sealed here and the config has not caught up: hand it to the destination.
            Gate::Away(z) => {
                self.via(worker.clone(), z, addr, false, |r| {
                    self.send_accept(&worker, r, addr, guard, Ballot::ZERO, page)
                })
                .await?;
                return Ok(guard.map_or(0, |g| g + 1));
            }
        };
        let m = self.members(&worker, group).ok_or(Status::Unmapped)?;
        match self.self_index(&worker, &m).filter(|_| !replaying) {
            // We hold the register, so we propose and drive the fan-out. A guard left to us
            // is derived here, where the slab is authoritative, and sent on to the peers.
            Some(k) => {
                let g = match guard {
                    Some(g) => g,
                    None => self.alloc.guard(&worker, addr).await?,
                };
                self.drive(worker, addr, group, m, k, g, page).await
            }
            // Otherwise the close member proposes, which `imm` zero says. The data crosses
            // the wire once and needs no reply body.
            None => self.forward(worker, addr, m, false, guard, page).await,
        }
    }

    /// `propose` with the guard derived here. OCC takes the version the client read (and
    /// conflicts if it did not read); an Immutable fill takes `3 * epoch`.
    pub async fn write(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        page: Page<'_>,
    ) -> Result<u64, Status> {
        let guard = self.alloc.guard(&worker, addr).await?;
        self.propose(worker, addr, Some(guard), page).await
    }

    /// Delete a page.
    ///
    /// An Immutable page becomes a tombstone: an ordinary guarded accept whose value is a
    /// state and not an event, so a repeat is free, and the entry itself goes when the
    /// control plane advances the extent's epoch past it.
    ///
    /// A mutable page has no epoch to advance, so there is no barrier that could make
    /// releasing its register safe. A member that missed the release still holds the old
    /// value at its old version, a released register reads as version zero, and the next
    /// repair round picks the higher of the two: bytes a client was told were discarded
    /// come back. So a mutable discard is an accept of zeroes, which is what a hole reads
    /// as anyway, and orders against concurrent writes like the write it is. It leaves the
    /// slot allocated, which a discard is entitled to do: freeing it needs a barrier this
    /// class does not have.
    pub async fn trim(&'static self, worker: Rc<Worker>, addr: GlobalAddr) -> Result<(), Status> {
        let kind = self.alloc.kind_of(&worker, addr)?;
        if kind != Kind::Immutable {
            let mut zeroes = PoolBuf::alloc(fabric::BLOCK).await;
            zeroes.fill(0);

            return self
                .write(worker, addr, Page::Small(&zeroes))
                .await
                .map(drop);
        }
        let epoch = worker.config().tombstone_epoch_of(addr.0);
        // A live page sits at `3e + 1`; that is what a trim guards on.
        let guard = 3 * epoch + 1;
        // Homed elsewhere: the gateway resolves the group.
        if let Some(z) = self.away(&worker, addr)? {
            return self
                .via(worker.clone(), z, addr, false, |r| {
                    self.send_trim(&worker, r, addr, guard, Ballot::ZERO)
                })
                .await;
        }
        let group = self.group(&worker, addr);
        let replaying = match self.gate(worker.clone(), addr, group).await? {
            Gate::Serve { replaying } => replaying,
            Gate::Away(z) => {
                return self
                    .via(worker.clone(), z, addr, false, |r| {
                        self.send_trim(&worker, r, addr, guard, Ballot::ZERO)
                    })
                    .await;
            }
        };
        // Immutable cache entries are invalidated by the epoch bump, which a delete only
        // reaches later. Dropping our own entry closes the window here; a cohort replica
        // elsewhere holds its copy until the epoch advances.
        self.cache.forget(&worker, addr).await;
        let m = self.members(&worker, group).ok_or(Status::Unmapped)?;
        match self.self_index(&worker, &m).filter(|_| !replaying) {
            Some(k) => {
                let term = self.term_for(worker.clone(), group, addr).await?;
                let b = Ballot::new(term, k);
                let need = self.quorum(&worker, addr.universe());
                let local = self.alloc.accept_trim(&worker, addr, guard, b);
                let mut peers = self.peers(worker.clone(), addr.universe(), &m, Some(k));
                self.fan_out(&worker, local, &mut peers, need, |r| {
                    self.send_trim(&worker, r, addr, guard, b)
                })
                .await?;
                self.stat(&worker, |s| s.one_shot += 1);
                Ok(())
            }
            None => {
                // `imm` zero: the close member picks the ballot and fans out.
                self.delegate(worker.clone(), addr.universe(), &m, false, |r| {
                    self.send_trim(&worker, r, addr, guard, Ballot::ZERO)
                })
                .await
            }
        }
    }

    /// Peers of ours in this group, as routes. A member we hold no direct link to is reached
    /// through one we do rather than dropped, so the quorum stays a fixed 2 of 3 on a topology
    /// that is not a full mesh. [`Self::fan_out`] and [`Self::fan_peers`] refuse rather than
    /// ack short when even that is unavailable.
    fn peers(&self, worker: Rc<Worker>, u: u32, m: &[u32; 3], me: Option<u8>) -> Vec<Route> {
        (0..3u8)
            .filter(|i| Some(*i) != me)
            .filter_map(|i| self.route(worker.clone(), u, m, i))
            .collect()
    }

    /// Members we hold a direct link to, in member order. The first is the one we delegate
    /// to; the rest are the failover order.
    fn candidates<'a>(
        &self,
        worker: &'a Worker,
        u: u32,
        m: &[u32; 3],
    ) -> impl Iterator<Item = (&'a Link, u8)> {
        let m = *m;
        (0..3u8).filter_map(move |i| self.link_of(worker, u, m[i as usize]).map(|l| (l, i)))
    }

    /// The first member we hold a link to, plus its index.
    pub(super) fn close<'a>(
        &self,
        worker: &'a Worker,
        u: u32,
        m: &[u32; 3],
    ) -> Option<(&'a Link, u8)> {
        self.candidates(worker, u, m).next()
    }

    /// Member indices for the data leg, adjacent ones first: a `GET` routed through a
    /// neighbor crosses the wire twice. Metadata legs are a trailer each and route freely.
    pub(super) fn nearest_first(&self, worker: &Worker, u: u32, m: &[u32; 3]) -> [u8; 3] {
        nearest_first(m, |n| self.link_of(worker, u, n).is_some())
    }

    /// How to reach member `k`: directly if we hold a link, else forwarded through one we do.
    pub(super) fn route(&self, worker: Rc<Worker>, u: u32, m: &[u32; 3], k: u8) -> Option<Route> {
        let to = To::Member(Member::new(k)?);
        let node = *m.get(k as usize)?;
        match self.link_of(&worker, u, node) {
            Some(_) => Some(Route {
                worker,
                universe: u,
                node,
                via: Via::new(to, Hops::NONE),
            }),
            None => {
                let node = self.close(&worker, u, m)?.0.peer();
                Some(Route {
                    node,
                    worker,
                    universe: u,
                    via: Via::new(to, Hops::ONE),
                })
            }
        }
    }

    /// We are member `k`: stage the page locally and fan out concurrently, acking as soon as
    /// a quorum is durable. Latency is one remote hop plus the slower of the local write and
    /// one peer accept; the third acceptor is in flight and nobody waits.
    /// A term to propose at and the address to propose for, in one visit to the group's
    /// core.
    ///
    /// Both live in the same row, and on the path a write normally takes both answers are
    /// already there: a term this node holds, and an address nobody else is writing.
    /// Asking for them one after the other made the core answer twice with an integer
    /// each, for no more than the reading of two fields. Only a write that has to prepare
    /// pays two visits now, and it is about to pay for a round of messages anyway.
    async fn lead(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        group: GroupId,
    ) -> Result<(u32, Claim), Status> {
        let core = self.core_of(group);
        // Built on this core, not the group's: a claim gives itself back from a
        // destructor, and an answer nobody waited for is dropped inside the rendezvous.
        match at(core, move |_, l| {
            match l.terms.entry(group).or_insert(Term::new(0)).issuable() {
                None => Fast::Prepare,
                // The address is only taken once there is a term to take it for.
                Some(t) if l.inflight.insert(addr.0) => Fast::Ready(t),
                Some(_) => Fast::Busy,
            }
        })
        .await
        {
            Fast::Ready(term) => Ok((
                term,
                Claim {
                    paxos: self,
                    worker: worker.clone(),
                    addr,
                    core,
                    active: true,
                },
            )),
            Fast::Busy => Err(Status::Conflict { current: 0 }),
            Fast::Prepare => {
                let term = self.term_for(worker.clone(), group, addr).await?;
                let claim = self.claim(worker, addr, group).await?;
                Ok((term, claim))
            }
        }
    }

    #[allow(clippy::too_many_arguments)]
    async fn drive(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        group: GroupId,
        m: [u32; 3],
        k: u8,
        guard: u64,
        page: Page<'_>,
    ) -> Result<u64, Status> {
        let (term, claim) = self.lead(worker.clone(), addr, group).await?;
        let b = Ballot::new(term, k);
        let need = self.quorum(&worker, addr.universe());
        let mut peers = self.peers(worker.clone(), addr.universe(), &m, Some(k));
        let r = self
            .round(worker.clone(), addr, &mut peers, need, guard, b, page)
            .await;
        claim.release().await;
        match r {
            Ok(()) => {
                self.stat(&worker, |s| {
                    s.one_shot += 1;
                    s.accept_ok += 1;
                });
                // Tell the zones that asked to keep this extent warm. Detached and after the
                // decision: a write must not wait on another zone.
                self.fan_warm(worker, addr, guard + 1);
                Ok(guard + 1)
            }
            Err(e) => {
                // Members refresh on a rejected ballot. A quorum of the other two can raise
                // the term without us and the refusal carries no term back (an `ACCEPT` reply
                // has no trailer), so rejection is the only signal. Dropping the held flag
                // makes the next attempt prepare, re-establishing a term we may propose at.
                self.refresh(group).await;
                self.stat(&worker, |s| match e {
                    Status::Conflict { .. } => s.guard_conflicts += 1,
                    _ => s.accept_rejected += 1,
                });
                Err(e)
            }
        }
    }

    /// One accept round as the proposer. Our own leg is staged, not committed, until the
    /// peers answer: a proposer that installed its value regardless would sit a version ahead
    /// of a group that never agreed to it, every retry would guard on that version, and no
    /// apply-if-newer repair could pull the fork back.
    #[allow(clippy::too_many_arguments)]
    async fn round(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        peers: &mut Vec<Route>,
        need: usize,
        guard: u64,
        b: Ballot,
        page: Page<'_>,
    ) -> Result<(), Status> {
        let staged = self.stage_local(&worker, addr, guard, b, page);
        // Every leg stages through our own registered memory, so no leg holds a buffer the
        // caller owns and none has to settle before the round answers.
        let votes = self.fan_peers(&worker, peers, need, false, |r| {
            self.send_accept(&worker, r, addr, Some(guard), b, page)
        });
        match join2(staged, votes).await {
            (Ok(p), Ok(c)) => p.commit(self, &worker, c).await,
            (Ok(p), Err(e)) => {
                p.abandon(self, &worker).await;
                Err(e)
            }
            (Err(e), _) => Err(e),
        }
    }

    /// We are not a member: hand the page to the close member, which proposes. One fabric
    /// write, and the data crosses the wire once.
    async fn forward(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        m: [u32; 3],
        once: bool,
        guard: Option<u64>,
        page: Page<'_>,
    ) -> Result<u64, Status> {
        self.delegate(worker.clone(), addr.universe(), &m, once, |r| {
            self.send_accept(&worker, r, addr, guard, Ballot::ZERO, page)
        })
        .await?;
        // A guard left to the acceptor leaves us without the new version. Nothing on the
        // ublk path reads it, and an `ACCEPT` has no reply body to carry it back.
        Ok(guard.map_or(0, |g| g + 1))
    }

    /// Hand a proposal to a member and let it propose. A member that does not answer is not
    /// the group's answer: the next candidate is tried, and only a group with nobody home is
    /// unavailable. Anything but a transport failure is the group's verdict.
    ///
    /// `once` stops after the first candidate, for the same reason [`Self::via`] takes it: a
    /// request that timed out may already have been applied, and some work must not be
    /// applied twice.
    async fn delegate<S, F>(
        &self,
        worker: Rc<Worker>,
        u: u32,
        m: &[u32; 3],
        once: bool,
        mut send: S,
    ) -> Result<(), Status>
    where
        S: FnMut(Route) -> F,
        F: Future<Output = Result<(), Status>>,
    {
        for (link, _) in self.candidates(&worker, u, m) {
            match send(Route {
                worker: worker.clone(),
                universe: u,
                node: link.peer(),
                via: Via::direct(To::Owner),
            })
            .await
            {
                Err(Status::Io) if !once => continue,
                r => return r,
            }
        }
        self.stat(&worker, |s| s.groups_unavailable += 1);
        Err(Status::Io)
    }

    async fn stage_local(
        &'static self,
        worker: &Worker,
        addr: GlobalAddr,
        guard: u64,
        b: Ballot,
        page: Page<'_>,
    ) -> Result<Proposed, Status> {
        let p = match page {
            Page::Small(p) => self.alloc.begin_block(worker, addr, guard, b, p).await,
        };
        p.map(Proposed)
    }

    /// The route's `imm` tells the target whether to apply or propose: it names a member on
    /// the proposer's own fan-out, and is zero on the leg from a non-member, which hands the
    /// proposal over entire.
    async fn send_accept(
        &self,
        worker: &Worker,
        r: Route,
        addr: GlobalAddr,
        guard: Option<u64>,
        b: Ballot,
        page: Page<'_>,
    ) -> Result<(), Status> {
        let page_ref = self.page_ref(addr)?;
        match page {
            // A block already pays a copy through registered memory, so gathering its
            // trailer beside it is free. Every accept carries its guard, ballot and epoch
            // explicitly; nothing is derived from the frame's shape.
            Page::Small(p) => {
                let mut t = PoolBuf::alloc(2 * fabric::BLOCK).await;
                t[..fabric::BLOCK].copy_from_slice(p);
                let req = fabric::AcceptReq {
                    guard: match guard {
                        Some(g) => fabric::Guard::At(g),
                        None => fabric::Guard::Derived,
                    },
                    ballot: b.raw(),
                    epoch: worker.config().epoch_of(addr.0),
                };
                req.encode(&mut t[fabric::BLOCK..])
                    .map_err(Status::from_wire)?;
                let cmd = Cmd::Accept {
                    page: page_ref,
                    via: r.via(),
                };
                r.send(cmd, t.buf()).await
            }
        }
    }

    async fn send_trim(
        &self,
        worker: &Worker,
        r: Route,
        addr: GlobalAddr,
        guard: u64,
        b: Ballot,
    ) -> Result<(), Status> {
        let page = self.page_ref(addr)?;
        let mut t = PoolBuf::alloc(fabric::BLOCK).await;
        let req = fabric::TrimReq {
            guard,
            ballot: b.raw(),
            epoch: worker.config().epoch_of(addr.0),
        };
        req.encode(&mut t).map_err(Status::from_wire)?;
        r.send(Cmd::Trim { page, via: r.via() }, t.buf()).await
    }

    /// However many peer accepts the quorum needs beside our own leg. Losing legs are
    /// abandoned, not canceled: their futures are dropped and whatever they were doing
    /// completes unobserved. `settle` forbids that for a payload the caller does not own.
    async fn fan_peers<S, F>(
        &self,
        worker: &Worker,
        peers: &mut Vec<Route>,
        need: usize,
        settle: bool,
        mut send: S,
    ) -> Result<Carried, Status>
    where
        S: FnMut(Route) -> F,
        F: Future<Output = Result<(), Status>>,
    {
        // A quorum we cannot reach means the group is unavailable, not smaller: acking on
        // the local write alone would let an isolated member decide.
        if !carried(peers.len(), need) {
            self.stat(worker, |s| s.groups_unavailable += 1);
            return Err(Status::Io);
        }
        let want = need.saturating_sub(1);
        match (peers.pop(), peers.pop()) {
            _ if want == 0 => Ok(Carried(())),
            (None, _) => Ok(Carried(())),
            // Two members and a quorum of two: the one peer must land.
            (Some(a), None) => send(a).await.map(|()| Carried(())),
            (Some(a), Some(b)) => {
                let q = if settle {
                    let (x, y) = join2(send(a), send(b)).await;
                    [Some(x), Some(y)]
                } else {
                    runtime::quorum([send(a), send(b)], want).await
                };
                let ok = q.iter().flatten().filter(|r| r.is_ok()).count();
                if carried(ok, need) {
                    Ok(Carried(()))
                } else {
                    // Prefer a refusal (the group's verdict) over a peer we could not reach,
                    // so one member behind on its term does not read as a group that is gone.
                    let e = q
                        .into_iter()
                        .flatten()
                        .filter_map(|r| r.err())
                        .min_by_key(|e| matches!(e, Status::Io) as u8)
                        .unwrap_or(Status::Io);
                    Err(e)
                }
            }
        }
    }

    /// Await the local accept and however many peer accepts the quorum still needs.
    async fn fan_out<L, S, F>(
        &self,
        worker: &Worker,
        local: L,
        peers: &mut Vec<Route>,
        need: usize,
        mut send: S,
    ) -> Result<(), Status>
    where
        L: Future<Output = Result<(), Status>>,
        S: FnMut(Route) -> F,
        F: Future<Output = Result<(), Status>>,
    {
        // A quorum we cannot reach means the group is unavailable, not smaller: acking on
        // the local write alone would let an isolated member decide.
        if !carried(peers.len(), need) {
            self.stat(worker, |s| s.groups_unavailable += 1);
            return Err(Status::Io);
        }
        match (peers.pop(), peers.pop()) {
            (None, _) => local.await,
            (Some(a), None) => {
                let (l, r) = join2(local, send(a)).await;
                // Two members and a quorum of two: both must land.
                if need >= 2 { l.and(r) } else { l.or(r) }
            }
            (Some(a), Some(b)) => {
                let want = need.saturating_sub(1);
                let (l, q) = join2(local, runtime::quorum([send(a), send(b)], want)).await;
                let ok = q.iter().flatten().filter(|r| r.is_ok()).count();
                if carried(ok, need) {
                    // The local write makes this node an acceptor of the value it chose,
                    // so its failure is still a failure.
                    l
                } else {
                    // Prefer a refusal (the group's verdict) over a peer we could not reach,
                    // so one member behind on its term does not read as a group that is gone.
                    let e = q
                        .into_iter()
                        .flatten()
                        .filter_map(|r| r.err())
                        .min_by_key(|e| matches!(e, Status::Io) as u8)
                        .unwrap_or(Status::Io);
                    l.and(Err(e))
                }
            }
        }
    }

    /// Take the address for one proposal, for as long as the returned claim is held.
    ///
    /// The guard forbids pipelining two writes to one page, so the proposer serializes
    /// same-key proposals rather than letting them race and both lose. It is also the
    /// one-value-per-ballot rule: a second attempt at one version would reuse a ballot, which
    /// repair could then use to resurrect a value that was never chosen.
    async fn claim(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        group: GroupId,
    ) -> Result<Claim, Status> {
        let core = self.core_of(group);
        let taken = at(core, move |_, l| l.inflight.insert(addr.0)).await;
        // Built here rather than on the owner, so it is never a value in flight: a reply
        // that nobody is waiting for is dropped inside the rendezvous, which is no place
        // for a destructor that wants to hop.
        if taken {
            Ok(Claim {
                paxos: self,
                worker,
                addr,
                core,
                active: true,
            })
        } else {
            Err(Status::Conflict { current: 0 })
        }
    }

    /// Give an address back. Not called directly: [`Claim`] is the only holder.
    async fn unclaim(&'static self, addr: GlobalAddr, core: CoreId) {
        at(core, move |_, l| {
            l.inflight.remove(&addr.0);
        })
        .await;
    }
}

/// One proposer's hold on one address, from [`Paxos::claim`] until it is dropped.
///
/// A round is a long await: peers to reach, a page to make durable, a term to settle. The
/// claim used to be two calls with all of that in between, so every way out that was not
/// the last line stranded the address until the process restarted. Holding it is the same
/// obligation, but a destructor discharges it, and a destructor runs on every path.
///
/// It cannot await, so the hand-back is a task of its own on the core that let go. What is
/// left is the width of the claiming hop itself: a caller that disappears while the owner
/// is still being asked leaves an address claimed by nobody. That was the shape of the
/// whole round before, and is now the shape of one message.
/// What one visit to a group's core can settle before a write commits to anything.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
enum Fast {
    /// A term this node may issue at, and the address taken for this write.
    Ready(u32),
    /// Another write on this node holds the address.
    Busy,
    /// No term to issue at, so the slow path, which is a prepare round.
    Prepare,
}

#[must_use = "an unheld claim is released at once, leaving the address open to a racing write"]
struct Claim {
    paxos: &'static Paxos,
    worker: Rc<Worker>,
    addr: GlobalAddr,
    core: CoreId,
    active: bool,
}

impl Claim {
    /// Give the address back and wait for the owner to hear, which is what a proposer
    /// wants before it answers: the next write to this page is usually the same client.
    async fn release(mut self) {
        self.paxos.unclaim(self.addr, self.core).await;
        self.active = false;
    }
}

impl Drop for Claim {
    fn drop(&mut self) {
        if !self.active {
            return;
        }
        let (paxos, worker, addr, core) = (self.paxos, self.worker.clone(), self.addr, self.core);
        // Detached because a destructor cannot await. If the slab has no room the address
        // stays claimed, which is what dropping a claim did every time before this.
        let _ = runtime::spawn_local(async move {
            let _worker = worker;
            paxos.unclaim(addr, core).await;
        });
    }
}

/// A quorum of peers accepted. Nothing else builds one, and there is nothing in it to read:
/// its whole purpose is to be a thing that had to come from [`Paxos::fan_peers`].
#[must_use = "a quorum nobody spends is a round that answered without installing anything"]
struct Carried(());

/// The proposer's own leg of an accept: page durable, slot held, and the register still
/// reading as it did before.
///
/// The proposer must not install its local register until a quorum carries, or a refused
/// proposal leaves this node a version ahead of the group that refused it. That rule used to
/// be a shape the code happened to have, an allocator token and a `Result<(), _>` beside it
/// in one match. Now the token is only settled through here, and committing one takes the
/// quorum that justifies it, so the wrong order is not something that can be written.
#[must_use = "a proposal holds a slot until it is committed or given back"]
struct Proposed(Pending);

impl Proposed {
    /// Install the register. Takes the quorum by value so a caller cannot hold one round's
    /// votes and commit the next round's page under them.
    async fn commit(
        self,
        paxos: &'static Paxos,
        worker: &Worker,
        _: Carried,
    ) -> Result<(), Status> {
        paxos.alloc.finish(worker, self.0).await.map(|_| ())
    }

    /// Give the slot back. The peers that did accept keep what they took: an unchosen value
    /// on an acceptor is what a later prepare is for.
    async fn abandon(self, paxos: &'static Paxos, worker: &Worker) {
        paxos.alloc.abandon(worker, self.0).await;
    }
}
