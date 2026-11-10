//! Racer control policy and reactor adapters for enrollment and immutable publications.

#[cfg(test)]
use crate::control::rest::Response;
use crate::drivers::Busy;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
#[cfg(test)]
use crate::runtime::Reactor;
use crate::runtime::RequestScope;
#[cfg(test)]
use crate::topology::Membership;
#[cfg(test)]
use racer_crypto::identity::BundleInstaller;
use racer_crypto::identity::KeyPurpose;
use racer_crypto::identity::Keyring;
use racer_io::ring::Descriptor;
#[cfg(test)]
use racer_io::scoped::fs::prelude::*;
use racer_io::scoped::fs::secure;
#[cfg(test)]
use racer_io::scoped::fs::secure::{
    BENEATH, NO_MAGICLINKS, atomic_write, directory, read_at, read_path,
};
use racer_wire::control as wire;
#[cfg(test)]
use racer_wire::control::BundleGeneration;
use racer_wire::control::CacheDefinition;
use racer_wire::control::CacheId;
use racer_wire::control::ClusterId;
#[cfg(test)]
use racer_wire::control::EnrollmentId;
use racer_wire::control::KeyId;
#[cfg(test)]
use racer_wire::control::MembershipVersion;
#[cfg(test)]
use racer_wire::control::NodeId;
#[cfg(test)]
use racer_wire::control::Publication;
use racer_wire::control::PublicationSequence;
use std::cell::Cell;
#[cfg(test)]
use std::cell::RefCell;
#[cfg(test)]
use std::ffi::CString;
use std::net::SocketAddr;
#[cfg(test)]
#[cfg(test)]
#[cfg(test)]
use std::path::Path;
use std::path::PathBuf;
use std::rc::Rc;
use std::sync::Arc;
#[cfg(test)]
use std::time::Duration;
use std::time::Instant;
#[cfg(test)]
use std::time::SystemTime;

pub mod cell;
pub mod managed;
pub mod poll;
pub mod publication;
pub mod rails;
pub mod rest;
pub mod session;
use crate::control::cell::PublicationCell;
#[cfg(test)]
use publication::PublicationTarget;

// HTTPS enrollment, snapshot polling, and independent network keyring delivery.
// Bounded long polls, accepted cursors, jittered retry; no node/status reporting.

/// Controller address and deployment-provided server trust.
pub struct ControlEndpoint {
    /// HTTPS base URL shared by enrollment and control feeds.
    pub url: String,

    /// Deployment-provided trust, independent of rotating peer trust roots.
    pub trust_bundle: PathBuf,

    /// Elected, certificate-pinned endpoints that replace `url` and
    /// `trust_bundle` in managed mode. Discovery refreshes the shared set.
    pub discovered: Option<rest::DiscoveredEndpoints>,
}

/// Report this node's shares and RDMA rails to the embedded cluster control.
///
/// Called from the control reactor; implementations must only enqueue work.
pub type Report = Arc<dyn Fn(u32, Vec<wire::RailMapping>) + Send + Sync>;

/// Managed-mode bindings from the embedded authority service.
#[derive(Clone)]
pub struct Managed {
    /// Elected endpoints refreshed by discovery; read on every connection checkout.
    pub endpoints: rest::DiscoveredEndpoints,

    /// Background Pod report of shares and discovered rails.
    pub report: Report,

    /// Device selector from control bootstrap; preferred over the enrollment copy.
    pub block_devices: Option<String>,
}

/// Reject overlapping owner-local operations without retaining a borrow.
fn enter(flag: &Cell<bool>) -> Result<Busy<'_>> {
    Busy::try_enter(flag).map_err(Into::into)
}

/// Identify failures that may recover on a later scoped attempt.
pub(crate) fn transient(e: Error) -> bool {
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

/// Immutable Racer domain view with independently versioned topology.
pub struct Snapshot {
    /// Cluster whose validated state this generation describes.
    pub cluster: ClusterId,

    /// Accepted publication cursor, independent of the membership version.
    pub sequence: PublicationSequence,

    /// Shared immutable topology retained by in-flight operations.
    pub membership: std::sync::Arc<crate::topology::Membership>,

    /// Cache namespaces admitted by this publication, indexed by id.
    pub caches: Arc<Caches>,

    /// Node pools of `caches`, as validated from the publication. Placement
    /// reads them through `membership`.
    pub pools: Vec<racer_wire::control::Pool>,

    /// Cluster-wide page size, fixed for the life of the process: a publication that
    /// changes it is rejected.
    pub page_size: racer_wire::object::PageSize,

    content_hash: [u8; 32],

    membership_hash: [u8; 32],
}

#[cfg(test)]
impl Snapshot {
    /// Build a membership-only fixture without exposing mutable generation digests.
    pub(crate) fn membership_fixture(membership: Arc<Membership>) -> Self {
        Self {
            cluster: ClusterId("test".into()),
            sequence: PublicationSequence(1),
            membership,
            caches: Arc::default(),
            pools: Vec::new(),
            page_size: racer_wire::object::PageSize::DEFAULT,
            content_hash: [0; 32],
            membership_hash: [0; 32],
        }
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
    /// Publish staged resources infallibly while the publication lock is held.
    fn commit(self: Box<Self>);
}

/// Current positive admission set, shared by cache lookups and late publications.
/// No removal history is needed: an absent UID/key is a miss. Reintroducing a UID
/// denotes the same immutable namespace; a different namespace requires a new UID.
pub struct Availability {
    publications: Arc<PublicationCell<Snapshot>>,

    keys: Rc<Keyring>,

    /// Worker-local copy of the published cache index, reloaded on epoch change.
    /// It holds only the cache index, never the membership, so idle workers do
    /// not consume publication grace capacity.
    caches: cell::EpochCopy<Arc<Caches>>,
}

impl Availability {
    /// Bind admission checks to current publications and worker-local keys.
    pub fn new(publications: Arc<PublicationCell<Snapshot>>, keys: Rc<Keyring>) -> Self {
        Self {
            publications,
            keys,
            caches: cell::EpochCopy::default(),
        }
    }

    /// Run `f` over the current cache definition, if published. Takes no lock
    /// and no shared reference count while the publication is unchanged.
    #[inline]
    fn with_cache<R>(&self, cache: &CacheId, f: impl FnOnce(Option<&CacheDefinition>) -> R) -> R {
        self.caches.with(
            &self.publications,
            |s| s.caches.clone(),
            |caches| f(caches.and_then(|c| c.find(cache))),
        )
    }

    /// Check whether the current publication admits this cache.
    pub fn cache(&self, cache: &CacheId) -> bool {
        self.with_cache(cache, |c| c.is_some())
    }

    /// Published mode of this cache. Unpublished caches report `Cached`; the
    /// other admission checks already refuse them.
    pub fn mode(&self, cache: &CacheId) -> racer_wire::control::CacheMode {
        self.with_cache(cache, |c| c.map(|c| c.mode).unwrap_or_default())
    }

    /// Whether this cache retains no pages or metadata.
    pub fn passthrough(&self, cache: &CacheId) -> bool {
        self.mode(cache).passthrough()
    }

    /// Require both cache admission and an active page key for metadata reads.
    pub fn metadata(&self, cache: &CacheId) -> bool {
        self.cache(cache) && self.keys.has_active(cache, KeyPurpose::Page)
    }

    /// Generation of the installed keyring, which changes whenever keys are
    /// added or retired. Retention sweeps restart when it changes.
    pub fn key_generation(&self) -> Option<u64> {
        self.keys.generation().ok().flatten()
    }

    /// Require cache admission and the exact key needed by a stored page.
    pub fn page(&self, cache: &CacheId, key: KeyId) -> bool {
        self.cache(cache) && self.keys.admits(cache, key, KeyPurpose::Page)
    }
}

/// Published cache definitions in publication order, with an id index built
/// once at publication so lookups by id are logarithmic, not linear.
#[derive(Debug, Default)]
pub struct Caches {
    definitions: Vec<CacheDefinition>,

    /// Positions into `definitions`, sorted by cache id.
    by_id: Vec<u32>,
}

impl Caches {
    /// Index definitions by id.
    pub fn new(definitions: Vec<CacheDefinition>) -> Self {
        let mut by_id: Vec<u32> = (0..definitions.len() as u32).collect();
        by_id.sort_by(|a, b| {
            definitions[*a as usize]
                .id
                .cmp(&definitions[*b as usize].id)
        });
        Self { definitions, by_id }
    }

    /// Position of `cache` in publication order.
    pub fn position(&self, cache: &CacheId) -> Option<usize> {
        self.by_id
            .binary_search_by(|i| self.definitions[*i as usize].id.cmp(cache))
            .ok()
            .map(|i| self.by_id[i] as usize)
    }

    /// Definition of `cache`, if published.
    pub fn find(&self, cache: &CacheId) -> Option<&CacheDefinition> {
        self.position(cache).map(|i| &self.definitions[i])
    }

    /// Whether `cache` is published.
    pub fn contains(&self, cache: &CacheId) -> bool {
        self.position(cache).is_some()
    }
}

impl std::ops::Deref for Caches {
    type Target = [CacheDefinition];

    fn deref(&self) -> &[CacheDefinition] {
        &self.definitions
    }
}

impl From<Vec<CacheDefinition>> for Caches {
    fn from(definitions: Vec<CacheDefinition>) -> Self {
        Self::new(definitions)
    }
}

/// Publish a single-node fixture admitting the supplied cache namespaces.
#[cfg(test)]
pub(crate) fn for_caches(
    env: &racer_io::Env,
    keys: Rc<Keyring>,
    caches: Vec<CacheId>,
) -> Rc<Availability> {
    for_cache_modes(
        env,
        keys,
        caches
            .into_iter()
            .map(|id| (id, racer_wire::control::CacheMode::Cached))
            .collect(),
    )
}

/// Publish a single-node fixture admitting the supplied caches in their modes.
#[cfg(test)]
pub(crate) fn for_cache_modes(
    env: &racer_io::Env,
    keys: Rc<Keyring>,
    caches: Vec<(CacheId, racer_wire::control::CacheMode)>,
) -> Rc<Availability> {
    let availability = Rc::new(Availability::new(
        Arc::new(PublicationCell::new(Snapshot::retention(1))),
        keys,
    ));
    availability.publish_modes(env, caches);
    availability
}

#[cfg(test)]
impl Availability {
    /// Publish the next single-node fixture with `caches` in their modes, as a
    /// controller would after a ClusterCache edit.
    pub(crate) fn publish_modes(
        &self,
        env: &racer_io::Env,
        caches: Vec<(CacheId, racer_wire::control::CacheMode)>,
    ) {
        use racer_wire::control::SCHEMA_VERSION;
        let sequence = self
            .publications
            .current()
            .unwrap()
            .map_or(1, |snapshot| snapshot.sequence.0 + 1);
        PublicationTarget::new(
            self.keys.cluster().clone(),
            self.publications.clone(),
            env.ctx.clock.clone(),
        )
        .apply(
            Publication {
                pools: Vec::new(),
                schema_version: SCHEMA_VERSION,
                cluster: self.keys.cluster().clone(),
                sequence: PublicationSequence(sequence),
                membership_version: racer_wire::control::MembershipVersion(1),
                members: vec![wire::Member {
                    node: self.keys.node().clone(),
                    shares: std::num::NonZeroU32::new(1).unwrap(),
                    peer_endpoint: "127.0.0.1:7443".into(),
                    rails: vec![],
                    site: String::new(),
                }],
                caches: caches
                    .into_iter()
                    .enumerate()
                    .map(|(i, (id, mode))| {
                        let name = format!("rotation-{i}");
                        let (client_socket, origin_socket) =
                            wire::canonical_socket_paths(&name).unwrap();
                        CacheDefinition {
                            id,
                            name,
                            client_socket,
                            origin_socket,
                            priority: racer_wire::control::DEFAULT_PRIORITY,
                            mode,
                            origin: None,
                            writes: None,
                        }
                    })
                    .collect(),
                page_size: racer_wire::object::PageSize::DEFAULT,
            }
            .with_default_pool(),
        )
        .unwrap();
    }
}

// Racer policy and health around the generic worker-local REST transport.

/// Racer's owner-local filesystem, timer, admission, and readiness adapter.
pub struct ReactorControlIo {
    reactor: Rc<crate::runtime::Reactor>,

    #[cfg(test)]
    connect_probe: Option<Rc<tests::scenarios::ConnectProbe>>,
}

impl secure::Host for ReactorControlIo {
    type Scope = RequestScope;

    fn reactor(&self) -> &racer_io::scoped::Reactor<Self::Scope> {
        &self.reactor
    }

    fn fresh_scope(&self, parent: &RequestScope) -> Result<RequestScope> {
        parent.check()?;
        let mut scope = parent.clone();
        let mut id = [0; 16];
        self.reactor
            .ctx()
            .rng
            .fill(&mut id)
            .map_err(|_| Error::Io)?;
        scope.request = crate::model::RequestId(id);
        Ok(scope)
    }

    fn fence<'a>(&'a self, previous: &'a RequestScope) -> Operation<'a, ()> {
        self.reactor.file_fence(previous.request)
    }

    fn is_missing(error: Error) -> bool {
        error == Error::MissingKey
    }
}

impl rest::Io for ReactorControlIo {
    type Reactor = crate::runtime::Reactor;

    type Charge = flow_control::Charge<crate::admission::AdmissionPolicy>;

    fn reactor(&self) -> Rc<crate::runtime::Reactor> {
        self.reactor.clone()
    }

    fn charge(&self, bytes: usize) -> Result<Self::Charge> {
        self.reactor
            .admission()
            .reserve(None, crate::admission::ResourceClass::RequestContext, bytes)
            .map_err(Into::into)
    }

    fn ctx(&self) -> &racer_io::WorkerCtx {
        self.reactor.ctx()
    }

    fn sim(&self) -> Option<racer_io::sim::SimOs> {
        self.reactor.sim_os()
    }

    type FileBytes = racer_io::scoped::fs::ReadBuffer;

    type Error = Error;

    type Scope = RequestScope;

    type Lease = crate::admission::ConnectionReservation;

    fn lease(&self) -> Result<Option<Rc<Self::Lease>>> {
        let lease = Rc::new(
            self.reactor
                .reserve_connection(crate::admission::ResourceClass::ControlConnection)?,
        );
        #[cfg(test)]
        if let Some(probe) = &self.connect_probe {
            probe.observe(&lease);
        }
        Ok(Some(lease))
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
            let interest =
                if read { libc::POLLIN } else { 0 } | if write { libc::POLLOUT } else { 0 };
            self.reactor
                .readiness_with_lease(fd, interest as u32, lease, scope)
                .await
                // This boundary is socket readiness only. Keep availability
                // retries here, never on the shared filesystem error conversion.
                .map_err(|error| match error {
                    Error::Os(_) => Error::Unavailable,
                    error => error,
                })?;
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
            rest::dns::resolve(self, host, port, scope).await
        })
    }

    fn read_file<'a>(
        &'a self,
        path: &'a std::path::Path,
        limit: usize,
        scope: &'a RequestScope,
    ) -> Operation<'a, Self::FileBytes> {
        Box::pin(async move { secure::read_path(&self.reactor, path, limit, scope).await })
    }
}

impl ReactorControlIo {
    /// Use the serving worker's reactor for all control I/O and timers.
    pub fn new(reactor: Rc<crate::runtime::Reactor>) -> Self {
        Self {
            reactor,
            #[cfg(test)]
            connect_probe: None,
        }
    }

    /// Wait until an absolute deadline under the caller's cancellation scope.
    pub fn sleep<'a>(&'a self, until: Instant, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move { self.reactor.sleep_until(until, scope).await })
    }
}

/// Build client-certificate verification from the supplied peer trust roots.
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

#[cfg(test)]
pub(crate) mod tests {
    //! Control transport, publication, and durable identity lifecycle scenarios.

    use super::*;
    use crate::enrollment::{Enrollment, LocalSigningIdentity};
    use crate::test_support::enrollment as testing;
    use crate::test_support::projected_file;
    use futures::TryFutureExt;
    use racer_wire::control::canonical_socket_paths;
    use racer_wire::control::decode_enrollment_request;
    use racer_wire::control::decode_publication;
    use racer_wire::control::encode_enrollment_request;
    use racer_wire::control::encode_publication;
    use racer_wire::control::validate_definitions;
    use std::num::NonZeroU32;

    #[test]
    fn socket_readiness_errno_is_transient_but_file_errno_is_not() {
        use crate::control::rest::Io;
        use racer_io::sim::{Fault, SimOs};
        let sim = SimOs::new();
        let env = racer_io::Env::sim(&sim, racer_io::WorkerCtx::host());
        let r = Rc::new(Reactor::new(
            Rc::new(flow_control::Quotas::new(
                crate::admission::AdmissionPolicy::new(
                    crate::test_support::cluster::config(false).limits,
                ),
            )),
            &env,
        ));
        let io = ReactorControlIo::new(r.clone());
        let scope = testing::scope();
        let (socket, _peer) = sim.socket_pair();
        sim.inject("poll", Fault::Errno(libc::EIO)).unwrap();
        let error =
            testing::drive(&r, io.ready(Rc::new(socket), true, false, None, &scope)).unwrap_err();
        assert_eq!(error, Error::Unavailable);
        assert!(transient(error));
        sim.write_file(Path::new("/token"), b"token").unwrap();
        sim.inject("read", Fault::Errno(libc::EIO)).unwrap();
        let result = testing::drive(&r, io.read_file(Path::new("/token"), 32, &scope));
        assert!(matches!(result, Err(Error::Os(libc::EIO))));
        assert!(!transient(Error::Os(libc::EIO)));
    }

    #[test]
    fn atomic_write_preserves_cause_and_requires_reconciliation_after_publication() {
        use crate::error::PublicationCause;
        use racer_io::sim::{Fault, SimOs};
        use std::future::Future;
        use std::task::{Context, Poll};
        for case in [
            "success",
            "write",
            "rename",
            "cancel-before-rename",
            "cancel-rename",
            "sync",
        ] {
            let sim = SimOs::new();
            let env = racer_io::Env::sim(&sim, racer_io::WorkerCtx::host());
            let r = Reactor::new(
                Rc::new(flow_control::Quotas::new(
                    crate::admission::AdmissionPolicy::new(
                        crate::test_support::cluster::config(false).limits,
                    ),
                )),
                &env,
            );
            let scope = testing::scope();
            sim.write_file(Path::new("/identity"), b"old").unwrap();
            sim.disk().sync_all().unwrap();
            let dir = testing::drive(
                &r,
                r.file_open(
                    None,
                    CString::new("/").unwrap(),
                    libc::O_RDONLY | libc::O_DIRECTORY,
                    0,
                    &scope,
                ),
            )
            .unwrap();
            match case {
                "write" => sim.inject("write", Fault::Errno(libc::ENOSPC)).unwrap(),
                "rename" => sim.inject("rename", Fault::Errno(libc::EIO)).unwrap(),
                "cancel-rename" | "sync" => {
                    sim.inject("rename", Fault::HoldCompletion(20)).unwrap()
                }
                // Keep the rename submitted but unexecuted so the cancel wins.
                "cancel-before-rename" => sim.inject("rename", Fault::Delay(1_000_000)).unwrap(),
                _ => (),
            }
            let mut operation = Box::pin(atomic_write(&r, &dir, "identity", b"new", &scope));
            if case == "cancel-before-rename" {
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                for _ in 0..100 {
                    assert!(matches!(operation.as_mut().poll(&mut cx), Poll::Pending));
                    r.poll_budgeted(64).unwrap();
                    r.wait(Duration::from_millis(1)).unwrap();
                    if sim
                        .trace()
                        .iter()
                        .any(|event| event.operation == "submit:rename")
                    {
                        break;
                    }
                }
                assert_eq!(sim.read_file(Path::new("/identity")).unwrap(), b"old");
                scope.cancel().unwrap();
            }
            if matches!(case, "cancel-rename" | "sync") {
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                for _ in 0..100 {
                    assert!(matches!(operation.as_mut().poll(&mut cx), Poll::Pending));
                    r.poll_budgeted(64).unwrap();
                    r.wait(Duration::from_millis(1)).unwrap();
                    if sim
                        .trace()
                        .iter()
                        .any(|event| event.operation == "complete:rename")
                    {
                        break;
                    }
                }
                assert_eq!(sim.read_file(Path::new("/identity")).unwrap(), b"new");
                if case == "cancel-rename" {
                    scope.cancel().unwrap();
                } else {
                    sim.inject("fsync", Fault::Errno(libc::EIO)).unwrap();
                }
            }
            let result = testing::drive(&r, Box::pin(operation.map_err(Error::from)));
            let expected = match case {
                "write" => Err(Error::Os(libc::ENOSPC)),
                "rename" => Err(Error::RenameUncertain(PublicationCause::Os(libc::EIO))),
                // A cancel before the rename runs leaves its outcome uncertain.
                "cancel-before-rename" => Err(Error::RenameUncertain(PublicationCause::Canceled)),
                // The rename already succeeded, so the cancel only stops the sync.
                "cancel-rename" => Err(Error::PublishedNotDurable(PublicationCause::Canceled)),
                "sync" => Err(Error::PublishedNotDurable(PublicationCause::Os(libc::EIO))),
                _ => Ok(()),
            };
            assert_eq!(result, expected, "{case}");
            if let Err(error) = result {
                // Control must fail closed, not replay a namespace mutation just
                // because its underlying cause looks transient.
                assert!(!transient(error), "{case}: {error:?}");
            }
            testing::drive(&r, r.file_fence(scope.request)).unwrap();
            assert_eq!(r.in_flight(), 0);
            let published = matches!(case, "success" | "cancel-rename" | "sync");
            let expected_bytes = if published { b"new" } else { b"old" };
            let recovery = testing::scope();
            let read = testing::drive(
                &r,
                Box::pin(read_at(&r, &dir, "identity", 32, false, &recovery)),
            )
            .unwrap();
            assert_eq!(&*read, expected_bytes);
            // Reconcile the actual target and certify its namespace durability;
            // do not issue another replacement to make an ambiguous error vanish.
            testing::drive(&r, r.file_sync(dir, &recovery)).unwrap();
            sim.disk().crash().unwrap();
            assert_eq!(
                sim.read_file(Path::new("/identity")).unwrap(),
                expected_bytes
            );
            assert_eq!(
                sim.trace()
                    .iter()
                    .filter(|event| event.operation == "submit:rename")
                    .count(),
                usize::from(case != "write")
            );
        }
    }

    pub(crate) mod bundle_tests {
        //! Key bundle validation and replay behavior at the application boundary.

        use super::*;
        use std::os::unix::fs::symlink;
        use std::sync::Arc;
        #[test]
        fn bundle_installation_is_idempotent_and_rejects_rollback() {
            use racer_crypto::identity::KeyEpochs;
            use racer_crypto::identity::KeyPurpose;
            let publication = racer_wire::control::decode_publication(include_bytes!(concat!(
                env!("CARGO_MANIFEST_DIR"),
                "/wire/testdata/publication.json"
            )))
            .unwrap();
            let keys = Rc::new(Keyring::new(
                publication.cluster,
                publication.members[0].node.clone(),
                Arc::new(KeyEpochs::with_clock(crate::enrollment::wall_clock(
                    &racer_io::Clock::host(),
                ))),
            ));
            let installer = BundleInstaller::new(keys.clone());
            let (ca, _) = testing::ca();
            let mut bundle = wire::decode_bundle(include_bytes!(concat!(
                env!("CARGO_MANIFEST_DIR"),
                "/wire/testdata/bundle.json"
            )))
            .unwrap();
            bundle.generation = BundleGeneration(2);
            bundle.peer_trust_roots = vec![ca.der().to_vec()];
            // The wire vector repeats material across purposes; use distinct material.
            let key = &bundle.cache_keys[2];
            bundle.cache_keys[2] = wire::CacheEncryptionKey::new(
                key.key.clone(),
                key.state,
                zeroize::Zeroizing::new([2; 32]),
            );
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
            assert!(matches!(
                installer.install(conflict),
                Err(racer_crypto::identity::BundleError::Replay)
            ));
            let mut conflict = bundle.clone();
            let key = &conflict.cache_keys[0];
            conflict.cache_keys[0] = wire::CacheEncryptionKey::new(
                key.key.clone(),
                key.state,
                zeroize::Zeroizing::new([3; 32]),
            );
            assert!(matches!(
                installer.install(conflict),
                Err(racer_crypto::identity::BundleError::Replay)
            ));
            assert!(wire::decode_bundle(b"{}").is_err());
            bundle.generation = BundleGeneration(0);
            assert!(installer.install(bundle).is_err());
            assert_eq!(installer.generation(), Some(BundleGeneration(2)));
            assert!(keys.active(&cache, KeyPurpose::Page).is_ok());
        }
        #[test]
        fn coherent_projection_rejects_partial_and_escaping_links() {
            let env = racer_io::Env::host();
            let Some(reactor) = testing::reactor(&env) else {
                return;
            };
            let d = testing::Directory::new();
            let scope = testing::scope();
            let read = || {
                testing::drive(
                    &reactor,
                    Box::pin(crate::test_support::projected_file(
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
                    "/wire/testdata/bundle.json"
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

    pub(crate) mod client_tests {
        //! Scripted control client responses and synchronization progress.

        use super::session::Session;
        use super::*;
        use crate::test_support::enrollment as testing;
        use racer_crypto::identity::KeyEpochs;
        use racer_wire::control::ClusterId;
        use racer_wire::control::NodeId;
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
        fn client(d: &testing::Directory, reactor: Rc<Reactor>) -> Session {
            let io = Rc::new(ReactorControlIo::new(reactor));
            let cluster = ClusterId("11111111-1111-4111-8111-111111111111".into());
            let keys = Rc::new(Keyring::new(
                cluster.clone(),
                NodeId("22222222-2222-4222-8222-222222222222".into()),
                Arc::new(KeyEpochs::with_clock(crate::enrollment::wall_clock(
                    &racer_io::Clock::host(),
                ))),
            ));
            Session::new(
                ControlEndpoint {
                    url: "https://localhost".into(),
                    trust_bundle: d.0.join("trust"),
                    discovered: None,
                },
                Rc::new(Enrollment::new(
                    cluster.clone(),
                    d.0.join("token"),
                    d.0.join("identity"),
                    io.clone(),
                )),
                rails::RailJournal::new(Arc::new(Default::default()), d.0.join("identity"), io),
                NonZeroU32::new(4).unwrap(),
                keys.clone(),
                BundleInstaller::new(keys),
                Rc::new(PublicationTarget::new(
                    cluster,
                    Arc::new(PublicationCell::new(Snapshot::retention(2))),
                    racer_io::Clock::host(),
                )),
            )
        }
        #[test]
        fn lagging_replica_retries_preserve_state_without_enrollment() {
            let env = racer_io::Env::host();
            session::tests::lagging_replica(&env);
        }
        #[test]
        fn changed_binding_latches_restart_across_abandoned_or_failed_persistence() {
            let env = racer_io::Env::host();
            for abandon in [false, true] {
                let Some(r) = testing::reactor(&env) else {
                    return;
                };
                let d = testing::Directory::new();
                let client = client(&d, r.clone());
                let (ca, key) = testing::ca();
                client
                    .auth
                    .enrollment
                    .set_peer_trust_roots(vec![ca.der().to_vec()])
                    .unwrap();
                let scope = testing::scope();
                let request = testing::drive(
                    &r,
                    client
                        .auth
                        .enrollment
                        .prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
                )
                .unwrap();
                let old = testing::drive(
                    &r,
                    client.auth.enrollment.accept_response(
                        testing::issue(&request, &ca, &key, "22222222-2222-4222-8222-222222222222"),
                        &scope,
                    ),
                )
                .unwrap();
                *client.auth.identity.borrow_mut() = Some(old.clone());
                client.auth.started.set(true);
                let request = testing::drive(
                    &r,
                    client
                        .auth
                        .enrollment
                        .prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
                )
                .unwrap();
                let response =
                    testing::issue(&request, &ca, &key, "33333333-3333-4333-8333-333333333333");
                let committed = std::fs::read(d.0.join("identity/identity.json")).unwrap();
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
                    client.bind_keyring(client.auth.keys.borrow().clone()),
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
                    testing::drive(&r, client.keyring_progress(&scope)),
                    Err(Error::NodeIdentityChanged)
                ));
                testing::drive(&r, r.fence_matching(|_| true)).unwrap();
                assert_eq!(
                    std::fs::read(d.0.join("identity/identity.json")).unwrap(),
                    committed
                );
            }
        }
        #[test]
        fn status_policy_backoff_and_shutdown_are_explicit() {
            let env = racer_io::Env::host();
            session::tests::status_retry_shutdown(&env);
        }
        #[test]
        fn keyring_cursor_status_and_retry_are_independent() {
            let env = racer_io::Env::host();
            session::tests::independent_keyring(&env);
        }
    }

    pub(crate) mod wire_tests {
        //! Shared control wire codecs preserve application contract validation.

        use super::*;
        use racer_wire::control::decode_bundle;
        use racer_wire::control::decode_enrollment_response;
        use racer_wire::control::encode_bundle;
        use racer_wire::control::encode_enrollment_response;

        #[test]
        fn application_roundtrips_preserve_exact_wire_records() {
            const ROOT: &str = concat!(env!("CARGO_MANIFEST_DIR"), "/wire/testdata/");
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
                        encode_publication(&publication).unwrap()
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
        //! Cache definitions retain canonical names and socket ownership.

        use super::*;
        #[test]
        fn replacement_definitions_and_socket_paths_are_validated() {
            let mut defs = decode_publication(include_bytes!(concat!(
                env!("CARGO_MANIFEST_DIR"),
                "/wire/testdata/publication.json"
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
        //! Domain projection, retained membership, and atomic publication scenarios.

        use super::*;
        #[test]
        fn preparation_reuses_same_version_without_reading_member_weights() {
            let env = racer_io::Env::host();
            let store = store(&env, 2);
            let first = store.apply(publication(1)).unwrap();
            let before = crate::topology::scored_members();
            // Cache-only: pools, and so membership, are unchanged.
            let mut next = publication(2);
            next.caches[0].priority = 1;
            let prepared = store.project(next).unwrap();
            assert_eq!(
                crate::topology::scored_members(),
                before,
                "no topology constructor for cache-only publication"
            );
            assert!(Arc::ptr_eq(&first.membership, &prepared.membership));
            let mut conflict = publication(2);
            conflict.members[0].shares = std::num::NonZeroU32::new(99).unwrap();
            assert!(matches!(
                store.project(conflict),
                Err(Error::IncompatibleMembership)
            ));
            assert_eq!(crate::topology::scored_members(), before);
        }

        #[test]
        fn background_preparation_keeps_cpu_work_off_the_polling_thread() {
            let env = racer_io::Env::host();
            let store = store(&env, 2);
            let before = crate::topology::scored_members();
            let job = PublicationTarget::projection(&store, Arc::new(publication(1))).unwrap();
            let prepared = std::thread::spawn(job).join().unwrap().unwrap();
            assert_eq!(
                crate::topology::scored_members(),
                before,
                "member snapshots must be constructed off thread"
            );
            assert_eq!(store.cursor().unwrap(), None, "preparation cannot publish");
            store.accept(prepared, None).unwrap();
            assert_eq!(store.cursor().unwrap(), Some(PublicationSequence(1)));
        }

        #[test]
        fn background_preparation_admission_and_canceled_scope_are_bounded() {
            let env = racer_io::Env::host();
            preparation_fence(&env, false);
        }

        #[test]
        fn canceled_background_wait_keeps_job_admission_until_completion() {
            let env = racer_io::Env::host();
            preparation_fence(&env, true);
        }

        /// Domain projection gate proving that Sync, not the adapter, owns job admission.
        struct GatedTarget {
            target: PublicationTarget,

            wait: RefCell<Option<std::sync::mpsc::Receiver<()>>>,

            submissions: Rc<Cell<usize>>,
        }

        impl crate::control::poll::Install for GatedTarget {
            fn prepare(
                &self,
                document: crate::control::poll::Document,
            ) -> Result<crate::control::poll::Preparation> {
                self.submissions.set(self.submissions.get() + 1);
                let wait = self
                    .wait
                    .borrow_mut()
                    .take()
                    .expect("only one job admitted");
                let job = self.target.prepare(document)?;
                Ok(Box::new(move || {
                    wait.recv_timeout(Duration::from_secs(5))
                        .map_err(|_| Error::DeadlineExceeded)?;
                    job()
                }))
            }

            fn install(
                &self,
                document: &crate::control::poll::Document,
                prepared: &crate::control::poll::Prepared,
            ) -> Result<()> {
                self.target.install(document, prepared)
            }
        }

        /// One full publication followed by a held long poll.
        struct PublicationSource(Cell<bool>);

        impl crate::control::poll::Fetch for PublicationSource {
            fn get<'a>(
                &'a self,
                _: &'a str,
                _: Option<&'a str>,
                _: usize,
                scope: &'a RequestScope,
            ) -> racer_io::Operation<'a, Response, crate::control::poll::FetchError> {
                Box::pin(async move {
                    if self.0.replace(true) {
                        crate::drivers::poll_scoped(scope, |_| {
                            std::task::Poll::<Result<Response>>::Pending
                        })
                        .await
                        .map_err(Into::into)
                    } else {
                        Ok(Response {
                            status: 200,
                            body: wire::encode_publication(&publication(1)).unwrap(),
                            retry_after: None,
                        })
                    }
                })
            }

            fn close(&self) {}
        }

        /// Cancel before or during a turn and fence the retained finite CPU closure.
        fn preparation_fence(env: &racer_io::Env, cancel_wait: bool) {
            use std::future::Future;
            let clock = racer_io::SimClock::new(407);
            let sim_env = clock.ctx(0);
            let env = racer_io::Env::new(env.driver.clone(), sim_env.clone());
            let sim_ctx = sim_env.clone();
            let sim = racer_io::sim::SimOs::new();
            let env = racer_io::Env::sim(&sim, env.ctx.clone());
            let reactor = Rc::new(Reactor::new(
                Rc::new(flow_control::Quotas::new(
                    crate::admission::AdmissionPolicy::new(
                        crate::test_support::cluster::config(false).limits,
                    ),
                )),
                &env,
            ));
            let target = store(&env, 2);
            let published = target.published.clone();
            let submissions = Rc::new(Cell::new(0));
            let (release, wait) = std::sync::mpsc::channel();
            let mut sync = crate::control::poll::Poll::new(
                Rc::new(ReactorControlIo::new(reactor.clone())),
                crate::control::poll::Doc::Publication,
                Box::new(PublicationSource(Cell::new(false))),
                Box::new(GatedTarget {
                    target,
                    wait: RefCell::new(Some(wait)),
                    submissions: submissions.clone(),
                }),
                crate::control::poll::Schedule {
                    turn: Duration::from_secs(5),
                    tick: Duration::from_millis(1),
                    retry_min: Duration::from_millis(1),
                    retry_max: Duration::from_secs(1),
                },
            )
            .unwrap();
            let scope = RequestScope::on(
                &sim_ctx.clock,
                crate::model::RequestId([7; 16]),
                sim_ctx.clock.now() + Duration::from_secs(10),
            )
            .unwrap();
            testing::drive(&reactor, Box::pin(sync.turn(&scope))).unwrap();
            assert_eq!(submissions.get(), 1);
            assert_eq!(sync.status().pending, Some(1));
            assert_eq!(sync.status().accepted, None);
            if cancel_wait {
                let mut turn = Box::pin(sync.turn(&scope));
                let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
                assert!(turn.as_mut().poll(&mut cx).is_pending());
                scope.cancel().unwrap();
                assert!(matches!(
                    turn.as_mut().poll(&mut cx),
                    std::task::Poll::Ready(Err(Error::Canceled))
                ));
            } else {
                scope.cancel().unwrap();
                assert_eq!(
                    testing::drive(&reactor, Box::pin(sync.turn(&scope))),
                    Err(Error::Canceled)
                );
            }
            assert_eq!(submissions.get(), 1);
            assert_eq!(
                testing::drive(&reactor, Box::pin(sync.shutdown(&scope))),
                Err(Error::Canceled)
            );
            release.send(()).unwrap();
            let shutdown_scope = RequestScope::on(
                &sim_ctx.clock,
                crate::model::RequestId([8; 16]),
                sim_ctx.clock.now() + Duration::from_secs(10),
            )
            .unwrap();
            let mut shutdown = Box::pin(sync.shutdown(&shutdown_scope));
            let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
            let until = Instant::now() + Duration::from_secs(5);
            loop {
                if let std::task::Poll::Ready(result) = shutdown.as_mut().poll(&mut cx) {
                    result.unwrap();
                    break;
                }
                assert!(Instant::now() < until, "preparation failed to terminate");
                clock.advance(Duration::from_millis(1));
                std::thread::sleep(Duration::from_millis(1));
            }
            drop(shutdown);
            assert!(sync.status().stopped);
            assert!(published.current().unwrap().is_none());
        }

        fn publication(sequence: u64) -> Publication {
            let mut p = decode_publication(include_bytes!(concat!(
                env!("CARGO_MANIFEST_DIR"),
                "/wire/testdata/publication.json"
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
        fn store(env: &racer_io::Env, retained: usize) -> PublicationTarget {
            PublicationTarget::new(
                publication(1).cluster,
                Arc::new(PublicationCell::new(Snapshot::retention(retained))),
                env.ctx.clock.clone(),
            )
        }
        fn membership(sequence: u64, version: u64) -> Publication {
            let mut next = publication(sequence);
            next.membership_version.0 = version;
            next
        }
        #[test]
        fn default_two_old_generations_resolve_without_local_request_pins() {
            let env = racer_io::Env::host();
            let store = store(&env, 2);
            for version in 1..20 {
                store.apply(membership(version, version)).unwrap();
                for old in version.saturating_sub(2).max(1)..=version {
                    assert_eq!(
                        store
                            .published
                            .resolve(old, racer_io::Clock::host().now())
                            .unwrap()
                            .unwrap()
                            .version
                            .0,
                        old
                    );
                }
                if version > 3 {
                    assert!(
                        store
                            .published
                            .resolve(version - 3, racer_io::Clock::host().now())
                            .unwrap()
                            .is_none()
                    );
                }
            }
        }
        #[test]
        fn page_size_is_adopted_once_and_never_changed() {
            let env = racer_io::Env::host();
            let store = store(&env, 2);
            let mut first = publication(1);
            first.page_size = racer_wire::object::PageSize::new(64 << 10).unwrap();
            assert_eq!(store.apply(first).unwrap().page_size.get(), 64 << 10);
            assert!(matches!(
                store.apply(publication(2)),
                Err(Error::InvalidConfiguration)
            ));
            let mut same = publication(3);
            same.page_size = racer_wire::object::PageSize::new(64 << 10).unwrap();
            assert_eq!(store.apply(same).unwrap().page_size.get(), 64 << 10);
        }
        /// A projection prepared before the first acceptance cannot commit a
        /// page size that differs from the one accepted meanwhile.
        #[test]
        fn page_size_is_rechecked_at_commit() {
            let env = racer_io::Env::host();
            let store = store(&env, 2);
            let mut small = publication(1);
            small.page_size = racer_wire::object::PageSize::new(64 << 10).unwrap();
            let small = store.project(small).unwrap();
            let large = store.project(membership(2, 2)).unwrap();
            store.accept(small, None).unwrap();
            let r = store.accept(large, None);
            assert_eq!(r.err(), Some(Error::InvalidConfiguration));
        }
        #[test]
        fn cache_only_history_uses_one_slot_and_weak_registry_stays_bounded() {
            let env = racer_io::Env::host();
            let store = store(&env, 0);
            let first = store.apply(publication(1)).unwrap();
            let mut history = vec![first.clone()];
            for sequence in 2..40 {
                let mut next = publication(sequence);
                next.caches[0].priority = sequence as u16;
                let snapshot = store.apply(next).unwrap();
                assert!(Arc::ptr_eq(&first.membership, &snapshot.membership));
                history.push(snapshot);
                assert!(
                    store
                        .published
                        .resolve(2, racer_io::Clock::host().now())
                        .unwrap()
                        .is_none()
                );
            }
            let next = membership(40, 2);
            assert!(matches!(store.apply(next.clone()), Err(Error::Overloaded)));
            let weak = Arc::downgrade(&first.membership);
            drop(first);
            drop(history);
            store.apply(next).unwrap();
            assert!(weak.upgrade().is_none());
            for version in 3..100 {
                store.apply(membership(version + 40, version)).unwrap();
                assert!(
                    store
                        .published
                        .resolve(version, racer_io::Clock::host().now())
                        .unwrap()
                        .is_some()
                );
                assert!(
                    store
                        .published
                        .resolve(version - 1, racer_io::Clock::host().now())
                        .unwrap()
                        .is_none()
                );
            }
        }
        #[test]
        fn delayed_thread_lease_blocks_admission_until_release() {
            let env = racer_io::Env::host();
            let store = store(&env, 1);
            let published = store.published.clone();
            store.apply(publication(1)).unwrap();
            let (ready_tx, ready_rx) = std::sync::mpsc::channel();
            let (release_tx, release_rx) = std::sync::mpsc::channel();
            let worker = std::thread::spawn(move || {
                let snapshot = published.current().unwrap().unwrap();
                ready_tx.send(()).unwrap();
                release_rx.recv().unwrap();
                let incoming = published
                    .resolve(1, racer_io::Clock::host().now())
                    .unwrap()
                    .unwrap();
                assert!(Arc::ptr_eq(&snapshot.membership, &incoming));
            });
            ready_rx.recv().unwrap();
            let current = store.apply(membership(2, 2)).unwrap();
            assert!(matches!(
                store.apply(membership(3, 3)),
                Err(Error::Overloaded)
            ));
            release_tx.send(()).unwrap();
            worker.join().unwrap();
            store.apply(membership(3, 3)).unwrap();
            assert_eq!(current.membership.version, MembershipVersion(2));
        }
        #[test]
        fn atomic_replay_rollback_and_leased_history() {
            let env = racer_io::Env::host();
            let store = store(&env, 1);
            let first = store.apply(publication(1)).unwrap();
            assert!(Arc::ptr_eq(&first, &store.apply(publication(1)).unwrap()));
            let mut changed = publication(1);
            changed.caches[0].priority = 1;
            assert!(matches!(store.apply(changed), Err(Error::Replay)));
            let second = store.apply(membership(2, 2)).unwrap();
            assert!(matches!(store.apply(publication(1)), Err(Error::Replay)));
            assert!(matches!(
                store.apply(membership(3, 3)),
                Err(Error::Overloaded)
            ));
            assert_eq!(store.cursor().unwrap(), Some(PublicationSequence(2)));
            drop(first);
            assert!(store.apply(membership(3, 3)).is_ok());
            drop(second);
            let mut changed = membership(4, 3);
            changed.members[0].peer_endpoint = "192.0.2.7:7443".into();
            assert!(matches!(
                store.apply(changed.clone()),
                Err(Error::IncompatibleMembership)
            ));
            changed.membership_version.0 += 1;
            assert!(store.apply(changed).is_ok());
        }
        #[test]
        fn site_changes_require_new_membership_and_preserve_leased_history() {
            let env = racer_io::Env::host();
            let store = store(&env, 3);
            let old = store.apply(publication(1)).unwrap();
            let mut next = publication(2);
            next.members[0].site = "site1".into();
            assert!(matches!(
                store.apply(next.clone()),
                Err(Error::IncompatibleMembership)
            ));
            next.membership_version.0 = 2;
            let current = store.apply(next).unwrap();
            assert!(old.membership.members()[0].site.is_empty());
            assert_eq!(current.membership.members()[0].site, "site1");
            assert_eq!(
                old.membership.placement_identity(),
                current.membership.placement_identity()
            );
            assert!(
                store.apply(membership(3, 3)).unwrap().membership.members()[0]
                    .site
                    .is_empty()
            );
        }
        #[test]
        fn staged_resources_commit_only_on_accepted_replacement() {
            let env = racer_io::Env::host();
            struct Transition(Rc<std::cell::Cell<usize>>);
            impl CacheTransition for Transition {
                fn commit(self: Box<Self>) {
                    self.0.set(self.0.get() + 1);
                }
            }
            let committed = Rc::new(std::cell::Cell::new(0));
            let store = store(&env, 0);
            let publish = |p| store.apply_staged(p, Some(Box::new(Transition(committed.clone()))));
            let first = publish(publication(1)).unwrap();
            assert_eq!(committed.get(), 1);
            assert!(matches!(publish(membership(2, 2)), Err(Error::Overloaded)));
            assert_eq!(committed.get(), 1);
            drop(first);
            publish(membership(2, 2)).unwrap();
            assert_eq!(committed.get(), 2);
        }

        /// Same-version races cannot swap topology allocations or commit resources.
        #[test]
        fn stale_projection_and_foreign_cluster_leave_publication_unchanged() {
            let env = racer_io::Env::host();
            let target = store(&env, 2);
            target.apply(publication(1)).unwrap();
            let old_job = target.projection(Arc::new(membership(3, 2))).unwrap();
            let current = target.apply(membership(2, 2)).unwrap();
            let stale = old_job().unwrap();
            assert!(!Arc::ptr_eq(&current.membership, &stale.membership));
            assert!(matches!(
                target.accept(stale, None),
                Err(Error::IncompatibleMembership)
            ));
            assert!(Arc::ptr_eq(&target.current().unwrap(), &current));
            let mut foreign = membership(3, 3);
            foreign.cluster = ClusterId("00000000-0000-4000-8000-000000000001".into());
            assert!(matches!(target.project(foreign), Err(Error::Unauthorized)));
            let other = PublicationTarget::new(
                ClusterId("foreign".into()),
                target.published.clone(),
                racer_io::Clock::host(),
            );
            assert!(matches!(
                other.accept(current.clone(), None),
                Err(Error::Unauthorized)
            ));
            assert_eq!(target.cursor().unwrap(), Some(PublicationSequence(2)));
        }

        /// Domain topology byte estimates and configured grace deadlines reach Published.
        #[test]
        fn domain_grace_retention_honors_bytes_time_and_external_pins() {
            let env = racer_io::Env::host();
            let probe = store(&env, 2).project(publication(1)).unwrap();
            let bytes = probe.membership.retained_bytes();
            assert!(bytes > 0);
            for budget in [0, bytes] {
                let mut policy = Snapshot::retention(2);
                policy.grace_bytes = budget;
                let published = Arc::new(PublicationCell::new(policy));
                let target = PublicationTarget::new(
                    publication(1).cluster,
                    published.clone(),
                    racer_io::Clock::host(),
                );
                target.apply(publication(1)).unwrap();
                target.apply(membership(2, 2)).unwrap();
                let now = racer_io::Clock::host().now();
                let held = published.resolve(1, now).unwrap();
                assert_eq!(held.is_some(), budget >= bytes);
                let expired = now + Duration::from_secs(31);
                assert_eq!(
                    published.resolve(1, expired).unwrap().is_some(),
                    held.is_some()
                );
                drop(held);
                assert!(published.resolve(1, expired).unwrap().is_none());
            }
        }
    }

    pub(crate) mod scenarios {
        //! Real TLS and reactor scenarios for bounded control transport ownership.

        use super::*;
        use crate::test_support::enrollment as testing;
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
                    ) => {}
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
                discovered: None,
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

        /// Count connection attempts through the real adapter's admission hook.
        pub(crate) struct ConnectProbe {
            leases: RefCell<Vec<std::rc::Weak<crate::admission::ConnectionReservation>>>,

            pub(crate) addresses: [SocketAddr; 2],
        }

        impl ConnectProbe {
            /// Record one connection attempt's admission slot.
            pub(crate) fn observe(&self, lease: &Rc<crate::admission::ConnectionReservation>) {
                self.leases.borrow_mut().push(Rc::downgrade(lease));
            }

            /// Connection attempts admitted so far.
            fn calls(&self) -> usize {
                self.leases.borrow().len()
            }
        }

        /// A first address that accepts TCP but never answers TLS gets only its
        /// share of the deadline; the next address then connects, and
        /// cancellation or overall expiry ends the attempt with every socket closed.
        #[test]
        fn real_connect_blackhole_yields_to_next_address_and_cleans_fds() {
            use std::task::Context;
            use std::task::Poll;
            let env = racer_io::Env::host();
            for mode in ["local", "cancel", "parent"] {
                let Some(reactor) = testing::reactor(&env) else {
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
                // Outside "local" the second listener stays open but silent,
                // so both addresses hang until the caller's deadline.
                let silent = healthy.try_clone().unwrap();
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
                let started = Instant::now();
                let scope = RequestScope::host(
                    crate::model::RequestId([7; 16]),
                    started + Duration::from_millis(400),
                )
                .unwrap();
                let io = Rc::new(ConnectProbe {
                    leases: RefCell::new(Vec::new()),
                    addresses,
                });
                let transport = session::transport(&ControlEndpoint {
                    url: "https://localhost".into(),
                    trust_bundle: trust,
                    discovered: None,
                });
                let mut driver = ReactorControlIo::new(reactor.clone());
                driver.connect_probe = Some(io.clone());
                transport.attach_io(Rc::new(driver));
                let mut connect = transport.connect(None, &scope);
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                let watchdog = Instant::now() + Duration::from_secs(2);
                let result = loop {
                    if let Poll::Ready(result) = connect.as_mut().poll(&mut cx) {
                        break result;
                    }
                    assert!(Instant::now() < watchdog);
                    if mode == "cancel" && Instant::now() >= started + Duration::from_millis(50) {
                        let _ = scope.cancel();
                    }
                    reactor.poll_budgeted(64).unwrap();
                    reactor.wait(Duration::from_millis(1)).unwrap();
                };
                drop(connect);
                match mode {
                    "local" => {
                        let connection = result.unwrap();
                        assert_eq!(io.calls(), 2);
                        assert!(
                            Instant::now() < scope.deadline.0,
                            "TLS retains overall budget"
                        );
                        drop(connection);
                    }
                    "cancel" => {
                        assert!(matches!(result, Err(Error::Canceled)));
                        assert_eq!(io.calls(), 1);
                    }
                    _ => {
                        assert!(
                            matches!(result, Err(Error::DeadlineExceeded)),
                            "{:?}",
                            result.as_ref().err()
                        );
                        assert_eq!(io.calls(), 2);
                    }
                }
                transport.close_idle();
                let mut closed = accept(&first);
                closed
                    .set_read_timeout(Some(Duration::from_secs(1)))
                    .unwrap();
                let mut byte = [0; 1];
                // The client may have sent its TLS hello before closing.
                while closed.read(&mut byte).unwrap() != 0 {}
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
                drop(silent);
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
                racer_wire::control::ClusterId("11111111-1111-4111-8111-111111111111".into()),
                d.0.join("token"),
                d.0.join("identity"),
                Rc::new(ReactorControlIo::new(r.clone())),
            );
            enrollment
                .set_peer_trust_roots(vec![ca.der().to_vec()])
                .unwrap();
            let request = testing::drive(
                r,
                enrollment.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
            )
            .unwrap();
            testing::drive(
                r,
                enrollment.accept_response(
                    testing::issue(&request, ca, key, "22222222-2222-4222-8222-222222222222"),
                    &scope,
                ),
            )
            .unwrap()
        }
        #[test]
        fn real_ring_server_auth_and_mutual_tls() {
            let env = racer_io::Env::host();
            let Some(r) = testing::reactor(&env) else {
                return;
            };
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
            let transport = session::transport(&ControlEndpoint {
                url: format!("https://127.0.0.1:{port}"),
                trust_bundle: trust,
                discovered: None,
            });
            transport.attach_io(Rc::new(ReactorControlIo::new(r.clone())));
            let scope = testing::scope();
            let first = testing::drive(
                &r,
                Box::pin(async {
                    transport
                        .connect(None, &scope)
                        .await?
                        .request(
                            rest::Request {
                                method: rest::Method::Post,
                                path: wire::BOOTSTRAP_PATH,
                                bearer: Some("fixture.token"),
                                header: None,
                                body: &[],
                                limit: 65536,
                            },
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
                        .connect(
                            Some(rest::Identity {
                                certificate_chain: identity.certificate_chain(),
                                private_key: identity.private_key_der(),
                                expires: identity.expires_at(),
                            }),
                            &scope,
                        )
                        .await?
                        .request(
                            rest::Request {
                                method: rest::Method::Get,
                                path: wire::SNAPSHOT_PATH,
                                bearer: None,
                                header: None,
                                body: &[],
                                limit: 65536,
                            },
                            &scope,
                        )
                        .await
                }),
            )
            .unwrap();
            assert_eq!(second.status, 200);
            assert_eq!(second.body, b"{}");
            server.join().unwrap();
        }
        #[test]
        fn backend_disconnect_recovers_on_fresh_connection_without_circuit() {
            let env = racer_io::Env::host();
            let Some(r) = testing::reactor(&env) else {
                return;
            };
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
            let transport = session::transport(&endpoint);
            transport.attach_io(Rc::new(ReactorControlIo::new(r.clone())));
            let scope = testing::scope();
            let get = || {
                testing::drive(
                    &r,
                    Box::pin(async {
                        transport
                            .connect(
                                Some(rest::Identity {
                                    certificate_chain: identity.certificate_chain(),
                                    private_key: identity.private_key_der(),
                                    expires: identity.expires_at(),
                                }),
                                &scope,
                            )
                            .await?
                            .request(
                                rest::Request {
                                    method: rest::Method::Get,
                                    path: wire::SNAPSHOT_PATH,
                                    bearer: None,
                                    header: None,
                                    body: &[],
                                    limit: 65536,
                                },
                                &scope,
                            )
                            .await
                    }),
                )
            };
            assert_eq!(get().unwrap().status, 200);
            assert!(matches!(get(), Err(Error::Io)));
            // Retry scheduling belongs to the feed, not a capacity-one link circuit.
            assert_eq!(get().unwrap().status, 204);
            transport.close_idle();
            server.join().unwrap();
        }
        #[test]
        fn malformed_delta_base_and_method_are_rejected_before_writing() {
            let env = racer_io::Env::host();
            session::tests::canonical_delta_and_get_only(&env);
            let Some(r) = testing::reactor(&env) else {
                return;
            };
            let d = testing::Directory::new();
            let (ca, key) = testing::ca();
            let tls = server_config(&ca, &key, true);
            let trust = d.0.join("trust");
            std::fs::write(&trust, ca.pem()).unwrap();
            let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
            let endpoint = ControlEndpoint {
                url: format!("https://{}", listener.local_addr().unwrap()),
                trust_bundle: trust,
                discovered: None,
            };
            // Digest syntax is generated by the typed codec now, but this public
            // REST boundary must still reject injected headers before HTTP writes.
            let cases = [
                ("X-Racer-Delta-Base", "abc\r\nInjected: yes"),
                ("X-Racer-Delta-Base", "abc\nInjected: yes"),
                ("X-Racer-Delta-Base", "abc\0def"),
                ("X-Racer-Delta-Base\r\nInjected", "abc"),
                ("Content-Length", "123"),
            ];
            let count = cases.len();
            let server = std::thread::spawn(move || {
                for _ in 0..count {
                    let mut stream = rustls::StreamOwned::new(
                        rustls::ServerConnection::new(tls.clone()).unwrap(),
                        accept(&listener),
                    );
                    assert_disconnected(&mut stream);
                }
                let mut stream = rustls::StreamOwned::new(
                    rustls::ServerConnection::new(tls).unwrap(),
                    accept(&listener),
                );
                let request = read_head(&mut stream);
                assert!(request.starts_with("GET /v1/snapshot HTTP/1.1\r\n"));
                assert!(request.contains(&format!("X-Racer-Delta-Base: {}\r\n", "a".repeat(64))));
                stream.write_all(b"HTTP/1.1 204 No Content\r\nContent-Length: 0\r\nConnection: close\r\n\r\n").unwrap();
                stream.flush().unwrap();
            });
            let transport = session::transport(&endpoint);
            transport.attach_io(Rc::new(ReactorControlIo::new(r.clone())));
            let scope = testing::scope();
            for header in cases {
                let connection = testing::drive(&r, transport.connect(None, &scope)).unwrap();
                assert!(matches!(
                    testing::drive(
                        &r,
                        connection.request(
                            rest::Request {
                                method: rest::Method::Get,
                                path: wire::SNAPSHOT_PATH,
                                bearer: None,
                                header: Some(header),
                                body: &[],
                                limit: 1024
                            },
                            &scope
                        )
                    ),
                    Err(Error::InvalidRequest)
                ));
            }
            let connection = testing::drive(&r, transport.connect(None, &scope)).unwrap();
            assert_eq!(
                testing::drive(
                    &r,
                    connection.request(
                        rest::Request {
                            method: rest::Method::Get,
                            path: wire::SNAPSHOT_PATH,
                            bearer: None,
                            header: Some(("X-Racer-Delta-Base", &"a".repeat(64))),
                            body: &[],
                            limit: 1024
                        },
                        &scope
                    )
                )
                .unwrap()
                .status,
                204
            );
            server.join().unwrap();
        }
        #[test]
        fn not_yet_valid_identity_is_rejected_before_transport_io() {
            let env = racer_io::Env::host();
            let Some(r) = testing::reactor(&env) else {
                return;
            };
            let d = testing::Directory::new();
            let (ca, key) = testing::ca();
            let identity = enrolled(&r, &d, &ca, &key);
            assert!(identity.valid_at(racer_io::Clock::host().wall_now()));
            let clock = racer_io::SimClock::new(17);
            let environment = clock.ctx(1);
            // Roll only the scoped wall clock back after real enrollment. The generic
            // Identity has expiry only; Racer must enforce its not-before policy itself.
            clock.set_wall_time(SystemTime::now() - Duration::from_secs(3600));
            let env = racer_io::Env::new(env.driver.clone(), environment.clone());
            assert!(!identity.valid_at(env.ctx.clock.clone().wall_now()));
            session::tests::reject_future_identity(&env, identity);
        }
    }

    pub(crate) mod enrollment_tests {
        //! Enrollment correlation, certificate policy, and durable identity recovery.

        #[test]
        fn enrollment_requires_exactly_one_identity_san() {
            let env = racer_io::Env::host();
            let Some(r) = testing::reactor(&env) else {
                return;
            };
            let d = testing::Directory::new();
            let scope = testing::scope();
            let enrollment = Enrollment::new(
                racer_wire::control::ClusterId("11111111-1111-4111-8111-111111111111".into()),
                d.0.join("token"),
                d.0.join("identity"),
                Rc::new(ReactorControlIo::new(r.clone())),
            );
            let request = testing::drive(
                &r,
                enrollment.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
            )
            .unwrap();
            let (ca, key) = testing::ca();
            enrollment
                .set_peer_trust_roots(vec![ca.der().to_vec()])
                .unwrap();
            let response = testing::issue(&request, &ca, &key, OLD_NODE);
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
                let now = racer_io::Clock::host().wall_now();
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
                    testing::drive(&r, enrollment.accept_response(multiple, &scope)),
                    Err(Error::Unauthorized)
                ));
            }
            assert!(testing::drive(&r, enrollment.accept_response(response, &scope)).is_ok());
        }

        /// A chain from an issuer root missing from the current trust roots is
        /// rejected and flagged as untrusted, the pending request survives, and
        /// the same response verifies once refreshed roots include that issuer.
        /// Other certificate policy failures are not flagged.
        #[test]
        fn enrollment_from_unknown_issuer_is_flagged_and_retryable_with_new_roots() {
            let env = racer_io::Env::host();
            let Some(r) = testing::reactor(&env) else {
                return;
            };
            let d = testing::Directory::new();
            let scope = testing::scope();
            let enrollment = Enrollment::new(
                racer_wire::control::ClusterId("11111111-1111-4111-8111-111111111111".into()),
                d.0.join("token"),
                d.0.join("identity"),
                Rc::new(ReactorControlIo::new(r.clone())),
            );
            let request = testing::drive(
                &r,
                enrollment.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
            )
            .unwrap();
            let (old, _) = testing::ca();
            let (new, new_key) = testing::ca();
            enrollment
                .set_peer_trust_roots(vec![old.der().to_vec()])
                .unwrap();
            let response = testing::issue(&request, &new, &new_key, OLD_NODE);
            assert!(matches!(
                testing::drive(&r, enrollment.accept_response(response.clone(), &scope)),
                Err(Error::Unauthorized)
            ));
            assert!(enrollment.take_untrusted_chain());
            assert!(!enrollment.take_untrusted_chain());
            // A trusted issuer with a mismatched cluster is not a trust lag.
            let mut foreign = response.clone();
            foreign.cluster =
                racer_wire::control::ClusterId("44444444-4444-4444-8444-444444444444".into());
            enrollment
                .set_peer_trust_roots(vec![old.der().to_vec(), new.der().to_vec()])
                .unwrap();
            assert!(matches!(
                testing::drive(&r, enrollment.accept_response(foreign, &scope)),
                Err(Error::Unauthorized)
            ));
            assert!(!enrollment.take_untrusted_chain());
            assert!(testing::drive(&r, enrollment.accept_response(response, &scope)).is_ok());
            assert!(!enrollment.take_untrusted_chain());
        }

        #[test]
        fn enrollment_and_renewal_refresh_authenticated_physical_inventory() {
            use rdma_verbs::simulation::Device;
            use rdma_verbs::simulation::Simulation;
            let env = racer_io::Env::host();
            let Some(r) = testing::reactor(&env) else {
                return;
            };
            let d = testing::Directory::new();
            let scope = testing::scope();
            let io = Rc::new(ReactorControlIo::new(r.clone()));
            let journal = rails::RailJournal::new(
                Arc::new(Default::default()),
                d.0.join("identity"),
                io.clone(),
            );
            let enrollment = Enrollment::new(
                racer_wire::control::ClusterId("11111111-1111-4111-8111-111111111111".into()),
                d.0.join("token"),
                d.0.join("identity"),
                io,
            );
            let first = Simulation::new()
                .with_devices(vec![Device::new("nic-a", [1; 16])])
                .unwrap();
            let request = {
                let _environment = first.enter();
                let nics = testing::drive(&r, journal.persist(&scope)).unwrap();
                testing::drive(
                    &r,
                    enrollment.prepare(nics, NonZeroU32::new(4).unwrap(), &scope),
                )
                .unwrap()
            };
            assert_eq!(request.rdma_nics.len(), 1);
            assert_eq!(request.rdma_nics[0].device, "nic-a");
            assert_eq!(request.rdma_nics[0].gid, Some([1; 16]));
            let second = Simulation::new()
                .with_devices(vec![Device::new("nic-b", [2; 16])])
                .unwrap();
            let _environment = second.enter();
            let nics = testing::drive(&r, journal.persist(&scope)).unwrap();
            let renewed = testing::drive(
                &r,
                enrollment.prepare(nics, NonZeroU32::new(4).unwrap(), &scope),
            )
            .unwrap();
            assert_eq!(renewed.enrollment, request.enrollment);
            assert_eq!(renewed.csr_der, request.csr_der);
            assert_eq!(renewed.rdma_nics[0].device, "nic-b");
            let absent = Simulation::new().with_devices(vec![]).unwrap();
            let _environment = absent.enter();
            let nics = testing::drive(&r, journal.persist(&scope)).unwrap();
            assert!(
                testing::drive(
                    &r,
                    enrollment.prepare(nics, NonZeroU32::new(4).unwrap(), &scope)
                )
                .unwrap()
                .rdma_nics
                .is_empty()
            );
        }
        use super::*;
        use crate::test_support::enrollment as testing;
        const OLD_NODE: &str = "22222222-2222-4222-8222-222222222222";
        const NEW_NODE: &str = "33333333-3333-4333-8333-333333333333";

        #[test]
        fn renewal_tracks_short_issued_lifetime_and_preserves_default() {
            let env = racer_io::Env::host();
            let Some(r) = testing::reactor(&env) else {
                return;
            };
            for (lifetime, due) in [(120, 80), (86400, 57600), (86700, 57600)] {
                let d = testing::Directory::new();
                let e = Enrollment::new(
                    ClusterId("11111111-1111-4111-8111-111111111111".into()),
                    d.0.join("token"),
                    d.0.join("identity"),
                    Rc::new(ReactorControlIo::new(r.clone())),
                );
                let scope = testing::scope();
                let request =
                    testing::drive(&r, e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope))
                        .unwrap();
                let (ca, key) = testing::ca();
                e.set_peer_trust_roots(vec![ca.der().to_vec()]).unwrap();
                let start = SystemTime::now()
                    .duration_since(std::time::UNIX_EPOCH)
                    .unwrap()
                    .as_secs();
                let wall = std::time::UNIX_EPOCH + Duration::from_secs(start);
                let mut response = testing::issue_at(&request, &ca, &key, OLD_NODE, wall);
                let mut csr = rcgen::CertificateSigningRequestParams::from_der(
                    &rustls::pki_types::CertificateSigningRequestDer::from(request.csr_der.clone()),
                )
                .unwrap();
                csr.params.not_before = wall.into();
                csr.params.not_after = (wall + Duration::from_secs(lifetime)).into();
                csr.params.subject_alt_names = vec![rcgen::SanType::URI(
                    format!("spiffe://{}/node/{OLD_NODE}", request.cluster.0)
                        .try_into()
                        .unwrap(),
                )];
                csr.params.key_usages = vec![rcgen::KeyUsagePurpose::DigitalSignature];
                csr.params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ClientAuth];
                response.certificate_chain = vec![csr.signed_by(&ca, &key).unwrap().der().to_vec()];
                let identity = testing::drive(&r, e.accept_response(response, &scope)).unwrap();
                let clock = racer_io::SimClock::new_at(
                    51,
                    std::time::Instant::now(),
                    wall + Duration::from_secs(due - 1),
                );
                let sim_env = clock.ctx(1);
                let sim_ctx = sim_env.clone();
                assert!(!identity.renewal_due_at(sim_ctx.clock.clone().wall_now()));
                clock.advance(Duration::from_secs(1));
                assert!(identity.renewal_due_at(sim_ctx.clock.clone().wall_now()));
                assert!(identity.valid_at(sim_ctx.clock.clone().wall_now()));
                clock.advance(Duration::from_secs(lifetime - due));
                assert!(!identity.valid_at(sim_ctx.clock.clone().wall_now()));
                assert!(identity.renewal_due_at(sim_ctx.clock.clone().wall_now()));
            }
        }

        #[test]
        fn authenticated_replacement_preserves_cluster_key_and_correlation_checks() {
            let env = racer_io::Env::host();
            for expired in [false, true] {
                let Some(r) = testing::reactor(&env) else {
                    return;
                };
                let d = testing::Directory::new();
                let e = Enrollment::new(
                    ClusterId("11111111-1111-4111-8111-111111111111".into()),
                    d.0.join("token"),
                    d.0.join("identity"),
                    Rc::new(ReactorControlIo::new(r.clone())),
                );
                let (ca, key) = testing::ca();
                e.set_peer_trust_roots(vec![ca.der().to_vec()]).unwrap();
                let scope = testing::scope();
                let request =
                    testing::drive(&r, e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope))
                        .unwrap();
                testing::drive(
                    &r,
                    e.accept_response(testing::issue(&request, &ca, &key, OLD_NODE), &scope),
                )
                .unwrap();
                let clock = racer_io::SimClock::new_at(
                    31,
                    std::time::Instant::now(),
                    SystemTime::now() + Duration::from_secs(172800),
                );
                // The expired case continues on a reactor built on a clock two
                // days ahead, over the same enrollment files.
                let env = if expired {
                    racer_io::Env::new(env.driver.clone(), clock.ctx(1))
                } else {
                    env.clone()
                };
                let (r, e) = if expired {
                    let Some(r) = testing::reactor(&env) else {
                        return;
                    };
                    let e = Enrollment::new(
                        e.cluster().clone(),
                        d.0.join("token"),
                        d.0.join("identity"),
                        Rc::new(ReactorControlIo::new(r.clone())),
                    );
                    e.set_peer_trust_roots(vec![ca.der().to_vec()]).unwrap();
                    (r, e)
                } else {
                    (r, e)
                };
                if expired {
                    assert!(
                        testing::drive(&r, e.load_identity(&scope))
                            .unwrap()
                            .is_none()
                    );
                }
                // Certificates must be valid at the worker clock that verifies them, which
                // runs two days ahead of host time in the expired case.
                let issued_at = r.ctx().clock.wall_now() - Duration::from_secs(1);
                let issue = |request: &_, ca: &_, key: &_, node: &str| {
                    testing::issue_at(request, ca, key, node, issued_at)
                };
                let old = std::fs::read(d.0.join("identity/identity.json")).unwrap();
                let request =
                    testing::drive(&r, e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope))
                        .unwrap();
                let response = issue(&request, &ca, &key, NEW_NODE);
                let mut wrong_san = response.clone();
                wrong_san.node = NodeId(OLD_NODE.into());
                let mut wrong_id = response.clone();
                wrong_id.enrollment = EnrollmentId(OLD_NODE.into());
                let mut foreign_request = request.clone();
                foreign_request.cluster = ClusterId("44444444-4444-4444-8444-444444444444".into());
                let foreign = issue(&foreign_request, &ca, &key, NEW_NODE);
                let (rogue_ca, rogue_key) = testing::ca();
                let rogue = issue(&request, &rogue_ca, &rogue_key, NEW_NODE);
                let other = Enrollment::new(
                    e.cluster().clone(),
                    d.0.join("token"),
                    d.0.join("other"),
                    Rc::new(ReactorControlIo::new(r.clone())),
                );
                let mut other_request = testing::drive(
                    &r,
                    other.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
                )
                .unwrap();
                other_request.enrollment = request.enrollment.clone();
                let wrong_key = issue(&other_request, &ca, &key, NEW_NODE);
                for bad in [wrong_san, wrong_id, foreign, rogue, wrong_key] {
                    assert!(matches!(
                        testing::drive(&r, e.accept_response(bad, &scope)),
                        Err(Error::Unauthorized)
                    ));
                    assert_eq!(
                        std::fs::read(d.0.join("identity/identity.json")).unwrap(),
                        old
                    );
                    assert!(d.0.join("identity/pending.json").exists());
                }
                let identity = testing::drive(&r, e.accept_response(response, &scope)).unwrap();
                assert_eq!(identity.node().0, NEW_NODE);
                assert_eq!(
                    testing::drive(&r, e.load_identity(&scope))
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
                    Rc::new(ReactorControlIo::new(r.clone())),
                );
                foreign
                    .set_peer_trust_roots(vec![ca.der().to_vec()])
                    .unwrap();
                let request = testing::drive(
                    &r,
                    foreign.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
                )
                .unwrap();
                assert!(matches!(
                    testing::drive(
                        &r,
                        foreign
                            .accept_response(testing::issue(&request, &ca, &key, NEW_NODE), &scope)
                    ),
                    Err(Error::Unauthorized)
                ));
            }
        }

        #[test]
        fn replacement_crash_at_each_completion_recovers_and_reauthenticates() {
            let env = racer_io::Env::host();
            let Some(r) = testing::reactor(&env) else {
                return;
            };
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
                        Rc::new(ReactorControlIo::new(r.clone())),
                    );
                    e.set_peer_trust_roots(vec![ca.der().to_vec()]).unwrap();
                    e
                };
                let e = enrollment();
                let scope = testing::scope();
                let request =
                    testing::drive(&r, e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope))
                        .unwrap();
                testing::drive(
                    &r,
                    e.accept_response(testing::issue(&request, &ca, &key, OLD_NODE), &scope),
                )
                .unwrap();
                let request =
                    testing::drive(&r, e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope))
                        .unwrap();
                let mut accept =
                    e.accept_response(testing::issue(&request, &ca, &key, NEW_NODE), &scope);
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
                // This fixture exclusively owns the reactor. Fence the abandoned
                // namespace writes without running recovery's pending cleanup yet.
                testing::drive(&r, r.fence_matching(|_| true)).unwrap();
                let pending_exists = d.0.join("identity/pending.json").exists();
                let restarted = enrollment();
                let recovered = testing::drive(&r, restarted.load_identity(&scope))
                    .unwrap()
                    .unwrap();
                saw_old |= recovered.node().0 == OLD_NODE;
                saw_new |= recovered.node().0 == NEW_NODE;
                saw_committed_pending |= recovered.node().0 == NEW_NODE && pending_exists;
                assert!([OLD_NODE, NEW_NODE].contains(&recovered.node().0.as_str()));
                // Startup must submit a request again, regardless of what survived.
                let request = testing::drive(
                    &r,
                    restarted.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
                )
                .unwrap();
                let current = testing::drive(
                    &r,
                    restarted
                        .accept_response(testing::issue(&request, &ca, &key, NEW_NODE), &scope),
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
            let env = racer_io::Env::host();
            let Some(r) = testing::reactor(&env) else {
                return;
            };
            let scope = testing::scope();
            let directory = testing::Directory::new();
            let token = directory.0.join("token");
            std::fs::write(&token, "first.token").unwrap();
            let cluster = ClusterId("11111111-1111-4111-8111-111111111111".into());
            let enrollment = Enrollment::new(
                cluster.clone(),
                token.clone(),
                directory.0.join("identity"),
                Rc::new(ReactorControlIo::new(r.clone())),
            );
            assert!(!directory.0.join("identity").exists());
            let request = testing::drive(
                &r,
                enrollment.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
            )
            .unwrap();
            let again = Enrollment::new(
                cluster,
                token.clone(),
                directory.0.join("identity"),
                Rc::new(ReactorControlIo::new(r.clone())),
            );
            assert_eq!(
                request.csr_der,
                testing::drive(
                    &r,
                    again.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope)
                )
                .unwrap()
                .csr_der
            );
            assert_eq!(
                request.enrollment,
                testing::drive(
                    &r,
                    again.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope)
                )
                .unwrap()
                .enrollment
            );
            assert_eq!(
                &*testing::drive(&r, again.read_token(&scope)).unwrap(),
                "first.token"
            );
            std::fs::write(&token, "rotated.token").unwrap();
            assert_eq!(
                &*testing::drive(&r, again.read_token(&scope)).unwrap(),
                "rotated.token"
            );
            let (ca, key) = testing::ca();
            again.set_peer_trust_roots(vec![ca.der().to_vec()]).unwrap();
            let response =
                testing::issue(&request, &ca, &key, "22222222-2222-4222-8222-222222222222");
            let mut wrong = response.clone();
            wrong.node.0 = "33333333-3333-4333-8333-333333333333".into();
            assert!(testing::drive(&r, again.accept_response(wrong, &scope)).is_err());
            assert!(directory.0.join("identity/pending.json").exists());
            let identity = testing::drive(&r, again.accept_response(response, &scope)).unwrap();
            assert!(identity.valid_at(racer_io::Clock::host().wall_now()));
            assert!(!identity.renewal_due_at(racer_io::Clock::host().wall_now()));
            assert_eq!(
                testing::drive(&r, again.load_identity(&scope))
                    .unwrap()
                    .unwrap()
                    .node(),
                identity.node()
            );
            assert!(!directory.0.join("identity/pending.json").exists());
            let fresh = testing::drive(
                &r,
                again.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
            )
            .unwrap();
            assert_ne!(fresh.enrollment, request.enrollment);
            assert_ne!(fresh.csr_der, request.csr_der);
            assert_eq!(
                testing::drive(&r, again.load_identity(&scope))
                    .unwrap()
                    .unwrap()
                    .node(),
                identity.node()
            );
            std::fs::write(directory.0.join("identity/pending.json"), b"{broken").unwrap();
            assert!(
                testing::drive(
                    &r,
                    again.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope)
                )
                .is_err()
            );
        }
        #[test]
        fn durable_rail_journal_prevents_restart_renumbering_and_corruption_fails_closed() {
            use rdma_verbs::simulation::Device;
            use rdma_verbs::simulation::Simulation;
            let env = racer_io::Env::host();
            let Some(r) = testing::reactor(&env) else {
                return;
            };
            let scope = testing::scope();
            let directory = testing::Directory::new();
            let make = || {
                let io = Rc::new(ReactorControlIo::new(r.clone()));
                let e = Enrollment::new(
                    ClusterId("11111111-1111-4111-8111-111111111111".into()),
                    directory.0.join("token"),
                    directory.0.join("identity"),
                    io.clone(),
                );
                let journal = rails::RailJournal::new(
                    Arc::new(Default::default()),
                    directory.0.join("identity"),
                    io,
                );
                (e, journal)
            };
            let all = Simulation::new()
                .with_devices(vec![Device::new("a", [1; 16]), Device::new("b", [2; 16])])
                .unwrap();
            let first = {
                let _environment = all.enter();
                let (e, journal) = make();
                let nics = testing::drive(&r, journal.persist(&scope)).unwrap();
                testing::drive(&r, e.prepare(nics, NonZeroU32::new(4).unwrap(), &scope)).unwrap()
            };
            assert_eq!(first.rdma_nics[1].rail.0, 1);
            let only_b = Simulation::new()
                .with_devices(vec![Device::new("b", [3; 16])])
                .unwrap();
            let _environment = only_b.enter();
            let (e, journal) = make();
            let nics = testing::drive(&r, journal.persist(&scope)).unwrap();
            let restarted =
                testing::drive(&r, e.prepare(nics, NonZeroU32::new(4).unwrap(), &scope)).unwrap();
            assert_eq!(restarted.rdma_nics.len(), 1);
            assert_eq!(restarted.rdma_nics[0].rail.0, 1);
            assert_eq!(restarted.rdma_nics[0].gid, Some([3; 16]));
            std::fs::write(directory.0.join("identity/rdma-rails.json"), b"broken").unwrap();
            assert!(matches!(
                testing::drive(&r, make().1.persist(&scope)),
                Err(Error::CorruptRecord)
            ));
        }
        #[test]
        fn saturated_rail_journal_still_persists_and_enrolls_known_ports() {
            use crate::rdma::MAX_JOURNAL_BYTES;
            use racer_wire::control::RailId;
            let env = racer_io::Env::host();
            let Some(r) = testing::reactor(&env) else {
                return;
            };
            let scope = testing::scope();
            let directory = testing::Directory::new();
            let inventory: Arc<crate::rdma::Inventory> = Arc::new(Default::default());
            use rdma_verbs::simulation::Device;
            use rdma_verbs::simulation::Simulation;
            let name = |i| format!("{i:04}{}", "x".repeat(59));
            // Hot-plug reuses withdrawn rails, so only a restored journal
            // can sit at the byte bound.
            let mut bytes = 0;
            let mut rails: Vec<_> = (0..1024)
                .map(|i| (name(i), 1u8, i as u16))
                .take_while(|e| {
                    bytes += serde_json::to_vec(e).unwrap().len() + 1;
                    bytes + 40 < MAX_JOURNAL_BYTES
                })
                .collect();
            // Top up with a shorter name so a longer replacement cannot fit.
            let encoded =
                |rails: &Vec<(String, u8, u16)>| serde_json::to_vec(rails).unwrap().len() + 22;
            for len in (1..=63).rev() {
                let mut topped = rails.clone();
                topped.push(("y".repeat(len), 1u8, rails.len() as u16));
                if encoded(&topped) <= MAX_JOURNAL_BYTES {
                    rails = topped;
                    break;
                }
            }
            inventory
                .restore(
                    format!(
                        r#"{{"version":2,"rails":{}}}"#,
                        serde_json::to_string(&rails).unwrap()
                    )
                    .as_bytes(),
                )
                .unwrap();
            assert!(inventory.reservations().unwrap().len() <= MAX_JOURNAL_BYTES);
            let expected_journal = inventory.reservations().unwrap();
            let sim = Simulation::new()
                .with_devices(vec![
                    Device::new(name(0), [2; 16]),
                    Device::new(format!("2000{}", "\"".repeat(59)), [3; 16]),
                ])
                .unwrap();
            let _devices = sim.enter();
            let make = || {
                let io = Rc::new(ReactorControlIo::new(r.clone()));
                let e = Enrollment::new(
                    ClusterId("11111111-1111-4111-8111-111111111111".into()),
                    directory.0.join("token"),
                    directory.0.join("identity"),
                    io.clone(),
                );
                (
                    e,
                    rails::RailJournal::new(inventory.clone(), directory.0.join("identity"), io),
                )
            };
            for _ in 0..2 {
                let (e, journal) = make();
                let nics = testing::drive(&r, journal.persist(&scope)).unwrap();
                let request =
                    testing::drive(&r, e.prepare(nics, NonZeroU32::new(4).unwrap(), &scope))
                        .unwrap();
                assert_eq!(request.rdma_nics.len(), 1);
                assert_eq!(request.rdma_nics[0].device, name(0));
                assert_eq!(request.rdma_nics[0].rail, RailId(0));
                assert_eq!(request.rdma_nics[0].gid, Some([2; 16]));
                assert_eq!(
                    std::fs::read(directory.0.join("identity/rdma-rails.json")).unwrap(),
                    expected_journal
                );
            }
        }
        #[test]
        fn rejects_symlinked_identity_and_insecure_modes() {
            let env = racer_io::Env::host();
            let Some(r) = testing::reactor(&env) else {
                return;
            };
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
                Rc::new(ReactorControlIo::new(r.clone())),
            );
            assert!(
                testing::drive(
                    &r,
                    enrollment.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope)
                )
                .is_err()
            );
            std::fs::set_permissions(
                directory.0.join("real"),
                std::fs::Permissions::from_mode(0o755),
            )
            .unwrap();
            let insecure = Enrollment::new(
                enrollment.cluster().clone(),
                directory.0.join("token"),
                directory.0.join("real"),
                Rc::new(ReactorControlIo::new(r.clone())),
            );
            assert!(
                testing::drive(
                    &r,
                    insecure.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope)
                )
                .is_err()
            );
        }
    }

    pub(crate) mod async_files_tests {
        //! Scoped file access, durable replacement, and cancellation fences.

        use super::*;
        use crate::test_support::enrollment as testing;
        #[test]
        fn secure_adapters_preserve_token_symlinks_identity_rejection_and_errors() {
            use racer_io::sim::SimOs;
            let sim = SimOs::new();
            let env = racer_io::Env::sim(&sim, racer_io::WorkerCtx::host());
            let r = testing::reactor(&env).unwrap();
            let scope = testing::scope();
            sim.write_file(Path::new("/token-data"), b"token").unwrap();
            sim.symlink(Path::new("token-data"), Path::new("/token"))
                .unwrap();
            assert_eq!(
                &*testing::drive(&r, Box::pin(read_path(&r, Path::new("/token"), 5, &scope)))
                    .unwrap(),
                b"token"
            );
            let dir = testing::drive(
                &r,
                Box::pin(directory(&r, Path::new("/"), false, false, &scope)),
            )
            .unwrap();
            assert!(matches!(
                testing::drive(&r, Box::pin(read_at(&r, &dir, "token", 5, false, &scope))),
                Err(Error::Os(libc::ELOOP))
            ));
            assert!(matches!(
                testing::drive(
                    &r,
                    Box::pin(read_at(&r, &dir, "token-data", 4, false, &scope))
                ),
                Err(Error::InvalidRequest)
            ));
            assert!(matches!(
                testing::drive(
                    &r,
                    Box::pin(read_at(
                        &r,
                        &dir,
                        "token-data",
                        1024 * 1024 + 1,
                        false,
                        &scope
                    ))
                ),
                Err(Error::Overloaded)
            ));
            assert!(matches!(
                testing::drive(
                    &r,
                    Box::pin(directory(&r, Path::new("/../escape"), false, false, &scope))
                ),
                Err(Error::InvalidConfiguration)
            ));
            sim.chmod(Path::new("/token-data"), 0o644).unwrap();
            assert!(matches!(
                testing::drive(
                    &r,
                    Box::pin(read_at(&r, &dir, "token-data", 5, true, &scope))
                ),
                Err(Error::Unauthorized)
            ));
            sim.chmod(Path::new("/token-data"), 0o600).unwrap();
            assert_eq!(
                &*testing::drive(
                    &r,
                    Box::pin(read_at(&r, &dir, "token-data", 5, true, &scope))
                )
                .unwrap(),
                b"token"
            );
            let stat: libc::statx = unsafe { std::mem::zeroed() };
            assert_eq!(
                secure::check_private(&stat, true).map_err(Error::from),
                Err(Error::Io)
            );
        }
        #[test]
        fn enrollment_creates_private_child_beneath_kubelet_host_path() {
            use std::os::unix::fs::PermissionsExt;
            let env = racer_io::Env::host();
            let Some(r) = testing::reactor(&env) else {
                return;
            };
            let d = testing::Directory::new();
            let mount = d.0.join("identity");
            std::fs::create_dir(&mount).unwrap();
            std::fs::set_permissions(&mount, std::fs::Permissions::from_mode(0o755)).unwrap();
            let scope = testing::scope();
            let enrollment = |path| {
                Enrollment::new(
                    racer_wire::control::ClusterId("11111111-1111-4111-8111-111111111111".into()),
                    d.0.join("token"),
                    path,
                    Rc::new(ReactorControlIo::new(r.clone())),
                )
            };
            // A DirectoryOrCreate mount itself cannot store private identity state.
            let insecure = enrollment(mount.clone());
            assert!(matches!(
                testing::drive(&r, insecure.load_identity(&scope)),
                Err(Error::Unauthorized)
            ));
            let private = mount.join("private");
            let e = enrollment(private.clone());
            assert!(
                testing::drive(&r, e.load_identity(&scope))
                    .unwrap()
                    .is_none()
            );
            let request =
                testing::drive(&r, e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope)).unwrap();
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
            let identity = testing::drive(&r, e.accept_response(response, &scope)).unwrap();
            let restarted = enrollment(private.clone());
            restarted
                .set_peer_trust_roots(vec![ca.der().to_vec()])
                .unwrap();
            assert_eq!(
                testing::drive(&r, restarted.load_identity(&scope))
                    .unwrap()
                    .unwrap()
                    .node(),
                identity.node()
            );
            // Keep rejecting insecure existing private directories, even on restart.
            std::fs::set_permissions(&private, std::fs::Permissions::from_mode(0o755)).unwrap();
            assert!(matches!(
                testing::drive(&r, restarted.load_identity(&scope)),
                Err(Error::Unauthorized)
            ));
        }
        #[test]
        fn canceled_replacement_never_exposes_partial_state_and_retry_fences_late_work() {
            let env = racer_io::Env::host();
            let Some(r) = testing::reactor(&env) else {
                return;
            };
            let d = testing::Directory::new();
            let scope = testing::scope();
            let dir = testing::drive(
                &r,
                Box::pin(directory(&r, &d.0.join("private"), true, true, &scope)),
            )
            .unwrap();
            for (old_length, new_length) in [(1001, 1003), (32771, 65539), (32769, 32771)] {
                let old = vec![11; old_length];
                let new = vec![22; new_length];
                // Stop between each submission/completion boundary. The worker is not
                // allowed to publish replacement bytes before the final durability fence.
                for stop in 0..12u8 {
                    let scope = testing::scope();
                    testing::drive(
                        &r,
                        Box::pin(
                            atomic_write(&r, &dir, "state", &old, &scope).map_err(Error::from),
                        ),
                    )
                    .unwrap();
                    let mut turn = testing::scope();
                    turn.request = crate::model::RequestId([stop; 16]);
                    let mut operation: crate::error::Operation<'_, ()> =
                        Box::pin(atomic_write(&r, &dir, "state", &new, &turn).map_err(Error::from));
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
                        Box::pin(
                            atomic_write(&r, &dir, "state", b"retry", &scope).map_err(Error::from),
                        ),
                    )
                    .unwrap();
                    assert_eq!(
                        &*testing::drive(
                            &r,
                            Box::pin(read_at(&r, &dir, "state", 10, true, &scope))
                        )
                        .unwrap(),
                        b"retry"
                    );
                }
                assert_eq!(r.in_flight(), 0);
            }
        }
        #[test]
        fn canceled_replacement_at_each_submission_boundary_is_complete_or_old() {
            let env = racer_io::Env::host();
            let Some(r) = testing::reactor(&env) else {
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
                Box::pin(atomic_write(&r, &dir, "identity", &old, &setup).map_err(Error::from)),
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
                assert!(*bytes == old || *bytes == new, "partial committed identity");
                testing::drive(
                    &r,
                    Box::pin(atomic_write(&r, &dir, "identity", &old, &check).map_err(Error::from)),
                )
                .unwrap();
            }
            assert_eq!(r.in_flight(), 0);
        }
        #[test]
        fn real_ring_enrollment_durability_projection_and_abandoned_retry() {
            let env = racer_io::Env::host();
            let Some(r) = testing::reactor(&env) else {
                return;
            };
            let d = testing::Directory::new();
            let scope = testing::scope();
            let e = Enrollment::new(
                racer_wire::control::ClusterId("11111111-1111-4111-8111-111111111111".into()),
                d.0.join("token"),
                d.0.join("identity"),
                Rc::new(ReactorControlIo::new(r.clone())),
            );
            let mut abandoned = e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope);
            let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
            assert!(abandoned.as_mut().poll(&mut cx).is_pending());
            // Merely polling cannot perform the open/mkdir. No hidden executor drives it.
            for _ in 0..10 {
                assert!(abandoned.as_mut().poll(&mut cx).is_pending());
            }
            assert!(!d.0.join("identity").exists());
            assert_eq!(r.in_flight(), 1);
            drop(abandoned);
            let pending =
                testing::drive(&r, e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope)).unwrap();
            assert_eq!(
                pending.csr_der,
                testing::drive(&r, e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope))
                    .unwrap()
                    .csr_der
            );
            let (ca, key) = testing::ca();
            e.set_peer_trust_roots(vec![ca.der().to_vec()]).unwrap();
            let response =
                testing::issue(&pending, &ca, &key, "22222222-2222-4222-8222-222222222222");
            let identity = testing::drive(&r, e.accept_response(response, &scope)).unwrap();
            assert_eq!(
                identity.node(),
                testing::drive(&r, e.load_identity(&scope))
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

#[cfg(test)]
mod caches_tests {
    //! Indexed cache lookup agrees with publication order.

    use super::*;

    fn definition(id: &str, priority: u16) -> CacheDefinition {
        CacheDefinition {
            id: CacheId(id.into()),
            name: id.into(),
            client_socket: PathBuf::new(),
            origin_socket: PathBuf::new(),
            priority,
            mode: racer_wire::control::CacheMode::default(),
            origin: None,
            writes: None,
        }
    }

    #[test]
    fn lookup_by_id_preserves_publication_order() {
        let caches = Caches::new(vec![
            definition("c", 0),
            definition("a", 1),
            definition("b", 2),
        ]);
        assert_eq!(caches.position(&CacheId("a".into())), Some(1));
        assert_eq!(caches.find(&CacheId("b".into())).unwrap().priority, 2);
        assert!(caches.contains(&CacheId("c".into())));
        assert!(!caches.contains(&CacheId("d".into())));
        assert_eq!(caches[0].id, CacheId("c".into()));
        assert!(!Caches::default().contains(&CacheId("a".into())));
    }
}
