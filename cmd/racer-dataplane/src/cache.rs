// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Worker-local disk-primary cache, driven by the caller's ring.
//! [`Cache::metadata`] / [`Cache::poll_metadata`] precede aligned [`Cache::page`]
//! / [`Cache::poll_fault`] calls. Only Pending returns the consumed fault.
//! Call [`Cache::poll`] for maintenance; transports implement [`Upstream`].
//! Cache owns identities, TTL, deadlines and validated plaintext publication;
//! adapters own transport health/fallback, retained across CRC validation.

use crate::{
    allocator::{self, Kind, ReadHandle},
    buffers::{BUFFER_SIZE, Buffer, Destination, Fill, Key, PublicationAuthority},
    metadata::{Checksum, ETag, Metadata as Record},
    sharding::{ShardState, WorkerContext},
    uring::{self, Ring, Work},
};
use std::{
    io,
    num::NonZeroUsize,
    rc::Rc,
    task::Waker,
    time::{Duration, Instant, UNIX_EPOCH},
};

pub const METADATA_SIZE: usize = Record::SIZE;
const META_SIZE: usize = METADATA_SIZE;
const COOLDOWN: Duration = Duration::from_secs(1);
/// Maximum complete peer descriptor size, including routing and budget framing.
pub const MAX_PEER_INPUT: usize = 3500;
mod invalidation {
    //! Version-specific metadata rejection, including late metadata completions.
    use super::*;
    use std::{cell::RefCell, collections::HashMap, rc::Weak};

    #[derive(Default)]
    pub(super) struct Freshness {
        rejected: RefCell<Vec<[u8; 32]>>,
        pub(super) generation: std::cell::Cell<u64>,
    }
    impl Freshness {
        fn reject(&self, version: [u8; 32]) {
            let mut versions = self.rejected.borrow_mut();
            if !versions.contains(&version) && versions.len() < 128 {
                versions.push(version);
            }
        }
        pub(super) fn check(&self, version: &[u8]) -> Result<()> {
            let versions = self.rejected.borrow();
            if versions.iter().any(|v| v == version) {
                Err(Error::Precondition)
            } else if versions.len() == 128 {
                // A continuously occupied object cannot grow an unbounded rejection
                // history. Drain its existing faults before accepting more metadata.
                Err(busy("metadata rejection history"))
            } else {
                Ok(())
            }
        }
    }
    #[derive(Default)]
    pub(super) struct Invalidation(HashMap<[u8; 32], Weak<Freshness>>);
    impl Invalidation {
        pub(super) fn object(&mut self, key: [u8; 32]) -> Rc<Freshness> {
            self.0.retain(|_, state| state.strong_count() != 0);
            if let Some(state) = self.0.get(&key).and_then(Weak::upgrade) {
                return state;
            }
            let state = Rc::new(Freshness::default());
            self.0.insert(key, Rc::downgrade(&state));
            state
        }
    }
    pub(super) fn precondition(error: &Error) -> bool {
        error.evidence().reason() == crate::outcome::PeerReason::Precondition
    }
    impl Cache {
        pub(super) fn poll_scrub(&mut self, ring: &mut Ring) -> Result<Work> {
            let mut changed = false;
            if let Some(scrub) = &mut self.scrub {
                if let Some(result) = scrub.read.take() {
                    let bad = match result {
                        Ok(buffer) => {
                            allocator::crc64(buffer.as_slice()) != scrub.lease.info().crc64
                        }
                        Err(_) => true,
                    };
                    if bad
                        && self.shards[scrub.shard]
                            .allocator
                            .remove_if_same(&scrub.key, &scrub.lease)
                    {
                        changed = true;
                    }
                    self.scrub = None;
                    self.scrub_at = crate::environment::now() + COOLDOWN;
                }
            } else if crate::environment::now() >= self.scrub_at {
                let shard = self.scrub_cursor % self.shards.len();
                let index = self.scrub_cursor / self.shards.len();
                self.scrub_cursor = self.scrub_cursor.wrapping_add(1);
                self.scrub_at = crate::environment::now() + COOLDOWN;
                if let Some((key, lease)) = self.shards[shard].allocator.scrub_candidate(index) {
                    if let Ok(fill) = ring.pool().private_fill() {
                        match self.shards[shard].allocator.scrub_read(ring, &lease, fill) {
                            Ok(read) => {
                                self.scrub = Some(Scrub {
                                    key,
                                    shard,
                                    lease,
                                    read,
                                })
                            }
                            Err(rejected) if rejected.error.kind() == io::ErrorKind::WouldBlock => {
                            }
                            Err(rejected) => return Err(rejected.error.into()),
                        }
                    }
                }
            }
            Ok(Work {
                runnable: changed,
                deadline: self.scrub.is_none().then_some(self.scrub_at),
            })
        }
        pub(super) fn reject_page<U: Upstream>(
            &mut self,
            fault: Fault<U>,
            error: Error,
            _ring: &mut Ring,
        ) -> Result<Progress<Fault<U>, CachedValue>> {
            let Spec::Page(page) = &fault.spec else {
                unreachable!()
            };
            let key = page.object.metadata_key().0;
            fault.freshness.reject(*page.version());
            let shard = local_replica(&key, self.shards.len()).0;
            if self.shards[shard]
                .allocator
                .lookup_metadata(&key, now())
                .is_some_and(|m| m.checksum.0 == *page.version())
            {
                self.shards[shard].allocator.remove(&key);
                self.sweep_work.runnable = true;
            }
            Err(error)
        }
    }
}

#[derive(Clone)]
pub enum CachedValue {
    Metadata(Record),
    File(allocator::FileValue),
    Buffer(Buffer),
}
impl CachedValue {
    pub fn len(&self) -> usize {
        match self {
            Self::Metadata(_) => Record::SIZE,
            Self::File(file) => file.info().len,
            Self::Buffer(buffer) => buffer.as_slice().len(),
        }
    }
    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }
    pub fn checksum(&self) -> Option<u64> {
        match self {
            Self::Metadata(record) => Some(allocator::crc64(&record.to_bytes())),
            Self::File(file) => Some(file.info().crc64),
            Self::Buffer(buffer) => buffer.checksum(),
        }
    }
}
#[cfg(test)]
use std::sync::atomic::Ordering;

/// Transport-independent failures. Adapters map these to their own protocol.
#[derive(Debug)]
pub enum Error {
    NotFound,
    Gone,
    Precondition,
    Timeout,
    Unavailable,
    InvalidData(&'static str),
    Io(io::Error),
    /// Typed attribution, including foreign I/O classified once on entry.
    Outcome(Box<crate::outcome::Classified>),
    /// Permanent local storage admission failure, not an upstream failure.
    Admission(io::Error),
    /// Shared candidate outcome retaining the complete typed failure chain.
    Shared(std::sync::Arc<Error>),
}
impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::NotFound => f.write_str("not found"),
            Self::Gone => f.write_str("gone"),
            Self::Precondition => f.write_str("precondition failed"),
            Self::Timeout => f.write_str("cache deadline elapsed"),
            Self::Unavailable => f.write_str("upstream unavailable"),
            Self::InvalidData(message) => f.write_str(message),
            Self::Io(error) | Self::Admission(error) => error.fmt(f),
            Self::Shared(error) => error.fmt(f),
            Self::Outcome(error) => error.fmt(f),
        }
    }
}
impl std::error::Error for Error {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::Io(error) | Self::Admission(error) => Some(error),
            Self::Shared(error) => Some(error.as_ref()),
            Self::Outcome(error) => Some(error.as_ref()),
            _ => None,
        }
    }
}
impl From<io::Error> for Error {
    fn from(error: io::Error) -> Self {
        // Nested public I/O payloads must retain their causal facts, even when
        // the outer kind is a timeout or not-found. Inspect them only on entry.
        if error.get_ref().is_some_and(|e| {
            e.is::<crate::outcome::Failure>() || e.is::<Error>() || e.is::<io::Error>()
        }) {
            return Self::Outcome(Box::new(crate::outcome::Classified::boundary(error)));
        }
        match error.kind() {
            io::ErrorKind::NotFound => Self::NotFound,
            io::ErrorKind::TimedOut => Self::Timeout,
            _ => Self::Outcome(Box::new(crate::outcome::Classified::boundary(error))),
        }
    }
}
pub type Result<T> = std::result::Result<T, Error>;
impl From<Error> for io::Error {
    fn from(error: Error) -> Self {
        error.into_io()
    }
}
impl Error {
    pub(crate) fn healthy_http_status(&self) -> bool {
        match self {
            Self::NotFound | Self::Gone | Self::Precondition => true,
            Self::Outcome(error) => error.healthy_status(),
            Self::Io(error) => error
                .get_ref()
                .and_then(|e| e.downcast_ref::<http_metadata::HttpStatus>())
                .is_some_and(|s| s.0 < 500),
            _ => false,
        }
    }
    pub(crate) fn io_kind(&self) -> io::ErrorKind {
        match self.root() {
            Self::Timeout => io::ErrorKind::TimedOut,
            Self::NotFound => io::ErrorKind::NotFound,
            Self::InvalidData(_) => io::ErrorKind::InvalidData,
            Self::Io(error) => error.kind(),
            Self::Outcome(error) => error.io_kind(),
            _ => io::ErrorKind::Other,
        }
    }
    pub(crate) fn into_io(self) -> io::Error {
        match self {
            Self::Io(error) => error,
            Self::Outcome(error) => error.into_io(),
            error => io::Error::new(error.io_kind(), error),
        }
    }
    pub(crate) fn evidence(&self) -> crate::outcome::Evidence<'_> {
        use crate::outcome::{Evidence, PeerReason};
        let reason = match self {
            Self::Shared(error) => return error.evidence(),
            Self::Outcome(error) => return error.evidence(),
            // Explicit Io is a public compatibility entry point. Internal
            // producers use From<io::Error> or typed constructors instead.
            Self::Io(error) => return crate::outcome::legacy::collect(error),
            Self::NotFound => PeerReason::NotFound,
            Self::Gone => PeerReason::Gone,
            Self::Precondition => PeerReason::Precondition,
            Self::Timeout => PeerReason::Deadline,
            Self::Unavailable => PeerReason::Unavailable,
            Self::InvalidData(_) => PeerReason::Protocol,
            Self::Admission(_) => PeerReason::Busy,
        };
        Evidence {
            fallback: Some(reason),
            admission: matches!(self, Self::Admission(_)),
            caller_timeout: matches!(self, Self::Timeout),
            ..Evidence::default()
        }
    }
    pub(crate) fn attempt_failure(&self) -> Option<&crate::outcome::AttemptFailure> {
        match self {
            Self::Shared(error) => error.attempt_failure(),
            Self::Outcome(error) => error.routed(),
            Self::Io(_) => crate::outcome::legacy::error_detail(self),
            _ => None,
        }
    }
    pub fn root(&self) -> &Self {
        match self {
            Self::Shared(error) => error.root(),
            _ => self,
        }
    }
}
fn invalid(message: &'static str) -> Error {
    Error::InvalidData(message)
}
fn now() -> u64 {
    crate::environment::wall()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs()
}
fn runnable() -> Work {
    Work {
        runnable: true,
        deadline: None,
    }
}
fn retry() -> Work {
    Work {
        runnable: false,
        deadline: Some(crate::environment::now() + Duration::from_millis(1)),
    }
}
fn digest(domain: &[u8], parts: &[&[u8]]) -> [u8; 32] {
    let mut hash = blake3::Hasher::new();
    hash.update(domain);
    for part in parts {
        hash.update(&(part.len() as u64).to_le_bytes());
        hash.update(part);
    }
    *hash.finalize().as_bytes()
}

/// Explicit logical origin identity, independent of its transport address.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Namespace([u8; 32]);
impl Namespace {
    /// Scope a logical origin identity to a configured volume.
    /// Routing epochs and resolved socket addresses are not cache identities.
    pub fn volume(universe: &[u8], volume: &str, generation: u64, backend: Self) -> Self {
        Self(digest(
            b"racer-volume-v5",
            &[
                universe,
                volume.as_bytes(),
                &generation.to_le_bytes(),
                backend.digest(),
            ],
        ))
    }
    pub fn new(identity: &str) -> Result<Self> {
        if identity.is_empty()
            || identity.len() > 1024
            || !identity.bytes().all(|b| (33..=126).contains(&b))
        {
            return Err(invalid("invalid origin identity"));
        }
        Ok(Self(digest(b"racer-origin-v1", &[identity.as_bytes()])))
    }
    pub fn digest(&self) -> &[u8; 32] {
        &self.0
    }
}

/// Parsed policy facts. Duplicate/malformed wire directives are adapter errors.
#[derive(Clone, Copy, Debug, Default)]
pub struct CachePolicy {
    pub max_age: Option<u64>,
    pub shared_max_age: Option<u64>,
    pub disabled: bool,
    pub age: u64,
}
impl CachePolicy {
    pub fn effective_ttl(self) -> u64 {
        if self.disabled {
            0
        } else {
            self.shared_max_age
                .or(self.max_age)
                .unwrap_or(0)
                .saturating_sub(self.age)
        }
    }
}
#[derive(Clone, Debug)]
pub struct BackendMetadata {
    pub len: u64,
    pub checksum: Checksum,
    pub policy: CachePolicy,
}

/// Parsed inclusive byte interval, with checked bounds.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct ContentRange {
    start: u64,
    end: u64,
    total: u64,
}
impl ContentRange {
    pub fn new(start: u64, end: u64, total: u64) -> Result<Self> {
        if start > end || end >= total {
            return Err(invalid("invalid content range"));
        }
        Ok(Self { start, end, total })
    }
    pub fn start(&self) -> u64 {
        self.start
    }
    pub fn end(&self) -> u64 {
        self.end
    }
    pub fn total(&self) -> u64 {
        self.total
    }
}
/// Absence of a range means a full-object result, accepted only for a full page.
/// Adapters must reject non-identity content encodings and malformed framing.
#[derive(Clone, Debug)]
pub struct BackendPage {
    pub checksum: Checksum,
    pub range: Option<ContentRange>,
}

mod metadata;
pub(crate) use metadata::{http_metadata, peer_wire};
mod context;
pub use context::Context;

#[derive(Clone, Copy)]
struct ObjectKey([u8; 32]);
#[derive(Clone, Copy)]
struct MetadataKey([u8; 32]);
#[derive(Clone, Copy)]
struct PageKey([u8; 32]);

#[derive(Clone)]
struct Object {
    key: ObjectKey,
    target: Rc<str>,
}
impl Object {
    fn new(namespace: &[u8; 32], target: &str) -> Result<Self> {
        if !target.starts_with('/') || !target.bytes().all(|b| (33..=126).contains(&b) && b != b'#')
        {
            return Err(invalid("invalid object target"));
        }
        let path = blake3::hash(target.as_bytes());
        Ok(Self {
            key: ObjectKey(digest(b"object", &[namespace, path.as_bytes()])),
            target: target.into(),
        })
    }
    fn metadata_key(&self) -> MetadataKey {
        MetadataKey(digest(b"metadata", &[&self.key.0]))
    }
}

impl Record {
    pub(crate) fn from_backend(facts: BackendMetadata) -> Self {
        let BackendMetadata {
            len,
            checksum,
            policy,
        } = facts;
        let ttl = policy.effective_ttl();
        Self {
            len,
            // Zero denotes a request-scoped resolution, never a reusable cache
            // entry. It survives peer transit without extending freshness.
            expires: if ttl == 0 {
                0
            } else {
                now().saturating_add(ttl)
            },
            checksum,
        }
    }
    #[cfg(test)]
    fn encode(&self, out: &mut [u8]) {
        out.copy_from_slice(&self.to_bytes());
    }
    #[cfg(test)]
    fn decode(bytes: &[u8]) -> Result<Self> {
        Ok(Self::from_bytes(bytes)?)
    }
    fn page(self: &Rc<Self>, object: &Object, offset: u64) -> Result<PageRequest> {
        if !offset.is_multiple_of(BUFFER_SIZE as u64) || offset >= self.len {
            return Err(invalid("invalid page offset"));
        }
        let len = NonZeroUsize::new((self.len - offset).min(BUFFER_SIZE as u64) as usize).unwrap();
        Ok(PageRequest {
            key: PageKey(digest(
                b"page",
                &[
                    &object.key.0,
                    &self.checksum.0,
                    &self.len.to_le_bytes(),
                    &offset.to_le_bytes(),
                ],
            )),
            object: object.clone(),
            metadata: self.clone(),
            offset,
            len,
        })
    }
}
/// Cache-validated page identity. Only Cache can construct request values.
/// ```compile_fail
/// use racer_dataplane::cache::PageRequest;
/// let request = PageRequest { offset: 1 };
/// ```
#[derive(Clone)]
pub struct PageRequest {
    key: PageKey,
    object: Object,
    metadata: Rc<Record>,
    offset: u64,
    len: NonZeroUsize,
}
impl PageRequest {
    pub fn target(&self) -> &str {
        &self.object.target
    }
    pub fn key(&self) -> &[u8; 32] {
        &self.key.0
    }
    pub fn len(&self) -> usize {
        self.len.get()
    }
    pub fn is_empty(&self) -> bool {
        false
    }
    pub fn offset(&self) -> u64 {
        self.offset
    }
    pub fn object_len(&self) -> u64 {
        self.metadata.len
    }
    pub fn version(&self) -> &[u8; 32] {
        &self.metadata.checksum.0
    }
    pub fn checksum(&self) -> Checksum {
        self.metadata.checksum
    }
    pub fn range(&self) -> ContentRange {
        ContentRange {
            start: self.offset,
            end: self.offset + self.len() as u64 - 1,
            total: self.object_len(),
        }
    }
    pub fn is_full_object(&self) -> bool {
        self.offset == 0 && self.len() as u64 == self.object_len()
    }
    fn validate_backend(&self, facts: &BackendPage) -> Result<()> {
        if facts
            .range
            .map_or(!self.is_full_object(), |range| range != self.range())
        {
            return Err(invalid("wrong backend range"));
        }
        if facts.checksum != self.checksum() {
            return Err(Error::Precondition);
        }
        Ok(())
    }
}
/// Cache-created metadata identity with fixed stored transfer length.
/// ```compile_fail
/// use racer_dataplane::cache::MetadataRequest;
/// let forged = MetadataRequest { object: todo!() };
/// ```
#[derive(Clone)]
pub struct MetadataRequest {
    object: Object,
}
// Benchmark fidelity: construct real cache identities once, outside timing. The
// fixture then uses the same peer descriptor codec as production handlers.
#[cfg(feature = "dev-bench")]
pub(crate) fn benchmark_request(metadata: bool, record: Record) -> UpstreamRequest {
    let object = Object::new(&[0; 32], "/transport-benchmark/object").unwrap();
    if metadata {
        UpstreamRequest::PeerMetadata(MetadataRequest { object })
    } else {
        UpstreamRequest::PeerPage(Rc::new(record).page(&object, 0).unwrap())
    }
}
impl MetadataRequest {
    pub fn target(&self) -> &str {
        &self.object.target
    }
    pub fn key(&self) -> [u8; 32] {
        self.object.metadata_key().0
    }
    pub fn len(&self) -> usize {
        META_SIZE
    }
    pub fn is_empty(&self) -> bool {
        false
    }
}
#[derive(Clone)]
enum Spec {
    Metadata(Object),
    Page(PageRequest),
}
impl Spec {
    fn key(&self) -> [u8; 32] {
        match self {
            Self::Metadata(o) => o.metadata_key().0,
            Self::Page(p) => p.key.0,
        }
    }
    fn len(&self) -> usize {
        match self {
            Self::Metadata(_) => META_SIZE,
            Self::Page(p) => p.len.get(),
        }
    }
    fn object(&self) -> &Object {
        match self {
            Self::Metadata(o) => o,
            Self::Page(p) => &p.object,
        }
    }
}

/// Parsed, untrusted peer descriptor facts. Cache validates target, bounds,
/// version, and optional transport-advertised key/length before creating a fault.
pub struct PeerDescriptor<'a> {
    target: &'a str,
    page: Option<PeerPage>,
    expected: Option<([u8; 32], usize)>,
}
pub struct PeerPage {
    offset: u64,
    object_len: u64,
    checksum: Checksum,
}
impl PeerPage {
    pub fn new(offset: u64, object_len: u64, checksum: Checksum) -> Self {
        Self {
            offset,
            object_len,
            checksum,
        }
    }
}
impl<'a> PeerDescriptor<'a> {
    pub fn target(&self) -> &'a str {
        self.target
    }
    pub fn metadata(target: &'a str) -> Self {
        Self {
            target,
            page: None,
            expected: None,
        }
    }
    pub fn page(target: &'a str, page: PeerPage) -> Self {
        Self {
            target,
            page: Some(page),
            expected: None,
        }
    }
    pub fn with_expected(mut self, key: [u8; 32], len: usize) -> Self {
        self.expected = Some((key, len));
        self
    }
}

/// Owned small request descriptors; cloning shares object/record allocations.
#[derive(Clone)]
pub enum UpstreamRequest {
    BackendMetadata(MetadataRequest),
    BackendPage(PageRequest),
    PeerMetadata(MetadataRequest),
    PeerPage(PageRequest),
}
/// Unpublished receive completion. Length is checked before any slicing.
/// Racer computes CRC64/ECMA-182 before publication and retains it for later scrub.
/// Peer checksums are verified; backend checksums are ignored. Foreground cache
/// hits do not checksum payloads.
pub struct Received {
    pub destination: Destination,
    pub len: usize,
    /// Expected checksum for peer transfers only.
    pub checksum: Option<u64>,
}
pub enum UpstreamResult {
    Metadata(Record),
    BackendPage {
        received: Received,
        facts: BackendPage,
    },
    PeerPage(Received),
}
#[must_use]
pub enum ExchangeProgress<E> {
    Pending {
        exchange: E,
        work: Work,
    },
    /// The adapter has canceled its current transport. Cache drops its old
    /// authority and reacquires storage before calling Upstream::resume_peer.
    RetryPeer {
        exchange: E,
    },
    /// Completed peer bytes with a transport fallback if semantic validation
    /// fails. The continuation must not retain the completed destination. Cache
    /// drops both old capabilities and resumes with fresh storage on invalid
    /// input; valid but expired metadata goes directly to backend instead.
    ReadyPeer {
        result: UpstreamResult,
        retry: E,
    },
    Ready(UpstreamResult),
}
/// Payload exchanges own Destination, never publication authority. Metadata
/// exchanges own only small transport storage. Drop cancels; driver-held storage
/// stays pinned until I/O quiesces. Fallbacks share the candidate deadline.
pub trait Upstream {
    type Exchange;
    /// Metadata exchanges never reserve payload storage.
    fn start_metadata(
        &mut self,
        _request: UpstreamRequest,
        _deadline: Instant,
        _ring: &mut Ring,
    ) -> Result<Self::Exchange> {
        Err(invalid("metadata transport unsupported"))
    }
    fn resume_metadata(
        &mut self,
        _exchange: Self::Exchange,
        _deadline: Instant,
        _ring: &mut Ring,
    ) -> Result<Self::Exchange> {
        Err(invalid("metadata retry unsupported"))
    }
    /// Per-consumer candidate budget, including queueing, validation and all
    /// same-peer transport fallbacks. Called once per candidate, never on takeover.
    fn candidate_deadline(&mut self, caller: Instant) -> Instant {
        caller
    }
    /// Evidence already established by an initiated attempt survives candidate
    /// expiry while the consumer still has caller time to coordinate a successor.
    fn proven_failure(&self, _error: &Error) -> bool {
        false
    }
    /// Pin the network dependency for this candidate, independent of completed
    /// value identity. Routed adapters must supply this for local owners too.
    fn network_scope(&self, _value: [u8; 32]) -> Option<crate::buffers::NetworkFlightKey> {
        None
    }
    /// Report semantic validation of a ReadyPeer completion before its retry
    /// continuation is dropped or resumed. Health authority stays in the adapter.
    fn peer_validated(&mut self, _retry: &mut Self::Exchange, _valid: bool) {}
    fn has_peer(&self) -> bool {
        false
    }
    /// Free payload slots required for downstream progress before peer receives.
    /// Routed adapters use remaining canonical distance, including on fallback.
    /// Local-only adapters need no reserve.
    fn receive_reserve(&self) -> Result<usize> {
        Ok(0)
    }
    /// Return true to reselect a destination. Topology adapters must never
    /// authorize backend access merely because a relay failed.
    fn peer_failed(&mut self, error: Error) -> Result<bool> {
        let _ = error;
        Ok(false)
    }
    fn start(
        &mut self,
        request: UpstreamRequest,
        destination: Destination,
        deadline: Instant,
        ring: &mut Ring,
    ) -> Result<Self::Exchange>;
    fn poll(
        &mut self,
        exchange: Self::Exchange,
        ring: &mut Ring,
    ) -> Result<ExchangeProgress<Self::Exchange>>;
    /// Resume a peer transport fallback with fresh storage and the unchanged
    /// candidate deadline. Transport fallback never restarts that budget.
    /// The exchange must not retain the canceled destination itself; drivers may
    /// retain it until I/O quiesces. A shared fill can satisfy the fault instead,
    /// in which case the cache drops this exchange without resuming it.
    fn resume_peer(
        &mut self,
        _exchange: Self::Exchange,
        _destination: Destination,
        _deadline: Instant,
        _ring: &mut Ring,
    ) -> Result<Self::Exchange> {
        Err(Error::Unavailable)
    }
}
#[must_use]
pub enum Progress<F, T> {
    Pending { fault: F, work: Work },
    Ready(T),
}

#[derive(Clone, Copy)]
enum PageCrc {
    Compute,
    Supplied(u64),
}
impl PageCrc {
    fn option(self) -> Option<u64> {
        match self {
            Self::Compute => None,
            Self::Supplied(n) => Some(n),
        }
    }
}
#[allow(clippy::large_enum_variant)]
enum Loading<E> {
    Metadata(Record),
    MetadataExchange(E),
    MetadataRetry(E),
    Admitting(Buffer),
    File(allocator::FileValue),
    Publishing(allocator::ReadLease),
    Materializing(uring::Ticket<uring::Read>, allocator::FileValue),
    Materialized,
    Shared(Buffer),
    ChecksumPending(ValidatedReceive),
    Checksum(crate::crypto::ChecksumTicket),
    Acquire,
    RetryPeer {
        exchange: E,
    },
    Upstream {
        exchange: E,
        authority: PublicationAuthority,
    },
    Done,
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum Route {
    Select,
    Peer,
    Backend,
}
/// One affine page or peer lookup. Drop to cancel; I/O drivers retain buffer
/// ownership until completion. Poll only on the cache that created it.
/// ```compile_fail
/// use racer_dataplane::cache::{Fault, Upstream};
/// fn duplicate<U: Upstream>(fault: Fault<U>) { let _copy = fault.clone(); }
/// ```
/// ```compile_fail
/// use racer_dataplane::cache::{Fault, Upstream};
/// fn move_worker<U: Upstream + 'static>(fault: Fault<U>) {
///     std::thread::spawn(move || drop(fault));
/// }
/// ```
/// ```compile_fail
/// use racer_dataplane::{cache::{Cache, Fault, Upstream}, uring::Ring};
/// fn reuse<U: Upstream>(cache: &mut Cache, fault: Fault<U>, ring: &mut Ring, upstream: &mut U) {
///     let _ = cache.poll_fault(fault, ring, upstream);
///     let _ = cache.poll_fault(fault, ring, upstream);
/// }
/// ```
#[must_use]
pub struct Fault<U: Upstream> {
    generation: u64,
    freshness: Rc<invalidation::Freshness>,
    buffered: bool,
    _admission: FaultAdmission,
    resource_retries: usize,
    resource_polls: usize,
    resource_retry_at: Instant,

    network: Option<crate::buffers::NetworkFlight>,
    scope: Option<crate::buffers::NetworkFlightKey>,
    network_done: bool,
    classified: bool,

    // Retains adapter health authority and alternate transport until async CRC
    // and semantic validation finish. Never retains a receive destination.
    validation: Option<U::Exchange>,
    crypto: Option<Rc<std::cell::RefCell<crate::crypto::Worker>>>,
    context: Context,
    spec: Spec,
    key: [u8; 32],
    shard: LocalIndex,
    owner: Rc<()>,
    state: Loading<U::Exchange>,
    route: Route,
    deadline: Instant,
    candidate_deadline: Option<Instant>,
}
impl<U: Upstream> Fault<U> {
    pub(crate) fn representation_checksum(&self) -> Option<Checksum> {
        match &self.spec {
            Spec::Metadata(_) => None,
            Spec::Page(page) => Some(page.checksum()),
        }
    }
    fn classify(&mut self, metrics: &crate::metrics::Local, outcome: crate::metrics::Outcome) {
        if !self.classified {
            metrics.lookup(
                match self.spec {
                    Spec::Metadata(_) => crate::metrics::Kind::Metadata,
                    Spec::Page(_) => crate::metrics::Kind::Page,
                },
                outcome,
            );
            self.classified = true;
        }
    }
    /// Exact transfer length (the last payload page is clipped at EOF).
    pub fn len(&self) -> usize {
        self.spec.len()
    }
    pub fn is_empty(&self) -> bool {
        false
    }
    pub fn key(&self) -> &[u8; 32] {
        &self.key
    }
    pub fn target(&self) -> &str {
        &self.spec.object().target
    }
    pub fn deadline(&self) -> Instant {
        self.candidate_deadline
            .unwrap_or(self.deadline)
            .min(self.deadline)
    }
    /// Wait for buffer acquisition before prefetching another page.
    pub fn can_prefetch(&self) -> bool {
        !matches!(
            self.state,
            Loading::Acquire | Loading::RetryPeer { .. } | Loading::Done
        )
    }
}

/// Metadata lookup with a typed result. Drop to cancel.
/// ```compile_fail
/// use racer_dataplane::{cache::{Cache, MetadataFault, Upstream}, uring::Ring};
/// fn reuse<U: Upstream>(cache: &mut Cache, fault: MetadataFault<U>, ring: &mut Ring, upstream: &mut U) {
///     let _ = cache.poll_metadata(fault, ring, upstream);
///     let _ = cache.poll_metadata(fault, ring, upstream);
/// }
/// ```
#[must_use]
pub struct MetadataFault<U: Upstream>(Fault<U>);
impl<U: Upstream> MetadataFault<U> {
    pub fn target(&self) -> &str {
        self.0.target()
    }
    pub fn key(&self) -> &[u8; 32] {
        self.0.key()
    }
    pub fn deadline(&self) -> Instant {
        self.0.deadline()
    }
}

/// Resolved object identity and version. Cloning shares the small metadata
/// record; it never retains a 4 MiB pool buffer. Pages may outlive metadata TTL.
#[derive(Clone)]
pub struct Metadata {
    object: Object,
    record: Rc<Record>,
    owner: Rc<()>,
    context: Context,
}
impl Metadata {
    pub fn len(&self) -> u64 {
        self.record.len
    }
    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }
    pub fn etag(&self) -> ETag {
        self.record.checksum.etag()
    }
    pub fn checksum(&self) -> Checksum {
        self.record.checksum
    }
    pub fn target(&self) -> &str {
        &self.object.target
    }
    pub fn version(&self) -> &[u8; 32] {
        &self.record.checksum.0
    }
    pub fn expires(&self) -> u64 {
        self.record.expires
    }
}

/// Worker-local cache with uniquely owned shards and receiving-worker replicas.
///
/// Worker-affine transports and allocations cannot cross a thread boundary:
/// ```compile_fail
/// use racer_dataplane::cache::Cache;
/// fn move_worker(cache: Cache) { std::thread::spawn(move || drop(cache)); }
/// ```
pub struct Cache {
    maintenance: bool,
    sealed: bool,
    invalidation: invalidation::Invalidation,
    metrics: crate::metrics::Local,
    limits: Limits,
    active_faults: Rc<std::cell::Cell<usize>>,
    owner: Rc<()>,
    ring: Option<Rc<crate::uring::Identity>>,
    namespace: [u8; 32],
    shards: Vec<ShardState>,
    cursor: usize,
    sweep_work: Work,
    completion_epoch: u64,
    scrub: Option<Scrub>,
    scrub_cursor: usize,
    scrub_at: Instant,
}
/// Worker-wide across all volume generations sharing this cache. Saturation is
/// terminal Busy; internal peer work can use the reserved portion.
#[derive(Clone, Copy, Debug)]
pub struct Limits {
    pub active_faults: usize,
    pub internal_reserve: usize,
    pub resource_retries: usize,
}
impl Default for Limits {
    fn default() -> Self {
        Self {
            active_faults: 128,
            internal_reserve: 32,
            resource_retries: 32,
        }
    }
}
struct FaultAdmission(Rc<std::cell::Cell<usize>>);
impl Drop for FaultAdmission {
    fn drop(&mut self) {
        self.0.set(self.0.get() - 1);
    }
}
pub(crate) fn busy(resource: &'static str) -> Error {
    Error::Admission(io::Error::new(io::ErrorKind::WouldBlock, resource))
}
/// Index into this receiving worker's ordered replicas, never a logical ShardId.
#[derive(Clone, Copy)]
struct LocalIndex(usize);
fn local_replica(key: &[u8; 32], count: usize) -> LocalIndex {
    LocalIndex((u64::from_le_bytes(key[..8].try_into().unwrap()) % count as u64) as usize)
}
impl Cache {
    pub fn set_metrics(&mut self, metrics: crate::metrics::Local) {
        self.metrics = metrics;
    }
    pub fn metrics(&self) -> &crate::metrics::Local {
        &self.metrics
    }
    pub fn set_limits(&mut self, limits: Limits) -> Result<()> {
        if limits.active_faults == 0 || limits.internal_reserve >= limits.active_faults {
            return Err(invalid("invalid cache resource limits"));
        }
        self.limits = limits;
        Ok(())
    }
    pub fn new(
        context: &WorkerContext,
        namespace: Namespace,
        shards: Vec<ShardState>,
    ) -> Result<Self> {
        if !ShardState::validate_collection(context, &shards) {
            return Err(invalid(
                "cache requires the worker's complete ordered shard assignment",
            ));
        }
        if shards
            .iter()
            .any(|s| !std::sync::Arc::ptr_eq(&s.slab, &shards[0].slab))
        {
            return Err(invalid("cache shards belong to different slabs"));
        }
        Ok(Self {
            maintenance: false,
            sealed: false,
            invalidation: Default::default(),
            limits: Limits::default(),
            metrics: crate::metrics::Local::default(),
            active_faults: Rc::new(std::cell::Cell::new(0)),
            owner: Rc::new(()),
            ring: None,
            namespace: namespace.0,
            sweep_work: Work::default(),
            completion_epoch: 0,
            shards,
            cursor: 0,
            scrub: None,
            scrub_cursor: 0,
            scrub_at: crate::environment::now() + COOLDOWN,
        })
    }
    /// Build a replacement cache while keeping the execution context, ring and
    /// pool alive. Requires exactly this generation's complete ordered local set.
    /// The runtime must drain/retire the old cache and route faults to their
    /// original cache; faults and resolved metadata cannot cross cache identities.
    pub fn for_generation(
        context: &WorkerContext,
        generation: &crate::sharding::StorageGeneration,
        namespace: Namespace,
        shards: Vec<ShardState>,
    ) -> Result<Self> {
        if shards.iter().any(|s| !generation.matches(s)) {
            return Err(invalid("cache shards belong to another storage generation"));
        }
        Self::new(context, namespace, shards)
    }
    pub(crate) fn use_guard(&self) -> Rc<()> {
        self.owner.clone()
    }
    pub(crate) fn maintenance(&mut self, enabled: bool) {
        self.maintenance = enabled;
        if !enabled {
            self.sealed = false;
        }
        if enabled {
            self.scrub = None;
        }
    }
    pub(crate) fn maintenance_idle(&self) -> bool {
        self.active_faults.get() == 0
            && Rc::strong_count(&self.owner) == 1
            && self.shards.iter().all(|s| s.allocator.maintenance_idle())
    }
    pub(crate) fn seal_maintenance(&mut self) -> bool {
        if self.maintenance_idle() {
            // All application users have gone. Prevent background eviction from
            // restarting a checkpoint after this worker acknowledged its fence.
            self.sealed = true;
        }
        self.sealed
    }
    pub(crate) fn inherit_settings(&mut self, old: &Self) {
        self.limits = old.limits;
        self.metrics = old.metrics.clone();
        self.maintenance = old.maintenance;
    }
    /// Retire a drained generation one shard per reactor turn. Its final inode
    /// owner remains on the process setup thread until all kernel leases end.
    pub(crate) fn retire_one(&mut self) -> bool {
        debug_assert!(self.maintenance_idle());
        self.shards.pop();
        self.shards.is_empty()
    }
    /// Start a metadata lookup using the exact original path and query.
    pub fn metadata<U: Upstream>(
        &mut self,
        target: &str,
        deadline: Instant,
    ) -> Result<MetadataFault<U>> {
        self.metadata_in(&Context::new(Namespace(self.namespace)), target, deadline)
    }
    /// Admit a lookup with an immutable volume and checksum-worker context.
    pub fn metadata_in<U: Upstream>(
        &mut self,
        context: &Context,
        target: &str,
        deadline: Instant,
    ) -> Result<MetadataFault<U>> {
        let object = Object::new(context.namespace().digest(), target)?;
        Ok(MetadataFault(self.fault(
            Spec::Metadata(object),
            deadline,
            false,
            context,
        )?))
    }
    pub fn poll_metadata<U: Upstream>(
        &mut self,
        fault: MetadataFault<U>,
        ring: &mut Ring,
        upstream: &mut U,
    ) -> Result<Progress<MetadataFault<U>, Metadata>> {
        let object = fault.0.spec.object().clone();
        let context = fault.0.context.clone();
        match self.poll_value(fault.0, ring, upstream)? {
            Progress::Pending { fault, work } => Ok(Progress::Pending {
                fault: MetadataFault(fault),
                work,
            }),
            Progress::Ready(CachedValue::Metadata(record)) => Ok(Progress::Ready(Metadata {
                object,
                record: Rc::new(record),
                owner: self.owner.clone(),
                context,
            })),
            Progress::Ready(_) => unreachable!(),
        }
    }
    /// Start an aligned page lookup pinned to this resolved object's version.
    pub fn page<U: Upstream>(
        &mut self,
        metadata: &Metadata,
        offset: u64,
        deadline: Instant,
    ) -> Result<Fault<U>> {
        if !Rc::ptr_eq(&self.owner, &metadata.owner) {
            return Err(invalid("foreign cache metadata"));
        }
        self.fault(
            Spec::Page(metadata.record.page(&metadata.object, offset)?),
            deadline,
            false,
            &metadata.context,
        )
    }
    /// Validate parsed peer facts; codecs and peer authentication live in adapters.
    pub fn peer_fault<U: Upstream>(
        &mut self,
        descriptor: PeerDescriptor<'_>,
        deadline: Instant,
    ) -> Result<Fault<U>> {
        self.peer_fault_in(
            &Context::new(Namespace(self.namespace)),
            descriptor,
            deadline,
        )
    }
    /// Validate peer facts in the receiving volume's immutable admission context.
    pub fn peer_fault_in<U: Upstream>(
        &mut self,
        context: &Context,
        descriptor: PeerDescriptor<'_>,
        deadline: Instant,
    ) -> Result<Fault<U>> {
        if peer_wire::encoded_len(
            descriptor.target.len(),
            descriptor.page.is_some(),
            false,
            false,
        ) > MAX_PEER_INPUT
        {
            return Err(invalid("peer input too large"));
        }
        let object = Object::new(context.namespace().digest(), descriptor.target)?;
        let spec = if let Some(page) = descriptor.page {
            let record = Rc::new(Record {
                len: page.object_len,
                expires: 0,
                checksum: page.checksum,
            });
            Spec::Page(record.page(&object, page.offset)?)
        } else {
            Spec::Metadata(object)
        };
        if descriptor
            .expected
            .is_some_and(|(key, len)| key != spec.key() || len != spec.len())
        {
            return Err(invalid("peer key/length mismatch"));
        }
        self.fault(spec, deadline, true, context)
    }
    /// Finish accepted cache admissions before closing the slab. The caller must
    /// drop its outstanding faults and then quiesce transport drivers and ring.
    pub fn shutdown(&mut self, ring: &mut Ring) -> Result<()> {
        let deadline = crate::environment::now() + Duration::from_secs(5);
        loop {
            ring.progress()?;
            let (done, work) = self.poll_shutdown(ring)?;
            if done {
                return Ok(());
            }
            if crate::environment::now() >= deadline {
                return Err(Error::Timeout);
            }
            if !work.runnable {
                ring.wait(Some(work.deadline.unwrap_or(deadline).min(deadline)))?;
            }
        }
    }
    /// One cleanup batch, allowing the owning reactor to service transport event
    /// ACKs while accepted writes/checkpoints finish. Never starts new scrub work.
    pub(crate) fn poll_shutdown(&mut self, ring: &mut Ring) -> Result<(bool, Work)> {
        self.bind(ring)?;
        self.scrub = None;
        let mut work = Work::default();
        for shard in &mut self.shards {
            let failed = shard.allocator.is_failed();
            work.merge(shard.allocator.poll_contained(ring, 128)?);
            if !failed && shard.allocator.is_failed() {
                self.metrics.storage_quarantine();
            }
        }
        let done = self
            .shards
            .iter()
            .all(|shard| shard.allocator.is_failed() || shard.allocator.is_idle());
        Ok((done, work))
    }
    fn bind(&mut self, ring: &Ring) -> Result<()> {
        if let Some(identity) = &self.ring {
            if !Rc::ptr_eq(identity, ring.identity()) {
                return Err(invalid("foreign cache ring"));
            }
        } else {
            for shard in &self.shards {
                if let Some(view) = &shard.buffers
                    && !view.pool().same_pool(ring.pool())
                {
                    return Err(invalid("foreign shard buffer pool"));
                }
            }
            self.ring = Some(ring.identity().clone());
        }
        Ok(())
    }
    fn fault<U: Upstream>(
        &mut self,
        spec: Spec,
        deadline: Instant,
        internal: bool,
        context: &Context,
    ) -> Result<Fault<U>> {
        let limit = self.limits.active_faults
            - if internal {
                0
            } else {
                self.limits.internal_reserve
            };
        if self.active_faults.get() >= limit {
            return Err(busy("active fault limit"));
        }
        self.active_faults.set(self.active_faults.get() + 1);
        let key = spec.key();
        let shard = local_replica(&key, self.shards.len());
        let freshness = self.invalidation.object(spec.object().metadata_key().0);
        Ok(Fault {
            generation: freshness.generation.get(),
            freshness,
            buffered: false,
            _admission: FaultAdmission(self.active_faults.clone()),
            resource_retries: 0,
            resource_polls: 0,
            resource_retry_at: crate::environment::now(),

            network: None,
            scope: None,
            network_done: false,
            classified: false,

            validation: None,
            crypto: context.crypto.clone(),
            context: context.clone(),
            spec,
            key,
            shard,
            owner: self.owner.clone(),
            state: Loading::Acquire,
            route: Route::Select,
            deadline,
            candidate_deadline: None,
        })
    }
    fn resource_wait<U: Upstream>(
        &self,
        fault: &mut Fault<U>,
        site: crate::metrics::ResourceWaitSite,
    ) -> Result<Work> {
        // Early polling is bounded by parking before another resource attempt in
        // poll_value. Only timed retry exhaustion is terminal overload.
        fault.resource_polls = fault.resource_polls.saturating_add(1);
        let now = crate::environment::now();
        if now < fault.resource_retry_at {
            return Ok(Work {
                runnable: false,
                deadline: Some(fault.resource_retry_at),
            });
        }
        if fault.resource_retries >= self.limits.resource_retries {
            // Exhaustion is a shared terminal failure, not producer cancellation.
            self.metrics.resource_exhaustion(site);
            return Err(Self::finish_failure(fault, busy("resource retry limit")));
        }
        fault.resource_retries += 1;
        fault.resource_polls = 0;
        fault.resource_retry_at = now + Duration::from_millis(10);
        Ok(Work {
            runnable: false,
            deadline: Some(fault.resource_retry_at),
        })
    }
    fn admit<U: Upstream>(
        &mut self,
        fault: &Fault<U>,
        buffer: &Buffer,
        checksum: PageCrc,
    ) -> Result<()> {
        // Admission retains the original CRC; hits never rehash the payload.
        let shard = &mut self.shards[fault.shard.0].allocator;
        let time = now();
        // lookup can expire stale slab metadata even if we don't admit below.
        self.sweep_work.runnable = true;
        let result = match &fault.spec {
            Spec::Metadata(_) => return Err(invalid("payload admission for metadata")),
            Spec::Page(_) => {
                if shard.lookup(&fault.key, time).is_some() {
                    return Ok(());
                }
                shard
                    .insert_payload(fault.key, buffer.clone(), checksum.option())
                    .map_err(|e| e.error)
            }
        };
        // Rejection can schedule eviction checkpoints; keep polling the shard.
        match result {
            Ok(()) => Ok(()),
            Err(error) if error.kind() == io::ErrorKind::WouldBlock => {
                Err(busy("slab admission pressure"))
            }
            Err(error) => Err(Error::Admission(error)),
        }
    }
    fn validate(spec: &Spec, bytes: &[u8]) -> Result<()> {
        if bytes.len() != spec.len() {
            return Err(invalid("wrong value length"));
        }
        if matches!(spec, Spec::Metadata(_)) {
            return Err(invalid("metadata received in payload storage"));
        }
        Ok(())
    }
    fn publish<U: Upstream>(
        &mut self,
        fault: &Fault<U>,
        mut fill: Fill,
        len: usize,
        checksum: PageCrc,
    ) -> Result<Buffer> {
        if len != fault.len() || len > fill.as_mut_slice().len() {
            return Err(invalid("wrong value length"));
        }
        Self::validate(&fault.spec, &fill.as_mut_slice()[..len])?;
        if crate::environment::now() >= fault.deadline() {
            return Err(Error::Timeout);
        }
        let checksum = checksum
            .option()
            .unwrap_or_else(|| crate::allocator::crc64(&fill.as_mut_slice()[..len]));
        let buffer = fill.publish_checked(len, checksum)?;
        Ok(buffer)
    }
    fn start_origin<U: Upstream>(
        &mut self,
        fault: &mut Fault<U>,
        fill: Fill,
        ring: &mut Ring,
        upstream: &mut U,
    ) -> Result<()> {
        fault.classify(&self.metrics, crate::metrics::Outcome::Miss);
        if fault.route == Route::Select {
            // Size cannot authorize origin access. The adapter admits client
            // sizes and validates the complete encoding for the selected route.
            fault.route = if upstream.has_peer() {
                Route::Peer
            } else {
                Route::Backend
            };
        }
        let peer = fault.route == Route::Peer;
        let request = match &fault.spec {
            Spec::Metadata(_) => return Err(invalid("metadata cannot acquire payload storage")),
            Spec::Page(page) => {
                if peer {
                    UpstreamRequest::PeerPage(page.clone())
                } else {
                    UpstreamRequest::BackendPage(page.clone())
                }
            }
        };
        let (authority, destination) = fill.split_destination();
        let exchange = upstream.start(
            request,
            destination,
            fault.candidate_deadline.unwrap_or(fault.deadline),
            ring,
        )?;
        fault.state = Loading::Upstream {
            exchange,
            authority,
        };
        Ok(())
    }

    /// Materialize a payload for transports requiring registered storage (RDMA).
    /// Use poll_value or poll_metadata for metadata. Also drive Self::poll.
    pub fn poll_fault<U: Upstream>(
        &mut self,
        mut fault: Fault<U>,
        ring: &mut Ring,
        upstream: &mut U,
    ) -> Result<Progress<Fault<U>, Buffer>> {
        fault.buffered = true;
        match self.poll_value(fault, ring, upstream)? {
            Progress::Ready(CachedValue::Buffer(buffer)) => Ok(Progress::Ready(buffer)),
            Progress::Ready(_) => Err(invalid("buffer requested for metadata")),
            Progress::Pending { fault, work } => Ok(Progress::Pending { fault, work }),
        }
    }

    pub fn poll_value<U: Upstream>(
        &mut self,
        mut fault: Fault<U>,
        ring: &mut Ring,
        upstream: &mut U,
    ) -> Result<Progress<Fault<U>, CachedValue>> {
        if !Rc::ptr_eq(&self.owner, &fault.owner) {
            return Err(invalid("foreign cache fault"));
        }
        self.bind(ring)?;
        if crate::environment::now() >= fault.deadline {
            return Err(Error::Timeout);
        }
        if self.shards[fault.shard.0].allocator.is_failed() {
            return Err(Self::finish_failure(
                &mut fault,
                busy("slab shard quarantined"),
            ));
        }
        if fault.network.is_none()
            && matches!(fault.state, Loading::Acquire)
            && matches!(fault.spec, Spec::Metadata(_))
        {
            self.sweep_work.runnable = true;
            if let Some(record) = self.shards[fault.shard.0]
                .allocator
                .lookup_metadata(&fault.key, now())
            {
                if let Err(error) = fault.freshness.check(&record.checksum.0) {
                    return Err(Self::finish_failure(&mut fault, error));
                }
                fault.classify(&self.metrics, crate::metrics::Outcome::MetadataHit);
                return Ok(Progress::Ready(CachedValue::Metadata(record)));
            }
        }
        if fault.network.is_none()
            && matches!(fault.state, Loading::Acquire)
            && matches!(fault.spec, Spec::Page(_))
        {
            if let Some(lease) = self.shards[fault.shard.0]
                .allocator
                .lookup(&fault.key, now())
            {
                fault.state = Loading::Publishing(lease);
                fault.network_done = true;
            }
        }
        if crate::environment::now() >= fault.deadline {
            // Consumer budget exhaustion relinquishes ownership. A surviving
            // consumer can restart this candidate with its own remaining budget.
            return Err(Error::Timeout);
        }
        let candidate_end = *fault.candidate_deadline.get_or_insert_with(|| {
            upstream
                .candidate_deadline(fault.deadline)
                .min(fault.deadline)
        });
        // Unrelated runnable work can revisit a sleeper before its deadline.
        // Bound actual resource attempts without turning those visits into Busy.
        let time = crate::environment::now();
        if fault.resource_polls >= self.limits.resource_retries.saturating_mul(128)
            && time < fault.resource_retry_at
            && time < candidate_end
        {
            let deadline = fault.resource_retry_at.min(candidate_end);
            return Ok(Progress::Pending {
                fault,
                work: Work {
                    runnable: false,
                    deadline: Some(deadline),
                },
            });
        }
        // Only the consumer coordinator may reselect after candidate outcome.
        if fault.scope.is_none() {
            fault.scope = upstream.network_scope(fault.key);
            if fault.scope.is_none() {
                fault.scope = Some(crate::buffers::NetworkFlightKey {
                    value: fault.key,
                    routing: [0; 32],
                    version: 0,
                    destination: 0,
                    dependency: crate::buffers::NetworkDependency::Canonical { slot: 0 },
                });
            }
        }
        if let Some(scope) = &fault.scope
            && !fault.network_done
        {
            if fault.network.is_none() {
                match ring.pool().network_flight(scope.clone()) {
                    Ok(lease) => fault.network = Some(lease),
                    Err(_) => {
                        let mut work = self.resource_wait(
                            &mut fault,
                            crate::metrics::ResourceWaitSite::NetworkFlight,
                        )?;
                        work.deadline = Some(work.deadline.unwrap().min(candidate_end));
                        if crate::environment::now() >= candidate_end {
                            return Err(Error::Timeout);
                        }
                        return Ok(Progress::Pending { fault, work });
                    }
                }
            }
            if let Some(lease) = &mut fault.network {
                match lease.poll(&Waker::from(ring.wake_handle())) {
                    crate::buffers::NetworkProgress::Metadata(record) => {
                        fault.classify(&self.metrics, crate::metrics::Outcome::Coalesced);
                        fault.network = None;
                        fault.network_done = true;
                        fault.state = Loading::Metadata(record);
                    }
                    crate::buffers::NetworkProgress::File(file) => {
                        fault.classify(&self.metrics, crate::metrics::Outcome::Coalesced);
                        fault.network = None;
                        fault.network_done = true;
                        fault.state = Loading::File(file);
                    }
                    crate::buffers::NetworkProgress::Produce => {}
                    crate::buffers::NetworkProgress::Pending => {
                        fault.classify(&self.metrics, crate::metrics::Outcome::Coalesced);
                        if crate::environment::now() >= candidate_end {
                            return Err(Error::Timeout);
                        }

                        let deadline = candidate_end;
                        return Ok(Progress::Pending {
                            fault,
                            work: Work {
                                runnable: false,
                                deadline: Some(deadline),
                            },
                        });
                    }
                    crate::buffers::NetworkProgress::Ready(result) => {
                        fault.classify(&self.metrics, crate::metrics::Outcome::Coalesced);
                        fault.network = None;
                        fault.network_done = true;
                        match result {
                            Ok(buffer) => fault.state = Loading::Shared(buffer),
                            Err(error) => {
                                let error = Error::Shared(error);
                                if crate::environment::now() >= candidate_end
                                    && !upstream.proven_failure(&error)
                                {
                                    return Err(Error::Timeout);
                                }
                                return self.candidate_failed(fault, error, ring, upstream);
                            }
                        }
                    }
                }
            }
        }
        // At expiry poll initiated IO for proven failure, never validate late success.
        if crate::environment::now() >= candidate_end
            && !matches!(
                fault.state,
                Loading::Upstream { .. } | Loading::MetadataExchange(_)
            )
        {
            return Err(Error::Timeout);
        }
        let state = std::mem::replace(&mut fault.state, Loading::Done);
        let acquiring = matches!(state, Loading::Acquire);
        let result = self.step(&mut fault, state, ring, upstream);
        // Adapters may do synchronous work; completion never extends the budget.
        if crate::environment::now() >= fault.deadline {
            return Err(Error::Timeout);
        }
        if crate::environment::now() >= candidate_end
            && !result
                .as_ref()
                .err()
                .is_some_and(|e| upstream.proven_failure(e))
        {
            // Private expiry relinquishes the producer lease to a surviving consumer.
            return Err(Error::Timeout);
        }
        match result {
            Ok(Step::Metadata(record)) => {
                if !matches!(fault.spec, Spec::Metadata(_)) {
                    return Err(invalid("unexpected metadata result"));
                }
                if let Err(error) = fault.freshness.check(&record.checksum.0) {
                    return Err(Self::finish_failure(&mut fault, error));
                }
                let shard = &mut self.shards[fault.shard.0].allocator;
                self.sweep_work.runnable = true;
                // A joined late completion must not overwrite a subsequently admitted version.
                if fault.generation == fault.freshness.generation.get() {
                    if shard.lookup_metadata(&fault.key, now()).is_none() {
                        if let Err(error) = shard.insert_metadata(fault.key, record, now()) {
                            if error.kind() == io::ErrorKind::WouldBlock {
                                fault.state = Loading::Metadata(record);
                                let work = self.resource_wait(
                                    &mut fault,
                                    crate::metrics::ResourceWaitSite::MetadataAdmission,
                                )?;
                                return Ok(Progress::Pending { fault, work });
                            }
                            return Err(Self::finish_failure(&mut fault, Error::Admission(error)));
                        }
                    }
                    // Even request-scoped resolutions supersede earlier completions.
                    fault.freshness.generation.set(fault.generation + 1);
                }
                if let Some(mut flight) = fault.network.take() {
                    flight.finish_metadata(record);
                }
                Ok(Progress::Ready(CachedValue::Metadata(record)))
            }
            // Only a fresh, unshared acquisition rejection is retryable here.
            // Shared admission already represents terminal producer exhaustion.
            Err(Error::Admission(ref error))
                if acquiring && error.kind() == io::ErrorKind::WouldBlock =>
            {
                fault.state = Loading::Acquire;
                let work = self.resource_wait(
                    &mut fault,
                    crate::metrics::ResourceWaitSite::UpstreamAdmission,
                )?;
                Ok(Progress::Pending { fault, work })
            }
            // resource_wait may already have published a shared terminal Busy.
            // Preserve it before validation recovery can blame a healthy peer.
            Err(error) if matches!(error.root(), Error::Admission(_)) => {
                let error = Self::finish_failure(&mut fault, error);
                Err(error)
            }
            Err(_) if fault.validation.is_some() && fault.route == Route::Peer => {
                let mut exchange = fault.validation.take().unwrap();
                upstream.peer_validated(&mut exchange, false);
                fault.state = Loading::RetryPeer { exchange };
                Ok(Progress::Pending {
                    fault,
                    work: runnable(),
                })
            }
            Err(error) => {
                let error = Self::finish_failure(&mut fault, error);
                self.candidate_failed(fault, error, ring, upstream)
            }
            Ok(Step::Pending(mut work)) => {
                work.merge(Work {
                    runnable: false,
                    deadline: Some(candidate_end),
                });
                Ok(Progress::Pending { fault, work })
            }
            Ok(Step::Ready(buffer)) => {
                if !matches!(fault.state, Loading::Materialized) {
                    if let Err(error) = self.admit(&fault, &buffer, PageCrc::Compute) {
                        if matches!(&error, Error::Admission(e) if e.kind() == io::ErrorKind::WouldBlock)
                        {
                            fault.state = Loading::Admitting(buffer);
                            let work = self.resource_wait(
                                &mut fault,
                                crate::metrics::ResourceWaitSite::PayloadAdmission,
                            )?;
                            return Ok(Progress::Pending { fault, work });
                        }
                        return Err(Self::finish_failure(&mut fault, error));
                    }
                }
                if matches!(fault.spec, Spec::Page(_))
                    && !matches!(fault.state, Loading::Materialized)
                {
                    if let Some(lease) = self.shards[fault.shard.0]
                        .allocator
                        .lookup(&fault.key, now())
                    {
                        fault.state = Loading::Publishing(lease);
                        return Ok(Progress::Pending {
                            fault,
                            work: runnable(),
                        });
                    }
                    return Err(Self::finish_failure(
                        &mut fault,
                        invalid("admitted payload disappeared"),
                    ));
                }
                if let Some(mut flight) = fault.network.take() {
                    flight.finish(Ok(&buffer));
                }
                Ok(Progress::Ready(CachedValue::Buffer(buffer)))
            }
            Ok(Step::File(file)) => {
                if file.info().kind != Kind::Payload || file.info().len != fault.len() {
                    return Err(Self::finish_failure(
                        &mut fault,
                        invalid("wrong file value kind or length"),
                    ));
                }
                fault.classify(&self.metrics, crate::metrics::Outcome::DiskHit);
                if let Some(mut flight) = fault.network.take() {
                    flight.finish_file(&file);
                }
                if fault.buffered {
                    fault.network_done = true;
                    fault.state = Loading::File(file);
                    Ok(Progress::Pending {
                        fault,
                        work: runnable(),
                    })
                } else {
                    Ok(Progress::Ready(CachedValue::File(file)))
                }
            }
        }
    }
    fn finish_failure<U: Upstream>(fault: &mut Fault<U>, error: Error) -> Error {
        if let Some(mut flight) = fault.network.take() {
            let error = std::sync::Arc::new(error);
            flight.finish(Err(error.clone()));
            Error::Shared(error)
        } else {
            error
        }
    }
    fn candidate_failed<U: Upstream>(
        &mut self,
        mut fault: Fault<U>,
        error: Error,
        ring: &mut Ring,
        upstream: &mut U,
    ) -> Result<Progress<Fault<U>, CachedValue>> {
        if invalidation::precondition(&error) {
            return if matches!(fault.spec, Spec::Page(_)) {
                self.reject_page(fault, error, ring)
            } else {
                Err(error)
            };
        }
        // Joiners retain the pinned route and receive the producer's exact evidence.
        if fault.route != Route::Peer && !(fault.scope.is_some() && upstream.has_peer()) {
            return Err(error);
        }
        fault.route = if upstream.peer_failed(error)? {
            Route::Select
        } else {
            Route::Backend
        };
        fault.scope = None;
        fault.candidate_deadline = None;
        fault.network_done = false;
        fault.state = Loading::Acquire;
        Ok(Progress::Pending {
            fault,
            work: retry(),
        })
    }
    fn step<U: Upstream>(
        &mut self,
        fault: &mut Fault<U>,
        state: Loading<U::Exchange>,
        ring: &mut Ring,
        upstream: &mut U,
    ) -> Result<Step> {
        match state {
            Loading::Metadata(record) => return Ok(Step::Metadata(record)),
            Loading::Acquire if matches!(fault.spec, Spec::Metadata(_)) => {
                fault.classify(&self.metrics, crate::metrics::Outcome::Miss);
                if fault.route == Route::Select {
                    fault.route = if upstream.has_peer() {
                        Route::Peer
                    } else {
                        Route::Backend
                    };
                }
                let request = MetadataRequest {
                    object: fault.spec.object().clone(),
                };
                let request = if fault.route == Route::Peer {
                    UpstreamRequest::PeerMetadata(request)
                } else {
                    UpstreamRequest::BackendMetadata(request)
                };
                fault.state = Loading::MetadataExchange(upstream.start_metadata(
                    request,
                    fault.deadline(),
                    ring,
                )?);
            }
            Loading::MetadataRetry(exchange) => {
                fault.state = Loading::MetadataExchange(upstream.resume_metadata(
                    exchange,
                    fault.deadline(),
                    ring,
                )?);
            }
            Loading::MetadataExchange(exchange) => {
                let result = match upstream.poll(exchange, ring)? {
                    ExchangeProgress::Pending { exchange, work } => {
                        fault.state = Loading::MetadataExchange(exchange);
                        return Ok(Step::Pending(work));
                    }
                    ExchangeProgress::RetryPeer { exchange } => {
                        if fault.route != Route::Peer {
                            return Err(invalid("backend requested peer retry"));
                        }
                        fault.state = Loading::MetadataRetry(exchange);
                        return Ok(Step::Pending(runnable()));
                    }
                    ExchangeProgress::ReadyPeer { result, mut retry } => {
                        let valid = matches!(result, UpstreamResult::Metadata(_))
                            && fault.route == Route::Peer;
                        upstream.peer_validated(&mut retry, valid);
                        if !valid {
                            fault.state = Loading::MetadataRetry(retry);
                            return Ok(Step::Pending(runnable()));
                        }
                        result
                    }
                    ExchangeProgress::Ready(result) => result,
                };
                let UpstreamResult::Metadata(record) = result else {
                    return Err(invalid("metadata result kind mismatch"));
                };
                if fault.route == Route::Peer && record.expires != 0 && record.expires <= now() {
                    return Err(invalid("stale peer metadata"));
                }
                return Ok(Step::Metadata(record));
            }
            Loading::Admitting(buffer) => return Ok(Step::Ready(buffer)),
            Loading::Publishing(lease) => {
                if let Some(file) = lease.ready() {
                    return Ok(Step::File(file));
                }
                fault.state = Loading::Publishing(lease);
                return Ok(Step::Pending(match ring.slab_deadline() {
                    Some(deadline) => Work {
                        runnable: false,
                        deadline: Some(deadline),
                    },
                    None => runnable(),
                }));
            }
            Loading::File(file) => {
                if file.info().kind != Kind::Payload || file.info().len != fault.len() {
                    return Err(invalid("wrong file value kind or length"));
                }
                if !fault.buffered {
                    return Ok(Step::File(file));
                }
                match ring.pool().stage(Key::new(fault.key)) {
                    Ok(fill) => match file.read(ring, fill) {
                        Ok(ticket) => fault.state = Loading::Materializing(ticket, file),
                        Err(e) if e.error.kind() == io::ErrorKind::WouldBlock => {
                            fault.state = Loading::File(file);
                            return Ok(Step::Pending(self.resource_wait(
                                fault,
                                crate::metrics::ResourceWaitSite::MaterializeRead,
                            )?));
                        }
                        Err(e) => return Err(e.error.into()),
                    },
                    Err(_) => {
                        fault.state = Loading::File(file);
                        return Ok(Step::Pending(self.resource_wait(
                            fault,
                            crate::metrics::ResourceWaitSite::MaterializeBuffer,
                        )?));
                    }
                }
            }
            Loading::Materializing(mut ticket, file) => match ring.take_read(&mut ticket)? {
                Some(done) => {
                    if done.result? != file.info().len {
                        return Err(invalid("short file materialization"));
                    }
                    fault.state = Loading::Materialized;
                    return Ok(Step::Ready(
                        done.resource
                            .publish_checked(file.info().len, file.info().crc64)?,
                    ));
                }
                None => {
                    fault.state = Loading::Materializing(ticket, file);
                    return Ok(Step::Pending(Work::default()));
                }
            },
            Loading::Materialized => unreachable!(),
            Loading::Shared(buffer) => {
                Self::validate(&fault.spec, buffer.as_slice())?;
                return Ok(Step::Ready(buffer));
            }
            Loading::ChecksumPending(received) => {
                let ValidatedReceive {
                    fill,
                    len,
                    checksum,
                } = received;
                let result = fault.crypto.as_ref().unwrap().borrow_mut().checksum(
                    fill,
                    len,
                    checksum.option(),
                );
                match result {
                    Ok(ticket) => fault.state = Loading::Checksum(ticket),
                    Err(rejected) if rejected.error == crate::crypto::Error::WouldBlock => {
                        fault.state = Loading::ChecksumPending(ValidatedReceive {
                            fill: rejected.resource,
                            len,
                            checksum,
                        });
                        return Ok(Step::Pending(self.resource_wait(
                            fault,
                            crate::metrics::ResourceWaitSite::ChecksumQueue,
                        )?));
                    }
                    Err(rejected) => return Err(io::Error::other(rejected.error).into()),
                }
            }
            Loading::Checksum(mut ticket) => {
                let result = fault
                    .crypto
                    .as_ref()
                    .unwrap()
                    .borrow_mut()
                    .take_checksum(&mut ticket);
                match result {
                    None => {
                        fault.state = Loading::Checksum(ticket);
                        return Ok(Step::Pending(Work::default()));
                    }
                    Some(Err(error)) => {
                        if let Some(mut retry) = fault.validation.take() {
                            upstream.peer_validated(&mut retry, false);
                            fault.state = Loading::RetryPeer { exchange: retry };
                        } else {
                            return Err(io::Error::other(error).into());
                        }
                    }
                    Some(Ok((fill, len, checksum))) => {
                        if let Some(mut retry) = fault.validation.take() {
                            upstream.peer_validated(&mut retry, true);
                        }
                        return self
                            .publish_received(
                                fault,
                                ValidatedReceive {
                                    fill,
                                    len,
                                    checksum: PageCrc::Supplied(checksum),
                                },
                            )
                            .map(Step::Ready);
                    }
                }
            }
            Loading::Acquire => {
                // Recheck before allocating: a different producer may have admitted
                // the payload while this consumer waited for network ownership.
                self.sweep_work.runnable = true;
                if let Some(lease) = self.shards[fault.shard.0]
                    .allocator
                    .lookup(&fault.key, now())
                {
                    fault.state = Loading::Publishing(lease);
                    return Ok(Step::Pending(runnable()));
                }
                let reserve = if fault.route == Route::Backend {
                    0
                } else {
                    upstream.receive_reserve()?
                };
                match ring.pool().stage_reserved(Key::new(fault.key), reserve) {
                    Ok(fill) => {
                        self.start_origin(fault, fill, ring, upstream)?;
                    }
                    Err(_) => {
                        fault.state = Loading::Acquire;
                        return Ok(Step::Pending(self.resource_wait(
                            fault,
                            crate::metrics::ResourceWaitSite::ReceiveBuffer,
                        )?));
                    }
                }
            }
            Loading::Upstream {
                exchange,
                authority,
            } => match upstream.poll(exchange, ring)? {
                ExchangeProgress::Pending { exchange, work } => {
                    fault.state = Loading::Upstream {
                        exchange,
                        authority,
                    };
                    return Ok(Step::Pending(work));
                }
                ExchangeProgress::Ready(result) => {
                    if crate::environment::now() >= fault.deadline() {
                        return Err(Error::Timeout);
                    }
                    if fault.crypto.is_some() {
                        fault.state = Loading::ChecksumPending(Self::validate_receive(
                            fault, authority, result,
                        )?);
                        return Ok(Step::Pending(runnable()));
                    }
                    return self.receive(fault, authority, result).map(Step::Ready);
                }
                ExchangeProgress::ReadyPeer { result, mut retry } => {
                    if fault.route != Route::Peer {
                        return Err(invalid("backend supplied peer completion"));
                    }
                    match Self::validate_receive(fault, authority, result) {
                        Ok(received) => {
                            if fault.crypto.is_some() {
                                fault.validation = Some(retry);
                                fault.state = Loading::ChecksumPending(received);
                                return Ok(Step::Pending(runnable()));
                            }
                            upstream.peer_validated(&mut retry, true);
                            // Expiry and publication errors are not malformed
                            // transport input and must not trigger this retry.
                            return self.publish_received(fault, received).map(Step::Ready);
                        }
                        Err(_) => {
                            upstream.peer_validated(&mut retry, false);
                            fault.state = Loading::RetryPeer { exchange: retry };
                        }
                    }
                }
                ExchangeProgress::RetryPeer { exchange } => {
                    drop(authority);
                    if fault.route != Route::Peer {
                        return Err(invalid("backend requested peer retry"));
                    }
                    fault.state = Loading::RetryPeer { exchange };
                }
            },
            Loading::RetryPeer { exchange } => {
                // Preserve the continuation across pressure;
                // never hand the old authority to a new transport destination.
                match ring
                    .pool()
                    .stage_reserved(Key::new(fault.key), upstream.receive_reserve()?)
                {
                    Ok(fill) => {
                        let (authority, destination) = fill.split_destination();
                        let exchange = upstream.resume_peer(
                            exchange,
                            destination,
                            fault.candidate_deadline.unwrap_or(fault.deadline),
                            ring,
                        )?;
                        fault.state = Loading::Upstream {
                            exchange,
                            authority,
                        };
                    }
                    Err(_) => {
                        fault.state = Loading::RetryPeer { exchange };
                        return Ok(Step::Pending(self.resource_wait(
                            fault,
                            crate::metrics::ResourceWaitSite::ReceiveBuffer,
                        )?));
                    }
                }
            }
            Loading::Done => return Err(invalid("fault already consumed")),
        }
        Ok(Step::Pending(runnable()))
    }

    fn receive<U: Upstream>(
        &mut self,
        fault: &Fault<U>,
        authority: PublicationAuthority,
        result: UpstreamResult,
    ) -> Result<Buffer> {
        let received = Self::validate_receive(fault, authority, result)?;
        self.publish_received(fault, received)
    }
    fn validate_receive<U: Upstream>(
        fault: &Fault<U>,
        authority: PublicationAuthority,
        result: UpstreamResult,
    ) -> Result<ValidatedReceive> {
        let received = match (&fault.spec, fault.route, result) {
            (Spec::Page(page), Route::Backend, UpstreamResult::BackendPage { received, facts }) => {
                page.validate_backend(&facts)?;
                received
            }
            (Spec::Page(_), Route::Peer, UpstreamResult::PeerPage(received)) => received,
            _ => return Err(invalid("upstream result kind mismatch")),
        };
        let Received {
            destination,
            len,
            checksum,
        } = received;
        let mut fill = authority
            .reunite(destination)
            .map_err(|_| invalid("foreign receive destination"))?;
        if len != fault.len() || len > fill.as_mut_slice().len() {
            return Err(invalid("wrong receive length"));
        }
        let checksum = if fault.route == Route::Peer {
            let expected = checksum.ok_or_else(|| invalid("peer checksum is required"))?;
            if fault.crypto.is_none()
                && crate::allocator::crc64(&fill.as_mut_slice()[..len]) != expected
            {
                return Err(invalid("peer checksum mismatch"));
            }
            PageCrc::Supplied(expected)
        } else {
            PageCrc::Compute
        };
        Self::validate(&fault.spec, &fill.as_mut_slice()[..len])?;
        Ok(ValidatedReceive {
            fill,
            len,
            checksum,
        })
    }
    fn publish_received<U: Upstream>(
        &mut self,
        fault: &Fault<U>,
        received: ValidatedReceive,
    ) -> Result<Buffer> {
        self.publish(fault, received.fill, received.len, received.checksum)
    }

    /// Bounded round-robin disk maintenance; no transport service is performed.
    pub fn poll(&mut self, ring: &mut Ring, budget: usize) -> Result<Work> {
        self.bind(ring)?;
        if self.sealed {
            return Ok(Work::default());
        }
        if budget == 0 {
            return Ok(runnable());
        }
        let mut work = Work::default();
        // Poll every shard before sleeping; budget exhaustion remains runnable.
        if self.cursor == 0 {
            self.completion_epoch = ring.completion_epoch();
        }
        // Carry sweep outcomes across small-budget turns before deciding to sleep.
        let count = budget.min(self.shards.len() - self.cursor);
        for _ in 0..count {
            let allocator = &mut self.shards[self.cursor].allocator;
            let failed = allocator.is_failed();
            self.sweep_work.merge(allocator.poll_contained(ring, 1)?);
            if !failed && allocator.is_failed() {
                self.metrics.storage_quarantine();
            }
            self.cursor = (self.cursor + 1) % self.shards.len();
        }
        if self.cursor != 0 {
            work.runnable = true;
        } else {
            work.merge(std::mem::take(&mut self.sweep_work));
            // Revisit earlier shards if a completion arrived during the sweep.
            work.runnable |= self.completion_epoch != ring.completion_epoch();
        }
        if !self.maintenance {
            work.merge(self.poll_scrub(ring)?);
        }
        Ok(work)
    }
}
struct ValidatedReceive {
    fill: Fill,
    len: usize,
    checksum: PageCrc,
}
struct Scrub {
    key: [u8; 32],
    shard: usize,
    lease: allocator::ReadLease,
    read: ReadHandle,
}
enum Step {
    Metadata(Record),
    File(allocator::FileValue),
    Pending(Work),
    Ready(Buffer),
}

#[cfg(test)]
include!(concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/tests/storage/cache.rs"
));
