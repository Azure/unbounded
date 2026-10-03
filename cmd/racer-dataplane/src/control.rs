//! HTTPS enrollment, snapshot polling, and independent network keyring delivery.
//! Bounded long polls, accepted cursors, jittered retry; no node/status reporting.
pub(crate) mod async_files;
pub mod enrollment;
pub mod secrets {
    use super::{
        wire::{self, BundleGeneration, KeyringBundle},
        *,
    };
    use sha2::{Digest, Sha256};

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
        pub fn install(
            &self,
            mut bundle: KeyringBundle,
        ) -> Result<(BundleGeneration, Vec<Vec<u8>>)> {
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
                if bundle.generation < *generation
                    || bundle.generation == *generation && hash != *old
                {
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
    #[cfg(test)]
    mod tests {
        use super::*;
        use std::{os::unix::fs::symlink, sync::Arc};
        #[test]
        fn bundle_installation_is_idempotent_and_rejects_rollback() {
            use racer_identity::{KeyEpochs, KeyPurpose};
            let publication = wire::decode_publication(include_bytes!(concat!(
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
            }
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
                    Box::pin(async_files::projected_file(
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
}
pub mod state;
#[cfg(test)]
pub(crate) mod testing {
    use crate as dataplane;
    use std::{
        path::PathBuf,
        sync::atomic::{AtomicU64, Ordering},
    };
    #[allow(dead_code)]
    mod io {
        include!(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/tests/support/enrollment.rs"
        ));
    }
    pub(super) use io::drive;
    pub(crate) use io::{ControlIo as FixtureIo, ca, signing_identity};
    pub(super) struct Directory(pub PathBuf);
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
    pub(super) fn scope() -> crate::runtime::deadline::RequestScope {
        crate::runtime::deadline::RequestScope::new(
            crate::model::RequestId([7; 16]),
            std::time::Instant::now() + std::time::Duration::from_secs(10),
        )
        .unwrap()
    }
    pub(super) fn reactor() -> Option<std::rc::Rc<crate::runtime::reactor::Reactor>> {
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
    pub(super) fn issue(
        request: &super::wire::EnrollmentRequest,
        ca: &rcgen::Certificate,
        key: &rcgen::KeyPair,
        node: &str,
    ) -> super::wire::EnrollmentResponse {
        issue_at(
            request,
            ca,
            key,
            node,
            uring_runtime::environment::wall_now() - std::time::Duration::from_secs(1),
        )
    }
    pub(crate) fn issue_at(
        request: &super::wire::EnrollmentRequest,
        ca: &rcgen::Certificate,
        key: &rcgen::KeyPair,
        node: &str,
        not_before: std::time::SystemTime,
    ) -> super::wire::EnrollmentResponse {
        let der = rustls::pki_types::CertificateSigningRequestDer::from(request.csr_der.clone());
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
        super::wire::EnrollmentResponse {
            schema_version: 1,
            cluster: request.cluster.clone(),
            node: crate::model::NodeId(node.into()),
            enrollment: request.enrollment.clone(),
            certificate_chain: vec![cert.der().to_vec()],
        }
    }
}
pub mod transport;
pub mod wire;
use racer_identity::Keyring;

use self::{
    enrollment::{Enrollment, LocalSigningIdentity},
    secrets::BundleInstaller,
    state::SnapshotStore,
    transport::{ControlIo, ControlTransport, HttpResponse},
    wire::{EnrollmentRequest, EnrollmentResponse, SnapshotRequest, SnapshotResponse},
};
use crate::{
    error::{Error, Operation, Result},
    runtime::deadline::RequestScope,
};
use std::{
    cell::{Cell, RefCell},
    path::PathBuf,
    rc::Rc,
    time::{Duration, Instant},
};
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
    lifecycle: RefCell<Option<Rc<crate::app::caches::CachePublication>>>,
    projection_error: Cell<Option<Error>>,
    renewal_error: Cell<Option<Error>>,
    renew_next: Cell<Option<Instant>>,
    startup_bundle: RefCell<Option<wire::KeyringBundle>>,
    binding_check: Cell<bool>,
    restart_required: Cell<bool>,
    pending: RefCell<Option<Rc<state::PreparedPublication>>>,
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
            let body = wire::encode_enrollment_request(request)?;
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
            wire::decode_enrollment_response(&body)
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
            if let Ok(full) = wire::decode_publication(&body) {
                return Ok(SnapshotResponse::Updated(full));
            }
            if let Some(s) = snapshot {
                let base = wire::Publication {
                    schema_version: wire::SCHEMA_VERSION,
                    cluster: s.cluster.clone(),
                    sequence: s.sequence,
                    membership_version: s.membership.version,
                    members: s.membership.members().to_vec(),
                    caches: s.caches.clone(),
                };
                if let Ok(next) = wire::apply_delta(&base, &body) {
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
            Ok(SnapshotResponse::Updated(wire::decode_publication(
                &self.response(response, false)?,
            )?))
        })
    }
    /// Runtime must attach its owner-local reactor adapter before start.
    pub fn attach_io(&self, io: Rc<dyn ControlIo>) {
        if let Some(reactor) = io.reactor() {
            self.enrollment.attach_reactor(reactor);
        }
        self.key_transport.attach_io(io.clone());
        self.transport.attach_io(io);
    }
    pub(crate) fn attach_cache_publication(
        &self,
        lifecycle: Rc<crate::app::caches::CachePublication>,
    ) {
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
            use std::{future::Future, task::Poll};
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
        response: HttpResponse,
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
    fn response(&self, response: HttpResponse, unchanged: bool) -> Result<Vec<u8>> {
        Self::response_with_retry(response, unchanged, &self.retry_after)
    }
    fn response_with_retry(
        mut response: HttpResponse,
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
#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        control::{state::PublishedState, testing},
        model::{ClusterId, NodeId},
    };
    use racer_identity::KeyEpochs;
    use std::sync::Arc;
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
            Rc::new(SnapshotStore::new(cluster, Arc::new(PublishedState), 2)),
        )
    }
    #[test]
    fn lagging_replica_retries_preserve_state_without_enrollment() {
        let Some(r) = testing::reactor() else { return };
        let scope = testing::scope();
        use super::transport::scenarios::{FixtureIo, scripted_server};
        use futures::executor::block_on;
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
            let mut publication = wire::decode_publication(include_bytes!(concat!(
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
            let (_, canonical_members) = wire::canonical_content(&publication).unwrap();
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
                        (path, 200, wire::encode_publication(&publication).unwrap()),
                        (
                            format!("{}?after=12", wire::SNAPSHOT_PATH),
                            200,
                            wire::encode_publication(&rollback).unwrap(),
                        ),
                    ],
                ],
            );
            client.transport = ControlTransport::new(endpoint);
            client.attach_io(Rc::new(FixtureIo));
            let scope = testing::scope();
            for error in [Error::Unavailable, Error::Overloaded] {
                assert_eq!(block_on(client.poll_publication(&scope)), Err(error));
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
                assert_eq!(block_on(client.renew_if_due(&scope)), Ok(()));
                assert_eq!(client.renewal_error(), None);
                assert!(!d.0.join("identity/pending.json").exists());
                assert_eq!(
                    client.identity().unwrap().certificate_chain(),
                    identity.certificate_chain()
                );
            }
            assert_eq!(block_on(client.poll_publication(&scope)), Ok(()));
            assert!(Arc::ptr_eq(&accepted, &client.snapshots.current().unwrap()));
            assert_eq!(block_on(client.poll_publication(&scope)), Ok(()));
            assert_eq!(
                client.snapshots.cursor().unwrap(),
                Some(wire::PublicationSequence(12))
            );
            assert!(client.pending.borrow().is_none());
            let diagnostic = client.membership_diagnostic().unwrap();
            assert_eq!(diagnostic.accepted_sequence, 12);
            assert_eq!(diagnostic.pending_sequence, 0);
            assert_eq!(
                block_on(client.poll_publication(&scope)),
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
        let unavailable = HttpResponse {
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
                HttpResponse {
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
                HttpResponse {
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
                    HttpResponse {
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
        let response = |status, body: &[u8]| HttpResponse {
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
