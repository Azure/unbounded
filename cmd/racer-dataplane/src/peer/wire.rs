//! Logical peer operations, distinct from HTTP encoding and routing policy.
//!
//! Wire requests carry encrypted origin context; responses and cacheable records
//! never contain credentials. CopyOnly must never trigger origin access.
use crate::security::forwarding::ForwardedHead;
use crate::{
    error::{Error, Result},
    http::codec::{Codec, Header, MessageHead, StartLine},
    security::signing::SignedHead,
};
use crate::{
    memory::pool::CiphertextPage,
    model::{
        context::PeerOriginContext,
        identity::{ObjectId, PageId},
        metadata::{MetadataSelector, ObjectMetadata},
    },
    topology::paths::RouteBudget,
};
use base64::{Engine, engine::general_purpose::STANDARD};
use std::sync::Arc;

pub use crate::security::forwarding::{RequestBinding, VerifiedRequest, VerifiedResponse};

pub enum FetchMode {
    CopyOnly,
    Acquire,
}
pub enum Operation {
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
    Page {
        metadata: ObjectMetadata,
        ciphertext: CiphertextPage,
    },
    Metadata(ObjectMetadata),
    Miss,
    VersionUnavailable,
    Unavailable,
    Overloaded,
    OriginRejected,
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

pub const VERSION: &str = "1";
pub const REQUEST_TARGET: &str = "/racer/peer/v1/exchange";
pub const MAX_HOPS: usize = 8;
pub const MAX_SIGNED_HEAD: usize = crate::security::protocol::MAX_HEAD;
pub const MAX_ENVELOPE_HEAD: usize = (MAX_HOPS + 1) * (MAX_SIGNED_HEAD * 2);

/// Logical fields are decoded by the canonical security profile, not inferred
/// from unauthenticated transport headers. This bridge also owns charged decoding.
pub trait LogicalCodec {
    fn request(
        &self,
        authentication: ForwardedHead,
        scope: &crate::runtime::deadline::RequestScope,
    ) -> Result<SignedRequest>;
    fn response(
        &self,
        authentication: ForwardedHead,
        body: Vec<u8>,
        scope: &crate::runtime::deadline::RequestScope,
    ) -> Result<SignedResponse>;
}
pub use super::decode::SecurityCodec;

/// Versioned HTTP envelope. Original signed heads are embedded byte-for-byte using
/// the HTTP codec; this wrapper is framing only and is never a signing authority.
pub struct WireCodec;
impl WireCodec {
    pub fn encode(
        authentication: &ForwardedHead,
        response: bool,
        body_length: usize,
    ) -> Result<MessageHead> {
        if authentication.hops.len() > MAX_HOPS
            || body_length > crate::model::range::PAGE_BYTES as usize + 16
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
        Codec::new(MAX_ENVELOPE_HEAD, crate::model::range::PAGE_BYTES + 16).encode_head(&head)?;
        Ok(head)
    }

    pub fn decode(head: MessageHead, response: bool) -> Result<(ForwardedHead, usize)> {
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
        let mut seen = std::collections::HashSet::new();
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
                    let text =
                        std::str::from_utf8(&header.value).map_err(|_| Error::InvalidRequest)?;
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
        if !version
            || length > crate::model::range::PAGE_BYTES as usize + 16
            || (!response && length != 0)
        {
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
}

fn encode_signed(head: &SignedHead) -> Result<Vec<u8>> {
    if head.signature.len() != 64 {
        return Err(Error::InvalidRequest);
    }
    let bytes = Codec::new(MAX_SIGNED_HEAD, crate::model::range::PAGE_BYTES + 16)
        .encode_head(&head.head)?;
    let mut framed = Vec::with_capacity(bytes.len() + 64);
    framed.extend_from_slice(&head.signature);
    framed.extend_from_slice(&bytes);
    Ok(STANDARD.encode(framed).into_bytes())
}

fn decode_signed(bytes: &[u8]) -> Result<SignedHead> {
    if bytes.len() > (MAX_SIGNED_HEAD + 64).div_ceil(3) * 4 {
        return Err(Error::InvalidRequest);
    }
    let decoded = STANDARD.decode(bytes).map_err(|_| Error::InvalidRequest)?;
    if STANDARD.encode(&decoded).as_bytes() != bytes || decoded.len() <= 64 {
        return Err(Error::InvalidRequest);
    }
    let (head, consumed) = Codec::new(MAX_SIGNED_HEAD, crate::model::range::PAGE_BYTES + 16)
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
mod tests {
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
            WireCodec::decode(WireCodec::encode(&original, true, 27).unwrap(), true).unwrap();
        assert_eq!(length, 27);
        assert_eq!(decoded.original.signature, original.original.signature);
        assert_eq!(decoded.original.head.headers[0].value, b"AAEC/w==");
    }
    #[test]
    fn rejects_versions_duplicates_holes_and_request_bodies() {
        let mut head = WireCodec::encode(&envelope(), false, 0).unwrap();
        head.headers[0].value = b"2".to_vec();
        assert!(WireCodec::decode(head, false).is_err());
        let mut head = WireCodec::encode(&envelope(), false, 0).unwrap();
        head.headers.push(Header {
            name: "Content-Length".into(),
            value: b"0".to_vec(),
        });
        assert!(WireCodec::decode(head, false).is_err());
        let mut head = WireCodec::encode(&envelope(), false, 0).unwrap();
        head.headers.push(Header {
            name: "racer-hop-1".into(),
            value: encode_signed(&envelope().original).unwrap(),
        });
        assert!(WireCodec::decode(head, false).is_err());
        assert!(WireCodec::encode(&envelope(), false, 1).is_err());
        assert!(WireCodec::encode(&envelope(), true, usize::MAX).is_err());
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
}
