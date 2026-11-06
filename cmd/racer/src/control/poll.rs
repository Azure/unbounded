//! One concrete control-plane long-poll client for the two documents Racer
//! follows: membership publications and keyring bundles.
//!
//! Each [`Poll`] owns one document's cursor, retry, preparation job, and
//! installation state. The two polls are independent, so a keyring is never
//! queued behind a large publication. Per-document policy (route, size bound,
//! authentication recovery, delta support) lives on [`Doc`]. Replay checks on
//! installation stay with `cell::accept` and the bundle installer.
//!
//! I/O, credentials, and install hooks stay on their owner thread. Only owned
//! preparation closures and immutable shared documents cross thread boundaries.

use super::{ReactorControlIo, Snapshot, transient};
use crate::control::rest::{self, Scope as _};
use crate::error::{Error, Operation, Result};
use crate::runtime::RequestScope;
use racer_wire::control as wire;
use sha2::{Digest as _, Sha256};
use std::{
    cell::{Cell, RefCell},
    rc::Rc,
    sync::Arc,
    thread::JoinHandle,
    time::{Duration, Instant},
};

#[cfg(test)]
mod tests;

/// Header carrying the canonical digest of the retained delta base.
pub const DIGEST_HEADER: &str = "X-Racer-Delta-Base";

/// The control documents a node follows, each with its own policy.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Doc {
    /// Membership publications: identity-authenticated, large, delta-encoded.
    Publication,
    /// Keyring bundles: token fallback at bootstrap, small, full documents only.
    Keyring,
}

/// Recovery policy after a missing or rejected client identity.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Rejection {
    /// Never send a token and require a valid client identity.
    IdentityOnly,
    /// Retry once on a fresh server-authenticated connection with a fresh token.
    TokenFallback,
}

impl Doc {
    /// Long-poll route without a query.
    pub fn path(self) -> &'static str {
        match self {
            Self::Publication => wire::SNAPSHOT_PATH,
            Self::Keyring => wire::KEYRING_PATH,
        }
    }

    /// Hard bound on a successful response body.
    pub fn limit(self) -> usize {
        match self {
            Self::Publication => wire::MAX_PUBLICATION_BYTES,
            Self::Keyring => wire::MAX_BUNDLE_BYTES,
        }
    }

    /// Authentication recovery. Keyrings need the token path at bootstrap,
    /// before any client identity exists.
    pub fn rejection(self) -> Rejection {
        match self {
            Self::Publication => Rejection::IdentityOnly,
            Self::Keyring => Rejection::TokenFallback,
        }
    }

    /// Whether a response may be a delta against the retained base.
    pub fn deltas(self) -> bool {
        self == Self::Publication
    }

    /// Decode and validate a full document.
    pub fn decode(self, bytes: &[u8]) -> Result<Document> {
        Ok(match self {
            Self::Publication => Document::Publication(Arc::new(wire::decode_publication(bytes)?)),
            Self::Keyring => Document::Keyring(Arc::new(wire::decode_bundle(bytes)?)),
        })
    }

    /// Decode a response once, as a delta against `base` (the retained base
    /// and its [`Document::digest`]) when the body is a delta and this document
    /// supports deltas, otherwise as a full document. Returns the validated
    /// document with its digest.
    fn decode_response(
        self,
        bytes: &[u8],
        base: Option<(&Document, &str)>,
    ) -> Result<(Document, String)> {
        match self {
            Self::Publication => {
                let base = base.and_then(|(document, digest)| match document {
                    Document::Publication(base) => Some((base.as_ref(), digest)),
                    Document::Keyring(_) => None,
                });
                let (document, digest) = wire::decode_publication_response(bytes, base)?;
                Ok((Document::Publication(Arc::new(document)), digest))
            }
            Self::Keyring => {
                let document = self.decode(bytes)?;
                let digest = document.digest()?;
                Ok((document, digest))
            }
        }
    }
}

/// A shared immutable control document.
#[derive(Clone)]
pub enum Document {
    /// A membership publication.
    Publication(Arc<wire::Publication>),
    /// A keyring bundle.
    Keyring(Arc<wire::KeyringBundle>),
}

impl std::fmt::Debug for Document {
    /// Identify the document by kind and cursor without dumping its contents.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        let kind = match self {
            Self::Publication(_) => "Publication",
            Self::Keyring(_) => "Keyring",
        };
        write!(f, "{kind}({})", self.version())
    }
}

impl Document {
    /// Ordered cursor serialized into the `after` query parameter.
    pub fn version(&self) -> u64 {
        match self {
            Self::Publication(document) => document.sequence.0,
            Self::Keyring(document) => document.generation.0,
        }
    }

    /// Canonical content fingerprint sent as the delta-base header.
    pub fn digest(&self) -> Result<String> {
        match self {
            Self::Publication(document) => Ok(wire::content_hash(document)?),
            Self::Keyring(document) => bundle_digest(document),
        }
    }
}

/// Hash the canonical bundle content directly: sorted roots and the compact
/// key table (canonical by construction), without re-encoding and
/// re-validating the whole JSON bundle on each poll.
fn bundle_digest(document: &wire::KeyringBundle) -> Result<String> {
    let mut hash = Sha256::new();
    hash.update(b"racer/keyring/digest/v3\0");
    hash.update(document.schema_version.to_be_bytes());
    hash.update((document.cluster.0.len() as u64).to_be_bytes());
    hash.update(document.cluster.0.as_bytes());
    hash.update(document.generation.0.to_be_bytes());
    let mut roots: Vec<_> = document.peer_trust_roots.iter().collect();
    roots.sort();
    hash.update((roots.len() as u64).to_be_bytes());
    for root in roots {
        hash.update((root.len() as u64).to_be_bytes());
        hash.update(root);
    }
    let keys = wire::encode_cache_keys(&document.cache_keys)?;
    hash.update(&*keys);
    Ok(format!("{:x}", hash.finalize()))
}

/// Prepared immutable state returned from the background thread.
#[derive(Clone)]
pub enum Prepared {
    /// A projected publication snapshot.
    Snapshot(Arc<Snapshot>),
    /// A keyring bundle whose trust roots were validated.
    Keyring(Arc<wire::KeyringBundle>),
}

/// Finite CPU-only preparation. Only this closure crosses threads. It must
/// terminate independently of the owner: no owner callbacks, I/O, or waits
/// for owner-held locks. Dropping the poll joins it rather than detaching work.
pub type Preparation = Box<dyn FnOnce() -> Result<Prepared> + Send + 'static>;

/// Error policy without conflating cancellation with permanent rejection.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum FailureClass {
    /// Retry after bounded backoff.
    Retry,
    /// Authentication was explicitly rejected.
    Rejected,
    /// The caller abandoned this turn; do not count it as a failed attempt.
    Canceled,
    /// Invalid or incompatible state.
    Permanent,
}

/// Classify a control error for retry and rejection policy.
pub fn classify(error: Error) -> FailureClass {
    match error {
        Error::Canceled => FailureClass::Canceled,
        Error::Unauthorized => FailureClass::Rejected,
        error if transient(error) => FailureClass::Retry,
        _ => FailureClass::Permanent,
    }
}

/// Failed fetch plus a validated server backoff hint.
#[derive(Clone, Copy, Debug)]
pub struct FetchError {
    /// Original domain or transport error.
    pub error: Error,

    /// Validated Retry-After, used only with retry-classified failures.
    pub retry_after: Option<Duration>,
}

impl<E: Into<Error>> From<E> for FetchError {
    /// Local failures carry no remote retry hint.
    fn from(error: E) -> Self {
        Self {
            error: error.into(),
            retry_after: None,
        }
    }
}

/// Network side of a poll: the session's authenticated REST client, or a
/// scripted wire boundary in tests. Implementations enforce the byte bound.
pub trait Fetch {
    /// Perform a GET with an optional canonical delta-base digest.
    fn get<'a>(
        &'a self,
        path: &'a str,
        digest: Option<&'a str>,
        limit: usize,
        scope: &'a RequestScope,
    ) -> racer_io::Operation<'a, rest::Response, FetchError>;

    /// Release idle transport state during shutdown.
    fn close(&self);

    /// Try another control seat next, after a response the poll rejected,
    /// such as an older cursor from a stale seat. Default: no effect.
    fn demote(&self) {}
}

/// Domain validation and install side of a poll. May retain owner-local Rc.
pub trait Install {
    /// Capture only thread-safe preparation inputs. Do not perform heavy work here.
    fn prepare(&self, document: Document) -> Result<Preparation>;

    /// Validate and atomically install prepared state. Every error must leave
    /// live resources unchanged. Retryable failure retains prepared state.
    fn install(&self, document: &Document, prepared: &Prepared) -> Result<()>;

    /// Observe a decoded document whose digest was computed, before acceptance.
    fn received(&self, _: &Document) {}
}

/// Validate every response, including structured failure code/status agreement.
/// Non-success status is an error even when the body is well formed.
pub fn validate_response(response: &rest::Response) -> Result<()> {
    if response.status == 200 || response.status == 204 {
        return Ok(());
    }
    let (status, error) = match wire::decode_error(&response.body)?.code {
        wire::ProtocolFailure::InvalidRequest => (400, Error::InvalidRequest),
        wire::ProtocolFailure::Unauthenticated => (401, Error::Unauthorized),
        wire::ProtocolFailure::Forbidden => (403, Error::Unauthorized),
        wire::ProtocolFailure::Conflict => (409, Error::Replay),
        wire::ProtocolFailure::TooLarge => (413, Error::InvalidRequest),
        wire::ProtocolFailure::UnsupportedVersion => (426, Error::IncompatibleMembership),
        wire::ProtocolFailure::Overloaded => (429, Error::Overloaded),
        wire::ProtocolFailure::Unavailable => (503, Error::Unavailable),
    };
    if status != response.status {
        return Err(Error::InvalidRequest);
    }
    Err(error)
}

/// Scheduling bounds supplied by the caller.
#[derive(Clone, Copy, Debug)]
pub struct Schedule {
    /// Maximum wall time allowed for one turn.
    pub turn: Duration,

    /// Local preparation/install retry timer, independent of network completion.
    pub tick: Duration,

    /// Initial network, preparation, and rejected-candidate retry delay.
    pub retry_min: Duration,

    /// Maximum exponential backoff, before a larger server Retry-After.
    pub retry_max: Duration,
}

impl Schedule {
    /// Production bounds: responsive during held polls and remote backoff.
    pub fn control() -> Self {
        Self {
            turn: wire::POLL_WAIT + Duration::from_secs(10),
            tick: Duration::from_millis(10),
            retry_min: Duration::from_secs(1),
            retry_max: Duration::from_secs(30),
        }
    }
}

/// Coherent owner diagnostics. Pending is receipt, never acceptance.
#[derive(Clone, Copy, Debug)]
pub struct Status {
    /// Last successfully installed version.
    pub accepted: Option<u64>,

    /// Latest unaccepted version, including queued or preparing documents.
    pub pending: Option<u64>,

    /// Last non-cancellation error from this poll.
    pub error: Option<Error>,

    /// Next network attempt time, including any rejected-candidate retry floor.
    pub next_fetch: Instant,

    /// Whether submissions are permanently closed.
    pub stopped: bool,

    /// Local tick timers armed so far; diagnostics for idle-wake tests.
    pub ticks: u64,
}

/// A single-owner long poll. Dropping a turn does not discard preparation work.
/// Call shutdown to fence background work with a bounded scope. Drop joins any
/// remaining finite CPU closure.
pub struct Poll {
    io: Rc<ReactorControlIo>,

    doc: Doc,

    fetch: Box<dyn Fetch>,

    install: Box<dyn Install>,

    schedule: Schedule,

    /// Latest complete decoded document, including unaccepted state, with its
    /// digest once computed, so each poll does not rehash the delta base.
    last: RefCell<Option<(Document, Option<String>)>>,

    state: RefCell<Progress>,

    stopped: Cell<bool>,

    ticks: Cell<u64>,
}

/// Owner-local state separated from the independently borrowed network future.
struct Progress {
    accepted: Option<Document>,

    queued: Option<Document>,

    job: Option<Job>,

    prepared: Option<(Document, Prepared)>,

    error: Option<Error>,

    failures: u32,

    rejected: Option<Refused>,

    next_fetch: Instant,

    next_prepare: Instant,
}

/// A rejected candidate version has its own retry history, independent of GETs.
/// Corrected content at the same version may retry and clears history on install.
struct Refused {
    version: u64,

    failures: u32,

    until: Instant,
}

/// One owned background job retained until its completion fence.
struct Job {
    document: Document,

    handle: JoinHandle<Result<Prepared>>,
}

impl Drop for Poll {
    /// Fence preparation before dropping the install hook or other owner resources.
    fn drop(&mut self) {
        if let Some(job) = self.state.get_mut().job.take() {
            // A worker panic is an error, not a second panic during owner unwind.
            let _ = job.handle.join();
        }
    }
}

impl Poll {
    /// Create a poll; restore startup acceptance explicitly with `seed`.
    pub fn new(
        io: Rc<ReactorControlIo>,
        doc: Doc,
        fetch: Box<dyn Fetch>,
        install: Box<dyn Install>,
        schedule: Schedule,
    ) -> Result<Self> {
        if schedule.turn.is_zero()
            || schedule.tick.is_zero()
            || schedule.retry_min.is_zero()
            || schedule.retry_max < schedule.retry_min
        {
            return Err(Error::InvalidConfiguration);
        }
        let now = rest::Io::ctx(io.as_ref()).clock.now();
        if [schedule.turn, schedule.tick, schedule.retry_max]
            .into_iter()
            .any(|delay| now.checked_add(delay).is_none())
        {
            return Err(Error::InvalidConfiguration);
        }
        Ok(Self {
            io,
            doc,
            fetch,
            install,
            schedule,
            last: RefCell::new(None),
            state: RefCell::new(Progress {
                accepted: None,
                queued: None,
                job: None,
                prepared: None,
                error: None,
                failures: 0,
                rejected: None,
                next_fetch: now,
                next_prepare: now,
            }),
            stopped: Cell::new(false),
            ticks: Cell::new(0),
        })
    }

    /// The followed document.
    pub fn doc(&self) -> Doc {
        self.doc
    }

    fn now(&self) -> Instant {
        rest::Io::ctx(self.io.as_ref()).clock.now()
    }

    /// Lease the latest decoded document, which is not necessarily accepted.
    pub fn last(&self) -> Option<Document> {
        self.last
            .borrow()
            .as_ref()
            .map(|(document, _)| document.clone())
    }

    /// Restore a known delta base, for example an accepted startup document.
    pub fn restore(&self, document: Option<Document>) {
        *self.last.borrow_mut() = document.map(|document| (document, None));
    }

    /// The retained base with its digest, computing and caching the digest on
    /// first use. The cache is replaced together with the document.
    fn base(&self) -> Result<Option<(Document, String)>> {
        let mut last = self.last.borrow_mut();
        let Some((document, digest)) = last.as_mut() else {
            return Ok(None);
        };
        let digest = match digest {
            Some(digest) => digest.clone(),
            None => digest.insert(document.digest()?).clone(),
        };
        Ok(Some((document.clone(), digest)))
    }

    /// Seed a document already installed by startup, before any work is submitted.
    pub fn seed(&mut self, document: Document) -> Result<()> {
        let state = self.state.get_mut();
        if state.job.is_some()
            || state.queued.is_some()
            || state.prepared.is_some()
            || state.accepted.is_some()
            || self.stopped.get()
        {
            return Err(Error::Unavailable);
        }
        state.accepted = Some(document.clone());
        self.restore(Some(document));
        Ok(())
    }

    /// Prefer another seat on the next fetch, after a rejection seen outside the poll.
    pub fn demote(&self) {
        self.fetch.demote();
    }

    /// Inspect acceptance, pending receipt, and retry state without network I/O.
    pub fn status(&self) -> Status {
        let state = self.state.borrow();
        let accepted = state.accepted.as_ref().map(Document::version);
        let pending = self
            .last()
            .map(|d| d.version())
            .filter(|v| Some(*v) != accepted);
        Status {
            accepted,
            pending,
            error: state.error,
            next_fetch: state.fetch_at(),
            stopped: self.stopped.get(),
            ticks: self.ticks.get(),
        }
    }

    /// Fetch after the retained cursor, then try full decode, delta, and at most
    /// one cursor-free full retry under the original scope. Errors retain the base.
    ///
    /// A failure while the scope is still live demotes the seat that served it,
    /// so a live seat with stale state cannot hold the poll.
    pub async fn fetch(
        &self,
        scope: &RequestScope,
    ) -> std::result::Result<Option<Document>, FetchError> {
        let result = self.fetch_once(scope).await;
        if result.is_err() && scope.check().is_ok() {
            self.fetch.demote();
        }
        result
    }

    async fn fetch_once(
        &self,
        scope: &RequestScope,
    ) -> std::result::Result<Option<Document>, FetchError> {
        scope.check()?;
        let base = self.base()?;
        let route = self.doc.path();
        let limit = self.doc.limit();
        let path = base.as_ref().map_or_else(
            || route.to_owned(),
            |(d, _)| format!("{route}?after={}", d.version()),
        );
        let digest = base.as_ref().map(|(_, digest)| digest.as_str());
        let response = self.fetch.get(&path, digest, limit, scope).await?;
        scope.check()?;
        if response.status == 204 {
            if base.is_none() || !response.body.is_empty() {
                return Err(Error::InvalidRequest.into());
            }
            return Ok(None);
        }
        check_response(&response, limit)?;
        let delta_base = base
            .as_ref()
            .filter(|_| self.doc.deltas())
            .map(|(document, digest)| (document, digest.as_str()));
        let decoded = self.doc.decode_response(&response.body, delta_base);
        // Even a new version must have a usable digest before becoming a base.
        // Otherwise one invalid candidate poisons every subsequent request.
        let (document, document_digest) = match decoded {
            Ok(decoded) => decoded,
            Err(_) => {
                let response = self.fetch.get(route, None, limit, scope).await?;
                check_response(&response, limit)?;
                self.doc.decode_response(&response.body, None)?
            }
        };
        scope.check()?;
        self.install.received(&document);
        if let Some((base, _)) = &base
            && (document.version() < base.version()
                || document.version() == base.version() && Some(document_digest.as_str()) != digest)
        {
            return Err(Error::Replay.into());
        }
        *self.last.borrow_mut() = Some((document.clone(), Some(document_digest)));
        Ok(Some(document))
    }

    /// Sleep for one local tick, counting armed timers for diagnostics.
    fn tick<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, ()> {
        self.ticks.set(self.ticks.get() + 1);
        let now = self.now();
        self.io
            .sleep(now.checked_add(self.schedule.tick).unwrap_or(now), scope)
    }

    /// Run one scoped fetch while local installation advances on a real timer.
    /// Dropping this future closes its checked-out socket, not the owned CPU job.
    /// Rejected versions back off independently of successful GET receipts; a
    /// later fetch may supersede them without retaining a rejected delta base.
    pub async fn turn(&mut self, scope: &RequestScope) -> Result<()> {
        if self.stopped.get() {
            return Err(Error::Canceled);
        }
        let now = self.now();
        let scope = scope.narrowed(now.checked_add(self.schedule.turn).unwrap_or(now));
        scope.check()?;
        let next_fetch = self.state.borrow().fetch_at();
        let this = &*self;
        let state = &self.state;
        let network = async {
            this.io.sleep(next_fetch, &scope).await?;
            this.fetch(&scope).await
        };
        futures::pin_mut!(network);
        loop {
            scope.check()?;
            state.borrow_mut().advance(this)?;
            // Local work polls its CPU job on the tick timer. With none pending,
            // only the network can make progress, so park on it alone.
            let result = if state.borrow().has_local_work() {
                let tick = this.tick(&scope);
                match futures::future::select(network.as_mut(), tick).await {
                    futures::future::Either::Left((result, _)) => result,
                    futures::future::Either::Right((result, _)) => {
                        result?;
                        continue;
                    }
                }
            } else {
                network.as_mut().await
            };
            let mut progress = state.borrow_mut();
            match result {
                Ok(document) => {
                    progress.failures = 0;
                    progress.next_fetch = this.now() + this.schedule.tick;
                    if let Some(document) = document
                        && progress
                            .accepted
                            .as_ref()
                            .is_none_or(|old| document.version() > old.version())
                        && progress
                            .job
                            .as_ref()
                            .is_none_or(|job| document.version() > job.document.version())
                        && progress
                            .prepared
                            .as_ref()
                            .is_none_or(|(old, _)| document.version() > old.version())
                    {
                        progress.queued = Some(document);
                    }
                    progress.advance(this)?;
                    return Ok(());
                }
                Err(failure) => {
                    // A seat serving an older or conflicting generation (for
                    // example one that restarted during a rollout and has not
                    // caught up) is a stale source, not local corruption. The
                    // document was discarded and the retained base is
                    // unchanged, and `fetch` already demoted the seat, so
                    // retry with backoff instead of failing the process.
                    let stale_seat = failure.error == Error::Replay;
                    if classify(failure.error) != FailureClass::Canceled {
                        progress.error = Some(failure.error);
                        progress.failures = progress.failures.saturating_add(1);
                        let delay = retry_delay(this.rng(), this.schedule, progress.failures);
                        let delay = if stale_seat || classify(failure.error) == FailureClass::Retry
                        {
                            delay.max(failure.retry_after.unwrap_or_default())
                        } else {
                            delay
                        };
                        let now = this.now();
                        progress.next_fetch =
                            now.checked_add(delay).or(scope.deadline()).unwrap_or(now);
                    }
                    if stale_seat {
                        return Err(Error::Unavailable);
                    }
                    return Err(failure.error);
                }
            }
        }
    }

    fn rng(&self) -> &racer_io::Rng {
        &rest::Io::ctx(self.io.as_ref()).rng
    }

    /// Advance local preparation and installation on the tick timer without any
    /// network I/O, until no received document awaits preparation or install.
    /// Callers that need a received document installed before fetching again use
    /// this instead of another turn, which would issue a redundant GET whenever
    /// preparation outlasts one tick.
    pub async fn settle(&mut self, scope: &RequestScope) -> Result<()> {
        if self.stopped.get() {
            return Err(Error::Canceled);
        }
        let now = self.now();
        let scope = scope.narrowed(now.checked_add(self.schedule.turn).unwrap_or(now));
        let state = &self.state;
        loop {
            scope.check()?;
            state.borrow_mut().advance(self)?;
            if !state.borrow().has_local_work() {
                return Ok(());
            }
            self.tick(&scope).await?;
        }
    }

    /// Stop fetches and boundedly wait for the sole CPU job. On timeout or caller
    /// cancellation the job stays owned; invoke shutdown again with a fresh scope.
    pub async fn shutdown(&mut self, scope: &RequestScope) -> Result<()> {
        self.stopped.set(true);
        self.fetch.close();
        let accepted = {
            let state = self.state.get_mut();
            state.queued.take();
            state.prepared.take();
            state.accepted.clone()
        };
        self.restore(accepted);
        let now = self.now();
        let scope = scope.narrowed(now.checked_add(self.schedule.turn).unwrap_or(now));
        while self
            .state
            .borrow()
            .job
            .as_ref()
            .is_some_and(|job| !job.handle.is_finished())
        {
            scope.check()?;
            self.io
                .sleep(self.now() + self.schedule.tick, &scope)
                .await?;
        }
        if let Some(job) = self.state.get_mut().job.take() {
            job.handle.join().map_err(|_| Error::Internal)?.ok();
        }
        Ok(())
    }
}

/// Map an install-time replay to a retryable stale-source failure.
///
/// Local acceptance can see newer state than the poll's own cursor: another
/// session in this process may already have installed a newer generation into
/// shared state, or a seat restarted during a rollout may serve an older
/// generation to a fresh poll that has no cursor to send. The install already
/// refused the document and the caller has rejected it and restored the base,
/// so nothing older is ever accepted. Demote the seat that served it and report
/// `Unavailable` so the owner retries with backoff instead of exiting. The
/// recorded status error stays `Replay` for diagnostics.
fn stale_install(poll: &Poll, error: Error) -> Error {
    if error != Error::Replay {
        return error;
    }
    poll.fetch.demote();
    Error::Unavailable
}

/// Enforce success framing even when a scripted fetch is used.
fn check_response(response: &rest::Response, limit: usize) -> Result<()> {
    if response.status != 200 || response.body.len() > limit {
        return Err(Error::InvalidRequest);
    }
    Ok(())
}

impl Progress {
    /// Whether a received document still awaits preparation, its CPU job, or install.
    fn has_local_work(&self) -> bool {
        self.queued.is_some() || self.job.is_some() || self.prepared.is_some()
    }

    /// Enforce both transport backoff and the independent candidate retry floor.
    fn fetch_at(&self) -> Instant {
        self.rejected.as_ref().map_or(self.next_fetch, |rejected| {
            self.next_fetch.max(rejected.until)
        })
    }

    /// Reject without advancing the delta base or resetting failure history on a
    /// successful receipt. Keep one version's history, bounded regardless of input.
    fn reject(&mut self, poll: &Poll, version: u64, now: Instant) {
        let failures = self
            .rejected
            .as_ref()
            .filter(|old| old.version == version)
            .map_or(1, |old| old.failures.saturating_add(1));
        self.rejected = Some(Refused {
            version,
            failures,
            until: now
                .checked_add(retry_delay(poll.rng(), poll.schedule, failures))
                .unwrap_or(now),
        });
        poll.restore(self.accepted.clone());
    }

    /// Retry preparation of `document` after a jittered minimum delay.
    fn requeue(&mut self, poll: &Poll, document: Document, now: Instant) {
        self.queued = Some(document);
        self.next_prepare = now
            .checked_add(jitter(poll.rng(), poll.schedule.retry_min))
            .unwrap_or(now);
    }

    /// Reap at most one job, install at most once, and submit at most one job.
    fn advance(&mut self, poll: &Poll) -> Result<()> {
        let now = poll.now();
        if self
            .job
            .as_ref()
            .is_some_and(|job| job.handle.is_finished())
        {
            let job = self.job.take().expect("finished job");
            let result = job.handle.join().unwrap_or(Err(Error::Internal));
            let superseded = self
                .queued
                .as_ref()
                .is_some_and(|d| d.version() > job.document.version());
            if !superseded {
                match result {
                    Ok(prepared) => self.prepared = Some((job.document, prepared)),
                    Err(error) => {
                        if classify(error) != FailureClass::Canceled {
                            self.error = Some(error);
                        }
                        if matches!(
                            classify(error),
                            FailureClass::Retry | FailureClass::Canceled
                        ) {
                            self.requeue(poll, job.document, now);
                        } else {
                            self.reject(poll, job.document.version(), now);
                            return Err(error);
                        }
                    }
                }
            }
        }
        if self.prepared.as_ref().is_some_and(|(d, _)| {
            self.queued
                .as_ref()
                .is_some_and(|q| q.version() > d.version())
        }) {
            self.prepared.take();
        }
        if let Some((document, prepared)) = &self.prepared {
            match poll.install.install(document, prepared) {
                Ok(()) => {
                    self.accepted = Some(document.clone());
                    self.prepared.take();
                    self.error = None;
                    self.rejected = None;
                }
                Err(error) => {
                    if classify(error) == FailureClass::Canceled {
                        return Err(error);
                    }
                    self.error = Some(error);
                    if classify(error) != FailureClass::Retry {
                        let version = document.version();
                        self.prepared.take();
                        self.reject(poll, version, now);
                        return Err(stale_install(poll, error));
                    }
                }
            }
        }
        if self.job.is_none()
            && self.prepared.is_none()
            && now >= self.next_prepare
            && let Some(document) = self.queued.take()
        {
            if self
                .accepted
                .as_ref()
                .is_some_and(|old| old.version() >= document.version())
            {
                return Ok(());
            }
            let work = match poll.install.prepare(document.clone()) {
                Ok(work) => work,
                Err(error) => {
                    match classify(error) {
                        FailureClass::Retry | FailureClass::Canceled => {
                            self.requeue(poll, document, now);
                        }
                        FailureClass::Rejected | FailureClass::Permanent => {
                            self.reject(poll, document.version(), now);
                        }
                    }
                    if classify(error) != FailureClass::Canceled {
                        self.error = Some(error);
                    }
                    return Err(error);
                }
            };
            match std::thread::Builder::new()
                .name("controlplane-prepare".into())
                .spawn(work)
            {
                Ok(handle) => self.job = Some(Job { document, handle }),
                Err(_) => {
                    self.requeue(poll, document, now);
                    return Err(Error::Io);
                }
            }
        }
        Ok(())
    }
}

/// Capped exponential retry shared by independent network and candidate counters.
fn retry_delay(rng: &racer_io::Rng, schedule: Schedule, failures: u32) -> Duration {
    jitter(
        rng,
        schedule
            .retry_min
            .saturating_mul(1 << failures.saturating_sub(1).min(20))
            .min(schedule.retry_max),
    )
}

/// Equal jitter in [half, full] of the capped exponential delay. Remote hints are
/// applied afterward as a floor, so jitter never shortens Retry-After. If entropy
/// fails, retain the full delay rather than spinning or discarding the real error.
fn jitter(rng: &racer_io::Rng, delay: Duration) -> Duration {
    let mut bytes = [0; 4];
    if rng.fill(&mut bytes).is_err() {
        return delay;
    }
    jitter_sample(delay, u32::from_le_bytes(bytes))
}

/// Scale without floating-point rounding or duration overflow, even at MAX.
fn jitter_sample(delay: Duration, sample: u32) -> Duration {
    let half = delay / 2;
    let span = delay - half;
    let nanos = span.as_nanos() * u128::from(sample) / u128::from(u32::MAX);
    (half
        + Duration::new(
            (nanos / 1_000_000_000) as u64,
            (nanos % 1_000_000_000) as u32,
        ))
    .max(Duration::from_nanos(1).min(delay))
}
