// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! RDMA offer exchange and persistent, mutually authenticated TLS control channel.
//! HTTP HEAD selects an activated volume/shard. After the response both endpoints
//! transfer that same TLS connection to the RDMA completion source. Only bulk
//! reads, window binds and invalidations use verbs. TLS controls retain the exact
//! offer, route, cache generation and peer certificate binding for their lifetime.
use crate::{
    control::{Prepared, credentials::Provider},
    http::{Headers, Progress},
    http_client as client, http_server as http, rdma,
    uring::{Ring, Work},
};
use peer_identity::{FabricId, MAX_RAILS, NodeId, RailId};
use std::{
    cell::{Cell, RefCell},
    collections::VecDeque,
    io,
    rc::Rc,
    sync::Arc,
    time::{Duration, Instant},
};

pub const HEADER: &str = "X-Racer-Rdma";
const VERSION: u8 = 3;
const PREFIX: usize = 106;
const MAX_CONTROL: usize = 4096;
const CHANNEL_DEPTH: usize = 128;
const LIFETIME: Duration = Duration::from_secs(300);
const DRAIN: Duration = Duration::from_secs(30);
fn invalid(message: &'static str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, message)
}
fn field<const N: usize>(bytes: &[u8], offset: usize) -> [u8; N] {
    bytes[offset..offset + N].try_into().unwrap()
}
pub(crate) fn transport_nonce() -> io::Result<[u8; 16]> {
    let mut bytes = [0; 16];
    crate::environment::random(&mut bytes).map_err(|e| io::Error::other(e.to_string()))?;
    if bytes == [0; 16] {
        return Err(invalid("zero transport nonce"));
    }
    Ok(bytes)
}
pub fn rails_for_shard(shard: u64, local: usize, remote: usize) -> Option<(usize, usize)> {
    (local != 0 && remote != 0).then(|| {
        (
            (shard % local as u64) as usize,
            (shard % remote as u64) as usize,
        )
    })
}
#[derive(Clone, Debug)]
pub struct TransportConfig {
    pub fabric: String,
    pub connections: usize,
    pub depth: usize,
    pub timeout: Duration,
}
impl Default for TransportConfig {
    fn default() -> Self {
        Self {
            fabric: String::new(),
            connections: 32,
            depth: 16,
            timeout: Duration::from_secs(5),
        }
    }
}
impl TransportConfig {
    pub(crate) fn validate(&self) -> io::Result<()> {
        if self.fabric.len() > u16::MAX as usize
            || !(1..=4096).contains(&self.connections)
            || !(1..=128).contains(&self.depth)
            || self.timeout.is_zero()
            || self.timeout > DRAIN
            || crate::environment::now()
                .checked_add(self.timeout)
                .is_none()
        {
            return Err(control_wire::argument_error());
        }
        Ok(())
    }
}

/// Pure structural codec. Authorization comes exclusively from ControlChannel.
pub(crate) mod control_wire {
    use super::{invalid, io};
    use crate::buffers::BUFFER_SIZE;
    pub(crate) const HEADER: usize = 112;
    pub(crate) fn argument_error() -> io::Error {
        io::Error::new(
            io::ErrorKind::InvalidInput,
            "invalid RDMA argument or identity",
        )
    }
    pub(crate) fn protocol_error() -> io::Error {
        invalid("invalid RDMA protocol state")
    }
    pub(crate) fn capacity_error() -> io::Error {
        io::Error::new(
            io::ErrorKind::WouldBlock,
            "RDMA capacity exhausted; use HTTP or retry",
        )
    }
    #[cfg(test)]
    pub(crate) fn test_grant(mut frame: Frame) -> Frame {
        frame.kind = 2;
        frame.session = [2; 16];
        frame.grant = 1;
        frame.address = 0x400000;
        frame.key = 0x123401;
        frame.metadata = 8;
        frame
    }
    #[derive(Clone, Copy, Default)]
    pub(crate) struct Frame {
        pub kind: u8,
        pub session: [u8; 16],
        pub request: u64,
        pub value: [u8; 32],
        pub len: u32,
        pub grant: u64,
        pub address: u64,
        pub key: u32,
        pub metadata: u16,
    }
    impl Frame {
        pub(crate) fn encode(self, out: &mut [u8]) {
            out[..HEADER].fill(0);
            out[..4].copy_from_slice(b"RCR4");
            out[4] = self.kind;
            out[8..24].copy_from_slice(&self.session);
            out[24..32].copy_from_slice(&self.request.to_be_bytes());
            out[32..64].copy_from_slice(&self.value);
            out[64..68].copy_from_slice(&self.len.to_be_bytes());
            out[68..76].copy_from_slice(&self.grant.to_be_bytes());
            out[76..84].copy_from_slice(&self.address.to_be_bytes());
            out[84..88].copy_from_slice(&self.key.to_be_bytes());
            out[88..90].copy_from_slice(&self.metadata.to_be_bytes());
        }
        pub(crate) fn decode(bytes: &[u8]) -> io::Result<Self> {
            if bytes.len() < HEADER
                || &bytes[..4] != b"RCR4"
                || bytes[5..8]
                    .iter()
                    .chain(&bytes[90..HEADER])
                    .any(|b| *b != 0)
            {
                return Err(protocol_error());
            }
            let f = Self {
                kind: bytes[4],
                session: super::field(bytes, 8),
                request: u64::from_be_bytes(super::field(bytes, 24)),
                value: super::field(bytes, 32),
                len: u32::from_be_bytes(super::field(bytes, 64)),
                grant: u64::from_be_bytes(super::field(bytes, 68)),
                address: u64::from_be_bytes(super::field(bytes, 76)),
                key: u32::from_be_bytes(super::field(bytes, 84)),
                metadata: u16::from_be_bytes(super::field(bytes, 88)),
            };
            if matches!(f.kind, 5 | 6) {
                if bytes.len() != HEADER
                    || f.request != 0
                    || f.value != [0; 32]
                    || f.len != 0
                    || f.grant != 0
                    || f.address != 0
                    || f.key != 0
                    || f.metadata != 0
                {
                    return Err(protocol_error());
                }
            } else if bytes.len() != HEADER + usize::from(f.metadata)
                || f.len == 0
                || f.len as usize > BUFFER_SIZE
                || f.request == 0
                || !matches!(f.kind, 1..=4 | 7)
            {
                return Err(protocol_error());
            }
            Ok(f)
        }
        pub(crate) fn request_valid(&self, last: u64) -> bool {
            self.request > last && self.grant == 0 && self.address == 0 && self.key == 0
        }
        pub(crate) fn grant_valid(&self) -> bool {
            self.metadata == 8
                && self.grant != 0
                && self.address != 0
                && self.address.checked_add(self.len as u64).is_some()
        }
        pub(crate) fn negative_valid(&self) -> bool {
            self.grant == 0
                && self.address == 0
                && self.key == 0
                && usize::from(self.metadata) == 32 + crate::rdma::PeerFailure::LEN
        }
        pub(crate) fn same_value(&self, other: &Self) -> bool {
            self.value == other.value && self.len == other.len
        }
        pub(crate) fn acknowledges(&self, grant: &Self) -> bool {
            self.same_value(grant)
                && self.grant == grant.grant
                && self.address == grant.address
                && self.key == grant.key
                && self.metadata == 8
        }
    }
}

#[repr(C)]
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub(crate) struct Endpoint {
    pub gid: [u8; 16],
    pub qpn: u32,
    pub psn: u32,
    pub lid: u16,
    pub mtu: u8,
    pub ethernet: u8,
}
/// Untrusted QP parameters. Parsing never confers transport authority.
#[derive(Clone, Debug)]
pub struct Offer {
    pub(crate) version: u32,
    pub(crate) fabric: String,
    pub(crate) nonce: [u8; 16],
    pub(crate) challenge: [u8; 16],
    pub(crate) endpoint: Endpoint,
    pub(crate) rail: u32,
    pub(crate) rails: u32,
    pub(crate) reads: u8,
}
impl Offer {
    pub fn encode(&self) -> Vec<u8> {
        let mut v = Vec::with_capacity(76 + self.fabric.len());
        v.extend_from_slice(&self.version.to_be_bytes());
        v.extend_from_slice(&self.nonce);
        v.extend_from_slice(&self.challenge);
        v.extend_from_slice(&self.endpoint.gid);
        v.extend_from_slice(&self.endpoint.qpn.to_be_bytes());
        v.extend_from_slice(&self.endpoint.psn.to_be_bytes());
        v.extend_from_slice(&self.endpoint.lid.to_be_bytes());
        v.extend_from_slice(&[self.endpoint.mtu, self.endpoint.ethernet, self.reads, 0]);
        v.extend_from_slice(&self.rail.to_be_bytes());
        v.extend_from_slice(&self.rails.to_be_bytes());
        v.extend_from_slice(&(self.fabric.len() as u16).to_be_bytes());
        v.extend_from_slice(self.fabric.as_bytes());
        v
    }
    pub fn decode(bytes: &[u8]) -> io::Result<Self> {
        if bytes.len() < 76 || u32::from_be_bytes(field(bytes, 0)) != 2 {
            return Err(invalid("invalid RDMA offer"));
        }
        let len = u16::from_be_bytes(field(bytes, 74)) as usize;
        if bytes.len() != 76 + len || len == 0 || bytes[65] != 0 {
            return Err(invalid("invalid RDMA offer length"));
        }
        let offer = Self {
            version: 2,
            nonce: field(bytes, 4),
            challenge: field(bytes, 20),
            fabric: String::from_utf8(bytes[76..].to_vec())
                .map_err(|_| invalid("invalid fabric"))?,
            endpoint: Endpoint {
                gid: field(bytes, 36),
                qpn: u32::from_be_bytes(field(bytes, 52)),
                psn: u32::from_be_bytes(field(bytes, 56)),
                lid: u16::from_be_bytes(field(bytes, 60)),
                mtu: bytes[62],
                ethernet: bytes[63],
            },
            reads: bytes[64],
            rail: u32::from_be_bytes(field(bytes, 66)),
            rails: u32::from_be_bytes(field(bytes, 70)),
        };
        FabricId::new(&offer.fabric)?;
        RailId::new(offer.rail, offer.rails)?;
        if offer.endpoint.qpn == 0
            || offer.endpoint.qpn > 0xffffff
            || offer.endpoint.psn > 0xffffff
            || !(1..=5).contains(&offer.endpoint.mtu)
            || offer.endpoint.ethernet > 1
            || offer.reads == 0
            || offer.nonce == [0; 16]
        {
            return Err(invalid("invalid RDMA endpoint"));
        }
        Ok(offer)
    }
    pub fn fabric(&self) -> &str {
        &self.fabric
    }
    pub fn challenge(&self) -> [u8; 16] {
        self.challenge
    }
    pub fn rail_index(&self) -> usize {
        self.rail as usize
    }
    pub fn rail_count(&self) -> usize {
        self.rails as usize
    }
}

/// Affine authorization minted only after the TLS peer and selected route match.
pub struct AuthenticatedOffer(Offer, [u8; 32]);
impl AuthenticatedOffer {
    pub(crate) fn into_parts(self) -> (Offer, [u8; 32]) {
        (self.0, self.1)
    }
}
struct Outgoing {
    tag: u64,
    bytes: Vec<u8>,
    offset: usize,
    deadline: Instant,
}
/// Bounded framed full-duplex channel owned by exactly one RDMA connection.
pub struct ControlChannel {
    tls: Option<client::TlsChannel>,
    binding: [u8; 32],
    provider: Option<Arc<Provider>>,
    revision: u64,
    membership: Option<(Context, crate::tls::PeerIdentity, NodeId)>,
    deadline: Instant,
    drain: Cell<Option<Instant>>,
    closed: bool,
    outgoing: VecDeque<Outgoing>,
    sent: VecDeque<u64>,
    received: VecDeque<Vec<u8>>,
    input: Vec<u8>,
    expected: usize,
    frame_deadline: Option<Instant>,
    #[cfg(test)]
    simulated: bool,
    #[cfg(test)]
    simulated_tls: Option<crate::tls::SimulatedSession>,
}
impl ControlChannel {
    #[cfg(test)]
    pub(crate) fn is_simulated(&self) -> bool {
        self.simulated
    }
    fn new(
        tls: client::TlsChannel,
        binding: [u8; 32],
        context: &Context,
        peer: NodeId,
    ) -> io::Result<Self> {
        context.authorize(tls.peer_identity(), peer)?;
        let identity = tls.peer_identity().unwrap().clone();
        let provider = context.credentials();
        let revision = tls.revision();
        Ok(Self {
            tls: Some(tls),
            binding,
            provider,
            revision,
            membership: Some((context.clone(), identity, peer)),
            deadline: crate::environment::now() + LIFETIME,
            drain: Cell::new(None),
            closed: false,
            outgoing: VecDeque::new(),
            sent: VecDeque::new(),
            received: VecDeque::new(),
            input: Vec::with_capacity(MAX_CONTROL + 36),
            expected: 4,
            frame_deadline: None,
            #[cfg(test)]
            simulated: false,
            #[cfg(test)]
            simulated_tls: None,
        })
    }
    pub(crate) fn matches_transport(&self, binding: &[u8; 32]) -> io::Result<()> {
        if !self.healthy() || &self.binding != binding {
            return Err(invalid("foreign or expired TLS control channel"));
        }
        Ok(())
    }
    pub(crate) fn healthy(&self) -> bool {
        let now = crate::environment::now();
        if self.closed || now >= self.deadline {
            return false;
        }
        if let Some(provider) = &self.provider {
            if provider.current().revision != self.revision && self.drain.get().is_none() {
                // Rotation closes admission only. Slot deadlines remain the
                // authority for accepted RPCs; no replacement-relative cutoff.
                self.drain.set(Some(self.deadline));
            }
        }
        let tls_expired = self
            .tls
            .as_ref()
            .is_some_and(|tls| !tls.admits_new_request());
        #[cfg(test)]
        let tls_expired = tls_expired
            || self
                .simulated_tls
                .is_some_and(|tls| tls.admission.expired(u64::MAX));
        if (now >= self.deadline - DRAIN || tls_expired) && self.drain.get().is_none() {
            self.drain.set(Some(self.deadline));
        }
        !self.drain.get().is_some_and(|until| now >= until)
    }
    pub(crate) fn admitting(&self) -> bool {
        self.healthy()
            && self.drain.get().is_none()
            && self
                .membership
                .as_ref()
                .is_none_or(|(context, identity, node)| {
                    context.authorize(Some(identity), *node).is_ok()
                })
    }
    pub(crate) fn rejection(&self, metadata: &[u8]) -> rdma::PeerFailure {
        use crate::http_client::attempt::PeerReason;
        let mut failure = rdma::PeerFailure {
            identity: [0; 32],
            candidate: 0,
            reason: PeerReason::Unavailable,
            evidence: None,
        };
        match crate::cache::peer_wire::routed_descriptor(metadata) {
            Ok((Some(cursor), descriptor)) => {
                if let Some((context, _, _)) = &self.membership
                    && let Some(routing) = context.prepared.routing_for_volume(&context.volume_id)
                    && routing.validate(&cursor, descriptor.target()).is_ok()
                {
                    failure.identity = cursor.identity;
                    failure.candidate = routing.destination(&cursor);
                } else {
                    failure.reason = PeerReason::Protocol;
                }
            }
            Ok((None, _)) => {}
            Err(_) => failure.reason = PeerReason::Protocol,
        }
        failure
    }
    pub(crate) fn enqueue(&mut self, tag: u64, frame: &[u8]) -> io::Result<()> {
        if !self.healthy() {
            return Err(io::ErrorKind::ConnectionAborted.into());
        }
        if frame.len() < control_wire::HEADER || frame.len() > MAX_CONTROL {
            return Err(invalid("invalid TLS control length"));
        }
        if self.outgoing.len() + self.sent.len() >= CHANNEL_DEPTH {
            return Err(control_wire::capacity_error());
        }
        let mut bytes = Vec::with_capacity(frame.len() + 36);
        bytes.extend_from_slice(&((frame.len() + 32) as u32).to_be_bytes());
        bytes.extend_from_slice(&self.binding);
        bytes.extend_from_slice(frame);
        self.outgoing.push_back(Outgoing {
            tag,
            bytes,
            offset: 0,
            deadline: crate::environment::now() + Duration::from_secs(5),
        });
        Ok(())
    }
    pub(crate) fn take_sent(&mut self) -> Option<u64> {
        self.sent.pop_front()
    }
    pub(crate) fn take_received(&mut self) -> Option<Vec<u8>> {
        self.received.pop_front()
    }
    pub(crate) fn close(&mut self) {
        self.closed = true;
        self.tls.take();
        self.outgoing.clear();
        self.sent.clear();
        self.received.clear();
    }
    pub(crate) fn poll(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Work> {
        if !self.healthy() {
            return Err(io::ErrorKind::ConnectionAborted.into());
        }
        #[cfg(test)]
        if self.simulated {
            return Ok(Work {
                runnable: false,
                deadline: Some(self.deadline),
            });
        }
        let mut work = Work {
            runnable: false,
            deadline: Some(self.deadline.min(self.drain.get().unwrap_or(self.deadline))),
        };
        for _ in 0..budget {
            let Some(front) = self.outgoing.front_mut() else {
                break;
            };
            let deadline = front.deadline.min(self.deadline);
            work.deadline = Some(work.deadline.unwrap().min(deadline));
            match self.tls.as_mut().unwrap().poll_write(
                ring,
                &front.bytes[front.offset..],
                deadline,
            )? {
                Progress::Pending(w) => {
                    work.runnable |= w.runnable;
                    break;
                }
                Progress::Ready(0) => return Err(io::ErrorKind::WriteZero.into()),
                Progress::Ready(n) => {
                    front.offset += n;
                    work.runnable = true;
                    if front.offset == front.bytes.len() {
                        self.sent.push_back(self.outgoing.pop_front().unwrap().tag);
                    }
                }
            }
        }
        for _ in 0..budget {
            if self.received.len() >= CHANNEL_DEPTH {
                break;
            }
            let mut bytes = [0; MAX_CONTROL + 36];
            let deadline = self
                .frame_deadline
                .unwrap_or(self.deadline)
                .min(self.deadline);
            work.deadline = Some(work.deadline.unwrap().min(deadline));
            match self.tls.as_mut().unwrap().poll_read(
                ring,
                &mut bytes[..self.expected - self.input.len()],
                deadline,
            )? {
                Progress::Pending(w) => {
                    work.runnable |= w.runnable;
                    break;
                }
                Progress::Ready(0) => return Err(io::ErrorKind::UnexpectedEof.into()),
                Progress::Ready(n) => {
                    if self.input.is_empty() {
                        self.frame_deadline =
                            Some(crate::environment::now() + Duration::from_secs(5));
                    }
                    self.input.extend_from_slice(&bytes[..n]);
                    work.runnable = true;
                    self.decode_input()?;
                }
            }
        }
        if let Some(deadline) = self.frame_deadline {
            work.deadline = Some(work.deadline.unwrap().min(deadline));
        }
        Ok(work)
    }
    fn decode_input(&mut self) -> io::Result<()> {
        if self.input.len() != self.expected {
            return Ok(());
        }
        if self.expected == 4 {
            let size = u32::from_be_bytes(field(&self.input, 0)) as usize;
            if !(32 + control_wire::HEADER..=32 + MAX_CONTROL).contains(&size) {
                return Err(invalid("oversized TLS control frame"));
            }
            self.expected = size + 4;
        } else {
            if self.input[4..36] != self.binding {
                return Err(invalid("foreign TLS session binding"));
            }
            control_wire::Frame::decode(&self.input[36..])?;
            self.received.push_back(self.input[36..].to_vec());
            self.input.clear();
            self.expected = 4;
            self.frame_deadline = None;
        }
        Ok(())
    }
    #[cfg(test)]
    fn simulated(binding: [u8; 32]) -> Self {
        Self {
            tls: None,
            binding,
            provider: None,
            revision: 0,
            membership: None,
            deadline: crate::environment::now() + LIFETIME,
            drain: Cell::new(None),
            closed: false,
            outgoing: VecDeque::new(),
            sent: VecDeque::new(),
            received: VecDeque::new(),
            input: Vec::new(),
            expected: 4,
            frame_deadline: None,
            simulated: true,
            simulated_tls: None,
        }
    }
}
#[cfg(test)]
pub(crate) fn test_channels(
    a: &Offer,
    b: &Offer,
) -> (
    (AuthenticatedOffer, ControlChannel),
    (AuthenticatedOffer, ControlChannel),
) {
    let mut bytes = a.encode();
    bytes.extend_from_slice(&b.encode());
    bytes.extend_from_slice(&transport_nonce().unwrap());
    let binding = *blake3::hash(&bytes).as_bytes();
    (
        (
            AuthenticatedOffer(b.clone(), binding),
            ControlChannel::simulated(binding),
        ),
        (
            AuthenticatedOffer(a.clone(), binding),
            ControlChannel::simulated(binding),
        ),
    )
}

#[derive(Clone)]
pub struct Rails(Vec<Option<rdma::Transport>>);
impl Rails {
    #[cfg(test)]
    pub(crate) fn test_sources(&self) -> Vec<(usize, rdma::Source)> {
        self.0
            .iter()
            .enumerate()
            .filter_map(|(i, t)| t.as_ref().map(|t| (i, t.test_source())))
            .collect()
    }
    pub fn new(transports: Vec<Option<rdma::Transport>>, total: usize) -> io::Result<Self> {
        if total == 0 || total > MAX_RAILS as usize || transports.len() != total {
            return Err(invalid("invalid physical rail catalog"));
        }
        Ok(Self(transports))
    }
    pub fn total(&self) -> usize {
        self.0.len()
    }
    fn prepare(&self, context: &Context, challenge: [u8; 16]) -> io::Result<rdma::Connecting> {
        let rail = (context.shard % self.total() as u64) as usize;
        self.0[rail]
            .as_ref()
            .ok_or_else(|| {
                io::Error::new(io::ErrorKind::NotConnected, "selected rail unavailable")
            })?
            .prepare_for_fabric(context.fabric().as_str(), challenge, rail, self.total())
    }
}
#[derive(Clone)]
pub struct Context {
    prepared: Arc<Prepared>,
    volume_id: String,
    volume: [u8; 32],
    shard: u64,
    routing: [u8; 32],
    credentials: Option<Arc<Provider>>,
    authority: Rc<RefCell<Option<Arc<Prepared>>>>,
}
impl Context {
    pub fn new(
        prepared: Arc<Prepared>,
        volume: &str,
        shard: u64,
        routing: &[u8],
    ) -> io::Result<Self> {
        if routing.len() > 1024 || prepared.fabric().is_none() {
            return Err(invalid("invalid routing context or disabled fabric"));
        }
        let config = prepared
            .config_snapshot()
            .volumes
            .iter()
            .find(|v| v.id == volume)
            .ok_or_else(|| invalid("unknown volume"))?;
        let mut hash = blake3::Hasher::new_derive_key("racer/rdma/volume/v1");
        hash.update(&(volume.len() as u64).to_be_bytes());
        hash.update(volume.as_bytes());
        hash.update(&config.cache_generation.to_be_bytes());
        let topology = prepared
            .routing_for_volume(volume)
            .ok_or_else(|| invalid("unknown routing volume"))?;
        let route = [routing, topology.identity.as_slice()].concat();
        Ok(Self {
            authority: Rc::new(RefCell::new(Some(prepared.clone()))),
            prepared,
            volume_id: volume.into(),
            volume: *hash.finalize().as_bytes(),
            shard,
            routing: blake3::derive_key("racer/rdma/route/v1", &route),
            credentials: None,
        })
    }
    pub fn with_credentials(mut self, provider: Option<Arc<Provider>>) -> Self {
        self.credentials = provider;
        self
    }
    pub fn credentials(&self) -> Option<Arc<Provider>> {
        self.credentials.clone()
    }
    /// Worker receive authority, updated when topology membership changes.
    pub fn with_authority(mut self, authority: Rc<RefCell<Option<Arc<Prepared>>>>) -> Self {
        self.authority = authority;
        self
    }
    pub fn authority(&self) -> Rc<RefCell<Option<Arc<Prepared>>>> {
        self.authority.clone()
    }
    pub fn prepared(&self) -> &Arc<Prepared> {
        &self.prepared
    }
    pub fn volume_id(&self) -> &str {
        &self.volume_id
    }
    pub fn volume(&self) -> [u8; 32] {
        self.volume
    }
    pub fn shard(&self) -> u64 {
        self.shard
    }
    fn fabric(&self) -> &FabricId {
        self.prepared.fabric().unwrap()
    }
    fn authorize(
        &self,
        identity: Option<&crate::tls::PeerIdentity>,
        node: NodeId,
    ) -> io::Result<()> {
        let authority = self.authority.borrow();
        let prepared = authority
            .as_ref()
            .ok_or_else(|| invalid("no receive authority"))?;
        let peer = prepared
            .eligible_node_for_volume(&self.volume_id, node)
            .ok_or_else(|| invalid("ineligible TLS node"))?;
        let expected = prepared.peer_identity(peer.id())?;
        if identity != Some(&expected) {
            return Err(io::Error::new(
                io::ErrorKind::PermissionDenied,
                "TLS peer certificate membership mismatch",
            ));
        }
        Ok(())
    }
    fn validate(&self, frame: &Frame, remote: NodeId) -> io::Result<()> {
        if frame.node != remote
            || frame.volume != self.volume
            || frame.shard != self.shard
            || frame.routing != self.routing
        {
            return Err(invalid("negotiation identity/context mismatch"));
        }
        Ok(())
    }
    fn frame(&self, reply: bool, offer: &Offer) -> Frame {
        Frame {
            reply,
            node: self.prepared.local_node(),
            volume: self.volume,
            shard: self.shard,
            routing: self.routing,
            offer: offer.clone(),
        }
    }
}
struct Frame {
    reply: bool,
    node: NodeId,
    volume: [u8; 32],
    shard: u64,
    routing: [u8; 32],
    offer: Offer,
}
impl Frame {
    fn bytes(&self) -> Vec<u8> {
        let mut bytes = vec![VERSION, if self.reply { 2 } else { 1 }];
        bytes.extend_from_slice(&self.node.bytes());
        bytes.extend_from_slice(&self.volume);
        bytes.extend_from_slice(&self.shard.to_be_bytes());
        bytes.extend_from_slice(&self.routing);
        bytes.extend_from_slice(&self.offer.encode());
        bytes
    }
    fn encode(&self) -> String {
        hex(&self.bytes())
    }
    fn decode(value: &[u8]) -> io::Result<Self> {
        let bytes = unhex(value)?;
        if bytes.len() < PREFIX || bytes[0] != VERSION || !matches!(bytes[1], 1 | 2) {
            return Err(invalid("invalid negotiation version/header"));
        }
        let offer = Offer::decode(&bytes[PREFIX..])?;
        if offer.challenge() == [0; 16] {
            return Err(invalid("missing offer challenge"));
        }
        Ok(Self {
            reply: bytes[1] == 2,
            node: NodeId::from_bytes(&bytes[2..34])?,
            volume: field(&bytes, 34),
            shard: u64::from_be_bytes(field(&bytes, 66)),
            routing: field(&bytes, 74),
            offer,
        })
    }
}
fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}
fn unhex(value: &[u8]) -> io::Result<Vec<u8>> {
    if value.len() > 2 * (PREFIX + peer_identity::MAX_OFFER_LEN) || value.len() % 2 != 0 {
        return Err(invalid("oversized/odd negotiation header"));
    }
    fn digit(b: u8) -> io::Result<u8> {
        match b {
            b'0'..=b'9' => Ok(b - b'0'),
            b'a'..=b'f' => Ok(b - b'a' + 10),
            _ => Err(invalid("noncanonical negotiation hex")),
        }
    }
    value
        .chunks_exact(2)
        .map(|p| Ok(digit(p[0])? * 16 + digit(p[1])?))
        .collect()
}
fn parse_fields<'a>(fields: impl Iterator<Item = (&'a str, &'a [u8])>) -> io::Result<Frame> {
    let mut found = None;
    for (name, value) in fields {
        if name.eq_ignore_ascii_case(HEADER) {
            if found.replace(value).is_some() {
                return Err(invalid("duplicate negotiation header"));
            }
        }
    }
    Frame::decode(found.ok_or_else(|| invalid("missing negotiation header"))?)
}
fn validate_offer(
    context: &Context,
    rails: &Rails,
    offer: &Offer,
    challenge: [u8; 16],
) -> io::Result<()> {
    if offer.fabric() != context.fabric().as_str()
        || offer.challenge() != challenge
        || rails_for_shard(context.shard, rails.total(), offer.rail_count())
            != Some((
                (context.shard % rails.total() as u64) as usize,
                offer.rail_index(),
            ))
    {
        return Err(invalid("offer fabric/challenge/rail mismatch"));
    }
    Ok(())
}
fn binding(hello: &Frame, reply: &Frame, target: &str, universe: &[u8]) -> [u8; 32] {
    let mut hash = blake3::Hasher::new_derive_key("racer/rdma/tls-session/v1");
    for bytes in [
        hello.bytes().as_slice(),
        reply.bytes().as_slice(),
        target.as_bytes(),
        universe,
    ] {
        hash.update(&(bytes.len() as u64).to_be_bytes());
        hash.update(bytes);
    }
    *hash.finalize().as_bytes()
}
pub fn is_negotiation(headers: Headers<'_>) -> bool {
    headers.iter().any(|(n, _)| n.eq_ignore_ascii_case(HEADER))
}
pub struct RequestHint {
    pub node: NodeId,
    pub volume: [u8; 32],
    pub shard: u64,
    pub is_finish: bool,
}
pub fn request_hint(headers: Headers<'_>) -> io::Result<RequestHint> {
    let f = parse_fields(headers.iter())?;
    if f.reply {
        return Err(invalid("expected offer request"));
    }
    Ok(RequestHint {
        node: f.node,
        volume: f.volume,
        shard: f.shard,
        is_finish: false,
    })
}
fn fresh(deadline: Instant, now: Instant) -> io::Result<()> {
    if now >= deadline {
        Err(io::ErrorKind::TimedOut.into())
    } else {
        Ok(())
    }
}
fn timeout(value: Duration) -> io::Result<()> {
    if value.is_zero() || value > Duration::from_secs(60) {
        Err(invalid("negotiation timeout must be in (0,60s]"))
    } else {
        Ok(())
    }
}
#[must_use]
pub struct Established {
    pub connection: rdma::Connection,
    pub context: Rc<Context>,
    pub peer: NodeId,
    pub confirmation_deadline: Option<Instant>,
}
enum ClientState {
    Reply(client::HeadExchange, rdma::Connecting, Frame),
    Confirming(rdma::Connection),
    Done,
}
#[must_use]
pub struct Client {
    context: Rc<Context>,
    rails: Rails,
    peer: NodeId,
    target: String,
    deadline: Instant,
    state: ClientState,
}
impl Client {
    pub fn start(
        context: Rc<Context>,
        rails: Rails,
        peer_id: &str,
        target: &str,
        duration: Duration,
    ) -> io::Result<Self> {
        timeout(duration)?;
        let peer = context
            .prepared
            .eligible_peer_for_volume(context.volume_id(), peer_id)
            .ok_or_else(|| invalid("outbound peer is not eligible for volume"))?;
        let remote = peer.node();
        let deadline = crate::environment::now() + duration;
        let provider = context
            .credentials()
            .ok_or_else(|| invalid("RDMA requires TLS credentials"))?;
        let snapshot = provider.current();
        let make_connection = || {
            client::Connection::new_tls(
                peer.endpoint().address(),
                peer.endpoint().host(),
                &snapshot.context,
                crate::tls::ExpectedPeer::Identity(context.prepared.peer_identity(peer_id)?),
            )
        };
        #[cfg(test)]
        let mut connection = if crate::simulation::current().is_some() {
            client::Connection::new_simulated_peer(
                peer.endpoint().address(),
                peer.endpoint().host(),
                provider.identity().clone(),
                context.prepared.peer_identity(peer_id)?,
            )?
        } else {
            make_connection()?
        };
        #[cfg(not(test))]
        let mut connection = make_connection()?;
        connection.set_tls_revision(snapshot.revision, snapshot.expires_unix);
        let qp = rails.prepare(&context, transport_nonce()?)?;
        let hello = context.frame(false, qp.offer());
        let encoded = hello.encode();
        let exchange = connection.head(
            client::Request::new(
                target,
                &[(HEADER, &encoded), ("X-Racer-Volume", context.volume_id())],
            )?,
            deadline,
        )?;
        Ok(Self {
            context,
            rails,
            peer: remote,
            target: target.into(),
            deadline,
            state: ClientState::Reply(exchange, qp, hello),
        })
    }
    pub fn poll(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Progress<Established>> {
        let result = self.poll_inner(ring, budget);
        if result.is_err() {
            self.state = ClientState::Done;
        }
        result
    }
    fn poll_inner(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Progress<Established>> {
        fresh(self.deadline, crate::environment::now())?;
        if let ClientState::Confirming(connection) = &self.state {
            if !connection.is_healthy() {
                return Err(invalid("RDMA confirmation failed"));
            }
            if !connection.is_confirmed() {
                return Ok(Progress::Pending(Work {
                    runnable: false,
                    deadline: Some(self.deadline),
                }));
            }
            let ClientState::Confirming(connection) =
                std::mem::replace(&mut self.state, ClientState::Done)
            else {
                unreachable!()
            };
            return Ok(Progress::Ready(Established {
                connection,
                context: self.context.clone(),
                peer: self.peer,
                confirmation_deadline: None,
            }));
        }
        let ClientState::Reply(exchange, _, _) = &mut self.state else {
            return Err(invalid("negotiation already finished"));
        };
        let response = match exchange.poll(ring, budget)? {
            Progress::Pending(w) => return Ok(Progress::Pending(w)),
            Progress::Ready(r) => r,
        };
        if response.status() != 200 || response.content_length() != Some(0) {
            return Err(invalid("negotiation response rejected"));
        }
        let reply = parse_fields(response.headers().iter())?;
        self.context.validate(&reply, self.peer)?;
        let ClientState::Reply(_, qp, hello) =
            std::mem::replace(&mut self.state, ClientState::Done)
        else {
            unreachable!()
        };
        if !reply.reply {
            return Err(invalid("expected offer reply"));
        }
        validate_offer(
            &self.context,
            &self.rails,
            &reply.offer,
            hello.offer.challenge(),
        )?;
        let binding = binding(
            &hello,
            &reply,
            &self.target,
            &self.context.prepared.config_snapshot().universe,
        );
        let tcp = response
            .recycle()
            .ok_or_else(|| invalid("offer response closed TCP"))?;
        #[cfg(test)]
        let channel = if crate::simulation::current().is_some() {
            self.context
                .authorize(tcp.peer_identity().as_ref(), self.peer)?;
            let identity = tcp.peer_identity().unwrap();
            let captured = tcp.simulated_tls();
            drop(tcp);
            let mut channel = ControlChannel::simulated(binding);
            channel.membership = Some(((*self.context).clone(), identity, self.peer));
            channel.provider = self.context.credentials();
            channel.revision = captured.revision;
            channel.simulated_tls = Some(captured);
            channel
        } else {
            ControlChannel::new(tcp.into_tls_channel()?, binding, &self.context, self.peer)?
        };
        #[cfg(not(test))]
        let channel =
            ControlChannel::new(tcp.into_tls_channel()?, binding, &self.context, self.peer)?;
        let connection = qp.connect_authenticated(
            AuthenticatedOffer(reply.offer, binding),
            channel,
            self.context.shard,
        )?;
        connection.begin_confirmation(true, self.deadline)?;
        self.state = ClientState::Confirming(connection);
        Ok(Progress::Pending(Work {
            runnable: true,
            deadline: Some(self.deadline),
        }))
    }
}

struct Lease(Rc<Cell<usize>>);
impl Drop for Lease {
    fn drop(&mut self) {
        self.0.set(self.0.get() - 1);
    }
}
struct Queued {
    established: Established,
    deadline: Instant,
    _lease: Lease,
}
#[derive(Default)]
struct Store {
    completed: VecDeque<Queued>,
}
#[must_use]
pub struct Task {
    store: Rc<RefCell<Store>>,
    sending: Option<http::SendingHeadHeaders>,
    connection: Option<rdma::Connected>,
    context: Rc<Context>,
    peer: NodeId,
    binding: [u8; 32],
    lease: Option<Lease>,
    deadline: Instant,
    replacement: Option<Rc<rdma::Connection>>,
}
pub struct Server {
    context: Rc<Context>,
    rails: Rails,
    duration: Duration,
    capacity: usize,
    leases: Rc<Cell<usize>>,
    store: Rc<RefCell<Store>>,
    replacement: Option<Rc<rdma::Connection>>,
}
impl Server {
    pub fn new(
        context: Rc<Context>,
        rails: Rails,
        capacity: usize,
        duration: Duration,
    ) -> io::Result<Self> {
        timeout(duration)?;
        if capacity == 0 {
            return Err(invalid("zero negotiation capacity"));
        }
        Ok(Self {
            context,
            rails,
            duration,
            capacity,
            leases: Rc::new(Cell::new(0)),
            store: Rc::new(RefCell::new(Store::default())),
            replacement: None,
        })
    }
    pub(crate) fn replacing(mut self, connection: Rc<rdma::Connection>) -> Self {
        self.replacement = Some(connection);
        self
    }
    pub fn start(&mut self, request: http::Request) -> io::Result<Task> {
        let now = crate::environment::now();
        self.poll(now);
        let hello = parse_fields(request.headers().iter())?;
        if hello.reply {
            return Err(invalid("expected offer request"));
        }
        self.context.validate(&hello, hello.node)?;
        self.context
            .authorize(request.peer_identity(), hello.node)?;
        let http::Request::Head(request) = request else {
            return Err(invalid("negotiation requires HEAD"));
        };
        if self.leases.get() >= self.capacity {
            return Err(control_wire::capacity_error());
        }
        validate_offer(
            &self.context,
            &self.rails,
            &hello.offer,
            hello.offer.challenge(),
        )?;
        self.leases.set(self.leases.get() + 1);
        let lease = Lease(self.leases.clone());
        let qp = self.rails.prepare(&self.context, hello.offer.challenge())?;
        let reply = self.context.frame(true, qp.offer());
        let binding = binding(
            &hello,
            &reply,
            request.target(),
            &self.context.prepared.config_snapshot().universe,
        );
        let connection =
            qp.connect(AuthenticatedOffer(hello.offer, binding), self.context.shard)?;
        let encoded = reply.encode();
        let sending = request.respond(http::ResponseHead::new(
            200,
            Some(0),
            &[(HEADER, encoded.as_bytes())],
        )?)?;
        Ok(Task {
            store: self.store.clone(),
            sending: Some(sending),
            connection: Some(connection),
            context: self.context.clone(),
            peer: hello.node,
            binding,
            lease: Some(lease),
            deadline: now + self.duration,
            replacement: self.replacement.clone(),
        })
    }
    pub fn poll_task(
        &mut self,
        task: &mut Task,
        ring: &mut Ring,
        budget: usize,
    ) -> io::Result<Progress<http::Completed>> {
        if !Rc::ptr_eq(&task.store, &self.store) {
            return Err(invalid("foreign negotiation task"));
        }
        let result = (|| {
            fresh(task.deadline, crate::environment::now())?;
            let mut done = match task
                .sending
                .as_mut()
                .ok_or_else(|| invalid("completed negotiation task"))?
                .poll(ring, budget)?
            {
                Progress::Pending(mut w) => {
                    w.deadline = Some(w.deadline.map_or(task.deadline, |d| d.min(task.deadline)));
                    return Ok(Progress::Pending(w));
                }
                Progress::Ready(done) => done,
            };
            task.sending.take();
            let tcp = done
                .take_connection()
                .ok_or_else(|| invalid("offer reply closed TCP"))?;
            #[cfg(test)]
            let channel = if crate::simulation::current().is_some() {
                task.context.authorize(tcp.peer_identity(), task.peer)?;
                let identity = tcp.peer_identity().unwrap().clone();
                let captured = tcp.simulated_tls();
                drop(tcp);
                let mut channel = ControlChannel::simulated(task.binding);
                channel.membership = Some(((*task.context).clone(), identity, task.peer));
                channel.provider = task.context.credentials();
                channel.revision = captured.revision;
                channel.simulated_tls = Some(captured);
                channel
            } else {
                ControlChannel::new(
                    tcp.into_tls_channel()?,
                    task.binding,
                    &task.context,
                    task.peer,
                )?
            };
            #[cfg(not(test))]
            let channel = ControlChannel::new(
                tcp.into_tls_channel()?,
                task.binding,
                &task.context,
                task.peer,
            )?;
            let connection = task
                .connection
                .take()
                .unwrap()
                .authenticate_channel(channel)?;
            connection.begin_confirmation(false, task.deadline)?;
            // The runtime retains the predecessor until outstanding DMA retires.
            task.replacement.take();
            self.store.borrow_mut().completed.push_back(Queued {
                established: Established {
                    connection,
                    context: task.context.clone(),
                    peer: task.peer,
                    confirmation_deadline: Some(task.deadline),
                },
                deadline: task.deadline,
                _lease: task.lease.take().unwrap(),
            });
            Ok(Progress::Ready(done))
        })();
        if result.is_err() {
            task.sending.take();
            task.connection.take();
            task.lease.take();
        }
        result
    }
    pub fn poll(&mut self, now: Instant) -> Work {
        let mut store = self.store.borrow_mut();
        store.completed.retain(|q| now < q.deadline);
        Work {
            runnable: false,
            deadline: store.completed.iter().map(|q| q.deadline).min(),
        }
    }
    pub fn take_completed(&mut self, now: Instant) -> Option<Established> {
        self.poll(now);
        Some(self.store.borrow_mut().completed.pop_front()?.established)
    }
    pub fn reserved(&self) -> usize {
        self.leases.get()
    }
    pub fn has_pending(&self, _: &http::ConnectionId) -> bool {
        false
    }
    pub fn clear(&mut self) {
        *self.store.borrow_mut() = Store::default();
    }
}
impl Drop for Server {
    fn drop(&mut self) {
        self.clear();
    }
}

#[cfg(test)]
pub(crate) fn test_confirmation_admission() {
    tests::confirmation_admission();
}
#[cfg(test)]
impl rdma::Connection {
    pub(crate) fn test_pump(&self, remote: &Self, corrupt: bool) -> usize {
        let (transport, local) = self.test_endpoint();
        let (other, peer) = remote.test_endpoint();
        transport.test_progress(32).unwrap();
        let mut reads = 0;
        for post in local.posts() {
            gate(post, None);
            if !local.effect(&peer, post, corrupt).unwrap() {
                break;
            }
            reads += usize::from(post.opcode == 3);
            local.complete(post, 0).unwrap();
            for receive in peer.receives() {
                peer.complete(receive, 0).unwrap();
            }
            other.test_progress(32).unwrap();
            transport.test_progress(32).unwrap();
        }
        reads
    }
}
#[cfg(test)]
pub(crate) fn test_confirmations(rails: &Rails) {
    for transport in rails.0.iter().flatten() {
        transport.test_progress(32).unwrap();
        let qps = rdma::test_qps();
        for local in qps.iter().filter(|q| q.belongs_to(transport)) {
            let Some(peer) = qps.iter().find(|q| local.pairs_with(q)) else {
                continue;
            };
            for post in local
                .posts()
                .into_iter()
                .filter(|p| matches!(p.kind, 5 | 6))
            {
                if local.effect(peer, post, false).unwrap() {
                    local.complete(post, 0).unwrap();
                    for receive in peer.receives() {
                        peer.complete(receive, 0).unwrap();
                    }
                }
            }
        }
        transport.test_progress(32).unwrap();
    }
}
#[cfg(test)]
pub(crate) fn gate(
    post: rdma::TestPost,
    edge: Option<(usize, std::net::SocketAddr)>,
) -> Option<bool> {
    if post.opcode != 1 {
        return None;
    }
    let w = crate::simulation::current()?;
    let target = w.request_target(&post.value)?;
    if post.kind == 4 {
        w.event("rdma-negative", &target, format!("wr={}", post.id));
    }
    if post.kind != 1 {
        return None;
    }
    let (node, endpoint) = edge?;
    let result = w.intercept(
        Some(node),
        endpoint,
        &target,
        crate::simulation::Phase::RdmaRequest,
    );
    let name = if result.as_ref().is_some_and(Option::is_some) {
        "rdma-refused"
    } else {
        "rdma-deliver"
    };
    if result.is_none() || result.as_ref().is_some_and(Option::is_some) {
        w.event(name, &target, format!("from={node} to={endpoint}"));
    }
    result.map(|r| r.is_some())
}

pub mod peer_identity {
    use super::invalid;
    use std::{fmt, io, str::FromStr};
    pub const MAX_FABRIC_LEN: usize = 256;
    pub const MAX_RAILS: u32 = 256;
    pub const MAX_OFFER_LEN: usize = 76 + MAX_FABRIC_LEN;
    pub const MAX_HANDSHAKE_LEN: usize = MAX_OFFER_LEN;
    pub const MAX_AUTHORITY_LEN: usize = 1024;
    #[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
    pub struct NodeId([u8; 32]);
    impl NodeId {
        pub fn from_bytes(bytes: &[u8]) -> io::Result<Self> {
            Ok(Self(
                bytes
                    .try_into()
                    .map_err(|_| invalid("node identity must be 32 bytes"))?,
            ))
        }
        pub fn bytes(self) -> [u8; 32] {
            self.0
        }
    }
    impl FromStr for NodeId {
        type Err = io::Error;
        fn from_str(value: &str) -> io::Result<Self> {
            if value.len() != 64 || !value.bytes().all(|b| b.is_ascii_hexdigit()) {
                return Err(invalid("node identity must be exactly 64 hex digits"));
            }
            let mut bytes = [0; 32];
            for (i, b) in bytes.iter_mut().enumerate() {
                *b = u8::from_str_radix(&value[2 * i..2 * i + 2], 16).unwrap();
            }
            Ok(Self(bytes))
        }
    }
    impl fmt::Display for NodeId {
        fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
            for b in self.0 {
                write!(f, "{b:02x}")?;
            }
            Ok(())
        }
    }
    #[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
    pub struct FabricId(String);
    impl FabricId {
        pub fn new(value: &str) -> io::Result<Self> {
            if value.is_empty()
                || value.len() > MAX_FABRIC_LEN
                || !value.bytes().all(|b| (33..=126).contains(&b))
            {
                return Err(invalid("fabric must be 1..=256 visible ASCII bytes"));
            }
            Ok(Self(value.into()))
        }
        pub fn as_str(&self) -> &str {
            &self.0
        }
    }
    #[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
    pub struct RailId {
        index: u32,
        count: u32,
    }
    impl RailId {
        pub fn new(index: u32, count: u32) -> io::Result<Self> {
            if count == 0 || count > MAX_RAILS || index >= count {
                return Err(invalid("invalid rail index/count"));
            }
            Ok(Self { index, count })
        }
        pub fn index(self) -> u32 {
            self.index
        }
        pub fn count(self) -> u32 {
            self.count
        }
    }
}
#[cfg(test)]
include!(concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/tests/security/negotiation.rs"
));

#[cfg(test)]
#[path = "../tests/security/simulated_negotiation.rs"]
mod simulated_tls_tests;
