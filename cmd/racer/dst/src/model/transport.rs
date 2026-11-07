//! Message hops, relays, links, connections and their failures.

use super::*;

impl<'s> Model<'s> {
    /// `msg` reached its holder (production `peer/server.rs` verifies, then
    /// `decide_hop` serves or relays).
    pub(super) fn hop(&mut self, mid: u64, verified: bool) {
        let now = self.now();
        let Some(m) = self.msgs.get(&mid) else {
            return;
        };
        let at = m.at;
        if self.nodes[at as usize].down {
            // A crash fails the connection: link health, not silence.
            self.report.counters.messages_lost += 1;
            self.last_hops.remove(&mid);
            self.drop_msg(mid);
            return;
        }
        if self.hung(at) {
            // Accepted, never answered or forwarded; the requester's attempt times out.
            self.report.counters.messages_lost += 1;
            self.lose_msg(mid);
            return;
        }
        if now >= m.abandon {
            // The requester gave up: relays and the destination see the upstream connection
            // close and drop the exchange.
            self.report.counters.requests_abandoned += 1;
            self.drop_msg(mid);
            return;
        }
        if !verified {
            // Production verifies every request's signature on the crypto helper
            // (`peer/server.rs` `verify_request_async`) before it dispatches or relays it; the
            // destination's worker then takes it.
            // The helper is the one paired with the worker holding the connection
            // (`src/worker.rs` `WorkerPair`).
            let costs = self.fabric.costs;
            let conn = self.conn_worker(mid);
            let m = &self.msgs[&mid];
            let bound = self.spec.crypto_queue;
            if bound != 0 && self.fabric.crypto_outstanding(now, at, conn) >= bound {
                // The crypto submission fails `Overloaded` before any key work.
                self.report.counters.verify_sheds += 1;
                // The unsigned overload head ends the last link's exchange cleanly
                // (`peer/server.rs` `serve_connection` returns the connection for reuse).
                if let Some(id) = self.msg_conns.get_mut(&mid).and_then(Vec::pop) {
                    self.queued.remove(&id);
                    self.conns.finish(now, id);
                }
                // The last link's sender sees the connection fail or an unsigned overload at
                // once: not silence.
                self.last_hops.remove(&mid);
                let m = self.msgs.get_mut(&mid).expect("message");
                let previous = m.path.pop().expect("a request path");
                if m.path.is_empty() {
                    m.path.push(previous);
                }
                // Production answers the unsigned overload head (`peer/server.rs`); the previous
                // holder reads it as `Overloaded` (`peer/transport.rs`).
                let fault = match unverified_failure(Fault::Overloaded) {
                    UnverifiedFailure::Overloaded => Fault::Overloaded,
                    UnverifiedFailure::Close => Fault::Io,
                };
                return self.fail_hop(mid, previous, fault);
            }
            let ready = self
                .fabric
                .crypto(now, at, conn, costs.verify(m.path.len()));
            // The destination's worker takes the request once it is verified, not before: a
            // reservation at a future start would hold the worker idle until then (`reserve` is
            // one FIFO server), delaying everything it signs or searches meanwhile.
            let next = if at == m.dest {
                Ev::Verified { msg: mid }
            } else {
                Ev::Hop {
                    msg: mid,
                    verified: true,
                }
            };
            self.engine.at(ready, next);
            return;
        }
        self.trace.record(now, TAG_EVENT, 3, u64::from(at));
        let known = self.versions.borrow().known(at, m.route.version);
        match decide_hop(&at, &m.path, &m.dest, m.route.links, known) {
            Hop::StaleMembership => {
                // Production `peer/server.rs`: a node that does not resolve the request's
                // version answers `StaleMembership`, signed.
                self.report.counters.stale_membership_replies += 1;
                let asker = self.asker(mid, now);
                self.reply(&asker, at, ResponseKind::StaleMembership);
            }
            Hop::Serve if m.ping.is_some() => {
                // Production mode `ping` is answered `Miss` at once, without looking at the
                // cache or flights.
                let asker = self.asker(mid, now);
                self.reply(&asker, at, ResponseKind::Miss);
            }
            Hop::Serve => {
                let (page, route) = (m.page, m.route);
                let asker = self.asker(mid, now);
                self.serve(asker, at, page, route);
            }
            Hop::Relay(plan) => {
                if plan.outbound == Err(RouteFault::Invalid) {
                    self.drop_msg(mid);
                    return self.violation(format!("relay {at} received a route revisiting it"));
                }
                // Production `peer/server.rs` reserves one `ResourceClass::Relay` transfer on the
                // relay's worker before it forwards; none free is `Overloaded`.
                let worker = (mid % u64::from(self.spec.workers.max(1))) as u8;
                let active = self.relays_active.entry((at, worker)).or_default();
                if *active >= self.spec.relay_transfers {
                    self.report.counters.relay_sheds += 1;
                    if self.diag {
                        let m = &self.msgs[&mid];
                        let kind = if m.ping.is_some() { "ping" } else { "probe" };
                        let key = format!("relay_shed:{kind}:hop{}", m.path.len());
                        *self.report.attempt_errors.entry(key).or_default() += 1;
                    }
                    return self.fail_hop(mid, at, Fault::Overloaded);
                }
                *active += 1;
                self.relay_charges
                    .entry(mid)
                    .or_default()
                    .push((at, worker, now));
                self.report.counters.relays += 1;
                self.forward(mid, at, Some(plan));
            }
        }
    }

    /// Drop `mid` without an answer: its relays see the exchange end.
    pub(super) fn drop_msg(&mut self, mid: u64) {
        self.release_relays(mid);
        self.msgs.remove(&mid);
    }

    /// Drop `mid` as [`Self::drop_msg`], but its connections stay open until the requester
    /// gives up ([`Ev::Abandon`]): nobody answers or closes them before.
    pub(super) fn lose_msg(&mut self, mid: u64) {
        let held = self.msg_conns.remove(&mid);
        self.drop_msg(mid);
        if let Some(held) = held {
            self.msg_conns.insert(mid, held);
        }
    }

    /// Close every connection `mid` holds (a failed or abandoned exchange is never reused,
    /// `src/peer/transport.rs`).
    pub(super) fn close_conns(&mut self, mid: u64) {
        for id in self.msg_conns.remove(&mid).unwrap_or_default() {
            if self.queued.remove(&id).is_some() {
                self.report.counters.conn_queue_abandoned += 1;
            }
            self.conns.close(id);
        }
    }

    /// `mid` was answered: its connections go idle for reuse.
    pub(super) fn finish_conns(&mut self, mid: u64) {
        let now = self.now();
        for id in self.msg_conns.remove(&mid).unwrap_or_default() {
            self.queued.remove(&id);
            self.conns.finish(now, id);
        }
    }

    /// Start the hops whose connections an ingress slot just accepted.
    pub(super) fn admit_queued(&mut self) {
        let now = self.now();
        for id in self.conns.take_admitted() {
            if let Some((mid, arrive)) = self.queued.remove(&id) {
                let hop = Ev::Hop {
                    msg: mid,
                    verified: false,
                };
                self.engine.at(arrive.max(now), hop);
            }
        }
    }

    /// The relays of `mid` release their `Relay` transfers.
    pub(super) fn release_relays(&mut self, mid: u64) {
        self.release_relays_after(mid, false);
    }

    /// As [`Self::release_relays`]; `body` labels the hold (`MODEL_DIAG`) as a page body's.
    pub(super) fn release_relays_after(&mut self, mid: u64, body: bool) {
        let now = self.now();
        let kind = if self.diag {
            match self.msgs.get(&mid) {
                Some(m) if m.ping.is_some() => "ping",
                Some(_) => "unanswered",
                None if body => "page",
                None => "answered",
            }
        } else {
            ""
        };
        // Production keeps each connection until the answer is written upstream; an exchange
        // that ends without one closes its connections.
        if self.answered_msgs.remove(&mid) {
            self.finish_conns(mid);
        } else {
            self.close_conns(mid);
        }
        for (node, worker, start) in self.relay_charges.remove(&mid).unwrap_or_default() {
            if self.diag {
                // Log2 buckets of the hold in ms.
                let ms = (now - start) / MILLIS;
                let bucket = if ms == 0 { 0 } else { 1u64 << ms.ilog2() };
                let key = format!("relay_hold:{kind}:{bucket}ms");
                *self.report.attempt_errors.entry(key).or_default() += 1;
            }
            let key = (node, worker);
            let active = self.relays_active.get_mut(&key).expect("a charged relay");
            *active -= 1;
            if *active == 0 {
                self.relays_active.remove(&key);
            }
        }
    }

    /// The relays of `mid` release their transfers at `at` (production keeps the reservation
    /// on the connection while the body streams, `peer/server.rs` `relay.reservation`).
    pub(super) fn release_relays_at(&mut self, mid: u64, at: Nanos) {
        if at <= self.now() {
            self.release_relays_after(mid, true);
        } else if self.relay_charges.contains_key(&mid) || self.msg_conns.contains_key(&mid) {
            self.engine.at(at, Ev::Release { msg: mid });
        } else {
            self.answered_msgs.remove(&mid);
        }
    }

    /// Take `mid` off its route: whoever answers it now answers its requester.
    pub(super) fn asker(&mut self, mid: u64, now: Nanos) -> Asker {
        let m = self.msgs.remove(&mid).expect("message");
        Asker {
            from: m.from,
            worker: m.worker,
            read: m.read,
            id: m.id,
            at: now,
            abandon: m.abandon,
            path: m.path,
            dest: m.dest,
            msg: mid,
            version: m.route.version,
            attempts: m.route.attempts,
            links: m.route.charged,
            ping: m.ping,
        }
    }

    /// Route `msg` one link from `holder` (production `Paths::shortest`
    /// with `LinkHealth` blocks and `Paths::detour`). `plan` is the relay plan at a relay.
    pub(super) fn forward(&mut self, mid: u64, holder: u32, plan: Option<RelayPlan>) {
        let now = self.now();
        let at = self.instant(now);
        let m = &self.msgs[&mid];
        let dest = m.dest;
        // Production `shortest_probe_async`: a requester's own ping may use the link to the
        // destination it holds silent, and always detours past the primary relay.
        let own_ping = m.ping.is_some() && holder == m.from;
        if !own_ping && self.nodes[holder as usize].silence.silent(&dest, at) {
            // The destination is a silent next hop of this node.
            return self.fail_hop(mid, holder, Fault::Unavailable);
        }
        let search = match plan {
            None => search_budget(&m.path, m.route.links, &holder),
            Some(plan) => plan.search,
        };
        let Ok((keep, links)) = search else {
            self.note_cut(mid, Cut::Route);
            return self.fail_hop(mid, holder, Fault::HopBudgetExhausted);
        };
        let Some(members) = self
            .versions
            .borrow()
            .get(m.route.version)
            .map(|v| v.members.clone())
        else {
            // The request's version is no longer resolvable here.
            return self.fail_hop(mid, holder, Fault::IncompatibleMembership);
        };
        let position = |n: u32| members.position(&n.to_be_bytes());
        let (Some(from), Some(to)) = (position(holder), position(dest)) else {
            return self.fail_hop(mid, holder, Fault::Unavailable);
        };
        let Some(visited) = m.path[..keep]
            .iter()
            .map(|n| position(*n))
            .collect::<Option<Vec<usize>>>()
        else {
            return self.fail_hop(mid, holder, Fault::Unavailable);
        };
        let node = &self.nodes[holder as usize];
        let mut blocked: Vec<usize> = node
            .silence
            .silent_peers(at)
            .filter(|p| !(own_ping && **p == dest))
            .filter_map(|p| position(*p))
            .filter(|p| members.is_neighbor(from, *p))
            .collect();
        // Production `Paths::blocked` (`src/topology.rs`): neighbors whose circuit is open, on
        // the worker that searches.
        if let Some(links) = self.links.get(&(holder, self.link_worker(mid, holder))) {
            blocked.extend(
                links
                    .failed
                    .iter()
                    .filter(|p| !links.circuits.available(p, at))
                    .filter_map(|p| position(*p))
                    .filter(|p| members.is_neighbor(from, *p)),
            );
        }
        blocked.sort_unstable();
        blocked.dedup();
        let mut level = node.detours.level(&dest, at);
        if own_ping {
            level = level.max(1);
        }
        blocked.extend(members.detour(from, to, level));
        let seed = mid.to_be_bytes();
        let query = PathQuery {
            from,
            to,
            links,
            visited: &visited,
            blocked: &blocked,
            seed: &seed,
        };
        let worker = (mid % u64::from(self.spec.workers.max(1))) as u8;
        // Production (`src/app.rs` `Paths::with_limits(..).with_queue(DEFAULT_SEARCH_QUEUE)`)
        // runs at most `RACER_ACTIVE_PATH_SEARCHES_PER_WORKER` (8) searches per worker and lets
        // `topology::DEFAULT_SEARCH_QUEUE` (64) more wait in arrival order; past those a search
        // fails `Overloaded`. A slot is held until the search's CPU work is done.
        let start = {
            let ends = self.searches.entry((holder, worker)).or_default();
            ends.retain(|end| *end > now);
            search_start(ends, now)
        };
        let (route, polls) = self.route(&members, query, || start.is_some());

        // A cache hit answers on the first poll, each further poll expands
        // up to 32 nodes, on one worker's CPU. A queued search starts when a slot frees.
        let costs = self.fabric.costs;
        let cost = polls.saturating_sub(1) * 32 * costs.cpu_path_expand_ns_per_node;
        let ready = if polls > 1 {
            let start = start.unwrap_or(now);
            self.fabric.cpu_from(now, start, holder, worker, cost)
        } else {
            self.fabric.cpu(now, holder, worker, cost)
        };
        if polls > 1 && route != Err(topology::Error::Overloaded) {
            self.searches
                .entry((holder, worker))
                .or_default()
                .push(ready);
        }
        // A search shed on its first poll counts too: the route was not cached and every slot
        // was busy (it was left out before, hiding most sheds).
        if route == Err(topology::Error::Overloaded) {
            self.report.counters.search_sheds += 1;
        }
        let route = match route {
            Ok(route) => route,
            Err(topology::Error::Overloaded) => {
                return self.fail_hop(mid, holder, Fault::Overloaded);
            }
            Err(_) => {
                self.report.counters.route_unreachable += 1;
                self.note_cut(mid, Cut::Route);
                return self.fail_hop(mid, holder, Fault::Unavailable);
            }
        };
        let next = u32::from_be_bytes(members.members()[route[1]].0);
        self.link(mid, holder, plan, next, ready);
    }

    /// Run one route search to completion; returns the route and the polls it took. A cache
    /// hit answers on the first poll; a search that has to run needs one of the holder worker's
    /// search slots (`admit`), or fails `Overloaded`.
    pub(super) fn route(
        &self,
        members: &Members,
        query: PathQuery<'_>,
        admit: impl FnOnce() -> bool,
    ) -> (Result<Vec<usize>, topology::Error>, u64) {
        let mut future = std::pin::pin!(self.paths.route(members, query));
        let mut cx = std::task::Context::from_waker(std::task::Waker::noop());
        let mut admit = Some(admit);
        let mut polls = 0;
        loop {
            polls += 1;
            if let std::task::Poll::Ready(route) = future.as_mut().poll(&mut cx) {
                return (route, polls);
            }
            if admit.take().is_some_and(|admit| !admit()) {
                // Dropping the future cancels the search.
                return (Err(topology::Error::Overloaded), polls);
            }
            // Searches run one at a time, so a search never waits on another.
            assert!(polls < 1_000, "a route search did not finish");
        }
    }

    /// Send `msg` from `holder` over its link to `next` once `ready`.
    pub(super) fn link(
        &mut self,
        mid: u64,
        holder: u32,
        plan: Option<RelayPlan>,
        next: u32,
        ready: Nanos,
    ) {
        let now = self.now();
        if plan.is_some_and(|plan| plan.outbound.is_err()) {
            self.note_cut(mid, Cut::Route);
            return self.fail_hop(mid, holder, Fault::HopBudgetExhausted);
        }
        let conn = self.link_worker(mid, holder);
        // Production `request_network` acquires the link's circuit before the exchange
        // (`health.acquire(&next)?`, `src/peer.rs`); an open circuit fails it `Unavailable`.
        if self.spec.circuits {
            let at = self.instant(now);
            if let Some(links) = self.links.get(&(holder, conn))
                && links.circuits.acquire(&next, at).is_err()
            {
                self.report.counters.link_circuit_open += 1;
                self.note_cut(mid, Cut::Circuit);
                return self.fail_hop(mid, holder, Fault::Unavailable);
            }
        }
        let cut = self.cut(holder, next);
        if cut {
            let why = if self.nodes[next as usize].down {
                // A peer chose a crashed member: how long after
                // the crash.
                let age = (now - self.nodes[next as usize].down_at) / SECONDS;
                let c = &mut self.report.counters;
                c.dead_member_links += 1;
                c.dead_member_link_age_max_s = c.dead_member_link_age_max_s.max(age);
                Cut::Down
            } else {
                Cut::Partition
            };
            self.note_cut(mid, why);
        }
        // The sender's pool hands out a connection before any I/O (`racer_core::connections`).
        let acquired = (!cut && self.spec.connections != PeerConnections::Off)
            .then(|| self.conns.acquire(now, holder, conn, next));
        match acquired {
            // A refusal is the receiver's unsigned overload head, read as `Overloaded` too.
            Some(Acquire::Overloaded | Acquire::Refused) => {
                return self.fail_hop(mid, holder, Fault::Overloaded);
            }
            Some(
                Acquire::Reused(id)
                | Acquire::Accepted(id)
                | Acquire::Queued(id)
                | Acquire::Dropped(id),
            ) => {
                let held = self.msg_conns.entry(mid).or_default();
                if held.is_empty() {
                    let abandon = self.msgs[&mid].abandon.max(now + 1);
                    self.timeouts_queued += 1;
                    self.engine.at(abandon, Ev::Abandon { msg: mid });
                }
                held.push(id);
            }
            None => {}
        }
        let m = self.msgs.get_mut(&mid).expect("message");
        if let Some(plan) = plan {
            m.path.push(holder);
            m.route.links = plan.outbound.expect("checked above");
        }
        m.at = next;
        if cut {
            self.report.counters.link_failures += 1;
            let failed = Ev::LinkDown {
                msg: mid,
                holder,
                worker: conn,
                next,
            };
            return self.engine.at(now + CONNECT_TIMEOUT, failed);
        }
        // The link connected: production records `LinkOutcome::Success` once the exchange
        // completes (`route::exchange_evidence`); the Model records it on connect.
        if let Some(links) = self.links.get_mut(&(holder, conn))
            && links.failed.remove(&next)
        {
            links.circuits.success(&next);
        }
        let m = self.msgs.get_mut(&mid).expect("message");
        if next == m.dest {
            // The last link: this sender learns whether the destination answers.
            self.last_hops.insert(mid, (holder, next, m.page, now));
            let abandon = m.abandon.max(now + 1);
            self.timeouts_queued += 1;
            self.engine.at(abandon, Ev::LastHop { msg: mid });
        }
        // Production signs inline on the I/O worker (`src/peer.rs` `sign_request_to`,
        // `append_request`); only verification goes to the crypto helper.
        let proofs = self.msgs[&mid].path.len();
        let cost = self.fabric.costs.sign_request(proofs);
        let signed = self.fabric.cpu_from(now, ready, holder, conn, cost);
        let arrive = self.fabric.message(signed, holder, next);
        match acquired {
            Some(Acquire::Queued(id)) => {
                // Production stops accepting with every ingress slot held; the connection waits
                // in the kernel accept queue (`peer/server.rs` `listen`).
                self.queued.insert(id, (mid, arrive));
            }
            Some(Acquire::Dropped(_)) => {
                // A full accept queue drops the handshake: the sender hears nothing and its
                // attempt times out.
                self.report.counters.messages_lost += 1;
                self.lose_msg(mid);
            }
            _ => self.engine.at(
                arrive,
                Ev::Hop {
                    msg: mid,
                    verified: false,
                },
            ),
        }
    }

    /// Forwarding `msg` from `holder` failed with `fault` (production
    /// `relay_failure`).
    pub(super) fn fail_hop(&mut self, mid: u64, holder: u32, fault: Fault) {
        let Some(m) = self.msgs.remove(&mid) else {
            return;
        };
        let asker = Asker {
            from: m.from,
            worker: m.worker,
            read: m.read,
            id: m.id,
            at: self.now(),
            abandon: m.abandon,
            path: m.path,
            dest: m.dest,
            msg: mid,
            version: m.route.version,
            attempts: m.route.attempts,
            links: m.route.charged,
            ping: m.ping,
        };
        if holder == asker.from && asker.path.len() == 1 {
            // The requester's own send failed: a transport error. A shed route search fails as
            // production `request_network` (`src/peer.rs`) does, with `Overloaded`, which is no
            // evidence against the destination.
            if let Some(ping) = asker.ping {
                let now = self.now();
                return self.end_ping(ping, now, Err(false));
            }
            let error = if fault == Fault::Overloaded {
                Error::Overloaded
            } else {
                Error::Unavailable
            };
            let failed = Input::Replied {
                id: asker.id,
                reply: Reply::Failed {
                    error,
                    sent: true,
                    scope_live: true,
                },
                answer: None,
            };
            let input = Ev::Input {
                node: asker.from,
                worker: asker.worker,
                read: asker.read,
                input: failed,
            };
            return self.engine.at(self.now(), input);
        }
        self.report.counters.relay_failures += 1;
        match relay_failure(fault, asker.ping.is_some()) {
            RelayFailure::Respond(r) => self.reply(&asker, holder, response_kind(r)),
            RelayFailure::RespondIfLive(r) if self.now() < m.route.deadline => {
                self.reply(&asker, holder, response_kind(r));
            }
            RelayFailure::RespondIfLive(_) => self.release_relays(mid),
            RelayFailure::Propagate => self.fail_probe(&asker, holder),
        }
    }

    /// The route back from `from` to `asker`: `from`, then the request's path reversed.
    pub(super) fn back(asker: &Asker, from: u32) -> Vec<u32> {
        let mut route = Vec::with_capacity(asker.path.len() + 1);
        route.push(from);
        route.extend(asker.path.iter().rev().copied());
        route
    }

    /// `from` answers `asker`, ready at `ready`: an answer from the destination before the
    /// requester gave up clears its last link. Returns whether the
    /// answer is the destination's own (signed by it).
    pub(super) fn answering(&mut self, asker: &Asker, from: u32, ready: Nanos) -> bool {
        let own = from == asker.dest;
        if own
            && ready < asker.abandon
            && let Some((sender, dest, ..)) = self.last_hops.remove(&asker.msg)
        {
            let at = self.instant(self.now());
            self.nodes[sender as usize].silence.answered(&dest, at);
        }
        own
    }

    /// Whether every link of `route` is up (a reply over a cut link is
    /// lost and the requester times out).
    pub(super) fn alive(&mut self, route: &[u32]) -> bool {
        let alive = !self.nodes[route[0] as usize].down
            && route.windows(2).all(|hop| !self.cut(hop[0], hop[1]));
        if !alive {
            self.report.counters.replies_lost += 1;
        }
        alive
    }

    /// A message from `asker`'s replier back over `route`, leaving at `ready`.
    pub(super) fn message_back(&self, ready: Nanos, route: &[u32]) -> Nanos {
        route
            .windows(2)
            .fold(ready, |t, hop| self.fabric.message(t, hop[0], hop[1]))
    }

    /// `node` signs a peer response on the I/O worker holding `mid`'s connection, inline as
    /// production does (`Forwarding::sign_response` from `peer/server.rs`); returns when the
    /// signature is ready.
    pub(super) fn sign(&mut self, node: u32, mid: u64) -> Nanos {
        let now = self.now();
        let conn = self.conn_worker(mid);
        self.fabric
            .cpu(now, node, conn, self.fabric.costs.crypto_sign_response_ns)
    }

    /// The worker of a node that holds request `mid`'s connection, as the Model spreads peer
    /// connections over workers (`probe % workers`). An inference: production
    /// assigns accepted connections to workers by its listener, not by request.
    /// Record acquisition `read` when first seen; every so often forget ended ones.
    pub(super) fn track_acquisition(&mut self, node: u32, worker: u32, read: ReadId) {
        if self.acquisitions.contains_key(&(node, worker, read)) {
            return;
        }
        let now = self.now();
        self.acquisitions
            .insert((node, worker, read), (now, Vec::new()));
        self.acquisitions_added += 1;
        if self.acquisitions_added >= 4096.max(self.acquisitions.len()) {
            self.acquisitions_added = 0;
            let nodes = &self.nodes;
            self.acquisitions
                .retain(|(n, w, r), _| nodes[*n as usize].cores[*w as usize].read(*r).is_some());
        }
    }

    /// Walk probe of acquisition `read` to `to` ended as `how`.
    pub(super) fn asked(&mut self, node: u32, worker: u32, read: ReadId, to: u32, how: Asked) {
        if let Some((_, asked)) = self.acquisitions.get_mut(&(node, worker, read)) {
            asked.push((to, how));
        }
    }

    /// Note why the walk probe `mid` failed in transport.
    pub(super) fn note_cut(&mut self, mid: u64, cut: Cut) {
        if let Some(m) = self.msgs.get(&mid)
            && m.ping.is_none()
        {
            self.cuts.insert((m.from, m.worker, m.id), cut);
        }
    }

    /// Classify an origin request of acquisition `read` for `page`, ending at `done` with
    /// `outcome`.
    pub(super) fn classify_origin(
        &mut self,
        node: u32,
        worker: u32,
        read: ReadId,
        page: Page,
        done: Nanos,
        outcome: OriginOutcome,
    ) {
        let now = self.now();
        let Some(fill) = self.nodes[node as usize].cores[worker as usize]
            .read(read)
            .map(|r| r.fill())
        else {
            return;
        };
        let rank0 = fill.owners.first().copied().unwrap_or(node);
        let rank = fill.rank;
        let (started, asked) = self
            .acquisitions
            .get(&(node, worker, read))
            .map_or((now, &[][..]), |(s, a)| (*s, &a[..]));
        let request = repeats::Request {
            now,
            node,
            page,
            rank0,
            rank,
            started,
            asked,
            done,
            ok: outcome == OriginOutcome::Ok,
        };
        let nodes = &self.nodes;
        let held = |n: u32| {
            let holder = &nodes[n as usize];
            !holder.down && holder.stored.contains(&page)
        };
        // An unexplained repeat is counted (`origin_per_page.repeats.unexplained`), not a hard
        // invariant: `model-range-readers` refetches pages a node holds through its range windows
        // (see `dst/MODEL.md`). The combined failover test asserts it is zero.
        if let Some(why) = self.report.repeats.origin(&request, held)
            && self.diag
        {
            *self.report.attempt_errors.entry(why).or_default() += 1;
        }
    }

    /// The worker whose link health a send from `holder` uses: the requester's read worker,
    /// or the connection's worker at a relay.
    pub(super) fn link_worker(&self, mid: u64, holder: u32) -> u8 {
        match self.msgs.get(&mid) {
            Some(m) if holder == m.from => m.worker as u8,
            _ => self.conn_worker(mid),
        }
    }

    /// The link `holder` to `next` failed to connect on `worker`: open or extend its circuit.
    pub(super) fn link_failed(&mut self, holder: u32, worker: u8, next: u32) {
        if !self.spec.circuits {
            return;
        }
        let at = self.instant(self.now());
        let links = self
            .links
            .entry((holder, worker))
            .or_insert_with(Links::new);
        if links.circuits.failure(&next, at, link_backoff).is_ok() {
            links.failed.insert(next);
        }
    }

    pub(super) fn conn_worker(&self, mid: u64) -> u8 {
        (mid % u64::from(self.spec.workers.max(1))) as u8
    }
}
