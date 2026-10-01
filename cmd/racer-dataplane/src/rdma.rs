//! Match discovered ports to trusted local fabric associations and publication.
//! Fabric strings are opaque labels: a GID or enumeration order is never a label.
//!
//! Public contracts always compile; native FFI is gated by `rdma`. Attach bounded
//! lifecycle endpoints, activate devices against publication, and run `WithNative`
//! on the crypto role. I/O turns consume mailboxes and drive session progress.
mod ffi;
pub mod lifecycle;

use self::lifecycle::{
    DeviceHandle, Endpoint, IoPort, QueuePairHandle, Region, Ticket, Window, wait,
};
use crate::{
    error::{Error, Operation, Result},
    memory::pool::{BufferPool, CiphertextPage},
    model::{NodeId, PageEnvelope, ResourceClass, TransferId},
    runtime::{
        admission::Admission,
        deadline::{Deadline, RequestScope},
    },
    security::{certificates::VerifiedPeer, signing::VerifiedHead},
    topology::rails::{RailId, RailMapping},
};
use base64::{Engine, engine::general_purpose::STANDARD};
use sha2::{Digest, Sha256};
use std::{
    cell::{Cell, RefCell},
    future::poll_fn,
    rc::Rc,
    task::Poll,
};

pub const SETUP_HEADER: &str = "racer-rdma-setup";
pub const SETUP_BINDING_HEADER: &str = "racer-rdma-setup-binding";
/// Bounded authenticated RC sessions. Each transfer uses a dedicated session;
/// terminal QP destruction fences remote writes before registered memory reuse.
pub struct Sessions {
    devices: Rc<Devices>,
    per_neighbor: usize,
    live: RefCell<Vec<(NodeId, Rc<QueuePairHandle>)>>,
    draining: Cell<bool>,
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
        crate::runtime::environment::fill_random(&mut nonce).map_err(|_| Error::Io)?;
        let mut encoded = b"racer-rdma-setup-v1\0".to_vec();
        encoded.extend_from_slice(&rail.0.to_be_bytes());
        encoded.extend_from_slice(&nonce);
        encoded.extend_from_slice(&endpoint.gid);
        for v in [endpoint.qpn, endpoint.psn, endpoint.mtu] {
            encoded.extend_from_slice(&v.to_be_bytes());
        }
        encoded.extend_from_slice(&endpoint.lid.to_be_bytes());
        encoded.extend_from_slice(&[endpoint.port, endpoint.link_layer]);
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
        let endpoint = Endpoint {
            gid: b[18..34].try_into().unwrap(),
            qpn: u32::from_be_bytes(b[34..38].try_into().unwrap()),
            psn: u32::from_be_bytes(b[38..42].try_into().unwrap()),
            mtu: u32::from_be_bytes(b[42..46].try_into().unwrap()),
            lid: u16::from_be_bytes(b[46..48].try_into().unwrap()),
            port: b[48],
            link_layer: b[49],
        };
        endpoint.validate()?;
        Ok(endpoint)
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
/// component list. VerifiedHead establishes identity, freshness and replay checks.
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
        self.live.borrow_mut().push((NodeId("test".into()), qp));
    }
    pub fn register_driver(&self, waker: &std::task::Waker) {
        self.devices.register_driver(waker);
    }
    #[cfg(test)]
    pub(crate) fn track_peer_test(&self, peer: NodeId, qp: Rc<QueuePairHandle>) {
        self.live.borrow_mut().push((peer, qp));
    }
    pub fn new(devices: Rc<Devices>, per_neighbor: usize) -> Self {
        Self {
            devices,
            per_neighbor,
            live: RefCell::new(Vec::new()),
            draining: Cell::new(false),
        }
    }
    pub fn ready(&self, rail: RailId) -> bool {
        !self.draining.get() && self.per_neighbor > 0 && self.devices.ready(rail)
    }
    /// Both sides prepare before exchanging signed setup headers. The routing
    /// owner must first validate the complete path's authenticated rail mapping.
    pub fn prepare<'a>(
        &'a self,
        peer: &'a VerifiedPeer,
        rail: RailId,
        scope: &'a RequestScope,
    ) -> Operation<'a, PreparedSession> {
        self.prepare_admitted(peer, rail, None, scope)
    }
    pub(crate) fn prepare_admitted<'a>(
        &'a self,
        peer: &'a VerifiedPeer,
        rail: RailId,
        permit: Option<std::sync::Arc<crate::peer::adaptive::Permit>>,
        scope: &'a crate::runtime::deadline::RequestScope,
    ) -> Operation<'a, PreparedSession> {
        Box::pin(wait(scope, move |cx| {
            self.register_driver(cx.waker());
            self.poll_prepare(peer, rail, permit.clone())
        }))
    }
    fn poll_prepare(
        &self,
        peer: &VerifiedPeer,
        rail: RailId,
        permit: Option<std::sync::Arc<crate::peer::adaptive::Permit>>,
    ) -> Poll<Result<PreparedSession>> {
        if !self.ready(rail) {
            return Poll::Ready(Err(Error::Unavailable));
        }
        let mut live = self.live.borrow_mut();
        live.retain(|(_, qp)| !qp.stopped());
        if live.len()
            >= self
                .per_neighbor
                .saturating_mul(crate::topology::MAX_DEGREE)
            || live.iter().filter(|(node, _)| node == peer.node()).count() >= self.per_neighbor
        {
            return Poll::Ready(Err(Error::Overloaded));
        }
        let qp = std::task::ready!(QueuePairHandle::poll_new_admitted(
            self.devices.select(rail)?.handle,
            permit
        ))?;
        // Bound a peer that opens setup but never completes the exchange. A
        // transfer subsequently replaces this with its original request deadline.
        qp.expire_at(crate::runtime::environment::now() + std::time::Duration::from_secs(30));
        let setup = SetupParameters::new(rail, qp.endpoint)?;
        live.push((peer.node().clone(), qp.clone()));
        Poll::Ready(Ok(PreparedSession {
            qp,
            peer: peer.node().clone(),
            setup,
            finished: Cell::new(false),
        }))
    }
    pub fn progress(&self) -> Result<usize> {
        let mut count = 0;
        for (_, qp) in self.live.borrow().iter() {
            match qp.progress() {
                Ok(n) => count += n,
                Err(_) => {
                    let _ = qp.stop();
                }
            }
        }
        self.live.borrow_mut().retain(|(_, qp)| !qp.stopped());
        // Errors belong to the attempt ticket. The paired native service keeps
        // failed fences quarantined; neither a timeout nor CQ error is node-fatal.
        Ok(count)
    }
    pub fn drain(&self) -> Operation<'_, ()> {
        Box::pin(async move {
            self.draining.set(true);
            let qps: Vec<_> = self
                .live
                .borrow()
                .iter()
                .map(|(_, qp)| qp.clone())
                .collect();
            for qp in &qps {
                qp.stop()?;
            }
            for qp in qps {
                futures::future::poll_fn(|cx| qp.poll_stopped(cx)).await?;
            }
            self.live.borrow_mut().clear();
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
        let qps: Vec<_> = self
            .live
            .borrow()
            .iter()
            .map(|(_, qp)| qp.clone())
            .collect();
        Box::pin(async move {
            for qp in &qps {
                qp.stop()?;
            }
            for qp in qps {
                futures::future::poll_fn(|cx| qp.poll_stopped(cx)).await?;
            }
            Ok(())
        })
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
            wait(scope, |cx| {
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
            let _ = self.qp.stop();
        }
    }
}
impl Drop for SessionLease {
    fn drop(&mut self) {
        let _ = self.qp.stop();
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
                    let _ = self.qp.stop();
                    return Poll::Ready(Err(error));
                }
                self.qp.poll_connected(cx)
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
        self.qp.progress()
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
        self.qp.stop()
    }
}
#[cfg(test)]
mod session_tests {
    use super::*;
    #[test]
    fn real_signed_setup_rejects_tampering_and_replay() {
        use crate::{
            control::wire::{BundleGeneration, KeyringBundle, SCHEMA_VERSION},
            http::{Header, MessageHead, StartLine},
            model::ClusterId,
            security::{
                certificates::Certificates,
                identity::tests::{CLUSTER, NODE, issued},
                keyring::{KeyEpochs, Keyring},
                signing::{Signatures, SignedHead},
            },
        };
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
        crate::security::connection::tests::replay_and_binding_checks();
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
        s.rail = RailId(8);
        assert_eq!(s.endpoint(), Err(Error::InvalidRequest));
        s.rail = RailId(7);
        s.encoded.push(0);
        assert!(s.endpoint().is_err());
    }
}

pub const DESCRIPTOR_HEADER: &str = "racer-rdma-descriptor";
pub const COMPLETION_HEADER: &str = "racer-rdma-completion";
/// Type-2B windows expose exactly one transfer buffer. Bind CQE precedes export;
/// invalidation plus terminal QP destruction precedes any CPU access or reuse.
pub struct Grant {
    transfer: TransferId,
    buffer: Option<RegisteredLease>,
    qp: Rc<QueuePairHandle>,
    window: Rc<Window>,
    bound: Ticket,
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
            struct Abort<'a>(Option<&'a QueuePairHandle>);
            impl Drop for Abort<'_> {
                fn drop(&mut self) {
                    if let Some(qp) = self.0 {
                        let _ = qp.stop();
                    }
                }
            }
            let mut abort = Abort(Some(&session.qp));
            let (window, bound) = wait(scope, |cx| {
                session.qp.register_waiter(cx);
                session.qp.poll_bind(buffer.region.clone())
            })
            .await?;
            // The returned Grant takes over abort-on-drop ownership.
            abort.0 = None;
            Ok(Grant {
                transfer,
                buffer: Some(buffer),
                qp: session.qp.clone(),
                window,
                bound,
                deadline,
                binding: session.binding(),
            })
        })
    }
    pub fn transfer(&self) -> TransferId {
        self.transfer
    }
    pub fn descriptor(&self) -> Result<RemoteDescriptor> {
        if crate::runtime::environment::now() >= self.deadline.0 {
            return Err(Error::DeadlineExceeded);
        }
        if !self.qp.ready() {
            return Err(Error::Unavailable);
        }
        self.bound.result().ok_or(Error::Unavailable)??;
        let buffer = self.buffer.as_ref().ok_or(Error::InvalidRequest)?;
        Ok(RemoteDescriptor {
            transfer: self.transfer,
            address: self.window.address.get(),
            length: buffer.len() as u64,
            scoped_key: self.window.key.get(),
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
                    let _ = self.qp.stop();
                    return Poll::Ready(Err(error));
                }
                if crate::runtime::environment::now() >= self.deadline.0 {
                    let _ = self.qp.stop();
                    return Poll::Ready(Err(Error::DeadlineExceeded));
                }
                if let Err(error) = self.qp.progress() {
                    return Poll::Ready(Err(error));
                }
                self.bound.poll(cx)
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
            self.bound.result().ok_or(Error::Unavailable)??;
            let mut invalidated = None;
            let cancellation = scope.cancellation.subscribe()?;
            poll_fn(|cx| {
                cancellation.register(cx.waker());
                if let Err(error) = scope.check() {
                    return Poll::Ready(Err(error));
                }
                if crate::runtime::environment::now() >= self.deadline.0 {
                    return Poll::Ready(Err(Error::DeadlineExceeded));
                }
                if let Err(error) = self.qp.progress() {
                    return Poll::Ready(Err(error));
                }
                if invalidated.is_none() {
                    match self.qp.poll_invalidate(self.window.clone(), cx) {
                        Poll::Pending => return Poll::Pending,
                        Poll::Ready(Err(error)) => return Poll::Ready(Err(error)),
                        Poll::Ready(Ok(ticket)) => invalidated = Some(ticket),
                    }
                }
                invalidated.as_ref().unwrap().poll(cx)
            })
            .await?;
            // Cancellation/expiry returns no buffer. Grant Drop requests stop;
            // the native service retains DMA ownership until the real fence.
            poll_fn(|cx| {
                cancellation.register(cx.waker());
                scope.check()?;
                if crate::runtime::environment::now() >= self.deadline.0 {
                    return Poll::Ready(Err(Error::DeadlineExceeded));
                }
                self.qp.poll_stopped(cx)
            })
            .await?;
            self.buffer.take().ok_or(Error::InvalidRequest)
        })
    }
}
impl Drop for Grant {
    fn drop(&mut self) {
        // Dropping a future/grant is an abort, never implicit completion. On a
        // failed fence QP-owned window/region references preserve quarantine.
        let _ = self.qp.stop();
    }
}
pub(crate) fn completion_bytes(binding: [u8; 32], transfer: TransferId) -> Vec<u8> {
    let mut bytes = b"racer-rdma-complete-v1\0".to_vec();
    bytes.extend_from_slice(&binding);
    bytes.extend_from_slice(&transfer.0);
    bytes
}
#[cfg(test)]
mod permission_tests {
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

/// Ciphertext-only movement. Failed attempts are fenced before HTTP fallback.
pub struct RdmaTransfer {
    sessions: Rc<Sessions>,
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
struct AbortOnDrop(Rc<QueuePairHandle>);
impl Drop for AbortOnDrop {
    fn drop(&mut self) {
        let _ = self.0.stop();
    }
}
impl RdmaTransfer {
    pub fn register_driver(&self, waker: &std::task::Waker) {
        self.sessions.register_driver(waker);
    }
    pub fn new(sessions: Rc<Sessions>) -> Self {
        Self { sessions }
    }
    pub fn ready(&self, rail: RailId) -> bool {
        self.sessions.ready(rail)
    }
    pub fn progress(&self) -> Result<usize> {
        self.sessions.progress()
    }
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
            let _abort = AbortOnDrop(session.qp.clone());
            let mut buffer = RegisteredLease::acquire(session, page.bytes().len(), scope).await?;
            buffer.copy_from(page.bytes(), scope).await?;
            let ticket = wait(scope, |cx| {
                session.qp.register_waiter(cx);
                session.qp.poll_write(
                    buffer.region.clone(),
                    descriptor.descriptor.address,
                    descriptor.descriptor.scoped_key,
                )
            })
            .await?;
            let cancellation = scope.cancellation.subscribe()?;
            poll_fn(|cx| {
                cancellation.register(cx.waker());
                if let Err(error) = scope.check() {
                    return Poll::Ready(Err(error));
                }
                if let Err(error) = session.progress() {
                    return Poll::Ready(Err(error));
                }
                ticket.poll(cx)
            })
            .await?;
            // Source buffer is safe after its write CQE. Stop this single-use QP
            // before returning a control completion or admitting a fallback.
            // Request termination abandons this wait, not the native owner's
            // quarantine. Only a successful terminal fence permits completion.
            wait(scope, |cx| session.qp.poll_stopped(cx)).await?;
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
        admission: &'a Rc<Admission>,
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
    pub fn drain(&self) -> Operation<'_, ()> {
        self.sessions.drain()
    }
    pub fn fence_cut(&self) -> Operation<'static, ()> {
        self.sessions.fence_cut()
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
            let region = wait(scope, |cx| {
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
        Box::pin(wait(scope, |cx| {
            self.region.register_waiter(cx);
            self.region.poll_copy_from(ciphertext)
        }))
    }
    pub fn to_vec<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, Vec<u8>> {
        Box::pin(wait(scope, |cx| self.region.poll_copy_to(cx)))
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
/// A native slot owns both a registered buffer and bounded handoff staging.
pub(crate) fn native_slot_charge(length: usize) -> Result<usize> {
    registered_charge(length)?
        .checked_mul(2)
        .ok_or(Error::Overloaded)
}

#[cfg(test)]
mod registered_tests {
    use super::*;
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
/// Administrator-provided local association. Publication only contains an opaque
/// fabric name and NUMA hint; it cannot identify a physical NIC by itself.
#[derive(Clone, Debug)]
pub struct FabricPort {
    pub fabric: String,
    pub device: String,
    pub port: u8,
    pub gid: Option<[u8; 16]>,
}
#[derive(Clone, Debug)]
pub struct DiscoveredPort {
    pub device: String,
    pub port: u8,
    pub gid: [u8; 16],
    pub numa_node: Option<usize>,
}
pub(crate) fn discovered_port(device: &ffi::NativeDevice) -> Result<DiscoveredPort> {
    Ok(DiscoveredPort {
        device: device.name.clone(),
        port: device.endpoint.port,
        gid: device.endpoint.gid,
        numa_node: device.numa_node(),
    })
}
/// Pure deterministic matching used by native activation. An absent association,
/// duplicate candidate, reused physical port or mismatched NUMA fails closed.
pub fn match_publication(
    publication: &[RailMapping],
    associations: &[FabricPort],
    discovered: &[DiscoveredPort],
) -> Result<Vec<(RailMapping, usize)>> {
    if publication.len() > 64 || associations.len() > 64 || discovered.len() > 64 {
        return Err(Error::InvalidConfiguration);
    }
    let mut result: Vec<(RailMapping, usize)> = Vec::new();
    for published in publication {
        if published.fabric.is_empty() {
            return Err(Error::InvalidConfiguration);
        }
        if result.iter().any(|(r, _)| r.rail == published.rail) {
            return Err(Error::InvalidConfiguration);
        }
        let mappings: Vec<_> = associations
            .iter()
            .filter(|a| a.fabric == published.fabric)
            .collect();
        if mappings.len() != 1 {
            return Err(Error::Unavailable);
        }
        let mapping = mappings[0];
        if mapping.port == 0 || mapping.device.is_empty() {
            return Err(Error::InvalidConfiguration);
        }
        let candidates: Vec<_> = discovered
            .iter()
            .enumerate()
            .filter(|(_, d)| {
                d.device == mapping.device
                    && d.port == mapping.port
                    && d.gid != [0; 16]
                    && mapping.gid.is_none_or(|gid| d.gid == gid)
                    && published
                        .numa_node
                        .is_none_or(|numa| d.numa_node == Some(numa))
            })
            .collect();
        if candidates.len() != 1 || result.iter().any(|(_, index)| *index == candidates[0].0) {
            return Err(Error::Unavailable);
        }
        let mut actual = published.clone();
        actual.numa_node = candidates[0].1.numa_node;
        result.push((actual, candidates[0].0));
    }
    Ok(result)
}
impl Devices {
    #[cfg(test)]
    pub(crate) fn test(port: std::rc::Rc<IoPort>) -> Self {
        let devices = Self::new();
        devices.selected.borrow_mut().push(Device {
            handle: std::rc::Rc::new(DeviceHandle {
                port: port.clone(),
                rail: RailId(0),
                generation: port
                    .shared
                    .generation
                    .load(std::sync::atomic::Ordering::Acquire),
            }),
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
        associations: Vec<FabricPort>,
        admission: &'a Admission,
        bytes_per_slot: usize,
        scope: &'a RequestScope,
    ) -> Operation<'a, Vec<RailMapping>> {
        Box::pin(async move {
            scope.check()?;
            if bytes_per_slot == 0 || bytes_per_slot > MAX_CIPHERTEXT {
                return Err(Error::InvalidConfiguration);
            }
            let port = self.port.borrow().clone().ok_or(Error::Unavailable)?;
            port.reopen()?;
            let charge = native_slot_charge(bytes_per_slot)?;
            // One native registered allocation plus one bounded handoff staging
            // allocation per slot. Both remain charged through native quarantine.
            let quotas = (0..port.capacity())
                .map(|_| admission.reserve(None, ResourceClass::Registered, charge))
                .collect::<Result<Vec<_>>>()?;
            port.configure(publication, associations, quotas, bytes_per_slot, scope)
                .await?;
            struct ActivationGuard<'a> {
                port: &'a IoPort,
                completed: bool,
            }
            impl Drop for ActivationGuard<'_> {
                fn drop(&mut self) {
                    if !self.completed {
                        self.port.close();
                    }
                }
            }
            let mut guard = ActivationGuard {
                port: &port,
                completed: false,
            };
            let cancel = scope.cancellation.subscribe()?;
            let mappings = futures::future::poll_fn(|cx| {
                port.register_driver(cx.waker());
                cancel.register(cx.waker());
                if port
                    .shared
                    .closed
                    .load(std::sync::atomic::Ordering::Acquire)
                {
                    return std::task::Poll::Ready(Err(Error::Unavailable));
                }
                if let Err(error) = scope.check() {
                    port.close();
                    return std::task::Poll::Ready(Err(error));
                }
                port.activation()
                    .map_or(std::task::Poll::Pending, std::task::Poll::Ready)
            })
            .await?;
            guard.completed = true;
            *self.selected.borrow_mut() = mappings
                .iter()
                .map(|mapping| Device {
                    handle: Rc::new(DeviceHandle {
                        port: port.clone(),
                        rail: mapping.rail,
                        generation: port
                            .shared
                            .generation
                            .load(std::sync::atomic::Ordering::Acquire),
                    }),
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
            && self.port.borrow().as_ref().is_some_and(|port| {
                !port
                    .shared
                    .closed
                    .load(std::sync::atomic::Ordering::Acquire)
            })
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
    pub fn revalidate(&self, published: &[RailMapping], alignment_enabled: bool) -> bool {
        let actual = self.mappings.borrow();
        let valid = alignment_enabled
            && !actual.is_empty()
            && actual.len() == published.len()
            && actual.iter().all(|a| {
                published.iter().any(|p| {
                    p.rail == a.rail
                        && p.fabric == a.fabric
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
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn unconfigured_rails_require_http() {
        let devices = Devices::new();
        assert!(!devices.ready(RailId(0)));
        assert!(devices.select(RailId(0)).is_err());
    }
    #[test]
    fn discovery_never_invents_fabric_matches_and_rejects_ambiguity() {
        let publication = vec![RailMapping {
            rail: RailId(7),
            fabric: "fabric-a".into(),
            numa_node: Some(1),
        }];
        let mapping = FabricPort {
            fabric: "fabric-a".into(),
            device: "mlx5_0".into(),
            port: 1,
            gid: None,
        };
        let port = DiscoveredPort {
            device: "mlx5_0".into(),
            port: 1,
            gid: [1; 16],
            numa_node: Some(1),
        };
        assert!(match_publication(&publication, &[], &[port.clone()]).is_err());
        assert!(
            match_publication(
                &publication,
                &[mapping.clone(), mapping.clone()],
                &[port.clone()]
            )
            .is_err()
        );
        assert!(
            match_publication(
                &publication,
                &[mapping.clone()],
                &[port.clone(), port.clone()]
            )
            .is_err()
        );
        assert!(
            match_publication(
                &publication,
                &[mapping.clone()],
                &[DiscoveredPort {
                    numa_node: Some(0),
                    ..port.clone()
                }]
            )
            .is_err()
        );
        assert_eq!(
            match_publication(&publication, &[mapping], &[port]).unwrap()[0].0,
            publication[0]
        );
    }
}
