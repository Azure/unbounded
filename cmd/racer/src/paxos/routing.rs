use super::*;

// --- routing ---

impl Paxos {
    pub fn alloc(&self) -> &'static Allocator {
        self.alloc
    }

    pub fn cache(&self) -> &'static Cache {
        self.cache
    }

    /// Give consensus a way back to the healer. Once at startup, before any worker runs;
    /// separate from [`open`] because the healer is built from the `Paxos` it heals.
    pub fn attach_heal(&self, heal: &'static Heal) {
        let _ = self.heal.set(heal);
    }

    pub fn heal(&self) -> &'static Heal {
        self.heal.get().expect("attached at startup")
    }

    /// The consensus group an address belongs to. Placement follows the extent's kind: a
    /// mutable block routes on its own address, an immutable block on the 4 MiB stripe that
    /// contains it. An address no extent covers has no kind, so it falls back to routing on
    /// itself; every caller here independently refuses an unmapped address, and the rows
    /// that outlive their extent are routed from the slab class instead.
    pub(crate) fn group(&self, worker: &Worker, addr: GlobalAddr) -> GroupId {
        let cfg = worker.config();
        cfg.group(addr.0)
            .unwrap_or_else(|| cfg.group_of(addr.0, Kind::Mutable))
    }

    /// The core holding a group's consensus state: the group index modulo the core count,
    /// which in any real deployment is also the core the allocator shards the page to. The
    /// index alone, so universes share one core layout rather than crowding the low cores.
    pub(super) fn core_of(&self, group: GroupId) -> CoreId {
        CoreId::of(group.index() as usize % runtime::cores())
    }

    /// The three acceptors, from the catalog of the group's own universe. A group in a
    /// universe we hold no configuration for has no members here, which stops a frame naming
    /// one.
    pub(crate) fn members(&self, worker: &Worker, group: GroupId) -> Option<[u32; 3]> {
        worker
            .config()
            .universe(group.universe())?
            .catalog
            .get(group.index() as usize)
            .map(crate::config::Trio::nodes)
    }

    /// Our index in the group, if we are a member. Only a member may propose.
    pub(crate) fn self_index(&self, worker: &Worker, m: &[u32; 3]) -> Option<u8> {
        let me = worker.config().node.id;
        m.iter().position(|&n| n == me).map(|i| i as u8)
    }

    /// The link to `node` in `universe`. Per pair: the same peer in two universes publishes
    /// two namespaces, and asking without naming the universe would let a frame leave the one
    /// it arrived on.
    pub(crate) fn link_of<'a>(
        &self,
        worker: &'a Worker,
        universe: u32,
        node: u32,
    ) -> Option<&'a Link> {
        worker.compiled().link(universe, node)
    }

    /// Where a frame we will not serve goes next. `imm` names a member of `addr`'s group
    /// rather than a peer, so a forwarded frame carries no node id and passes on unchanged.
    /// Zero means the sender could not name a member (it routed by zone alone), so we do the
    /// lookup and pick a member we can reach.
    ///
    /// The link is always inside the address's own universe, so a relay cannot carry a frame
    /// out of the partition it arrived in. `None` for a foreign address we cannot route.
    pub fn forward_link<'a>(
        &self,
        worker: &'a Worker,
        op: Op,
        addr: GlobalAddr,
        to: To,
    ) -> Option<&'a Link> {
        // Homed elsewhere: pass it toward that place, which resolves the group itself.
        if !self.local_for(worker, op, addr) {
            let z = self.away(worker, addr).ok().flatten()?;
            return worker
                .config()
                .gateways_for(z, addr.0)
                .take(GATEWAY_TRIES)
                .find_map(|n| self.link_of(worker, addr.universe(), n));
        }
        let m = self.members(worker, self.group(worker, addr))?;
        match to {
            To::Owner => self.close(worker, addr.universe(), &m).map(|(l, _)| l),
            To::Member(k) => self.link_of(worker, addr.universe(), *m.get(k.index() as usize)?),
        }
    }

    /// Whether a frame addressed to `imm` is ours to answer or one we must pass on. Zero is
    /// "you own this", which only a group member can; `k + 1` names member `k`. An address
    /// homed in another zone is never ours.
    pub fn serves(&self, worker: &Worker, op: Op, addr: GlobalAddr, to: To) -> bool {
        if !self.local_for(worker, op, addr) {
            return false;
        }
        let me = self
            .members(worker, self.group(worker, addr))
            .and_then(|m| self.self_index(worker, &m));
        match to {
            To::Owner => me.is_some(),
            To::Member(k) => me == Some(k.index()),
        }
    }

    /// Whether `addr` is homed in a zone this node does not describe. A universe's catalog
    /// covers only our own zone of it, so nothing about a foreign address may be resolved
    /// locally.
    pub(super) fn foreign(&self, worker: &Worker, addr: GlobalAddr) -> bool {
        let cfg = worker.config();
        cfg.zone_of(addr.0).is_some_and(|z| z != cfg.node.zone)
    }

    /// The zone still answering for `addr` while its extent is pulled into ours. `None`
    /// unless this node is the migration's destination.
    pub(super) fn inbound(&self, worker: &Worker, addr: GlobalAddr) -> Option<u32> {
        let cfg = worker.config();
        let here = cfg.next_zone_of(addr.0)? == cfg.node.zone;
        cfg.zone_of(addr.0).filter(|&z| here && z != cfg.node.zone)
    }

    /// Whether this zone answers `op` for `addr`. A migration destination takes the extent's
    /// bulk stream before client traffic, so `LEARN` is the one op an inbound extent is
    /// already local for.
    fn local_for(&self, worker: &Worker, op: Op, addr: GlobalAddr) -> bool {
        !self.foreign(worker, addr) || (op == Op::Learn && self.inbound(worker, addr).is_some())
    }

    /// The zone to send `addr` to, if it is not homed here. We resolve only the zone; the
    /// gateway we reach holds that zone's catalog and does the rest. `imm` is zero on the way
    /// out because we cannot name a member, and the hop budget is two: one to reach a member,
    /// one spare for a shard mid-migration at the far end. An unroutable foreign address is
    /// an error, not a fallback: resolving it against our own catalog would name a group in
    /// the wrong zone.
    pub(super) fn away(&self, worker: &Worker, addr: GlobalAddr) -> Result<Option<u32>, Status> {
        if !self.foreign(worker, addr) {
            return Ok(None);
        }
        worker
            .config()
            .zone_of(addr.0)
            .ok_or(Status::Unmapped)
            .map(Some)
    }

    /// Routes into `zone` for `addr`, best-ranked gateway first, capped at [`GATEWAY_TRIES`].
    ///
    /// The ring is the same rendezvous hash the cache uses, so every sender picks the same
    /// order for one address without negotiating. Unlike the cache's ring this one
    /// *promotes*: a gateway we hold no link to is skipped and the next takes its place,
    /// sound because any gateway resolves any address of its zone.
    fn gateways(&self, worker: Rc<Worker>, zone: u32, addr: GlobalAddr) -> Vec<Route> {
        let cfg = worker.config();
        let mut out = Vec::new();
        for g in cfg.gateways_for(zone, addr.0) {
            if self.link_of(&worker, addr.universe(), g).is_none() {
                continue;
            }
            out.push(Route {
                worker: worker.clone(),
                universe: addr.universe(),
                node: g,
                via: Via::new(To::Owner, Hops::TWO),
            });
            if out.len() == GATEWAY_TRIES {
                break;
            }
        }
        out
    }

    /// A route into `zone`, through the best-ranked gateway we hold a link to. Two hops: one
    /// to reach a member of the group, one spare for a shard the far side is handing on.
    ///
    /// One shot, for paths that cannot retry: a relay owns no buffer it could send twice.
    /// Everything that can retry uses [`Self::via`].
    pub(super) fn toward(
        &self,
        worker: Rc<Worker>,
        zone: u32,
        addr: GlobalAddr,
    ) -> Result<Route, Status> {
        self.gateways(worker, zone, addr)
            .into_iter()
            .next()
            .ok_or(Status::Io)
    }

    /// Run `send` against `zone`'s gateways in ring order until one answers. A gateway that
    /// does not answer is not the zone's answer: the next is tried, and only a zone with
    /// nobody home is unavailable. Anything but a transport failure is the far zone's verdict
    /// and is returned as it stands.
    ///
    /// `once` gives up after the first gateway instead. [`Self::gateways`] has already
    /// skipped the ones we hold no link to, so every retry here follows a request that
    /// reached the wire and then timed out, which the far zone may well have applied. Work
    /// that would not survive being applied twice takes the single shot and reports the
    /// failure it cannot rule out.
    pub(super) async fn via<S, F, T>(
        &self,
        worker: Rc<Worker>,
        zone: u32,
        addr: GlobalAddr,
        once: bool,
        mut send: S,
    ) -> Result<T, Status>
    where
        S: FnMut(Route) -> F,
        F: Future<Output = Result<T, Status>>,
    {
        let mut routes = self.gateways(worker.clone(), zone, addr);
        if once {
            routes.truncate(1);
        }
        if routes.is_empty() {
            self.stat(&worker, |s| s.zones_unavailable += 1);
            return Err(Status::Io);
        }
        let last = routes.len() - 1;
        for (i, r) in routes.into_iter().enumerate() {
            match send(r).await {
                Err(Status::Io) if i < last => self.stat(&worker, |s| s.gateway_retries += 1),
                r => return r,
            }
        }
        self.stat(&worker, |s| s.zones_unavailable += 1);
        Err(Status::Io)
    }

    /// [`Self::via`] for a read: the sink cannot be moved into a closure, so the ring is
    /// walked here instead. A page the far zone refuses is the far zone's answer.
    pub(super) async fn pull_away(
        &self,
        worker: Rc<Worker>,
        zone: u32,
        addr: GlobalAddr,
        mut sink: Sink<'_>,
    ) -> Result<Register, Status> {
        let routes = self.gateways(worker.clone(), zone, addr);
        if routes.is_empty() {
            self.stat(&worker, |s| s.zones_unavailable += 1);
            return Err(Status::Io);
        }
        let last = routes.len() - 1;
        for (i, r) in routes.into_iter().enumerate() {
            match self
                .pull_from(worker.clone(), r, addr, sink.reborrow())
                .await
            {
                Err(Status::Io) if i < last => self.stat(&worker, |s| s.gateway_retries += 1),
                r => return r,
            }
        }
        self.stat(&worker, |s| s.zones_unavailable += 1);
        Err(Status::Io)
    }

    /// How many acceptors must apply a value for it to be chosen: a majority of the three,
    /// unconditionally. Reachability does not enter into it; a quorum that shrank to what we
    /// could see would let an isolated node decide alone.
    ///
    /// Exception: a universe naming no peers at all is a single-node deployment, so a local
    /// accept is a decision. Per universe, since one lone node says nothing about another.
    pub(super) fn quorum(&self, worker: &Worker, universe: u32) -> usize {
        if worker.compiled().has_links(universe) {
            2
        } else {
            1
        }
    }

    /// The page reference naming `addr`. Only the block offset goes on the wire: the
    /// universe is the namespace we are about to send on, and the extent is the control
    /// plane's business.
    pub(super) fn page_ref(&self, addr: GlobalAddr) -> Result<PageRef, Status> {
        PageRef::new(addr.lba()).ok_or(Status::Unmapped)
    }

    pub(super) fn stat(&self, worker: &Worker, f: impl FnOnce(&mut Stats)) {
        here(worker, |l| f(&mut l.stats));
    }

    pub fn local_stats(&self, worker: &Worker) -> Stats {
        here(worker, |l| l.stats)
    }
}
