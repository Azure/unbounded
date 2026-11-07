//! Model construction, world queries, fault and storm scheduling, and the event loop.

use super::*;

impl<'s> Model<'s> {
    pub(super) fn new(scenario: &'s Scenario, spec: &'s ModelSpec, choices: Choices) -> Self {
        let n = scenario.nodes.max(1);
        let base = Instant::now();
        let versions = Rc::new(RefCell::new(
            if spec.control.as_ref().is_some_and(|c| c.combined) {
                Versions::external(n, membership::RETAINED)
            } else {
                Versions::new(n, spec.membership_lag, membership::RETAINED)
            },
        ));
        let phantom = Phantom::new();
        let placement = Rc::new(Placement::with_rankings(4096, phantom));
        let fabric = Fabric::new(n, scenario.sites, &scenario.costs, &choices);
        let origin = Origin::new(&scenario.costs, choices.clone());
        let workers = spec.workers.max(1);
        let nodes = (0..n)
            .map(|node| Node {
                cores: (0..workers)
                    .map(|worker| core(node, worker, workers, &versions, &placement, base, spec))
                    .collect(),
                stored: Store::new(spec.stored_pages),
                // Production: half the client request timeout, and the peer attempt timeout for
                // one ping.
                silence: Silence::new(
                    SILENCE_CAPACITY,
                    Duration::from_nanos(spec.read_timeout / 2),
                    Duration::from_nanos(ATTEMPT_TIMEOUT),
                ),
                pings: 0,
                detours: Detours::new(SILENCE_CAPACITY),
                down: false,
                down_at: 0,
            })
            .collect();
        Self {
            spec,
            engine: Engine::new(),
            trace: TraceHash::default(),
            workload_rng: SplitMix64::new(mix64(choices.seed() ^ 0x5741_524b)),
            zipf: spec
                .workload
                .map_or_else(Vec::new, |w| zipf_cdf(w.objects, w.zipf_s)),
            arrivals_until: scenario
                .end
                .saturating_sub(2 * spec.read_timeout + 10 * SECONDS),
            choices,
            base,
            fabric,
            origin,
            versions,
            placement,
            nodes,
            clients: BTreeMap::new(),
            refreshing: BTreeMap::new(),
            ranges: BTreeMap::new(),
            serving: BTreeMap::new(),
            admitted: BTreeMap::new(),
            probes: BTreeMap::new(),
            answered: BTreeSet::new(),
            bodies: BTreeMap::new(),
            next_body: 0,
            ticking: false,
            publishing: false,
            relay_charges: BTreeMap::new(),
            relays_active: BTreeMap::new(),
            searches: BTreeMap::new(),
            links: BTreeMap::new(),
            acquisitions: BTreeMap::new(),
            acquisitions_added: 0,
            cuts: BTreeMap::new(),
            report: Report::default(),
            expected_reads: 0,
            timeouts_queued: 0,
            diag: std::env::var_os("MODEL_DIAG").is_some(),
            walks: BTreeMap::new(),
            ranked: BTreeSet::new(),
            msgs: BTreeMap::new(),
            next_msg: 0,
            last_hops: BTreeMap::new(),
            partitions: BTreeSet::new(),
            pings: BTreeMap::new(),
            paths: Paths::with_limits(ROUTE_CACHE_ENTRIES, ROUTE_CACHE_ENTRIES * 256, 8),
            conns: Connections::new(match spec.connections {
                PeerConnections::Custom(config) => config,
                _ => ConnectionConfig::production(workers),
            }),
            msg_conns: BTreeMap::new(),
            queued: BTreeMap::new(),
            answered_msgs: BTreeSet::new(),
        }
    }

    /// Whether `node` is listed in the membership it adopted. A node that is not is not ready
    /// (production `/readyz`, `WorkerApplication::membership_usable`), so its clients do not
    /// read through it.
    pub(super) fn member(&self, node: u32) -> bool {
        let versions = self.versions.borrow();
        versions.adopted(node).position(node).is_some()
    }

    /// The owners of `page` under the latest membership (diagnostics).
    pub(super) fn latest_owners(&self, page: Page) -> Vec<u32> {
        owners(&self.placement, self.versions.borrow().latest(), page)
    }

    /// The owners of `page` under membership `number`, while it is retained.
    pub(super) fn owners_at(&self, number: u64, page: Page) -> Option<Vec<u32>> {
        let versions = self.versions.borrow();
        versions
            .get(number)
            .map(|v| owners(&self.placement, v, page))
    }

    pub(super) fn instant(&self, at: Nanos) -> Instant {
        self.base + Duration::from_nanos(at)
    }

    pub(super) fn nanos(&self, at: Instant) -> Nanos {
        at.saturating_duration_since(self.base).as_nanos() as Nanos
    }

    pub(super) fn now(&self) -> Nanos {
        self.engine.now()
    }

    /// Count coverage name `name`; a name [`coverage::VARIANTS`] lacks is a hard failure.
    pub(super) fn cover(&mut self, name: &'static str) {
        if !coverage::VARIANTS.contains(&name) {
            self.violation(format!("coverage name {name} is not in coverage::VARIANTS"));
        }
        *self.report.coverage.entry(name).or_default() += 1;
    }

    pub(super) fn violation(&mut self, message: String) {
        self.report.violation(message);
    }

    pub(super) fn schedule_faults(&mut self, faults: Vec<(Nanos, crate::world::Fault)>) {
        for (at, fault) in faults {
            self.engine.at(at, Ev::Fault(fault));
        }
    }

    /// Whether `node`'s peer server hangs now.
    pub(super) fn hung(&self, node: u32) -> bool {
        let now = self.now();
        self.spec
            .hangs
            .iter()
            .any(|h| h.node == node && (h.at..h.until).contains(&now))
    }

    /// Whether the link `a - b` is cut: partitioned, or `b` is down.
    pub(super) fn cut(&self, a: u32, b: u32) -> bool {
        self.nodes[b as usize].down
            || self.partitions.contains(&(a.min(b), a.max(b)))
            || self.site_cut(a, b)
    }

    /// Whether a [`SiteCut`] separates `a` and `b` now.
    pub(super) fn site_cut(&self, a: u32, b: u32) -> bool {
        if self.spec.site_cuts.is_empty() {
            return false;
        }
        let now = self.now();
        let site = &self.fabric.site;
        let (sa, sb) = (site[a as usize], site[b as usize]);
        self.spec.site_cuts.iter().any(|c| {
            (c.from..c.until).contains(&now) && ((c.a, c.b) == (sa, sb) || (c.b, c.a) == (sa, sb))
        })
    }

    /// Origin error rates now: the scenario's plus active bursts.
    pub(super) fn origin_errors(&self) -> OriginErrors {
        let now = self.now();
        let mut e = self.spec.origin_errors;
        for b in &self.spec.origin_bursts {
            if (b.from..b.until).contains(&now) {
                e.p412 += b.errors.p412;
                e.p403 += b.errors.p403;
                e.p5xx += b.errors.p5xx;
            }
        }
        e
    }

    /// Schedule `node`'s next [`Workload`] read.
    pub(super) fn next_arrival(&mut self, node: u32) {
        let Some(w) = self.spec.workload else {
            return;
        };
        if w.rate <= 0.0 {
            return;
        }
        let u = 1.0 - unit(&mut self.workload_rng);
        let gap = ((-u.ln()) * (SECONDS as f64 / w.rate)).ceil().max(1.0) as Nanos;
        if self.now().saturating_add(gap) <= self.arrivals_until {
            self.engine.after(gap, Ev::Arrival { node });
        }
    }

    pub(super) fn schedule_storms(&mut self) {
        for node in 0..self.nodes.len() as u32 {
            self.next_arrival(node);
        }
        let mut churn = self.spec.churn.clone();
        if let Some(random) = self.spec.random_churn {
            let n = self.nodes.len() as u64;
            for _ in 0..random.events {
                let node = self.choices.below(n) as u32;
                let at = self.choices.below(random.within.max(1));
                churn.push(Churn {
                    at,
                    node,
                    join: false,
                });
                churn.push(Churn {
                    at: at + random.away,
                    node,
                    join: true,
                });
            }
        }
        for churn in churn {
            let live = churn.join;
            self.engine.at(
                churn.at,
                Ev::Membership {
                    node: churn.node,
                    live,
                },
            );
        }
        for outage in self.spec.outages.clone() {
            self.engine.at(outage.at, Ev::Outage { node: outage.node });
        }
        let n = self.nodes.len() as u32;
        for storm in self.spec.storms.clone() {
            for i in 0..storm.nodes {
                let node = (storm.first + i) % n;
                let at = storm.at + self.choices.below(storm.window.max(1));
                if let Some(range) = storm.range {
                    let object = storm.object;
                    self.engine.at(
                        at,
                        Ev::Open {
                            node,
                            object,
                            range,
                        },
                    );
                    self.expected_reads += u64::from(self.spec.pages_per_object);
                    continue;
                }
                for number in 0..self.spec.pages_per_object {
                    let page = Page {
                        object: storm.object,
                        number,
                    };
                    let read = Ev::Read {
                        node,
                        page,
                        start: at,
                        attempt: 0,
                    };
                    self.engine.at(at, read);
                    self.expected_reads += 1;
                }
            }
        }
    }

    /// Keep flight waiters expiring while reads run.
    pub(super) fn tick_soon(&mut self) {
        if !self.ticking {
            self.ticking = true;
            self.engine.after(TICK, Ev::Tick);
        }
    }

    pub(super) fn run(&mut self, end: Nanos, max_steps: u64) {
        while let Some(event) = self.engine.pop_until(end) {
            if self.engine.processed() > max_steps {
                break;
            }
            self.step(event);
            self.admit_queued();
        }
    }
}
