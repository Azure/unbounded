//! Effect dispatch and completion, client failures and the end-of-run checks.

use super::*;

impl<'s> Model<'s> {
    /// Complete each effect on the cost model. Dispatch only: the answer is a fact of the
    /// world, never a choice of where to go next.
    pub(super) fn dispatch(&mut self, node: u32, worker: u32, effects: Vec<Effect<ModelTypes>>) {
        for effect in effects {
            self.cover(effect_name(&effect));
            if let Some(id) = effect.id()
                && !self.answered.insert((node, worker, id))
            {
                self.violation(format!("node {node} worker {worker} reused effect {id:?}"));
            }
            if let Effect::Acquisition { read, .. } = &effect {
                self.track_acquisition(node, worker, *read);
            }
            self.complete(node, worker, effect);
        }
    }

    pub(super) fn complete(&mut self, node: u32, worker: u32, effect: Effect<ModelTypes>) {
        use acquisition::Io as Acq;
        let now = self.now();
        let costs = self.fabric.costs;
        let input = |read, input| Ev::Input {
            node,
            worker,
            read,
            input,
        };
        match effect {
            Effect::Acquisition { read, page, effect } => match effect {
                Acq::Rank { id, basis, .. } => {
                    let chunks = self.ranking_chunks(node, worker, page, &basis);
                    let at = self.fabric.cpu(
                        now,
                        node,
                        worker as u8,
                        chunks * costs.cpu_ranking_chunk_ns,
                    );
                    let result = self.rank(node, page, basis);
                    self.engine
                        .at(at, input(read, Input::Ranked { id, result }));
                }
                Acq::Hints { id, .. } => {
                    let answer = Input::Hints {
                        id,
                        hints: Vec::new(),
                        rank0: Default::default(),
                    };
                    self.engine
                        .at(now + costs.cpu_request_ns, input(read, answer));
                }
                Acq::Hedge { id, .. } => {
                    let answer = Input::Hedged {
                        id,
                        end: HedgeEnd::NotStarted,
                        copy: false,
                    };
                    self.engine.at(now, input(read, answer));
                }
                Acq::Refresh { id, version } => {
                    // Refresh: the newest version this node adopted, and
                    // version - 1 while it is recent and lists this node.
                    let versions = self.versions.borrow();
                    let latest = versions.adopted(node).number;
                    let recent_previous = versions
                        .previous_recent_member(node, version)
                        .then(|| version - 1);
                    drop(versions);
                    let answer = Input::Membership {
                        id,
                        latest: Ok(Some((latest, latest))),
                        recent_previous,
                    };
                    self.engine
                        .at(now + costs.cpu_request_ns, input(read, answer));
                }
                Acq::Send {
                    id,
                    probe,
                    dest,
                    version,
                } => {
                    self.report.counters.peer_probes += 1;
                    let Some(to) = dest.filter(|d| (*d as usize) < self.nodes.len()) else {
                        self.probes
                            .insert((node, worker, id), (node, page, false, now));
                        let failed = Input::Replied {
                            id,
                            reply: Reply::Failed {
                                error: Error::Unavailable,
                                sent: false,
                                scope_live: true,
                            },
                            answer: None,
                        };
                        self.engine.at(now, input(read, failed));
                        return;
                    };
                    // Production `request_traced`: reserve the route and
                    // the remote credits on the read's request budget, then bound the exchange.
                    let at_now = self.instant(now);
                    let Some(budget) =
                        self.nodes[node as usize].cores[worker as usize].request_budget(read)
                    else {
                        return self.violation(format!("node {node} sent for unknown {read:?}"));
                    };
                    let caller_deadline = budget.deadline();
                    let plan = plan_attempt(
                        budget,
                        AttemptInputs {
                            now: at_now,
                            caller_deadline,
                            remaining_opportunities: probe.remaining_opportunities,
                            route: probe.route,
                            mode: probe.mode,
                            attempt_timeout: Duration::from_nanos(self.spec.attempt_timeout),
                            failure_links: MAX_LINKS,
                        },
                    );
                    let copy_only = probe.mode == racer_core::read::walk::Mode::CopyOnly;
                    if copy_only {
                        self.report.counters.peer_requests_copy_only += 1;
                    } else {
                        self.report.counters.peer_requests_acquire += 1;
                    }
                    self.probes
                        .insert((node, worker, id), (to, page, copy_only, now));
                    let plan = match plan {
                        Ok(plan) => plan,
                        Err(error) => {
                            let failed = Input::Replied {
                                id,
                                reply: Reply::Failed {
                                    error,
                                    sent: false,
                                    scope_live: false,
                                },
                                answer: None,
                            };
                            self.engine.at(now, input(read, failed));
                            return;
                        }
                    };
                    let deadline = self.nanos(plan.overall);
                    let timeout = self.nanos(plan.attempt_end).max(now + 1);
                    self.timeouts_queued += 1;
                    self.engine.at(
                        timeout,
                        Ev::ProbeTimeout {
                            node,
                            worker,
                            read,
                            id,
                        },
                    );
                    self.next_msg += 1;
                    let mid = self.next_msg;
                    let msg = Msg {
                        from: node,
                        worker,
                        read,
                        id,
                        dest: to,
                        page,
                        route: Route {
                            version,
                            copy_only,
                            deadline,
                            attempts: plan.remote_attempts,
                            links: plan.links,
                            charged: plan.links,
                        },
                        path: vec![node],
                        at: node,
                        abandon: timeout,
                        ping: None,
                    };
                    self.msgs.insert(mid, msg);
                    self.forward(mid, node, None);
                }
                Acq::Validate { id, .. } => {
                    let at = self.fabric.crypto(
                        now,
                        node,
                        worker as u8,
                        costs.open(self.spec.page_bytes),
                    );
                    let answer = Input::Validated {
                        id,
                        result: Ok(None),
                    };
                    self.engine.at(at, input(read, answer));
                }
                Acq::Batch { id, .. } => {
                    let answer = Input::Batched {
                        id,
                        result: Ok(false),
                    };
                    self.engine
                        .at(now + costs.cpu_request_ns, input(read, answer));
                }
                Acq::Sleep { id, until } => {
                    self.report.counters.stale_waits += 1;
                    let at = self.nanos(until).max(now);
                    self.engine
                        .at(at, input(read, Input::Timer { id, result: Ok(()) }));
                }
                Acq::Origin { id, .. } => {
                    self.report.counters.origin_requests += 1;
                    *self.report.origin_per_page.entry(page).or_default() += 1;
                    let (at, outcome) = self.origin.request(
                        now,
                        page.object,
                        self.spec.page_bytes,
                        self.origin_errors(),
                    );
                    self.classify_origin(node, worker, read, page, at, outcome);
                    if self.diag {
                        // Which owner asked origin, after what walk.
                        let owners = self.latest_owners(page);
                        let rank = owners.iter().position(|o| *o == node);
                        let walk = self.walks.get(&(node, worker, read)).cloned();
                        let key = format!("origin:rank{rank:?}:{}", walk.unwrap_or_default());
                        *self.report.attempt_errors.entry(key).or_default() += 1;
                    }
                    // Count the origin outcome.
                    let c = &mut self.report.counters;
                    let result = match outcome {
                        OriginOutcome::Ok => {
                            c.origin_bytes += self.spec.page_bytes;
                            Ok(())
                        }
                        OriginOutcome::PreconditionFailed => {
                            c.origin_412 += 1;
                            Err(Error::VersionUnavailable)
                        }
                        OriginOutcome::Forbidden => {
                            c.origin_403 += 1;
                            Err(Error::OriginForbidden)
                        }
                        OriginOutcome::ServerError => {
                            c.origin_5xx += 1;
                            Err(Error::Unavailable)
                        }
                    };
                    self.engine
                        .at(at, input(read, Input::Origin { id, result }));
                }
                Acq::Publish { id, .. } => {
                    self.report.counters.disk_writes += 1;
                    let at = self.fabric.disk_write(now, node, self.spec.page_bytes);
                    let evicted = self.nodes[node as usize].stored.insert(page);
                    self.report.counters.evictions += evicted;
                    self.engine
                        .at(at, input(read, Input::Published { id, result: Ok(()) }));
                }
            },
            Effect::Completed {
                request,
                page,
                result,
            } => self.completed(node, worker, request, page, result),
            Effect::Worker { to, msg } => {
                let at = now + costs.cpu_request_ns;
                self.engine.at(
                    at,
                    Ev::Worker {
                        node,
                        worker: to,
                        msg,
                    },
                );
            }
            Effect::Window { window, effect } => self.window_effect(node, worker, window, effect),
            Effect::Deliver { window, next } => self.deliver(node, worker, window, next),
            Effect::Metadata { id, key } => {
                // A metadata HEAD: origin latency and errors, no body.
                self.report.counters.metadata_requests += 1;
                let (at, outcome) = self
                    .origin
                    .request(now, key.object, 0, self.origin_errors());
                let result = match outcome {
                    OriginOutcome::Ok => Ok(1),
                    OriginOutcome::PreconditionFailed => Err(Error::VersionUnavailable),
                    OriginOutcome::Forbidden => Err(Error::OriginForbidden),
                    OriginOutcome::ServerError => Err(Error::Unavailable),
                };
                let answer = Ev::MetadataInput {
                    node,
                    worker,
                    id,
                    result,
                };
                self.engine.at(at, answer);
            }
            Effect::Silence {
                page,
                dest,
                sent,
                note,
            } => {
                // Production `CandidatePolicy::apply` (with `shed_busy` on).
                let at = self.instant(now);
                let silence = &mut self.nodes[node as usize].silence;
                match note {
                    SilenceNote::Shed => silence.shed(dest, page, at),
                    SilenceNote::Answered => silence.answered(&dest, at),
                    SilenceNote::TimedOut { needed } => {
                        // Production `LinkHealth::silence_timed_out`: later routes to `dest`
                        // skip the relay that may have eaten this exchange.
                        self.nodes[node as usize].detours.timed_out(dest, sent, at);
                        if self.diag {
                            let key = format!(
                                "silence_timeout:needed{}:waited{}ms",
                                needed.min(9),
                                at.saturating_duration_since(sent).as_millis() / 500 * 500
                            );
                            *self.report.attempt_errors.entry(key).or_default() += 1;
                        }
                        let silence = &mut self.nodes[node as usize].silence;
                        if silence.timed_out(dest, page, sent, at, needed) {
                            self.poll_pings(node);
                        }
                    }
                }
            }
            Effect::MetadataReady { request, result } => {
                let Some((page, start, attempt)) = self.refreshing.remove(&(node, worker, request))
                else {
                    return self.violation(format!("node {node} unknown metadata {request:?}"));
                };
                match result {
                    Ok(_) => self.client_read(node, page, start, attempt),
                    Err(error) => self.client_failed(node, page, start, attempt, error),
                }
            }
        }
    }

    pub(super) fn completed(
        &mut self,
        node: u32,
        worker: u32,
        request: RequestId,
        page: Page,
        result: Result<(), Error>,
    ) {
        let now = self.now();
        if let Some(body) = self.admitted.remove(&(node, worker, request)) {
            self.nodes[node as usize].cores[worker as usize]
                .serve
                .end_body(body);
        }
        if let Some(asker) = self.serving.remove(&(node, worker, request)) {
            if self.diag {
                let owners = self.latest_owners(page);
                let me = match owners.iter().position(|o| *o == node) {
                    Some(r) => format!("owner{r}"),
                    None => "helper".into(),
                };
                let ms = (now - asker.at) / (250 * MILLIS) * 250;
                let r = match result {
                    Ok(()) => "ok".to_string(),
                    Err(e) => format!("{e:?}"),
                };
                *self
                    .report
                    .attempt_errors
                    .entry(format!("served:{me}:{r}:{ms}ms"))
                    .or_default() += 1;
            }
            if result.is_ok() && self.nodes[node as usize].stored.contains(&page) {
                self.send_page(asker, node, page);
            } else {
                // Production `peer_error` (`src/read.rs`): an expected
                // acquisition failure is answered with its response kind; any other is a
                // transport failure.
                let error = result.err().unwrap_or(Error::Unavailable);
                *self
                    .report
                    .attempt_errors
                    .entry(format!("serve:{error:?}"))
                    .or_default() += 1;
                match peer_error(error) {
                    Some(kind) => self.reply(&asker, node, kind),
                    None => self.fail_probe(&asker, node),
                }
            }
            return;
        }
        let Some((start, attempt)) = self.clients.remove(&(node, worker, request)) else {
            self.violation(format!("node {node} completed unknown read {request:?}"));
            return;
        };
        self.trace.record(
            now,
            TAG_READ_END,
            u64::from(node),
            u64::from(result.is_ok()),
        );
        match result {
            Ok(()) => {
                self.report.counters.reads_completed += 1;
                self.report.latency(now - start);
            }
            Err(error) => self.client_failed(node, page, start, attempt, error),
        }
    }

    /// A client read attempt failed with `error`: retry or fail the read.
    pub(super) fn client_failed(
        &mut self,
        node: u32,
        page: Page,
        start: Nanos,
        attempt: u8,
        error: Error,
    ) {
        let elapsed = (self.now() - start) / SECONDS;
        *self
            .report
            .attempt_errors
            .entry(format!("{error:?}#{attempt}@{elapsed}s"))
            .or_default() += 1;
        match error {
            // The client retries a recoverable failure after a linear backoff.
            error if recoverable(error) && attempt + 1 < MAX_ATTEMPTS => {
                self.report.counters.read_retries += 1;
                let backoff = 2 * self.fabric.rtt.rtt(0, 0).max(MILLIS) * u64::from(attempt + 1);
                let retry = Ev::Read {
                    node,
                    page,
                    start,
                    attempt: attempt + 1,
                };
                self.engine.after(backoff, retry);
            }
            error => {
                self.report.counters.reads_failed += 1;
                *self.report.errors.entry(format!("{error:?}")).or_default() += 1;
            }
        }
    }

    /// End-of-run checks: every read ended and every core is quiescent.
    pub(super) fn finish(&mut self) {
        for walk in std::mem::take(&mut self.walks).into_values() {
            if !walk.ends_with("Page ") {
                *self
                    .report
                    .attempt_errors
                    .entry(format!("walk:{walk}"))
                    .or_default() += 1;
            }
        }
        // A request still in an accept queue when the run ends is in flight, not lost: its
        // requester's attempt bound lies past the end.
        let waiting: BTreeSet<u64> = self.queued.values().map(|(mid, _)| *mid).collect();
        for mid in &waiting {
            self.msgs.remove(mid);
            self.relay_charges.remove(mid);
        }
        if !self.msgs.is_empty() {
            self.violation(format!("{} peer requests never ended", self.msgs.len()));
        }
        if !self.relay_charges.is_empty() {
            let held = self.relay_charges.len();
            self.violation(format!("{held} requests still hold relay transfers"));
        }
        let stats = self.conns.stats();
        let c = &mut self.report.counters;
        c.conn_reused = stats.reused;
        c.conn_accepted = stats.accepted;
        c.conn_queued = stats.queued;
        c.conn_dropped = stats.dropped;
        c.conn_overloaded_endpoint = stats.overloaded_endpoint;
        c.conn_overloaded_pool = stats.overloaded_pool;
        c.conn_idle_cleared = stats.idle_cleared;
        c.conn_idle_expired = stats.idle_expired;
        c.conn_most_queued = stats.most_queued as u64;
        c.conn_idle_evicted = stats.idle_evicted;
        c.conn_refused = stats.refused;
        let c = &self.report.counters;
        let ended = c.reads_completed
            + c.reads_failed
            + c.reads_skipped_not_member
            + c.reads_skipped_not_enrolled;
        if ended != self.expected_reads {
            self.violation(format!("{} of {} reads ended", ended, self.expected_reads));
        }
        for (n, node) in self.nodes.iter().enumerate() {
            let stored = node.stored.len() as u64;
            let max = &mut self.report.counters.stored_pages_max;
            *max = (*max).max(stored);
            for (w, core) in node.cores.iter().enumerate() {
                let refreshes = core.metadata().0;
                if core.flights() + core.requests() + core.reads() + core.windows() + refreshes != 0
                {
                    let message = format!(
                        "node {n} worker {w} not quiescent: {} flights {} requests {} reads",
                        core.flights(),
                        core.requests(),
                        core.reads()
                    );
                    self.report.violation(message);
                }
                let observed = core.observed();
                self.report.counters.helpers += observed.helpers;
                self.report.counters.no_helper += observed.no_helper;
                self.report.counters.flights_reelected += observed.reelected;
                self.report.counters.fills += observed.fills;
                self.report.counters.fills_failed += observed.fills_failed;
            }
        }
        if !self.ranges.is_empty() || !self.refreshing.is_empty() {
            let (r, m) = (self.ranges.len(), self.refreshing.len());
            self.violation(format!(
                "{r} range reads and {m} metadata refreshes unended"
            ));
        }
        if !self.serving.is_empty() {
            self.violation(format!("{} serving reads unanswered", self.serving.len()));
        }
        if !self.bodies.is_empty() {
            self.violation(format!("{} bodies never ended", self.bodies.len()));
        }
    }
}
