// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Authenticated RDMA negotiation over two HTTP HEADs on the same TCP connection.
//! Dispatch [`is_negotiation`] for every target before ordinary data dispatch;
//! untrusted [`request_hint`] selects only an existing activated [`Context`].
//! Pin that context/server through Finish and connection lifetime, preserve sparse
//! [`Rails`] indexes, drive worker-owned RDMA Sources, and merge HTTP/server deadlines.
//! The responder installs authentication before Ready; the initiator verifies Ready
//! before activation. Admit [`Established`] by generation and enforce its inbound
//! confirmation deadline. `has_pending` routes draining same-TCP continuations.
//!
//! Wire: one case-insensitive X-Racer-Rdma header, canonical lowercase hex of
//! `version:u8, kind:u8, sender:[u8;32], volume:[u8;32], shard:BE-u64,`
//! `route-digest:[u8;32]`, then Hello/Reply/Finish/SignedControl Ready. Success is
//! HEAD 200, Content-Length: 0. The transcript binds identities, universe, epoch,
//! version, cache generation, shard, route, exact target and both complete offers.
//! Ready signs the domain, challenge and request ID zero. Failure selects HTTP.
use crate::{
    control::Prepared,
    crypto::{self, auth},
    http::{Headers, Progress},
    http_client as client, http_server as http,
    peer_identity::{FabricId, MAX_HANDSHAKE_LEN, MAX_RAILS, NodeId, RailId},
    rdma,
    uring::{Ring, Work},
};
use std::{
    cell::{Cell, RefCell},
    collections::{HashMap, VecDeque},
    io,
    rc::Rc,
    sync::Arc,
    time::{Duration, Instant},
};

pub const HEADER: &str = "X-Racer-Rdma";
// Call only after checking the enclosing fixed-size wire prefix.
fn field<const N: usize>(bytes: &[u8], offset: usize) -> [u8; N] {
    bytes[offset..offset + N].try_into().unwrap()
}
pub(crate) fn transport_nonce() -> io::Result<[u8; 16]> {
    let mut bytes = [0; 16];
    crate::environment::random(&mut bytes).map_err(|e| io::Error::other(e.to_string()))?;
    Ok(bytes)
}
/// Symmetric shard mapping, including unequal rail counts.
pub fn rails_for_shard(shard: u64, local: usize, remote: usize) -> Option<(usize, usize)> {
    if local == 0 || remote == 0 {
        None
    } else {
        Some((
            (shard % local as u64) as usize,
            (shard % remote as u64) as usize,
        ))
    }
}
/// Worker-local transport capacity and negotiation deadline policy.
#[derive(Clone, Debug)]
pub struct TransportConfig {
    /// Legacy default for `prepare`; explicit per-negotiation fabric may override it.
    pub fabric: String,
    pub connections: usize,
    /// Concurrent RPCs in each direction on a connection.
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
            || crate::environment::now()
                .checked_add(self.timeout)
                .is_none()
        {
            return Err(control_wire::argument_error());
        }
        Ok(())
    }
}
#[cfg(test)]
impl rdma::Connection {
    /// One compatibility NIC turn; corruption changes destination bytes only.
    // The cluster uses transport registry events, including pre-admission QPs.
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
/// Pure control-body codec. Parsed fields are untrusted until the transport
/// verifies the signed envelope, session, request and outstanding capability.
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
            out[..4].copy_from_slice(b"RCR3");
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
            let bad = || invalid("invalid RDMA control body");
            if bytes.len() < HEADER
                || &bytes[..4] != b"RCR3"
                || bytes[5..8]
                    .iter()
                    .chain(&bytes[90..HEADER])
                    .any(|b| *b != 0)
            {
                return Err(bad());
            }
            let f = Self {
                kind: bytes[4],
                session: bytes[8..24].try_into().unwrap(),
                request: u64::from_be_bytes(super::field(bytes, 24)),
                value: super::field(bytes, 32),
                len: u32::from_be_bytes(super::field(bytes, 64)),
                grant: u64::from_be_bytes(super::field(bytes, 68)),
                address: u64::from_be_bytes(super::field(bytes, 76)),
                key: u32::from_be_bytes(super::field(bytes, 84)),
                metadata: u16::from_be_bytes(super::field(bytes, 88)),
            };
            if bytes.len() != HEADER + usize::from(f.metadata)
                || f.len == 0
                || f.len as usize > BUFFER_SIZE
                || f.request == 0
                || !(1..=4).contains(&f.kind)
            {
                return Err(bad());
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
    type ControlAuth = (crate::crypto::auth::Session, crate::crypto::Snapshot);
    pub(crate) fn verify(
        wire: &[u8],
        auth: Option<&mut ControlAuth>,
        required: bool,
    ) -> io::Result<(Frame, Vec<u8>)> {
        let body = if let Some((session, snapshot)) = auth {
            let signed = crate::crypto::auth::SignedControl::decode(wire)?;
            let verified = session.verify(snapshot, signed)?;
            let mut body = verified.body().to_vec();
            if body.len() < HEADER || &body[..4] != b"RCR4" {
                return Err(invalid("unsigned control body"));
            }
            body[..4].copy_from_slice(b"RCR3");
            if Frame::decode(&body)?.request != verified.request_id() {
                return Err(invalid("control correlation"));
            }
            body
        } else {
            if required {
                return Err(invalid("missing control authentication"));
            }
            wire.to_vec()
        };
        let frame = Frame::decode(&body)?;
        Ok((frame, body))
    }
}
const VERSION: u8 = 2;
const PREFIX: usize = 106; // version, kind, sender, volume, shard, route digest
const MAX_FRAME: usize = PREFIX + MAX_HANDSHAKE_LEN;
const READY: &[u8] = b"racer/rdma/ready/v2";
const MAX_TIMEOUT: Duration = Duration::from_secs(60);

// C POD shared with the verbs shim. This is wire data, never a DMA capability.
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

/// Untrusted negotiation data; `encode()` is the canonical signed transcript.
/// Only `crypto::auth::Session::take_offer` creates transport authorization.
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
        let bad = || invalid("invalid RDMA offer");
        if bytes.len() < 76 || u32::from_be_bytes(field(bytes, 0)) != 2 {
            return Err(bad());
        }
        let len = u16::from_be_bytes(field(bytes, 74)) as usize;
        if bytes.len() != 76 + len || len == 0 || bytes[65] != 0 {
            return Err(bad());
        }
        let offer = Self {
            version: u32::from_be_bytes(field(bytes, 0)),
            fabric: String::from_utf8(bytes[76..].to_vec()).map_err(|_| bad())?,
            nonce: field(bytes, 4),
            challenge: field(bytes, 20),
            endpoint: Endpoint {
                gid: field(bytes, 36),
                qpn: u32::from_be_bytes(field(bytes, 52)),
                psn: u32::from_be_bytes(field(bytes, 56)),
                lid: u16::from_be_bytes(field(bytes, 60)),
                mtu: bytes[62],
                ethernet: bytes[63],
            },
            reads: bytes[64],
            rail: u32::from_be_bytes(bytes[66..70].try_into().unwrap()),
            rails: u32::from_be_bytes(bytes[70..74].try_into().unwrap()),
        };
        if offer.endpoint.qpn == 0
            || offer.endpoint.qpn > 0xffffff
            || offer.endpoint.psn > 0xffffff
            || !(1..=5).contains(&offer.endpoint.mtu)
            || offer.endpoint.ethernet > 1
            || offer.reads == 0
            || offer.rails == 0
            || offer.rail >= offer.rails
            || offer.nonce == [0; 16]
        {
            return Err(bad());
        }
        Ok(offer)
    }
    pub fn fabric(&self) -> &str {
        &self.fabric
    }
    /// Untrusted hint; reading it does not authorize activation.
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

fn invalid(message: &'static str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, message)
}
fn auth_error(error: crypto::Error) -> io::Error {
    io::Error::new(io::ErrorKind::PermissionDenied, error)
}
fn fresh(deadline: Instant, now: Instant) -> io::Result<()> {
    if now >= deadline {
        Err(io::Error::new(
            io::ErrorKind::TimedOut,
            "RDMA negotiation expired",
        ))
    } else {
        Ok(())
    }
}
fn timeout(value: Duration) -> io::Result<()> {
    if value.is_zero() || value > MAX_TIMEOUT {
        return Err(invalid("negotiation timeout must be in (0, 60s]"));
    }
    Ok(())
}

/// One stable discovery catalog; failed/unregistered indexes stay `None`.
/// Clone transports on the owning worker only. Never compact this vector.
#[derive(Clone)]
pub struct Rails(Vec<Option<rdma::Transport>>);
impl Rails {
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
        let (rail, _) = rdma::rails_for_shard(context.shard, self.total(), self.total()).unwrap();
        self.0[rail]
            .as_ref()
            .ok_or_else(|| {
                io::Error::new(io::ErrorKind::NotConnected, "selected rail unavailable")
            })?
            .prepare_for_fabric(context.fabric().as_str(), challenge, rail, self.total())
    }
}

/// Immutable activated volume/shard policy. Peers share canonical routing bytes,
/// shard and cache generation; local revision, DAG, URL and listener may differ.
pub struct Context {
    prepared: Arc<Prepared>,
    volume_id: String,
    volume: [u8; 32],
    shard: u64,
    routing: [u8; 32],
}
impl Context {
    fn authorize(&self, session: &mut auth::Session) -> io::Result<rdma::AuthenticatedOffer> {
        session
            .take_offer(self.snapshot())
            .map_err(auth_error)?
            .ok_or_else(|| invalid("missing authenticated offer"))
    }
    fn activate(
        &self,
        connection: rdma::Connected,
        session: auth::Session,
    ) -> io::Result<rdma::Connection> {
        connection.authenticate_session(session, self.snapshot().clone())
    }
    pub fn new(
        prepared: Arc<Prepared>,
        volume: &str,
        shard: u64,
        routing: &[u8],
    ) -> io::Result<Self> {
        if routing.len() > auth::MAX_ROUTING_CONTEXT || prepared.fabric().is_none() {
            return Err(invalid("invalid routing context or disabled fabric"));
        }
        let config = prepared
            .config_snapshot()
            .volumes
            .iter()
            .find(|v| v.id == volume)
            .ok_or_else(|| invalid("unknown negotiation volume"))?;
        let mut hash = blake3::Hasher::new_derive_key("racer/rdma/volume/v1");
        hash.update(&(volume.len() as u64).to_be_bytes());
        hash.update(volume.as_bytes());
        hash.update(&config.cache_generation.to_be_bytes());
        let topology = prepared
            .routing_for_volume(volume)
            .ok_or_else(|| invalid("unknown routing volume"))?;
        let route_bytes = [routing, topology.identity.as_slice()].concat();
        Ok(Self {
            prepared,
            volume_id: volume.to_owned(),
            volume: *hash.finalize().as_bytes(),
            shard,
            routing: blake3::derive_key("racer/rdma/route/v1", &route_bytes),
        })
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
    fn snapshot(&self) -> &crypto::Snapshot {
        self.prepared.crypto_snapshot()
    }
    fn peers(
        &self,
        remote: NodeId,
        initiator: bool,
        target: &str,
    ) -> io::Result<auth::PeerContext> {
        let mut nodes = [self.prepared.local_node().bytes(), remote.bytes()];
        if !initiator {
            nodes.swap(0, 1);
        }
        // Bind protocol version, canonical route and the exact ordinary target.
        let mut routing = vec![VERSION];
        routing.extend_from_slice(&self.routing);
        routing.extend_from_slice(blake3::hash(target.as_bytes()).as_bytes());
        auth::PeerContext::new(nodes[0], nodes[1])
            .and_then(|p| p.with_negotiation(self.volume, self.shard, &routing))
            .map_err(auth_error)
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
    fn frame(&self, kind: Kind, payload: Vec<u8>) -> Frame {
        Frame {
            kind,
            node: self.prepared.local_node(),
            volume: self.volume,
            shard: self.shard,
            routing: self.routing,
            payload,
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Kind {
    Hello = 1,
    Reply = 2,
    Finish = 3,
    Ready = 4,
}
struct Frame {
    kind: Kind,
    node: NodeId,
    volume: [u8; 32],
    shard: u64,
    routing: [u8; 32],
    payload: Vec<u8>,
}
impl Frame {
    fn encode(&self) -> String {
        let mut bytes = vec![VERSION, self.kind as u8];
        bytes.extend_from_slice(&self.node.bytes());
        bytes.extend_from_slice(&self.volume);
        bytes.extend_from_slice(&self.shard.to_be_bytes());
        bytes.extend_from_slice(&self.routing);
        bytes.extend_from_slice(&self.payload);
        hex(&bytes)
    }
    fn decode(value: &[u8]) -> io::Result<Self> {
        let bytes = unhex(value)?;
        if bytes.len() < PREFIX || bytes[0] != VERSION {
            return Err(invalid("invalid negotiation version/header"));
        }
        let kind = match bytes[1] {
            1 => Kind::Hello,
            2 => Kind::Reply,
            3 => Kind::Finish,
            4 => Kind::Ready,
            _ => return Err(invalid("unknown negotiation message")),
        };
        let payload = &bytes[PREFIX..];
        match kind {
            Kind::Hello | Kind::Reply => {
                handshake_offer(kind, payload)?;
            }
            Kind::Finish if payload.len() == 96 => {}
            Kind::Ready if payload.len() == 112 + READY.len() + 16 => {}
            _ => return Err(invalid("invalid negotiation message length")),
        }
        Ok(Self {
            kind,
            node: NodeId::from_bytes(&bytes[2..34])?,
            volume: bytes[34..66].try_into().unwrap(),
            shard: u64::from_be_bytes(bytes[66..74].try_into().unwrap()),
            routing: bytes[74..106].try_into().unwrap(),
            payload: payload.to_vec(),
        })
    }
}
fn hex(bytes: &[u8]) -> String {
    const DIGITS: &[u8] = b"0123456789abcdef";
    let mut out = String::with_capacity(bytes.len() * 2);
    for &b in bytes {
        out.push(DIGITS[(b >> 4) as usize] as char);
        out.push(DIGITS[(b & 15) as usize] as char);
    }
    out
}
fn unhex(value: &[u8]) -> io::Result<Vec<u8>> {
    if value.len() > 2 * MAX_FRAME || value.len() % 2 != 0 {
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
fn handshake_offer(kind: Kind, payload: &[u8]) -> io::Result<rdma::Offer> {
    if payload.len() < 64 + 77 || payload.len() > MAX_HANDSHAKE_LEN {
        return Err(invalid("invalid bounded handshake"));
    }
    let bytes = match kind {
        Kind::Hello => &payload[64..],
        Kind::Reply => &payload[32..payload.len() - 96],
        _ => return Err(invalid("expected offer handshake")),
    };
    let offer = rdma::Offer::decode(bytes)?;
    FabricId::new(offer.fabric())?;
    RailId::new(offer.rail_index() as u32, offer.rail_count() as u32)?;
    if offer.challenge() == [0; 16] || offer.encode() != bytes {
        return Err(invalid("invalid challenge/noncanonical offer"));
    }
    Ok(offer)
}
fn validate_offer(
    context: &Context,
    rails: &Rails,
    offer: &rdma::Offer,
    challenge: [u8; 16],
) -> io::Result<()> {
    if offer.fabric() != context.fabric().as_str()
        || offer.challenge() != challenge
        || rdma::rails_for_shard(context.shard, rails.total(), offer.rail_count())
            != Some((
                (context.shard % rails.total() as u64) as usize,
                offer.rail_index(),
            ))
    {
        return Err(invalid("offer fabric/challenge/rail mismatch"));
    }
    Ok(())
}
/// Header dispatch only; no URL is reserved. Even malformed negotiation headers
/// must be dispatched here rather than falling through to ordinary object HEAD.
pub fn is_negotiation(headers: Headers<'_>) -> bool {
    headers
        .iter()
        .any(|(name, _)| name.eq_ignore_ascii_case(HEADER))
}

/// Unauthenticated dispatch hints, never authorization or a shard ownership
/// grant. Select only a preconfigured Context; Server revalidates all claims.
pub struct RequestHint {
    pub node: NodeId,
    pub volume: [u8; 32],
    pub shard: u64,
    /// Dispatch only: never route a new Hello to a draining generation.
    pub is_finish: bool,
}
pub fn request_hint(headers: Headers<'_>) -> io::Result<RequestHint> {
    let frame = parse_fields(headers.iter())?;
    if !matches!(frame.kind, Kind::Hello | Kind::Finish) {
        return Err(invalid("expected negotiation request"));
    }
    Ok(RequestHint {
        node: frame.node,
        volume: frame.volume,
        shard: frame.shard,
        is_finish: frame.kind == Kind::Finish,
    })
}
fn parse_fields<'a>(fields: impl Iterator<Item = (&'a str, &'a [u8])>) -> io::Result<Frame> {
    let mut found = None;
    for (name, value) in fields {
        if name.eq_ignore_ascii_case(HEADER) {
            if found.is_some() {
                return Err(invalid("duplicate negotiation header"));
            }
            found = Some(value);
        }
    }
    Frame::decode(found.ok_or_else(|| invalid("missing negotiation header"))?)
}
fn ready_body(challenge: [u8; 16]) -> Vec<u8> {
    [READY, challenge.as_slice()].concat()
}
fn sign_ready(
    session: &mut auth::Session,
    snapshot: &crypto::Snapshot,
    challenge: [u8; 16],
) -> io::Result<Vec<u8>> {
    let control = auth::Control::new(0, ready_body(challenge)).map_err(auth_error)?;
    Ok(session
        .sign(snapshot, control)
        .map_err(auth_error)?
        .encode()
        .to_vec())
}
fn verify_ready(
    session: &mut auth::Session,
    snapshot: &crypto::Snapshot,
    challenge: [u8; 16],
    bytes: &[u8],
) -> io::Result<()> {
    let signed = auth::SignedControl::decode(bytes).map_err(auth_error)?;
    let verified = session.verify(snapshot, signed).map_err(auth_error)?;
    if verified.request_id() != 0 || verified.body() != ready_body(challenge) {
        return Err(invalid("invalid authenticated Ready"));
    }
    Ok(())
}

/// Authenticated connection/pinned policy; never reinstall its session. Ready
/// consumed responder TX/initiator RX zero. Enforce inbound confirmation deadline
/// until authenticated RDMA arrives: TCP send success does not prove receipt.
#[must_use]
pub struct Established {
    pub connection: rdma::Connection,
    pub context: Rc<Context>,
    pub peer: NodeId,
    pub confirmation_deadline: Option<Instant>,
}

struct AwaitReply {
    qp: rdma::Connecting,
    auth: auth::Initiator,
}
struct AwaitReady {
    connection: rdma::Connected,
    session: auth::Session,
}
enum ClientState {
    Reply(client::HeadExchange, AwaitReply),
    Ready(client::HeadExchange, AwaitReady),
    Done,
}
/// Single-use client. Error or drop retires both TCP and QP through their RAII
/// owners. Polling never drives the ring or RDMA sources.
/// ```compile_fail
/// use racer_dataplane::negotiation::Client;
/// fn duplicate(client: Client) { let _ = client.clone(); }
/// ```
/// ```compile_fail
/// use racer_dataplane::negotiation::Client;
/// fn move_worker(client: Client) { std::thread::spawn(move || drop(client)); }
/// ```
#[must_use]
pub struct Client {
    context: Rc<Context>,
    rails: Rails,
    peer: NodeId,
    target: String,
    challenge: [u8; 16],
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
        client::Request::new(target, &[])?;
        let peer = context
            .prepared
            .eligible_peer_for_volume(&context.volume_id, peer_id)
            .ok_or_else(|| invalid("outbound peer is not eligible for volume"))?;
        let remote = peer.node();
        let deadline = crate::environment::now() + duration;
        let challenge = transport_nonce()?;
        if challenge == [0; 16] {
            return Err(io::Error::other("zero random challenge"));
        }
        let qp = rails.prepare(&context, challenge)?;
        let (auth, hello) = auth::Initiator::start(
            context.snapshot().clone(),
            context.peers(remote, true, target)?,
            Some(qp.offer()),
            duration,
        )
        .map_err(auth_error)?;
        let frame = context.frame(Kind::Hello, hello.encode().to_vec()).encode();
        let connection =
            client::Connection::new(peer.endpoint().address(), peer.endpoint().host())?;
        let exchange =
            connection.head(client::Request::new(target, &[(HEADER, &frame)])?, deadline)?;
        Ok(Self {
            context,
            rails,
            peer: remote,
            target: target.to_owned(),
            challenge,
            deadline,
            state: ClientState::Reply(exchange, AwaitReply { qp, auth }),
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
        let exchange = match &mut self.state {
            ClientState::Reply(exchange, _) | ClientState::Ready(exchange, _) => exchange,
            ClientState::Done => return Err(invalid("negotiation already finished")),
        };
        let response = match exchange.poll(ring, budget)? {
            Progress::Pending(w) => return Ok(Progress::Pending(w)),
            Progress::Ready(response) => response,
        };
        if response.status() != 200 || response.content_length() != Some(0) {
            return Err(invalid("negotiation HTTP response rejected"));
        }
        let frame = parse_fields(response.headers().iter())?;
        self.context.validate(&frame, self.peer)?;
        fresh(self.deadline, crate::environment::now())?;
        match std::mem::replace(&mut self.state, ClientState::Done) {
            ClientState::Reply(_, pending) => {
                if frame.kind != Kind::Reply {
                    return Err(invalid("expected Reply"));
                }
                let offer = handshake_offer(Kind::Reply, &frame.payload)?;
                validate_offer(&self.context, &self.rails, &offer, self.challenge)?;
                let reply = auth::Reply::decode(&frame.payload).map_err(auth_error)?;
                let (mut session, finish) = pending.auth.finish(reply).map_err(auth_error)?;
                let authenticated = self.context.authorize(&mut session)?;
                let connection = pending.qp.connect(authenticated, self.context.shard)?;
                let tcp = response
                    .recycle()
                    .ok_or_else(|| invalid("Reply closed negotiation TCP"))?;
                let frame = self
                    .context
                    .frame(Kind::Finish, finish.encode().to_vec())
                    .encode();
                let exchange = tcp.head(
                    client::Request::new(&self.target, &[(HEADER, &frame)])?,
                    self.deadline,
                )?;
                self.state = ClientState::Ready(
                    exchange,
                    AwaitReady {
                        connection,
                        session,
                    },
                );
                Ok(Progress::Pending(Work {
                    runnable: true,
                    deadline: Some(self.deadline),
                }))
            }
            ClientState::Ready(_, mut pending) => {
                if frame.kind != Kind::Ready {
                    return Err(invalid("expected authenticated Ready"));
                }
                verify_ready(
                    &mut pending.session,
                    self.context.snapshot(),
                    self.challenge,
                    &frame.payload,
                )?;
                Ok(Progress::Ready(Established {
                    connection: self.context.activate(pending.connection, pending.session)?,
                    context: self.context.clone(),
                    peer: self.peer,
                    confirmation_deadline: None,
                }))
            }
            ClientState::Done => unreachable!(),
        }
    }
}

/// Counts all reserved handshakes: pending Hello, active HTTP tasks and undrained
/// established connections. The lease follows affine state and releases on drop.
struct Lease(Rc<Cell<usize>>);
impl Drop for Lease {
    fn drop(&mut self) {
        self.0.set(self.0.get() - 1);
    }
}
struct AwaitFinish {
    qp: rdma::Connecting,
    auth: auth::Responder,
    peer: NodeId,
    target: String,
    challenge: [u8; 16],
    deadline: Instant,
    lease: Lease,
}
struct Queued {
    established: Established,
    deadline: Instant,
    _lease: Lease,
}
#[derive(Default)]
struct Store {
    pending: HashMap<http::ConnectionId, AwaitFinish>,
    completed: VecDeque<Queued>,
}
enum AfterSend {
    Reply(AwaitFinish),
    Ready(Queued),
}
/// HTTP task owned by the existing HTTP scheduler. Dropping a task drops any
/// pending/connected QP before completion and releases its capacity reservation.
/// ```compile_fail
/// use racer_dataplane::negotiation::Task;
/// fn duplicate(task: Task) { let _ = task.clone(); }
/// ```
#[must_use]
pub struct Task {
    store: Rc<RefCell<Store>>,
    identity: http::ConnectionId,
    sending: Option<http::SendingHeadHeaders>,
    after: Option<AfterSend>,
    deadline: Instant,
}
/// Bounded per-volume/shard responder, composed with a data Handler. Admission is
/// based on configured direct node eligibility, not the outbound volume DAG.
/// ```compile_fail
/// use racer_dataplane::negotiation::Server;
/// fn move_worker(server: Server) { std::thread::spawn(move || drop(server)); }
/// ```
pub struct Server {
    context: Rc<Context>,
    rails: Rails,
    duration: Duration,
    capacity: usize,
    leases: Rc<Cell<usize>>,
    store: Rc<RefCell<Store>>,
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
        })
    }
    /// Call only after `is_negotiation`; errors drop/close the request TCP. The
    /// outer Handler can hold `Result<Task>` and surface errors from its poll.
    pub fn start(&mut self, request: http::Request) -> io::Result<Task> {
        let now = crate::environment::now();
        self.poll(now);
        let identity = request.connection_id();
        let frame = parse_fields(request.headers().iter())?;
        let http::Request::Head(request) = request else {
            return Err(invalid("negotiation requires HEAD"));
        };
        let peer = self
            .context
            .prepared
            .eligible_node_for_volume(self.context.volume_id(), frame.node)
            .ok_or_else(|| invalid("incoming node is not a configured eligible direct peer"))?;
        self.context.validate(&frame, peer.node())?;
        let (reply, after, deadline) = match frame.kind {
            Kind::Hello => {
                if self.store.borrow().pending.contains_key(&identity) {
                    // A second Hello invalidates the first one, never overwrites it.
                    self.store.borrow_mut().pending.remove(&identity);
                    return Err(invalid("duplicate Hello on TCP connection"));
                }
                if self.leases.get() >= self.capacity {
                    return Err(io::Error::new(
                        io::ErrorKind::WouldBlock,
                        "negotiation capacity",
                    ));
                }
                self.leases.set(self.leases.get() + 1);
                let lease = Lease(self.leases.clone());
                let offer = handshake_offer(Kind::Hello, &frame.payload)?;
                let challenge = offer.challenge();
                validate_offer(&self.context, &self.rails, &offer, challenge)?;
                let qp = self.rails.prepare(&self.context, challenge)?;
                let (auth, reply) = auth::Responder::accept(
                    self.context.snapshot().clone(),
                    self.context.peers(peer.node(), false, request.target())?,
                    auth::Hello::decode(&frame.payload).map_err(auth_error)?,
                    Some(qp.offer()),
                    self.duration,
                )
                .map_err(auth_error)?;
                let deadline = now + self.duration;
                let pending = AwaitFinish {
                    qp,
                    auth,
                    peer: peer.node(),
                    target: request.target().to_owned(),
                    challenge,
                    deadline,
                    lease,
                };
                (
                    self.context.frame(Kind::Reply, reply.encode().to_vec()),
                    AfterSend::Reply(pending),
                    deadline,
                )
            }
            Kind::Finish => {
                let pending = self
                    .store
                    .borrow_mut()
                    .pending
                    .remove(&identity)
                    .ok_or_else(|| invalid("Finish requires Hello on the same TCP connection"))?;
                fresh(pending.deadline, now)?;
                if pending.peer != peer.node() || pending.target != request.target() {
                    return Err(invalid("Finish peer/target mismatch"));
                }
                let mut session = pending
                    .auth
                    .finish(auth::Finish::decode(&frame.payload).map_err(auth_error)?)
                    .map_err(auth_error)?;
                let offer = self.context.authorize(&mut session)?;
                let connection = pending.qp.connect(offer, self.context.shard)?;
                let ready = sign_ready(&mut session, self.context.snapshot(), pending.challenge)?;
                // A peer may send RDMA as soon as it sees Ready, before the HTTP
                // send completion is collected or Manager admits this connection.
                let connection = self.context.activate(connection, session)?;
                let deadline = pending.deadline;
                let established = Established {
                    connection,
                    context: self.context.clone(),
                    peer: pending.peer,
                    confirmation_deadline: Some(deadline),
                };
                (
                    self.context.frame(Kind::Ready, ready),
                    AfterSend::Ready(Queued {
                        established,
                        deadline,
                        _lease: pending.lease,
                    }),
                    deadline,
                )
            }
            _ => return Err(invalid("unexpected negotiation request message")),
        };
        let encoded = reply.encode();
        let sending = request.respond(http::ResponseHead::new(
            200,
            Some(0),
            &[(HEADER, encoded.as_bytes())],
        )?)?;
        Ok(Task {
            store: self.store.clone(),
            identity,
            sending: Some(sending),
            after: Some(after),
            deadline,
        })
    }
    pub fn poll_task(
        &mut self,
        task: &mut Task,
        ring: &mut Ring,
        budget: usize,
    ) -> io::Result<Progress<http::Completed>> {
        if !Rc::ptr_eq(&task.store, &self.store) {
            return Err(invalid("task belongs to another negotiation server"));
        }
        let result = self.poll_task_inner(task, ring, budget);
        if result.is_err() {
            task.after.take();
            task.sending.take();
        }
        result
    }
    fn poll_task_inner(
        &mut self,
        task: &mut Task,
        ring: &mut Ring,
        budget: usize,
    ) -> io::Result<Progress<http::Completed>> {
        fresh(task.deadline, crate::environment::now())?;
        let sending = task
            .sending
            .as_mut()
            .ok_or_else(|| invalid("negotiation task already completed"))?;
        match sending.poll(ring, budget)? {
            Progress::Pending(mut work) => {
                let deadline = work
                    .deadline
                    .map_or(task.deadline, |d| d.min(task.deadline));
                work.deadline = Some(deadline);
                Ok(Progress::Pending(work))
            }
            Progress::Ready(done) => {
                fresh(task.deadline, crate::environment::now())?;
                task.sending.take();
                match task.after.take().unwrap() {
                    AfterSend::Reply(pending) => {
                        if !task.identity.is_closed() {
                            self.store
                                .borrow_mut()
                                .pending
                                .insert(task.identity.clone(), pending);
                        }
                    }
                    AfterSend::Ready(ready) => self.store.borrow_mut().completed.push_back(ready),
                }
                Ok(Progress::Ready(done))
            }
        }
    }
    /// Bounded pending/undrained expiry; active tasks expire through `poll_task`.
    pub fn poll(&mut self, now: Instant) -> Work {
        let mut store = self.store.borrow_mut();
        store
            .pending
            .retain(|id, p| !id.is_closed() && now < p.deadline);
        store.completed.retain(|p| now < p.deadline);
        let deadline = store
            .pending
            .values()
            .map(|p| p.deadline)
            .chain(store.completed.iter().map(|p| p.deadline))
            .min();
        Work {
            runnable: false,
            deadline,
        }
    }
    /// Drain one connection only after Ready's HTTP send completed. Expired
    /// entries are retired first. Runtime now owns the confirmation timeout.
    pub fn take_completed(&mut self, now: Instant) -> Option<Established> {
        self.poll(now);
        Some(self.store.borrow_mut().completed.pop_front()?.established)
    }
    pub fn reserved(&self) -> usize {
        self.leases.get()
    }
    /// Locate draining same-TCP continuation; `start` still authenticates it.
    pub fn has_pending(&self, identity: &http::ConnectionId) -> bool {
        self.store.borrow().pending.contains_key(identity)
    }
    /// Retire queued/pending state. Drop HTTP tasks too when retiring generation.
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
include!(concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/tests/security/negotiation.rs"
));

pub mod peer_identity {
    //! Validated identities used by RDMA negotiation. These are values, not proof of
    //! membership or authentication; membership requires `control::EligiblePeer`.
    use std::{fmt, io, str::FromStr};

    /// Bounded offer fabric; hex auth fits the 8192-byte HTTP header buffer.
    pub const MAX_FABRIC_LEN: usize = 256;
    /// Matches the bounded physical discovery capacity, not a shard/worker count.
    pub const MAX_RAILS: u32 = 256;
    /// Current RDMA v1 fixed wire prefix plus a bounded fabric.
    pub const MAX_OFFER_LEN: usize = 76 + MAX_FABRIC_LEN;
    /// Signed replies add 128 bytes to an offer; hello and finish are smaller.
    pub const MAX_HANDSHAKE_LEN: usize = 128 + MAX_OFFER_LEN;
    /// Bound the prepared HTTP authority used alongside negotiation headers.
    pub const MAX_AUTHORITY_LEN: usize = 1024;

    use super::invalid;

    /// Exactly the 32 bytes of the remote control `Snapshot.node`.
    #[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
    pub struct NodeId([u8; 32]);
    impl NodeId {
        pub fn from_bytes(bytes: &[u8]) -> io::Result<Self> {
            let bytes = bytes
                .try_into()
                .map_err(|_| invalid("node identity must be 32 bytes"))?;
            Ok(Self(bytes))
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
            for (i, byte) in bytes.iter_mut().enumerate() {
                *byte = u8::from_str_radix(&value[2 * i..2 * i + 2], 16).unwrap();
            }
            Ok(Self(bytes))
        }
    }
    impl fmt::Display for NodeId {
        fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
            for byte in self.0 {
                write!(f, "{byte:02x}")?;
            }
            Ok(())
        }
    }

    /// Visible ASCII fabric name; empty configuration disables RDMA.
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
            Ok(Self(value.to_owned()))
        }
        pub fn as_str(&self) -> &str {
            &self.0
        }
    }

    /// Bounded untrusted catalog index/count, not an RNIC or shard capability.
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
