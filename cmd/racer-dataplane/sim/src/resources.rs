//! Simulated resources: FIFO servers for NICs, disks, CPUs and the origin.
//!
//! Every resource is a deterministic FIFO server: a request that becomes
//! ready at `t` starts at `max(t, free_at)` and holds the server for its
//! service time. Reservations are made when the request is issued, so no
//! per-chunk events are needed and per-node state is a few `u64`s (struct of
//! arrays, indexed by `u32` node id).

use racer_core::time::SplitMix64;

use crate::config::{Config, Costs};
use crate::engine::{Nanos, SECONDS, mix, unit};

/// Service time of `bytes` at `bytes_per_sec`, rounded up, at least 1 ns
/// for a nonzero transfer.
pub fn service_ns(bytes: u64, bytes_per_sec: u64) -> Nanos {
    if bytes == 0 {
        return 0;
    }
    let bps = u128::from(bytes_per_sec.max(1));
    (u128::from(bytes) * u128::from(SECONDS))
        .div_ceil(bps)
        .max(1) as Nanos
}

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
    /// Same-site RTT `local_ns`; cross-site RTT `remote_ns` plus a stable
    /// random jitter in `0..jitter_ns`.
    pub fn generate(
        sites: u32,
        local_ns: u32,
        remote_ns: u32,
        jitter_ns: u32,
        rng: &mut SplitMix64,
    ) -> Self {
        let sites = sites.max(1);
        let n = sites as usize;
        let mut rtt = vec![0u32; n * n];
        for a in 0..n {
            for b in a..n {
                let v = if a == b {
                    local_ns
                } else {
                    remote_ns.saturating_add(rng.below(u64::from(jitter_ns)) as u32)
                };
                rtt[a * n + b] = v;
                rtt[b * n + a] = v;
            }
        }
        Self { sites, rtt }
    }

    pub fn sites(&self) -> u32 {
        self.sites
    }

    pub fn rtt(&self, a: u16, b: u16) -> Nanos {
        Nanos::from(self.rtt[a as usize * self.sites as usize + b as usize])
    }

    pub fn one_way(&self, a: u16, b: u16) -> Nanos {
        self.rtt(a, b) / 2
    }

    pub fn approximate_bytes(&self) -> usize {
        self.rtt.capacity() * 4
    }
}

/// Per-node NICs, disks, CPUs, with node placement into sites.
pub struct Fabric {
    pub site: Vec<u16>,
    /// Slowdown multiplier per node (1 = normal), applied to CPU, disk and
    /// NIC service times.
    pub slow: Vec<u8>,
    pub tx_free: Vec<Nanos>,
    pub rx_free: Vec<Nanos>,
    pub disk_read_free: Vec<Nanos>,
    pub disk_write_free: Vec<Nanos>,
    /// `nodes * workers` entries.
    pub cpu_free: Vec<Nanos>,
    pub crypto_free: Vec<Nanos>,
    pub workers: u8,
    pub nic_bps: u64,
    pub chunk_bytes: u64,
    pub disk_read_bps: u64,
    pub disk_write_bps: u64,
    pub disk_latency: Nanos,
    pub rtt: RttMatrix,
    pub costs: Costs,
}

impl Fabric {
    pub fn new(config: &Config, rng: &mut SplitMix64) -> Self {
        let n = config.nodes as usize;
        let sites = config.sites.clamp(1, u32::from(u16::MAX));
        let rtt = RttMatrix::generate(
            sites,
            config.local_rtt_ns,
            config.remote_rtt_ns,
            config.rtt_jitter_ns,
            rng,
        );
        let workers = config.workers.max(1);
        Self {
            site: (0..n).map(|i| (i as u32 % sites) as u16).collect(),
            slow: vec![1; n],
            tx_free: vec![0; n],
            rx_free: vec![0; n],
            disk_read_free: vec![0; n],
            disk_write_free: vec![0; n],
            cpu_free: vec![0; n * workers as usize],
            crypto_free: vec![0; n],
            workers,
            nic_bps: config.nic_bps,
            chunk_bytes: config.chunk_bytes.max(1),
            disk_read_bps: config.disk_read_bps,
            disk_write_bps: config.disk_write_bps,
            disk_latency: config.disk_latency_ns,
            rtt,
            costs: config.costs,
        }
    }

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

    /// Transfer `bytes` along `route` (node ids, first is the sender) with
    /// cut-through relay per chunk: each chunk is forwarded by a relay as soon
    /// as it has fully arrived there, so a long transfer pipelines across
    /// hops. Returns the time the last byte reaches the final node.
    pub fn transfer(&mut self, now: Nanos, route: &[u32], bytes: u64) -> Nanos {
        if route.len() < 2 {
            return now;
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
                let start = ready.max(self.tx_free[a as usize]);
                // The receiver is a work-conserving backlog of its own
                // service time from the transfer's issue, not a slot at the
                // chunk's (possibly far future) send time: a slow sender
                // trickles its chunks without holding the receiver's NIC for
                // other senders. With equal-speed senders this matches a
                // FIFO slot reservation.
                let received = self.rx_free[b as usize].max(now) + receive;
                let end = (start + send).max(received);
                self.tx_free[a as usize] = start + send;
                self.rx_free[b as usize] = received;
                ready = end + self.latency(a, b);
            }
            done = done.max(ready);
        }
        done
    }

    pub fn disk_read(&mut self, now: Nanos, node: u32, bytes: u64) -> Nanos {
        let service = self.scale(node, service_ns(bytes, self.disk_read_bps));
        reserve(&mut self.disk_read_free[node as usize], now, service).1 + self.disk_latency
    }

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

    /// Run `ns` on the node's crypto helper.
    pub fn crypto(&mut self, now: Nanos, node: u32, ns: Nanos) -> Nanos {
        let service = self.scale(node, ns);
        reserve(&mut self.crypto_free[node as usize], now, service).1
    }

    /// Forget queued work on a crashed node.
    pub fn reset_node(&mut self, node: u32, now: Nanos) {
        let i = node as usize;
        for v in [
            &mut self.tx_free[i],
            &mut self.rx_free[i],
            &mut self.disk_read_free[i],
            &mut self.disk_write_free[i],
            &mut self.crypto_free[i],
        ] {
            *v = now;
        }
        let w = self.workers as usize;
        for v in &mut self.cpu_free[i * w..(i + 1) * w] {
            *v = now;
        }
    }

    pub fn approximate_bytes(&self) -> usize {
        self.site.capacity() * 2
            + self.slow.capacity()
            + (self.tx_free.capacity()
                + self.rx_free.capacity()
                + self.disk_read_free.capacity()
                + self.disk_write_free.capacity()
                + self.cpu_free.capacity()
                + self.crypto_free.capacity())
                * 8
            + self.rtt.approximate_bytes()
    }
}

/// Result of one origin request.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum OriginOutcome {
    Ok,
    /// 412: the object changed; the caller must refresh metadata.
    PreconditionFailed,
    /// 403: denied.
    Forbidden,
    /// 5xx: transient.
    ServerError,
}

impl OriginOutcome {
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
    pub p412: f64,
    pub p403: f64,
    pub p5xx: f64,
}

/// The origin: per-cache concurrency cap (a pool of `cap` FIFO slots per
/// cache), one shared bandwidth FIFO, fixed latency, injected errors.
pub struct Origin {
    slots: Vec<Nanos>,
    cap: u32,
    caches: u32,
    bandwidth_free: Nanos,
    bps: u64,
    latency: Nanos,
    rng: SplitMix64,
}

impl Origin {
    pub fn new(config: &Config, rng: SplitMix64) -> Self {
        let caches = config.origin_caches.max(1);
        let cap = config.origin_concurrency.max(1);
        Self {
            slots: vec![0; (caches * cap) as usize],
            cap,
            caches,
            bandwidth_free: 0,
            bps: config.origin_bps,
            latency: config.origin_latency_ns,
            rng,
        }
    }

    /// Origin cache that serves `object`.
    pub fn cache_of(&self, object: u64) -> u32 {
        (mix(object) % u64::from(self.caches)) as u32
    }

    /// Issue a request for `bytes` of `object`. Returns completion time and
    /// outcome. Errors still pay latency but no bandwidth.
    pub fn request(
        &mut self,
        now: Nanos,
        object: u64,
        bytes: u64,
        errors: OriginErrors,
    ) -> (Nanos, OriginOutcome) {
        let u = unit(&mut self.rng);
        let outcome = if u < errors.p412 {
            OriginOutcome::PreconditionFailed
        } else if u < errors.p412 + errors.p403 {
            OriginOutcome::Forbidden
        } else if u < errors.p412 + errors.p403 + errors.p5xx {
            OriginOutcome::ServerError
        } else {
            OriginOutcome::Ok
        };
        let base = (self.cache_of(object) * self.cap) as usize;
        let pool = &mut self.slots[base..base + self.cap as usize];
        // Earliest free slot; ties go to the lowest index (deterministic).
        let (slot, _) = pool
            .iter()
            .enumerate()
            .min_by_key(|(i, t)| (**t, *i))
            .expect("cap >= 1");
        let start = now.max(pool[slot]);
        let first_byte = start + self.latency;
        let end = if outcome == OriginOutcome::Ok {
            reserve(
                &mut self.bandwidth_free,
                first_byte,
                service_ns(bytes, self.bps),
            )
            .1
        } else {
            first_byte
        };
        pool[slot] = end;
        (end, outcome)
    }

    pub fn approximate_bytes(&self) -> usize {
        self.slots.capacity() * 8
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::engine::{MILLIS, Role, Streams};

    fn fabric(nodes: u32, sites: u32) -> Fabric {
        let config = Config {
            nodes,
            sites,
            rtt_jitter_ns: 0,
            ..Config::default()
        };
        Fabric::new(&config, &mut Streams::new(1).stream(Role::Topology, 0))
    }

    #[test]
    fn service_time_rounds_up() {
        assert_eq!(service_ns(0, 10), 0);
        assert_eq!(service_ns(1, SECONDS), 1);
        assert_eq!(service_ns(1, 3 * SECONDS), 1);
        assert_eq!(service_ns(1 << 20, 1 << 20), SECONDS);
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
        let m = RttMatrix::generate(4, 100, 1000, 500, &mut SplitMix64::new(3));
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
        // 4 chunks over 2 hops: store-and-forward would cost 8 chunk times,
        // cut-through costs 4 + 1.
        let done = f.transfer(0, &[0, 1, 2], 4 * chunk);
        assert_eq!(done, 5 * per_chunk + 2 * lat);
        // Direct link is now busy at node 0 tx; a second transfer queues.
        let again = f.transfer(0, &[0, 2], chunk);
        assert!(again > per_chunk + lat);
        assert_eq!(f.transfer(7, &[1], 10), 7);
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
        assert!(f.crypto(0, 1, 10) == 40);
    }

    /// A slow sender takes longer, but holds the receiver's NIC only for the
    /// receiver's own service time, so a fast sender is not queued behind it.
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
        let config = Config {
            origin_caches: 1,
            origin_concurrency: 2,
            origin_latency_ns: 10 * MILLIS,
            ..Config::default()
        };
        let mut o = Origin::new(&config, SplitMix64::new(1));
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
}
