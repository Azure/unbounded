//! Scoped long polls, credential policy, and a single-owner synchronization loop.

use crate::Error;
use std::{
    cell::{Cell, RefCell},
    fmt::Display,
    rc::Rc,
    sync::Arc,
    thread::JoinHandle,
    time::{Duration, Instant},
};
use uring_runtime::{Operation, Scope as _};
use wire_codec::rest::{self, Scope as _};

/// Caller-local codec implementation; documents may belong to another crate.
pub trait Codec {
    /// Owned immutable wire state, also retained as the next delta base.
    type Document: Send + std::marker::Sync + 'static;

    /// Ordered cursor serialized into the `after` query parameter.
    type Version: Copy + Ord + Display;

    /// Domain errors preserve structured wire validation failures.
    type Error: Copy + Send + From<Error> + From<rest::Error> + From<uring_runtime::Error> + 'static;

    /// Read the document's cursor.
    fn version(&self, document: &Self::Document) -> Self::Version;

    /// Decode and validate a full document.
    fn decode(&self, bytes: &[u8]) -> Result<Self::Document, Self::Error>;

    /// Apply and validate a delta against the exact retained base.
    fn delta(&self, base: &Self::Document, bytes: &[u8]) -> Result<Self::Document, Self::Error>;

    /// Return a canonical content fingerprint suitable for a request header.
    fn digest(&self, document: &Self::Document) -> Result<String, Self::Error>;
}

/// Error policy without conflating scope cancellation with permanent rejection.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum FailureClass {
    /// Retry after bounded backoff.
    Retry,
    /// Authentication was explicitly rejected.
    Rejected,
    /// The caller abandoned this turn; do not count it as a failed attempt.
    Cancelled,
    /// Invalid or incompatible state requires the adapter's attention.
    Permanent,
}

/// Owner-local transport plus timer and domain error policy.
pub trait Host: rest::Io {
    /// Sleep until an absolute deadline under the same cancellation scope.
    fn sleep<'a>(
        &'a self,
        until: Instant,
        scope: &'a Self::Scope,
    ) -> Operation<'a, (), Self::Error>;

    /// Classify without erasing cancellation or authentication rejection.
    fn classify(&self, error: Self::Error) -> FailureClass;
}

/// Owned credential lease whose bytes remain valid during a TLS checkout.
pub trait Identity {
    /// Borrow certificate/key bytes with their validated expiration.
    fn identity(&self) -> rest::Identity<'_>;
}

/// Policy for recovery after missing or rejected client identity.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RejectionPolicy {
    /// Never send a token and require a valid client identity.
    IdentityOnly,
    /// Retry once on a fresh server-authenticated connection with a fresh token.
    TokenFallback,
}

/// Authentication and wire-error validation supplied by the application adapter.
pub trait Credentials<H: Host> {
    /// Owner-local identity snapshot, not required to be Send.
    type Identity: Identity;

    /// Token owner should erase its bytes on drop.
    type Token: AsRef<str>;

    /// Snapshot an optional currently usable TLS identity.
    fn identity(&self) -> Result<Option<Self::Identity>, H::Error>;

    /// Select whether token recovery is permitted for this feed.
    fn policy(&self) -> RejectionPolicy;

    /// Read a fresh optional token after server-authenticated TLS is established.
    fn token<'a>(&'a self, scope: &'a H::Scope) -> Operation<'a, Option<Self::Token>, H::Error>;

    /// Notify the adapter that identity authentication was rejected.
    fn rejected(&self);

    /// Validate every response, including structured failure code/status agreement.
    /// Return an error for non-success status; do not trust HTTP status alone.
    fn validate_response(&self, response: &rest::Response) -> Result<(), H::Error>;
}

/// Failed fetch plus a validated server backoff hint.
#[derive(Clone, Copy, Debug)]
pub struct FetchError<E> {
    /// Original domain or transport error.
    pub error: E,

    /// Validated Retry-After, used only with retry-classified failures.
    pub retry_after: Option<Duration>,
}

impl<E> From<E> for FetchError<E> {
    /// Local failures carry no remote retry hint.
    fn from(error: E) -> Self {
        Self {
            error,
            retry_after: None,
        }
    }
}

/// Scoped GET source. Implementations must enforce the response byte bound.
pub trait Source<H: Host> {
    /// Perform a GET with an optional canonical delta-base digest.
    fn get<'a>(
        &'a self,
        path: &'a str,
        digest: Option<&'a str>,
        limit: usize,
        scope: &'a H::Scope,
    ) -> Operation<'a, rest::Response, FetchError<H::Error>>;

    /// Release idle transport state during shutdown.
    fn close(&self);
}

/// REST client with application-owned authentication and structured error checks.
pub struct Client<H: Host, A> {
    transport: rest::Transport<H>,

    credentials: A,

    digest_header: String,
}

impl<H: Host, A: Credentials<H>> Client<H, A> {
    /// Bind an existing transport to credential policy and a delta header name.
    pub fn new(transport: rest::Transport<H>, credentials: A, digest_header: String) -> Self {
        Self {
            transport,
            credentials,
            digest_header,
        }
    }

    /// Validate structured errors before using Retry-After or rejection policy.
    fn validate(&self, response: rest::Response) -> Result<rest::Response, FetchError<H::Error>> {
        match self.credentials.validate_response(&response) {
            Ok(()) => Ok(response),
            Err(error) => Err(FetchError {
                error,
                retry_after: if matches!(response.status, 429 | 503) {
                    response.retry_after
                } else {
                    None
                },
            }),
        }
    }
}

impl<H: Host, A: Credentials<H>> Source<H> for Client<H, A> {
    /// Prefer identity authentication; token fallback gets a fresh TLS connection.
    fn get<'a>(
        &'a self,
        path: &'a str,
        digest: Option<&'a str>,
        limit: usize,
        scope: &'a H::Scope,
    ) -> Operation<'a, rest::Response, FetchError<H::Error>> {
        Box::pin(async move {
            let io = self.transport.io()?;
            let identity = self.credentials.identity()?;
            if let Some(identity) = identity {
                let result = async {
                    let connection = self
                        .transport
                        .connect(Some(identity.identity()), scope)
                        .await?;
                    let response = connection
                        .request(
                            rest::Request {
                                method: rest::Method::Get,
                                path,
                                bearer: None,
                                header: digest.map(|digest| (self.digest_header.as_str(), digest)),
                                body: &[],
                                limit,
                            },
                            scope,
                        )
                        .await?;
                    self.validate(response)
                }
                .await;
                match result {
                    Err(failure) if io.classify(failure.error) == FailureClass::Rejected => {
                        self.credentials.rejected();
                        self.transport.close_idle();
                        if self.credentials.policy() == RejectionPolicy::IdentityOnly {
                            return Err(failure);
                        }
                    }
                    other => return other,
                }
            } else if self.credentials.policy() == RejectionPolicy::IdentityOnly {
                return Err(H::Error::from(rest::Error::Unauthorized).into());
            }
            // No token leaves this owner until CA and server name verification.
            let connection = self.transport.connect(None, scope).await?;
            let token = self.credentials.token(scope).await?;
            let token = token.ok_or_else(|| H::Error::from(rest::Error::Unauthorized))?;
            let response = connection
                .request(
                    rest::Request {
                        method: rest::Method::Get,
                        path,
                        bearer: Some(token.as_ref()),
                        header: digest.map(|digest| (self.digest_header.as_str(), digest)),
                        body: &[],
                        limit,
                    },
                    scope,
                )
                .await?;
            self.validate(response)
        })
    }

    /// Drop the idle TLS connection without touching other feeds.
    fn close(&self) {
        self.transport.close_idle();
    }
}

/// A long-poll route and its last complete document, including unaccepted state.
pub struct Feed<C: Codec> {
    codec: C,

    path: String,

    limit: usize,

    last: RefCell<Option<Arc<C::Document>>>,
}

impl<C: Codec> Feed<C> {
    /// Configure a route without a query and a hard successful-body byte limit.
    pub fn new(codec: C, path: String, limit: usize) -> Result<Self, C::Error> {
        if !path.starts_with('/') || path.contains(['?', '#']) || limit == 0 {
            return Err(rest::Error::InvalidConfiguration.into());
        }
        Ok(Self {
            codec,
            path,
            limit,
            last: RefCell::new(None),
        })
    }

    /// Inspect the codec used by application adapters.
    pub fn codec(&self) -> &C {
        &self.codec
    }

    /// Lease the latest decoded document, which is not necessarily accepted.
    pub fn last(&self) -> Option<Arc<C::Document>> {
        self.last.borrow().clone()
    }

    /// Restore a known base, for example an already accepted startup document.
    pub fn restore(&self, document: Option<Arc<C::Document>>) {
        *self.last.borrow_mut() = document;
    }

    /// Fetch after the retained cursor, then try full decode, delta, and at most
    /// one cursor-free full retry under the original scope. Errors retain the base.
    pub async fn fetch<H, S>(
        &self,
        source: &S,
        scope: &H::Scope,
    ) -> Result<Option<Arc<C::Document>>, FetchError<C::Error>>
    where
        H: Host<Error = C::Error>,
        S: Source<H>,
    {
        scope.check()?;
        let base = self.last();
        let path = base.as_ref().map_or_else(
            || self.path.clone(),
            |d| format!("{}?after={}", self.path, self.codec.version(d)),
        );
        let digest = base.as_ref().map(|d| self.codec.digest(d)).transpose()?;
        let response = source
            .get(&path, digest.as_deref(), self.limit, scope)
            .await?;
        scope.check()?;
        if response.status == 204 {
            if base.is_none() || !response.body.is_empty() {
                return Err(C::Error::from(rest::Error::InvalidRequest).into());
            }
            return Ok(None);
        }
        self.check_response(&response)?;
        let decoded = self.codec.decode(&response.body).or_else(|full_error| {
            base.as_ref().map_or(Err(full_error), |base| {
                self.codec.delta(base, &response.body)
            })
        });
        let document = match decoded {
            Ok(document) => document,
            Err(_) => {
                let response = source.get(&self.path, None, self.limit, scope).await?;
                self.check_response(&response)?;
                self.codec.decode(&response.body)?
            }
        };
        scope.check()?;
        // Even a new version must have a usable digest before becoming a base.
        // Otherwise one invalid candidate poisons every subsequent request.
        let document_digest = self.codec.digest(&document)?;
        if let Some(base) = &base
            && (self.codec.version(&document) < self.codec.version(base)
                || self.codec.version(&document) == self.codec.version(base)
                    && Some(document_digest.as_str()) != digest.as_deref())
        {
            return Err(C::Error::from(Error::Replay).into());
        }
        let document = Arc::new(document);
        self.restore(Some(document.clone()));
        Ok(Some(document))
    }

    /// Enforce success framing even when a custom source is used.
    fn check_response(&self, response: &rest::Response) -> Result<(), C::Error> {
        if response.status != 200 || response.body.len() > self.limit {
            return Err(rest::Error::InvalidRequest.into());
        }
        Ok(())
    }
}

/// Finite CPU-only preparation. Only this closure, not the target, crosses threads.
/// It must terminate independently of the owner: no owner callbacks, I/O, or waits
/// for owner-held locks. Driver destruction joins it rather than detaching work.
pub type Preparation<P, E> = Box<dyn FnOnce() -> Result<P, E> + Send + 'static>;

/// Domain validation and install policy. The target can retain owner-local Rc.
pub trait Target<C: Codec> {
    /// Prepared immutable state returned from the background thread.
    type Prepared: Send + 'static;

    /// Capture only thread-safe preparation inputs. Do not perform heavy work here.
    fn prepare(
        &self,
        document: Arc<C::Document>,
    ) -> Result<Preparation<Self::Prepared, C::Error>, C::Error>;

    /// Validate and atomically install prepared state. Every error must leave live
    /// resources unchanged; use rollback guards for fallible staging. Retryable
    /// failure retains prepared state. The driver cannot undo adapter side effects.
    fn install(&self, document: &C::Document, prepared: &Self::Prepared) -> Result<(), C::Error>;
}

/// Scheduling bounds supplied by the caller, not domain constants.
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

/// Coherent owner diagnostics. Pending is receipt, never acceptance.
#[derive(Clone, Copy, Debug)]
pub struct Status<V, E> {
    /// Last successfully installed version.
    pub accepted: Option<V>,

    /// Latest unaccepted version, including queued or preparing documents.
    pub pending: Option<V>,

    /// Last non-cancellation error from this feed.
    pub error: Option<E>,

    /// Next network attempt time, including any rejected-candidate retry floor.
    pub next_fetch: Instant,

    /// Whether submissions are permanently closed.
    pub stopped: bool,
}

/// A single-owner feed driver. Dropping a turn does not discard preparation work.
/// Call shutdown to fence background work asynchronously with a bounded scope.
/// Drop joins any remaining finite CPU closure, preserving owned completion even
/// after canceled shutdown. Rust cannot impose a deadline on arbitrary closures.
pub struct Sync<H: Host, C: Codec<Error = H::Error>, S, T: Target<C>> {
    host: Rc<H>,

    feed: Feed<C>,

    source: S,

    target: T,

    schedule: Schedule,

    state: Progress<C, T>,

    stopped: Cell<bool>,
}

/// Owner-local state separated from the independently borrowed network future.
struct Progress<C: Codec, T: Target<C>> {
    accepted: Option<Arc<C::Document>>,

    queued: Option<Arc<C::Document>>,

    job: Option<Job<C, T>>,

    prepared: Option<(Arc<C::Document>, T::Prepared)>,

    error: Option<C::Error>,

    failures: u32,

    rejected: Option<Rejection<C::Version>>,

    next_fetch: Instant,

    next_prepare: Instant,
}

/// A rejected candidate version has its own retry history, independent of GETs.
/// Corrected content at the same version may retry and clears history on install.
struct Rejection<V> {
    version: V,

    failures: u32,

    until: Instant,
}

/// One owned background job retained until its completion fence.
struct Job<C: Codec, T: Target<C>> {
    document: Arc<C::Document>,

    handle: JoinHandle<Result<T::Prepared, C::Error>>,
}

impl<H: Host, C: Codec<Error = H::Error>, S, T: Target<C>> Drop for Sync<H, C, S, T> {
    /// Fence preparation before dropping its target or any other owner resources.
    fn drop(&mut self) {
        if let Some(job) = self.state.job.take() {
            // A worker panic is an error, not a second panic during owner unwind.
            let _ = job.handle.join();
        }
    }
}

impl<H: Host, C: Codec<Error = H::Error>, S: Source<H>, T: Target<C>> Sync<H, C, S, T>
where
    H::Error: From<Error>,
{
    /// Create a driver; restore startup acceptance explicitly with `seed`.
    pub fn new(
        host: Rc<H>,
        feed: Feed<C>,
        source: S,
        target: T,
        schedule: Schedule,
    ) -> Result<Self, H::Error> {
        if schedule.turn.is_zero()
            || schedule.tick.is_zero()
            || schedule.retry_min.is_zero()
            || schedule.retry_max < schedule.retry_min
        {
            return Err(rest::Error::InvalidConfiguration.into());
        }
        let now = uring_runtime::environment::now();
        if [schedule.turn, schedule.tick, schedule.retry_max]
            .into_iter()
            .any(|delay| now.checked_add(delay).is_none())
        {
            return Err(rest::Error::InvalidConfiguration.into());
        }
        Ok(Self {
            host,
            feed,
            source,
            target,
            schedule,
            state: Progress {
                accepted: None,
                queued: None,
                job: None,
                prepared: None,
                error: None,
                failures: 0,
                rejected: None,
                next_fetch: now,
                next_prepare: now,
            },
            stopped: Cell::new(false),
        })
    }

    /// Seed a document already installed by startup, before any work is submitted.
    pub fn seed(&mut self, document: Arc<C::Document>) -> Result<(), H::Error> {
        if self.state.job.is_some()
            || self.state.queued.is_some()
            || self.state.prepared.is_some()
            || self.state.accepted.is_some()
            || self.stopped.get()
        {
            return Err(Error::Pending.into());
        }
        self.feed.restore(Some(document.clone()));
        self.state.accepted = Some(document);
        Ok(())
    }

    /// Inspect acceptance, pending receipt, and retry state without network I/O.
    pub fn status(&self) -> Status<C::Version, H::Error> {
        let accepted = self
            .state
            .accepted
            .as_ref()
            .map(|d| self.feed.codec.version(d));
        let pending = self
            .feed
            .last()
            .map(|d| self.feed.codec.version(&d))
            .filter(|v| Some(*v) != accepted);
        Status {
            accepted,
            pending,
            error: self.state.error,
            next_fetch: self.state.fetch_at(),
            stopped: self.stopped.get(),
        }
    }

    /// Run one scoped fetch while local installation advances on a real timer.
    /// Dropping this future closes its checked-out socket, not the owned CPU job.
    /// Rejected versions back off independently of successful GET receipts; a
    /// later fetch may supersede them without retaining a rejected delta base.
    pub async fn turn(&mut self, scope: &H::Scope) -> Result<(), H::Error> {
        if self.stopped.get() {
            return Err(uring_runtime::Error::Cancelled.into());
        }
        let now = uring_runtime::environment::now();
        let scope = scope.narrowed(now.checked_add(self.schedule.turn).unwrap_or(now));
        scope.check()?;
        let next_fetch = self.state.fetch_at();
        let network = async {
            self.host.sleep(next_fetch, &scope).await?;
            self.feed.fetch::<H, S>(&self.source, &scope).await
        };
        futures::pin_mut!(network);
        loop {
            scope.check()?;
            self.state
                .advance(&self.feed, &self.target, self.host.as_ref(), self.schedule)?;
            let now = uring_runtime::environment::now();
            let tick = self
                .host
                .sleep(now.checked_add(self.schedule.tick).unwrap_or(now), &scope);
            match futures::future::select(network.as_mut(), tick).await {
                futures::future::Either::Left((result, _)) => match result {
                    Ok(document) => {
                        self.state.failures = 0;
                        self.state.next_fetch =
                            uring_runtime::environment::now() + self.schedule.tick;
                        if let Some(document) = document
                            && self.state.accepted.as_ref().is_none_or(|old| {
                                self.feed.codec.version(&document) > self.feed.codec.version(old)
                            })
                            && self.state.job.as_ref().is_none_or(|job| {
                                self.feed.codec.version(&document)
                                    > self.feed.codec.version(&job.document)
                            })
                            && self.state.prepared.as_ref().is_none_or(|(old, _)| {
                                self.feed.codec.version(&document) > self.feed.codec.version(old)
                            })
                        {
                            self.state.queued = Some(document);
                        }
                        self.state.advance(
                            &self.feed,
                            &self.target,
                            self.host.as_ref(),
                            self.schedule,
                        )?;
                        return Ok(());
                    }
                    Err(failure) => {
                        if self.host.classify(failure.error) != FailureClass::Cancelled {
                            self.state.error = Some(failure.error);
                            self.state.failures = self.state.failures.saturating_add(1);
                            let delay = retry_delay(self.schedule, self.state.failures);
                            let delay = if self.host.classify(failure.error) == FailureClass::Retry
                            {
                                delay.max(failure.retry_after.unwrap_or_default())
                            } else {
                                delay
                            };
                            let now = uring_runtime::environment::now();
                            self.state.next_fetch =
                                now.checked_add(delay).unwrap_or(scope.deadline());
                        }
                        return Err(failure.error);
                    }
                },
                futures::future::Either::Right((result, _)) => result?,
            }
        }
    }

    /// Stop fetches and boundedly wait for the sole CPU job. On timeout or caller
    /// cancellation the job stays owned; invoke shutdown again with a fresh scope.
    pub async fn shutdown(&mut self, scope: &H::Scope) -> Result<(), H::Error> {
        self.stopped.set(true);
        self.source.close();
        self.state.queued.take();
        self.state.prepared.take();
        self.feed.restore(self.state.accepted.clone());
        let now = uring_runtime::environment::now();
        let scope = scope.narrowed(now.checked_add(self.schedule.turn).unwrap_or(now));
        while self
            .state
            .job
            .as_ref()
            .is_some_and(|job| !job.handle.is_finished())
        {
            scope.check()?;
            self.host
                .sleep(
                    uring_runtime::environment::now() + self.schedule.tick,
                    &scope,
                )
                .await?;
        }
        if let Some(job) = self.state.job.take() {
            job.handle
                .join()
                .map_err(|_| H::Error::from(Error::Internal))?
                .ok();
        }
        Ok(())
    }
}

impl<C: Codec, T: Target<C>> Progress<C, T> {
    /// Enforce both transport backoff and the independent candidate retry floor.
    fn fetch_at(&self) -> Instant {
        self.rejected.as_ref().map_or(self.next_fetch, |rejected| {
            self.next_fetch.max(rejected.until)
        })
    }

    /// Reject without advancing the delta base or resetting failure history on a
    /// successful receipt. Keep one version's history, bounded regardless of input.
    fn reject(&mut self, version: C::Version, feed: &Feed<C>, schedule: Schedule, now: Instant) {
        let failures = self
            .rejected
            .as_ref()
            .filter(|old| old.version == version)
            .map_or(1, |old| old.failures.saturating_add(1));
        self.rejected = Some(Rejection {
            version,
            failures,
            until: now
                .checked_add(retry_delay(schedule, failures))
                .unwrap_or(now),
        });
        feed.restore(self.accepted.clone());
    }

    /// Reap at most one job, install at most once, and submit at most one job.
    fn advance<H: Host<Error = C::Error>>(
        &mut self,
        feed: &Feed<C>,
        target: &T,
        host: &H,
        schedule: Schedule,
    ) -> Result<(), C::Error> {
        let now = uring_runtime::environment::now();
        if self
            .job
            .as_ref()
            .is_some_and(|job| job.handle.is_finished())
        {
            let job = self.job.take().expect("finished job");
            let result = job
                .handle
                .join()
                .unwrap_or_else(|_| Err(Error::Internal.into()));
            let superseded = self
                .queued
                .as_ref()
                .is_some_and(|d| feed.codec.version(d) > feed.codec.version(&job.document));
            if !superseded {
                match result {
                    Ok(prepared) => self.prepared = Some((job.document, prepared)),
                    Err(error) => {
                        if host.classify(error) != FailureClass::Cancelled {
                            self.error = Some(error);
                        }
                        if host.classify(error) == FailureClass::Retry
                            || host.classify(error) == FailureClass::Cancelled
                        {
                            self.queued = Some(job.document);
                            self.next_prepare =
                                now.checked_add(jitter(schedule.retry_min)).unwrap_or(now);
                        } else {
                            self.reject(feed.codec.version(&job.document), feed, schedule, now);
                            return Err(error);
                        }
                    }
                }
            }
        }
        if self.prepared.as_ref().is_some_and(|(d, _)| {
            self.queued
                .as_ref()
                .is_some_and(|q| feed.codec.version(q) > feed.codec.version(d))
        }) {
            self.prepared.take();
        }
        if let Some((document, prepared)) = &self.prepared {
            match target.install(document, prepared) {
                Ok(()) => {
                    self.accepted = Some(document.clone());
                    self.prepared.take();
                    self.error = None;
                    self.rejected = None;
                }
                Err(error) => {
                    if host.classify(error) == FailureClass::Cancelled {
                        return Err(error);
                    }
                    self.error = Some(error);
                    if host.classify(error) != FailureClass::Retry {
                        let version = feed.codec.version(document);
                        self.prepared.take();
                        self.reject(version, feed, schedule, now);
                        return Err(error);
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
                .is_some_and(|old| feed.codec.version(old) >= feed.codec.version(&document))
            {
                return Ok(());
            }
            let work = match target.prepare(document.clone()) {
                Ok(work) => work,
                Err(error) => {
                    match host.classify(error) {
                        FailureClass::Retry | FailureClass::Cancelled => {
                            self.queued = Some(document);
                            self.next_prepare =
                                now.checked_add(jitter(schedule.retry_min)).unwrap_or(now);
                        }
                        FailureClass::Rejected | FailureClass::Permanent => {
                            self.reject(feed.codec.version(&document), feed, schedule, now);
                        }
                    }
                    if host.classify(error) != FailureClass::Cancelled {
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
                    self.queued = Some(document);
                    self.next_prepare = now.checked_add(jitter(schedule.retry_min)).unwrap_or(now);
                    return Err(uring_runtime::Error::Io.into());
                }
            }
        }
        Ok(())
    }
}

/// Capped exponential retry shared by independent network and candidate counters.
fn retry_delay(schedule: Schedule, failures: u32) -> Duration {
    jitter(
        schedule
            .retry_min
            .saturating_mul(1 << failures.saturating_sub(1).min(20))
            .min(schedule.retry_max),
    )
}

/// Equal jitter in [half, full] of the capped exponential delay. Remote hints are
/// applied afterward as a floor, so jitter never shortens Retry-After. If entropy
/// fails, retain the full delay rather than spinning or discarding the real error.
fn jitter(delay: Duration) -> Duration {
    let mut bytes = [0; 4];
    if uring_runtime::environment::fill_random(&mut bytes).is_err() {
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

#[cfg(test)]
mod tests {
    //! Pure retry arithmetic checks, including duration limits.

    use super::*;

    /// Both endpoints and very large schedules stay inside the promised bounds.
    #[test]
    fn jitter_is_bounded_inclusive_and_not_constant() {
        for delay in [
            Duration::from_nanos(1),
            Duration::from_secs(10),
            Duration::MAX,
        ] {
            assert_eq!(
                jitter_sample(delay, 0),
                (delay / 2).max(Duration::from_nanos(1))
            );
            assert_eq!(jitter_sample(delay, u32::MAX), delay);
            let middle = jitter_sample(delay, u32::MAX / 2);
            assert!(middle >= delay / 2 && middle <= delay);
        }
        assert_ne!(
            jitter_sample(Duration::from_secs(1), 1),
            jitter_sample(Duration::from_secs(1), u32::MAX)
        );
    }
}
