//! Signed peer operations and canonical encoding/decoding with charged ownership.
//! Binary fields use padded standard base64, integers minimal decimal, and keys
//! lowercase hex. Encoders never include page bytes; transports preserve signed heads.

use crate::admission::AdmissionPolicy;
use crate::admission::ResourceClass;
use crate::error::Error;
use crate::error::Result;
#[cfg(test)]
use crate::http::Codec;
use crate::http::ConnectionLease;
use crate::http::HttpIo;
use crate::memory::BufferPool;
use crate::memory::CiphertextPage;
use crate::model::ExpiresAt;
use crate::model::MetadataSelector;
use crate::model::Nonce;
use crate::model::ObjectMetadata;
use crate::model::PageEnvelope;
use crate::model::*;
use crate::peer::forwarding::ForwardedHead;
use crate::runtime::RequestScope;
use crate::security::EncryptedAuthorization;
use crate::security::OpaqueMetadata;
use crate::security::PeerOriginContext;
use crate::topology::RouteBudget;
#[cfg(test)]
use base64::Engine;
#[cfg(test)]
use base64::engine::general_purpose::STANDARD;
#[cfg(test)]
use http1::Header;
use http1::MessageHead;
use http1::StartLine;
use racer_control_wire::CacheId;
use racer_control_wire::KeyId;
use racer_control_wire::MembershipVersion;
use racer_control_wire::NodeId;
use racer_crypto::identity::Certificates;
use racer_crypto::identity::Keyring;
use racer_crypto::identity::VerifiedPeer;
use std::rc::Rc;
#[cfg(test)]
use std::sync::Arc;
#[cfg(test)]
use std::time::Duration;
use std::time::SystemTime;
#[cfg(test)]
use std::time::UNIX_EPOCH;
use uring_runtime::environment::Deadline;

pub enum FetchMode {
    CopyOnly,
    Acquire,
}

/// Retained HTTP provenance and exclusively socket-owned ordered sessions.
pub struct Signatures {
    inner: Rc<peer_wire::Signatures>,

    #[cfg(test)]
    keys: Rc<Keyring>,
}
pub use peer_wire::SignedHead;
pub struct VerifiedHead {
    pub(crate) signed: SignedHead,

    pub(crate) peer: VerifiedPeer,
}
impl Signatures {
    pub(super) fn wire(&self) -> &peer_wire::Signatures {
        &self.inner
    }

    #[cfg(test)]
    pub(crate) fn sign_fields(&self, head: MessageHead) -> Result<SignedHead> {
        peer_wire::test_util::sign_fields(&self.inner, head).map_err(Into::into)
    }

    pub fn new(keys: Rc<Keyring>, certificates: Rc<Certificates>) -> Self {
        Self {
            #[cfg(test)]
            keys: keys.clone(),
            inner: Rc::new(peer_wire::Signatures::new(
                keys,
                certificates,
                Rc::new(RequestMac),
            )),
        }
    }
    pub fn node(&self) -> &NodeId {
        self.inner.node()
    }
    /// Sign retained provenance. Replay admission belongs exclusively to the
    /// immediate-hop head signed by the owning connection session.
    pub fn sign(&self, head: MessageHead) -> Result<SignedHead> {
        self.inner.sign(head).map_err(Into::into)
    }
    /// Verify identity and retained provenance, without replay admission. Network
    /// callers must first admit the carrying immediate-hop head on its socket.
    pub fn verify_proof(&self, head: SignedHead) -> Result<VerifiedHead> {
        let (signed, peer) = self.inner.verify_proof(head)?.into_parts();
        Ok(VerifiedHead { signed, peer })
    }
    /// Validate a historical hop. The owning connection admits the fresh outer head.
    pub(crate) fn verify_historical(&self, signed: &SignedHead) -> Result<VerifiedPeer> {
        self.inner.verify_historical(signed).map_err(Into::into)
    }
    /// Only an opaque, already bound request may use its original deadline
    /// instead of the fresh-message replay window. Revalidate current keys/trust.
    pub(crate) fn verify_retained_request(
        &self,
        binding: &super::forwarding::RequestBinding,
    ) -> Result<VerifiedPeer> {
        let signed = binding.retained_proof()?;
        self.inner
            .verify_retained_request(signed, number(&signed.head, "racer-route-deadline")?)
            .map_err(Into::into)
    }
}
/// Racer credential key selection remains outside the wire mechanism.
struct RequestMac;

impl peer_wire::RequestMac for RequestMac {
    fn sign(&self, keys: &Keyring, head: &mut MessageHead) -> peer_wire::Result<()> {
        use peer_wire::{field, mac_base, push_binary};
        if head.unique("racer-kind")? == Some(b"request".as_slice()) {
            let cache = CacheId(field(head, "racer-cache")?);
            let key = keys.active(
                &cache,
                racer_crypto::identity::KeyPurpose::OriginCredentials,
            )?;
            push_binary(head, "racer-mac-key", &key.id().0);
            let mut tag = [0; 32];
            key.request_mac(&cache, &mac_base(head)?, &mut tag)?;
            push_binary(head, "racer-request-mac", &tag);
        }
        Ok(())
    }

    fn verify(&self, keys: &Keyring, head: &MessageHead) -> peer_wire::Result<()> {
        use peer_wire::{Error, decode_binary, field, mac_base};
        if head.unique("racer-kind")? == Some(b"request".as_slice()) {
            let cache = CacheId(field(head, "racer-cache")?);
            let id = decode_binary(field(head, "racer-mac-key")?.as_bytes())?
                .try_into()
                .map_err(|_| Error::Unauthorized)?;
            let key = keys.lease(
                Some(&cache),
                KeyId(id),
                racer_crypto::identity::KeyPurpose::OriginCredentials,
            )?;
            key.verify_request_mac(
                &cache,
                key.id(),
                &mac_base(head)?,
                &decode_binary(field(head, "racer-request-mac")?.as_bytes())?,
            )?;
        }
        Ok(())
    }
}

impl From<peer_wire::Error> for Error {
    fn from(error: peer_wire::Error) -> Self {
        match error {
            peer_wire::Error::InvalidRequest => Self::InvalidRequest,
            peer_wire::Error::HeaderTooLarge => Self::HeaderTooLarge,
            peer_wire::Error::Unauthorized => Self::Unauthorized,
            peer_wire::Error::Replay => Self::Replay,
            peer_wire::Error::DeadlineExceeded => Self::DeadlineExceeded,
            peer_wire::Error::HopBudgetExhausted => Self::HopBudgetExhausted,
            peer_wire::Error::Unavailable => Self::Unavailable,
            peer_wire::Error::Identity(error) => error.into(),
            peer_wire::Error::Runtime(error) => error.into(),
        }
    }
}
#[cfg(test)]
pub(crate) fn is_auth_field(name: &str) -> bool {
    peer_wire::is_auth_field(name)
}
pub fn node_field(head: &MessageHead, name: &str) -> Result<NodeId> {
    peer_wire::node_field(head, name).map_err(Into::into)
}
pub fn receiver(head: &MessageHead) -> Result<NodeId> {
    peer_wire::receiver(head).map_err(Into::into)
}
/// Domain-separated SHA-256 binding of the exact signature base and Ed25519
/// signature. Includes the signature itself, so a request ID is never sufficient.
pub fn signed_digest(head: &SignedHead) -> Result<[u8; 32]> {
    peer_wire::signed_digest(head).map_err(Into::into)
}
/// Never cloned, reset, or detached from its socket. Idle pooling moves this value.
pub use peer_wire::Session;

#[cfg(test)]
use peer_wire::test_util::{random, signature_base, signature_input, transcript};

#[cfg(test)]
fn session_head(
    signatures: &Signatures,
    peer: &NodeId,
    phase: &str,
    a: &[u8; 32],
    b: &[u8; 32],
    response: bool,
) -> Result<MessageHead> {
    peer_wire::test_util::session_head(&signatures.inner, peer, phase, a, b, response)
        .map_err(Into::into)
}

#[cfg(test)]
fn verify_session(
    signatures: &Signatures,
    head: MessageHead,
    peer: Option<&NodeId>,
    phase: &str,
    response: bool,
) -> Result<(NodeId, [u8; 32], [u8; 32])> {
    peer_wire::test_util::verify_session(&signatures.inner, head, peer, phase, response)
        .map_err(Into::into)
}
pub async fn connect(
    io: &HttpIo,
    connection: ConnectionLease,
    signatures: Rc<Signatures>,
    peer: &NodeId,
    parent: &RequestScope,
) -> Result<ConnectionLease> {
    peer_wire::connect(io, connection, signatures.inner.clone(), peer, parent).await
}
pub async fn accept(
    io: &HttpIo,
    connection: ConnectionLease,
    signatures: Rc<Signatures>,
    parent: &RequestScope,
) -> Result<ConnectionLease> {
    peer_wire::accept(io, connection, signatures.inner.clone(), parent).await
}
pub enum Operation {
    Subscribe {
        subscription: super::subscriptions::Subscription,
        mode: FetchMode,
    },
    Bootstrap {
        object: ObjectId,
        mode: FetchMode,
    },
    Page {
        page: PageId,
        mode: FetchMode,
    },
    Metadata {
        object: ObjectId,
        selector: MetadataSelector,
        mode: FetchMode,
    },
}
/// Locally constructed operation, not evidence of authenticated ingress.
pub struct PeerRequest {
    pub operation: Operation,
    pub origin: PeerOriginContext,
    pub route: RouteBudget,
}
/// Unsigned local result. Transport must sign it against the admitted request.
pub enum PeerResponse {
    Selected {
        metadata: ObjectMetadata,
        ciphertext: CiphertextPage,
        grant: super::subscriptions::TransferGrant,
    },
    Bootstrap {
        metadata: ObjectMetadata,
        page_zero: Option<CiphertextPage>,
    },
    Page {
        metadata: ObjectMetadata,
        ciphertext: CiphertextPage,
    },
    Metadata(ObjectMetadata),
    Miss,
    /// Authoritative origin absence, only for fresh metadata Acquire.
    NotFound,
    VersionUnavailable,
    Unavailable,
    Overloaded,
    OriginRejected,
    OriginForbidden,
    /// Authenticated peer no longer retains the requested routing epoch.
    StaleMembership,
}
/// Owned, unverified wire input/output. The original and all forwarding signatures
/// travel with the operation, including opaque encrypted origin credentials.
/// Verification must check that the operation and effective route agree with the
/// original signed fields and the complete forwarding chain before admitting work.
pub struct SignedRequest {
    pub authentication: ForwardedHead,
    pub request: PeerRequest,
}
/// Owned, unverified wire response, including signed misses and errors. A relay
/// preserves both the original head and ciphertext; it never decrypts the body.
/// Verification checks logical response fields against the signed head and binds
/// them to the outstanding request. Page bodies are neither signed nor hashed.
pub struct SignedResponse {
    pub authentication: ForwardedHead,
    pub response: PeerResponse,
}
pub use peer_wire::{
    MAX_ENVELOPE_HEAD, MAX_HEAD, MAX_HOPS, MAX_SIGNED_HEAD, PROFILE, REQUEST_TARGET, VERSION,
};
/// Per-worker progress floor: retained inbound/outbound envelopes, decoded context,
/// signing/encoding scratch and simultaneous receive/send staging during a relay.
/// Runtime admission still rejects concurrent work when this shared budget is full.
pub const MIN_REQUEST_CONTEXT_BYTES: usize = 8 * MAX_ENVELOPE_HEAD;

// The profile's opaque payload cap must track the application's page contract.
const _: () = assert!(peer_wire::MAX_BODY == PAGE_BYTES as usize + 16);

/// Canonical Kubernetes UUID spelling. Reject normalization at the trust boundary.
pub fn uuid(value: &str) -> Result<()> {
    peer_wire::uuid(value).map_err(Into::into)
}

pub fn binary(bytes: &[u8]) -> String {
    peer_wire::binary(bytes)
}
pub fn decode_binary(value: &[u8]) -> Result<Vec<u8>> {
    peer_wire::decode_binary(value).map_err(Into::into)
}
pub fn field(head: &MessageHead, name: &str) -> Result<String> {
    peer_wire::field(head, name).map_err(Into::into)
}
pub fn number(head: &MessageHead, name: &str) -> Result<u64> {
    peer_wire::number(head, name).map_err(Into::into)
}
pub fn push(head: &mut MessageHead, name: &str, value: impl ToString) {
    peer_wire::push(head, name, value);
}
pub fn push_binary(head: &mut MessageHead, name: &str, bytes: &[u8]) {
    peer_wire::push_binary(head, name, bytes);
}
pub fn millis(time: SystemTime) -> Result<u64> {
    peer_wire::millis(time).map_err(Into::into)
}
/// Stable environment clock mapping. Decode wire deadlines with `decode_deadline`,
/// never reconstruct them from a new relative timeout at each hop.
pub fn encode_deadline(deadline: Deadline) -> Result<u64> {
    peer_wire::encode_deadline(deadline).map_err(Into::into)
}
pub fn decode_deadline(value: u64) -> Result<Deadline> {
    peer_wire::decode_deadline(value).map_err(Into::into)
}
/// Canonical node-list encoding: concatenated u32 big-endian length + UTF-8 ID,
/// then padded base64. Empty lists encode as an empty header value.
pub fn nodes(nodes: &[NodeId]) -> Result<String> {
    peer_wire::nodes(nodes).map_err(Into::into)
}
pub fn decode_nodes(value: &[u8]) -> Result<Vec<NodeId>> {
    peer_wire::decode_nodes(value).map_err(Into::into)
}
fn object_fields(head: &mut MessageHead, object: &ObjectId) -> Result<()> {
    uuid(&object.cache.0)?;
    push(head, "racer-cache", &object.cache.0);
    push(head, "racer-key", wire_codec::encode_hex(&object.key.0));
    Ok(())
}
fn version_fields(head: &mut MessageHead, version: &ObjectVersion) -> Result<()> {
    object_fields(head, &version.object)?;
    push(head, "racer-etag", version.etag.as_str());
    Ok(())
}
pub fn route_headers(head: &mut MessageHead, route: &RouteBudget) -> Result<()> {
    if route.membership.0 == 0 {
        return Err(Error::InvalidRequest);
    }
    push(head, "racer-route-membership", route.membership.0);
    push_binary(head, "racer-route-request", &route.request.0);
    push_binary(head, "racer-route-attempt", &route.attempt.0);
    uuid(&route.destination.0)?;
    push(head, "racer-route-destination", &route.destination.0);
    push(head, "racer-route-visited", nodes(&route.visited)?);
    push(head, "racer-route-links", route.remaining_links);
    push(head, "racer-route-attempts", route.remaining_attempts);
    push(
        head,
        "racer-route-deadline",
        encode_deadline(route.deadline)?,
    );
    Ok(())
}
/// Encode every logical request field, including absent/present opaque context.
/// Authentication headers are added only by `Signatures`.
pub fn request_head(request: &PeerRequest) -> Result<MessageHead> {
    if request
        .origin
        .metadata
        .as_ref()
        .map(|m| m.as_header())
        .is_some_and(|b| b.len() > 8192)
        || request
            .origin
            .authorization
            .as_ref()
            .is_some_and(|a| a.ciphertext.len() > 8192 + 16)
    {
        return Err(Error::InvalidRequest);
    }
    if request.origin.request != request.route.request
        || request.origin.attempt != request.route.attempt
    {
        return Err(Error::InvalidRequest);
    }
    let mut head = MessageHead {
        start: StartLine::Request {
            method: "POST".into(),
            target: "/racer/peer/v1".into(),
        },
        headers: Vec::new(),
    };
    push(&mut head, "racer-kind", "request");
    push(&mut head, "content-length", 0);
    let (op_object, mode) = match &request.operation {
        Operation::Subscribe { subscription, mode } => {
            version_fields(&mut head, &subscription.version)?;
            push(&mut head, "racer-operation", "subscribe");
            push_binary(&mut head, "racer-subscription", &subscription.id);
            push(
                &mut head,
                "racer-subscription-sequence",
                subscription.sequence,
            );
            push(&mut head, "racer-page-budget", subscription.page_budget);
            push(&mut head, "racer-byte-budget", subscription.byte_budget);
            let mut intervals = Vec::new();
            for interval in subscription.demand.intervals() {
                intervals.extend_from_slice(&interval.start.to_be_bytes());
                intervals.extend_from_slice(&interval.end.to_be_bytes());
            }
            push_binary(&mut head, "racer-demand", &intervals);
            if matches!(mode, FetchMode::CopyOnly)
                && (request.origin.authorization.is_some() || request.origin.metadata.is_some())
            {
                return Err(Error::Unauthorized);
            }
            (&subscription.version.object, mode)
        }
        Operation::Bootstrap { object: id, mode } => {
            object_fields(&mut head, id)?;
            push(&mut head, "racer-operation", "bootstrap");
            push(&mut head, "racer-selector", "fresh");
            (id, mode)
        }
        Operation::Page { page, mode } => {
            push(&mut head, "racer-operation", "page");
            version_fields(&mut head, &page.version)?;
            push(&mut head, "racer-page", page.number.0);
            let start = page
                .number
                .0
                .checked_mul(PAGE_BYTES)
                .ok_or(Error::InvalidRange)?;
            let end = start
                .checked_add(PAGE_BYTES - 1)
                .ok_or(Error::InvalidRange)?;
            push(&mut head, "range", format!("bytes={start}-{end}"));
            (&page.version.object, mode)
        }
        Operation::Metadata {
            object: id,
            selector,
            mode,
        } => {
            object_fields(&mut head, id)?;
            push(&mut head, "racer-operation", "metadata");
            match selector {
                crate::model::MetadataSelector::Fresh => push(&mut head, "racer-selector", "fresh"),
                crate::model::MetadataSelector::Pinned(etag) => {
                    push(&mut head, "racer-selector", "pinned");
                    push(&mut head, "racer-etag", etag.as_str());
                }
            }
            (id, mode)
        }
    };
    if op_object != &request.origin.object {
        return Err(Error::InvalidRequest);
    }
    if matches!(mode, FetchMode::CopyOnly) && request.route.remaining_attempts != 0 {
        return Err(Error::InvalidRequest);
    }
    push(
        &mut head,
        "racer-mode",
        match mode {
            FetchMode::CopyOnly => "copy",
            FetchMode::Acquire => "acquire",
        },
    );
    push_binary(&mut head, "racer-request", &request.origin.request.0);
    push_binary(&mut head, "racer-attempt", &request.origin.attempt.0);
    push(
        &mut head,
        "racer-metadata-present",
        u8::from(request.origin.metadata.is_some()),
    );
    if let Some(metadata) = &request.origin.metadata {
        push_binary(&mut head, "racer-metadata", metadata.as_header());
    }
    push(
        &mut head,
        "racer-authorization-present",
        u8::from(request.origin.authorization.is_some()),
    );
    if let Some(auth) = &request.origin.authorization {
        if auth.ciphertext.len() < 16 {
            return Err(Error::InvalidRequest);
        }
        push_binary(&mut head, "racer-authorization-key", &auth.key_id.0);
        push_binary(&mut head, "racer-authorization-nonce", &auth.nonce.0);
        push_binary(&mut head, "racer-authorization", &auth.ciphertext);
    }
    route_headers(&mut head, &request.route)?;
    Ok(head)
}
fn metadata_fields(head: &mut MessageHead, metadata: &ObjectMetadata) -> Result<()> {
    version_fields(head, &metadata.version)?;
    push(head, "racer-length", metadata.length);
    push(head, "racer-expires", metadata.expires_at.to_header()?);
    push(head, "racer-metadata-version", 2);
    if let Some(content_type) = &metadata.content_type {
        push(head, "racer-content-type", content_type.as_str());
    }
    Ok(())
}
/// Encode all response outcomes against SHA-256 of the exact original signed
/// request. `path` is the verified forward path including the responder.
pub fn response_head(
    response: &PeerResponse,
    request_digest: &[u8; 32],
    path: &[NodeId],
) -> Result<MessageHead> {
    let mut head = MessageHead {
        start: StartLine::Response { status: 200 },
        headers: Vec::new(),
    };
    push(&mut head, "racer-kind", "response");
    push_binary(&mut head, "racer-request-binding", request_digest);
    push(&mut head, "racer-response-path", nodes(path)?);
    let (outcome, length) = match response {
        PeerResponse::Selected {
            metadata: m,
            ciphertext,
            grant,
        } => {
            if grant.page != ciphertext.envelope().page
                || ciphertext.bytes().len() != ciphertext.envelope().ciphertext_length as usize
            {
                return Err(Error::InvalidRequest);
            }
            page_fields(&mut head, m, ciphertext.envelope())?;
            grant_fields(&mut head, grant)?;
            (
                "selected",
                u64::from(ciphertext.envelope().ciphertext_length),
            )
        }
        PeerResponse::Bootstrap {
            metadata: m,
            page_zero,
        } => match page_zero {
            Some(page) => {
                if m.length == 0 || page.envelope().page.number.0 != 0 {
                    return Err(Error::InvalidRequest);
                }
                let page_head = response_head(
                    &PeerResponse::Page {
                        metadata: m.clone(),
                        ciphertext: page.clone(),
                    },
                    request_digest,
                    path,
                )?;
                for header in page_head.headers {
                    if !matches!(
                        header.name.as_str(),
                        "racer-kind"
                            | "racer-request-binding"
                            | "racer-response-path"
                            | "racer-outcome"
                            | "content-length"
                    ) {
                        head.headers.push(header);
                    }
                }
                push(&mut head, "racer-page-present", 1);
                ("bootstrap", u64::from(page.envelope().ciphertext_length))
            }
            None => {
                if m.length != 0 {
                    return Err(Error::InvalidRequest);
                }
                metadata_fields(&mut head, m)?;
                push(&mut head, "racer-page-present", 0);
                ("bootstrap", 0)
            }
        },
        PeerResponse::Page {
            metadata: m,
            ciphertext,
        } => {
            let e = ciphertext.envelope();
            if ciphertext.bytes().len() != e.ciphertext_length as usize {
                return Err(Error::InvalidRequest);
            }
            page_fields(&mut head, m, e)?;
            ("page", u64::from(e.ciphertext_length))
        }
        PeerResponse::Metadata(m) => {
            metadata_fields(&mut head, m)?;
            ("metadata", 0)
        }
        PeerResponse::Miss => ("miss", 0),
        PeerResponse::NotFound => ("not-found", 0),
        PeerResponse::VersionUnavailable => ("version-unavailable", 0),
        PeerResponse::Unavailable => ("unavailable", 0),
        PeerResponse::Overloaded => ("overloaded", 0),
        PeerResponse::OriginRejected => ("origin-rejected", 0),
        PeerResponse::OriginForbidden => ("origin-forbidden", 0),
        PeerResponse::StaleMembership => ("stale-membership", 0),
    };
    head.start = StartLine::Response {
        status: match response {
            PeerResponse::NotFound => 404,
            PeerResponse::OriginRejected => 401,
            PeerResponse::OriginForbidden => 403,
            _ => 200,
        },
    };
    push(&mut head, "racer-outcome", outcome);
    push(&mut head, "content-length", length);
    Ok(head)
}
pub(crate) fn grant_fields(
    head: &mut MessageHead,
    grant: &crate::peer::subscriptions::TransferGrant,
) -> Result<()> {
    uuid(&grant.receiver.0)?;
    if grant.membership.0 == 0 {
        return Err(Error::InvalidRequest);
    }
    push_binary(head, "racer-subscription", &grant.subscription_id);
    push(head, "racer-subscription-sequence", grant.sequence);
    push(head, "racer-grant-membership", grant.membership.0);
    push(head, "racer-grant-receiver", &grant.receiver.0);
    push(head, "racer-grant-deadline", grant.deadline);
    push(head, "racer-page-budget", grant.remaining_page_budget);
    push(head, "racer-byte-budget", grant.remaining_byte_budget);
    Ok(())
}
fn page_fields(
    head: &mut MessageHead,
    m: &ObjectMetadata,
    e: &crate::model::PageEnvelope,
) -> Result<()> {
    m.immutable().validate_page(e)?;
    if e.plaintext_length.checked_add(16) != Some(e.ciphertext_length) {
        return Err(Error::InvalidRequest);
    }
    metadata_fields(head, m)?;
    push(head, "racer-page", e.page.number.0);
    push_binary(head, "racer-page-key", &e.key_id.0);
    push_binary(head, "racer-page-nonce", &e.nonce.0);
    push(head, "racer-plaintext-length", e.plaintext_length);
    push(head, "racer-ciphertext-length", e.ciphertext_length);
    let start = e
        .page
        .number
        .0
        .checked_mul(PAGE_BYTES)
        .ok_or(Error::InvalidRange)?;
    let end = start
        .checked_add(u64::from(e.plaintext_length))
        .and_then(|n| n.checked_sub(1))
        .ok_or(Error::InvalidRange)?;
    push(
        head,
        "content-range",
        format!("bytes {start}-{end}/{}", m.length),
    );
    Ok(())
}

/// Canonical page metadata without materializing an opaque transit body.
pub(crate) fn opaque_page_head(
    m: &ObjectMetadata,
    e: &crate::model::PageEnvelope,
    bootstrap: bool,
    binding: &[u8; 32],
    path: &[NodeId],
) -> Result<MessageHead> {
    if bootstrap && (m.length == 0 || e.page.number.0 != 0) {
        return Err(Error::InvalidRequest);
    }
    let mut head = MessageHead {
        start: StartLine::Response { status: 200 },
        headers: Vec::new(),
    };
    push(&mut head, "racer-kind", "response");
    push_binary(&mut head, "racer-request-binding", binding);
    push(&mut head, "racer-response-path", nodes(path)?);
    page_fields(&mut head, m, e)?;
    if bootstrap {
        push(&mut head, "racer-page-present", 1);
    }
    push(
        &mut head,
        "racer-outcome",
        if bootstrap { "bootstrap" } else { "page" },
    );
    push(&mut head, "content-length", e.ciphertext_length);
    Ok(head)
}
/// Exact logical agreement, rejecting unknown application fields as well as
/// missing fields. Only the signing layer's fixed authentication fields are elided.
pub fn agrees(actual: &MessageHead, expected: &MessageHead, ignore_route: bool) -> Result<()> {
    peer_wire::agrees(actual, expected, ignore_route).map_err(Into::into)
}

/// Versioned HTTP envelope. Signed header values and signatures are preserved using
/// the HTTP codec; this wrapper is framing only and is never a signing authority.
pub fn encode_envelope(
    authentication: &ForwardedHead,
    response: bool,
    body_length: usize,
) -> Result<MessageHead> {
    peer_wire::encode_envelope(authentication, response, body_length).map_err(Into::into)
}
/// Decode framing only; proof verification and socket replay admission are separate.
pub fn decode_envelope(head: MessageHead, response: bool) -> Result<(ForwardedHead, usize)> {
    peer_wire::decode_envelope(head, response).map_err(Into::into)
}
pub(crate) fn encode_signed(head: &SignedHead) -> Result<Vec<u8>> {
    peer_wire::encode_signed(head).map_err(Into::into)
}
pub(crate) fn decode_signed(bytes: &[u8]) -> Result<SignedHead> {
    peer_wire::decode_signed(bytes).map_err(Into::into)
}

/// Decode the canonical security profile while retaining charged body ownership.
pub struct SecurityCodec {
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    buffers: BufferPool,
}
impl SecurityCodec {
    pub fn new(admission: Rc<flow_control::Quotas<AdmissionPolicy>>, buffers: BufferPool) -> Self {
        Self { admission, buffers }
    }
}
fn bytes(head: &MessageHead, name: &str) -> Result<Vec<u8>> {
    decode_binary(field(head, name)?.as_bytes())
}
fn array<const N: usize>(head: &MessageHead, name: &str) -> Result<[u8; N]> {
    bytes(head, name)?
        .try_into()
        .map_err(|_| Error::InvalidRequest)
}
fn object(head: &MessageHead) -> Result<ObjectId> {
    let cache = field(head, "racer-cache")?;
    uuid(&cache)?;
    if cache.is_empty() || cache.len() > 256 {
        return Err(Error::InvalidRequest);
    }
    let key = field(head, "racer-key")?;
    let decoded = wire_codec::decode_hex(key.as_bytes()).map_err(|_| Error::InvalidRequest)?;
    Ok(ObjectId {
        cache: CacheId(cache),
        key: CacheKey(decoded),
    })
}
fn etag(head: &MessageHead) -> Result<StrongEtag> {
    StrongEtag::parse(field(head, "racer-etag")?.as_bytes())
}
fn version(head: &MessageHead) -> Result<ObjectVersion> {
    Ok(ObjectVersion {
        object: object(head)?,
        etag: etag(head)?,
    })
}
pub(crate) fn demand(head: &MessageHead) -> Result<super::subscriptions::Demand> {
    use super::subscriptions::Demand;
    use super::subscriptions::MAX_DEMAND_INTERVALS;
    use super::subscriptions::PageInterval;
    let encoded = bytes(head, "racer-demand")?;
    if encoded.len() % 16 != 0 || encoded.len() / 16 > MAX_DEMAND_INTERVALS {
        return Err(Error::InvalidRequest);
    }
    Demand::new(
        encoded
            .chunks_exact(16)
            .map(|chunk| PageInterval {
                start: u64::from_be_bytes(chunk[..8].try_into().unwrap()),
                end: u64::from_be_bytes(chunk[8..].try_into().unwrap()),
            })
            .collect(),
    )
}
pub(crate) fn grant(head: &MessageHead) -> Result<super::subscriptions::TransferGrant> {
    Ok(super::subscriptions::TransferGrant {
        subscription_id: array(head, "racer-subscription")?,
        sequence: number(head, "racer-subscription-sequence")?,
        page: PageId {
            version: version(head)?,
            number: PageNumber(number(head, "racer-page")?),
        },
        membership: MembershipVersion(number(head, "racer-grant-membership")?),
        receiver: node_field(head, "racer-grant-receiver")?,
        deadline: number(head, "racer-grant-deadline")?,
        remaining_page_budget: number(head, "racer-page-budget")?
            .try_into()
            .map_err(|_| Error::InvalidRequest)?,
        remaining_byte_budget: number(head, "racer-byte-budget")?,
    })
}
fn metadata(head: &MessageHead) -> Result<ObjectMetadata> {
    let content_type = match head.unique("racer-metadata-version")? {
        Some(b"2") => head
            .unique("racer-content-type")?
            .map(crate::model::ContentType::parse)
            .transpose()?,
        _ => return Err(Error::InvalidRequest),
    };
    Ok(ObjectMetadata {
        content_type,
        version: version(head)?,
        length: number(head, "racer-length")?,
        expires_at: ExpiresAt::from_unix_millis(number(head, "racer-expires")?)?,
    })
}

pub(crate) fn page_descriptor(head: &MessageHead) -> Result<(ObjectMetadata, PageEnvelope)> {
    let metadata = metadata(head)?;
    let envelope = PageEnvelope {
        page: PageId {
            version: metadata.version.clone(),
            number: PageNumber(number(head, "racer-page")?),
        },
        key_id: KeyId(array(head, "racer-page-key")?),
        nonce: Nonce(array(head, "racer-page-nonce")?),
        plaintext_length: number(head, "racer-plaintext-length")?
            .try_into()
            .map_err(|_| Error::InvalidRequest)?,
        ciphertext_length: number(head, "racer-ciphertext-length")?
            .try_into()
            .map_err(|_| Error::InvalidRequest)?,
    };
    metadata.immutable().validate_page(&envelope)?;
    if envelope.plaintext_length.checked_add(16) != Some(envelope.ciphertext_length) {
        return Err(Error::InvalidRequest);
    }
    Ok((metadata, envelope))
}
fn present(head: &MessageHead, name: &str) -> Result<bool> {
    match number(head, name)? {
        0 => Ok(false),
        1 => Ok(true),
        _ => Err(Error::InvalidRequest),
    }
}
/// Decode only the canonical signed metadata. The caller must authenticate the
/// original and reverse proofs before exposing this head or any body bytes.
pub(crate) fn opaque_response_head(head: &MessageHead, length: usize) -> Result<MessageHead> {
    let outcome = field(head, "racer-outcome")?;
    let binding = array(head, "racer-request-binding")?;
    let path = decode_nodes(field(head, "racer-response-path")?.as_bytes())?;
    if matches!(outcome.as_str(), "page" | "selected")
        || (outcome == "bootstrap" && present(head, "racer-page-present")?)
    {
        let (metadata, envelope) = page_descriptor(head)?;
        if length != envelope.ciphertext_length as usize {
            return Err(Error::InvalidRequest);
        }
        let mut canonical = opaque_page_head(
            &metadata,
            &envelope,
            outcome == "bootstrap",
            &binding,
            &path,
        )?;
        if outcome == "selected" {
            canonical
                .headers
                .iter_mut()
                .find(|h| h.name == "racer-outcome")
                .unwrap()
                .value = b"selected".to_vec();
            grant_fields(&mut canonical, &grant(head)?)?;
        }
        return Ok(canonical);
    }
    if length != 0 {
        return Err(Error::InvalidRequest);
    }
    let response = bodyless_response(head, &outcome)?;
    response_head(&response, &binding, &path)
}

fn bodyless_response(head: &MessageHead, outcome: &str) -> Result<PeerResponse> {
    Ok(match outcome {
        "bootstrap" => PeerResponse::Bootstrap {
            metadata: metadata(head)?,
            page_zero: None,
        },
        "metadata" => PeerResponse::Metadata(metadata(head)?),
        "miss" => PeerResponse::Miss,
        "not-found" => PeerResponse::NotFound,
        "version-unavailable" => PeerResponse::VersionUnavailable,
        "unavailable" => PeerResponse::Unavailable,
        "overloaded" => PeerResponse::Overloaded,
        "origin-rejected" => PeerResponse::OriginRejected,
        "origin-forbidden" => PeerResponse::OriginForbidden,
        "stale-membership" => PeerResponse::StaleMembership,
        _ => return Err(Error::InvalidRequest),
    })
}
fn route(head: &MessageHead) -> Result<RouteBudget> {
    let remaining_links = number(head, "racer-route-links")?
        .try_into()
        .map_err(|_| Error::InvalidRequest)?;
    let visited = decode_nodes(field(head, "racer-route-visited")?.as_bytes())?;
    if remaining_links as usize + visited.len() > 9 {
        return Err(Error::HopBudgetExhausted);
    }
    Ok(RouteBudget {
        membership: MembershipVersion(number(head, "racer-route-membership")?),
        request: RequestId(array(head, "racer-route-request")?),
        attempt: AttemptId(array(head, "racer-route-attempt")?),
        destination: node_field(head, "racer-route-destination")?,
        visited,
        remaining_links,
        remaining_attempts: number(head, "racer-route-attempts")?
            .try_into()
            .map_err(|_| Error::InvalidRequest)?,
        deadline: decode_deadline(number(head, "racer-route-deadline")?)?,
    })
}
impl SecurityCodec {
    pub fn request(
        &self,
        authentication: ForwardedHead,
        scope: &RequestScope,
    ) -> Result<SignedRequest> {
        scope.check()?;
        let head = &authentication.original.head;
        // Reserve the complete encoded context before allocating decoded fields.
        let length = head.headers.iter().try_fold(0usize, |n, h| {
            n.checked_add(h.value.len()).ok_or(Error::InvalidRequest)
        })?;
        if length > MAX_HEAD {
            return Err(Error::InvalidRequest);
        }
        let _decode_reservation =
            self.admission
                .reserve(None, ResourceClass::RequestContext, length.max(1))?;
        let mode = match field(head, "racer-mode")?.as_str() {
            "copy" => FetchMode::CopyOnly,
            "acquire" => FetchMode::Acquire,
            _ => return Err(Error::InvalidRequest),
        };
        let object = object(head)?;
        let reservation = self.admission.reserve(
            Some(&object.cache),
            ResourceClass::RequestContext,
            length.checked_add(512).ok_or(Error::InvalidRequest)?,
        )?;
        let operation = match field(head, "racer-operation")?.as_str() {
            "subscribe" => Operation::Subscribe {
                subscription: super::subscriptions::Subscription {
                    id: array(head, "racer-subscription")?,
                    version: version(head)?,
                    demand: demand(head)?,
                    sequence: number(head, "racer-subscription-sequence")?,
                    page_budget: number(head, "racer-page-budget")?
                        .try_into()
                        .map_err(|_| Error::InvalidRequest)?,
                    byte_budget: number(head, "racer-byte-budget")?,
                },
                mode,
            },
            "bootstrap" => Operation::Bootstrap {
                object: object.clone(),
                mode,
            },
            "page" => Operation::Page {
                page: PageId {
                    version: version(head)?,
                    number: PageNumber(number(head, "racer-page")?),
                },
                mode,
            },
            "metadata" => Operation::Metadata {
                object: object.clone(),
                selector: match field(head, "racer-selector")?.as_str() {
                    "fresh" => MetadataSelector::Fresh,
                    "pinned" => MetadataSelector::Pinned(etag(head)?),
                    _ => return Err(Error::InvalidRequest),
                },
                mode,
            },
            _ => return Err(Error::InvalidRequest),
        };
        let original_route = route(head)?;
        let effective_route = route(authentication.hops.last().map(|h| &h.head).unwrap_or(head))?;
        let mut origin_scope = scope.clone();
        origin_scope.request = original_route.request;
        origin_scope.deadline.0 = origin_scope.deadline.0.min(effective_route.deadline.0);
        let origin = PeerOriginContext {
            object,
            request: RequestId(array(head, "racer-request")?),
            attempt: AttemptId(array(head, "racer-attempt")?),
            metadata: if present(head, "racer-metadata-present")? {
                Some(OpaqueMetadata::from_header(&bytes(
                    head,
                    "racer-metadata",
                )?)?)
            } else {
                None
            },
            authorization: if present(head, "racer-authorization-present")? {
                Some(EncryptedAuthorization {
                    key_id: KeyId(array(head, "racer-authorization-key")?),
                    nonce: Nonce(array(head, "racer-authorization-nonce")?),
                    ciphertext: bytes(head, "racer-authorization")?,
                })
            } else {
                None
            },
            reservation,
            scope: origin_scope,
        };
        let mut request = PeerRequest {
            operation,
            origin,
            route: original_route,
        };
        agrees(head, &request_head(&request)?, false)?;
        request.route = effective_route;
        Ok(SignedRequest {
            authentication,
            request,
        })
    }
    pub fn response(
        &self,
        authentication: ForwardedHead,
        body: Vec<u8>,
        scope: &RequestScope,
    ) -> Result<SignedResponse> {
        self.response_reserved(authentication, body, None, scope)
    }
    /// Transfer the completed receive allocation's charge into the decoded page.
    pub fn response_reserved(
        &self,
        authentication: ForwardedHead,
        body: Vec<u8>,
        reservation: Option<flow_control::Charge<AdmissionPolicy>>,
        scope: &RequestScope,
    ) -> Result<SignedResponse> {
        scope.check()?;
        let head = &authentication.original.head;
        let outcome = field(head, "racer-outcome")?;
        let response = match outcome.as_str() {
            "page" | "selected" | "bootstrap"
                if outcome != "bootstrap" || present(head, "racer-page-present")? =>
            {
                let (metadata, envelope) = page_descriptor(head)?;
                if body.len() != envelope.ciphertext_length as usize {
                    return Err(Error::InvalidRequest);
                }
                let reservation = match reservation {
                    Some(reservation) => {
                        reservation.validate(ResourceClass::Ciphertext, body.capacity())?;
                        if !self.admission.owns(&reservation)
                            || reservation.key() != Some(&metadata.version.object.cache)
                        {
                            return Err(Error::InvalidRequest);
                        }
                        reservation
                    }
                    None => self.admission.reserve(
                        Some(&metadata.version.object.cache),
                        ResourceClass::Ciphertext,
                        body.capacity(),
                    )?,
                };
                let ciphertext = self.buffers.ciphertext(reservation, envelope, body)?;
                if outcome == "selected" {
                    PeerResponse::Selected {
                        metadata,
                        ciphertext,
                        grant: grant(head)?,
                    }
                } else if outcome == "bootstrap" {
                    if ciphertext.envelope().page.number.0 != 0 {
                        return Err(Error::InvalidRequest);
                    }
                    PeerResponse::Bootstrap {
                        metadata,
                        page_zero: Some(ciphertext),
                    }
                } else {
                    PeerResponse::Page {
                        metadata,
                        ciphertext,
                    }
                }
            }
            outcome => {
                if !body.is_empty() {
                    return Err(Error::InvalidRequest);
                }
                bodyless_response(head, outcome)?
            }
        };
        let binding = array(head, "racer-request-binding")?;
        let path = decode_nodes(field(head, "racer-response-path")?.as_bytes())?;
        agrees(head, &response_head(&response, &binding, &path)?, false)?;
        Ok(SignedResponse {
            authentication,
            response,
        })
    }
}
#[cfg(test)]
pub(crate) mod tests {
    use super::*;

    mod metadata_tests {
        use super::*;
        #[test]
        fn received_page_keeps_one_charge_and_rejects_foreign_reservations() {
            let signers = crate::peer::tests::signers();
            let cache = CacheId("cccccccc-1111-4111-8111-111111111111".into());
            let mut limits = crate::test_support::cluster::config(false).limits;
            limits.ciphertext_bytes = std::num::NonZeroUsize::new(19).unwrap();
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                limits.clone(),
            )));
            let foreign = flow_control::Quotas::new(AdmissionPolicy::new(limits));
            let buffers = BufferPool::new(admission.clone());
            let codec = SecurityCodec::new(admission.clone(), buffers.clone());
            let metadata = ObjectMetadata {
                content_type: None,
                version: ObjectVersion {
                    object: ObjectId {
                        cache: cache.clone(),
                        key: CacheKey([3; 32]),
                    },
                    etag: StrongEtag::test_value("v1"),
                },
                length: 3,
                expires_at: ExpiresAt::from_system_time(UNIX_EPOCH).unwrap(),
            };
            let page = buffers
                .ciphertext(
                    admission
                        .reserve(Some(&cache), ResourceClass::Ciphertext, 19)
                        .unwrap(),
                    PageEnvelope {
                        page: PageId {
                            version: metadata.version.clone(),
                            number: PageNumber(0),
                        },
                        key_id: KeyId([1; 16]),
                        nonce: Nonce([2; 24]),
                        plaintext_length: 3,
                        ciphertext_length: 19,
                    },
                    vec![9; 19],
                )
                .unwrap();
            let response = PeerResponse::Page {
                metadata,
                ciphertext: page,
            };
            let mut head = response_head(
                &response,
                &[3; 32],
                &[signers[0].node().clone(), signers[2].node().clone()],
            )
            .unwrap();
            push(&mut head, "racer-receiver", &signers[0].node().0);
            let authentication = ForwardedHead {
                original: std::sync::Arc::new(signers[2].sign(head).unwrap()),
                hops: vec![],
            };
            drop(response);
            let scope = RequestScope::new(
                RequestId([1; 16]),
                uring_runtime::environment::now() + Duration::from_secs(30),
            )
            .unwrap();
            for case in 0..5 {
                let reservation = match case {
                    1 => foreign.reserve(Some(&cache), ResourceClass::Ciphertext, 19),
                    2 => admission.reserve(
                        Some(&CacheId("other".into())),
                        ResourceClass::Ciphertext,
                        19,
                    ),
                    3 => admission.reserve(Some(&cache), ResourceClass::Plaintext, 19),
                    4 => admission.reserve(Some(&cache), ResourceClass::Ciphertext, 18),
                    _ => admission.reserve(Some(&cache), ResourceClass::Ciphertext, 19),
                }
                .unwrap();
                let auth = ForwardedHead {
                    original: authentication.original.clone(),
                    hops: vec![],
                };
                let result = codec.response_reserved(auth, vec![9; 19], Some(reservation), &scope);
                if case == 0 {
                    let result = result.unwrap();
                    assert_eq!(admission.used(ResourceClass::Ciphertext), 19);
                    let PeerResponse::Page { ciphertext, .. } = result.response else {
                        panic!("page required")
                    };
                    assert_eq!(ciphertext.bytes(), &[9; 19]);
                    drop(ciphertext);
                } else {
                    assert!(
                        result.is_err(),
                        "foreign or insufficient charge accepted: {case}"
                    );
                }
                assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
                assert_eq!(admission.used(ResourceClass::Plaintext), 0);
                assert_eq!(foreign.used(ResourceClass::Ciphertext), 0);
            }
        }
        #[test]
        fn explicit_metadata_version_round_trips_and_rejects_unknown_or_unsigned_shape() {
            let mut m = ObjectMetadata {
                content_type: None,
                version: ObjectVersion {
                    object: ObjectId {
                        cache: CacheId(crate::test_support::security::CACHE.into()),
                        key: CacheKey([0; 32]),
                    },
                    etag: StrongEtag::test_value("v1"),
                },
                length: 17,
                expires_at: ExpiresAt::from_system_time(UNIX_EPOCH).unwrap(),
            };
            let path = [NodeId(crate::test_support::security::NODE.into())];
            for typed in [false, true] {
                if typed {
                    m.content_type = Some(
                        crate::model::ContentType::parse(b"text/plain; charset=utf-8").unwrap(),
                    );
                }
                let head =
                    response_head(&PeerResponse::Metadata(m.clone()), &[0; 32], &path).unwrap();
                assert_eq!(metadata(&head).unwrap(), m);
                let mut missing_version =
                    response_head(&PeerResponse::Metadata(m.clone()), &[0; 32], &path).unwrap();
                missing_version
                    .headers
                    .retain(|h| h.name != "racer-metadata-version");
                assert!(metadata(&missing_version).is_err());
                assert_eq!(
                    head.unique("racer-metadata-version").unwrap(),
                    Some(b"2".as_slice())
                );
                assert_eq!(
                    head.unique("content-length").unwrap(),
                    Some(b"0".as_slice())
                );
                if typed {
                    for value in [b"3".as_slice(), b"1", b""] {
                        let mut bad =
                            response_head(&PeerResponse::Metadata(m.clone()), &[0; 32], &path)
                                .unwrap();
                        bad.headers
                            .iter_mut()
                            .find(|h| h.name == "racer-metadata-version")
                            .unwrap()
                            .value = value.to_vec();
                        assert!(metadata(&bad).is_err());
                    }
                    let mut bad =
                        response_head(&PeerResponse::Metadata(m.clone()), &[0; 32], &path).unwrap();
                    bad.headers.retain(|h| h.name != "racer-metadata-version");
                    assert!(metadata(&bad).is_err());
                    let mut bad =
                        response_head(&PeerResponse::Metadata(m.clone()), &[0; 32], &path).unwrap();
                    push(&mut bad, "racer-content-type", "text/plain");
                    assert!(metadata(&bad).is_err());
                }
            }
        }
    }

    mod envelope_tests {
        use super::*;
        fn envelope() -> ForwardedHead {
            ForwardedHead {
                original: Arc::new(SignedHead {
                    head: MessageHead {
                        start: StartLine::Response { status: 404 },
                        headers: vec![Header {
                            name: "racer-opaque".into(),
                            value: b"AAEC/w==".to_vec(),
                        }],
                    },
                    signature: vec![7; 64],
                }),
                hops: vec![],
            }
        }
        #[test]
        fn envelope_preserves_signature_and_opaque_headers() {
            let original = envelope();
            let (decoded, length) =
                decode_envelope(encode_envelope(&original, true, 27).unwrap(), true).unwrap();
            assert_eq!(length, 27);
            assert_eq!(decoded.original.signature, original.original.signature);
            assert_eq!(decoded.original.head.headers[0].value, b"AAEC/w==");
        }
        #[test]
        fn rejects_versions_duplicates_holes_and_request_bodies() {
            for version in ["1", "2", "3", "4", "6"] {
                let mut head = encode_envelope(&envelope(), false, 0).unwrap();
                head.headers[0].value = version.as_bytes().to_vec();
                assert!(decode_envelope(head, false).is_err());
            }
            let mut legacy = encode_envelope(&envelope(), false, 0).unwrap();
            legacy.start = StartLine::Request {
                method: "POST".into(),
                target: "/racer/peer/v2/exchange".into(),
            };
            assert!(decode_envelope(legacy, false).is_err());
            let mut head = encode_envelope(&envelope(), false, 0).unwrap();
            head.headers[0].value = b"1".to_vec();
            assert!(decode_envelope(head, false).is_err());
            let mut head = encode_envelope(&envelope(), false, 0).unwrap();
            head.headers.push(Header {
                name: "Content-Length".into(),
                value: b"0".to_vec(),
            });
            assert!(decode_envelope(head, false).is_err());
            let mut head = encode_envelope(&envelope(), false, 0).unwrap();
            head.headers.push(Header {
                name: "racer-hop-1".into(),
                value: encode_signed(&envelope().original).unwrap(),
            });
            assert!(decode_envelope(head, false).is_err());
            assert!(encode_envelope(&envelope(), false, 1).is_err());
            assert!(encode_envelope(&envelope(), true, usize::MAX).is_err());
        }
        #[test]
        fn rejects_trailing_or_oversized_embedded_head() {
            let mut encoded = STANDARD
                .decode(encode_signed(&envelope().original).unwrap())
                .unwrap();
            encoded.extend_from_slice(b"extra");
            assert!(decode_signed(STANDARD.encode(encoded).as_bytes()).is_err());
            assert!(decode_signed(&vec![b'A'; MAX_SIGNED_HEAD * 2]).is_err());
        }
        #[test]
        fn maximum_signed_heads_and_hop_count_fit_outer_signature_profile() {
            let signers = crate::test_support::security::network(2);
            let mut head = MessageHead {
                start: StartLine::Response { status: 200 },
                headers: vec![
                    Header {
                        name: "racer-receiver".into(),
                        value: signers[1].node().0.as_bytes().to_vec(),
                    },
                    Header {
                        name: "padding".into(),
                        value: b"x".to_vec(),
                    },
                ],
            };
            let small = signers[0].sign(head).unwrap();
            let codec = Codec::new(MAX_SIGNED_HEAD);
            let length = codec.encode_head(&small.head).unwrap().len();
            head = small.head;
            head.headers
                .retain(|h| !is_auth_field(&h.name) || h.name == "racer-receiver");
            head.headers
                .iter_mut()
                .find(|h| h.name == "padding")
                .unwrap()
                .value
                .resize(1 + MAX_SIGNED_HEAD - length, b'x');
            let maximum = signers[0].sign(head).unwrap();
            assert_eq!(
                codec.encode_head(&maximum.head).unwrap().len(),
                MAX_SIGNED_HEAD
            );
            let encoded = encode_signed(&maximum).unwrap();
            assert!(encoded.len() > MAX_SIGNED_HEAD);
            let mut envelope = ForwardedHead {
                original: Arc::new(maximum),
                hops: (0..MAX_HOPS)
                    .map(|_| decode_signed(&encoded).unwrap())
                    .collect(),
            };
            let mut outer = encode_envelope(&envelope, true, 0).unwrap();
            push(&mut outer, "racer-receiver", &signers[1].node().0);
            let outer = signers[0].sign_fields(outer).unwrap();
            signers[1].verify_proof(outer).unwrap();
            envelope.hops.push(decode_signed(&encoded).unwrap());
            assert!(encode_envelope(&envelope, true, 0).is_err());
            let mut too_large = decode_signed(&encoded).unwrap();
            too_large
                .head
                .headers
                .iter_mut()
                .find(|h| h.name == "padding")
                .unwrap()
                .value
                .push(b'x');
            assert!(matches!(
                encode_signed(&too_large),
                Err(Error::HeaderTooLarge)
            ));
            let mut decoded = STANDARD.decode(&encoded).unwrap();
            decoded.insert(decoded.len() - 4, b'x');
            assert!(decode_signed(STANDARD.encode(decoded).as_bytes()).is_err());
        }
    }

    pub(crate) mod sessions {
        use super::*;
        use crate::admission::AdmissionPolicy;
        use crate::admission::ResourceClass;
        use crate::http::Codec;
        use crate::http::Endpoint;
        use crate::model::RequestId;
        use crate::runtime::Reactor;
        use std::future::Future;
        use std::task::Context;
        use std::task::Poll;
        use std::time::Instant;
        fn frame() -> MessageHead {
            MessageHead {
                start: StartLine::Request {
                    method: "POST".into(),
                    target: REQUEST_TARGET.into(),
                },
                headers: vec![http1::Header {
                    name: "content-length".into(),
                    value: b"0".to_vec(),
                }],
            }
        }
        fn clone_head(head: &MessageHead) -> MessageHead {
            let codec = Codec::new(MAX_ENVELOPE_HEAD);
            codec
                .decode_head(&codec.encode_head(head).unwrap())
                .unwrap()
                .unwrap()
                .0
        }
        pub(crate) fn pair() -> (Session, Session) {
            let n = crate::test_support::security::network(3);
            let id = random().unwrap();
            (
                peer_wire::test_util::session(n[0].inner.clone(), n[1].node().clone(), id, 0),
                peer_wire::test_util::session(n[1].inner.clone(), n[0].node().clone(), id, 1),
            )
        }
        pub(crate) fn signer(session: &Session) -> Rc<Signatures> {
            Rc::new(Signatures {
                inner: session.signatures.clone(),
                keys: peer_wire::test_util::keys(&session.signatures),
            })
        }
        pub(crate) fn hello(signatures: &Signatures, peer: &NodeId) -> MessageHead {
            session_head(signatures, peer, "hello", &[1; 32], &[0; 32], false).unwrap()
        }
        pub(crate) fn finish(
            signatures: &Signatures,
            peer: &NodeId,
            challenge: MessageHead,
        ) -> MessageHead {
            let (_, a, b) =
                verify_session(signatures, challenge, Some(peer), "challenge", true).unwrap();
            assert_eq!(a, [1; 32]);
            session_head(signatures, peer, "finish", &a, &b, false).unwrap()
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
        fn historical_proof_requires_fresh_head_and_session_cannot_be_reinstalled() {
            let (mut a, mut b) = pair();
            let mut original = frame();
            push(&mut original, "racer-receiver", &a.peer.0);
            let proof = std::sync::Arc::new(a.signatures.sign(original).unwrap());
            let auth = ForwardedHead {
                original: proof.clone(),
                hops: vec![],
            };
            let mut previous = None;
            for sequence in 1..=2 {
                let wire = a.sign(encode_envelope(&auth, false, 0).unwrap()).unwrap();
                let copy = clone_head(&wire);
                let decoded = b.admit(wire).unwrap();
                let (retained, _) = decode_envelope(decoded, false).unwrap();
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
            let mut conn = crate::http::from_accepted(socket.into(), &admission).unwrap();
            crate::http::install_session(&mut conn, a).unwrap();
            assert!(crate::http::install_session(&mut conn, b).is_err());
            conn.set_framing(Some(0), Some(0), false);
            conn.next_round().unwrap();
            assert!(!conn.is_reusable());
            assert_eq!(conn.state().session.as_ref().unwrap().tx, 2);
        }
        #[test]
        fn parallel_connections_isolate_counters_and_wall_rollback_cannot_resurrect_frames() {
            let clock = uring_runtime::environment::SimulationClock::new_at(
                92,
                Instant::now(),
                std::time::SystemTime::now(),
            );
            let environment = clock.environment(1);
            let _guard = environment.enter();
            let (mut a, mut b) = pair();
            let id = random().unwrap();
            let mut other_a =
                peer_wire::test_util::session(a.signatures.clone(), a.peer.clone(), id, 0);
            let mut other_b =
                peer_wire::test_util::session(b.signatures.clone(), b.peer.clone(), id, 1);
            let old = a.sign(frame()).unwrap();
            b.admit(clone_head(&old)).unwrap();
            assert!(other_b.admit(clone_head(&old)).is_err());
            assert_eq!(other_b.rx, 0);
            other_b.admit(other_a.sign(frame()).unwrap()).unwrap();
            clock.advance(Duration::from_secs(61));
            clock.set_wall_time(uring_runtime::environment::wall_now() - Duration::from_secs(61));
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
                let mut bad = peer_wire::test_util::session(
                    a.signatures.clone(),
                    a.peer.clone(),
                    a.id,
                    a.direction,
                );
                match mutation {
                    0 => bad.id[0] ^= 1,
                    1 => bad.direction = 1,
                    2 => bad.signatures = b.signatures.clone(),
                    _ => bad.tx = 9000,
                };
                assert!(b.admit(bad.sign(frame()).unwrap()).is_err());
                assert_eq!(b.rx, 0);
            }
            b.admit(clone_head(&valid)).unwrap();
            assert!(matches!(b.admit(valid), Err(peer_wire::Error::Replay)));
            assert_eq!(b.rx, 1);
            b.admit(a.sign(frame()).unwrap()).unwrap();
        }
        #[test]
        fn duplicate_wrong_session_direction_identity_and_forged_high_sequence() {
            replay_and_binding_checks();
        }
        #[test]
        pub(crate) fn more_than_4096_frames_at_fixed_time_have_constant_session_storage() {
            let clock = uring_runtime::environment::SimulationClock::new_at(
                71,
                Instant::now(),
                std::time::SystemTime::now(),
            );
            let environment = clock.environment(0);
            let _guard = environment.enter();
            let (mut a, mut b) = pair();
            let now = uring_runtime::environment::wall_now();
            let bytes = std::mem::size_of_val(&a) + std::mem::size_of_val(&b);
            for sequence in 1..=5000 {
                let h = a.sign(frame()).unwrap();
                assert_eq!(number(&h, "racer-timestamp").unwrap(), millis(now).unwrap());
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
            let mut reconnected = peer_wire::test_util::session(
                b.signatures.clone(),
                b.peer.clone(),
                random().unwrap(),
                1,
            );
            assert!(reconnected.admit(clone_head(&old)).is_err());
            assert_eq!(reconnected.rx, 0);
            b.expires = uring_runtime::environment::now();
            assert!(matches!(
                b.admit(old),
                Err(peer_wire::Error::DeadlineExceeded)
            ));
            a.tx = u64::MAX;
            assert!(matches!(a.sign(frame()), Err(peer_wire::Error::Replay)));
            reconnected.rx = u64::MAX;
            let mut sender = peer_wire::test_util::session(
                a.signatures.clone(),
                a.peer.clone(),
                reconnected.id,
                0,
            );
            assert!(matches!(
                reconnected.admit(sender.sign(frame()).unwrap()),
                Err(peer_wire::Error::Replay)
            ));
        }
        #[test]
        fn concurrent_workers_complete_independent_connection_handshakes() {
            let workers: Vec<_> = (0..4)
                .map(|_| {
                    std::thread::spawn(
                        loopback_mutual_authentication_pool_reuse_and_fresh_reconnect,
                    )
                })
                .collect();
            for worker in workers {
                worker.join().unwrap();
            }
        }
        #[test]
        fn loopback_mutual_authentication_pool_reuse_and_fresh_reconnect() {
            let n = crate::test_support::security::network(2);
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            )));
            let reactor = Rc::new(Reactor::new(admission.clone()));
            let io = crate::http::new_io(
                reactor.clone(),
                Codec::new(MAX_ENVELOPE_HEAD),
                admission.clone(),
                u64::MAX,
            );
            let pool = crate::http::new_pool(reactor.clone(), admission.clone(), 2);
            let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
            let endpoint = Endpoint::Peer(listener.local_addr().unwrap().to_string());
            let listener = Rc::new(uring_runtime::reactor::descriptor::Descriptor::from(
                listener,
            ));
            let scope =
                RequestScope::new(RequestId([1; 16]), Instant::now() + Duration::from_secs(30))
                    .unwrap();
            let mut previous_id = None;
            for _ in 0..2 {
                let client = async {
                    let conn = pool.checkout(&endpoint, &scope).await?;
                    connect(&io, conn, n[0].clone(), n[1].node(), &scope).await
                };
                let server = async {
                    let fd = reactor.accept(listener.clone(), &scope).await?;
                    let conn = crate::http::from_accepted(fd, &admission)?;
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
            let n = crate::test_support::security::network(3);
            let a = random().unwrap();
            let b = random().unwrap();
            assert_ne!(a, random().unwrap());
            assert_ne!(
                transcript(n[0].node(), n[1].node(), &a, &b),
                transcript(n[1].node(), n[0].node(), &a, &b)
            );
            let reply = session_head(&n[1], n[0].node(), "challenge", &a, &b, true).unwrap();
            let (_, x, y) = verify_session(
                &n[0],
                clone_head(&reply),
                Some(n[1].node()),
                "challenge",
                true,
            )
            .unwrap();
            assert_eq!((x, y), (a, b));
            assert!(
                verify_session(
                    &n[0],
                    clone_head(&reply),
                    Some(n[2].node()),
                    "challenge",
                    true
                )
                .is_err()
            );
            assert!(
                verify_session(&n[0], clone_head(&reply), Some(n[1].node()), "ready", true)
                    .is_err()
            );
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
                assert!(verify_session(&n[0], bad, Some(n[1].node()), "challenge", true).is_err());
            }
        }
        #[test]
        fn handshake_codec_bounds() {
            let n = crate::test_support::security::network(2);
            let h = session_head(
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
                    .value = binary(&value).into_bytes();
                assert!(verify_session(&n[0], bad, Some(n[1].node()), "challenge", true).is_err());
            }
            let mut extra = clone_head(&h);
            push(&mut extra, "racer-extra", 1);
            assert!(verify_session(&n[0], extra, Some(n[1].node()), "challenge", true).is_err());
        }
        #[test]
        fn socket_admission_rejects_replay_before_dispatch_and_closes_pool_slot() {
            use uring_runtime::reactor::IoBuffer;
            let n = crate::test_support::security::network(2);
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            )));
            let reactor = Rc::new(Reactor::new(admission.clone()));
            let io = crate::http::new_io(reactor.clone(), Codec::new(65536), admission.clone(), 0);
            let scope =
                RequestScope::new(RequestId([2; 16]), Instant::now() + Duration::from_secs(10))
                    .unwrap();
            let (a, b) = std::os::unix::net::UnixStream::pair().unwrap();
            let a = crate::http::from_accepted(a.into(), &admission).unwrap();
            let b = crate::http::from_accepted(b.into(), &admission).unwrap();
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
            let n = crate::test_support::security::network(2);
            for end in ["cancel", "expiry", "drop"] {
                let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(false).limits,
                )));
                let reactor = Rc::new(Reactor::new(admission.clone()));
                reactor.init().unwrap();
                let io =
                    crate::http::new_io(reactor.clone(), Codec::new(65536), admission.clone(), 0);
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
                let conn = crate::http::from_accepted(socket.into(), &admission).unwrap();
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

    mod signature_tests {
        use super::*;
        use crate::test_support::security::clone_head;
        use crate::test_support::security::mac_test_keys;
        use crate::test_support::security::network;
        use crate::test_support::security::node;
        use std::time::SystemTime;
        fn head(receiver: usize) -> MessageHead {
            let mut head = MessageHead {
                start: StartLine::Request {
                    method: "POST".into(),
                    target: "/racer/peer/v1".into(),
                },
                headers: Vec::new(),
            };
            push(&mut head, "racer-receiver", node(receiver).0);
            push(&mut head, "content-length", 0);
            push(&mut head, "racer-kind", "test");
            head
        }
        fn canonical_fixture(response: bool, fields: usize, value_bytes: usize) -> MessageHead {
            let mut head = head(1);
            if response {
                head.start = StartLine::Response { status: 200 };
            }
            push(&mut head, "racer-timestamp", "1700000000123");
            push(&mut head, "racer-signer", node(0).0);
            for i in (0..fields).rev() {
                push(
                    &mut head,
                    &format!("x-fixture-{i:02}"),
                    "a".repeat(value_bytes),
                );
            }
            let input = signature_input(&head).unwrap();
            push(&mut head, "signature-input", format!("racer={input}"));
            push(&mut head, "signature", "racer=:fixture:");
            head
        }
        #[test]
        fn signature_base_preserves_order_case_and_rejects_invalid_components() {
            for response in [false, true] {
                let mut head = canonical_fixture(response, 8, 16);
                let expected = signature_base(&head).unwrap();
                assert_ne!(expected.last(), Some(&b'\n'));
                head.headers.reverse();
                for h in &mut head.headers {
                    h.name = h.name.to_ascii_uppercase();
                }
                assert_eq!(signature_base(&head).unwrap(), expected);
                for fault in [
                    "duplicate",
                    "duplicate-input",
                    "duplicate-signature",
                    "leading",
                    "trailing",
                    "non-ascii",
                    "newline",
                    "bad-name",
                    "timestamp",
                    "signer",
                    "missing-input",
                    "coverage",
                    "oversized",
                ] {
                    let mut head = canonical_fixture(response, 8, 16);
                    match fault {
                        "duplicate" => push(&mut head, "X-Fixture-00", "duplicate"),
                        "duplicate-input" => push(&mut head, "Signature-Input", "duplicate"),
                        "duplicate-signature" => push(&mut head, "Signature", "duplicate"),
                        "missing-input" => head.headers.retain(|h| h.name != "signature-input"),
                        _ => {
                            let (name, value) = match fault {
                                "leading" => ("x-fixture-00", b" leading".to_vec()),
                                "trailing" => ("x-fixture-00", b"trailing\t".to_vec()),
                                "non-ascii" => ("x-fixture-00", vec![0xff]),
                                "newline" => ("x-fixture-00", b"x\r\ny".to_vec()),
                                "timestamp" => ("racer-timestamp", b"01700000000123".to_vec()),
                                "signer" => ("racer-signer", b"not-a-uuid".to_vec()),
                                "coverage" => ("signature-input", b"racer=()".to_vec()),
                                "oversized" => ("x-fixture-00", vec![b'a'; MAX_ENVELOPE_HEAD]),
                                "bad-name" => ("x-fixture-00", b"valid".to_vec()),
                                _ => unreachable!(),
                            };
                            let field = head.headers.iter_mut().find(|h| h.name == name).unwrap();
                            field.value = value;
                            if fault == "bad-name" {
                                field.name = "invalid name".into();
                            }
                        }
                    }
                    assert!(
                        signature_base(&head).is_err(),
                        "response={response} fault={fault}"
                    );
                }
            }
        }
        #[test]
        #[ignore = "release-only canonical signature-base construction benchmark, no cryptography"]
        fn signature_base_benchmark() {
            use std::hint::black_box;
            use std::time::Instant;
            // This ignored benchmark must fail at runtime, not prevent debug builds.
            #[allow(clippy::assertions_on_constants)]
            {
                assert!(!cfg!(debug_assertions), "run with --release");
            }
            for (label, fields, value_bytes) in
                [("small", 0, 0), ("fields", 24, 64), ("envelope", 24, 4096)]
            {
                for response in [false, true] {
                    let head = canonical_fixture(response, fields, value_bytes);
                    let expected = signature_base(&head).unwrap();
                    const ITERATIONS: usize = 2000;
                    for sample in 0..6 {
                        let start = Instant::now();
                        for _ in 0..ITERATIONS {
                            black_box(signature_base(black_box(&head)).unwrap());
                        }
                        let elapsed = start.elapsed();
                        assert_eq!(signature_base(&head).unwrap(), expected);
                        if sample != 0 {
                            println!(
                                "signature_base case={label} response={response} fields={} base_bytes={} sample={sample} iterations={ITERATIONS} ns_per_op={:.0}",
                                head.headers.len(),
                                expected.len(),
                                elapsed.as_nanos() as f64 / ITERATIONS as f64
                            );
                        }
                    }
                }
            }
        }
        #[test]
        fn request_mac_rotates_and_rejects_missing_retired_or_mutated_tags() {
            use racer_control_wire::*;
            let network = network(2);
            let make = || {
                let mut request = head(1);
                request
                    .headers
                    .iter_mut()
                    .find(|h| h.name == "racer-kind")
                    .unwrap()
                    .value = b"request".to_vec();
                push(&mut request, "racer-cache", node(88).0);
                request
            };
            let old = network[0].sign(make()).unwrap();
            assert!(network[1].verify_proof(clone_head(&old)).is_ok());
            let mut tampered = clone_head(&old);
            tampered
                .head
                .headers
                .iter_mut()
                .find(|h| h.name == "racer-request-mac")
                .unwrap()
                .value[0] ^= 1;
            assert!(network[1].verify_proof(tampered).is_err());
            let mut missing = clone_head(&old);
            missing
                .head
                .headers
                .retain(|h| h.name != "racer-request-mac");
            assert!(network[1].verify_proof(missing).is_err());
            for signer in &network {
                let mut keys = mac_test_keys();
                for key in &mut keys {
                    key.key.id.0[4..12].copy_from_slice(&2u64.to_be_bytes());
                    let (reference, state, mut material) = key.clone().into_installation();
                    material[0] ^= 1;
                    *key = racer_control_wire::CacheEncryptionKey::new(reference, state, material);
                }
                signer
                    .keys
                    .install(KeyringBundle {
                        schema_version: SCHEMA_VERSION,
                        cluster: signer.keys.cluster().clone(),
                        generation: BundleGeneration(2),
                        peer_trust_roots: (*signer.keys.peer_trust_roots().unwrap()).clone(),
                        cache_keys: keys,
                    })
                    .unwrap();
            }
            assert!(
                network[1].verify_proof(clone_head(&old)).is_err(),
                "removed epoch closes new admission"
            );
            let current = network[0].sign(make()).unwrap();
            assert!(network[1].verify_proof(current).is_ok());
        }
        #[test]
        pub(crate) fn malformed_fields_unknown_algorithm_and_historical_expiry() {
            let n = network(2);
            let original = n[0].sign(head(1)).unwrap();
            let mut duplicate = clone_head(&original);
            push(&mut duplicate.head, "Racer-Kind", "test");
            assert!(n[1].verify_historical(&duplicate).is_err());
            let mut algorithm = clone_head(&original);
            for h in &mut algorithm.head.headers {
                if h.name == "signature-input" {
                    h.value = String::from_utf8(h.value.clone())
                        .unwrap()
                        .replace("ed25519", "rsa-pss-sha512")
                        .into_bytes();
                }
            }
            assert!(n[1].verify_historical(&algorithm).is_err());
            for time in [
                SystemTime::now() - Duration::from_secs(61),
                SystemTime::now() + Duration::from_secs(6),
            ] {
                let mut stale = clone_head(&original);
                stale.head.headers.retain(|h| {
                    h.name != "signature"
                        && h.name != "signature-input"
                        && h.name != "racer-timestamp"
                });
                push(&mut stale.head, "racer-timestamp", millis(time).unwrap());
                let input = signature_input(&stale.head).unwrap();
                push(&mut stale.head, "signature-input", format!("racer={input}"));
                stale.signature = n[0]
                    .keys
                    .signing_identity()
                    .unwrap()
                    .sign(&signature_base(&stale.head).unwrap())
                    .unwrap();
                push(
                    &mut stale.head,
                    "signature",
                    format!("racer=:{}:", binary(&stale.signature)),
                );
                assert!(matches!(n[1].verify_historical(&stale), Err(Error::Replay)));
            }
        }
    }

    mod canonical_tests {
        use super::*;
        use std::time::Instant;

        #[test]
        fn object_hex_round_trip_and_rejections_preserve_boundary_errors() {
            let expected = ObjectId {
                cache: CacheId("11111111-1111-4111-8111-111111111111".into()),
                key: CacheKey(std::array::from_fn(|i| (i as u8) * 8)),
            };
            let mut head = MessageHead {
                start: StartLine::Response { status: 200 },
                headers: Vec::new(),
            };
            object_fields(&mut head, &expected).unwrap();
            assert_eq!(object(&head).unwrap(), expected);
            assert_eq!(
                field(&head, "racer-key").unwrap(),
                "0008101820283038404850586068707880889098a0a8b0b8c0c8d0d8e0e8f0f8"
            );
            for value in [
                "".to_owned(),
                "0".repeat(63),
                "0".repeat(65),
                "AB".repeat(32),
                format!("{}g0", "00".repeat(31)),
                format!("{} 0", "00".repeat(31)),
                format!("{}é", "00".repeat(31)),
            ] {
                head.headers
                    .iter_mut()
                    .find(|h| h.name == "racer-key")
                    .unwrap()
                    .value = value.into_bytes();
                assert_eq!(object(&head), Err(Error::InvalidRequest));
            }
        }

        #[test]
        fn canonical_binary_numbers_node_lists_and_deadline_round_trip() {
            assert_eq!(binary(&[0, 1, 255]), "AAH/");
            assert!(decode_binary(b"YQ").is_err());
            assert!(decode_binary(b"YR==").is_err());
            let mut head = MessageHead {
                start: StartLine::Response { status: 200 },
                headers: Vec::new(),
            };
            push(&mut head, "n", "01");
            assert!(number(&head, "n").is_err());
            let nodes = vec![
                crate::test_support::security::node(1),
                crate::test_support::security::node(2),
            ];
            assert_eq!(
                decode_nodes(crate::peer::protocol::nodes(&nodes).unwrap().as_bytes()).unwrap(),
                nodes
            );
            assert!(
                decode_nodes(
                    crate::peer::protocol::nodes(&[nodes[0].clone(), nodes[0].clone()])
                        .unwrap()
                        .as_bytes()
                )
                .is_err()
            );
            let deadline = Deadline(Instant::now() + Duration::from_secs(30));
            let encoded = encode_deadline(deadline).unwrap();
            let decoded = decode_deadline(encoded).unwrap();
            assert_eq!(encode_deadline(decoded).unwrap(), encoded);
            assert!(decoded.0 <= deadline.0);
            assert_eq!(MAX_HEAD, MAX_SIGNED_HEAD);
        }
    }
}
