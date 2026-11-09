//! Peer connection slots: per-worker outbound pools and per-node ingress, as production holds
//! them, for the simulators.
//!
//! Production facts this table follows (all in the dataplane crate, `cmd/racer/src`):
//!
//! - Outbound. Each I/O worker has its own HTTP pool. A pool keeps at most `per_endpoint`
//!   connections busy per next hop (`src/http.rs` `Endpoint::capacity`, `PoolConfig::per_endpoint`
//!   from `connections_per_neighbor`, 8) and reuses an idle one first, newest first
//!   (`http/src/connection.rs` `acquire`). Every connection, busy or idle, holds one
//!   `OrdinaryOutboundConnection` slot (63 of `outbound_connections`, `src/admission.rs`
//!   `reserve_connection`). When none is free
//!   the pool closes all of its idle connections and tries once more; past that the exchange fails
//!   `Overloaded` at once, before any I/O (`http/src/connection.rs`, `clear_idle` then
//!   `reserve()?`). Over the per-endpoint cap it also fails `Overloaded` at once.
//! - Ingress. The peer listener reserves a `PeerIngressConnection` slot (`peer_connections`,
//!   2,048 per worker) on any worker that has one before it calls `accept` (`src/peer/server.rs`
//!   `listen`, `AcceptMode::Distributed`, through `flow_core::admission::Handoff::reserve`). With
//!   every worker full it accepts the connection only to write the unsigned overload head and
//!   close it, so the sender's handshake fails `Overloaded` at once (`Config::refuse_when_full`).
//!   The queue and backlog below model the listener before that change (`refuse_when_full`
//!   off): connections wait in the kernel accept queue in arrival order and are accepted as
//!   slots free; past the backlog (`std::net::TcpListener::bind`, 128) Linux drops the handshake
//!   and the sender hears nothing until its attempt gives up.
//! - A connection runs one exchange at a time and holds its ingress slot while idle too: the
//!   server waits up to `request_timeout` (30 s) for the next head (`src/peer/server.rs`
//!   `serve_connection`, `header_scope`), and the sender keeps it in its pool for the pool's idle
//!   timeout (30 s, `src/http.rs` `new_pool`). A failed or abandoned exchange is never reused
//!   (`src/peer/transport.rs`), so its connection closes.
//!
//! Time is caller-supplied nanoseconds. Freed ingress slots admit queued connections in arrival
//! order; callers collect them with [`Connections::take_admitted`] and start their exchanges.

use std::cmp::Reverse;
use std::collections::BTreeMap;
use std::collections::BinaryHeap;
use std::collections::HashMap;
use std::collections::VecDeque;

use racer_core::admission::Floors;
use racer_core::admission::Limits;
use racer_core::admission::ResourceClass;
use racer_core::admission::Rules;

/// The listener's accept backlog: `std::net::TcpListener::bind` passes 128 to `listen`
/// (`io/src/ring/fd.rs` `tcp_listener`; rust-lang/rust issue 94406). An inference for any host
/// whose `net.core.somaxconn` is lower, which caps it.
pub const LISTEN_BACKLOG: usize = 128;

/// The peer pool's idle timeout (`src/http.rs` `new_pool`) and the server's wait for the next
/// head (`request_timeout`, `src/config.rs`), both 30 s by default.
pub const IDLE_TIMEOUT_NS: u64 = 30_000_000_000;

/// Connection limits of one node.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Config {
    /// Inbound peer connections the node holds at once, over all of its workers.
    pub ingress: usize,
    /// Peer connections, busy or idle, one worker's pool holds at once.
    pub outbound: usize,
    /// Busy connections one worker's pool holds to one next hop.
    pub per_endpoint: usize,
    /// Connections that wait for an ingress slot; past it a connection is never accepted.
    pub backlog: usize,
    /// How long an idle connection stays open, in nanoseconds.
    pub idle_timeout: u64,
    /// Not production: a full listener closes its longest-idle inbound connection to accept a
    /// new one, rather than leaving the new one in the accept queue. The proposed fix for idle
    /// connections pinning ingress slots (see `dst/MODEL.md`, "Peer connection slots").
    pub evict_idle: bool,
    /// A full listener with nothing to evict accepts the connection only to answer the unsigned
    /// overload head at once, rather than leaving it in the accept queue, so the sender goes
    /// elsewhere (production: `src/peer/server.rs` `listen`, `refuse`).
    pub refuse_when_full: bool,
}

impl Config {
    /// Production defaults for a node of `workers` I/O workers (`Limits::default`).
    pub fn production(workers: u32) -> Self {
        Self::from_limits(&Limits::default(), workers)
    }

    /// The limits before peer ingress was sized apart from client connections, for comparison
    /// (`dst/MODEL.md`, "Peer connection slots"): 45 ingress slots per worker (64 client
    /// connections less 3 control and 16 outbound), 15 ordinary outbound, 2 busy per next hop,
    /// and a full listener that leaves new connections in its accept queue.
    pub fn legacy(workers: u32) -> Self {
        let workers = workers.max(1) as usize;
        Self {
            ingress: 45 * workers,
            outbound: 15,
            per_endpoint: 2,
            refuse_when_full: false,
            ..Self::production(1)
        }
    }

    /// Slots from production admission `limits`: ingress summed over `workers`, outbound per worker.
    pub fn from_limits(limits: &Limits, workers: u32) -> Self {
        let rules = Rules::new(limits.clone(), Floors::for_page(4096, 0));
        let workers = workers.max(1) as usize;
        Self {
            ingress: rules
                .limit(ResourceClass::PeerIngressConnection)
                .saturating_mul(workers),
            outbound: rules.limit(ResourceClass::OrdinaryOutboundConnection),
            per_endpoint: limits.connections_per_neighbor.get(),
            backlog: LISTEN_BACKLOG,
            idle_timeout: IDLE_TIMEOUT_NS,
            evict_idle: false,
            refuse_when_full: true,
        }
    }
}

pub type ConnId = u64;

/// What a sender gets when it asks for a connection.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Acquire {
    /// An idle connection, already accepted: the exchange starts at once.
    Reused(ConnId),
    /// A new connection the receiver accepted at once.
    Accepted(ConnId),
    /// A new connection waiting in the receiver's accept queue; it starts when
    /// [`Connections::take_admitted`] returns it.
    Queued(ConnId),
    /// A new connection the receiver's full accept queue dropped: the sender hears nothing.
    /// It still holds the sender's slot until closed.
    Dropped(ConnId),
    /// No outbound slot or per-endpoint capacity: fails `Overloaded` before any I/O.
    Overloaded,
    /// The receiver's listener is full and answers the unsigned overload head at once
    /// (`Config::refuse_when_full`).
    Refused,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum State {
    Busy,
    Idle { until: u64 },
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Ingress {
    Held,
    Queued,
    Dropped,
}

struct Conn {
    from: u32,
    worker: u8,
    to: u32,
    state: State,
    ingress: Ingress,
}

#[derive(Default)]
struct Endpoint {
    busy: usize,
    idle: Vec<ConnId>,
}

#[derive(Default)]
struct Pool {
    open: usize,
    endpoints: BTreeMap<u32, Endpoint>,
}

#[derive(Default)]
struct Listener {
    held: usize,
    queue: VecDeque<ConnId>,
    /// Idle inbound connections by when they went idle (`Config::evict_idle` only).
    idle: BTreeMap<(u64, ConnId), ()>,
}

/// Counters for reports.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Stats {
    pub reused: u64,
    pub accepted: u64,
    pub queued: u64,
    pub dropped: u64,
    pub overloaded_endpoint: u64,
    pub overloaded_pool: u64,
    pub idle_cleared: u64,
    pub idle_expired: u64,
    /// Idle inbound connections a full listener closed (`Config::evict_idle`).
    pub idle_evicted: u64,
    /// Connections a full listener refused (`Config::refuse_when_full`).
    pub refused: u64,
    pub most_queued: usize,
}

/// Every node's peer connections.
pub struct Connections {
    config: Config,
    next: ConnId,
    conns: HashMap<ConnId, Conn>,
    pools: HashMap<(u32, u8), Pool>,
    listeners: HashMap<u32, Listener>,
    expiries: BinaryHeap<Reverse<(u64, ConnId)>>,
    admitted: Vec<ConnId>,
    stats: Stats,
}

impl Connections {
    pub fn new(config: Config) -> Self {
        Self {
            config,
            next: 0,
            conns: HashMap::new(),
            pools: HashMap::new(),
            listeners: HashMap::new(),
            expiries: BinaryHeap::new(),
            admitted: Vec::new(),
            stats: Stats::default(),
        }
    }

    pub fn stats(&self) -> Stats {
        self.stats
    }

    /// Connections open now, busy or idle.
    #[cfg(test)]
    pub fn open(&self) -> usize {
        self.conns.len()
    }

    /// Ingress slots `node` holds and connections waiting in its accept queue.
    #[cfg(test)]
    pub fn ingress(&self, node: u32) -> (usize, usize) {
        self.listeners
            .get(&node)
            .map_or((0, 0), |l| (l.held, l.queue.len()))
    }

    /// Close idle connections whose timeout is at or before `now`.
    pub fn advance(&mut self, now: u64) {
        while let Some(Reverse((until, id))) = self.expiries.peek().copied() {
            if until > now {
                break;
            }
            self.expiries.pop();
            let expired = self
                .conns
                .get(&id)
                .is_some_and(|c| c.state == State::Idle { until });
            if expired {
                self.stats.idle_expired += 1;
                self.close(id);
            }
        }
    }

    /// Worker `worker` of `from` asks for a connection to `to` at `now`.
    pub fn acquire(&mut self, now: u64, from: u32, worker: u8, to: u32) -> Acquire {
        self.advance(now);
        let config = self.config;
        let pool = self.pools.entry((from, worker)).or_default();
        let endpoint = pool.endpoints.entry(to).or_default();
        if endpoint.busy >= config.per_endpoint {
            self.stats.overloaded_endpoint += 1;
            return Acquire::Overloaded;
        }
        if let Some(id) = endpoint.idle.pop() {
            endpoint.busy += 1;
            let conn = self.conns.get_mut(&id).expect("idle connection");
            if let (State::Idle { until }, true) = (conn.state, config.evict_idle)
                && let Some(listener) = self.listeners.get_mut(&to)
            {
                listener
                    .idle
                    .remove(&(until.saturating_sub(config.idle_timeout), id));
            }
            conn.state = State::Busy;
            self.stats.reused += 1;
            return Acquire::Reused(id);
        }
        let full =
            |l: &Listener| l.held >= config.ingress && (!config.evict_idle || l.idle.is_empty());
        if config.refuse_when_full && self.listeners.get(&to).is_some_and(full) {
            self.stats.refused += 1;
            if endpoint.busy == 0 && endpoint.idle.is_empty() {
                pool.endpoints.remove(&to);
            }
            return Acquire::Refused;
        }
        if pool.open >= config.outbound {
            let idle: Vec<ConnId> = pool
                .endpoints
                .values()
                .flat_map(|e| e.idle.iter().copied())
                .collect();
            self.stats.idle_cleared += idle.len() as u64;
            for id in idle {
                self.close(id);
            }
        }
        // Closing idle connections may have dropped this pool or endpoint entry.
        let pool = self.pools.entry((from, worker)).or_default();
        if pool.open >= config.outbound {
            self.stats.overloaded_pool += 1;
            if pool
                .endpoints
                .get(&to)
                .is_some_and(|e| e.busy == 0 && e.idle.is_empty())
            {
                pool.endpoints.remove(&to);
            }
            return Acquire::Overloaded;
        }
        pool.open += 1;
        pool.endpoints.entry(to).or_default().busy += 1;
        let id = self.next;
        self.next += 1;
        if config.evict_idle
            && self
                .listeners
                .get(&to)
                .is_some_and(|l| l.held >= config.ingress)
            && let Some(&(_, oldest)) = self.listeners.get(&to).and_then(|l| l.idle.keys().next())
        {
            self.stats.idle_evicted += 1;
            self.close(oldest);
        }
        let listener = self.listeners.entry(to).or_default();
        let (ingress, acquired) = if listener.held < config.ingress {
            listener.held += 1;
            self.stats.accepted += 1;
            (Ingress::Held, Acquire::Accepted(id))
        } else if listener.queue.len() < config.backlog {
            listener.queue.push_back(id);
            self.stats.queued += 1;
            self.stats.most_queued = self.stats.most_queued.max(listener.queue.len());
            (Ingress::Queued, Acquire::Queued(id))
        } else {
            self.stats.dropped += 1;
            (Ingress::Dropped, Acquire::Dropped(id))
        };
        self.conns.insert(
            id,
            Conn {
                from,
                worker,
                to,
                state: State::Busy,
                ingress,
            },
        );
        acquired
    }

    /// The exchange on `id` ended cleanly at `now`: an accepted connection goes idle for reuse; one
    /// never accepted closes.
    pub fn finish(&mut self, now: u64, id: ConnId) {
        let Some(conn) = self.conns.get_mut(&id) else {
            return;
        };
        if conn.state != State::Busy {
            return;
        }
        if conn.ingress != Ingress::Held {
            return self.close(id);
        }
        let until = now.saturating_add(self.config.idle_timeout);
        conn.state = State::Idle { until };
        let endpoint = self
            .pools
            .get_mut(&(conn.from, conn.worker))
            .and_then(|p| p.endpoints.get_mut(&conn.to))
            .expect("busy connection's endpoint");
        endpoint.busy -= 1;
        endpoint.idle.push(id);
        self.expiries.push(Reverse((until, id)));
        if self.config.evict_idle {
            let to = conn.to;
            self.listeners
                .get_mut(&to)
                .expect("an accepted connection's listener")
                .idle
                .insert((now, id), ());
        }
    }

    /// Close `id` (a failed or abandoned exchange, or an idle timeout), releasing its slots.
    pub fn close(&mut self, id: ConnId) {
        let Some(conn) = self.conns.remove(&id) else {
            return;
        };
        let key = (conn.from, conn.worker);
        let pool = self.pools.get_mut(&key).expect("connection's pool");
        pool.open -= 1;
        let endpoint = pool.endpoints.get_mut(&conn.to).expect("endpoint");
        match conn.state {
            State::Busy => endpoint.busy -= 1,
            State::Idle { .. } => endpoint.idle.retain(|i| *i != id),
        }
        if endpoint.busy == 0 && endpoint.idle.is_empty() {
            pool.endpoints.remove(&conn.to);
        }
        if pool.open == 0 {
            self.pools.remove(&key);
        }
        let listener = self.listeners.get_mut(&conn.to).expect("listener");
        if let State::Idle { until } = conn.state {
            listener
                .idle
                .remove(&(until.saturating_sub(self.config.idle_timeout), id));
        }
        match conn.ingress {
            Ingress::Held => {
                listener.held -= 1;
                if let Some(next) = listener.queue.pop_front() {
                    listener.held += 1;
                    self.conns
                        .get_mut(&next)
                        .expect("queued connection")
                        .ingress = Ingress::Held;
                    self.admitted.push(next);
                }
            }
            Ingress::Queued => listener.queue.retain(|i| *i != id),
            Ingress::Dropped => {}
        }
        if listener.held == 0 && listener.queue.is_empty() && listener.idle.is_empty() {
            self.listeners.remove(&conn.to);
        }
    }

    /// Queued connections accepted since the last call, in acceptance order.
    pub fn take_admitted(&mut self) -> Vec<ConnId> {
        std::mem::take(&mut self.admitted)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn small() -> Config {
        Config {
            ingress: 2,
            outbound: 3,
            per_endpoint: 2,
            backlog: 1,
            idle_timeout: 100,
            evict_idle: false,
            refuse_when_full: false,
        }
    }

    #[test]
    fn a_full_listener_can_refuse_instead_of_queueing() {
        let mut c = Connections::new(Config {
            refuse_when_full: true,
            ..small()
        });
        c.acquire(0, 1, 0, 9);
        c.acquire(0, 2, 0, 9);
        assert_eq!(c.acquire(0, 3, 0, 9), Acquire::Refused);
        assert_eq!(c.stats().refused, 1);
        assert_eq!(c.open(), 2);
        assert_eq!(c.ingress(9), (2, 0));
    }

    #[test]
    fn a_full_listener_can_evict_its_longest_idle_connection() {
        let mut c = Connections::new(Config {
            evict_idle: true,
            ..small()
        });
        let Acquire::Accepted(a) = c.acquire(0, 1, 0, 9) else {
            panic!()
        };
        let Acquire::Accepted(b) = c.acquire(0, 2, 0, 9) else {
            panic!()
        };
        c.finish(5, b);
        c.finish(10, a);
        // Full: the connection idle since 5 closes for the newcomer.
        assert!(matches!(c.acquire(20, 3, 0, 9), Acquire::Accepted(_)));
        assert_eq!(c.stats().idle_evicted, 1);
        assert_eq!(c.ingress(9), (2, 0));
        // Its owner reconnects: `a` is the only idle one left and goes next.
        assert!(matches!(c.acquire(20, 2, 0, 9), Acquire::Accepted(_)));
        assert_eq!(c.open(), 2);
        // Busy connections are never evicted.
        assert!(matches!(c.acquire(20, 4, 0, 9), Acquire::Queued(_)));
        // A reused connection is busy again and not evictable.
        let mut c = Connections::new(Config {
            evict_idle: true,
            ..small()
        });
        let Acquire::Accepted(a) = c.acquire(0, 1, 0, 9) else {
            panic!()
        };
        c.acquire(0, 2, 0, 9);
        c.finish(1, a);
        assert_eq!(c.acquire(2, 1, 0, 9), Acquire::Reused(a));
        assert!(matches!(c.acquire(3, 3, 0, 9), Acquire::Queued(_)));
        assert_eq!(c.stats().idle_evicted, 0);
    }

    #[test]
    fn production_defaults_follow_the_admission_rules() {
        let config = Config::production(4);
        // 2,048 peer connections per worker, on 4 workers.
        assert_eq!(config.ingress, 2048 * 4);
        // 64 outbound less the metadata reserve.
        assert_eq!(config.outbound, 63);
        assert_eq!(config.per_endpoint, 8);
        assert_eq!(config.backlog, 128);
        assert_eq!(config.idle_timeout, 30_000_000_000);
        assert!(config.refuse_when_full);
        assert!(!config.evict_idle);
        assert_eq!(Config::production(0).ingress, 2048);
        let legacy = Config::legacy(2);
        assert_eq!(
            (legacy.ingress, legacy.outbound, legacy.per_endpoint),
            (90, 15, 2)
        );
        assert!(!legacy.refuse_when_full);
        assert_eq!(legacy.backlog, 128);
    }

    #[test]
    fn ingress_queues_in_arrival_order_then_drops() {
        let mut c = Connections::new(small());
        assert_eq!(c.acquire(0, 1, 0, 9), Acquire::Accepted(0));
        assert_eq!(c.acquire(0, 2, 0, 9), Acquire::Accepted(1));
        assert_eq!(c.acquire(0, 3, 0, 9), Acquire::Queued(2));
        assert_eq!(c.acquire(0, 4, 0, 9), Acquire::Dropped(3));
        assert_eq!(c.ingress(9), (2, 1));
        c.close(0);
        assert_eq!(c.take_admitted(), vec![2]);
        assert_eq!(c.ingress(9), (2, 0));
        // A dropped connection never held a slot.
        c.close(3);
        assert_eq!(c.ingress(9), (2, 0));
        assert!(c.take_admitted().is_empty());
        let stats = c.stats();
        assert_eq!((stats.accepted, stats.queued, stats.dropped), (2, 1, 1));
    }

    #[test]
    fn a_closed_queued_connection_leaves_the_queue() {
        let mut c = Connections::new(small());
        c.acquire(0, 1, 0, 9);
        c.acquire(0, 2, 0, 9);
        assert_eq!(c.acquire(0, 3, 0, 9), Acquire::Queued(2));
        c.close(2);
        assert_eq!(c.ingress(9), (2, 0));
        c.close(0);
        assert!(c.take_admitted().is_empty());
        assert_eq!(c.ingress(9), (1, 0));
    }

    #[test]
    fn idle_connections_are_reused_and_hold_ingress_until_they_expire() {
        let mut c = Connections::new(small());
        let Acquire::Accepted(id) = c.acquire(0, 1, 0, 9) else {
            panic!("accepted");
        };
        c.finish(10, id);
        assert_eq!(c.ingress(9), (1, 0));
        assert_eq!(c.acquire(20, 1, 0, 9), Acquire::Reused(id));
        c.finish(30, id);
        c.advance(129);
        assert_eq!(c.ingress(9), (1, 0));
        c.advance(130);
        assert_eq!(c.ingress(9), (0, 0));
        assert_eq!(c.open(), 0);
        assert_eq!(c.stats().idle_expired, 1);
    }

    #[test]
    fn an_expiry_admits_a_queued_connection() {
        let mut c = Connections::new(small());
        let Acquire::Accepted(a) = c.acquire(0, 1, 0, 9) else {
            panic!()
        };
        c.acquire(0, 2, 0, 9);
        c.finish(0, a);
        assert_eq!(c.acquire(5, 3, 0, 9), Acquire::Queued(2));
        c.advance(100);
        assert_eq!(c.take_admitted(), vec![2]);
    }

    #[test]
    fn the_per_endpoint_cap_fails_overloaded() {
        let mut c = Connections::new(small());
        c.acquire(0, 1, 0, 9);
        c.acquire(0, 1, 0, 9);
        assert_eq!(c.acquire(0, 1, 0, 9), Acquire::Overloaded);
        // Another worker has its own pool.
        assert!(matches!(c.acquire(0, 1, 1, 9), Acquire::Queued(_)));
        assert_eq!(c.stats().overloaded_endpoint, 1);
    }

    #[test]
    fn a_full_pool_closes_its_idle_connections_before_failing() {
        let mut c = Connections::new(Config {
            ingress: 10,
            ..small()
        });
        let Acquire::Accepted(idle) = c.acquire(0, 1, 0, 5) else {
            panic!()
        };
        c.finish(0, idle);
        c.acquire(0, 1, 0, 6);
        c.acquire(0, 1, 0, 7);
        // Three open, one idle: the pool clears it and opens a fourth.
        assert!(matches!(c.acquire(0, 1, 0, 8), Acquire::Accepted(_)));
        assert_eq!(c.ingress(5), (0, 0));
        assert_eq!(c.stats().idle_cleared, 1);
        // All three busy now: overloaded.
        assert_eq!(c.acquire(0, 1, 0, 4), Acquire::Overloaded);
        assert_eq!(c.stats().overloaded_pool, 1);
        assert_eq!(c.open(), 3);
    }

    #[test]
    fn clearing_every_idle_connection_still_opens_a_new_one() {
        let mut c = Connections::new(Config {
            outbound: 1,
            ..small()
        });
        let Acquire::Accepted(idle) = c.acquire(0, 1, 0, 5) else {
            panic!()
        };
        c.finish(0, idle);
        assert!(matches!(c.acquire(0, 1, 0, 6), Acquire::Accepted(_)));
        assert_eq!(c.open(), 1);
    }

    #[test]
    fn an_unaccepted_connection_closes_when_its_exchange_ends() {
        let mut c = Connections::new(small());
        c.acquire(0, 1, 0, 9);
        c.acquire(0, 2, 0, 9);
        let Acquire::Queued(q) = c.acquire(0, 3, 0, 9) else {
            panic!()
        };
        c.finish(0, q);
        assert_eq!(c.open(), 2);
        assert_eq!(c.ingress(9), (2, 0));
        // Finishing twice or closing an unknown connection is harmless.
        c.finish(0, q);
        c.close(q);
    }
}
