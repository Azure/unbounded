use super::*;

// --- acceptor side ---

impl Paxos {
    /// The member side of an `ACCEPT`, reached from `server::dispatch` after the command is
    /// decoded. [`To::Owner`] means the sender is not a member and we are the proposer; else
    /// we apply at the member index it names.
    pub async fn accept(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        to: To,
        req: fabric::AcceptReq,
        page: Page<'_>,
    ) -> Result<(), Status> {
        let To::Member(k) = to else {
            // The originator sent the block and the guard; we pick the ballot and drive the
            // round, so the close member proposes rather than relays.
            let guard = match req.guard {
                // `Derived` says the sender is not a member and had nothing to observe, so
                // the guard is ours; see `write`.
                fabric::Guard::Derived => None,
                fabric::Guard::At(g) => Some(g),
            };
            return self.propose(worker, addr, guard, page).await.map(|_| ());
        };
        let group = self.group(&worker, addr);
        self.gate_accept(worker.clone(), addr, group).await?;
        let _ = k;
        let b = Ballot::from_raw(req.ballot);
        // First conjunct of the acceptance rule. A ballot below our promise is a proposer
        // that missed a term bump; it refreshes on the rejection. A ballot above it is a
        // promise we adopt, which lets one member's prepare grant the whole group its new
        // term.
        if !promised(self.held_term(group).await, b) {
            self.stat(&worker, |s| s.accept_rejected += 1);
            return Err(Status::Conflict { current: 0 });
        }
        self.observe(group, b.term()).await;
        // A member's own fan-out always states the guard it observed. Nothing is derived
        // from the shape of the frame: every accept carries its guard, ballot and epoch.
        let fabric::Guard::At(guard) = req.guard else {
            return Err(Status::Unmapped);
        };
        let Page::Small(p) = page;
        let r = self
            .alloc
            .accept_block(&worker, addr, guard, b, p)
            .await
            .map(|_| ());
        self.stat(&worker, |s| match r {
            Ok(()) => s.accept_ok += 1,
            Err(Status::Conflict { .. }) => s.guard_conflicts += 1,
            Err(_) => s.accept_rejected += 1,
        });
        r
    }

    /// The member side of a `TRIM`.
    pub async fn accept_trim(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        to: To,
        req: fabric::TrimReq,
    ) -> Result<(), Status> {
        if to == To::Owner {
            return self.trim(worker, addr).await;
        }
        let group = self.group(&worker, addr);
        self.gate_accept(worker.clone(), addr, group).await?;
        let b = Ballot::from_raw(req.ballot);
        self.alloc.accept_trim(&worker, addr, req.guard, b).await
    }

    /// The member side of `GETMETA`: the hedged read at a replica, and the hop that feeds
    /// the cache's width estimator.
    pub async fn get_meta(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
    ) -> Result<fabric::MetaReply, Status> {
        let (r, w) = self.register_and_width(worker.clone(), addr).await?;
        Ok(self.reply(&worker, addr, r, w))
    }

    /// The trailer a gathered `GET` carries. The register is the one the page was read under,
    /// not a fresh look: read apart from the bytes it could name a version they were never
    /// written at, and a learner would install the pair.
    pub async fn gathered(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        r: Register,
    ) -> Result<fabric::MetaReply, Status> {
        let (_, w) = self.register_and_width(worker.clone(), addr).await?;
        Ok(self.reply(&worker, addr, r, w))
    }

    fn reply(
        &'static self,
        worker: &Worker,
        addr: GlobalAddr,
        r: Register,
        w: u8,
    ) -> fabric::MetaReply {
        fabric::MetaReply {
            reg: r.into(),
            state: state_of(self.alloc, worker, addr, r.version),
            width: w,
        }
    }

    /// One transaction on the core owning the address's group, for both the register and the
    /// replication width. The sketch updates there, so the owner sees every read of the page,
    /// including reads it no longer serves the bytes for.
    pub(super) async fn register_and_width(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
    ) -> Result<(Register, u8), Status> {
        let owner = self.alloc.owner_core(&worker, addr)?;
        let (alloc, cache) = (self.alloc, self.cache);
        runtime::to::<Server, _, _>(owner, move |_, worker| {
            let r = alloc.register_local(worker, addr)?;
            Ok((r, cache.observe_local(worker, addr)))
        })
        .await
    }

    /// The member side of `PREPARE`. A read carries no request body, so the requested term
    /// is not on the wire: the acceptor raises its own promise by one and reports it, and
    /// the preparer takes the maximum it hears back.
    pub async fn prepare(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
    ) -> Result<fabric::PrepareReply, Status> {
        let group = self.group(&worker, addr);
        let term = self.bump(group).await?;
        let r = self.alloc.register(&worker, addr).await?;
        self.stat(&worker, |s| s.prepares += 1);
        Ok(fabric::PrepareReply {
            reg: r.into(),
            term,
        })
    }

    /// The member side of `TERM`: the promise we hold for a group. Unlike `PREPARE` it raises
    /// nothing. Read by a member recovering a lost promise, which needs the floor we already
    /// refuse below, not a new round.
    pub async fn term(&'static self, group: GroupId) -> Result<fabric::TermReply, Status> {
        Ok(fabric::TermReply {
            term: self.held_term(group).await,
        })
    }

    /// The member side of `LEARN`: a value we may be behind on, and the member holding it.
    /// Apply-if-newer, so a repeated learn is free and a migration's bulk and live streams
    /// commute.
    ///
    /// `repair` also admits the equal-register case for a small page: our entry matches but
    /// our bytes fail their checksum, which metadata alone cannot see, so the copy that reads
    /// back replaces ours at the same register.
    pub async fn learn(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        r: Register,
        from: u8,
        repair: bool,
    ) -> Result<(), Status> {
        let group = self.group(&worker, addr);
        let kind = self.alloc.kind_of(&worker, addr)?;
        // A register we cannot read is not a reason to refuse the value: it says we hold
        // nothing for the address, which is what makes the learn apply.
        let held = match self.alloc.register(&worker, addr).await {
            Ok(h) => h,
            Err(Status::Missing) => Register::default(),
            Err(e) => return Err(e),
        };
        // A repaired block may need reinstalling at a register that has not moved: our entry
        // matches but our bytes do not survive their own read, which metadata alone cannot
        // see. The read below decides that; a healthy copy bails out there.
        let equal = repair && !empty(kind, r.version);
        if !supersedes(held.key(), r, equal) {
            self.stat(&worker, |s| s.learn_stale += 1);
            return Ok(());
        }
        if held.key() == r.key() {
            let mut page = PoolBuf::alloc(fabric::BLOCK).await;
            match self.alloc.read_block(&worker, addr, &mut page).await {
                Ok(got) if got.key() >= r.key() => {
                    self.stat(&worker, |s| s.learn_stale += 1);
                    return Ok(());
                }
                Ok(_) | Err(Status::Missing) => {}
                Err(e) => return Err(e),
            }
        }
        // A tombstone is a value with nothing behind it: no page to pull, so a replica
        // takes the register alone.
        if kind == Kind::Immutable && r.version % 3 == 2 {
            self.alloc.learn_tombstone(&worker, addr, r).await?;
            self.stat(&worker, |s| s.repairs += 1);
            return Ok(());
        }
        let route = match self.inbound(&worker, addr) {
            // A migration's bulk stream: the value is in the zone handing the extent over,
            // and no member of our own group has a copy. One gateway only, unlike the client
            // paths: the sweep that drove this `LEARN` drives it again next round, so a
            // timed-out gateway costs a retry interval, not a lost page.
            Some(z) => self.toward(worker.clone(), z, addr)?,
            None => {
                let m = self.members(&worker, group).ok_or(Status::Unmapped)?;
                self.route(worker.clone(), addr.universe(), &m, from)
                    .ok_or(Status::Io)?
            }
        };
        let mut buf = PoolBuf::alloc(fabric::BLOCK).await;
        let got = self
            .pull_from(worker.clone(), route, addr, Sink::Small(&mut buf))
            .await?;
        self.alloc
            .learn_block(&worker, addr, got, &buf, repair)
            .await?;
        self.stat(&worker, |s| s.repairs += 1);
        Ok(())
    }

    /// Whether this extent has already been frozen here.
    pub async fn sealed(&'static self, extent: u32) -> bool {
        at(CoreId::of(0), move |_, l| l.seals.contains_key(&extent)).await
    }

    /// Freeze an extent at this zone. Every group holding pages of it must refuse later
    /// accepts, so the seal goes to every node in the catalog rather than one group's quorum.
    /// Idempotent, so each source node driving its own fan-out is only redundant.
    ///
    /// The term is the universe's topology epoch: agreed by every node in the zone and
    /// monotone, which is all the seal table asks.
    pub async fn seal_extent(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        extent: u32,
    ) -> Result<(), Status> {
        let cfg = worker.config();
        // Fan out over this address's universe: a node in another universe holds no
        // register of this extent and no seal table row for it.
        let u = cfg.universe(addr.universe()).ok_or(Status::Unmapped)?;
        let term = u.epoch;
        let mut nodes: Vec<u32> = u.zone_nodes();
        nodes.retain(|&n| n != cfg.node.id);
        for n in nodes {
            if self.link_of(&worker, u.id, n).is_some() {
                let r = Route {
                    worker: worker.clone(),
                    universe: u.id,
                    node: n,
                    via: Via::direct(To::Owner),
                };
                let _ = self.send_seal(r, addr, extent, term).await;
            }
        }
        // Ours last: while it is absent this node keeps driving the fan-out, so a partial
        // round is retried rather than forgotten.
        self.seal(worker, extent, term).await
    }

    async fn send_seal(
        &self,
        r: Route,
        addr: GlobalAddr,
        extent: u32,
        term: u32,
    ) -> Result<(), Status> {
        let anchor = self.page_ref(addr)?;
        let mut t = PoolBuf::alloc(fabric::BLOCK).await;
        fabric::SealReq { term, extent }
            .encode(&mut t)
            .map_err(Status::from_wire)?;
        r.send(
            Cmd::Seal {
                anchor,
                via: r.via(),
            },
            t.buf(),
        )
        .await
    }

    /// Hand one register to the zone taking an extent over. `LEARN` names the value; the
    /// destination pulls the bytes from here when it turns out to be behind.
    pub async fn push(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        r: Register,
        zone: u32,
    ) -> Result<(), Status> {
        self.via(worker.clone(), zone, addr, false, |g| {
            self.send_learn(g, addr, r, 0, false)
        })
        .await
    }

    /// Whether all three members now named for `addr` hold it at `version` or later. Asked by
    /// a node shedding a group before it forgets a register.
    ///
    /// All three, not a quorum: a config rollout is not atomic, so a quorum of the new
    /// membership can leave a quorum of the old one (us and a member that has not caught up)
    /// able to run a round that never saw the value. Every quorum of either membership holds
    /// a member that answered here, so no round can regress.
    ///
    /// A member that is behind, or that we hold no link to, is a `false`: the drop waits for
    /// a later sweep.
    pub async fn confirmed(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        version: u64,
    ) -> bool {
        let Some(m) = self.members(&worker, self.group(&worker, addr)) else {
            return false;
        };
        let me = self.self_index(&worker, &m);
        let regs = self.metas(worker, addr, &m, me, None).await;
        regs.iter().all(|r| r.is_some_and(|r| r.version >= version))
    }

    /// The member side of `SEAL`: this node refuses every later accept for any address in
    /// the shard. Idempotent, and monotone in `term`.
    pub async fn seal(
        &'static self,
        _worker: Rc<Worker>,
        extent: u32,
        term: u32,
    ) -> Result<(), Status> {
        // An extent's pages are spread over every core, so the refusal has to be too.
        for core in 0..runtime::cores() {
            at(CoreId::of(core), move |_, l| {
                l.stats.seals += 1;
                let e = l.seals.entry(extent).or_insert(term);
                *e = sealed_at(Some(*e), term);
            })
            .await;
        }
        self.persist().await
    }

    /// The seal's refusal, and the replay flag, in one hop to the group's own core. Both
    /// tables are empty on the common path, so this is two predictable branches. The rule
    /// itself is [`Gate::decide`].
    pub(super) async fn gate(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        group: GroupId,
    ) -> Result<Gate, Status> {
        let cfg = worker.config();
        let core = self.core_of(group);
        let id = self.shard_of(&worker, addr);
        let (sealed, replaying) = at(core, move |_, l| {
            (
                id.is_some_and(|id| l.seals.contains_key(&id)),
                l.replaying.contains(&group),
            )
        })
        .await;
        let next = cfg.next_zone_of(addr.0).filter(|z| *z != cfg.node.zone);
        Gate::decide(sealed, replaying, next)
    }

    /// The extent `addr` falls in, as the seal table names it.
    fn shard_of(&self, worker: &Worker, addr: GlobalAddr) -> Option<u32> {
        worker.config().extent_id_of(addr.0)
    }

    /// [`Self::gate`] for the acceptor half of a round; see [`Gate::accepts`].
    async fn gate_accept(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        group: GroupId,
    ) -> Result<(), Status> {
        self.gate(worker, addr, group).await?.accepts()
    }

    /// Whether this node is still replaying `group`, and so must not be counted toward a
    /// quorum for it.
    ///
    /// A replaying node lost values it had already accepted. Counting it breaks quorum
    /// intersection: a write acked by it and one other member survives on that member alone,
    /// and a round reaching this node and the third finds nothing at that version and is free
    /// to decide something else. It refuses accepts; `LEARN` is untouched, since that traffic
    /// ends the replay.
    ///
    /// `PREPARE` must stay untouched too: a lost register reports as missing and so already
    /// counts among the members that did not answer, the conservative side. Refusing outright
    /// would discard the registers it does hold and, since a replay ends only when a repair
    /// comparison comes back clean and repairing is a prepare round, leave the group with no
    /// way out.
    pub(crate) async fn replaying(&'static self, group: GroupId) -> bool {
        let core = self.core_of(group);
        at(core, move |_, l| l.replaying.contains(&group)).await
    }

    /// The groups this core is replaying. `core_of` is the group modulo the core count, so a
    /// core's own replay set is exactly the candidates the sweep picks from, and asking is a
    /// borrow rather than a hop per group.
    pub fn replaying_here(&self, worker: &Worker) -> Vec<GroupId> {
        here(worker, |l| l.replaying.iter().copied().collect())
    }

    /// Recover this group's promise from its other members, then rejoin it.
    ///
    /// A reformat destroys the promise table along with the registers. The registers pulled
    /// back cover every address the group holds (a ballot below the one on a register is
    /// refused as a regression) but not an address the group holds nothing for, where any
    /// guard and any ballot pass. There the promise is the only thing between a stale accept
    /// and a value some round had already fixed.
    ///
    /// The promise to recover is [`super::protocol::recovered_term`].
    pub async fn rejoin(&'static self, worker: Rc<Worker>, group: GroupId) -> Result<(), Status> {
        let m = self.members(&worker, group).ok_or(Status::Unmapped)?;
        let me = self.self_index(&worker, &m).ok_or(Status::Unmapped)?;
        let mut peers = [0u32; 2];
        let mut n = 0;
        for i in 0..3u8 {
            if i == me {
                continue;
            }
            let link = self
                .link_of(&worker, group.universe(), m[i as usize])
                .ok_or(Status::Io)?;
            // Group-addressed, like the anti-entropy ops: the command names the group and
            // the reply names the promise.
            let cmd = Cmd::Term {
                group: GroupIx::new(group.index()).ok_or(Status::Unmapped)?,
            };
            let t = PoolBuf::alloc(fabric::BLOCK).await;
            link.send(cmd, t.buf()).await.map_err(Status::from_wire)?;
            peers[n] = fabric::TermReply::decode(&t)
                .map_err(Status::from_wire)?
                .term;
            n += 1;
        }
        // Durable before use, as in `bump`: a promise lost to a restart was never a
        // promise.
        self.recover(group, peers).await;
        self.persist().await?;
        self.set_replaying(group, false).await;
        Ok(())
    }

    /// Mark a group as still replaying, or caught up. Driven by the anti-entropy sweep.
    pub async fn set_replaying(&'static self, group: GroupId, on: bool) {
        let core = self.core_of(group);
        at(core, move |_, l| {
            if on {
                l.replaying.insert(group);
            } else {
                l.replaying.remove(&group);
            }
        })
        .await;
    }
}
