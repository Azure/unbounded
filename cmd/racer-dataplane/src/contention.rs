//! Payload-free, discrete-event pressure model. Only admission is production code;
//! queues, service times, placement, and page ownership are explicit abstractions.

use crate::{
    model::{identity::CacheId, limits::ResourceClass, range::PAGE_BYTES},
    runtime::admission::{Admission, Reservation},
};
use std::{
    collections::{BTreeMap, VecDeque},
    sync::Arc,
};

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

type Key = (usize, u64, u64); // cache, object, page

struct Bundle {
    plain: Arc<Reservation>,
    cipher: Arc<Reservation>,
}
impl Bundle {
    fn idle(&self) -> bool {
        Arc::strong_count(&self.plain) == 1 && Arc::strong_count(&self.cipher) == 1
    }
}
struct Flight {
    bundle: Bundle,
    dirty: Option<Reservation>,
    _flight: Reservation,
    _source: Option<Arc<Reservation>>,
    waiters: Vec<(usize, u64, Reservation)>,
}
struct Worker {
    admission: Admission,
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
    ready: BTreeMap<u64, Arc<Reservation>>,
    pipe: Option<Reservation>,
    _context: Reservation,
    _connection: Reservation,
    attempts: usize,
    sending_until: Option<u64>,
    queued: bool,
}
enum Event {
    Arrive(usize, Request),
    Pump(usize),
    Fill(usize, Key),
    Sent(usize, Arc<Reservation>),
    Disk(usize, Arc<Reservation>, Reservation),
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
                    admission: Admission::new(limits),
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
    ) -> Option<Reservation> {
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
    ) -> Option<Reservation> {
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
                Event::Terminate(id, canceled) => {
                    if let Some(active) = self.active.remove(&id) {
                        if canceled {
                            self.report.canceled += 1;
                        } else {
                            self.report.failed += 1;
                        }
                        self.release(id, active);
                    }
                }
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

mod fidelity;
mod global_admission;
mod queues;
mod scenarios;
