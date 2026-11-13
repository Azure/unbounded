//! FIFO cost model: deterministic servers for NICs, disks, CPUs and the origin.
//!
//! Parameters come from [`Costs`] (`costs.toml`), and every random draw (RTT
//! jitter, origin error outcomes) comes from [`Choices`]. Model fidelity
//! charges every operation to this model. Full fidelity prices simulated OS
//! operations from the same [`Costs`] table through `Driver::Sim` op costs and
//! uses the [`Fabric`] here only for per-node slowdown state.
//!
//! Every resource is a deterministic FIFO server: a request that becomes
//! ready at `t` starts at `max(t, free_at)` and holds the server for its
//! service time. Reservations are made when the request is issued, so no
//! per-chunk events are needed and per-node state is a few `u64`s (struct of
//! arrays, indexed by `u32` node id).

use std::cmp::Reverse;
use std::collections::BinaryHeap;

pub use crate::costs::service_ns;
use crate::{
    choices::{Choices, mix},
    costs::Costs,
    engine::Nanos,
};

/// Reserve a FIFO server slot. Returns `(start, end)`.
pub fn reserve(free_at: &mut Nanos, ready: Nanos, service: Nanos) -> (Nanos, Nanos) {
    let start = ready.max(*free_at);
    let end = start + service;
    *free_at = end;
    (start, end)
}

/// One-way latencies between sites, derived from a symmetric RTT matrix.
#[derive(Clone, Debug)]
pub struct RttMatrix {
    sites: u32,
    rtt: Vec<u32>,
}

impl RttMatrix {
    /// Same-site RTT `local_ns`; cross-site RTT `remote_ns` plus a stable random jitter in
    /// `0..jitter_ns`.
    pub fn generate(
        sites: u32,
        local_ns: u32,
        remote_ns: u32,
        jitter_ns: u32,
        choices: &Choices,
    ) -> Self {
        let sites = sites.max(1);
        let n = sites as usize;
        let mut rtt = vec![0u32; n * n];
        for a in 0..n {
            for b in a..n {
                let v = if a == b {
                    local_ns
                } else {
                    remote_ns.saturating_add(choices.below(u64::from(jitter_ns)) as u32)
                };
                rtt[a * n + b] = v;
                rtt[b * n + a] = v;
            }
        }
        Self { sites, rtt }
    }

    /// Number of sites.
    pub fn sites(&self) -> u32 {
        self.sites
    }

    /// Round-trip time between sites `a` and `b`.
    pub fn rtt(&self, a: u16, b: u16) -> Nanos {
        Nanos::from(self.rtt[a as usize * self.sites as usize + b as usize])
    }

    /// One-way latency between sites `a` and `b` (half the RTT).
    pub fn one_way(&self, a: u16, b: u16) -> Nanos {
        self.rtt(a, b) / 2
    }

    /// Approximate heap bytes held.
    pub fn approximate_bytes(&self) -> usize {
        self.rtt.capacity() * 4
    }
}

/// Per-node NICs, disks, CPUs, with node placement into sites.
pub struct Fabric {
    /// Site of each node, indexed by node id.
    pub site: std::rc::Rc<[u16]>,
    /// Slowdown multiplier per node (1 = normal), applied to CPU, disk and NIC service times.
    pub slow: Vec<u8>,
    /// When each node's NIC transmit side is next free.
    pub tx_free: Vec<Nanos>,
    /// When each node's NIC receive side is next free.
    pub rx_free: Vec<Nanos>,
    /// When each node's disk read queue is next free.
    pub disk_read_free: Vec<Nanos>,
    /// When each node's disk write queue is next free.
    pub disk_write_free: Vec<Nanos>,
    /// `nodes * workers` entries.
    pub cpu_free: Vec<Nanos>,
    /// `nodes * workers` entries: production pairs each worker with its own crypto helper
    /// (`src/worker.rs` `WorkerPair`).
    pub crypto_free: Vec<Nanos>,
    /// `nodes * workers` entries: completion times of the jobs each crypto helper holds, oldest
    /// first. Production bounds a worker's crypto submissions (`src/security.rs`
    /// `CryptoClient::new`, `runtime/src/offload.rs` `AdmissionQueue::enter`).
    pub crypto_jobs: Vec<std::collections::VecDeque<Nanos>>,
    /// Workers per node.
    pub workers: u8,
    /// NIC bandwidth in bytes per second.
    pub nic_bps: u64,
    /// Bytes per NIC transfer chunk.
    pub chunk_bytes: u64,
    /// Disk read bandwidth in bytes per second.
    pub disk_read_bps: u64,
    /// Disk write bandwidth in bytes per second.
    pub disk_write_bps: u64,
    /// Fixed latency added to every disk operation.
    pub disk_latency: Nanos,
    /// Inter-site round-trip times.
    pub rtt: RttMatrix,
    /// The cost table the fabric was built from.
    pub costs: Costs,
}

impl Fabric {
    /// `nodes` nodes spread round-robin over `sites` sites, priced by `costs`.
    pub fn new(nodes: u32, sites: u32, costs: &Costs, choices: &Choices) -> Self {
        let n = nodes as usize;
        let sites = sites.clamp(1, u32::from(u16::MAX));
        let ns = |v: u64| u32::try_from(v).unwrap_or(u32::MAX);
        let rtt = RttMatrix::generate(
            sites,
            ns(costs.rtt_local_ns),
            ns(costs.rtt_remote_ns),
            ns(costs.rtt_jitter_ns),
            choices,
        );
        let workers = u8::try_from(costs.cpu_workers.clamp(1, 255)).unwrap_or(1);
        Self {
            site: (0..n).map(|i| (i as u32 % sites) as u16).collect(),
            slow: vec![1; n],
            tx_free: vec![0; n],
            rx_free: vec![0; n],
            disk_read_free: vec![0; n],
            disk_write_free: vec![0; n],
            cpu_free: vec![0; n * workers as usize],
            crypto_free: vec![0; n * workers as usize],
            crypto_jobs: vec![std::collections::VecDeque::new(); n * workers as usize],
            workers,
            nic_bps: costs.nic_bytes_per_sec,
            chunk_bytes: costs.nic_chunk_bytes.max(1),
            disk_read_bps: costs.disk_read_bytes_per_sec,
            disk_write_bps: costs.disk_write_bytes_per_sec,
            disk_latency: costs.disk_latency_ns,
            rtt,
            costs: *costs,
        }
    }

    /// Number of nodes.
    pub fn nodes(&self) -> u32 {
        self.site.len() as u32
    }

    fn scale(&self, node: u32, ns: Nanos) -> Nanos {
        ns * Nanos::from(self.slow[node as usize])
    }

    /// One-way latency between two nodes.
    pub fn latency(&self, a: u32, b: u32) -> Nanos {
        self.rtt
            .one_way(self.site[a as usize], self.site[b as usize])
    }

    /// Send a small control message (no bandwidth reservation).
    pub fn message(&self, now: Nanos, from: u32, to: u32) -> Nanos {
        now + self.latency(from, to)
    }

    /// Transfer `bytes` along `route` (node ids, first is the sender) with cut-through relay per
    /// chunk: each chunk is forwarded by a relay as soon as it has fully arrived there, so a long
    /// transfer pipelines across hops.
    pub fn transfer(&mut self, now: Nanos, route: &[u32], bytes: u64) -> Nanos {
        self.transfer_until(now, route, bytes, Nanos::MAX)
            .expect("no cutoff")
    }

    /// [`Fabric::transfer`] for an exchange the receiver abandons at `cutoff`, as production drops
    /// it: no chunk starts at or after `cutoff`, started chunks keep their reservations.
    pub fn transfer_until(
        &mut self,
        now: Nanos,
        route: &[u32],
        bytes: u64,
        cutoff: Nanos,
    ) -> Option<Nanos> {
        if route.len() < 2 {
            return Some(now);
        }
        let mut done = now;
        let mut sent = 0u64;
        let mut first = true;
        while first || sent < bytes {
            first = false;
            let chunk = (bytes - sent).min(self.chunk_bytes);
            sent += chunk;
            let mut ready = now;
            for hop in route.windows(2) {
                let (a, b) = (hop[0], hop[1]);
                let wire = service_ns(chunk, self.nic_bps);
                let (send, receive) = (self.scale(a, wire), self.scale(b, wire));
                // NICs are work-conserving backlogs from the transfer's issue, not slots at a
                // chunk's future send time, so a relay or slow sender does not idle the other side.
                let backlog = self.tx_free[a as usize].max(now);
                let start = ready.max(backlog);
                if start >= cutoff {
                    return None;
                }
                let tx_done = backlog + send;
                let received = self.rx_free[b as usize].max(now) + receive;
                let end = (ready + send).max(tx_done).max(received);
                self.tx_free[a as usize] = tx_done;
                self.rx_free[b as usize] = received;
                ready = end + self.latency(a, b);
            }
            done = done.max(ready);
        }
        (done < cutoff || cutoff == Nanos::MAX).then_some(done)
    }

    /// Read `bytes` from `node`'s disk starting at `now`; returns when the read completes.
    pub fn disk_read(&mut self, now: Nanos, node: u32, bytes: u64) -> Nanos {
        let service = self.scale(node, service_ns(bytes, self.disk_read_bps));
        reserve(&mut self.disk_read_free[node as usize], now, service).1 + self.disk_latency
    }

    /// Write `bytes` to `node`'s disk starting at `now`; returns when the write completes.
    pub fn disk_write(&mut self, now: Nanos, node: u32, bytes: u64) -> Nanos {
        let service = self.scale(node, service_ns(bytes, self.disk_write_bps));
        reserve(&mut self.disk_write_free[node as usize], now, service).1 + self.disk_latency
    }

    /// Run `ns` of CPU on one worker of `node`.
    pub fn cpu(&mut self, now: Nanos, node: u32, worker: u8, ns: Nanos) -> Nanos {
        let i = node as usize * self.workers as usize + (worker % self.workers) as usize;
        let service = self.scale(node, ns);
        reserve(&mut self.cpu_free[i], now, service).1
    }

    /// Run `ns` on a worker of `node` for work whose input is ready at `ready`, possibly after
    /// `now`, without holding the worker idle until then: [`reserve`] keeps one free time per
    /// server, so booking at a future start would also block every earlier job that arrives in
    /// between. The worker is charged from `now` instead, and the work ends no earlier than
    /// `ready + ns`. Exact when `ready <= now`; otherwise the charge moves earlier by at most
    /// `ready - now`.
    pub fn cpu_from(
        &mut self,
        now: Nanos,
        ready: Nanos,
        node: u32,
        worker: u8,
        ns: Nanos,
    ) -> Nanos {
        let end = self.cpu(now, node, worker, ns);
        end.max(ready.saturating_add(self.scale(node, ns)))
    }

    /// Run `ns` on the crypto helper paired with `worker` of `node`.
    pub fn crypto(&mut self, now: Nanos, node: u32, worker: u8, ns: Nanos) -> Nanos {
        let i = node as usize * self.workers as usize + (worker % self.workers) as usize;
        let service = self.scale(node, ns);
        let end = reserve(&mut self.crypto_free[i], now, service).1;
        self.crypto_jobs[i].push_back(end);
        end
    }

    /// Crypto jobs of one worker of `node` not yet complete at `now`.
    pub fn crypto_outstanding(&mut self, now: Nanos, node: u32, worker: u8) -> usize {
        let i = node as usize * self.workers as usize + (worker % self.workers) as usize;
        let jobs = &mut self.crypto_jobs[i];
        while jobs.front().is_some_and(|&end| end <= now) {
            jobs.pop_front();
        }
        jobs.len()
    }

    /// Forget queued work on a crashed node.
    pub fn reset_node(&mut self, node: u32, now: Nanos) {
        let i = node as usize;
        for v in [
            &mut self.tx_free[i],
            &mut self.rx_free[i],
            &mut self.disk_read_free[i],
            &mut self.disk_write_free[i],
        ] {
            *v = now;
        }
        let w = self.workers as usize;
        for v in &mut self.cpu_free[i * w..(i + 1) * w] {
            *v = now;
        }
        for v in &mut self.crypto_free[i * w..(i + 1) * w] {
            *v = now;
        }
        for jobs in &mut self.crypto_jobs[i * w..(i + 1) * w] {
            jobs.clear();
        }
    }

    /// Approximate heap bytes held.
    pub fn approximate_bytes(&self) -> usize {
        self.site.len() * 2
            + self.slow.capacity()
            + (self.tx_free.capacity()
                + self.rx_free.capacity()
                + self.disk_read_free.capacity()
                + self.disk_write_free.capacity()
                + self.cpu_free.capacity()
                + self.crypto_free.capacity()
                + self.crypto_jobs.iter().map(|j| j.capacity()).sum::<usize>())
                * 8
            + self.crypto_jobs.capacity() * std::mem::size_of::<std::collections::VecDeque<Nanos>>()
            + self.rtt.approximate_bytes()
    }
}

/// Result of one origin request.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum OriginOutcome {
    /// The request succeeded.
    Ok,
    /// 412: the object changed; the caller must refresh metadata.
    PreconditionFailed,
    /// 403: denied.
    Forbidden,
    /// 5xx: transient.
    ServerError,
}

impl OriginOutcome {
    /// Short name used in reports.
    pub fn name(self) -> &'static str {
        match self {
            Self::Ok => "ok",
            Self::PreconditionFailed => "412",
            Self::Forbidden => "403",
            Self::ServerError => "5xx",
        }
    }
}

/// Injected origin error rates, each a probability in `[0, 1]`.
#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub struct OriginErrors {
    /// Probability of a 412 answer.
    pub p412: f64,
    /// Probability of a 403 answer.
    pub p403: f64,
    /// Probability of a 5xx answer.
    pub p5xx: f64,
}

/// The origin: per-cache concurrency cap (a pool of `cap` FIFO slots per cache), one shared
/// bandwidth FIFO, fixed latency, injected errors.
pub struct Origin {
    slots: Vec<Nanos>,
    cap: u32,
    caches: u32,
    bandwidth_free: Nanos,
    bps: u64,
    latency: Nanos,
    rng: Choices,
    /// Tracked requests in issue order.
    pending: Vec<OriginEntry>,
    dead: usize,
    /// Outstanding requests whose completion a cancellation moved earlier, by `(end, seq)`; rebuilt
    /// by each cancellation (see [`Origin::pop_moved`]).
    moved: BinaryHeap<Reverse<(Nanos, u64)>>,
    next_seq: u64,
    /// Slot and bandwidth free times left by finished tracked requests.
    base_slots: Vec<Nanos>,
    base_bandwidth: Nanos,
}

/// Handle of a tracked origin request.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct OriginHandle(u64);

#[derive(Clone, Copy, Debug)]
struct OriginEntry {
    seq: u64,
    live: bool,
    /// A cancellation moved this request's completion: its original completion time is stale and
    /// [`Origin::pop_moved`] delivers it.
    moved: bool,
    tag: u64,
    issue: Nanos,
    cache: u32,
    slot: usize,
    start: Nanos,
    /// Body service time; 0 for error outcomes (no bandwidth).
    service: Nanos,
    /// When the body transfer begins (`end - service`).
    body: Nanos,
    end: Nanos,
}

impl Origin {
    /// An origin priced by `costs`, drawing error outcomes from `choices`.
    pub fn new(costs: &Costs, choices: Choices) -> Self {
        let caches = u32::try_from(costs.origin_caches.max(1)).unwrap_or(u32::MAX);
        let cap = u32::try_from(costs.origin_concurrency.max(1)).unwrap_or(u32::MAX);
        Self {
            slots: vec![0; (caches * cap) as usize],
            cap,
            caches,
            bandwidth_free: 0,
            bps: costs.origin_bytes_per_sec,
            latency: costs.origin_latency_ns,
            rng: choices,
            pending: Vec::new(),
            dead: 0,
            moved: BinaryHeap::new(),
            next_seq: 0,
            base_slots: vec![0; (caches * cap) as usize],
            base_bandwidth: 0,
        }
    }

    /// Origin cache that serves `object`.
    pub fn cache_of(&self, object: u64) -> u32 {
        (mix(object) % u64::from(self.caches)) as u32
    }

    fn outcome(&mut self, errors: OriginErrors) -> OriginOutcome {
        // Raw draw 0 maps to `Ok` (shrinking toward no injected errors).
        let u = 1.0 - self.rng.unit();
        if u < errors.p412 {
            OriginOutcome::PreconditionFailed
        } else if u < errors.p412 + errors.p403 {
            OriginOutcome::Forbidden
        } else if u < errors.p412 + errors.p403 + errors.p5xx {
            OriginOutcome::ServerError
        } else {
            OriginOutcome::Ok
        }
    }

    /// Earliest free slot of `cache`; ties go to the lowest index (deterministic). Returns the
    /// global slot index.
    fn pick(slots: &[Nanos], cache: u32, cap: u32) -> usize {
        let base = (cache * cap) as usize;
        let (slot, _) = slots[base..base + cap as usize]
            .iter()
            .enumerate()
            .min_by_key(|(i, t)| (**t, *i))
            .expect("cap >= 1");
        base + slot
    }

    /// Schedule one request against the current free times.
    fn schedule(&mut self, now: Nanos, cache: u32, service: Nanos) -> (usize, Nanos, Nanos, Nanos) {
        let slot = Self::pick(&self.slots, cache, self.cap);
        let start = now.max(self.slots[slot]);
        let first_byte = start + self.latency;
        let (body, end) = if service > 0 {
            reserve(&mut self.bandwidth_free, first_byte, service)
        } else {
            (first_byte, first_byte)
        };
        self.slots[slot] = end;
        (slot, start, body, end)
    }

    /// Issue a request for `bytes` of `object`. Returns completion time and outcome. Errors still
    /// pay latency but no bandwidth.
    pub fn request(
        &mut self,
        now: Nanos,
        object: u64,
        bytes: u64,
        errors: OriginErrors,
    ) -> (Nanos, OriginOutcome) {
        let outcome = self.outcome(errors);
        let service = if outcome == OriginOutcome::Ok {
            service_ns(bytes, self.bps)
        } else {
            0
        };
        let (_, _, _, end) = self.schedule(now, self.cache_of(object), service);
        (end, outcome)
    }

    /// Like [`Origin::request`], but the request can later be canceled.
    pub fn request_tracked(
        &mut self,
        now: Nanos,
        object: u64,
        bytes: u64,
        errors: OriginErrors,
        tag: u64,
    ) -> (OriginHandle, Nanos, OriginOutcome) {
        let outcome = self.outcome(errors);
        let service = if outcome == OriginOutcome::Ok {
            service_ns(bytes, self.bps)
        } else {
            0
        };
        let cache = self.cache_of(object);
        let (slot, start, body, end) = self.schedule(now, cache, service);
        self.next_seq += 1;
        let seq = self.next_seq;
        self.pending.push(OriginEntry {
            seq,
            live: true,
            moved: false,
            tag,
            issue: now,
            cache,
            slot,
            start,
            service,
            body,
            end,
        });
        (OriginHandle(seq), end, outcome)
    }

    /// The tracked request completed: forget it.
    pub fn finish(&mut self, handle: OriginHandle) {
        if let Some(e) = self.take(handle) {
            self.retire(&e);
        }
    }

    /// Mark `handle` dead and return its entry if it was live.
    fn take(&mut self, handle: OriginHandle) -> Option<OriginEntry> {
        let i = self
            .pending
            .binary_search_by_key(&handle.0, |e| e.seq)
            .ok()?;
        let e = &mut self.pending[i];
        if !e.live {
            return None;
        }
        e.live = false;
        let taken = *e;
        self.dead += 1;
        if self.dead > 64 && self.dead * 2 > self.pending.len() {
            self.pending.retain(|e| e.live);
            self.dead = 0;
        }
        Some(taken)
    }

    fn retire(&mut self, e: &OriginEntry) {
        self.base_slots[e.slot] = self.base_slots[e.slot].max(e.end);
        if e.service > 0 {
            self.base_bandwidth = self.base_bandwidth.max(e.end);
        }
    }

    /// Cancel a tracked request at `now`. Returns how many outstanding requests moved earlier;
    /// [`Origin::pop_moved`] delivers them.
    pub fn cancel(&mut self, handle: OriginHandle, now: Nanos) -> usize {
        let Some(mut canceled) = self.take(handle) else {
            return 0;
        };
        if canceled.end <= now {
            self.retire(&canceled);
            return 0;
        }
        if canceled.start <= now {
            // Started: it held the slot (and maybe bandwidth) until now.
            canceled.end = now;
            canceled.service = canceled.service.min(now.saturating_sub(canceled.body));
            self.retire(&canceled);
        }
        // Rebuild free times from finished requests and the requests issued before the canceled one
        // (their schedule does not depend on it), then replay the later outstanding ones in issue
        // order.
        self.slots.clone_from(&self.base_slots);
        self.bandwidth_free = self.base_bandwidth;
        let split = self.pending.partition_point(|e| e.seq < handle.0);
        for e in self.pending[..split].iter().filter(|e| e.live) {
            self.slots[e.slot] = self.slots[e.slot].max(e.end);
            if e.service > 0 {
                self.bandwidth_free = self.bandwidth_free.max(e.end);
            }
        }
        let mut moved = 0;
        let (latency, cap) = (self.latency, self.cap);
        for e in self.pending[split..].iter_mut().filter(|e| e.live) {
            let before = e.end;
            if e.start <= now {
                // Started: slot fixed. A body already flowing keeps its reservation; otherwise it
                // waits for bandwidth again.
                if e.service > 0 && e.body > now {
                    let ready = (e.start + latency).max(now);
                    let (body, end) = reserve(&mut self.bandwidth_free, ready, e.service);
                    e.body = body;
                    e.end = end;
                } else if e.service > 0 {
                    self.bandwidth_free = self.bandwidth_free.max(e.end);
                }
            } else {
                let slot = Self::pick(&self.slots, e.cache, cap);
                let start = e.issue.max(self.slots[slot]).max(now);
                let first_byte = start + latency;
                let (body, end) = if e.service > 0 {
                    reserve(&mut self.bandwidth_free, first_byte, e.service)
                } else {
                    (first_byte, first_byte)
                };
                e.slot = slot;
                e.start = start;
                e.body = body;
                e.end = end;
            }
            self.slots[e.slot] = self.slots[e.slot].max(e.end);
            debug_assert!(e.end <= before, "cancellation never delays a request");
            if e.end != before {
                e.moved = true;
                moved += 1;
            }
        }
        if moved > 0 {
            // Rebuild rather than push per move: a cancellation can move every queued request, and
            // stale heap entries would pile up.
            self.moved = self
                .pending
                .iter()
                .filter(|e| e.live && e.moved)
                .map(|e| Reverse((e.end, e.seq)))
                .collect();
        }
        moved
    }

    fn entry(&self, handle: OriginHandle) -> Option<&OriginEntry> {
        let i = self
            .pending
            .binary_search_by_key(&handle.0, |e| e.seq)
            .ok()?;
        Some(&self.pending[i]).filter(|e| e.live)
    }

    /// Current completion time of an outstanding tracked request.
    pub fn end(&self, handle: OriginHandle) -> Option<Nanos> {
        self.entry(handle).map(|e| e.end)
    }

    /// Whether the completion time returned at issue still holds and is `now` (no cancellation
    /// moved it).
    pub fn due_as_issued(&self, handle: OriginHandle, now: Nanos) -> bool {
        self.entry(handle).is_some_and(|e| !e.moved && e.end == now)
    }

    /// Earliest completion among moved requests.
    pub fn next_moved(&mut self) -> Option<Nanos> {
        while let Some(&Reverse((end, seq))) = self.moved.peek() {
            if self.entry(OriginHandle(seq)).is_some_and(|e| e.end == end) {
                return Some(end);
            }
            self.moved.pop();
        }
        None
    }

    /// Tags of moved requests completing at or before `now`, in completion then issue order. The
    /// caller then calls [`Origin::finish`] for each.
    pub fn pop_moved(&mut self, now: Nanos) -> Vec<u64> {
        let mut due = Vec::new();
        while let Some(end) = self.next_moved()
            && end <= now
        {
            let Reverse((_, seq)) = self.moved.pop().expect("peeked");
            due.push(self.entry(OriginHandle(seq)).expect("live").tag);
        }
        due
    }

    /// Outstanding tracked requests.
    pub fn outstanding(&self) -> usize {
        self.pending.len() - self.dead
    }

    /// Approximate heap bytes held.
    pub fn approximate_bytes(&self) -> usize {
        self.slots.capacity() * 16 + self.pending.capacity() * size_of::<OriginEntry>()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::engine::{MILLIS, SECONDS};

    fn fabric(nodes: u32, sites: u32) -> Fabric {
        let costs = Costs {
            rtt_jitter_ns: 0,
            ..Costs::checked_in()
        };
        Fabric::new(nodes, sites, &costs, &Choices::seeded(1))
    }

    #[test]
    fn service_time_rounds_up() {
        assert_eq!(service_ns(0, 10), 0);
        assert_eq!(service_ns(1, SECONDS), 1);
        assert_eq!(service_ns(1, 3 * SECONDS), 1);
        assert_eq!(service_ns(1 << 20, 1 << 20), SECONDS);
    }

    #[test]
    fn work_ready_later_does_not_hold_the_worker_idle() {
        let mut f = fabric(1, 1);
        // Ready in 1 ms: ends 1 ms plus its cost from now.
        assert_eq!(f.cpu_from(0, 1_000_000, 0, 0, 10), 1_000_010);
        // The worker is not booked until then: a job arriving now runs at once.
        assert_eq!(f.cpu(0, 0, 0, 10), 20);
        // Ready now: an ordinary FIFO reservation.
        assert_eq!(f.cpu_from(20, 0, 0, 0, 5), 25);
    }

    #[test]
    fn fifo_queues() {
        let mut free = 0;
        assert_eq!(reserve(&mut free, 10, 5), (10, 15));
        assert_eq!(reserve(&mut free, 11, 5), (15, 20));
        assert_eq!(reserve(&mut free, 100, 1), (100, 101));
    }

    #[test]
    fn rtt_symmetric() {
        let m = RttMatrix::generate(4, 100, 1000, 500, &Choices::seeded(3));
        for a in 0..4 {
            assert_eq!(m.rtt(a, a), 100);
            for b in 0..4 {
                assert_eq!(m.rtt(a, b), m.rtt(b, a));
                if a != b {
                    assert!((1000..1500).contains(&m.rtt(a, b)));
                }
            }
        }
    }

    #[test]
    fn cut_through_pipelines_chunks() {
        let mut f = fabric(3, 1);
        let chunk = f.chunk_bytes;
        let per_chunk = service_ns(chunk, f.nic_bps);
        let lat = f.latency(0, 1);
        // 4 chunks over 2 hops: store-and-forward would cost 8 chunk times, cut-through costs 4 +
        // 1.
        let done = f.transfer(0, &[0, 1, 2], 4 * chunk);
        assert_eq!(done, 5 * per_chunk + 2 * lat);
        // Direct link is now busy at node 0 tx; a second transfer queues.
        let again = f.transfer(0, &[0, 2], chunk);
        assert!(again > per_chunk + lat);
        assert_eq!(f.transfer(7, &[1], 10), 7);
    }

    #[test]
    fn abandoned_transfer_stops_sending_at_the_cutoff() {
        let mut f = fabric(3, 1);
        let chunk = f.chunk_bytes;
        let per_chunk = service_ns(chunk, f.nic_bps);
        let lat = f.latency(0, 1);
        // Completes before the cutoff: same as an uncut transfer.
        assert_eq!(
            f.transfer_until(0, &[0, 1], chunk, Nanos::MAX - 1),
            Some(per_chunk + lat)
        );
        // Node 0 is busy until `per_chunk`; a cutoff at that instant means no chunk may start, so
        // nothing is reserved.
        assert_eq!(f.transfer_until(0, &[0, 1], 4 * chunk, per_chunk), None);
        assert_eq!(f.tx_free[0], per_chunk);
        // A cutoff partway through: chunks that started keep their slots, later chunks are never
        // sent.
        assert_eq!(f.transfer_until(0, &[0, 1], 8 * chunk, 3 * per_chunk), None);
        assert_eq!(f.tx_free[0], 3 * per_chunk);
        // Arriving exactly at the cutoff is too late.
        let mut g = fabric(2, 1);
        assert_eq!(g.transfer_until(0, &[0, 1], chunk, per_chunk + lat), None);
    }

    /// A relay forwarding a body still queued upstream must not keep its own transmit side idle
    /// until the body arrives.
    #[test]
    fn relay_transmit_is_not_held_by_a_body_queued_upstream() {
        let mut f = fabric(3, 1);
        let chunk = f.chunk_bytes;
        let per_chunk = service_ns(chunk, f.nic_bps);
        let lat = f.latency(0, 1);
        // Node 0 is busy for 100 chunks.
        f.transfer(0, &[0, 2], 100 * chunk);
        // A body from 0 relayed by 1 to 2 waits behind that backlog.
        let relayed = f.transfer(0, &[0, 1, 2], chunk);
        assert!(relayed >= 101 * per_chunk + 2 * lat, "relayed {relayed}");
        // The relay's own send at time 0 waits only for the relayed chunk's service time, not for
        // that chunk to arrive.
        assert_eq!(f.transfer(0, &[1, 0], chunk), 2 * per_chunk + lat);
        // The relay's backlog is the work it has accepted, not a future slot.
        assert_eq!(f.tx_free[1], 2 * per_chunk);
        // Two chunks issued to an idle relay at once still serialize.
        let mut g = fabric(2, 1);
        assert_eq!(g.transfer(0, &[0, 1], 2 * chunk), 2 * per_chunk + lat);
    }

    #[test]
    fn zero_byte_transfer_still_pays_latency() {
        let mut f = fabric(2, 2);
        assert_eq!(f.transfer(0, &[0, 1], 0), f.latency(0, 1));
    }

    #[test]
    fn slow_node_scales_service() {
        let mut f = fabric(2, 1);
        let fast = f.cpu(0, 0, 0, 100);
        f.slow[1] = 4;
        assert_eq!(fast, 100);
        assert_eq!(f.cpu(0, 1, 0, 100), 400);
        // Workers are independent; same worker queues.
        assert_eq!(f.cpu(0, 0, 1, 100), 100);
        assert_eq!(f.cpu(0, 0, 0, 100), 200);
        f.reset_node(0, 1000);
        assert_eq!(f.cpu(0, 0, 0, 100), 1100);
        assert!(f.disk_read(0, 1, 4096) > f.disk_latency);
        assert!(f.crypto(0, 1, 0, 10) == 40);
    }

    /// A slow sender takes longer, but holds the receiver's NIC only for the receiver's own service
    /// time, so a fast sender is not queued behind it.
    #[test]
    fn slow_sender_does_not_hold_the_receiver() {
        let mut f = fabric(3, 1);
        f.slow[0] = 100;
        let slow = f.transfer(0, &[0, 2], 1 << 20);
        let fast = f.transfer(0, &[1, 2], 1 << 20);
        assert!(fast * 10 < slow, "fast {fast} slow {slow}");
    }

    #[test]
    fn origin_cap_and_errors() {
        let costs = Costs {
            origin_caches: 1,
            origin_concurrency: 2,
            origin_latency_ns: 10 * MILLIS,
            ..Costs::checked_in()
        };
        let mut o = Origin::new(&costs, Choices::seeded(1));
        let none = OriginErrors::default();
        let (a, _) = o.request(0, 1, 0, none);
        let (b, _) = o.request(0, 1, 0, none);
        let (c, _) = o.request(0, 1, 0, none);
        assert_eq!(a, 10 * MILLIS);
        assert_eq!(b, 10 * MILLIS);
        assert_eq!(c, 20 * MILLIS, "third request waits for a slot");
        let all = OriginErrors { p403: 1.0, ..none };
        assert_eq!(o.request(0, 1, 1 << 20, all).1, OriginOutcome::Forbidden);
        let mut counts = [0u32; 4];
        let mixed = OriginErrors {
            p412: 0.25,
            p403: 0.25,
            p5xx: 0.25,
        };
        for _ in 0..4000 {
            let k = match o.request(0, 2, 0, mixed).1 {
                OriginOutcome::Ok => 0,
                OriginOutcome::PreconditionFailed => 1,
                OriginOutcome::Forbidden => 2,
                OriginOutcome::ServerError => 3,
            };
            counts[k] += 1;
        }
        for c in counts {
            assert!((800..1200).contains(&c), "{counts:?}");
        }
    }

    fn origin(caches: u32, concurrency: u32, bps: u64) -> Origin {
        let costs = Costs {
            origin_caches: u64::from(caches),
            origin_concurrency: u64::from(concurrency),
            origin_latency_ns: 10 * MILLIS,
            origin_bytes_per_sec: bps,
            ..Costs::checked_in()
        };
        Origin::new(&costs, Choices::seeded(1))
    }

    #[test]
    fn tracked_requests_match_untracked_without_cancellation() {
        let none = OriginErrors::default();
        let mixed = OriginErrors { p5xx: 0.2, ..none };
        let mut a = origin(3, 2, 1 << 20);
        let mut b = origin(3, 2, 1 << 20);
        for i in 0..200u64 {
            let now = i * 7 * MILLIS;
            let (end, outcome) = a.request(now, i % 5, 1 << 18, mixed);
            let (_, tracked_end, tracked_outcome) =
                b.request_tracked(now, i % 5, 1 << 18, mixed, i);
            assert_eq!(
                (end, outcome),
                (tracked_end, tracked_outcome),
                "request {i}"
            );
        }
    }

    #[test]
    fn canceling_a_transfer_releases_bandwidth_to_later_requests() {
        // One cache, plenty of slots, 1 MiB/s: each 1 MiB body takes 1 s.
        let mut o = origin(1, 8, 1 << 20);
        let none = OriginErrors::default();
        let (first, a_end, _) = o.request_tracked(0, 1, 1 << 20, none, 1);
        let (b, b_end, _) = o.request_tracked(0, 1, 1 << 20, none, 2);
        let (c, c_end, _) = o.request_tracked(0, 1, 1 << 20, none, 3);
        assert_eq!(a_end, 10 * MILLIS + SECONDS);
        assert_eq!(b_end, 10 * MILLIS + 2 * SECONDS);
        assert_eq!(c_end, 10 * MILLIS + 3 * SECONDS);
        // Cancel the first halfway through its body.
        let at = 10 * MILLIS + SECONDS / 2;
        assert_eq!(o.cancel(first, at), 2);
        assert_eq!(o.end(b), Some(at + SECONDS));
        assert_eq!(o.end(c), Some(at + 2 * SECONDS));
        assert_eq!(o.end(first), None);
        assert!(!o.due_as_issued(b, b_end));
        assert_eq!(o.outstanding(), 2);
        // Canceling again is a no-op.
        assert_eq!(o.cancel(first, at), 0);
        // A new request queues behind the moved ones.
        let (d, d_end, _) = o.request_tracked(at, 1, 1 << 20, none, 4);
        assert_eq!(d_end, at + 3 * SECONDS);
        assert!(o.due_as_issued(d, d_end));
        // Moved requests are delivered in completion order.
        assert_eq!(o.next_moved(), Some(at + SECONDS));
        assert!(o.pop_moved(at + SECONDS - 1).is_empty());
        assert_eq!(o.pop_moved(at + 2 * SECONDS), vec![2, 3]);
        o.finish(b);
        o.finish(c);
        assert_eq!(o.next_moved(), None);
        assert_eq!(o.outstanding(), 1);
    }

    #[test]
    fn canceling_releases_the_concurrency_slot() {
        // One slot, no bandwidth limit to speak of: errors pay latency only.
        let mut o = origin(1, 1, u64::MAX / 2);
        let none = OriginErrors::default();
        let (first, a_end, _) = o.request_tracked(0, 1, 0, none, 1);
        let (second, b_end, _) = o.request_tracked(0, 1, 0, none, 2);
        let (third, c_end, _) = o.request_tracked(0, 1, 0, none, 3);
        assert_eq!(
            (a_end, b_end, c_end),
            (10 * MILLIS, 20 * MILLIS, 30 * MILLIS)
        );
        // Canceling a request that has not started removes it entirely.
        assert_eq!(o.cancel(third, MILLIS), 0);
        // Canceling the running one frees the slot now: the second starts.
        assert_eq!(o.cancel(first, 4 * MILLIS), 1);
        assert_eq!(o.end(second), Some(14 * MILLIS));
        let (_, d_end, _) = o.request_tracked(5 * MILLIS, 1, 0, none, 4);
        assert_eq!(d_end, 24 * MILLIS);
    }

    #[test]
    fn canceling_a_queued_request_moves_only_later_ones_on_shared_bandwidth() {
        // Two caches share bandwidth; cache slots are independent.
        let mut o = origin(2, 1, 1 << 20);
        let none = OriginErrors::default();
        let (x, y) =
            (0..64)
                .map(|i| (i, o.cache_of(i)))
                .fold((None, None), |(x, y), (i, c)| match c {
                    0 if x.is_none() => (Some(i), y),
                    1 if y.is_none() => (x, Some(i)),
                    _ => (x, y),
                });
        let (x, y) = (x.unwrap(), y.unwrap());
        let (_, a_end, _) = o.request_tracked(0, x, 1 << 20, none, 1);
        let (b, b_end, _) = o.request_tracked(0, y, 1 << 20, none, 2);
        let (c, c_end, _) = o.request_tracked(0, y, 1 << 20, none, 3);
        assert_eq!(a_end, 10 * MILLIS + SECONDS);
        assert_eq!(b_end, 10 * MILLIS + 2 * SECONDS);
        // `c` waits for `b`'s slot, then for bandwidth.
        assert_eq!(c_end, b_end + 10 * MILLIS + SECONDS);
        // Cancel `b` while it waits for bandwidth (started, no body yet): `c` gets the slot now and
        // the bandwidth once `a` is done.
        assert_eq!(o.cancel(b, 500 * MILLIS), 1);
        assert_eq!(o.end(c), Some(a_end + SECONDS));
        // Completing the rest leaves nothing outstanding.
        assert_eq!(o.outstanding(), 2);
    }
}
