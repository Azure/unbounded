//! Bounded authenticated RC sessions. Each transfer uses a dedicated session;
//! terminal QP destruction fences remote writes before registered memory reuse.
use super::{
    device::Devices,
    verbs::{Endpoint, QueuePairHandle},
};
use crate::{
    error::{Error, Operation, Result},
    model::identity::NodeId,
    security::{certificates::VerifiedPeer, signing::VerifiedHead},
    topology::rails::RailId,
};
use base64::{Engine, engine::general_purpose::STANDARD};
use sha2::{Digest, Sha256};
use std::{
    cell::{Cell, RefCell},
    rc::Rc,
};

pub const SETUP_HEADER: &str = "racer-rdma-setup";
pub const SETUP_BINDING_HEADER: &str = "racer-rdma-setup-binding";
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
        getrandom::getrandom(&mut nonce).map_err(|_| Error::Io)?;
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
        scope: &'a crate::runtime::deadline::RequestScope,
    ) -> Operation<'a, PreparedSession> {
        Box::pin(super::verbs::wait(scope, move |cx| {
            self.register_driver(cx.waker());
            self.poll_prepare(peer, rail)
        }))
    }
    fn poll_prepare(
        &self,
        peer: &VerifiedPeer,
        rail: RailId,
    ) -> std::task::Poll<Result<PreparedSession>> {
        if !self.ready(rail) {
            return std::task::Poll::Ready(Err(Error::Unavailable));
        }
        let mut live = self.live.borrow_mut();
        live.retain(|(_, qp)| !qp.stopped());
        if live.len() >= self.per_neighbor.saturating_mul(36)
            || live.iter().filter(|(node, _)| node == peer.node()).count() >= self.per_neighbor
        {
            return std::task::Poll::Ready(Err(Error::Overloaded));
        }
        let qp = std::task::ready!(QueuePairHandle::poll_new(self.devices.select(rail)?.handle))?;
        // Bound a peer that opens setup but never completes the exchange. A
        // transfer subsequently replaces this with its original request deadline.
        qp.expire_at(std::time::Instant::now() + std::time::Duration::from_secs(30));
        let setup = SetupParameters::new(rail, qp.endpoint)?;
        live.push((peer.node().clone(), qp.clone()));
        std::task::Poll::Ready(Ok(PreparedSession {
            qp,
            peer: peer.node().clone(),
            setup,
            finished: Cell::new(false),
        }))
    }
    /// Legacy one-message setup cannot bind a locally created QP to the signed
    /// exchange. Use prepare/finish instead; never silently trust encoded bytes.
    pub fn establish(
        &self,
        _peer: VerifiedPeer,
        _setup: SetupParameters,
    ) -> Operation<'_, SessionLease> {
        Box::pin(async { Err(Error::Unauthorized) })
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
    /// Nonterminal accepted-operation cut for cache/key retirement. Call after
    /// pausing affected producers; unrelated future sessions remain admissible.
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
        scope: &'a crate::runtime::deadline::RequestScope,
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
            super::verbs::wait(scope, |cx| {
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
    pub fn wait_ready<'a>(
        &'a self,
        scope: &'a crate::runtime::deadline::RequestScope,
    ) -> Operation<'a, ()> {
        Box::pin(async move {
            let cancellation = scope.cancellation.subscribe()?;
            futures::future::poll_fn(|cx| {
                cancellation.register(cx.waker());
                if let Err(error) = scope.check() {
                    let _ = self.qp.stop();
                    return std::task::Poll::Ready(Err(error));
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
mod tests {
    use super::*;
    #[test]
    fn real_signed_setup_rejects_tampering_and_replay() {
        use crate::{
            control::wire::{BundleGeneration, KeyringBundle, SCHEMA_VERSION},
            http::codec::{Header, MessageHead, StartLine},
            model::identity::ClusterId,
            security::{
                certificates::Certificates,
                identity::tests::{CLUSTER, NODE, issued},
                keyring::{KeyEpochs, Keyring},
                replay::{ReplayState, ReplayWindow},
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
        let signatures = Signatures::new(
            keys,
            certificates,
            Rc::new(ReplayWindow::new(Arc::new(ReplayState::default()), 32)),
        );
        signatures
            .configure_authenticated_peer_challenge(node, signatures.challenge().unwrap())
            .unwrap();
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
        let verified = signatures.verify(signatures.sign(head()).unwrap()).unwrap();
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
        assert!(matches!(signatures.verify(replay), Err(Error::Replay)));
        let mut tampered = signatures.sign(head()).unwrap();
        tampered
            .head
            .headers
            .iter_mut()
            .find(|h| h.name == SETUP_HEADER)
            .unwrap()
            .value[0] = b'A';
        assert!(signatures.verify(tampered).is_err());
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
