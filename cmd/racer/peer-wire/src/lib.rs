//! Canonical peer heads, retained proofs, and socket-owned replay admission.
//!
//! Payload interpretation, credentials, topology selection, and telemetry belong
//! to callers. This crate implements the Racer peer wire profile without importing
//! its application model. Historical proof verification does not admit a message;
//! the carrying connection must first admit its fresh session head.

mod chains;
pub mod control;
pub mod forwarding;
pub mod link;
mod session;
#[cfg(test)]
mod tests;

pub use chains::{
    Chain, Chains, MAX_CHAIN_BYTES, MAX_CHAINS_FIELD, MAX_CHAINS_PER_MESSAGE, MAX_SESSION_CHAINS,
    fingerprint, resolve,
};
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

    pub use crate::chains::test_util::*;
    pub use crate::session::test_util::*;
}

use base64::{Engine, engine::general_purpose::STANDARD};
use http1::{Header, MessageHead, StartLine};
use racer_crypto::identity::{Certificates, Keyring, VerifiedPeer};
use racer_wire::control::NodeId;
use sha2::{Digest, Sha256};
use std::{
    collections::BTreeSet,
    rc::Rc,
    sync::Arc,
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

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
    Runtime(racer_io::Error),
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

impl From<racer_io::Error> for Error {
    fn from(value: racer_io::Error) -> Self {
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

    /// Worker clock and entropy for timestamps, deadlines, and challenges.
    ctx: racer_io::WorkerCtx,

    /// The local identity's chain, reused while the identity keeps it.
    local: std::cell::RefCell<Option<Arc<Chain>>>,
}

/// Unverified signed HTTP provenance, excluding payload bytes.
pub struct SignedHead {
    /// Exact HTTP fields carried by the proof.
    pub head: MessageHead,

    /// Detached Ed25519 signature, also represented in the signature field.
    pub signature: Vec<u8>,

    /// Not on the wire: the chain named by the signed `racer-certificate`
    /// fingerprint, resolved from the carrying session when decoded or set by
    /// the signer. Verification requires it and checks it against the field.
    pub chain: Option<Arc<Chain>>,
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
        ctx: racer_io::WorkerCtx,
    ) -> Self {
        Self {
            keys,
            certificates,
            request_mac,
            ctx,
            local: Default::default(),
        }
    }

    /// Return the local identity's chain, re-encoding only after rotation.
    fn local_chain(
        &self,
        identity: &racer_crypto::identity::SigningIdentity,
    ) -> Result<Arc<Chain>> {
        let mut local = self.local.borrow_mut();
        if let Some(chain) = local.as_ref()
            && chain.certificates() == identity.certificate_chain()
        {
            return Ok(chain.clone());
        }
        let chain = Chain::new(identity.certificate_chain())?;
        chains::remember(&chain);
        *local = Some(chain.clone());
        Ok(chain)
    }

    /// Worker clock and entropy used by this signer and its sessions.
    pub fn ctx(&self) -> &racer_io::WorkerCtx {
        &self.ctx
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
        let signed = self.sign_fields_with(head, false)?;
        Codec::new(MAX_SIGNED_HEAD).encode_head(&signed.head)?;
        Ok(signed)
    }

    /// Sign a handshake head that also carries the signer's full chain.
    pub(crate) fn sign_handshake(&self, head: MessageHead) -> Result<SignedHead> {
        receiver(&head)?;
        let signed = self.sign_fields_with(head, true)?;
        Codec::new(MAX_HEAD).encode_head(&signed.head)?;
        Ok(signed)
    }

    pub(crate) fn sign_fields(&self, head: MessageHead) -> Result<SignedHead> {
        self.sign_fields_with(head, false)
    }

    fn sign_fields_with(&self, mut head: MessageHead, full_chain: bool) -> Result<SignedHead> {
        let identity = self.keys.signing_identity()?;
        if identity.node() != self.node() {
            return Err(Error::Unauthorized);
        }
        let chain = self.local_chain(&identity)?;
        push(&mut head, "racer-profile", PROFILE);
        uuid(&self.keys.cluster().0)?;
        uuid(&self.node().0)?;
        push(&mut head, "racer-cluster", &self.keys.cluster().0);
        push(&mut head, "racer-signer", &self.node().0);
        push_binary(&mut head, "racer-certificate", chain.fingerprint());
        if full_chain {
            push_binary(&mut head, "racer-certificates", chain.encoded());
        }
        push(
            &mut head,
            "racer-timestamp",
            millis(self.ctx.clock.wall_now())?,
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
        Ok(SignedHead {
            head,
            signature,
            chain: Some(chain),
        })
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
        self.verify_signed_age(
            &signed.head,
            &signed.signature,
            signed.chain.as_deref(),
            true,
        )
    }

    /// Revalidate retained request authority bounded by its original signed deadline.
    /// The caller must additionally retain its verified path and consumed deadline.
    pub fn verify_retained_request(
        &self,
        signed: &SignedHead,
        deadline: u64,
    ) -> Result<VerifiedPeer> {
        if deadline <= millis(self.ctx.clock.wall_now())? {
            return Err(Error::DeadlineExceeded);
        }
        if field(&signed.head, "racer-kind")? != "request"
            || deadline > number(&signed.head, "racer-route-deadline")?
        {
            return Err(Error::Unauthorized);
        }
        self.verify_signed_age(
            &signed.head,
            &signed.signature,
            signed.chain.as_deref(),
            false,
        )
    }

    fn verify_signed_age(
        &self,
        head: &MessageHead,
        signature: &[u8],
        chain: Option<&Chain>,
        fresh: bool,
    ) -> Result<VerifiedPeer> {
        if field(head, "racer-profile")? != PROFILE
            || field(head, "racer-cluster")? != self.keys.cluster().0
        {
            return Err(Error::Unauthorized);
        }
        uuid(&self.keys.cluster().0)?;
        self.request_mac.verify(&self.keys, head)?;
        let base = signature_base(head)?;
        if signature.len() != 64
            || field(head, "signature")? != format!("racer=:{}:", binary(signature))
        {
            return Err(Error::Unauthorized);
        }
        let signer = node_field(head, "racer-signer")?;
        let chain = chain.ok_or(Error::Unauthorized)?;
        if fingerprint(head)? != *chain.fingerprint() {
            return Err(Error::Unauthorized);
        }
        let timestamp = UNIX_EPOCH
            .checked_add(Duration::from_millis(number(head, "racer-timestamp")?))
            .ok_or(Error::Unauthorized)?;
        let now = self.ctx.clock.wall_now();
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
            .verify_signed_fingerprint(
                chain.fingerprint(),
                chain.certificates(),
                &signer,
                &base,
                signature,
            )
            .map_err(Into::into)
    }
}

/// Fields reserved exclusively to the authentication layer.
pub fn is_auth_field(name: &str) -> bool {
    [
        "signature-input",
        "signature",
        "racer-profile",
        "racer-cluster",
        "racer-signer",
        "racer-receiver",
        "racer-certificate",
        "racer-certificates",
        "racer-chains",
        "racer-proof-signers",
        "racer-session",
        "racer-direction",
        "racer-sequence",
        "racer-timestamp",
        "racer-mac-key",
        "racer-request-mac",
    ]
    .iter()
    .any(|reserved| name.eq_ignore_ascii_case(reserved))
}

/// Canonical domain-separated request-MAC message, excluding the MAC itself.
pub fn mac_base(head: &MessageHead) -> Result<Vec<u8>> {
    fn append(out: &mut Vec<u8>, value: &[u8]) -> Result<()> {
        let length = u32::try_from(value.len()).map_err(|_| Error::InvalidRequest)?;
        out.extend_from_slice(&length.to_be_bytes());
        out.extend_from_slice(value);
        Ok(())
    }
    let StartLine::Request { method, target } = &head.start else {
        return Err(Error::Unauthorized);
    };
    let components = Components::new(head)?;
    let mut out = b"racer/request-mac/message/v1\0".to_vec();
    let start_len =
        u32::try_from(method.len() + 1 + target.len()).map_err(|_| Error::InvalidRequest)?;
    out.extend_from_slice(&start_len.to_be_bytes());
    out.extend_from_slice(method.as_bytes());
    out.push(b' ');
    out.extend_from_slice(target.as_bytes());
    for (name, value) in &components.fields {
        if name.starts_with('@') || name == "racer-request-mac" {
            continue;
        }
        append(&mut out, name.as_bytes())?;
        append(&mut out, value)?;
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

/// Covered components of one head, validated and indexed once per frame.
///
/// `fields` holds lowercased header names (excluding the signature fields)
/// sorted bytewise with their values; duplicate names are rejected here, so
/// each lookup is unique by construction.
struct Components<'a> {
    start: &'a StartLine,
    fields: Vec<(String, &'a [u8])>,
}

impl<'a> Components<'a> {
    fn new(head: &'a MessageHead) -> Result<Self> {
        // Full head validation (names, size, framing); not implied elsewhere on
        // the verify path.
        Codec::new(MAX_ENVELOPE_HEAD).encode_head(head)?;
        let mut fields = Vec::with_capacity(head.headers.len());
        for h in &head.headers {
            if h.value.first().is_some_and(|b| b.is_ascii_whitespace())
                || h.value.last().is_some_and(|b| b.is_ascii_whitespace())
                || h.value.iter().any(u8::is_ascii_control)
            {
                return Err(Error::Unauthorized);
            }
            fields.push((h.name.to_ascii_lowercase(), h.value.as_slice()));
        }
        fields.sort_unstable_by(|a, b| a.0.cmp(&b.0));
        if fields.windows(2).any(|w| w[0].0 == w[1].0) {
            return Err(Error::Unauthorized);
        }
        fields.retain(|(name, _)| name != "signature" && name != "signature-input");
        Ok(Self {
            start: &head.start,
            fields,
        })
    }

    fn derived(&self) -> &'static [&'static str] {
        match self.start {
            StartLine::Request { .. } => &["@method", "@request-target"],
            StartLine::Response { .. } => &["@status"],
        }
    }

    fn signature_input(&self, head: &MessageHead) -> Result<String> {
        let timestamp = number(head, "racer-timestamp")? / 1000;
        let keyid = field(head, "racer-signer")?;
        uuid(&keyid)?;
        let mut out = String::with_capacity(160 + 24 * self.fields.len());
        out.push('(');
        let names = self
            .derived()
            .iter()
            .copied()
            .chain(self.fields.iter().map(|(n, _)| n.as_str()));
        for (i, name) in names.enumerate() {
            if i > 0 {
                out.push(' ');
            }
            out.push('"');
            out.push_str(name);
            out.push('"');
        }
        use std::fmt::Write as _;
        let _ = write!(
            out,
            ");created={timestamp};keyid=\"{keyid}\";alg=\"ed25519\";tag=\"{PROFILE}\""
        );
        Ok(out)
    }

    fn signature_base(&self, input: &str) -> Vec<u8> {
        let mut base = Vec::with_capacity(
            128 + input.len()
                + self
                    .fields
                    .iter()
                    .map(|(n, v)| n.len() + v.len() + 5)
                    .sum::<usize>(),
        );
        let line = |base: &mut Vec<u8>, name: &str, value: &[u8]| {
            base.push(b'"');
            base.extend_from_slice(name.as_bytes());
            base.extend_from_slice(b"\": ");
            base.extend_from_slice(value);
            base.push(b'\n');
        };
        match self.start {
            StartLine::Request { method, target } => {
                line(&mut base, "@method", method.as_bytes());
                line(&mut base, "@request-target", target.as_bytes());
            }
            StartLine::Response { status } => {
                line(&mut base, "@status", status.to_string().as_bytes());
            }
        }
        for (name, value) in &self.fields {
            line(&mut base, name, value);
        }
        base.extend_from_slice(b"\"@signature-params\": ");
        base.extend_from_slice(input.as_bytes());
        base
    }
}

fn signature_input(head: &MessageHead) -> Result<String> {
    Components::new(head)?.signature_input(head)
}

/// RFC 9421 section 2.5 signature base under the strict canonical peer profile.
fn signature_base(head: &MessageHead) -> Result<Vec<u8>> {
    let components = Components::new(head)?;
    let input = components.signature_input(head)?;
    let carried = field(head, "signature-input")?;
    if carried.strip_prefix("racer=") != Some(input.as_str()) {
        return Err(Error::Unauthorized);
    }
    Ok(components.signature_base(&input))
}

/// Bind the exact signature base and signature without hashing payload bytes.
pub fn signed_digest(head: &SignedHead) -> Result<[u8; 32]> {
    let base = signature_base(&head.head)?;
    let mut hash = Sha256::new();
    hash.update(b"racer-peer-v1/signed-head\0");
    hash.update((base.len() as u64).to_be_bytes());
    hash.update(base);
    hash.update((head.signature.len() as u64).to_be_bytes());
    hash.update(&head.signature);
    Ok(hash.finalize().into())
}

/// Current framing version.
pub const VERSION: &str = "2";
/// Current exchange target.
pub const REQUEST_TARGET: &str = "/racer/peer/v1/exchange";
/// Maximum carried hop count (links on one route), shared with `racer_wire::route`.
pub const MAX_HOPS: usize = racer_wire::route::MAX_LINKS as usize;
/// Maximum retained signed HTTP head (one proof).
///
/// Proofs name their chain by fingerprint, so this bounds only logical fields.
/// The largest request proof carries 8 KiB of metadata and 8 KiB of sealed
/// authorization, about 22 KiB in base64, plus fixed route and auth fields.
pub const MAX_SIGNED_HEAD: usize = 32 * 1024;
/// Padded base64 length of `n` bytes.
const fn base64_len(n: usize) -> usize {
    n.div_ceil(3) * 4
}
/// Outer session fields, framing, and the sender-local proof-signer hint.
const OUTER_HEAD: usize = 16 * 1024;
/// Maximum expanded envelope and its outer authentication head: every proof
/// (original and hops) framed with its detached signature, the most chains one
/// message can attach, and the outer session fields.
pub const MAX_ENVELOPE_HEAD: usize =
    (MAX_HOPS + 1) * (base64_len(64 + MAX_SIGNED_HEAD) + 64) + MAX_CHAINS_FIELD + OUTER_HEAD;
/// Canonical signature profile tag.
pub const PROFILE: &str = "racer-peer-v2";
/// Maximum ordinary canonical field or head.
pub const MAX_HEAD: usize = 64 * 1024;
/// Maximum opaque payload admitted by this wire profile, including the AEAD tag.
pub const MAX_BODY: usize = 16 * 1024 * 1024 + 16;

/// Reject noncanonical UUID spelling at the trust boundary.
pub fn uuid(value: &str) -> Result<()> {
    if !racer_wire::control::valid_uuid(value) {
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
    racer_io::ctx::unix_millis(time).map_err(Into::into)
}
/// Encode a deadline using `clock`'s stable anchor.
pub fn encode_deadline(clock: &racer_io::Clock, deadline: Instant) -> Result<u64> {
    racer_io::ctx::instant_to_unix_millis(deadline, clock.anchor()).map_err(Into::into)
}
/// Decode a deadline through `clock`'s stable anchor without renewing its
/// authority at each hop.
pub fn decode_deadline(clock: &racer_io::Clock, value: u64) -> Result<Instant> {
    racer_io::ctx::unix_millis_to_instant(value, clock.anchor()).map_err(Into::into)
}
/// Encode a bounded node list with big-endian length prefixes and padded base64.
pub fn nodes(nodes: &[NodeId]) -> Result<String> {
    if nodes.len() > MAX_HOPS + 1 {
        return Err(Error::HopBudgetExhausted);
    }
    let mut bytes = Vec::new();
    for (index, node) in nodes.iter().enumerate() {
        uuid(&node.0)?;
        if nodes[..index].contains(node) {
            return Err(Error::InvalidRequest);
        }
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
    // Sender-local hint: the carrying session replaces it with the chains this
    // session has not yet sent, so no proof is decoded again on the send path.
    let mut signers: Vec<[u8; 32]> = Vec::with_capacity(authentication.hops.len() + 1);
    for proof in std::iter::once(authentication.original.as_ref()).chain(&authentication.hops) {
        let fingerprint = fingerprint(&proof.head)?;
        if !signers.contains(&fingerprint) {
            signers.push(fingerprint);
        }
    }
    headers.push(Header {
        name: PROOF_SIGNERS.into(),
        value: signers
            .iter()
            .map(|f| binary(f))
            .collect::<Vec<_>>()
            .join(",")
            .into_bytes(),
    });
    if !response {
        headers.push(Header::new("host", b""));
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

/// Status of the unsigned answer to a request its holder could not verify for overload.
pub const OVERLOADED_STATUS: u16 = 429;

/// The unsigned overload answer: status [`OVERLOADED_STATUS`] and an empty body. A holder sends
/// it when it sheds a request before verifying it, for example with its crypto queue full. It
/// carries nothing from the request and costs no key operation, so it reveals only that the
/// holder is busy, which closing the connection would reveal as well.
pub fn overloaded_head() -> MessageHead {
    MessageHead {
        start: StartLine::Response {
            status: OVERLOADED_STATUS,
        },
        headers: vec![Header {
            name: "content-length".into(),
            value: b"0".to_vec(),
        }],
    }
}

/// Whether `head` is exactly [`overloaded_head`]. Anything else keeps the signed envelope path.
pub fn is_overloaded(head: &MessageHead) -> bool {
    matches!(
        head.start,
        StartLine::Response {
            status: OVERLOADED_STATUS
        }
    ) && matches!(
        head.headers.as_slice(),
        [Header { name, value }] if name.eq_ignore_ascii_case("content-length") && value == b"0"
    )
}

/// Unsigned sender-local field naming every proof signer's chain fingerprint.
pub(crate) const PROOF_SIGNERS: &str = "racer-proof-signers";

/// Parse the proof-signer hint written by [`encode_envelope`].
pub(crate) fn proof_signers(value: &[u8]) -> Result<Vec<[u8; 32]>> {
    let mut signers = Vec::new();
    for part in value.split(|b| *b == b',') {
        if signers.len() == MAX_CHAINS_PER_MESSAGE {
            return Err(Error::InvalidRequest);
        }
        signers.push(
            decode_binary(part)?
                .try_into()
                .map_err(|_| Error::InvalidRequest)?,
        );
    }
    Ok(signers)
}

/// Decode framing and resolve every proof's chain from its carrier, normally the
/// session that admitted the message. Callers separately verify proofs.
pub fn decode_envelope(
    head: MessageHead,
    response: bool,
    chains: &dyn Chains,
) -> Result<(ForwardedHead, usize)> {
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
            "racer-original" => {
                let mut proof = decode_signed(&header.value)?;
                resolve(&mut proof, chains)?;
                original = Some(Arc::new(proof));
            }
            // Present only when no session consumed it, as in sessionless fixtures.
            "connection" | "host" | PROOF_SIGNERS => {}
            _ => {
                let suffix = name
                    .strip_prefix("racer-hop-")
                    .ok_or(Error::InvalidRequest)?;
                let index = suffix.parse::<usize>().map_err(|_| Error::InvalidRequest)?;
                if index >= MAX_HOPS || suffix != index.to_string() {
                    return Err(Error::InvalidRequest);
                }
                let mut proof = decode_signed(&header.value)?;
                resolve(&mut proof, chains)?;
                hops.insert(index, proof);
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

/// Decode exactly one bounded signed head, rejecting trailing bytes. The chain
/// stays unresolved until [`resolve`] consults the carrier.
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
        chain: None,
    })
}
