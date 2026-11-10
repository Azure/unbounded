//! Engine event handling: one step per popped [`Ev`].

use super::*;

impl<'s> Model<'s> {
    pub(super) fn step(&mut self, event: Ev) {
        let now = self.now();
        self.versions.borrow_mut().now = now;
        // Idle connections that timed out free their ingress slots first.
        self.conns.advance(now);
        self.admit_queued();
        match event {
            Ev::Arrival { node } => {
                self.next_arrival(node);
                if self.nodes[node as usize].down {
                    return;
                }
                let u = unit(&mut self.workload_rng);
                let object = (self.zipf.partition_point(|&c| c <= u) as u64)
                    .min(self.zipf.len().max(1) as u64 - 1);
                let number = self
                    .workload_rng
                    .below(u64::from(self.spec.pages_per_object.max(1)))
                    as u32;
                let w = self.spec.workload.expect("workload");
                let pages = if unit(&mut self.workload_rng) < w.range_fraction {
                    self.report.counters.range_reads += 1;
                    w.range_pages
                        .max(1)
                        .min(self.spec.pages_per_object.max(1) - number)
                } else {
                    1
                };
                self.report.counters.workload_reads += 1;
                for number in number..number + pages {
                    self.expected_reads += 1;
                    let page = Page { object, number };
                    let read = Ev::Read {
                        node,
                        page,
                        start: now,
                        attempt: 0,
                    };
                    self.step(read);
                }
            }
            Ev::Membership { node, live } => {
                if self.versions.borrow_mut().set(node, live) {
                    self.report.counters.membership_changes += 1;
                    if !self.publishing {
                        self.publishing = true;
                        let batch = membership::BATCH;
                        self.engine.at((now / batch + 1) * batch, Ev::Publish);
                    }
                }
            }
            Ev::Refund {
                node,
                worker,
                read,
                remote,
                links,
            } => {
                let core = &mut self.nodes[node as usize].cores[worker as usize];
                // A read that already ended has nothing to refund.
                if let Some(budget) = core.request_budget(read) {
                    let refunded = budget
                        .refund_remote(remote)
                        .and_then(|()| budget.refund_links(links));
                    if let Err(error) = refunded {
                        self.violation(format!("stale refund grew the budget: {error:?}"));
                    }
                    self.report.counters.stale_links_refunded += u64::from(links);
                }
            }
            Ev::Publish => {
                self.publishing = false;
                if self.versions.borrow_mut().publish(now) {
                    self.report.counters.membership_publications += 1;
                }
            }
            Ev::Read {
                node,
                page,
                start,
                attempt,
            } => {
                self.trace.record(now, TAG_EVENT, 1, u64::from(node));
                self.tick_soon();
                if self.versions.borrow().applied(node) == Some(0) {
                    // Combined: the node's control client holds no snapshot yet.
                    self.report.counters.reads_skipped_not_enrolled += 1;
                    return;
                }
                if !self.member(node) {
                    self.report.counters.reads_skipped_not_member += 1;
                    return;
                }
                if self.spec.metadata {
                    self.refresh(node, page, start, attempt);
                } else {
                    self.client_read(node, page, start, attempt);
                }
            }
            Ev::Open {
                node,
                object,
                range,
            } => {
                self.trace.record(now, TAG_EVENT, 6, u64::from(node));
                self.tick_soon();
                if !self.member(node) {
                    let pages = u64::from(self.spec.pages_per_object);
                    self.report.counters.reads_skipped_not_member += pages;
                    return;
                }
                self.open(node, object, range);
            }
            Ev::WindowInput {
                node,
                worker,
                window,
                input,
            } => {
                self.trace.record(now, TAG_EVENT, 7, u64::from(node));
                let mut out = Vec::new();
                let at = self.instant(now);
                // A closed window refuses its late answers.
                let _ = self.nodes[node as usize].cores[worker as usize]
                    .on_window(window, input, true, at, &mut out);
                self.dispatch(node, worker, out);
                self.pump(node, worker, window);
            }
            Ev::Pump {
                node,
                worker,
                window,
            } => self.pump(node, worker, window),
            Ev::Outage { node } => {
                self.trace.record(now, TAG_EVENT, 9, u64::from(node));
                let capacity = self.spec.stored_pages;
                let lost = self.nodes[node as usize].stored.len() as u64;
                self.report.counters.outage_lost_pages += lost;
                self.nodes[node as usize].stored = Store::new(capacity);
            }
            Ev::Fault(fault) => {
                use crate::world::Fault as F;
                self.trace.record(now, TAG_EVENT, 11, fault_code(fault));
                match fault {
                    F::Crash { node } => {
                        self.report.counters.crashes += 1;
                        self.nodes[node as usize].down = true;
                        self.nodes[node as usize].down_at = now;
                        // The process loses its memory. The Model has one page tier, so every
                        // stored page goes with it (production recovers only its last
                        // checkpoint).
                        let lost = self.nodes[node as usize].stored.len() as u64;
                        self.report.counters.crash_lost_pages += lost;
                        self.nodes[node as usize].stored = Store::new(self.spec.stored_pages);
                    }
                    F::Restart { node } => self.nodes[node as usize].down = false,
                    F::Partition { a, b } if a != b => {
                        self.partitions.insert((a.min(b), a.max(b)));
                    }
                    F::Heal { a, b } => {
                        self.partitions.remove(&(a.min(b), a.max(b)));
                    }
                    F::SlowPeer { node, factor } => self.fabric.slow[node as usize] = factor.max(1),
                    // The Model has no wall or monotonic clock per node.
                    _ => {}
                }
            }
            Ev::Release { msg } => self.release_relays_after(msg, true),
            Ev::LinkDown {
                msg,
                holder,
                worker,
                next,
            } => {
                // Production `request_network` (`src/peer.rs`): a socket failure is evidence
                // against the link (`route::exchange_evidence`, `LinkOutcome::Refused`), recorded
                // when the connect attempt fails.
                self.link_failed(holder, worker, next);
                self.fail_hop(msg, holder, Fault::Unavailable)
            }
            Ev::MetadataInput {
                node,
                worker,
                id,
                result,
            } => {
                self.trace.record(now, TAG_EVENT, 8, u64::from(node));
                let mut out = Vec::new();
                let at = self.instant(now);
                if self.nodes[node as usize].cores[worker as usize]
                    .on_metadata(id, result, at, &mut out)
                    .is_err()
                {
                    self.violation(format!("node {node} refused metadata answer {id:?}"));
                }
                self.dispatch(node, worker, out);
            }
            Ev::Input {
                node,
                worker,
                read,
                input,
            } => {
                self.trace.record(now, TAG_EVENT, 2, u64::from(node));
                if let Input::Replied { id, reply, .. } = &input {
                    let cut = self.cuts.remove(&(node, worker, *id));
                    let Some((to, page, _, sent)) = self.probes.remove(&(node, worker, *id)) else {
                        // The attempt timed out first; its late answer is dropped.
                        self.report.counters.late_answers += 1;
                        return;
                    };
                    if let Reply::Answer { .. } = reply {
                        // The requester verifies the signed response on its crypto helper: the
                        // walk does not wait for the slot.
                        let verify = self.fabric.costs.crypto_verify_ns;
                        self.fabric.crypto(now, node, worker as u8, verify);
                    }
                    self.asked(node, worker, read, to, asked(reply, cut));
                    if self.diag {
                        let owners = self.latest_owners(page);
                        let role = |n: u32| match owners.iter().position(|o| *o == n) {
                            Some(r) => format!("owner{r}"),
                            None => "x".into(),
                        };
                        let kind = match reply {
                            Reply::Answer { kind, signed: true } => format!("{kind:?}"),
                            Reply::Answer {
                                kind,
                                signed: false,
                            } => format!("relay{kind:?}"),
                            other => format!("{other:?}"),
                        };
                        let ms = (now - sent) / (250 * MILLIS) * 250;
                        let key = format!("reply:{}->{}:{kind}:{ms}ms", role(node), role(to));
                        *self.report.attempt_errors.entry(key).or_default() += 1;
                        let walk = self.walks.entry((node, worker, read)).or_default();
                        walk.push(if owners.contains(&to) { 'O' } else { 'H' });
                        walk.extend(kind.chars().take(4));
                        walk.push(' ');
                    }
                }
                let mut out = Vec::new();
                let at = self.instant(now);
                // A stopped acquisition refuses its late answers.
                let _ = self.nodes[node as usize].cores[worker as usize]
                    .on_acquisition(read, input, at, &mut out);
                self.dispatch(node, worker, out);
                self.pump_all(node, worker);
            }
            Ev::Hop { msg, verified } => self.hop(msg, verified),
            Ev::Verified { msg } => {
                if let Some(m) = self.msgs.get(&msg) {
                    let w = owner_worker(m.page, self.spec.workers.max(1)) as u8;
                    let ready = self
                        .fabric
                        .cpu(now, m.at, w, self.fabric.costs.cpu_request_ns);
                    let hop = Ev::Hop {
                        msg,
                        verified: true,
                    };
                    self.engine.at(ready, hop);
                }
            }
            Ev::Abandon { msg } => {
                self.timeouts_queued -= 1;
                let waiting = self
                    .msg_conns
                    .get(&msg)
                    .is_some_and(|ids| ids.iter().any(|id| self.queued.contains_key(id)));
                self.close_conns(msg);
                if waiting {
                    // Never accepted: no holder will see it again.
                    self.report.counters.requests_abandoned += 1;
                    self.drop_msg(msg);
                }
            }
            Ev::LastHop { msg } => {
                self.timeouts_queued -= 1;
                if let Some((sender, dest, page, sent)) = self.last_hops.remove(&msg) {
                    self.report.counters.silence_timeouts += 1;
                    let (sent, at) = (self.instant(sent), self.instant(now));
                    let known = self.nodes[sender as usize].silence.unanswered(&dest, at);
                    let want = self.nodes[sender as usize].silence.timed_out(
                        dest,
                        page,
                        sent,
                        at,
                        LINK_TIMEOUTS,
                    );
                    if self.diag {
                        let key = format!("lasthop:known{known}:want{want}");
                        *self.report.attempt_errors.entry(key).or_default() += 1;
                    }
                    if want {
                        self.poll_pings(sender);
                    }
                }
            }
            Ev::PingTimeout { ping } => {
                self.timeouts_queued -= 1;
                if let Some((from, to)) = self.pings.remove(&ping) {
                    // Unanswered within the attempt timeout: silent.
                    let answered = racer_core::liveness::ping_verdict(Err(true));
                    self.ping_done(from, to, answered);
                }
            }
            Ev::PingEnd { ping, answered } => {
                if let Some((from, to)) = self.pings.remove(&ping) {
                    self.ping_done(from, to, answered);
                }
            }
            Ev::PollPings { from } => self.poll_pings(from),
            Ev::ProbeTimeout {
                node,
                worker,
                read,
                id,
            } => {
                self.timeouts_queued -= 1;
                let cut = self.cuts.remove(&(node, worker, id));
                if let Some((to, page, copy_only, _)) = self.probes.remove(&(node, worker, id)) {
                    self.asked(node, worker, read, to, Asked::Unreachable(cut));
                    if self.diag {
                        let owners = self.latest_owners(page);
                        let role = match owners.iter().position(|o| *o == to) {
                            Some(r) => format!("owner{r}"),
                            None => "helper".into(),
                        };
                        let me = match owners.iter().position(|o| *o == node) {
                            Some(r) => format!("owner{r}"),
                            None => "reader".into(),
                        };
                        // Does `to` wait on a probe of the same page, and to whom?
                        let waits = self
                            .probes
                            .iter()
                            .filter(|((n, _, _), (_, p, _, _))| *n == to && *p == page)
                            .map(|(_, (t, _, _, _))| {
                                if *t == node {
                                    "back"
                                } else if owners.contains(t) {
                                    "owner"
                                } else {
                                    "other"
                                }
                            })
                            .next()
                            .unwrap_or("none");
                        *self
                            .report
                            .attempt_errors
                            .entry(format!(
                                "timeout:{me}->{role}:copy{copy_only}:waits-{waits}"
                            ))
                            .or_default() += 1;
                    }
                    self.trace.record(now, TAG_EVENT, 10, u64::from(node));
                    self.report.counters.probe_timeouts += 1;
                    let mut out = Vec::new();
                    let at = self.instant(now);
                    let timed_out = Input::Replied {
                        id,
                        reply: Reply::TimedOut,
                        answer: None,
                    };
                    let fed = self.nodes[node as usize].cores[worker as usize]
                        .on_acquisition(read, timed_out, at, &mut out);
                    if self.diag {
                        let silence = out
                            .iter()
                            .filter(|e| matches!(e, Effect::Silence { .. }))
                            .count();
                        let key = format!("probe_timeout_fed:{fed:?}:silence{silence}");
                        *self.report.attempt_errors.entry(key).or_default() += 1;
                    }
                    self.dispatch(node, worker, out);
                    self.pump_all(node, worker);
                }
            }
            Ev::BodyEnd { body } => {
                self.trace.record(now, TAG_EVENT, 4, body);
                match self.bodies.remove(&body) {
                    Some((node, worker, body)) => {
                        self.nodes[node as usize].cores[worker as usize]
                            .serve
                            .end_body(body);
                    }
                    None => self.violation(format!("body {body} ended twice")),
                }
            }
            Ev::Worker { node, worker, msg } => {
                self.trace.record(now, TAG_EVENT, 5, u64::from(node));
                let mut out = Vec::new();
                let at = self.instant(now);
                // A fetch answer may arrive after its window closed (the range read failed);
                // the core refuses it. A refused fetch request is a hard failure.
                let late = matches!(msg, WorkerMsg::Fetched { .. });
                if self.nodes[node as usize].cores[worker as usize]
                    .on_worker_msg(msg, at, &mut out)
                    .is_err()
                    && !late
                {
                    self.violation(format!("node {node} worker {worker} refused a message"));
                }
                self.dispatch(node, worker, out);
                self.pump_all(node, worker);
            }
            Ev::Tick => {
                self.ticking = false;
                let at = self.instant(now);
                for node in 0..self.nodes.len() {
                    for worker in 0..self.nodes[node].cores.len() {
                        let mut out = Vec::new();
                        self.nodes[node].cores[worker].tick(at, &mut out);
                        self.dispatch(node as u32, worker as u32, out);
                        self.pump_all(node as u32, worker as u32);
                    }
                }
                if !self.clients.is_empty() || !self.serving.is_empty() {
                    self.tick_soon();
                }
            }
        }
    }
}
