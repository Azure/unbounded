use super::*;

/// The right to run a group's prepare round, held for as long as the round runs.
///
/// One prepare per group at a time. Every prepare raises both peers' promises by one, so
/// concurrent rounds turn a burst of writes into a term escalation the accepts can never
/// catch: each is refused as stale, refreshes, and prepares again.
///
/// This was a flag set by one hop and cleared by another with the whole round in between,
/// so a writer that gave up in the middle left the group looking busy for the life of the
/// process, and every later write to it paid the full wait before deciding to ignore it.
/// The obligation is the same one; a destructor is what discharges it now, and a destructor
/// runs on every path out.
#[must_use = "an unheld lease is given up at once, letting a second prepare race this one"]
struct Prepare {
    paxos: &'static Paxos,
    worker: Rc<Worker>,
    group: GroupId,
    core: CoreId,
    active: bool,
}

impl Prepare {
    /// Hand the round back and wait for the owner to hear, so a writer already sleeping on
    /// this group wakes to the term this round settled rather than to a second round.
    async fn release(mut self) {
        self.paxos.unprepare(self.group, self.core).await;
        self.active = false;
    }
}

impl Drop for Prepare {
    fn drop(&mut self) {
        if !self.active {
            return;
        }
        let (paxos, worker, group, core) = (self.paxos, self.worker.clone(), self.group, self.core);
        let _ = runtime::spawn_local(async move {
            let _worker = worker;
            paxos.unprepare(group, core).await;
        });
    }
}

/// Whether this task runs the group's prepare, waits for the one already running, or
/// already has a term to issue at.
enum Lead {
    Held(u32),
    Wait,
    Go,
    /// Waited out a round that never finished. Prepare anyway, holding nothing.
    Give,
}

/// How long a writer waits for another task's prepare round before rechecking.
const PREPARE_WAIT: std::time::Duration = std::time::Duration::from_micros(20);
/// How many times it rechecks before giving up and preparing itself.
const PREPARE_WAITS: usize = 256;

// --- terms, repair ---

impl Paxos {
    /// The term this node may issue one-shot accepts at. A term read back from the
    /// superblock is not held until raised once, so a restart costs one prepare per group
    /// and never a stale ballot.
    pub(super) async fn term_for(
        &'static self,
        worker: Rc<Worker>,
        group: GroupId,
        addr: GlobalAddr,
    ) -> Result<u32, Status> {
        let core = self.core_of(group);
        // Waiting out whoever holds the lease keeps all three members in step; see
        // [`Prepare`] for why running two rounds at once is worse than waiting.
        //
        // The wait itself belongs on the core that owns the answer. Rechecking from here
        // meant a message each way per recheck, so a writer queued behind one prepare could
        // spend hundreds of them asking a question only that core can answer, and every one
        // of them landed on the core the prepare was trying to make progress on. Now the
        // whole wait is one message out and one back, whichever way it ends.
        let take = runtime::to_async_with::<Server, _, _, _>(core, move |worker| async move {
            for _ in 0..PREPARE_WAITS {
                let lead = here(&worker, |l| {
                    match l.terms.entry(group).or_insert(Term::new(0)).issuable() {
                        Some(t) => Lead::Held(t),
                        // Taken at the last moment, so the gap between a group being marked
                        // and someone holding the lease for it stays the width of the reply.
                        None if l.preparing.insert(group) => Lead::Go,
                        None => Lead::Wait,
                    }
                });
                match lead {
                    Lead::Wait => runtime::sleep(PREPARE_WAIT).await,
                    settled => return settled,
                }
            }
            Lead::Give
        })
        .await;
        match take {
            Lead::Held(t) => Ok(t),
            Lead::Go => {
                // Built here rather than on the owner: a reply nobody is waiting for is
                // dropped inside the rendezvous, which is no place for a destructor that
                // wants to hop.
                let lease = Prepare {
                    paxos: self,
                    worker: worker.clone(),
                    group,
                    core,
                    active: true,
                };
                let r = self.prepare_round(worker, addr, None).await;
                lease.release().await;
                r.map(|(t, ..)| t)
            }
            // Prepare anyway, without the lease: the group is no worse off than it was, and
            // a writer that never returns is worse.
            Lead::Wait | Lead::Give => self
                .prepare_round(worker, addr, None)
                .await
                .map(|(t, ..)| t),
        }
    }

    /// The term we currently promise, without raising it. Used where a ballot has to be
    /// derived rather than sent.
    pub(super) async fn held_term(&'static self, group: GroupId) -> u32 {
        let core = self.core_of(group);
        at(core, move |_, l| {
            l.terms.get(&group).map_or(0, |t| t.promise())
        })
        .await
    }

    /// Hand a group's prepare round back. Not called directly: [`Prepare`] is the only
    /// holder.
    async fn unprepare(&'static self, group: GroupId, core: CoreId) {
        at(core, move |_, l| {
            l.preparing.remove(&group);
        })
        .await;
    }

    /// Give up the right to issue one-shot accepts at the term we hold, so the next
    /// `term_for` runs a prepare round. The promise itself is untouched: durable, and only
    /// ever rising.
    pub(super) async fn refresh(&'static self, group: GroupId) {
        let core = self.core_of(group);
        at(core, move |_, l| {
            if let Some(t) = l.terms.get_mut(&group) {
                t.release();
            }
        })
        .await;
    }

    /// Raise this group's promise by one and return it. Durable before use, so a promise
    /// never dies with the process. The rule itself is [`Term::raise`].
    pub(super) async fn bump(&'static self, group: GroupId) -> Result<u32, Status> {
        let core = self.core_of(group);
        let t = at(core, move |_, l| {
            l.stats.term_bumps += 1;
            l.terms.entry(group).or_insert(Term::new(0)).raise()
        })
        .await;
        self.persist().await?;
        Ok(t)
    }

    /// Record a term another member reported; see [`Term::adopt`].
    pub(super) async fn observe(&'static self, group: GroupId, term: u32) {
        let core = self.core_of(group);
        at(core, move |_, l| {
            l.terms.entry(group).or_insert(Term::new(term)).adopt(term);
        })
        .await;
    }

    /// Take a group's promise back from its other members, holding nothing at it. The rule is
    /// [`Term::recover`]; the callers are [`Paxos::rejoin`] and nothing else.
    pub(super) async fn recover(&'static self, group: GroupId, peers: [u32; 2]) {
        let core = self.core_of(group);
        at(core, move |_, l| {
            l.terms.entry(group).or_insert(Term::new(0)).recover(peers);
        })
        .await;
    }

    /// The prepare round: raise the term at a quorum, and decide which reported register was
    /// chosen.
    ///
    /// The classical rule "highest version, ties on ballot" is not enough: a losing one-shot
    /// can sit on a single acceptor at the same version as the chosen value with a higher
    /// ballot, and picking it would resurrect a value that never reached a quorum. So a
    /// `(version, ballot)` held by a majority wins outright; a three-way split at one version
    /// means nothing was chosen and the highest ballot is a free choice; and two responses
    /// that disagree at the top version are unresolvable in this round, so we retry.
    async fn prepare_round(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        below: Option<Register>,
    ) -> Result<(u32, Settled), Status> {
        // An unresolvable top version is a race, not the client's `Conflict`: retry here so
        // the only `Conflict` a caller sees is a guard mismatch.
        let mut last = self.prepare_once(worker.clone(), addr, below).await;
        for _ in 1..PREPARE_RETRIES {
            if !matches!(last, Err(Status::Conflict { .. })) {
                break;
            }
            last = self.prepare_once(worker.clone(), addr, below).await;
        }
        last
    }

    async fn prepare_once(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        below: Option<Register>,
    ) -> Result<(u32, Settled), Status> {
        let group = self.group(&worker, addr);
        let m = self.members(&worker, group).ok_or(Status::Unmapped)?;
        let me = self.self_index(&worker, &m);
        let need = self.quorum(&worker, addr.universe());

        let mut regs: [Option<Register>; 3] = [None; 3];
        let mut term = 0;
        let mut answered = 0;
        let mut heard = 0;

        // Our own promise counts on the same terms a peer's does.
        if let Some(k) = me {
            let t = self.bump(group).await?;
            // A register we lost is not a vote for version zero: a store that was wiped
            // holds nothing and a page never written holds nothing, and only the first of
            // those is a reason to keep counting the member as a possible carrier. It is
            // still an answer, though, and one that will not change when asked again.
            heard += 1;
            match self.alloc.register(&worker, addr).await {
                Ok(r) => {
                    regs[k as usize] = Some(r);
                    term = term.max(t);
                    answered += 1;
                }
                Err(Status::Missing) => {}
                Err(e) => return Err(e),
            }
        }
        // Ask both peers at once. The count below is only as good as the answers it has, so
        // this waits for every one; but a member that is gone must cost one link timeout for
        // the round, not one apiece.
        let mut pending: Vec<(usize, Route)> = (0..3u8)
            .filter(|i| Some(*i) != me)
            .filter_map(|i| {
                self.route(worker.clone(), addr.universe(), &m, i)
                    .map(|r| (i as usize, r))
            })
            .collect();
        let mut take = |i: usize, r: Result<(Register, u32), Status>| match r {
            Ok((reg, t)) => {
                regs[i] = Some(reg);
                term = term.max(t);
                answered += 1;
                heard += 1;
            }
            // Holding nothing is an answer; anything else leaves the member unaccounted for.
            Err(Status::Missing) => heard += 1,
            Err(_) => {}
        };
        // Two at a time. A member has a local leg and so at most two to send; a non-member
        // asking all three pays one extra round trip for the odd one out, and has to, because
        // the member it skipped is the one the count cannot do without.
        while let Some((i, a)) = pending.pop() {
            match pending.pop() {
                Some((j, b)) => {
                    let (x, y) = join2(
                        self.send_prepare(a, addr),
                        self.send_prepare(b, addr),
                    )
                    .await;
                    take(i, x);
                    take(j, y);
                }
                None => take(i, self.send_prepare(a, addr).await),
            }
        }
        // Availability is a question about the members, not about what they hold. A member
        // reporting that it holds nothing has answered, and asking it again will not change
        // the answer, so the round has everything it is going to get. Counting the votes
        // here instead would abandon a group exactly when a quorum of it agrees the page was
        // never written, and the `Io` that came of it reached the guest as `EIO` for a page
        // it is entitled to read as zeroes. What the vote count is for is `choose` below,
        // where a member that lost its index must stay among the members that could still be
        // carrying a value rather than become a vote for version zero.
        if heard < need {
            self.stat(&worker, |s| s.groups_unavailable += 1);
            return Err(Status::Io);
        }

        // The term to propose at is the highest any responder promised: an acceptor rejects
        // a ballot below its own promise and takes one at or above it. Members left behind
        // catch up from the ballot on the accept that follows, so nothing here has to make
        // them agree first.
        self.observe(group, term).await;

        // A value at a version can still be chosen only if the acceptors we did not hear
        // from could carry it to a quorum, so count each distinct value's votes plus the
        // silent members. Exactly one candidate must be preserved; two means the acceptor
        // that decides between them stayed silent.
        //
        // None at all means nothing was chosen *at that version*, which is not the same as
        // nothing having been chosen: a one-shot that reached a single acceptor leaves it a
        // version ahead of a value two others agreed on, and taking it because it sits on
        // top would drop an acknowledged write. So walk the versions downwards and answer
        // with the first one a quorum could stand behind.
        let kind = self.alloc.kind_of(&worker, addr)?;
        match choose(&regs, answered, 3 - heard, need, kind, below) {
            Choice::Chosen(r) => Ok((term, Settled::Chosen(r))),
            Choice::Free(r) => Ok((term, Settled::Free(r))),
            // An unresolvable top version is a race, not the client's `Conflict`:
            // `prepare_round` retries here so the only `Conflict` a caller sees is a guard
            // mismatch.
            Choice::Ambiguous => Err(Status::Conflict { current: 0 }),
            Choice::Missing => Err(Status::Missing),
        }
    }

    async fn send_prepare(
        &self,
        r: Route,
        addr: GlobalAddr,
    ) -> Result<(Register, u32), Status> {
        let page = self.page_ref(addr)?;
        let t = PoolBuf::alloc(fabric::BLOCK).await;
        r.send(Cmd::Prepare { page, via: r.via() }, t.buf()).await?;
        let reply = fabric::PrepareReply::decode(&t).map_err(Status::from_wire)?;
        Ok((reply.reg.into(), reply.term))
    }

    /// Prepare, then copy the chosen value to a quorum.
    ///
    /// Classically the write-back is the one unguarded write in the system; here it is
    /// apply-if-newer, strictly weaker in what it will overwrite and so unable to regress a
    /// version. That is what makes the term unnecessary for safety, and what collapses
    /// repair and `learn` into one operation.
    ///
    /// Returns the register the round settled on, the only authoritative answer anyone has
    /// about a key whose nearest copy came back `MISSING`.
    pub async fn repair(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
    ) -> Result<Register, Status> {
        // An extent still homed elsewhere is not ours to run a round on: every
        // page-addressed op but `LEARN` is relayed to the zone handing it over rather than
        // served here, so a round between the destination's own members cannot even be
        // asked. Replication there is a hand-off, not a decision.
        if self.inbound(&worker, addr).is_some() {
            return self.replicate(worker, addr).await;
        }
        // A free choice nobody can serve is no answer: the copy holding it may have lost
        // its bytes. Nothing was chosen in that case, so stepping down to the next register
        // on offer is legal, and the only choice that converges.
        let mut below = None;
        let mut last = Err(Status::Io);
        for _ in 0..3 {
            let (_, settled) = self.prepare_round(worker.clone(), addr, below).await?;
            let best = settled.register();
            if below.is_some_and(|b: Register| best.key() >= b.key()) {
                break;
            }
            last = self.settle(worker.clone(), addr, best).await;
            // Only a round that found nothing chosen may step down, which is the whole
            // difference `Settled` keeps: a chosen value is the group's answer, readable or
            // not, and there is no next register to ask for below it.
            let Some(next) = settled.step_down() else {
                break;
            };
            if !matches!(last, Err(Status::Missing | Status::Io)) {
                break;
            }
            below = Some(next);
        }
        last
    }

    /// Hand one register of an extent this zone is taking over to the rest of its group.
    ///
    /// The source seals the extent before it pushes, so no new value can appear behind us
    /// and the bulk stream is apply-if-newer: the copies converge on the newest register
    /// whatever order they arrive in, and no round is needed to decide between them.
    ///
    /// It is needed at all because the source pushes each register through one gateway,
    /// which is one member of one destination group. Without this the destination holds a
    /// single copy of everything it has been handed, its live page count never reaches the
    /// source's, and the migration never completes. The peer pulls the bytes from the
    /// source zone exactly as it would for a push that arrived there directly.
    ///
    /// A member holding nothing for the address hands on nothing; the member that does
    /// reaches it from its own sweep of the same group.
    async fn replicate(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
    ) -> Result<Register, Status> {
        let m = self
            .members(&worker, self.group(&worker, addr))
            .ok_or(Status::Unmapped)?;
        let me = self.self_index(&worker, &m).ok_or(Status::Unmapped)?;
        let held = match self.alloc.register(&worker, addr).await {
            Ok(h) => h,
            Err(Status::Missing) => return Ok(Register::default()),
            Err(e) => return Err(e),
        };
        for k in (0..3u8).filter(|&k| k != me) {
            let Some(route) = self.route(worker.clone(), addr.universe(), &m, k) else {
                continue;
            };
            // One register's failure is one register's retry on the next sweep.
            let _ = self.send_learn(route, addr, held, me, false).await;
        }
        Ok(held)
    }

    /// Copy `best` to a quorum. Split out so a free choice nobody can serve can be retried
    /// one register down.
    async fn settle(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        best: Register,
    ) -> Result<Register, Status> {
        let group = self.group(&worker, addr);
        let m = self.members(&worker, group).ok_or(Status::Unmapped)?;
        let me = self.self_index(&worker, &m);
        // Whoever holds readable bytes for the winning value is the source every laggard
        // pulls from; metadata alone cannot detect a damaged small page.
        let regs = self.metas(worker.clone(), addr, &m, me, None).await;
        let src = self
            .repair_source(worker.clone(), addr, best, &m, me, &regs)
            .await?;
        // The prepare phase only *selects* a value; it becomes the group's answer once a
        // quorum holds it. The source does already, so count the learns that land and refuse
        // to call the round authoritative until they add up. Reporting a value only one
        // acceptor carries would let the next round (which need not reach that acceptor)
        // settle on a different one, and a client told the first would then see the second.
        // A member already past `best` counts: it built what it holds on top.
        let [a, b] = match src {
            0 => [1, 2],
            1 => [0, 2],
            2 => [0, 1],
            _ => unreachable!("member index is within the trio"),
        };
        let (x, y) = join2(
            self.hand(worker.clone(), addr, best, src, &m, me, a),
            self.hand(worker.clone(), addr, best, src, &m, me, b),
        )
        .await;
        if !carried(
            x as usize + y as usize,
            self.quorum(&worker, addr.universe()),
        ) {
            self.stat(&worker, |s| s.groups_unavailable += 1);
            return Err(Status::Io);
        }
        self.stat(&worker, |s| s.repairs += 1);
        Ok(best)
    }

    /// Put one more copy of the chosen value where it belongs: our own slab if we are the
    /// target, otherwise a `LEARN` telling that member where to pull it from.
    #[allow(clippy::too_many_arguments)]
    async fn hand(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        best: Register,
        src: u8,
        m: &[u32; 3],
        me: Option<u8>,
        i: u8,
    ) -> bool {
        if Some(i) == me {
            self.learn(worker, addr, best, src, true).await.is_ok()
        } else if let Some(r) = self.route(worker.clone(), addr.universe(), m, i) {
            self.send_learn(r, addr, best, src, true)
                .await
                .is_ok()
        } else {
            false
        }
    }

    pub(super) async fn send_learn(
        &self,
        route: Route,
        addr: GlobalAddr,
        r: Register,
        from: u8,
        repair: bool,
    ) -> Result<(), Status> {
        let page = self.page_ref(addr)?;
        let mut t = PoolBuf::alloc(fabric::BLOCK).await;
        fabric::LearnReq {
            reg: r.into(),
            from: Member::new(from).ok_or(Status::Unmapped)?,
            repair,
        }
        .encode(&mut t)
        .map_err(Status::from_wire)?;
        route
            .send(
                Cmd::Learn {
                    page,
                    via: route.via(),
                },
                t.buf(),
            )
            .await
    }

    async fn repair_source(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        best: Register,
        m: &[u32; 3],
        me: Option<u8>,
        regs: &[Option<Register>; 3],
    ) -> Result<u8, Status> {
        let kind = self.alloc.kind_of(&worker, addr)?;
        if empty(kind, best.version) {
            return regs
                .iter()
                .position(|r| *r == Some(best))
                .map(|i| i as u8)
                .ok_or(Status::Missing);
        }

        let mut last = Status::Missing;
        let mut page = PoolBuf::alloc(fabric::BLOCK).await;
        for i in 0..3u8 {
            if regs[i as usize] != Some(best) {
                continue;
            }
            let got = if Some(i) == me {
                self.read_local(&worker, addr, Sink::Small(&mut page)).await
            } else if let Some(route) = self.route(worker.clone(), addr.universe(), m, i) {
                self.pull_from(worker.clone(), route, addr, Sink::Small(&mut page))
                    .await
            } else {
                Err(Status::Io)
            };
            match got {
                Ok(r) if r == best => return Ok(i),
                Ok(_) => last = Status::Missing,
                Err(e) => last = e,
            }
        }
        Err(last)
    }

    /// Write the promise table and the seal table back to the superblock. Both are tiny and
    /// change rarely, so they can afford a full rewrite and the superblock's redundancy
    /// rather than the mblocks' delta scheme.
    pub(super) async fn persist(&'static self) -> Result<(), Status> {
        let mut c = layout::Consensus::default();
        for i in 0..runtime::cores() {
            let (terms, seals) = at(CoreId::of(i), move |_, l| {
                let t: Vec<(GroupId, u32)> =
                    l.terms.iter().map(|(&g, x)| (g, x.promise())).collect();
                let s: Vec<(u32, u32)> = if i == 0 {
                    l.seals.iter().map(|(&k, &v)| (k, v)).collect()
                } else {
                    Vec::new()
                };
                (t, s)
            })
            .await;
            c.terms.extend(terms);
            c.seals.extend(
                seals
                    .into_iter()
                    .map(|(extent, term)| layout::Seal { extent, term }),
            );
        }
        c.terms.sort_unstable();
        c.terms.truncate(layout::MAX_TERMS);
        c.seals.truncate(layout::MAX_SEALS);
        self.alloc.save_consensus(&c).await
    }
}
