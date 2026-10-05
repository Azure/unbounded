//! Strict fixed-length HTTP/1.1 with caller-owned scheduling and resource policy.
//!
//! # Heads and field values
//!
//! [`Codec`] limits the wire size of a message head, not its body. It accepts
//! HTTP/1.1 requests and responses, rejects transfer coding and duplicate
//! Content-Length or Host fields, and leaves body limits to [`connection`].
//! Response codes from 100 to 599 are valid at the codec boundary; the connection
//! exchange layer rejects informational responses and upgrades.
//!
//! Header names retain their spelling and order, and ordinary repeated fields
//! remain separate. Decoding removes at most one separator space after the colon;
//! all remaining value bytes, including tabs and non-ASCII bytes, are preserved.
//! [`Opaque::NAMES`] selects case-insensitive names that instead require exactly
//! one separator space and no leading or trailing space or tab in the value.
//! [`Header`] clears its value bytes on drop. Heads intentionally have no `Debug`
//! implementation, so ordinary diagnostic formatting cannot expose those values.
//!
//! [`ContentType`] preserves accepted MIME bytes without normalizing them.
//! [`range`] parses single byte ranges while leaving numeric policy to the caller.
//!
//! # Connections and policy
//!
//! [`connection::Context`] supplies the reactor, scope, admission charges, outbound
//! slots, endpoint policy, and per-connection state. Charges cover selected owned
//! buffers and decoded-head storage; they are not total allocator accounting and
//! are independent of the reactor's completion reserve. There is no executor, DNS
//! resolver, or background maintenance task in this crate. The caller drives the
//! reactor and polls pool waiters. Idle maintenance budgets count endpoints, not
//! sockets; the same budget separately bounds waiter wakeups.
//!
//! [`connection::State`] hooks customize admission, signing, and session handling.
//! Its default `attach` replaces state, and its default `idle` returns state
//! unchanged. Implementors must provide any transient-attachment cleanup they
//! need. Leases retain buffers and charges through completion and cancellation
//! fences, and unfinished exchanges cannot return reusable connections to a pool.
//! Read-ahead beyond message framing must be retained so exchange completion can
//! reject pipelining. HEAD and 304 representation lengths do not allocate bodies.
//!
//! # Body transfer profiles
//!
//! [`delivery`] sends immutable backing through a staging pipe and owned sends;
//! [`relay`] transfers opaque fixed-length bodies with bounded synchronous steps.
//! Both adapt [`flow_control::pipe::PipeLease`] without selecting application
//! authorization, telemetry, quotas, or scheduling policy. Callers retain exchange
//! finalization and must keep complete transfer owners through completion fences.
//!
//! The optional `test-util` feature exposes connection framing, pool inspection,
//! and deterministic relay fallback helpers without an application dependency.

#![warn(missing_docs)]

pub mod connection;
mod transfer;

pub use transfer::{delivery, relay};

use std::marker::PhantomData;
use zeroize::Zeroize;

/// A bounded HTTP/1.1 head parser and encoder with caller-selected opaque fields.
pub struct Codec<O: Opaque = ()> {
    /// Maximum encoded head length, including the final empty line.
    header_limit: usize,

    /// Selects field policy without storing a policy instance.
    opaque: PhantomData<O>,
}

/// An owned request or response head, excluding body bytes.
pub struct MessageHead {
    /// The request method and target, or response status.
    pub start: StartLine,

    /// Fields in wire order, including permitted duplicates.
    pub headers: Vec<Header>,
}

/// The semantic parts of an HTTP/1.1 start line.
pub enum StartLine {
    /// A request with its original method and target spelling.
    Request {
        /// The nonempty HTTP token naming the method.
        method: String,

        /// The nonempty visible-ASCII request target.
        target: String,
    },
    /// A response without its discarded reason phrase.
    Response {
        /// A three-digit status from 100 through 599.
        status: u16,
    },
}

/// An owned field whose value is cleared when the field is dropped.
pub struct Header {
    /// Original field-name spelling; matching is case-insensitive.
    pub name: String,

    /// Field bytes after removing at most one separator space during decoding.
    pub value: Vec<u8>,
}

/// Fields requiring one separator space and no edge spaces or tabs.
///
/// Names match case-insensitively. The unit type selects no opaque fields.
pub trait Opaque: 'static {
    /// Names whose values use the stricter separator and edge-whitespace rules.
    const NAMES: &'static [&'static str];
}

impl Opaque for () {
    /// The default codec has no opaque field names.
    const NAMES: &'static [&'static str] = &[];
}

/// A syntax failure or a head that exceeds the configured storage bounds.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    /// The bytes or owned fields violate the supported grammar.
    Malformed,
    /// The wire head or checked decoded storage exceeds its limit.
    HeadTooLarge,
}

/// Byte-preserving ASCII MIME value with unique case-insensitive parameter names.
///
/// Tabs, edge spaces, lists, and non-ASCII bytes are rejected. Callers impose
/// field-size caps; quoted parameter values retain their spaces and escapes.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ContentType(String);

/// A parser operation that reports the crate's syntax or size error.
type Result<T> = std::result::Result<T, Error>;

impl<O: Opaque> Codec<O> {
    /// Construct a codec with a maximum head length measured in wire bytes.
    pub fn new(header_limit: usize) -> Self {
        Self {
            header_limit,
            opaque: PhantomData,
        }
    }

    /// Return the wire limit used by connection receive buffers.
    pub(crate) fn header_limit(&self) -> usize {
        self.header_limit
    }

    /// Make a codec whose wire limit cannot exceed this codec's limit.
    pub(crate) fn limited(&self, limit: usize) -> Self {
        Self::new(self.header_limit.min(limit))
    }

    /// Bound decoded field descriptors and owned wire data for admission charging.
    ///
    /// This requires a complete head but does not validate its grammar. It is not
    /// total allocator accounting. Connection I/O charges before decoding.
    pub(crate) fn decoded_allocation(&self, bytes: &[u8]) -> Result<usize> {
        let end = head_end(bytes).ok_or(Error::Malformed)?;
        Ok(HeadLayout::checked(bytes, end, self.header_limit)?.allocation)
    }

    /// Decode a complete head, or return `None` while more bytes can still fit.
    ///
    /// The returned length excludes read-ahead body bytes. Body caps are enforced
    /// by I/O after request method and response status semantics are known.
    pub fn decode_head(&self, bytes: &[u8]) -> Result<Option<(MessageHead, usize)>> {
        let bounded = &bytes[..bytes.len().min(self.header_limit)];
        let Some(end) = head_end(bounded) else {
            if bytes.len() >= self.header_limit {
                return Err(Error::HeadTooLarge);
            }
            if invalid_line_endings(bytes) {
                return Err(Error::Malformed);
            }
            return Ok(None);
        };
        if invalid_line_endings(&bytes[..end]) {
            return Err(Error::Malformed);
        }
        let layout = HeadLayout::checked(bytes, end, self.header_limit)?;
        let first = layout.first;
        let mut slots = [];
        let start = if bytes.starts_with(b"HTTP/") {
            let mut response = httparse::Response::new(&mut slots);
            response
                .parse(&bytes[..first + 2])
                .map_err(|_| Error::Malformed)?;
            if response.version != Some(1) {
                return Err(Error::Malformed);
            }
            StartLine::Response {
                status: response.code.ok_or(Error::Malformed)?,
            }
        } else {
            let mut request = httparse::Request::new(&mut slots);
            request
                .parse(&bytes[..first + 2])
                .map_err(|_| Error::Malformed)?;
            if request.version != Some(1) {
                return Err(Error::Malformed);
            }
            StartLine::Request {
                method: request.method.ok_or(Error::Malformed)?.into(),
                target: request.path.ok_or(Error::Malformed)?.into(),
            }
        };
        let mut headers = Vec::with_capacity(layout.fields);
        for line in bytes[first + 2..end - 2].split(|b| *b == b'\n') {
            if line.is_empty() {
                continue;
            }
            let line = line.strip_suffix(b"\r").ok_or(Error::Malformed)?;
            if line.is_empty() {
                continue;
            }
            let colon = line
                .iter()
                .position(|b| *b == b':')
                .ok_or(Error::Malformed)?;
            let name = std::str::from_utf8(&line[..colon])
                .map_err(|_| Error::Malformed)?
                .to_owned();
            let value = &line[colon + 1..];
            if opaque_field::<O>(&name) {
                validate_opaque_edges(value.strip_prefix(b" ").ok_or(Error::Malformed)?)?;
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

    /// Validate and encode a head with one separator space and no reason phrase.
    ///
    /// Size errors are checked before syntax errors, without allocating output.
    pub fn encode_head(&self, head: &MessageHead) -> Result<Vec<u8>> {
        let start_length = match &head.start {
            StartLine::Request { method, target } => method
                .len()
                .checked_add(target.len())
                .and_then(|n| n.checked_add(12))
                .ok_or(Error::HeadTooLarge)?,
            StartLine::Response { .. } => 15,
        };
        let length = head.headers.iter().try_fold(
            start_length.checked_add(2).ok_or(Error::HeadTooLarge)?,
            |total, h| {
                total
                    .checked_add(h.name.len())
                    .and_then(|n| n.checked_add(h.value.len()))
                    .and_then(|n| n.checked_add(4))
                    .ok_or(Error::HeadTooLarge)
            },
        )?;
        if length > self.header_limit {
            return Err(Error::HeadTooLarge);
        }
        self.validate(head)?;
        let start = match &head.start {
            StartLine::Request { method, target } => format!("{method} {target} HTTP/1.1\r\n"),
            StartLine::Response { status } => format!("HTTP/1.1 {status:03} \r\n"),
        };
        let mut bytes = Vec::with_capacity(length);
        bytes.extend_from_slice(start.as_bytes());
        for h in &head.headers {
            bytes.extend_from_slice(h.name.as_bytes());
            bytes.extend_from_slice(b": ");
            bytes.extend_from_slice(&h.value);
            bytes.extend_from_slice(b"\r\n");
        }
        bytes.extend_from_slice(b"\r\n");
        Ok(bytes)
    }

    /// Check owned syntax, opaque edges, and supported framing in that order.
    fn validate(&self, head: &MessageHead) -> Result<()> {
        match &head.start {
            StartLine::Request { method, target } => {
                if method.is_empty()
                    || !method.bytes().all(is_token)
                    || target.is_empty()
                    || !target.bytes().all(|b| b > 32 && b < 127)
                {
                    return Err(Error::Malformed);
                }
            }
            StartLine::Response { status } if !(100..=599).contains(status) => {
                return Err(Error::Malformed);
            }
            _ => (),
        }
        for h in &head.headers {
            if h.name.is_empty()
                || !h.name.bytes().all(is_token)
                || !h
                    .value
                    .iter()
                    .all(|b| *b == b'\t' || (*b >= 32 && *b != 127))
            {
                return Err(Error::Malformed);
            }
            if opaque_field::<O>(&h.name) {
                validate_opaque_edges(&h.value)?;
            }
        }
        head.content_length()?;
        head.closes_connection()?;
        head.unique("host")?;
        Ok(())
    }
}

impl MessageHead {
    /// Iterate over matching values in wire order without normalizing bytes.
    pub fn values<'a>(&'a self, name: &'a str) -> impl Iterator<Item = &'a [u8]> + 'a {
        self.headers.iter().filter_map(move |h| {
            h.name
                .eq_ignore_ascii_case(name)
                .then_some(h.value.as_slice())
        })
    }

    /// Return the sole matching value, rejecting case-insensitive duplicates.
    pub fn unique(&self, name: &str) -> Result<Option<&[u8]>> {
        let mut found = None;
        for h in &self.headers {
            if h.name.eq_ignore_ascii_case(name) {
                if found.is_some() {
                    return Err(Error::Malformed);
                }
                found = Some(h.value.as_slice());
            }
        }
        Ok(found)
    }

    /// Parse one decimal content length and reject all transfer-encoding fields.
    pub fn content_length(&self) -> Result<Option<u64>> {
        if self
            .headers
            .iter()
            .any(|h| h.name.eq_ignore_ascii_case("transfer-encoding"))
        {
            return Err(Error::Malformed);
        }
        self.unique("content-length")?.map(decimal).transpose()
    }

    /// Detect `close`, accepting only nonempty `close` and `keep-alive` tokens.
    pub fn closes_connection(&self) -> Result<bool> {
        let mut close = false;
        for value in self.values("connection") {
            for token in value.split(|b| *b == b',') {
                let token = trim_ows(token);
                if token.is_empty() || !token.iter().copied().all(is_token) {
                    return Err(Error::Malformed);
                }
                if !token.eq_ignore_ascii_case(b"close")
                    && !token.eq_ignore_ascii_case(b"keep-alive")
                {
                    return Err(Error::Malformed);
                }
                close |= token.eq_ignore_ascii_case(b"close");
            }
        }
        Ok(close)
    }
}

impl Header {
    /// Copy exact field bytes. Validation remains at the codec boundary.
    pub fn new(name: impl Into<String>, value: impl AsRef<[u8]>) -> Self {
        Self {
            name: name.into(),
            value: value.as_ref().to_vec(),
        }
    }
}

impl Drop for Header {
    /// Clear potentially sensitive field values before releasing their storage.
    fn drop(&mut self) {
        self.value.zeroize();
    }
}

impl std::fmt::Display for Error {
    /// Display the stable error variant name without including head contents.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{self:?}")
    }
}

impl std::error::Error for Error {}

impl ContentType {
    /// Validate one MIME type with optional unique parameters and retain its bytes.
    pub fn parse(bytes: &[u8]) -> Result<Self> {
        if bytes.is_empty()
            || bytes.iter().any(|b| !(0x20..=0x7e).contains(b))
            || bytes.first() == Some(&b' ')
            || bytes.last() == Some(&b' ')
        {
            return Err(Error::Malformed);
        }
        let mut rest = bytes;
        token(&mut rest)?;
        consume(&mut rest, b'/')?;
        token(&mut rest)?;
        let mut parameters: Vec<&[u8]> = Vec::new();
        while !rest.is_empty() {
            spaces(&mut rest);
            consume(&mut rest, b';')?;
            spaces(&mut rest);
            let name = token(&mut rest)?;
            if parameters.iter().any(|old| old.eq_ignore_ascii_case(name)) {
                return Err(Error::Malformed);
            }
            parameters.push(name);
            spaces(&mut rest);
            consume(&mut rest, b'=')?;
            spaces(&mut rest);
            if rest.first() == Some(&b'"') {
                rest = &rest[1..];
                loop {
                    let byte = *rest.first().ok_or(Error::Malformed)?;
                    rest = &rest[1..];
                    match byte {
                        b'"' => break,
                        b'\\' => {
                            rest = rest.get(1..).ok_or(Error::Malformed)?;
                        }
                        _ => {}
                    }
                }
            } else {
                token(&mut rest)?;
            }
        }
        Ok(Self(
            String::from_utf8(bytes.to_vec()).map_err(|_| Error::Malformed)?,
        ))
    }

    /// Borrow the original accepted MIME spelling.
    pub fn as_str(&self) -> &str {
        &self.0
    }

    /// Borrow the original accepted MIME bytes.
    pub fn as_bytes(&self) -> &[u8] {
        self.0.as_bytes()
    }
}

/// Single byte-range grammar with caller-selected numeric validation and limits.
pub mod range {
    use crate::Error;

    /// One inclusive, open-ended, or suffix byte range.
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub enum ByteRange {
        /// An inclusive interval whose first offset does not exceed its last.
        Closed {
            /// The first requested byte offset.
            first: u64,

            /// The last requested byte offset.
            last: u64,
        },
        /// All bytes starting at the given offset.
        From(u64),
        /// The requested suffix length, including zero if numeric policy allows it.
        Suffix(u64),
    }

    /// A satisfied Content-Range with a known complete representation length.
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub struct ContentRange {
        /// First included byte offset.
        pub first: u64,

        /// Last included byte offset.
        pub last: u64,

        /// Complete representation length, greater than the last offset.
        pub total: u64,
    }

    impl ByteRange {
        /// Parse one range without trimming, normalizing, or merging. The numeric
        /// parser receives nonempty components exactly as written, even if malformed.
        pub fn parse_with<E: From<Error>>(
            value: &[u8],
            mut decimal: impl FnMut(&[u8]) -> Result<u64, E>,
        ) -> Result<Self, E> {
            let bounds = value.strip_prefix(b"bytes=").ok_or(Error::Malformed)?;
            let separator = bounds
                .iter()
                .position(|&b| b == b'-')
                .ok_or(Error::Malformed)?;
            let (first, rest) = bounds.split_at(separator);
            let last = &rest[1..];
            match (first.is_empty(), last.is_empty()) {
                (true, true) => Err(Error::Malformed.into()),
                (true, false) => Ok(Self::Suffix(decimal(last)?)),
                (false, true) => Ok(Self::From(decimal(first)?)),
                (false, false) => {
                    let (first, last) = (decimal(first)?, decimal(last)?);
                    if first > last {
                        return Err(Error::Malformed.into());
                    }
                    Ok(Self::Closed { first, last })
                }
            }
        }
    }

    impl ContentRange {
        /// Parse exact byte syntax and enforce first <= last < total. Wildcard
        /// representations and unsatisfied ranges are separate caller contracts.
        pub fn parse_with<E: From<Error>>(
            value: &[u8],
            mut decimal: impl FnMut(&[u8]) -> Result<u64, E>,
        ) -> Result<Self, E> {
            let value = value.strip_prefix(b"bytes ").ok_or(Error::Malformed)?;
            let slash = value
                .iter()
                .position(|&b| b == b'/')
                .ok_or(Error::Malformed)?;
            let bounds = &value[..slash];
            let dash = bounds
                .iter()
                .position(|&b| b == b'-')
                .ok_or(Error::Malformed)?;
            let first = decimal(&bounds[..dash])?;
            let last = decimal(&bounds[dash + 1..])?;
            let total = decimal(&value[slash + 1..])?;
            if first > last || last >= total {
                return Err(Error::Malformed.into());
            }
            Ok(Self { first, last, total })
        }
    }
}

/// Structural offsets and checked storage shared by decoding and admission.
struct HeadLayout {
    /// Offset of the first CRLF, which ends the start line.
    first: usize,

    /// Number of field descriptors to reserve.
    fields: usize,

    /// Checked charge for descriptors, owned bytes, and the head itself.
    allocation: usize,
}

impl HeadLayout {
    /// Count complete lines once after the caller locates the terminating CRLF pair.
    ///
    /// Grammar validation stays with decoding, preserving admission error ordering.
    fn checked(bytes: &[u8], end: usize, limit: usize) -> Result<Self> {
        if end > limit {
            return Err(Error::HeadTooLarge);
        }
        let mut lines = bytes[..end]
            .windows(2)
            .enumerate()
            .filter_map(|(offset, pair)| (pair == b"\r\n").then_some(offset));
        let first = lines.next().ok_or(Error::Malformed)?;
        let fields = lines.count().saturating_sub(1);
        if fields > limit / 4 {
            return Err(Error::HeadTooLarge);
        }
        let allocation = fields
            .checked_mul(std::mem::size_of::<Header>())
            .and_then(|n| n.checked_add(end))
            .and_then(|n| n.checked_add(std::mem::size_of::<MessageHead>()))
            .ok_or(Error::HeadTooLarge)?;
        Ok(Self {
            first,
            fields,
            allocation,
        })
    }
}

/// Find the first complete head terminator without including read-ahead bytes.
fn head_end(bytes: &[u8]) -> Option<usize> {
    bytes
        .windows(4)
        .position(|w| w == b"\r\n\r\n")
        .map(|n| n + 4)
}

/// Match a field name against the caller's case-insensitive opaque list.
fn opaque_field<O: Opaque>(name: &str) -> bool {
    O::NAMES.iter().any(|n| name.eq_ignore_ascii_case(n))
}

/// Reject spaces or tabs at either edge of an opaque value, allowing empty values.
fn validate_opaque_edges(value: &[u8]) -> Result<()> {
    if value.first().is_some_and(|b| *b == b' ' || *b == b'\t')
        || value.last().is_some_and(|b| *b == b' ' || *b == b'\t')
    {
        return Err(Error::Malformed);
    }
    Ok(())
}

/// Test whether one byte belongs to an HTTP token.
pub fn is_token(b: u8) -> bool {
    b.is_ascii_alphanumeric() || b"!#$%&'*+-.^_`|~".contains(&b)
}

/// Detect bare line breaks while allowing a final CR awaiting the next fragment.
fn invalid_line_endings(bytes: &[u8]) -> bool {
    bytes.iter().enumerate().any(|(i, b)| {
        (*b == b'\n' && (i == 0 || bytes[i - 1] != b'\r'))
            || (*b == b'\r' && i + 1 < bytes.len() && bytes[i + 1] != b'\n')
    })
}

/// Borrow a value without leading or trailing HTTP optional whitespace.
pub fn trim_ows(mut value: &[u8]) -> &[u8] {
    while value.first().is_some_and(|b| *b == b' ' || *b == b'\t') {
        value = &value[1..];
    }
    while value.last().is_some_and(|b| *b == b' ' || *b == b'\t') {
        value = &value[..value.len() - 1];
    }
    value
}

/// Parse nonempty unsigned decimal bytes with checked arithmetic and no trimming.
fn decimal(value: &[u8]) -> Result<u64> {
    if value.is_empty() {
        return Err(Error::Malformed);
    }
    value.iter().try_fold(0u64, |n, b| {
        if !b.is_ascii_digit() {
            return Err(Error::Malformed);
        }
        n.checked_mul(10)
            .and_then(|n| n.checked_add(u64::from(*b - b'0')))
            .ok_or(Error::Malformed)
    })
}

/// Consume MIME separator spaces, without treating tabs as spaces.
fn spaces(rest: &mut &[u8]) {
    while rest.first() == Some(&b' ') {
        *rest = &rest[1..];
    }
}

/// Consume one required MIME delimiter.
fn consume(rest: &mut &[u8], byte: u8) -> Result<()> {
    if rest.first() != Some(&byte) {
        return Err(Error::Malformed);
    }
    *rest = &rest[1..];
    Ok(())
}

/// Consume and borrow one nonempty MIME token.
fn token<'a>(rest: &mut &'a [u8]) -> Result<&'a [u8]> {
    let length = rest.iter().take_while(|b| is_token(**b)).count();
    if length == 0 {
        return Err(Error::Malformed);
    }
    let value = &rest[..length];
    *rest = &rest[length..];
    Ok(value)
}

/// Parser contracts for size bounds, exact bytes, MIME values, and byte ranges.
#[cfg(test)]
mod tests {
    use super::range::{ByteRange, ContentRange};
    use super::*;

    /// The ordinary head cap used by parser fixtures.
    const MAX_HEAD_BYTES: usize = 32 * 1024;
    /// Opaque field policy used by codec tests.
    struct Fields;
    impl Opaque for Fields {
        /// Fields with strict separator and edge-whitespace requirements.
        const NAMES: &'static [&'static str] = &["authorization", "x-metadata"];
    }
    /// A codec using the test opaque policy.
    type TestCodec = Codec<Fields>;

    /// Construction copies bytes, while encoding still rejects invalid field syntax.
    #[test]
    fn header_constructor_copies_exact_bytes_and_codec_still_validates() {
        let mut value = b" padded\t".to_vec();
        let header = Header::new("X-MiXeD", &value);
        value.fill(0);
        assert_eq!(header.name, "X-MiXeD");
        assert_eq!(header.value, b" padded\t");
        let head = MessageHead {
            start: StartLine::Response { status: 200 },
            headers: vec![Header::new("X-Test", b"bad\r\nvalue")],
        };
        assert_eq!(
            Codec::<()>::new(128).encode_head(&head),
            Err(Error::Malformed)
        );
    }

    /// Maximum wire heads and dense fields have checked descriptor and byte charges.
    #[test]
    fn maximum_wire_heads_and_maximum_field_count_have_checked_decoded_bounds() {
        for limit in [MAX_HEAD_BYTES, 18 * 64 * 1024] {
            let codec = TestCodec::new(limit);
            let mut bytes = b"GET / HTTP/1.1\r\nX: ".to_vec();
            bytes.resize(limit - 4, b'a');
            bytes.extend_from_slice(b"\r\n\r\n");
            let (head, used) = codec.decode_head(&bytes).unwrap().unwrap();
            assert_eq!(used, limit);
            assert_eq!(codec.encode_head(&head).unwrap(), bytes);
            let allocation = codec.decoded_allocation(&bytes).unwrap();
            assert!(allocation >= head.headers[0].value.len() + std::mem::size_of::<Header>());
            let mut fields = b"GET / HTTP/1.1\r\n".to_vec();
            while fields.len() + 6 <= limit {
                fields.extend_from_slice(b"X:\r\n");
            }
            fields.extend_from_slice(b"\r\n");
            let count = (fields.len() - 18) / 4;
            let allocation = codec.decoded_allocation(&fields).unwrap();
            assert!(allocation >= count * std::mem::size_of::<Header>());
            assert_eq!(
                codec.decode_head(&fields).unwrap().unwrap().0.headers.len(),
                count
            );
            assert!(matches!(
                TestCodec::new(limit - 1).decoded_allocation(&bytes),
                Err(Error::HeadTooLarge)
            ));
        }
    }

    /// Every fragment boundary retains ordinary field bytes and duplicate ordering.
    #[test]
    fn fragmented_head_preserves_opaque_values_and_duplicates() {
        let codec = TestCodec::new(1024);
        let bytes = b"GET /a HTTP/1.1\r\nX-Opaque:  \xff\t \r\nx-opaque: second\r\nContent-Length: 3\r\n\r\nabc";
        let end = bytes.len() - 3;
        for length in 0..end {
            assert!(codec.decode_head(&bytes[..length]).unwrap().is_none());
        }
        let (head, used) = codec.decode_head(bytes).unwrap().unwrap();
        assert_eq!(used, end);
        assert_eq!(head.headers[0].value, b" \xff\t ");
        assert_eq!(head.values("x-OPAQUE").count(), 2);
        assert_eq!(head.unique("x-opaque"), Err(Error::Malformed));
        assert_eq!(codec.encode_head(&head).unwrap(), bytes[..end]);
    }

    /// Unsupported framing and malformed head syntax cannot enter the codec.
    #[test]
    fn rejects_smuggling_and_malformed_syntax() {
        let codec = TestCodec::new(1024);
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

    /// Wire limits ignore body declarations and read-ahead but still reject injection.
    #[test]
    fn bounds_heads_without_counting_read_ahead_or_head_representation_length() {
        let bytes = b"HTTP/1.1 200 OK\r\nContent-Length: 99999999\r\n\r\n";
        let codec = TestCodec::new(bytes.len());
        assert_eq!(codec.decode_head(bytes).unwrap().unwrap().1, bytes.len());
        assert!(TestCodec::new(bytes.len() - 1).decode_head(bytes).is_err());
        assert!(TestCodec::new(8).decode_head(b"GET /thi").is_err());
        let head = MessageHead {
            start: StartLine::Response { status: 200 },
            headers: vec![Header {
                name: "x".into(),
                value: b"a\r\ninjected: true".to_vec(),
            }],
        };
        assert!(codec.encode_head(&head).is_err());
    }

    /// Strict fields reject ambiguous separators while retaining accepted high bytes.
    #[test]
    fn opaque_field_raw_separators_are_validated_before_decoding() {
        let codec = TestCodec::new(MAX_HEAD_BYTES);
        for field in ["Authorization", "X-Metadata"] {
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
            Err(Error::HeadTooLarge)
        ));
    }

    /// Opaque policy applies case-insensitively only when a name is configured.
    #[test]
    fn empty_and_custom_opaque_lists_are_case_insensitive() {
        let bytes = b"GET / HTTP/1.1\r\nAuThOrIzAtIoN:  padded \r\n\r\n";
        let plain = Codec::<()>::new(1024);
        let head = plain.decode_head(bytes).unwrap().unwrap().0;
        assert_eq!(plain.encode_head(&head).unwrap(), bytes);
        assert!(TestCodec::new(1024).decode_head(bytes).is_err());
        assert!(TestCodec::new(1024).encode_head(&head).is_err());
    }

    /// Accepted MIME syntax preserves every byte, including parameter quoting.
    #[test]
    fn mime_preserves_case_spaces_quotes_and_escapes() {
        for value in [
            "Text/Plain",
            "text/plain ; Charset = utf-8",
            "text/plain;x=\"\"",
            "text/plain;x=\"a;b\\\"c\";y=z",
            "text/plain;x=\" \\\\ \"",
        ] {
            let parsed = ContentType::parse(value.as_bytes()).unwrap();
            assert_eq!(parsed.as_str(), value);
            assert_eq!(parsed.as_bytes(), value.as_bytes());
        }
        assert!(ContentType::parse(format!("a/{}", "b".repeat(300)).as_bytes()).is_ok());
    }

    /// Invalid MIME syntax is rejected instead of repaired or normalized.
    #[test]
    fn malformed_mime_is_rejected_without_normalization() {
        for value in [
            "",
            "text",
            "text/",
            "/plain",
            " text/plain",
            "text/plain ",
            "text/plain, text/html",
            "text/plain;",
            "text/plain;x",
            "text/plain;x=",
            "text/plain;x=\"",
            "text/plain;x=\"\\",
            "text/plain;x=a;X=b",
            "text/plain\r\nx:y",
            "text/\tplain",
            "text/pläin",
            "text/plain;x=\"a\"junk",
        ] {
            assert_eq!(
                ContentType::parse(value.as_bytes()),
                Err(Error::Malformed),
                "{value:?}"
            );
        }
    }

    /// Parse canonical unsigned decimal fixtures without redundant leading zeros.
    fn canonical(bytes: &[u8]) -> std::result::Result<u64, Error> {
        if bytes.is_empty() || (bytes.len() > 1 && bytes[0] == b'0') {
            return Err(Error::Malformed);
        }
        bytes.iter().try_fold(0u64, |n, b| {
            if !b.is_ascii_digit() {
                return Err(Error::Malformed);
            }
            n.checked_mul(10)
                .and_then(|n| n.checked_add(u64::from(b - b'0')))
                .ok_or(Error::Malformed)
        })
    }

    /// Range syntax delegates numeric limits and permits a policy-approved zero suffix.
    #[test]
    fn single_range_preserves_numeric_policy_and_zero_suffix() {
        for (wire, expected) in [
            ("bytes=0-1", ByteRange::Closed { first: 0, last: 1 }),
            ("bytes=2-", ByteRange::From(2)),
            ("bytes=-0", ByteRange::Suffix(0)),
            ("bytes=-18446744073709551615", ByteRange::Suffix(u64::MAX)),
        ] {
            assert_eq!(
                ByteRange::parse_with(wire.as_bytes(), canonical),
                Ok(expected)
            );
        }
        for wire in [
            "bytes=-",
            "Bytes=0-1",
            " bytes=0-1",
            "bytes=0-1 ",
            "bytes=00-1",
            "bytes=-01",
            "bytes=2-1",
            "bytes=0-1,2-3",
            "bytes=+1-2",
            "bytes=1--2",
            "bytes=-18446744073709551616",
        ] {
            assert_eq!(
                ByteRange::parse_with(wire.as_bytes(), canonical),
                Err(Error::Malformed),
                "{wire}"
            );
        }
    }

    /// Content ranges enforce bound ordering and pass unmodified numbers to policy.
    #[test]
    fn content_range_checks_structure_and_exact_numeric_inputs() {
        assert_eq!(
            ContentRange::parse_with(b"bytes 0-1/2", canonical),
            Ok(ContentRange {
                first: 0,
                last: 1,
                total: 2
            })
        );
        for wire in [
            "bytes */2",
            "bytes 0-1/*",
            "bytes 0-1/1",
            "bytes 2-1/3",
            "bytes 00-1/2",
            "bytes 0-01/2",
            "bytes 0-1/02",
            "bytes 0-1/2 ",
            "bytes  0-1/2",
            "bytes 0-1/2/3",
            "bytes 0--1/2",
            "bytes 0-0/0",
        ] {
            assert_eq!(
                ContentRange::parse_with(wire.as_bytes(), canonical),
                Err(Error::Malformed),
                "{wire}"
            );
        }
        let mut seen = Vec::new();
        let result = ContentRange::parse_with(b"bytes 01-02/03", |bytes| {
            seen.push(bytes.to_vec());
            Ok::<_, Error>(seen.len() as u64)
        });
        assert!(result.is_ok());
        assert_eq!(seen, [b"01", b"02", b"03"]);
    }

    /// Admission charging shares layout bounds without taking over syntax validation.
    #[test]
    fn checked_layout_excludes_read_ahead_and_preserves_admission_error_order() {
        let wire = b"GET / HTTP/1.1\r\nX: a\r\nY:\r\n\r\nbody\r\n\r\n";
        let end = wire.len() - b"body\r\n\r\n".len();
        let codec = Codec::<()>::new(end);
        let expected = 2 * std::mem::size_of::<Header>() + end + std::mem::size_of::<MessageHead>();
        assert_eq!(codec.decoded_allocation(wire), Ok(expected));
        let (head, used) = codec.decode_head(wire).unwrap().unwrap();
        assert_eq!(used, end);
        assert_eq!(head.headers.len(), 2);
        assert_eq!(head.headers[0].value, b"a");
        assert_eq!(head.headers[1].value, b"");
        assert_eq!(
            Codec::<()>::new(end - 1).decoded_allocation(wire),
            Err(Error::HeadTooLarge)
        );

        // Allocation requires a terminator even when incomplete bytes exceed the cap.
        assert_eq!(
            Codec::<()>::new(1).decoded_allocation(b"not a head"),
            Err(Error::Malformed)
        );
        let malformed = b"GET / HTTP/1.1\nX: a\r\n\r\n";
        assert!(codec.decoded_allocation(malformed).is_ok());
        assert!(matches!(
            codec.decode_head(malformed),
            Err(Error::Malformed)
        ));
        assert_eq!(
            Codec::<()>::new(1).decoded_allocation(malformed),
            Err(Error::HeadTooLarge)
        );
    }

    /// Decode and encode keep their existing precedence between size and syntax errors.
    #[test]
    fn layout_refactor_preserves_incomplete_and_invalid_head_error_order() {
        let codec = Codec::<()>::new(4);
        assert!(matches!(
            codec.decode_head(b"\nxxx"),
            Err(Error::HeadTooLarge)
        ));
        assert!(matches!(codec.decode_head(b"\nx"), Err(Error::Malformed)));
        assert!(codec.decode_head(b"x\r").unwrap().is_none());
        assert!(matches!(
            Codec::<()>::new(0).decode_head(b""),
            Err(Error::HeadTooLarge)
        ));
        let complete = b"GET / HTTP/1.1\nX: a\r\n\r\n";
        assert!(matches!(
            Codec::<()>::new(complete.len()).decode_head(complete),
            Err(Error::Malformed)
        ));
        assert!(matches!(
            Codec::<()>::new(complete.len() - 1).decode_head(complete),
            Err(Error::HeadTooLarge)
        ));
        let invalid = MessageHead {
            start: StartLine::Response { status: 99 },
            headers: vec![Header::new("X", b"bad\r\nvalue")],
        };
        assert_eq!(codec.encode_head(&invalid), Err(Error::HeadTooLarge));
        assert_eq!(
            Codec::<()>::new(1024).encode_head(&invalid),
            Err(Error::Malformed)
        );
    }

    /// The codec accepts informational statuses and removes only one ordinary space.
    #[test]
    fn codec_retains_status_and_separator_semantics() {
        let codec = Codec::<()>::new(1024);
        for status in [100, 101, 199, 200, 599] {
            let wire = format!("HTTP/1.1 {status} reason\r\nX:  padded\t \r\n\r\n");
            let (head, used) = codec.decode_head(wire.as_bytes()).unwrap().unwrap();
            assert_eq!(used, wire.len());
            assert!(matches!(head.start, StartLine::Response { status: value } if value == status));
            assert_eq!(head.headers[0].value, b" padded\t ");
            assert_eq!(
                codec.encode_head(&head).unwrap(),
                format!("HTTP/1.1 {status} \r\nX:  padded\t \r\n\r\n").as_bytes()
            );
        }
        for (suffix, expected) in [
            (&b"value"[..], &b"value"[..]),
            (&b" value"[..], &b"value"[..]),
            (&b"  value"[..], &b" value"[..]),
            (&b"\tvalue"[..], &b"\tvalue"[..]),
        ] {
            let mut wire = b"GET / HTTP/1.1\r\nX:".to_vec();
            wire.extend_from_slice(suffix);
            wire.extend_from_slice(b"\r\n\r\n");
            let (head, _) = codec.decode_head(&wire).unwrap().unwrap();
            assert_eq!(head.headers[0].value, expected);
        }
    }
}
