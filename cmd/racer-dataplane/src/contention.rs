//! Payload-free, discrete-event pressure model. Only admission is production code;
//! queues, service times, placement, and page ownership are explicit abstractions.
use crate::model::CacheId;
use crate::model::PAGE_BYTES;
use crate::model::ResourceClass;

use crate::runtime::admission::AdmissionPolicy;
use std::collections::BTreeMap;
use std::collections::VecDeque;
use std::sync::Arc;

const PLAIN: usize = PAGE_BYTES as usize;
const CIPHER: usize = PLAIN + 16;
const CONTEXT: usize = 256;
const CLASSES: [ResourceClass; 11] = [
    ResourceClass::Plaintext,
    ResourceClass::Ciphertext,
    ResourceClass::DirtyCiphertext,
    ResourceClass::Registered,
    ResourceClass::RequestContext,
    ResourceClass::Flight,
    ResourceClass::Waiter,
    ResourceClass::Connection,
    ResourceClass::Pipe,
    ResourceClass::ControlProgress,
    ResourceClass::Relay,
];

#[derive(Clone, Debug)]
struct Config {
    nodes: usize,
    workers: usize,
    pages_per_worker: usize,
    dirty_pages: usize,
    pipes: usize,
    queue_entries: usize,
    window: usize,
    network_bytes_per_tick: u64,
    disk_bytes_per_tick: u64,
    reader_bytes_per_tick: u64,
    origin_bytes_per_tick: u64,
    completion_ticks: u64,
    retry_ticks: u64,
    max_attempts: usize,
    max_events: usize,
}
impl Default for Config {
    fn default() -> Self {
        Self {
            nodes: 2000,
            workers: 1,
            pages_per_worker: 8,
            dirty_pages: 2,
            pipes: 8,
            queue_entries: 64,
            window: 2,
            network_bytes_per_tick: 1024 * 1024,
            disk_bytes_per_tick: 512 * 1024,
            reader_bytes_per_tick: 1024 * 1024,
            origin_bytes_per_tick: 16 * 1024 * 1024,
            completion_ticks: 2,
            retry_ticks: 64,
            max_attempts: 3,
            max_events: 2_000_000,
        }
    }
}

#[derive(Clone, Debug)]
struct Request {
    at: u64,
    node: usize,
    cache: usize,
    object: u64,
    first_page: u64,
    pages: usize,
    reader_bytes_per_tick: Option<u64>,
    cancel_after: Option<u64>,
    deadline: u64,
}
impl Request {
    fn new(node: usize, object: u64) -> Self {
        Self {
            at: 0,
            node,
            cache: 0,
            object,
            first_page: 0,
            pages: 4,
            reader_bytes_per_tick: None,
            cancel_after: None,
            deadline: 1_000_000,
        }
    }
}

#[derive(Clone, Debug, Default, PartialEq, Eq)]
struct Report {
    submitted: usize,
    completed: usize,
    failed: usize,
    canceled: usize,
    retries: usize,
    hits: usize,
    fills: usize,
    joined: usize,
    persistence_skips: usize,
    evictions: usize,
    origin_bytes: u64,
    peer_bytes: u64,
    delivered_bytes: u64,
    disk_bytes: u64,
    events: usize,
    peak_events: usize,
    peak_requests: usize,
    peak_logical_bytes: u64,
    peak_worker: [usize; 11],
    rejections: [usize; 11],
    final_used: [usize; 11],
    latency_ticks: Vec<u64>,
    end_tick: u64,
    trace_hash: u64,
}
impl Report {
    fn outcomes(&self) -> (usize, usize, usize) {
        (self.completed, self.failed, self.canceled)
    }
}

type Key = (usize, u64, u64); // cache, object, page

struct Bundle {
    plain: Arc<flow_control::Charge<AdmissionPolicy>>,
    cipher: Arc<flow_control::Charge<AdmissionPolicy>>,
}
impl Bundle {
    fn idle(&self) -> bool {
        Arc::strong_count(&self.plain) == 1 && Arc::strong_count(&self.cipher) == 1
    }
}
struct Flight {
    bundle: Bundle,
    dirty: Option<flow_control::Charge<AdmissionPolicy>>,
    _flight: flow_control::Charge<AdmissionPolicy>,
    _source: Option<Arc<flow_control::Charge<AdmissionPolicy>>>,
    waiters: Vec<(usize, u64, flow_control::Charge<AdmissionPolicy>)>,
}
struct Worker {
    admission: flow_control::Quotas<AdmissionPolicy>,
    cache: BTreeMap<Key, Bundle>,
    lru: VecDeque<Key>,
    flights: BTreeMap<Key, Flight>,
    pipe_queue: VecDeque<usize>,
    sampled: [usize; 11],
}
struct Active {
    request: Request,
    worker: usize,
    next: u64,
    send_next: u64,
    outstanding: usize,
    ready: BTreeMap<u64, Arc<flow_control::Charge<AdmissionPolicy>>>,
    pipe: Option<flow_control::Charge<AdmissionPolicy>>,
    _context: flow_control::Charge<AdmissionPolicy>,
    _connection: flow_control::Charge<AdmissionPolicy>,
    attempts: usize,
    sending_until: Option<u64>,
    queued: bool,
}
enum Event {
    Arrive(usize, Request),
    Pump(usize),
    Fill(usize, Key),
    Sent(usize, Arc<flow_control::Charge<AdmissionPolicy>>),
    Disk(
        usize,
        Arc<flow_control::Charge<AdmissionPolicy>>,
        flow_control::Charge<AdmissionPolicy>,
    ),
    Terminate(usize, bool),
    Release(Active),
}

/// A single FIFO service clock models a work-conserving serialized resource.
/// Transfers occupy source and destination NICs together; this intentionally
/// conservative store-and-forward model is not a fluid network simulator.
#[derive(Default)]
struct Node {
    nic_free: u64,
    disk_free: u64,
}
struct Simulator {
    config: Config,
    workers: Vec<Worker>,
    nodes: Vec<Node>,
    active: BTreeMap<usize, Active>,
    directory: BTreeMap<Key, std::collections::BTreeSet<usize>>,
    partitions: Vec<(usize, u64, u64)>,
    events: BTreeMap<(u64, u64), Event>,
    sequence: u64,
    now: u64,
    origin_free: u64,
    logical_bytes: u64,
    touched: std::collections::BTreeSet<usize>,
    report: Report,
}
impl Simulator {
    fn new(config: Config) -> Self {
        use std::num::NonZeroUsize as Nz;
        assert!(config.nodes > 0 && config.workers > 0 && config.window > 0);
        assert!(config.max_attempts > 0 && config.retry_ticks > 0);
        assert!(config.network_bytes_per_tick > 0 && config.disk_bytes_per_tick > 0);
        assert!(config.origin_bytes_per_tick > 0 && config.reader_bytes_per_tick > 0);
        let workers = (0..config.nodes * config.workers)
            .map(|_| {
                let mut limits = crate::test_support::cluster::config(false).limits;
                limits.plaintext_bytes =
                    Nz::new(config.pages_per_worker.checked_mul(PLAIN).unwrap()).unwrap();
                limits.ciphertext_bytes =
                    Nz::new(config.pages_per_worker.checked_mul(CIPHER).unwrap()).unwrap();
                limits.dirty_bytes =
                    Nz::new(config.dirty_pages.checked_mul(CIPHER).unwrap()).unwrap();
                limits.pipes = Nz::new(config.pipes).unwrap();
                limits.queue_entries = Nz::new(config.queue_entries).unwrap();
                limits.flights = Nz::new(config.pages_per_worker).unwrap();
                limits.waiters_per_flight = Nz::new(config.queue_entries).unwrap();
                limits.client_connections = Nz::new(config.queue_entries + config.pipes).unwrap();
                limits.request_context_bytes =
                    Nz::new((config.queue_entries + config.pipes) * CONTEXT).unwrap();
                limits.metadata_entries =
                    Nz::new(config.pages_per_worker.max(config.queue_entries)).unwrap();
                Worker {
                    admission: flow_control::Quotas::new(AdmissionPolicy::new(limits)),
                    cache: BTreeMap::new(),
                    lru: VecDeque::new(),
                    flights: BTreeMap::new(),
                    pipe_queue: VecDeque::new(),
                    sampled: [0; 11],
                }
            })
            .collect();
        Self {
            nodes: (0..config.nodes).map(|_| Node::default()).collect(),
            config,
            workers,
            active: BTreeMap::new(),
            directory: BTreeMap::new(),
            partitions: Vec::new(),
            events: BTreeMap::new(),
            sequence: 0,
            now: 0,
            origin_free: 0,
            logical_bytes: 0,
            touched: Default::default(),
            report: Report::default(),
        }
    }
    fn partition(&mut self, node: usize, start: u64, end: u64) {
        assert!(node < self.config.nodes && start < end);
        self.partitions.push((node, start, end));
    }
    fn blocked(&self, node: usize) -> bool {
        self.partitions
            .iter()
            .any(|&(n, start, end)| n == node && start <= self.now && self.now < end)
    }
    fn schedule(&mut self, at: u64, event: Event) {
        assert!(at >= self.now);
        self.sequence = self.sequence.checked_add(1).unwrap();
        self.events.insert((at, self.sequence), event);
        self.report.peak_events = self.report.peak_events.max(self.events.len());
    }
    fn reserve(
        &mut self,
        w: usize,
        cache: usize,
        class: ResourceClass,
        amount: usize,
    ) -> Option<flow_control::Charge<AdmissionPolicy>> {
        self.touched.insert(w);
        // Match HttpPool/ConnectionLease and PipePool's global transport charges.
        let cache = match class {
            ResourceClass::Pipe | ResourceClass::Connection => None,
            _ => Some(CacheId(cache.to_string())),
        };
        let result = self.workers[w]
            .admission
            .reserve(cache.as_ref(), class, amount);
        if result.is_err() {
            self.report.rejections[class as usize] += 1;
        }
        result.ok()
    }
    fn evict(&mut self, w: usize, key: Key) {
        self.workers[w].cache.remove(&key);
        self.workers[w].lru.retain(|k| *k != key);
        if let Some(owners) = self.directory.get_mut(&key) {
            owners.remove(&w);
            if owners.is_empty() {
                self.directory.remove(&key);
            }
        }
        self.report.evictions += 1;
        self.touched.insert(w);
    }
    fn reserve_page(
        &mut self,
        w: usize,
        cache: usize,
        class: ResourceClass,
        amount: usize,
    ) -> Option<flow_control::Charge<AdmissionPolicy>> {
        if let Some(r) = self.reserve(w, cache, class, amount) {
            return Some(r);
        }
        // Same cache-local then global diagnosis as Fill::reserve_with_reclamation.
        // Only fully idle bundles are evicted; queued disk writes are modeled as
        // submitted I/O and cannot be discarded before their completion event.
        for _ in 0..2 {
            let (scope, mut deficit) = self.workers[w].admission.reclamation(
                &CacheId(cache.to_string()),
                class,
                amount,
            )?;
            let keys: Vec<_> = self.workers[w].lru.iter().copied().collect();
            for key in keys {
                if scope.as_ref().is_some_and(|c| c.0 != key.0.to_string())
                    || !self.workers[w].cache[&key].idle()
                {
                    continue;
                }
                self.evict(w, key);
                deficit = deficit.saturating_sub(amount);
                if deficit == 0 {
                    break;
                }
            }
            if let Some(r) = self.reserve(w, cache, class, amount) {
                return Some(r);
            }
        }
        None
    }
    fn sample(&mut self) {
        for w in std::mem::take(&mut self.touched) {
            for (i, class) in CLASSES.iter().enumerate() {
                let used = self.workers[w].admission.used(*class);
                assert!(
                    used <= self.workers[w].admission.limit(*class),
                    "worker {w} {class:?}"
                );
                self.report.peak_worker[i] = self.report.peak_worker[i].max(used);
                if i < 5 {
                    self.logical_bytes -= self.workers[w].sampled[i] as u64;
                    self.logical_bytes += used as u64;
                }
                self.workers[w].sampled[i] = used;
            }
        }
        self.report.peak_logical_bytes = self.report.peak_logical_bytes.max(self.logical_bytes);
        self.report.peak_requests = self.report.peak_requests.max(self.active.len());
    }
    fn arrive(&mut self, id: usize, request: Request) {
        let w = request.node * self.config.workers + request.object as usize % self.config.workers;
        let Some(context) = self.reserve(w, request.cache, ResourceClass::RequestContext, CONTEXT)
        else {
            self.report.failed += 1;
            return;
        };
        let Some(connection) = self.reserve(w, request.cache, ResourceClass::Connection, 1) else {
            self.report.failed += 1;
            return;
        };
        let deadline = self.now.checked_add(request.deadline).unwrap();
        self.schedule(deadline, Event::Terminate(id, false));
        if let Some(delay) = request.cancel_after {
            self.schedule(
                self.now.checked_add(delay).unwrap(),
                Event::Terminate(id, true),
            );
        }
        self.active.insert(
            id,
            Active {
                worker: w,
                next: request.first_page,
                send_next: request.first_page,
                request,
                outstanding: 0,
                ready: BTreeMap::new(),
                pipe: None,
                _context: context,
                _connection: connection,
                attempts: 0,
                sending_until: None,
                queued: false,
            },
        );
        self.schedule(self.now, Event::Pump(id));
    }
    fn retry(&mut self, id: usize, mut active: Active) {
        active.attempts += 1;
        if active.attempts >= self.config.max_attempts {
            self.report.failed += 1;
            self.detach_waiters(active.worker, id);
            self.release(id, active);
        } else {
            self.report.retries += 1;
            self.active.insert(id, active);
            self.schedule(self.now + self.config.retry_ticks, Event::Pump(id));
        }
    }
    fn release(&mut self, id: usize, mut active: Active) {
        if active.queued {
            let w = active.worker;
            let was_head = self.workers[w].pipe_queue.front() == Some(&id);
            self.workers[w].pipe_queue.retain(|queued| *queued != id);
            active.queued = false;
            if was_head {
                self.wake_pipe(w);
            }
        }
        let until = active
            .sending_until
            .unwrap_or(self.now)
            .max(self.now + self.config.completion_ticks);
        self.schedule(until, Event::Release(active));
    }
    fn detach_waiters(&mut self, w: usize, id: usize) {
        // Registrations belong to the request; bundles and source pins belong to
        // the flight until Fill completes, even when its last waiter detaches.
        for flight in self.workers[w].flights.values_mut() {
            flight.waiters.retain(|(waiter, _, _)| *waiter != id);
        }
        self.touched.insert(w);
    }
    fn terminate(&mut self, id: usize, canceled: bool) {
        if let Some(active) = self.active.remove(&id) {
            if canceled {
                self.report.canceled += 1;
            } else {
                self.report.failed += 1;
            }
            self.detach_waiters(active.worker, id);
            self.release(id, active);
        }
    }
    fn wake_pipe(&mut self, w: usize) {
        if self.workers[w].admission.used(ResourceClass::Pipe)
            >= self.workers[w].admission.limit(ResourceClass::Pipe)
        {
            return;
        }
        // Notification does not relinquish FIFO position. Only acquisition or
        // cancellation removes the head, just like PipePool's Waiting guard.
        if let Some(&id) = self.workers[w].pipe_queue.front() {
            self.schedule(self.now, Event::Pump(id));
        }
    }
    fn pump(&mut self, id: usize) {
        let Some(mut active) = self.active.remove(&id) else {
            return;
        };
        let w = active.worker;
        let cache = active.request.cache;
        self.touched.insert(w);
        if active.pipe.is_none() {
            let head = self.workers[w].pipe_queue.front().copied();
            if head.is_none() || head == Some(id) {
                active.pipe = self.reserve(w, cache, ResourceClass::Pipe, 1);
            }
            if active.pipe.is_none() {
                if !active.queued {
                    if self.workers[w].pipe_queue.len() >= self.config.queue_entries {
                        self.report.failed += 1;
                        self.release(id, active);
                        return;
                    }
                    active.queued = true;
                    self.workers[w].pipe_queue.push_back(id);
                }
                self.active.insert(id, active);
                return;
            }
            if active.queued {
                assert_eq!(self.workers[w].pipe_queue.pop_front(), Some(id));
                active.queued = false;
                // Several pipes may have been released before this pump ran.
                self.wake_pipe(w);
            }
        }
        let end = active.request.first_page + active.request.pages as u64;
        let mut pressure = false;
        while active.next < end && active.outstanding < self.config.window {
            let page = active.next;
            let key = (cache, active.request.object, page);
            if let Some(bundle) = self.workers[w].cache.get(&key) {
                active.ready.insert(page, bundle.plain.clone());
                self.workers[w].lru.retain(|k| *k != key);
                self.workers[w].lru.push_back(key);
                self.report.hits += 1;
            } else if self.workers[w].flights.contains_key(&key) {
                if self.workers[w].flights[&key].waiters.len() >= self.config.queue_entries {
                    pressure = true;
                    break;
                }
                let Some(waiter) = self.reserve(w, cache, ResourceClass::Waiter, 1) else {
                    pressure = true;
                    break;
                };
                self.workers[w]
                    .flights
                    .get_mut(&key)
                    .unwrap()
                    .waiters
                    .push((id, page, waiter));
                self.report.joined += 1;
            } else {
                if self.blocked(active.request.node) {
                    pressure = true;
                    break;
                }
                let Some(plain) = self.reserve_page(w, cache, ResourceClass::Plaintext, PLAIN)
                else {
                    pressure = true;
                    break;
                };
                let Some(cipher) = self.reserve_page(w, cache, ResourceClass::Ciphertext, CIPHER)
                else {
                    pressure = true;
                    break;
                };
                let Some(flight) = self.reserve(w, cache, ResourceClass::Flight, 1) else {
                    pressure = true;
                    break;
                };
                let Some(waiter) = self.reserve(w, cache, ResourceClass::Waiter, 1) else {
                    pressure = true;
                    break;
                };
                let dirty = self.reserve(w, cache, ResourceClass::DirtyCiphertext, CIPHER);
                if dirty.is_none() {
                    self.report.persistence_skips += 1;
                }
                let source = self.directory.get(&key).and_then(|owners| {
                    owners
                        .iter()
                        .copied()
                        .find(|owner| !self.blocked(*owner / self.config.workers))
                });
                let pin = source.map(|owner| self.workers[owner].cache[&key].cipher.clone());
                let node = active.request.node;
                let start = self.now.max(self.nodes[node].nic_free);
                let finish = if let Some(owner) = source {
                    let source_node = owner / self.config.workers;
                    let finish = start.max(self.nodes[source_node].nic_free)
                        + (CIPHER as u64).div_ceil(self.config.network_bytes_per_tick);
                    self.nodes[source_node].nic_free = finish;
                    self.report.peer_bytes += CIPHER as u64;
                    finish
                } else {
                    self.origin_free = self.now.max(self.origin_free)
                        + (CIPHER as u64).div_ceil(self.config.origin_bytes_per_tick);
                    let finish = start.max(self.origin_free)
                        + (CIPHER as u64).div_ceil(self.config.network_bytes_per_tick);
                    self.report.origin_bytes += CIPHER as u64;
                    finish
                };
                self.nodes[node].nic_free = finish;
                self.workers[w].flights.insert(
                    key,
                    Flight {
                        bundle: Bundle {
                            plain: Arc::new(plain),
                            cipher: Arc::new(cipher),
                        },
                        dirty,
                        _flight: flight,
                        _source: pin,
                        waiters: vec![(id, page, waiter)],
                    },
                );
                self.report.fills += 1;
                self.schedule(finish + self.config.completion_ticks, Event::Fill(w, key));
            }
            active.next += 1;
            active.outstanding += 1;
            active.attempts = 0;
        }
        if active.sending_until.is_none() {
            if let Some(plain) = active.ready.remove(&active.send_next) {
                let rate = active
                    .request
                    .reader_bytes_per_tick
                    .unwrap_or(self.config.reader_bytes_per_tick);
                let node = active.request.node;
                let finish = self.now.max(self.nodes[node].nic_free)
                    + PAGE_BYTES
                        .div_ceil(rate)
                        .max(PAGE_BYTES.div_ceil(self.config.network_bytes_per_tick))
                    + self.config.completion_ticks;
                self.nodes[node].nic_free = finish;
                active.sending_until = Some(finish);
                self.schedule(finish, Event::Sent(id, plain));
            }
        }
        // Progress events will retry prefetch pressure; do not fail a request
        // merely because its own bounded window currently pins every buffer.
        if pressure && active.outstanding == 0 {
            self.retry(id, active);
        } else {
            self.active.insert(id, active);
        }
    }
    fn fill(&mut self, w: usize, key: Key) {
        let flight = self.workers[w].flights.remove(&key).unwrap();
        self.touched.insert(w);
        if let Some(dirty) = flight.dirty {
            let node = w / self.config.workers;
            let finish = self.now.max(self.nodes[node].disk_free)
                + (CIPHER as u64).div_ceil(self.config.disk_bytes_per_tick);
            self.nodes[node].disk_free = finish;
            self.report.disk_bytes += CIPHER as u64;
            self.schedule(finish, Event::Disk(w, flight.bundle.cipher.clone(), dirty));
        }
        for (id, page, _waiter) in flight.waiters {
            if let Some(active) = self.active.get_mut(&id) {
                active.ready.insert(page, flight.bundle.plain.clone());
                self.schedule(self.now, Event::Pump(id));
            }
        }
        self.workers[w].cache.insert(key, flight.bundle);
        self.workers[w].lru.push_back(key);
        self.directory.entry(key).or_default().insert(w);
    }
    fn run(&mut self, requests: Vec<Request>) -> Report {
        assert_eq!(self.report.submitted, 0, "simulators are single-use");
        self.report.submitted = requests.len();
        for (id, request) in requests.into_iter().enumerate() {
            assert!(request.node < self.config.nodes && request.pages > 0);
            assert!(request.reader_bytes_per_tick != Some(0));
            request
                .first_page
                .checked_add(request.pages as u64)
                .unwrap();
            self.schedule(request.at, Event::Arrive(id, request));
        }
        while let Some(((at, sequence), event)) = self.events.pop_first() {
            if matches!(&event, Event::Terminate(id, _) if !self.active.contains_key(id)) {
                continue;
            }
            self.now = at;
            self.report.events += 1;
            assert!(
                self.report.events < self.config.max_events,
                "contention event budget exhausted"
            );
            self.report.trace_hash =
                self.report.trace_hash.wrapping_mul(0x100000001b3) ^ at ^ sequence.rotate_left(17);
            match event {
                Event::Arrive(id, request) => self.arrive(id, request),
                Event::Pump(id) => self.pump(id),
                Event::Fill(w, key) => self.fill(w, key),
                Event::Sent(id, plain) => {
                    if let Some(mut active) = self.active.remove(&id) {
                        self.touched.insert(active.worker);
                        active.sending_until = None;
                        active.send_next += 1;
                        active.outstanding -= 1;
                        self.report.delivered_bytes += PAGE_BYTES;
                        if active.send_next
                            == active.request.first_page + active.request.pages as u64
                        {
                            self.report.completed += 1;
                            self.report.latency_ticks.push(self.now - active.request.at);
                            self.release(id, active);
                        } else {
                            self.active.insert(id, active);
                            self.schedule(self.now, Event::Pump(id));
                        }
                    }
                    drop(plain);
                }
                Event::Disk(w, cipher, dirty) => {
                    drop((cipher, dirty));
                    self.touched.insert(w);
                }
                Event::Terminate(id, canceled) => self.terminate(id, canceled),
                Event::Release(active) => {
                    let w = active.worker;
                    let had_pipe = active.pipe.is_some();
                    drop(active);
                    self.touched.insert(w);
                    if had_pipe {
                        self.wake_pipe(w);
                    }
                }
            }
            self.sample();
        }
        assert!(self.active.is_empty());
        for worker in &mut self.workers {
            assert!(worker.flights.is_empty());
            assert!(worker.cache.values().all(Bundle::idle));
            worker.cache.clear();
            worker.lru.clear();
            assert!(worker.pipe_queue.is_empty());
            for (i, class) in CLASSES.iter().enumerate() {
                self.report.final_used[i] += worker.admission.used(*class);
            }
        }
        self.directory.clear();
        self.report.end_tick = self.now;
        assert_eq!(self.report.final_used, [0; 11]);
        let mut latency = self.report.latency_ticks.clone();
        latency.sort_unstable();
        let percentile = |p: usize| {
            latency
                .get(latency.len().saturating_sub(1) * p / 100)
                .copied()
                .unwrap_or(0)
        };
        println!(
            "contention nodes={} workers={} submitted={} completed={} failed={} canceled={} ticks={} events={} peak_events={} logical_peak_bytes={} p50={} p95={} p99={} fills={} hits={} joined={} retries={} skips={} origin_bytes={} peer_bytes={} delivered_bytes={} disk_bytes={} peak_worker={:?} rejections={:?} trace={:016x}",
            self.config.nodes,
            self.config.workers,
            self.report.submitted,
            self.report.completed,
            self.report.failed,
            self.report.canceled,
            self.report.end_tick,
            self.report.events,
            self.report.peak_events,
            self.report.peak_logical_bytes,
            percentile(50),
            percentile(95),
            percentile(99),
            self.report.fills,
            self.report.hits,
            self.report.joined,
            self.report.retries,
            self.report.persistence_skips,
            self.report.origin_bytes,
            self.report.peer_bytes,
            self.report.delivered_bytes,
            self.report.disk_bytes,
            self.report.peak_worker,
            self.report.rejections,
            self.report.trace_hash
        );
        self.report.clone()
    }
}

mod waiter_detach {
    use super::*;
    use crate::error::Error;
    use crate::model::CacheKey;
    use crate::model::MembershipVersion;
    use crate::model::ObjectId;
    use crate::model::ObjectVersion;
    use crate::model::OriginContext;
    use crate::model::PageId;
    use crate::model::PageNumber;
    use crate::model::RequestId;
    use crate::model::StrongEtag;
    use crate::read::flight::AcquisitionBudget;
    use crate::read::flight::AcquisitionEvent;
    use crate::read::flight::AcquisitionFailure;
    use crate::read::flight::Flights;
    use crate::read::flight::JoinedFlight;
    use crate::runtime::deadline::RequestScope;
    use crate::topology::Membership;
    use std::rc::Rc;
    use std::task::Context;
    use std::task::Poll;
    use std::time::Duration;
    use std::time::Instant;

    fn config() -> Config {
        Config {
            nodes: 2,
            workers: 1,
            pages_per_worker: 2,
            queue_entries: 2,
            pipes: 4,
            window: 2,
            ..Config::default()
        }
    }
    fn request() -> Request {
        Request {
            pages: 2,
            ..Request::new(0, 0)
        }
    }
    fn waiters(sim: &Simulator) -> usize {
        sim.workers[0].admission.used(ResourceClass::Waiter)
    }
    fn join<'a>(
        flights: &Rc<Flights>,
        page: &PageId,
        origin: &'a OriginContext,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
    ) -> crate::error::Result<JoinedFlight<'a>> {
        let membership = Arc::new(Membership::validate(MembershipVersion(1), vec![]).unwrap());
        flights.join(page.clone(), membership, origin, scope, budget)
    }

    #[test]
    fn terminal_waiter_replacement_matches_production_before_completion() {
        for canceled in [true, false] {
            let mut sim = Simulator::new(config());
            for id in [0, 1] {
                sim.arrive(id, request());
                sim.pump(id);
            }
            let real = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                sim.workers[0].admission.policy().limits().clone(),
            )));
            let flights = Rc::new(Flights::new(
                real.clone(),
                crate::test_support::availability(),
            ));
            let page = PageId {
                version: ObjectVersion {
                    object: ObjectId {
                        cache: CacheId(crate::security::test_support::CACHE.into()),
                        key: CacheKey([0; 32]),
                    },
                    etag: StrongEtag::test_value("v1"),
                },
                number: PageNumber(0),
            };
            let origin = OriginContext {
                object: page.version.object.clone(),
                metadata: None,
                authorization: None,
            };
            let deadline = Instant::now() + Duration::from_secs(60);
            let scope = RequestScope::new(RequestId([0; 16]), deadline).unwrap();
            let terminal_scope = RequestScope::new(RequestId([1; 16]), deadline).unwrap();
            let mut budgets =
                std::array::from_fn::<_, 3, _>(|_| AcquisitionBudget::new(deadline, 3, 8));
            let [first_budget, terminal_budget, replacement_budget] = &mut budgets;
            let JoinedFlight::Waiter(mut first) =
                join(&flights, &page, &origin, &scope, first_budget).unwrap()
            else {
                panic!("expected first registration")
            };
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            let Poll::Ready(Ok(AcquisitionEvent::Lead(leader))) =
                first.wait().as_mut().poll(&mut cx)
            else {
                panic!("expected first acquisition")
            };
            let fill_reservation =
                crate::runtime::admission::reserve_fill(&real, &origin.object.cache, true).unwrap();
            let operation = flights
                .retain_operation(
                    &leader,
                    crate::telemetry::Metrics::default()
                        .lease(crate::telemetry::Gauge::ActiveFills)
                        .unwrap(),
                )
                .unwrap();
            let JoinedFlight::Waiter(mut terminal) =
                join(&flights, &page, &origin, &terminal_scope, terminal_budget).unwrap()
            else {
                panic!("expected second registration")
            };
            assert!(terminal.wait().as_mut().poll(&mut cx).is_pending());
            assert_eq!(real.used(ResourceClass::Waiter), 2);
            assert_eq!(waiters(&sim), 4, "two registrations per page");
            assert!(matches!(
                join(&flights, &page, &origin, &scope, replacement_budget),
                Err(Error::Overloaded)
            ));
            if canceled {
                terminal_scope.cancel().unwrap();
            }
            drop(terminal);
            sim.now = 1;
            sim.terminate(1, canceled);
            sim.sample();
            assert_eq!(real.used(ResourceClass::Waiter), 1);
            assert_eq!(waiters(&sim), 2);
            assert_eq!(sim.workers[0].sampled[ResourceClass::Waiter as usize], 2);
            for (class, bytes) in [
                (ResourceClass::Plaintext, PLAIN),
                (ResourceClass::Ciphertext, CIPHER),
                (ResourceClass::DirtyCiphertext, CIPHER),
            ] {
                assert_eq!(real.used(class), bytes);
                assert_eq!(sim.workers[0].admission.used(class), 2 * bytes);
            }
            assert_eq!(real.used(ResourceClass::ControlProgress), 1);
            assert_eq!(real.used(ResourceClass::Flight), 1);
            assert!(!operation.cancellation_requested());
            assert_eq!(sim.workers[0].admission.used(ResourceClass::Flight), 2);
            assert_eq!(
                sim.workers[0].admission.used(ResourceClass::RequestContext),
                2 * CONTEXT
            );
            let JoinedFlight::Waiter(mut replacement) =
                join(&flights, &page, &origin, &scope, replacement_budget).unwrap()
            else {
                panic!("replacement must join before completion")
            };
            assert!(replacement.wait().as_mut().poll(&mut cx).is_pending());
            sim.arrive(2, request());
            sim.pump(2);
            assert_eq!(real.used(ResourceClass::Waiter), 2);
            assert_eq!(waiters(&sim), 4);
            assert_eq!(
                (sim.report.fills, sim.report.joined, sim.report.retries),
                (2, 4, 0)
            );
            assert_eq!(sim.active[&2].outstanding, 2);
            assert!(sim.active[&2].ready.is_empty());
            for flight in sim.workers[0].flights.values() {
                assert_eq!(
                    flight.waiters.iter().map(|w| w.0).collect::<Vec<_>>(),
                    vec![0, 2]
                );
            }
            assert_eq!(
                sim.events
                    .values()
                    .filter(|event| matches!(event, Event::Fill(..)))
                    .count(),
                2
            );
            operation.complete().unwrap();
            drop(fill_reservation);
            flights
                .fail(leader, AcquisitionFailure::Terminal(Error::Io))
                .unwrap();
            drop((first, replacement));
            assert!(CLASSES.iter().all(|class| real.used(*class) == 0));
        }
    }

    #[test]
    fn last_waiter_detach_keeps_peer_source_and_completion_owners() {
        let mut sim = Simulator::new(config());
        let key = (0, 0, 0);
        let source = Bundle {
            plain: Arc::new(sim.reserve(1, 0, ResourceClass::Plaintext, PLAIN).unwrap()),
            cipher: Arc::new(
                sim.reserve(1, 0, ResourceClass::Ciphertext, CIPHER)
                    .unwrap(),
            ),
        };
        let pin = Arc::downgrade(&source.cipher);
        sim.workers[1].cache.insert(key, source);
        sim.directory.entry(key).or_default().insert(1);
        sim.arrive(
            0,
            Request {
                pages: 1,
                ..request()
            },
        );
        sim.pump(0);
        assert_eq!(pin.strong_count(), 2);
        sim.config.max_attempts = 2;
        for expected in [1, 0] {
            let active = sim.active.remove(&0).unwrap();
            sim.retry(0, active);
            assert_eq!(waiters(&sim), expected);
        }
        assert!(sim.workers[0].flights[&key].waiters.is_empty());
        assert_eq!(pin.strong_count(), 2);
        for (class, amount) in [
            (ResourceClass::Plaintext, PLAIN),
            (ResourceClass::Ciphertext, CIPHER),
            (ResourceClass::DirtyCiphertext, CIPHER),
            (ResourceClass::Flight, 1),
        ] {
            assert_eq!(sim.workers[0].admission.used(class), amount);
        }
        assert!(
            sim.events
                .values()
                .any(|event| matches!(event, Event::Fill(0, k) if *k == key))
        );
        assert!(
            sim.events
                .values()
                .any(|event| matches!(event, Event::Release(_)))
        );
        sim.detach_waiters(0, 0);
        assert_eq!(waiters(&sim), 0, "repeat detach is harmless");
    }

    #[test]
    fn replacement_reads_finish_after_cancellation_or_deadline_detaches_waiters() {
        for canceled in [true, false] {
            let mut terminal = request();
            if canceled {
                terminal.cancel_after = Some(1);
            } else {
                terminal.deadline = 1;
            }
            let replacement = Request { at: 2, ..request() };
            let report = Simulator::new(config()).run(vec![request(), terminal, replacement]);
            assert_eq!(
                report.outcomes(),
                (2, usize::from(!canceled), usize::from(canceled))
            );
            assert_eq!((report.fills, report.joined, report.retries), (2, 4, 0));
            assert_eq!(report.final_used, [0; 11]);
        }
    }
}

mod fidelity {
    //! Allocation-backed oracles for the simulator's Bundle and page reclamation.
    //! Dirty Fill and crypto checks below cover production contracts against explicit
    //! Bundle owner traces, not the simulator's event scheduling or completion timing.
    use super::*;
    use crate::read::dispatch::WorkerMap;
    use crate::error::Operation;
    use crate::memory::BufferPool;
    use crate::memory::VerifiedBytes;
    use crate::memory::VerifiedPage;
    use crate::memory::cache::MemoryCache;
    use crate::memory::page::PageResult;
    use crate::model::CacheKey;
    use crate::model::MetadataSelector;
    use crate::model::Nonce;
    use crate::model::ObjectId;
    use crate::model::ObjectVersion;
    use crate::model::OriginContext;
    use crate::model::PageEnvelope;
    use crate::model::PageId;
    use crate::model::PageNumber;
    use crate::model::RequestId;
    use crate::model::StrongEtag;
    use crate::model::VersionMetadata;
    use crate::model::WorkerId;
    use crate::origin::MetadataReply;
    use crate::origin::Origin;
    use crate::origin::OriginPage;
    use crate::read::candidates::CandidatePolicy;
    use crate::read::candidates::OriginAuthority;
    use crate::read::dispatch::WorkerDirectory;
    use crate::read::fill::Fill;
    use crate::read::fill::FillDependencies;
    use crate::read::flight::AcquisitionBudget;
    use crate::read::flight::Flights;
    use crate::runtime::crypto;
    use crate::runtime::crypto::CryptoClient;
    use crate::runtime::deadline::RequestScope;
    use crate::runtime::reactor::Reactor;
    use crate::runtime::worker::CryptoRuntime;
    use crate::runtime::worker::CryptoService;
    use crate::security::aead::PageCrypto;
    use crate::security::aead::PageCryptoEngine;
    use crate::security::credentials::CredentialCrypto;
    use crate::store::StoreReader;
    use crate::store::StoreWriter;
    use crate::store::catalog::Index;
    use crate::store::catalog::SegmentClock;
    use crate::topology::Member;
    use crate::topology::Membership;
    use crate::topology::Placement;
    use std::num::NonZeroU32;
    use std::num::NonZeroUsize;
    use std::rc::Rc;
    use std::task::Context;
    use std::time::Duration;
    use std::time::Instant;
    use uring_runtime::reactor::IoBuffer;

    // The abstract side owns no payload. Production pages put the same non-cloneable
    // charges inside Arc<VerifiedBytes>/Arc<CiphertextBytes> (memory/pool.rs:41-58).
    struct MetadataOwners {
        bundle: super::Bundle,
    }

    impl MetadataOwners {
        fn reserve(admission: &flow_control::Quotas<AdmissionPolicy>, cache: &CacheId) -> Self {
            let reserved = admission.reserve_fill(cache, false).unwrap();
            Self {
                bundle: super::Bundle {
                    plain: Arc::new(reserved.plaintext),
                    cipher: Arc::new(reserved.ciphertext),
                },
            }
        }
    }

    impl Clone for MetadataOwners {
        fn clone(&self) -> Self {
            Self {
                bundle: super::Bundle {
                    plain: self.bundle.plain.clone(),
                    cipher: self.bundle.cipher.clone(),
                },
            }
        }
    }

    fn admission(pages: usize) -> Rc<flow_control::Quotas<AdmissionPolicy>> {
        let mut limits = crate::test_support::cluster::config(false).limits;
        limits.plaintext_bytes = NonZeroUsize::new(pages * PLAIN).unwrap();
        limits.ciphertext_bytes = NonZeroUsize::new(pages * CIPHER).unwrap();
        limits.dirty_bytes = NonZeroUsize::new(CIPHER).unwrap();
        limits.metadata_entries = NonZeroUsize::new(16).unwrap();
        Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits)))
    }

    fn descriptor(cache: &CacheId, version: &str, length: usize) -> VersionMetadata {
        VersionMetadata {
            content_type: None,
            version: ObjectVersion {
                object: ObjectId {
                    cache: cache.clone(),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value(version),
            },
            length: length as u64,
        }
    }

    fn page_id(metadata: &VersionMetadata) -> PageId {
        PageId {
            version: metadata.version.clone(),
            number: PageNumber(0),
        }
    }

    fn allocated_page(
        admission: &Rc<flow_control::Quotas<AdmissionPolicy>>,
        cache: &CacheId,
        version: &str,
        length: usize,
    ) -> PageResult {
        let metadata = descriptor(cache, version, length);
        let id = page_id(&metadata);
        let reserved = admission.reserve_fill(cache, false).unwrap();
        let pool = BufferPool::new(admission.clone());
        let mut plaintext = pool.plaintext(reserved.plaintext, length).unwrap();
        plaintext.bytes_mut().unwrap().fill(7);
        let (bytes, reservation) = plaintext.into_parts();
        // Structural fixture only: use the production allocation/owner layout, not
        // an authentication oracle (security/aead.rs:267-280 performs this transfer).
        let plaintext = VerifiedPage {
            inner: Arc::new(VerifiedBytes {
                page: id.clone(),
                bytes: bytes.into_vec(),
                reservation,
            }),
        };
        let ciphertext = pool
            .ciphertext(
                reserved.ciphertext,
                PageEnvelope {
                    page: id,
                    key_id: crate::model::key_id_from_generation(1, 1).unwrap(),
                    nonce: Nonce([2; 24]),
                    plaintext_length: length as u32,
                    ciphertext_length: (length + 16) as u32,
                },
                vec![9; length + 16],
            )
            .unwrap();
        PageResult {
            metadata: metadata.for_pin(),
            plaintext,
            ciphertext,
        }
    }

    fn occupancy(admission: &flow_control::Quotas<AdmissionPolicy>) -> [usize; 3] {
        admission.reclaim_buffers();
        [
            ResourceClass::Plaintext,
            ResourceClass::Ciphertext,
            ResourceClass::DirtyCiphertext,
        ]
        .map(|class| admission.used(class))
    }

    fn checkpoint(
        label: &str,
        model: &flow_control::Quotas<AdmissionPolicy>,
        real: &flow_control::Quotas<AdmissionPolicy>,
        expected: [usize; 3],
    ) {
        assert_eq!(occupancy(model), expected, "abstract: {label}");
        assert_eq!(occupancy(real), expected, "production: {label}");
    }

    #[test]
    fn duplicate_owners_match_full_page_occupancy_until_each_last_owner() {
        assert_eq!((PLAIN, CIPHER), (16 * 1024 * 1024, 16 * 1024 * 1024 + 16));
        let model = admission(2);
        let real = admission(2);
        let cache = CacheId(crate::security::test_support::CACHE.into());
        let memory = MemoryCache::new(
            BufferPool::new(real.clone()),
            crate::test_support::availability(),
        );
        let retained = MetadataOwners::reserve(&model, &cache);
        let page = allocated_page(&real, &cache, "v1", PLAIN);
        let id = page.plaintext.page().clone();
        memory.publish(page).unwrap();
        assert!(retained.bundle.idle(), "published bundle has no readers");
        checkpoint("publish", &model, &real, [PLAIN, CIPHER, 0]);

        let duplicate = retained.clone();
        assert!(!retained.bundle.idle());
        assert!(!duplicate.bundle.idle());
        let read = memory.get(&id).unwrap().unwrap();
        let again = memory.get(&id).unwrap().unwrap();
        assert!(Arc::ptr_eq(&read.plaintext.inner, &again.plaintext.inner));
        assert!(Arc::ptr_eq(&read.ciphertext.inner, &again.ciphertext.inner));
        drop(again);
        checkpoint(
            "duplicate reader charges once",
            &model,
            &real,
            [PLAIN, CIPHER, 0],
        );
        assert!(!retained.bundle.idle());
        assert_eq!(memory.evict_idle(usize::MAX), Ok(0));

        // Either allocation protects the whole bundle, memory/cache.rs:184-186.
        drop(duplicate.bundle.plain);
        drop(read.plaintext);
        assert!(!retained.bundle.idle());
        assert_eq!(memory.evict_idle(usize::MAX), Ok(0));
        checkpoint("ciphertext-only reader", &model, &real, [PLAIN, CIPHER, 0]);
        drop(duplicate.bundle.cipher);
        drop(read.ciphertext);
        assert!(retained.bundle.idle(), "last ciphertext reader released");
        checkpoint("readers released", &model, &real, [PLAIN, CIPHER, 0]);
        let plain_owner = retained.bundle.plain.clone();
        let plain = memory.get(&id).unwrap().unwrap().plaintext;
        assert!(!retained.bundle.idle());
        assert_eq!(memory.evict_idle(usize::MAX), Ok(0));
        checkpoint("plaintext-only reader", &model, &real, [PLAIN, CIPHER, 0]);

        // Lookup removal is not a completion fence, memory/cache.rs:148-180.
        let last_plain = plain_owner.clone();
        let last_real_plain = plain.clone();
        assert!(
            !retained.bundle.idle(),
            "live readers before lookup removal"
        );
        drop(retained);
        memory.remove_cache(&cache).unwrap();
        assert!(memory.get(&id).unwrap().is_none());
        checkpoint(
            "cache removed while readers live",
            &model,
            &real,
            [PLAIN, 0, 0],
        );
        drop((plain_owner, plain));
        checkpoint("one reader remains", &model, &real, [PLAIN, 0, 0]);
        assert_eq!(last_real_plain.bytes().len(), PLAIN);
        assert_eq!(last_real_plain.bytes()[PLAIN - 1], 7);
        drop((last_plain, last_real_plain));
        checkpoint("last owner released", &model, &real, [0, 0, 0]);
    }

    #[test]
    fn fair_reclamation_matches_metadata_trace_and_keeps_other_cache() {
        // Use full pages for the fixed-page simulator's reclamation oracle.
        for class in [ResourceClass::Plaintext, ResourceClass::Ciphertext] {
            let mut model = Simulator::new(Config {
                nodes: 1,
                workers: 1,
                pages_per_worker: 4,
                dirty_pages: 1,
                queue_entries: 16,
                ..Config::default()
            });
            let real = admission(4);
            // The simulator uses numeric IDs; the real cache uses published UUIDs.
            let a = CacheId(crate::security::test_support::CACHE.into());
            let b = CacheId("44444444-4444-4444-8444-444444444444".into());
            let memory = MemoryCache::new(
                BufferPool::new(real.clone()),
                crate::test_support::availability_for(vec![a.clone(), b.clone()]),
            );
            let keys = [(1, 0, 0), (0, 1, 0), (0, 2, 0)];
            let mut ids = Vec::new();
            for (key, (cache, version)) in
                keys.into_iter()
                    .zip([(&b, "other"), (&a, "old"), (&a, "new")])
            {
                let bundle = Bundle {
                    plain: Arc::new(
                        model
                            .reserve_page(0, key.0, ResourceClass::Plaintext, PLAIN)
                            .unwrap(),
                    ),
                    cipher: Arc::new(
                        model
                            .reserve_page(0, key.0, ResourceClass::Ciphertext, CIPHER)
                            .unwrap(),
                    ),
                };
                assert!(bundle.idle());
                // Seed retained pages without scheduling network/disk service. All
                // reservation, idle selection, and eviction run the shared model code.
                model.workers[0].cache.insert(key, bundle);
                model.workers[0].lru.push_back(key);
                model.directory.entry(key).or_default().insert(0);
                let page = allocated_page(&real, cache, version, PLAIN);
                ids.push(page.plaintext.page().clone());
                memory.publish(page).unwrap();
            }
            checkpoint(
                "three retained final pages",
                &model.workers[0].admission,
                &real,
                [3 * PLAIN, 3 * CIPHER, 0],
            );
            let amount = if matches!(class, ResourceClass::Plaintext) {
                PLAIN
            } else {
                CIPHER
            };
            assert_eq!(model.report.evictions, 0);
            assert!(model.workers[0].cache.values().all(Bundle::idle));
            for (admission, a) in [
                (&model.workers[0].admission, &CacheId("0".into())),
                (real.as_ref(), &a),
            ] {
                assert!(matches!(
                    admission.reserve(Some(a), class, amount),
                    Err(flow_control::Error::Overloaded)
                ));
                // Local deficit takes precedence over global spare room,
                // runtime/admission.rs:149-156. Evicting B cannot remedy A's share.
                let (owner, deficit) = admission.reclamation(a, class, amount).unwrap();
                assert_eq!(owner, Some(a.clone()));
                assert!(deficit > 0 && deficit <= amount);
            }
            checkpoint(
                "failed reservation is unchanged",
                &model.workers[0].admission,
                &real,
                [3 * PLAIN, 3 * CIPHER, 0],
            );
            // Either kind of external owner must prevent reserve_page from evicting
            // A's pages, even though B is idle and global capacity remains available.
            let pins: Vec<_> = keys[1..]
                .iter()
                .map(|key| {
                    let bundle = &model.workers[0].cache[key];
                    if matches!(class, ResourceClass::Plaintext) {
                        bundle.plain.clone()
                    } else {
                        bundle.cipher.clone()
                    }
                })
                .collect();
            let reads: Vec<_> = ids[1..]
                .iter()
                .map(|id| {
                    let page = memory.get(id).unwrap().unwrap();
                    if matches!(class, ResourceClass::Plaintext) {
                        (Some(page.plaintext), None)
                    } else {
                        (None, Some(page.ciphertext))
                    }
                })
                .collect();
            assert!(model.workers[0].cache[&keys[0]].idle());
            assert!(
                keys[1..]
                    .iter()
                    .all(|key| !model.workers[0].cache[key].idle())
            );
            assert!(model.reserve_page(0, 0, class, amount).is_none());
            assert_eq!(memory.reclaim_idle(class, Some(&a), amount, |_| 0), 0);
            assert_eq!(model.report.evictions, 0);
            assert_eq!(
                model.workers[0].lru.iter().copied().collect::<Vec<_>>(),
                keys
            );
            assert_eq!(model.workers[0].cache.len(), 3);
            assert_eq!(model.directory.len(), 3);
            checkpoint(
                "busy requesting-cache pages survive",
                &model.workers[0].admission,
                &real,
                [3 * PLAIN, 3 * CIPHER, 0],
            );
            drop((pins, reads));
            assert!(model.workers[0].cache.values().all(Bundle::idle));

            let model_next = model
                .reserve_page(0, 0, class, amount)
                .expect("reclaim and retry must admit one page");
            assert_eq!(memory.reclaim_idle(class, Some(&a), amount, |_| 0), amount);
            assert_eq!(occupancy(&real), [2 * PLAIN, 2 * CIPHER, 0]);
            assert!(memory.get(&ids[1]).unwrap().is_none());
            assert!(memory.get(&ids[0]).unwrap().is_some());
            assert!(memory.get(&ids[2]).unwrap().is_some());
            assert_eq!(model.report.evictions, 1);
            assert!(!model.workers[0].cache.contains_key(&keys[1]));
            assert_eq!(model.workers[0].cache.len(), 2);
            assert_eq!(
                model.workers[0].lru.iter().copied().collect::<Vec<_>>(),
                [keys[0], keys[2]]
            );
            assert!(!model.directory.contains_key(&keys[1]));
            assert_eq!(model.directory.len(), 2);
            for key in [keys[0], keys[2]] {
                assert!(model.workers[0].cache[&key].idle());
                assert_eq!(
                    model.directory[&key].iter().copied().collect::<Vec<_>>(),
                    [0]
                );
            }
            let real_next = real.reserve(Some(&a), class, amount).unwrap();
            let expected = if matches!(class, ResourceClass::Plaintext) {
                [3 * PLAIN, 2 * CIPHER, 0]
            } else {
                [2 * PLAIN, 3 * CIPHER, 0]
            };
            checkpoint(
                "retry admitted",
                &model.workers[0].admission,
                &real,
                expected,
            );
            drop((model_next, real_next));
            assert!(model.workers[0].cache.values().all(Bundle::idle));
            checkpoint(
                "only oldest requesting-cache bundle reclaimed",
                &model.workers[0].admission,
                &real,
                [2 * PLAIN, 2 * CIPHER, 0],
            );
            for key in [keys[0], keys[2]] {
                model.evict(0, key);
            }
            assert!(model.workers[0].cache.is_empty());
            assert!(model.workers[0].lru.is_empty());
            assert!(model.directory.is_empty());
            assert_eq!(model.report.evictions, 3);
            assert_eq!(memory.evict_idle(usize::MAX), Ok(2 * (PLAIN + CIPHER)));
            checkpoint("drained", &model.workers[0].admission, &real, [0, 0, 0]);
        }
    }

    struct NoTransport;
    impl Origin for NoTransport {
        fn bootstrap_reserved<'a>(
            &'a self,
            _: &'a OriginAuthority,
            _: &'a OriginContext,
            _: flow_control::Charge<AdmissionPolicy>,
            _: &'a RequestScope,
        ) -> Operation<'a, MetadataReply> {
            Box::pin(async { panic!("bootstrap fixture already owns origin metadata") })
        }
        fn page_reserved<'a>(
            &'a self,
            _: &'a OriginAuthority,
            _: &'a OriginContext,
            _: &'a PageId,
            _: flow_control::Charge<AdmissionPolicy>,
            _: &'a RequestScope,
        ) -> Operation<'a, OriginPage> {
            Box::pin(async { panic!("bootstrap fixture already owns origin bytes") })
        }
        fn metadata<'a>(
            &'a self,
            _: &'a OriginAuthority,
            _: &'a OriginContext,
            _: MetadataSelector,
            _: &'a RequestScope,
        ) -> Operation<'a, MetadataReply> {
            Box::pin(async { panic!("bootstrap fixture already owns origin metadata") })
        }
    }

    fn scope() -> RequestScope {
        RequestScope::new(
            RequestId([1; 16]),
            Instant::now() + Duration::from_secs(600),
        )
        .unwrap()
    }

    #[test]
    fn dirty_pressure_matches_metadata_skip_while_real_bootstrap_read_succeeds() {
        // Standalone Fill contract: dirty overload skips persistence while preserving
        // the read and idle working set. No simulator dirty event is driven here.
        let model = admission(4);
        let real = admission(4);
        let cache = CacheId(crate::security::test_support::CACHE.into());
        let model_dirty = model
            .reserve(Some(&cache), ResourceClass::DirtyCiphertext, CIPHER)
            .unwrap();
        let real_dirty = real
            .reserve(Some(&cache), ResourceClass::DirtyCiphertext, CIPHER)
            .unwrap();
        let buffers = BufferPool::new(real.clone());
        let keys = Rc::new(crate::security::test_support::keys());
        let availability = crate::control::for_caches(keys.clone(), vec![cache.clone()]);
        let memory = Rc::new(MemoryCache::new(buffers.clone(), availability.clone()));
        let mut old_model = MetadataOwners::reserve(&model, &cache);
        Arc::get_mut(&mut old_model.bundle.plain)
            .unwrap()
            .shrink(3)
            .unwrap();
        Arc::get_mut(&mut old_model.bundle.cipher)
            .unwrap()
            .shrink(19)
            .unwrap();
        let old = allocated_page(&real, &cache, "old", 3);
        let old_id = old.plaintext.page().clone();
        memory.publish(old).unwrap();
        assert!(old_model.bundle.idle());
        let mut next_model = MetadataOwners::reserve(&model, &cache);
        Arc::get_mut(&mut next_model.bundle.plain)
            .unwrap()
            .shrink(3)
            .unwrap();
        Arc::get_mut(&mut next_model.bundle.cipher)
            .unwrap()
            .shrink(19)
            .unwrap();
        assert!(next_model.bundle.idle());
        assert!(matches!(
            model.reserve(Some(&cache), ResourceClass::DirtyCiphertext, CIPHER),
            Err(flow_control::Error::Overloaded)
        ));
        assert!(old_model.bundle.idle());
        assert!(next_model.bundle.idle());

        let worker = WorkerId(0);
        let index = Rc::new(Index::new(worker, 16, availability.clone()));
        let segments = Rc::new(page_alloc::Segments::new(64 * 1024 * 1024));
        // Never opened: dirty saturation must bypass writer enqueue and all disk I/O.
        let reactor = Rc::new(Reactor::new(real.clone()));
        let slabs = Rc::new(page_alloc::Slab::new(
            std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
                .join("target/contention-fidelity-unopened/worker-0-slab-0.dat"),
            128 * 1024 * 1024,
            64 * 1024 * 1024,
            crate::model::PAGE_BYTES as usize + crate::store::MAX_HEADER_BYTES + 16,
        ));
        let disk = Rc::new(StoreReader::new(
            Rc::new(SegmentClock::new(index.clone(), segments.clone(), 1)),
            index.clone(),
            segments.clone(),
            slabs.clone(),
            real.clone(),
            reactor.clone(),
            buffers.clone(),
        ));
        let writer = Rc::new(StoreWriter::new(
            index,
            segments,
            slabs,
            real.clone(),
            reactor,
            availability.clone(),
        ));
        let (port, engine_port) = crypto::pair(worker, 0, NonZeroUsize::new(4).unwrap());
        let client = Rc::new(CryptoClient::new(port));
        let mut engine = PageCryptoEngine::new(CryptoRuntime { port: engine_port });
        let peers = crate::test_support::NoPeers::requester();
        let node = crate::model::NodeId(crate::security::test_support::NODE.into());
        let membership = Arc::new(
            Membership::validate(
                crate::model::MembershipVersion(1),
                vec![Member {
                    node: node.clone(),
                    shares: NonZeroU32::new(1).unwrap(),
                    peer_endpoint: "127.0.0.1:8000".into(),
                    rails: vec![],
                    site: String::new(),
                }],
            )
            .unwrap(),
        );
        let fill = Fill::new(FillDependencies {
            memory: memory.clone(),
            buffers: buffers.clone(),
            disk,
            writer: writer.clone(),
            origin: Rc::new(NoTransport),
            candidates: Rc::new(CandidatePolicy::new(
                node,
                Rc::new(Placement::new(8)),
                peers,
                Rc::new(CredentialCrypto::new(keys.clone(), real.clone())),
                Arc::new(Default::default()),
            )),
            flights: Rc::new(Flights::new(real.clone(), availability)),
            crypto: Rc::new(PageCrypto::new(keys.clone(), client.clone())),
            credentials: Rc::new(CredentialCrypto::new(keys, real.clone())),
            admission: real.clone(),
            metadata_owner: Arc::new(
                WorkerDirectory::new(
                    Arc::new(WorkerMap::new(vec![worker]).unwrap()),
                    vec![worker],
                    16,
                )
                .unwrap(),
            ),
        });
        let metadata = descriptor(&cache, "new", 3);
        let mut plaintext = buffers
            .plaintext(fill.reserve_bootstrap(&cache).unwrap(), 3)
            .unwrap();
        plaintext.bytes_mut().unwrap().copy_from_slice(b"abc");
        let context = OriginContext {
            object: metadata.version.object.clone(),
            metadata: None,
            authorization: None,
        };
        let scope = scope();
        let mut budget = AcquisitionBudget::new(scope.deadline.0, 4, 8);
        let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
        let _owner = queue.enter();
        let mut read = fill.publish_bootstrap_with_context(
            OriginPage {
                metadata: metadata.for_pin(),
                plaintext,
            },
            membership,
            &context,
            &scope,
            &mut budget,
        );
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut result = None;
        for _ in 0..32 {
            uring_runtime::drivers::poll(&mut cx, 8);
            engine.poll_budgeted(8).unwrap();
            client.poll_budgeted(8).unwrap();
            if let std::task::Poll::Ready(value) = read.as_mut().poll(&mut cx) {
                result = Some(value.unwrap());
                break;
            }
        }
        let result = result.expect("dirty pressure must not fail or stall the bootstrap read");
        drop(read);
        let result_model = next_model.clone();
        assert!(old_model.bundle.idle());
        assert!(!next_model.bundle.idle());
        assert!(!result_model.bundle.idle());
        assert_eq!(result.plaintext.bytes(), b"abc");
        assert_eq!(result.ciphertext.bytes().len(), 19);
        // Actual Fill overload-to-None policy, read/fill.rs:400-408,720-726.
        assert_eq!(writer.pending_count(), 0);
        assert_eq!(writer.discarded_count(), 0);
        assert!(memory.get(&old_id).unwrap().is_some());
        checkpoint(
            "read succeeds with persistence skipped",
            &model,
            &real,
            [6, 38, CIPHER],
        );
        drop(result);
        drop(result_model);
        assert!(old_model.bundle.idle());
        assert!(next_model.bundle.idle());
        drop(fill);
        assert_eq!(uring_runtime::drivers::pending(), 0);
        drop((old_model, next_model, model_dirty, real_dirty));
        assert_eq!(memory.evict_idle(usize::MAX), Ok(44));
        checkpoint("dirty and page owners drained", &model, &real, [0, 0, 0]);
        assert!(CLASSES.iter().all(|class| real.used(*class) == 0));
    }

    #[test]
    fn canceled_crypto_matches_metadata_owner_trace_through_completion_reap() {
        // Standalone crypto contract: a manually retained Bundle represents accepted
        // work through reap. This does not verify Simulator's cancellation scheduling.
        let model = admission(1);
        let real = admission(1);
        let cache = CacheId(crate::security::test_support::CACHE.into());
        let mut caller = MetadataOwners::reserve(&model, &cache);
        Arc::get_mut(&mut caller.bundle.plain)
            .unwrap()
            .shrink(3)
            .unwrap();
        assert!(caller.bundle.idle());
        let completion_owner = caller.clone();
        assert!(!caller.bundle.idle());
        assert!(!completion_owner.bundle.idle());
        let reserved = real.reserve_fill(&cache, false).unwrap();
        let buffers = BufferPool::new(real.clone());
        let plaintext = buffers.plaintext(reserved.plaintext, 3).unwrap();
        let (port, engine_port) = crypto::pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
        let client = Rc::new(CryptoClient::new(port));
        let crypto = PageCrypto::new(
            Rc::new(crate::security::test_support::keys()),
            client.clone(),
        );
        let mut engine = PageCryptoEngine::new(CryptoRuntime { port: engine_port });
        let scope = scope();
        let mut work = crypto.encrypt(
            page_id(&descriptor(&cache, "v1", 3)),
            plaintext,
            reserved.ciphertext,
            &scope,
        );
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(work.as_mut().poll(&mut cx).is_pending());
        assert_eq!(client.outstanding(), 1);
        assert!(!caller.bundle.idle());
        assert!(!completion_owner.bundle.idle());
        checkpoint("accepted crypto", &model, &real, [3, CIPHER, 0]);
        scope.cancel().unwrap();
        assert!(work.as_mut().poll(&mut cx).is_pending());
        assert!(!caller.bundle.idle());
        assert!(!completion_owner.bundle.idle());
        drop(work);
        drop(caller);
        model.stop();
        real.stop();
        client.poll_budgeted(1).unwrap();
        // Unique ownership is idle by Bundle's predicate, but this owner is held by
        // the completion trace, not a worker cache eligible for reclamation.
        assert!(completion_owner.bundle.idle());
        checkpoint(
            "canceled caller is not a fence",
            &model,
            &real,
            [3, CIPHER, 0],
        );
        assert_eq!(client.outstanding(), 1);
        // Failed inputs survive the engine as well, security/aead.rs:259-260;
        // abandoned completion release occurs in runtime/crypto.rs:497-518.
        engine.poll_budgeted(1).unwrap();
        assert!(completion_owner.bundle.idle());
        checkpoint(
            "completion published but not reaped",
            &model,
            &real,
            [3, CIPHER, 0],
        );
        assert_eq!(client.outstanding(), 1);
        client.poll_budgeted(1).unwrap();
        assert!(completion_owner.bundle.idle());
        drop(completion_owner);
        assert_eq!(client.outstanding(), 0);
        checkpoint(
            "completion reaped after admission stop",
            &model,
            &real,
            [0, 0, 0],
        );
        assert!(CLASSES.iter().all(|class| real.used(*class) == 0));
    }
}

mod scenarios {
    //! Scenario-level oracles for the metadata-only contention model.
    use super::*;

    fn small_config() -> Config {
        Config {
            nodes: 4,
            workers: 2,
            pages_per_worker: 16,
            dirty_pages: 4,
            pipes: 16,
            queue_entries: 64,
            max_attempts: 32,
            ..Config::default()
        }
    }

    fn requested_bytes(requests: &[Request]) -> u64 {
        requests.iter().map(|r| r.pages as u64 * PAGE_BYTES).sum()
    }

    fn assert_drained(config: &Config, requests: &[Request], report: &Report) {
        assert_eq!(report.submitted, requests.len(), "{report:?}");
        assert_eq!(
            report.completed + report.failed + report.canceled,
            report.submitted,
            "every request must have exactly one terminal outcome: {report:?}"
        );
        assert_eq!(report.final_used, [0; 11], "owners leaked: {report:?}");
        assert!(
            report.events < config.max_events,
            "event budget: {report:?}"
        );
        assert!(report.peak_events <= report.events, "{report:?}");
        assert!(report.peak_requests <= requests.len(), "{report:?}");
        assert!(
            report.delivered_bytes <= requested_bytes(requests),
            "delivery duplicated requested bytes: {report:?}"
        );
        // These are per-authority peaks, not fleet totals. Adding workers or nodes
        // must never multiply the amount a single admission authority can own.
        for (class, limit) in [
            (ResourceClass::Plaintext, config.pages_per_worker * PLAIN),
            (ResourceClass::Ciphertext, config.pages_per_worker * CIPHER),
            (ResourceClass::DirtyCiphertext, config.dirty_pages * CIPHER),
            (ResourceClass::Pipe, config.pipes),
        ] {
            assert!(
                report.peak_worker[class as usize] <= limit,
                "{class:?} exceeded per-worker limit {limit}: {report:?}"
            );
        }
    }

    fn assert_success(config: &Config, requests: &[Request], report: &Report) {
        assert_drained(config, requests, report);
        assert_eq!(report.completed, requests.len(), "{report:?}");
        assert_eq!(
            report.delivered_bytes,
            requested_bytes(requests),
            "{report:?}"
        );
        assert_eq!(report.latency_ticks.len(), requests.len(), "{report:?}");
    }

    fn compare_success(configs: [&Config; 2], requests: &[Request]) -> [Report; 2] {
        configs.map(|config| {
            let report = run(config, requests);
            assert_success(config, requests, &report);
            report
        })
    }

    fn run(config: &Config, requests: &[Request]) -> Report {
        let mut sim = Simulator::new(config.clone());
        sim.run(requests.to_vec())
    }

    fn total_latency(report: &Report) -> u64 {
        report.latency_ticks.iter().sum()
    }

    #[test]
    fn overlapping_ranges_join_cold_fills_and_deliver_each_read() {
        let config = Config {
            window: 4,
            ..small_config()
        };
        let mut overlap = Request::new(0, 0);
        overlap.first_page = 2;
        let requests = vec![Request::new(0, 0), overlap, Request::new(0, 0)];
        let report = run(&config, &requests);

        assert_success(&config, &requests, &report);
        assert!(report.joined > 0, "cold readers did not join: {report:?}");
        // Twelve page deliveries cover only six different page identities. There
        // is enough room to retain the entire union without eviction pressure.
        assert!(report.fills >= 6 && report.fills < 12, "{report:?}");
        assert_eq!(report.evictions, 0, "{report:?}");
    }

    #[test]
    fn identical_object_numbers_in_distinct_caches_do_not_singleflight() {
        let config = small_config();
        let mut first = Request::new(0, 0);
        first.pages = 2;
        let mut second = first.clone();
        second.cache = 1;
        let requests = vec![first, second];
        let report = run(&config, &requests);

        assert_success(&config, &requests, &report);
        assert_eq!(report.fills, 4, "cache identity was aliased: {report:?}");
        assert_eq!(report.joined, 0, "cache identity was aliased: {report:?}");
    }

    #[test]
    fn slow_reader_keeps_a_bounded_window_and_eventually_makes_progress() {
        let config = Config {
            nodes: 1,
            workers: 1,
            pages_per_worker: 4,
            window: 2,
            ..small_config()
        };
        let mut request = Request::new(0, 0);
        request.pages = 12;
        let fast_requests = vec![request.clone()];
        let fast = run(&config, &fast_requests);
        request.reader_bytes_per_tick = Some(PAGE_BYTES / 256);
        let slow_requests = vec![request];
        let slow = run(&config, &slow_requests);

        assert_success(&config, &fast_requests, &fast);
        assert_success(&config, &slow_requests, &slow);
        assert!(total_latency(&slow) > total_latency(&fast), "{slow:?}");
        assert!(
            slow.evictions > 0,
            "range never reclaimed idle pages: {slow:?}"
        );
        assert!(
            slow.peak_worker[ResourceClass::Plaintext as usize]
                < requested_bytes(&slow_requests) as usize,
            "entire range was retained instead of windowed: {slow:?}"
        );
    }

    #[test]
    fn larger_request_window_overlaps_fill_and_reader_service() {
        let narrow_config = Config {
            nodes: 1,
            workers: 1,
            pages_per_worker: 16,
            window: 1,
            reader_bytes_per_tick: PAGE_BYTES / 64,
            ..small_config()
        };
        let wide_config = Config {
            window: 4,
            ..narrow_config.clone()
        };
        let mut request = Request::new(0, 0);
        request.pages = 12;
        let requests = vec![request];
        let [narrow, wide] = compare_success([&narrow_config, &wide_config], &requests);
        assert_eq!(wide.fills, narrow.fills, "window changed page identity");
        assert!(
            total_latency(&wide) < total_latency(&narrow),
            "a larger window did not overlap service: narrow={narrow:?}, wide={wide:?}"
        );
    }

    #[test]
    fn slow_disk_sheds_persistence_without_losing_reader_progress() {
        let fast_config = Config {
            nodes: 1,
            workers: 1,
            pages_per_worker: 32,
            dirty_pages: 1,
            window: 4,
            disk_bytes_per_tick: PAGE_BYTES * 4,
            ..small_config()
        };
        let slow_config = Config {
            disk_bytes_per_tick: PAGE_BYTES / 1024,
            ..fast_config.clone()
        };
        let mut request = Request::new(0, 0);
        request.pages = 24;
        let requests = vec![request];
        let [fast, slow] = compare_success([&fast_config, &slow_config], &requests);
        assert!(slow.disk_bytes > 0, "disk path was not exercised: {slow:?}");
        assert!(
            slow.persistence_skips > fast.persistence_skips,
            "slow dirty owners must increase optional persistence pressure: fast={fast:?}, slow={slow:?}"
        );
        assert!(
            slow.disk_bytes < fast.disk_bytes,
            "fast={fast:?}, slow={slow:?}"
        );
    }

    #[test]
    fn cache_churn_reclaims_idle_bundles_and_allows_a_new_cache_to_progress() {
        let config = Config {
            nodes: 1,
            workers: 1,
            pages_per_worker: 8,
            window: 2,
            ..small_config()
        };
        let mut warm = Request::new(0, 0);
        warm.pages = 8;
        let mut churn = Request::new(0, 1);
        churn.at = 2048;
        churn.pages = 12;
        let mut newcomer = Request::new(0, 2);
        newcomer.at = churn.at;
        newcomer.cache = 1;
        newcomer.pages = 2;
        let mut revisit = Request::new(0, 0);
        revisit.at = 8192;
        let requests = vec![warm, churn, newcomer, revisit];
        let report = run(&config, &requests);

        // Success for every request includes the newcomer while the old cache is
        // still active, and the old cache's later read after fair-share reclamation.
        assert_success(&config, &requests, &report);
        assert!(
            report.evictions > 0,
            "idle bundles were never reclaimed: {report:?}"
        );
        assert!(report.fills > config.pages_per_worker, "{report:?}");
    }

    #[test]
    fn workers_share_node_network_bandwidth() {
        let fast_config = Config {
            nodes: 1,
            workers: 4,
            window: 1,
            network_bytes_per_tick: PAGE_BYTES,
            origin_bytes_per_tick: PAGE_BYTES * 64,
            disk_bytes_per_tick: PAGE_BYTES * 64,
            reader_bytes_per_tick: PAGE_BYTES * 64,
            ..small_config()
        };
        let slow_config = Config {
            network_bytes_per_tick: PAGE_BYTES / 64,
            ..fast_config.clone()
        };
        let requests: Vec<_> = (0..4)
            .map(|object| {
                let mut request = Request::new(0, object);
                request.pages = 1;
                request
            })
            .collect();
        let [fast, slow] = compare_success([&fast_config, &slow_config], &requests);
        assert!(total_latency(&slow) > total_latency(&fast), "{slow:?}");
        // Each object maps to a different worker, but all four inbound pages must
        // cross the same node NIC. Independent worker NICs violate this floor.
        let nic_ticks = requested_bytes(&requests).div_ceil(slow_config.network_bytes_per_tick);
        assert!(
            slow.latency_ticks.iter().copied().max().unwrap() >= nic_ticks,
            "worker count multiplied node bandwidth: {slow:?}"
        );
    }

    #[test]
    fn cold_nodes_contend_for_one_global_origin_queue() {
        let fast_config = Config {
            nodes: 8,
            workers: 1,
            network_bytes_per_tick: PAGE_BYTES * 64,
            disk_bytes_per_tick: PAGE_BYTES * 64,
            reader_bytes_per_tick: PAGE_BYTES * 64,
            origin_bytes_per_tick: PAGE_BYTES * 64,
            ..small_config()
        };
        let slow_config = Config {
            origin_bytes_per_tick: PAGE_BYTES / 64,
            ..fast_config.clone()
        };
        let requests: Vec<_> = (0..fast_config.nodes)
            .map(|node| {
                let mut request = Request::new(node, node as u64);
                request.pages = 1;
                request
            })
            .collect();
        let [fast, slow] = compare_success([&fast_config, &slow_config], &requests);
        assert!(slow.origin_bytes >= requested_bytes(&requests), "{slow:?}");
        assert!(total_latency(&slow) > total_latency(&fast), "{slow:?}");
        let origin_ticks = requested_bytes(&requests).div_ceil(slow_config.origin_bytes_per_tick);
        assert!(
            slow.latency_ticks.iter().copied().max().unwrap() >= origin_ticks,
            "origin bandwidth was multiplied by node count: {slow:?}"
        );
    }

    #[test]
    fn warm_peer_fanout_is_limited_by_source_node_network() {
        let config = Config {
            nodes: 9,
            workers: 1,
            network_bytes_per_tick: PAGE_BYTES / 64,
            origin_bytes_per_tick: PAGE_BYTES * 64,
            disk_bytes_per_tick: PAGE_BYTES * 64,
            reader_bytes_per_tick: PAGE_BYTES * 64,
            window: 1,
            ..small_config()
        };
        let mut warm = Request::new(0, 0);
        warm.pages = 1;
        let mut requests = vec![warm];
        for node in 1..config.nodes {
            let mut request = Request::new(node, 0);
            request.at = 4096;
            request.pages = 1;
            requests.push(request);
        }
        let report = run(&config, &requests);

        assert_success(&config, &requests, &report);
        let destinations = (config.nodes - 1) as u64;
        assert_eq!(report.origin_bytes, CIPHER as u64, "{report:?}");
        assert_eq!(
            report.peer_bytes,
            destinations * CIPHER as u64,
            "fanout must fetch every copy from the warm peer: {report:?}"
        );
        // Node 0 is warm and idle before the simultaneous arrivals. Every destination
        // schedules its single fill before any fill completes, so all copies use
        // node 0 and must serialize on its NIC, despite having distinct destination NICs.
        let source_ticks = destinations * (CIPHER as u64).div_ceil(config.network_bytes_per_tick);
        // Eight ciphertext transfers require 520 ticks. Without source serialization,
        // each peer read takes only 65 + 64 + 2 * 2 = 133 ticks (fill, send, completions),
        // and even the initial origin-backed warmup takes only 134 ticks.
        assert!(
            report.latency_ticks.iter().copied().max().unwrap() >= source_ticks,
            "fanout bypassed the cumulative source NIC bound of {source_ticks} ticks: {report:?}"
        );
    }

    #[test]
    fn partition_retries_recover_after_heal() {
        let config = small_config();
        let requests = vec![Request::new(0, 0), Request::new(1, 1)];
        let healthy = run(&config, &requests);
        let mut sim = Simulator::new(config.clone());
        sim.partition(0, 0, 256);
        let healed = sim.run(requests.clone());

        assert_success(&config, &requests, &healthy);
        assert_success(&config, &requests, &healed);
        assert!(
            healed.retries > healthy.retries,
            "partition had no effect: {healed:?}"
        );
        assert!(
            total_latency(&healed) > total_latency(&healthy),
            "{healed:?}"
        );
    }

    #[test]
    fn retry_exhaustion_is_bounded_and_does_not_poison_later_requests() {
        let config = Config {
            max_attempts: 2,
            ..small_config()
        };
        let mut blocked = Request::new(0, 0);
        blocked.pages = 1;
        let mut recovery = Request::new(0, 1);
        recovery.at = 4096;
        let requests = vec![blocked, recovery];
        let mut sim = Simulator::new(config.clone());
        sim.partition(0, 0, 2048);
        let report = sim.run(requests.clone());

        assert_drained(&config, &requests, &report);
        assert_eq!(
            report.failed, 1,
            "partitioned request did not exhaust retries: {report:?}"
        );
        assert_eq!(
            report.completed, 1,
            "post-heal request could not progress: {report:?}"
        );
        assert_eq!(report.canceled, 0, "{report:?}");
        assert!(
            report.retries > 0 && report.retries <= config.max_attempts,
            "{report:?}"
        );
        assert_eq!(
            report.delivered_bytes,
            requests[1].pages as u64 * PAGE_BYTES,
            "{report:?}"
        );
    }

    #[test]
    fn cancellation_and_deadline_drain_inflight_owners_before_recovery() {
        for cancel in [true, false] {
            let config = Config {
                nodes: 1,
                workers: 1,
                pages_per_worker: 2,
                dirty_pages: 1,
                window: 1,
                network_bytes_per_tick: PAGE_BYTES / 64,
                disk_bytes_per_tick: PAGE_BYTES / 256,
                completion_ticks: 128,
                ..small_config()
            };
            let mut interrupted = Request::new(0, 0);
            interrupted.pages = 1;
            if cancel {
                interrupted.cancel_after = Some(8);
            } else {
                interrupted.deadline = 8;
            }
            let interrupted_requests = vec![interrupted.clone()];
            let interrupted_report = run(&config, &interrupted_requests);
            assert_drained(&config, &interrupted_requests, &interrupted_report);
            assert_eq!(interrupted_report.completed, 0, "{interrupted_report:?}");
            assert_eq!(
                interrupted_report.canceled,
                usize::from(cancel),
                "{interrupted_report:?}"
            );
            assert_eq!(
                interrupted_report.failed,
                usize::from(!cancel),
                "{interrupted_report:?}"
            );
            assert!(
                interrupted_report.fills > 0,
                "no in-flight fill: {interrupted_report:?}"
            );
            assert!(
                interrupted_report.end_tick >= config.completion_ticks,
                "request termination prematurely dropped scheduled ownership: {interrupted_report:?}"
            );

            let mut recovery = Request::new(0, 1);
            recovery.at = 4096;
            let requests = vec![interrupted, recovery];
            let report = run(&config, &requests);
            assert_drained(&config, &requests, &report);
            assert_eq!(
                report.completed, 1,
                "recovery failed, cancel={cancel}: {report:?}"
            );
            assert_eq!(report.canceled, usize::from(cancel), "{report:?}");
            assert_eq!(report.failed, usize::from(!cancel), "{report:?}");
            assert_eq!(
                report.delivered_bytes,
                requests[1].pages as u64 * PAGE_BYTES,
                "{report:?}"
            );
        }
    }

    #[test]
    fn canceling_one_joined_reader_preserves_the_surviving_read() {
        let config = Config {
            nodes: 1,
            workers: 1,
            pages_per_worker: 4,
            window: 2,
            ..small_config()
        };
        let mut canceled = Request::new(0, 0);
        canceled.cancel_after = Some(8);
        let requests = vec![canceled, Request::new(0, 0)];
        let report = run(&config, &requests);

        assert_drained(&config, &requests, &report);
        assert_eq!(report.canceled, 1, "{report:?}");
        assert_eq!(
            report.completed, 1,
            "surviving reader lost its fill: {report:?}"
        );
        assert_eq!(report.failed, 0, "{report:?}");
        assert!(report.joined > 0, "no shared flight to cancel: {report:?}");
        assert!(
            report.fills < 8,
            "shared cold pages were fetched twice: {report:?}"
        );
        assert_eq!(report.delivered_bytes, 4 * PAGE_BYTES, "{report:?}");
    }

    // A fixed integer generator avoids external RNG dependencies and host entropy.
    // Its state is local to each workload, so test execution order cannot affect it.
    fn next_random(state: &mut u64) -> u64 {
        *state = state.wrapping_add(0x9e37_79b9_7f4a_7c15);
        let mut value = *state;
        value = (value ^ (value >> 30)).wrapping_mul(0xbf58_476d_1ce4_e5b9);
        value = (value ^ (value >> 27)).wrapping_mul(0x94d0_49bb_1331_11eb);
        value ^ (value >> 31)
    }

    fn generated_requests(seed: u64, config: &Config) -> Vec<Request> {
        let mut state = seed;
        let mut requests = Vec::new();
        for index in 0..96 {
            let mut request = Request::new(
                (next_random(&mut state) % config.nodes as u64) as usize,
                next_random(&mut state) % 8,
            );
            request.at = (index / 16) * 512 + next_random(&mut state) % 64;
            request.cache = (next_random(&mut state) % 3) as usize;
            request.first_page = next_random(&mut state) % 4;
            request.pages = 1 + (next_random(&mut state) % 4) as usize;
            if next_random(&mut state).is_multiple_of(4) {
                request.reader_bytes_per_tick = Some(PAGE_BYTES / 128);
            }
            match next_random(&mut state) % 8 {
                0 => request.cancel_after = Some(8),
                1 => request.deadline = 16,
                _ => {}
            }
            requests.push(request);
        }
        // A quiet recovery wave probes every node and worker after generated
        // pressure, cancellations, deadlines, and partitions have all subsided.
        for node in 0..config.nodes {
            for worker in 0..config.workers {
                let mut request = Request::new(node, 1024 * config.workers as u64 + worker as u64);
                request.at = 65_536;
                request.pages = 2;
                requests.push(request);
            }
        }
        requests
    }

    fn run_generated(config: &Config, requests: &[Request]) -> Report {
        let mut sim = Simulator::new(config.clone());
        sim.partition(0, 0, 256);
        sim.partition(config.nodes - 1, 1024, 1536);
        sim.run(requests.to_vec())
    }

    #[test]
    fn generated_seed_matrix_replays_identical_reports_and_drains() {
        let mut traces = Vec::new();
        for seed in [1, 7, 42, 0xdead_beef] {
            let config = Config {
                nodes: 8,
                workers: 1 + (seed as usize % 2),
                pages_per_worker: 8,
                dirty_pages: 2,
                max_attempts: 8,
                ..small_config()
            };
            let requests = generated_requests(seed, &config);
            let report = run_generated(&config, &requests);
            let replay_requests = generated_requests(seed, &config);
            let replay = run_generated(&config, &replay_requests);
            let recovery_count = config.nodes * config.workers;
            let pressure_requests = &requests[..requests.len() - recovery_count];
            let pressure = run_generated(&config, pressure_requests);

            assert_eq!(report, replay, "replay diverged for seed={seed}");
            assert_drained(&config, &requests, &report);
            assert_drained(&config, pressure_requests, &pressure);
            assert_eq!(
                report.completed,
                pressure.completed + recovery_count,
                "recovery wave failed, seed={seed}: {report:?}"
            );
            assert_eq!(
                report.failed, pressure.failed,
                "recovery added failures, seed={seed}"
            );
            assert_eq!(
                report.canceled, pressure.canceled,
                "recovery added cancellations, seed={seed}"
            );
            assert_eq!(
                report.delivered_bytes,
                pressure.delivered_bytes + recovery_count as u64 * 2 * PAGE_BYTES,
                "recovery delivery mismatch, seed={seed}"
            );
            assert!(
                report.fills > 0 && report.delivered_bytes > 0,
                "seed={seed}: {report:?}"
            );
            assert!(
                report.failed + report.canceled > 0,
                "faults had no effect, seed={seed}: {report:?}"
            );
            assert!(
                report.evictions > 0,
                "no cache pressure, seed={seed}: {report:?}"
            );
            traces.push(report.trace_hash);
        }
        assert!(
            traces.windows(2).any(|pair| pair[0] != pair[1]),
            "distinct seeds produced identical traces"
        );
    }

    #[test]
    fn two_thousand_nodes_keep_per_worker_bounds_under_cold_start() {
        let config = Config {
            nodes: 2000,
            workers: 2,
            pages_per_worker: 8,
            max_attempts: 16,
            retry_ticks: 256,
            ..Config::default()
        };
        let mut requests = Vec::with_capacity(config.nodes * config.workers);
        for node in 0..config.nodes {
            for worker in 0..config.workers {
                // Distinct objects eliminate accidental fleet-wide warm hits while
                // object modulo workers drives both authorities on every node.
                let object = (node * config.workers + worker) as u64;
                let mut request = Request::new(node, object);
                request.at = (node % 16) as u64;
                requests.push(request);
            }
        }
        let report = run(&config, &requests);

        assert_success(&config, &requests, &report);
        assert!(
            report.peak_requests >= config.nodes,
            "no fleet overlap: {report:?}"
        );
        assert!(
            report.origin_bytes >= requested_bytes(&requests),
            "{report:?}"
        );
        assert!(
            report.disk_bytes > 0,
            "cold fills did not reach disk: {report:?}"
        );
        assert!(
            report.peak_logical_bytes > (config.pages_per_worker * PLAIN) as u64,
            "fleet pressure never exceeded one worker: {report:?}"
        );
    }

    // Model mechanics: queue bounds, cancellation, and scheduled-owner edge cases.
    //
    // These tests drive Simulator events and inspect its accounting, not production
    // I/O or completion fences. Allocation-backed comparisons live in fidelity.rs;
    // production queue behavior belongs to the memory/runtime and app scenarios.
    // Keep event ordering and terminal accounting here even when fidelity tests
    // compare the same resource owners: those comparisons do not run this scheduler.

    fn global_capacity_config() -> Config {
        Config {
            nodes: 1,
            workers: 1,
            pages_per_worker: 16,
            dirty_pages: 16,
            pipes: 6,
            max_events: 1_000,
            ..Config::default()
        }
    }

    #[test]
    fn two_live_caches_can_use_global_transport_capacity_until_exhaustion() {
        for class in [ResourceClass::Pipe, ResourceClass::Connection] {
            assert_global_capacity(class);
        }
    }

    fn assert_global_capacity(class: ResourceClass) {
        // Transport leases are node-wide; live caches must not impose fair-share caps.
        let mut sim = Simulator::new(global_capacity_config());
        let contexts = [0, 1].map(|cache| {
            sim.reserve(0, cache, ResourceClass::RequestContext, CONTEXT)
                .unwrap()
        });
        let limit = sim.workers[0].admission.limit(class);
        let mut leases = Vec::new();
        for slot in 0..limit {
            // Both caches own transport leases, but cache 0 exceeds half the
            // capacity while cache 1 remains live with ample context quota.
            let cache = usize::from(slot == 0);
            leases.push(sim.reserve(0, cache, class, 1).unwrap_or_else(|| {
                panic!("{class:?} falsely rejected at {slot}/{limit} global usage")
            }));
        }
        assert!(leases.iter().all(|lease| lease.key().is_none()));
        assert_eq!(sim.report.rejections[class as usize], 0);
        assert_eq!(sim.workers[0].admission.used(class), limit);
        for cache in [0, 1] {
            assert!(sim.reserve(0, cache, class, 1).is_none());
        }
        assert_eq!(sim.workers[0].admission.used(class), limit);
        drop(leases.pop());
        let reused = sim.reserve(0, 1, class, 1).unwrap();
        assert_eq!(sim.workers[0].admission.used(class), limit);
        drop((leases, reused, contexts));
        assert!(
            CLASSES
                .iter()
                .all(|class| sim.workers[0].admission.used(*class) == 0)
        );
    }

    #[test]
    fn two_caches_with_spare_pipes_do_not_park_readers() {
        let mut sim = Simulator::new(global_capacity_config());
        let requests = (0..5)
            .map(|object| Request {
                cache: usize::from(object == 0),
                pages: 1,
                ..Request::new(0, object)
            })
            .collect();
        let report = sim.run(requests);
        assert_eq!(report.completed, 5, "{report:?}");
        assert_eq!((report.failed, report.canceled, report.retries), (0, 0, 0));
        assert_eq!(report.rejections, [0; 11], "{report:?}");
        assert_eq!(report.peak_worker[ResourceClass::Pipe as usize], 5);
        assert_eq!(report.peak_worker[ResourceClass::Connection as usize], 5);
        assert_eq!(report.delivered_bytes, 5 * PAGE_BYTES);
        assert_eq!(report.final_used, [0; 11]);
    }

    #[test]
    fn cache_scoped_resources_still_enforce_fairness_with_global_room() {
        for (class, amount) in [
            (ResourceClass::Plaintext, PLAIN),
            (ResourceClass::Ciphertext, CIPHER),
            (ResourceClass::DirtyCiphertext, CIPHER),
            (ResourceClass::Registered, CIPHER),
            (ResourceClass::RequestContext, CONTEXT),
            (ResourceClass::Flight, 1),
            (ResourceClass::Waiter, 1),
        ] {
            let mut sim = Simulator::new(global_capacity_config());
            let limit = sim.workers[0].admission.limit(class);
            let first = sim.reserve(0, 0, class, limit / 2).unwrap();
            let second = sim.reserve(0, 1, class, amount).unwrap();
            assert_eq!(first.key(), Some(&CacheId("0".into())));
            assert_eq!(second.key(), Some(&CacheId("1".into())));
            assert!(sim.workers[0].admission.used(class) + amount <= limit);
            assert!(sim.reserve(0, 0, class, amount).is_none(), "{class:?}");
            assert_eq!(sim.report.rejections[class as usize], 1);
            drop((first, second));
            assert_eq!(sim.workers[0].admission.used(class), 0);
        }
    }

    fn config() -> Config {
        Config {
            nodes: 1,
            workers: 1,
            pages_per_worker: 2,
            dirty_pages: 1,
            pipes: 1,
            queue_entries: 3,
            window: 1,
            network_bytes_per_tick: PAGE_BYTES * 2,
            origin_bytes_per_tick: PAGE_BYTES * 2,
            disk_bytes_per_tick: PAGE_BYTES * 2,
            reader_bytes_per_tick: PAGE_BYTES / 64,
            ..Config::default()
        }
    }

    fn request() -> Request {
        let mut request = Request::new(0, 0);
        request.pages = 1;
        request
    }

    fn assert_outcomes(report: &Report, completed: usize, failed: usize, canceled: usize) {
        assert_eq!(
            report.submitted,
            completed + failed + canceled,
            "{report:?}"
        );
        assert_eq!(
            report.outcomes(),
            (completed, failed, canceled),
            "{report:?}"
        );
        assert_eq!(report.delivered_bytes, completed as u64 * PAGE_BYTES);
        assert_eq!(report.latency_ticks.len(), completed);
        assert_eq!(report.final_used, [0; 11]);
    }

    fn request_ownership(sim: &Simulator) -> [usize; 3] {
        [
            ResourceClass::RequestContext,
            ResourceClass::Connection,
            ResourceClass::Pipe,
        ]
        .map(|class| sim.workers[0].admission.used(class))
    }

    fn pump_next(sim: &mut Simulator, expected: usize) {
        let ((at, _), event) = sim.events.pop_first().unwrap();
        assert_eq!(at, sim.now);
        assert!(matches!(event, Event::Pump(id) if id == expected));
        sim.pump(expected);
    }

    #[test]
    fn same_tick_newer_pump_cannot_bypass_a_notified_head() {
        for pipes in [1, 2] {
            let mut sim = Simulator::new(Config { pipes, ..config() });
            let held: Vec<_> = (0..pipes)
                .map(|_| sim.reserve(0, 0, ResourceClass::Pipe, 1).unwrap())
                .collect();
            for id in 1..=2 {
                sim.arrive(id, request());
                pump_next(&mut sim, id);
            }

            drop(held);
            // The newcomer already has a pump scheduled ahead of the release wake.
            sim.arrive(3, request());
            sim.wake_pipe(0);
            assert_eq!(sim.workers[0].pipe_queue, VecDeque::from([1, 2]));
            assert!(sim.active[&1].queued);
            pump_next(&mut sim, 3);
            assert!(sim.active[&3].pipe.is_none());
            assert_eq!(sim.workers[0].pipe_queue, VecDeque::from([1, 2, 3]));

            pump_next(&mut sim, 1);
            assert!(sim.active[&1].pipe.is_some());
            assert!(!sim.active[&1].queued);
            assert_eq!(sim.workers[0].pipe_queue, VecDeque::from([2, 3]));
            if pipes == 1 {
                // Return the only pipe to let the second waiter make progress.
                drop(sim.active.get_mut(&1).unwrap().pipe.take());
                sim.wake_pipe(0);
            }
            // With two free pipes, acquiring the head must notify its successor.
            pump_next(&mut sim, 2);
            assert!(sim.active[&2].pipe.is_some());
            assert!(sim.active[&3].pipe.is_none());
            assert_eq!(sim.workers[0].pipe_queue, VecDeque::from([3]));
        }
    }

    #[test]
    fn cancellation_removes_every_queue_position_before_delayed_release() {
        for canceled in 1..=3 {
            let mut sim = Simulator::new(config());
            let held = sim.reserve(0, 0, ResourceClass::Pipe, 1).unwrap();
            for id in 1..=3 {
                sim.arrive(id, request());
                pump_next(&mut sim, id);
            }

            let active = sim.active.remove(&canceled).unwrap();
            sim.release(canceled, active);
            let survivors: VecDeque<_> = (1..=3).filter(|id| *id != canceled).collect();
            assert_eq!(sim.workers[0].pipe_queue, survivors);
            assert_eq!(request_ownership(&sim), [3 * CONTEXT, 3, 1]);
            assert!(
                sim.events
                    .values()
                    .all(|event| !matches!(event, Event::Pump(_)))
            );
            // Queue capacity is reusable even while canceled context ownership is
            // retained by the completion event. One spare context admits this request.
            sim.arrive(4, request());
            pump_next(&mut sim, 4);
            assert!(sim.active[&4].queued);
            assert_eq!(sim.workers[0].pipe_queue.back(), Some(&4));
            assert_eq!(sim.workers[0].pipe_queue.len(), 3);
            assert_eq!(sim.report.failed, 0);
            drop(held);
        }
    }

    #[test]
    fn canceling_a_notified_head_wakes_its_successor_before_delayed_release() {
        let mut sim = Simulator::new(config());
        let held = sim.reserve(0, 0, ResourceClass::Pipe, 1).unwrap();
        for id in 1..=2 {
            sim.arrive(id, request());
            pump_next(&mut sim, id);
        }
        drop(held);
        sim.wake_pipe(0);
        let head = sim.active.remove(&1).unwrap();
        sim.release(1, head);
        assert_eq!(sim.workers[0].pipe_queue, VecDeque::from([2]));
        pump_next(&mut sim, 1); // The canceled head's scheduled pump is harmless.
        pump_next(&mut sim, 2);
        assert!(sim.active[&2].pipe.is_some());
        assert!(sim.workers[0].pipe_queue.is_empty());
    }

    #[test]
    fn one_pipe_drains_a_full_bounded_queue_sequentially() {
        let config = config();
        let count = config.pipes + config.queue_entries;
        let service = PAGE_BYTES.div_ceil(config.reader_bytes_per_tick) + config.completion_ticks;
        let mut sim = Simulator::new(config.clone());

        // Check the actual queue before running its completion events. Repeated
        // pumps must not insert duplicate waiters or acquire another pipe.
        for id in 0..count {
            sim.arrive(id, request());
            sim.pump(id);
        }
        assert_eq!(sim.workers[0].pipe_queue, VecDeque::from([1, 2, 3]));
        assert_eq!(request_ownership(&sim), [count * CONTEXT, count, 1]);
        for id in 1..count {
            assert!(sim.active[&id].queued);
            assert!(sim.active[&id].pipe.is_none());
            sim.pump(id);
        }
        assert_eq!(sim.workers[0].pipe_queue, VecDeque::from([1, 2, 3]));

        // A fresh run exercises the model event dispatcher and terminal accounting.
        let report = Simulator::new(config.clone()).run(vec![request(); count]);
        assert_outcomes(&report, count, 0, 0);
        assert_eq!(report.peak_requests, count);
        assert_eq!(report.peak_worker[ResourceClass::Pipe as usize], 1);
        // Once the queue exists, newcomers wait without attempting raw admission.
        assert_eq!(report.rejections[ResourceClass::Pipe as usize], 1);
        assert_eq!(report.retries, 0, "pipe waiters should be event-driven");
        assert_eq!(report.fills, 1);
        assert_eq!(report.hits, count - 1);
        for pair in report.latency_ticks.windows(2) {
            assert_eq!(pair[1] - pair[0], service + config.completion_ticks);
        }
    }

    #[test]
    fn canceling_the_only_queued_request_allows_later_recovery() {
        let config = Config {
            queue_entries: 1,
            ..config()
        };
        let mut canceled = request();
        canceled.cancel_after = Some(8);
        let mut recovery = request();
        recovery.at = 512;
        let report = Simulator::new(config).run(vec![request(), canceled, recovery]);

        assert_outcomes(&report, 2, 0, 1);
        assert_eq!(report.peak_requests, 2);
        assert_eq!(report.rejections[ResourceClass::Pipe as usize], 1);
        assert_eq!(report.fills, 1);
        assert_eq!(report.hits, 1, "canceled waiter must not begin a read");
        assert_eq!(report.retries, 0);
    }

    #[test]
    fn canceling_a_queued_request_preserves_survivor_fifo_order() {
        let config = config();
        let mut fast = request();
        fast.reader_bytes_per_tick = Some(PAGE_BYTES);
        let survivors = vec![request(), fast.clone(), request()];
        let baseline = Simulator::new(config.clone()).run(survivors);
        let mut canceled = request();
        canceled.cancel_after = Some(8);
        let report = Simulator::new(config).run(vec![request(), canceled, fast, request()]);

        assert_outcomes(&baseline, 3, 0, 0);
        assert_outcomes(&report, 3, 0, 1);
        // All arrive at zero. The distinct reader rates make a FIFO inversion
        // visible in completion latencies, even though all survivors still finish.
        assert_eq!(
            report.latency_ticks, baseline.latency_ticks,
            "canceling a waiter that owns no pipe must not reorder surviving waiters"
        );
    }

    #[test]
    fn canceled_queue_tail_frees_capacity_for_a_replacement_waiter() {
        let config = config();
        let mut canceled = request();
        canceled.cancel_after = Some(8);
        let mut replacement = request();
        // The canceled request's delayed release has completed, while the first
        // reader is still using the only pipe. One queue slot must be available.
        replacement.at = 8 + config.completion_ticks + 1;
        let report = Simulator::new(config).run(vec![
            request(),
            request(),
            request(),
            canceled,
            replacement,
        ]);

        assert_outcomes(&report, 4, 0, 1);
        assert_eq!(report.peak_requests, 4);
        assert_eq!(report.rejections[ResourceClass::RequestContext as usize], 0);
        assert_eq!(report.hits, 3);
        assert_eq!(report.retries, 0);
    }

    #[test]
    fn request_context_bound_rejects_excess_arrivals_and_recovers() {
        for queue_entries in [1, 3] {
            let config = Config {
                queue_entries,
                ..config()
            };
            let capacity = config.pipes + config.queue_entries;
            let excess = 5;
            let mut sim = Simulator::new(config.clone());
            for id in 0..capacity {
                sim.arrive(id, request());
            }
            assert_eq!(request_ownership(&sim), [capacity * CONTEXT, capacity, 0]);
            let scheduled = sim.events.len();
            for id in capacity..capacity + excess {
                sim.arrive(id, request());
                assert!(!sim.active.contains_key(&id));
                assert_eq!(sim.active.len(), capacity);
                assert_eq!(request_ownership(&sim), [capacity * CONTEXT, capacity, 0]);
                assert_eq!(sim.events.len(), scheduled, "rejection scheduled work");
            }
            assert_eq!(sim.report.failed, excess);
            assert_eq!(
                sim.report.rejections[ResourceClass::RequestContext as usize],
                excess
            );

            let mut requests = vec![request(); capacity + excess];
            let mut recovery = request();
            recovery.at = 4096;
            requests.push(recovery);
            let report = Simulator::new(config).run(requests);
            assert_outcomes(&report, capacity + 1, excess, 0);
            assert_eq!(report.peak_requests, capacity);
            assert_eq!(
                report.peak_worker[ResourceClass::RequestContext as usize],
                capacity * CONTEXT
            );
            assert_eq!(
                report.peak_worker[ResourceClass::Connection as usize],
                capacity
            );
            assert_eq!(
                report.rejections[ResourceClass::RequestContext as usize],
                excess
            );
        }
    }

    #[test]
    fn connection_rejection_rolls_back_the_new_request_context() {
        let mut sim = Simulator::new(config());
        let capacity = sim.config.pipes + sim.config.queue_entries;
        // Normally context admission hits its equally sized bound first. Hold one
        // independent connection to exercise the subsequent reservation failure.
        let connection = sim.reserve(0, 0, ResourceClass::Connection, 1).unwrap();
        for id in 0..capacity - 1 {
            sim.arrive(id, request());
        }
        let before = request_ownership(&sim);
        let scheduled = sim.events.len();
        sim.arrive(capacity - 1, request());
        assert_eq!(sim.report.failed, 1);
        assert_eq!(sim.report.rejections[ResourceClass::Connection as usize], 1);
        assert_eq!(
            sim.report.rejections[ResourceClass::RequestContext as usize],
            0
        );
        assert_eq!(request_ownership(&sim), before);
        assert_eq!(sim.active.len(), capacity - 1);
        assert_eq!(sim.events.len(), scheduled);

        drop(connection);
        sim.arrive(capacity, request());
        assert!(sim.active.contains_key(&capacity));
        assert_eq!(request_ownership(&sim), [capacity * CONTEXT, capacity, 0]);
    }

    #[test]
    fn idle_nodes_add_no_polling_events_or_logical_ownership() {
        let config = config();
        let mut later = request();
        later.at = 1_000_000_000;
        let requests = vec![request(), request(), later];
        let baseline = Simulator::new(config.clone()).run(requests.clone());
        assert_outcomes(&baseline, 3, 0, 0);

        for nodes in [2, 2000] {
            let config = Config {
                nodes,
                ..config.clone()
            };
            let empty = Simulator::new(config.clone()).run(Vec::new());
            assert_eq!(empty, Report::default(), "idle fleet generated work");
            let report = Simulator::new(config).run(requests.clone());
            // Compare deterministic work, not wall-clock time. This also checks
            // that the long idle interval contributes no periodic polling events.
            assert_eq!(report, baseline, "idle nodes changed the active workload");
        }
        assert!(baseline.events < 64, "long idle interval generated polling");
    }

    #[test]
    fn canceled_send_keeps_page_and_pipe_until_their_scheduled_completions() {
        for cancel_near_completion in [false, true] {
            let mut sim = Simulator::new(Config {
                pages_per_worker: 1,
                ..config()
            });
            let key = (0, 0, 0);
            let plain = Arc::new(sim.reserve(0, 0, ResourceClass::Plaintext, PLAIN).unwrap());
            let cipher = Arc::new(
                sim.reserve(0, 0, ResourceClass::Ciphertext, CIPHER)
                    .unwrap(),
            );
            let page = Arc::downgrade(&plain);
            sim.workers[0].cache.insert(key, Bundle { plain, cipher });
            sim.workers[0].lru.push_back(key);
            sim.directory.entry(key).or_default().insert(0);
            sim.arrive(0, request());
            let ((at, _), event) = sim.events.pop_first().unwrap();
            assert_eq!(at, 0);
            assert!(matches!(event, Event::Pump(0)));
            sim.pump(0);
            let sent_at = sim.active[&0].sending_until.unwrap();
            assert_eq!(page.strong_count(), 2, "cache and send must own the page");
            assert!(!sim.workers[0].cache[&key].idle());
            assert_eq!(request_ownership(&sim), [CONTEXT, 1, 1]);

            sim.now = if cancel_near_completion {
                sent_at - 1
            } else {
                1
            };
            // Use the same ownership transition as Terminate, then inspect the
            // events before the dispatcher could hide an early release by draining.
            let active = sim.active.remove(&0).unwrap();
            sim.release(0, active);
            let release_at = sent_at.max(sim.now + sim.config.completion_ticks);
            assert!(sim.events.iter().any(|(&(at, _), event)| {
                at == release_at && matches!(event, Event::Release(active) if active.pipe.is_some())
            }));
            assert_eq!(request_ownership(&sim), [CONTEXT, 1, 1]);

            // Remove the cache owner so only the scheduled send can keep quota
            // alive. A Weak observes identity without extending its lifetime.
            sim.evict(0, key);
            assert_eq!(page.strong_count(), 1);
            assert_eq!(
                sim.workers[0].admission.used(ResourceClass::Plaintext),
                PLAIN
            );
            assert_eq!(sim.workers[0].admission.used(ResourceClass::Ciphertext), 0);
            sim.now = sent_at - 1;
            assert!(
                sim.reserve_page(0, 0, ResourceClass::Plaintext, PLAIN)
                    .is_none()
            );
            assert!(sim.reserve(0, 0, ResourceClass::Pipe, 1).is_none());
            assert_eq!(page.strong_count(), 1);
            assert_eq!(request_ownership(&sim), [CONTEXT, 1, 1]);

            let ((at, _), event) = sim.events.pop_first().unwrap();
            assert_eq!(
                at, sent_at,
                "send was released before its scheduled completion"
            );
            sim.now = at;
            let Event::Sent(0, plain) = event else {
                panic!("canceled send must retain its scheduled completion event");
            };
            assert_eq!(Arc::as_ptr(&plain), page.as_ptr());
            assert_eq!(
                sim.workers[0].admission.used(ResourceClass::Plaintext),
                PLAIN
            );
            // The canceled Sent branch has no Active to advance; it drops this pin.
            drop(plain);
            assert_eq!(page.strong_count(), 0);
            assert_eq!(sim.workers[0].admission.used(ResourceClass::Plaintext), 0);
            assert_eq!(request_ownership(&sim), [CONTEXT, 1, 1]);
            assert_eq!(sim.report.delivered_bytes, 0);

            let ((at, _), event) = sim.events.pop_first().unwrap();
            assert_eq!(at, release_at);
            sim.now = at;
            let Event::Release(active) = event else {
                panic!("pipe must remain owned by the scheduled request release");
            };
            assert!(active.pipe.is_some());
            assert_eq!(request_ownership(&sim), [CONTEXT, 1, 1]);
            drop(active);
            sim.wake_pipe(0);
            assert_eq!(request_ownership(&sim), [0; 3]);
            assert!(
                CLASSES
                    .iter()
                    .all(|class| sim.workers[0].admission.used(*class) == 0)
            );
            let replacement_page = sim
                .reserve_page(0, 0, ResourceClass::Plaintext, PLAIN)
                .unwrap();
            let replacement_pipe = sim.reserve(0, 0, ResourceClass::Pipe, 1).unwrap();
            drop((replacement_page, replacement_pipe));
            assert_eq!(sim.run(Vec::new()).final_used, [0; 11]);
        }
    }
}
