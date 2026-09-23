// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Request-bound detached signatures for plaintext peer HTTP.
use crate::{
    http::Headers,
    signing::{self, Keys},
};
use std::{collections::BTreeMap, io, time::SystemTime};

pub mod replay {
    //! Process-wide, nonce-only replay admission (never scoped by worker or volume).
    use std::{
        collections::hash_map::RandomState,
        fmt::Write,
        hash::BuildHasher,
        io,
        sync::{
            Mutex, OnceLock,
            atomic::{AtomicU64, AtomicUsize, Ordering},
        },
        time::{Duration, Instant},
    };

    pub const LIFETIME: Duration = Duration::from_secs(121);
    const CLEANUP_BUDGET: usize = 64;

    #[derive(Clone, Copy, Debug)]
    pub struct Config {
        pub capacity: usize,
        pub shards: usize,
    }
    impl Default for Config {
        fn default() -> Self {
            Self {
                capacity: 1_048_576,
                shards: 64,
            }
        }
    }
    impl Config {
        pub fn validate(self) -> io::Result<Self> {
            if self.capacity == 0
                || self.capacity > 16_777_216
                || self.shards == 0
                || self.shards > 1024
                || self.shards > self.capacity
            {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidInput,
                    "replay capacity must be 1..=16777216; shards 1..=1024 and <= capacity",
                ));
            }
            Ok(self)
        }
    }

    const NONE: usize = usize::MAX;
    #[derive(Clone)]
    struct Entry {
        nonce: [u8; 32],
        expires: Instant,
        next: usize,
        prev: usize,
    }
    struct Entries {
        // Fixed separate-chaining table. Slots are also a circular expiry FIFO:
        // deleting its head frees exactly the next insertion slot after wraparound.
        // Unlike a growable HashSet, churn cannot allocate or trigger a full rehash.
        slots: Vec<Entry>,
        buckets: Vec<usize>,
        head: usize,
        len: usize,
    }
    #[repr(align(128))]
    struct Shard {
        entries: Mutex<Entries>,
        capacity: usize,
        occupancy: AtomicUsize,
        accepted: AtomicU64,
        replayed: AtomicU64,
        full: AtomicU64,
    }
    pub(crate) struct ReplayLedger {
        shards: Vec<Shard>,
        hash: RandomState,
        #[cfg(test)]
        simulation_hash: Option<[u8; 32]>,
        capacity: usize,
    }
    impl ReplayLedger {
        pub(crate) fn new(config: Config) -> io::Result<Self> {
            let config = config.validate()?;
            let mut shards = Vec::new();
            shards
                .try_reserve_exact(config.shards)
                .map_err(io::Error::other)?;
            for i in 0..config.shards {
                let capacity = config.capacity / config.shards
                    + usize::from(i < config.capacity % config.shards);
                let mut slots = Vec::new();
                let mut buckets = Vec::new();
                // All storage is reserved at setup; no growth or allocation on admission.
                slots
                    .try_reserve_exact(capacity)
                    .map_err(io::Error::other)?;
                slots.resize(
                    capacity,
                    Entry {
                        nonce: [0; 32],
                        expires: Instant::now(),
                        next: NONE,
                        prev: NONE,
                    },
                );
                let bucket_count = capacity.next_power_of_two();
                buckets
                    .try_reserve_exact(bucket_count)
                    .map_err(io::Error::other)?;
                buckets.resize(bucket_count, NONE);
                shards.push(Shard {
                    entries: Mutex::new(Entries {
                        slots,
                        buckets,
                        head: 0,
                        len: 0,
                    }),
                    capacity,
                    occupancy: AtomicUsize::new(0),
                    accepted: AtomicU64::new(0),
                    replayed: AtomicU64::new(0),
                    full: AtomicU64::new(0),
                });
            }
            Ok(Self {
                shards,
                hash: RandomState::new(),
                #[cfg(test)]
                simulation_hash: None,
                capacity: config.capacity,
            })
        }
        #[cfg(test)]
        pub(crate) fn simulated(config: Config, key: [u8; 32]) -> io::Result<Self> {
            let mut ledger = Self::new(config)?;
            ledger.simulation_hash = Some(key);
            Ok(ledger)
        }
        fn hash_nonce(&self, nonce: [u8; 32]) -> usize {
            #[cfg(test)]
            if let Some(key) = self.simulation_hash {
                return u64::from_le_bytes(
                    blake3::keyed_hash(&key, &nonce).as_bytes()[..8]
                        .try_into()
                        .unwrap(),
                ) as usize;
            }
            self.hash.hash_one(nonce) as usize
        }
        pub(crate) fn accept(&self, nonce: [u8; 32], now: Instant) -> io::Result<()> {
            let hash = self.hash_nonce(nonce);
            let shard = &self.shards[hash % self.shards.len()];
            let mut entries = shard.entries.lock().map_err(|_| super::invalid())?;
            // Callers may have waited for this lock: preserve FIFO expiry ordering.
            let now = if entries.len == 0 {
                now
            } else {
                now.max(
                    entries.slots[(entries.head + entries.len - 1) % shard.capacity].expires
                        - LIFETIME,
                )
            };
            for _ in 0..CLEANUP_BUDGET {
                let head = entries.head;
                let expired = &entries.slots[head];
                if entries.len == 0 || expired.expires > now {
                    break;
                }
                let (prev, next) = (expired.prev, expired.next);
                if prev == NONE {
                    let bucket = (self.hash_nonce(expired.nonce) / self.shards.len())
                        & (entries.buckets.len() - 1);
                    entries.buckets[bucket] = next;
                } else {
                    entries.slots[prev].next = next;
                }
                if next != NONE {
                    entries.slots[next].prev = prev;
                }
                entries.head = (head + 1) % shard.capacity;
                entries.len -= 1;
            }
            shard.occupancy.store(entries.len, Ordering::Relaxed);
            // Duplicate detection precedes pressure; live entries are never evicted.
            let bucket = (hash / self.shards.len()) & (entries.buckets.len() - 1);
            let mut index = entries.buckets[bucket];
            while index != NONE {
                let entry = &entries.slots[index];
                if entry.nonce == nonce {
                    shard.replayed.fetch_add(1, Ordering::Relaxed);
                    return Err(super::invalid());
                }
                index = entry.next;
            }
            if entries.len == shard.capacity {
                shard.full.fetch_add(1, Ordering::Relaxed);
                return Err(io::Error::new(
                    io::ErrorKind::WouldBlock,
                    "peer replay ledger full",
                ));
            }
            let slot = (entries.head + entries.len) % shard.capacity;
            let next = entries.buckets[bucket];
            entries.slots[slot] = Entry {
                nonce,
                expires: now + LIFETIME,
                next,
                prev: NONE,
            };
            if next != NONE {
                entries.slots[next].prev = slot;
            }
            entries.buckets[bucket] = slot;
            entries.len += 1;
            shard.occupancy.store(entries.len, Ordering::Relaxed);
            shard.accepted.fetch_add(1, Ordering::Relaxed);
            Ok(())
        }
        pub(crate) fn render(&self, out: &mut String) {
            let sum = |get: fn(&Shard) -> &AtomicU64| {
                self.shards
                    .iter()
                    .map(|s| get(s).load(Ordering::Relaxed))
                    .sum::<u64>()
            };
            for (name, help, value) in [
                ("capacity", "Configured nonce capacity.", self.capacity),
                ("shards", "Replay ledger shard count.", self.shards.len()),
                (
                    "max_shard_occupancy",
                    "Largest retained shard occupancy; shard capacity is total capacity divided across shards.",
                    self.shards
                        .iter()
                        .map(|s| s.occupancy.load(Ordering::Relaxed))
                        .max()
                        .unwrap_or(0),
                ),
                (
                    "occupancy",
                    "Retained nonces including expired entries awaiting bounded cleanup.",
                    self.shards
                        .iter()
                        .map(|s| s.occupancy.load(Ordering::Relaxed))
                        .sum(),
                ),
            ] {
                writeln!(out, "# HELP racer_dataplane_replay_{name} {help}\n# TYPE racer_dataplane_replay_{name} gauge\nracer_dataplane_replay_{name} {value}").unwrap();
            }
            writeln!(out, "# HELP racer_dataplane_replay_accepted_total Admitted nonces.\n# TYPE racer_dataplane_replay_accepted_total counter\nracer_dataplane_replay_accepted_total {}", sum(|s| &s.accepted)).unwrap();
            writeln!(out, "# HELP racer_dataplane_replay_rejections_total Nonce admission rejections.\n# TYPE racer_dataplane_replay_rejections_total counter").unwrap();
            for (reason, count) in [
                ("replay", sum(|s| &s.replayed)),
                ("capacity", sum(|s| &s.full)),
            ] {
                writeln!(
                    out,
                    "racer_dataplane_replay_rejections_total{{reason=\"{reason}\"}} {count}"
                )
                .unwrap();
            }
        }
    }

    static LEDGER: OnceLock<ReplayLedger> = OnceLock::new();
    /// Call once at startup, before listeners/workers. Invalid configuration and
    /// reservation failures are returned here, never parsed on a request path.
    pub fn initialize(config: Config) -> io::Result<()> {
        LEDGER.set(ReplayLedger::new(config)?).map_err(|_| {
            io::Error::new(
                io::ErrorKind::AlreadyExists,
                "replay ledger already initialized",
            )
        })
    }
    pub(crate) fn accept(nonce: [u8; 32]) -> io::Result<()> {
        // Real-socket library fixtures do not run the daemon's startup entry point.
        #[cfg(test)]
        LEDGER.get_or_init(ReplayLedger::default);
        LEDGER
            .get()
            .ok_or_else(|| io::Error::other("replay ledger not initialized"))?
            .accept(nonce, crate::environment::now())
    }
    pub(crate) fn render(out: &mut String) {
        if let Some(ledger) = LEDGER.get() {
            ledger.render(out);
        }
    }

    #[cfg(test)]
    impl Default for ReplayLedger {
        fn default() -> Self {
            Self::new(Config {
                capacity: 65536,
                shards: 64,
            })
            .unwrap()
        }
    }

    #[cfg(test)]
    include!(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/tests/security/replay.rs"
    ));
}
#[cfg(test)]
pub(crate) use replay::ReplayLedger;

const CLOCK_WINDOW: u64 = 60;
fn timestamp() -> io::Result<u64> {
    Ok(crate::environment::wall()
        .duration_since(SystemTime::UNIX_EPOCH)
        .map_err(|_| invalid())?
        .as_secs())
}

fn invalid() -> io::Error {
    io::Error::new(
        io::ErrorKind::InvalidData,
        "invalid peer HTTP authentication",
    )
}
fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}
fn decode(value: &[u8]) -> io::Result<Vec<u8>> {
    if value.len() > 192 || value.len() % 2 != 0 {
        return Err(invalid());
    }
    value
        .chunks_exact(2)
        .map(|p| {
            let a = (p[0] as char).to_digit(16).ok_or_else(invalid)?;
            let b = (p[1] as char).to_digit(16).ok_or_else(invalid)?;
            Ok((a * 16 + b) as u8)
        })
        .collect()
}
fn one<'a>(headers: Headers<'a>, name: &str) -> io::Result<&'a [u8]> {
    let mut values = headers.iter().filter(|(n, _)| n.eq_ignore_ascii_case(name));
    let value = values.next().ok_or_else(invalid)?.1;
    if values.next().is_some() {
        return Err(invalid());
    }
    Ok(value)
}
fn semantics<'a>(headers: impl Iterator<Item = (&'a str, &'a [u8])>) -> io::Result<Vec<u8>> {
    let mut selected = BTreeMap::new();
    for (name, value) in headers {
        let name = name.to_ascii_lowercase();
        if (name.starts_with("x-racer-") && name != "x-racer-signature")
            || matches!(
                name.as_str(),
                "etag"
                    | "content-range"
                    | "content-encoding"
                    | "content-type"
                    | "range"
                    | "if-range"
            )
        {
            if selected.insert(name, value).is_some() {
                return Err(invalid());
            }
        }
    }
    let fields: Vec<&[u8]> = selected
        .iter()
        .flat_map(|(n, v)| [n.as_bytes(), *v])
        .collect();
    Ok(signing::canonical(b"headers", &fields))
}
#[derive(Clone)]
pub struct Policy {
    pub keys: Keys,
    pub universe: [u8; 32],
    pub node: [u8; 32],
    pub peers: std::collections::BTreeSet<[u8; 32]>,
}
pub struct Pending {
    context: Vec<u8>,
}
pub struct Incoming {
    context: Vec<u8>,
    pub nonce: [u8; 32],
}
impl Policy {
    pub fn request(
        &self,
        peer: [u8; 32],
        method: &str,
        target: &str,
        headers: &mut Vec<(String, Vec<u8>)>,
    ) -> io::Result<Pending> {
        let keys = self.keys.pinned();
        let mut nonce = [0; 32];
        crate::environment::random(&mut nonce).map_err(|e| io::Error::other(e.to_string()))?;
        for (name, value) in [
            ("X-Racer-Version", "2".to_owned()),
            ("X-Racer-Source", hex(&self.node)),
            ("X-Racer-Destination", hex(&peer)),
            ("X-Racer-Nonce", hex(&nonce)),
            ("X-Racer-Time", timestamp()?.to_string()),
        ] {
            headers.push((name.into(), value.into_bytes()));
        }
        let selected = semantics(headers.iter().map(|(n, v)| (n.as_str(), v.as_slice())))?;
        let context = signing::canonical(
            b"racer/http/request/v2",
            &[
                &self.universe,
                method.as_bytes(),
                target.as_bytes(),
                &0u64.to_be_bytes(),
                &0u64.to_be_bytes(),
                &selected,
            ],
        );
        headers.push((
            "X-Racer-Signature".into(),
            hex(&keys.sign(b"racer/http/request/v2", &[&context])?).into_bytes(),
        ));
        Ok(Pending { context })
    }
    pub fn receive(
        &self,
        method: &str,
        target: &str,
        headers: Headers<'_>,
    ) -> io::Result<Incoming> {
        let keys = self.keys.pinned();
        if one(headers, "x-racer-version")? != b"2" {
            return Err(invalid());
        }
        let source: [u8; 32] = decode(one(headers, "x-racer-source")?)?
            .try_into()
            .map_err(|_| invalid())?;
        if !self.peers.contains(&source)
            || decode(one(headers, "x-racer-destination")?)? != self.node
        {
            return Err(invalid());
        }
        let nonce = decode(one(headers, "x-racer-nonce")?)?
            .try_into()
            .map_err(|_| invalid())?;
        let sent: u64 = std::str::from_utf8(one(headers, "x-racer-time")?)
            .map_err(|_| invalid())?
            .parse()
            .map_err(|_| invalid())?;
        if timestamp()?.abs_diff(sent) > CLOCK_WINDOW {
            return Err(invalid());
        }
        let selected = semantics(headers.iter())?;
        let context = signing::canonical(
            b"racer/http/request/v2",
            &[
                &self.universe,
                method.as_bytes(),
                target.as_bytes(),
                &0u64.to_be_bytes(),
                &0u64.to_be_bytes(),
                &selected,
            ],
        );
        keys.verify(
            b"racer/http/request/v2",
            &[&context],
            &decode(one(headers, "x-racer-signature")?)?,
        )?;
        Ok(Incoming { context, nonce })
    }
}
impl Incoming {
    pub fn accept_once(&self) -> io::Result<()> {
        #[cfg(test)]
        if let Some(world) = crate::simulation::current() {
            return world.accept_nonce(self.nonce);
        }
        replay::accept(self.nonce)
    }
    pub fn response(
        &self,
        keys: &Keys,
        status: u16,
        len: u64,
        headers: &[(&str, &[u8])],
    ) -> io::Result<String> {
        let selected = semantics(headers.iter().copied())?;
        Ok(hex(&keys.sign(
            b"racer/http/response/v2",
            &[
                &self.context,
                &status.to_be_bytes(),
                &len.to_be_bytes(),
                &selected,
            ],
        )?))
    }
}
impl Pending {
    pub fn verify(
        &self,
        keys: &Keys,
        status: u16,
        len: u64,
        headers: Headers<'_>,
    ) -> io::Result<()> {
        let selected = semantics(headers.iter())?;
        keys.verify(
            b"racer/http/response/v2",
            &[
                &self.context,
                &status.to_be_bytes(),
                &len.to_be_bytes(),
                &selected,
            ],
            &decode(one(headers, "x-racer-signature")?)?,
        )?;
        Ok(())
    }
}

/// Request-bound failure attribution, shared by HTTP and authenticated RDMA.
pub(crate) mod failure {
    use crate::{cache, http::Headers, http_client as client};
    use cache::{
        http_metadata::{decimal, identity_encoding, text},
        peer_wire::unhex,
    };
    use client::attempt::{PeerFailure, PeerReason};
    use std::{io, net::SocketAddr};

    // Invoke only after HTTP framing/nonce or authenticated RDMA session,
    // request and descriptor binding has been checked by the transport adapter.
    pub(crate) fn reported(
        failure: PeerFailure,
        attempt: &mut Option<crate::handlers::Attempt>,
    ) -> cache::Result<()> {
        if let Some(a) = attempt {
            if failure.identity != a.route.cursor.identity || failure.candidate != a.route.candidate
            {
                return Err(invalid("foreign peer failure context").into());
            }
            if failure.reason == PeerReason::OwnerUnavailable {
                if let Some(owner) = a.owner.take() {
                    owner.failure(crate::environment::now());
                }
                return Err(io::Error::other(AttemptFailure {
                    route: a.route.clone(),
                    evidence: None,
                    reported: true,
                })
                .into());
            }
            if establishes_owner_reachability(failure.reason) {
                a.owner_reachable();
            }
        } else if failure.identity != [0; 32]
            || failure.candidate != 0
            || failure.reason == PeerReason::OwnerUnavailable
        {
            return Err(invalid("unrouted peer failure context").into());
        }
        Err(io::Error::other(failure).into())
    }
    impl client::Origin {
        pub(crate) fn error(permit: crate::breaker::Permit, error: &cache::Error, peer: bool) {
            let evidence = attempt_evidence(error);
            if matches!(
                failure_reason(error),
                PeerReason::Busy | PeerReason::Cancelled
            ) || error_chain(error).any(|e| {
                matches!(
                    e.downcast_ref::<cache::Error>(),
                    Some(cache::Error::Timeout | cache::Error::Admission(_))
                )
            }) || evidence.is_some_and(|e| {
                matches!(
                    e.cause,
                    client::attempt::Cause::CallerDeadline
                        | client::attempt::Cause::LocalPressure
                        | client::attempt::Cause::Cancelled
                        | client::attempt::Cause::BreakerRejected
                ) || !e.initiated && e.cause != client::attempt::Cause::Protocol
            }) {
                #[cfg(test)]
                if crate::simulation::current().is_some_and(|world| {
                    world.activate_mutant(crate::simulation::history::Mutant::LocalFailureAsRemote)
                }) {
                    permit.failure();
                    return;
                }
                drop(permit);
                return;
            }
            let healthy_status = matches!(
                error,
                cache::Error::NotFound | cache::Error::Gone | cache::Error::Precondition
            ) || matches!(error, cache::Error::Io(error) if error.get_ref().and_then(|e| e.downcast_ref::<cache::http_metadata::HttpStatus>()).is_some_and(|s| s.0 < 500));
            #[cfg(test)]
            if let Some(w) = crate::simulation::current() {
                w.event(
                    "breaker-error",
                    "",
                    format!(
                        "peer={peer} error={error:?} outcome={}",
                        if !peer && healthy_status {
                            "success"
                        } else {
                            "failure"
                        }
                    ),
                );
            }
            if !peer && healthy_status {
                permit.success();
            } else {
                permit.failure();
            }
        }
    }

    fn invalid(message: &'static str) -> io::Error {
        io::Error::new(io::ErrorKind::InvalidData, message)
    }
    pub(crate) fn error_status(error: &cache::Error) -> u16 {
        match failure_reason(error) {
            PeerReason::OwnerUnavailable | PeerReason::Busy | PeerReason::Unavailable => 503,
            PeerReason::Deadline => 504,
            PeerReason::NotFound => 404,
            PeerReason::Gone => 410,
            PeerReason::Precondition => 412,
            _ => 502,
        }
    }
    // io::Error::source skips its immediate payload. Preserve adapter/fanout types.
    pub(crate) fn error_chain<'a>(
        error: &'a (dyn std::error::Error + 'static),
    ) -> impl Iterator<Item = &'a (dyn std::error::Error + 'static)> {
        std::iter::successors(Some(error), |error| {
            if let Some(error) = error.downcast_ref::<io::Error>() {
                error
                    .get_ref()
                    .map(|e| e as &(dyn std::error::Error + 'static))
            } else {
                error.source()
            }
        })
    }
    pub(crate) fn error_detail<T: std::error::Error + 'static>(error: &cache::Error) -> Option<&T> {
        error_chain(error).find_map(|e| e.downcast_ref::<T>())
    }
    pub(crate) fn attempt_evidence(error: &cache::Error) -> Option<&client::attempt::Failure> {
        error_detail::<AttemptFailure>(error)
            .and_then(|f| f.evidence.as_ref())
            .or_else(|| error_detail(error))
    }
    pub(crate) fn failure_reason(error: &cache::Error) -> PeerReason {
        use client::attempt::Cause;
        if let Some(f) = semantic_failure(error) {
            return f.reason;
        }
        if owner_failure(error).is_some() {
            return PeerReason::OwnerUnavailable;
        }
        if let Some(f) = attempt_evidence(error) {
            return match f.cause {
                Cause::LocalPressure | Cause::BreakerRejected => PeerReason::Busy,
                Cause::CallerDeadline | Cause::ServiceTimeout => PeerReason::Deadline,
                Cause::Cancelled => PeerReason::Cancelled,
                Cause::Protocol => PeerReason::Protocol,
                Cause::Connection | Cause::Other => PeerReason::Service,
            };
        }
        let mut kind = io::ErrorKind::Other;
        for error in error_chain(error) {
            if let Some(error) = error.downcast_ref::<cache::Error>() {
                match error {
                    cache::Error::NotFound => return PeerReason::NotFound,
                    cache::Error::Gone => return PeerReason::Gone,
                    cache::Error::Precondition => return PeerReason::Precondition,
                    cache::Error::Timeout => return PeerReason::Deadline,
                    cache::Error::Unavailable => return PeerReason::Unavailable,
                    cache::Error::Admission(_) => return PeerReason::Busy,
                    cache::Error::InvalidData(_) => return PeerReason::Protocol,
                    _ => {}
                }
            }
            if let Some(error) = error.downcast_ref::<io::Error>() {
                kind = error.kind();
            }
        }
        match kind {
            io::ErrorKind::WouldBlock => PeerReason::Busy,
            io::ErrorKind::InvalidData => PeerReason::Protocol,
            io::ErrorKind::TimedOut => PeerReason::Deadline,
            io::ErrorKind::Interrupted => PeerReason::Cancelled,
            _ => PeerReason::Service,
        }
    }
    pub(crate) fn metric_failure(error: &cache::Error) -> crate::metrics::HttpFailure {
        use crate::metrics::{HttpErrorReason as R, HttpFailure, HttpPressure as P};
        use client::attempt::Cause;
        let reason = match failure_reason(error) {
            PeerReason::OwnerUnavailable => R::OwnerUnavailable,
            PeerReason::Busy => R::Busy,
            PeerReason::Unavailable => R::Unavailable,
            PeerReason::Protocol => R::Protocol,
            PeerReason::Service => R::Service,
            PeerReason::Deadline => R::Deadline,
            PeerReason::Cancelled => R::Cancelled,
            PeerReason::NotFound => R::NotFound,
            PeerReason::Gone => R::Gone,
            PeerReason::Precondition => R::Precondition,
        };
        // A semantic report describes the downstream cause, not this hop's socket.
        let cause = semantic_failure(error)
            .and_then(|f| f.evidence.map(|e| e.cause))
            .or_else(|| attempt_evidence(error).map(|e| e.cause));
        let pressure = match cause {
            Some(Cause::LocalPressure) => Some(P::LocalPressure),
            Some(Cause::BreakerRejected) => Some(P::BreakerRejected),
            _ if error_chain(error).any(|e| {
                matches!(
                    e.downcast_ref::<cache::Error>(),
                    Some(cache::Error::Admission(_))
                )
            }) =>
            {
                Some(P::Admission)
            }
            _ if reason == R::Busy
                && error_chain(error).any(|e| {
                    e.downcast_ref::<io::Error>()
                        .is_some_and(|e| e.kind() == io::ErrorKind::WouldBlock)
                }) =>
            {
                Some(P::WouldBlock)
            }
            _ => None,
        };
        HttpFailure { reason, pressure }
    }
    #[derive(Debug)]
    pub(crate) struct OwnerUnavailable(pub(crate) u32);
    impl std::fmt::Display for OwnerUnavailable {
        fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
            write!(f, "owner {} unavailable", self.0)
        }
    }
    impl std::error::Error for OwnerUnavailable {}
    pub(crate) fn semantic_failure(error: &cache::Error) -> Option<PeerFailure> {
        error_detail::<PeerFailure>(error).copied()
    }
    pub(crate) fn peer_failure(
        error: &cache::Error,
        identity: [u8; 32],
        candidate: u32,
    ) -> PeerFailure {
        if let Some(f) = semantic_failure(error) {
            return f;
        }
        let reason = match failure_reason(error) {
            PeerReason::OwnerUnavailable if owner_failure(error) != Some(candidate) => {
                PeerReason::Service
            }
            reason => reason,
        };
        PeerFailure {
            identity,
            candidate,
            reason,
            evidence: attempt_evidence(error)
                .and_then(crate::http_client::attempt::PeerEvidence::from_failure),
        }
    }
    pub(crate) fn owner_failure(error: &cache::Error) -> Option<u32> {
        error_detail::<OwnerUnavailable>(error).map(|e| e.0)
    }
    /// Immutable attribution captured when the direct exchange starts. A trusted
    /// report is request-bound; this type alone is not a cryptographic proof.
    #[derive(Clone, Debug)]
    pub(crate) struct AttemptRoute {
        pub(crate) cursor: crate::routing::Cursor,
        pub(crate) candidate: u32,
        pub(crate) endpoint: SocketAddr,
        pub(crate) final_hop: bool,
        pub(crate) context: String,
    }
    #[derive(Debug)]
    pub(crate) struct AttemptFailure {
        pub(crate) route: AttemptRoute,
        pub(crate) evidence: Option<client::attempt::Failure>,
        pub(crate) reported: bool,
    }
    impl AttemptFailure {
        pub(crate) fn owner_evidence(&self) -> bool {
            self.reported
                || (self.route.final_hop
                    && self.evidence.as_ref().is_some_and(|e| {
                        e.endpoint.tcp() == Some(self.route.endpoint) && e.owner_evidence()
                    }))
        }
    }
    impl std::fmt::Display for AttemptFailure {
        fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
            write!(f, "{self:?}")
        }
    }
    impl std::error::Error for AttemptFailure {}
    pub(crate) fn validate_owner_report(
        headers: Headers<'_>,
        length: Option<u64>,
        route: &AttemptRoute,
    ) -> io::Result<()> {
        let slot =
            text(headers, "x-racer-owner-unavailable")?.ok_or_else(|| invalid("missing owner"))?;
        if decimal(slot)? != u64::from(route.candidate)
            || length != Some(0)
            || text(headers, "x-racer-attempt")? != Some(route.context.as_str())
        {
            return Err(invalid("mismatched owner report"));
        }
        identity_encoding(headers)?;
        Ok(())
    }
    pub(crate) fn validate_peer_report(
        headers: Headers<'_>,
        length: Option<u64>,
        status: u16,
        route: &AttemptRoute,
    ) -> io::Result<PeerFailure> {
        let failure = PeerFailure::decode(&unhex(
            text(headers, "x-racer-failure")?.ok_or_else(|| invalid("missing peer failure"))?,
        )?)?;
        if length != Some(0)
            || text(headers, "x-racer-attempt")? != Some(route.context.as_str())
            || failure.identity != route.cursor.identity
            || failure.candidate != route.candidate
            || status != error_status(&io::Error::other(failure).into())
        {
            return Err(invalid("invalid peer failure framing/context"));
        }
        if let Some(owner) = text(headers, "x-racer-owner-unavailable")? {
            if failure.reason != PeerReason::OwnerUnavailable
                || decimal(owner)? != u64::from(failure.candidate)
            {
                return Err(invalid("contradictory owner report"));
            }
        }
        identity_encoding(headers)?;
        Ok(failure)
    }
    /// Only validated terminal value semantics establish owner reachability.
    pub(crate) fn establishes_owner_reachability(reason: PeerReason) -> bool {
        matches!(
            reason,
            PeerReason::NotFound | PeerReason::Gone | PeerReason::Precondition
        )
    }
    pub(crate) fn io_error(error: cache::Error) -> io::Error {
        if let cache::Error::Io(error) = error {
            return error;
        }
        let kind = match error.root() {
            cache::Error::Timeout => io::ErrorKind::TimedOut,
            cache::Error::NotFound => io::ErrorKind::NotFound,
            cache::Error::InvalidData(_) => io::ErrorKind::InvalidData,
            cache::Error::Io(error) => error.kind(),
            _ => io::ErrorKind::Other,
        };
        io::Error::new(kind, error)
    }
}

#[cfg(test)]
include!(concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/tests/security/http_auth.rs"
));

#[cfg(test)]
pub(crate) fn test_wall_authentication(world: &crate::simulation::World) {
    tests::wall_authentication(world);
}
