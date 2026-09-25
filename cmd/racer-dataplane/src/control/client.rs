//! Two HTTPS operations: server-authenticated enrollment and mTLS snapshot polling.
//! One bounded long poll, accepted cursor, jittered retry; no node/status reporting.
use super::{
    caches::{CacheEvent, CacheRegistry},
    enrollment::{Enrollment, LocalSigningIdentity},
    secrets::SecretWatcher,
    snapshot::SnapshotStore,
    transport::{ControlIo, ControlTransport, HttpResponse},
    wire::{self, EnrollmentRequest, EnrollmentResponse, SnapshotRequest, SnapshotResponse},
};
use crate::{
    error::{Error, Operation, Result},
    runtime::deadline::RequestScope,
    security::keyring::Keyring,
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
    enrollment: Rc<Enrollment>,
    keys: RefCell<Rc<Keyring>>,
    secrets: SecretWatcher,
    snapshots: Rc<SnapshotStore>,
    caches: Rc<CacheRegistry>,
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
    events: RefCell<Vec<CacheEvent>>,
    lifecycle: RefCell<Option<Rc<dyn super::caches::CacheLifecycle>>>,
    projection_error: Cell<Option<Error>>,
    renewal_error: Cell<Option<Error>>,
    renew_next: Cell<Option<Instant>>,
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
pub struct ControlProgress {
    pub identity: Option<crate::model::identity::NodeId>,
    pub snapshot: Option<super::snapshot::SnapshotLease>,
    pub cache_events: Vec<CacheEvent>,
    pub next_attempt: Instant,
}
impl ControlClient {
    pub fn new(
        endpoint: ControlEndpoint,
        enrollment: Rc<Enrollment>,
        keys: Rc<Keyring>,
        secrets: SecretWatcher,
        snapshots: Rc<SnapshotStore>,
        caches: Rc<CacheRegistry>,
    ) -> Self {
        Self {
            transport: ControlTransport::new(endpoint),
            enrollment,
            keys: RefCell::new(keys),
            secrets,
            snapshots,
            caches,
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
            events: RefCell::new(Vec::new()),
            lifecycle: RefCell::new(None),
            projection_error: Cell::new(None),
            renewal_error: Cell::new(None),
            renew_next: Cell::new(None),
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
            let token = self.enrollment.read_token()?;
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
            let identity = self.identity.borrow().clone().ok_or(Error::Unauthorized)?;
            let connection = self.transport.authenticated(&identity, scope).await?;
            let path = match request.after {
                Some(n) => format!("{}?after={}", wire::SNAPSHOT_PATH, n.0),
                None => wire::SNAPSHOT_PATH.into(),
            };
            let response = connection
                .request("GET", &path, None, &[], wire::MAX_PUBLICATION_BYTES, scope)
                .await?;
            if response.status == 204 {
                return Ok(SnapshotResponse::Unchanged);
            }
            Ok(SnapshotResponse::Updated(wire::decode_publication(
                &self.response(response, false)?,
            )?))
        })
    }
    pub fn run<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            if self.lifecycle.borrow().is_none() {
                return Err(Error::InvalidConfiguration);
            }
            while !self.started.get() {
                match self.start(scope).await {
                    Ok(_) => (),
                    Err(e) if transient(e) => self.wait_retry(scope).await?,
                    Err(e) => return Err(e),
                }
            }
            self.activate_identity()?;
            loop {
                scope.check()?;
                if self.stopped.get() {
                    return Ok(());
                }
                let now = Instant::now();
                if let Some(next) = self.next.get().filter(|next| *next > now) {
                    self.transport.io()?.sleep(next, scope).await?;
                }
                match self.progress(scope).await {
                    Ok(_) => (),
                    Err(
                        Error::Io
                        | Error::Unavailable
                        | Error::Overloaded
                        | Error::DeadlineExceeded,
                    ) => (),
                    Err(e) => return Err(e),
                }
            }
        })
    }
    /// Runtime must attach its owner-local reactor adapter before start.
    pub fn attach_io(&self, io: Rc<dyn ControlIo>) {
        self.transport.attach_io(io);
    }
    pub fn attach_cache_lifecycle(&self, lifecycle: Rc<dyn super::caches::CacheLifecycle>) {
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
    async fn wait_retry(&self, scope: &RequestScope) -> Result<()> {
        if let Some(next) = self.next.get() {
            self.transport.io()?.sleep(next, scope).await?;
        }
        Ok(())
    }
    fn backoff(&self) -> Result<Instant> {
        let failures = self.failures.get().saturating_add(1);
        self.failures.set(failures);
        let mut random = [0; 8];
        getrandom::getrandom(&mut random).map_err(|_| Error::Io)?;
        let ceiling = (1u64 << failures.saturating_sub(1).min(5)).min(30) * 1000;
        let delay = Duration::from_millis(1000 + u64::from_ne_bytes(random) % (ceiling - 1000 + 1));
        let delay = delay.max(self.retry_after.take().unwrap_or_default());
        Instant::now()
            .checked_add(delay)
            .ok_or(Error::InvalidRequest)
    }
    /// Replace the initially unresolved worker-local view with one built from the
    /// same shared epochs and the authenticated startup NodeId, then activate it.
    pub fn bind_keyring(&self, keys: Rc<Keyring>) -> Result<()> {
        let identity = self.identity.borrow();
        let identity = identity.as_ref().ok_or(Error::Unauthorized)?;
        if keys.node() != identity.node() || keys.cluster() != identity.cluster() {
            return Err(Error::Unauthorized);
        }
        self.secrets.bind_keyring(keys.clone());
        self.secrets.reload_now()?;
        let signing = identity.signing_identity(&keys.peer_trust_roots()?)?;
        keys.install_signing_identity(signing)?;
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
                return self.identity.borrow().clone().ok_or(Error::Unauthorized);
            }
            if self.next.get().is_some_and(|n| n > Instant::now()) {
                return Err(Error::Unavailable);
            }
            let mut turn = scope.clone();
            turn.deadline.0 = turn
                .deadline
                .0
                .min(Instant::now() + Duration::from_secs(40));
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
        let bundle = self.secrets.read_bundle()?;
        if &bundle.cluster != self.enrollment.cluster() {
            return Err(Error::Unauthorized);
        }
        self.enrollment
            .set_peer_trust_roots(bundle.peer_trust_roots.clone())?;
        let identity = match self.enrollment.load_identity()? {
            Some(i) => i,
            None => {
                let request = self.enrollment.prepare(scope).await?;
                self.enrollment
                    .accept_response(self.enroll(&request, scope).await?)?
            }
        };
        *self.identity.borrow_mut() = Some(identity.clone());
        self.started.set(true);
        self.next.set(Some(Instant::now()));
        Ok(identity)
    }
    pub fn activate_identity(&self) -> Result<()> {
        let identity = self.identity.borrow().clone().ok_or(Error::Unauthorized)?;
        let keys = self.keys.borrow();
        if keys.node() != identity.node() || keys.cluster() != identity.cluster() {
            return Err(Error::Unauthorized);
        }
        self.secrets.reload_now()?;
        keys.install_signing_identity(identity.signing_identity(&keys.peer_trust_roots()?)?)
    }
    pub fn identity(&self) -> Option<LocalSigningIdentity> {
        self.identity.borrow().clone()
    }
    /// One bounded owner turn. Call again at next_attempt; only this owner polls.
    pub fn progress<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, ControlProgress> {
        Box::pin(async move {
            let _busy = enter(&self.busy)?;
            scope.check()?;
            if self.stopped.get() {
                return Err(Error::Cancelled);
            }
            if !self.started.get() {
                return Err(Error::InvalidConfiguration);
            }
            if self.next.get().is_some_and(|n| n > Instant::now()) {
                return self.state();
            }
            let mut turn = scope.clone();
            turn.deadline.0 = turn
                .deadline
                .0
                .min(Instant::now() + wire::POLL_WAIT + Duration::from_secs(10));
            *self.active_scope.borrow_mut() = Some(turn.clone());
            let _turn = ActiveTurn(&self.active_scope);
            let result = self.advance(&turn).await;
            match result {
                Ok(()) => {
                    if self.renewal_error.get().is_none() {
                        self.failures.set(0);
                    }
                    self.next.set(Some(Instant::now()));
                    self.state()
                }
                Err(e) => {
                    if transient(e) {
                        self.next.set(Some(self.backoff()?));
                    }
                    Err(e)
                }
            }
        })
    }
    async fn advance(&self, scope: &RequestScope) -> Result<()> {
        // A malformed projection retains the installed epoch and does not stop polls.
        match self.secrets.reload_now() {
            Ok((_, roots)) => {
                self.enrollment.set_peer_trust_roots(roots)?;
                self.projection_error.set(None);
            }
            Err(e) => self.projection_error.set(Some(e)),
        }
        let renewal = self
            .identity
            .borrow()
            .as_ref()
            .is_none_or(|i| i.renewal_due());
        if renewal
            && (self
                .identity
                .borrow()
                .as_ref()
                .is_none_or(|i| !i.valid_now())
                || self.renew_next.get().is_none_or(|n| n <= Instant::now()))
        {
            match self.renew(scope).await {
                Ok(()) => {
                    self.renewal_error.set(None);
                    self.renew_next.set(None);
                }
                Err(e) => {
                    self.renewal_error.set(Some(e));
                    if !transient(e) {
                        return Err(e);
                    }
                    self.renew_next.set(Some(self.backoff()?));
                    if self
                        .identity
                        .borrow()
                        .as_ref()
                        .is_none_or(|i| !i.valid_now())
                    {
                        return Err(e);
                    }
                    // Already accepted identity remains useful while renewal retries.
                }
            }
        }
        match self
            .poll(
                SnapshotRequest {
                    after: self.snapshots.cursor()?,
                },
                scope,
            )
            .await?
        {
            SnapshotResponse::Updated(publication) => {
                // Validate before acceptance; event delivery is infallible after this.
                super::caches::validate_definitions(&publication.caches)?;
                let transition = self
                    .lifecycle
                    .borrow()
                    .as_ref()
                    .map(|l| l.stage(&publication.caches))
                    .transpose()?;
                let snapshot = self.snapshots.publish_staged(publication, transition)?;
                let events = self.caches.reconcile(&snapshot.caches)?;
                self.events.borrow_mut().extend(events);
            }
            SnapshotResponse::Unchanged => (),
        }
        Ok(())
    }
    async fn renew(&self, scope: &RequestScope) -> Result<()> {
        let request = self.enrollment.prepare(scope).await?;
        let response = self.enroll(&request, scope).await?;
        let identity = self.enrollment.accept_response(response)?;
        if self
            .identity
            .borrow()
            .as_ref()
            .is_some_and(|old| old.node() != identity.node())
        {
            return Err(Error::Unauthorized);
        }
        let keys = self.keys.borrow();
        keys.install_signing_identity(identity.signing_identity(&keys.peer_trust_roots()?)?)?;
        *self.identity.borrow_mut() = Some(identity);
        Ok(())
    }
    fn state(&self) -> Result<ControlProgress> {
        Ok(ControlProgress {
            identity: self.identity.borrow().as_ref().map(|i| i.node().clone()),
            snapshot: self.snapshots.current().ok(),
            cache_events: self.events.borrow_mut().drain(..).collect(),
            next_attempt: self.next.get().unwrap_or_else(Instant::now),
        })
    }
    fn response(&self, response: HttpResponse, unchanged: bool) -> Result<Vec<u8>> {
        if response.status == 200 || unchanged && response.status == 204 {
            return Ok(response.body);
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
            self.retry_after.set(response.retry_after);
        }
        Err(error)
    }
    /// Cancels the current owner turn; dropping its future closes its TLS socket.
    pub fn shutdown(&self) -> Result<()> {
        self.stopped.set(true);
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
        control::{snapshot::PublishedState, testing},
        model::identity::{ClusterId, NodeId},
        security::keyring::KeyEpochs,
    };
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
            SecretWatcher::new(d.0.clone(), keys),
            Rc::new(SnapshotStore::new(cluster, Arc::new(PublishedState), 2)),
            Rc::new(CacheRegistry),
        )
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
        use std::os::unix::fs::symlink;
        let d = testing::Directory::new();
        let client = client(&d);
        let (ca, _) = testing::ca();
        let mut bundle = wire::decode_bundle(include_bytes!("testdata/bundle.json")).unwrap();
        bundle.generation.0 = 1;
        bundle.peer_trust_roots = vec![ca.der().to_vec()];
        // Production rejects material reuse across independent key purposes.
        bundle.cache_keys[2].material = [2; 32];
        std::fs::create_dir(d.0.join("epoch")).unwrap();
        std::fs::write(
            d.0.join("epoch/bundle.json"),
            wire::encode_bundle(&bundle).unwrap(),
        )
        .unwrap();
        symlink("epoch", d.0.join("..data")).unwrap();
        assert_eq!(
            client.secrets.reload_now().unwrap().0,
            wire::BundleGeneration(1)
        );
        assert_eq!(
            client.secrets.reload_now().unwrap().0,
            wire::BundleGeneration(1)
        );
        bundle.cache_keys[0].material = [3; 32];
        std::fs::write(
            d.0.join("epoch/bundle.json"),
            wire::encode_bundle(&bundle).unwrap(),
        )
        .unwrap();
        assert!(matches!(client.secrets.reload_now(), Err(Error::Replay)));
        std::fs::write(d.0.join("epoch/bundle.json"), b"{}").unwrap();
        assert!(client.secrets.reload_now().is_err());
        assert!(
            client
                .keys
                .borrow()
                .active(
                    &bundle.cache_keys[0].key.cache,
                    crate::security::keyring::KeyPurpose::Page
                )
                .is_ok()
        );
    }
}
