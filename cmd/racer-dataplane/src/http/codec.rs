//! Strict, bounded HTTP/1.1 heads. Endpoint adapters own field semantics.
//!
//! Header order, spelling, duplicates, and opaque value bytes are preserved. The
//! one optional SP immediately following a colon is syntax, not value data; any
//! additional whitespace is retained except on Authorization/Racer-Metadata,
//! which require exactly one separator and reject edge whitespace. Encoding
//! always emits that separator SP.
//! Raw heads deliberately do not implement Debug (they can contain credentials).
use crate::error::{Error, Result};
use zeroize::Zeroize;
pub const MAX_HEAD_BYTES: usize = 32 * 1024;

pub enum StartLine {
    Request { method: String, target: String },
    Response { status: u16 },
}
pub struct Header {
    pub name: String,
    pub value: Vec<u8>,
}
impl Drop for Header {
    fn drop(&mut self) {
        self.value.zeroize();
    }
}
pub struct MessageHead {
    pub start: StartLine,
    pub headers: Vec<Header>,
}

impl MessageHead {
    /// All occurrences, in wire order. Semantic consumers must reject ambiguous
    /// singleton fields rather than silently taking the first value.
    pub fn values<'a>(&'a self, name: &'a str) -> impl Iterator<Item = &'a [u8]> + 'a {
        self.headers.iter().filter_map(move |header| {
            header
                .name
                .eq_ignore_ascii_case(name)
                .then_some(header.value.as_slice())
        })
    }
    pub fn unique(&self, name: &str) -> Result<Option<&[u8]>> {
        let mut found = None;
        for header in &self.headers {
            if header.name.eq_ignore_ascii_case(name) {
                if found.is_some() {
                    return Err(Error::InvalidRequest);
                }
                found = Some(header.value.as_slice());
            }
        }
        Ok(found)
    }
    /// This fixed-length protocol rejects transfer coding, including identity.
    /// Content-Length duplicates are rejected even if their values are identical.
    pub fn content_length(&self) -> Result<Option<u64>> {
        if self
            .headers
            .iter()
            .any(|h| h.name.eq_ignore_ascii_case("transfer-encoding"))
        {
            return Err(Error::InvalidRequest);
        }
        self.unique("content-length")?.map(decimal).transpose()
    }
    pub fn closes_connection(&self) -> Result<bool> {
        let mut close = false;
        for value in self.values("connection") {
            for token in value.split(|b| *b == b',') {
                let token = trim_ows(token);
                if token.is_empty() || !token.iter().copied().all(is_token) {
                    return Err(Error::InvalidRequest);
                }
                // Headers defining framing/authentication must not be nominated
                // as hop-by-hop fields and then stripped by an intermediary.
                if !token.eq_ignore_ascii_case(b"close")
                    && !token.eq_ignore_ascii_case(b"keep-alive")
                {
                    return Err(Error::InvalidRequest);
                }
                close |= token.eq_ignore_ascii_case(b"close");
            }
        }
        Ok(close)
    }
}

pub struct Codec {
    header_limit: usize,
    body_limit: u64,
}
impl Codec {
    pub fn new(header_limit: usize, body_limit: u64) -> Self {
        Self {
            header_limit: header_limit.min(MAX_HEAD_BYTES),
            body_limit,
        }
    }
    pub fn header_limit(&self) -> usize {
        self.header_limit
    }
    pub fn body_limit(&self) -> u64 {
        self.body_limit
    }
    pub fn limited(&self, header_limit: usize) -> Self {
        Self::new(self.header_limit.min(header_limit), self.body_limit)
    }

    /// Returns bytes consumed from a possibly larger read, never body bytes.
    /// Content-Length is representation metadata on HEAD responses, so the body
    /// cap is enforced by HttpIo after method/status semantics are known.
    pub fn decode_head(&self, bytes: &[u8]) -> Result<Option<(MessageHead, usize)>> {
        let bounded = &bytes[..bytes.len().min(self.header_limit)];
        let Some(end) = bounded
            .windows(4)
            .position(|w| w == b"\r\n\r\n")
            .map(|n| n + 4)
        else {
            if bytes.len() >= self.header_limit {
                return Err(Error::HeaderTooLarge);
            }
            if invalid_line_endings(bytes) {
                return Err(Error::InvalidRequest);
            }
            return Ok(None);
        };
        if end > self.header_limit {
            return Err(Error::HeaderTooLarge);
        }
        if invalid_line_endings(&bytes[..end]) {
            return Err(Error::InvalidRequest);
        }
        // Allocate parser slots from the bounded head size, not an untrusted
        // field count or body length. httparse supplies vetted syntax validation.
        let count = bytes[..end].windows(2).filter(|w| *w == b"\r\n").count();
        let mut slots = vec![httparse::EMPTY_HEADER; count.saturating_sub(2)];
        let start = if bytes.starts_with(b"HTTP/") {
            let mut response = httparse::Response::new(&mut slots);
            if response
                .parse(&bytes[..end])
                .map_err(|_| Error::InvalidRequest)?
                != httparse::Status::Complete(end)
                || response.version != Some(1)
            {
                return Err(Error::InvalidRequest);
            }
            StartLine::Response {
                status: response.code.ok_or(Error::InvalidRequest)?,
            }
        } else {
            let mut request = httparse::Request::new(&mut slots);
            if request
                .parse(&bytes[..end])
                .map_err(|_| Error::InvalidRequest)?
                != httparse::Status::Complete(end)
                || request.version != Some(1)
            {
                return Err(Error::InvalidRequest);
            }
            StartLine::Request {
                method: request.method.ok_or(Error::InvalidRequest)?.into(),
                target: request.path.ok_or(Error::InvalidRequest)?.into(),
            }
        };
        // Do not take httparse's trimmed values: signed opaque fields must retain
        // the exact application bytes, including trailing SP/HTAB and obs-text.
        let first = bytes[..end]
            .windows(2)
            .position(|w| w == b"\r\n")
            .ok_or(Error::InvalidRequest)?;
        let mut headers = Vec::with_capacity(slots.len());
        for line in bytes[first + 2..end - 2].split(|b| *b == b'\n') {
            if line.is_empty() {
                continue;
            }
            let line = line.strip_suffix(b"\r").ok_or(Error::InvalidRequest)?;
            if line.is_empty() {
                continue;
            }
            let colon = line
                .iter()
                .position(|b| *b == b':')
                .ok_or(Error::InvalidRequest)?;
            let name = std::str::from_utf8(&line[..colon])
                .map_err(|_| Error::InvalidRequest)?
                .to_owned();
            let value = &line[colon + 1..];
            if opaque_field(&name) {
                let Some(value) = value.strip_prefix(b" ") else {
                    return Err(Error::InvalidRequest);
                };
                validate_opaque_edges(value)?;
            }
            headers.push(Header {
                name,
                value: value.strip_prefix(b" ").unwrap_or(value).to_vec(),
            });
        }
        let head = MessageHead { start, headers };
        self.validate(&head)?;
        Ok(Some((head, end)))
    }
    pub fn encode_head(&self, head: &MessageHead) -> Result<Vec<u8>> {
        let start_length = match &head.start {
            StartLine::Request { method, target } => method
                .len()
                .checked_add(target.len())
                .and_then(|n| n.checked_add(12))
                .ok_or(Error::HeaderTooLarge)?,
            StartLine::Response { .. } => 15,
        };
        let length = head.headers.iter().try_fold(
            start_length.checked_add(2).ok_or(Error::HeaderTooLarge)?,
            |total, h| {
                total
                    .checked_add(h.name.len())
                    .and_then(|n| n.checked_add(h.value.len()))
                    .and_then(|n| n.checked_add(4))
                    .ok_or(Error::HeaderTooLarge)
            },
        )?;
        if length > self.header_limit {
            return Err(Error::HeaderTooLarge);
        }
        self.validate(head)?;
        let start = match &head.start {
            StartLine::Request { method, target } => format!("{method} {target} HTTP/1.1\r\n"),
            StartLine::Response { status } => format!("HTTP/1.1 {status:03} \r\n"),
        };
        let mut bytes = Vec::with_capacity(length);
        bytes.extend_from_slice(start.as_bytes());
        for header in &head.headers {
            bytes.extend_from_slice(header.name.as_bytes());
            bytes.extend_from_slice(b": ");
            bytes.extend_from_slice(&header.value);
            bytes.extend_from_slice(b"\r\n");
        }
        bytes.extend_from_slice(b"\r\n");
        Ok(bytes)
    }
    fn validate(&self, head: &MessageHead) -> Result<()> {
        match &head.start {
            StartLine::Request { method, target } => {
                if method.is_empty()
                    || !method.bytes().all(is_token)
                    || target.is_empty()
                    || !target.bytes().all(|b| b > 32 && b < 127)
                {
                    return Err(Error::InvalidRequest);
                }
            }
            StartLine::Response { status } if !(100..=599).contains(status) => {
                return Err(Error::InvalidRequest);
            }
            _ => {}
        }
        for header in &head.headers {
            if header.name.is_empty()
                || !header.name.bytes().all(is_token)
                || !header
                    .value
                    .iter()
                    .all(|b| *b == b'\t' || (*b >= 32 && *b != 127))
            {
                return Err(Error::InvalidRequest);
            }
            if opaque_field(&header.name) {
                validate_opaque_edges(&header.value)?;
            }
        }
        head.content_length()?;
        head.closes_connection()?;
        // Host remains optional for the local UDS profile, but never ambiguous.
        head.unique("host")?;
        Ok(())
    }
}

fn opaque_field(name: &str) -> bool {
    name.eq_ignore_ascii_case("authorization") || name.eq_ignore_ascii_case("racer-metadata")
}
fn validate_opaque_edges(value: &[u8]) -> Result<()> {
    if value.first().is_some_and(|b| *b == b' ' || *b == b'\t')
        || value.last().is_some_and(|b| *b == b' ' || *b == b'\t')
    {
        return Err(Error::InvalidRequest);
    }
    Ok(())
}

fn is_token(b: u8) -> bool {
    b.is_ascii_alphanumeric() || b"!#$%&'*+-.^_`|~".contains(&b)
}
fn invalid_line_endings(bytes: &[u8]) -> bool {
    bytes.iter().enumerate().any(|(i, b)| {
        (*b == b'\n' && (i == 0 || bytes[i - 1] != b'\r'))
            || (*b == b'\r' && i + 1 < bytes.len() && bytes[i + 1] != b'\n')
    })
}
fn trim_ows(mut value: &[u8]) -> &[u8] {
    while value.first().is_some_and(|b| *b == b' ' || *b == b'\t') {
        value = &value[1..];
    }
    while value.last().is_some_and(|b| *b == b' ' || *b == b'\t') {
        value = &value[..value.len() - 1];
    }
    value
}
fn decimal(value: &[u8]) -> Result<u64> {
    if value.is_empty() {
        return Err(Error::InvalidRequest);
    }
    value.iter().try_fold(0u64, |n, b| {
        if !b.is_ascii_digit() {
            return Err(Error::InvalidRequest);
        }
        n.checked_mul(10)
            .and_then(|n| n.checked_add(u64::from(*b - b'0')))
            .ok_or(Error::InvalidRequest)
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn fragmented_head_preserves_opaque_values_and_duplicates() {
        let codec = Codec::new(1024, 16);
        let bytes = b"GET /a HTTP/1.1\r\nX-Opaque:  \xff\t \r\nx-opaque: second\r\nContent-Length: 3\r\n\r\nabc";
        let end = bytes.len() - 3;
        for length in 0..end {
            assert!(codec.decode_head(&bytes[..length]).unwrap().is_none());
        }
        let (head, used) = codec.decode_head(bytes).unwrap().unwrap();
        assert_eq!(used, end);
        assert_eq!(head.headers[0].value, b" \xff\t ");
        assert_eq!(head.values("x-OPAQUE").count(), 2);
        assert_eq!(head.unique("x-opaque"), Err(Error::InvalidRequest));
        assert_eq!(codec.encode_head(&head).unwrap(), bytes[..end]);
    }
    #[test]
    fn rejects_smuggling_and_malformed_syntax() {
        let codec = Codec::new(1024, 16);
        for bytes in [
            &b"GET / HTTP/1.1\n\n"[..],
            &b"GET / HTTP/1.0\r\n\r\n"[..],
            &b"GET / HTTP/1.1\r\nContent-Length: 1\r\ncontent-length: 1\r\n\r\n"[..],
            &b"GET / HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n"[..],
            &b"GET / HTTP/1.1\r\nContent-Length: +1\r\n\r\n"[..],
            &b"GET / HTTP/1.1\r\nContent-Length: 18446744073709551616\r\n\r\n"[..],
            &b"GET / HTTP/1.1\r\nX: first\r\n second\r\n\r\n"[..],
            &b"GET / HTTP/1.1\r\nConnection: content-length\r\n\r\n"[..],
            &b"GET / HTTP/1.1\r\nBad : x\r\n\r\n"[..],
        ] {
            assert!(codec.decode_head(bytes).is_err(), "accepted malformed head");
        }
    }
    #[test]
    fn bounds_heads_without_counting_read_ahead_or_head_representation_length() {
        let bytes = b"HTTP/1.1 200 OK\r\nContent-Length: 99999999\r\n\r\n";
        let codec = Codec::new(bytes.len(), 16);
        assert_eq!(codec.decode_head(bytes).unwrap().unwrap().1, bytes.len());
        assert!(Codec::new(bytes.len() - 1, 16).decode_head(bytes).is_err());
        assert!(Codec::new(8, 16).decode_head(b"GET /thi").is_err());
        let head = MessageHead {
            start: StartLine::Response { status: 200 },
            headers: vec![Header {
                name: "x".into(),
                value: b"a\r\ninjected: true".to_vec(),
            }],
        };
        assert!(codec.encode_head(&head).is_err());
    }
    #[test]
    fn opaque_field_raw_separators_are_validated_before_decoding() {
        let codec = Codec::new(MAX_HEAD_BYTES, 16);
        for field in ["Authorization", "Racer-Metadata"] {
            for suffix in [
                &b"x"[..],
                &b"  x"[..],
                &b"\tx"[..],
                &b" \tx"[..],
                &b" x "[..],
                &b" x\t"[..],
            ] {
                let mut bytes = format!("GET / HTTP/1.1\r\n{field}:").into_bytes();
                bytes.extend_from_slice(suffix);
                bytes.extend_from_slice(b"\r\n\r\n");
                assert!(codec.decode_head(&bytes).is_err());
            }
            let mut bytes = format!("GET / HTTP/1.1\r\n{field}: ").into_bytes();
            bytes.extend_from_slice(b"\xffopaque\x80\r\n\r\n");
            let (head, _) = codec.decode_head(&bytes).unwrap().unwrap();
            assert_eq!(head.unique(field).unwrap().unwrap(), b"\xffopaque\x80");
            assert_eq!(codec.encode_head(&head).unwrap(), bytes);
        }
        assert!(matches!(
            codec.decode_head(&vec![b'x'; MAX_HEAD_BYTES]),
            Err(Error::HeaderTooLarge)
        ));
    }
}
