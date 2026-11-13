//! Answering probes at the holder and sending pages back.

use super::*;

impl<'s> Model<'s> {
    /// Answer `asker`'s probe at node `to`.
    pub(super) fn serve(&mut self, asker: Asker, to: u32, page: Page, route: Route) {
        let copy_only = route.copy_only;
        if self.diag {
            let owners = self.latest_owners(page);
            if owners.contains(&asker.from) {
                let key = format!(
                    "owner_probe:copy_only{copy_only}:attempts{}:dest_owner{}",
                    route.attempts,
                    owners.contains(&to)
                );
                *self.report.attempt_errors.entry(key).or_default() += 1;
            }
        }
        let now = self.now();
        let worker = owner_worker(page, self.spec.workers.max(1));
        // The destination resolved the request's version (`decide_hop`).
        let (owners, column_mate) = {
            let versions = self.versions.borrow();
            let version = versions.get(route.version).expect("a known version");
            // Production `Membership::same_column` in the default pool: an owner never sheds its
            // own column mates below the exempt cap.
            let column_mate = match (version.position(to), version.position(asker.from)) {
                (Some(a), Some(b)) => version
                    .members
                    .pools()
                    .first()
                    .is_some_and(|pool| pool.same_column(a, b)),
                _ => false,
            };
            (owners(&self.placement, version, page), column_mate)
        };
        let stored = self.nodes[to as usize].stored.contains(&page);
        let at = self.instant(now);
        let core = &mut self.nodes[to as usize].cores[worker as usize];
        let request = ServeRequest {
            facts: ServeFacts {
                operation: OperationKind::Page,
                copy_only,
                object_matches: true,
                cache_available: !self
                    .spec
                    .outages
                    .iter()
                    .any(|o| o.node == to && (o.at..o.until).contains(&now)),
            },
            body: true,
            local_flight: core.acquiring(&page),
        };
        let view = ServeView {
            owners: &owners,
            local: to,
            requester: asker.from,
            column_mate,
        };
        let served = core.serve(&page, request, &view, at);
        self.cover(served_name(&served));
        let decision = match served {
            Served::Defer(_) => {
                self.report.counters.deferrals += 1;
                return self.reply(&asker, to, ResponseKind::Miss);
            }
            Served::Decided(decision) => decision,
        };
        match decision {
            Decision::Shed => {
                self.report.counters.sheds += 1;
                self.reply(&asker, to, ResponseKind::Overloaded)
            }
            Decision::Reject(_) | Decision::Miss => self.reply(&asker, to, ResponseKind::Miss),
            _ if stored => {
                self.nodes[to as usize].stored.touch(page);
                self.send_page(asker, to, page)
            }
            // Production `Fill::copy_only_joining` (`src/read/fill.rs`, `join` from
            // `copy_join_allowed`, `src/read.rs`): with no copy, a joinable copy-only probe waits on
            // the running flight within the serve scope and answers with its fill.
            Decision::CopyOnly { join: true } => {
                let deadline = self.instant((now + SERVE_TIMEOUT).min(route.deadline));
                let mut out = Vec::new();
                let core = &mut self.nodes[to as usize].cores[worker as usize];
                match core.copy_page(page, deadline, at, &mut out) {
                    Some(request) => {
                        self.report.counters.copy_joins += 1;
                        self.serve_admitted(to, worker, request, page, asker);
                        self.dispatch(to, worker, out);
                    }
                    None => self.reply(&asker, to, ResponseKind::Miss),
                }
            }
            Decision::CopyOnly { join: false } => self.reply(&asker, to, ResponseKind::Miss),
            Decision::Acquire => {
                // Production `inherited_budget` (`src/read.rs`): the serving
                // read spends the probe's credits within its own serve scope and the route's
                // signed deadline. The probe travelled one hop.
                let scope = self.instant(now + SERVE_TIMEOUT);
                let inherited = inherited_budget(
                    scope,
                    self.instant(route.deadline),
                    route.attempts,
                    route.links,
                    asker.path.len(),
                );
                let budget = match inherited {
                    Ok(budget) => AcquisitionBudgetSpec::from(&budget),
                    Err(error) => {
                        return match peer_error(error) {
                            Some(kind) => self.reply(&asker, to, kind),
                            None => self.fail_probe(&asker, to),
                        };
                    }
                };
                let mut out = Vec::new();
                let core = &mut self.nodes[to as usize].cores[worker as usize];
                let joined = core.acquiring(&page);
                if joined {
                    self.report.counters.flight_joins += 1;
                }
                if self.diag {
                    let own = owners.contains(&to);
                    *self
                        .report
                        .attempt_errors
                        .entry(format!("srv:owner{own}:joined{joined}"))
                        .or_default() += 1;
                }
                let core = &mut self.nodes[to as usize].cores[worker as usize];
                let request = core.read_page(page, budget, at, &mut out);
                self.serve_admitted(to, worker, request, page, asker);
                self.dispatch(to, worker, out);
            }
        }
    }

    /// Record serving read `request` for `asker`, counting its body from admission.
    pub(super) fn serve_admitted(
        &mut self,
        to: u32,
        worker: u32,
        request: RequestId,
        page: Page,
        asker: Asker,
    ) {
        let key = (to, worker, request);
        self.serving.insert(key, asker);
        let body = self.nodes[to as usize].cores[worker as usize]
            .serve
            .begin_body(page);
        self.admitted.insert(key, body);
    }

    /// `to` streams its copy of `page` to the asker; the reply lands when the body does.
    pub(super) fn send_page(&mut self, asker: Asker, to: u32, page: Page) {
        let now = self.now();
        let worker = owner_worker(page, self.spec.workers.max(1));
        if self.diag {
            let (page_bodies, _) = self.nodes[to as usize].cores[worker as usize]
                .serve
                .streaming(&page);
            *self
                .report
                .attempt_errors
                .entry(format!("fanin:{:03}", page_bodies / 8 * 8))
                .or_default() += 1;
        }
        let body = self.nodes[to as usize].cores[worker as usize]
            .serve
            .begin_body(page);
        let (page_bodies, _) = self.nodes[to as usize].cores[worker as usize]
            .serve
            .streaming(&page);
        let peak = &mut self.report.counters.page_peak_fanin;
        *peak = (*peak).max(page_bodies as u64);
        self.next_body += 1;
        let key = self.next_body;
        self.bodies.insert(key, (to, worker, body));
        // Production drops an exchange its requester abandoned: no chunk starts at or after the
        // requester's attempt bound.
        self.answering(&asker, to, now);
        let sealed = self.sign(to, asker.msg);
        let route = Self::back(&asker, to);
        if !self.alive(&route) {
            self.release_relays(asker.msg);
            return self.engine.at(now, Ev::BodyEnd { body: key });
        }
        let end = self
            .fabric
            .transfer_until(sealed, &route, self.spec.page_bytes, asker.abandon);
        if end.is_some() {
            self.answered_msgs.insert(asker.msg);
        }
        self.release_relays_at(asker.msg, end.unwrap_or(asker.abandon));
        let Some(end) = end else {
            self.report.counters.replies_abandoned += 1;
            self.engine
                .at(asker.abandon.max(now), Ev::BodyEnd { body: key });
            return;
        };
        if self.diag {
            let owner = self.latest_owners(page).contains(&to);
            let secs = (end - now) / (MILLIS * 250);
            *self
                .report
                .attempt_errors
                .entry(format!(
                    "send:{}:{}q",
                    if owner { "owner" } else { "helper" },
                    secs
                ))
                .or_default() += 1;
        }
        self.report.counters.peer_bytes += self.spec.page_bytes;
        self.report.counters.peer_pages += 1;
        self.report.counters.peer_bytes += self.spec.page_bytes * (route.len() as u64 - 2);
        if !self
            .owners_at(asker.version, page)
            .is_some_and(|owners| owners.contains(&to))
        {
            self.report.counters.helper_copies += 1;
        }
        self.engine.at(end, Ev::BodyEnd { body: key });
        self.engine.at(
            end,
            Ev::Input {
                node: asker.from,
                worker: asker.worker,
                read: asker.read,
                input: Input::Replied {
                    id: asker.id,
                    reply: Reply::Answer {
                        kind: ResponseKind::Page,
                        signed: true,
                    },
                    answer: Some(to),
                },
            },
        );
    }
}
