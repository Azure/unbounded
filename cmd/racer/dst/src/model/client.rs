//! Client reads, window pumping, rankings and visit order.

use super::*;

impl<'s> Model<'s> {
    /// Start client read attempt `attempt` of `page` on `node`.
    pub(super) fn client_read(&mut self, node: u32, page: Page, start: Nanos, attempt: u8) {
        let now = self.now();
        if self.nodes[node as usize].stored.touch(page) {
            self.report.counters.local_hits += 1;
            self.report.counters.reads_completed += 1;
            self.report.latency(now - start);
            return;
        }
        let worker = owner_worker(page, self.spec.workers.max(1));
        let budget = AcquisitionBudgetSpec {
            deadline: self.instant(now + self.spec.read_timeout),
            attempts: 8,
            links: 16,
            route_failed: false,
        };
        let at = self.instant(now);
        let core = &mut self.nodes[node as usize].cores[worker as usize];
        if core.acquiring(&page) {
            self.report.counters.flight_joins += 1;
        }
        let mut out = Vec::new();
        let request = core.read_page(page, budget, at, &mut out);
        self.clients
            .insert((node, worker, request), (start, attempt));
        self.dispatch(node, worker, out);
    }

    /// Refresh `page`'s object metadata through the owning worker's cohort, then read.
    pub(super) fn refresh(&mut self, node: u32, page: Page, start: Nanos, attempt: u8) {
        let now = self.now();
        let worker = owner_worker(page, self.spec.workers.max(1));
        let (at, deadline) = (
            self.instant(now),
            self.instant(now + self.spec.read_timeout),
        );
        let mut out = Vec::new();
        let request = self.nodes[node as usize].cores[worker as usize].refresh_metadata(
            page.object,
            RefreshPurpose::Current,
            deadline,
            3,
            at,
            &mut out,
        );
        self.refreshing
            .insert((node, worker, request), (page, start, attempt));
        self.dispatch(node, worker, out);
    }

    /// Open a client range read of `object` on `node`. Its window lives on worker
    /// `node % workers`, so pages owned by the other workers cross as worker fetches.
    pub(super) fn open(&mut self, node: u32, object: u64, range: RangeRead) {
        let now = self.now();
        let worker = node % self.spec.workers.max(1);
        let config = WindowConfig {
            start: 0,
            end: u64::from(self.spec.pages_per_object),
            window_pages: range.window_pages.max(1),
            sealed: false,
            passthrough: false,
            spill: range.spill,
            budget: PageBudget::Client {
                deadline: self.instant(now + self.spec.read_timeout),
            },
        };
        let key_of = Box::new(move |number: u64| Page {
            object,
            number: number as u32,
        });
        let window = self.nodes[node as usize].cores[worker as usize].open(config, key_of);
        let state = RangeState {
            object,
            start: now,
            delivered: 0,
            total: u64::from(self.spec.pages_per_object),
            leased: range.leased,
        };
        self.ranges.insert((node, worker, window), state);
        self.pump(node, worker, window);
    }

    /// Pump every open window of `(node, worker)`: a fill that ended inside the core (a
    /// flight settled, a worker fetch answered) may have freed or failed a slot.
    pub(super) fn pump_all(&mut self, node: u32, worker: u32) {
        let windows: Vec<_> = self
            .ranges
            .range((node, worker, WindowId(0))..=(node, worker, WindowId(u64::MAX)))
            .map(|(key, _)| key.2)
            .collect();
        for window in windows {
            self.pump(node, worker, window);
        }
    }

    /// Plan window work and deliver what is ready.
    pub(super) fn pump(&mut self, node: u32, worker: u32, window: WindowId) {
        let Some(state) = self.ranges.get(&(node, worker, window)) else {
            return;
        };
        let facts = Facts {
            leased: state.leased,
            ..Facts::default()
        };
        let at = self.instant(self.now());
        let mut out = Vec::new();
        let _ = self.nodes[node as usize].cores[worker as usize].pump(
            window,
            at,
            &facts,
            |_| true,
            &mut out,
        );
        self.dispatch(node, worker, out);
    }

    /// Complete window effect `effect` of `(node, worker, window)`.
    pub(super) fn window_effect(
        &mut self,
        node: u32,
        worker: u32,
        window: WindowId,
        effect: window::Effect,
    ) {
        use window::Effect as Win;
        let now = self.now();
        let costs = self.fabric.costs;
        let Some(object) = self.ranges.get(&(node, worker, window)).map(|r| r.object) else {
            // Effects of a closed window (releases, discards) need no answer.
            return;
        };
        let key = |number: u64| Page {
            object,
            number: number as u32,
        };
        let answer = |input| Ev::WindowInput {
            node,
            worker,
            window,
            input,
        };
        match effect {
            Win::Probe { id, page, .. } => {
                let found = self.nodes[node as usize].stored.touch(key(page));
                let at = if found {
                    self.report.counters.disk_reads += 1;
                    self.fabric.disk_read(now, node, self.spec.page_bytes)
                } else {
                    now + costs.cpu_request_ns
                };
                self.engine
                    .at(at, answer(window::Input::Probed { id, found }));
            }
            // Provider selection is not modeled: the selection cannot start and its pages
            // are filled directly.
            Win::Select { id, .. } => {
                let input = window::Input::Selected {
                    id,
                    ok: false,
                    started: false,
                };
                self.engine.at(now + costs.cpu_request_ns, answer(input));
            }
            Win::Hold { id, .. } => {
                self.report.counters.spill_holds += 1;
                let at = self.fabric.disk_write(now, node, 0);
                let input = window::Input::Held {
                    id,
                    result: Ok(id.0),
                };
                self.engine.at(at, answer(input));
            }
            // The held page is persisted when the node already stores it; otherwise the spill
            // fill fails and the window fills the page directly.
            Win::SpillFill { id, page } => {
                let ok = self.nodes[node as usize].stored.contains(&key(page));
                let at = self.fabric.disk_write(now, node, self.spec.page_bytes);
                self.engine
                    .at(at, answer(window::Input::SpillFilled { id, ok }));
            }
            Win::Fetch { .. } => self.violation(format!("node {node} leaked a window fetch")),
            Win::Release(_) | Win::Discard(_) => {}
        }
    }

    /// The client of `(node, worker, window)` received `next`.
    pub(super) fn deliver(&mut self, node: u32, worker: u32, window: WindowId, next: Next) {
        let now = self.now();
        let Some(state) = self.ranges.get_mut(&(node, worker, window)) else {
            return self.violation(format!("node {node} delivered to a closed window"));
        };
        match next {
            Next::Wait => {}
            Next::Page { .. } => {
                state.delivered += 1;
                let start = state.start;
                self.report.counters.reads_completed += 1;
                self.report.latency(now - start);
                let at = now + self.fabric.costs.cpu_request_ns;
                let pump = Ev::Pump {
                    node,
                    worker,
                    window,
                };
                self.engine.at(at, pump);
            }
            Next::End | Next::Fail(_) => {
                let state = self
                    .ranges
                    .remove(&(node, worker, window))
                    .expect("present");
                let missing = state.total - state.delivered;
                if let Next::Fail(error) = next {
                    self.report.counters.reads_failed += missing;
                    *self.report.errors.entry(format!("{error:?}")).or_default() += missing;
                } else if missing != 0 {
                    self.violation(format!("window ended with {missing} pages undelivered"));
                }
                let at = self.instant(now);
                let mut out = Vec::new();
                let core = &mut self.nodes[node as usize].cores[worker as usize];
                core.close(window, at, &mut out);
                core.drop_window(window);
                self.dispatch(node, worker, out);
            }
        }
    }

    /// Rank `page` for `node` (production `CandidatePolicy::rank_for`).
    /// Polls of `node`'s ranking of `page` on `worker` (one
    /// `ranking_chunk_ns` each). A worker's first ranking of a page under a membership scores
    /// every member, `WORK_QUANTUM` (256) per poll; later ones answer from its placement cache
    /// in one poll.
    pub(super) fn ranking_chunks(
        &mut self,
        node: u32,
        worker: u32,
        page: Page,
        basis: &RankBasis<Ranking, u64>,
    ) -> u64 {
        let number = match basis {
            RankBasis::Verify(ranking) => ranking.0,
            RankBasis::Latest(number) | RankBasis::Fallback(number) => *number,
            RankBasis::Previous { version } => *version,
        };
        let Some(members) = self.versions.borrow().get(number).map(|v| v.count() as u64) else {
            return 1;
        };
        ranking_polls(self.ranked.insert((node, worker, number, page)), members)
    }

    pub(super) fn rank(
        &self,
        node: u32,
        page: Page,
        basis: RankBasis<Ranking, u64>,
    ) -> Result<RankAnswer<Ranking, u64>, Error> {
        let versions = self.versions.borrow();
        let number = match &basis {
            RankBasis::Verify(ranking) => ranking.0,
            RankBasis::Latest(number) | RankBasis::Fallback(number) => *number,
            RankBasis::Previous { version } => *version,
        };
        let version = versions.get(number).ok_or(Error::IncompatibleMembership)?;
        let owners = owners(&self.placement, version, page);
        if let RankBasis::Previous { version: previous } = basis {
            // Previous owners that are not members of the walked version are not asked.
            let walked = versions.get(previous + 1).ok_or(Error::Unavailable)?;
            let owners: Vec<u32> = owners
                .iter()
                .map(|o| {
                    if walked.position(*o).is_some() {
                        *o
                    } else {
                        NOT_MEMBER
                    }
                })
                .collect();
            let recent = versions.previous_recent_member(node, previous + 1);
            return Ok(RankAnswer {
                ranked: Ranked {
                    recent_member: recent,
                    ..Ranked::new(previous, &owners, None)
                },
                ranking: None,
                recent: recent.then_some(previous),
            });
        }
        if let RankBasis::Verify(initial) = &basis
            && initial.1 != owners
        {
            // Public rankings are not authority.
            return Err(Error::IncompatibleMembership);
        }
        let rank = owners.iter().position(|o| *o == node);
        let mut ranked = Ranked {
            members: version.count(),
            ..Ranked::new(number, &owners, rank)
        };
        if rank.is_none() {
            ranked.order = self.visit_order(node, page, &owners, version);
        }
        Ok(RankAnswer {
            ranked,
            ranking: Some(Box::new((number, owners))),
            recent: None,
        })
    }

    /// A non-owner's visit order over `owners` (production
    /// `CandidatePolicy::visit_order`): `Spread` visits mesh neighbors first, then by descending
    /// `replica_score`, so readers spread over the owners instead of all asking rank 0. One
    /// membership version, so `lineage_first` never moves an owner. Owners that left this
    /// node's requests unanswered or shed them go last (`demote_unanswered`).
    pub(super) fn visit_order(
        &self,
        node: u32,
        page: Page,
        owners: &[u32],
        version: &Version,
    ) -> VisitOrder {
        let id = |n: u32| NodeId(format!("n{n:08}"));
        let mut key = [0u8; 32];
        key[..8].copy_from_slice(&page.object.to_be_bytes());
        let object = ObjectId {
            cache: CacheId("model".to_owned()),
            key: CacheKey(key),
        };
        let requester = id(node);
        let order = visit_order(
            ReplicaSelection::Spread,
            owners.len(),
            |r| match (version.position(node), version.position(owners[r])) {
                (Some(a), Some(b)) => version.members.is_neighbor(a, b),
                _ => false,
            },
            |r| {
                replica_score(
                    &requester,
                    &object,
                    PageNumber(u64::from(page.number)),
                    &id(owners[r]),
                )
            },
        );
        let at = self.instant(self.now());
        let silence = &self.nodes[node as usize].silence;
        demote_unanswered(order, |r| {
            if silence.silent(&owners[r], at) {
                Answering::Silent
            } else if silence.unanswered(&owners[r], at) {
                Answering::Unanswered
            } else {
                Answering::Answers
            }
        })
    }
}
