//! Signed peer operations and canonical encoding/decoding with charged ownership.
//! Binary fields use padded standard base64, integers minimal decimal, and keys
//! lowercase hex. Encoders never include page bytes; transports preserve signed heads.
use crate::error::Error;
use crate::error::Result;
use crate::http::Codec;
use crate::memory::BufferPool;
use crate::memory::CiphertextPage;
use crate::model::EncryptedAuthorization;
use crate::model::ExpiresAt;
use crate::model::KeyId;
use crate::model::MetadataSelector;
use crate::model::Nonce;
use crate::model::ObjectMetadata;
use crate::model::OpaqueMetadata;
use crate::model::PageEnvelope;
use crate::model::PeerOriginContext;
use crate::model::ResourceClass;
use crate::model::*;
use crate::admission::AdmissionPolicy;
use uring_runtime::deadline::Deadline;
use crate::runtime::RequestScope;
use crate::peer::protocol::SignedHead;
use crate::peer::forwarding::ForwardedHead;
use crate::topology::RouteBudget;
use base64::Engine;
use base64::engine::general_purpose::STANDARD;
use http1::Header;
use http1::MessageHead;
use http1::StartLine;
use std::rc::Rc;
use std::sync::Arc;
use std::time::Duration;
use std::time::SystemTime;
use std::time::UNIX_EPOCH;

pub use crate::peer::forwarding::VerifiedRequest;
pub use crate::peer::forwarding::VerifiedResponse;

pub enum FetchMode {
    CopyOnly,
    Acquire,
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
pub const VERSION: &str = "5";
pub const REQUEST_TARGET: &str = "/racer/peer/v5/exchange";
pub const MAX_HOPS: usize = 8;
pub const MAX_SIGNED_HEAD: usize = MAX_HEAD;
pub const MAX_ENVELOPE_HEAD: usize = (MAX_HOPS + 1) * (MAX_SIGNED_HEAD * 2);
/// Per-worker progress floor: retained inbound/outbound envelopes, decoded context,
/// signing/encoding scratch and simultaneous receive/send staging during a relay.
/// Runtime admission still rejects concurrent work when this shared budget is full.
pub const MIN_REQUEST_CONTEXT_BYTES: usize = 8 * MAX_ENVELOPE_HEAD;

pub const PROFILE: &str = "racer-peer-v5";
pub const MAX_HEAD: usize = 64 * 1024;

/// Canonical Kubernetes UUID spelling. Reject normalization at the trust boundary.
pub fn uuid(value: &str) -> Result<()> {
    if !racer_identity::canonical_uuid(value) {
        return Err(Error::InvalidRequest);
    }
    Ok(())
}

pub fn binary(bytes: &[u8]) -> String {
    STANDARD.encode(bytes)
}
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
pub fn field(head: &MessageHead, name: &str) -> Result<String> {
    let value = head.unique(name)?.ok_or(Error::Unauthorized)?;
    if value.len() > MAX_HEAD {
        return Err(Error::InvalidRequest);
    }
    String::from_utf8(value.to_vec()).map_err(|_| Error::Unauthorized)
}
pub fn number(head: &MessageHead, name: &str) -> Result<u64> {
    let value = field(head, name)?;
    let n: u64 = value.parse().map_err(|_| Error::Unauthorized)?;
    if n.to_string() != value {
        return Err(Error::Unauthorized);
    }
    Ok(n)
}
pub fn push(head: &mut MessageHead, name: &str, value: impl ToString) {
    head.headers.push(Header {
        name: name.into(),
        value: value.to_string().into_bytes(),
    });
}
pub fn push_binary(head: &mut MessageHead, name: &str, bytes: &[u8]) {
    push(head, name, binary(bytes));
}
pub fn millis(time: SystemTime) -> Result<u64> {
    u64::try_from(
        time.duration_since(UNIX_EPOCH)
            .map_err(|_| Error::InvalidRequest)?
            .as_millis(),
    )
    .map_err(|_| Error::InvalidRequest)
}
/// Stable environment clock mapping. Decode wire deadlines with `decode_deadline`,
/// never reconstruct them from a new relative timeout at each hop.
pub fn encode_deadline(deadline: Deadline) -> Result<u64> {
    let (mono, wall) = uring_runtime::environment::clock_anchor();
    let time = if deadline.0 >= mono {
        wall.checked_add(deadline.0.duration_since(mono))
    } else {
        wall.checked_sub(mono.duration_since(deadline.0))
    }
    .ok_or(Error::InvalidRequest)?;
    millis(time)
}
pub fn decode_deadline(value: u64) -> Result<Deadline> {
    let (mono, wall) = uring_runtime::environment::clock_anchor();
    let base = millis(wall)?;
    // Account for submillisecond wall-clock origin, making encode/decode exact.
    let fraction = wall
        .duration_since(UNIX_EPOCH)
        .map_err(|_| Error::InvalidRequest)?
        .subsec_nanos()
        % 1_000_000;
    let instant = if value >= base {
        mono.checked_add(Duration::from_millis(value - base))
    } else {
        mono.checked_sub(Duration::from_millis(base - value))
    }
    .and_then(|i| i.checked_sub(Duration::from_nanos(u64::from(fraction))))
    .ok_or(Error::InvalidRequest)?;
    Ok(Deadline(instant))
}
/// Canonical node-list encoding: concatenated u32 big-endian length + UTF-8 ID,
/// then padded base64. Empty lists encode as an empty header value.
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
fn object_fields(head: &mut MessageHead, object: &ObjectId) -> Result<()> {
    uuid(&object.cache.0)?;
    push(head, "racer-cache", &object.cache.0);
    push(
        head,
        "racer-key",
        object
            .key
            .0
            .iter()
            .map(|b| format!("{b:02x}"))
            .collect::<String>(),
    );
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
            if &grant.page != &ciphertext.envelope().page
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
            if crate::peer::protocol::is_auth_field(&name)
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

#[cfg(test)]
mod canonical_tests {
    use super::*;
    use std::time::Instant;
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
            crate::security::test_support::node(1),
            crate::security::test_support::node(2),
        ];
        assert_eq!(
            decode_nodes(super::nodes(&nodes).unwrap().as_bytes()).unwrap(),
            nodes
        );
        assert!(
            decode_nodes(
                super::nodes(&[nodes[0].clone(), nodes[0].clone()])
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

/// Versioned HTTP envelope. Signed header values and signatures are preserved using
/// the HTTP codec; this wrapper is framing only and is never a signing authority.
pub fn encode_envelope(
    authentication: &ForwardedHead,
    response: bool,
    body_length: usize,
) -> Result<MessageHead> {
    if authentication.hops.len() > MAX_HOPS
        || body_length > crate::model::PAGE_BYTES as usize + 16
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
/// Decode framing only; proof verification and socket replay admission are separate.
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
    let mut seen = crate::runtime::HashSet::default();
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
    if !version || length > crate::model::PAGE_BYTES as usize + 16 || (!response && length != 0) {
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
pub(crate) fn encode_signed(head: &SignedHead) -> Result<Vec<u8>> {
    if head.signature.len() != 64 {
        return Err(Error::InvalidRequest);
    }
    let bytes = Codec::new(MAX_SIGNED_HEAD).encode_head(&head.head)?;
    let mut framed = Vec::with_capacity(bytes.len() + 64);
    framed.extend_from_slice(&head.signature);
    framed.extend_from_slice(&bytes);
    Ok(STANDARD.encode(framed).into_bytes())
}
pub(crate) fn decode_signed(bytes: &[u8]) -> Result<SignedHead> {
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

#[cfg(test)]
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
        let signers = crate::security::test_support::network(2);
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
        head.headers.retain(|h| {
            !crate::peer::protocol::is_auth_field(&h.name) || h.name == "racer-receiver"
        });
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
fn node(head: &MessageHead, name: &str) -> Result<NodeId> {
    crate::peer::protocol::node_field(head, name)
}
fn object(head: &MessageHead) -> Result<ObjectId> {
    let cache = field(head, "racer-cache")?;
    uuid(&cache)?;
    if cache.is_empty() || cache.len() > 256 {
        return Err(Error::InvalidRequest);
    }
    let key = field(head, "racer-key")?;
    if key.len() != 64
        || !key
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
    {
        return Err(Error::InvalidRequest);
    }
    let mut decoded = [0; 32];
    for (index, byte) in decoded.iter_mut().enumerate() {
        *byte = u8::from_str_radix(&key[index * 2..index * 2 + 2], 16)
            .map_err(|_| Error::InvalidRequest)?;
    }
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
        receiver: node(head, "racer-grant-receiver")?,
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

#[cfg(test)]
mod metadata_tests {
    use super::*;
    use std::time::Duration;
    use std::time::UNIX_EPOCH;
    #[test]
    fn received_page_keeps_one_charge_and_rejects_foreign_reservations() {
        use crate::peer::forwarding::ForwardedHead;
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
                    cache: CacheId(crate::security::test_support::CACHE.into()),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value("v1"),
            },
            length: 17,
            expires_at: ExpiresAt::from_system_time(UNIX_EPOCH).unwrap(),
        };
        let path = [NodeId(crate::security::test_support::NODE.into())];
        for typed in [false, true] {
            if typed {
                m.content_type =
                    Some(crate::model::ContentType::parse(b"text/plain; charset=utf-8").unwrap());
            }
            let head = response_head(&PeerResponse::Metadata(m.clone()), &[0; 32], &path).unwrap();
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
                        response_head(&PeerResponse::Metadata(m.clone()), &[0; 32], &path).unwrap();
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
        destination: node(head, "racer-route-destination")?,
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
