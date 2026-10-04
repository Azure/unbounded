use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use racer_control_wire::CacheId;
use racer_control_wire::ClusterId;
use racer_control_wire::KeyId;
use racer_control_wire::MembershipVersion;
use racer_control_wire::NodeId;
use crate::runtime::Reactor;
use crate::runtime::RequestScope;
use crate::topology::Member;
use crate::topology::Membership;
use base64::Engine;
use base64::engine::general_purpose::STANDARD;
use racer_control_wire as wire;
use racer_control_wire::BundleGeneration;
use racer_control_wire::CacheDefinition;
use racer_control_wire::EnrollmentId;
use racer_control_wire::EnrollmentRequest;
use racer_control_wire::EnrollmentResponse;
use racer_control_wire::KeyringBundle;
use racer_control_wire::Publication;
use racer_control_wire::PublicationSequence;
use racer_control_wire::SnapshotRequest;
use racer_control_wire::SnapshotResponse;
use racer_control_wire::canonical_content;
use racer_control_wire::validate_publication;
use racer_identity::KeyPurpose;
use racer_identity::Keyring;
use rest_client::Response;
use serde::Deserialize;
use serde::Serialize;
use sha2::Digest;
use sha2::Sha256;
use std::cell::Cell;
use std::cell::RefCell;
use std::ffi::CString;
use std::ffi::OsStr;
use std::net::SocketAddr;
use std::os::fd::AsRawFd;
use std::os::fd::FromRawFd;
use std::os::unix::ffi::OsStrExt;
use std::path::Component;
use std::path::Path;
use std::path::PathBuf;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::Mutex;
use std::sync::Weak;
use std::time::Duration;
use std::time::Instant;
use std::time::SystemTime;
use std::time::UNIX_EPOCH;
use uring_runtime::reactor::Descriptor;
use zeroize::Zeroize;
use zeroize::Zeroizing;

// HTTPS enrollment, snapshot polling, and independent network keyring delivery.
// Bounded long polls, accepted cursors, jittered retry; no node/status reporting.

pub struct BundleInstaller {
    keys: RefCell<Rc<Keyring>>,
    accepted: RefCell<Option<(BundleGeneration, [u8; 32], Vec<Vec<u8>>)>>,
}
impl BundleInstaller {
    pub fn new(keys: Rc<Keyring>) -> Self {
        Self {
            keys: RefCell::new(keys),
            accepted: RefCell::new(None),
        }
    }
    pub fn generation(&self) -> Option<BundleGeneration> {
        self.accepted
            .borrow()
            .as_ref()
            .map(|(generation, _, _)| *generation)
    }
    pub fn bind_keyring(&self, keys: Rc<Keyring>) {
        *self.keys.borrow_mut() = keys;
        *self.accepted.borrow_mut() = None;
    }
    pub fn install(&self, mut bundle: KeyringBundle) -> Result<(BundleGeneration, Vec<Vec<u8>>)> {
        bundle.peer_trust_roots.sort();
        bundle.cache_keys.sort_by(|a, b| {
            (&a.key.cache, a.key.purpose as u8, a.key.id.0).cmp(&(
                &b.key.cache,
                b.key.purpose as u8,
                b.key.id.0,
            ))
        });
        let encoded = zeroize::Zeroizing::new(wire::encode_bundle(&bundle)?);
        let hash: [u8; 32] = Sha256::digest(&*encoded).into();
        if let Some((generation, old, roots)) = self.accepted.borrow().as_ref() {
            if bundle.generation < *generation || bundle.generation == *generation && hash != *old {
                return Err(Error::Replay);
            }
            if bundle.generation == *generation {
                return Ok((*generation, roots.clone()));
            }
        }
        let roots = bundle.peer_trust_roots.clone();
        let generation = self.keys.borrow().install(bundle)?;
        *self.accepted.borrow_mut() = Some((generation, hash, roots.clone()));
        Ok((generation, roots))
    }
}

pub struct ControlEndpoint {
    pub url: String,
    /// Deployment-provided trust, independent of rotating peer trust roots.
    pub trust_bundle: PathBuf,
}
pub struct ControlClient {
    transport: ControlTransport,
    key_transport: ControlTransport,
    enrollment: Rc<Enrollment>,
    keys: RefCell<Rc<Keyring>>,
    secrets: BundleInstaller,
    keyring_busy: Cell<bool>,
    keyring_next: Cell<Option<Instant>>,
    keyring_failures: Cell<u32>,
    keyring_retry_after: Cell<Option<Duration>>,
    snapshots: Rc<SnapshotStore>,
    identity: RefCell<Option<LocalSigningIdentity>>,
    started: Cell<bool>,
    busy: Cell<bool>,
    enrollment_busy: Cell<bool>,
    poll_busy: Cell<bool>,
    stopped: Cell<bool>,
    failures: Cell<u32>,
    retry_after: Cell<Option<Duration>>,
    next: Cell<Option<Instant>>,
    active_scope: RefCell<Option<RequestScope>>,
    lifecycle: RefCell<Option<Rc<crate::app::CachePublication>>>,
    projection_error: Cell<Option<Error>>,
    renewal_error: Cell<Option<Error>>,
    renew_next: Cell<Option<Instant>>,
    startup_bundle: RefCell<Option<wire::KeyringBundle>>,
    binding_check: Cell<bool>,
    restart_required: Cell<bool>,
    pending: RefCell<Option<Rc<crate::control::PreparedPublication>>>,
    install_next: Cell<Option<Instant>>,
}
struct Busy<'a>(&'a Cell<bool>);
struct ActiveTurn<'a>(&'a RefCell<Option<RequestScope>>);
impl Drop for ActiveTurn<'_> {
    fn drop(&mut self) {
        self.0.borrow_mut().take();
    }
}
impl Drop for Busy<'_> {
    fn drop(&mut self) {
        self.0.set(false);
    }
}
fn enter(flag: &Cell<bool>) -> Result<Busy<'_>> {
    if flag.replace(true) {
        Err(Error::Overloaded)
    } else {
        Ok(Busy(flag))
    }
}
impl ControlClient {
    pub fn new(
        endpoint: ControlEndpoint,
        enrollment: Rc<Enrollment>,
        keys: Rc<Keyring>,
        secrets: BundleInstaller,
        snapshots: Rc<SnapshotStore>,
    ) -> Self {
        Self {
            key_transport: ControlTransport::new(ControlEndpoint {
                url: endpoint.url.clone(),
                trust_bundle: endpoint.trust_bundle.clone(),
            }),
            transport: ControlTransport::new(endpoint),
            keyring_busy: Cell::new(false),
            keyring_next: Cell::new(None),
            keyring_failures: Cell::new(0),
            keyring_retry_after: Cell::new(None),
            enrollment,
            keys: RefCell::new(keys),
            secrets,
            snapshots,
            identity: RefCell::new(None),
            started: Cell::new(false),
            busy: Cell::new(false),
            enrollment_busy: Cell::new(false),
            poll_busy: Cell::new(false),
            stopped: Cell::new(false),
            failures: Cell::new(0),
            retry_after: Cell::new(None),
            next: Cell::new(None),
            active_scope: RefCell::new(None),
            lifecycle: RefCell::new(None),
            projection_error: Cell::new(None),
            renewal_error: Cell::new(None),
            renew_next: Cell::new(None),
            startup_bundle: RefCell::new(None),
            binding_check: Cell::new(false),
            restart_required: Cell::new(false),
            pending: RefCell::new(None),
            install_next: Cell::new(None),
        }
    }
    /// Reads the projected token at submission; retries reuse the same request ID.
    pub fn enroll<'a>(
        &'a self,
        request: &'a EnrollmentRequest,
        scope: &'a RequestScope,
    ) -> Operation<'a, EnrollmentResponse> {
        Box::pin(async move {
            let _busy = enter(&self.enrollment_busy)?;
            if self.stopped.get() {
                return Err(Error::Cancelled);
            }
            let body = racer_control_wire::encode_enrollment_request(request)?;
            let connection = self.transport.bootstrap(scope).await?;
            let token = self.enrollment.read_token_async(scope).await?;
            let response = connection
                .request(
                    "POST",
                    wire::BOOTSTRAP_PATH,
                    Some(&token),
                    &body,
                    wire::MAX_ENROLLMENT_BYTES,
                    scope,
                )
                .await?;
            let body = self.response(response, false)?;
            Ok(wire::decode_enrollment_response(&body)?)
        })
    }
    /// Requires the locally activated certificate and matching local signing key.
    /// New/pooled TLS connections must not outlive client certificate validity.
    pub fn poll<'a>(
        &'a self,
        request: SnapshotRequest,
        scope: &'a RequestScope,
    ) -> Operation<'a, SnapshotResponse> {
        Box::pin(async move {
            let _busy = enter(&self.poll_busy)?;
            if self.stopped.get() {
                return Err(Error::Cancelled);
            }
            if self.restart_required.get() {
                return Err(Error::NodeIdentityChanged);
            }
            let identity = self.identity.borrow().clone().ok_or(Error::Unauthorized)?;
            let connection = self.transport.authenticated(&identity, scope).await?;
            let snapshot = self
                .pending
                .borrow()
                .as_ref()
                .map(|p| p.snapshot.clone())
                .or_else(|| self.snapshots.current().ok());
            let snapshot = snapshot.filter(|s| Some(s.sequence) == request.after);
            let hash = snapshot
                .as_ref()
                .map(|s| {
                    self.pending
                        .borrow()
                        .as_ref()
                        .filter(|p| p.snapshot.sequence == s.sequence)
                        .map(|p| Ok(p.content_hash()))
                        .unwrap_or_else(|| self.snapshots.content_hash(s.sequence))
                })
                .transpose()?;
            let path = match request.after {
                Some(n) => format!("{}?after={}", wire::SNAPSHOT_PATH, n.0),
                None => wire::SNAPSHOT_PATH.into(),
            };
            let response = connection
                .request_delta(
                    "GET",
                    &path,
                    None,
                    &[],
                    wire::MAX_PUBLICATION_BYTES,
                    hash.as_deref(),
                    scope,
                )
                .await?;
            if response.status == 204 {
                return Ok(SnapshotResponse::Unchanged);
            }
            let body = self.response(response, false)?;
            if let Ok(full) = racer_control_wire::decode_publication(&body) {
                return Ok(SnapshotResponse::Updated(full));
            }
            if let Some(s) = snapshot {
                let base = racer_control_wire::Publication {
                    schema_version: wire::SCHEMA_VERSION,
                    cluster: s.cluster.clone(),
                    sequence: s.sequence,
                    membership_version: s.membership.version,
                    members: s
                        .membership
                        .members()
                        .iter()
                        .cloned()
                        .map(Into::into)
                        .collect(),
                    caches: s.caches.clone(),
                };
                if let Ok(next) = racer_control_wire::apply_delta(&base, &body) {
                    return Ok(SnapshotResponse::Updated(next));
                }
            }
            // One full retry within the original scope, without advancing cursor.
            let response = self
                .transport
                .authenticated(&identity, scope)
                .await?
                .request(
                    "GET",
                    wire::SNAPSHOT_PATH,
                    None,
                    &[],
                    wire::MAX_PUBLICATION_BYTES,
                    scope,
                )
                .await?;
            Ok(SnapshotResponse::Updated(
                racer_control_wire::decode_publication(&self.response(response, false)?)?,
            ))
        })
    }
    /// Runtime must attach its owner-local reactor adapter before start.
    pub fn attach_io(&self, io: Rc<ReactorControlIo>) {
        self.enrollment.attach_reactor(io.reactor());
        self.key_transport.attach_io(io.clone());
        self.transport.attach_io(io);
    }
    pub(crate) fn attach_cache_publication(&self, lifecycle: Rc<crate::app::CachePublication>) {
        *self.lifecycle.borrow_mut() = Some(lifecycle);
    }
    pub fn projection_error(&self) -> Option<Error> {
        self.projection_error.get()
    }
    pub fn renewal_error(&self) -> Option<Error> {
        self.renewal_error.get()
    }
    pub fn next_attempt(&self) -> Option<Instant> {
        self.next.get()
    }
    fn backoff(&self) -> Result<Instant> {
        let failures = self.failures.get().saturating_add(1);
        self.failures.set(failures);
        let mut random = [0; 8];
        uring_runtime::environment::fill_random(&mut random).map_err(|_| Error::Io)?;
        let ceiling = (1u64 << failures.saturating_sub(1).min(5)).min(30) * 1000;
        let delay = Duration::from_millis(1000 + u64::from_ne_bytes(random) % (ceiling - 1000 + 1));
        let delay = delay.max(self.retry_after.take().unwrap_or_default());
        uring_runtime::environment::now()
            .checked_add(delay)
            .ok_or(Error::InvalidRequest)
    }
    /// Replace the initially unresolved worker-local view with one built from the
    /// same shared epochs and the authenticated startup NodeId, then activate it.
    pub fn bind_keyring(&self, keys: Rc<Keyring>) -> Result<()> {
        if self.restart_required.get() {
            return Err(Error::NodeIdentityChanged);
        }
        let identity = self.identity.borrow();
        let identity = identity.as_ref().ok_or(Error::Unauthorized)?;
        if keys.node() != identity.node() || keys.cluster() != identity.cluster() {
            return Err(Error::Unauthorized);
        }
        self.secrets.bind_keyring(keys.clone());
        if let Some(bundle) = self.startup_bundle.borrow().as_ref() {
            self.secrets.install(bundle.clone())?;
        }
        let signing = identity.signing_identity(&keys.peer_trust_roots()?)?;
        keys.install_signing_identity(signing)?;
        self.startup_bundle.borrow_mut().take();
        *self.keys.borrow_mut() = keys;
        Ok(())
    }
    /// Returns the authenticated, locally validated identity for unresolved startup.
    /// Keyring activation is deliberately deferred to activate_identity, allowing
    /// the integration owner to bind an initially unresolved Keyring NodeId first.
    pub fn start<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, LocalSigningIdentity> {
        Box::pin(async move {
            let _busy = enter(&self.busy)?;
            scope.check()?;
            if self.stopped.get() {
                return Err(Error::Cancelled);
            }
            if self.started.get() {
                if self.restart_required.get() {
                    return Err(Error::NodeIdentityChanged);
                }
                return self.identity.borrow().clone().ok_or(Error::Unauthorized);
            }
            if self
                .next
                .get()
                .is_some_and(|n| n > uring_runtime::environment::now())
            {
                return Err(Error::Unavailable);
            }
            let mut turn = scope.clone();
            turn.deadline.0 = turn
                .deadline
                .0
                .min(uring_runtime::environment::now() + Duration::from_secs(40));
            *self.active_scope.borrow_mut() = Some(turn.clone());
            let _turn = ActiveTurn(&self.active_scope);
            let result = self.start_inner(&turn).await;
            if result.as_ref().is_err_and(|e| transient(*e)) {
                self.next.set(Some(self.backoff()?));
            }
            result
        })
    }
    async fn start_inner(&self, scope: &RequestScope) -> Result<LocalSigningIdentity> {
        self.transport.io()?;
        let bundle = self
            .fetch_keyring(None, scope)
            .await?
            .ok_or(Error::InvalidRequest)?;
        if &bundle.cluster != self.enrollment.cluster() {
            return Err(Error::Unauthorized);
        }
        self.enrollment
            .set_peer_trust_roots(bundle.peer_trust_roots.clone())?;
        // Recover interrupted persistence, but a disk certificate is not evidence
        // of the current Kubernetes Node binding, even while it remains valid.
        self.enrollment.load_identity_async(scope).await?;
        let request = self.enrollment.prepare(scope).await?;
        let identity = self
            .enrollment
            .accept_response_async(self.enroll(&request, scope).await?, scope)
            .await?;
        *self.identity.borrow_mut() = Some(identity.clone());
        *self.startup_bundle.borrow_mut() = Some(bundle);
        self.started.set(true);
        self.next.set(Some(uring_runtime::environment::now()));
        Ok(identity)
    }
    pub fn activate_identity(&self) -> Result<()> {
        if self.restart_required.get() {
            return Err(Error::NodeIdentityChanged);
        }
        let identity = self.identity.borrow().clone().ok_or(Error::Unauthorized)?;
        let keys = self.keys.borrow();
        if keys.node() != identity.node() || keys.cluster() != identity.cluster() {
            return Err(Error::Unauthorized);
        }
        if let Some(bundle) = self.startup_bundle.borrow().as_ref() {
            self.secrets.install(bundle.clone())?;
        }
        keys.install_signing_identity(identity.signing_identity(&keys.peer_trust_roots()?)?)?;
        self.startup_bundle.borrow_mut().take();
        Ok(())
    }
    pub fn identity(&self) -> Option<LocalSigningIdentity> {
        self.identity.borrow().clone()
    }
    /// One bounded owner turn. Call again at next_attempt; only this owner polls.
    pub fn progress<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            let _busy = enter(&self.busy)?;
            scope.check()?;
            if self.stopped.get() {
                return Err(Error::Cancelled);
            }
            if !self.started.get() {
                return Err(Error::InvalidConfiguration);
            }
            if self.restart_required.get() {
                return Err(Error::NodeIdentityChanged);
            }
            if self.pending.borrow().is_some() {
                match self.install_pending() {
                    Ok(()) => return Ok(()),
                    Err(Error::Unavailable | Error::Overloaded | Error::Io) => (),
                    Err(error) => return Err(error),
                }
            }
            let mut turn = scope.clone();
            turn.deadline.0 = turn
                .deadline
                .0
                .min(uring_runtime::environment::now() + wire::POLL_WAIT + Duration::from_secs(10));
            *self.active_scope.borrow_mut() = Some(turn.clone());
            let _turn = ActiveTurn(&self.active_scope);
            let mut advance = Box::pin(self.advance(&turn));
            let result = std::future::poll_fn(|cx| {
                if self.pending.borrow().is_some()
                    && self
                        .install_next
                        .get()
                        .is_none_or(|next| next <= uring_runtime::environment::now())
                {
                    self.install_next.set(Some(
                        uring_runtime::environment::now() + Duration::from_millis(10),
                    ));
                    match self.install_pending() {
                        Ok(()) | Err(Error::Unavailable | Error::Overloaded | Error::Io) => (),
                        Err(error) => return std::task::Poll::Ready(Err(error)),
                    }
                }
                std::future::Future::poll(advance.as_mut(), cx)
            })
            .await;
            match result {
                Ok(()) => {
                    if self.renewal_error.get().is_none() {
                        self.failures.set(0);
                    }
                    if self
                        .next
                        .get()
                        .is_none_or(|next| next <= uring_runtime::environment::now())
                    {
                        self.next.set(Some(uring_runtime::environment::now()));
                    }
                    Ok(())
                }
                Err(e) => {
                    if transient(e) {
                        self.next.set(Some(
                            match self
                                .renew_next
                                .get()
                                .filter(|next| *next > uring_runtime::environment::now())
                            {
                                Some(next) => next,
                                None => self.backoff()?,
                            },
                        ));
                    }
                    Err(e)
                }
            }
        })
    }
    async fn advance(&self, scope: &RequestScope) -> Result<()> {
        // Key delivery has its own owner task and is never canceled by this turn.
        let renewal_done = Cell::new(false);
        let mut renewal = Box::pin(async {
            loop {
                self.renew_if_due(scope).await?;
                renewal_done.set(true);
                self.transport
                    .io()?
                    .sleep(
                        uring_runtime::environment::now() + Duration::from_secs(1),
                        scope,
                    )
                    .await?;
            }
            #[allow(unreachable_code)]
            Ok::<(), Error>(())
        });
        let mut polling = Box::pin(async {
            std::future::poll_fn(|_| {
                if self
                    .identity
                    .borrow()
                    .as_ref()
                    .is_some_and(|identity| identity.valid_now())
                {
                    std::task::Poll::Ready(())
                } else if renewal_done.get() {
                    std::task::Poll::Ready(())
                } else {
                    std::task::Poll::Pending
                }
            })
            .await;
            if self
                .next
                .get()
                .filter(|next| *next > uring_runtime::environment::now())
                .is_some()
            {
                return Ok(());
            }
            self.poll_publication(scope).await
        });
        let mut polled = None;
        std::future::poll_fn(|cx| {
            use std::future::Future;
            use std::task::Poll;
            if let Poll::Ready(result) = renewal.as_mut().poll(cx) {
                return Poll::Ready(result);
            }
            if polled.is_none() {
                if let Poll::Ready(result) = polling.as_mut().poll(cx) {
                    polled = Some(result);
                }
            }
            if renewal_done.get() {
                if let Some(result) = polled.take() {
                    return Poll::Ready(result);
                }
            }
            Poll::Pending
        })
        .await
    }
    /// One independent bounded keyring turn. Errors preserve accepted keys and
    /// the cursor, including conflicts from a controller behind our generation.
    pub fn keyring_progress<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            let _busy = enter(&self.keyring_busy)?;
            if self.stopped.get() {
                return Err(Error::Cancelled);
            }
            if self.restart_required.get() {
                return Err(Error::NodeIdentityChanged);
            }
            if let Some(next) = self.keyring_next.get() {
                self.key_transport.io()?.sleep(next, scope).await?;
            }
            let mut turn = scope.clone();
            turn.deadline.0 = turn
                .deadline
                .0
                .min(uring_runtime::environment::now() + wire::POLL_WAIT + Duration::from_secs(10));
            let result = async {
                if let Some(bundle) = self.fetch_keyring(self.secrets.generation(), &turn).await? {
                    turn.check()?;
                    let (_, roots) = self.secrets.install(bundle)?;
                    self.enrollment.set_peer_trust_roots(roots)?;
                }
                Ok::<(), Error>(())
            }
            .await;
            self.projection_error.set(result.err());
            let delay = if result.is_err() {
                let failures = self.keyring_failures.get().saturating_add(1);
                self.keyring_failures.set(failures);
                let mut random = [0; 8];
                uring_runtime::environment::fill_random(&mut random).map_err(|_| Error::Io)?;
                let ceiling = (1u64 << failures.min(5)).min(30) * 1000;
                Duration::from_millis(1000 + u64::from_ne_bytes(random) % (ceiling - 999))
            } else {
                self.keyring_failures.set(0);
                Duration::from_millis(10)
            };
            let delay = delay.max(self.keyring_retry_after.take().unwrap_or_default());
            self.keyring_next
                .set(uring_runtime::environment::now().checked_add(delay));
            scope.check()
        })
    }
    async fn fetch_keyring(
        &self,
        after: Option<wire::BundleGeneration>,
        scope: &RequestScope,
    ) -> Result<Option<wire::KeyringBundle>> {
        scope.check()?;
        if self.stopped.get() {
            return Err(Error::Cancelled);
        }
        if self.restart_required.get() {
            return Err(Error::NodeIdentityChanged);
        }
        let path = after.map_or_else(
            || wire::KEYRING_PATH.to_owned(),
            |n| format!("{}?after={}", wire::KEYRING_PATH, n.0),
        );
        let identity = self.identity.borrow().clone().filter(|i| i.valid_now());
        if let Some(identity) = identity {
            let result = async {
                let response = self
                    .key_transport
                    .authenticated(&identity, scope)
                    .await?
                    .request("GET", &path, None, &[], wire::MAX_BUNDLE_BYTES, scope)
                    .await?;
                self.keyring_response(response, after)
            }
            .await;
            match result {
                Err(Error::Unauthorized) => {
                    self.binding_check.set(true);
                    self.key_transport.close_idle();
                }
                other => return other,
            }
        }
        // Recovery is a fresh server-authenticated TLS connection, never an
        // insecure retry. No token is sent until the mounted CA and name verify.
        let connection = self.key_transport.bootstrap(scope).await?;
        let token = self.enrollment.read_token_async(scope).await?;
        let response = connection
            .request(
                "GET",
                &path,
                Some(&token),
                &[],
                wire::MAX_BUNDLE_BYTES,
                scope,
            )
            .await?;
        self.keyring_response(response, after)
    }
    fn keyring_response(
        &self,
        response: Response,
        after: Option<wire::BundleGeneration>,
    ) -> Result<Option<wire::KeyringBundle>> {
        if response.status == 204 {
            return if after.is_some() && response.body.is_empty() {
                Ok(None)
            } else {
                Err(Error::InvalidRequest)
            };
        }
        let bytes = zeroize::Zeroizing::new(Self::response_with_retry(
            response,
            false,
            &self.keyring_retry_after,
        )?);
        let bundle = wire::decode_bundle(&bytes)?;
        if after.is_some_and(|n| bundle.generation < n) {
            return Err(Error::Replay);
        }
        Ok(Some(bundle))
    }
    async fn renew_if_due(&self, scope: &RequestScope) -> Result<()> {
        let renewal = self.binding_check.get()
            || self
                .identity
                .borrow()
                .as_ref()
                .is_none_or(|i| i.renewal_due());
        if renewal
            && self
                .renew_next
                .get()
                .is_none_or(|n| n <= uring_runtime::environment::now())
        {
            match self.renew(scope).await {
                Ok(()) => {
                    self.binding_check.set(false);
                    self.renewal_error.set(None);
                    self.renew_next.set(None);
                }
                Err(e) => {
                    self.renewal_error.set(Some(e));
                    if !transient(e) && e != Error::Unauthorized {
                        return Err(e);
                    }
                    self.renew_next.set(Some(self.backoff()?));
                    if self
                        .identity
                        .borrow()
                        .as_ref()
                        .is_none_or(|i| !i.valid_now())
                    {
                        return Err(if e == Error::Unauthorized {
                            Error::Unavailable
                        } else {
                            e
                        });
                    }
                    // Already accepted identity remains useful while renewal retries.
                }
            }
        }
        Ok(())
    }
    async fn poll_publication(&self, scope: &RequestScope) -> Result<()> {
        let after = self
            .pending
            .borrow()
            .as_ref()
            .map(|p| p.snapshot.sequence)
            .or(self.snapshots.cursor()?);
        let polled = self.poll(SnapshotRequest { after }, scope).await;
        // A lagging replica's 503 does not invalidate the accepted Node binding.
        // Explicit authentication rejection still triggers bounded enrollment.
        if matches!(polled, Err(Error::Unauthorized)) {
            self.binding_check.set(true);
            return Err(Error::Unavailable);
        }
        match polled? {
            SnapshotResponse::Updated(publication) => {
                scope.check()?;
                let prepared = self.snapshots.prepare(publication)?;
                if self
                    .pending
                    .borrow()
                    .as_ref()
                    .is_none_or(|old| prepared.snapshot.sequence > old.snapshot.sequence)
                {
                    *self.pending.borrow_mut() = Some(Rc::new(prepared));
                }
                match self.install_pending() {
                    Ok(()) | Err(Error::Unavailable | Error::Overloaded | Error::Io) => (),
                    Err(error) => return Err(error),
                }
            }
            SnapshotResponse::Unchanged => (),
        }
        Ok(())
    }
    /// Retry prepared immutable state independently of remote progress.
    fn install_pending(&self) -> Result<()> {
        let publication = self
            .pending
            .borrow()
            .as_ref()
            .cloned()
            .ok_or(Error::Internal)?;
        let transition = self
            .lifecycle
            .borrow()
            .as_ref()
            .map(|l| l.stage(&publication.snapshot.caches))
            .transpose()?;
        self.snapshots.publish_prepared(&publication, transition)?;
        self.pending.borrow_mut().take();
        self.next.set(Some(uring_runtime::environment::now()));
        Ok(())
    }
    async fn renew(&self, scope: &RequestScope) -> Result<()> {
        let request = self.enrollment.prepare(scope).await?;
        let response = self.enroll(&request, scope).await?;
        self.accept_renewal(response, scope).await
    }
    // Only called with a response from server-authenticated token enrollment.
    async fn accept_renewal(
        &self,
        response: EnrollmentResponse,
        scope: &RequestScope,
    ) -> Result<()> {
        let changed = self
            .identity
            .borrow()
            .as_ref()
            .is_some_and(|old| old.node() != &response.node);
        // The response arrived over authenticated TLS. Latch before persistence:
        // cancellation or a failed fsync after rename must also stop this graph.
        // Enrollment still validates all evidence before committing any identity.
        self.restart_required.set(changed);
        let accepted = self.enrollment.accept_response_async(response, scope).await;
        if changed {
            // No new identity enters old keyrings, snapshots, replay, or sessions.
            // Startup reauthenticates whether persistence succeeded or failed.
            return Err(Error::NodeIdentityChanged);
        }
        let identity = accepted?;
        let keys = self.keys.borrow();
        keys.install_signing_identity(identity.signing_identity(&keys.peer_trust_roots()?)?)?;
        *self.identity.borrow_mut() = Some(identity);
        Ok(())
    }
    /// Diagnostic only. Pending receipt is not acceptance or worker application.
    pub(crate) fn membership_diagnostic(&self) -> Result<crate::telemetry::MembershipDiagnostic> {
        let (sequence, membership, hash) = self.snapshots.accepted_identity()?;
        let pending = self.pending.borrow();
        Ok(crate::telemetry::MembershipDiagnostic {
            accepted_sequence: sequence,
            accepted_membership: membership,
            accepted_hash: hash,
            pending_sequence: pending.as_ref().map_or(0, |p| p.snapshot.sequence.0),
            pending_membership: pending
                .as_ref()
                .map_or(0, |p| p.snapshot.membership.version.0),
            ..Default::default()
        })
    }
    fn response(&self, response: Response, unchanged: bool) -> Result<Vec<u8>> {
        Self::response_with_retry(response, unchanged, &self.retry_after)
    }
    fn response_with_retry(
        mut response: Response,
        unchanged: bool,
        retry_after: &Cell<Option<Duration>>,
    ) -> Result<Vec<u8>> {
        if response.status == 200 || unchanged && response.status == 204 {
            return Ok(std::mem::take(&mut response.body));
        }
        let failure = wire::decode_error(&response.body)?.code;
        use wire::ProtocolFailure::*;
        let (status, error) = match failure {
            InvalidRequest => (400, Error::InvalidRequest),
            Unauthenticated => (401, Error::Unauthorized),
            Forbidden => (403, Error::Unauthorized),
            Conflict => (409, Error::Replay),
            TooLarge => (413, Error::InvalidRequest),
            UnsupportedVersion => (426, Error::IncompatibleMembership),
            Overloaded => (429, Error::Overloaded),
            Unavailable => (503, Error::Unavailable),
        };
        if status != response.status {
            return Err(Error::InvalidRequest);
        }
        if matches!(status, 429 | 503) {
            retry_after.set(response.retry_after);
        }
        Err(error)
    }
    /// Cancels the current owner turn; dropping its future closes its TLS socket.
    pub fn shutdown(&self) -> Result<()> {
        self.stopped.set(true);
        self.transport.close_idle();
        self.key_transport.close_idle();
        if let Some(scope) = self.active_scope.borrow().as_ref() {
            scope.cancel()?;
        }
        self.identity.borrow_mut().take();
        Ok(())
    }
}
fn transient(e: Error) -> bool {
    matches!(
        e,
        Error::Io | Error::Unavailable | Error::Overloaded | Error::DeadlineExceeded
    )
}

// Validate and atomically publish complete immutable state; retain last good state.

impl From<wire::Error> for Error {
    fn from(error: wire::Error) -> Self {
        match error {
            wire::Error::InvalidRequest => Self::InvalidRequest,
            wire::Error::IncompatibleMembership => Self::IncompatibleMembership,
            wire::Error::Overloaded => Self::Overloaded,
            wire::Error::Replay => Self::Replay,
        }
    }
}

pub struct Snapshot {
    pub cluster: ClusterId,
    pub sequence: PublicationSequence,
    pub membership: std::sync::Arc<crate::topology::Membership>,
    pub caches: Vec<CacheDefinition>,
}
pub struct PreparedPublication {
    pub(crate) snapshot: std::sync::Arc<crate::control::Snapshot>,
    content_hash: [u8; 32],
    membership_hash: [u8; 32],
}
impl PreparedPublication {
    pub fn content_hash(&self) -> String {
        self.content_hash
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect()
    }
}
/// One node-wide publication cell. Its implementation publishes immutable leases
/// atomically; worker handles never become independent authorities for membership.
#[derive(Default)]
pub struct PublishedState {
    state: Mutex<State>,
}
#[derive(Default)]
struct State {
    current: Option<std::sync::Arc<crate::control::Snapshot>>,
    memberships: Vec<(MembershipVersion, Weak<Membership>)>,
    content_hash: [u8; 32],
    membership_hash: [u8; 32],
    grace: Vec<(
        std::time::Instant,
        std::sync::Arc<crate::topology::Membership>,
    )>,
}
impl PublishedState {
    /// Incoming wire versions resolve here once, then travel as operation leases.
    /// Weak entries never prolong a generation's lifetime and publication prunes
    /// dead entries before admission, bounding the registry as well as live state.
    pub fn membership(
        &self,
        version: MembershipVersion,
    ) -> Result<std::sync::Arc<crate::topology::Membership>> {
        let mut state = self.state.lock().map_err(|_| Error::Unavailable)?;
        let now = uring_runtime::environment::now();
        state.grace.retain(|(until, _)| *until > now);
        state
            .memberships
            .iter()
            .find(|(v, _)| *v == version)
            .and_then(|(_, membership)| membership.upgrade())
            .ok_or(Error::IncompatibleMembership)
    }

    #[cfg(test)]
    pub(crate) fn for_membership(
        membership: std::sync::Arc<crate::topology::Membership>,
    ) -> Arc<Self> {
        Arc::new(Self {
            state: Mutex::new(State {
                memberships: vec![(membership.version, Arc::downgrade(&membership))],
                current: Some(Arc::new(Snapshot {
                    cluster: ClusterId("test".into()),
                    sequence: PublicationSequence(1),
                    membership,
                    caches: vec![],
                })),
                content_hash: [0; 32],
                membership_hash: [0; 32],
                grace: Vec::new(),
            }),
        })
    }

    pub fn current(&self) -> Result<std::sync::Arc<crate::control::Snapshot>> {
        self.state
            .lock()
            .map_err(|_| Error::Unavailable)?
            .current
            .clone()
            .ok_or(Error::Unavailable)
    }
}
pub struct SnapshotStore {
    cluster: ClusterId,
    published: Arc<PublishedState>,
    /// Maximum old live membership generations, in addition to the current one.
    retained_limit: usize,
}
impl SnapshotStore {
    /// Read-only coherent accepted identity; never describes a prepared update.
    pub(crate) fn accepted_identity(&self) -> Result<(u64, u64, [u8; 32])> {
        let state = self
            .published
            .state
            .lock()
            .map_err(|_| Error::Unavailable)?;
        let current = state.current.as_ref().ok_or(Error::Unavailable)?;
        Ok((
            current.sequence.0,
            current.membership.version.0,
            state.membership_hash,
        ))
    }

    pub fn new(cluster: ClusterId, published: Arc<PublishedState>, retained_limit: usize) -> Self {
        Self {
            cluster,
            published,
            retained_limit,
        }
    }
    /// Cursor advances only after complete validation and atomic acceptance.
    pub fn cursor(&self) -> Result<Option<PublicationSequence>> {
        Ok(self
            .published
            .state
            .lock()
            .map_err(|_| Error::Unavailable)?
            .current
            .as_ref()
            .map(|s| s.sequence))
    }
    pub fn current(&self) -> Result<std::sync::Arc<crate::control::Snapshot>> {
        self.published.current()
    }
    pub fn content_hash(&self, sequence: PublicationSequence) -> Result<String> {
        let state = self
            .published
            .state
            .lock()
            .map_err(|_| Error::Unavailable)?;
        if state
            .current
            .as_ref()
            .is_none_or(|s| s.sequence != sequence)
        {
            return Err(Error::Replay);
        }
        Ok(state
            .content_hash
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect())
    }
    /// Reject cluster mismatch, rollback, conflicting replay, or invalid members.
    /// Skipped sequences are legal; disconnected nodes retain the last good state.
    pub fn publish(
        &self,
        publication: Publication,
    ) -> Result<std::sync::Arc<crate::control::Snapshot>> {
        self.publish_staged(publication, None)
    }
    /// Commit prepared cache resources after every fallible validation, before the
    /// new immutable publication becomes visible to any reader of the shared cell.
    pub fn publish_staged(
        &self,
        publication: Publication,
        transition: Option<Box<dyn CacheTransition>>,
    ) -> Result<std::sync::Arc<crate::control::Snapshot>> {
        let prepared = self.prepare(publication)?;
        self.publish_prepared(&prepared, transition)
    }
    pub fn prepare(&self, publication: Publication) -> Result<PreparedPublication> {
        if publication.cluster != self.cluster {
            return Err(Error::Unauthorized);
        }
        let publication = validate_publication(&publication)?;
        let (content, membership) = canonical_content(&publication)?;
        let content_hash: [u8; 32] = Sha256::digest(content).into();
        let membership_hash: [u8; 32] = Sha256::digest(membership).into();
        let current = self.current().ok();
        let mut validated = Membership::validate(
            publication.membership_version,
            publication.members.into_iter().map(Member::from).collect(),
        )?;
        if let Some(current) = &current {
            validated = validated.with_predecessor(&current.membership);
        }
        let mut next = Snapshot {
            cluster: publication.cluster,
            sequence: publication.sequence,
            membership: Arc::new(validated),
            caches: publication.caches,
        };
        if let Some(current) = current {
            if current.membership.version == next.membership.version {
                next.membership = current.membership.clone();
            }
        }
        let prepared = PreparedPublication {
            snapshot: Arc::new(next),
            content_hash,
            membership_hash,
        };
        self.check_prepared(&prepared)?;
        Ok(prepared)
    }
    fn check_prepared(&self, prepared: &PreparedPublication) -> Result<()> {
        let state = self
            .published
            .state
            .lock()
            .map_err(|_| Error::Unavailable)?;
        Self::check_state(&state, prepared)
    }
    fn check_state(state: &State, prepared: &PreparedPublication) -> Result<()> {
        let next = &prepared.snapshot;
        if let Some(old) = &state.current {
            if next.sequence < old.sequence || next.membership.version.0 < old.membership.version.0
            {
                return Err(Error::Replay);
            }
            if next.sequence == old.sequence
                && (prepared.content_hash != state.content_hash
                    || next.membership.version != old.membership.version)
            {
                return Err(Error::Replay);
            }
            if next.membership.version == old.membership.version
                && prepared.membership_hash != state.membership_hash
            {
                return Err(Error::IncompatibleMembership);
            }
        }
        Ok(())
    }
    pub fn publish_prepared(
        &self,
        prepared: &PreparedPublication,
        transition: Option<Box<dyn CacheTransition>>,
    ) -> Result<std::sync::Arc<crate::control::Snapshot>> {
        let mut state = self
            .published
            .state
            .lock()
            .map_err(|_| Error::Unavailable)?;
        Self::check_state(&state, prepared)?;
        let publication = &prepared.snapshot;
        if let Some(old) = state
            .current
            .as_ref()
            .filter(|old| old.sequence == publication.sequence)
        {
            return Ok(old.clone());
        }
        let now = uring_runtime::environment::now();
        state.grace.retain(|(until, _)| *until > now);
        // Grace-only owners are disposable under the configured generation bound.
        // Externally pinned operations still block replacement rather than revoke.
        while state.memberships.len() > self.retained_limit && !state.grace.is_empty() {
            let Some(index) = state
                .grace
                .iter()
                .position(|(_, m)| Arc::strong_count(m) == 1)
            else {
                break;
            };
            state.grace.remove(index);
            state.memberships.retain(|(_, m)| m.strong_count() != 0);
        }
        state.memberships.retain(|(_, m)| m.strong_count() != 0);
        let same_membership = state
            .current
            .as_ref()
            .filter(|s| s.membership.version == publication.membership.version);
        let membership = if let Some(current) = same_membership {
            current.membership.clone()
        } else {
            // Only the current publication's sole structural lease can disappear
            // on replacement. External snapshot and membership leases both count.
            // Resolution and admission share this lock, so an incoming request
            // cannot acquire that lease between this check and replacement.
            let replaceable = state.current.as_ref().is_some_and(|s| {
                Arc::strong_count(s) == 1 && Arc::strong_count(&s.membership) == 1
            });
            if state.memberships.len() - usize::from(replaceable) > self.retained_limit {
                return Err(Error::Overloaded);
            }
            publication.membership.clone()
        };
        // Preparation shares the current membership for cache-only updates. A
        // concurrently replaced base must be prepared again, never cloned here.
        if !Arc::ptr_eq(&membership, &publication.membership) {
            return Err(Error::Replay);
        }
        let next = publication.clone();
        if let Some(transition) = transition {
            transition.commit();
        }
        if self.retained_limit >= 2 {
            if let Some(old) = &state.current {
                if old.membership.version != next.membership.version {
                    let old = old.membership.clone();
                    state
                        .grace
                        .push((now + std::time::Duration::from_secs(30), old));
                    while state.grace.len() > self.retained_limit
                        || state
                            .grace
                            .iter()
                            .map(|(_, m)| m.retained_bytes())
                            .sum::<usize>()
                            > 128 * 1024 * 1024
                    {
                        state.grace.remove(0);
                    }
                }
            }
        }
        state.current = Some(next.clone());
        state.memberships.retain(|(_, m)| m.strong_count() != 0);
        if !state
            .memberships
            .iter()
            .any(|(v, _)| *v == next.membership.version)
        {
            state
                .memberships
                .push((next.membership.version, Arc::downgrade(&next.membership)));
        }
        state.content_hash = prepared.content_hash;
        state.membership_hash = prepared.membership_hash;
        Ok(next)
    }
}
/// Cache definitions. Removal closes new admission while
/// accepted socket, key, and I/O owners drain independently.
///
/// Socket paths are fixed: /run/racer/<cache name>/client/socket and
/// /run/racer/<cache name>/origin/socket. Separate endpoint directories let pods
/// mount only the endpoint authorized by a future admission controller. The
/// dataplane owns the client listener; the application adapter owns the origin
/// listener. Never unlink an adapter-owned origin socket during cache removal.
/// A sole owner stages all listener/resource changes before publication. Dropping
/// an uncommitted transition must undo preparation. Commit cannot fail; removal
/// stops admission and arranges drain/fences before releasing old resources.
pub trait CacheTransition {
    fn commit(self: Box<Self>);
}

/// Current positive admission set, shared by cache lookups and late publications.
/// No removal history is needed: an absent UID/key is a miss. Reintroducing a UID
/// denotes the same immutable namespace; a different namespace requires a new UID.
pub struct Availability {
    publications: Arc<PublishedState>,
    keys: Rc<Keyring>,
}
impl Availability {
    pub fn new(publications: Arc<PublishedState>, keys: Rc<Keyring>) -> Self {
        Self { publications, keys }
    }
    pub fn cache(&self, cache: &CacheId) -> bool {
        self.publications
            .current()
            .is_ok_and(|s| s.caches.iter().any(|c| &c.id == cache))
    }
    pub fn metadata(&self, cache: &CacheId) -> bool {
        self.cache(cache) && self.keys.active(cache, KeyPurpose::Page).is_ok()
    }
    pub fn page(&self, cache: &CacheId, key: KeyId) -> bool {
        self.cache(cache) && self.keys.lease(Some(cache), key, KeyPurpose::Page).is_ok()
    }
}

#[cfg(test)]
pub(crate) fn for_caches(keys: Rc<Keyring>, caches: Vec<CacheId>) -> Rc<Availability> {
    use racer_control_wire::SCHEMA_VERSION;
    let publications = Arc::new(PublishedState::default());
    SnapshotStore::new(keys.cluster().clone(), publications.clone(), 1)
        .publish(Publication {
            schema_version: SCHEMA_VERSION,
            cluster: keys.cluster().clone(),
            sequence: PublicationSequence(1),
            membership_version: racer_control_wire::MembershipVersion(1),
            members: vec![wire::Member {
                node: keys.node().clone(),
                shares: std::num::NonZeroU32::new(1).unwrap(),
                peer_endpoint: "127.0.0.1:7443".into(),
                rails: vec![],
                site: String::new(),
            }],
            caches: caches
                .into_iter()
                .enumerate()
                .map(|(i, id)| {
                    let name = format!("rotation-{i}");
                    let (client_socket, origin_socket) =
                        wire::canonical_socket_paths(&name).unwrap();
                    CacheDefinition {
                        id,
                        name,
                        client_socket,
                        origin_socket,
                    }
                })
                .collect(),
        })
        .unwrap();
    Rc::new(Availability::new(publications, keys))
}

// Racer policy and health around the generic worker-local REST transport.

/// Racer's owner-local filesystem, timer, admission, and readiness adapter.
pub struct ReactorControlIo {
    reactor: Rc<crate::runtime::Reactor>,
    #[cfg(test)]
    connect_probe: Option<Rc<tests::scenarios::ConnectProbe>>,
}
impl rest_client::Io for ReactorControlIo {
    type Error = Error;
    type Scope = RequestScope;
    type Lease = crate::admission::ConnectionReservation;

    fn lease(&self) -> Result<Option<Rc<Self::Lease>>> {
        self.reactor
            .reserve_connection(crate::admission::ResourceClass::ControlConnection)
            .map(|lease| Some(Rc::new(lease)))
    }
    fn ready<'a>(
        &'a self,
        fd: Rc<Descriptor>,
        read: bool,
        write: bool,
        lease: Option<Rc<Self::Lease>>,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        Box::pin(async move {
            #[cfg(test)]
            if let Some(probe) = &self.connect_probe {
                if probe.suppress(&fd, read, write, &lease, scope)? {
                    let mut wait = scope.clone();
                    if probe.mode == "parent" {
                        wait.deadline.0 = probe.parent_deadline;
                    }
                    return self
                        .sleep(wait.deadline.0 + std::time::Duration::from_millis(1), &wait)
                        .await;
                }
            }
            let interest =
                if read { libc::POLLIN } else { 0 } | if write { libc::POLLOUT } else { 0 };
            self.reactor
                .readiness_with_lease(fd, interest as u32, lease, scope)
                .await?;
            scope.check()
        })
    }
    fn resolve<'a>(
        &'a self,
        host: &'a str,
        port: u16,
        scope: &'a RequestScope,
    ) -> Operation<'a, Vec<SocketAddr>> {
        Box::pin(async move {
            #[cfg(test)]
            if let Some(probe) = &self.connect_probe {
                return Ok(probe.addresses.to_vec());
            }
            rest_client::dns::resolve(self, host, port, scope).await
        })
    }
    fn read_file<'a>(
        &'a self,
        path: &'a std::path::Path,
        limit: usize,
        scope: &'a RequestScope,
    ) -> Operation<'a, zeroize::Zeroizing<Vec<u8>>> {
        Box::pin(async move { crate::control::read_path(&self.reactor, path, limit, scope).await })
    }
}

impl ReactorControlIo {
    pub fn new(reactor: Rc<crate::runtime::Reactor>) -> Self {
        Self {
            reactor,
            #[cfg(test)]
            connect_probe: None,
        }
    }
    pub(crate) fn reactor(&self) -> Rc<crate::runtime::Reactor> {
        self.reactor.clone()
    }
    pub fn sleep<'a>(&'a self, until: Instant, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            scope.check()?;
            let duration = until.saturating_duration_since(uring_runtime::environment::now());
            if duration.is_zero() {
                return Ok(());
            }
            #[cfg(test)]
            if uring_runtime::reactor::simulation::Simulation::current().is_some() {
                return std::future::poll_fn(|cx| {
                    scope.check()?;
                    if uring_runtime::environment::now() >= until {
                        std::task::Poll::Ready(Ok(()))
                    } else {
                        cx.waker().wake_by_ref();
                        std::task::Poll::Pending
                    }
                })
                .await;
            }
            let raw = unsafe {
                libc::timerfd_create(
                    libc::CLOCK_MONOTONIC,
                    libc::TFD_CLOEXEC | libc::TFD_NONBLOCK,
                )
            };
            if raw < 0 {
                return Err(Error::Io);
            }
            let fd = Rc::new(unsafe { Descriptor::from_raw_fd(raw) });
            let interval = libc::itimerspec {
                it_interval: libc::timespec {
                    tv_sec: 0,
                    tv_nsec: 0,
                },
                it_value: libc::timespec {
                    tv_sec: duration
                        .as_secs()
                        .try_into()
                        .map_err(|_| Error::InvalidRequest)?,
                    tv_nsec: duration.subsec_nanos() as _,
                },
            };
            if unsafe { libc::timerfd_settime(fd.as_raw_fd(), 0, &interval, std::ptr::null_mut()) }
                != 0
            {
                return Err(Error::Io);
            }
            self.reactor
                .readiness(fd, libc::POLLIN as u32, scope)
                .await?;
            scope.check()
        })
    }
}

pub struct ControlTransport {
    health: Rc<crate::topology::LinkHealth>,
    endpoint: racer_control_wire::NodeId,
    inner: rest_client::Transport<ReactorControlIo>,
}
pub struct ControlConnection {
    health: Rc<crate::topology::LinkHealth>,
    endpoint: racer_control_wire::NodeId,
    inner: rest_client::Connection<ReactorControlIo>,
}
impl ControlTransport {
    pub fn new(endpoint: ControlEndpoint) -> Self {
        Self {
            health: Rc::new(crate::topology::LinkHealth::new(1)),
            endpoint: racer_control_wire::NodeId(endpoint.url.clone()),
            inner: rest_client::Transport::new(rest_client::Config {
                url: endpoint.url,
                trust_bundle: endpoint.trust_bundle,
                max_trust_bundle: wire::MAX_BUNDLE_BYTES,
                max_error_body: wire::MAX_ENROLLMENT_BYTES,
            }),
        }
    }
    pub fn attach_io(&self, io: Rc<ReactorControlIo>) {
        self.inner.attach_io(io);
    }
    pub fn io(&self) -> Result<Rc<ReactorControlIo>> {
        self.inner.io()
    }
    pub fn close_idle(&self) {
        self.inner.close_idle();
    }
    pub fn bootstrap<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, ControlConnection> {
        self.connect(None, scope)
    }
    pub fn authenticated<'a>(
        &'a self,
        identity: &'a LocalSigningIdentity,
        scope: &'a RequestScope,
    ) -> Operation<'a, ControlConnection> {
        self.connect(Some(identity), scope)
    }
    fn connect<'a>(
        &'a self,
        identity: Option<&'a LocalSigningIdentity>,
        scope: &'a RequestScope,
    ) -> Operation<'a, ControlConnection> {
        Box::pin(async move {
            self.health
                .run(
                    &self.endpoint,
                    Box::pin(async move {
                        scope.check()?;
                        if identity.is_some_and(|i| !i.valid_now()) {
                            return Err(Error::Unauthorized);
                        }
                        let identity = identity.map(|i| rest_client::Identity {
                            certificate_chain: i.certificate_chain(),
                            private_key: i.private_key_der(),
                            expires: i.expires_at(),
                        });
                        let inner = self.inner.connect(identity, scope).await?;
                        Ok(ControlConnection {
                            health: self.health.clone(),
                            endpoint: self.endpoint.clone(),
                            inner,
                        })
                    }),
                )
                .await
        })
    }
}
impl ControlConnection {
    pub fn request<'a>(
        self,
        method: &'a str,
        path: &'a str,
        token: Option<&'a str>,
        body: &'a [u8],
        limit: usize,
        scope: &'a RequestScope,
    ) -> Operation<'a, Response> {
        self.request_delta(method, path, token, body, limit, None, scope)
    }
    pub fn request_delta<'a>(
        self,
        method: &'a str,
        path: &'a str,
        token: Option<&'a str>,
        body: &'a [u8],
        limit: usize,
        base: Option<&'a str>,
        scope: &'a RequestScope,
    ) -> Operation<'a, Response> {
        Box::pin(async move {
            self.health
                .run(
                    &self.endpoint,
                    Box::pin(async move {
                        let method = match method {
                            "GET" => rest_client::Method::Get,
                            "POST" => rest_client::Method::Post,
                            _ => return Err(Error::InvalidRequest),
                        };
                        if base.is_some_and(|b| {
                            b.len() != 64 || !b.bytes().all(|b| b.is_ascii_hexdigit())
                        }) {
                            return Err(Error::InvalidRequest);
                        }
                        self.inner
                            .request(
                                rest_client::Request {
                                    method,
                                    path,
                                    bearer: token,
                                    header: base.map(|base| ("X-Racer-Delta-Base", base)),
                                    body,
                                    limit,
                                },
                                scope,
                            )
                            .await
                    }),
                )
                .await
        })
    }
}

// Generate node-private Ed25519 keys locally and enroll/rotate with node-bound SA identity.
pub struct Enrollment {
    inventory: Arc<crate::rdma::Inventory>,
    inventory_restored: Cell<bool>,
    shares: Cell<u32>,
    cluster: ClusterId,
    token_path: PathBuf,
    identity_directory: PathBuf,
    roots: RefCell<Vec<Vec<u8>>>,
    reactor: RefCell<Option<Rc<crate::runtime::Reactor>>>,
    busy: Cell<bool>,
    previous: Cell<Option<crate::model::RequestId>>,
}
/// Non-exportable signing identity. Do not share private keys through cluster Secrets.
#[derive(Clone)]
pub struct LocalSigningIdentity {
    cluster: ClusterId,
    node: NodeId,
    enrollment: EnrollmentId,
    private_material: Vec<u8>,
    certificate_chain: Vec<Vec<u8>>,
    not_before: u64,
    not_after: u64,
}
impl Drop for LocalSigningIdentity {
    fn drop(&mut self) {
        self.private_material.zeroize();
    }
}
#[derive(Serialize, Deserialize)]
struct PendingIdentity {
    cluster: String,
    enrollment: String,
    private_key: String,
    csr: String,
}
impl Drop for PendingIdentity {
    fn drop(&mut self) {
        self.private_key.zeroize();
    }
}
#[derive(Serialize, Deserialize)]
struct PersistedIdentity {
    pending: PendingIdentity,
    response: String,
}
impl PersistedIdentity {
    fn decode(bytes: &[u8]) -> Result<Self> {
        serde_json::from_value(wire::strict_json(bytes, wire::MAX_ENROLLMENT_BYTES * 3)?)
            .map_err(|_| Error::CorruptRecord)
    }

    fn response(&self) -> Result<EnrollmentResponse> {
        Ok(wire::decode_enrollment_response(
            &STANDARD
                .decode(&self.response)
                .map_err(|_| Error::CorruptRecord)?,
        )?)
    }

    fn encode(&self) -> Result<Zeroizing<Vec<u8>>> {
        Ok(Zeroizing::new(
            serde_json::to_vec(self).map_err(|_| Error::Io)?,
        ))
    }
}
fn token(b: &[u8]) -> Result<Zeroizing<String>> {
    let token = Zeroizing::new(
        std::str::from_utf8(b)
            .map_err(|_| Error::Unauthorized)?
            .trim()
            .to_owned(),
    );
    if token.is_empty()
        || !token
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'-' | b'_' | b'.'))
    {
        return Err(Error::Unauthorized);
    }
    Ok(token)
}
impl Enrollment {
    pub fn new(cluster: ClusterId, token_path: PathBuf, identity_directory: PathBuf) -> Self {
        Self {
            inventory: Arc::new(Default::default()),
            inventory_restored: Cell::new(false),
            shares: Cell::new(4),
            cluster,
            token_path,
            identity_directory,
            roots: RefCell::new(Vec::new()),
            reactor: RefCell::new(None),
            busy: Cell::new(false),
            previous: Cell::new(None),
        }
    }
    pub fn set_shares(&self, shares: std::num::NonZeroU32) {
        self.shares.set(shares.get());
    }
    pub fn with_inventory(mut self, inventory: Arc<crate::rdma::Inventory>) -> Self {
        self.inventory = inventory;
        self
    }
    /// Persist a fresh private key and retry-stable request before submission.
    /// All issuance uses the projected token, including lifetime-based renewal.
    pub fn prepare<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, EnrollmentRequest> {
        Box::pin(async move {
            let r = self.reactor()?;
            let (_guard, scope) = self.begin(&r, scope).await?;
            let dir =
                crate::control::directory(&r, &self.identity_directory, true, true, &scope).await?;
            if !self.inventory_restored.get() {
                match crate::control::read_at(
                    &r,
                    &dir,
                    "rdma-rails.json",
                    crate::rdma::MAX_JOURNAL_BYTES,
                    true,
                    &scope,
                )
                .await
                {
                    Ok(bytes) => self.inventory.restore(&bytes)?,
                    Err(Error::MissingKey) => (),
                    Err(error) => return Err(error),
                }
                self.inventory_restored.set(true);
            }
            self.inventory.refresh()?;
            // Persist reservations before reporting. A crash can withdraw ports,
            // but must never reuse an old physical port's automatic rail.
            let reservations = self.inventory.reservations()?;
            if reservations.len() > crate::rdma::MAX_JOURNAL_BYTES {
                return Err(Error::Overloaded);
            }
            crate::control::atomic_write(&r, &dir, "rdma-rails.json", &reservations, &scope)
                .await?;
            match crate::control::read_at(
                &r,
                &dir,
                "pending.json",
                wire::MAX_ENROLLMENT_BYTES,
                true,
                &scope,
            )
            .await
            {
                Ok(b) => {
                    r.file_sync(dir.clone(), &scope).await?;
                    return self.request(&decode_pending(&b)?);
                }
                Err(Error::MissingKey) => (),
                Err(e) => return Err(e),
            }
            let pending = self.generate()?;
            let encoded = Zeroizing::new(serde_json::to_vec(&pending).map_err(|_| Error::Io)?);
            crate::control::atomic_write(&r, &dir, "pending.json", &encoded, &scope).await?;
            self.request(&pending)
        })
    }
    pub fn attach_reactor(&self, reactor: Rc<crate::runtime::Reactor>) {
        *self.reactor.borrow_mut() = Some(reactor);
    }
    fn reactor(&self) -> Result<Rc<crate::runtime::Reactor>> {
        self.reactor
            .borrow()
            .clone()
            .ok_or(Error::InvalidConfiguration)
    }
    async fn begin<'a>(
        &'a self,
        r: &crate::runtime::Reactor,
        scope: &RequestScope,
    ) -> Result<(Busy<'a>, RequestScope)> {
        scope.check()?;
        let guard = enter(&self.busy)?;
        if let Some(id) = self.previous.get() {
            r.file_fence(id).await?;
        }
        let mut scope = scope.clone();
        let mut id = [0; 16];
        uring_runtime::environment::fill_random(&mut id).map_err(|_| Error::Io)?;
        scope.request = crate::model::RequestId(id);
        self.previous.set(Some(scope.request));
        Ok((guard, scope))
    }
    pub fn read_token_async<'a>(
        &'a self,
        scope: &'a RequestScope,
    ) -> Operation<'a, Zeroizing<String>> {
        Box::pin(async move {
            let r = self.reactor()?;
            let b =
                crate::control::read_path(&r, &self.token_path, wire::MAX_ENROLLMENT_BYTES, scope)
                    .await?;
            token(&b)
        })
    }
    pub fn cluster(&self) -> &ClusterId {
        &self.cluster
    }
    pub fn accept_response_async<'a>(
        &'a self,
        response: EnrollmentResponse,
        scope: &'a RequestScope,
    ) -> Operation<'a, LocalSigningIdentity> {
        Box::pin(async move {
            let r = self.reactor()?;
            let (_guard, scope) = self.begin(&r, scope).await?;
            let dir = directory(&r, &self.identity_directory, false, true, &scope).await?;
            match read_at(
                &r,
                &dir,
                "identity.json",
                wire::MAX_ENROLLMENT_BYTES * 3,
                true,
                &scope,
            )
            .await
            {
                Ok(b) => {
                    let old = PersistedIdentity::decode(&b)?.response()?;
                    if old.cluster != response.cluster {
                        return Err(Error::Unauthorized);
                    }
                }
                Err(Error::MissingKey) => (),
                Err(e) => return Err(e),
            }
            let bytes = read_at(
                &r,
                &dir,
                "pending.json",
                wire::MAX_ENROLLMENT_BYTES,
                true,
                &scope,
            )
            .await?;
            let (identity, bytes) = self.accept_pending(&bytes, &response)?;
            atomic_write(&r, &dir, "identity.json", &bytes, &scope).await?;
            remove(&r, &dir, "pending.json", &scope).await?;
            Ok(identity)
        })
    }
    pub fn load_identity_async<'a>(
        &'a self,
        scope: &'a RequestScope,
    ) -> Operation<'a, Option<LocalSigningIdentity>> {
        Box::pin(async move {
            let r = self.reactor()?;
            let (_guard, scope) = self.begin(&r, scope).await?;
            let dir = match directory(&r, &self.identity_directory, false, true, &scope).await {
                Ok(dir) => dir,
                Err(Error::MissingKey) => return Ok(None),
                Err(e) => return Err(e),
            };
            let bytes = match read_at(
                &r,
                &dir,
                "identity.json",
                wire::MAX_ENROLLMENT_BYTES * 3,
                true,
                &scope,
            )
            .await
            {
                Ok(b) => b,
                Err(Error::MissingKey) => return Ok(None),
                Err(e) => return Err(e),
            };
            let p = PersistedIdentity::decode(&bytes)?;
            let response = p.response()?;
            match self.validate(&p.pending, &response) {
                Ok(identity) => {
                    r.file_sync(dir.clone(), &scope).await?;
                    match read_at(
                        &r,
                        &dir,
                        "pending.json",
                        wire::MAX_ENROLLMENT_BYTES,
                        true,
                        &scope,
                    )
                    .await
                    {
                        Ok(b) if decode_pending(&b)?.enrollment == identity.enrollment.0 => {
                            remove(&r, &dir, "pending.json", &scope).await?
                        }
                        Ok(_) | Err(Error::MissingKey) => (),
                        Err(e) => return Err(e),
                    }
                    Ok(Some(identity))
                }
                Err(Error::Unauthorized) => Ok(None),
                Err(e) => Err(e),
            }
        })
    }
    pub fn set_peer_trust_roots(&self, roots: Vec<Vec<u8>>) -> Result<()> {
        verifier(&roots)?;
        *self.roots.borrow_mut() = roots;
        Ok(())
    }
    fn generate(&self) -> Result<PendingIdentity> {
        if !wire::valid_uuid(&self.cluster.0) {
            return Err(Error::InvalidConfiguration);
        }
        let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).map_err(|_| Error::Io)?;
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new())
            .map_err(|_| Error::InvalidRequest)?;
        params.distinguished_name = rcgen::DistinguishedName::new();
        let csr = params.serialize_request(&key).map_err(|_| Error::Io)?;
        let mut id = [0; 16];
        uring_runtime::environment::fill_random(&mut id).map_err(|_| Error::Io)?;
        id[6] = (id[6] & 0x0f) | 0x40;
        id[8] = (id[8] & 0x3f) | 0x80;
        let h = format!("{:032x}", u128::from_be_bytes(id));
        let enrollment = format!(
            "{}-{}-{}-{}-{}",
            &h[..8],
            &h[8..12],
            &h[12..16],
            &h[16..20],
            &h[20..]
        );
        let pending = PendingIdentity {
            cluster: self.cluster.0.clone(),
            enrollment,
            private_key: STANDARD.encode(key.serialize_der()),
            csr: STANDARD.encode(csr.der()),
        };
        Ok(pending)
    }
    fn request(&self, p: &PendingIdentity) -> Result<EnrollmentRequest> {
        if p.cluster != self.cluster.0 {
            return Err(Error::Unauthorized);
        }
        let r = EnrollmentRequest {
            rdma_nics: self.inventory.snapshot()?.nics,
            shares: self.shares.get(),
            schema_version: 1,
            cluster: self.cluster.clone(),
            enrollment: EnrollmentId(p.enrollment.clone()),
            csr_der: STANDARD.decode(&p.csr).map_err(|_| Error::CorruptRecord)?,
        };
        racer_control_wire::encode_enrollment_request(&r)?;
        // A corrupt pending key must never be submitted, even if its CSR parses.
        use x509_parser::prelude::FromDer;
        let secret = Zeroizing::new(
            STANDARD
                .decode(&p.private_key)
                .map_err(|_| Error::CorruptRecord)?,
        );
        let key = racer_crypto::ed25519::SigningKey::from_pkcs8_der(&secret)
            .map_err(|_| Error::CorruptRecord)?;
        let (_, csr) =
            x509_parser::certification_request::X509CertificationRequest::from_der(&r.csr_der)
                .map_err(|_| Error::CorruptRecord)?;
        if csr
            .certification_request_info
            .subject_pki
            .subject_public_key
            .data
            .as_ref()
            != key.verifying_key().as_bytes()
        {
            return Err(Error::Unauthorized);
        }
        csr.verify_signature().map_err(|_| Error::Unauthorized)?;
        Ok(r)
    }
    /// Validate identity correlation and key pairing before fenced persistence.
    fn accept_pending(
        &self,
        bytes: &[u8],
        response: &EnrollmentResponse,
    ) -> Result<(LocalSigningIdentity, Zeroizing<Vec<u8>>)> {
        let pending = decode_pending(bytes)?;
        self.request(&pending)?;
        let identity = self.validate(&pending, response)?;
        let persisted = PersistedIdentity {
            pending,
            response: STANDARD.encode(wire::encode_enrollment_response(response)?),
        };
        Ok((identity, persisted.encode()?))
    }

    fn validate(
        &self,
        p: &PendingIdentity,
        r: &EnrollmentResponse,
    ) -> Result<LocalSigningIdentity> {
        wire::encode_enrollment_response(r)?;
        if r.cluster != self.cluster
            || p.cluster != self.cluster.0
            || r.enrollment.0 != p.enrollment
        {
            return Err(Error::Unauthorized);
        }
        let certs: Vec<_> = r
            .certificate_chain
            .iter()
            .cloned()
            .map(rustls::pki_types::CertificateDer::from)
            .collect();
        verifier(&self.roots.borrow())?
            .verify_client_cert(&certs[0], &certs[1..], unix_time())
            .map_err(|_| Error::Unauthorized)?;
        let (_, cert) = x509_parser::parse_x509_certificate(&r.certificate_chain[0])
            .map_err(|_| Error::Unauthorized)?;
        let san = cert
            .subject_alternative_name()
            .map_err(|_| Error::Unauthorized)?
            .ok_or(Error::Unauthorized)?;
        let expected = format!("spiffe://{}/node/{}", self.cluster.0, r.node.0);
        if san.value.general_names.len() != 1
            || !matches!(&san.value.general_names[0], x509_parser::extensions::GeneralName::URI(uri) if *uri == expected)
        {
            return Err(Error::Unauthorized);
        }
        if cert.public_key().algorithm.algorithm.to_id_string() != "1.3.101.112" || cert.is_ca() {
            return Err(Error::Unauthorized);
        }
        let usage = cert
            .key_usage()
            .map_err(|_| Error::Unauthorized)?
            .ok_or(Error::Unauthorized)?;
        let extended = cert
            .extended_key_usage()
            .map_err(|_| Error::Unauthorized)?
            .ok_or(Error::Unauthorized)?;
        if !usage.value.digital_signature() || !extended.value.client_auth {
            return Err(Error::Unauthorized);
        }
        let private_material = Zeroizing::new(
            STANDARD
                .decode(&p.private_key)
                .map_err(|_| Error::CorruptRecord)?,
        );
        let key = racer_crypto::ed25519::SigningKey::from_pkcs8_der(&private_material)
            .map_err(|_| Error::CorruptRecord)?;
        if cert.public_key().subject_public_key.data.as_ref() != key.verifying_key().as_bytes() {
            return Err(Error::Unauthorized);
        }
        let not_before = u64::try_from(cert.validity().not_before.timestamp())
            .map_err(|_| Error::Unauthorized)?;
        let not_after = u64::try_from(cert.validity().not_after.timestamp())
            .map_err(|_| Error::Unauthorized)?;
        if not_after <= not_before
            || not_after - not_before > wire::CERTIFICATE_LIFETIME.as_secs() + 300
        {
            return Err(Error::Unauthorized);
        }
        Ok(LocalSigningIdentity {
            cluster: self.cluster.clone(),
            node: r.node.clone(),
            enrollment: r.enrollment.clone(),
            private_material: private_material.to_vec(),
            certificate_chain: r.certificate_chain.clone(),
            not_before,
            not_after,
        })
    }
}
fn decode_pending(b: &[u8]) -> Result<PendingIdentity> {
    serde_json::from_value(wire::strict_json(b, wire::MAX_ENROLLMENT_BYTES)?)
        .map_err(|_| Error::CorruptRecord)
}
fn verifier(roots: &[Vec<u8>]) -> Result<Arc<dyn rustls::server::danger::ClientCertVerifier>> {
    let mut store = rustls::RootCertStore::empty();
    for r in roots {
        store
            .add(rustls::pki_types::CertificateDer::from(r.clone()))
            .map_err(|_| Error::Unauthorized)?;
    }
    rustls::server::WebPkiClientVerifier::builder_with_provider(
        Arc::new(store),
        Arc::new(rustls::crypto::ring::default_provider()),
    )
    .build()
    .map_err(|_| Error::Unauthorized)
}
impl LocalSigningIdentity {
    pub fn node(&self) -> &NodeId {
        &self.node
    }
    pub fn cluster(&self) -> &ClusterId {
        &self.cluster
    }
    pub fn certificate_chain(&self) -> &[Vec<u8>] {
        &self.certificate_chain
    }
    pub(crate) fn private_key_der(&self) -> &[u8] {
        &self.private_material
    }
    pub fn signing_identity(
        &self,
        roots: &[Vec<u8>],
    ) -> Result<Arc<racer_identity::SigningIdentity>> {
        racer_identity::SigningIdentity::from_pkcs8(
            self.cluster.clone(),
            self.node.clone(),
            &self.private_material,
            self.certificate_chain.clone(),
            roots,
        )
        .map(Arc::new)
        .map_err(Into::into)
    }
    pub fn expires_at(&self) -> SystemTime {
        UNIX_EPOCH + Duration::from_secs(self.not_after)
    }
    pub fn valid_now(&self) -> bool {
        uring_runtime::environment::wall_now() >= UNIX_EPOCH + Duration::from_secs(self.not_before)
            && uring_runtime::environment::wall_now() < self.expires_at()
    }
    pub fn renewal_due(&self) -> bool {
        let lifetime = Duration::from_secs(self.not_after - self.not_before);
        uring_runtime::environment::wall_now()
            >= UNIX_EPOCH
                + Duration::from_secs(self.not_before)
                + (lifetime * 2 / 3).min(wire::RENEW_AFTER)
    }
}

// Bounded file operations issued only through the serving worker's reactor.
const BENEATH: u64 = 0x08;
const NO_MAGICLINKS: u64 = 0x02;
const NO_SYMLINKS: u64 = 0x04;
fn name(s: &OsStr) -> Result<CString> {
    if s.as_bytes().len() > 4096 {
        return Err(Error::InvalidRequest);
    }
    CString::new(s.as_bytes()).map_err(|_| Error::InvalidRequest)
}
fn component(s: &str) -> Result<CString> {
    if !matches!(Path::new(s).components().next(), Some(Component::Normal(_))) || s.contains('/') {
        return Err(Error::InvalidRequest);
    }
    name(s.as_ref())
}
pub(crate) async fn directory(
    r: &Reactor,
    path: &Path,
    create: bool,
    private: bool,
    scope: &RequestScope,
) -> Result<Rc<Descriptor>> {
    // Bound traversal work before issuing the first operation.
    name(path.as_os_str())?;
    let mut fd = r
        .file_open(
            None,
            CString::new(if path.is_absolute() { "/" } else { "." }).unwrap(),
            libc::O_RDONLY | libc::O_DIRECTORY,
            NO_SYMLINKS,
            scope,
        )
        .await?;
    for part in path.components() {
        let Component::Normal(part) = part else {
            if matches!(part, Component::RootDir | Component::CurDir) {
                continue;
            }
            return Err(Error::InvalidConfiguration);
        };
        if create {
            match r.file_mkdir(fd.clone(), name(part)?, scope).await {
                Ok(()) => r.file_sync(fd.clone(), scope).await?,
                // An earlier canceled mkdir may have created this component but
                // missed the parent fsync. Reestablish that durability fence.
                Err(Error::Replay) => r.file_sync(fd.clone(), scope).await?,
                Err(e) => return Err(e),
            }
        }
        fd = r
            .file_open(
                Some(fd),
                name(part)?,
                libc::O_RDONLY | libc::O_DIRECTORY,
                BENEATH | NO_SYMLINKS,
                scope,
            )
            .await?;
    }
    if private {
        check_private(&r.file_stat(fd.clone(), scope).await?, false)?;
    }
    Ok(fd)
}
fn check_private(stat: &libc::statx, regular: bool) -> Result<()> {
    #[cfg(test)]
    let uid = if uring_runtime::reactor::simulation::Simulation::current().is_some() {
        0
    } else {
        unsafe { libc::geteuid() }
    };
    #[cfg(not(test))]
    let uid = unsafe { libc::geteuid() };
    let mask = libc::STATX_MODE | libc::STATX_UID | libc::STATX_NLINK;
    if stat.stx_mask & mask != mask {
        return Err(Error::Io);
    }
    if stat.stx_mode & 0o077 != 0 || stat.stx_uid != uid || regular && stat.stx_nlink != 1 {
        return Err(Error::Unauthorized);
    }
    Ok(())
}
pub(crate) async fn read_file(
    r: &Reactor,
    fd: Rc<Descriptor>,
    limit: usize,
    private: bool,
    scope: &RequestScope,
) -> Result<Zeroizing<Vec<u8>>> {
    if limit > 1024 * 1024 {
        return Err(Error::Overloaded);
    }
    let stat = r.file_stat(fd.clone(), scope).await?;
    let mask = libc::STATX_TYPE | libc::STATX_SIZE;
    if stat.stx_mask & mask != mask
        || stat.stx_mode as u32 & libc::S_IFMT != libc::S_IFREG
        || stat.stx_size > limit as u64
    {
        return Err(Error::InvalidRequest);
    }
    if private {
        check_private(&stat, true)?;
    }
    let mut out = Zeroizing::new(Vec::new());
    loop {
        let buffer = r.file_buffer((limit + 1 - out.len()).min(16384))?;
        let completion = r
            .read_at(fd.clone(), out.len() as u64, buffer, (), scope)
            .await?;
        if completion.bytes == 0 {
            return Ok(out);
        }
        out.extend_from_slice(completion.buffer.prefix(completion.bytes)?);
        if out.len() > limit {
            return Err(Error::Overloaded);
        }
    }
}
pub(crate) async fn read_path(
    r: &Reactor,
    path: &Path,
    limit: usize,
    scope: &RequestScope,
) -> Result<Zeroizing<Vec<u8>>> {
    let fd = r
        .file_open(
            None,
            name(path.as_os_str())?,
            libc::O_RDONLY,
            NO_MAGICLINKS,
            scope,
        )
        .await?;
    read_file(r, fd, limit, false, scope).await
}
pub(crate) async fn read_at(
    r: &Reactor,
    dir: &Rc<Descriptor>,
    file: &str,
    limit: usize,
    private: bool,
    scope: &RequestScope,
) -> Result<Zeroizing<Vec<u8>>> {
    let fd = r
        .file_open(
            Some(dir.clone()),
            component(file)?,
            libc::O_RDONLY,
            BENEATH | NO_SYMLINKS,
            scope,
        )
        .await?;
    read_file(r, fd, limit, private, scope).await
}
#[cfg(test)]
pub(crate) async fn projected_file(
    r: &Reactor,
    path: &Path,
    file: &str,
    limit: usize,
    scope: &RequestScope,
) -> Result<Zeroizing<Vec<u8>>> {
    let dir = directory(r, path, false, false, scope).await?;
    // One openat2 resolves ..data and pins the target directory across rotation.
    // BENEATH rejects absolute/escaping links; NO_MAGICLINKS rejects proc escapes.
    let generation = r
        .file_open(
            Some(dir),
            CString::new("..data").unwrap(),
            libc::O_RDONLY | libc::O_DIRECTORY,
            BENEATH | NO_MAGICLINKS,
            scope,
        )
        .await?;
    read_at(r, &generation, file, limit, false, scope).await
}
pub(crate) async fn atomic_write(
    r: &Reactor,
    dir: &Rc<Descriptor>,
    target: &str,
    bytes: &[u8],
    scope: &RequestScope,
) -> Result<()> {
    component(target)?;
    if bytes.len() > 1024 * 1024 {
        return Err(Error::Overloaded);
    }
    let temporary = format!(".{target}.stage");
    remove(r, dir, &temporary, scope).await?;
    let fd = r
        .file_open(
            Some(dir.clone()),
            component(&temporary)?,
            libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL,
            BENEATH | NO_SYMLINKS,
            scope,
        )
        .await?;
    let mut buffer = r.file_bytes(bytes)?;
    let mut offset = 0;
    while buffer.remaining() != 0 {
        let completion = r.write_at(fd.clone(), offset, buffer, (), scope).await?;
        buffer = completion.buffer;
        buffer.advance(completion.bytes)?;
        offset += completion.bytes as u64;
    }
    r.file_sync(fd, scope).await?;
    r.file_rename(
        dir.clone(),
        component(&temporary)?,
        component(target)?,
        scope,
    )
    .await?;
    r.file_sync(dir.clone(), scope).await
}
pub(crate) async fn remove(
    r: &Reactor,
    dir: &Rc<Descriptor>,
    file: &str,
    scope: &RequestScope,
) -> Result<()> {
    match r.file_unlink(dir.clone(), component(file)?, scope).await {
        Ok(()) | Err(Error::MissingKey) => (),
        Err(e) => return Err(e),
    }
    r.file_sync(dir.clone(), scope).await
}

/// Racer TLS time adapter over the runtime's scoped wall clock.
fn unix_time() -> rustls::pki_types::UnixTime {
    rustls::pki_types::UnixTime::since_unix_epoch(
        uring_runtime::environment::wall_now()
            .duration_since(std::time::SystemTime::UNIX_EPOCH)
            .unwrap_or_default(),
    )
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use racer_control_wire::canonical_socket_paths;
    use racer_control_wire::content_hash;
    use racer_control_wire::decode_enrollment_request;
    use racer_control_wire::decode_publication;
    use racer_control_wire::encode_enrollment_request;
    use racer_control_wire::encode_publication;
    use racer_control_wire::validate_definitions;

    pub(crate) mod bundle_tests {
        use super::*;
        use std::os::unix::fs::symlink;
        use std::sync::Arc;
        #[test]
        fn bundle_installation_is_idempotent_and_rejects_rollback() {
            use racer_identity::KeyEpochs;
            use racer_identity::KeyPurpose;
            let publication = racer_control_wire::decode_publication(include_bytes!(concat!(
                env!("CARGO_MANIFEST_DIR"),
                "/../../internal/racer/wire/testdata/publication.json"
            )))
            .unwrap();
            let keys = Rc::new(Keyring::new(
                publication.cluster,
                publication.members[0].node.clone(),
                Arc::new(KeyEpochs::default()),
            ));
            let installer = BundleInstaller::new(keys.clone());
            let (ca, _) = testing::ca();
            let mut bundle = wire::decode_bundle(include_bytes!(concat!(
                env!("CARGO_MANIFEST_DIR"),
                "/../../internal/racer/wire/testdata/bundle.json"
            )))
            .unwrap();
            bundle.generation = BundleGeneration(2);
            bundle.peer_trust_roots = vec![ca.der().to_vec()];
            // Retain page keys only: the wire vector repeats material across purposes.
            bundle.cache_keys.truncate(2);
            let cache = bundle.cache_keys[0].key.cache.clone();
            for _ in 0..2 {
                assert_eq!(
                    installer.install(bundle.clone()).unwrap().0,
                    BundleGeneration(2)
                );
                assert!(keys.active(&cache, KeyPurpose::Page).is_ok());
                bundle.cache_keys.reverse();
            }
            let mut conflict = bundle.clone();
            conflict.cache_keys.pop();
            assert!(matches!(installer.install(conflict), Err(Error::Replay)));
            assert!(wire::decode_bundle(b"{}").is_err());
            bundle.generation = BundleGeneration(0);
            assert!(installer.install(bundle).is_err());
            assert_eq!(installer.generation(), Some(BundleGeneration(2)));
            assert!(keys.active(&cache, KeyPurpose::Page).is_ok());
        }
        #[test]
        fn coherent_projection_rejects_partial_and_escaping_links() {
            let Some(reactor) = testing::reactor() else {
                return;
            };
            let d = testing::Directory::new();
            let scope = testing::scope();
            let read = || {
                testing::drive(
                    &reactor,
                    Box::pin(crate::control::projected_file(
                        &reactor,
                        &d.0,
                        "bundle.json",
                        wire::MAX_BUNDLE_BYTES,
                        &scope,
                    )),
                )
            };
            std::fs::create_dir(d.0.join("epoch-a")).unwrap();
            std::fs::write(
                d.0.join("epoch-a/bundle.json"),
                include_bytes!(concat!(
                    env!("CARGO_MANIFEST_DIR"),
                    "/../../internal/racer/wire/testdata/bundle.json"
                )),
            )
            .unwrap();
            symlink("epoch-a", d.0.join("..data")).unwrap();
            symlink("/dev/null", d.0.join("bundle.json")).unwrap();
            assert!(wire::decode_bundle(&read().unwrap()).is_ok());
            std::fs::create_dir(d.0.join("epoch-b")).unwrap();
            symlink("epoch-b", d.0.join("..next")).unwrap();
            std::fs::rename(d.0.join("..next"), d.0.join("..data")).unwrap();
            assert!(read().is_err());
            std::fs::write(d.0.join("epoch-b/bundle.json"), b"{\"generation\":null}").unwrap();
            assert!(wire::decode_bundle(&read().unwrap()).is_err());
            std::fs::remove_file(d.0.join("..data")).unwrap();
            symlink("../", d.0.join("..data")).unwrap();
            assert!(read().is_err());
        }
    }

    pub(crate) mod testing {
        use crate as dataplane;
        use std::path::PathBuf;
        use std::sync::atomic::AtomicU64;
        use std::sync::atomic::Ordering;
        #[allow(dead_code)]
        mod io {
            include!(concat!(
                env!("CARGO_MANIFEST_DIR"),
                "/tests/support/enrollment.rs"
            ));
        }
        pub(crate) use io::ca;
        pub(crate) use io::drive;
        pub(crate) use io::signing_identity;
        pub(crate) struct Directory(pub PathBuf);
        impl Directory {
            pub fn new() -> Self {
                static NEXT: AtomicU64 = AtomicU64::new(0);
                let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
                    .join("target/control-tests")
                    .join(format!(
                        "{}-{}",
                        std::process::id(),
                        NEXT.fetch_add(1, Ordering::Relaxed)
                    ));
                std::fs::create_dir_all(&path).unwrap();
                Self(path)
            }
        }
        impl Drop for Directory {
            fn drop(&mut self) {
                let _ = std::fs::remove_dir_all(&self.0);
            }
        }
        pub(crate) fn scope() -> crate::runtime::RequestScope {
            crate::runtime::RequestScope::new(
                crate::model::RequestId([7; 16]),
                std::time::Instant::now() + std::time::Duration::from_secs(10),
            )
            .unwrap()
        }
        pub(crate) fn reactor() -> Option<std::rc::Rc<crate::runtime::Reactor>> {
            match io_uring::IoUring::new(2) {
                Ok(ring) => drop(ring),
                Err(e)
                    if matches!(
                        e.raw_os_error(),
                        Some(libc::ENOSYS | libc::EPERM | libc::EACCES)
                    ) =>
                {
                    eprintln!("control io_uring unavailable: {e}");
                    return None;
                }
                Err(e) => panic!("io_uring setup: {e}"),
            }
            Some(io::reactor())
        }
        pub(crate) fn issue(
            request: &racer_control_wire::EnrollmentRequest,
            ca: &rcgen::Certificate,
            key: &rcgen::KeyPair,
            node: &str,
        ) -> racer_control_wire::EnrollmentResponse {
            issue_at(
                request,
                ca,
                key,
                node,
                uring_runtime::environment::wall_now() - std::time::Duration::from_secs(1),
            )
        }
        pub(crate) fn issue_at(
            request: &racer_control_wire::EnrollmentRequest,
            ca: &rcgen::Certificate,
            key: &rcgen::KeyPair,
            node: &str,
            not_before: std::time::SystemTime,
        ) -> racer_control_wire::EnrollmentResponse {
            let der =
                rustls::pki_types::CertificateSigningRequestDer::from(request.csr_der.clone());
            let mut csr = rcgen::CertificateSigningRequestParams::from_der(&der).unwrap();
            csr.params.not_before = not_before.into();
            csr.params.not_after = (not_before + std::time::Duration::from_secs(86400)).into();
            csr.params.subject_alt_names = vec![rcgen::SanType::URI(
                format!("spiffe://{}/node/{node}", request.cluster.0)
                    .try_into()
                    .unwrap(),
            )];
            csr.params.key_usages = vec![rcgen::KeyUsagePurpose::DigitalSignature];
            csr.params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ClientAuth];
            let cert = csr.signed_by(ca, key).unwrap();
            racer_control_wire::EnrollmentResponse {
                schema_version: 1,
                cluster: request.cluster.clone(),
                node: racer_control_wire::NodeId(node.into()),
                enrollment: request.enrollment.clone(),
                certificate_chain: vec![cert.der().to_vec()],
            }
        }
    }

    pub(crate) mod client_tests {
        use super::*;
        use crate::control::PublishedState;
        use crate::control::tests::testing;
        use racer_control_wire::ClusterId;
        use racer_control_wire::NodeId;
        use racer_identity::KeyEpochs;
        use std::sync::Arc;
        #[test]
        fn busy_guard_rejects_overlap_without_releasing_the_owner() {
            let flag = Cell::new(false);
            let owner = enter(&flag).unwrap();
            assert!(matches!(enter(&flag), Err(Error::Overloaded)));
            assert!(flag.get());
            drop(owner);
            assert!(!flag.get());
            let retry = enter(&flag).unwrap();
            assert!(flag.get());
            drop(retry);
            assert!(!flag.get());
        }
        fn client(d: &testing::Directory) -> ControlClient {
            let cluster = ClusterId("11111111-1111-4111-8111-111111111111".into());
            let keys = Rc::new(Keyring::new(
                cluster.clone(),
                NodeId("22222222-2222-4222-8222-222222222222".into()),
                Arc::new(KeyEpochs::default()),
            ));
            ControlClient::new(
                ControlEndpoint {
                    url: "https://localhost".into(),
                    trust_bundle: d.0.join("trust"),
                },
                Rc::new(Enrollment::new(
                    cluster.clone(),
                    d.0.join("token"),
                    d.0.join("identity"),
                )),
                keys.clone(),
                BundleInstaller::new(keys),
                Rc::new(SnapshotStore::new(
                    cluster,
                    Arc::new(PublishedState::default()),
                    2,
                )),
            )
        }
        #[test]
        fn lagging_replica_retries_preserve_state_without_enrollment() {
            let Some(r) = testing::reactor() else { return };
            let scope = testing::scope();
            use crate::control::tests::scenarios::scripted_server;
            for pending in [false, true] {
                let d = testing::Directory::new();
                let mut client = client(&d);
                let (ca, key) = testing::ca();
                client
                    .enrollment
                    .set_peer_trust_roots(vec![ca.der().to_vec()])
                    .unwrap();
                client.enrollment.attach_reactor(r.clone());
                let request = testing::drive(&r, client.enrollment.prepare(&scope)).unwrap();
                let identity = testing::drive(
                    &r,
                    client.enrollment.accept_response_async(
                        testing::issue(&request, &ca, &key, "22222222-2222-4222-8222-222222222222"),
                        &scope,
                    ),
                )
                .unwrap();
                assert!(!identity.renewal_due());
                *client.identity.borrow_mut() = Some(identity.clone());
                client.started.set(true);
                let mut publication =
                    racer_control_wire::decode_publication(include_bytes!(concat!(
                        env!("CARGO_MANIFEST_DIR"),
                        "/../../internal/racer/wire/testdata/publication.json"
                    )))
                    .unwrap();
                publication.sequence = wire::PublicationSequence(10);
                let accepted = client.snapshots.publish(publication.clone()).unwrap();
                let cursor = if pending {
                    publication.sequence = wire::PublicationSequence(11);
                    *client.pending.borrow_mut() = Some(Rc::new(
                        client.snapshots.prepare(publication.clone()).unwrap(),
                    ));
                    11
                } else {
                    10
                };
                let prepared = client.pending.borrow().clone();
                let diagnostic = client.membership_diagnostic().unwrap();
                assert_eq!(diagnostic.accepted_sequence, 10);
                assert_eq!(diagnostic.pending_sequence, if pending { 11 } else { 0 });
                let (_, canonical_members) =
                    racer_control_wire::canonical_content(&publication).unwrap();
                use sha2::Digest;
                assert_eq!(
                    diagnostic.accepted_hash.as_slice(),
                    sha2::Sha256::digest(canonical_members).as_slice()
                );
                let path = format!("{}?after={cursor}", wire::SNAPSHOT_PATH);
                publication.sequence = wire::PublicationSequence(12);
                let mut rollback = publication.clone();
                rollback.sequence = wire::PublicationSequence(9);
                let (endpoint, server) = scripted_server(
                    &d,
                    &ca,
                    &key,
                    vec![
                        vec![(path.clone(), 503, br#"{"code":"unavailable"}"#.to_vec())],
                        vec![(path.clone(), 429, br#"{"code":"overloaded"}"#.to_vec())],
                        vec![
                            (path.clone(), 204, vec![]),
                            (
                                path,
                                200,
                                racer_control_wire::encode_publication(&publication).unwrap(),
                            ),
                            (
                                format!("{}?after=12", wire::SNAPSHOT_PATH),
                                200,
                                racer_control_wire::encode_publication(&rollback).unwrap(),
                            ),
                        ],
                    ],
                );
                client.transport = ControlTransport::new(endpoint);
                client.attach_io(Rc::new(ReactorControlIo::new(r.clone())));
                let scope = testing::scope();
                for error in [Error::Unavailable, Error::Overloaded] {
                    assert_eq!(
                        testing::drive(&r, Box::pin(client.poll_publication(&scope))),
                        Err(error)
                    );
                    assert_eq!(
                        client.snapshots.cursor().unwrap(),
                        Some(wire::PublicationSequence(10))
                    );
                    assert!(Arc::ptr_eq(&accepted, &client.snapshots.current().unwrap()));
                    if let Some(prepared) = &prepared {
                        assert!(Rc::ptr_eq(
                            prepared,
                            client.pending.borrow().as_ref().unwrap()
                        ));
                    } else {
                        assert!(client.pending.borrow().is_none());
                    }
                    let before = Instant::now();
                    assert!(client.backoff().unwrap() >= before + Duration::from_secs(1));
                    // No token exists in this fixture.
                    // A spurious binding recheck would fail instead of returning Ok.
                    assert_eq!(
                        testing::drive(&r, Box::pin(client.renew_if_due(&scope))),
                        Ok(())
                    );
                    assert_eq!(client.renewal_error(), None);
                    assert!(!d.0.join("identity/pending.json").exists());
                    assert_eq!(
                        client.identity().unwrap().certificate_chain(),
                        identity.certificate_chain()
                    );
                }
                assert_eq!(
                    testing::drive(&r, Box::pin(client.poll_publication(&scope))),
                    Ok(())
                );
                assert!(Arc::ptr_eq(&accepted, &client.snapshots.current().unwrap()));
                assert_eq!(
                    testing::drive(&r, Box::pin(client.poll_publication(&scope))),
                    Ok(())
                );
                assert_eq!(
                    client.snapshots.cursor().unwrap(),
                    Some(wire::PublicationSequence(12))
                );
                assert!(client.pending.borrow().is_none());
                let diagnostic = client.membership_diagnostic().unwrap();
                assert_eq!(diagnostic.accepted_sequence, 12);
                assert_eq!(diagnostic.pending_sequence, 0);
                assert_eq!(
                    testing::drive(&r, Box::pin(client.poll_publication(&scope))),
                    Err(Error::Replay)
                );
                assert!(!transient(Error::Replay));
                assert_eq!(
                    client.snapshots.cursor().unwrap(),
                    Some(wire::PublicationSequence(12))
                );
                client.transport.close_idle();
                server.join().unwrap();
            }
        }
        #[test]
        fn changed_binding_latches_restart_across_abandoned_or_failed_persistence() {
            for abandon in [false, true] {
                let Some(r) = testing::reactor() else { return };
                let d = testing::Directory::new();
                let client = client(&d);
                let (ca, key) = testing::ca();
                client
                    .enrollment
                    .set_peer_trust_roots(vec![ca.der().to_vec()])
                    .unwrap();
                client.enrollment.attach_reactor(r.clone());
                let scope = testing::scope();
                let request = testing::drive(&r, client.enrollment.prepare(&scope)).unwrap();
                let old = testing::drive(
                    &r,
                    client.enrollment.accept_response_async(
                        testing::issue(&request, &ca, &key, "22222222-2222-4222-8222-222222222222"),
                        &scope,
                    ),
                )
                .unwrap();
                *client.identity.borrow_mut() = Some(old.clone());
                client.started.set(true);
                let request = testing::drive(&r, client.enrollment.prepare(&scope)).unwrap();
                let response =
                    testing::issue(&request, &ca, &key, "33333333-3333-4333-8333-333333333333");
                let committed = std::fs::read(d.0.join("identity/identity.json")).unwrap();
                client.enrollment.attach_reactor(r.clone());
                let scope = testing::scope();
                if abandon {
                    let mut renewal = Box::pin(client.accept_renewal(response, &scope));
                    let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
                    assert!(renewal.as_mut().poll(&mut cx).is_pending());
                    drop(renewal);
                } else {
                    // Disk failure before commit still requires the graph to exit.
                    std::fs::remove_file(d.0.join("identity/pending.json")).unwrap();
                    assert_eq!(
                        testing::drive(&r, Box::pin(client.accept_renewal(response, &scope))),
                        Err(Error::NodeIdentityChanged)
                    );
                }
                assert_eq!(client.identity().unwrap().node(), old.node());
                assert_eq!(client.activate_identity(), Err(Error::NodeIdentityChanged));
                assert_eq!(
                    client.bind_keyring(client.keys.borrow().clone()),
                    Err(Error::NodeIdentityChanged)
                );
                assert!(matches!(
                    testing::drive(&r, client.start(&scope)),
                    Err(Error::NodeIdentityChanged)
                ));
                assert!(matches!(
                    testing::drive(&r, client.progress(&scope)),
                    Err(Error::NodeIdentityChanged)
                ));
                assert!(matches!(
                    testing::drive(&r, client.poll(SnapshotRequest { after: None }, &scope)),
                    Err(Error::NodeIdentityChanged)
                ));
                testing::drive(&r, r.drain()).unwrap();
                assert_eq!(
                    std::fs::read(d.0.join("identity/identity.json")).unwrap(),
                    committed
                );
            }
        }
        #[test]
        fn status_policy_backoff_and_shutdown_are_explicit() {
            let d = testing::Directory::new();
            let client = client(&d);
            assert!(!d.0.join("identity").exists());
            let unavailable = Response {
                status: 503,
                body: br#"{"code":"unavailable"}"#.to_vec(),
                retry_after: Some(Duration::from_secs(60)),
            };
            assert_eq!(client.response(unavailable, false), Err(Error::Unavailable));
            let before = Instant::now();
            let next = client.backoff().unwrap();
            assert!(next >= before + Duration::from_secs(60));
            for _ in 0..20 {
                let before = Instant::now();
                let next = client.backoff().unwrap();
                assert!(next >= before + Duration::from_secs(1));
                assert!(next <= Instant::now() + Duration::from_secs(30));
            }
            assert_eq!(
                client.response(
                    Response {
                        status: 413,
                        body: br#"{"code":"too_large"}"#.to_vec(),
                        retry_after: None
                    },
                    false
                ),
                Err(Error::InvalidRequest)
            );
            assert_eq!(
                client.response(
                    Response {
                        status: 503,
                        body: br#"{"code":"forbidden"}"#.to_vec(),
                        retry_after: None
                    },
                    false
                ),
                Err(Error::InvalidRequest)
            );
            client.shutdown().unwrap();
            assert!(matches!(
                futures::executor::block_on(client.progress(&testing::scope())),
                Err(Error::Cancelled)
            ));
        }
        #[test]
        fn projection_failure_retains_last_accepted_epoch() {
            let d = testing::Directory::new();
            let client = client(&d);
            let (ca, _) = testing::ca();
            let mut bundle = wire::decode_bundle(include_bytes!(concat!(
                env!("CARGO_MANIFEST_DIR"),
                "/../../internal/racer/wire/testdata/bundle.json"
            )))
            .unwrap();
            bundle.generation.0 = 2;
            bundle.peer_trust_roots = vec![ca.der().to_vec()];
            // Production rejects material reuse across independent key purposes.
            let key = &bundle.cache_keys[2];
            bundle.cache_keys[2] = wire::CacheEncryptionKey::new(
                key.key.clone(),
                key.state,
                zeroize::Zeroizing::new([2; 32]),
            );
            assert_eq!(
                client.secrets.install(bundle.clone()).unwrap().0,
                wire::BundleGeneration(2)
            );
            assert_eq!(
                client.secrets.install(bundle.clone()).unwrap().0,
                wire::BundleGeneration(2)
            );
            let key = &bundle.cache_keys[0];
            bundle.cache_keys[0] = wire::CacheEncryptionKey::new(
                key.key.clone(),
                key.state,
                zeroize::Zeroizing::new([3; 32]),
            );
            assert!(matches!(
                client.secrets.install(bundle.clone()),
                Err(Error::Replay)
            ));
            assert!(
                client
                    .keyring_response(
                        Response {
                            status: 200,
                            body: b"{}".to_vec(),
                            retry_after: None
                        },
                        Some(wire::BundleGeneration(2))
                    )
                    .is_err()
            );
            bundle.generation = wire::BundleGeneration(0);
            assert!(client.secrets.install(bundle.clone()).is_err());
            assert_eq!(client.secrets.generation(), Some(wire::BundleGeneration(2)));
            assert!(
                client
                    .keys
                    .borrow()
                    .active(
                        &bundle.cache_keys[0].key.cache,
                        racer_identity::KeyPurpose::Page
                    )
                    .is_ok()
            );
        }
        #[test]
        fn keyring_cursor_status_and_retry_are_independent() {
            let d = testing::Directory::new();
            let client = client(&d);
            let response = |status, body: &[u8]| Response {
                status,
                body: body.to_vec(),
                retry_after: Some(Duration::from_secs(7)),
            };
            assert!(client.keyring_response(response(204, b""), None).is_err());
            assert!(
                client
                    .keyring_response(response(204, b""), Some(wire::BundleGeneration(1)))
                    .unwrap()
                    .is_none()
            );
            assert!(
                client
                    .keyring_response(response(204, b"x"), Some(wire::BundleGeneration(1)))
                    .is_err()
            );
            assert!(matches!(
                client.keyring_response(
                    response(409, br#"{"code":"conflict"}"#),
                    Some(wire::BundleGeneration(1))
                ),
                Err(Error::Replay)
            ));
            assert!(matches!(
                client.keyring_response(response(503, br#"{"code":"unavailable"}"#), None),
                Err(Error::Unavailable)
            ));
            assert_eq!(
                client.keyring_retry_after.get(),
                Some(Duration::from_secs(7))
            );
            assert_eq!(client.retry_after.get(), None);
            client.keyring_busy.set(true);
            assert!(matches!(
                futures::executor::block_on(client.keyring_progress(&testing::scope())),
                Err(Error::Overloaded)
            ));
            assert!(!client.poll_busy.get());
            assert!(!client.enrollment_busy.get());
        }
    }

    pub(crate) mod wire_tests {
        use super::*;
        use racer_control_wire::decode_bundle;
        use racer_control_wire::decode_enrollment_response;
        use racer_control_wire::encode_bundle;
        use racer_control_wire::encode_enrollment_response;

        #[test]
        fn application_roundtrips_preserve_exact_wire_records() {
            const ROOT: &str = concat!(
                env!("CARGO_MANIFEST_DIR"),
                "/../../internal/racer/wire/testdata/"
            );
            for name in [
                "publication.json",
                "bootstrap-request.json",
                "bootstrap-response.json",
                "bundle.json",
            ] {
                let input = std::fs::read(format!("{ROOT}{name}")).unwrap();
                let bytes = input.strip_suffix(b"\n").unwrap_or(&input);
                let output = match name {
                    "publication.json" => {
                        let publication = decode_publication(bytes).unwrap();
                        let roundtrip = encode_publication(&publication).unwrap();
                        assert_eq!(
                            content_hash(&publication).unwrap(),
                            wire::content_hash(&wire::decode_publication(bytes).unwrap()).unwrap()
                        );
                        roundtrip
                    }
                    "bootstrap-request.json" => {
                        encode_enrollment_request(&decode_enrollment_request(bytes).unwrap())
                            .unwrap()
                    }
                    "bootstrap-response.json" => {
                        encode_enrollment_response(&decode_enrollment_response(bytes).unwrap())
                            .unwrap()
                    }
                    _ => encode_bundle(&decode_bundle(bytes).unwrap()).unwrap(),
                };
                // The publication fixture is intentionally not canonically sorted.
                if name == "publication.json" {
                    assert_eq!(
                        output,
                        wire::encode_publication(&wire::decode_publication(bytes).unwrap())
                            .unwrap()
                    );
                } else {
                    assert_eq!(output, bytes, "{name}");
                }
            }
        }

        #[test]
        fn wire_failures_keep_application_error_meanings() {
            for (wire, app) in [
                (wire::Error::InvalidRequest, Error::InvalidRequest),
                (
                    wire::Error::IncompatibleMembership,
                    Error::IncompatibleMembership,
                ),
                (wire::Error::Overloaded, Error::Overloaded),
                (wire::Error::Replay, Error::Replay),
            ] {
                assert_eq!(Error::from(wire), app);
            }
            assert_eq!(
                wire::strict_json(b"{\"x\":1,\"x\":2}", 64).map_err(Error::from),
                Err(Error::InvalidRequest)
            );
            assert_eq!(
                wire::strict_json(b"{}", 1).map_err(Error::from),
                Err(Error::Overloaded)
            );
        }
    }

    pub(crate) mod cache_tests {
        use super::*;
        #[test]
        fn replacement_definitions_and_socket_paths_are_validated() {
            let mut defs = decode_publication(include_bytes!(concat!(
                env!("CARGO_MANIFEST_DIR"),
                "/../../internal/racer/wire/testdata/publication.json"
            )))
            .unwrap()
            .caches;
            validate_definitions(&defs).unwrap();
            defs[0].id.0 = "66666666-6666-4666-8666-666666666666".into();
            validate_definitions(&defs).unwrap();
            let mut bad = defs.clone();
            bad[0].name = "..".into();
            assert!(validate_definitions(&bad).is_err());
            validate_definitions(&defs).unwrap();
            defs[0].name = "renamed".into();
            (defs[0].client_socket, defs[0].origin_socket) =
                canonical_socket_paths("renamed").unwrap();
            validate_definitions(&defs).unwrap();
            for name in ["", ".", "..", "a/b", "A", "-a", "a-", "a..b"] {
                assert!(canonical_socket_paths(name).is_err());
            }
            let maximum = format!("{}.{}", "a".repeat(63), "b".repeat(18));
            assert_eq!(
                canonical_socket_paths(&maximum)
                    .unwrap()
                    .0
                    .as_os_str()
                    .len(),
                107
            );
            assert!(canonical_socket_paths(&(maximum + "b")).is_err());
        }
    }

    pub(crate) mod publication_tests {
        use super::*;
        fn publication(sequence: u64) -> Publication {
            let mut p = decode_publication(include_bytes!(concat!(
                env!("CARGO_MANIFEST_DIR"),
                "/../../internal/racer/wire/testdata/publication.json"
            )))
            .unwrap();
            p.sequence.0 = sequence;
            p.membership_version.0 = 1;
            // Lifecycle cases use unique ASCII device IDs; wire parity is separate.
            for member in &mut p.members {
                for (index, rail) in member.rails.iter_mut().enumerate() {
                    rail.device = format!("nic-{index}");
                }
            }
            p
        }
        fn store(retained: usize) -> SnapshotStore {
            SnapshotStore::new(
                publication(1).cluster,
                Arc::new(PublishedState::default()),
                retained,
            )
        }
        fn membership(sequence: u64, version: u64) -> Publication {
            let mut next = publication(sequence);
            next.membership_version.0 = version;
            next
        }
        #[test]
        fn default_two_old_generations_resolve_without_local_request_pins() {
            let store = store(2);
            for version in 1..20 {
                store.publish(membership(version, version)).unwrap();
                for old in version.saturating_sub(2).max(1)..=version {
                    assert_eq!(
                        store
                            .published
                            .membership(MembershipVersion(old))
                            .unwrap()
                            .version
                            .0,
                        old
                    );
                }
                assert!(store.published.state.lock().unwrap().memberships.len() <= 3);
            }
        }
        #[test]
        fn cache_only_history_uses_one_slot_and_weak_registry_stays_bounded() {
            let store = store(0);
            let first = store.publish(publication(1)).unwrap();
            let mut history = vec![first.clone()];
            for sequence in 2..40 {
                let mut next = publication(sequence);
                next.caches.clear();
                let snapshot = store.publish(next).unwrap();
                assert!(Arc::ptr_eq(&first.membership, &snapshot.membership));
                history.push(snapshot);
                assert_eq!(store.published.state.lock().unwrap().memberships.len(), 1);
            }
            let next = membership(40, 2);
            assert!(matches!(
                store.publish(next.clone()),
                Err(Error::Overloaded)
            ));
            let weak = Arc::downgrade(&first.membership);
            drop(first);
            drop(history);
            store.publish(next).unwrap();
            assert!(weak.upgrade().is_none());
            for version in 3..100 {
                store.publish(membership(version + 40, version)).unwrap();
                assert_eq!(store.published.state.lock().unwrap().memberships.len(), 1);
                assert!(matches!(
                    store.published.membership(MembershipVersion(version - 1)),
                    Err(Error::IncompatibleMembership)
                ));
            }
        }
        #[test]
        fn delayed_thread_lease_blocks_admission_until_release() {
            let store = store(1);
            let published = store.published.clone();
            store.publish(publication(1)).unwrap();
            let (ready_tx, ready_rx) = std::sync::mpsc::channel();
            let (release_tx, release_rx) = std::sync::mpsc::channel();
            let worker = std::thread::spawn(move || {
                let snapshot = published.current().unwrap();
                ready_tx.send(()).unwrap();
                release_rx.recv().unwrap();
                let incoming = published.membership(MembershipVersion(1)).unwrap();
                assert!(Arc::ptr_eq(&snapshot.membership, &incoming));
            });
            ready_rx.recv().unwrap();
            let current = store.publish(membership(2, 2)).unwrap();
            assert!(matches!(
                store.publish(membership(3, 3)),
                Err(Error::Overloaded)
            ));
            release_tx.send(()).unwrap();
            worker.join().unwrap();
            store.publish(membership(3, 3)).unwrap();
            assert_eq!(current.membership.version, MembershipVersion(2));
        }
        #[test]
        fn atomic_replay_rollback_and_leased_history() {
            let store = store(1);
            let first = store.publish(publication(1)).unwrap();
            assert!(Arc::ptr_eq(&first, &store.publish(publication(1)).unwrap()));
            let mut changed = publication(1);
            changed.caches[0].id = CacheId("66666666-6666-4666-8666-666666666666".into());
            assert!(matches!(store.publish(changed), Err(Error::Replay)));
            let second = store.publish(membership(2, 2)).unwrap();
            assert!(matches!(store.publish(publication(1)), Err(Error::Replay)));
            assert!(matches!(
                store.publish(membership(3, 3)),
                Err(Error::Overloaded)
            ));
            assert_eq!(store.cursor().unwrap(), Some(PublicationSequence(2)));
            drop(first);
            assert!(store.publish(membership(3, 3)).is_ok());
            drop(second);
            let mut changed = membership(4, 3);
            changed.members[0].peer_endpoint = "192.0.2.7:7443".into();
            assert!(matches!(
                store.publish(changed.clone()),
                Err(Error::IncompatibleMembership)
            ));
            changed.membership_version.0 += 1;
            assert!(store.publish(changed).is_ok());
        }
        #[test]
        fn site_changes_require_new_membership_and_preserve_leased_history() {
            let store = store(3);
            let old = store.publish(publication(1)).unwrap();
            let mut next = publication(2);
            next.members[0].site = "site1".into();
            assert!(matches!(
                store.publish(next.clone()),
                Err(Error::IncompatibleMembership)
            ));
            next.membership_version.0 = 2;
            let current = store.publish(next).unwrap();
            assert!(old.membership.members()[0].site.is_empty());
            assert_eq!(current.membership.members()[0].site, "site1");
            assert_eq!(
                old.membership.placement_identity(),
                current.membership.placement_identity()
            );
            assert!(
                store
                    .publish(membership(3, 3))
                    .unwrap()
                    .membership
                    .members()[0]
                    .site
                    .is_empty()
            );
        }
        #[test]
        fn staged_resources_commit_only_on_accepted_replacement() {
            struct Transition(Rc<std::cell::Cell<usize>>);
            impl CacheTransition for Transition {
                fn commit(self: Box<Self>) {
                    self.0.set(self.0.get() + 1);
                }
            }
            let committed = Rc::new(std::cell::Cell::new(0));
            let store = store(0);
            let publish =
                |p| store.publish_staged(p, Some(Box::new(Transition(committed.clone()))));
            let first = publish(publication(1)).unwrap();
            assert_eq!(committed.get(), 1);
            assert!(matches!(publish(membership(2, 2)), Err(Error::Overloaded)));
            assert_eq!(committed.get(), 1);
            drop(first);
            publish(membership(2, 2)).unwrap();
            assert_eq!(committed.get(), 2);
        }
    }

    pub(crate) mod scenarios {
        use super::*;
        use crate::control::Enrollment;
        use crate::control::tests::testing;
        use std::cell::Cell;
        use std::cell::RefCell;
        use std::io::Read;
        use std::io::Write;
        use std::sync::Arc;
        use std::time::Duration;
        use std::time::SystemTime;

        fn server_config(
            ca: &rcgen::Certificate,
            ca_key: &rcgen::KeyPair,
            optional_client: bool,
        ) -> Arc<rustls::ServerConfig> {
            let mut params =
                rcgen::CertificateParams::new(vec!["localhost".into(), "127.0.0.1".into()])
                    .unwrap();
            params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ServerAuth];
            let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
            let cert = params.signed_by(&key, ca, ca_key).unwrap();
            let mut roots = rustls::RootCertStore::empty();
            roots.add(ca.der().clone()).unwrap();
            let provider = Arc::new(rustls::crypto::ring::default_provider());
            let verifier = rustls::server::WebPkiClientVerifier::builder_with_provider(
                Arc::new(roots),
                provider.clone(),
            );
            let verifier = if optional_client {
                verifier.allow_unauthenticated()
            } else {
                verifier
            }
            .build()
            .unwrap();
            Arc::new(
                rustls::ServerConfig::builder_with_provider(provider)
                    .with_safe_default_protocol_versions()
                    .unwrap()
                    .with_client_cert_verifier(verifier)
                    .with_single_cert(
                        vec![cert.der().clone()],
                        rustls::pki_types::PrivatePkcs8KeyDer::from(key.serialize_der()).into(),
                    )
                    .unwrap(),
            )
        }
        fn accept(listener: &std::net::TcpListener) -> std::net::TcpStream {
            listener.set_nonblocking(true).unwrap();
            let deadline = Instant::now() + Duration::from_secs(10);
            let socket = loop {
                match listener.accept() {
                    Ok((socket, _)) => break socket,
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                        assert!(Instant::now() < deadline, "missing backend connection");
                        std::thread::sleep(Duration::from_millis(1));
                    }
                    Err(e) => panic!("accept: {e}"),
                }
            };
            socket
                .set_read_timeout(Some(Duration::from_secs(10)))
                .unwrap();
            socket
                .set_write_timeout(Some(Duration::from_secs(10)))
                .unwrap();
            socket
        }
        fn read_head(stream: &mut impl Read) -> String {
            let mut request = Vec::new();
            while !request.ends_with(b"\r\n\r\n") {
                assert!(request.len() < 16384);
                let mut byte = [0];
                stream.read_exact(&mut byte).unwrap();
                request.push(byte[0]);
            }
            String::from_utf8(request).unwrap()
        }
        fn assert_disconnected(stream: &mut impl Read) {
            match stream.read(&mut [0]) {
                Ok(0) => (),
                Err(e)
                    if matches!(
                        e.kind(),
                        std::io::ErrorKind::UnexpectedEof | std::io::ErrorKind::ConnectionReset
                    ) =>
                {
                    ()
                }
                result => panic!("connection not discarded: {result:?}"),
            }
        }
        /// Each group uses exactly one TLS connection; EOF proves actual retirement.
        pub(in crate::control) fn scripted_server(
            d: &testing::Directory,
            ca: &rcgen::Certificate,
            ca_key: &rcgen::KeyPair,
            groups: Vec<Vec<(String, u16, Vec<u8>)>>,
        ) -> (ControlEndpoint, std::thread::JoinHandle<()>) {
            let config = server_config(ca, ca_key, false);
            let trust = d.0.join("trust");
            std::fs::write(&trust, ca.pem()).unwrap();
            let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
            let endpoint = ControlEndpoint {
                url: format!("https://{}", listener.local_addr().unwrap()),
                trust_bundle: trust,
            };
            let server = std::thread::spawn(move || {
                for group in groups {
                    let mut stream = rustls::StreamOwned::new(
                        rustls::ServerConnection::new(config.clone()).unwrap(),
                        accept(&listener),
                    );
                    let mut disconnected = false;
                    for (path, status, body) in group {
                        assert!(
                            read_head(&mut stream).starts_with(&format!("GET {path} HTTP/1.1\r\n"))
                        );
                        if status == 0 {
                            disconnected = true;
                            break;
                        }
                        let head = format!(
                            "HTTP/1.1 {status} Result\r\nContent-Type: application/json\r\nContent-Length: {}\r\nRetry-After: 1\r\n\r\n",
                            body.len()
                        );
                        stream.write_all(head.as_bytes()).unwrap();
                        stream.write_all(&body).unwrap();
                        stream.flush().unwrap();
                    }
                    if !disconnected {
                        assert_disconnected(&mut stream);
                    }
                }
            });
            (endpoint, server)
        }

        // Test-only observation and first-writable suppression on the real reactor adapter.
        // No alternative transport, filesystem, admission, or readiness implementation.
        pub(crate) struct ConnectProbe {
            calls: Cell<usize>,
            held: RefCell<Option<std::rc::Weak<Descriptor>>>,
            leases: RefCell<Vec<std::rc::Weak<crate::admission::ConnectionReservation>>>,
            pub(crate) addresses: [SocketAddr; 2],
            pub(crate) mode: &'static str,
            pub(crate) parent_deadline: Instant,
        }
        impl ConnectProbe {
            pub(crate) fn suppress(
                &self,
                fd: &Rc<Descriptor>,
                read: bool,
                write: bool,
                lease: &Option<Rc<crate::admission::ConnectionReservation>>,
                scope: &RequestScope,
            ) -> Result<bool> {
                self.leases
                    .borrow_mut()
                    .push(Rc::downgrade(lease.as_ref().expect("admission lease")));
                if !read && write {
                    let call = self.calls.get();
                    self.calls.set(call + 1);
                    if call == 0 {
                        *self.held.borrow_mut() = Some(Rc::downgrade(fd));
                        assert!(scope.deadline.0 < self.parent_deadline);
                        if self.mode == "cancel" {
                            scope.cancel()?;
                        }
                        // Suppress a real socket's writable notification, not Simulation::connect.
                        return Ok(true);
                    }
                    // Inspect the actual OS socket at the public readiness boundary.
                    let raw = unsafe { libc::dup(fd.as_raw_fd()) };
                    assert!(raw >= 0);
                    let socket = unsafe { std::net::TcpStream::from_raw_fd(raw) };
                    assert_eq!(socket.peer_addr().unwrap(), self.addresses[1]);
                }
                Ok(false)
            }
        }
        #[test]
        fn real_connect_readiness_blackhole_yields_to_next_address_and_cleans_fds() {
            use std::task::Context;
            use std::task::Poll;
            for mode in ["local", "cancel", "parent"] {
                let Some(reactor) = testing::reactor() else {
                    return;
                };
                let d = testing::Directory::new();
                let (ca, key) = testing::ca();
                let config = server_config(&ca, &key, true);
                let trust = d.0.join("trust");
                std::fs::write(&trust, ca.pem()).unwrap();
                let first = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
                let healthy = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
                let addresses = [first.local_addr().unwrap(), healthy.local_addr().unwrap()];
                let server = (mode == "local").then(|| {
                    std::thread::spawn(move || {
                        let mut socket = accept(&healthy);
                        let mut tls = rustls::ServerConnection::new(config).unwrap();
                        while tls.is_handshaking() {
                            match tls.complete_io(&mut socket) {
                                Ok(_) => (),
                                // Bootstrap can finish client-side before its final flight
                                // is flushed. This test drops at checkout, before a request.
                                Err(e) if e.kind() == std::io::ErrorKind::UnexpectedEof => break,
                                Err(e) => panic!("blackhole fixture handshake: {e}"),
                            }
                        }
                        assert_eq!(
                            tls.protocol_version(),
                            Some(rustls::ProtocolVersion::TLSv1_3)
                        );
                        assert_disconnected(&mut socket);
                    })
                });
                let scope = RequestScope::new(
                    crate::model::RequestId([7; 16]),
                    Instant::now() + Duration::from_millis(300),
                )
                .unwrap();
                let io = Rc::new(ConnectProbe {
                    calls: Cell::new(0),
                    held: RefCell::new(None),
                    leases: RefCell::new(Vec::new()),
                    addresses,
                    mode,
                    parent_deadline: scope.deadline.0,
                });
                let transport = ControlTransport::new(ControlEndpoint {
                    url: "https://localhost".into(),
                    trust_bundle: trust,
                });
                let mut driver = ReactorControlIo::new(reactor.clone());
                driver.connect_probe = Some(io.clone());
                transport.attach_io(Rc::new(driver));
                let mut connect = transport.bootstrap(&scope);
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                let watchdog = Instant::now() + Duration::from_secs(2);
                let result = loop {
                    if let Poll::Ready(result) = connect.as_mut().poll(&mut cx) {
                        break result;
                    }
                    assert!(Instant::now() < watchdog);
                    reactor.poll_budgeted(64).unwrap();
                    reactor.wait(Duration::from_millis(1)).unwrap();
                };
                drop(connect);
                match mode {
                    "local" => {
                        let connection = result.unwrap();
                        assert_eq!(io.calls.get(), 2);
                        assert!(
                            Instant::now() < scope.deadline.0,
                            "TLS retains overall budget"
                        );
                        drop(connection);
                    }
                    "cancel" => assert!(matches!(result, Err(Error::Cancelled))),
                    _ => assert!(matches!(result, Err(Error::DeadlineExceeded))),
                }
                if mode != "local" {
                    assert_eq!(io.calls.get(), 1);
                }
                assert!(io.held.borrow().as_ref().unwrap().upgrade().is_none());
                let mut closed = accept(&first);
                closed
                    .set_read_timeout(Some(Duration::from_secs(1)))
                    .unwrap();
                assert_eq!(
                    closed.read(&mut [0; 1]).unwrap(),
                    0,
                    "both failed socket FDs closed"
                );
                assert_eq!(reactor.in_flight(), 0);
                assert!(
                    io.leases
                        .borrow()
                        .iter()
                        .all(|lease| lease.upgrade().is_none())
                );
                if let Some(server) = server {
                    server.join().unwrap();
                }
            }
        }
        fn enrolled(
            r: &Rc<crate::runtime::Reactor>,
            d: &testing::Directory,
            ca: &rcgen::Certificate,
            key: &rcgen::KeyPair,
        ) -> LocalSigningIdentity {
            let scope = testing::scope();
            let enrollment = Enrollment::new(
                racer_control_wire::ClusterId("11111111-1111-4111-8111-111111111111".into()),
                d.0.join("token"),
                d.0.join("identity"),
            );
            enrollment
                .set_peer_trust_roots(vec![ca.der().to_vec()])
                .unwrap();
            enrollment.attach_reactor(r.clone());
            let request = testing::drive(r, enrollment.prepare(&scope)).unwrap();
            testing::drive(
                r,
                enrollment.accept_response_async(
                    testing::issue(&request, ca, key, "22222222-2222-4222-8222-222222222222"),
                    &scope,
                ),
            )
            .unwrap()
        }
        #[test]
        fn real_ring_server_auth_and_mutual_tls() {
            let Some(r) = testing::reactor() else { return };
            let d = testing::Directory::new();
            let (ca, ca_key) = testing::ca();
            let identity = enrolled(&r, &d, &ca, &ca_key);
            let config = server_config(&ca, &ca_key, true);
            let trust = d.0.join("trust.pem");
            std::fs::write(&trust, ca.pem()).unwrap();
            let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
            let port = listener.local_addr().unwrap().port();
            let server = std::thread::spawn(move || {
                for mutual in [false, true] {
                    let mut stream = rustls::StreamOwned::new(
                        rustls::ServerConnection::new(config.clone()).unwrap(),
                        accept(&listener),
                    );
                    let request = read_head(&mut stream);
                    assert_eq!(stream.conn.peer_certificates().is_some(), mutual);
                    assert_eq!(
                        stream.conn.protocol_version(),
                        Some(rustls::ProtocolVersion::TLSv1_3)
                    );
                    if !mutual {
                        assert!(request.contains("Authorization: Bearer fixture.token"));
                    }
                    stream.write_all(b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n2\r\n{}\r\n0\r\n\r\n").unwrap();
                    stream.flush().unwrap();
                }
            });
            let transport = ControlTransport::new(ControlEndpoint {
                url: format!("https://127.0.0.1:{port}"),
                trust_bundle: trust,
            });
            transport.attach_io(Rc::new(ReactorControlIo::new(r.clone())));
            let scope = testing::scope();
            let first = testing::drive(
                &r,
                Box::pin(async {
                    transport
                        .bootstrap(&scope)
                        .await?
                        .request(
                            "POST",
                            wire::BOOTSTRAP_PATH,
                            Some("fixture.token"),
                            &[],
                            65536,
                            &scope,
                        )
                        .await
                }),
            )
            .unwrap();
            assert_eq!(first.body, b"{}");
            let second = testing::drive(
                &r,
                Box::pin(async {
                    transport
                        .authenticated(&identity, &scope)
                        .await?
                        .request("GET", wire::SNAPSHOT_PATH, None, &[], 65536, &scope)
                        .await
                }),
            )
            .unwrap();
            assert_eq!(second.status, 200);
            assert_eq!(second.body, b"{}");
            server.join().unwrap();
        }
        #[test]
        fn backend_disconnect_recovers_after_health_retry_boundary() {
            let Some(r) = testing::reactor() else { return };
            let d = testing::Directory::new();
            let (ca, key) = testing::ca();
            let identity = enrolled(&r, &d, &ca, &key);
            let response = |status| {
                (
                    wire::SNAPSHOT_PATH.to_owned(),
                    status,
                    if status == 204 {
                        vec![]
                    } else {
                        b"{}".to_vec()
                    },
                )
            };
            let (endpoint, server) = scripted_server(
                &d,
                &ca,
                &key,
                vec![vec![response(200), response(0)], vec![response(204)]],
            );
            let transport = ControlTransport::new(endpoint);
            transport.attach_io(Rc::new(ReactorControlIo::new(r.clone())));
            let scope = testing::scope();
            let get = || {
                testing::drive(
                    &r,
                    Box::pin(async {
                        transport
                            .authenticated(&identity, &scope)
                            .await?
                            .request("GET", wire::SNAPSHOT_PATH, None, &[], 65536, &scope)
                            .await
                    }),
                )
            };
            assert_eq!(get().unwrap().status, 200);
            assert!(matches!(get(), Err(Error::Io)));
            // Keep Racer's link circuit integration separate from generic pool policy.
            transport
                .health
                .observe_at(
                    &transport.endpoint,
                    crate::topology::LinkOutcome::Refused,
                    Instant::now() - Duration::from_secs(60),
                )
                .unwrap();
            assert_eq!(get().unwrap().status, 204);
            transport.close_idle();
            server.join().unwrap();
        }
        #[test]
        fn malformed_delta_base_and_method_are_rejected_before_writing() {
            let Some(r) = testing::reactor() else { return };
            let d = testing::Directory::new();
            let (ca, key) = testing::ca();
            let config = server_config(&ca, &key, true);
            let trust = d.0.join("trust");
            std::fs::write(&trust, ca.pem()).unwrap();
            let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
            let endpoint = ControlEndpoint {
                url: format!("https://{}", listener.local_addr().unwrap()),
                trust_bundle: trust,
            };
            let cases = [
                ("DELETE", None),
                ("GET", Some("bad".to_owned())),
                ("GET", Some("g".repeat(64))),
                ("GET", Some("a".repeat(63))),
                ("GET", Some("a".repeat(65))),
                ("GET", Some(format!("{}\r\n", "a".repeat(62)))),
            ];
            let count = cases.len();
            let server = std::thread::spawn(move || {
                for _ in 0..count {
                    let mut stream = rustls::StreamOwned::new(
                        rustls::ServerConnection::new(config.clone()).unwrap(),
                        accept(&listener),
                    );
                    assert_disconnected(&mut stream);
                }
                for base in ["a".repeat(64), "ABCDEF0123456789".repeat(4)] {
                    let mut stream = rustls::StreamOwned::new(
                        rustls::ServerConnection::new(config.clone()).unwrap(),
                        accept(&listener),
                    );
                    assert!(
                        read_head(&mut stream)
                            .ends_with(&format!("X-Racer-Delta-Base: {base}\r\n\r\n"))
                    );
                    stream
                        .write_all(b"HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n")
                        .unwrap();
                    stream.flush().unwrap();
                }
            });
            let transport = ControlTransport::new(endpoint);
            transport.attach_io(Rc::new(ReactorControlIo::new(r.clone())));
            let scope = testing::scope();
            for (method, base) in cases {
                let connection = testing::drive(&r, transport.bootstrap(&scope)).unwrap();
                assert!(matches!(
                    testing::drive(
                        &r,
                        connection.request_delta(
                            method,
                            "/enroll",
                            None,
                            &[],
                            1024,
                            base.as_deref(),
                            &scope,
                        )
                    ),
                    Err(Error::InvalidRequest)
                ));
            }
            for base in ["a".repeat(64), "ABCDEF0123456789".repeat(4)] {
                let connection = testing::drive(&r, transport.bootstrap(&scope)).unwrap();
                assert_eq!(
                    testing::drive(
                        &r,
                        connection.request_delta(
                            "GET",
                            "/enroll",
                            None,
                            &[],
                            1024,
                            Some(&base),
                            &scope,
                        )
                    )
                    .unwrap()
                    .status,
                    204
                );
            }
            server.join().unwrap();
        }
        #[test]
        fn not_yet_valid_identity_is_rejected_before_transport_io() {
            let Some(r) = testing::reactor() else { return };
            let d = testing::Directory::new();
            let (ca, key) = testing::ca();
            let identity = enrolled(&r, &d, &ca, &key);
            assert!(identity.valid_now());
            let clock = uring_runtime::environment::SimulationClock::new(17);
            let environment = clock.environment(1);
            // Roll only the scoped wall clock back after real enrollment. The generic
            // Identity has expiry only; Racer must enforce its not-before policy itself.
            clock.set_wall_time(SystemTime::now() - Duration::from_secs(3600));
            let _guard = environment.enter();
            assert!(!identity.valid_now());
            let transport = ControlTransport::new(ControlEndpoint {
                url: "https://localhost".into(),
                trust_bundle: d.0.join("missing"),
            });
            // No Io attached: reaching generic transport would yield InvalidConfiguration.
            let scope = RequestScope::new(
                crate::model::RequestId([7; 16]),
                uring_runtime::environment::now() + Duration::from_secs(10),
            )
            .unwrap();
            assert!(matches!(
                futures::executor::block_on(transport.authenticated(&identity, &scope)),
                Err(Error::Unauthorized)
            ));
        }
    }

    pub(crate) mod enrollment_tests {
        #[test]
        fn enrollment_requires_exactly_one_identity_san() {
            let enrollment = super::super::Enrollment::new(
                racer_control_wire::ClusterId("11111111-1111-4111-8111-111111111111".into()),
                "/unused/token".into(),
                "/unused/identity".into(),
            );
            let pending = enrollment.generate().unwrap();
            let request = enrollment.request(&pending).unwrap();
            let (ca, key) = testing::ca();
            enrollment
                .set_peer_trust_roots(vec![ca.der().to_vec()])
                .unwrap();
            let response = testing::issue(&request, &ca, &key, OLD_NODE);
            assert!(enrollment.validate(&pending, &response).is_ok());
            for extra in [
                rcgen::SanType::DnsName("extra.example".try_into().unwrap()),
                rcgen::SanType::URI(
                    format!("spiffe://{}/node/{OLD_NODE}", request.cluster.0)
                        .try_into()
                        .unwrap(),
                ),
            ] {
                let der =
                    rustls::pki_types::CertificateSigningRequestDer::from(request.csr_der.clone());
                let mut csr = rcgen::CertificateSigningRequestParams::from_der(&der).unwrap();
                let now = uring_runtime::environment::wall_now();
                csr.params.not_before = (now - Duration::from_secs(1)).into();
                csr.params.not_after = (now + Duration::from_secs(3600)).into();
                csr.params.subject_alt_names = vec![
                    rcgen::SanType::URI(
                        format!("spiffe://{}/node/{OLD_NODE}", request.cluster.0)
                            .try_into()
                            .unwrap(),
                    ),
                    extra,
                ];
                csr.params.key_usages = vec![rcgen::KeyUsagePurpose::DigitalSignature];
                csr.params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ClientAuth];
                let mut multiple = response.clone();
                multiple.certificate_chain = vec![csr.signed_by(&ca, &key).unwrap().der().to_vec()];
                assert!(matches!(
                    enrollment.validate(&pending, &multiple),
                    Err(Error::Unauthorized)
                ));
            }
        }

        #[test]
        fn enrollment_and_renewal_refresh_authenticated_physical_inventory() {
            use rdma_verbs::simulation::Device;
            use rdma_verbs::simulation::Simulation;
            let enrollment = super::Enrollment::new(
                racer_control_wire::ClusterId("11111111-1111-4111-8111-111111111111".into()),
                "/unused/token".into(),
                "/unused/identity".into(),
            );
            let pending = enrollment.generate().unwrap();
            let first = Simulation::new()
                .with_devices(vec![Device::new("nic-a", [1; 16])])
                .unwrap();
            let request = {
                let _environment = first.enter();
                enrollment.inventory.refresh().unwrap();
                enrollment.request(&pending).unwrap()
            };
            assert_eq!(request.rdma_nics.len(), 1);
            assert_eq!(request.rdma_nics[0].device, "nic-a");
            assert_eq!(request.rdma_nics[0].gid, Some([1; 16]));
            let second = Simulation::new()
                .with_devices(vec![Device::new("nic-b", [2; 16])])
                .unwrap();
            let _environment = second.enter();
            enrollment.inventory.refresh().unwrap();
            let renewed = enrollment.request(&pending).unwrap();
            assert_eq!(renewed.enrollment, request.enrollment);
            assert_eq!(renewed.csr_der, request.csr_der);
            assert_eq!(renewed.rdma_nics[0].device, "nic-b");
            let absent = Simulation::new().with_devices(vec![]).unwrap();
            let _environment = absent.enter();
            enrollment.inventory.refresh().unwrap();
            assert!(enrollment.request(&pending).unwrap().rdma_nics.is_empty());
        }
        use super::*;
        use crate::control::tests::testing;
        const OLD_NODE: &str = "22222222-2222-4222-8222-222222222222";
        const NEW_NODE: &str = "33333333-3333-4333-8333-333333333333";

        #[test]
        fn renewal_tracks_short_issued_lifetime_and_preserves_default() {
            for (lifetime, due) in [(120, 80), (86400, 57600), (86700, 57600)] {
                let identity = LocalSigningIdentity {
                    cluster: ClusterId(String::new()),
                    node: NodeId(String::new()),
                    enrollment: EnrollmentId(String::new()),
                    private_material: Vec::new(),
                    certificate_chain: Vec::new(),
                    not_before: 1000,
                    not_after: 1000 + lifetime,
                };
                let clock = uring_runtime::environment::SimulationClock::new_at(
                    51,
                    std::time::Instant::now(),
                    UNIX_EPOCH + Duration::from_secs(1000 + due - 1),
                );
                let _time = clock.environment(1).enter();
                assert!(!identity.renewal_due());
                clock.advance(Duration::from_secs(1));
                assert!(identity.renewal_due());
                assert!(identity.valid_now());
                clock.advance(Duration::from_secs(lifetime - due));
                assert!(!identity.valid_now());
                assert!(identity.renewal_due());
            }
        }

        #[test]
        fn authenticated_replacement_preserves_cluster_key_and_correlation_checks() {
            let Some(r) = testing::reactor() else { return };
            for expired in [false, true] {
                let d = testing::Directory::new();
                let e = Enrollment::new(
                    ClusterId("11111111-1111-4111-8111-111111111111".into()),
                    d.0.join("token"),
                    d.0.join("identity"),
                );
                e.attach_reactor(r.clone());
                let (ca, key) = testing::ca();
                e.set_peer_trust_roots(vec![ca.der().to_vec()]).unwrap();
                let scope = testing::scope();
                let request = testing::drive(&r, e.prepare(&scope)).unwrap();
                testing::drive(
                    &r,
                    e.accept_response_async(testing::issue(&request, &ca, &key, OLD_NODE), &scope),
                )
                .unwrap();
                let clock = uring_runtime::environment::SimulationClock::new_at(
                    31,
                    std::time::Instant::now(),
                    SystemTime::now() + Duration::from_secs(172800),
                );
                let _time = expired.then(|| clock.environment(1).enter());
                if expired {
                    assert!(
                        testing::drive(&r, e.load_identity_async(&scope))
                            .unwrap()
                            .is_none()
                    );
                }
                let old = std::fs::read(d.0.join("identity/identity.json")).unwrap();
                let request = testing::drive(&r, e.prepare(&scope)).unwrap();
                let response = testing::issue(&request, &ca, &key, NEW_NODE);
                let mut wrong_san = response.clone();
                wrong_san.node = NodeId(OLD_NODE.into());
                let mut wrong_id = response.clone();
                wrong_id.enrollment = EnrollmentId(OLD_NODE.into());
                let mut foreign_request = request.clone();
                foreign_request.cluster = ClusterId("44444444-4444-4444-8444-444444444444".into());
                let foreign = testing::issue(&foreign_request, &ca, &key, NEW_NODE);
                let (rogue_ca, rogue_key) = testing::ca();
                let rogue = testing::issue(&request, &rogue_ca, &rogue_key, NEW_NODE);
                let other =
                    Enrollment::new(e.cluster.clone(), d.0.join("token"), d.0.join("other"));
                other.attach_reactor(r.clone());
                let mut other_request = testing::drive(&r, other.prepare(&scope)).unwrap();
                other_request.enrollment = request.enrollment.clone();
                let wrong_key = testing::issue(&other_request, &ca, &key, NEW_NODE);
                for bad in [wrong_san, wrong_id, foreign, rogue, wrong_key] {
                    assert!(matches!(
                        testing::drive(&r, e.accept_response_async(bad, &scope)),
                        Err(Error::Unauthorized)
                    ));
                    assert_eq!(
                        std::fs::read(d.0.join("identity/identity.json")).unwrap(),
                        old
                    );
                    assert!(d.0.join("identity/pending.json").exists());
                }
                let identity =
                    testing::drive(&r, e.accept_response_async(response, &scope)).unwrap();
                assert_eq!(identity.node().0, NEW_NODE);
                assert_eq!(
                    testing::drive(&r, e.load_identity_async(&scope))
                        .unwrap()
                        .unwrap()
                        .node()
                        .0,
                    NEW_NODE
                );
                assert!(!d.0.join("identity/pending.json").exists());

                // Even with new configuration, roots, and a fresh pending request,
                // a hostPath pinned by an existing identity cannot adopt a cluster.
                let foreign = Enrollment::new(
                    foreign_request.cluster,
                    d.0.join("token"),
                    d.0.join("identity"),
                );
                foreign
                    .set_peer_trust_roots(vec![ca.der().to_vec()])
                    .unwrap();
                foreign.attach_reactor(r.clone());
                let request = testing::drive(&r, foreign.prepare(&scope)).unwrap();
                assert!(matches!(
                    testing::drive(
                        &r,
                        foreign.accept_response_async(
                            testing::issue(&request, &ca, &key, NEW_NODE),
                            &scope
                        )
                    ),
                    Err(Error::Unauthorized)
                ));
            }
        }

        #[test]
        fn replacement_crash_at_each_completion_recovers_and_reauthenticates() {
            let Some(r) = testing::reactor() else { return };
            let (ca, key) = testing::ca();
            let mut saw_old = false;
            let mut saw_new = false;
            let mut saw_committed_pending = false;
            for boundary in 0..45 {
                let d = testing::Directory::new();
                let enrollment = || {
                    let e = Enrollment::new(
                        ClusterId("11111111-1111-4111-8111-111111111111".into()),
                        d.0.join("token"),
                        d.0.join("identity"),
                    );
                    e.attach_reactor(r.clone());
                    e.set_peer_trust_roots(vec![ca.der().to_vec()]).unwrap();
                    e
                };
                let e = enrollment();
                let scope = testing::scope();
                let request = testing::drive(&r, e.prepare(&scope)).unwrap();
                testing::drive(
                    &r,
                    e.accept_response_async(testing::issue(&request, &ca, &key, OLD_NODE), &scope),
                )
                .unwrap();
                let request = testing::drive(&r, e.prepare(&scope)).unwrap();
                let mut accept =
                    e.accept_response_async(testing::issue(&request, &ca, &key, NEW_NODE), &scope);
                let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
                for _ in 0..boundary {
                    if accept.as_mut().poll(&mut cx).is_ready() {
                        break;
                    }
                    let until = std::time::Instant::now() + Duration::from_secs(5);
                    while r.in_flight() != 0 {
                        assert!(std::time::Instant::now() < until);
                        if r.poll_budgeted(1).unwrap() == 0 {
                            r.wait(Duration::from_millis(1)).unwrap();
                        }
                    }
                }
                drop(accept);
                testing::drive(&r, r.file_fence(e.previous.get().unwrap())).unwrap();
                let pending_exists = d.0.join("identity/pending.json").exists();
                let restarted = enrollment();
                let recovered = testing::drive(&r, restarted.load_identity_async(&scope))
                    .unwrap()
                    .unwrap();
                saw_old |= recovered.node().0 == OLD_NODE;
                saw_new |= recovered.node().0 == NEW_NODE;
                saw_committed_pending |= recovered.node().0 == NEW_NODE && pending_exists;
                assert!([OLD_NODE, NEW_NODE].contains(&recovered.node().0.as_str()));
                // Startup must submit a request again, regardless of what survived.
                let request = testing::drive(&r, restarted.prepare(&scope)).unwrap();
                let current = testing::drive(
                    &r,
                    restarted.accept_response_async(
                        testing::issue(&request, &ca, &key, NEW_NODE),
                        &scope,
                    ),
                )
                .unwrap();
                assert_eq!(current.node().0, NEW_NODE);
                assert!(!d.0.join("identity/pending.json").exists());
            }
            assert!(saw_old && saw_new && saw_committed_pending);
            assert_eq!(r.in_flight(), 0);
        }

        #[test]
        fn durable_retry_key_pairing_identity_and_rotation() {
            let Some(r) = testing::reactor() else { return };
            let scope = testing::scope();
            let directory = testing::Directory::new();
            let token = directory.0.join("token");
            std::fs::write(&token, "first.token").unwrap();
            let cluster = ClusterId("11111111-1111-4111-8111-111111111111".into());
            let enrollment =
                Enrollment::new(cluster.clone(), token.clone(), directory.0.join("identity"));
            assert!(!directory.0.join("identity").exists());
            enrollment.attach_reactor(r.clone());
            let request = testing::drive(&r, enrollment.prepare(&scope)).unwrap();
            let again = Enrollment::new(cluster, token.clone(), directory.0.join("identity"));
            again.attach_reactor(r.clone());
            assert_eq!(
                request.csr_der,
                testing::drive(&r, again.prepare(&scope)).unwrap().csr_der
            );
            assert_eq!(
                request.enrollment,
                testing::drive(&r, again.prepare(&scope))
                    .unwrap()
                    .enrollment
            );
            assert_eq!(
                &*testing::drive(&r, again.read_token_async(&scope)).unwrap(),
                "first.token"
            );
            std::fs::write(&token, "rotated.token").unwrap();
            assert_eq!(
                &*testing::drive(&r, again.read_token_async(&scope)).unwrap(),
                "rotated.token"
            );
            let (ca, key) = testing::ca();
            again.set_peer_trust_roots(vec![ca.der().to_vec()]).unwrap();
            let response =
                testing::issue(&request, &ca, &key, "22222222-2222-4222-8222-222222222222");
            let mut wrong = response.clone();
            wrong.node.0 = "33333333-3333-4333-8333-333333333333".into();
            assert!(testing::drive(&r, again.accept_response_async(wrong, &scope)).is_err());
            assert!(directory.0.join("identity/pending.json").exists());
            let identity =
                testing::drive(&r, again.accept_response_async(response, &scope)).unwrap();
            assert!(identity.valid_now());
            assert!(!identity.renewal_due());
            assert_eq!(
                testing::drive(&r, again.load_identity_async(&scope))
                    .unwrap()
                    .unwrap()
                    .node(),
                identity.node()
            );
            assert!(!directory.0.join("identity/pending.json").exists());
            let fresh = testing::drive(&r, again.prepare(&scope)).unwrap();
            assert_ne!(fresh.enrollment, request.enrollment);
            assert_ne!(fresh.csr_der, request.csr_der);
            assert_eq!(
                testing::drive(&r, again.load_identity_async(&scope))
                    .unwrap()
                    .unwrap()
                    .node(),
                identity.node()
            );
            std::fs::write(directory.0.join("identity/pending.json"), b"{broken").unwrap();
            assert!(testing::drive(&r, again.prepare(&scope)).is_err());
        }
        #[test]
        fn durable_rail_journal_prevents_restart_renumbering_and_corruption_fails_closed() {
            use rdma_verbs::simulation::Device;
            use rdma_verbs::simulation::Simulation;
            let Some(r) = testing::reactor() else { return };
            let scope = testing::scope();
            let directory = testing::Directory::new();
            let make = || {
                let e = Enrollment::new(
                    ClusterId("11111111-1111-4111-8111-111111111111".into()),
                    directory.0.join("token"),
                    directory.0.join("identity"),
                );
                e.attach_reactor(r.clone());
                e
            };
            let all = Simulation::new()
                .with_devices(vec![Device::new("a", [1; 16]), Device::new("b", [2; 16])])
                .unwrap();
            let first = {
                let _environment = all.enter();
                testing::drive(&r, make().prepare(&scope)).unwrap()
            };
            assert_eq!(first.rdma_nics[1].rail.0, 1);
            let only_b = Simulation::new()
                .with_devices(vec![Device::new("b", [3; 16])])
                .unwrap();
            let _environment = only_b.enter();
            let restarted = testing::drive(&r, make().prepare(&scope)).unwrap();
            assert_eq!(restarted.rdma_nics.len(), 1);
            assert_eq!(restarted.rdma_nics[0].rail.0, 1);
            assert_eq!(restarted.rdma_nics[0].gid, Some([3; 16]));
            std::fs::write(directory.0.join("identity/rdma-rails.json"), b"broken").unwrap();
            assert!(matches!(
                testing::drive(&r, make().prepare(&scope)),
                Err(Error::CorruptRecord)
            ));
        }
        #[test]
        fn saturated_rail_journal_still_persists_and_enrolls_known_ports() {
            use crate::rdma::MAX_JOURNAL_BYTES;
            use racer_control_wire::RailId;
            use racer_control_wire::RailMapping;
            let Some(r) = testing::reactor() else { return };
            let scope = testing::scope();
            let directory = testing::Directory::new();
            let inventory: Arc<crate::rdma::Inventory> = Arc::new(Default::default());
            use rdma_verbs::simulation::Device;
            use rdma_verbs::simulation::Simulation;
            let name = |i| format!("{i:04}{}", "x".repeat(59));
            for batch in 0..20 {
                inventory
                    .update(
                        (batch * 64..(batch + 1) * 64)
                            .map(|i| RailMapping {
                                device: name(i),
                                port: 1,
                                rail: RailId(0),
                                gid: Some([1; 16]),
                                numa_node: None,
                            })
                            .collect(),
                    )
                    .unwrap();
                assert!(inventory.reservations().unwrap().len() <= MAX_JOURNAL_BYTES);
            }
            let journal = inventory.reservations().unwrap();
            let sim = Simulation::new()
                .with_devices(vec![
                    Device::new(name(0), [2; 16]),
                    Device::new(name(2000), [3; 16]),
                ])
                .unwrap();
            let _environment = sim.enter();
            let make = || {
                let e = Enrollment::new(
                    ClusterId("11111111-1111-4111-8111-111111111111".into()),
                    directory.0.join("token"),
                    directory.0.join("identity"),
                )
                .with_inventory(inventory.clone());
                e.attach_reactor(r.clone());
                e
            };
            for _ in 0..2 {
                let request = testing::drive(&r, make().prepare(&scope)).unwrap();
                assert_eq!(request.rdma_nics.len(), 1);
                assert_eq!(request.rdma_nics[0].device, name(0));
                assert_eq!(request.rdma_nics[0].rail, RailId(0));
                assert_eq!(request.rdma_nics[0].gid, Some([2; 16]));
                assert_eq!(
                    std::fs::read(directory.0.join("identity/rdma-rails.json")).unwrap(),
                    journal
                );
            }
        }
        #[test]
        fn rejects_symlinked_identity_and_insecure_modes() {
            let Some(r) = testing::reactor() else { return };
            let scope = testing::scope();
            use std::os::unix::fs::PermissionsExt;
            use std::os::unix::fs::symlink;
            let directory = testing::Directory::new();
            std::fs::create_dir(directory.0.join("real")).unwrap();
            symlink("real", directory.0.join("link")).unwrap();
            let enrollment = Enrollment::new(
                ClusterId("11111111-1111-4111-8111-111111111111".into()),
                directory.0.join("token"),
                directory.0.join("link"),
            );
            enrollment.attach_reactor(r.clone());
            assert!(testing::drive(&r, enrollment.prepare(&scope)).is_err());
            std::fs::set_permissions(
                directory.0.join("real"),
                std::fs::Permissions::from_mode(0o755),
            )
            .unwrap();
            let insecure = Enrollment::new(
                enrollment.cluster.clone(),
                directory.0.join("token"),
                directory.0.join("real"),
            );
            insecure.attach_reactor(r.clone());
            assert!(testing::drive(&r, insecure.prepare(&scope)).is_err());
        }
    }

    pub(crate) mod async_files_tests {
        use super::*;
        use crate::control::Enrollment;
        use crate::control::tests::testing;
        #[test]
        fn enrollment_creates_private_child_beneath_kubelet_host_path() {
            use std::os::unix::fs::PermissionsExt;
            let Some(r) = testing::reactor() else {
                return;
            };
            let d = testing::Directory::new();
            let mount = d.0.join("identity");
            std::fs::create_dir(&mount).unwrap();
            std::fs::set_permissions(&mount, std::fs::Permissions::from_mode(0o755)).unwrap();
            let scope = testing::scope();
            let enrollment = |path| {
                let e = Enrollment::new(
                    racer_control_wire::ClusterId("11111111-1111-4111-8111-111111111111".into()),
                    d.0.join("token"),
                    path,
                );
                e.attach_reactor(r.clone());
                e
            };
            // A DirectoryOrCreate mount itself cannot store private identity state.
            let insecure = enrollment(mount.clone());
            assert!(matches!(
                testing::drive(&r, insecure.load_identity_async(&scope)),
                Err(Error::Unauthorized)
            ));
            let private = mount.join("private");
            let e = enrollment(private.clone());
            assert!(
                testing::drive(&r, e.load_identity_async(&scope))
                    .unwrap()
                    .is_none()
            );
            let request = testing::drive(&r, e.prepare(&scope)).unwrap();
            assert_eq!(
                std::fs::metadata(&private).unwrap().permissions().mode() & 0o777,
                0o700
            );
            assert_eq!(
                std::fs::metadata(&mount).unwrap().permissions().mode() & 0o777,
                0o755
            );
            let (ca, key) = testing::ca();
            e.set_peer_trust_roots(vec![ca.der().to_vec()]).unwrap();
            let response =
                testing::issue(&request, &ca, &key, "22222222-2222-4222-8222-222222222222");
            let identity = testing::drive(&r, e.accept_response_async(response, &scope)).unwrap();
            let restarted = enrollment(private.clone());
            restarted
                .set_peer_trust_roots(vec![ca.der().to_vec()])
                .unwrap();
            assert_eq!(
                testing::drive(&r, restarted.load_identity_async(&scope))
                    .unwrap()
                    .unwrap()
                    .node(),
                identity.node()
            );
            // Keep rejecting insecure existing private directories, even on restart.
            std::fs::set_permissions(&private, std::fs::Permissions::from_mode(0o755)).unwrap();
            assert!(matches!(
                testing::drive(&r, restarted.load_identity_async(&scope)),
                Err(Error::Unauthorized)
            ));
        }
        #[test]
        fn canceled_transaction_never_exposes_partial_replacement() {
            let Some(r) = testing::reactor() else {
                return;
            };
            let d = testing::Directory::new();
            let scope = testing::scope();
            let dir = testing::drive(
                &r,
                Box::pin(directory(&r, &d.0.join("private"), true, true, &scope)),
            )
            .unwrap();
            let old = vec![7; 1001];
            let new = vec![9; 1003];
            for cut in 0..10 {
                let scope = testing::scope();
                testing::drive(&r, Box::pin(atomic_write(&r, &dir, "state", &old, &scope)))
                    .unwrap();
                let mut request = testing::scope();
                request.request = crate::model::RequestId([cut; 16]);
                let mut future = Box::pin(atomic_write(&r, &dir, "state", &new, &request));
                use std::future::Future;
                let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
                for _ in 0..cut {
                    if future.as_mut().poll(&mut cx).is_ready() {
                        break;
                    }
                    let until = std::time::Instant::now() + std::time::Duration::from_secs(5);
                    while r.in_flight() != 0 {
                        assert!(std::time::Instant::now() < until);
                        r.poll_budgeted(1).unwrap();
                        r.wait(std::time::Duration::from_millis(1)).unwrap();
                    }
                }
                request.cancel().unwrap();
                drop(future);
                testing::drive(&r, r.file_fence(request.request)).unwrap();
                let bytes =
                    testing::drive(&r, Box::pin(read_at(&r, &dir, "state", 2000, true, &scope)))
                        .unwrap();
                assert!(
                    *bytes == old || *bytes == new,
                    "partial transaction at cut {cut}"
                );
                testing::drive(
                    &r,
                    Box::pin(atomic_write(&r, &dir, "state", b"retry", &scope)),
                )
                .unwrap();
                assert_eq!(
                    &*testing::drive(&r, Box::pin(read_at(&r, &dir, "state", 2000, true, &scope)))
                        .unwrap(),
                    b"retry"
                );
            }
        }
        #[test]
        fn canceled_replacement_never_exposes_partial_state_and_retry_fences_late_work() {
            let Some(r) = testing::reactor() else {
                return;
            };
            let d = testing::Directory::new();
            let scope = testing::scope();
            let dir = testing::drive(
                &r,
                Box::pin(directory(&r, &d.0.join("private"), true, true, &scope)),
            )
            .unwrap();
            let old = vec![11; 32771];
            let new = vec![22; 65539];
            // Stop between each submission/completion boundary. The worker is not
            // allowed to publish replacement bytes before the final durability fence.
            for stop in 0..12u8 {
                let scope = testing::scope();
                testing::drive(&r, Box::pin(atomic_write(&r, &dir, "state", &old, &scope)))
                    .unwrap();
                let mut turn = testing::scope();
                turn.request = crate::model::RequestId([stop; 16]);
                let mut operation: crate::error::Operation<'_, ()> =
                    Box::pin(atomic_write(&r, &dir, "state", &new, &turn));
                let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
                for _ in 0..stop {
                    if operation.as_mut().poll(&mut cx).is_ready() {
                        break;
                    }
                    let until = std::time::Instant::now() + std::time::Duration::from_secs(5);
                    while r.in_flight() != 0 {
                        assert!(std::time::Instant::now() < until);
                        r.poll_budgeted(1).unwrap();
                        r.wait(std::time::Duration::from_millis(1)).unwrap();
                    }
                }
                turn.cancel().unwrap();
                drop(operation);
                testing::drive(&r, r.file_fence(turn.request)).unwrap();
                let bytes = testing::drive(
                    &r,
                    Box::pin(read_at(&r, &dir, "state", new.len(), true, &scope)),
                )
                .unwrap();
                assert!(
                    *bytes == old || *bytes == new,
                    "partial state at boundary {stop}"
                );
                testing::drive(
                    &r,
                    Box::pin(atomic_write(&r, &dir, "state", b"retry", &scope)),
                )
                .unwrap();
                assert_eq!(
                    &*testing::drive(&r, Box::pin(read_at(&r, &dir, "state", 10, true, &scope)))
                        .unwrap(),
                    b"retry"
                );
            }
            assert_eq!(r.in_flight(), 0);
        }
        #[test]
        fn canceled_replacement_at_each_submission_boundary_is_complete_or_old() {
            let Some(r) = testing::reactor() else {
                return;
            };
            let d = testing::Directory::new();
            let setup = testing::scope();
            let dir = testing::drive(
                &r,
                Box::pin(directory(&r, &d.0.join("private"), true, true, &setup)),
            )
            .unwrap();
            let old = vec![b'a'; 32769];
            let new = vec![b'b'; 49153];
            testing::drive(
                &r,
                Box::pin(atomic_write(&r, &dir, "identity", &old, &setup)),
            )
            .unwrap();
            // Atomic write has seven sequential SQEs: unlink, dir sync, open, write,
            // file sync, rename, dir sync. Cancel before/after each possible boundary.
            for boundary in 0..7 {
                let scope = testing::scope();
                let mut future = Box::pin(atomic_write(&r, &dir, "identity", &new, &scope));
                let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
                for _ in 0..=boundary {
                    assert!(future.as_mut().poll(&mut cx).is_pending());
                    if r.in_flight() == 0 {
                        panic!("missing owned submission");
                    }
                    if boundary == 0 {
                        break;
                    }
                    let until = std::time::Instant::now() + std::time::Duration::from_secs(5);
                    while r.in_flight() != 0 {
                        assert!(std::time::Instant::now() < until);
                        r.poll_budgeted(1).unwrap();
                        r.wait(std::time::Duration::from_millis(1)).unwrap();
                    }
                }
                scope.cancel().unwrap();
                drop(future);
                testing::drive(&r, r.file_fence(scope.request)).unwrap();
                let check = testing::scope();
                let bytes = testing::drive(
                    &r,
                    Box::pin(read_at(&r, &dir, "identity", new.len(), true, &check)),
                )
                .unwrap();
                assert!(
                    &*bytes == &old || &*bytes == &new,
                    "partial committed identity"
                );
                testing::drive(
                    &r,
                    Box::pin(atomic_write(&r, &dir, "identity", &old, &check)),
                )
                .unwrap();
            }
            assert_eq!(r.in_flight(), 0);
        }
        #[test]
        fn cancel_each_persistence_boundary_preserves_complete_old_or_new_file() {
            let Some(r) = testing::reactor() else {
                return;
            };
            let d = testing::Directory::new();
            let scope = testing::scope();
            let dir = testing::drive(
                &r,
                Box::pin(directory(&r, &d.0.join("identity"), true, true, &scope)),
            )
            .unwrap();
            let old = vec![b'a'; 32769];
            let new = vec![b'b'; 32771];
            for boundary in 0..12 {
                let scope = testing::scope();
                testing::drive(&r, Box::pin(atomic_write(&r, &dir, "state", &old, &scope)))
                    .unwrap();
                let mut turn = testing::scope();
                turn.request = crate::model::RequestId([boundary; 16]);
                let mut write: crate::error::Operation<'_, ()> =
                    Box::pin(atomic_write(&r, &dir, "state", &new, &turn));
                let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
                for _ in 0..boundary {
                    if write.as_mut().poll(&mut cx).is_ready() {
                        break;
                    }
                    // Complete one SQE without polling its successor, then interrupt.
                    let until = std::time::Instant::now() + std::time::Duration::from_secs(5);
                    while r.in_flight() != 0 {
                        assert!(std::time::Instant::now() < until);
                        r.poll_budgeted(1).unwrap();
                        r.wait(std::time::Duration::from_millis(1)).unwrap();
                    }
                }
                turn.cancel().unwrap();
                drop(write);
                testing::drive(&r, r.file_fence(turn.request)).unwrap();
                let current = testing::drive(
                    &r,
                    Box::pin(read_at(&r, &dir, "state", new.len(), true, &scope)),
                )
                .unwrap();
                assert!(
                    *current == old || *current == new,
                    "partial replacement at boundary {boundary}"
                );
                // Reusing the deterministic staging name after the fence cannot be
                // overwritten by a late operation from the canceled transaction.
                testing::drive(
                    &r,
                    Box::pin(atomic_write(&r, &dir, "state", b"retry", &scope)),
                )
                .unwrap();
                assert_eq!(
                    &*testing::drive(&r, Box::pin(read_at(&r, &dir, "state", 10, true, &scope)))
                        .unwrap(),
                    b"retry"
                );
            }
            assert_eq!(r.in_flight(), 0);
        }
        #[test]
        fn real_ring_enrollment_durability_projection_and_abandoned_retry() {
            let Some(r) = testing::reactor() else {
                return;
            };
            let d = testing::Directory::new();
            let scope = testing::scope();
            let e = Enrollment::new(
                racer_control_wire::ClusterId("11111111-1111-4111-8111-111111111111".into()),
                d.0.join("token"),
                d.0.join("identity"),
            );
            e.attach_reactor(r.clone());
            let mut abandoned = e.prepare(&scope);
            let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
            assert!(abandoned.as_mut().poll(&mut cx).is_pending());
            // Merely polling cannot perform the open/mkdir. No hidden executor drives it.
            for _ in 0..10 {
                assert!(abandoned.as_mut().poll(&mut cx).is_pending());
            }
            assert!(!d.0.join("identity").exists());
            assert_eq!(r.in_flight(), 1);
            drop(abandoned);
            let pending = testing::drive(&r, e.prepare(&scope)).unwrap();
            assert_eq!(
                pending.csr_der,
                testing::drive(&r, e.prepare(&scope)).unwrap().csr_der
            );
            let (ca, key) = testing::ca();
            e.set_peer_trust_roots(vec![ca.der().to_vec()]).unwrap();
            let response =
                testing::issue(&pending, &ca, &key, "22222222-2222-4222-8222-222222222222");
            let identity = testing::drive(&r, e.accept_response_async(response, &scope)).unwrap();
            assert_eq!(
                identity.node(),
                testing::drive(&r, e.load_identity_async(&scope))
                    .unwrap()
                    .unwrap()
                    .node()
            );
            assert!(!d.0.join("identity/pending.json").exists());
            let bytes = vec![42; 100_003];
            testing::drive(
                &r,
                Box::pin(async {
                    let dir = directory(&r, &d.0.join("identity"), false, true, &scope).await?;
                    atomic_write(&r, &dir, "large", &bytes, &scope).await?;
                    assert_eq!(
                        &*read_at(&r, &dir, "large", bytes.len(), true, &scope).await?,
                        &bytes
                    );
                    Ok(())
                }),
            )
            .unwrap();
            std::fs::create_dir(d.0.join("epoch-a")).unwrap();
            std::fs::write(d.0.join("epoch-a/bundle.json"), b"old").unwrap();
            std::fs::create_dir(d.0.join("epoch-b")).unwrap();
            std::fs::write(d.0.join("epoch-b/bundle.json"), b"new").unwrap();
            std::os::unix::fs::symlink("epoch-a", d.0.join("..data")).unwrap();
            testing::drive(
                &r,
                Box::pin(async {
                    let dir = directory(&r, &d.0, false, false, &scope).await?;
                    let generation = r
                        .file_open(
                            Some(dir),
                            CString::new("..data").unwrap(),
                            libc::O_RDONLY | libc::O_DIRECTORY,
                            BENEATH | NO_MAGICLINKS,
                            &scope,
                        )
                        .await?;
                    std::os::unix::fs::symlink("epoch-b", d.0.join("..next")).unwrap();
                    std::fs::rename(d.0.join("..next"), d.0.join("..data")).unwrap();
                    assert_eq!(
                        &*read_at(&r, &generation, "bundle.json", 3, false, &scope).await?,
                        b"old"
                    );
                    assert_eq!(
                        &*projected_file(&r, &d.0, "bundle.json", 3, &scope).await?,
                        b"new"
                    );
                    Ok(())
                }),
            )
            .unwrap();
            std::fs::remove_file(d.0.join("..data")).unwrap();
            std::os::unix::fs::symlink("../", d.0.join("..data")).unwrap();
            assert!(
                testing::drive(
                    &r,
                    Box::pin(projected_file(&r, &d.0, "bundle.json", 3, &scope))
                )
                .is_err()
            );
            assert_eq!(r.in_flight(), 0);
        }
    }
}
