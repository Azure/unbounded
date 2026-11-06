//! Racer authentication and renewal policy around two independent long polls.

use super::poll::{
    self, Doc, Document, Fetch, FetchError, Install, Poll, Preparation, Prepared, Rejection,
    Schedule,
};
use super::publication::PublicationTarget;
use super::rails::RailJournal;
use super::{ControlEndpoint, ReactorControlIo, transient};
use crate::control::rest;
use crate::enrollment::{Enrollment, LocalSigningIdentity};
use crate::error::{Error, Operation, Result};
use crate::runtime::RequestScope;
use racer_crypto::identity::{BundleInstaller, Keyring};
use racer_wire::control as wire;
use std::{
    cell::{Cell, RefCell},
    ops::{Deref, DerefMut},
    rc::Rc,
    time::{Duration, Instant},
};
use zeroize::Zeroizing;

/// Shortest serving renewal recheck, the former fixed cadence.
const RENEWAL_MIN_WAIT: Duration = Duration::from_secs(1);

/// Longest serving renewal sleep, bounding wall-clock steps.
const RENEWAL_MAX_WAIT: Duration = Duration::from_secs(30);

/// Owner-local session. Only the two polls own cursors, retry, and preparation jobs.
pub struct Session {
    block_devices: RefCell<Option<String>>,
    rails: RailJournal,

    shares: std::num::NonZeroU32,

    /// Managed-mode report sink and the last rails it accepted.
    report: RefCell<Option<(super::Report, Option<Vec<wire::RailMapping>>)>>,

    /// Rails persisted before the node serves peers, reported by
    /// [`Session::mark_serving`]. The primary admits a new member only once
    /// its Pod reports, so the report must not precede the peer listener.
    unreported: RefCell<Option<Vec<wire::RailMapping>>>,

    /// Set once the peer listener is open.
    serving: std::cell::Cell<bool>,

    endpoint: ControlEndpoint,

    enrollment_transport: rest::Transport<ReactorControlIo>,

    pub(super) auth: Rc<Authentication>,

    pub(super) snapshots: Rc<PublicationTarget>,

    publications: RefCell<Option<Poll>>,

    keyrings: RefCell<Option<Poll>>,

    received: Rc<Cell<Option<(u64, u64)>>>,

    publication_next: Cell<Option<Instant>>,

    publication_fetch_started: Rc<Cell<bool>>,

    keyring_error: Rc<Cell<Option<Error>>>,

    renewal_error: Cell<Option<Error>>,

    renewal_next: Cell<Option<Instant>>,

    issuance_failures: Cell<u32>,

    issuance_retry_after: Cell<Option<Duration>>,

    busy: Cell<bool>,

    enrollment_busy: Cell<bool>,
}

/// Shared credential and node-binding authority; no feed progress lives here.
pub(super) struct Authentication {
    pub(super) enrollment: Rc<Enrollment<Rc<ReactorControlIo>>>,

    pub(super) keys: RefCell<Rc<Keyring>>,

    pub(super) secrets: BundleInstaller,

    pub(super) identity: RefCell<Option<LocalSigningIdentity>>,

    pub(super) started: Cell<bool>,

    stopped: Cell<bool>,

    binding_check: Cell<bool>,

    restart_required: Cell<bool>,

    startup_bundle: RefCell<Option<wire::KeyringBundle>>,
}

impl Authentication {
    /// Reject every new submission after stop or authenticated node replacement.
    fn check(&self) -> Result<()> {
        if self.stopped.get() {
            return Err(Error::Canceled);
        }
        if self.restart_required.get() {
            return Err(Error::NodeIdentityChanged);
        }
        Ok(())
    }
}

/// Borrow a driver by ownership so diagnostics never borrow across a long poll.
/// Cancellation restores the same Sync, including its owned background job.
struct Turn<'a, T> {
    slot: &'a RefCell<Option<T>>,

    driver: Option<T>,
}

impl<'a, T> Turn<'a, T> {
    /// There is exactly one active owner per feed, independently of the other feed.
    fn take(slot: &'a RefCell<Option<T>>) -> Result<Self> {
        let driver = slot.borrow_mut().take().ok_or(Error::Overloaded)?;
        Ok(Self {
            slot,
            driver: Some(driver),
        })
    }
}

impl<T> Deref for Turn<'_, T> {
    type Target = T;

    fn deref(&self) -> &T {
        self.driver.as_ref().expect("owned driver")
    }
}

impl<T> DerefMut for Turn<'_, T> {
    fn deref_mut(&mut self) -> &mut T {
        self.driver.as_mut().expect("owned driver")
    }
}

impl<T> Drop for Turn<'_, T> {
    fn drop(&mut self) {
        *self.slot.borrow_mut() = self.driver.take();
    }
}

/// Authenticated REST client for one document. Identity first; token
/// fallback only where the document's policy allows it.
struct Client {
    transport: rest::Transport<ReactorControlIo>,

    auth: Rc<Authentication>,

    doc: Doc,

    /// Set on request entry so renewal completion never cancels an active GET.
    started: Option<Rc<Cell<bool>>>,
}

impl Client {
    /// Snapshot a currently usable TLS identity.
    fn identity(&self) -> Result<Option<LocalSigningIdentity>> {
        self.auth.check()?;
        Ok(self
            .auth
            .identity
            .borrow()
            .clone()
            .filter(|i| i.valid_at(self.auth.enrollment.wall_now())))
    }

    /// Validate structured errors before using Retry-After or rejection policy.
    fn validate(response: rest::Response) -> std::result::Result<rest::Response, FetchError> {
        match poll::validate_response(&response) {
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

    async fn request(
        &self,
        connection: rest::Connection<ReactorControlIo>,
        path: &str,
        bearer: Option<&str>,
        digest: Option<&str>,
        limit: usize,
        scope: &RequestScope,
    ) -> std::result::Result<rest::Response, FetchError> {
        let response = connection
            .request(
                rest::Request {
                    method: rest::Method::Get,
                    path,
                    bearer,
                    header: digest.map(|digest| (poll::DIGEST_HEADER, digest)),
                    body: &[],
                    limit,
                },
                scope,
            )
            .await?;
        Self::validate(response)
    }
}

impl Fetch for Client {
    /// Prefer identity authentication; token fallback gets a fresh TLS connection.
    fn get<'a>(
        &'a self,
        path: &'a str,
        digest: Option<&'a str>,
        limit: usize,
        scope: &'a RequestScope,
    ) -> racer_io::Operation<'a, rest::Response, FetchError> {
        if let Some(started) = &self.started {
            started.set(true);
        }
        Box::pin(async move {
            let policy = self.doc.rejection();
            if let Some(identity) = self.identity()? {
                let result = async {
                    let connection = self
                        .transport
                        .connect(
                            Some(rest::Identity {
                                certificate_chain: identity.certificate_chain(),
                                private_key: identity.private_key_der(),
                                expires: identity.expires_at(),
                            }),
                            scope,
                        )
                        .await?;
                    self.request(connection, path, None, digest, limit, scope)
                        .await
                }
                .await;
                match result {
                    Err(failure)
                        if poll::classify(failure.error) == poll::FailureClass::Rejected =>
                    {
                        self.auth.binding_check.set(true);
                        self.transport.close_idle();
                        if policy == Rejection::IdentityOnly {
                            return Err(failure);
                        }
                    }
                    other => return other,
                }
            } else if policy == Rejection::IdentityOnly {
                return Err(Error::Unauthorized.into());
            }
            // No token leaves this owner until CA and server name verification.
            let connection = self.transport.connect(None, scope).await?;
            self.auth.check()?;
            let token: Zeroizing<String> = self.auth.enrollment.read_token(scope).await?;
            self.request(connection, path, Some(token.as_str()), digest, limit, scope)
                .await
        })
    }

    /// Drop the idle TLS connection without touching the other poll.
    fn close(&self) {
        self.transport.close_idle();
    }

    /// Move the next request away from the seat that served the last response.
    fn demote(&self) {
        self.transport.demote();
    }
}

/// Retain the newest receipt for diagnostics, including equal-sequence replacements.
fn observe_receipt(received: &Cell<Option<(u64, u64)>>, document: &wire::Publication) {
    if received
        .get()
        .is_none_or(|old| document.sequence.0 >= old.0)
    {
        received.set(Some((document.sequence.0, document.membership_version.0)));
    }
}

/// Keep domain installation local while delegating CPU projection to PublicationTarget.
struct PublicationInstall {
    target: Rc<PublicationTarget>,

    received: Rc<Cell<Option<(u64, u64)>>>,

    auth: Rc<Authentication>,
}

impl Install for PublicationInstall {
    fn prepare(&self, document: Document) -> Result<Preparation> {
        self.auth.check()?;
        self.received(&document);
        self.target.prepare(document)
    }

    fn install(&self, document: &Document, prepared: &Prepared) -> Result<()> {
        self.auth.check()?;
        self.target.install(document, prepared)
    }

    fn received(&self, document: &Document) {
        if let Document::Publication(document) = document {
            observe_receipt(&self.received, document);
        }
    }
}

/// Key acceptance is separate from publication acceptance and identity renewal.
struct BundleInstall {
    auth: Rc<Authentication>,

    error: Rc<Cell<Option<Error>>>,
}

impl Install for BundleInstall {
    fn prepare(&self, document: Document) -> Result<Preparation> {
        self.auth.check()?;
        let Document::Keyring(document) = document else {
            return Err(Error::InvalidRequest);
        };
        if &document.cluster != self.auth.enrollment.cluster() {
            return Err(Error::Unauthorized);
        }
        Ok(Box::new(move || {
            super::verifier(&document.peer_trust_roots)?;
            Ok(Prepared::Keyring(document))
        }))
    }

    fn install(&self, _: &Document, prepared: &Prepared) -> Result<()> {
        let result = (|| {
            let Prepared::Keyring(prepared) = prepared else {
                return Err(Error::InvalidRequest);
            };
            self.auth.check()?;
            if self.auth.started.get() {
                self.auth.secrets.install((**prepared).clone())?;
            } else if self
                .auth
                .keys
                .borrow()
                .generation()?
                .is_some_and(|accepted| prepared.generation.0 < accepted)
            {
                // Keys are shared node-wide, so an earlier session may already
                // have installed a newer bundle. A fresh startup poll has no
                // cursor, so a seat restarted during a rollout can still serve
                // it an older generation. Refuse it here, before it becomes the
                // startup bundle that `activate_identity` would fail to install.
                return Err(Error::Replay);
            }
            self.auth
                .enrollment
                .set_peer_trust_roots(prepared.peer_trust_roots.clone())?;
            if !self.auth.started.get() {
                *self.auth.startup_bundle.borrow_mut() = Some((**prepared).clone());
            }
            Ok(())
        })();
        self.error.set(result.err());
        result
    }
}

impl Session {
    /// Compose without issuing I/O. Attach the serving reactor before starting.
    pub fn new(
        endpoint: ControlEndpoint,
        enrollment: Rc<Enrollment<Rc<ReactorControlIo>>>,
        rails: RailJournal,
        shares: std::num::NonZeroU32,
        keys: Rc<Keyring>,
        secrets: BundleInstaller,
        snapshots: Rc<PublicationTarget>,
    ) -> Self {
        let enrollment_transport = transport(&endpoint);
        Self {
            rails,
            shares,
            report: RefCell::new(None),
            unreported: RefCell::new(None),
            serving: std::cell::Cell::new(false),
            endpoint,
            enrollment_transport,
            block_devices: RefCell::new(None),
            auth: Rc::new(Authentication {
                enrollment,
                keys: RefCell::new(keys),
                secrets,
                identity: RefCell::new(None),
                started: Cell::new(false),
                stopped: Cell::new(false),
                binding_check: Cell::new(false),
                restart_required: Cell::new(false),
                startup_bundle: RefCell::new(None),
            }),
            snapshots,
            publications: RefCell::new(None),
            keyrings: RefCell::new(None),
            received: Rc::new(Cell::new(None)),
            publication_next: Cell::new(None),
            publication_fetch_started: Rc::new(Cell::new(false)),
            keyring_error: Rc::new(Cell::new(None)),
            renewal_error: Cell::new(None),
            renewal_next: Cell::new(None),
            issuance_failures: Cell::new(0),
            issuance_retry_after: Cell::new(None),
            busy: Cell::new(false),
            enrollment_busy: Cell::new(false),
        }
    }

    /// Initialize exactly two independent drivers on the same explicit reactor.
    pub fn attach_io(&self, io: Rc<ReactorControlIo>) -> Result<()> {
        if self.publications.borrow().is_some() || self.keyrings.borrow().is_some() {
            return Err(Error::InvalidConfiguration);
        }
        self.enrollment_transport.attach_io(io.clone());
        let client = |doc, started| {
            let transport = transport(&self.endpoint);
            transport.attach_io(io.clone());
            Box::new(Client {
                transport,
                auth: self.auth.clone(),
                doc,
                started,
            })
        };
        *self.publications.borrow_mut() = Some(Poll::new(
            io.clone(),
            Doc::Publication,
            client(
                Doc::Publication,
                Some(self.publication_fetch_started.clone()),
            ),
            Box::new(PublicationInstall {
                target: self.snapshots.clone(),
                received: self.received.clone(),
                auth: self.auth.clone(),
            }),
            Schedule::control(),
        )?);
        *self.keyrings.borrow_mut() = Some(Poll::new(
            io.clone(),
            Doc::Keyring,
            client(Doc::Keyring, None),
            Box::new(BundleInstall {
                auth: self.auth.clone(),
                error: self.keyring_error.clone(),
            }),
            Schedule::control(),
        )?);
        Ok(())
    }

    /// Report shares and rails to managed cluster control whenever rails change.
    pub fn set_report(&self, report: super::Report) {
        *self.report.borrow_mut() = Some((report, None));
    }

    /// The peer listener is open: report the rails held back so far. A
    /// managed primary admits a new member only after this report ("Joining
    /// members" in designs/racer-core-simulation.md).
    pub fn mark_serving(&self) {
        self.serving.set(true);
        let held = self.unreported.borrow_mut().take();
        if let Some(nics) = held {
            self.report_rails(&nics);
        }
    }

    /// Forward persisted rails once per distinct value; the sink only enqueues.
    /// Before [`Session::mark_serving`] the newest rails are held instead.
    fn report_rails(&self, nics: &[wire::RailMapping]) {
        if !self.serving.get() {
            *self.unreported.borrow_mut() = Some(nics.to_vec());
            return;
        }
        let mut slot = self.report.borrow_mut();
        if let Some((report, last)) = slot.as_mut()
            && last.as_deref() != Some(nics)
        {
            report(self.shares.get(), nics.to_vec());
            *last = Some(nics.to_vec());
        }
    }

    /// Local rollout belongs to PublicationTarget, never to the feed mechanism.
    pub(crate) fn attach_cache_publication(&self, lifecycle: Rc<crate::app::CachePublication>) {
        self.snapshots.attach_cache_publication(lifecycle);
    }

    /// Last key-feed failure, independent of renewal and topology.
    pub fn projection_error(&self) -> Option<Error> {
        self.keyring_error.get()
    }

    /// Last issuance failure; accepted identity may still be usable.
    pub fn renewal_error(&self) -> Option<Error> {
        self.renewal_error.get()
    }

    /// Issuance retry deadline, independent of the publication feed's next fetch.
    #[cfg(test)]
    pub(crate) fn renewal_attempt(&self) -> Option<Instant> {
        self.renewal_next.get()
    }

    /// Startup uses issuance retry policy; the publication poll schedules serving polls.
    pub fn next_attempt(&self) -> Option<Instant> {
        if self.auth.started.get() {
            self.publication_next.get()
        } else {
            self.renewal_next.get()
        }
    }

    /// When the serving renewal loop next needs to check `renew_if_due`: the
    /// identity's renewal time or a scheduled retry, at least [`RENEWAL_MIN_WAIT`]
    /// and at most [`RENEWAL_MAX_WAIT`] from `now`. The cap bounds the effect of
    /// wall-clock steps on a monotonic sleep.
    fn renewal_wake(&self, now: Instant) -> Instant {
        let wall = self.auth.enrollment.wall_now();
        let due = self.identity().map_or(Duration::ZERO, |identity| {
            identity
                .renewal_at()
                .duration_since(wall)
                .unwrap_or(Duration::ZERO)
        });
        let retry = self
            .renewal_next
            .get()
            .map(|next| next.saturating_duration_since(now));
        // A due identity waits only on its scheduled retry.
        let wait = match retry {
            Some(retry) if due.is_zero() => retry,
            Some(retry) => due.min(retry),
            None => due,
        };
        now + wait.clamp(RENEWAL_MIN_WAIT, RENEWAL_MAX_WAIT)
    }

    /// Token issuance is not a feed; preserve bounded independent renewal retry.
    fn issuance_backoff(&self, ctx: &racer_io::WorkerCtx) -> Result<Instant> {
        let failures = self.issuance_failures.get().saturating_add(1);
        self.issuance_failures.set(failures);
        let mut random = [0; 8];
        ctx.rng.fill(&mut random).map_err(|_| Error::Io)?;
        let ceiling = (1u64 << failures.saturating_sub(1).min(5)).min(30) * 1000;
        let delay = Duration::from_millis(1000 + u64::from_ne_bytes(random) % (ceiling - 999))
            .max(self.issuance_retry_after.take().unwrap_or_default());
        ctx.now().checked_add(delay).ok_or(Error::InvalidRequest)
    }

    /// Submit a retry-stable CSR after server-authenticated TLS and a fresh token read.
    pub fn enroll<'a>(
        &'a self,
        request: &'a wire::EnrollmentRequest,
        scope: &'a RequestScope,
    ) -> Operation<'a, wire::EnrollmentResponse> {
        Box::pin(async move {
            self.auth.check()?;
            let _busy = super::enter(&self.enrollment_busy)?;
            let body = wire::encode_enrollment_request(request)?;
            let connection = self.enrollment_transport.connect(None, scope).await?;
            let token = self.auth.enrollment.read_token(scope).await?;
            let response = connection
                .request(
                    rest::Request {
                        method: rest::Method::Post,
                        path: wire::BOOTSTRAP_PATH,
                        bearer: Some(&token),
                        header: None,
                        body: &body,
                        limit: wire::MAX_ENROLLMENT_BYTES,
                    },
                    scope,
                )
                .await?;
            if let Err(error) = validate_response(&response) {
                if matches!(error, Error::Unavailable | Error::Overloaded) {
                    self.issuance_retry_after.set(response.retry_after);
                }
                return Err(error);
            }
            if response.status != 200 {
                return Err(Error::InvalidRequest);
            }
            Ok(wire::decode_enrollment_response(&response.body)?)
        })
    }

    /// Authenticate current node binding even when a disk certificate remains valid.
    pub fn start<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, LocalSigningIdentity> {
        Box::pin(async move {
            let _busy = super::enter(&self.busy)?;
            self.auth.check()?;
            scope.check()?;
            if self.auth.started.get() {
                return self.identity().ok_or(Error::Unauthorized);
            }
            if self
                .renewal_next
                .get()
                .is_some_and(|n| n > scope.clock().now())
            {
                return Err(Error::Unavailable);
            }
            let result = self.start_inner(scope).await;
            if result.as_ref().is_err_and(|e| transient(*e)) {
                self.renewal_next.set(Some(
                    self.issuance_backoff(&self.enrollment_transport.io()?.reactor.ctx().clone())?,
                ));
            }
            result
        })
    }

    /// Key feed must accept trust before identity recovery and fresh issuance.
    async fn start_inner(&self, scope: &RequestScope) -> Result<LocalSigningIdentity> {
        while self.auth.startup_bundle.borrow().is_none() {
            let mut turn = self.keyring_startup_turn(scope);
            std::future::poll_fn(|cx| {
                let result = turn.as_mut().poll(cx);
                if self.auth.startup_bundle.borrow().is_some() {
                    std::task::Poll::Ready(Ok(()))
                } else {
                    result
                }
            })
            .await?;
        }
        self.auth.enrollment.load_identity(scope).await?;
        let nics = self.rails.persist(scope).await?;
        self.report_rails(&nics);
        let request = self
            .auth
            .enrollment
            .prepare(nics, self.shares, scope)
            .await?;
        let response = self.enroll(&request, scope).await?;
        let block_devices = response.block_devices.clone();
        let identity = self
            .auth
            .enrollment
            .accept_response(response, scope)
            .await?;
        *self.block_devices.borrow_mut() = block_devices;
        *self.auth.identity.borrow_mut() = Some(identity.clone());
        self.auth.started.set(true);
        self.renewal_next.set(None);
        self.issuance_failures.set(0);
        Ok(identity)
    }

    /// Bind shared key epochs to the authenticated startup node, then activate.
    pub fn bind_keyring(&self, keys: Rc<Keyring>) -> Result<()> {
        self.auth.check()?;
        let identity = self.identity().ok_or(Error::Unauthorized)?;
        if keys.node() != identity.node() || keys.cluster() != identity.cluster() {
            return Err(Error::Unauthorized);
        }
        self.auth.secrets.bind_keyring(keys.clone());
        *self.auth.keys.borrow_mut() = keys;
        self.activate_identity()
    }

    /// Activate bootstrap keys only in the correctly bound node graph.
    pub fn activate_identity(&self) -> Result<()> {
        self.auth.check()?;
        let identity = self.identity().ok_or(Error::Unauthorized)?;
        let keys = self.auth.keys.borrow();
        if keys.node() != identity.node() || keys.cluster() != identity.cluster() {
            return Err(Error::Unauthorized);
        }
        if let Some(bundle) = self.auth.startup_bundle.borrow().as_ref() {
            self.auth.secrets.install(bundle.clone())?;
        }
        keys.install_signing_identity(identity.signing_identity(
            &keys.peer_trust_roots()?,
            &self.auth.enrollment.wall_clock(),
        )?)?;
        self.auth.startup_bundle.borrow_mut().take();
        Ok(())
    }

    /// Lease the current locally validated signing identity.
    pub fn identity(&self) -> Option<LocalSigningIdentity> {
        self.auth.identity.borrow().clone()
    }

    /// Startup selector only. Renewal never changes a live device layout.
    pub fn block_devices(&self) -> Option<String> {
        self.block_devices.borrow().clone()
    }

    /// One publication poll turn, concurrent with independent renewal policy.
    pub fn progress<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            self.auth.check()?;
            if !self.auth.started.get() {
                return Err(Error::InvalidConfiguration);
            }
            let _busy = super::enter(&self.busy)?;
            // Without usable identity, do not start an identity-only fetch that
            // could race renewal and advance its own authentication retry clock.
            if self
                .identity()
                .is_none_or(|identity| !identity.valid_at(self.auth.enrollment.wall_now()))
            {
                self.renew_if_due(scope).await?;
                if self
                    .identity()
                    .is_none_or(|identity| !identity.valid_at(self.auth.enrollment.wall_now()))
                {
                    return Err(Error::Unavailable);
                }
            }
            let mut driver = Turn::take(&self.publications)?;
            let renewed = Cell::new(false);
            let renewal = async {
                loop {
                    self.renew_if_due(scope).await?;
                    renewed.set(true);
                    let wake = self.renewal_wake(scope.clock().now());
                    let io = self.enrollment_transport.io()?;
                    let sleep = io.sleep(wake, scope);
                    // A rejected fetch sets `binding_check` on this lane and wakes
                    // this select, so renewal starts without waiting for `wake`.
                    let rejected = std::future::poll_fn(|_| {
                        if self.auth.binding_check.get() {
                            std::task::Poll::Ready(Ok(()))
                        } else {
                            std::task::Poll::Pending
                        }
                    });
                    futures::future::select(sleep, Box::pin(rejected))
                        .await
                        .factor_first()
                        .0?;
                }
                #[allow(unreachable_code)]
                Ok::<(), Error>(())
            };
            let result = {
                // A quick 204 must not repeatedly abandon the current issuance.
                // Only authentication policy is coordinated here; the poll owns all
                // feed receipt, preparation, installation, and retry scheduling.
                let publication = async {
                    let mut completed = None;
                    loop {
                        self.publication_fetch_started.set(false);
                        let result = {
                            let mut turn = Box::pin(driver.turn(scope));
                            std::future::poll_fn(|cx| {
                                // The poll must always be polled during backoff: it
                                // drives local preparation and worker barriers.
                                let result = turn.as_mut().poll(cx);
                                if result.is_ready() {
                                    return result;
                                }
                                if renewed.get()
                                    && !self.publication_fetch_started.get()
                                    && let Some(result) = completed.take()
                                {
                                    return std::task::Poll::Ready(result);
                                }
                                std::task::Poll::Pending
                            })
                            .await
                        };
                        if renewed.get() || result.as_ref().is_err_and(|error| !transient(*error)) {
                            return result;
                        }
                        // A completed fetch may only have submitted preparation.
                        // Keep the poll ticking while issuance is held; never restart
                        // an active long poll just because renewal completed.
                        completed = Some(result);
                    }
                };
                match futures::future::select(Box::pin(renewal), Box::pin(publication)).await {
                    futures::future::Either::Left((result, _))
                    | futures::future::Either::Right((result, _)) => result,
                }
            };
            self.publication_next.set(Some(driver.status().next_fetch));
            if driver.status().pending.is_none() {
                self.received.set(None);
            }
            result.map_err(|error| {
                if error == Error::Unauthorized {
                    Error::Unavailable
                } else {
                    error
                }
            })
        })
    }

    /// Independently advance key delivery, keeping its own retry and acceptance state.
    pub fn keyring_progress<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            self.auth.check()?;
            let mut driver = Turn::take(&self.keyrings)?;
            let result = driver.turn(scope).await;
            self.keyring_error
                .set(if result.is_ok() && driver.status().pending.is_none() {
                    None
                } else {
                    driver.status().error
                });
            match result {
                Err(Error::NodeIdentityChanged | Error::Canceled) => result,
                _ if self.auth.started.get() => scope.check(),
                _ => result,
            }
        })
    }

    /// Startup keyring turn: after a receipt, settle local preparation before the
    /// caller loops. Another turn would GET again once preparation outlasts one
    /// tick, sending a redundant bootstrap-token request before trust is installed.
    fn keyring_startup_turn<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            self.auth.check()?;
            let mut driver = Turn::take(&self.keyrings)?;
            let result = match driver.turn(scope).await {
                Ok(()) => driver.settle(scope).await,
                Err(error) => Err(error),
            };
            self.keyring_error
                .set(if result.is_ok() && driver.status().pending.is_none() {
                    None
                } else {
                    driver.status().error
                });
            result
        })
    }

    /// Renewal failures never reset either feed or discard a still-valid identity.
    async fn renew_if_due(&self, scope: &RequestScope) -> Result<()> {
        if (self.auth.binding_check.get()
            || self
                .identity()
                .is_none_or(|i| i.renewal_due_at(self.auth.enrollment.wall_now())))
            && self
                .renewal_next
                .get()
                .is_none_or(|n| n <= scope.clock().now())
        {
            let result = async {
                let nics = self.rails.persist(scope).await?;
                self.report_rails(&nics);
                let request = self
                    .auth
                    .enrollment
                    .prepare(nics, self.shares, scope)
                    .await?;
                self.accept_renewal(self.enroll(&request, scope).await?, scope)
                    .await
            }
            .await;
            match result {
                Ok(()) => {
                    self.auth.binding_check.set(false);
                    self.renewal_error.set(None);
                    self.renewal_next.set(None);
                    self.issuance_failures.set(0);
                }
                Err(error) => {
                    self.renewal_error.set(Some(error));
                    if !transient(error) && error != Error::Unauthorized {
                        return Err(error);
                    }
                    self.renewal_next.set(Some(self.issuance_backoff(
                        &self.enrollment_transport.io()?.reactor.ctx().clone(),
                    )?));
                    if self
                        .identity()
                        .is_none_or(|i| !i.valid_at(self.auth.enrollment.wall_now()))
                    {
                        return Err(if error == Error::Unauthorized {
                            Error::Unavailable
                        } else {
                            error
                        });
                    }
                }
            }
        }
        Ok(())
    }

    /// Latch changed binding before persistence, including failed or abandoned writes.
    pub(super) async fn accept_renewal(
        &self,
        response: wire::EnrollmentResponse,
        scope: &RequestScope,
    ) -> Result<()> {
        let changed = self
            .identity()
            .is_some_and(|old| old.node() != &response.node);
        if changed {
            self.auth.restart_required.set(true);
        }
        let accepted = self.auth.enrollment.accept_response(response, scope).await;
        if changed {
            return Err(Error::NodeIdentityChanged);
        }
        let identity = accepted?;
        let keys = self.auth.keys.borrow();
        keys.install_signing_identity(identity.signing_identity(
            &keys.peer_trust_roots()?,
            &self.auth.enrollment.wall_clock(),
        )?)?;
        *self.auth.identity.borrow_mut() = Some(identity);
        Ok(())
    }

    /// Read-only receipt versus acceptance diagnostics, safe during a held long poll.
    pub(crate) fn membership_diagnostic(&self) -> Result<crate::telemetry::MembershipDiagnostic> {
        let accepted = self.snapshots.current()?;
        let pending = self
            .received
            .get()
            .filter(|(sequence, _)| *sequence > accepted.sequence.0);
        Ok(crate::telemetry::MembershipDiagnostic {
            accepted_sequence: accepted.sequence.0,
            accepted_membership: accepted.membership.version.0,
            accepted_hash: accepted.membership_hash,
            pending_sequence: pending.map_or(0, |p| p.0),
            pending_membership: pending.map_or(0, |p| p.1),
            ..Default::default()
        })
    }

    /// Close admission; owner must drop active turns before calling shutdown.
    pub fn stop(&self) {
        self.auth.stopped.set(true);
        self.enrollment_transport.close_idle();
    }

    /// Explicitly fence both owned CPU jobs while the serving reactor is alive.
    pub fn shutdown<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            self.stop();
            let mut publications = Turn::take(&self.publications)?;
            let mut keys = Turn::take(&self.keyrings)?;
            let (publication, key) =
                futures::join!(publications.shutdown(scope), keys.shutdown(scope));
            publication?;
            key?;
            self.auth.identity.borrow_mut().take();
            Ok(())
        })
    }
}

/// Validate the structured code/status agreement before retry or auth classification.
pub(super) fn validate_response(response: &rest::Response) -> Result<()> {
    poll::validate_response(response)
}

/// Separate REST pool per feed and for issuance. Managed pools follow the shared
/// discovered set; standalone pools reread deployment-provided trust.
pub(super) fn transport(endpoint: &ControlEndpoint) -> rest::Transport<ReactorControlIo> {
    let transport = rest::Transport::new(rest::Config {
        url: endpoint.url.clone(),
        trust_bundle: endpoint.trust_bundle.clone(),
        max_trust_bundle: wire::MAX_BUNDLE_BYTES,
        max_error_body: wire::MAX_ENROLLMENT_BYTES,
    });
    if let Some(discovered) = &endpoint.discovered {
        transport.set_endpoints(discovered.clone());
    }
    transport
}

#[cfg(test)]
pub(super) mod tests {
    //! Scripted application policy over the real long-poll owners.

    use super::*;
    use std::sync::Arc;
    use std::{collections::VecDeque, future::Future, task::Context};

    /// Valid bundle at `generation` whose peer trust roots and key material pass validation.
    fn startup_bundle(generation: u64, ca: &[u8]) -> wire::KeyringBundle {
        let mut bundle = wire::decode_bundle(include_bytes!(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/wire/testdata/bundle.json"
        )))
        .unwrap();
        bundle.generation = racer_wire::control::BundleGeneration(generation);
        bundle.peer_trust_roots = vec![ca.to_vec()];
        // The wire vector repeats material across purposes; use distinct material.
        let key = &bundle.cache_keys[2];
        bundle.cache_keys[2] = wire::CacheEncryptionKey::new(
            key.key.clone(),
            key.state,
            zeroize::Zeroizing::new([2; 32]),
        );
        bundle
    }

    /// Before startup completes, a bundle older than the node-wide accepted one
    /// (installed by an earlier session in this process) is refused as a replay
    /// instead of becoming the startup bundle. Equal and newer ones are staged.
    #[test]
    fn startup_bundle_older_than_shared_keys_is_refused() {
        let env = racer_io::Env::host();
        let session = session(&env);
        let (ca, _) = crate::test_support::enrollment::ca();
        // An earlier session in this process already installed generation 2.
        session
            .auth
            .secrets
            .install(startup_bundle(2, ca.der()))
            .unwrap();
        assert!(!session.auth.started.get());
        let error = Rc::new(Cell::new(None));
        let install = BundleInstall {
            auth: session.auth.clone(),
            error: error.clone(),
        };
        let stage = |generation| {
            let bundle = Arc::new(startup_bundle(generation, ca.der()));
            install.install(
                &Document::Keyring(bundle.clone()),
                &Prepared::Keyring(bundle),
            )
        };
        assert_eq!(stage(1), Err(Error::Replay));
        assert_eq!(error.get(), Some(Error::Replay));
        assert!(session.auth.startup_bundle.borrow().is_none());
        for generation in [2, 3] {
            stage(generation).unwrap();
            assert_eq!(error.get(), None);
            assert_eq!(
                session
                    .auth
                    .startup_bundle
                    .borrow()
                    .as_ref()
                    .map(|b| b.generation.0),
                Some(generation)
            );
        }
        // The shared keys were never rolled back.
        assert_eq!(session.auth.keys.borrow().generation().unwrap(), Some(2));
    }

    /// Receipts never regress by sequence and remain independent of acceptance.
    #[test]
    fn receipt_observation_preserves_equal_sequence_and_reset_behavior() {
        let received = Cell::new(None);
        for (sequence, membership, expected) in [
            (10, 1, (10, 1)),
            (9, 2, (10, 1)),
            (10, 3, (10, 3)),
            (11, 4, (11, 4)),
        ] {
            let mut publication = document(sequence);
            publication.membership_version.0 = membership;
            observe_receipt(&received, &publication);
            assert_eq!(received.get(), Some(expected));
        }
        received.set(None);
        let publication = document(1);
        observe_receipt(&received, &publication);
        assert_eq!(received.get(), Some((1, publication.membership_version.0)));
    }

    /// Scripted wire boundary; the real Poll still owns all protocol progress.
    struct Script {
        responses: RefCell<VecDeque<rest::Response>>,

        // Keep the wire path/digest pair visible without a fixture-only alias.
        #[allow(clippy::type_complexity)]
        requests: Rc<RefCell<Vec<(String, Option<String>)>>>,
    }

    impl Fetch for Script {
        fn get<'a>(
            &'a self,
            path: &'a str,
            digest: Option<&'a str>,
            _: usize,
            _: &'a RequestScope,
        ) -> racer_io::Operation<'a, rest::Response, FetchError> {
            Box::pin(async move {
                self.requests
                    .borrow_mut()
                    .push((path.into(), digest.map(str::to_owned)));
                let response = self
                    .responses
                    .borrow_mut()
                    .pop_front()
                    .expect("script exhausted");
                validate_response(&response).map_err(|error| FetchError {
                    error,
                    retry_after: response.retry_after,
                })?;
                Ok(response)
            })
        }

        fn close(&self) {}
    }

    /// A poll over a scripted wire boundary with the session's real install hook.
    fn scripted(env: &racer_io::Env, session: &Session, doc: Doc, source: Script) -> Poll {
        let install: Box<dyn Install> = match doc {
            Doc::Publication => Box::new(PublicationInstall {
                target: session.snapshots.clone(),
                received: session.received.clone(),
                auth: session.auth.clone(),
            }),
            Doc::Keyring => Box::new(BundleInstall {
                auth: session.auth.clone(),
                error: session.keyring_error.clone(),
            }),
        };
        Poll::new(
            host(env),
            doc,
            Box::new(source),
            install,
            Schedule::control(),
        )
        .unwrap()
    }

    /// Explicit simulated clock progression with a host watchdog and CPU yield.
    fn drive<T>(clock: &racer_io::SimClock, operation: impl Future<Output = T>) -> T {
        let mut operation = std::pin::pin!(operation);
        let until = Instant::now() + Duration::from_secs(5);
        loop {
            if let std::task::Poll::Ready(result) = operation
                .as_mut()
                .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
            {
                return result;
            }
            assert!(Instant::now() < until, "bounded scripted driver stalled");
            clock.advance(Duration::from_millis(10));
            std::thread::sleep(Duration::from_millis(1));
        }
    }

    /// Application error/scoping adapter, without any external sockets.
    fn host(env: &racer_io::Env) -> Rc<ReactorControlIo> {
        Rc::new(ReactorControlIo::new(Rc::new(
            crate::runtime::Reactor::new(
                Rc::new(flow_control::Quotas::new(
                    crate::admission::AdmissionPolicy::new(
                        crate::test_support::cluster::config(false).limits,
                    ),
                )),
                env,
            ),
        )))
    }

    /// Long enough for several independent retry turns.
    fn scope(env: &racer_io::Env) -> RequestScope {
        RequestScope::on(
            &env.ctx.clock,
            crate::model::RequestId([8; 16]),
            env.ctx.now() + Duration::from_secs(300),
        )
        .unwrap()
    }

    /// Valid wire fixture retains the exact document rather than rebuilding a snapshot.
    fn document(sequence: u64) -> wire::Publication {
        let mut p = wire::decode_publication(include_bytes!(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/wire/testdata/publication.json"
        )))
        .unwrap();
        p.sequence.0 = sequence;
        p
    }

    /// Session fixture intentionally has no token and no attached transport until requested.
    fn session(env: &racer_io::Env) -> Session {
        let p = document(1);
        let keys = Rc::new(Keyring::new(
            p.cluster.clone(),
            p.members[0].node.clone(),
            Arc::new(Default::default()),
        ));
        Session::new(
            ControlEndpoint {
                url: "https://localhost".into(),
                trust_bundle: "missing".into(),
                discovered: None,
            },
            Rc::new(Enrollment::new(
                p.cluster.clone(),
                "missing-token".into(),
                "missing-identity".into(),
                host(env),
            )),
            RailJournal::new(
                Arc::new(Default::default()),
                "missing-identity".into(),
                host(env),
            ),
            std::num::NonZeroU32::new(4).unwrap(),
            keys.clone(),
            BundleInstaller::new(keys),
            Rc::new(PublicationTarget::new(
                p.cluster,
                Arc::new(crate::control::cell::PublicationCell::new(
                    super::super::Snapshot::retention(0),
                )),
                env.ctx.clock.clone(),
            )),
        )
    }

    /// Preserve the former lagging-replica scenario through real generic scheduling.
    pub(crate) fn lagging_replica(env: &racer_io::Env) {
        let clock = racer_io::SimClock::new(731);
        let sim_env = clock.ctx(0);
        let env = racer_io::Env::new(env.driver.clone(), sim_env.clone());
        let sim_ctx = sim_env.clone();
        for pending in [false, true] {
            let session = session(&env);
            let first = document(10);
            let accepted = session.snapshots.apply(first.clone()).unwrap();
            let mut next = document(11);
            next.membership_version.0 += 1;
            let mut responses = VecDeque::new();
            if pending {
                responses.push_back(rest::Response {
                    status: 200,
                    body: wire::encode_publication(&next).unwrap(),
                    retry_after: None,
                });
            }
            for (status, code) in [(503, "unavailable"), (429, "overloaded")] {
                responses.push_back(rest::Response {
                    status,
                    body: format!("{{\"code\":\"{code}\"}}").into_bytes(),
                    retry_after: Some(Duration::from_secs(7)),
                });
            }
            responses.push_back(rest::Response {
                status: 204,
                body: vec![],
                retry_after: None,
            });
            let requests = Rc::new(RefCell::new(Vec::new()));
            let mut sync = Poll::new(
                host(&env),
                Doc::Publication,
                Box::new(Script {
                    responses: RefCell::new(responses),
                    requests: requests.clone(),
                }),
                Box::new(PublicationInstall {
                    target: session.snapshots.clone(),
                    received: session.received.clone(),
                    auth: session.auth.clone(),
                }),
                Schedule::control(),
            )
            .unwrap();
            sync.seed(Document::Publication(Arc::new(first))).unwrap();
            let scope = scope(&env);
            if pending {
                drive(&clock, sync.turn(&scope)).unwrap();
            }
            for error in [Error::Unavailable, Error::Overloaded] {
                assert_eq!(drive(&clock, sync.turn(&scope)), Err(error));
                assert!(sync.status().next_fetch >= sim_ctx.clock.now() + Duration::from_secs(7));
                assert_eq!(sync.status().accepted, Some(10));
                assert_eq!(sync.status().pending, pending.then_some(11));
                assert!(Arc::ptr_eq(
                    &accepted,
                    &session.snapshots.current().unwrap()
                ));
                assert!(!session.auth.binding_check.get());
                assert_eq!(session.renewal_error(), None);
            }
            drive(&clock, sync.turn(&scope)).unwrap();
            assert!(
                requests
                    .borrow()
                    .iter()
                    .skip(usize::from(pending))
                    .all(|(path, digest)| path.ends_with(if pending {
                        "after=11"
                    } else {
                        "after=10"
                    }) && digest.as_ref().is_some_and(|d| d.len() == 64))
            );
            let diagnostic = session.membership_diagnostic().unwrap();
            assert_eq!(diagnostic.accepted_sequence, 10);
            if pending {
                assert_eq!(diagnostic.pending_sequence, 11);
            }
            drive(&clock, sync.shutdown(&scope)).unwrap();
        }
    }

    /// Structured failures feed generic Retry-After, while stop fences both drivers.
    pub(crate) fn status_retry_shutdown(env: &racer_io::Env) {
        for (status, code, expected) in [
            (413, "too_large", Error::InvalidRequest),
            (503, "forbidden", Error::InvalidRequest),
            (503, "unavailable", Error::Unavailable),
            (401, "unauthenticated", Error::Unauthorized),
        ] {
            assert_eq!(
                validate_response(&rest::Response {
                    status,
                    body: format!("{{\"code\":\"{code}\"}}").into_bytes(),
                    retry_after: Some(Duration::from_secs(60))
                }),
                Err(expected)
            );
        }
        let clock = racer_io::SimClock::new(732);
        let env = racer_io::Env::new(env.driver.clone(), clock.ctx(0));
        let session = session(&env);
        session.attach_io(host(&env)).unwrap();
        let scope = scope(&env);
        drive(&clock, session.shutdown(&scope)).unwrap();
        assert_eq!(
            drive(&clock, session.progress(&scope)),
            Err(Error::Canceled)
        );
        assert_eq!(
            drive(&clock, session.keyring_progress(&scope)),
            Err(Error::Canceled)
        );
        assert!(
            session
                .publications
                .borrow()
                .as_ref()
                .unwrap()
                .status()
                .stopped
        );
        assert!(session.keyrings.borrow().as_ref().unwrap().status().stopped);
    }

    /// Key feed validates 204 and tracks its cursor independently of publication/issuance.
    pub(crate) fn independent_keyring(env: &racer_io::Env) {
        let clock = racer_io::SimClock::new(733);
        let env = racer_io::Env::new(env.driver.clone(), clock.ctx(0));
        let session = session(&env);
        session.attach_io(host(&env)).unwrap();
        let scope = scope(&env);
        let key_owner = Turn::take(&session.keyrings).unwrap();
        assert_eq!(
            drive(&clock, session.keyring_progress(&scope)),
            Err(Error::Overloaded)
        );
        assert!(session.publications.borrow().is_some());
        assert!(!session.enrollment_busy.get());
        drop(key_owner);
        for (seed, body, success) in [
            (false, vec![], false),
            (true, vec![], true),
            (true, vec![b'x'], false),
        ] {
            let source = Script {
                responses: RefCell::new(VecDeque::from([rest::Response {
                    status: 204,
                    body,
                    retry_after: None,
                }])),
                requests: Rc::new(RefCell::new(vec![])),
            };
            let feed = scripted(&env, &session, Doc::Keyring, source);
            if seed {
                let bundle = wire::decode_bundle(include_bytes!(concat!(
                    env!("CARGO_MANIFEST_DIR"),
                    "/wire/testdata/bundle.json"
                )))
                .unwrap();
                feed.restore(Some(Document::Keyring(Arc::new(bundle))));
            }
            assert_eq!(drive(&clock, feed.fetch(&scope)).is_ok(), success);
        }
        assert!(
            session
                .publications
                .borrow()
                .as_ref()
                .unwrap()
                .status()
                .accepted
                .is_none()
        );
        assert!(session.issuance_retry_after.get().is_none());
        drive(&clock, session.shutdown(&scope)).unwrap();
    }

    /// No string method or externally supplied delta digest exists in the feed API.
    pub(crate) fn canonical_delta_and_get_only(env: &racer_io::Env) {
        let clock = racer_io::SimClock::new(734);
        let env = racer_io::Env::new(env.driver.clone(), clock.ctx(0));
        let p = Document::Publication(Arc::new(document(1)));
        let digest = p.digest().unwrap();
        assert_eq!(digest.len(), 64);
        assert!(digest.bytes().all(|b| b.is_ascii_hexdigit()));
        let requests = Rc::new(RefCell::new(vec![]));
        let source = Script {
            responses: RefCell::new(VecDeque::from([rest::Response {
                status: 204,
                body: vec![],
                retry_after: None,
            }])),
            requests: requests.clone(),
        };
        let feed = scripted(&env, &session(&env), Doc::Publication, source);
        feed.restore(Some(p));
        drive(&clock, feed.fetch(&scope(&env))).unwrap();
        assert_eq!(
            requests.borrow()[0],
            (format!("{}?after=1", wire::SNAPSHOT_PATH), Some(digest))
        );
    }

    /// Not-before policy is enforced before the generic transport receives an identity.
    pub(crate) fn reject_future_identity(env: &racer_io::Env, identity: LocalSigningIdentity) {
        let session = session(env);
        *session.auth.identity.borrow_mut() = Some(identity);
        let client = Client {
            transport: transport(&session.endpoint),
            auth: session.auth.clone(),
            doc: Doc::Publication,
            started: None,
        };
        assert!(client.identity().unwrap().is_none());
        assert_eq!(Doc::Publication.rejection(), Rejection::IdentityOnly);
        assert_eq!(Doc::Keyring.rejection(), Rejection::TokenFallback);
    }

    /// Pending documents, not lossy reconstructed snapshots, form the next delta base.
    #[test]
    fn pending_publication_delta_uses_exact_received_document() {
        let clock = racer_io::SimClock::new(735);
        let env = racer_io::Env::new(racer_io::Driver::Uring, clock.ctx(0));
        let session = session(&env);
        let first = document(10);
        let held = session.snapshots.apply(first.clone()).unwrap();
        let mut pending = document(11);
        pending.membership_version.0 += 1;
        pending.members[0].site = "edge-a".into();
        let mut next = pending.clone();
        next.sequence.0 = 12;
        next.caches.clear();
        next.pools.clear();
        let delta = serde_json::json!({
            "delta_version": wire::DELTA_VERSION, "cluster": pending.cluster.0,
            "base_sequence": "11", "base_hash": wire::content_hash(&pending).unwrap(),
            "sequence": "12", "membership_version": next.membership_version.0.to_string(),
            "content_hash": wire::content_hash(&next).unwrap(),
            "upsert_members": [], "remove_members": [], "caches": [],
            "pools": [], "page_bytes": 16777216
        });
        let requests = Rc::new(RefCell::new(vec![]));
        let source = Script {
            responses: RefCell::new(VecDeque::from([
                rest::Response {
                    status: 200,
                    body: wire::encode_publication(&pending).unwrap(),
                    retry_after: None,
                },
                rest::Response {
                    status: 200,
                    body: serde_json::to_vec(&delta).unwrap(),
                    retry_after: None,
                },
            ])),
            requests: requests.clone(),
        };
        let mut sync = Poll::new(
            host(&env),
            Doc::Publication,
            Box::new(source),
            Box::new(PublicationInstall {
                target: session.snapshots.clone(),
                received: session.received.clone(),
                auth: session.auth.clone(),
            }),
            Schedule::control(),
        )
        .unwrap();
        sync.seed(Document::Publication(Arc::new(first))).unwrap();
        let scope = scope(&env);
        drive(&clock, sync.turn(&scope)).unwrap();
        drive(&clock, sync.turn(&scope)).unwrap();
        assert_eq!(sync.status().accepted, Some(10));
        assert_eq!(sync.status().pending, Some(12));
        assert_eq!(
            requests.borrow().len(),
            2,
            "no full fallback for valid pending-base delta"
        );
        assert_eq!(
            requests.borrow()[1].1.as_deref(),
            Some(wire::content_hash(&pending).unwrap().as_str())
        );
        assert_eq!(
            session.membership_diagnostic().unwrap().pending_sequence,
            12
        );
        assert!(Arc::ptr_eq(&held, &session.snapshots.current().unwrap()));
        drive(&clock, sync.shutdown(&scope)).unwrap();
    }

    /// Managed reports carry configured shares and are sent only when rails change.
    #[test]
    fn managed_report_forwards_distinct_rails_only() {
        let env = racer_io::Env::host();
        let session = session(&env);
        session.mark_serving();
        session.report_rails(&[]);
        let seen = Arc::new(std::sync::Mutex::new(Vec::new()));
        let sink = seen.clone();
        session.set_report(Arc::new(move |shares, rails| {
            sink.lock().unwrap().push((shares, rails))
        }));
        let rail = wire::RailMapping {
            rail: wire::RailId(0),
            device: "mlx5_0".into(),
            port: 1,
            gid: None,
            numa_node: Some(0),
        };
        session.report_rails(&[]);
        session.report_rails(&[]);
        session.report_rails(std::slice::from_ref(&rail));
        session.report_rails(std::slice::from_ref(&rail));
        assert_eq!(*seen.lock().unwrap(), vec![(4, vec![]), (4, vec![rail])]);
    }

    /// Nothing is reported before the peer listener is
    /// open; then only the newest rails, once.
    #[test]
    fn managed_report_waits_until_serving() {
        let env = racer_io::Env::host();
        let session = session(&env);
        let seen = Arc::new(std::sync::Mutex::new(Vec::new()));
        let sink = seen.clone();
        session.set_report(Arc::new(move |shares, rails| {
            sink.lock().unwrap().push((shares, rails))
        }));
        let rail = wire::RailMapping {
            rail: wire::RailId(0),
            device: "mlx5_0".into(),
            port: 1,
            gid: None,
            numa_node: Some(0),
        };
        session.report_rails(&[]);
        session.report_rails(std::slice::from_ref(&rail));
        assert!(seen.lock().unwrap().is_empty(), "reported before serving");
        session.mark_serving();
        assert_eq!(*seen.lock().unwrap(), vec![(4, vec![rail.clone()])]);
        session.mark_serving();
        session.report_rails(std::slice::from_ref(&rail));
        assert_eq!(seen.lock().unwrap().len(), 1, "same rails reported twice");
        session.report_rails(&[]);
        assert_eq!(*seen.lock().unwrap(), vec![(4, vec![rail]), (4, vec![])]);
    }

    /// The serving renewal loop sleeps until its scheduled retry within
    /// fixed bounds instead of rechecking every second.
    #[test]
    fn renewal_wake_follows_retry_within_bounds() {
        let env = racer_io::Env::host();
        let session = session(&env);
        let now = Instant::now();
        // No identity and no retry keeps the former one-second recheck.
        assert_eq!(session.renewal_wake(now), now + RENEWAL_MIN_WAIT);
        session
            .renewal_next
            .set(Some(now + Duration::from_secs(10)));
        assert_eq!(session.renewal_wake(now), now + Duration::from_secs(10));
        session
            .renewal_next
            .set(Some(now + Duration::from_secs(100)));
        assert_eq!(session.renewal_wake(now), now + RENEWAL_MAX_WAIT);
        session.renewal_next.set(Some(now));
        assert_eq!(session.renewal_wake(now), now + RENEWAL_MIN_WAIT);
    }

    /// Without any persisted rails, serving reports nothing (there is
    /// nothing to report yet; enrollment persists rails first).
    #[test]
    fn serving_without_rails_reports_nothing() {
        let env = racer_io::Env::host();
        let session = session(&env);
        let seen = Arc::new(std::sync::Mutex::new(Vec::new()));
        let sink = seen.clone();
        session.set_report(Arc::new(move |shares, rails| {
            sink.lock().unwrap().push((shares, rails))
        }));
        session.mark_serving();
        assert!(seen.lock().unwrap().is_empty());
    }
}
