// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Shared header primitives: the borrowed [`Header`] view, the coarse
//! [`ParseError`] both the request and response parsers map onto, and
//! the small case-insensitive lookup / value-extraction helpers reused
//! across the request and response codecs.

/// A single parsed header, borrowing the name and value from the source
/// buffer. The name is compared case-insensitively via [`header`]; the
/// raw casing is preserved here.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Header<'a> {
    pub name: &'a str,
    pub value: &'a str,
}

/// Why a request or response head failed to parse.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ParseError {
    /// The buffer does not yet contain a full header block (no blank
    /// line terminator). The caller should read more bytes and retry.
    Incomplete,
    /// The request line was malformed (wrong field count, bad version,
    /// non-UTF-8, ...).
    BadRequestLine,
    /// The response status line was malformed (missing/invalid code,
    /// bad version, ...).
    BadStatusLine,
    /// A header line lacked a `:` separator or was otherwise malformed.
    BadHeader,
    /// The bytes were not valid UTF-8 where text was required.
    NotUtf8,
}

/// Upper bound on headers parsed from a single request or response.
/// Heads with more headers than this are rejected
/// (`httparse::Error::TooManyHeaders`).
pub(crate) const MAX_HEADERS: usize = 64;

/// Collect raw [`httparse::Header`]s into owned borrowed [`Header`]
/// views, validating each value is UTF-8 and trimming surrounding
/// whitespace. Returns [`ParseError::NotUtf8`] on a non-UTF-8 value.
pub(crate) fn collect_headers<'a>(
    raw: &[httparse::Header<'a>],
) -> Result<Vec<Header<'a>>, ParseError> {
    let mut headers = Vec::with_capacity(raw.len());
    for h in raw.iter() {
        let value = std::str::from_utf8(h.value)
            .map_err(|_| ParseError::NotUtf8)?
            .trim();
        headers.push(Header {
            name: h.name,
            value,
        });
    }
    Ok(headers)
}

/// Look up a header by case-insensitive name, returning its (already
/// trimmed) value. Returns the first match if a header is repeated.
pub(crate) fn header<'a>(headers: &[Header<'a>], name: &str) -> Option<&'a str> {
    headers
        .iter()
        .find(|h| h.name.eq_ignore_ascii_case(name))
        .map(|h| h.value)
}

/// Count headers matching `name` case-insensitively.
pub(crate) fn header_count(headers: &[Header<'_>], name: &str) -> usize {
    headers
        .iter()
        .filter(|h| h.name.eq_ignore_ascii_case(name))
        .count()
}

/// True when any matching header value contains `token`.
pub(crate) fn any_header_value_has_token(headers: &[Header<'_>], name: &str, token: &str) -> bool {
    headers
        .iter()
        .filter(|h| h.name.eq_ignore_ascii_case(name))
        .any(|h| header_value_has_token(h.value, token))
}

/// Return whether a comma-separated header value contains `token`, using
/// HTTP's case-insensitive token matching rules.
pub(crate) fn header_value_has_token(value: &str, token: &str) -> bool {
    value
        .split(',')
        .any(|part| part.trim().eq_ignore_ascii_case(token))
}

/// Parse a `Content-Length` header value (case-insensitive name) into a
/// byte count, or `None` if absent or unparseable.
pub(crate) fn content_length(headers: &[Header]) -> Option<u64> {
    header(headers, "content-length").and_then(|v| v.trim().parse::<u64>().ok())
}

/// Parse the start offset out of a `Content-Range: bytes <start>-<end>/<total>`
/// header (case-insensitive name). Returns `None` when the header is
/// absent or unparseable; callers consult it only for validation, so a
/// `None` simply skips that check.
pub(crate) fn content_range_start(headers: &[Header]) -> Option<u64> {
    let value = header(headers, "content-range")?;
    // "bytes <start>-<end>/<total>"; isolate <start>.
    let spec = value.trim().strip_prefix("bytes")?.trim_start();
    let range = spec.split('/').next()?;
    let start = range.split('-').next()?.trim();
    start.parse::<u64>().ok()
}

/// Map an [`httparse::Error`] onto our coarser [`ParseError`]. httparse
/// does not finely distinguish request/status line from header faults,
/// so we classify by which token position the error names: invalid
/// header names/values are [`ParseError::BadHeader`], and everything
/// else (version, token, new-line, status, too-many-headers) is treated
/// as a malformed request line.
pub(crate) fn map_httparse_error(e: httparse::Error) -> ParseError {
    match e {
        httparse::Error::HeaderName | httparse::Error::HeaderValue => ParseError::BadHeader,
        _ => ParseError::BadRequestLine,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn headers<'a>(pairs: &[(&'a str, &'a str)]) -> Vec<Header<'a>> {
        pairs
            .iter()
            .map(|&(name, value)| Header { name, value })
            .collect()
    }

    #[test]
    fn header_lookup_is_case_insensitive_and_first_wins() {
        let hs = headers(&[("Content-Length", "1"), ("content-length", "2")]);
        assert_eq!(header(&hs, "content-length"), Some("1"));
        assert_eq!(header(&hs, "CONTENT-LENGTH"), Some("1"));
        assert_eq!(header(&hs, "missing"), None);
    }

    #[test]
    fn content_length_parses_or_none() {
        assert_eq!(
            content_length(&headers(&[("Content-Length", "4096")])),
            Some(4096)
        );
        assert_eq!(content_length(&headers(&[("content-length", "x")])), None);
        assert_eq!(content_length(&headers(&[("Content-Type", "x")])), None);
    }

    #[test]
    fn header_value_token_matching_is_case_insensitive() {
        assert!(header_value_has_token("keep-alive, Close", "close"));
        assert!(header_value_has_token(" keep-alive ", "KEEP-ALIVE"));
        assert!(!header_value_has_token("keep-alive", "close"));
        assert!(!header_value_has_token("upgrade", "up"));
    }

    #[test]
    fn content_range_start_extracts_offset() {
        let hs = headers(&[("Content-Range", "bytes 8192-12287/100000")]);
        assert_eq!(content_range_start(&hs), Some(8192));
        // Case-insensitive name.
        let lower = headers(&[("content-range", "bytes 5-9/10")]);
        assert_eq!(content_range_start(&lower), Some(5));
        // Absent header.
        assert_eq!(
            content_range_start(&headers(&[("Content-Length", "10")])),
            None
        );
    }
}
