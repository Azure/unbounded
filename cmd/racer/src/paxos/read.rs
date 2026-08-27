use super::*;

// --- read path ---

impl Paxos {
    /// Reads take no ballot and write nothing. A read is believed when two replicas report
    /// the same `(version, ballot)`: two conflicting one-shots at one version carry different
    /// ballots, so a matching pair implies matching bytes and needs no digest.
    ///
    /// The only caller that records an OCC observation: the ring must not see a peer's `GET`.
    pub async fn read(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        sink: Sink<'_>,
    ) -> Result<Register, Status> {
        let r = self.read_uncounted(worker.clone(), addr, sink).await;
        match r {
            // A hole is an observation too, at version zero: without it the first write to
            // an OCC page could never pass its check.
            Ok(reg) => self.alloc.observed(&worker, addr, reg.version).await,
            Err(Status::Hole) => self.alloc.observed(&worker, addr, 0).await,
            Err(_) => {}
        }
        r
    }

    /// A peer's `GET` with `imm == 0`: it resolved our zone and stopped there, so it asks for
    /// the group's answer rather than this member's copy. Running the hedged round here makes
    /// a cross-zone read linearizable for the one round trip the reader paid. The observation
    /// ring must not see it: the read is a client's, not ours.
    pub async fn read_for(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        sink: Sink<'_>,
    ) -> Result<Register, Status> {
        self.read_uncounted(worker, addr, sink).await
    }

    async fn read_uncounted(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        mut sink: Sink<'_>,
    ) -> Result<Register, Status> {
        // Homed in another zone: the gateway resolves the group and the member it reaches
        // runs the round, so this costs one round trip rather than three and the metadata
        // legs stay inside the owning zone.
        if let Some(z) = self.away(&worker, addr)? {
            if let Some(r) = self.warmed_leg(worker.clone(), addr, sink.reborrow()).await {
                return Ok(r);
            }
            return self.pull_away(worker, z, addr, sink).await;
        }
        let group = self.group(&worker, addr);
        let m = self.members(&worker, group).ok_or(Status::Unmapped)?;
        let me = self.self_index(&worker, &m);
        let need = self.quorum(&worker, addr.universe());
        let kind = self.alloc.kind_of(&worker, addr)?;

        // A group member holds the authoritative block, so it is never a cache client.
        let client = me.is_none() && need > 1;
        // The width the last round advertised. Only ever learned *from* a reply, so the
        // first read of a key is always uncached, which is what the admission filter wants.
        let w = if client {
            self.cache.hint(&worker, addr)
        } else {
            0
        };
        if w > 0
            && let Some(r) = self
                .hedged_cached(worker.clone(), addr, &m, me, w, sink.reborrow())
                .await?
        {
            return Ok(r);
        }

        let (source, first) = match self
            .fetch(worker.clone(), addr, &m, me, sink.reborrow())
            .await
        {
            Ok(v) => v,
            // The adjacent copy has nothing. `MISSING` is not a vote, so heal from the
            // other two and ask again rather than reporting a hole.
            Err(e @ (Status::Hole | Status::Missing)) => {
                if need <= 1 {
                    return Err(e);
                }
                self.stat(&worker, |s| s.read_failed += 1);
                let best = self.repair(worker.clone(), addr).await?;
                // The repair round is the first authoritative word on this key. If nothing
                // was ever chosen the client is reading a hole, which the wire cannot say:
                // `MISSING` is all a peer can report about a page it lacks.
                if empty(kind, best.version) {
                    return Err(Status::Hole);
                }
                (
                    None,
                    self.pull_best(worker.clone(), addr, &m, me, best, sink.reborrow())
                        .await?,
                )
            }
            Err(e) => return Err(e),
        };
        if need <= 1 {
            self.stat(&worker, |s| s.read_matched += 1);
            return Ok(first);
        }
        let others = self.metas(worker.clone(), addr, &m, me, source).await;
        if others.contains(&Some(first)) {
            self.stat(&worker, |s| s.read_matched += 1);
            if client {
                self.offer(&worker, addr, &sink, first).await;
            }
            return Ok(first);
        }
        // No pair including the copy we already hold. If two others agree, their value is
        // chosen and costs one more round trip; if nobody agrees, nothing was chosen and we
        // must repair first. Returning an unconfirmed value would lose linearizability.
        match matching(&others) {
            Some((r, _)) => {
                self.stat(&worker, |s| s.read_remote_match += 1);
                self.pull_any(worker.clone(), addr, &m, me, &others, r, sink.reborrow())
                    .await?;
                if client {
                    self.offer(&worker, addr, &sink, r).await;
                }
                Ok(r)
            }
            None => {
                self.stat(&worker, |s| s.read_failed += 1);
                let best = self.repair(worker.clone(), addr).await?;
                if empty(kind, best.version) {
                    return Err(Status::Hole);
                }
                self.pull_best(worker, addr, &m, me, best, sink).await
            }
        }
    }

    /// The cross-zone read's cache leg: our own copy, or a cohort peer's. `None` unless the
    /// extent named this zone in `warm_zones`.
    ///
    /// No confirmation round: confirming would cost the fabric crossing this exists to avoid.
    /// Sound because only an immutable extent may be warmed, and the cache filters on the
    /// extent's live version, a function of the tombstone epoch, so an entry either carries
    /// the value or is recognizably not it. A trim or epoch advance invalidates every copy in
    /// every zone at once, without a message.
    ///
    /// Width one: the warm placed one copy per cohort at the rendezvous winner of each
    /// column, where `holds` and `replica` look at width one.
    async fn warmed_leg(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        sink: Sink<'_>,
    ) -> Option<Register> {
        if !worker.config().warmed_here(addr.0) {
            return None;
        }
        if self.cache.holds(&worker, addr, 1) {
            return self.cache.load_immutable(&worker, addr, sink.buf()).await;
        }
        self.cached_leg(worker, addr, 1, sink).await
    }

    /// The cached leg and the metadata round, issued together, so a hit costs one round trip
    /// and at most two hops, what the uncached read costs. What changes is where the bytes
    /// come from: no media read at the owner and no page on the wire from it.
    ///
    /// `None` means the round agreed on nothing, which the uncached path repairs.
    async fn hedged_cached(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        m: &[u32; 3],
        me: Option<u8>,
        w: u8,
        mut sink: Sink<'_>,
    ) -> Result<Option<Register>, Status> {
        let (cached, others) = join2(
            self.cached_leg(worker.clone(), addr, w, sink.reborrow()),
            self.metas(worker.clone(), addr, m, me, None),
        )
        .await;
        let agreed = matching(&others);
        // Confirmation is on `(version, ballot)`, not version alone, so a copy left behind by
        // a migrated extent fails it: the term bump changed the ballot.
        if let Some((r, _)) = agreed
            && cached == Some(r)
        {
            self.cache.served(&worker);
            self.stat(&worker, |s| s.read_matched += 1);
            return Ok(Some(r));
        }
        if let Some(r) = cached {
            self.cache.forget_stale(&worker, addr, r).await;
        }
        let Some((r, idx)) = agreed else {
            return Ok(None);
        };
        // Stale or absent entry. The metadata round is done, so only the data leg is left:
        // one extra round trip on a rare path.
        self.stat(&worker, |s| s.read_remote_match += 1);
        let route = self
            .route(worker.clone(), addr.universe(), m, idx)
            .ok_or(Status::Io)?;
        // The confirmed register may name bytes this member cannot produce: a tombstone
        // has none, and a copy whose data write never landed answers the same way. Neither
        // is the group's answer, so fall back to the full round, which heals what it can
        // and knows a hole when it sees one. Reporting the miss instead would reach the
        // guest as `EIO` for a page it is entitled to read as zeroes.
        match self
            .pull_from(worker.clone(), route, addr, sink.reborrow())
            .await
        {
            Ok(_) => {}
            Err(Status::Hole | Status::Missing) => return Ok(None),
            Err(e) => return Err(e),
        }
        self.offer(&worker, addr, &sink, r).await;
        Ok(Some(r))
    }

    /// The page from wherever the cohort keeps it: locally if we are one of the `w` replicas,
    /// else a `CACHE_ONLY` `GET` at the highest-ranked live one. It carries the register the
    /// copy claims, believed only once the metadata round confirms it.
    async fn cached_leg(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        w: u8,
        sink: Sink<'_>,
    ) -> Option<Register> {
        if self.cache.holds(&worker, addr, w) {
            return self.cache.load(&worker, addr, sink.buf()).await;
        }
        let node = self.cache.replica(&worker, addr, w, |n| {
            self.link_of(&worker, addr.universe(), n).is_some()
        })?;
        let link = self.link_of(&worker, addr.universe(), node)?;
        let Sink::Small(p) = sink;
        // Gather mode: the peer's page and the register it claims arrive in one command. A
        // miss, or a shedding replica, answers `MISSING` and we fall back to the group.
        let cmd = Cmd::Get {
            page: self.page_ref(addr).ok()?,
            from: Source::Cache,
            want: Want::Gather,
        };
        let t = PoolBuf::alloc(2 * fabric::BLOCK).await;
        link.send(cmd, t.buf()).await.ok()?;
        p[..fabric::BLOCK].copy_from_slice(&t[..fabric::BLOCK]);
        read_register(&t[fabric::BLOCK..]).ok()
    }

    /// A reader that is one of the `w` replicas already has the page in flight from the group,
    /// so admission writes bytes it holds anyway. The cache decides whether to take it.
    async fn offer(&'static self, worker: &Worker, addr: GlobalAddr, sink: &Sink<'_>, r: Register) {
        let w = self.cache.hint(worker, addr);
        if w > 0 {
            self.cache.admit(worker, addr, sink.buf(), r, w).await;
        }
    }

    /// Read the page and its register from an adjacent member, or locally if we hold it.
    /// Returns the member index the bytes came from.
    ///
    /// Members we hold a link to are asked first, so the page comes off an adjacent node
    /// rather than being relayed. A member that does not answer is not the group's answer:
    /// the other two are tried in turn. `Hole` and `Missing` are answers, escalated to repair
    /// by the caller.
    async fn fetch(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        m: &[u32; 3],
        me: Option<u8>,
        mut sink: Sink<'_>,
    ) -> Result<(Option<u8>, Register), Status> {
        if let Some(k) = me {
            let r = self.read_local(&worker, addr, sink).await?;
            return Ok((Some(k), r));
        }
        for i in self.nearest_first(&worker, addr.universe(), m) {
            let Some(route) = self.route(worker.clone(), addr.universe(), m, i) else {
                continue;
            };
            match self
                .pull_from(worker.clone(), route, addr, sink.reborrow())
                .await
            {
                Ok(r) => return Ok((Some(i), r)),
                Err(Status::Io) => {}
                Err(e) => return Err(e),
            }
        }
        Err(Status::Io)
    }

    /// The `buf.len()` bytes at `off` within a 4 MiB page, from whichever member answers.
    /// This class takes no round on a hit, but a local miss at a node that holds no copy is
    /// not the group's answer.
    ///
    /// A member with nothing answers `MISSING`, which is not a vote: the wire cannot tell a
    pub(super) async fn read_local(
        &'static self,
        worker: &Worker,
        addr: GlobalAddr,
        sink: Sink<'_>,
    ) -> Result<Register, Status> {
        // The register comes back with the bytes, not from a separate look: an accept landing
        // between the two would pair a value with a version it was never written at.
        let Sink::Small(p) = sink;
        self.alloc.read_block(worker, addr, p).await
    }

    /// One `GET` at a peer, in gather mode: the block and its register arrive in one
    /// command, one block of payload and one of trailer.
    pub(super) async fn pull_from(
        &self,
        worker: Rc<Worker>,
        r: Route,
        addr: GlobalAddr,
        sink: Sink<'_>,
    ) -> Result<Register, Status> {
        let page = self.page_ref(addr)?;
        let from = Source::Group(r.via());
        let Sink::Small(p) = sink;
        let t = PoolBuf::alloc(2 * fabric::BLOCK).await;
        let get = Cmd::Get {
            page,
            from,
            want: Want::Gather,
        };
        r.send(get, t.buf()).await?;
        if p.len() < fabric::BLOCK {
            return Err(Status::Unmapped);
        }
        p[..fabric::BLOCK].copy_from_slice(&t[..fabric::BLOCK]);
        let (reg, width) = read_meta(&t[fabric::BLOCK..])?;
        self.cache.note_hint(&worker, addr, width);
        Ok(reg)
    }

    /// The bytes belonging to the register a repair round settled on.
    ///
    /// Repair carries a decision, not a page: `learn` moves a replica's entry but only pulls
    /// data where its own copy is behind or unreadable. So answer from whichever member's
    /// metadata equals what the round chose, remote first: ours just failed.
    async fn pull_best(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        m: &[u32; 3],
        me: Option<u8>,
        best: Register,
        sink: Sink<'_>,
    ) -> Result<Register, Status> {
        let after = self.metas(worker.clone(), addr, m, me, None).await;
        self.pull_any(worker, addr, m, me, &after, best, sink).await
    }

    /// The bytes for a register we already know which members hold. A member whose data write
    /// never landed reports `MISSING` for a register it still carries, and one copy failing is
    /// not the group's answer, so the others are asked. Remote first: ours just failed.
    #[allow(clippy::too_many_arguments)]
    async fn pull_any(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        m: &[u32; 3],
        me: Option<u8>,
        regs: &[Option<Register>; 3],
        want: Register,
        mut sink: Sink<'_>,
    ) -> Result<Register, Status> {
        let mut last = Status::Io;
        for i in 0..3u8 {
            if Some(i) == me || regs[i as usize] != Some(want) {
                continue;
            }
            let Some(route) = self.route(worker.clone(), addr.universe(), m, i) else {
                continue;
            };
            match self
                .pull_from(worker.clone(), route, addr, sink.reborrow())
                .await
            {
                Ok(_) => return Ok(want),
                Err(e) => last = e,
            }
        }
        match me {
            Some(k) if regs[k as usize] == Some(want) => {
                self.read_local(&worker, addr, sink).await.map(|_| want)
            }
            _ => Err(last),
        }
    }

    /// `GETMETA` at every member but `skip`. Slot `i` is member `i`'s register, or
    /// `None` if it did not answer.
    pub(super) async fn metas(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        m: &[u32; 3],
        me: Option<u8>,
        skip: Option<u8>,
    ) -> [Option<Register>; 3] {
        let mut out = [None; 3];
        let mut pending: Vec<(usize, Route)> = Vec::new();
        for i in 0..3u8 {
            if Some(i) == skip {
                continue;
            }
            if Some(i) == me {
                // Our own leg goes through the same call a peer's `GETMETA` would, so a
                // member's own client reads feed the sketch: the cache rests on the owner
                // seeing the whole read stream, not just the fabric's share.
                out[i as usize] = self
                    .register_and_width(worker.clone(), addr)
                    .await
                    .ok()
                    .map(|(r, _)| r);
            } else if let Some(r) = self.route(worker.clone(), addr.universe(), m, i) {
                pending.push((i as usize, r));
            }
        }
        // Two at a time. A member has a local leg and so at most two to send; a non-member
        // asking all three pays one extra round trip for the odd one out, and only the shed
        // does that.
        while let Some((i, a)) = pending.pop() {
            match pending.pop() {
                Some((j, b)) => {
                    let (x, y) = join2(
                        self.ask_meta(worker.clone(), a, addr),
                        self.ask_meta(worker.clone(), b, addr),
                    )
                    .await;
                    out[i] = x.ok();
                    out[j] = y.ok();
                }
                None => out[i] = self.ask_meta(worker.clone(), a, addr).await.ok(),
            }
        }
        out
    }

    /// One `GETMETA` at a peer: the metadata half of the hedged read.
    async fn ask_meta(
        &self,
        worker: Rc<Worker>,
        r: Route,
        addr: GlobalAddr,
    ) -> Result<Register, Status> {
        let cmd = Cmd::GetMeta {
            page: self.page_ref(addr)?,
            from: Source::Group(r.via()),
        };
        let t = PoolBuf::alloc(fabric::BLOCK).await;
        r.send(cmd, t.buf()).await?;
        let (reg, width) = read_meta(&t)?;
        self.cache.note_hint(&worker, addr, width);
        Ok(reg)
    }
}
