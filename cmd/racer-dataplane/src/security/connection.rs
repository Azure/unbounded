//! Authenticated peer messaging: HTTP signatures for retained forwarding proofs
//! and mutually authenticated, exclusively socket-owned ordered sessions.
//!
//! A verified proof establishes identity and provenance, not replay admission.
//! Only the carrying connection session admits a fresh immediate-hop message.
use super::{
    identity::{Certificates, Keyring, VerifiedPeer},
    protocol::{self as p, field, number, push, push_binary},
};
use crate::{
    error::{Error, Result},
    http::{
        Codec, MessageHead, StartLine,
        connection::{ConnectionLease, HttpIo},
    },
    model::NodeId,
    runtime::deadline::RequestScope,
};
use sha2::{Digest, Sha256};
use std::{
    collections::BTreeSet,
    rc::Rc,
    time::{Duration, UNIX_EPOCH},
};

pub struct Signatures {
    keys: Rc<Keyring>,
    certificates: Rc<Certificates>,
}
pub struct SignedHead {
    pub head: MessageHead,
    pub signature: Vec<u8>,
}
pub struct VerifiedHead {
    pub(crate) signed: SignedHead,
    pub(crate) peer: VerifiedPeer,
}
impl Signatures {
    pub fn new(keys: Rc<Keyring>, certificates: Rc<Certificates>) -> Self {
        Self { keys, certificates }
    }
    pub fn node(&self) -> &NodeId {
        self.keys.node()
    }
    /// Sign retained provenance. Replay admission belongs exclusively to the
    /// immediate-hop head signed by the owning connection session.
    pub fn sign(&self, head: MessageHead) -> Result<SignedHead> {
        if head.headers.iter().any(|h| {
            is_auth_field(&h.name.to_ascii_lowercase())
                && !h.name.eq_ignore_ascii_case("racer-receiver")
        }) {
            return Err(Error::InvalidRequest);
        }
        receiver(&head)?;
        let signed = self.sign_fields(head)?;
        Codec::new(p::MAX_HEAD).encode_head(&signed.head)?;
        Ok(signed)
    }
    pub(crate) fn sign_fields(&self, mut head: MessageHead) -> Result<SignedHead> {
        let identity = self.keys.signing_identity()?;
        if identity.node() != self.node() {
            return Err(Error::Unauthorized);
        }
        push(&mut head, "racer-profile", p::PROFILE);
        p::uuid(&self.keys.cluster().0)?;
        p::uuid(&self.node().0)?;
        push(&mut head, "racer-cluster", &self.keys.cluster().0);
        push(&mut head, "racer-signer", &self.node().0);
        push_binary(
            &mut head,
            "racer-certificates",
            &encode_chain(identity.certificate_chain())?,
        );
        push(
            &mut head,
            "racer-timestamp",
            p::millis(crate::runtime::environment::wall_now())?,
        );
        if head.unique("racer-kind")? == Some(b"request".as_slice()) {
            let cache = crate::model::CacheId(field(&head, "racer-cache")?);
            let key = self
                .keys
                .active(&cache, super::identity::KeyPurpose::OriginCredentials)?;
            push_binary(&mut head, "racer-mac-key", &key.id().0);
            let tag = racer_crypto::hmac_sha256(&*super::request_key(&key)?, &mac_base(&head)?);
            push_binary(&mut head, "racer-request-mac", &tag);
        }
        let input = signature_input(&head)?;
        push(&mut head, "signature-input", format!("racer={input}"));
        let signature = identity.sign(&signature_base(&head)?)?;
        if signature.len() != 64 {
            return Err(Error::Unauthorized);
        }
        push(
            &mut head,
            "signature",
            format!("racer=:{}:", p::binary(&signature)),
        );
        Codec::new(crate::peer::protocol::MAX_ENVELOPE_HEAD).encode_head(&head)?;
        Ok(SignedHead { head, signature })
    }
    /// Verify identity and retained provenance, without replay admission. Network
    /// callers must first admit the carrying immediate-hop head on its socket.
    pub fn verify_proof(&self, head: SignedHead) -> Result<VerifiedHead> {
        let peer = self.verify_historical(&head)?;
        if receiver(&head.head)? != *self.node() {
            return Err(Error::Unauthorized);
        }
        Ok(VerifiedHead { signed: head, peer })
    }
    /// Cryptographically validate an original or historical hop, without replay
    /// admission at this node. The owning connection admits the fresh outer head;
    /// forwarding then validates the complete retained chain and logical fields.
    pub(crate) fn verify_historical(&self, signed: &SignedHead) -> Result<VerifiedPeer> {
        self.verify_signed_age(signed, true)
    }
    /// Only an opaque, already bound request may use its original deadline
    /// instead of the fresh-message replay window. Revalidate current keys/trust.
    pub(crate) fn verify_retained_request(
        &self,
        binding: &super::forwarding::RequestBinding,
    ) -> Result<VerifiedPeer> {
        self.verify_signed_age(binding.retained_proof()?, false)
    }
    fn verify_signed_age(&self, signed: &SignedHead, fresh: bool) -> Result<VerifiedPeer> {
        let head = &signed.head;
        if field(head, "racer-profile")? != p::PROFILE
            || field(head, "racer-cluster")? != self.keys.cluster().0
        {
            return Err(Error::Unauthorized);
        }
        p::uuid(&self.keys.cluster().0)?;
        if head.unique("racer-kind")? == Some(b"request".as_slice()) {
            let cache = crate::model::CacheId(field(head, "racer-cache")?);
            let id = p::decode_binary(field(head, "racer-mac-key")?.as_bytes())?
                .try_into()
                .map_err(|_| Error::Unauthorized)?;
            let key = self.keys.lease(
                Some(&cache),
                crate::model::KeyId(id),
                super::identity::KeyPurpose::OriginCredentials,
            )?;
            let expected = racer_crypto::hmac_sha256(&*super::request_key(&key)?, &mac_base(head)?);
            if !racer_crypto::ct_eq(
                &p::decode_binary(field(head, "racer-request-mac")?.as_bytes())?,
                &expected,
            ) {
                return Err(Error::Unauthorized);
            }
        }
        let base = signature_base(head)?;
        if signed.signature.len() != 64
            || field(head, "signature")? != format!("racer=:{}:", p::binary(&signed.signature))
        {
            return Err(Error::Unauthorized);
        }
        let signer = node_field(head, "racer-signer")?;
        let chain = decode_chain(&p::decode_binary(
            field(head, "racer-certificates")?.as_bytes(),
        )?)?;
        let timestamp = UNIX_EPOCH
            .checked_add(Duration::from_millis(number(head, "racer-timestamp")?))
            .ok_or(Error::Unauthorized)?;
        let now = crate::runtime::environment::wall_now();
        if timestamp
            > now
                .checked_add(Duration::from_secs(5))
                .ok_or(Error::Unauthorized)?
            || fresh
                && now
                    .duration_since(timestamp)
                    .is_ok_and(|age| age >= Duration::from_secs(60))
        {
            return Err(Error::Replay);
        }
        self.certificates
            .verify_signed(&chain, &signer, &base, &signed.signature)
    }
}
pub(crate) fn is_auth_field(name: &str) -> bool {
    matches!(
        name,
        "signature-input"
            | "signature"
            | "racer-profile"
            | "racer-cluster"
            | "racer-signer"
            | "racer-receiver"
            | "racer-certificates"
            | "racer-session"
            | "racer-direction"
            | "racer-sequence"
            | "racer-timestamp"
            | "racer-mac-key"
            | "racer-request-mac"
    )
}
fn mac_base(head: &MessageHead) -> Result<Vec<u8>> {
    let mut out = b"racer/request-mac/message/v1\0".to_vec();
    let start = match &head.start {
        StartLine::Request { method, target } => format!("{method} {target}"),
        _ => return Err(Error::Unauthorized),
    };
    super::aead::field(&mut out, start.as_bytes())?;
    for name in components(head)? {
        if name.starts_with('@') || name == "racer-request-mac" {
            continue;
        }
        super::aead::field(&mut out, name.as_bytes())?;
        super::aead::field(&mut out, head.unique(&name)?.ok_or(Error::Unauthorized)?)?;
    }
    Ok(out)
}
pub fn node_field(head: &MessageHead, name: &str) -> Result<NodeId> {
    let node = field(head, name)?;
    p::uuid(&node)?;
    Ok(NodeId(node))
}
pub fn receiver(head: &MessageHead) -> Result<NodeId> {
    node_field(head, "racer-receiver")
}
fn components(head: &MessageHead) -> Result<Vec<String>> {
    Codec::new(crate::peer::protocol::MAX_ENVELOPE_HEAD).encode_head(head)?;
    let mut names = BTreeSet::new();
    for h in &head.headers {
        let name = h.name.to_ascii_lowercase();
        if !names.insert(name)
            || h.value.first().is_some_and(|b| b.is_ascii_whitespace())
            || h.value.last().is_some_and(|b| b.is_ascii_whitespace())
            || !h.value.is_ascii()
        {
            return Err(Error::Unauthorized);
        }
    }
    names.remove("signature");
    names.remove("signature-input");
    let mut components = match head.start {
        StartLine::Request { .. } => vec!["@method".into(), "@request-target".into()],
        StartLine::Response { .. } => vec!["@status".into()],
    };
    components.extend(names);
    Ok(components)
}
fn signature_input(head: &MessageHead) -> Result<String> {
    signature_input_for_components(head, &components(head)?)
}
fn signature_input_for_components(head: &MessageHead, components: &[String]) -> Result<String> {
    let components = components
        .iter()
        .map(|s| format!("\"{s}\""))
        .collect::<Vec<_>>()
        .join(" ");
    let timestamp = number(head, "racer-timestamp")? / 1000;
    let keyid = field(head, "racer-signer")?;
    p::uuid(&keyid)?;
    Ok(format!(
        "({components});created={timestamp};keyid=\"{keyid}\";alg=\"ed25519\";tag=\"{}\"",
        p::PROFILE
    ))
}
/// RFC 9421 section 2.5 signature base, using the strict Racer profile. The
/// verifier accepts only this canonical structured-field serialization, avoiding
/// duplicate labels, unsupported parameters and alternate parsing ambiguity.
pub fn signature_base(head: &MessageHead) -> Result<Vec<u8>> {
    let components = components(head)?;
    let input = signature_input_for_components(head, &components)?;
    if field(head, "signature-input")? != format!("racer={input}") {
        return Err(Error::Unauthorized);
    }
    let mut lines = Vec::new();
    for name in components {
        let value = match (name.as_str(), &head.start) {
            ("@method", StartLine::Request { method, .. }) => method.clone(),
            ("@request-target", StartLine::Request { target, .. }) => target.clone(),
            ("@status", StartLine::Response { status }) => status.to_string(),
            // The complete outer head was bounded by components(). Its base64
            // provenance fields can exceed the inner logical field's 64 KiB cap.
            _ => String::from_utf8(head.unique(&name)?.ok_or(Error::Unauthorized)?.to_vec())
                .map_err(|_| Error::Unauthorized)?,
        };
        lines.push(format!("\"{name}\": {value}"));
    }
    lines.push(format!("\"@signature-params\": {input}"));
    Ok(lines.join("\n").into_bytes())
}
/// Domain-separated SHA-256 binding of the exact signature base and Ed25519
/// signature. Includes the signature itself, so a request ID is never sufficient.
pub fn signed_digest(head: &SignedHead) -> Result<[u8; 32]> {
    let base = signature_base(&head.head)?;
    let mut hash = Sha256::new();
    hash.update(b"racer-peer-v2/signed-head\0");
    hash.update((base.len() as u64).to_be_bytes());
    hash.update(base);
    hash.update((head.signature.len() as u64).to_be_bytes());
    hash.update(&head.signature);
    Ok(hash.finalize().into())
}
fn encode_chain(chain: &[Vec<u8>]) -> Result<Vec<u8>> {
    if chain.is_empty() || chain.len() > 8 {
        return Err(Error::Unauthorized);
    }
    let mut bytes = Vec::new();
    for cert in chain {
        if cert.is_empty() || cert.len() > 16384 {
            return Err(Error::Unauthorized);
        }
        bytes.extend_from_slice(&(cert.len() as u32).to_be_bytes());
        bytes.extend_from_slice(cert);
    }
    if bytes.len() > 65536 {
        return Err(Error::Unauthorized);
    }
    Ok(bytes)
}
fn decode_chain(mut bytes: &[u8]) -> Result<Vec<Vec<u8>>> {
    if bytes.len() > 65536 {
        return Err(Error::Unauthorized);
    }
    let mut chain = Vec::new();
    while !bytes.is_empty() {
        if bytes.len() < 4 || chain.len() == 8 {
            return Err(Error::Unauthorized);
        }
        let n =
            u32::from_be_bytes(bytes[..4].try_into().map_err(|_| Error::Unauthorized)?) as usize;
        bytes = &bytes[4..];
        if n == 0 || n > 16384 || bytes.len() < n {
            return Err(Error::Unauthorized);
        }
        chain.push(bytes[..n].to_vec());
        bytes = &bytes[n..];
    }
    if chain.is_empty() {
        return Err(Error::Unauthorized);
    }
    Ok(chain)
}

const TARGET: &str = "/racer/peer/v2/session";
const DOMAIN: &[u8] = b"racer-peer-v2/connection\0";

/// Never cloned, reset, or detached from its socket. Idle pooling moves this value.
pub struct Session {
    signatures: Rc<Signatures>,
    peer: NodeId,
    id: [u8; 32],
    direction: u8,
    tx: u64,
    rx: u64,
    expires: std::time::Instant,
}
impl Session {
    fn new(signatures: Rc<Signatures>, peer: NodeId, id: [u8; 32], direction: u8) -> Self {
        Self {
            signatures,
            peer,
            id,
            direction,
            tx: 0,
            rx: 0,
            expires: crate::runtime::environment::now() + Duration::from_secs(3600),
        }
    }
    pub fn peer(&self) -> &NodeId {
        &self.peer
    }
    fn check(&self) -> Result<()> {
        if crate::runtime::environment::now() >= self.expires {
            return Err(Error::DeadlineExceeded);
        }
        Ok(())
    }
    pub(crate) fn sign(&mut self, mut head: MessageHead) -> Result<MessageHead> {
        self.check()?;
        if head
            .headers
            .iter()
            .any(|h| is_auth_field(&h.name.to_ascii_lowercase()))
        {
            return Err(Error::Unauthorized);
        }
        let next = self.tx.checked_add(1).ok_or(Error::Replay)?;
        p::push(&mut head, "racer-receiver", &self.peer.0);
        p::push_binary(&mut head, "racer-session", &self.id);
        p::push(&mut head, "racer-direction", self.direction);
        p::push(&mut head, "racer-sequence", next);
        let signed = self.signatures.sign_fields(head)?;
        self.tx = next;
        Ok(signed.head)
    }
    pub(crate) fn admit(&mut self, head: MessageHead) -> Result<MessageHead> {
        self.check()?;
        let signed = signed(head)?;
        let verified = self.signatures.verify_proof(signed)?;
        let mut head = verified.signed.head;
        let next = self.rx.checked_add(1).ok_or(Error::Replay)?;
        if verified.peer.node() != &self.peer
            || array::<32>(&head, "racer-session")? != self.id
            || p::number(&head, "racer-direction")? != u64::from(1 - self.direction)
            || p::number(&head, "racer-sequence")? != next
        {
            return Err(Error::Replay);
        }
        // A peer may carry historical proofs, but cannot present another node's
        // head as its own immediate hop, including on native controls.
        let mut last = head.unique("racer-original")?;
        for i in 0..crate::peer::protocol::MAX_HOPS {
            if let Some(hop) = head.unique(&format!("racer-hop-{i}"))? {
                last = Some(hop);
            }
        }
        if let Some(proof) = last {
            let proof = crate::peer::protocol::decode_signed(proof)?;
            if node_field(&proof.head, "racer-signer")? != self.peer
                || receiver(&proof.head)? != *self.signatures.node()
            {
                return Err(Error::Unauthorized);
            }
        }
        head.headers
            .retain(|h| !is_auth_field(&h.name.to_ascii_lowercase()));
        self.rx = next;
        Ok(head)
    }
}
fn array<const N: usize>(head: &MessageHead, field: &str) -> Result<[u8; N]> {
    p::decode_binary(p::field(head, field)?.as_bytes())?
        .try_into()
        .map_err(|_| Error::Unauthorized)
}
fn signed(head: MessageHead) -> Result<SignedHead> {
    let value = p::field(&head, "signature")?;
    let value = value
        .strip_prefix("racer=:")
        .and_then(|v| v.strip_suffix(':'))
        .ok_or(Error::Unauthorized)?;
    Ok(SignedHead {
        signature: p::decode_binary(value.as_bytes())?,
        head,
    })
}
fn random() -> Result<[u8; 32]> {
    let mut bytes = [0; 32];
    crate::runtime::environment::fill_random(&mut bytes).map_err(|_| Error::Unavailable)?;
    Ok(bytes)
}
fn transcript(client: &NodeId, server: &NodeId, a: &[u8; 32], b: &[u8; 32]) -> [u8; 32] {
    let mut h = Sha256::new();
    h.update(DOMAIN);
    h.update(client.0.as_bytes());
    h.update(server.0.as_bytes());
    h.update(a);
    h.update(b);
    h.finalize().into()
}
fn head(
    signatures: &Signatures,
    peer: &NodeId,
    phase: &str,
    a: &[u8; 32],
    b: &[u8; 32],
    response: bool,
) -> Result<MessageHead> {
    let mut h = MessageHead {
        start: if response {
            StartLine::Response { status: 200 }
        } else {
            StartLine::Request {
                method: "POST".into(),
                target: TARGET.into(),
            }
        },
        headers: vec![],
    };
    p::push(&mut h, "content-length", 0);
    p::push(&mut h, "racer-receiver", &peer.0);
    p::push(&mut h, "racer-handshake", phase);
    p::push_binary(&mut h, "racer-client-challenge", a);
    p::push_binary(&mut h, "racer-server-challenge", b);
    Ok(signatures.sign(h)?.head)
}
fn verify(
    signatures: &Signatures,
    head: MessageHead,
    peer: Option<&NodeId>,
    phase: &str,
    response: bool,
) -> Result<(NodeId, [u8; 32], [u8; 32])> {
    let verified = signatures.verify_proof(signed(head)?)?;
    let h = &verified.signed.head;
    let start = if response {
        matches!(h.start, StartLine::Response { status: 200 })
    } else {
        matches!(&h.start, StartLine::Request { method, target } if method == "POST" && target == TARGET)
    };
    if !start
        || h.content_length()? != Some(0)
        || p::field(h, "racer-handshake")? != phase
        || peer.is_some_and(|p| p != verified.peer.node())
    {
        return Err(Error::Unauthorized);
    }
    for field in &h.headers {
        if !is_auth_field(&field.name)
            && !matches!(
                field.name.as_str(),
                "content-length"
                    | "racer-handshake"
                    | "racer-client-challenge"
                    | "racer-server-challenge"
            )
        {
            return Err(Error::Unauthorized);
        }
    }
    Ok((
        verified.peer.node().clone(),
        array(h, "racer-client-challenge")?,
        array(h, "racer-server-challenge")?,
    ))
}
fn scope(parent: &RequestScope) -> Result<RequestScope> {
    parent.check()?;
    let mut scope = parent.clone();
    scope.deadline.0 = scope
        .deadline
        .0
        .min(crate::runtime::environment::now() + Duration::from_secs(5));
    Ok(scope)
}
pub async fn connect(
    io: &HttpIo,
    mut connection: ConnectionLease,
    signatures: Rc<Signatures>,
    peer: &NodeId,
    parent: &RequestScope,
) -> Result<ConnectionLease> {
    parent.check()?;
    if let Some(session) = connection.state().session.as_ref() {
        session.check()?;
        if session.peer() != peer {
            return Err(Error::Unauthorized);
        }
        return Ok(connection);
    }
    let scope = scope(parent)?;
    let io = &io.capped(65536);
    let a = random()?;
    let response = io
        .exchange_head(
            connection,
            head(&signatures, peer, "hello", &a, &[0; 32], false)?,
            &scope,
        )
        .await?;
    let (_, echoed, b) = verify(&signatures, response.value, Some(peer), "challenge", true)?;
    if echoed != a || b == [0; 32] {
        return Err(Error::Unauthorized);
    }
    connection = response.connection;
    connection.next_round()?;
    let response = io
        .exchange_head(
            connection,
            head(&signatures, peer, "finish", &a, &b, false)?,
            &scope,
        )
        .await?;
    let (_, echoed, remote) = verify(&signatures, response.value, Some(peer), "ready", true)?;
    if echoed != a || remote != b {
        return Err(Error::Unauthorized);
    }
    scope.check()?;
    connection = response.connection;
    connection.next_round()?;
    crate::http::connection::install_session(
        &mut connection,
        Session::new(
            signatures.clone(),
            peer.clone(),
            transcript(signatures.node(), peer, &a, &b),
            0,
        ),
    )?;
    Ok(connection)
}
pub async fn accept(
    io: &HttpIo,
    mut connection: ConnectionLease,
    signatures: Rc<Signatures>,
    parent: &RequestScope,
) -> Result<ConnectionLease> {
    parent.check()?;
    if let Some(session) = connection.state().session.as_ref() {
        session.check()?;
        return Ok(connection);
    }
    let scope = scope(parent)?;
    let io = &io.capped(65536);
    let incoming = io.receive_head(connection, &scope).await?;
    scope.check()?;
    let (peer, a, zero) = verify(&signatures, incoming.value, None, "hello", false)?;
    if a == [0; 32] || zero != [0; 32] {
        return Err(Error::Unauthorized);
    }
    scope.check()?;
    let b = random()?;
    connection = io
        .send_head(
            incoming.connection,
            head(&signatures, &peer, "challenge", &a, &b, true)?,
            &scope,
        )
        .await?
        .connection;
    connection.next_round()?;
    let incoming = io.receive_head(connection, &scope).await?;
    scope.check()?;
    let (_, echoed, remote) = verify(&signatures, incoming.value, Some(&peer), "finish", false)?;
    if echoed != a || remote != b {
        return Err(Error::Unauthorized);
    }
    scope.check()?;
    connection = io
        .send_head(
            incoming.connection,
            head(&signatures, &peer, "ready", &a, &b, true)?,
            &scope,
        )
        .await?
        .connection;
    scope.check()?;
    connection.next_round()?;
    let id = transcript(&peer, signatures.node(), &a, &b);
    crate::http::connection::install_session(
        &mut connection,
        Session::new(signatures, peer, id, 1),
    )?;
    Ok(connection)
}

#[cfg(test)]
pub(crate) mod signature_tests;

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use crate::{
        http::{
            Codec,
            connection::{Endpoint, HttpPool},
        },
        model::{RequestId, ResourceClass},
        runtime::{admission::AdmissionPolicy, reactor::Reactor},
    };
    use std::{
        future::Future,
        task::{Context, Poll},
        time::Instant,
    };

    fn frame() -> MessageHead {
        MessageHead {
            start: StartLine::Request {
                method: "POST".into(),
                target: crate::peer::protocol::REQUEST_TARGET.into(),
            },
            headers: vec![crate::http::Header {
                name: "content-length".into(),
                value: b"0".to_vec(),
            }],
        }
    }
    fn clone_head(head: &MessageHead) -> MessageHead {
        let codec = Codec::new(crate::peer::protocol::MAX_ENVELOPE_HEAD);
        codec
            .decode_head(&codec.encode_head(head).unwrap())
            .unwrap()
            .unwrap()
            .0
    }
    pub(crate) fn pair() -> (Session, Session) {
        let n = super::signature_tests::network(3);
        let id = random().unwrap();
        (
            Session::new(n[0].clone(), n[1].node().clone(), id, 0),
            Session::new(n[1].clone(), n[0].node().clone(), id, 1),
        )
    }
    pub(crate) fn signer(session: &Session) -> Rc<Signatures> {
        session.signatures.clone()
    }
    pub(crate) fn hello(signatures: &Signatures, peer: &NodeId) -> MessageHead {
        head(signatures, peer, "hello", &[1; 32], &[0; 32], false).unwrap()
    }
    pub(crate) fn finish(
        signatures: &Signatures,
        peer: &NodeId,
        challenge: MessageHead,
    ) -> MessageHead {
        let (_, a, b) = verify(signatures, challenge, Some(peer), "challenge", true).unwrap();
        assert_eq!(a, [1; 32]);
        head(signatures, peer, "finish", &a, &b, false).unwrap()
    }
    #[test]
    fn historical_proof_requires_fresh_head_and_session_cannot_be_reinstalled() {
        let (mut a, mut b) = pair();
        let mut original = frame();
        p::push(&mut original, "racer-receiver", &a.peer.0);
        let proof = std::sync::Arc::new(a.signatures.sign(original).unwrap());
        let auth = crate::security::forwarding::ForwardedHead {
            original: proof.clone(),
            hops: vec![],
        };
        let mut previous = None;
        for sequence in 1..=2 {
            let wire = a
                .sign(crate::peer::protocol::encode_envelope(&auth, false, 0).unwrap())
                .unwrap();
            let copy = clone_head(&wire);
            let decoded = b.admit(wire).unwrap();
            let (retained, _) = crate::peer::protocol::decode_envelope(decoded, false).unwrap();
            assert_eq!(retained.original.signature, proof.signature);
            assert_eq!(b.rx, sequence);
            if let Some(old) = previous {
                assert!(b.admit(old).is_err());
            }
            previous = Some(copy);
        }
        let admission = flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let (socket, _peer) = std::os::unix::net::UnixStream::pair().unwrap();
        let mut conn = crate::http::connection::from_accepted(socket.into(), &admission).unwrap();
        crate::http::connection::install_session(&mut conn, a).unwrap();
        assert!(crate::http::connection::install_session(&mut conn, b).is_err());
        conn.set_framing(Some(0), Some(0), false);
        conn.next_round().unwrap();
        assert!(!conn.is_reusable());
        assert_eq!(conn.state().session.as_ref().unwrap().tx, 2);
    }
    #[test]
    fn parallel_connections_isolate_counters_and_wall_rollback_cannot_resurrect_frames() {
        let clock = crate::runtime::environment::SimulationClock::new_at(
            92,
            Instant::now(),
            std::time::SystemTime::now(),
        );
        let environment = clock.environment(1);
        let _guard = environment.enter();
        let (mut a, mut b) = pair();
        let id = random().unwrap();
        let mut other_a = Session::new(a.signatures.clone(), a.peer.clone(), id, 0);
        let mut other_b = Session::new(b.signatures.clone(), b.peer.clone(), id, 1);
        let old = a.sign(frame()).unwrap();
        b.admit(clone_head(&old)).unwrap();
        assert!(other_b.admit(clone_head(&old)).is_err());
        assert_eq!(other_b.rx, 0);
        other_b.admit(other_a.sign(frame()).unwrap()).unwrap();
        clock.advance(Duration::from_secs(61));
        clock.set_wall_time(crate::runtime::environment::wall_now() - Duration::from_secs(61));
        assert!(b.admit(old).is_err());
        assert_eq!(b.rx, 1);
        b.admit(a.sign(frame()).unwrap()).unwrap();
        assert_eq!(other_b.rx, 1);
    }
    pub(crate) fn replay_and_binding_checks() {
        let (mut a, mut b) = pair();
        let valid = a.sign(frame()).unwrap();
        for name in [
            "racer-sequence",
            "racer-session",
            "racer-direction",
            "racer-signer",
        ] {
            let mut bad = clone_head(&valid);
            bad.headers
                .iter_mut()
                .find(|h| h.name == name)
                .unwrap()
                .value = b"18446744073709551615".to_vec();
            assert!(b.admit(bad).is_err());
            assert_eq!(b.rx, 0, "forged {name} advanced sequence");
        }
        // Valid signatures with incorrect session, direction, identity or sequence.
        for mutation in 0..4 {
            let mut bad = Session::new(a.signatures.clone(), a.peer.clone(), a.id, a.direction);
            match mutation {
                0 => bad.id[0] ^= 1,
                1 => bad.direction = 1,
                2 => bad.signatures = b.signatures.clone(),
                _ => bad.tx = 9000,
            }
            assert!(b.admit(bad.sign(frame()).unwrap()).is_err());
            assert_eq!(b.rx, 0);
        }
        b.admit(clone_head(&valid)).unwrap();
        assert!(matches!(b.admit(valid), Err(Error::Replay)));
        assert_eq!(b.rx, 1);
        b.admit(a.sign(frame()).unwrap()).unwrap();
    }
    #[test]
    fn duplicate_wrong_session_direction_identity_and_forged_high_sequence() {
        replay_and_binding_checks();
    }

    #[test]
    pub(crate) fn more_than_4096_frames_at_fixed_time_have_constant_session_storage() {
        let clock = crate::runtime::environment::SimulationClock::new_at(
            71,
            Instant::now(),
            std::time::SystemTime::now(),
        );
        let environment = clock.environment(0);
        let _guard = environment.enter();
        let (mut a, mut b) = pair();
        let now = crate::runtime::environment::wall_now();
        let bytes = std::mem::size_of_val(&a) + std::mem::size_of_val(&b);
        for sequence in 1..=5000 {
            let h = a.sign(frame()).unwrap();
            assert_eq!(
                p::number(&h, "racer-timestamp").unwrap(),
                p::millis(now).unwrap()
            );
            b.admit(h).unwrap();
            assert_eq!(b.rx, sequence);
            assert_eq!(a.tx, sequence);
            assert_eq!(std::mem::size_of_val(&a) + std::mem::size_of_val(&b), bytes);
        }
        assert_eq!(clock.elapsed(), Duration::ZERO);
    }
    #[test]
    pub(crate) fn reconnect_restart_expiry_and_counter_overflow_fail_closed() {
        let (mut a, mut b) = pair();
        let old = a.sign(frame()).unwrap();
        let mut reconnected =
            Session::new(b.signatures.clone(), b.peer.clone(), random().unwrap(), 1);
        assert!(reconnected.admit(clone_head(&old)).is_err());
        assert_eq!(reconnected.rx, 0);
        b.expires = crate::runtime::environment::now();
        assert!(matches!(b.admit(old), Err(Error::DeadlineExceeded)));
        a.tx = u64::MAX;
        assert!(matches!(a.sign(frame()), Err(Error::Replay)));
        reconnected.rx = u64::MAX;
        let mut sender = Session::new(a.signatures.clone(), a.peer.clone(), reconnected.id, 0);
        assert!(matches!(
            reconnected.admit(sender.sign(frame()).unwrap()),
            Err(Error::Replay)
        ));
    }
    fn drive<T>(reactor: &Reactor, future: impl Future<Output = T>) -> T {
        let mut future = std::pin::pin!(future);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let watchdog = Instant::now() + Duration::from_secs(30);
        loop {
            if let Poll::Ready(value) = future.as_mut().poll(&mut cx) {
                return value;
            }
            assert!(Instant::now() < watchdog);
            reactor.poll_budgeted(128).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
    }
    #[test]
    fn concurrent_workers_complete_independent_connection_handshakes() {
        let workers: Vec<_> = (0..4)
            .map(|_| {
                std::thread::spawn(loopback_mutual_authentication_pool_reuse_and_fresh_reconnect)
            })
            .collect();
        for worker in workers {
            worker.join().unwrap();
        }
    }

    #[test]
    fn loopback_mutual_authentication_pool_reuse_and_fresh_reconnect() {
        let n = super::signature_tests::network(2);
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let io = HttpIo::with_admission(
            reactor.clone(),
            Codec::new(crate::peer::protocol::MAX_ENVELOPE_HEAD),
            admission.clone(),
            u64::MAX,
        );
        let pool = HttpPool::new(reactor.clone(), admission.clone(), 2);
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let endpoint = Endpoint::Peer(listener.local_addr().unwrap().to_string());
        let listener = Rc::new(crate::runtime::reactor::Descriptor::from(listener));
        let scope = RequestScope::new(RequestId([1; 16]), Instant::now() + Duration::from_secs(30))
            .unwrap();
        let mut previous_id = None;
        for _ in 0..2 {
            let client = async {
                let conn = pool.checkout(&endpoint, &scope).await?;
                connect(&io, conn, n[0].clone(), n[1].node(), &scope).await
            };
            let server = async {
                let fd = reactor.accept(listener.clone(), &scope).await?;
                let conn = crate::http::connection::from_accepted(fd, &admission)?;
                accept(&io, conn, n[1].clone(), &scope).await
            };
            let (mut client, mut server) =
                drive(&reactor, async { futures::try_join!(client, server) }).unwrap();
            let id = client.state().session.as_ref().unwrap().id;
            assert_eq!(id, server.state().session.as_ref().unwrap().id);
            assert_ne!(Some(id), previous_id);
            previous_id = Some(id);
            for sequence in 1..=3 {
                let exchange = async {
                    let response = io.exchange_head(client, frame(), &scope).await?;
                    let mut conn = response.connection;
                    conn.finish_exchange()?;
                    Ok::<_, Error>(conn)
                };
                let respond = async {
                    let received = io.receive_head(server, &scope).await?;
                    let mut response = frame();
                    response.start = StartLine::Response { status: 200 };
                    let mut conn = io
                        .send_head(received.connection, response, &scope)
                        .await?
                        .connection;
                    conn.finish_exchange()?;
                    Ok::<_, Error>(conn)
                };
                (client, server) =
                    drive(&reactor, async { futures::try_join!(exchange, respond) }).unwrap();
                assert_eq!(client.state().session.as_ref().unwrap().rx, sequence);
                drop(client);
                client = drive(&reactor, pool.checkout(&endpoint, &scope)).unwrap();
                assert_eq!(client.state().session.as_ref().unwrap().id, id);
                assert_eq!(client.state().session.as_ref().unwrap().tx, sequence);
            }
            client.poison();
            drop(client);
            drop(server);
        }
        pool.close();
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
    #[test]
    pub(crate) fn signed_challenges_bind_both_identities_protocol_and_fresh_randomness() {
        let n = super::signature_tests::network(3);
        let a = random().unwrap();
        let b = random().unwrap();
        assert_ne!(a, random().unwrap());
        assert_ne!(
            transcript(n[0].node(), n[1].node(), &a, &b),
            transcript(n[1].node(), n[0].node(), &a, &b)
        );
        let reply = head(&n[1], n[0].node(), "challenge", &a, &b, true).unwrap();
        let (_, x, y) = verify(
            &n[0],
            clone_head(&reply),
            Some(n[1].node()),
            "challenge",
            true,
        )
        .unwrap();
        assert_eq!((x, y), (a, b));
        assert!(
            verify(
                &n[0],
                clone_head(&reply),
                Some(n[2].node()),
                "challenge",
                true
            )
            .is_err()
        );
        assert!(verify(&n[0], clone_head(&reply), Some(n[1].node()), "ready", true).is_err());
        for field in [
            "racer-client-challenge",
            "racer-server-challenge",
            "racer-receiver",
            "racer-profile",
        ] {
            let mut bad = clone_head(&reply);
            bad.headers
                .iter_mut()
                .find(|h| h.name == field)
                .unwrap()
                .value[0] ^= 1;
            assert!(verify(&n[0], bad, Some(n[1].node()), "challenge", true).is_err());
        }
    }
    #[test]
    fn handshake_codec_bounds() {
        let n = super::signature_tests::network(2);
        let h = head(
            &n[1],
            n[0].node(),
            "challenge",
            &random().unwrap(),
            &random().unwrap(),
            true,
        )
        .unwrap();
        let codec = Codec::new(65536);
        let bytes = codec.encode_head(&h).unwrap();
        for end in 0..bytes.len() {
            assert!(codec.decode_head(&bytes[..end]).unwrap().is_none());
        }
        for value in [vec![0; 65537], vec![255; 4], vec![0; 3]] {
            let mut bad = clone_head(&h);
            bad.headers
                .iter_mut()
                .find(|h| h.name == "racer-certificates")
                .unwrap()
                .value = p::binary(&value).into_bytes();
            assert!(verify(&n[0], bad, Some(n[1].node()), "challenge", true).is_err());
        }
        let mut extra = clone_head(&h);
        p::push(&mut extra, "racer-extra", 1);
        assert!(verify(&n[0], extra, Some(n[1].node()), "challenge", true).is_err());
    }
    #[test]
    fn socket_admission_rejects_replay_before_dispatch_and_closes_pool_slot() {
        use crate::runtime::reactor::IoBuffer;
        let n = super::signature_tests::network(2);
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let io = HttpIo::with_admission(reactor.clone(), Codec::new(65536), admission.clone(), 0);
        let scope = RequestScope::new(RequestId([2; 16]), Instant::now() + Duration::from_secs(10))
            .unwrap();
        let (a, b) = std::os::unix::net::UnixStream::pair().unwrap();
        let a = crate::http::connection::from_accepted(a.into(), &admission).unwrap();
        let b = crate::http::connection::from_accepted(b.into(), &admission).unwrap();
        let (mut a, mut b) = drive(&reactor, async {
            futures::try_join!(
                connect(&io, a, n[0].clone(), n[1].node(), &scope),
                accept(&io, b, n[1].clone(), &scope)
            )
        })
        .unwrap();
        let valid = a
            .state_mut()
            .session
            .as_mut()
            .unwrap()
            .sign(frame())
            .unwrap();
        let bytes = Codec::new(65536).encode_head(&valid).unwrap();
        let mut dispatches = 0;
        for replay in [false, true] {
            let mut buffer = io.buffer(bytes.len()).unwrap();
            buffer.bytes_mut().unwrap().copy_from_slice(&bytes);
            let send = reactor.send(a.socket(), buffer, a, &scope);
            let receive = io.receive_head(b, &scope);
            let (sent, received) = drive(&reactor, async { futures::join!(send, receive) });
            a = sent.unwrap().lease;
            if replay {
                assert!(matches!(received, Err(Error::Replay)));
                break;
            }
            b = received.unwrap().connection;
            dispatches += 1;
        }
        assert_eq!(dispatches, 1);
        drop(a);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
    #[test]
    fn handshake_cancel_expiry_and_abandonment_retain_only_fenced_admissions() {
        let n = super::signature_tests::network(2);
        for end in ["cancel", "expiry", "drop"] {
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            )));
            let reactor = Rc::new(Reactor::new(admission.clone()));
            reactor.init().unwrap();
            let io =
                HttpIo::with_admission(reactor.clone(), Codec::new(65536), admission.clone(), 0);
            let scope = RequestScope::new(
                RequestId([3; 16]),
                Instant::now()
                    + if end == "expiry" {
                        Duration::from_millis(30)
                    } else {
                        Duration::from_secs(10)
                    },
            )
            .unwrap();
            let (socket, _peer) = std::os::unix::net::UnixStream::pair().unwrap();
            let conn = crate::http::connection::from_accepted(socket.into(), &admission).unwrap();
            let mut work = Box::pin(accept(&io, conn, n[1].clone(), &scope));
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(work.as_mut().poll(&mut cx).is_pending());
            assert_eq!(admission.used(ResourceClass::Connection), 1);
            if end == "cancel" {
                scope.cancel().unwrap();
            }
            if end != "drop" {
                let result = drive(&reactor, work.as_mut());
                assert!(matches!(
                    result,
                    Err(Error::Cancelled | Error::DeadlineExceeded)
                ));
            }
            drop(work);
            drive(&reactor, reactor.drain()).unwrap();
            assert_eq!(admission.used(ResourceClass::Connection), 0);
        }
    }
}
