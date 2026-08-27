use super::*;

impl Paxos {
    // --- warming ---

    /// Tell every zone this extent asks to keep warm that `addr` has a new value.
    ///
    /// Called from the proposer once the round is decided, so nothing on the write path
    /// waits: the fan-out is a detached task and its failures are counted, not returned. A
    /// zone that hears nothing reads across the fabric as it always did, so every part of
    /// this is droppable.
    ///
    /// Declines under pressure: a warm arriving into a full store would evict something
    /// demand actually asked for.
    pub(super) fn fan_warm(&'static self, worker: Rc<Worker>, addr: GlobalAddr, version: u64) {
        let zones: Vec<u32> = {
            let cfg = worker.config();
            let z = cfg.warm_zones_of(addr.0);
            if z.is_empty() {
                return;
            }
            z.to_vec()
        };
        if self.cache.shedding(&worker) {
            self.stat(&worker, |s| s.warms_dropped += zones.len() as u64);
            return;
        }
        let task_worker = worker.clone();
        let spawned = runtime::spawn_local(async move {
            let worker = task_worker;
            for z in zones {
                let sent = self
                    .via(worker.clone(), z, addr, false, |r| {
                        self.send_warm(r, addr, version, fabric::Stage::Inbound)
                    })
                    .await;
                self.stat(&worker, |s| match sent {
                    Ok(()) => s.warms_sent += 1,
                    Err(_) => s.warms_dropped += 1,
                });
            }
        });
        if !spawned {
            self.stat(&worker, |s| s.warms_dropped += 1);
        }
    }

    async fn send_warm(
        &self,
        route: Route,
        addr: GlobalAddr,
        version: u64,
        stage: fabric::Stage,
    ) -> Result<(), Status> {
        let page = self.page_ref(addr)?;
        let mut t = PoolBuf::alloc(fabric::BLOCK).await;
        fabric::WarmReq { version, stage }
            .encode(&mut t)
            .map_err(Status::from_wire)?;
        // Sent on the link rather than through the route: a `WARM` is never forwarded, so
        // it carries neither an addressee nor a hop budget.
        route
            .link()
            .send(Cmd::Warm { page }, t.buf())
            .await
            .map_err(Status::from_wire)
    }

    /// A `WARM` arriving here, from either stage.
    ///
    /// Answers as soon as the frame is understood: the work it starts is detached, so the
    /// sender's command does not stay open for a fan-out or for the cross-zone read that
    /// follows at the holder. Every refusal is `Ok`, since declining to warm is not an error
    /// the sender could act on.
    ///
    /// Our own configuration decides, not the sender's: an extent that does not name this
    /// zone, or that vetoes caching here, is dropped whatever arrived.
    pub async fn warm(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        version: u64,
        stage: fabric::Stage,
    ) {
        let (wanted, me, universe) = {
            let cfg = worker.config();
            (
                cfg.warmed_here(addr.0) && cfg.cache_admit_of(addr.0) != 0,
                cfg.node.id,
                addr.universe(),
            )
        };
        if !wanted || self.cache.shedding(&worker) {
            self.stat(&worker, |s| s.warms_dropped += 1);
            return;
        }
        if stage != fabric::Stage::Inbound {
            self.take_warm(worker, addr, version);
            return;
        }
        // A gateway holds its whole zone's catalog, so it can name the rendezvous winner of
        // every cohort column. That is why the fan-out is two stages: the writing zone knows
        // nothing about this zone's catalog, and each of the three copies must go where a
        // reader of that cohort will look for it.
        let mut winners = [0u32; 3];
        {
            let cfg = worker.config();
            let Some(u) = cfg.universe(universe) else {
                self.stat(&worker, |s| s.warms_dropped += 1);
                return;
            };
            for (c, w) in winners.iter_mut().enumerate() {
                *w = u.cohort_winner(addr.0, c).unwrap_or(0);
            }
        }
        self.stat(&worker, |s| s.warms_taken += 1);
        for (i, n) in winners.into_iter().enumerate() {
            if n == 0 || winners[..i].contains(&n) {
                continue;
            }
            if n == me {
                self.take_warm(worker.clone(), addr, version);
                continue;
            }
            // Intra-zone and addressed to a node rather than a member, so no hop budget and
            // no `imm`: the holder is not a member of the page's group, and a frame it could
            // forward would go to the wrong place.
            if self.link_of(&worker, universe, n).is_none() {
                self.stat(&worker, |s| s.warms_dropped += 1);
                continue;
            }
            let route = Route {
                worker: worker.clone(),
                universe,
                node: n,
                via: Via::direct(To::Owner),
            };
            if self
                .send_warm(route, addr, version, fabric::Stage::Holder)
                .await
                .is_err()
            {
                self.stat(&worker, |s| s.warms_dropped += 1);
            }
        }
    }

    /// Pull `addr` across the fabric and put it in our cache, detached.
    ///
    /// Width one, and no demand estimate: the gateway sent this frame because we are the
    /// rendezvous winner of our cohort column for this address, so `holds` is true by
    /// construction and the extent's admission threshold has nothing to measure yet. The
    /// zero veto is still honoured, in `claim_here`.
    fn take_warm(&'static self, worker: Rc<Worker>, addr: GlobalAddr, version: u64) {
        let task_worker = worker.clone();
        if !runtime::spawn_local(async move {
            if self
                .pull_warm(task_worker.clone(), addr, version)
                .await
                .is_none()
            {
                self.stat(&task_worker, |s| s.warms_dropped += 1);
            }
        }) {
            self.stat(&worker, |s| s.warms_dropped += 1);
        }
    }

    async fn pull_warm(
        &'static self,
        worker: Rc<Worker>,
        addr: GlobalAddr,
        version: u64,
    ) -> Option<()> {
        let zone = self.away(&worker, addr).ok().flatten()?;
        // Already here. A warmed extent is immutable, so a cached copy at this version is
        // the value and there is nothing to replace; a repeated warm (a retried write, or
        // two gateways both relaying) costs one check.
        if self
            .cache
            .peek_immutable(&worker, addr)
            .await
            .is_some_and(|r| r.version >= version)
        {
            return Some(());
        }
        let mut buf = PoolBuf::alloc(fabric::BLOCK).await;
        let r = self
            .pull_away(worker.clone(), zone, addr, Sink::Small(&mut buf))
            .await
            .ok()?;
        self.cache.admit(&worker, addr, buf.buf(), r, 1).await;
        // We cached whatever the group agreed on, which may be newer than the version we
        // were told about if the warm raced a later write. The cache filters on the extent's
        // live version at every read, so an entry that is not the value is not served.
        self.stat(&worker, |s| s.warms_taken += 1);
        Some(())
    }
}
