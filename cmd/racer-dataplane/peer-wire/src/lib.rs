//! Canonical peer heads, retained proofs, and socket-owned replay admission.
//!
//! Payload interpretation, credentials, topology selection, and telemetry belong
//! to callers. This crate preserves the Racer v5 wire profile without importing
//! its application model. Historical proof verification does not admit a message;
//! the carrying connection must first admit its fresh session head.

pub mod control;
pub mod forwarding;
mod session;
#[cfg(test)]
mod tests;

pub use session::{Context, Session, accept, connect, install_session};

/// Test-only inspection and malformed-frame construction for adapter regressions.
#[cfg(feature = "test-util")]
pub mod test_util {
    use super::*;

    /// Borrow identity ownership for application key-rotation fixtures.
    pub fn keys(signatures: &Signatures) -> Rc<Keyring> {
        signatures.keys.clone()
    }

    /// Construct a canonical signature input for a malformed-frame fixture.
    pub fn signature_input(head: &MessageHead) -> Result<String> {
        super::signature_input(head)
    }

    /// Construct the exact RFC 9421 base for byte-vector fixtures.
    pub fn signature_base(head: &MessageHead) -> Result<Vec<u8>> {
        super::signature_base(head)
    }

    /// Sign an outer envelope fixture without the retained-head size limit.
    pub fn sign_fields(signatures: &Signatures, head: MessageHead) -> Result<SignedHead> {
        signatures.sign_fields(head)
    }

    pub use crate::session::test_util::*;
}

use base64::{Engine, engine::general_purpose::STANDARD};
use http1::{Header, MessageHead, StartLine};
use racer_control_wire::NodeId;
use racer_crypto::identity::{Certificates, Keyring, VerifiedPeer};
use sha2::{Digest, Sha256};
use std::{
    collections::BTreeSet,
    rc::Rc,
    sync::Arc,
    time::{Duration, SystemTime, UNIX_EPOCH},
};
use uring_runtime::environment::Deadline;

/// Failures contain no signed fields, credentials, or body bytes.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Error {
    /// The canonical wire representation is invalid.
    InvalidRequest,
    /// The encoded HTTP head exceeds its bound.
    HeaderTooLarge,
    /// Identity or field authentication failed.
    Unauthorized,
    /// A timestamp or session sequence is not admissible.
    Replay,
    /// The operation or retained authority expired.
    DeadlineExceeded,
    /// A route exceeded its monotonic limits.
    HopBudgetExhausted,
    /// Entropy is unavailable.
    Unavailable,
    /// Preserve the identity component's failure classification.
    Identity(racer_crypto::identity::Error),
    /// Preserve the runtime component's failure classification.
    Runtime(uring_runtime::Error),
}

/// A bounded wire mechanism result.
pub type Result<T> = std::result::Result<T, Error>;

impl From<http1::Error> for Error {
    fn from(value: http1::Error) -> Self {
        match value {
            http1::Error::Malformed => Self::InvalidRequest,
            http1::Error::HeadTooLarge => Self::HeaderTooLarge,
        }
    }
}

impl From<racer_crypto::identity::Error> for Error {
    fn from(value: racer_crypto::identity::Error) -> Self {
        Self::Identity(value)
    }
}

impl From<uring_runtime::Error> for Error {
    fn from(value: uring_runtime::Error) -> Self {
        Self::Runtime(value)
    }
}

/// Opaque field syntax used by the retained peer profile.
pub struct Opaque;

impl http1::Opaque for Opaque {
    const NAMES: &'static [&'static str] = &["authorization", "racer-metadata"];
}

type Codec = http1::Codec<Opaque>;

/// Application credential policy invoked at its original authentication boundary.
pub trait RequestMac {
    /// Add any request MAC fields before signature components are constructed.
    fn sign(&self, keys: &Keyring, head: &mut MessageHead) -> Result<()>;

    /// Check request credentials before certificate and signature verification.
    fn verify(&self, keys: &Keyring, head: &MessageHead) -> Result<()>;
}

/// Retained HTTP provenance signed with caller-owned identities and MAC policy.
pub struct Signatures {
    keys: Rc<Keyring>,

    certificates: Rc<Certificates>,

    request_mac: Rc<dyn RequestMac>,
}

/// Unverified signed HTTP provenance, excluding payload bytes.
pub struct SignedHead {
    /// Exact HTTP fields carried by the proof.
    pub head: MessageHead,

    /// Detached Ed25519 signature, also represented in the signature field.
    pub signature: Vec<u8>,
}

/// Authenticated provenance and its certificate identity.
pub struct VerifiedHead {
    signed: SignedHead,

    peer: VerifiedPeer,
}

impl VerifiedHead {
    /// Borrow the authenticated head without permitting mutation.
    pub fn signed(&self) -> &SignedHead {
        &self.signed
    }

    /// Borrow the authenticated peer identity.
    pub fn peer(&self) -> &VerifiedPeer {
        &self.peer
    }

    /// Consume authentication evidence into the original head and identity.
    pub fn into_parts(self) -> (SignedHead, VerifiedPeer) {
        (self.signed, self.peer)
    }
}

/// An original proof and ordered forwarding proofs, with no body interpretation.
pub struct ForwardedHead {
    /// Shared exact original proof retained by outstanding operations.
    pub original: Arc<SignedHead>,

    /// Ordered independently signed forwarding heads.
    pub hops: Vec<SignedHead>,
}

impl Signatures {
    /// Bind worker-local identity owners and application credential policy.
    pub fn new(
        keys: Rc<Keyring>,
        certificates: Rc<Certificates>,
        request_mac: Rc<dyn RequestMac>,
    ) -> Self {
        Self {
            keys,
            certificates,
            request_mac,
        }
    }

    /// Return the local authenticated node identity.
    pub fn node(&self) -> &NodeId {
        self.keys.node()
    }

    /// Sign retained provenance; replay admission belongs to its carrying session.
    pub fn sign(&self, head: MessageHead) -> Result<SignedHead> {
        if head.headers.iter().any(|h| {
            is_auth_field(&h.name.to_ascii_lowercase())
                && !h.name.eq_ignore_ascii_case("racer-receiver")
        }) {
            return Err(Error::InvalidRequest);
        }
        receiver(&head)?;
        let signed = self.sign_fields(head)?;
        Codec::new(MAX_HEAD).encode_head(&signed.head)?;
        Ok(signed)
    }

    pub(crate) fn sign_fields(&self, mut head: MessageHead) -> Result<SignedHead> {
        let identity = self.keys.signing_identity()?;
        if identity.node() != self.node() {
            return Err(Error::Unauthorized);
        }
        push(&mut head, "racer-profile", PROFILE);
        uuid(&self.keys.cluster().0)?;
        uuid(&self.node().0)?;
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
            millis(uring_runtime::environment::wall_now())?,
        );
        self.request_mac.sign(&self.keys, &mut head)?;
        let input = signature_input(&head)?;
        push(&mut head, "signature-input", format!("racer={input}"));
        let signature = identity.sign(&signature_base(&head)?)?;
        if signature.len() != 64 {
            return Err(Error::Unauthorized);
        }
        push(
            &mut head,
            "signature",
            format!("racer=:{}:", binary(&signature)),
        );
        Codec::new(MAX_ENVELOPE_HEAD).encode_head(&head)?;
        Ok(SignedHead { head, signature })
    }

    /// Verify retained provenance addressed to this node, without replay admission.
    pub fn verify_proof(&self, head: SignedHead) -> Result<VerifiedHead> {
        let peer = self.verify_historical(&head)?;
        if receiver(&head.head)? != *self.node() {
            return Err(Error::Unauthorized);
        }
        Ok(VerifiedHead { signed: head, peer })
    }

    /// Verify a historical hop under the fresh-message timestamp window.
    pub fn verify_historical(&self, signed: &SignedHead) -> Result<VerifiedPeer> {
        self.verify_signed_age(signed, true)
    }

    /// Revalidate retained request authority bounded by its original signed deadline.
    /// The caller must additionally retain its verified path and consumed deadline.
    pub fn verify_retained_request(
        &self,
        signed: &SignedHead,
        deadline: u64,
    ) -> Result<VerifiedPeer> {
        if deadline <= millis(uring_runtime::environment::wall_now())? {
            return Err(Error::DeadlineExceeded);
        }
        if field(&signed.head, "racer-kind")? != "request"
            || deadline > number(&signed.head, "racer-route-deadline")?
        {
            return Err(Error::Unauthorized);
        }
        self.verify_signed_age(signed, false)
    }

    fn verify_signed_age(&self, signed: &SignedHead, fresh: bool) -> Result<VerifiedPeer> {
        let head = &signed.head;
        if field(head, "racer-profile")? != PROFILE
            || field(head, "racer-cluster")? != self.keys.cluster().0
        {
            return Err(Error::Unauthorized);
        }
        uuid(&self.keys.cluster().0)?;
        self.request_mac.verify(&self.keys, head)?;
        let base = signature_base(head)?;
        if signed.signature.len() != 64
            || field(head, "signature")? != format!("racer=:{}:", binary(&signed.signature))
        {
            return Err(Error::Unauthorized);
        }
        let signer = node_field(head, "racer-signer")?;
        let chain = decode_chain(&decode_binary(
            field(head, "racer-certificates")?.as_bytes(),
        )?)?;
        let timestamp = UNIX_EPOCH
            .checked_add(Duration::from_millis(number(head, "racer-timestamp")?))
            .ok_or(Error::Unauthorized)?;
        let now = uring_runtime::environment::wall_now();
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
            .map_err(Into::into)
    }
}

/// Fields reserved exclusively to the authentication layer.
pub fn is_auth_field(name: &str) -> bool {
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

/// Canonical domain-separated request-MAC message, excluding the MAC itself.
pub fn mac_base(head: &MessageHead) -> Result<Vec<u8>> {
    fn append(out: &mut Vec<u8>, value: &[u8]) -> Result<()> {
        let length = u32::try_from(value.len()).map_err(|_| Error::InvalidRequest)?;
        out.extend_from_slice(&length.to_be_bytes());
        out.extend_from_slice(value);
        Ok(())
    }
    let mut out = b"racer/request-mac/message/v1\0".to_vec();
    let start = match &head.start {
        StartLine::Request { method, target } => format!("{method} {target}"),
        _ => return Err(Error::Unauthorized),
    };
    append(&mut out, start.as_bytes())?;
    for name in components(head)? {
        if name.starts_with('@') || name == "racer-request-mac" {
            continue;
        }
        append(&mut out, name.as_bytes())?;
        append(&mut out, head.unique(&name)?.ok_or(Error::Unauthorized)?)?;
    }
    Ok(out)
}

/// Parse a canonical UUID node field.
pub fn node_field(head: &MessageHead, name: &str) -> Result<NodeId> {
    let node = field(head, name)?;
    uuid(&node)?;
    Ok(NodeId(node))
}

/// Parse the signed immediate receiver.
pub fn receiver(head: &MessageHead) -> Result<NodeId> {
    node_field(head, "racer-receiver")
}

fn components(head: &MessageHead) -> Result<Vec<String>> {
    Codec::new(MAX_ENVELOPE_HEAD).encode_head(head)?;
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
    uuid(&keyid)?;
    Ok(format!(
        "({components});created={timestamp};keyid=\"{keyid}\";alg=\"ed25519\";tag=\"{}\"",
        PROFILE
    ))
}

/// RFC 9421 section 2.5 signature base under the strict canonical peer profile.
fn signature_base(head: &MessageHead) -> Result<Vec<u8>> {
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
            _ => String::from_utf8(head.unique(&name)?.ok_or(Error::Unauthorized)?.to_vec())
                .map_err(|_| Error::Unauthorized)?,
        };
        lines.push(format!("\"{name}\": {value}"));
    }
    lines.push(format!("\"@signature-params\": {input}"));
    Ok(lines.join("\n").into_bytes())
}

/// Bind the exact signature base and signature without hashing payload bytes.
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

/// Current framing version.
pub const VERSION: &str = "5";
/// Current exchange target.
pub const REQUEST_TARGET: &str = "/racer/peer/v5/exchange";
/// Maximum carried hop count.
pub const MAX_HOPS: usize = 8;
/// Maximum retained signed HTTP head.
pub const MAX_SIGNED_HEAD: usize = MAX_HEAD;
/// Maximum expanded envelope and its outer authentication head.
pub const MAX_ENVELOPE_HEAD: usize = (MAX_HOPS + 1) * (MAX_SIGNED_HEAD * 2);
/// Canonical signature profile tag.
pub const PROFILE: &str = "racer-peer-v5";
/// Maximum ordinary canonical field or head.
pub const MAX_HEAD: usize = 64 * 1024;
/// Maximum opaque payload admitted by this wire profile, including the AEAD tag.
pub const MAX_BODY: usize = 16 * 1024 * 1024 + 16;

/// Reject noncanonical UUID spelling at the trust boundary.
pub fn uuid(value: &str) -> Result<()> {
    if !racer_control_wire::valid_uuid(value) {
        return Err(Error::InvalidRequest);
    }
    Ok(())
}
/// Encode padded standard base64.
pub fn binary(bytes: &[u8]) -> String {
    STANDARD.encode(bytes)
}
/// Decode only canonical, bounded padded standard base64.
pub fn decode_binary(value: &[u8]) -> Result<Vec<u8>> {
    if value.len() > MAX_HEAD {
        return Err(Error::InvalidRequest);
    }
    let bytes = STANDARD.decode(value).map_err(|_| Error::Unauthorized)?;
    if binary(&bytes).as_bytes() != value {
        return Err(Error::Unauthorized);
    }
    Ok(bytes)
}
/// Read one bounded UTF-8 field, rejecting duplicates and absence.
pub fn field(head: &MessageHead, name: &str) -> Result<String> {
    let value = head.unique(name)?.ok_or(Error::Unauthorized)?;
    if value.len() > MAX_HEAD {
        return Err(Error::InvalidRequest);
    }
    String::from_utf8(value.to_vec()).map_err(|_| Error::Unauthorized)
}
/// Read an unsigned integer in minimal decimal spelling.
pub fn number(head: &MessageHead, name: &str) -> Result<u64> {
    let value = field(head, name)?;
    let n: u64 = value.parse().map_err(|_| Error::Unauthorized)?;
    if n.to_string() != value {
        return Err(Error::Unauthorized);
    }
    Ok(n)
}
/// Append one field without normalizing its value.
pub fn push(head: &mut MessageHead, name: &str, value: impl ToString) {
    head.headers.push(Header {
        name: name.into(),
        value: value.to_string().into_bytes(),
    });
}
/// Append a canonical binary field.
pub fn push_binary(head: &mut MessageHead, name: &str, bytes: &[u8]) {
    push(head, name, binary(bytes));
}
/// Map a wall-clock timestamp to canonical milliseconds.
pub fn millis(time: SystemTime) -> Result<u64> {
    uring_runtime::environment::unix_millis(time).map_err(Into::into)
}
/// Encode a deadline using the environment's stable clock mapping.
pub fn encode_deadline(deadline: Deadline) -> Result<u64> {
    deadline.to_unix_millis().map_err(Into::into)
}
/// Decode a deadline without renewing its authority at each hop.
pub fn decode_deadline(value: u64) -> Result<Deadline> {
    Deadline::from_unix_millis(value).map_err(Into::into)
}
/// Encode a bounded node list with big-endian length prefixes and padded base64.
pub fn nodes(nodes: &[NodeId]) -> Result<String> {
    if nodes.len() > MAX_HOPS + 1 {
        return Err(Error::HopBudgetExhausted);
    }
    let mut bytes = Vec::new();
    for node in nodes {
        uuid(&node.0)?;
        bytes.extend_from_slice(&(node.0.len() as u32).to_be_bytes());
        bytes.extend_from_slice(node.0.as_bytes());
    }
    Ok(binary(&bytes))
}
/// Decode a canonical node list, rejecting duplicates and malformed framing.
pub fn decode_nodes(value: &[u8]) -> Result<Vec<NodeId>> {
    let bytes = decode_binary(value)?;
    let mut rest = bytes.as_slice();
    let mut result = Vec::new();
    while !rest.is_empty() {
        if rest.len() < 4 || result.len() > MAX_HOPS {
            return Err(Error::Unauthorized);
        }
        let length =
            u32::from_be_bytes(rest[..4].try_into().map_err(|_| Error::Unauthorized)?) as usize;
        rest = &rest[4..];
        if length == 0 || length > 256 || rest.len() < length {
            return Err(Error::Unauthorized);
        }
        let node =
            NodeId(String::from_utf8(rest[..length].to_vec()).map_err(|_| Error::Unauthorized)?);
        uuid(&node.0)?;
        if result.contains(&node) {
            return Err(Error::Unauthorized);
        }
        result.push(node);
        rest = &rest[length..];
    }
    Ok(result)
}

/// Compare all logical fields, optionally excluding the fixed route schema.
pub fn agrees(actual: &MessageHead, expected: &MessageHead, ignore_route: bool) -> Result<()> {
    fn start(head: &MessageHead) -> String {
        match &head.start {
            StartLine::Request { method, target } => format!("{method} {target}"),
            StartLine::Response { status } => status.to_string(),
        }
    }
    let fields = |head: &MessageHead| -> Result<std::collections::BTreeMap<String, Vec<u8>>> {
        let mut map = std::collections::BTreeMap::new();
        for h in &head.headers {
            let name = h.name.to_ascii_lowercase();
            if is_auth_field(&name)
                || (ignore_route
                    && matches!(
                        name.as_str(),
                        "racer-route-membership"
                            | "racer-route-request"
                            | "racer-route-attempt"
                            | "racer-route-destination"
                            | "racer-route-visited"
                            | "racer-route-links"
                            | "racer-route-attempts"
                            | "racer-route-deadline"
                    ))
            {
                continue;
            }
            if map.insert(name, h.value.clone()).is_some() {
                return Err(Error::Unauthorized);
            }
        }
        Ok(map)
    };
    if start(actual) != start(expected) || fields(actual)? != fields(expected)? {
        return Err(Error::Unauthorized);
    }
    Ok(())
}

/// Encode framing only, retaining the exact original and all hop proofs.
pub fn encode_envelope(
    authentication: &ForwardedHead,
    response: bool,
    body_length: usize,
) -> Result<MessageHead> {
    if authentication.hops.len() > MAX_HOPS
        || body_length > MAX_BODY
        || (!response && body_length != 0)
    {
        return Err(Error::InvalidRequest);
    }
    let mut headers = vec![
        Header {
            name: "racer-peer-version".into(),
            value: VERSION.as_bytes().to_vec(),
        },
        Header {
            name: "content-length".into(),
            value: body_length.to_string().into_bytes(),
        },
        Header {
            name: "racer-original".into(),
            value: encode_signed(&authentication.original)?,
        },
    ];
    for (index, head) in authentication.hops.iter().enumerate() {
        headers.push(Header {
            name: format!("racer-hop-{index}"),
            value: encode_signed(head)?,
        });
    }
    let head = MessageHead {
        start: if response {
            StartLine::Response { status: 200 }
        } else {
            StartLine::Request {
                method: "POST".into(),
                target: REQUEST_TARGET.into(),
            }
        },
        headers,
    };
    Codec::new(MAX_ENVELOPE_HEAD).encode_head(&head)?;
    Ok(head)
}

/// Decode framing; callers separately verify proofs and admit the carrying session.
pub fn decode_envelope(head: MessageHead, response: bool) -> Result<(ForwardedHead, usize)> {
    match (&head.start, response) {
        (StartLine::Request { method, target }, false)
            if method == "POST" && target == REQUEST_TARGET => {}
        (StartLine::Response { status: 200 }, true) => {}
        _ => return Err(Error::InvalidRequest),
    }
    let mut original = None;
    let mut version = false;
    let mut length = None;
    let mut hops = std::collections::BTreeMap::new();
    let mut seen = BTreeSet::new();
    let mut total = 0usize;
    for header in head.headers {
        total = total
            .checked_add(header.name.len())
            .and_then(|n| n.checked_add(header.value.len()))
            .ok_or(Error::InvalidRequest)?;
        if total > MAX_ENVELOPE_HEAD {
            return Err(Error::InvalidRequest);
        }
        let name = header.name.to_ascii_lowercase();
        if !seen.insert(name.clone()) {
            return Err(Error::InvalidRequest);
        }
        match name.as_str() {
            "racer-peer-version" => {
                if header.value != VERSION.as_bytes() {
                    return Err(Error::InvalidRequest);
                }
                version = true;
            }
            "content-length" => {
                let text = std::str::from_utf8(&header.value).map_err(|_| Error::InvalidRequest)?;
                let parsed = text.parse::<usize>().map_err(|_| Error::InvalidRequest)?;
                if text != parsed.to_string() {
                    return Err(Error::InvalidRequest);
                }
                length = Some(parsed);
            }
            "racer-original" => original = Some(Arc::new(decode_signed(&header.value)?)),
            "connection" | "host" => {}
            _ => {
                let suffix = name
                    .strip_prefix("racer-hop-")
                    .ok_or(Error::InvalidRequest)?;
                let index = suffix.parse::<usize>().map_err(|_| Error::InvalidRequest)?;
                if index >= MAX_HOPS || suffix != index.to_string() {
                    return Err(Error::InvalidRequest);
                }
                hops.insert(index, decode_signed(&header.value)?);
            }
        }
    }
    let length = length.ok_or(Error::InvalidRequest)?;
    if !version || length > MAX_BODY || (!response && length != 0) {
        return Err(Error::InvalidRequest);
    }
    if hops.keys().copied().ne(0..hops.len()) {
        return Err(Error::InvalidRequest);
    }
    Ok((
        ForwardedHead {
            original: original.ok_or(Error::InvalidRequest)?,
            hops: hops.into_values().collect(),
        },
        length,
    ))
}

/// Frame one detached signature followed by its exact HTTP head in padded base64.
pub fn encode_signed(head: &SignedHead) -> Result<Vec<u8>> {
    if head.signature.len() != 64 {
        return Err(Error::InvalidRequest);
    }
    let bytes = Codec::new(MAX_SIGNED_HEAD).encode_head(&head.head)?;
    let mut framed = Vec::with_capacity(bytes.len() + 64);
    framed.extend_from_slice(&head.signature);
    framed.extend_from_slice(&bytes);
    Ok(STANDARD.encode(framed).into_bytes())
}

/// Decode exactly one bounded signed head, rejecting trailing bytes.
pub fn decode_signed(bytes: &[u8]) -> Result<SignedHead> {
    if bytes.len() > (MAX_SIGNED_HEAD + 64).div_ceil(3) * 4 {
        return Err(Error::InvalidRequest);
    }
    let decoded = STANDARD.decode(bytes).map_err(|_| Error::InvalidRequest)?;
    if STANDARD.encode(&decoded).as_bytes() != bytes || decoded.len() <= 64 {
        return Err(Error::InvalidRequest);
    }
    let (head, consumed) = Codec::new(MAX_SIGNED_HEAD)
        .decode_head(&decoded[64..])?
        .ok_or(Error::InvalidRequest)?;
    if consumed != decoded.len() - 64 {
        return Err(Error::InvalidRequest);
    }
    Ok(SignedHead {
        head,
        signature: decoded[..64].to_vec(),
    })
}
