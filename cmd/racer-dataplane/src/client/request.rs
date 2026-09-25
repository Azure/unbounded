//! Validate HEAD/bootstrap/pinned GET and preserve opaque adapter fields.
//!
//! Reject multipart ranges, weak/wildcard pins, unsupported methods, and ambiguous
//! duplicate metadata/Authorization fields before dispatch. Bootstrap is page zero;
//! arbitrary single ranges require If-Match. Canonical key wire encoding and target
//! syntax must be specified before implementing this adapter.
use crate::{
    error::{Result, pending},
    http::codec::MessageHead,
    model::{
        context::OriginContext,
        identity::{CacheId, StrongEtag},
        range::ByteRange,
    },
};
pub enum ReadKind {
    Head,
    Bootstrap,
    Pinned { etag: StrongEtag, range: ByteRange },
}
pub struct ClientRequest {
    pub kind: ReadKind,
    pub origin: OriginContext,
}
pub struct RequestParser {
    header_limit: usize,
}
impl RequestParser {
    pub fn new(header_limit: usize) -> Self {
        Self { header_limit }
    }
    pub fn parse(&self, _cache: &CacheId, _head: MessageHead) -> Result<ClientRequest> {
        pending("client.parse")
    }
}
#[cfg(test)]
mod tests { /* Pins, malformed ranges, exact key/header preservation, duplicate fields. */
}
