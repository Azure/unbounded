//! Incremental framing and ambiguity rejection. Endpoint modules own semantics.
//!
//! Raw HTTP data is deliberately non-Debug: it may contain Authorization. Do not
//! serialize raw requests into retries, traces, caches, or checkpoints.
use crate::error::{Result, pending};
pub enum StartLine {
    Request { method: String, target: String },
    Response { status: u16 },
}
pub struct Header {
    pub name: String,
    pub value: Vec<u8>,
}
pub struct MessageHead {
    pub start: StartLine,
    pub headers: Vec<Header>,
}
pub struct Codec {
    header_limit: usize,
    body_limit: u64,
}
impl Codec {
    pub fn new(header_limit: usize, body_limit: u64) -> Self {
        Self {
            header_limit,
            body_limit,
        }
    }
    pub fn decode_head(&self, _bytes: &[u8]) -> Result<Option<(MessageHead, usize)>> {
        pending("http.decode_head")
    }
    pub fn encode_head(&self, _head: &MessageHead) -> Result<Vec<u8>> {
        pending("http.encode_head")
    }
}
#[cfg(test)]
mod tests { /* Partial framing, conflicting lengths, duplicate fields, header limits. */
}
