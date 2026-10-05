use crate::admission::AdmissionPolicy;
use crate::admission::ResourceClass;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::memory::BufferPool;
use crate::memory::CiphertextPage;
use crate::model::PageEnvelope;
use crate::model::PageId;
use crate::model::TransferId;
use crate::peer::protocol::VerifiedHead;
use crate::runtime::RequestScope;
use crate::security::PageCryptoEngine;
use crate::topology::FAILURE_LINKS;
use crate::topology::Route;
use base64::Engine;
use base64::engine::general_purpose::STANDARD;
#[cfg(test)]
use racer_control_wire::CacheId;
#[cfg(test)]
use racer_control_wire::KeyId;
#[cfg(test)]
use racer_control_wire::MembershipVersion;
use racer_control_wire::NodeId;
use racer_control_wire::RailId;
use racer_control_wire::RailMapping;
use racer_crypto::identity::VerifiedPeer;
use rdma_verbs::DeviceHandle;
use rdma_verbs::Endpoint;
use rdma_verbs::IoPort;
#[cfg(test)]
use rdma_verbs::NativeService;
use rdma_verbs::PortInfo;
use rdma_verbs::QueuePairHandle;
use rdma_verbs::Region;
use rdma_verbs::discovery::Binding;
use sha2::Digest;
use sha2::Sha256;
use std::cell::Cell;
use std::cell::RefCell;
use std::future::poll_fn;
use std::path::Path;
use std::rc::Rc;
use std::task::Poll;
use uring_runtime::drivers::poll_scoped;
use uring_runtime::environment::Deadline;
#[cfg(test)]
use uring_runtime::group::Service;

// Match discovered ports to trusted local fabric associations and publication.
// Fabric strings are opaque labels: a GID or enumeration order is never a label.
//
// Public contracts always compile; native FFI is gated by `rdma`. Attach bounded
// lifecycle endpoints, activate devices against publication, and run `WithNative`
// on the crypto role. I/O turns consume mailboxes and drive session progress.

/// Compose page crypto and native progress on the existing paired crypto thread.
pub type WithNative = rdma_verbs::WithNative<PageCryptoEngine>;

pub const SETUP_HEADER: &str = "racer-rdma-setup";
pub const SETUP_BINDING_HEADER: &str = "racer-rdma-setup-binding";
/// Bounded authenticated RC sessions. Each transfer uses a dedicated session;
/// terminal QP destruction fences remote writes before registered memory reuse.
pub struct Sessions {
    devices: Rc<Devices>,

    per_neighbor: usize,

    live: rdma_verbs::QpSet<NodeId>,

    draining: Cell<bool>,

    #[cfg(test)]
    pub(crate) prepare_attempts: Cell<usize>,
}
pub struct SessionLease {
    pub(crate) qp: Rc<QueuePairHandle>,
    peer: NodeId,
    rail: RailId,
    binding: [u8; 32],
    claimed: Cell<bool>,
}
pub struct PreparedSession {
    qp: Rc<QueuePairHandle>,
    peer: NodeId,
    setup: SetupParameters,
    finished: Cell<bool>,
}
/// Canonical bytes belong in the signed SETUP_HEADER, never an unsigned body.
pub struct SetupParameters {
    pub rail: RailId,
    pub encoded: Vec<u8>,
}
impl SetupParameters {
    fn new(rail: RailId, endpoint: Endpoint) -> Result<Self> {
        let mut nonce = [0; 16];
        uring_runtime::environment::fill_random(&mut nonce).map_err(|_| Error::Io)?;
        let mut encoded = b"racer-rdma-setup-v1\0".to_vec();
        encoded.extend_from_slice(&rail.0.to_be_bytes());
        encoded.extend_from_slice(&nonce);
        encoded.extend_from_slice(&endpoint.to_bytes());
        Ok(Self { rail, encoded })
    }
    fn endpoint(&self) -> Result<Endpoint> {
        let prefix = b"racer-rdma-setup-v1\0";
        if self.encoded.len() != prefix.len() + 50 || !self.encoded.starts_with(prefix) {
            return Err(Error::InvalidRequest);
        }
        let b = &self.encoded[prefix.len()..];
        if u16::from_be_bytes(b[..2].try_into().unwrap()) != self.rail.0 || b[2..18] == [0; 16] {
            return Err(Error::InvalidRequest);
        }
        Endpoint::from_bytes(&b[18..]).map_err(Into::into)
    }
    pub fn header_value(&self) -> Vec<u8> {
        STANDARD.encode(&self.encoded).into_bytes()
    }
    /// Include this in the peer's signed acknowledgment of this exact offer.
    pub fn binding_header_value(&self) -> Vec<u8> {
        STANDARD.encode(Sha256::digest(&self.encoded)).into_bytes()
    }
    pub fn from_verified(head: &VerifiedHead, rail: RailId) -> Result<Self> {
        let value = signed_value(head, SETUP_HEADER, 128)?;
        let setup = Self {
            rail,
            encoded: value,
        };
        setup.endpoint()?;
        Ok(setup)
    }
}
/// The security owner must cover these extension headers in the signature's
/// component list. VerifiedHead establishes identity and freshness; the connection
/// session performs replay admission before native control dispatch.
pub(crate) fn signed_value(head: &VerifiedHead, name: &str, bound: usize) -> Result<Vec<u8>> {
    let value = head.signed.head.unique(name)?.ok_or(Error::Unauthorized)?;
    if value.len() > bound * 2 {
        return Err(Error::InvalidRequest);
    }
    // Require the extension to be explicitly signed, not merely present in a
    // verified message. The signature-input grammar uses quoted component names.
    let input = head
        .signed
        .head
        .unique("signature-input")?
        .ok_or(Error::Unauthorized)?;
    let quoted = format!("\"{name}\"");
    if !input.windows(quoted.len()).any(|w| w == quoted.as_bytes()) {
        return Err(Error::Unauthorized);
    }
    let bytes = STANDARD.decode(value).map_err(|_| Error::InvalidRequest)?;
    if bytes.len() > bound || STANDARD.encode(&bytes).as_bytes() != value {
        return Err(Error::InvalidRequest);
    }
    Ok(bytes)
}
impl Sessions {
    #[cfg(test)]
    pub(crate) fn track_test(&self, qp: Rc<QueuePairHandle>) {
        self.live.track(NodeId("test".into()), qp);
    }
    pub fn register_driver(&self, waker: &std::task::Waker) {
        self.devices.register_driver(waker);
    }
    #[cfg(test)]
    pub(crate) fn track_peer_test(&self, peer: NodeId, qp: Rc<QueuePairHandle>) {
        self.live.track(peer, qp);
    }
    pub fn new(devices: Rc<Devices>, per_neighbor: usize) -> Self {
        Self {
            devices,
            per_neighbor,
            live: rdma_verbs::QpSet::default(),
            draining: Cell::new(false),
            #[cfg(test)]
            prepare_attempts: Cell::new(0),
        }
    }
    pub fn ready(&self, rail: RailId) -> bool {
        !self.draining.get() && self.per_neighbor > 0 && self.devices.ready(rail)
    }
    /// Both sides prepare before exchanging signed setup headers. The routing
    /// owner must first validate the actual hop's authenticated Site and rail mapping.
    pub fn prepare<'a>(
        &'a self,
        peer: &'a VerifiedPeer,
        rail: RailId,
        scope: &'a RequestScope,
    ) -> Operation<'a, PreparedSession> {
        self.prepare_admitted(peer, rail, None, None, scope)
    }
    pub(crate) fn prepare_admitted<'a>(
        &'a self,
        peer: &'a VerifiedPeer,
        rail: RailId,
        permit: Option<std::sync::Arc<crate::peer::Permit>>,
        receive: Option<std::sync::Arc<crate::peer::receive::Permit>>,
        scope: &'a crate::runtime::RequestScope,
    ) -> Operation<'a, PreparedSession> {
        #[cfg(test)]
        self.prepare_attempts.set(self.prepare_attempts.get() + 1);
        Box::pin(poll_scoped(scope, move |cx| {
            self.register_driver(cx.waker());
            self.poll_prepare(peer, rail, permit.clone(), receive.clone())
        }))
    }
    fn poll_prepare(
        &self,
        peer: &VerifiedPeer,
        rail: RailId,
        permit: Option<std::sync::Arc<crate::peer::Permit>>,
        receive: Option<std::sync::Arc<crate::peer::receive::Permit>>,
    ) -> Poll<Result<PreparedSession>> {
        if !self.ready(rail) {
            return Poll::Ready(Err(Error::Unavailable));
        }
        self.live.admit(
            peer.node(),
            self.per_neighbor
                .saturating_mul(crate::topology::MAX_DEGREE),
            self.per_neighbor,
        )?;
        let qp = std::task::ready!(QueuePairHandle::poll_new(
            self.devices.select(rail)?.handle,
            if receive.is_some() {
                Some(std::sync::Arc::new((permit, receive)) as rdma_verbs::Guard)
            } else {
                permit.map(|p| p as rdma_verbs::Guard)
            }
        ))?;
        // Bound a peer that opens setup but never completes the exchange. A
        // transfer subsequently replaces this with its original request deadline.
        qp.expire_at(uring_runtime::environment::now() + std::time::Duration::from_secs(30));
        let setup = SetupParameters::new(rail, qp.endpoint)?;
        self.live.track(peer.node().clone(), qp.clone());
        Poll::Ready(Ok(PreparedSession {
            qp,
            peer: peer.node().clone(),
            setup,
            finished: Cell::new(false),
        }))
    }
    pub fn progress(&self) -> Result<usize> {
        // Errors belong to the attempt ticket. The paired native service keeps
        // failed fences quarantined; neither a timeout nor CQ error is node-fatal.
        Ok(self.live.progress())
    }
    pub fn drain(&self) -> Operation<'_, ()> {
        Box::pin(async move {
            self.draining.set(true);
            self.live.fence().await?;
            self.live.reap();
            self.devices.close();
            Ok(())
        })
    }
    /// Explicit nonterminal maintenance cut. Cache/key publication does not call
    /// this; live transfers drain under their own leases and terminal fences.
    /// The captured Rc owners survive request cancellation and are fenced on the
    /// paired native role. A timeout never turns cancellation into successful DMA
    /// release. Callers may keep driving unrelated HTTP while this awaits.
    pub fn fence_cut(&self) -> Operation<'static, ()> {
        let fence = self.live.fence();
        Box::pin(async move { fence.await.map_err(Into::into) })
    }
}
impl PreparedSession {
    pub fn setup(&self) -> &SetupParameters {
        &self.setup
    }
    /// Await mailbox submission without consuming setup on transient contention.
    /// The returned session still requires wait_ready for native completion.
    pub fn finish<'a>(
        self,
        head: &'a VerifiedHead,
        scope: &'a RequestScope,
    ) -> Operation<'a, SessionLease> {
        Box::pin(async move {
            scope.check()?;
            if head.peer.node() != &self.peer {
                return Err(Error::Unauthorized);
            }
            if signed_value(head, SETUP_BINDING_HEADER, 32)?
                != Sha256::digest(&self.setup.encoded).as_slice()
            {
                return Err(Error::Unauthorized);
            }
            let remote = SetupParameters::from_verified(head, self.setup.rail)?;
            if remote.encoded == self.setup.encoded {
                return Err(Error::Replay);
            }
            let endpoint = remote.endpoint()?;
            poll_scoped(scope, |cx| {
                self.qp.register_waiter(cx);
                self.qp.poll_connect(endpoint)
            })
            .await?;
            let mut pair = [&self.setup.encoded, &remote.encoded];
            pair.sort();
            let mut hash = Sha256::new();
            hash.update(b"racer-rdma-session-v1\0");
            for bytes in pair {
                hash.update(bytes);
            }
            self.finished.set(true);
            Ok(SessionLease {
                qp: self.qp.clone(),
                peer: self.peer.clone(),
                rail: self.setup.rail,
                binding: hash.finalize().into(),
                claimed: Cell::new(false),
            })
        })
    }
}
impl Drop for PreparedSession {
    fn drop(&mut self) {
        if !self.finished.get() {
            self.qp.stop();
        }
    }
}
impl Drop for SessionLease {
    fn drop(&mut self) {
        self.qp.stop();
    }
}
impl SessionLease {
    #[cfg(test)]
    pub(crate) fn test(qp: Rc<QueuePairHandle>, peer: NodeId) -> Self {
        Self {
            qp,
            peer,
            rail: RailId(0),
            binding: [7; 32],
            claimed: Cell::new(false),
        }
    }
    /// Connecting is genuinely asynchronous. Await this before preparing a
    /// receive grant; send_to also waits internally.
    pub fn wait_ready<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            let cancellation = scope.cancellation.subscribe()?;
            futures::future::poll_fn(|cx| {
                cancellation.register(cx.waker());
                if let Err(error) = scope.check() {
                    self.qp.stop();
                    return Poll::Ready(Err(error));
                }
                self.qp.poll_connected(cx).map_err(Into::into)
            })
            .await
        })
    }
    pub fn rail(&self) -> RailId {
        self.rail
    }
    pub fn peer(&self) -> &NodeId {
        &self.peer
    }
    pub fn binding(&self) -> [u8; 32] {
        self.binding
    }
    pub fn progress(&self) -> Result<usize> {
        self.qp.progress().map_err(Into::into)
    }
    pub fn ready(&self) -> bool {
        self.qp.ready() && !self.claimed.get()
    }
    pub(crate) fn claim(&self) -> Result<()> {
        if !self.qp.ready() || self.claimed.replace(true) {
            return Err(Error::Unavailable);
        }
        Ok(())
    }
    pub fn abort(&self) -> Result<()> {
        self.qp.stop();
        Ok(())
    }
}

pub const DESCRIPTOR_HEADER: &str = "racer-rdma-descriptor";
pub const COMPLETION_HEADER: &str = "racer-rdma-completion";
/// Type-2B windows expose exactly one transfer buffer. Bind CQE precedes export;
/// invalidation plus terminal QP destruction precedes any CPU access or reuse.
pub struct Grant {
    transfer: TransferId,

    buffer: Option<RegisteredLease>,

    receive: rdma_verbs::Receive,

    deadline: Deadline,

    binding: [u8; 32],
}
pub struct RemoteDescriptor {
    pub transfer: TransferId,
    pub address: u64,
    pub length: u64,
    pub scoped_key: u32,
}
/// Only a verified, explicitly signed header can create a send capability.
pub struct AuthenticatedDescriptor {
    pub(crate) descriptor: RemoteDescriptor,
    binding: [u8; 32],
}
impl RemoteDescriptor {
    fn encode(&self, binding: [u8; 32]) -> Vec<u8> {
        let mut bytes = b"racer-rdma-grant-v1\0".to_vec();
        bytes.extend_from_slice(&binding);
        bytes.extend_from_slice(&self.transfer.0);
        bytes.extend_from_slice(&self.address.to_be_bytes());
        bytes.extend_from_slice(&self.length.to_be_bytes());
        bytes.extend_from_slice(&self.scoped_key.to_be_bytes());
        bytes
    }
    fn decode(bytes: &[u8], binding: [u8; 32]) -> Result<Self> {
        let prefix = b"racer-rdma-grant-v1\0";
        if bytes.len() != prefix.len() + 68 || !bytes.starts_with(prefix) {
            return Err(Error::InvalidRequest);
        }
        let b = &bytes[prefix.len()..];
        if b[..32] != binding {
            return Err(Error::Unauthorized);
        }
        let descriptor = Self {
            transfer: TransferId(b[32..48].try_into().unwrap()),
            address: u64::from_be_bytes(b[48..56].try_into().unwrap()),
            length: u64::from_be_bytes(b[56..64].try_into().unwrap()),
            scoped_key: u32::from_be_bytes(b[64..68].try_into().unwrap()),
        };
        if descriptor.length == 0
            || descriptor.length > MAX_CIPHERTEXT as u64
            || descriptor.address == 0
            || descriptor.address.checked_add(descriptor.length).is_none()
        {
            return Err(Error::InvalidRange);
        }
        Ok(descriptor)
    }
}
impl AuthenticatedDescriptor {
    pub fn from_verified(
        head: &VerifiedHead,
        session: &SessionLease,
        transfer: TransferId,
    ) -> Result<Self> {
        if head.peer.node() != session.peer() {
            return Err(Error::Unauthorized);
        }
        let bytes = signed_value(head, DESCRIPTOR_HEADER, 128)?;
        let descriptor = RemoteDescriptor::decode(&bytes, session.binding())?;
        if descriptor.transfer != transfer {
            return Err(Error::Unauthorized);
        }
        Ok(Self {
            descriptor,
            binding: session.binding(),
        })
    }
    pub(crate) fn validate(&self, session: &SessionLease, length: usize) -> Result<()> {
        if self.binding != session.binding() || self.descriptor.length != length as u64 {
            return Err(Error::Unauthorized);
        }
        Ok(())
    }
}
impl Grant {
    pub fn bind<'a>(
        session: &'a SessionLease,
        buffer: RegisteredLease,
        transfer: TransferId,
        scope: &'a RequestScope,
    ) -> Operation<'a, Grant> {
        Box::pin(async move {
            scope.check()?;
            let deadline = scope.deadline;
            if buffer.rail != session.rail() {
                return Err(Error::InvalidRequest);
            }
            session.claim()?;
            session.qp.expire_at(deadline.0);
            let receive = rdma_verbs::Transfer::new(session.qp.clone())
                .bind(buffer.region.clone(), scope)
                .await?;
            Ok(Grant {
                transfer,
                buffer: Some(buffer),
                receive,
                deadline,
                binding: session.binding(),
            })
        })
    }
    pub fn transfer(&self) -> TransferId {
        self.transfer
    }
    pub fn descriptor(&self) -> Result<RemoteDescriptor> {
        if uring_runtime::environment::now() >= self.deadline.0 {
            return Err(Error::DeadlineExceeded);
        }
        let (address, scoped_key) = self.receive.descriptor()?;
        let buffer = self.buffer.as_ref().ok_or(Error::InvalidRequest)?;
        Ok(RemoteDescriptor {
            transfer: self.transfer,
            address,
            length: buffer.len() as u64,
            scoped_key,
        })
    }
    pub fn header_value(&self) -> Result<Vec<u8>> {
        Ok(STANDARD
            .encode(self.descriptor()?.encode(self.binding))
            .into_bytes())
    }
    pub fn wait_bound<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            let cancellation = scope.cancellation.subscribe()?;
            poll_fn(move |cx| {
                cancellation.register(cx.waker());
                if let Err(error) = scope.check() {
                    self.receive.abort();
                    return Poll::Ready(Err(error));
                }
                if uring_runtime::environment::now() >= self.deadline.0 {
                    self.receive.abort();
                    return Poll::Ready(Err(Error::DeadlineExceeded));
                }
                self.receive.poll_bound(cx).map_err(Into::into)
            })
            .await
        })
    }
    /// Receiver accepts completion only after the sender's successful write CQE
    /// has been attested in the signed control exchange. AEAD still authenticates
    /// the bytes later; this message alone never makes plaintext publishable.
    pub fn finish<'a>(
        mut self,
        head: &'a VerifiedHead,
        session: &'a SessionLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, RegisteredLease> {
        Box::pin(async move {
            scope.check()?;
            if head.peer.node() != session.peer() || session.binding() != self.binding {
                return Err(Error::Unauthorized);
            }
            let bytes = signed_value(head, COMPLETION_HEADER, 128)?;
            if bytes != completion_bytes(self.binding, self.transfer) {
                return Err(Error::Unauthorized);
            }
            self.receive.check_bound()?;
            let cancellation = scope.cancellation.subscribe()?;
            poll_fn(|cx| {
                cancellation.register(cx.waker());
                scope.check()?;
                if uring_runtime::environment::now() >= self.deadline.0 {
                    return Poll::Ready(Err(Error::DeadlineExceeded));
                }
                self.receive.poll_finish(cx).map_err(Into::into)
            })
            .await?;
            self.buffer.take().ok_or(Error::InvalidRequest)
        })
    }
}
pub(crate) fn completion_bytes(binding: [u8; 32], transfer: TransferId) -> Vec<u8> {
    let mut bytes = b"racer-rdma-complete-v1\0".to_vec();
    bytes.extend_from_slice(&binding);
    bytes.extend_from_slice(&transfer.0);
    bytes
}

/// Produced only by a successful native write CQE. Sign this header as part of
/// the request-bound HTTP control response; the ciphertext is not hashed here.
pub struct SendCompletion {
    binding: [u8; 32],
    transfer: TransferId,
}
impl SendCompletion {
    pub fn header_value(&self) -> Vec<u8> {
        STANDARD
            .encode(completion_bytes(self.binding, self.transfer))
            .into_bytes()
    }
}
/// Ciphertext-only movement. Failed attempts are fenced before HTTP fallback.
impl Sessions {
    pub fn send_to<'a>(
        &'a self,
        session: &'a SessionLease,
        page: CiphertextPage,
        descriptor: AuthenticatedDescriptor,
        scope: &'a RequestScope,
    ) -> Operation<'a, SendCompletion> {
        Box::pin(async move {
            scope.check()?;
            descriptor.validate(session, page.bytes().len())?;
            validate_envelope(page.envelope())?;
            if page.bytes().len() != page.envelope().ciphertext_length as usize {
                return Err(Error::InvalidRange);
            }
            session.wait_ready(scope).await?;
            session.claim()?;
            session.qp.expire_at(scope.deadline.0);
            let transfer = rdma_verbs::Transfer::new(session.qp.clone());
            let mut buffer = RegisteredLease::acquire(session, page.bytes().len(), scope).await?;
            buffer.copy_from(page.bytes(), scope).await?;
            transfer
                .write(
                    buffer.region.clone(),
                    descriptor.descriptor.address,
                    descriptor.descriptor.scoped_key,
                    scope,
                )
                .await?;
            Ok(SendCompletion {
                binding: session.binding(),
                transfer: descriptor.descriptor.transfer,
            })
        })
    }
    pub fn prepare_receive<'a>(
        &'a self,
        session: &'a SessionLease,
        envelope: &'a PageEnvelope,
        transfer: TransferId,
        scope: &'a RequestScope,
    ) -> Operation<'a, Grant> {
        Box::pin(async move {
            scope.check()?;
            validate_envelope(envelope)?;
            let buffer =
                RegisteredLease::acquire(session, envelope.ciphertext_length as usize, scope)
                    .await?;
            Grant::bind(session, buffer, transfer, scope).await
        })
    }
    /// This handoff requires a signed completion and returns ciphertext only.
    pub fn finish_receive<'a>(
        &'a self,
        session: &'a SessionLease,
        grant: Grant,
        head: &'a VerifiedHead,
        envelope: PageEnvelope,
        admission: &'a Rc<flow_control::Quotas<AdmissionPolicy>>,
        scope: &'a RequestScope,
    ) -> Operation<'a, CiphertextPage> {
        Box::pin(async move {
            validate_envelope(&envelope)?;
            let reservation = admission.reserve(
                Some(&envelope.page.version.object.cache),
                ResourceClass::Ciphertext,
                envelope.ciphertext_length as usize,
            )?;
            let buffer: RegisteredLease = grant.finish(head, session, scope).await?;
            if buffer.len() != envelope.ciphertext_length as usize {
                return Err(Error::InvalidRange);
            }
            let bytes = buffer.to_vec(scope).await?;
            BufferPool::new(admission.clone()).ciphertext(reservation, envelope, bytes)
        })
    }
}
fn validate_envelope(envelope: &PageEnvelope) -> Result<()> {
    if envelope.plaintext_length == 0
        || envelope.plaintext_length > 16 * 1024 * 1024
        || envelope.ciphertext_length != envelope.plaintext_length + 16
    {
        return Err(Error::InvalidRange);
    }
    Ok(())
}

pub const MAX_CIPHERTEXT: usize = 16 * 1024 * 1024 + 16;
/// Registered allocations carry their physical quota through the terminal fence.
pub struct RegisteredLease {
    pub(crate) region: Rc<Region>,
    pub(crate) rail: RailId,
}
impl RegisteredLease {
    /// Acquire the registered buffer preprovisioned for this session slot.
    pub fn acquire<'a>(
        session: &'a SessionLease,
        length: usize,
        scope: &'a RequestScope,
    ) -> Operation<'a, RegisteredLease> {
        Box::pin(async move {
            registered_charge(length)?;
            let region = poll_scoped(scope, |cx| {
                session.qp.register_waiter(cx);
                Region::poll_acquire(&session.qp, length)
            })
            .await?;
            Ok(RegisteredLease {
                region,
                rail: session.rail(),
            })
        })
    }
    pub fn len(&self) -> usize {
        self.region.length()
    }
    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }
    pub fn copy_from<'a>(
        &'a mut self,
        ciphertext: &'a [u8],
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        Box::pin(poll_scoped(scope, |cx| {
            self.region.register_waiter(cx);
            self.region.poll_copy_from(ciphertext)
        }))
    }
    pub fn to_vec<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, Vec<u8>> {
        Box::pin(poll_scoped(scope, |cx| self.region.poll_copy_to(cx)))
    }
}
fn registered_charge(length: usize) -> Result<usize> {
    if length == 0 || length > MAX_CIPHERTEXT {
        return Err(Error::InvalidRange);
    }
    // Native allocation is 4 KiB aligned. Charge all pinned pages, including a
    // short final page, rather than just the remotely visible byte range.
    length
        .checked_add(4095)
        .map(|n| n & !4095)
        .ok_or(Error::Overloaded)
}
fn validate_native_page_size(page_size: libc::c_long) -> Result<()> {
    if page_size == 4096 {
        Ok(())
    } else {
        Err(Error::Unavailable)
    }
}
/// A native slot owns both a registered buffer and bounded handoff staging.
pub(crate) fn native_slot_charge(length: usize) -> Result<usize> {
    registered_charge(length)?
        .checked_mul(2)
        .ok_or(Error::Overloaded)
}

pub struct Devices {
    port: RefCell<Option<Rc<IoPort>>>,
    selected: RefCell<Vec<Device>>,
    mappings: RefCell<Vec<RailMapping>>,
}

#[derive(Clone)]
pub struct Device {
    pub(crate) handle: Rc<DeviceHandle>,
    pub rail: RailId,
}
/// Match authenticated physical bindings against live eligible verbs ports.
pub fn match_publication(
    publication: &[RailMapping],
    discovered: &[PortInfo],
) -> Result<Vec<(RailMapping, usize)>> {
    Ok(rdma_verbs::discovery::match_ports(
        &publication.iter().map(binding).collect::<Vec<_>>(),
        discovered,
    )?
    .into_iter()
    .map(|(port, index)| (mapping(port), index))
    .collect())
}
impl Default for Devices {
    /// Create an unattached device owner with no selected rails or mappings.
    fn default() -> Self {
        Self::new()
    }
}

impl Devices {
    #[cfg(test)]
    pub(crate) fn test(port: std::rc::Rc<IoPort>) -> Self {
        let devices = Self::new();
        devices.selected.borrow_mut().push(Device {
            handle: port.device(0),
            rail: RailId(0),
        });
        *devices.port.borrow_mut() = Some(port);
        devices
    }
    pub fn new() -> Self {
        Self {
            port: RefCell::new(None),
            selected: RefCell::new(Vec::new()),
            mappings: RefCell::new(Vec::new()),
        }
    }
    pub fn attach(&self, port: IoPort) -> Result<()> {
        if self.port.borrow().is_some() {
            return Err(Error::InvalidConfiguration);
        }
        *self.port.borrow_mut() = Some(Rc::new(port));
        Ok(())
    }
    /// Allocate quota on I/O, then discover/register/provision on the paired native
    /// service. Awaiting this operation performs no filesystem or native syscall.
    pub fn activate<'a>(
        &'a self,
        publication: Vec<RailMapping>,
        admission: &'a flow_control::Quotas<AdmissionPolicy>,
        bytes_per_slot: usize,
        scope: &'a RequestScope,
    ) -> Operation<'a, Vec<RailMapping>> {
        Box::pin(async move {
            scope.check()?;
            if bytes_per_slot == 0 || bytes_per_slot > MAX_CIPHERTEXT {
                return Err(Error::InvalidConfiguration);
            }
            // Worker selection must finish before binding native tags. Tags are
            // rail IDs, so two physical ports with one tag would be ambiguous.
            let mut rails = std::collections::BTreeSet::new();
            if publication.iter().any(|nic| !rails.insert(nic.rail)) {
                return Err(Error::InvalidConfiguration);
            }
            // Racer's quota profile charges 4 KiB physical pages. This policy
            // does not belong in the generic native ABI.
            validate_native_page_size(unsafe { libc::sysconf(libc::_SC_PAGESIZE) })?;
            let port = self.port.borrow().clone().ok_or(Error::Unavailable)?;
            port.reopen()?;
            let charge = native_slot_charge(bytes_per_slot)?;
            // One native registered allocation plus one bounded handoff staging
            // allocation per slot. Both remain charged through native quarantine.
            let quotas = (0..port.capacity())
                .map(|_| {
                    admission
                        .reserve(None, ResourceClass::Registered, charge)
                        .map(|q| std::sync::Arc::new(q) as rdma_verbs::Guard)
                        .map_err(Error::from)
                })
                .collect::<Result<Vec<_>>>()?;
            let requested = publication.clone();
            let mappings = port
                .activate(
                    rdma_verbs::Configuration {
                        discover: !publication.is_empty(),
                        guards: quotas,
                        bytes: bytes_per_slot,
                        selector: Box::new(move |ports| {
                            match_publication(&requested, ports)
                                .map(|selected| {
                                    selected
                                        .into_iter()
                                        .map(|(r, i)| (u32::from(r.rail.0), i))
                                        .collect()
                                })
                                .map_err(|e| match e {
                                    Error::InvalidConfiguration => {
                                        rdma_verbs::Error::InvalidConfiguration
                                    }
                                    Error::Overloaded => rdma_verbs::Error::Overloaded,
                                    _ => rdma_verbs::Error::Unavailable,
                                })
                        }),
                    },
                    scope,
                )
                .await?;
            let mappings: Vec<_> = mappings
                .into_iter()
                .map(|selected| {
                    let mut mapping = publication
                        .iter()
                        .find(|m| {
                            u32::from(m.rail.0) == selected.tag
                                && m.device == selected.port.device
                                && m.port == selected.port.port
                        })
                        .expect("selector only returns published tags")
                        .clone();
                    mapping.numa_node = mapping.numa_node.or(selected.port.numa_node);
                    mapping.gid = Some(selected.port.gid);
                    mapping
                })
                .collect();
            *self.selected.borrow_mut() = mappings
                .iter()
                .map(|mapping| Device {
                    handle: port.device(u32::from(mapping.rail.0)),
                    rail: mapping.rail,
                })
                .collect();
            *self.mappings.borrow_mut() = mappings.clone();
            Ok(mappings)
        })
    }
    pub fn select(&self, rail: RailId) -> Result<Device> {
        self.selected
            .borrow()
            .iter()
            .find(|d| d.rail == rail)
            .cloned()
            .ok_or(Error::Unavailable)
    }
    pub fn ready(&self, rail: RailId) -> bool {
        self.select(rail).is_ok()
            && self
                .port
                .borrow()
                .as_ref()
                .is_some_and(|port| !port.closed())
    }
    pub fn close(&self) {
        if let Some(port) = self.port.borrow().as_ref() {
            port.close();
        }
    }
    pub fn capacity(&self) -> usize {
        self.port
            .borrow()
            .as_ref()
            .map_or(0, |port| port.capacity())
    }
    /// Call on every local membership publication. A mapping change revokes all
    /// old capabilities; drain the lifecycle generation before activating new rails.
    pub fn revalidate(&self, published: &[RailMapping]) -> bool {
        let actual = self.mappings.borrow();
        let valid = !actual.is_empty()
            && actual.len() == published.len()
            && actual.iter().all(|a| {
                published.iter().any(|p| {
                    p.rail == a.rail
                        && p.device == a.device
                        && p.port == a.port
                        && p.gid.is_none_or(|gid| a.gid == Some(gid))
                        && p.numa_node.is_none_or(|numa| a.numa_node == Some(numa))
                })
            });
        if !valid {
            self.close();
            self.selected.borrow_mut().clear();
        }
        valid
    }
    pub fn register_driver(&self, waker: &std::task::Waker) {
        if let Some(port) = self.port.borrow().as_ref() {
            port.register_driver(waker);
        }
    }
}

// Pre-enrollment inventory and deterministic per-worker physical NIC selection.

/// Shared durable-journal bound for admission, restoration, and enrollment I/O.
pub use rdma_verbs::discovery::MAX_JOURNAL_BYTES;

/// One process-wide inventory, shared by enrollment and every worker. Withdrawn
/// ports keep their rail reservation: neither outages nor GID changes renumber
/// surviving ports. Enrollment persists reservations across process restarts.
#[derive(Default)]
pub struct Inventory(rdma_verbs::discovery::Inventory);

/// Local discovery snapshot translated into Racer publication types.
#[derive(Clone, Default)]
pub struct Snapshot {
    pub generation: u64,

    pub nics: Vec<RailMapping>,
}

impl Inventory {
    /// Read the shared physical inventory using Racer's publication types.
    pub fn snapshot(&self) -> Result<Snapshot> {
        Ok(snapshot(self.0.snapshot()?))
    }

    /// Optional transport discovery failures withdraw RDMA, not HTTP service.
    pub fn refresh(&self) -> Result<Snapshot> {
        self.update(inventory())
    }

    /// Publish discovered ports while preserving their durable physical labels.
    pub fn update(&self, nics: Vec<RailMapping>) -> Result<Snapshot> {
        Ok(snapshot(self.0.update(nics.iter().map(binding).collect())?))
    }

    /// Keep the existing enrollment journal representation unchanged.
    pub fn reservations(&self) -> Result<Vec<u8>> {
        self.0.reservations().map_err(Into::into)
    }

    /// Restore before issuance, never discard a corrupt journal and renumber.
    pub fn restore(&self, bytes: &[u8]) -> Result<()> {
        self.0.restore(bytes).map_err(|error| match error {
            rdma_verbs::Error::InvalidRequest => Error::CorruptRecord,
            other => other.into(),
        })
    }
}

/// Erase application rail identity before passing physical bindings to verbs.
fn binding(nic: &RailMapping) -> Binding {
    Binding {
        rail: nic.rail.0,
        device: nic.device.clone(),
        port: nic.port,
        gid: nic.gid,
        numa_node: nic.numa_node,
    }
}

/// Restore application identity after physical discovery, without authorizing it.
fn mapping(nic: Binding) -> RailMapping {
    RailMapping {
        rail: RailId(nic.rail),
        device: nic.device,
        port: nic.port,
        gid: nic.gid,
        numa_node: nic.numa_node,
    }
}

/// Convert the physical snapshot at the application boundary.
fn snapshot(snapshot: rdma_verbs::discovery::Snapshot) -> Snapshot {
    Snapshot {
        generation: snapshot.generation,
        nics: snapshot.nics.into_iter().map(mapping).collect(),
    }
}

/// Discovery failures are optional-transport failures, never HTTP startup failures.
pub fn inventory() -> Vec<RailMapping> {
    inventory_at(
        rdma_verbs::inventory().unwrap_or_default(),
        Path::new("/sys/class/infiniband"),
    )
}

fn inventory_at(ports: Vec<PortInfo>, root: &Path) -> Vec<RailMapping> {
    rdma_verbs::discovery::inventory_at(ports, root)
        .into_iter()
        .take(64)
        .enumerate()
        .map(|(i, p)| RailMapping {
            device: p.device,
            port: p.port,
            rail: RailId(i as u16),
            gid: Some(p.gid),
            numa_node: p.numa_node,
        })
        .collect()
}

/// Select at most one device per rail. Explicit NUMA overrides detected locality;
/// prefer local, then unknown, then remote, spreading equal candidates by worker.
pub fn select_worker(
    published: &[RailMapping],
    discovered: &[RailMapping],
    worker: usize,
    numa: Option<usize>,
    capacity: usize,
) -> Vec<RailMapping> {
    rdma_verbs::discovery::select_worker(
        &published.iter().map(binding).collect::<Vec<_>>(),
        &discovered.iter().map(binding).collect::<Vec<_>>(),
        worker,
        numa,
        capacity,
    )
    .into_iter()
    .map(mapping)
    .collect()
}

// RDMA requires compatible authenticated mappings; discovery can only veto.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TransportPlan {
    Http,
    Rdma { rail: RailId },
}
/// Conservative route summary; actual transport admission uses select_hop.
pub fn select(route: &Route, page: &PageId) -> Result<TransportPlan> {
    validate_route(route)?;
    let members = route
        .nodes
        .iter()
        .map(|node| route.membership.member(node))
        .collect::<Result<Vec<_>>>()?;
    select_members(&route.membership, &members, page)
}
pub fn select_hop(
    route: &Route,
    page: &PageId,
    local: &racer_control_wire::NodeId,
    peer: &racer_control_wire::NodeId,
) -> Result<TransportPlan> {
    validate_route(route)?;
    if !route.nodes.windows(2).any(|pair| {
        (&pair[0] == local && &pair[1] == peer) || (&pair[1] == local && &pair[0] == peer)
    }) {
        return Err(Error::IncompatibleMembership);
    }
    select_members(
        &route.membership,
        &[
            route.membership.member(local)?,
            route.membership.member(peer)?,
        ],
        page,
    )
}
fn validate_route(route: &Route) -> Result<()> {
    if route.nodes.is_empty()
        || route.nodes.len() > usize::from(FAILURE_LINKS) + 1
        || route
            .nodes
            .iter()
            .enumerate()
            .any(|(i, node)| route.nodes[..i].contains(node))
    {
        return Err(Error::InvalidRequest);
    }
    for node in &route.nodes {
        route.membership.member(node)?;
    }
    Ok(())
}
fn select_members(
    membership: &crate::topology::Membership,
    members: &[&crate::topology::Member],
    page: &PageId,
) -> Result<TransportPlan> {
    if members
        .iter()
        .any(|m| m.site.is_empty() || m.site != members[0].site || m.rails.is_empty())
    {
        return Ok(TransportPlan::Http);
    }
    // Site is an admission boundary, not a new rail domain or hash scheme.
    let domain = membership.rail_domain();
    if domain.is_empty() {
        return Ok(TransportPlan::Http);
    }
    let mut digest = crate::topology::hash_domain(b"racer/rail/v2\0");
    crate::topology::hash_object(&mut digest, &page.version.object, page.number);
    let digest = crate::topology::hash_finish(digest);
    let sample = u64::from_be_bytes(digest[..8].try_into().unwrap());
    let rail = domain[(sample % domain.len() as u64) as usize];
    if !members
        .iter()
        .all(|m| m.rails.iter().any(|m| m.rail == rail))
    {
        return Ok(TransportPlan::Http);
    }
    Ok(TransportPlan::Rdma { rail })
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;

    pub(crate) mod session_tests {
        use super::*;
        #[test]
        fn real_signed_setup_rejects_tampering_and_replay() {
            use crate::peer::protocol::Signatures;
            use crate::peer::protocol::SignedHead;
            use crate::test_support::security::CLUSTER;
            use crate::test_support::security::NODE;
            use crate::test_support::security::issued;
            use http1::Header;
            use http1::MessageHead;
            use http1::StartLine;
            use racer_control_wire::BundleGeneration;
            use racer_control_wire::ClusterId;
            use racer_control_wire::KeyringBundle;
            use racer_control_wire::SCHEMA_VERSION;
            use racer_crypto::identity::Certificates;
            use racer_crypto::identity::KeyEpochs;
            use racer_crypto::identity::Keyring;
            use std::sync::Arc;
            let (pending, chain, roots) = issued();
            let cluster = ClusterId(CLUSTER.into());
            let node = NodeId(NODE.into());
            let identity = pending
                .accept(cluster.clone(), node.clone(), chain, &roots)
                .unwrap();
            let keys = Rc::new(Keyring::new(
                cluster.clone(),
                node.clone(),
                Arc::new(KeyEpochs::default()),
            ));
            keys.install(KeyringBundle {
                schema_version: SCHEMA_VERSION,
                cluster: cluster.clone(),
                generation: BundleGeneration(1),
                peer_trust_roots: roots,
                cache_keys: vec![],
            })
            .unwrap();
            keys.install_signing_identity(Arc::new(identity)).unwrap();
            let certificates = Rc::new(Certificates::new(cluster, keys.clone()));
            let signatures = Signatures::new(keys, certificates);
            let setup = SetupParameters::new(
                RailId(9),
                Endpoint {
                    gid: [1; 16],
                    qpn: 1,
                    psn: 2,
                    mtu: 3,
                    lid: 1,
                    port: 1,
                    link_layer: 1,
                },
            )
            .unwrap();
            let head = || MessageHead {
                start: StartLine::Request {
                    method: "POST".into(),
                    target: "/racer/peer/v1/rdma".into(),
                },
                headers: vec![
                    Header {
                        name: "racer-receiver".into(),
                        value: NODE.as_bytes().to_vec(),
                    },
                    Header {
                        name: SETUP_HEADER.into(),
                        value: setup.header_value(),
                    },
                    Header {
                        name: SETUP_BINDING_HEADER.into(),
                        value: setup.binding_header_value(),
                    },
                ],
            };
            let verified = signatures
                .verify_proof(signatures.sign(head()).unwrap())
                .unwrap();
            assert_eq!(
                SetupParameters::from_verified(&verified, RailId(9))
                    .unwrap()
                    .encoded,
                setup.encoded
            );
            assert!(SetupParameters::from_verified(&verified, RailId(8)).is_err());
            assert_eq!(
                signed_value(&verified, SETUP_BINDING_HEADER, 32).unwrap(),
                Sha256::digest(&setup.encoded).as_slice()
            );
            let replay = SignedHead {
                head: verified.signed.head,
                signature: verified.signed.signature,
            };
            signatures.verify_proof(replay).unwrap();
            let mut tampered = signatures.sign(head()).unwrap();
            tampered
                .head
                .headers
                .iter_mut()
                .find(|h| h.name == SETUP_HEADER)
                .unwrap()
                .value[0] = b'A';
            assert!(signatures.verify_proof(tampered).is_err());
        }
        #[test]
        fn setup_encoding_is_bounded_and_rail_bound() {
            let e = Endpoint {
                gid: [1; 16],
                qpn: 3,
                psn: 9,
                mtu: 3,
                lid: 2,
                port: 1,
                link_layer: 1,
            };
            let mut s = SetupParameters::new(RailId(7), e).unwrap();
            assert_eq!(s.endpoint().unwrap(), e);
            // Freeze the existing envelope layout independently of the codec.
            let prefix = b"racer-rdma-setup-v1\0";
            assert_eq!(s.encoded.len(), prefix.len() + 50);
            assert_eq!(&s.encoded[..prefix.len()], prefix);
            assert_eq!(&s.encoded[prefix.len()..prefix.len() + 2], &[0, 7]);
            assert_ne!(&s.encoded[prefix.len() + 2..prefix.len() + 18], &[0; 16]);
            assert_eq!(&s.encoded[prefix.len() + 18..prefix.len() + 34], &[1; 16]);
            assert_eq!(
                &s.encoded[prefix.len() + 34..],
                &[0, 0, 0, 3, 0, 0, 0, 9, 0, 0, 0, 3, 0, 2, 1, 1]
            );
            s.rail = RailId(8);
            assert_eq!(s.endpoint(), Err(Error::InvalidRequest));
            s.rail = RailId(7);
            s.encoded.push(0);
            assert!(s.endpoint().is_err());
        }
    }

    pub(crate) mod permission_tests {
        use super::*;
        #[test]
        fn descriptors_reject_overflow_wrong_session_and_trailing_bytes() {
            let d = RemoteDescriptor {
                transfer: TransferId([7; 16]),
                address: 4096,
                length: 17,
                scoped_key: 9,
            };
            let bytes = d.encode([2; 32]);
            assert_eq!(
                RemoteDescriptor::decode(&bytes, [2; 32]).unwrap().length,
                17
            );
            assert!(matches!(
                RemoteDescriptor::decode(&bytes, [3; 32]),
                Err(Error::Unauthorized)
            ));
            let mut extra = bytes;
            extra.push(0);
            assert!(RemoteDescriptor::decode(&extra, [2; 32]).is_err());
            let overflow = RemoteDescriptor {
                address: u64::MAX,
                ..d
            };
            assert!(matches!(
                RemoteDescriptor::decode(&overflow.encode([2; 32]), [2; 32]),
                Err(Error::InvalidRange)
            ));
            for length in [0, u64::MAX] {
                let invalid = RemoteDescriptor { length, ..d };
                assert!(RemoteDescriptor::decode(&invalid.encode([2; 32]), [2; 32]).is_err());
            }
        }
    }

    pub(crate) mod registered_tests {
        use super::*;
        #[test]
        fn native_page_policy_preserves_unavailable_for_unsupported_hosts() {
            assert_eq!(validate_native_page_size(4096), Ok(()));
            for page_size in [-1, 0, 1024, 8192, 65536] {
                assert_eq!(
                    validate_native_page_size(page_size),
                    Err(Error::Unavailable)
                );
            }
        }
        #[test]
        fn registered_quota_accounts_for_short_and_final_physical_pages() {
            assert_eq!(registered_charge(1), Ok(4096));
            assert_eq!(registered_charge(4096), Ok(4096));
            assert_eq!(registered_charge(4097), Ok(8192));
            assert_eq!(
                registered_charge(MAX_CIPHERTEXT),
                Ok(16 * 1024 * 1024 + 4096)
            );
            assert_eq!(registered_charge(0), Err(Error::InvalidRange));
            assert_eq!(registered_charge(usize::MAX), Err(Error::InvalidRange));
        }
        #[test]
        fn native_slot_quota_includes_aligned_staging_and_registration() {
            for (length, expected) in [
                (1, 8192),
                (4096, 8192),
                (4097, 16384),
                (MAX_CIPHERTEXT, 32 * 1024 * 1024 + 8192),
            ] {
                assert_eq!(native_slot_charge(length), Ok(expected));
            }
            for length in [0, MAX_CIPHERTEXT + 1, usize::MAX] {
                assert_eq!(native_slot_charge(length), Err(Error::InvalidRange));
            }
        }
    }

    pub(crate) mod publication_tests {
        use super::*;
        #[test]
        fn unconfigured_rails_require_http() {
            let devices = Devices::new();
            assert!(!devices.ready(RailId(0)));
            assert!(devices.select(RailId(0)).is_err());
        }
        #[test]
        fn discovery_matches_physical_ports_and_accepts_numa_override() {
            let publication = vec![RailMapping {
                rail: RailId(7),
                device: "mlx5_0".into(),
                port: 1,
                gid: Some([1; 16]),
                numa_node: Some(1),
            }];
            let port = PortInfo {
                device: "mlx5_0".into(),
                port: 1,
                gid: [1; 16],
                numa_node: Some(1),
            };
            assert!(match_publication(&publication, &[]).is_err());
            assert!(
                match_publication(
                    &[publication[0].clone(), publication[0].clone()],
                    std::slice::from_ref(&port)
                )
                .is_err()
            );
            assert!(match_publication(&publication, &[port.clone(), port.clone()]).is_err());
            assert!(
                match_publication(
                    &publication,
                    &[PortInfo {
                        numa_node: Some(0),
                        ..port.clone()
                    }]
                )
                .is_ok()
            );
            assert_eq!(
                match_publication(&publication, &[port]).unwrap()[0].0,
                publication[0]
            );
        }
    }

    pub(crate) mod discovery_tests {
        use super::*;
        #[test]
        fn journal_byte_admission_preserves_known_ports_and_restart_at_saturation() {
            // Exercise both maximal ordinary names and JSON-escaped local names.
            for fill in ['x', '"'] {
                let inventory = Inventory::default();
                let nic = |i| RailMapping {
                    device: format!("{i:04}{}", fill.to_string().repeat(59)),
                    port: 255,
                    rail: RailId(0),
                    gid: Some([1; 16]),
                    numa_node: None,
                };
                let mut accepted = 0;
                for batch in 0..20 {
                    let snapshot = inventory
                        .update((batch * 64..(batch + 1) * 64).map(nic).collect())
                        .unwrap();
                    accepted += snapshot.nics.len();
                    assert!(inventory.reservations().unwrap().len() <= MAX_JOURNAL_BYTES);
                }
                assert!(
                    accepted > 0 && accepted < 1024,
                    "byte bound must precede identity cap"
                );
                let journal = inventory.reservations().unwrap();
                assert!(inventory.update(vec![nic(2000)]).unwrap().nics.is_empty());
                assert_eq!(inventory.reservations().unwrap(), journal);
                let restarted = Inventory::default();
                restarted.restore(&journal).unwrap();
                let mut old = nic(0);
                old.gid = Some([2; 16]);
                let snapshot = restarted.update(vec![nic(2000), old]).unwrap();
                assert_eq!(snapshot.nics.len(), 1);
                assert_eq!(snapshot.nics[0].rail, RailId(0));
                assert_eq!(snapshot.nics[0].gid, Some([2; 16]));
                assert_eq!(restarted.reservations().unwrap(), journal);
            }
        }
        #[test]
        fn journal_restore_validates_names_ports_and_encoded_bounds_atomically() {
            let inventory = Inventory::default();
            inventory.restore(br#"[["valid",1,0]]"#).unwrap();
            let original = inventory.reservations().unwrap();
            for device in [
                "".to_owned(),
                ".".into(),
                "..".into(),
                "a/b".into(),
                "a\0b".into(),
                "a\nb".into(),
                "a\rb".into(),
                "x".repeat(64),
            ] {
                let bytes = serde_json::to_vec(&vec![(device.clone(), 1u8, 0u16)]).unwrap();
                assert_eq!(inventory.restore(&bytes), Err(Error::CorruptRecord));
                assert!(
                    inventory
                        .update(vec![RailMapping {
                            device,
                            port: 1,
                            rail: RailId(0),
                            gid: None,
                            numa_node: None
                        }])
                        .unwrap()
                        .nics
                        .is_empty()
                );
                assert_eq!(inventory.reservations().unwrap(), original);
            }
            assert_eq!(
                inventory.restore(br#"[["valid",0,0]]"#),
                Err(Error::CorruptRecord)
            );
            let oversized = serde_json::to_vec(
                &(0..1024)
                    .map(|i| (format!("{i:04}{}", "x".repeat(59)), 255u8, i as u16))
                    .collect::<Vec<_>>(),
            )
            .unwrap();
            assert!(oversized.len() > MAX_JOURNAL_BYTES);
            assert_eq!(inventory.restore(&oversized), Err(Error::CorruptRecord));
            let mut exact = original.clone();
            exact.resize(MAX_JOURNAL_BYTES, b' ');
            inventory.restore(&exact).unwrap();
            exact.push(b' ');
            assert_eq!(inventory.restore(&exact), Err(Error::CorruptRecord));
            assert_eq!(inventory.reservations().unwrap(), original);
        }
        #[test]
        fn reservations_survive_withdrawal_gid_change_hotplug_and_restart() {
            let nic = |device: &str, gid| RailMapping {
                device: device.into(),
                port: 1,
                rail: RailId(0),
                gid: Some([gid; 16]),
                numa_node: None,
            };
            let inventory = Inventory::default();
            assert!(inventory.update(vec![]).unwrap().nics.is_empty());
            let first = inventory.update(vec![nic("a", 1), nic("b", 2)]).unwrap();
            assert_eq!(first.nics[1].rail, RailId(1));
            let withdrawn = inventory.update(vec![nic("b", 3)]).unwrap();
            assert_eq!(withdrawn.nics[0].rail, RailId(1));
            assert!(withdrawn.generation > first.generation);
            let journal = inventory.reservations().unwrap();
            let restarted = Inventory::default();
            restarted.restore(&journal).unwrap();
            let next = restarted.update(vec![nic("b", 3), nic("c", 4)]).unwrap();
            assert_eq!(
                next.nics.iter().map(|n| n.rail).collect::<Vec<_>>(),
                vec![RailId(1), RailId(2)]
            );
            assert_eq!(
                restarted.update(vec![nic("a", 5)]).unwrap().nics[0].rail,
                RailId(0)
            );
            assert!(
                restarted
                    .update(vec![nic("a", 5), nic("a", 5)])
                    .unwrap()
                    .nics
                    .is_empty()
            );
            assert!(restarted.restore(b"bad").is_err());
            assert!(restarted.restore(br#"[["a",1,0],["b",1,0]]"#).is_err());
        }
        #[test]
        fn sysfs_pci_order_precedes_device_names_and_ports_have_ordinal_rails() {
            use std::os::unix::fs::symlink;
            struct Scratch(std::path::PathBuf);
            impl Drop for Scratch {
                fn drop(&mut self) {
                    std::fs::remove_dir_all(&self.0).unwrap();
                }
            }
            let dir = Scratch(
                Path::new(env!("CARGO_MANIFEST_DIR"))
                    .join("target")
                    .join(format!("nic-sysfs-{}", std::process::id())),
            );
            std::fs::create_dir_all(&dir.0).unwrap();
            for (name, bdf, numa) in [
                ("z", "0000:01:00.0", "2\n"),
                ("a", "0000:02:00.0", "-1\n"),
                ("b", "0000:03:00.0", "bad"),
            ] {
                std::fs::create_dir_all(dir.0.join(bdf)).unwrap();
                std::fs::create_dir_all(dir.0.join(name)).unwrap();
                std::fs::write(dir.0.join(bdf).join("numa_node"), numa).unwrap();
                symlink(dir.0.join(bdf), dir.0.join(name).join("device")).unwrap();
            }
            let port = |name: &str, port| PortInfo {
                device: name.into(),
                port,
                gid: [1; 16],
                numa_node: None,
            };
            let nics = inventory_at(
                vec![port("a", 1), port("z", 2), port("b", 1), port("z", 1)],
                &dir.0,
            );
            assert_eq!(
                nics.iter()
                    .map(|n| (n.device.as_str(), n.port, n.rail.0, n.numa_node))
                    .collect::<Vec<_>>(),
                vec![
                    ("z", 1, 0, Some(2)),
                    ("z", 2, 1, Some(2)),
                    ("a", 1, 2, None),
                    ("b", 1, 3, None)
                ]
            );
        }
        #[test]
        fn inventory_absence_and_unknown_numa_are_safe() {
            let root = Path::new("/nonexistent-racer-infiniband");
            assert!(inventory_at(vec![], root).is_empty());
            let port = |device: &str, port| PortInfo {
                device: device.into(),
                port,
                gid: [1; 16],
                numa_node: Some(99),
            };
            let nics = inventory_at(vec![port("z", 2), port("a", 1), port("../bad", 1)], root);
            assert_eq!(nics.len(), 2);
            assert_eq!(nics[0].device, "a");
            assert_eq!(nics[1].rail, RailId(1));
            assert!(nics.iter().all(|n| n.numa_node.is_none()));
        }
        #[test]
        fn worker_local_unknown_remote_override_and_same_rail_spreading() {
            let nic = |device: &str, numa| RailMapping {
                device: device.into(),
                port: 1,
                rail: RailId(7),
                gid: Some([1; 16]),
                numa_node: numa,
            };
            let detected = vec![
                nic("a", Some(0)),
                nic("b", Some(0)),
                nic("c", None),
                nic("d", Some(2)),
            ];
            let selected = |worker, numa| select_worker(&detected, &detected, worker, numa, 64);
            assert_eq!(selected(0, Some(0))[0].device, "a");
            assert_eq!(selected(1, Some(0))[0].device, "b");
            assert_eq!(selected(0, Some(1))[0].device, "c");
            assert_eq!(selected(0, Some(2))[0].device, "d");
            let override_nic = nic("a", Some(3));
            assert_eq!(
                select_worker(&[override_nic], &detected, 0, Some(3), 1)[0].numa_node,
                Some(3)
            );
            assert_eq!(
                select_worker(&detected[..2], &detected, 0, Some(9), 1).len(),
                1
            );
            assert!(select_worker(&detected, &[], 0, None, 64).is_empty());
            assert!(select_worker(&[], &detected, 0, None, 64).is_empty());
            assert!(select_worker(&detected, &detected, 0, None, 0).is_empty());
        }
    }

    pub(crate) mod rails_tests {
        use super::*;
        use crate::model::*;
        use crate::topology::Membership;
        use crate::topology::tests::fixtures::member;
        use crate::topology::tests::fixtures::object;
        use std::sync::Arc;
        fn mappings() -> Vec<RailMapping> {
            vec![
                RailMapping {
                    rail: RailId(7),
                    device: "a".into(),
                    port: 1,
                    gid: None,
                    numa_node: Some(0),
                },
                RailMapping {
                    rail: RailId(2),
                    device: "b".into(),
                    port: 1,
                    gid: None,
                    numa_node: Some(1),
                },
            ]
        }
        fn page(number: u64) -> PageId {
            PageId {
                version: ObjectVersion {
                    object: object(),
                    etag: StrongEtag::test_value("\"v1\""),
                },
                number: PageNumber(number),
            }
        }
        // Fixture-side hardware veto for summary tests. Real sessions additionally
        // require device activation; this helper never grants transport admission.
        fn local_compatible(
            route: &Route,
            plan: &TransportPlan,
            local: &NodeId,
            discovered: &[RailMapping],
        ) -> Result<bool> {
            if !route.nodes.contains(local) {
                return Err(Error::IncompatibleMembership);
            }
            let member = route.membership.member(local)?;
            let TransportPlan::Rdma { rail } = plan else {
                return Ok(true);
            };
            let Some(published) = member.rails.iter().find(|m| m.rail == *rail) else {
                return Ok(false);
            };
            let mut matching = discovered.iter().filter(|m| m.rail == *rail);
            Ok(matching.next().is_some_and(|hardware| {
                hardware.device == published.device
                    && hardware.port == published.port
                    && published
                        .numa_node
                        .is_none_or(|numa| hardware.numa_node == Some(numa))
            }) && matching.next().is_none())
        }
        fn select_with_local(
            route: &Route,
            page: &PageId,
            local: &NodeId,
            discovered: &[RailMapping],
        ) -> Result<TransportPlan> {
            let plan = select(route, page)?;
            Ok(if local_compatible(route, &plan, local, discovered)? {
                plan
            } else {
                TransportPlan::Http
            })
        }
        fn route(change: impl FnOnce(&mut Vec<crate::topology::Member>)) -> Route {
            let mut members: Vec<_> = (0..3)
                .map(|i| {
                    let mut member = member(i, 4);
                    member.site = "site1".into();
                    member.rails = mappings();
                    member
                })
                .collect();
            change(&mut members);
            let membership = Arc::new(Membership::validate(MembershipVersion(1), members).unwrap());
            Route {
                nodes: membership
                    .members()
                    .iter()
                    .map(|m| m.node.clone())
                    .collect(),
                membership,
            }
        }
        #[test]
        fn mixed_site_hops_preserve_global_rail_mapping_and_hardware_vetoes() {
            let mixed = route(|m| m[2].site = "site2".into());
            let a = &mixed.nodes[0];
            let b = &mixed.nodes[1];
            let c = &mixed.nodes[2];
            assert_eq!(select(&mixed, &page(0)).unwrap(), TransportPlan::Http);
            for (from, to, expected) in [
                (a, b, TransportPlan::Rdma { rail: RailId(2) }),
                (b, a, TransportPlan::Rdma { rail: RailId(2) }),
                (b, c, TransportPlan::Http),
                (c, b, TransportPlan::Http),
            ] {
                assert_eq!(select_hop(&mixed, &page(0), from, to).unwrap(), expected);
            }
            for other in [c, a, &NodeId("unknown".into())] {
                assert!(select_hop(&mixed, &page(0), a, other).is_err());
            }
            for local in [0, 1] {
                let missing = route(|m| m[local].site.clear());
                assert_eq!(
                    select_hop(&missing, &page(0), a, b).unwrap(),
                    TransportPlan::Http
                );
            }
            for changed in [
                route(|m| m[1].site.clear()),
                route(|m| m[1].rails.clear()),
                route(|m| {
                    m[1].rails
                        .iter_mut()
                        .for_each(|r| r.rail = RailId(r.rail.0 + 1))
                }),
            ] {
                assert_eq!(
                    select_hop(&changed, &page(0), a, b).unwrap(),
                    TransportPlan::Http
                );
            }
            let plan = select_hop(&mixed, &page(0), a, b).unwrap();
            assert!(local_compatible(&mixed, &plan, a, &mappings()).unwrap());
            assert!(!local_compatible(&mixed, &plan, a, &[]).unwrap());
        }
        #[test]
        fn golden_page_to_rail_vectors() {
            let route = route(|_| {});
            for (number, rail) in [(0, 2), (1, 7), (u64::MAX, 2)] {
                assert_eq!(
                    select(&route, &page(number)).unwrap(),
                    TransportPlan::Rdma { rail: RailId(rail) }
                );
            }
        }
        #[test]
        fn repeated_rails_and_different_physical_names_do_not_change_remote_eligibility() {
            let original = route(|_| {});
            let repeated = route(|members| {
                for (i, member) in members.iter_mut().enumerate() {
                    let mut extra = member.rails[0].clone();
                    extra.device = format!("extra-{i}");
                    member.rails.push(extra);
                    member.rails[0].device = format!("local-{i}");
                }
            });
            assert_eq!(
                original.membership.rail_domain(),
                repeated.membership.rail_domain()
            );
            for number in 0..100 {
                assert_eq!(
                    select(&original, &page(number)),
                    select(&repeated, &page(number))
                );
            }
        }
        #[test]
        fn intersection_over_all_hops_and_http_fallback() {
            for route in [
                route(|m| m[1].site.clear()),
                route(|m| m[1].rails.clear()),
                route(|m| {
                    for rail in &mut m[1].rails {
                        rail.rail = RailId(rail.rail.0 + 1);
                    }
                }),
            ] {
                assert_eq!(select(&route, &page(0)).unwrap(), TransportPlan::Http);
            }
            let full = route(|_| {});
            let partial = route(|m| m[1].rails.retain(|rail| rail.rail == RailId(7)));
            for number in 0..100 {
                let expected = match select(&full, &page(number)).unwrap() {
                    TransportPlan::Rdma { rail: RailId(7) } => {
                        TransportPlan::Rdma { rail: RailId(7) }
                    }
                    _ => TransportPlan::Http,
                };
                assert_eq!(select(&partial, &page(number)).unwrap(), expected);
                let mut alternate = partial.clone();
                alternate.nodes.remove(1);
                assert_eq!(
                    select(&alternate, &page(number)).unwrap(),
                    select(&full, &page(number)).unwrap()
                );
                let mut version = page(number);
                version.version.etag = StrongEtag::test_value("\"v2\"");
                assert_eq!(
                    select(&full, &version).unwrap(),
                    select(&full, &page(number)).unwrap()
                );
            }
        }
        #[test]
        fn deterministic_reverse_path_order_and_local_hardware() {
            let route = route(|m| {
                m[1].rails.reverse();
                for rail in &mut m[1].rails {
                    rail.numa_node = Some(99);
                }
            });
            let mut reverse = route.clone();
            reverse.nodes.reverse();
            let mut selected = std::collections::BTreeSet::new();
            for number in 0..100 {
                let plan = select(&route, &page(number)).unwrap();
                assert_eq!(plan, select(&reverse, &page(number)).unwrap());
                let TransportPlan::Rdma { rail } = plan else {
                    panic!("expected RDMA");
                };
                selected.insert(rail);
                for (local, hardware, expected) in [
                    (0, mappings(), plan),
                    (1, mappings(), TransportPlan::Http),
                    (0, vec![], TransportPlan::Http),
                ] {
                    assert_eq!(
                        select_with_local(&route, &page(number), &route.nodes[local], &hardware)
                            .unwrap(),
                        expected
                    );
                }
            }
            assert_eq!(selected.len(), 2);
            let mut invalid = route.clone();
            invalid.nodes.push(invalid.nodes[0].clone());
            assert_eq!(select(&invalid, &page(0)), Err(Error::InvalidRequest));
        }
    }

    pub(crate) mod scenarios {
        use super::*;
        use rdma_verbs::pair;
        use rdma_verbs::simulation;
        mod mailbox {
            //! Public handoffs held at deterministic native mailbox boundaries.
            use super::*;
            use crate::admission::AdmissionPolicy;
            use crate::model::*;
            use crate::test_support::security::network;
            use rdma_verbs::testing::Contention;
            use rdma_verbs::testing::State;
            use std::time::Duration;

            #[test]
            fn sessions_admit_64_neighbors_but_keep_per_neighbor_and_total_bounds() {
                let signers = network(2);
                let peer = verified(&signers, vec![]);
                let (_, io, _native, _) = fixture(2);
                let devices = Rc::new(Devices::test(io));
                let qp = immediate(QueuePairHandle::poll_new(
                    devices.select(RailId(0)).unwrap().handle,
                    None,
                ))
                .unwrap();
                let sessions = Sessions::new(devices, 1);
                for i in 0..63 {
                    sessions.track_peer_test(NodeId(format!("peer-{i}")), qp.clone());
                }
                let scope = scope();
                let prepared = done(&mut sessions.prepare(&peer.peer, RailId(0), &scope));
                assert!(matches!(
                    poll(&mut sessions.prepare(&peer.peer, RailId(0), &scope)),
                    Poll::Ready(Err(Error::Overloaded))
                ));
                drop(prepared);
                sessions.track_peer_test(NodeId("peer-63".into()), qp);
                assert!(matches!(
                    poll(&mut sessions.prepare(&peer.peer, RailId(0), &scope)),
                    Poll::Ready(Err(Error::Overloaded))
                ));
            }

            #[test]
            fn signed_setup_waits_for_slot_and_connect_mailboxes_without_consuming_admission() {
                let signers = network(2);
                let peer = verified(&signers, vec![]);
                let (_, io, mut native, _) = fixture(2);
                let sessions = Sessions::new(Rc::new(Devices::test(io.clone())), 2);
                let scope = scope();
                let mut prepare = sessions.prepare(&peer.peer, RailId(0), &scope);
                io.with_contention(Contention::Slot(0), || {
                    io.with_contention(Contention::Slot(1), || {
                        assert!(poll(&mut prepare).is_pending());
                        assert_eq!(io.snapshot(0).state, State::Ready);
                    })
                });
                let prepared = done(&mut prepare);
                drop(prepare);
                let remote = done(&mut sessions.prepare(&peer.peer, RailId(0), &scope));
                assert!(matches!(
                    QueuePairHandle::poll_new(io.device(0), None),
                    Poll::Ready(Err(rdma_verbs::Error::Overloaded))
                ));
                assert!(matches!(
                    poll(&mut sessions.prepare(&peer.peer, RailId(0), &scope)),
                    Poll::Ready(Err(Error::Overloaded))
                ));
                let ack = verified(
                    &signers,
                    vec![
                        header(SETUP_HEADER, remote.setup().header_value()),
                        header(
                            SETUP_BINDING_HEADER,
                            prepared.setup().binding_header_value(),
                        ),
                    ],
                );
                let mut finish = prepared.finish(&ack, &scope);
                io.with_contention(Contention::Slot(0), || {
                    assert!(poll(&mut finish).is_pending());
                    assert!(!io.snapshot(0).cancelled);
                });
                let session = done(&mut finish);
                drop(finish);
                let mut ready = session.wait_ready(&scope);
                assert!(poll(&mut ready).is_pending());
                native.poll_budgeted(2).unwrap();
                io.with_contention(Contention::Slot(0), || {
                    assert!(poll(&mut ready).is_pending())
                });
                done(&mut ready);
                assert!(session.ready());
            }

            #[test]
            fn receive_preparation_and_sender_wait_at_every_buffer_and_command_boundary() {
                sender_case(None);
            }
            #[test]
            fn successful_write_cancel_and_expiry_leave_failed_terminal_fence_quarantined() {
                for error in [Error::Cancelled, Error::DeadlineExceeded] {
                    sender_case(Some(error));
                }
            }
            fn sender_case(terminal: Option<Error>) {
                let clock = environment::SimulationClock::new(62);
                let _time = clock.environment(0).enter();
                let signers = network(2);
                let (sim, io, mut native, charges) = fixture(2);
                let charged = &charges[1];
                let receiver = claim(&io);
                let sender = claim(&io);
                connect_pair(&receiver, &sender, &mut native);
                let receive = SessionLease::test(receiver.clone(), signers[0].node().clone());
                let send = SessionLease::test(sender.clone(), signers[0].node().clone());
                let devices = Rc::new(Devices::test(io.clone()));
                let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(true).limits,
                )));
                let transfer = Sessions::new(devices, 2);
                let mut scope = scope();
                if terminal == Some(Error::DeadlineExceeded) {
                    scope.deadline.0 = environment::now() + Duration::from_secs(1);
                }
                let envelope = envelope();
                let id = TransferId([9; 16]);
                let mut prepare = transfer.prepare_receive(&receive, &envelope, id, &scope);
                io.with_contention(Contention::Slot(0), || {
                    assert!(poll(&mut prepare).is_pending())
                });
                let grant = done(&mut prepare);
                drop(prepare);
                native.poll_budgeted(2).unwrap();
                native.poll_budgeted(2).unwrap();
                done(&mut grant.wait_bound(&scope));
                let signed = verified(
                    &signers,
                    vec![header(DESCRIPTOR_HEADER, grant.header_value().unwrap())],
                );
                let descriptor =
                    AuthenticatedDescriptor::from_verified(&signed, &send, id).unwrap();
                let page = BufferPool::new(admission.clone())
                    .ciphertext(
                        admission
                            .reserve(
                                Some(&envelope.page.version.object.cache),
                                ResourceClass::Ciphertext,
                                32,
                            )
                            .unwrap(),
                        envelope,
                        vec![0xa5; 32],
                    )
                    .unwrap();
                let mut sending = transfer.send_to(&send, page, descriptor, &scope);
                io.with_contention(Contention::Slot(1), || {
                    assert!(poll(&mut sending).is_pending());
                    assert_eq!(admission.used(ResourceClass::Ciphertext), 32);
                });
                assert!(poll(&mut sending).is_pending());
                native.poll_budgeted(2).unwrap();
                native.poll_budgeted(2).unwrap();
                assert!(poll(&mut sending).is_pending());
                if let Some(error) = terminal {
                    let qpn = sender.endpoint.qpn;
                    sim.reject(simulation::Operation::Stop, Some(qpn), true);
                    native.poll_budgeted(2).unwrap();
                    assert!(!sender.stopped());
                    assert!(poll(&mut sending).is_pending());
                    if error == Error::Cancelled {
                        scope.cancel().unwrap();
                    } else {
                        clock.advance(Duration::from_secs(1));
                    }
                    assert!(matches!(poll(&mut sending), Poll::Ready(Err(e)) if e == error));
                    drop(sending);
                    assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
                    drop((send, sender));
                    native.retry_now(1);
                    native.poll_budgeted(2).unwrap();
                    assert_eq!(charged.get(), 1);
                    assert_eq!(io.snapshot(1).state, State::Owned);
                    assert!(!io.snapshot(1).fenced);
                    sim.reject(simulation::Operation::Stop, Some(qpn), false);
                    native.retry_now(1);
                    native.poll_budgeted(2).unwrap();
                    assert_eq!(io.snapshot(1).state, State::Ready);
                    drop((grant, receive, receiver));
                    native.close();
                    native.poll_budgeted(2).unwrap();
                    assert!(native.drained());
                    assert_eq!(charged.get(), 0);
                    return;
                }
                native.poll_budgeted(2).unwrap();
                done(&mut sending);
                drop(sending);
                assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
                drop(grant);
                native.poll_budgeted(2).unwrap();
                drop((receive, send, receiver, sender));
                native.poll_budgeted(2).unwrap();
                let qp = claim(&io);
                mark_connected(&qp, &mut native);
                let session = SessionLease::test(qp.clone(), signers[0].node().clone());
                let mut buffer = done(&mut RegisteredLease::acquire(&session, 32, &scope));
                let mut copy = buffer.copy_from(&[0x5a; 32], &scope);
                io.with_contention(Contention::Slot(0), || {
                    assert!(poll(&mut copy).is_pending())
                });
                done(&mut copy);
                drop(copy);
                let mut bind = Grant::bind(&session, buffer, id, &scope);
                io.with_contention(Contention::Slot(0), || {
                    assert!(poll(&mut bind).is_pending());
                    assert!(!io.snapshot(0).cancelled);
                });
                let grant = done(&mut bind);
                drop(bind);
                native.poll_budgeted(2).unwrap();
                native.poll_budgeted(2).unwrap();
                done(&mut grant.wait_bound(&scope));
                io.with_contention(Contention::Slot(0), || {
                    assert!(grant.header_value().is_ok(), "bound descriptor is cached")
                });
            }

            #[test]
            fn contended_grant_cancel_expiry_and_drop_abort_without_submitting_bind() {
                for mode in 0..3 {
                    let clock = environment::SimulationClock::new(63);
                    let _time = clock.environment(0).enter();
                    let (_, io, mut native, charges) = fixture(1);
                    let charged = &charges[0];
                    let qp = claim(&io);
                    mark_connected(&qp, &mut native);
                    let session = SessionLease::test(qp.clone(), NodeId("peer".into()));
                    let mut scope = scope();
                    assert!(matches!(
                        poll(&mut RegisteredLease::acquire(&session, 33, &scope)),
                        Poll::Ready(Err(Error::Overloaded))
                    ));
                    let buffer = done(&mut RegisteredLease::acquire(&session, 32, &scope));
                    if mode == 1 {
                        scope.deadline.0 = environment::now() + Duration::from_secs(1);
                    }
                    let mut bind = Grant::bind(&session, buffer, TransferId([1; 16]), &scope);
                    io.with_contention(Contention::Slot(0), || {
                        assert!(poll(&mut bind).is_pending());
                        match mode {
                            0 => {
                                scope.cancel().unwrap();
                                assert!(matches!(
                                    poll(&mut bind),
                                    Poll::Ready(Err(Error::Cancelled))
                                ));
                            }
                            1 => {
                                clock.advance(Duration::from_secs(1));
                                assert!(matches!(
                                    poll(&mut bind),
                                    Poll::Ready(Err(Error::DeadlineExceeded))
                                ));
                            }
                            _ => {}
                        }
                        drop(bind);
                        assert!(io.snapshot(0).cancelled);
                        assert!(!qp.stopped());
                        assert_eq!(charged.get(), 1);
                    });
                    assert!(!io.command_pending(0));
                    native.poll_budgeted(1).unwrap();
                    assert!(qp.stopped());
                    drop((session, qp));
                    native.poll_budgeted(1).unwrap();
                    assert_eq!(io.snapshot(0).state, State::Ready);
                    native.close();
                    native.poll_budgeted(1).unwrap();
                    assert_eq!(charged.get(), 0);
                }
            }

            #[test]
            fn canceled_or_abandoned_contended_signed_setup_releases_only_after_fence() {
                let signers = network(2);
                let peer = verified(&signers, vec![]);
                for cancel in [false, true] {
                    let (_, io, mut native, _) = fixture(2);
                    let sessions = Sessions::new(Rc::new(Devices::test(io.clone())), 2);
                    let scope = scope();
                    let prepared = done(&mut sessions.prepare(&peer.peer, RailId(0), &scope));
                    let remote = done(&mut sessions.prepare(&peer.peer, RailId(0), &scope));
                    let ack = verified(
                        &signers,
                        vec![
                            header(SETUP_HEADER, remote.setup().header_value()),
                            header(
                                SETUP_BINDING_HEADER,
                                prepared.setup().binding_header_value(),
                            ),
                        ],
                    );
                    let mut finish = prepared.finish(&ack, &scope);
                    io.with_contention(Contention::Slot(0), || {
                        assert!(poll(&mut finish).is_pending());
                        if cancel {
                            scope.cancel().unwrap();
                            assert!(matches!(
                                poll(&mut finish),
                                Poll::Ready(Err(Error::Cancelled))
                            ));
                        }
                        drop(finish);
                        assert!(io.snapshot(0).cancelled);
                        assert!(!io.snapshot(0).fenced);
                    });
                    assert!(!io.command_pending(0));
                    native.poll_budgeted(2).unwrap();
                    sessions.progress().unwrap();
                    native.poll_budgeted(2).unwrap();
                    assert_eq!(io.snapshot(0).state, State::Ready);
                }
            }

            #[test]
            fn activation_contention_retains_quota_and_cancellation_releases_unsubmitted_configuration()
             {
                for activation_lock in [false, true] {
                    let (io, port) = pair(1).unwrap();
                    let mut native = NativeService::new(port);
                    let admission = flow_control::Quotas::new(AdmissionPolicy::new(
                        crate::test_support::cluster::config(true).limits,
                    ));
                    let scope = scope();
                    let plan = || rdma_verbs::Configuration {
                        discover: false,
                        bytes: 4096,
                        selector: Box::new(|_| Ok(vec![])),
                        guards: vec![std::sync::Arc::new(
                            admission
                                .reserve(None, ResourceClass::Registered, 8192)
                                .unwrap(),
                        )],
                    };
                    let configure = io.configure(plan());
                    let mut configure: Operation<'_, ()> = Box::pin(async {
                        let mut configure = std::pin::pin!(configure);
                        poll_scoped(&scope, |cx| {
                            std::future::Future::poll(configure.as_mut(), cx)
                        })
                        .await
                    });
                    io.with_contention(
                        if activation_lock {
                            Contention::Activation
                        } else {
                            Contention::Configuration
                        },
                        || {
                            assert!(poll(&mut configure).is_pending());
                            assert_eq!(admission.used(ResourceClass::Registered), 8192);
                            scope.cancel().unwrap();
                            assert!(matches!(
                                poll(&mut configure),
                                Poll::Ready(Err(Error::Cancelled))
                            ));
                            drop(configure);
                            assert_eq!(admission.used(ResourceClass::Registered), 0);
                            assert!(!io.configuration_submitted());
                        },
                    );
                    futures::executor::block_on(io.configure(plan())).unwrap();
                    native.poll_budgeted(1).unwrap();
                    assert_eq!(admission.used(ResourceClass::Registered), 0);
                }
            }
            use uring_runtime::environment;

            #[test]
            fn receive_completion_waits_for_invalidation_mailbox() {
                receive_contended(false, None);
            }
            #[test]
            fn receive_completion_waits_for_fenced_readback_mailbox() {
                receive_contended(true, None);
            }
            #[test]
            fn receive_completion_contention_obeys_cancellation_and_deadline() {
                for readback in [false, true] {
                    for error in [Error::Cancelled, Error::DeadlineExceeded] {
                        receive_contended(readback, Some(error));
                    }
                }
            }
            #[test]
            fn receive_completion_does_not_retry_ciphertext_quota_exhaustion() {
                receive_contended(false, Some(Error::Overloaded));
            }
            fn receive_contended(readback: bool, terminal: Option<Error>) {
                receive_case(readback, terminal, false);
            }
            #[test]
            fn successful_invalidation_cancel_and_expiry_leave_failed_terminal_fence_quarantined() {
                for error in [Error::Cancelled, Error::DeadlineExceeded] {
                    receive_case(true, Some(error), true);
                }
            }

            fn receive_case(readback: bool, terminal: Option<Error>, failed_fence: bool) {
                let clock = environment::SimulationClock::new(61);
                let _time = clock.environment(0).enter();
                let (sim, io, mut native, charges) = fixture(2);
                let charged = &charges[0];
                let peer_metrics = crate::telemetry::Metrics::default();
                let peer_admission = crate::peer::AdaptivePeers::new(
                    crate::peer::Config {
                        total: 1,
                        per_peer: 1,
                    },
                    peer_metrics.clone(),
                )
                .unwrap();
                let peer = racer_control_wire::NodeId("native-peer".into());
                let permit = peer_admission.acquire(&peer).unwrap();
                let receive_quota = flow_control::Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(true).limits,
                ));
                let receive_gate = crate::peer::receive::Gate::with_metrics(
                    crate::peer::receive::Config {
                        active: 1,
                        ..Default::default()
                    },
                    peer_metrics.clone(),
                )
                .unwrap();
                let receive =
                    futures::executor::block_on(receive_gate.acquire(&receive_quota, &scope()))
                        .unwrap()
                        .unwrap();
                let qp = immediate(QueuePairHandle::poll_new(
                    io.device(0),
                    Some(std::sync::Arc::new((permit, receive))),
                ))
                .unwrap();
                let writer = claim(&io);
                connect_pair(&qp, &writer, &mut native);
                let signers = network(2);
                let session = SessionLease::test(qp.clone(), signers[0].node().clone());
                let devices = Rc::new(Devices::new());
                let transfer = Sessions::new(devices, 1);
                let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(true).limits,
                )));
                let envelope = envelope();
                let mut scope = scope();
                let id = TransferId([4; 16]);
                let grant = futures::executor::block_on(
                    transfer.prepare_receive(&session, &envelope, id, &scope),
                )
                .unwrap();
                native.poll_budgeted(2).unwrap();
                native.poll_budgeted(2).unwrap();
                let descriptor = grant.descriptor().unwrap();
                // Populate the receive allocation with real simulated DMA, not a private
                // native-memory fixture. Readback must still wait for the receiver fence.
                let source = immediate(Region::poll_acquire(&writer, 32)).unwrap();
                immediate(source.poll_copy_from(&[0xa5; 32])).unwrap();
                let written = immediate(writer.poll_write(
                    source.clone(),
                    descriptor.address,
                    descriptor.scoped_key,
                ))
                .unwrap();
                native.poll_budgeted(2).unwrap();
                native.poll_budgeted(2).unwrap();
                assert_eq!(written.result(), Some(Ok(())));
                drop((source, written));
                writer.stop();
                native.poll_budgeted(2).unwrap();
                drop(writer);
                let completion = verified(
                    &signers,
                    vec![header(
                        COMPLETION_HEADER,
                        STANDARD
                            .encode(completion_bytes(session.binding(), id))
                            .into_bytes(),
                    )],
                );
                if terminal == Some(Error::DeadlineExceeded) {
                    scope.deadline.0 = environment::now() + Duration::from_secs(1);
                }
                let quota = (terminal == Some(Error::Overloaded)).then(|| {
                    admission
                        .reserve(
                            None,
                            ResourceClass::Ciphertext,
                            admission.limit(ResourceClass::Ciphertext),
                        )
                        .unwrap()
                });
                let mut finish = transfer.finish_receive(
                    &session,
                    grant,
                    &completion,
                    envelope,
                    &admission,
                    &scope,
                );
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                if readback {
                    assert!(finish.as_mut().poll(&mut cx).is_pending());
                    native.poll_budgeted(2).unwrap();
                    native.poll_budgeted(2).unwrap();
                    assert!(finish.as_mut().poll(&mut cx).is_pending());
                    assert!(!qp.stopped());
                    if failed_fence {
                        sim.reject(simulation::Operation::Stop, Some(qp.endpoint.qpn), true);
                    }
                    native.poll_budgeted(2).unwrap();
                    assert_eq!(qp.stopped(), !failed_fence);
                }
                io.with_contention(Contention::Slot(0), || {
                if terminal != Some(Error::Overloaded) {
                    match finish.as_mut().poll(&mut cx) {
                        Poll::Pending => {}
                        Poll::Ready(Err(error)) => {
                            panic!("mailbox contention failed an admitted receive: {error:?}")
                        }
                        Poll::Ready(Ok(_)) => panic!("readback bypassed the held mailbox"),
                    }
                    assert_eq!(admission.used(ResourceClass::Ciphertext), 32);
                }
                if terminal == Some(Error::Cancelled) {
                    scope.cancel().unwrap();
                }
                if terminal == Some(Error::DeadlineExceeded) {
                    clock.advance(Duration::from_secs(1));
                }
                if let Some(error) = terminal {
                    assert!(matches!(finish.as_mut().poll(&mut cx), Poll::Ready(Err(e)) if e == error));
                }
            });
                if terminal.is_none() {
                    if !readback {
                        assert!(finish.as_mut().poll(&mut cx).is_pending());
                        native.poll_budgeted(2).unwrap();
                        native.poll_budgeted(2).unwrap();
                        assert!(finish.as_mut().poll(&mut cx).is_pending());
                        native.poll_budgeted(2).unwrap();
                    }
                    let Poll::Ready(Ok(page)) = finish.as_mut().poll(&mut cx) else {
                        panic!("receive did not finish after mailbox release")
                    };
                    assert_eq!(page.bytes(), &[0xa5; 32]);
                    assert_eq!(admission.used(ResourceClass::Ciphertext), 32);
                    drop(page);
                }
                drop(finish);
                drop(quota);
                assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
                if failed_fence {
                    assert!(!qp.stopped());
                    assert_eq!(charged.get(), 1);
                    assert!(
                        !io.snapshot(0).fenced,
                        "native receive bytes remain unavailable before the fence"
                    );
                    let qpn = qp.endpoint.qpn;
                    drop(session);
                    drop(qp);
                    assert_eq!(
                        peer_metrics.gauge(crate::telemetry::Gauge::PeerExchanges),
                        1
                    );
                    assert!(matches!(
                        peer_admission.acquire(&peer),
                        Err(Error::Overloaded)
                    ));
                    assert_eq!(
                        peer_metrics.gauge(crate::telemetry::Gauge::PeerReceiveActive),
                        1,
                        "failed native fence retains receive admission"
                    );
                    native.retry_now(0);
                    native.poll_budgeted(2).unwrap();
                    assert_eq!(charged.get(), 1);
                    assert_eq!(io.snapshot(0).state, State::Owned);
                    sim.reject(simulation::Operation::Stop, Some(qpn), false);
                    native.retry_now(0);
                    native.poll_budgeted(2).unwrap();
                    assert_eq!(io.snapshot(0).state, State::Ready);
                    assert_eq!(
                        peer_metrics.gauge(crate::telemetry::Gauge::PeerReceiveActive),
                        0
                    );
                    assert_eq!(receive_quota.used(ResourceClass::RequestContext), 0);
                    assert_eq!(
                        peer_metrics.gauge(crate::telemetry::Gauge::PeerExchanges),
                        0
                    );
                    native.close();
                    native.poll_budgeted(2).unwrap();
                    assert!(native.drained());
                    assert_eq!(charged.get(), 0);
                    return;
                }
                native.poll_budgeted(2).unwrap();
                assert!(qp.stopped());
                assert_eq!(charged.get(), 1);
                assert_eq!(io.snapshot(0).state, State::Owned);
                drop(session);
                drop(qp);
                native.poll_budgeted(2).unwrap();
                assert_eq!(io.snapshot(0).state, State::Ready);
                native.close();
                native.poll_budgeted(2).unwrap();
                assert!(native.drained());
                assert_eq!(charged.get(), 0);
            }
        }

        /// Production activation through the public simulated fabric.
        mod activation_tests {
            use super::*;
            use crate::admission::AdmissionPolicy;
            use crate::model::*;
            use crate::security;
            use crate::worker::CryptoRuntime;

            use crate::security::PageCryptoEngine;
            use racer_crypto::identity::KeyPurpose;
            use rdma_verbs::testing::Contention;
            use rdma_verbs::testing::State;
            use simulation::Fault;
            use simulation::Operation as NativeOp;
            use std::task::Context;

            fn fixture(
                slots: usize,
            ) -> (
                simulation::Simulation,
                Devices,
                NativeService,
                flow_control::Quotas<AdmissionPolicy>,
                RequestScope,
            ) {
                let sim = simulation::Simulation::new()
                    .with_devices(vec![simulation::Device::new("sim0", [1; 16])])
                    .unwrap();
                let (io, port) = pair(slots).unwrap();
                let native = {
                    let _environment = sim.enter();
                    NativeService::new(port)
                };
                let devices = Devices::new();
                devices.attach(io).unwrap();
                let admission = flow_control::Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(true).limits,
                ));
                let scope = RequestScope::new(
                    RequestId([1; 16]),
                    uring_runtime::environment::now() + std::time::Duration::from_secs(30),
                )
                .unwrap();
                (sim, devices, native, admission, scope)
            }
            fn activate<'a>(
                devices: &'a Devices,
                admission: &'a flow_control::Quotas<AdmissionPolicy>,
                scope: &'a RequestScope,
            ) -> Operation<'a, Vec<RailMapping>> {
                devices.activate(
                    vec![RailMapping {
                        rail: RailId(0),
                        device: "sim0".into(),
                        port: 1,
                        gid: None,
                        numa_node: None,
                    }],
                    admission,
                    4096,
                    scope,
                )
            }
            fn port(devices: &Devices) -> Rc<IoPort> {
                devices.port.borrow().as_ref().unwrap().clone()
            }

            #[test]
            fn repeated_rail_selects_exact_physical_binding_and_revokes_changed_gid() {
                let sim = simulation::Simulation::new()
                    .with_devices(vec![
                        simulation::Device::new("a", [1; 16]),
                        simulation::Device::new("b", [2; 16]),
                    ])
                    .unwrap();
                let _environment = sim.enter();
                let (io, native) = pair(2).unwrap();
                let mut service = NativeService::new(native);
                let devices = Devices::new();
                devices.attach(io).unwrap();
                let inventory = inventory();
                let publication: Vec<_> = inventory
                    .iter()
                    .cloned()
                    .map(|mut n| {
                        n.rail = RailId(7);
                        n
                    })
                    .collect();
                let selected = select_worker(&publication, &inventory, 1, None, 2);
                assert_eq!(selected.len(), 1);
                assert_eq!(selected[0].device, "b");
                let admission = flow_control::Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(true).limits,
                ));
                let scope = RequestScope::new(
                    RequestId([3; 16]),
                    uring_runtime::environment::now() + std::time::Duration::from_secs(30),
                )
                .unwrap();
                assert!(matches!(
                    futures::executor::block_on(devices.activate(
                        publication,
                        &admission,
                        4096,
                        &scope
                    )),
                    Err(Error::InvalidConfiguration)
                ));
                assert_eq!(admission.used(ResourceClass::Registered), 0);
                let mut activation = devices.activate(selected.clone(), &admission, 4096, &scope);
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                assert!(activation.as_mut().poll(&mut cx).is_pending());
                for _ in 0..8 {
                    service.poll_budgeted(8).unwrap();
                }
                let Poll::Ready(Ok(actual)) = activation.as_mut().poll(&mut cx) else {
                    panic!("activation pending");
                };
                drop(activation);
                assert_eq!(actual[0].device, "b");
                assert_eq!(actual[0].gid, Some([2; 16]));
                assert!(devices.ready(RailId(7)));
                assert!(devices.revalidate(&selected));
                let mut revoked = selected;
                revoked[0].gid = Some([3; 16]);
                assert!(!devices.revalidate(&revoked));
                assert!(!devices.ready(RailId(7)));
                for _ in 0..8 {
                    service.poll_budgeted(8).unwrap();
                }
                assert_eq!(admission.used(ResourceClass::Registered), 0);
                drop(service);
                assert_eq!(sim.live_resources(), 0);
            }

            #[test]
            fn configured_activation_spends_budget_and_yields_to_sibling_page_jobs() {
                let (sim, devices, mut native, admission, scope) = fixture(4);
                let shared = port(&devices);
                let mut activation = activate(&devices, &admission, &scope);
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                assert!(activation.as_mut().poll(&mut cx).is_pending());
                assert!(
                    !native.drained(),
                    "queued configuration owns accepted quota"
                );
                native.poll_budgeted(0).unwrap();
                assert!(sim.trace().is_empty());
                assert_eq!(admission.used(ResourceClass::Registered), 4 * 8192);
                let keys = crate::test_support::security::keys();
                let page = PageId {
                    version: ObjectVersion {
                        object: ObjectId {
                            cache: CacheId(crate::test_support::security::CACHE.into()),
                            key: CacheKey([3; 32]),
                        },
                        etag: StrongEtag::test_value("v1"),
                    },
                    number: PageNumber(0),
                };
                let cache = &page.version.object.cache;
                let sibling_admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(false).limits,
                )));
                let pool = BufferPool::new(sibling_admission.clone());
                let (io, port) =
                    security::pair(WorkerId(1), 1, std::num::NonZeroUsize::new(1).unwrap());
                let mut sibling = PageCryptoEngine::new(CryptoRuntime { port });
                for turn in 0..5 {
                    let Poll::Ready(Ok(permit)) = io.poll_reserve(
                        &mut cx,
                        security::CryptoId {
                            worker: WorkerId(1),
                            generation: 1,
                            sequence: turn + 1,
                        },
                    ) else {
                        panic!("reserve")
                    };
                    assert!(
                        io.try_submit(
                            permit.job(
                                security::CryptoInput::Encrypt {
                                    page: page.clone(),
                                    plaintext: pool
                                        .plaintext(
                                            sibling_admission
                                                .reserve(Some(cache), ResourceClass::Plaintext, 5)
                                                .unwrap(),
                                            5
                                        )
                                        .unwrap(),
                                    ciphertext: sibling_admission
                                        .reserve(Some(cache), ResourceClass::Ciphertext, 21)
                                        .unwrap(),
                                },
                                keys.active(cache, KeyPurpose::Page).unwrap(),
                                scope.clone()
                            )
                        )
                        .is_ok()
                    );
                    native.poll_budgeted(1).unwrap();
                    assert_eq!(
                        sim.take_trace()
                            .iter()
                            .filter(|event| event.operation == NativeOp::Register)
                            .count(),
                        usize::from(turn != 0)
                    );
                    assert_eq!(native.resource_count(), turn as usize);
                    if turn < 4 {
                        assert!(activation.as_mut().poll(&mut cx).is_pending());
                        assert!(
                            (0..shared.capacity()).all(|i| shared.snapshot(i).state == State::Idle)
                        );
                        assert!(!devices.ready(RailId(0)));
                    }
                    sibling.poll_budgeted(&mut cx, 1).unwrap();
                    let Poll::Ready(Ok(Some(completion))) = io.poll_completion(&mut cx) else {
                        panic!("sibling must progress")
                    };
                    assert!(matches!(
                        completion.outcome,
                        security::CryptoOutcome::Completed(_)
                    ));
                    drop(completion);
                    assert_eq!(sibling_admission.used(ResourceClass::Plaintext), 0);
                }
                assert!(matches!(
                    activation.as_mut().poll(&mut cx),
                    Poll::Ready(Ok(_))
                ));
                drop(activation);
                assert!(devices.ready(RailId(0)));
                devices.close();
                for remaining in (0..4).rev() {
                    native.poll_budgeted(1).unwrap();
                    assert_eq!(native.resource_count(), remaining);
                }
                assert_eq!(admission.used(ResourceClass::Registered), 0);
                assert_eq!(sim.live_resources(), 0);
            }

            #[test]
            fn partial_activation_errors_fence_before_retry_without_publishing_readiness() {
                for failure in [NativeOp::Register, NativeOp::Qp, NativeOp::Window] {
                    let (sim, devices, mut native, admission, scope) = fixture(3);
                    let io = port(&devices);
                    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                    let mut activation = activate(&devices, &admission, &scope);
                    assert!(activation.as_mut().poll(&mut cx).is_pending());
                    native.poll_budgeted(2).unwrap();
                    assert_eq!(native.resource_count(), 1);
                    sim.fault(failure, Fault::Reject);
                    native.poll_budgeted(1).unwrap();
                    assert!(matches!(
                        activation.as_mut().poll(&mut cx),
                        Poll::Ready(Err(Error::Unavailable))
                    ));
                    drop(activation);
                    assert!(!devices.ready(RailId(0)));
                    assert!((0..io.capacity()).all(|i| io.snapshot(i).state == State::Idle));
                    sim.fault(NativeOp::Stop, Fault::Reject);
                    native.poll_budgeted(1).unwrap();
                    assert!(native.resource_present(0));
                    assert!(!io.pool_drained());
                    assert!(admission.used(ResourceClass::Registered) >= 8192);
                    let mut retry = activate(&devices, &admission, &scope);
                    assert!(matches!(
                        retry.as_mut().poll(&mut cx),
                        Poll::Ready(Err(Error::Overloaded))
                    ));
                    drop(retry);
                    native.retry_now(0);
                    native.poll_budgeted(3).unwrap();
                    assert!(io.pool_drained());
                    assert_eq!(admission.used(ResourceClass::Registered), 0);
                    assert_eq!(sim.live_resources(), 0);
                    let mut retry = activate(&devices, &admission, &scope);
                    assert!(retry.as_mut().poll(&mut cx).is_pending());
                    native.poll_budgeted(3).unwrap();
                    assert!(retry.as_mut().poll(&mut cx).is_pending());
                    native.poll_budgeted(1).unwrap();
                    assert!(matches!(retry.as_mut().poll(&mut cx), Poll::Ready(Ok(_))));
                    drop(retry);
                    devices.close();
                    native.poll_budgeted(3).unwrap();
                    assert_eq!(admission.used(ResourceClass::Registered), 0);
                    assert_eq!(sim.live_resources(), 0);
                }
            }

            #[test]
            fn abandoned_activation_cleans_queued_discovered_and_partial_owners() {
                for turns in 0..=3 {
                    let (sim, devices, mut native, admission, scope) = fixture(4);
                    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                    let mut activation = activate(&devices, &admission, &scope);
                    assert!(activation.as_mut().poll(&mut cx).is_pending());
                    native.poll_budgeted(turns).unwrap();
                    assert!(!native.drained());
                    drop(activation);
                    native.poll_budgeted(1).unwrap();
                    native.poll_budgeted(4).unwrap();
                    assert!(native.drained());
                    assert!(port(&devices).pool_drained());
                    assert!(!devices.ready(RailId(0)));
                    assert_eq!(admission.used(ResourceClass::Registered), 0);
                    assert_eq!(sim.live_resources(), 0);
                }
            }

            #[test]
            fn activation_mailbox_contention_consumes_turn_without_losing_quota() {
                let (sim, devices, mut native, admission, scope) = fixture(2);
                let shared = port(&devices);
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                let mut activation = activate(&devices, &admission, &scope);
                assert!(activation.as_mut().poll(&mut cx).is_pending());
                native.poll_budgeted(1).unwrap();
                sim.take_trace();
                shared.with_contention(Contention::Slot(0), || {
                    native.poll_budgeted(1).unwrap();
                    assert!(sim.trace().is_empty());
                    assert_eq!(admission.used(ResourceClass::Registered), 2 * 8192);
                });
                native.poll_budgeted(2).unwrap();
                assert!(matches!(
                    activation.as_mut().poll(&mut cx),
                    Poll::Ready(Ok(_))
                ));
                drop(activation);
                devices.close();
                native.poll_budgeted(2).unwrap();
                assert_eq!(admission.used(ResourceClass::Registered), 0);
                assert_eq!(sim.live_resources(), 0);
            }

            #[test]
            fn configured_native_wrapper_drain_fences_one_slot_per_poll() {
                let sim = simulation::Simulation::new()
                    .with_devices(vec![simulation::Device::new("sim0", [1; 16])])
                    .unwrap();
                let (native_io, native_port) = pair(4).unwrap();
                let devices = Devices::new();
                devices.attach(native_io).unwrap();
                let admission = flow_control::Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(true).limits,
                ));
                let scope = super::scope();
                let (io, port) =
                    security::pair(WorkerId(0), 1, std::num::NonZeroUsize::new(1).unwrap());
                io.close_submissions().unwrap();
                let mut service = {
                    let _environment = sim.enter();
                    WithNative::new(PageCryptoEngine::new(CryptoRuntime { port }), native_port)
                };
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                let mut activation = activate(&devices, &admission, &scope);
                assert!(activation.as_mut().poll(&mut cx).is_pending());
                service.poll_budgeted(&mut cx, 4).unwrap();
                service.poll_budgeted(&mut cx, 1).unwrap();
                assert!(matches!(
                    activation.as_mut().poll(&mut cx),
                    Poll::Ready(Ok(_))
                ));
                drop(activation);
                sim.take_trace();
                let mut drain = service.drain(&scope);
                for slot in 0..4 {
                    let result = drain.as_mut().poll(&mut cx);
                    if slot < 3 {
                        assert!(result.is_pending());
                        assert_eq!(admission.used(ResourceClass::Registered), 4 * 8192);
                    } else {
                        assert_eq!(result, Poll::Ready(Ok(())));
                    }
                    assert_eq!(
                        sim.take_trace()
                            .iter()
                            .filter(|event| event.operation == NativeOp::Stop)
                            .count(),
                        1
                    );
                }
                drop(drain);
                assert_eq!(admission.used(ResourceClass::Registered), 0);
                assert_eq!(sim.live_resources(), 0);
            }

            #[test]
            fn cancellation_between_activation_turns_preserves_partial_owners_until_fenced() {
                let (sim, devices, mut native, admission, scope) = fixture(4);
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                let mut activation = activate(&devices, &admission, &scope);
                assert!(activation.as_mut().poll(&mut cx).is_pending());
                native.poll_budgeted(2).unwrap();
                scope.cancel().unwrap();
                assert!(matches!(
                    activation.as_mut().poll(&mut cx),
                    Poll::Ready(Err(Error::Cancelled))
                ));
                drop(activation);
                assert_eq!(admission.used(ResourceClass::Registered), 4 * 8192);
                native.poll_budgeted(1).unwrap();
                assert_eq!(admission.used(ResourceClass::Registered), 8192);
                assert!(!native.drained());
                native.poll_budgeted(1).unwrap();
                assert!(native.drained());
                assert_eq!(admission.used(ResourceClass::Registered), 0);
                assert_eq!(sim.live_resources(), 0);
            }
        }
        use crate::model::*;
        use crate::peer::protocol::Signatures;
        use crate::peer::protocol::VerifiedHead;
        use http1::Header;
        use http1::MessageHead;
        use http1::StartLine;
        use std::sync::Arc;
        use std::sync::atomic::AtomicUsize;
        use std::sync::atomic::Ordering;
        use std::task::Context;
        use std::task::Poll;
        use std::time::Duration;

        pub(super) struct Charge(Arc<AtomicUsize>);
        impl Drop for Charge {
            fn drop(&mut self) {
                self.0.fetch_sub(1, Ordering::AcqRel);
            }
        }
        pub(super) struct Observer(Arc<AtomicUsize>);
        impl Observer {
            pub fn get(&self) -> usize {
                self.0.load(Ordering::Acquire)
            }
        }
        pub(super) fn fixture(
            slots: usize,
        ) -> (
            simulation::Simulation,
            Rc<IoPort>,
            NativeService,
            Vec<Observer>,
        ) {
            let sim = simulation::Simulation::new()
                .with_devices(vec![simulation::Device::new("sim0", [1; 16])])
                .unwrap();
            let (io, port) = pair(slots).unwrap();
            let io = Rc::new(io);
            let mut native = {
                let _scope = sim.enter();
                NativeService::new(port)
            };
            let mut observers = Vec::new();
            let guards = (0..slots)
                .map(|_| {
                    let count = Arc::new(AtomicUsize::new(1));
                    observers.push(Observer(count.clone()));
                    Arc::new(Charge(count)) as rdma_verbs::Guard
                })
                .collect();
            futures::executor::block_on(io.configure(rdma_verbs::Configuration {
                discover: true,
                guards,
                bytes: 32,
                selector: Box::new(|ports| {
                    assert_eq!(ports[0].device, "sim0");
                    Ok(vec![(0, 0)])
                }),
            }))
            .unwrap();
            for _ in 0..=slots {
                native.poll_budgeted(1).unwrap();
            }
            io.activation().unwrap().unwrap();
            (sim, io, native, observers)
        }
        pub(super) fn immediate<T>(poll: Poll<rdma_verbs::Result<T>>) -> Result<T> {
            match poll {
                Poll::Ready(result) => result.map_err(Into::into),
                Poll::Pending => Err(Error::Overloaded),
            }
        }
        pub(super) fn claim(io: &Rc<IoPort>) -> Rc<QueuePairHandle> {
            immediate(QueuePairHandle::poll_new(io.device(0), None)).unwrap()
        }
        pub(super) fn connect_pair(
            a: &QueuePairHandle,
            b: &QueuePairHandle,
            native: &mut NativeService,
        ) {
            immediate(a.poll_connect(b.endpoint)).unwrap();
            immediate(b.poll_connect(a.endpoint)).unwrap();
            assert!(!a.ready() && !b.ready());
            native.poll_budgeted(256).unwrap();
            a.progress().unwrap();
            b.progress().unwrap();
            assert!(a.ready() && b.ready());
        }
        pub(super) fn mark_connected(qp: &QueuePairHandle, native: &mut NativeService) {
            immediate(qp.poll_connect(qp.endpoint)).unwrap();
            assert!(!qp.ready(), "connect is not executed on I/O");
            native.poll_budgeted(256).unwrap();
            qp.progress().unwrap();
            assert!(qp.ready());
        }
        pub(super) fn scope() -> RequestScope {
            RequestScope::new(
                RequestId([1; 16]),
                environment::now() + Duration::from_secs(10),
            )
            .unwrap()
        }
        pub(super) fn poll<T>(operation: &mut Operation<'_, T>) -> Poll<Result<T>> {
            operation
                .as_mut()
                .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
        }
        pub(super) fn done<T>(operation: &mut Operation<'_, T>) -> T {
            match poll(operation) {
                Poll::Ready(Ok(value)) => value,
                Poll::Ready(Err(e)) => panic!("unexpected error: {e:?}"),
                Poll::Pending => panic!("unexpected pending"),
            }
        }
        pub(super) fn verified(
            signers: &[Rc<Signatures>],
            mut headers: Vec<Header>,
        ) -> VerifiedHead {
            headers.push(Header {
                name: "racer-receiver".into(),
                value: signers[1].node().0.as_bytes().to_vec(),
            });
            signers[1]
                .verify_proof(
                    signers[0]
                        .sign(MessageHead {
                            start: StartLine::Request {
                                method: "POST".into(),
                                target: "/racer/peer/v1/rdma".into(),
                            },
                            headers,
                        })
                        .unwrap(),
                )
                .unwrap()
        }
        pub(super) fn header(name: &str, value: Vec<u8>) -> Header {
            Header {
                name: name.into(),
                value,
            }
        }
        pub(super) fn envelope() -> PageEnvelope {
            PageEnvelope {
                page: PageId {
                    version: ObjectVersion {
                        object: ObjectId {
                            cache: CacheId("mailbox-test".into()),
                            key: CacheKey([1; 32]),
                        },
                        etag: StrongEtag::test_value("v1"),
                    },
                    number: PageNumber(0),
                },
                key_id: KeyId([1; 16]),
                nonce: Nonce([2; 24]),
                plaintext_length: 16,
                ciphertext_length: 32,
            }
        }
        use uring_runtime::environment;

        mod lifecycle_tests {
            use super::*;
            use crate::admission::AdmissionPolicy;
            use crate::telemetry::Gauge;
            use crate::telemetry::Metrics;
            use rdma_verbs::testing::State;
            use std::task::Context;

            #[test]
            fn admitted_native_claim_keeps_capacity_after_proxy_drop_until_service_fence() {
                let (sim, io, mut native, _) = fixture(1);
                let metrics = Metrics::default();
                let admission = crate::peer::AdaptivePeers::new(
                    crate::peer::Config {
                        total: 1,
                        per_peer: 1,
                    },
                    metrics.clone(),
                )
                .unwrap();
                let peer = NodeId("native-peer".into());
                let qp = immediate(QueuePairHandle::poll_new(
                    io.device(0),
                    Some(admission.acquire(&peer).unwrap()),
                ))
                .unwrap();
                sim.reject(simulation::Operation::Stop, None, true);
                drop(qp);
                native.poll_budgeted(1).unwrap();
                assert_eq!(metrics.gauge(Gauge::PeerExchanges), 1);
                assert!(matches!(admission.acquire(&peer), Err(Error::Overloaded)));
                sim.reject(simulation::Operation::Stop, None, false);
                native.retry_now(0);
                native.poll_budgeted(1).unwrap();
                assert_eq!(metrics.gauge(Gauge::PeerExchanges), 0);
            }

            #[test]
            fn failed_native_service_teardown_quarantines_adaptive_permit_after_both_roles_drop() {
                let (sim, io, mut native, charges) = fixture(1);
                let charged = &charges[0];
                let metrics = Metrics::default();
                let admission = crate::peer::AdaptivePeers::new(
                    crate::peer::Config {
                        total: 1,
                        per_peer: 1,
                    },
                    metrics.clone(),
                )
                .unwrap();
                let peer = NodeId("native-quarantine".into());
                let qp = immediate(QueuePairHandle::poll_new(
                    io.device(0),
                    Some(admission.acquire(&peer).unwrap()),
                ))
                .unwrap();
                mark_connected(&qp, &mut native);
                let region = immediate(Region::poll_acquire(&qp, 16)).unwrap();
                let (window, ticket) = immediate(qp.poll_bind(region)).unwrap();
                native.poll_budgeted(1).unwrap();
                drop((window, ticket));
                sim.reject(simulation::Operation::Stop, None, true);
                drop(qp);
                drop(native);
                drop(io);
                assert_eq!(charged.get(), 1, "failed teardown keeps native ownership");
                assert_eq!(metrics.gauge(Gauge::PeerExchanges), 1);
                assert!(matches!(admission.acquire(&peer), Err(Error::Overloaded)));
                sim.reject(simulation::Operation::Stop, None, false);
            }

            #[test]
            fn simultaneous_timeout_and_healthy_write_preserve_worker_and_quarantine() {
                let (sim, io, mut native, charges) = fixture(2);
                let failed = claim(&io);
                let healthy = claim(&io);
                connect_pair(&failed, &healthy, &mut native);
                let receive = immediate(Region::poll_acquire(&failed, 16)).unwrap();
                let (grant, binding) = immediate(failed.poll_bind(receive.clone())).unwrap();
                native.poll_budgeted(2).unwrap();
                native.poll_budgeted(2).unwrap();
                assert_eq!(binding.result(), Some(Ok(())));
                let source = immediate(Region::poll_acquire(&healthy, 16)).unwrap();
                immediate(source.poll_copy_from(&[7; 16])).unwrap();
                let written =
                    immediate(healthy.poll_write(source, grant.address(), grant.key())).unwrap();
                native.poll_budgeted(2).unwrap();
                failed.expire_at(uring_runtime::environment::now());
                let sessions = Sessions::new(Rc::new(Devices::new()), 2);
                sessions.track_test(failed.clone());
                sessions.track_test(healthy.clone());
                sim.reject(simulation::Operation::Stop, Some(failed.endpoint.qpn), true);
                assert!(
                    sessions.progress().is_ok(),
                    "attempt timeout must not fail app's worker poll"
                );
                assert_eq!(failed.progress(), Err(rdma_verbs::Error::DeadlineExceeded));
                assert!(!failed.stopped());
                assert!(healthy.ready());
                native.poll_budgeted(2).unwrap();
                assert!(sessions.progress().is_ok());
                assert_eq!(written.result(), Some(Ok(())));
                assert_eq!(charges[0].get(), 1);
                assert_eq!(charges[1].get(), 1);
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                assert_eq!(
                    receive.poll_copy_to(&mut cx),
                    Poll::Ready(Err(rdma_verbs::Error::Unavailable))
                );
                sim.reject(
                    simulation::Operation::Stop,
                    Some(failed.endpoint.qpn),
                    false,
                );
                native.retry_now(0);
                native.poll_budgeted(2).unwrap();
                assert!(failed.stopped());
                assert!(immediate(receive.poll_copy_to(&mut cx)).is_ok());
                assert!(healthy.ready());
                assert_eq!(immediate(receive.poll_copy_to(&mut cx)).unwrap(), [7; 16]);
            }

            #[test]
            fn retirement_cut_is_captured_and_does_not_stop_later_sessions() {
                let (_, io, mut native, _) = fixture(2);
                let first = claim(&io);
                mark_connected(&first, &mut native);
                let sessions = Sessions::new(Rc::new(Devices::new()), 2);
                sessions.track_test(first.clone());
                let mut cut = sessions.fence_cut();
                let second = claim(&io);
                mark_connected(&second, &mut native);
                sessions.track_test(second.clone());
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                assert!(cut.as_mut().poll(&mut cx).is_pending());
                assert!(!first.ready());
                assert!(!first.stopped());
                assert!(second.ready());
                native.poll_budgeted(2).unwrap();
                assert!(matches!(cut.as_mut().poll(&mut cx), Poll::Ready(Ok(()))));
                assert!(first.stopped());
                assert!(second.ready());
                assert!(!io.closed());
                assert!(sessions.progress().is_ok());
            }

            #[test]
            fn cq_failure_is_attempt_local_and_slot_waits_for_all_leases_before_reuse() {
                let (sim, io, mut native, _) = fixture(2);
                let failed = claim(&io);
                let healthy = claim(&io);
                connect_pair(&failed, &healthy, &mut native);
                let region = immediate(Region::poll_acquire(&failed, 16)).unwrap();
                immediate(region.poll_copy_from(&[1; 16])).unwrap();
                sim.fault(
                    simulation::Operation::Write,
                    simulation::Fault::Completion(10),
                );
                let ticket = immediate(failed.poll_write(region.clone(), 4096, 7)).unwrap();
                native.poll_budgeted(2).unwrap();
                native.poll_budgeted(2).unwrap();
                let sessions = Sessions::new(Rc::new(Devices::new()), 2);
                sessions.track_test(failed.clone());
                sessions.track_test(healthy.clone());
                assert!(sessions.progress().is_ok());
                assert_eq!(ticket.result(), Some(Err(rdma_verbs::Error::Io)));
                assert!(healthy.ready());
                assert!(!failed.stopped());
                native.poll_budgeted(2).unwrap();
                sessions.progress().unwrap();
                assert!(failed.stopped());
                drop(failed);
                drop(ticket);
                native.poll_budgeted(2).unwrap();
                assert_eq!(io.snapshot(0).state, State::Owned);
                drop(region);
                native.poll_budgeted(2).unwrap();
                assert_eq!(io.snapshot(0).state, State::Ready);
                assert!(healthy.ready());
            }

            #[test]
            fn dropped_activation_does_not_publish_readiness_or_release_accepted_quota_early() {
                let (io, port) = pair(1).unwrap();
                let devices = Devices::new();
                devices.attach(io).unwrap();
                let admission = flow_control::Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(true).limits,
                ));
                let scope = scope();
                let mut operation = devices.activate(Vec::new(), &admission, 4096, &scope);
                assert!(
                    operation
                        .as_mut()
                        .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
                        .is_pending()
                );
                drop(operation);
                assert_eq!(admission.used(ResourceClass::Registered), 8192);
                let mut service = NativeService::new(port);
                service.poll_budgeted(1).unwrap();
                assert_eq!(admission.used(ResourceClass::Registered), 0);
                assert!(!devices.ready(RailId(0)));
            }
        }
    }
}
