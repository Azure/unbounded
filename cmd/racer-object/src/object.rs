// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Request semantics shared by the S3 and Azure APIs.
//!
//! Object identities must match the in-tree `internal/racerobject` library
//! byte for byte so both compute the same Racer keys. Everything here is
//! pure (no I/O) so it can be tested exhaustively and so the hot path can reuse
//! buffers owned by the connection.

use sha2::{Digest, Sha256};

/// Largest canonical JSON identity, matching the Racer field limit.
pub const MAX_CANONICAL: usize = 8 << 10;

/// Azure Blob service version reported on every Azure response.
pub const AZURE_VERSION: &str = "2026-02-06";

/// Client API served by the sidecar.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Api {
    S3,
    Azure,
}

/// An HTTP failure with a fixed error code per API. Messages always come from
/// the status text, never from request or upstream data.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Failure {
    BadRequest,
    Forbidden,
    NotFound,
    MethodNotAllowed,
    PreconditionFailed,
    /// Carries the object size for `Content-Range: bytes */size` when the
    /// sidecar resolved the range itself.
    RangeNotSatisfiable(Option<u64>),
    HeaderTooLarge,
    Internal,
    NotImplemented,
    BadGateway,
    Unavailable,
}

impl Failure {
    pub fn status(self) -> u16 {
        match self {
            Self::BadRequest => 400,
            Self::Forbidden => 403,
            Self::NotFound => 404,
            Self::MethodNotAllowed => 405,
            Self::PreconditionFailed => 412,
            Self::RangeNotSatisfiable(_) => 416,
            Self::HeaderTooLarge => 431,
            Self::Internal => 500,
            Self::NotImplemented => 501,
            Self::BadGateway => 502,
            Self::Unavailable => 503,
        }
    }

    pub fn code(self, api: Api) -> &'static str {
        match (self, api) {
            (Self::BadRequest, Api::S3) => "InvalidArgument",
            (Self::BadRequest, Api::Azure) => "InvalidInput",
            (Self::Forbidden, Api::S3) => "AccessDenied",
            (Self::Forbidden, Api::Azure) => "AuthorizationFailure",
            (Self::NotFound, Api::S3) => "NoSuchKey",
            (Self::NotFound, Api::Azure) => "BlobNotFound",
            (Self::MethodNotAllowed, Api::S3) => "MethodNotAllowed",
            (Self::MethodNotAllowed, Api::Azure) => "UnsupportedHttpVerb",
            (Self::PreconditionFailed, Api::S3) => "PreconditionFailed",
            (Self::PreconditionFailed, Api::Azure) => "ConditionNotMet",
            (Self::RangeNotSatisfiable(_), _) => "InvalidRange",
            (Self::HeaderTooLarge, Api::S3) => "RequestHeaderSectionTooLarge",
            (Self::HeaderTooLarge, Api::Azure) => "InvalidInput",
            (Self::Internal, _) => "InternalError",
            (Self::NotImplemented, _) => "NotImplemented",
            (Self::BadGateway, _) => "BadGateway",
            (Self::Unavailable, Api::S3) => "ServiceUnavailable",
            (Self::Unavailable, Api::Azure) => "ServerBusy",
        }
    }
}

/// Standard reason phrase for every status the sidecar emits.
pub fn status_text(status: u16) -> &'static str {
    match status {
        200 => "OK",
        206 => "Partial Content",
        304 => "Not Modified",
        400 => "Bad Request",
        403 => "Forbidden",
        404 => "Not Found",
        405 => "Method Not Allowed",
        412 => "Precondition Failed",
        416 => "Requested Range Not Satisfiable",
        431 => "Request Header Fields Too Large",
        500 => "Internal Server Error",
        501 => "Not Implemented",
        502 => "Bad Gateway",
        503 => "Service Unavailable",
        _ => "",
    }
}

// ---------------------------------------------------------------------------
// Object identity
// ---------------------------------------------------------------------------

/// Validated object identity: the Racer key and canonical JSON metadata.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Identity {
    /// Lowercase hex SHA-256 of `metadata`.
    pub key: [u8; 64],
    /// Canonical schema-1 JSON, byte-identical to `internal/racerobject`
    /// so the Racer key matches.
    pub metadata: Vec<u8>,
}

impl Identity {
    pub fn key_str(&self) -> &str {
        // Only lowercase hex digits are ever written to `key`.
        std::str::from_utf8(&self.key).unwrap_or_default()
    }
}

fn valid_bucket(bucket: &[u8]) -> bool {
    !bucket.is_empty()
        && bucket.len() <= 255
        && bucket != b"."
        && bucket != b".."
        && bucket
            .iter()
            .all(|&c| c.is_ascii_alphanumeric() || c == b'.' || c == b'-' || c == b'_')
}

/// Build the identity for an exact object. Keys and version IDs are never
/// cleaned, decoded, or normalized: they are hashed exactly as given.
pub fn identity(bucket: &[u8], key: &[u8], version: &[u8]) -> Option<Identity> {
    if !valid_bucket(bucket)
        || key.is_empty()
        || key.len() > 1024
        || std::str::from_utf8(key).is_err()
        || version.len() > 1024
        || std::str::from_utf8(version).is_err()
    {
        return None;
    }
    let mut json = Vec::with_capacity(48 + bucket.len() + key.len() + version.len());
    json.extend_from_slice(b"{\"schema\":1,\"bucket\":");
    json_string(&mut json, bucket);
    json.extend_from_slice(b",\"key\":");
    json_string(&mut json, key);
    if !version.is_empty() {
        json.extend_from_slice(b",\"versionId\":");
        json_string(&mut json, version);
    }
    json.push(b'}');
    // Racer rejects oversized or control-character metadata. JSON escapes C0 controls but leaves DEL as is.
    if json.len() > MAX_CANONICAL || json.contains(&0x7f) {
        return None;
    }
    let digest = Sha256::digest(&json);
    let mut hex = [0u8; 64];
    for (i, b) in digest.iter().enumerate() {
        hex[2 * i] = HEX_LOWER[usize::from(b >> 4)];
        hex[2 * i + 1] = HEX_LOWER[usize::from(b & 15)];
    }
    Some(Identity {
        key: hex,
        metadata: json,
    })
}

const HEX_LOWER: &[u8; 16] = b"0123456789abcdef";
const HEX_UPPER: &[u8; 16] = b"0123456789ABCDEF";

/// Append a JSON string in the canonical encoding used by
/// `internal/racerobject`, including HTML-safe escapes and U+2028/U+2029
/// escapes.
fn json_string(out: &mut Vec<u8>, s: &[u8]) {
    out.push(b'"');
    let mut i = 0;
    let mut start = 0;
    while i < s.len() {
        let c = s[i];
        let escape: Option<&[u8]> = match c {
            b'"' => Some(b"\\\""),
            b'\\' => Some(b"\\\\"),
            b'\x08' => Some(b"\\b"),
            b'\x0c' => Some(b"\\f"),
            b'\n' => Some(b"\\n"),
            b'\r' => Some(b"\\r"),
            b'\t' => Some(b"\\t"),
            _ => None,
        };
        if let Some(e) = escape {
            out.extend_from_slice(&s[start..i]);
            out.extend_from_slice(e);
            i += 1;
            start = i;
        } else if c < 0x20 || c == b'<' || c == b'>' || c == b'&' {
            out.extend_from_slice(&s[start..i]);
            out.extend_from_slice(b"\\u00");
            out.push(HEX_LOWER[usize::from(c >> 4)]);
            out.push(HEX_LOWER[usize::from(c & 15)]);
            i += 1;
            start = i;
        } else if c == 0xe2 && i + 2 < s.len() && s[i + 1] == 0x80 && (s[i + 2] & 0xfe) == 0xa8 {
            // U+2028 LINE SEPARATOR and U+2029 PARAGRAPH SEPARATOR.
            out.extend_from_slice(&s[start..i]);
            out.extend_from_slice(if s[i + 2] == 0xa8 {
                b"\\u2028"
            } else {
                b"\\u2029"
            });
            i += 3;
            start = i;
        } else {
            i += 1;
        }
    }
    out.extend_from_slice(&s[start..]);
    out.push(b'"');
}

// ---------------------------------------------------------------------------
// Request target
// ---------------------------------------------------------------------------

/// A request target split into raw path and query.
#[derive(Debug, Eq, PartialEq)]
pub struct Target<'a> {
    pub path: &'a [u8],
    pub query: &'a [u8],
}

/// Split a request target. `None` means the request line itself is
/// invalid (generic 400 before any routing).
pub fn split_target(target: &[u8]) -> Option<Target<'_>> {
    if target.iter().any(|&c| c < 0x20 || c == 0x7f) {
        return None;
    }
    let (rest, query) = match target.iter().position(|&c| c == b'?') {
        Some(i) => (&target[..i], &target[i + 1..]),
        None => (target, &target[target.len()..]),
    };
    let path = if rest == b"*" || rest.first() == Some(&b'/') {
        rest
    } else if let Some(after) = scheme_rest(rest) {
        // Absolute form: skip the authority. Without one the URI is opaque
        // and has an empty path, which the sidecar rejects.
        match after.strip_prefix(b"//") {
            Some(authority) => match authority.iter().position(|&c| c == b'/') {
                Some(i) => &authority[i..],
                None => &authority[authority.len()..],
            },
            None if after.first() == Some(&b'/') => after,
            None => &after[after.len()..],
        }
    } else {
        return None;
    };
    // Invalid escapes anywhere in the path are rejected.
    if !valid_escapes(path) {
        return None;
    }
    Some(Target { path, query })
}

/// Return the text after `scheme:` when `s` starts with a URI scheme.
fn scheme_rest(s: &[u8]) -> Option<&[u8]> {
    let first = *s.first()?;
    if !first.is_ascii_alphabetic() {
        return None;
    }
    for (i, &c) in s.iter().enumerate().skip(1) {
        match c {
            b':' => return Some(&s[i + 1..]),
            c if c.is_ascii_alphanumeric() || c == b'+' || c == b'-' || c == b'.' => {}
            _ => return None,
        }
    }
    None
}

fn hex_value(c: u8) -> Option<u8> {
    match c {
        b'0'..=b'9' => Some(c - b'0'),
        b'a'..=b'f' => Some(c - b'a' + 10),
        b'A'..=b'F' => Some(c - b'A' + 10),
        _ => None,
    }
}

fn valid_escapes(s: &[u8]) -> bool {
    let mut i = 0;
    while i < s.len() {
        if s[i] == b'%' {
            if i + 2 >= s.len() {
                return false;
            }
            if hex_value(s[i + 1]).is_none() || hex_value(s[i + 2]).is_none() {
                return false;
            }
            i += 3;
        } else {
            i += 1;
        }
    }
    true
}

/// Percent-decode with optional `+` to space (query mode). Returns `None` on
/// an invalid escape.
pub fn unescape(s: &[u8], plus_space: bool, out: &mut Vec<u8>) -> Option<()> {
    out.clear();
    out.reserve(s.len());
    let mut i = 0;
    while i < s.len() {
        match s[i] {
            b'%' => {
                let hi = hex_value(*s.get(i + 1)?)?;
                let lo = hex_value(*s.get(i + 2)?)?;
                out.push(hi << 4 | lo);
                i += 3;
            }
            b'+' if plus_space => {
                out.push(b' ');
                i += 1;
            }
            c => {
                out.push(c);
                i += 1;
            }
        }
    }
    Some(())
}

/// Bytes a path never needs escaped.
fn path_safe(c: u8) -> bool {
    c.is_ascii_alphanumeric()
        || matches!(
            c,
            b'-' | b'_'
                | b'.'
                | b'~'
                | b'$'
                | b'&'
                | b'+'
                | b','
                | b'/'
                | b':'
                | b';'
                | b'='
                | b'@'
        )
}

/// Whether a raw path is already a valid encoding.
fn valid_encoded(s: &[u8]) -> bool {
    s.iter().all(|&c| {
        matches!(
            c,
            b'!' | b'$'
                | b'&'
                | b'\''
                | b'('
                | b')'
                | b'*'
                | b'+'
                | b','
                | b';'
                | b'='
                | b':'
                | b'@'
                | b'['
                | b']'
                | b'%'
        ) || path_safe(c)
    })
}

/// Split a raw path into decoded bucket and key: split before decoding so an escaped slash is key data,
/// then decode each part exactly once. No dot-segment or slash cleaning.
pub fn bucket_key(raw: &[u8], bucket: &mut Vec<u8>, key: &mut Vec<u8>) -> bool {
    // Use the raw path when it is a valid encoding, otherwise
    // re-encode the decoded path, which turns %2F into a real slash.
    let mut reencoded = Vec::new();
    let escaped: &[u8] = if valid_encoded(raw) {
        raw
    } else {
        let mut decoded = Vec::new();
        if unescape(raw, false, &mut decoded).is_none() {
            return false;
        }
        reencoded.reserve(decoded.len() * 3);
        for &c in &decoded {
            if path_safe(c) {
                reencoded.push(c);
            } else {
                reencoded.extend_from_slice(&[
                    b'%',
                    HEX_UPPER[usize::from(c >> 4)],
                    HEX_UPPER[usize::from(c & 15)],
                ]);
            }
        }
        &reencoded
    };
    let Some(path) = escaped.strip_prefix(b"/") else {
        return false;
    };
    let Some(slash) = path.iter().position(|&c| c == b'/') else {
        return false;
    };
    let (b, k) = (&path[..slash], &path[slash + 1..]);
    if b.is_empty() || k.is_empty() {
        return false;
    }
    let mut part = Vec::new();
    if unescape(b, false, &mut part).is_none() {
        return false;
    }
    std::mem::swap(bucket, &mut part);
    unescape(k, false, key).is_some()
}

// ---------------------------------------------------------------------------
// Query
// ---------------------------------------------------------------------------

/// Parse a raw query (`&`-separated, `;` rejected, `+` is a space). `None`
/// means a parse error.
/// Pairs keep request order; names and values are decoded bytes.
pub fn parse_query(raw: &[u8]) -> Option<Vec<(Vec<u8>, Vec<u8>)>> {
    let mut pairs = Vec::new();
    let mut failed = false;
    for segment in raw.split(|&c| c == b'&') {
        if segment.contains(&b';') {
            failed = true;
            continue;
        }
        if segment.is_empty() {
            continue;
        }
        let (name, value) = match segment.iter().position(|&c| c == b'=') {
            Some(i) => (&segment[..i], &segment[i + 1..]),
            None => (segment, &segment[segment.len()..]),
        };
        let (mut n, mut v) = (Vec::new(), Vec::new());
        if unescape(name, true, &mut n).is_none() || unescape(value, true, &mut v).is_none() {
            failed = true;
            continue;
        }
        pairs.push((n, v));
    }
    (!failed).then_some(pairs)
}

/// Group parsed pairs by name, preserving first-seen order.
fn grouped(pairs: &[(Vec<u8>, Vec<u8>)]) -> Vec<(&[u8], Vec<&[u8]>)> {
    let mut groups: Vec<(&[u8], Vec<&[u8]>)> = Vec::new();
    let mut index: std::collections::HashMap<&[u8], usize> = std::collections::HashMap::new();
    for (name, value) in pairs {
        match index.entry(name.as_slice()) {
            std::collections::hash_map::Entry::Occupied(e) => groups[*e.get()].1.push(value),
            std::collections::hash_map::Entry::Vacant(e) => {
                e.insert(groups.len());
                groups.push((name, vec![value]));
            }
        }
    }
    groups
}

fn has_control(s: &[u8]) -> bool {
    s.iter().any(|&c| c < 0x20 || c == 0x7f)
}

/// S3 query: only `versionId` and a matching `x-id` are allowed.
///
/// When one request mixes a malformed parameter (400) and an unsupported one
/// (501), 400 wins.
pub fn s3_version(raw: &[u8], head: bool) -> Result<Vec<u8>, Failure> {
    if raw.is_empty() {
        return Ok(Vec::new());
    }
    let pairs = parse_query(raw).ok_or(Failure::BadRequest)?;
    let groups = grouped(&pairs);
    if groups.iter().any(|(_, v)| v.len() != 1 || v[0].is_empty()) {
        return Err(Failure::BadRequest);
    }
    let mut version: &[u8] = b"";
    for (name, values) in &groups {
        match *name {
            b"versionId" => version = values[0],
            b"x-id" => {
                let want: &[u8] = if head { b"HeadObject" } else { b"GetObject" };
                if values[0] != want {
                    return Err(Failure::NotImplemented);
                }
            }
            _ => return Err(Failure::NotImplemented),
        }
    }
    if has_control(version) {
        return Err(Failure::BadRequest);
    }
    Ok(version.to_vec())
}

/// Azure query: validate every parameter first so malformed input always
/// wins, then allow only `versionid` and `timeout`.
pub fn azure_version(raw: &[u8]) -> Result<Vec<u8>, Failure> {
    if raw.is_empty() {
        return Ok(Vec::new());
    }
    let pairs = parse_query(raw).ok_or(Failure::BadRequest)?;
    let groups = grouped(&pairs);
    for (name, values) in &groups {
        if name.is_empty()
            || std::str::from_utf8(name).is_err()
            || values.len() != 1
            || values[0].is_empty()
            || std::str::from_utf8(values[0]).is_err()
            || has_control(values[0])
        {
            return Err(Failure::BadRequest);
        }
        if *name == b"timeout" && !matches!(range_number(values[0]), Some(n) if n != 0) {
            return Err(Failure::BadRequest);
        }
    }
    let mut version: &[u8] = b"";
    for (name, values) in &groups {
        match *name {
            b"versionid" => version = values[0],
            b"timeout" => {}
            _ => return Err(Failure::NotImplemented),
        }
    }
    Ok(version.to_vec())
}

// ---------------------------------------------------------------------------
// Headers
// ---------------------------------------------------------------------------

/// Whether a request header name is acceptable for the API. Unknown provider
/// headers may change authorization or read semantics, so they are rejected
/// (even when empty) rather than silently dropped.
pub fn supported_header(api: Api, name: &[u8]) -> bool {
    let lower = |s: &[u8], p: &[u8]| s.len() >= p.len() && s[..p.len()].eq_ignore_ascii_case(p);
    let is = |s: &[u8]| name.eq_ignore_ascii_case(s);
    if is(b"if-modified-since") || is(b"if-unmodified-since") || is(b"if-range") {
        return false;
    }
    match api {
        Api::S3 => {
            if is(b"x-amz-date")
                || is(b"x-amz-content-sha256")
                || is(b"x-amz-security-token")
                || is(b"x-amz-region-set")
                || is(b"x-amz-s3session-token")
                || is(b"x-amz-user-agent")
            {
                return true;
            }
            !lower(name, b"x-amz-")
        }
        Api::Azure => {
            if is(b"x-ms-range")
                || is(b"x-ms-version")
                || is(b"x-ms-date")
                || is(b"x-ms-client-request-id")
                || is(b"x-ms-return-client-request-id")
                || is(b"x-ms-useragent")
            {
                return true;
            }
            !lower(name, b"x-ms-")
        }
    }
}

// ---------------------------------------------------------------------------
// Conditions
// ---------------------------------------------------------------------------

/// Parsed If-Match or If-None-Match.
#[derive(Debug, Default, Eq, PartialEq)]
pub struct Condition {
    pub present: bool,
    pub wildcard: bool,
    /// `(quoted tag, weak)` pairs.
    pub tags: Vec<(Vec<u8>, bool)>,
}

fn trim_set<'a>(mut s: &'a [u8], set: &[u8]) -> &'a [u8] {
    while let [first, rest @ ..] = s {
        if set.contains(first) {
            s = rest;
        } else {
            break;
        }
    }
    while let [rest @ .., last] = s {
        if set.contains(last) {
            s = rest;
        } else {
            break;
        }
    }
    s
}

fn trim_left_set<'a>(mut s: &'a [u8], set: &[u8]) -> &'a [u8] {
    while let [first, rest @ ..] = s {
        if set.contains(first) {
            s = rest;
        } else {
            break;
        }
    }
    s
}

/// Parse all values of one condition header (joined with ","). `None` means
/// malformed (400).
pub fn parse_condition(values: &[&[u8]]) -> Option<Condition> {
    let mut result = Condition {
        present: !values.is_empty(),
        ..Condition::default()
    };
    if values.is_empty() {
        return Some(result);
    }
    let joined = values.join(&b","[..]);
    let s = trim_set(&joined, b" \t");
    if s == b"*" {
        result.wildcard = true;
        return Some(result);
    }
    // Ignore empty list elements, but only recognize a wildcard on its own.
    let mut s = trim_left_set(s, b", \t");
    while !s.is_empty() {
        let weak = s.starts_with(b"W/");
        if weak {
            s = &s[2..];
        }
        s = s.strip_prefix(b"\"")?;
        // Commas inside a quoted tag are literal; backslashes are not escapes.
        let close = s.iter().position(|&c| c == b'"')?;
        let value = &s[..close];
        if value.iter().any(|&c| c < 0x21 || c == 0x7f) {
            return None;
        }
        let mut tag = Vec::with_capacity(value.len() + 2);
        tag.push(b'"');
        tag.extend_from_slice(value);
        tag.push(b'"');
        result.tags.push((tag, weak));
        s = trim_left_set(&s[close + 1..], b" \t");
        if !s.is_empty() && s[0] != b',' {
            return None;
        }
        s = trim_left_set(s, b", \t");
    }
    (!result.tags.is_empty()).then_some(result)
}

impl Condition {
    /// Strong comparison (If-Match) never matches weak candidates; weak
    /// comparison (If-None-Match) ignores weakness.
    pub fn matches(&self, etag: &[u8], weak: bool) -> bool {
        self.wildcard
            || self
                .tags
                .iter()
                .any(|(tag, w)| tag.as_slice() == etag && (weak || !w))
    }
}

// ---------------------------------------------------------------------------
// Range
// ---------------------------------------------------------------------------

/// A syntactically valid single byte range. Positions above `i64::MAX` are
/// clamped to it: object sizes are int64, so they select the same bytes.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RangeSpec {
    /// No range header: the whole object.
    Full,
    Suffix(u64),
    From(u64),
    Closed(u64, u64),
}

/// Decimal digits only (no signs or whitespace); values past `u64::MAX` are
/// invalid, values past `i64::MAX` clamp to it.
pub fn range_number(s: &[u8]) -> Option<u64> {
    if s.is_empty() || !s.iter().all(u8::is_ascii_digit) {
        return None;
    }
    let mut n: u64 = 0;
    for &c in s {
        n = n.checked_mul(10)?.checked_add(u64::from(c - b'0'))?;
    }
    Some(n.min(i64::MAX as u64))
}

/// Parse the selected range header values. `Err` means 400.
pub fn parse_range(values: &[&[u8]]) -> Result<RangeSpec, Failure> {
    let value = match values {
        [] => return Ok(RangeSpec::Full),
        [value] => *value,
        _ => return Err(Failure::BadRequest),
    };
    let s = trim_set(value, b" \t")
        .strip_prefix(b"bytes=")
        .ok_or(Failure::BadRequest)?;
    let dash = s
        .iter()
        .position(|&c| c == b'-')
        .ok_or(Failure::BadRequest)?;
    let (first, last) = (&s[..dash], &s[dash + 1..]);
    if first.is_empty() && last.is_empty() {
        return Err(Failure::BadRequest);
    }
    if first.is_empty() {
        return Ok(RangeSpec::Suffix(
            range_number(last).ok_or(Failure::BadRequest)?,
        ));
    }
    let start = range_number(first).ok_or(Failure::BadRequest)?;
    if last.is_empty() {
        return Ok(RangeSpec::From(start));
    }
    let end = range_number(last).ok_or(Failure::BadRequest)?;
    if end < start {
        return Err(Failure::BadRequest);
    }
    Ok(RangeSpec::Closed(start, end))
}

/// A resolved selection: `[offset, offset + length)` of an object of `size`.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Selection {
    pub offset: u64,
    pub length: u64,
    pub size: u64,
    pub partial: bool,
}

impl RangeSpec {
    /// Resolve against an object size (always <= `i64::MAX`).
    pub fn resolve(self, size: u64) -> Result<Selection, Failure> {
        let unsatisfiable = Failure::RangeNotSatisfiable(Some(size));
        let (offset, length) = match self {
            Self::Full => {
                return Ok(Selection {
                    offset: 0,
                    length: size,
                    size,
                    partial: false,
                });
            }
            Self::Suffix(n) => {
                if n == 0 || size == 0 {
                    return Err(unsatisfiable);
                }
                let length = n.min(size);
                (size - length, length)
            }
            Self::From(start) | Self::Closed(start, _) => {
                if start >= size {
                    return Err(unsatisfiable);
                }
                let end = match self {
                    Self::Closed(_, end) if end < size => end,
                    _ => size - 1,
                };
                (start, end - start + 1)
            }
        };
        Ok(Selection {
            offset,
            length,
            size,
            partial: true,
        })
    }

    /// Canonical Racer `Range` value, or `None` for the whole object.
    pub fn racer_header(self, out: &mut Vec<u8>) -> bool {
        let mut buf = itoa_buf();
        match self {
            Self::Full => return false,
            Self::Suffix(n) => {
                out.extend_from_slice(b"bytes=-");
                out.extend_from_slice(fmt_u64(n, &mut buf));
            }
            Self::From(a) => {
                out.extend_from_slice(b"bytes=");
                out.extend_from_slice(fmt_u64(a, &mut buf));
                out.push(b'-');
            }
            Self::Closed(a, b) => {
                out.extend_from_slice(b"bytes=");
                out.extend_from_slice(fmt_u64(a, &mut buf));
                out.push(b'-');
                out.extend_from_slice(fmt_u64(b, &mut buf));
            }
        }
        true
    }
}

/// Scratch space for `fmt_u64`.
pub fn itoa_buf() -> [u8; 20] {
    [0; 20]
}

/// Format a decimal without allocating.
pub fn fmt_u64(mut n: u64, buf: &mut [u8; 20]) -> &[u8] {
    let mut i = buf.len();
    loop {
        i -= 1;
        buf[i] = b'0' + (n % 10) as u8;
        n /= 10;
        if n == 0 {
            break;
        }
    }
    &buf[i..]
}

#[cfg(test)]
mod tests;
