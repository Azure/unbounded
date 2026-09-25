//! Sole wire encoding authority for Racer peer v1. Binary fields are padded
//! standard base64; integers are minimal decimal; keys are lowercase hex.
//! Encoders never include page bytes. Transports must preserve every signed head.
use crate::{
    error::{Error, Result},
    http::codec::{Header, MessageHead, StartLine},
    model::{
        identity::{NodeId, ObjectId, ObjectVersion},
        metadata::ObjectMetadata,
        range::PAGE_BYTES,
    },
    peer::wire::{FetchMode, Operation, PeerRequest, PeerResponse},
    runtime::deadline::Deadline,
    topology::paths::RouteBudget,
};
use base64::{Engine, engine::general_purpose::STANDARD};
use std::{
    sync::OnceLock,
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

pub const PROFILE: &str = "racer-peer-v1";
pub const MAX_HOPS: usize = 8;
pub const MAX_HEAD: usize = 64 * 1024;

/// Canonical Kubernetes UUID spelling. Reject normalization at the trust boundary.
pub fn uuid(value: &str) -> Result<()> {
    if value.len() != 36
        || !value.bytes().enumerate().all(|(i, b)| {
            if matches!(i, 8 | 13 | 18 | 23) {
                b == b'-'
            } else {
                b.is_ascii_digit() || (b'a'..=b'f').contains(&b)
            }
        })
    {
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
fn clock() -> &'static (Instant, SystemTime) {
    static CLOCK: OnceLock<(Instant, SystemTime)> = OnceLock::new();
    CLOCK.get_or_init(|| (Instant::now(), SystemTime::now()))
}
/// Stable process clock mapping. Decode wire deadlines with `decode_deadline`,
/// never reconstruct them from a new relative timeout at each hop.
pub fn encode_deadline(deadline: Deadline) -> Result<u64> {
    let (mono, wall) = clock();
    let time = if deadline.0 >= *mono {
        wall.checked_add(deadline.0.duration_since(*mono))
    } else {
        wall.checked_sub(mono.duration_since(deadline.0))
    }
    .ok_or(Error::InvalidRequest)?;
    millis(time)
}
pub fn decode_deadline(value: u64) -> Result<Deadline> {
    let (mono, wall) = clock();
    let base = millis(*wall)?;
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
fn object(head: &mut MessageHead, object: &ObjectId) -> Result<()> {
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
fn version(head: &mut MessageHead, version: &ObjectVersion) -> Result<()> {
    object(head, &version.object)?;
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
        .transpose()?
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
        Operation::Page { page, mode } => {
            push(&mut head, "racer-operation", "page");
            version(&mut head, &page.version)?;
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
            object(&mut head, id)?;
            push(&mut head, "racer-operation", "metadata");
            match selector {
                crate::model::metadata::MetadataSelector::Fresh => {
                    push(&mut head, "racer-selector", "fresh")
                }
                crate::model::metadata::MetadataSelector::Pinned(etag) => {
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
        push_binary(&mut head, "racer-metadata", metadata.as_header()?);
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
fn metadata(head: &mut MessageHead, metadata: &ObjectMetadata) -> Result<()> {
    version(head, &metadata.version)?;
    push(head, "racer-length", metadata.length);
    push(head, "racer-expires", millis(metadata.expires_at.0)?);
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
        PeerResponse::Page {
            metadata: m,
            ciphertext,
        } => {
            let e = ciphertext.envelope();
            m.immutable().validate_page(e)?;
            if e.plaintext_length.checked_add(16) != Some(e.ciphertext_length)
                || ciphertext.bytes().len() != e.ciphertext_length as usize
            {
                return Err(Error::InvalidRequest);
            }
            metadata(&mut head, m)?;
            push(&mut head, "racer-page", e.page.number.0);
            push_binary(&mut head, "racer-page-key", &e.key_id.0);
            push_binary(&mut head, "racer-page-nonce", &e.nonce.0);
            push(&mut head, "racer-plaintext-length", e.plaintext_length);
            push(&mut head, "racer-ciphertext-length", e.ciphertext_length);
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
                &mut head,
                "content-range",
                format!("bytes {start}-{end}/{}", m.length),
            );
            ("page", u64::from(e.ciphertext_length))
        }
        PeerResponse::Metadata(m) => {
            metadata(&mut head, m)?;
            ("metadata", 0)
        }
        PeerResponse::Miss => ("miss", 0),
        PeerResponse::VersionUnavailable => ("version-unavailable", 0),
        PeerResponse::Unavailable => ("unavailable", 0),
        PeerResponse::Overloaded => ("overloaded", 0),
        PeerResponse::OriginRejected => ("origin-rejected", 0),
    };
    push(&mut head, "racer-outcome", outcome);
    push(&mut head, "content-length", length);
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
            if super::signing::is_auth_field(&name)
                || (ignore_route
                    && matches!(
                        name.as_str(),
                        "racer-route-membership"
                            | "racer-route-request"
                            | "racer-route-attempt"
                            | "racer-route-destination"
                            | "racer-route-visited"
                            | "racer-route-links"
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
mod tests {
    use super::*;
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
            super::super::signing::tests::node(1),
            super::super::signing::tests::node(2),
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
        assert_eq!(MAX_HEAD, crate::peer::wire::MAX_SIGNED_HEAD);
    }
}
