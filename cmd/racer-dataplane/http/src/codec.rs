use std::marker::PhantomData;
use zeroize::Zeroize;
#[cfg(test)]
mod tests;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    Malformed,
    HeadTooLarge,
}
impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{self:?}")
    }
}
impl std::error::Error for Error {}
type Result<T> = std::result::Result<T, Error>;

/// Fields whose values require exactly one separator SP and no edge whitespace.
/// Matching is ASCII case-insensitive. Ordinary values retain their wire bytes.
pub trait Opaque: 'static {
    const NAMES: &'static [&'static str];
}
impl Opaque for () {
    const NAMES: &'static [&'static str] = &[];
}

pub enum StartLine {
    Request { method: String, target: String },
    Response { status: u16 },
}
pub struct Header {
    pub name: String,
    pub value: Vec<u8>,
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
    pub fn values<'a>(&'a self, name: &'a str) -> impl Iterator<Item = &'a [u8]> + 'a {
        self.headers.iter().filter_map(move |h| {
            h.name
                .eq_ignore_ascii_case(name)
                .then_some(h.value.as_slice())
        })
    }
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
pub struct Codec<O: Opaque = ()> {
    header_limit: usize,
    opaque: PhantomData<O>,
}
impl<O: Opaque> Codec<O> {
    pub fn new(header_limit: usize) -> Self {
        Self {
            header_limit,
            opaque: PhantomData,
        }
    }
    pub fn header_limit(&self) -> usize {
        self.header_limit
    }
    pub fn limited(&self, limit: usize) -> Self {
        Self::new(self.header_limit.min(limit))
    }
    /// Checked upper bound for decoded field descriptors and owned wire data.
    pub fn decoded_allocation(&self, bytes: &[u8]) -> Result<usize> {
        let end = bytes
            .windows(4)
            .position(|w| w == b"\r\n\r\n")
            .map(|n| n + 4)
            .ok_or(Error::Malformed)?;
        if end > self.header_limit {
            return Err(Error::HeadTooLarge);
        }
        let fields = bytes[..end]
            .windows(2)
            .filter(|w| *w == b"\r\n")
            .count()
            .saturating_sub(2);
        if fields > self.header_limit / 4 {
            return Err(Error::HeadTooLarge);
        }
        fields
            .checked_mul(std::mem::size_of::<Header>())
            .and_then(|n| n.checked_add(end))
            .and_then(|n| n.checked_add(std::mem::size_of::<MessageHead>()))
            .ok_or(Error::HeadTooLarge)
    }
    /// Body caps are enforced by I/O after request method/status semantics are known.
    pub fn decode_head(&self, bytes: &[u8]) -> Result<Option<(MessageHead, usize)>> {
        let bounded = &bytes[..bytes.len().min(self.header_limit)];
        let Some(end) = bounded
            .windows(4)
            .position(|w| w == b"\r\n\r\n")
            .map(|n| n + 4)
        else {
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
        self.decoded_allocation(&bytes[..end])?;
        let count = bytes[..end].windows(2).filter(|w| *w == b"\r\n").count();
        let first = bytes[..end]
            .windows(2)
            .position(|w| w == b"\r\n")
            .ok_or(Error::Malformed)?;
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
        let mut headers = Vec::with_capacity(count.saturating_sub(2));
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
fn opaque_field<O: Opaque>(name: &str) -> bool {
    O::NAMES.iter().any(|n| name.eq_ignore_ascii_case(n))
}
fn validate_opaque_edges(value: &[u8]) -> Result<()> {
    if value.first().is_some_and(|b| *b == b' ' || *b == b'\t')
        || value.last().is_some_and(|b| *b == b' ' || *b == b'\t')
    {
        return Err(Error::Malformed);
    }
    Ok(())
}
pub fn is_token(b: u8) -> bool {
    b.is_ascii_alphanumeric() || b"!#$%&'*+-.^_`|~".contains(&b)
}
fn invalid_line_endings(bytes: &[u8]) -> bool {
    bytes.iter().enumerate().any(|(i, b)| {
        (*b == b'\n' && (i == 0 || bytes[i - 1] != b'\r'))
            || (*b == b'\r' && i + 1 < bytes.len() && bytes[i + 1] != b'\n')
    })
}
pub fn trim_ows(mut value: &[u8]) -> &[u8] {
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
