//! Racer authentication and renewal policy around two independent generic feeds.

use super::publication::{PublicationCodec, PublicationTarget};
use super::rails::RailJournal;
use super::{ControlEndpoint, ReactorControlIo, transient};
use crate::error::{Error, Operation, Result};
use crate::runtime::RequestScope;
use controlplane::feed::{Identity, Preparation, RejectionPolicy, Schedule};
use controlplane::{Client, Codec, Credentials, Feed, Sync, Target};
use racer_control_wire as wire;
use racer_crypto::enrollment::{Enrollment, LocalSigningIdentity};
use racer_crypto::identity::{BundleInstaller, Keyring};
use sha2::{Digest, Sha256};
use std::{
    cell::{Cell, RefCell},
    ops::{Deref, DerefMut},
    rc::Rc,
    sync::Arc,
    time::{Duration, Instant},
};
use wire_codec::rest;
use zeroize::Zeroizing;

/// Owner-local session. Only generic Sync owns cursors, retry, and preparation jobs.
pub struct Session {
    block_devices: RefCell<Option<String>>,
    rails: RailJournal,

    shares: std::num::NonZeroU32,

    endpoint: ControlEndpoint,

    enrollment_transport: rest::Transport<ReactorControlIo>,

    pub(super) auth: Rc<Authentication>,

    pub(super) snapshots: Rc<PublicationTarget>,

    // Keep the concrete generic owner visible; no compatibility driver alias.
    #[allow(clippy::type_complexity)]
    publications: RefCell<
        Option<Sync<ReactorControlIo, ObservedPublication, PublicationSource, PublicationInstall>>,
    >,

    #[allow(clippy::type_complexity)]
    keyrings: RefCell<
        Option<
            Sync<
                ReactorControlIo,
                BundleCodec,
                Client<ReactorControlIo, FeedCredentials>,
                BundleTarget,
            >,
        >,
    >,

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
            return Err(Error::Cancelled);
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

/// Independent per-feed authentication policy using one node-binding authority.
struct FeedCredentials {
    auth: Rc<Authentication>,

    policy: RejectionPolicy,
}

/// Observe only request entry so renewal completion never cancels an active GET.
struct PublicationSource {
    client: Client<ReactorControlIo, FeedCredentials>,

    started: Rc<Cell<bool>>,
}

impl controlplane::feed::Source<ReactorControlIo> for PublicationSource {
    fn get<'a>(
        &'a self,
        path: &'a str,
        digest: Option<&'a str>,
        limit: usize,
        scope: &'a RequestScope,
    ) -> uring_runtime::Operation<'a, rest::Response, controlplane::feed::FetchError<Error>> {
        self.started.set(true);
        self.client.get(path, digest, limit, scope)
    }

    fn close(&self) {
        self.client.close();
    }
}

/// REST credential lease; borrows the crypto-owned private key without exporting it.
struct IdentityLease(LocalSigningIdentity);

impl Identity for IdentityLease {
    fn identity(&self) -> rest::Identity<'_> {
        rest::Identity {
            certificate_chain: self.0.certificate_chain(),
            private_key: self.0.private_key_der(),
            expires: self.0.expires_at(),
        }
    }
}

impl Credentials<ReactorControlIo> for FeedCredentials {
    type Identity = IdentityLease;

    type Token = Zeroizing<String>;

    fn identity(&self) -> Result<Option<Self::Identity>> {
        self.auth.check()?;
        Ok(self
            .auth
            .identity
            .borrow()
            .clone()
            .filter(|i| i.valid_now())
            .map(IdentityLease))
    }

    fn policy(&self) -> RejectionPolicy {
        self.policy
    }

    fn token<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, Option<Self::Token>> {
        Box::pin(async move {
            self.auth.check()?;
            Ok(Some(self.auth.enrollment.read_token(scope).await?))
        })
    }

    fn rejected(&self) {
        self.auth.binding_check.set(true);
    }

    fn validate_response(&self, response: &rest::Response) -> Result<()> {
        validate_response(response)
    }
}

/// Codec receipt observation is diagnostic only; generic Feed retains the delta base.
struct ObservedPublication {
    received: Rc<Cell<Option<(u64, u64)>>>,
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

impl Codec for ObservedPublication {
    type Document = wire::Publication;

    type Version = u64;

    type Error = Error;

    fn version(&self, document: &Self::Document) -> u64 {
        document.sequence.0
    }

    fn decode(&self, bytes: &[u8]) -> Result<Self::Document> {
        PublicationCodec.decode(bytes)
    }

    fn delta(&self, base: &Self::Document, bytes: &[u8]) -> Result<Self::Document> {
        PublicationCodec.delta(base, bytes)
    }

    fn digest(&self, document: &Self::Document) -> Result<String> {
        let digest = PublicationCodec.digest(document)?;
        observe_receipt(&self.received, document);
        Ok(digest)
    }
}

/// Keep domain installation local while delegating CPU projection to PublicationTarget.
struct PublicationInstall {
    target: Rc<PublicationTarget>,

    received: Rc<Cell<Option<(u64, u64)>>>,

    auth: Rc<Authentication>,
}

impl Target<ObservedPublication> for PublicationInstall {
    type Prepared = Arc<super::Snapshot>;

    fn prepare(
        &self,
        document: Arc<wire::Publication>,
    ) -> Result<Preparation<Self::Prepared, Error>> {
        self.auth.check()?;
        observe_receipt(&self.received, &document);
        self.target.prepare(document)
    }

    fn install(&self, document: &wire::Publication, prepared: &Self::Prepared) -> Result<()> {
        self.auth.check()?;
        self.target.install(document, prepared)
    }
}

/// Keyring documents have their own generation and no delta representation.
struct BundleCodec;

impl Codec for BundleCodec {
    type Document = wire::KeyringBundle;

    type Version = u64;

    type Error = Error;

    fn version(&self, document: &Self::Document) -> u64 {
        document.generation.0
    }

    fn decode(&self, bytes: &[u8]) -> Result<Self::Document> {
        Ok(wire::decode_bundle(bytes)?)
    }

    fn delta(&self, _: &Self::Document, _: &[u8]) -> Result<Self::Document> {
        Err(Error::InvalidRequest)
    }

    fn digest(&self, document: &Self::Document) -> Result<String> {
        let mut canonical = document.clone();
        canonical.peer_trust_roots.sort();
        canonical.cache_keys.sort_by(|a, b| {
            (&a.key.cache, a.key.purpose as u8, a.key.id.0).cmp(&(
                &b.key.cache,
                b.key.purpose as u8,
                b.key.id.0,
            ))
        });
        let bytes = Zeroizing::new(wire::encode_bundle(&canonical)?);
        Ok(format!("{:x}", Sha256::digest(&*bytes)))
    }
}

/// Key acceptance is separate from publication acceptance and identity renewal.
struct BundleTarget {
    auth: Rc<Authentication>,

    error: Rc<Cell<Option<Error>>>,
}

impl Target<BundleCodec> for BundleTarget {
    type Prepared = Arc<wire::KeyringBundle>;

    fn prepare(
        &self,
        document: Arc<wire::KeyringBundle>,
    ) -> Result<Preparation<Self::Prepared, Error>> {
        self.auth.check()?;
        if &document.cluster != self.auth.enrollment.cluster() {
            return Err(Error::Unauthorized);
        }
        Ok(Box::new(move || {
            super::verifier(&document.peer_trust_roots)?;
            Ok(document)
        }))
    }

    fn install(&self, _: &wire::KeyringBundle, prepared: &Self::Prepared) -> Result<()> {
        let result = (|| {
            self.auth.check()?;
            if self.auth.started.get() {
                self.auth.secrets.install((**prepared).clone())?;
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
        let source = |policy| {
            let transport = transport(&self.endpoint);
            transport.attach_io(io.clone());
            Client::new(
                transport,
                FeedCredentials {
                    auth: self.auth.clone(),
                    policy,
                },
                "X-Racer-Delta-Base".into(),
            )
        };
        *self.publications.borrow_mut() = Some(Sync::new(
            io.clone(),
            Feed::new(
                ObservedPublication {
                    received: self.received.clone(),
                },
                wire::SNAPSHOT_PATH.into(),
                wire::MAX_PUBLICATION_BYTES,
            )?,
            PublicationSource {
                client: source(RejectionPolicy::IdentityOnly),
                started: self.publication_fetch_started.clone(),
            },
            PublicationInstall {
                target: self.snapshots.clone(),
                received: self.received.clone(),
                auth: self.auth.clone(),
            },
            schedule(),
        )?);
        *self.keyrings.borrow_mut() = Some(Sync::new(
            io.clone(),
            Feed::new(
                BundleCodec,
                wire::KEYRING_PATH.into(),
                wire::MAX_BUNDLE_BYTES,
            )?,
            source(RejectionPolicy::TokenFallback),
            BundleTarget {
                auth: self.auth.clone(),
                error: self.keyring_error.clone(),
            },
            schedule(),
        )?);
        Ok(())
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

    /// Startup uses issuance retry policy; the generic feed schedules serving polls.
    pub fn next_attempt(&self) -> Option<Instant> {
        if self.auth.started.get() {
            self.publication_next.get()
        } else {
            self.renewal_next.get()
        }
    }

    /// Token issuance is not a feed; preserve bounded independent renewal retry.
    fn issuance_backoff(&self) -> Result<Instant> {
        let failures = self.issuance_failures.get().saturating_add(1);
        self.issuance_failures.set(failures);
        let mut random = [0; 8];
        uring_runtime::environment::fill_random(&mut random).map_err(|_| Error::Io)?;
        let ceiling = (1u64 << failures.saturating_sub(1).min(5)).min(30) * 1000;
        let delay = Duration::from_millis(1000 + u64::from_ne_bytes(random) % (ceiling - 999))
            .max(self.issuance_retry_after.take().unwrap_or_default());
        uring_runtime::environment::now()
            .checked_add(delay)
            .ok_or(Error::InvalidRequest)
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
                .is_some_and(|n| n > uring_runtime::environment::now())
            {
                return Err(Error::Unavailable);
            }
            let result = self.start_inner(scope).await;
            if result.as_ref().is_err_and(|e| transient(*e)) {
                self.renewal_next.set(Some(self.issuance_backoff()?));
            }
            result
        })
    }

    /// Key feed must accept trust before identity recovery and fresh issuance.
    async fn start_inner(&self, scope: &RequestScope) -> Result<LocalSigningIdentity> {
        while self.auth.startup_bundle.borrow().is_none() {
            let mut turn = self.keyring_progress(scope);
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
        keys.install_signing_identity(identity.signing_identity(&keys.peer_trust_roots()?)?)?;
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

    /// One publication Sync turn, concurrent with independent renewal policy.
    pub fn progress<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            self.auth.check()?;
            if !self.auth.started.get() {
                return Err(Error::InvalidConfiguration);
            }
            let _busy = super::enter(&self.busy)?;
            // Without usable identity, do not start an identity-only fetch that
            // could race renewal and advance its own authentication retry clock.
            if self.identity().is_none_or(|identity| !identity.valid_now()) {
                self.renew_if_due(scope).await?;
                if self.identity().is_none_or(|identity| !identity.valid_now()) {
                    return Err(Error::Unavailable);
                }
            }
            let mut driver = Turn::take(&self.publications)?;
            let renewed = Cell::new(false);
            let renewal = async {
                loop {
                    self.renew_if_due(scope).await?;
                    renewed.set(true);
                    self.enrollment_transport
                        .io()?
                        .sleep(
                            uring_runtime::environment::now() + Duration::from_secs(1),
                            scope,
                        )
                        .await?;
                }
                #[allow(unreachable_code)]
                Ok::<(), Error>(())
            };
            let result = {
                // A quick 204 must not repeatedly abandon the current issuance.
                // Only authentication policy is coordinated here; Sync owns all
                // feed receipt, preparation, installation, and retry scheduling.
                let publication = async {
                    let mut completed = None;
                    loop {
                        self.publication_fetch_started.set(false);
                        let result = {
                            let mut turn = Box::pin(driver.turn(scope));
                            std::future::poll_fn(|cx| {
                                // Sync must always be polled during backoff: it
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
                        // Keep Sync ticking while issuance is held; never restart
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

    /// Independently advance key delivery, keeping generic retry and acceptance state.
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
                Err(Error::NodeIdentityChanged | Error::Cancelled) => result,
                _ if self.auth.started.get() => scope.check(),
                _ => result,
            }
        })
    }

    /// Renewal failures never reset either feed or discard a still-valid identity.
    async fn renew_if_due(&self, scope: &RequestScope) -> Result<()> {
        if (self.auth.binding_check.get() || self.identity().is_none_or(|i| i.renewal_due()))
            && self
                .renewal_next
                .get()
                .is_none_or(|n| n <= uring_runtime::environment::now())
        {
            let result = async {
                let nics = self.rails.persist(scope).await?;
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
                    self.renewal_next.set(Some(self.issuance_backoff()?));
                    if self.identity().is_none_or(|i| !i.valid_now()) {
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
        keys.install_signing_identity(identity.signing_identity(&keys.peer_trust_roots()?)?)?;
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

/// Separate REST pool per feed and for issuance, all with deployment-provided trust.
pub(super) fn transport(endpoint: &ControlEndpoint) -> rest::Transport<ReactorControlIo> {
    rest::Transport::new(rest::Config {
        url: endpoint.url.clone(),
        trust_bundle: endpoint.trust_bundle.clone(),
        max_trust_bundle: wire::MAX_BUNDLE_BYTES,
        max_error_body: wire::MAX_ENROLLMENT_BYTES,
    })
}

/// Generic feed progress remains responsive during held polls and remote backoff.
fn schedule() -> Schedule {
    Schedule {
        turn: wire::POLL_WAIT + Duration::from_secs(10),
        tick: Duration::from_millis(10),
        retry_min: Duration::from_secs(1),
        retry_max: Duration::from_secs(30),
    }
}

#[cfg(test)]
pub(super) mod tests {
    //! Scripted application policy over the real generic feed and synchronization owners.

    use super::*;
    use controlplane::feed::{FetchError, Source};
    use std::{
        collections::VecDeque,
        future::Future,
        task::{Context, Poll},
    };

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

    /// Scripted wire boundary; real Feed/Sync still owns all protocol progress.
    struct Script {
        responses: RefCell<VecDeque<rest::Response>>,

        // Keep the wire path/digest pair visible without a fixture-only alias.
        #[allow(clippy::type_complexity)]
        requests: Rc<RefCell<Vec<(String, Option<String>)>>>,
    }

    impl Source<ReactorControlIo> for Script {
        fn get<'a>(
            &'a self,
            path: &'a str,
            digest: Option<&'a str>,
            _: usize,
            _: &'a RequestScope,
        ) -> uring_runtime::Operation<'a, rest::Response, FetchError<Error>> {
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

    /// Explicit simulated clock progression with a host watchdog and CPU yield.
    fn drive<T>(
        clock: &uring_runtime::environment::SimulationClock,
        operation: impl Future<Output = T>,
    ) -> T {
        let mut operation = std::pin::pin!(operation);
        let until = Instant::now() + Duration::from_secs(5);
        loop {
            if let Poll::Ready(result) = operation
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
    fn host() -> Rc<ReactorControlIo> {
        Rc::new(ReactorControlIo::new(Rc::new(
            crate::runtime::Reactor::new(Rc::new(flow_control::Quotas::new(
                crate::admission::AdmissionPolicy::new(
                    crate::test_support::cluster::config(false).limits,
                ),
            ))),
        )))
    }

    /// Long enough for several independent generic retry turns.
    fn scope() -> RequestScope {
        RequestScope::new(
            crate::model::RequestId([8; 16]),
            uring_runtime::environment::now() + Duration::from_secs(300),
        )
        .unwrap()
    }

    /// Valid wire fixture retains the exact document rather than rebuilding a snapshot.
    fn document(sequence: u64) -> wire::Publication {
        let mut p = wire::decode_publication(include_bytes!(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../../internal/racer/wire/testdata/publication.json"
        )))
        .unwrap();
        p.sequence.0 = sequence;
        p
    }

    /// Session fixture intentionally has no token and no attached transport until requested.
    fn session() -> Session {
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
            },
            Rc::new(Enrollment::new(
                p.cluster.clone(),
                "missing-token".into(),
                "missing-identity".into(),
                host(),
            )),
            RailJournal::new(
                Arc::new(Default::default()),
                "missing-identity".into(),
                host(),
            ),
            std::num::NonZeroU32::new(4).unwrap(),
            keys.clone(),
            BundleInstaller::new(keys),
            Rc::new(PublicationTarget::new(
                p.cluster,
                Arc::new(controlplane::Published::new(
                    super::super::Snapshot::retention(0),
                )),
            )),
        )
    }

    /// Preserve the former lagging-replica scenario through real generic scheduling.
    pub(crate) fn lagging_replica() {
        let clock = uring_runtime::environment::SimulationClock::new(731);
        let _time = clock.environment(0).enter();
        for pending in [false, true] {
            let session = session();
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
            let mut sync = Sync::new(
                host(),
                Feed::new(
                    ObservedPublication {
                        received: session.received.clone(),
                    },
                    wire::SNAPSHOT_PATH.into(),
                    wire::MAX_PUBLICATION_BYTES,
                )
                .unwrap(),
                Script {
                    responses: RefCell::new(responses),
                    requests: requests.clone(),
                },
                PublicationInstall {
                    target: session.snapshots.clone(),
                    received: session.received.clone(),
                    auth: session.auth.clone(),
                },
                schedule(),
            )
            .unwrap();
            sync.seed(Arc::new(first)).unwrap();
            let scope = scope();
            if pending {
                drive(&clock, sync.turn(&scope)).unwrap();
            }
            for error in [Error::Unavailable, Error::Overloaded] {
                assert_eq!(drive(&clock, sync.turn(&scope)), Err(error));
                assert!(
                    sync.status().next_fetch
                        >= uring_runtime::environment::now() + Duration::from_secs(7)
                );
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
    pub(crate) fn status_retry_shutdown() {
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
        let clock = uring_runtime::environment::SimulationClock::new(732);
        let _time = clock.environment(0).enter();
        let session = session();
        session.attach_io(host()).unwrap();
        let scope = scope();
        drive(&clock, session.shutdown(&scope)).unwrap();
        assert_eq!(
            drive(&clock, session.progress(&scope)),
            Err(Error::Cancelled)
        );
        assert_eq!(
            drive(&clock, session.keyring_progress(&scope)),
            Err(Error::Cancelled)
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
    pub(crate) fn independent_keyring() {
        let clock = uring_runtime::environment::SimulationClock::new(733);
        let _time = clock.environment(0).enter();
        let session = session();
        session.attach_io(host()).unwrap();
        let scope = scope();
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
            let feed = Feed::new(
                BundleCodec,
                wire::KEYRING_PATH.into(),
                wire::MAX_BUNDLE_BYTES,
            )
            .unwrap();
            if seed {
                let bundle = wire::decode_bundle(include_bytes!(concat!(
                    env!("CARGO_MANIFEST_DIR"),
                    "/../../internal/racer/wire/testdata/bundle.json"
                )))
                .unwrap();
                feed.restore(Some(Arc::new(bundle)));
            }
            let source = Script {
                responses: RefCell::new(VecDeque::from([rest::Response {
                    status: 204,
                    body,
                    retry_after: None,
                }])),
                requests: Rc::new(RefCell::new(vec![])),
            };
            assert_eq!(
                drive(&clock, feed.fetch::<ReactorControlIo, _>(&source, &scope)).is_ok(),
                success
            );
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
    pub(crate) fn canonical_delta_and_get_only() {
        let clock = uring_runtime::environment::SimulationClock::new(734);
        let _time = clock.environment(0).enter();
        let feed = Feed::new(
            PublicationCodec,
            wire::SNAPSHOT_PATH.into(),
            wire::MAX_PUBLICATION_BYTES,
        )
        .unwrap();
        let p = document(1);
        let digest = PublicationCodec.digest(&p).unwrap();
        assert_eq!(digest.len(), 64);
        assert!(digest.bytes().all(|b| b.is_ascii_hexdigit()));
        feed.restore(Some(Arc::new(p)));
        let requests = Rc::new(RefCell::new(vec![]));
        let source = Script {
            responses: RefCell::new(VecDeque::from([rest::Response {
                status: 204,
                body: vec![],
                retry_after: None,
            }])),
            requests: requests.clone(),
        };
        drive(&clock, feed.fetch::<ReactorControlIo, _>(&source, &scope())).unwrap();
        assert_eq!(
            requests.borrow()[0],
            (format!("{}?after=1", wire::SNAPSHOT_PATH), Some(digest))
        );
    }

    /// Not-before policy is enforced before the generic transport receives an identity.
    pub(crate) fn reject_future_identity(identity: LocalSigningIdentity) {
        let session = session();
        *session.auth.identity.borrow_mut() = Some(identity);
        let credentials = FeedCredentials {
            auth: session.auth.clone(),
            policy: RejectionPolicy::IdentityOnly,
        };
        assert!(credentials.identity().unwrap().is_none());
        assert_eq!(credentials.policy(), RejectionPolicy::IdentityOnly);
    }

    /// Pending documents, not lossy reconstructed snapshots, form the next delta base.
    #[test]
    fn pending_publication_delta_uses_exact_received_document() {
        let clock = uring_runtime::environment::SimulationClock::new(735);
        let _time = clock.environment(0).enter();
        let session = session();
        let first = document(10);
        let held = session.snapshots.apply(first.clone()).unwrap();
        let mut pending = document(11);
        pending.membership_version.0 += 1;
        pending.members[0].site = "edge-a".into();
        let mut next = pending.clone();
        next.sequence.0 = 12;
        next.caches.clear();
        let delta = serde_json::json!({
            "delta_version": 1, "cluster": pending.cluster.0,
            "base_sequence": "11", "base_hash": wire::content_hash(&pending).unwrap(),
            "sequence": "12", "membership_version": next.membership_version.0.to_string(),
            "content_hash": wire::content_hash(&next).unwrap(),
            "upsert_members": [], "remove_members": [], "caches": []
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
        let mut sync = Sync::new(
            host(),
            Feed::new(
                ObservedPublication {
                    received: session.received.clone(),
                },
                wire::SNAPSHOT_PATH.into(),
                wire::MAX_PUBLICATION_BYTES,
            )
            .unwrap(),
            source,
            PublicationInstall {
                target: session.snapshots.clone(),
                received: session.received.clone(),
                auth: session.auth.clone(),
            },
            schedule(),
        )
        .unwrap();
        sync.seed(Arc::new(first)).unwrap();
        let scope = scope();
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
}
