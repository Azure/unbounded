//! Decode the security owner's canonical fields, then compare its encoding exactly.
use super::wire::*;
use crate::{
    error::{Error, Result},
    http::codec::MessageHead,
    memory::pool::BufferPool,
    model::{
        context::{EncryptedAuthorization, OpaqueMetadata, PeerOriginContext},
        envelope::{KeyId, Nonce, PageEnvelope},
        identity::*,
        limits::ResourceClass,
        metadata::{ExpiresAt, MetadataSelector, ObjectMetadata},
    },
    runtime::{admission::Admission, deadline::RequestScope},
    security::{forwarding::ForwardedHead, protocol as p},
    topology::paths::RouteBudget,
};
use std::{
    rc::Rc,
    time::{Duration, UNIX_EPOCH},
};

pub struct SecurityCodec {
    admission: Rc<Admission>,
    buffers: Rc<BufferPool>,
}
impl SecurityCodec {
    pub fn new(admission: Rc<Admission>, buffers: Rc<BufferPool>) -> Self {
        Self { admission, buffers }
    }
}
fn bytes(head: &MessageHead, name: &str) -> Result<Vec<u8>> {
    p::decode_binary(p::field(head, name)?.as_bytes())
}
fn array<const N: usize>(head: &MessageHead, name: &str) -> Result<[u8; N]> {
    bytes(head, name)?
        .try_into()
        .map_err(|_| Error::InvalidRequest)
}
fn node(head: &MessageHead, name: &str) -> Result<NodeId> {
    crate::security::signing::node_field(head, name)
}
fn object(head: &MessageHead) -> Result<ObjectId> {
    let cache = p::field(head, "racer-cache")?;
    p::uuid(&cache)?;
    if cache.is_empty() || cache.len() > 256 {
        return Err(Error::InvalidRequest);
    }
    let key = p::field(head, "racer-key")?;
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
    StrongEtag::parse(p::field(head, "racer-etag")?.as_bytes())
}
fn version(head: &MessageHead) -> Result<ObjectVersion> {
    Ok(ObjectVersion {
        object: object(head)?,
        etag: etag(head)?,
    })
}
fn metadata(head: &MessageHead) -> Result<ObjectMetadata> {
    Ok(ObjectMetadata {
        version: version(head)?,
        length: p::number(head, "racer-length")?,
        expires_at: ExpiresAt(
            UNIX_EPOCH
                .checked_add(Duration::from_millis(p::number(head, "racer-expires")?))
                .ok_or(Error::InvalidRequest)?,
        ),
    })
}
fn present(head: &MessageHead, name: &str) -> Result<bool> {
    match p::number(head, name)? {
        0 => Ok(false),
        1 => Ok(true),
        _ => Err(Error::InvalidRequest),
    }
}
fn route(head: &MessageHead) -> Result<RouteBudget> {
    let remaining_links = p::number(head, "racer-route-links")?
        .try_into()
        .map_err(|_| Error::InvalidRequest)?;
    let visited = p::decode_nodes(p::field(head, "racer-route-visited")?.as_bytes())?;
    if remaining_links as usize + visited.len() > 9 {
        return Err(Error::HopBudgetExhausted);
    }
    Ok(RouteBudget {
        membership: MembershipVersion(p::number(head, "racer-route-membership")?),
        request: RequestId(array(head, "racer-route-request")?),
        attempt: AttemptId(array(head, "racer-route-attempt")?),
        destination: node(head, "racer-route-destination")?,
        visited,
        remaining_links,
        deadline: p::decode_deadline(p::number(head, "racer-route-deadline")?)?,
    })
}
impl LogicalCodec for SecurityCodec {
    fn request(
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
        if length > p::MAX_HEAD {
            return Err(Error::InvalidRequest);
        }
        let reservation =
            self.admission
                .reserve(None, ResourceClass::RequestContext, length.max(1))?;
        let mode = match p::field(head, "racer-mode")?.as_str() {
            "copy" => FetchMode::CopyOnly,
            "acquire" => FetchMode::Acquire,
            _ => return Err(Error::InvalidRequest),
        };
        let object = object(head)?;
        let operation = match p::field(head, "racer-operation")?.as_str() {
            "page" => Operation::Page {
                page: PageId {
                    version: version(head)?,
                    number: PageNumber(p::number(head, "racer-page")?),
                },
                mode,
            },
            "metadata" => Operation::Metadata {
                object: object.clone(),
                selector: match p::field(head, "racer-selector")?.as_str() {
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
        p::agrees(head, &p::request_head(&request)?, false)?;
        request.route = effective_route;
        Ok(SignedRequest {
            authentication,
            request,
        })
    }
    fn response(
        &self,
        authentication: ForwardedHead,
        body: Vec<u8>,
        scope: &RequestScope,
    ) -> Result<SignedResponse> {
        scope.check()?;
        let head = &authentication.original.head;
        let response = match p::field(head, "racer-outcome")?.as_str() {
            "page" => {
                let metadata = metadata(head)?;
                let envelope = PageEnvelope {
                    page: PageId {
                        version: metadata.version.clone(),
                        number: PageNumber(p::number(head, "racer-page")?),
                    },
                    key_id: KeyId(array(head, "racer-page-key")?),
                    nonce: Nonce(array(head, "racer-page-nonce")?),
                    plaintext_length: p::number(head, "racer-plaintext-length")?
                        .try_into()
                        .map_err(|_| Error::InvalidRequest)?,
                    ciphertext_length: p::number(head, "racer-ciphertext-length")?
                        .try_into()
                        .map_err(|_| Error::InvalidRequest)?,
                };
                metadata.immutable().validate_page(&envelope)?;
                if body.len() != envelope.ciphertext_length as usize
                    || envelope.plaintext_length.checked_add(16) != Some(envelope.ciphertext_length)
                {
                    return Err(Error::InvalidRequest);
                }
                let reservation = self.admission.reserve(
                    Some(&metadata.version.object.cache),
                    ResourceClass::Ciphertext,
                    body.len(),
                )?;
                PeerResponse::Page {
                    metadata,
                    ciphertext: self.buffers.ciphertext(reservation, envelope, body)?,
                }
            }
            outcome => {
                if !body.is_empty() {
                    return Err(Error::InvalidRequest);
                }
                match outcome {
                    "metadata" => PeerResponse::Metadata(metadata(head)?),
                    "miss" => PeerResponse::Miss,
                    "version-unavailable" => PeerResponse::VersionUnavailable,
                    "unavailable" => PeerResponse::Unavailable,
                    "overloaded" => PeerResponse::Overloaded,
                    "origin-rejected" => PeerResponse::OriginRejected,
                    _ => return Err(Error::InvalidRequest),
                }
            }
        };
        let binding = array(head, "racer-request-binding")?;
        let path = p::decode_nodes(p::field(head, "racer-response-path")?.as_bytes())?;
        p::agrees(head, &p::response_head(&response, &binding, &path)?, false)?;
        Ok(SignedResponse {
            authentication,
            response,
        })
    }
}
