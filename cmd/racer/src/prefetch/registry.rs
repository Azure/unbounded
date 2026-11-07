//! Ticket registry and scheduler state. Synchronous and clock-explicit, so
//! every rule is testable without I/O. The async driver lives in `service`.
//!
//! Units are `(version, page)` pairs shared by every ticket that wants them.
//! Each unit carries the set of interested tickets; a unit with no interest
//! that is not in flight is dropped. Dispatch is strict priority across
//! `high`, `normal`, `low`, round-robin across tickets of one priority, and
//! ref order within a ticket. A ticket dispatches only while its in-flight
//! bytes are under its `max_pending_bytes`.

use super::api::{
    self, Bytes, Reason, RefError, RefOutcome, RefStatus, Spec, State, Status, Summary,
};
use super::metrics::{Counter, Outcome};
use super::{Config, Priority, REFS_PAGE};
use crate::model::{CacheKey, ObjectId, ObjectVersion, PageSize, StrongEtag, VersionMetadata};
use racer_wire::control::{CacheId, NodeId};
use racer_wire::object::prefetch::{ErrorCode, PrefetchError, RefsFilter};
use std::collections::{BTreeMap, BTreeSet, HashMap, VecDeque};
use std::sync::Arc;
use std::time::{Duration, Instant};
use zeroize::Zeroizing;

/// One counter increment for a priority.
pub type CounterEvent = (Counter, Priority, u64);

/// A ticket's identity: its cache and client-chosen id.
pub type TicketKey = (CacheId, String);

/// One page of one immutable version.
#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct UnitKey {
    pub version: ObjectVersion,
    pub page: u64,
}

/// Request context a unit's fill uses. Kept in memory only.
pub struct UnitContext {
    pub metadata: Option<String>,
    pub authorization: Credential,
}

/// A ticket's origin credential (`Authorization` header), kept in memory only. `None` is a
/// ticket without one: its fills carry no credential.
pub type Credential = Option<Arc<Zeroizing<Vec<u8>>>>;

/// Whether two credentials are the same value.
fn same_credential(a: &Credential, b: &Credential) -> bool {
    match (a, b) {
        (None, None) => true,
        (Some(a), Some(b)) => Arc::ptr_eq(a, b) || a.as_slice() == b.as_slice(),
        _ => false,
    }
}

/// The result of resolving one ref's version at submit.
#[derive(Clone, Debug)]
pub enum Resolution {
    Version(VersionMetadata),
    Failed(RefError),
}

/// Why a dispatch will be retried.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RetryKind {
    Deferred,
    Overloaded,
    Unavailable,
    /// The owner could not decode or answer the operation.
    Unsupported,
}

impl RetryKind {
    fn final_error(self) -> RefError {
        match self {
            Self::Deferred | Self::Overloaded => RefError::BudgetTimeout,
            Self::Unavailable => RefError::OriginError,
            Self::Unsupported => RefError::PeerUnsupported,
        }
    }
}

/// The owner's answer for one dispatched unit.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum UnitOutcome {
    Admitted,
    Resident,
    Retry(RetryKind, Option<Duration>),
    Failed(RefError),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum UnitState {
    Queued,
    InFlight,
    Admitted,
    Failed(RefError),
}

impl UnitState {
    fn open(self) -> bool {
        matches!(self, Self::Queued | Self::InFlight)
    }
}

/// Credentials belong to tickets, not units: a fill uses the credential of the
/// highest-priority interested ticket whose credential this unit has not seen
/// rejected, so a ticket is never served with another ticket's credential
/// unless that ticket is the one being filled for. An origin refusal fails
/// only the tickets holding the refused credential; the unit is retried with
/// the next distinct credential among the remaining tickets, each distinct
/// credential at most once.
struct Unit {
    metadata: VersionMetadata,
    /// The first interested ref's origin metadata.
    ref_metadata: Option<String>,
    interest: BTreeSet<TicketKey>,
    /// The credential of the in-flight fill.
    credential: Option<Credential>,
    /// Credentials the origin refused for this unit.
    rejected: Vec<Credential>,
    state: UnitState,
    attempts: u32,
    not_before: Option<Instant>,
    owner: Option<NodeId>,
    dispatcher: Option<TicketKey>,
}

/// One ref after version resolution.
struct TicketRef {
    key: CacheKey,
    etag: Option<StrongEtag>,
    offset: u64,
    length: u64,
    version: Option<ObjectVersion>,
    pages: std::ops::Range<u64>,
    error: Option<RefError>,
}

struct Ticket {
    cache: CacheId,
    id: String,
    priority: Priority,
    /// Replaced by every accepted PUT.
    authorization: Credential,
    /// Units the origin refused this ticket's credential for; they fail only
    /// this ticket's refs.
    rejected: std::collections::HashSet<UnitKey>,
    generation: u64,
    sequence: u64,
    hash: [u8; 32],
    ttl_seconds: u64,
    max_pending_bytes: u64,
    expires_at: Instant,
    expires_at_ms: u64,
    created_at_ms: u64,
    updated_at_ms: u64,
    canceled: Option<Reason>,
    /// Interest released (canceled or expired). The ticket is retained for
    /// status until `remove_at`.
    released: bool,
    remove_at: Option<Instant>,
    refs: Vec<TicketRef>,
    /// Desired units, deduplicated, in ref order.
    units: Vec<UnitKey>,
    cursor: usize,
    open: usize,
    in_flight_bytes: u64,
    logical_requested: u64,
    aligned_requested: u64,
    already_resident: u64,
    joined: u64,
    fetched: u64,
    admitted: u64,
    /// Ref outcomes frozen when interest was released.
    frozen: Option<Vec<(RefOutcome, Option<RefError>)>>,
    finished_recorded: bool,
}

/// A batch of units of one version for one destination decision.
pub struct Batch {
    pub dispatcher: TicketKey,
    pub priority: Priority,
    pub metadata: VersionMetadata,
    pub context: Arc<UnitContext>,
    pub pages: Vec<u64>,
}

/// What a PUT does after the idempotency check.
#[derive(Debug, Eq, PartialEq)]
pub enum PutCheck {
    /// Same `(generation, sequence)` and payload: the current status.
    Replay(Box<Status>),
    /// Resolve versions and call [`Registry::apply_put`].
    Proceed,
}

/// Ticket and unit state for one node.
pub struct Registry {
    config: Config,
    /// Wall time for ticket timestamps.
    clock: racer_io::Clock,
    tickets: BTreeMap<TicketKey, Ticket>,
    units: HashMap<UnitKey, Unit>,
    rotation: [VecDeque<TicketKey>; 3],
    /// Bytes of queued and in-flight units.
    pending_bytes: u64,
    rng: u64,
    events: Vec<CounterEvent>,
    finished: Vec<(Priority, Outcome)>,
    /// Cluster page size refs are cut at.
    page: PageSize,
}

/// Wall-clock milliseconds for status timestamps.
pub fn wall_ms(clock: &racer_io::Clock) -> u64 {
    clock
        .wall_now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| d.as_millis().min(u128::from(u64::MAX)) as u64)
}

impl Registry {
    pub fn new(config: Config, seed: u64, clock: racer_io::Clock) -> Self {
        Self {
            config,
            clock,
            tickets: BTreeMap::new(),
            units: HashMap::new(),
            rotation: Default::default(),
            pending_bytes: 0,
            rng: seed | 1,
            events: Vec::new(),
            finished: Vec::new(),
            page: PageSize::DEFAULT,
        }
    }

    /// Cut refs into pages of `size`, the cluster page size. Set before the
    /// first ticket; a cluster never changes its page size once adopted.
    pub fn set_page_size(&mut self, size: PageSize) {
        self.page = size;
    }

    pub fn configure(&mut self, config: Config) {
        self.config = config;
    }

    pub fn config(&self) -> &Config {
        &self.config
    }

    /// Drain metric deltas recorded since the last call.
    pub fn take_events(&mut self) -> (Vec<CounterEvent>, Vec<(Priority, Outcome)>) {
        (
            std::mem::take(&mut self.events),
            std::mem::take(&mut self.finished),
        )
    }

    fn event(&mut self, counter: Counter, priority: Priority, value: u64) {
        if value != 0 {
            self.events.push((counter, priority, value));
        }
    }

    /// Uniform `[0, 1)` for backoff jitter.
    fn unit_random(&mut self) -> f64 {
        self.rng ^= self.rng << 13;
        self.rng ^= self.rng >> 7;
        self.rng ^= self.rng << 17;
        (self.rng >> 11) as f64 / (1u64 << 53) as f64
    }

    /// Queued and in-flight bytes across all tickets.
    pub fn pending_bytes(&self) -> u64 {
        self.pending_bytes
    }

    /// Node-wide gauges: queued bytes (by the highest interested priority)
    /// and live tickets per priority.
    pub fn gauges(&self) -> ([u64; 3], [u64; 3]) {
        let mut queued = [0; 3];
        let mut tickets = [0; 3];
        for ticket in self.tickets.values().filter(|t| !t.released) {
            tickets[ticket.priority.index()] += 1;
        }
        for unit in self.units.values().filter(|u| u.state == UnitState::Queued) {
            let priority = unit
                .interest
                .iter()
                .filter_map(|k| self.tickets.get(k))
                .map(|t| t.priority)
                .min()
                .unwrap_or_default();
            queued[priority.index()] += self.page.get();
        }
        (queued, tickets)
    }

    /// Whether any unit is queued or in flight.
    pub fn has_open(&self) -> bool {
        self.units.values().any(|unit| unit.state.open())
    }

    /// Validate a PUT against the existing ticket before resolving versions.
    pub fn check_put(
        &mut self,
        cache: &CacheId,
        id: &str,
        spec: &Spec,
        authorization: &Credential,
        now: Instant,
    ) -> Result<PutCheck, PrefetchError> {
        let key = (cache.clone(), id.to_owned());
        if let Some(uid) = &spec.cache_uid
            && uid != &cache.0
        {
            return Err(PrefetchError::new(
                ErrorCode::CacheUidMismatch,
                format!("cache_uid {uid} is not this cache"),
            ));
        }
        let Some(ticket) = self.tickets.get_mut(&key) else {
            return Ok(PutCheck::Proceed);
        };
        let (g, s) = (spec.generation, spec.sequence);
        if g < ticket.generation {
            return Err(PrefetchError::new(
                ErrorCode::StaleGeneration,
                format!("generation {g} is older than current {}", ticket.generation),
            ));
        }
        if g == ticket.generation {
            if s < ticket.sequence {
                return Err(PrefetchError::new(
                    ErrorCode::StaleSequence,
                    format!("sequence {s} is older than current {}", ticket.sequence),
                ));
            }
            if s == ticket.sequence {
                if spec.hash != ticket.hash {
                    return Err(PrefetchError::new(
                        ErrorCode::PayloadMismatch,
                        format!("generation {g} sequence {s} was submitted with another payload"),
                    ));
                }
                // A rotated credential is accepted: the ticket is applied again with it.
                if !ticket.released && !same_credential(&ticket.authorization, authorization) {
                    return Ok(PutCheck::Proceed);
                }
                if !ticket.released {
                    ticket.expires_at = now + Duration::from_secs(ticket.ttl_seconds);
                    ticket.expires_at_ms = wall_ms(&self.clock) + ticket.ttl_seconds * 1000;
                }
                return Ok(PutCheck::Replay(Box::new(
                    self.status(cache, id, None, None)?,
                )));
            }
        }
        Ok(PutCheck::Proceed)
    }

    /// Admission limits for a PUT that creates or changes a ticket.
    fn check_limits(&mut self, key: &TicketKey) -> Result<(), PrefetchError> {
        if self.pending_bytes >= self.config.node_pending_bytes {
            return Err(PrefetchError::new(
                ErrorCode::PendingLimit,
                "node prefetch pending bytes are at the limit",
            )
            .with_retry_after(5));
        }
        if !self.tickets.contains_key(key) && self.tickets.len() as u64 >= self.config.max_tickets {
            // Retained terminal tickets make room before live ones are refused.
            let oldest = self
                .tickets
                .iter()
                .filter(|(_, t)| t.released)
                .min_by_key(|(_, t)| t.remove_at)
                .map(|(k, _)| k.clone());
            match oldest {
                Some(oldest) => {
                    self.tickets.remove(&oldest);
                }
                None => {
                    return Err(PrefetchError::new(
                        ErrorCode::TicketLimit,
                        "node prefetch ticket limit reached",
                    )
                    .with_retry_after(5));
                }
            }
        }
        Ok(())
    }

    /// Create, update, supersede, or restart a ticket. Returns 201 or 200.
    #[allow(clippy::too_many_arguments)]
    pub fn apply_put(
        &mut self,
        cache: &CacheId,
        id: &str,
        spec: Spec,
        resolved: Vec<Resolution>,
        authorization: Credential,
        now: Instant,
    ) -> Result<(u16, Status), PrefetchError> {
        // Another PUT may have raced the version resolution.
        if let PutCheck::Replay(status) = self.check_put(cache, id, &spec, &authorization, now)? {
            return Ok((200, *status));
        }
        let key = (cache.clone(), id.to_owned());
        self.check_limits(&key)?;
        let wall = wall_ms(&self.clock);
        let created = !self.tickets.contains_key(&key);
        if let Some(old) = self.tickets.get(&key) {
            let priority = old.priority;
            self.event(Counter::WindowUpdates, priority, 1);
        }
        // Normalize refs into page sets.
        let mut refs = Vec::with_capacity(spec.refs.len());
        let mut desired: Vec<(UnitKey, VersionMetadata, Option<String>)> = Vec::new();
        let mut seen = std::collections::HashSet::new();
        let mut logical = 0u64;
        for (raw, resolution) in spec.refs.iter().zip(resolved) {
            let mut entry = TicketRef {
                key: raw.key,
                etag: raw.etag.clone(),
                offset: raw.offset,
                length: raw.length.unwrap_or(0),
                version: None,
                pages: 0..0,
                error: None,
            };
            match resolution {
                Resolution::Failed(error) => entry.error = Some(error),
                Resolution::Version(metadata) => {
                    entry.etag = Some(metadata.version.etag.clone());
                    entry.version = Some(metadata.version.clone());
                    let length = metadata.length;
                    if length == 0 && raw.offset == 0 && raw.length.is_none() {
                        entry.length = 0;
                    } else if raw.offset >= length {
                        entry.error = Some(RefError::InvalidRange);
                    } else {
                        let end = raw
                            .length
                            .map_or(length, |l| raw.offset.saturating_add(l).min(length));
                        entry.length = end - raw.offset;
                        let page = self.page.get();
                        entry.pages = raw.offset / page..end.div_ceil(page);
                        logical = logical.saturating_add(entry.length);
                        for page in entry.pages.clone() {
                            let unit = UnitKey {
                                version: metadata.version.clone(),
                                page,
                            };
                            if seen.insert(unit.clone()) {
                                desired.push((unit, metadata.clone(), raw.metadata.clone()));
                            }
                        }
                    }
                }
            }
            refs.push(entry);
        }
        // Release the old window's interest in pages it no longer wants.
        let wanted: std::collections::HashSet<&UnitKey> = desired.iter().map(|(k, ..)| k).collect();
        if let Some(old) = self.tickets.get(&key)
            && !old.released
        {
            let dropped: Vec<UnitKey> = old
                .units
                .iter()
                .filter(|unit| !wanted.contains(unit))
                .cloned()
                .collect();
            let priority = old.priority;
            for unit in dropped {
                if self.drop_interest(&key, &unit) {
                    self.event(Counter::WindowDroppedBytes, priority, self.page.get());
                }
            }
        }
        let previous = self.tickets.remove(&key);
        let kept: std::collections::HashSet<UnitKey> = previous
            .as_ref()
            .filter(|old| !old.released)
            .map(|old| old.units.iter().cloned().collect())
            .unwrap_or_default();
        let mut ticket = Ticket {
            cache: cache.clone(),
            id: id.to_owned(),
            priority: spec.priority,
            authorization,
            rejected: std::collections::HashSet::new(),
            generation: spec.generation,
            sequence: spec.sequence,
            hash: spec.hash,
            ttl_seconds: spec.ttl_seconds,
            max_pending_bytes: spec.max_pending_bytes,
            expires_at: now + Duration::from_secs(spec.ttl_seconds),
            expires_at_ms: wall + spec.ttl_seconds * 1000,
            created_at_ms: previous.as_ref().map_or(wall, |old| old.created_at_ms),
            updated_at_ms: wall,
            canceled: None,
            released: false,
            remove_at: None,
            refs,
            units: desired.iter().map(|(k, ..)| k.clone()).collect(),
            cursor: 0,
            open: 0,
            in_flight_bytes: 0,
            logical_requested: logical,
            aligned_requested: desired.len() as u64 * self.page.get(),
            already_resident: 0,
            joined: 0,
            fetched: 0,
            admitted: 0,
            frozen: None,
            finished_recorded: false,
        };
        if let Some(old) = &previous
            && !old.released
            && old.generation == spec.generation
        {
            // A window update keeps what this ticket already achieved.
            ticket.already_resident = old.already_resident;
            ticket.joined = old.joined;
            ticket.fetched = old.fetched;
            ticket.admitted = old.admitted;
            ticket.in_flight_bytes = old.in_flight_bytes;
        } else if let Some(old) = &previous
            && !old.released
        {
            ticket.in_flight_bytes = old.in_flight_bytes;
        }
        let priority = ticket.priority;
        self.event(Counter::RequestedBytes, priority, ticket.logical_requested);
        self.event(Counter::AlignedBytes, priority, ticket.aligned_requested);
        for (unit_key, metadata, ref_metadata) in desired {
            let already = kept.contains(&unit_key);
            match self.units.get_mut(&unit_key) {
                // The origin already refused this credential for the unit.
                Some(unit)
                    if unit.state.open()
                        && unit
                            .rejected
                            .iter()
                            .any(|c| same_credential(c, &ticket.authorization)) =>
                {
                    unit.interest.remove(&key);
                    ticket.rejected.insert(unit_key);
                }
                Some(unit) => {
                    unit.interest.insert(key.clone());
                    if unit.state.open() {
                        ticket.open += 1;
                    } else if unit.state == UnitState::Admitted && !already {
                        ticket.already_resident += self.page.get();
                        ticket.admitted += self.page.get();
                        self.events.push((
                            Counter::AlreadyResidentBytes,
                            priority,
                            self.page.get(),
                        ));
                    }
                }
                None => {
                    self.units.insert(
                        unit_key,
                        Unit {
                            metadata,
                            ref_metadata,
                            interest: BTreeSet::from([key.clone()]),
                            credential: None,
                            rejected: Vec::new(),
                            state: UnitState::Queued,
                            attempts: 0,
                            not_before: None,
                            owner: None,
                            dispatcher: None,
                        },
                    );
                    self.pending_bytes += self.page.get();
                    ticket.open += 1;
                }
            }
        }
        self.tickets.insert(key.clone(), ticket);
        self.schedule(&key);
        self.settle(&key);
        let status = self.status(cache, id, None, None)?;
        Ok((if created { 201 } else { 200 }, status))
    }

    /// Put a ticket with open units into its priority's rotation.
    fn schedule(&mut self, key: &TicketKey) {
        let Some(ticket) = self.tickets.get(key) else {
            return;
        };
        if ticket.open == 0 || ticket.released {
            return;
        }
        let rotation = &mut self.rotation[ticket.priority.index()];
        if !rotation.contains(key) {
            rotation.push_back(key.clone());
        }
        for other in Priority::ALL {
            if other != ticket.priority {
                self.rotation[other.index()].retain(|k| k != key);
            }
        }
    }

    /// Remove `key`'s interest in `unit`. Returns whether a queued unit was
    /// dropped because nothing else wants it.
    fn drop_interest(&mut self, key: &TicketKey, unit_key: &UnitKey) -> bool {
        let Some(unit) = self.units.get_mut(unit_key) else {
            return false;
        };
        unit.interest.remove(key);
        if !unit.interest.is_empty() {
            return false;
        }
        match unit.state {
            // In-flight fills complete normally and are dropped then.
            UnitState::InFlight => false,
            UnitState::Queued => {
                self.units.remove(unit_key);
                self.pending_bytes = self.pending_bytes.saturating_sub(self.page.get());
                true
            }
            UnitState::Admitted | UnitState::Failed(_) => {
                self.units.remove(unit_key);
                false
            }
        }
    }

    /// Record a ticket's outcome the first time its window settles.
    fn settle(&mut self, key: &TicketKey) {
        let Some(ticket) = self.tickets.get(key) else {
            return;
        };
        if ticket.finished_recorded || ticket.open != 0 || ticket.released {
            return;
        }
        let state = self.state_of(ticket);
        let outcome = match state {
            State::Succeeded => Outcome::Succeeded,
            State::Partial => Outcome::Partial,
            State::Failed => Outcome::Failed,
            _ => return,
        };
        let priority = ticket.priority;
        let failed = self.failed_bytes(ticket);
        self.finished.push((priority, outcome));
        self.event(Counter::FailedBytes, priority, failed);
        if let Some(ticket) = self.tickets.get_mut(key) {
            ticket.finished_recorded = true;
        }
    }

    /// Choose the next batch: strict priority, round-robin across tickets,
    /// ref order within a ticket. Marks the units in flight.
    pub fn next_batch(&mut self, now: Instant, max_pages: usize) -> Option<Batch> {
        for priority in Priority::ALL {
            let rounds = self.rotation[priority.index()].len();
            for _ in 0..rounds {
                let Some(key) = self.rotation[priority.index()].pop_front() else {
                    break;
                };
                let Some(ticket) = self.tickets.get(&key) else {
                    continue;
                };
                if ticket.released || ticket.open == 0 || ticket.priority != priority {
                    continue;
                }
                let batch = self.collect(&key, now, max_pages);
                self.rotation[priority.index()].push_back(key);
                if batch.is_some() {
                    return batch;
                }
            }
        }
        None
    }

    fn collect(&mut self, key: &TicketKey, now: Instant, max_pages: usize) -> Option<Batch> {
        const SCAN: usize = 8192;
        let ticket = self.tickets.get(key)?;
        let budget = ticket
            .max_pending_bytes
            .saturating_sub(ticket.in_flight_bytes);
        // An idle ticket may always dispatch one page, even under a bound
        // smaller than a page.
        let floor = usize::from(ticket.in_flight_bytes == 0);
        let room = usize::try_from(budget / self.page.get())
            .unwrap_or(usize::MAX)
            .max(floor)
            .min(max_pages);
        if room == 0 {
            return None;
        }
        let mut first_queued = None;
        let mut chosen: Vec<UnitKey> = Vec::new();
        // One batch carries one credential and one origin metadata value.
        let mut batch_context: Option<(Credential, Option<String>)> = None;
        for (index, unit_key) in ticket
            .units
            .iter()
            .enumerate()
            .skip(ticket.cursor)
            .take(SCAN)
        {
            let Some(unit) = self.units.get(unit_key) else {
                continue;
            };
            if unit.state != UnitState::Queued {
                continue;
            }
            first_queued.get_or_insert(index);
            if unit.not_before.is_some_and(|at| at > now) {
                continue;
            }
            if chosen
                .first()
                .is_some_and(|first| first.version != unit_key.version)
            {
                continue;
            }
            let Some(credential) = self.credential_for(unit) else {
                continue;
            };
            match &batch_context {
                Some((batch_credential, metadata))
                    if !same_credential(batch_credential, &credential)
                        || metadata != &unit.ref_metadata =>
                {
                    continue;
                }
                Some(_) => {}
                None => batch_context = Some((credential, unit.ref_metadata.clone())),
            }
            chosen.push(unit_key.clone());
            if chosen.len() >= room {
                break;
            }
        }
        let priority = ticket.priority;
        let cursor = first_queued.unwrap_or(ticket.cursor);
        if let Some(ticket) = self.tickets.get_mut(key) {
            ticket.cursor = cursor;
        }
        let first = chosen.first()?.clone();
        let unit = self.units.get(&first)?;
        let metadata = unit.metadata.clone();
        let (credential, ref_metadata) = batch_context?;
        let context = Arc::new(UnitContext {
            metadata: ref_metadata,
            authorization: credential.clone(),
        });
        let mut pages = Vec::with_capacity(chosen.len());
        for unit_key in &chosen {
            let unit = self.units.get_mut(unit_key).expect("chosen unit");
            unit.state = UnitState::InFlight;
            unit.dispatcher = Some(key.clone());
            unit.credential = Some(credential.clone());
            pages.push(unit_key.page);
        }
        pages.sort_unstable();
        pages.dedup();
        if let Some(ticket) = self.tickets.get_mut(key) {
            ticket.in_flight_bytes += chosen.len() as u64 * self.page.get();
        }
        Some(Batch {
            dispatcher: key.clone(),
            priority,
            metadata,
            context,
            pages,
        })
    }

    /// The credential a fill of `unit` uses: that of the highest-priority live
    /// interested ticket (ties by ticket key). `None` when no ticket can be
    /// served; `Some(None)` is a fill without a credential.
    fn credential_for(&self, unit: &Unit) -> Option<Credential> {
        unit.interest
            .iter()
            .filter_map(|key| self.tickets.get(key))
            .filter(|ticket| !ticket.released)
            .filter(|ticket| {
                !unit
                    .rejected
                    .iter()
                    .any(|c| same_credential(c, &ticket.authorization))
            })
            .min_by_key(|ticket| ticket.priority)
            .map(|ticket| ticket.authorization.clone())
    }

    /// The origin refused the in-flight fill's credential: fail the unit for
    /// the tickets holding it and keep it queued for the others. Returns false
    /// when no other ticket is left, so the unit fails as usual.
    fn reject_credential(&mut self, unit_key: &UnitKey) -> bool {
        let Some(unit) = self.units.get_mut(unit_key) else {
            return false;
        };
        let Some(used) = unit.credential.take() else {
            return false;
        };
        let refused: Vec<TicketKey> = unit
            .interest
            .iter()
            .filter(|key| {
                self.tickets
                    .get(*key)
                    .is_none_or(|t| t.released || same_credential(&t.authorization, &used))
            })
            .cloned()
            .collect();
        if refused.len() == unit.interest.len() {
            return false;
        }
        unit.rejected.push(used);
        for key in &refused {
            unit.interest.remove(key);
        }
        unit.state = UnitState::Queued;
        unit.not_before = None;
        let interested: Vec<TicketKey> = unit.interest.iter().cloned().collect();
        for key in &refused {
            if let Some(ticket) = self.tickets.get_mut(key) {
                ticket.rejected.insert(unit_key.clone());
                ticket.open = ticket.open.saturating_sub(1);
            }
            self.settle(key);
        }
        for key in interested {
            if let Some(ticket) = self.tickets.get_mut(&key) {
                ticket.cursor = 0;
            }
            self.schedule(&key);
        }
        true
    }

    /// Apply one owner answer.
    pub fn complete(
        &mut self,
        unit_key: &UnitKey,
        outcome: UnitOutcome,
        owner: Option<NodeId>,
        now: Instant,
    ) {
        let Some(unit) = self.units.get_mut(unit_key) else {
            return;
        };
        if unit.state != UnitState::InFlight {
            return;
        }
        let dispatcher = unit.dispatcher.take();
        if let Some(ticket) = dispatcher.as_ref().and_then(|k| self.tickets.get_mut(k)) {
            ticket.in_flight_bytes = ticket.in_flight_bytes.saturating_sub(self.page.get());
        }
        let dispatch_priority = dispatcher
            .as_ref()
            .and_then(|k| self.tickets.get(k))
            .map_or(Priority::Normal, |t| t.priority);
        if outcome == UnitOutcome::Failed(RefError::Forbidden) && self.reject_credential(unit_key) {
            return;
        }
        let max_attempts = self.config.max_attempts as u32;
        let next = match outcome {
            UnitOutcome::Admitted | UnitOutcome::Resident => UnitState::Admitted,
            UnitOutcome::Failed(error) => UnitState::Failed(error),
            UnitOutcome::Retry(kind, retry_after) => {
                let unit = self.units.get_mut(unit_key).expect("unit");
                unit.attempts += 1;
                if unit.attempts >= max_attempts {
                    UnitState::Failed(kind.final_error())
                } else {
                    let attempt = unit.attempts;
                    let delay =
                        racer_core::prefetch::backoff(attempt, self.unit_random(), retry_after);
                    let unit = self.units.get_mut(unit_key).expect("unit");
                    unit.not_before = Some(now + delay);
                    unit.state = UnitState::Queued;
                    let interested: Vec<TicketKey> = unit.interest.iter().cloned().collect();
                    self.event(Counter::Retries, dispatch_priority, 1);
                    if kind == RetryKind::Deferred {
                        self.event(Counter::Deferrals, dispatch_priority, 1);
                    }
                    for key in interested {
                        if let Some(ticket) = self.tickets.get_mut(&key) {
                            ticket.cursor = 0;
                        }
                        self.schedule(&key);
                    }
                    if self
                        .units
                        .get(unit_key)
                        .is_some_and(|u| u.interest.is_empty())
                    {
                        self.units.remove(unit_key);
                        self.pending_bytes = self.pending_bytes.saturating_sub(self.page.get());
                    }
                    return;
                }
            }
        };
        let unit = self.units.get_mut(unit_key).expect("unit");
        unit.state = next;
        unit.owner = owner;
        unit.not_before = None;
        self.pending_bytes = self.pending_bytes.saturating_sub(self.page.get());
        let interested: Vec<TicketKey> = unit.interest.iter().cloned().collect();
        let resident = outcome == UnitOutcome::Resident;
        for key in &interested {
            let Some(ticket) = self.tickets.get_mut(key) else {
                continue;
            };
            ticket.open = ticket.open.saturating_sub(1);
            let priority = ticket.priority;
            if next == UnitState::Admitted {
                ticket.admitted += self.page.get();
                let counter = if resident {
                    ticket.already_resident += self.page.get();
                    Counter::AlreadyResidentBytes
                } else if Some(key) == dispatcher.as_ref() {
                    ticket.fetched += self.page.get();
                    Counter::FetchedBytes
                } else {
                    ticket.joined += self.page.get();
                    Counter::JoinedBytes
                };
                self.events.push((counter, priority, self.page.get()));
                self.events
                    .push((Counter::AdmittedBytes, priority, self.page.get()));
            }
        }
        if interested.is_empty() {
            self.units.remove(unit_key);
        }
        for key in &interested {
            self.settle(key);
        }
    }

    /// Re-plan after a membership change: admitted pages of live tickets
    /// whose owner no longer serves the page's cache are queued again. An
    /// owner can leave a cache's pool and still be a cluster member.
    pub fn requeue_departed(&mut self, serves: impl Fn(&CacheId, &NodeId) -> bool) {
        let keys: Vec<UnitKey> = self.units.keys().cloned().collect();
        self.requeue_departed_among(keys.iter(), serves);
    }

    /// Like [`Self::requeue_departed`], but only checks `keys`. The pump runs
    /// this on just-completed units so a fill that lands after its owner left
    /// the pool is not left settled against that owner.
    pub fn requeue_departed_among<'a>(
        &mut self,
        keys: impl IntoIterator<Item = &'a UnitKey>,
        serves: impl Fn(&CacheId, &NodeId) -> bool,
    ) {
        let mut requeued = Vec::new();
        for key in keys {
            let Some(unit) = self.units.get_mut(key) else {
                continue;
            };
            if unit.state == UnitState::Admitted
                && unit
                    .owner
                    .as_ref()
                    .is_some_and(|owner| !serves(&key.version.object.cache, owner))
                && !unit.interest.is_empty()
            {
                unit.state = UnitState::Queued;
                unit.attempts = 0;
                unit.not_before = None;
                unit.owner = None;
                requeued.push(unit.interest.iter().cloned().collect::<Vec<_>>());
            }
        }
        for interested in requeued {
            self.pending_bytes += self.page.get();
            for key in interested {
                if let Some(ticket) = self.tickets.get_mut(&key) {
                    ticket.open += 1;
                    ticket.cursor = 0;
                    ticket.finished_recorded = false;
                    ticket.admitted = ticket.admitted.saturating_sub(self.page.get());
                }
                self.schedule(&key);
            }
        }
    }

    /// Release a ticket's interest and freeze its ref outcomes.
    fn release(&mut self, key: &TicketKey, now: Instant) {
        let Some(ticket) = self.tickets.get(key) else {
            return;
        };
        if ticket.released {
            return;
        }
        let frozen = self.ref_outcomes(ticket);
        let units = ticket.units.clone();
        for unit in &units {
            self.drop_interest(key, unit);
        }
        let retention = Duration::from_secs(self.config.ticket_retention_secs);
        if let Some(ticket) = self.tickets.get_mut(key) {
            ticket.frozen = Some(frozen);
            ticket.released = true;
            ticket.open = 0;
            ticket.remove_at = Some(now + retention);
        }
        for rotation in &mut self.rotation {
            rotation.retain(|k| k != key);
        }
    }

    fn cancel(&mut self, key: &TicketKey, reason: Reason, now: Instant) {
        let Some(ticket) = self.tickets.get_mut(key) else {
            return;
        };
        if ticket.canceled.is_some() {
            return;
        }
        let record = !ticket.finished_recorded;
        ticket.canceled = Some(reason);
        ticket.finished_recorded = true;
        ticket.updated_at_ms = wall_ms(&self.clock);
        let priority = ticket.priority;
        if record {
            self.finished.push((priority, Outcome::Canceled));
        }
        self.release(key, now);
    }

    /// `DELETE`: cancel with reason `client`.
    pub fn delete(
        &mut self,
        cache: &CacheId,
        id: &str,
        now: Instant,
    ) -> Result<Status, PrefetchError> {
        let key = (cache.clone(), id.to_owned());
        if !self.tickets.contains_key(&key) {
            return Err(not_found(id));
        }
        self.cancel(&key, Reason::Client, now);
        self.status(cache, id, None, None)
    }

    /// Cancel every ticket of `caches` (passthrough switch or removal).
    pub fn cancel_caches(
        &mut self,
        caches: &std::collections::HashSet<CacheId>,
        reason: Reason,
        now: Instant,
    ) {
        let keys: Vec<TicketKey> = self
            .tickets
            .keys()
            .filter(|(cache, _)| caches.contains(cache))
            .cloned()
            .collect();
        for key in keys {
            self.cancel(&key, reason, now);
        }
    }

    /// Cancel tickets of caches for which `keep` is false.
    pub fn retain_caches(&mut self, keep: impl Fn(&CacheId) -> bool, now: Instant) {
        let gone: std::collections::HashSet<CacheId> = self
            .tickets
            .keys()
            .filter(|(cache, _)| !keep(cache))
            .map(|(cache, _)| cache.clone())
            .collect();
        self.cancel_caches(&gone, Reason::CacheRemoved, now);
    }

    /// TTL expiry and retention garbage collection.
    pub fn expire(&mut self, now: Instant) {
        let expired: Vec<(TicketKey, bool)> = self
            .tickets
            .iter()
            .filter(|(_, t)| !t.released && t.expires_at <= now)
            .map(|(k, t)| (k.clone(), t.open != 0))
            .collect();
        for (key, open) in expired {
            if open {
                self.cancel(&key, Reason::Expired, now);
            } else {
                // A settled window keeps its outcome; it only lets go.
                self.release(&key, now);
            }
        }
        self.tickets
            .retain(|_, t| t.remove_at.is_none_or(|at| at > now));
    }

    /// Earliest time `expire` or a retry needs attention.
    pub fn next_deadline(&self) -> Option<Instant> {
        let tickets = self.tickets.values().map(|t| {
            if t.released {
                t.remove_at.unwrap_or(t.expires_at)
            } else {
                t.expires_at
            }
        });
        let retries = self
            .units
            .values()
            .filter(|u| u.state == UnitState::Queued)
            .filter_map(|u| u.not_before);
        tickets.chain(retries).min()
    }

    /// Units of a ticket's current desired set.
    pub fn desired(&self, cache: &CacheId, id: &str) -> Result<Vec<UnitKey>, PrefetchError> {
        let key = (cache.clone(), id.to_owned());
        let ticket = self.tickets.get(&key).ok_or_else(|| not_found(id))?;
        let mut out = Vec::new();
        for entry in &ticket.refs {
            if let Some(version) = &entry.version {
                out.extend(entry.pages.clone().map(|page| UnitKey {
                    version: version.clone(),
                    page,
                }));
            }
        }
        out.sort();
        out.dedup();
        Ok(out)
    }

    /// Whether the ticket's current window is settled.
    pub fn settled(&self, cache: &CacheId, id: &str) -> Option<bool> {
        self.tickets
            .get(&(cache.clone(), id.to_owned()))
            .map(|t| t.released || t.open == 0)
    }

    fn state_of(&self, ticket: &Ticket) -> State {
        if ticket.canceled.is_some() {
            return State::Canceled;
        }
        if !ticket.released && ticket.open != 0 {
            let started = ticket.units.iter().any(|key| {
                self.units
                    .get(key)
                    .is_some_and(|u| u.state != UnitState::Queued || u.attempts != 0)
            }) || ticket.refs.iter().any(|r| r.error.is_some());
            return if started {
                State::Running
            } else {
                State::Queued
            };
        }
        let outcomes = self.ref_outcomes(ticket);
        let failed = outcomes
            .iter()
            .filter(|(o, _)| *o == RefOutcome::Failed)
            .count();
        if failed == 0 {
            State::Succeeded
        } else if failed == outcomes.len() {
            State::Failed
        } else {
            State::Partial
        }
    }

    fn ref_outcomes(&self, ticket: &Ticket) -> Vec<(RefOutcome, Option<RefError>)> {
        if let Some(frozen) = &ticket.frozen {
            return frozen.clone();
        }
        ticket
            .refs
            .iter()
            .map(|entry| {
                if let Some(error) = entry.error {
                    return (RefOutcome::Failed, Some(error));
                }
                let Some(version) = &entry.version else {
                    return (RefOutcome::Pending, None);
                };
                let mut pending = false;
                for page in entry.pages.clone() {
                    let unit_key = UnitKey {
                        version: version.clone(),
                        page,
                    };
                    if ticket.rejected.contains(&unit_key) {
                        return (RefOutcome::Failed, Some(RefError::Forbidden));
                    }
                    let unit = self.units.get(&unit_key);
                    match unit.map(|u| u.state) {
                        Some(UnitState::Failed(error)) => {
                            return (RefOutcome::Failed, Some(error));
                        }
                        Some(UnitState::Admitted) => {}
                        _ => pending = true,
                    }
                }
                if pending {
                    (RefOutcome::Pending, None)
                } else {
                    (RefOutcome::Succeeded, None)
                }
            })
            .collect()
    }

    fn failed_bytes(&self, ticket: &Ticket) -> u64 {
        self.ref_outcomes(ticket)
            .iter()
            .zip(&ticket.refs)
            .filter(|((o, _), _)| *o == RefOutcome::Failed)
            .map(|(_, r)| (r.pages.end - r.pages.start) * self.page.get())
            .sum()
    }

    fn bytes(&self, ticket: &Ticket) -> Bytes {
        let mut queued = 0;
        let mut in_flight = 0;
        if !ticket.released {
            for key in ticket
                .units
                .iter()
                .filter(|k| !ticket.rejected.contains(*k))
            {
                match self.units.get(key).map(|u| u.state) {
                    Some(UnitState::Queued) => queued += self.page.get(),
                    Some(UnitState::InFlight) => in_flight += self.page.get(),
                    _ => {}
                }
            }
        }
        Bytes {
            logical_requested: ticket.logical_requested,
            aligned_requested: ticket.aligned_requested,
            already_resident: ticket.already_resident,
            queued,
            in_flight,
            joined: ticket.joined,
            fetched_from_origin: ticket.fetched,
            admitted: ticket.admitted,
            failed: self.failed_bytes(ticket),
        }
    }

    /// Full status, optionally with a page of refs.
    pub fn status(
        &self,
        cache: &CacheId,
        id: &str,
        refs: Option<RefsFilter>,
        cursor: Option<&str>,
    ) -> Result<Status, PrefetchError> {
        let key = (cache.clone(), id.to_owned());
        let ticket = self.tickets.get(&key).ok_or_else(|| not_found(id))?;
        let outcomes = self.ref_outcomes(ticket);
        let count = |o: RefOutcome| outcomes.iter().filter(|(x, _)| *x == o).count() as u64;
        let (listed, next_cursor) = match refs {
            None => (None, None),
            Some(filter) => {
                let offset = match cursor {
                    Some(cursor) => api::decode_cursor(cursor, ticket.generation, ticket.sequence)?,
                    None => 0,
                };
                let selected: Vec<RefStatus> = ticket
                    .refs
                    .iter()
                    .zip(&outcomes)
                    .enumerate()
                    .filter(|(_, (_, (o, _)))| {
                        filter == RefsFilter::All || *o == RefOutcome::Failed
                    })
                    .skip(offset)
                    .take(REFS_PAGE + 1)
                    .map(|(index, (entry, (outcome, error)))| RefStatus {
                        index,
                        key: entry.key.to_hex(),
                        etag: entry.etag.as_ref().map(|e| e.as_str().to_owned()),
                        offset: entry.offset,
                        length: entry.length,
                        outcome: *outcome,
                        error: *error,
                    })
                    .collect();
                let more = selected.len() > REFS_PAGE;
                let mut selected = selected;
                selected.truncate(REFS_PAGE);
                let next = more.then(|| {
                    api::encode_cursor(ticket.generation, ticket.sequence, offset + REFS_PAGE)
                });
                (Some(selected), Some(next))
            }
        };
        Ok(Status {
            id: ticket.id.clone(),
            cache_uid: ticket.cache.0.clone(),
            state: self.state_of(ticket),
            reason: ticket.canceled,
            priority: ticket.priority.as_str(),
            generation: ticket.generation,
            sequence: ticket.sequence,
            settled: ticket.released || ticket.open == 0,
            ttl_seconds: ticket.ttl_seconds,
            expires_at_ms: ticket.expires_at_ms,
            created_at_ms: ticket.created_at_ms,
            updated_at_ms: ticket.updated_at_ms,
            refs_total: outcomes.len() as u64,
            refs_succeeded: count(RefOutcome::Succeeded),
            refs_failed: count(RefOutcome::Failed),
            refs_pending: count(RefOutcome::Pending),
            bytes: self.bytes(ticket),
            coverage: None,
            refs: listed,
            next_cursor,
        })
    }

    /// Summaries of a cache's tickets, sorted by id.
    pub fn list(&self, cache: &CacheId) -> Vec<Summary> {
        self.tickets
            .range((cache.clone(), String::new())..)
            .take_while(|((c, _), _)| c == cache)
            .take(1024)
            .map(|(_, ticket)| Summary {
                id: ticket.id.clone(),
                state: self.state_of(ticket),
                reason: ticket.canceled,
                priority: ticket.priority.as_str(),
                generation: ticket.generation,
                sequence: ticket.sequence,
                settled: ticket.released || ticket.open == 0,
                expires_at_ms: ticket.expires_at_ms,
                bytes: self.bytes(ticket),
            })
            .collect()
    }

    /// Live and retained tickets.
    pub fn ticket_count(&self) -> usize {
        self.tickets.len()
    }

    /// Interest count of one unit, for tests and diagnostics.
    pub fn interest(&self, unit: &UnitKey) -> Option<usize> {
        self.units.get(unit).map(|u| u.interest.len())
    }
}

fn not_found(id: &str) -> PrefetchError {
    PrefetchError::new(ErrorCode::NotFound, format!("prefetch {id} not found"))
}

/// The object a ticket ref names in `cache`.
pub fn object(cache: &CacheId, key: CacheKey) -> ObjectId {
    ObjectId {
        cache: cache.clone(),
        key,
    }
}
