//! Prefetch ticket routes on the client socket: `/v1/prefetches[/{id}]`.
//!
//! This module validates the request line, query string, and headers. It does
//! not parse the JSON body; the application reads exactly `content_length`
//! bytes after a successful [`PrefetchRequest::Put`] and decodes them itself.
//! Every rejection carries a fixed error `code` and an HTTP status, and is sent
//! with a JSON body (unlike object read errors, which have empty bodies).

use crate::http::{Header, MessageHead, StartLine, is_token, trim_ows};
use crate::object::model::MAX_FIELD_BYTES;
use std::time::Duration;

/// Collection path. Item paths are `PREFETCH_PATH/{id}`.
pub const PREFETCH_PATH: &str = "/v1/prefetches";

/// Longest client-chosen ticket id.
pub const MAX_ID_BYTES: usize = 128;

/// Longest opaque refs cursor accepted in a query string.
pub const MAX_CURSOR_BYTES: usize = 256;

/// Whether a request target selects the prefetch route rather than objects.
pub fn is_prefetch_target(target: &str) -> bool {
    let path = target.split_once('?').map_or(target, |(path, _)| path);
    path == PREFETCH_PATH
        || path
            .strip_prefix(PREFETCH_PATH)
            .is_some_and(|rest| rest.starts_with('/'))
}

/// Parser bounds that come from node configuration.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Limits {
    /// Largest accepted `Content-Length` of a PUT.
    pub max_body_bytes: u64,
    /// Largest accepted `?wait=`.
    pub max_wait: Duration,
}

impl Default for Limits {
    fn default() -> Self {
        Self {
            max_body_bytes: 4 * 1024 * 1024,
            max_wait: Duration::from_secs(60),
        }
    }
}

/// Which refs a status response lists.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RefsFilter {
    Failed,
    All,
}

/// Validated `GET /v1/prefetches/{id}` query.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct StatusQuery {
    pub wait: Option<Duration>,
    pub coverage: bool,
    pub refs: Option<RefsFilter>,
    pub cursor: Option<String>,
}

/// A validated prefetch request head. Deliberately not Debug: it borrows the
/// opaque `Authorization` value.
pub enum PrefetchRequest<'a> {
    /// Create or update. The body has exactly `content_length` bytes.
    Put {
        id: String,
        content_length: u64,
        authorization: Option<&'a [u8]>,
    },
    Get {
        id: String,
        query: StatusQuery,
    },
    Delete {
        id: String,
    },
    List,
}

impl PrefetchRequest<'_> {
    /// The ticket id, absent for the collection.
    pub fn id(&self) -> Option<&str> {
        match self {
            Self::Put { id, .. } | Self::Get { id, .. } | Self::Delete { id } => Some(id),
            Self::List => None,
        }
    }
}

/// Fixed error codes of the prefetch API.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ErrorCode {
    InvalidRequest,
    NotFound,
    MethodNotAllowed,
    CacheUidMismatch,
    PayloadMismatch,
    StaleSequence,
    StaleGeneration,
    Passthrough,
    TooLarge,
    TicketLimit,
    PendingLimit,
    Unavailable,
}

impl ErrorCode {
    /// The `code` string sent on the wire.
    pub fn as_str(self) -> &'static str {
        match self {
            Self::InvalidRequest => "invalid_request",
            Self::NotFound => "not_found",
            Self::MethodNotAllowed => "method_not_allowed",
            Self::CacheUidMismatch => "cache_uid_mismatch",
            Self::PayloadMismatch => "payload_mismatch",
            Self::StaleSequence => "stale_sequence",
            Self::StaleGeneration => "stale_generation",
            Self::Passthrough => "passthrough",
            Self::TooLarge => "too_large",
            Self::TicketLimit => "ticket_limit",
            Self::PendingLimit => "pending_limit",
            Self::Unavailable => "unavailable",
        }
    }

    /// The HTTP status for this code.
    pub fn status(self) -> u16 {
        match self {
            Self::InvalidRequest => 400,
            Self::NotFound => 404,
            Self::MethodNotAllowed => 405,
            Self::CacheUidMismatch
            | Self::PayloadMismatch
            | Self::StaleSequence
            | Self::StaleGeneration
            | Self::Passthrough => 409,
            Self::TooLarge => 413,
            Self::TicketLimit | Self::PendingLimit => 429,
            Self::Unavailable => 503,
        }
    }
}

/// A prefetch rejection with its JSON body fields and response headers.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct PrefetchError {
    pub code: ErrorCode,
    pub message: String,
    /// `Allow` value for 405.
    pub allow: Option<&'static str>,
    /// `Retry-After` seconds for 429, 1 to 60.
    pub retry_after: Option<u64>,
}

impl PrefetchError {
    pub fn new(code: ErrorCode, message: impl Into<String>) -> Self {
        Self {
            code,
            message: message.into(),
            allow: None,
            retry_after: None,
        }
    }

    pub fn invalid(message: impl Into<String>) -> Self {
        Self::new(ErrorCode::InvalidRequest, message)
    }

    fn method(allow: &'static str) -> Self {
        Self {
            allow: Some(allow),
            ..Self::new(ErrorCode::MethodNotAllowed, "method not allowed")
        }
    }

    /// Add a `Retry-After`, clamped to 1..=60 seconds.
    pub fn with_retry_after(mut self, seconds: u64) -> Self {
        self.retry_after = Some(seconds.clamp(1, 60));
        self
    }

    /// The JSON body `{"code": ..., "message": ...}`.
    pub fn body(&self) -> Vec<u8> {
        let mut body = String::from("{\"code\":\"");
        body.push_str(self.code.as_str());
        body.push_str("\",\"message\":");
        push_json_string(&mut body, &self.message);
        body.push('}');
        body.into_bytes()
    }

    /// The complete response head for [`Self::body`].
    pub fn head(&self) -> MessageHead {
        let mut head = json_head(self.code.status(), self.body().len());
        if let Some(allow) = self.allow {
            head.headers.push(Header::new("Allow", allow));
        }
        if let Some(seconds) = self.retry_after {
            head.headers
                .push(Header::new("Retry-After", seconds.to_string()));
        }
        head
    }
}

impl std::fmt::Display for PrefetchError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}: {}", self.code.as_str(), self.message)
    }
}

/// Append `value` as a JSON string literal.
fn push_json_string(out: &mut String, value: &str) {
    out.push('"');
    for c in value.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            c if (c as u32) < 0x20 || c == '\u{7f}' => {
                out.push_str(&format!("\\u{:04x}", c as u32));
            }
            c => out.push(c),
        }
    }
    out.push('"');
}

/// A JSON response head with a fixed body length. The connection closes after
/// the response so a body is never left unread on a reused connection.
pub fn json_head(status: u16, length: usize) -> MessageHead {
    MessageHead {
        start: StartLine::Response { status },
        headers: vec![
            Header::new("Content-Type", "application/json"),
            Header::new("Content-Length", length.to_string()),
            Header::new("Connection", "close"),
        ],
    }
}

/// Validate a ticket id: `[A-Za-z0-9._-]{1,128}`, not `.` or `..`.
pub fn valid_id(id: &str) -> bool {
    !id.is_empty()
        && id.len() <= MAX_ID_BYTES
        && id != "."
        && id != ".."
        && id
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'_' | b'-'))
}

/// Parse a prefetch request head. The target must satisfy [`is_prefetch_target`].
pub fn parse<'a>(
    head: &'a MessageHead,
    limits: &Limits,
    header_limit: usize,
) -> Result<PrefetchRequest<'a>, PrefetchError> {
    let StartLine::Request { method, target } = &head.start else {
        return Err(PrefetchError::invalid("not a request"));
    };
    let (path, query) = match target.split_once('?') {
        Some((path, query)) => (path, Some(query)),
        None => (target.as_str(), None),
    };
    let id = match path.strip_prefix(PREFETCH_PATH) {
        Some("") => None,
        Some(rest) => Some(
            rest.strip_prefix('/')
                .ok_or_else(|| PrefetchError::invalid("unknown path"))?,
        ),
        None => return Err(PrefetchError::invalid("unknown path")),
    };
    // Method first: an unsupported method is 405 regardless of other faults.
    match (id.is_some(), method.as_str()) {
        (true, "PUT" | "GET" | "DELETE") | (false, "GET") => {}
        (true, _) => return Err(PrefetchError::method("GET, PUT, DELETE")),
        (false, _) => return Err(PrefetchError::method("GET")),
    }
    if let Some(id) = id
        && !valid_id(id)
    {
        return Err(PrefetchError::invalid("invalid prefetch id"));
    }
    let is_put = method == "PUT";
    let mut decoded = method.len().saturating_add(target.len());
    let mut host = None;
    let mut content_length = None;
    let mut content_type = None;
    let mut authorization = None;
    let mut seen = Vec::new();
    for header in &head.headers {
        decoded = decoded
            .saturating_add(header.name.len())
            .saturating_add(header.value.len());
        if decoded > header_limit {
            return Err(PrefetchError::new(
                ErrorCode::TooLarge,
                "request head too large",
            ));
        }
        if header.name.is_empty()
            || !header.name.bytes().all(is_token)
            || header
                .value
                .iter()
                .any(|&b| b == 0x7f || (b < 0x20 && b != b'\t'))
        {
            return Err(PrefetchError::invalid("invalid header"));
        }
        let name = header.name.to_ascii_lowercase();
        let value = header.value.as_slice();
        if matches!(
            name.as_str(),
            "host" | "content-length" | "content-type" | "authorization"
        ) {
            if seen.contains(&name) {
                return Err(PrefetchError::invalid(format!("duplicate {name}")));
            }
            seen.push(name.clone());
        }
        match name.as_str() {
            "host" => host = Some(trim_ows(value) == b"racer"),
            "content-length" => {
                content_length = Some(
                    canonical_decimal(trim_ows(value))
                        .ok_or_else(|| PrefetchError::invalid("invalid content-length"))?,
                );
            }
            "content-type" => content_type = Some(value),
            "transfer-encoding" | "expect" | "content-encoding" | "trailer" | "upgrade" => {
                return Err(PrefetchError::invalid(format!("{name} is not supported")));
            }
            "connection"
                if value
                    .split(|&b| b == b',')
                    .any(|token| trim_ows(token).eq_ignore_ascii_case(b"upgrade")) =>
            {
                return Err(PrefetchError::invalid("upgrade is not supported"));
            }
            "authorization" if is_put => {
                if value.len() > MAX_FIELD_BYTES {
                    return Err(PrefetchError::new(
                        ErrorCode::TooLarge,
                        "authorization too large",
                    ));
                }
                if value.is_empty()
                    || value.first() == Some(&b' ')
                    || value.last() == Some(&b' ')
                    || value.iter().any(|&b| b < 0x20 || b == 0x7f)
                {
                    return Err(PrefetchError::invalid("invalid authorization"));
                }
                authorization = Some(value);
            }
            "authorization" => {
                return Err(PrefetchError::invalid(
                    "authorization is accepted on PUT only",
                ));
            }
            _ => {}
        }
    }
    if host != Some(true) {
        return Err(PrefetchError::invalid("host must be racer"));
    }
    if !is_put && query.is_some() && method != "GET" {
        return Err(PrefetchError::invalid("unexpected query string"));
    }
    if is_put {
        if query.is_some() {
            return Err(PrefetchError::invalid("unexpected query string"));
        }
        let length = content_length
            .filter(|&length| length != 0)
            .ok_or_else(|| PrefetchError::invalid("a JSON body is required"))?;
        if length > limits.max_body_bytes {
            return Err(PrefetchError::new(
                ErrorCode::TooLarge,
                format!(
                    "body of {length} bytes exceeds {} bytes",
                    limits.max_body_bytes
                ),
            ));
        }
        let content_type =
            content_type.ok_or_else(|| PrefetchError::invalid("content-type is required"))?;
        let essence = trim_ows(
            content_type
                .split(|&b| b == b';')
                .next()
                .unwrap_or_default(),
        );
        if !essence.eq_ignore_ascii_case(b"application/json") {
            return Err(PrefetchError::invalid(
                "content-type must be application/json",
            ));
        }
        return Ok(PrefetchRequest::Put {
            id: id.unwrap_or_default().to_owned(),
            content_length: length,
            authorization,
        });
    }
    if content_length.is_some_and(|length| length != 0) {
        return Err(PrefetchError::invalid("request body is not allowed"));
    }
    match (method.as_str(), id) {
        ("GET", Some(id)) => Ok(PrefetchRequest::Get {
            id: id.to_owned(),
            query: parse_query(query.unwrap_or_default(), limits)?,
        }),
        ("DELETE", Some(id)) => Ok(PrefetchRequest::Delete { id: id.to_owned() }),
        ("GET", None) => {
            if query.is_some_and(|q| !q.is_empty()) {
                return Err(PrefetchError::invalid("unexpected query string"));
            }
            Ok(PrefetchRequest::List)
        }
        _ => Err(PrefetchError::method("GET")),
    }
}

/// Decimal without sign or leading zeros (except `0`).
fn canonical_decimal(value: &[u8]) -> Option<u64> {
    if value.is_empty()
        || !value.iter().all(u8::is_ascii_digit)
        || (value.len() > 1 && value[0] == b'0')
    {
        return None;
    }
    value.iter().try_fold(0u64, |n, digit| {
        n.checked_mul(10)?.checked_add(u64::from(digit - b'0'))
    })
}

/// Parse `application/x-www-form-urlencoded` status parameters.
pub fn parse_query(query: &str, limits: &Limits) -> Result<StatusQuery, PrefetchError> {
    let mut parsed = StatusQuery::default();
    let mut seen = Vec::new();
    if query.is_empty() {
        return Ok(parsed);
    }
    for pair in query.split('&') {
        let (name, value) = pair.split_once('=').unwrap_or((pair, ""));
        let name = percent_decode(name)?;
        let value = percent_decode(value)?;
        if seen.contains(&name) {
            return Err(PrefetchError::invalid(format!(
                "duplicate parameter {name}"
            )));
        }
        match name.as_str() {
            "wait" => {
                let wait = parse_duration(&value)
                    .ok_or_else(|| PrefetchError::invalid("invalid wait duration"))?;
                if wait > limits.max_wait {
                    return Err(PrefetchError::invalid(format!(
                        "wait exceeds {}s",
                        limits.max_wait.as_secs()
                    )));
                }
                parsed.wait = Some(wait);
            }
            "coverage" => {
                parsed.coverage = match value.as_str() {
                    "1" | "true" => true,
                    "0" | "false" => false,
                    _ => return Err(PrefetchError::invalid("invalid coverage")),
                };
            }
            "refs" => {
                parsed.refs = Some(match value.as_str() {
                    "failed" => RefsFilter::Failed,
                    "all" => RefsFilter::All,
                    _ => return Err(PrefetchError::invalid("refs must be failed or all")),
                });
            }
            "cursor" => {
                if value.is_empty()
                    || value.len() > MAX_CURSOR_BYTES
                    || !value
                        .bytes()
                        .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'-' | b'_' | b'.'))
                {
                    return Err(PrefetchError::invalid("invalid cursor"));
                }
                parsed.cursor = Some(value);
            }
            _ => return Err(PrefetchError::invalid(format!("unknown parameter {name}"))),
        }
        seen.push(name);
    }
    if parsed.cursor.is_some() && parsed.refs.is_none() {
        return Err(PrefetchError::invalid("cursor requires refs"));
    }
    Ok(parsed)
}

/// Percent-decode one form component; `+` is a space. Result must be UTF-8.
fn percent_decode(value: &str) -> Result<String, PrefetchError> {
    let bytes = value.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut i = 0;
    while i < bytes.len() {
        match bytes[i] {
            b'%' => {
                let hex = bytes
                    .get(i + 1..i + 3)
                    .filter(|h| h.iter().all(u8::is_ascii_hexdigit))
                    .and_then(|h| std::str::from_utf8(h).ok())
                    .and_then(|h| u8::from_str_radix(h, 16).ok())
                    .ok_or_else(|| PrefetchError::invalid("invalid percent-encoding"))?;
                out.push(hex);
                i += 3;
            }
            b'+' => {
                out.push(b' ');
                i += 1;
            }
            b => {
                out.push(b);
                i += 1;
            }
        }
    }
    String::from_utf8(out).map_err(|_| PrefetchError::invalid("query is not UTF-8"))
}

/// Parse a Go-style duration such as `500ms`, `30s`, `1m30s`, or `0`.
/// Signs are rejected. Fractions are accepted (`1.5s`).
pub fn parse_duration(value: &str) -> Option<Duration> {
    if value == "0" {
        return Some(Duration::ZERO);
    }
    if value.is_empty() {
        return None;
    }
    let mut rest = value;
    let mut total: u128 = 0;
    while !rest.is_empty() {
        let digits = rest
            .find(|c: char| !(c.is_ascii_digit() || c == '.'))
            .filter(|&n| n > 0)?;
        let (number, tail) = rest.split_at(digits);
        let unit_end = tail
            .find(|c: char| c.is_ascii_digit() || c == '.')
            .unwrap_or(tail.len());
        let (unit, tail) = tail.split_at(unit_end);
        let nanos_per: u128 = match unit {
            "ns" => 1,
            "us" | "\u{b5}s" | "\u{3bc}s" => 1_000,
            "ms" => 1_000_000,
            "s" => 1_000_000_000,
            "m" => 60_000_000_000,
            "h" => 3_600_000_000_000,
            _ => return None,
        };
        let (whole, fraction) = match number.split_once('.') {
            Some((whole, fraction)) => (whole, fraction),
            None => (number, ""),
        };
        if (whole.is_empty() && fraction.is_empty()) || fraction.contains('.') {
            return None;
        }
        let whole: u128 = if whole.is_empty() {
            0
        } else {
            whole.parse().ok()?
        };
        let mut nanos = whole.checked_mul(nanos_per)?;
        let mut scale = nanos_per;
        for digit in fraction.bytes().take(18) {
            scale /= 10;
            nanos = nanos.checked_add(u128::from(digit - b'0') * scale)?;
        }
        total = total.checked_add(nanos)?;
        rest = tail;
    }
    let nanos = u64::try_from(total).ok()?;
    Some(Duration::from_nanos(nanos))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn head(method: &str, target: &str, headers: &[(&str, &str)]) -> MessageHead {
        MessageHead {
            start: StartLine::Request {
                method: method.into(),
                target: target.into(),
            },
            headers: headers
                .iter()
                .map(|(name, value)| Header::new(*name, *value))
                .collect(),
        }
    }

    fn put_headers(length: &str) -> Vec<(&'static str, String)> {
        vec![
            ("Host", "racer".into()),
            ("Content-Type", "application/json; charset=utf-8".into()),
            ("Content-Length", length.into()),
        ]
    }

    fn run(method: &str, target: &str, headers: &[(&str, &str)]) -> Result<String, PrefetchError> {
        let head = head(method, target, headers);
        parse(&head, &Limits::default(), 32 * 1024).map(|request| match request {
            PrefetchRequest::Put {
                id,
                content_length,
                authorization,
            } => format!(
                "put {id} {content_length} {}",
                authorization.is_some_and(|a| a == b"Bearer x")
            ),
            PrefetchRequest::Get { id, query } => format!("get {id} {query:?}"),
            PrefetchRequest::Delete { id } => format!("delete {id}"),
            PrefetchRequest::List => "list".into(),
        })
    }

    const HOST: (&str, &str) = ("Host", "racer");

    #[test]
    fn targets_select_the_prefetch_route() {
        assert!(is_prefetch_target("/v1/prefetches"));
        assert!(is_prefetch_target("/v1/prefetches?x"));
        assert!(is_prefetch_target("/v1/prefetches/a"));
        assert!(!is_prefetch_target("/v1/prefetchesx"));
        assert!(!is_prefetch_target("/v1/objects/abc"));
    }

    #[test]
    fn methods_and_allow_headers() {
        assert_eq!(run("GET", "/v1/prefetches", &[HOST]).unwrap(), "list");
        assert_eq!(
            run("DELETE", "/v1/prefetches/a", &[HOST]).unwrap(),
            "delete a"
        );
        for method in ["POST", "HEAD", "PATCH"] {
            let error = run(method, "/v1/prefetches/a", &[HOST]).unwrap_err();
            assert_eq!(error.code.status(), 405);
            assert_eq!(error.allow, Some("GET, PUT, DELETE"));
            let head = error.head();
            assert!(
                head.headers
                    .iter()
                    .any(|h| h.name == "Allow" && h.value == b"GET, PUT, DELETE")
            );
        }
        for method in ["PUT", "DELETE", "POST"] {
            let error = run(method, "/v1/prefetches", &[HOST]).unwrap_err();
            assert_eq!(error.code.status(), 405);
            assert_eq!(error.allow, Some("GET"));
        }
    }

    #[test]
    fn ids_are_strict() {
        let long = "a".repeat(128);
        assert!(run("GET", &format!("/v1/prefetches/{long}"), &[HOST]).is_ok());
        assert!(run("GET", "/v1/prefetches/A-z_0.9", &[HOST]).is_ok());
        let too_long = "a".repeat(129);
        for id in [
            ".",
            "..",
            "",
            "a/b",
            "a%20b",
            "a b",
            too_long.as_str(),
            "\u{e9}",
        ] {
            let error = run("GET", &format!("/v1/prefetches/{id}"), &[HOST]).unwrap_err();
            assert_eq!(error.code, ErrorCode::InvalidRequest, "{id}");
        }
        assert!(run("GET", "/v1/prefetches/...", &[HOST]).is_ok());
    }

    #[test]
    fn put_requires_json_body_within_limits() {
        let headers = put_headers("10");
        let refs = headers
            .iter()
            .map(|(n, v)| (*n, v.as_str()))
            .collect::<Vec<_>>();
        assert_eq!(
            run("PUT", "/v1/prefetches/a", &refs).unwrap(),
            "put a 10 false"
        );
        let mut with_auth = refs.clone();
        with_auth.push(("Authorization", "Bearer x"));
        assert_eq!(
            run("PUT", "/v1/prefetches/a", &with_auth).unwrap(),
            "put a 10 true"
        );
        // Missing body, missing content type, wrong content type.
        for headers in [
            vec![HOST, ("Content-Type", "application/json")],
            vec![
                HOST,
                ("Content-Type", "application/json"),
                ("Content-Length", "0"),
            ],
            vec![HOST, ("Content-Length", "5")],
            vec![
                HOST,
                ("Content-Type", "text/plain"),
                ("Content-Length", "5"),
            ],
            vec![
                HOST,
                ("Content-Type", "application/json"),
                ("Content-Length", "05"),
            ],
            vec![
                HOST,
                ("Content-Type", "application/json"),
                ("Content-Length", "5"),
                ("Transfer-Encoding", "chunked"),
            ],
            vec![
                HOST,
                ("Content-Type", "application/json"),
                ("Content-Length", "5"),
                ("Expect", "100-continue"),
            ],
            vec![
                HOST,
                ("Content-Type", "application/json"),
                ("Content-Length", "5"),
                ("Content-Encoding", "gzip"),
            ],
            vec![
                ("Host", "other"),
                ("Content-Type", "application/json"),
                ("Content-Length", "5"),
            ],
        ] {
            let error = run("PUT", "/v1/prefetches/a", &headers).unwrap_err();
            assert_eq!(error.code, ErrorCode::InvalidRequest, "{headers:?}");
        }
        let error = run(
            "PUT",
            "/v1/prefetches/a",
            &[
                HOST,
                ("Content-Type", "application/json"),
                ("Content-Length", "4194305"),
            ],
        )
        .unwrap_err();
        assert_eq!(error.code, ErrorCode::TooLarge);
        assert_eq!(error.code.status(), 413);
    }

    #[test]
    fn get_and_delete_reject_bodies_and_authorization() {
        assert!(run("GET", "/v1/prefetches/a", &[HOST, ("Content-Length", "0")]).is_ok());
        for method in ["GET", "DELETE"] {
            assert_eq!(
                run(method, "/v1/prefetches/a", &[HOST, ("Content-Length", "3")])
                    .unwrap_err()
                    .code,
                ErrorCode::InvalidRequest
            );
            assert_eq!(
                run(
                    method,
                    "/v1/prefetches/a",
                    &[HOST, ("Authorization", "Bearer x")]
                )
                .unwrap_err()
                .code,
                ErrorCode::InvalidRequest
            );
        }
        assert!(run("DELETE", "/v1/prefetches/a?wait=1s", &[HOST]).is_err());
        assert!(run("GET", "/v1/prefetches?wait=1s", &[HOST]).is_err());
    }

    #[test]
    fn query_parameters() {
        let limits = Limits::default();
        let query = parse_query("wait=0m30s&coverage=1&refs=failed&cursor=abc", &limits).unwrap();
        assert_eq!(query.wait, Some(Duration::from_secs(30)));
        assert!(query.coverage);
        assert_eq!(query.refs, Some(RefsFilter::Failed));
        assert_eq!(query.cursor.as_deref(), Some("abc"));
        let query = parse_query("refs=%61ll&wait=500ms", &limits).unwrap();
        assert_eq!(query.refs, Some(RefsFilter::All));
        assert_eq!(query.wait, Some(Duration::from_millis(500)));
        assert_eq!(parse_query("", &limits).unwrap(), StatusQuery::default());
        for bad in [
            "wait=1s&wait=2s",
            "unknown=1",
            "wait=61s",
            "wait=-1s",
            "wait=1",
            "wait=",
            "coverage=yes",
            "refs=some",
            "cursor=abc",
            "refs=all&cursor=a%2Fb",
            "refs=%zz",
        ] {
            assert!(parse_query(bad, &limits).is_err(), "{bad}");
        }
    }

    #[test]
    fn percent_decode_requires_two_hex_digits() {
        assert_eq!(percent_decode("a%2Fb+c").unwrap(), "a/b c");
        assert_eq!(percent_decode("%61%6C").unwrap(), "al");
        for bad in ["%+f", "%-1", "%f", "%", "%g0", "% f"] {
            assert!(percent_decode(bad).is_err(), "{bad}");
        }
    }

    #[test]
    fn go_durations() {
        assert_eq!(parse_duration("0"), Some(Duration::ZERO));
        assert_eq!(parse_duration("0s"), Some(Duration::ZERO));
        assert_eq!(parse_duration("2m"), Some(Duration::from_secs(120)));
        assert_eq!(parse_duration("1.5s"), Some(Duration::from_millis(1500)));
        assert_eq!(parse_duration("1h2m3s"), Some(Duration::from_secs(3723)));
        assert_eq!(parse_duration("10us"), Some(Duration::from_micros(10)));
        for bad in ["", "s", "1", "1x", "+1s", "1..2s", ".s"] {
            assert_eq!(parse_duration(bad), None, "{bad}");
        }
    }

    #[test]
    fn error_bodies_are_json() {
        let error = PrefetchError::new(ErrorCode::StaleSequence, "a \"quoted\"\nline")
            .with_retry_after(120);
        assert_eq!(
            error.body(),
            br#"{"code":"stale_sequence","message":"a \"quoted\"\u000aline"}"#
        );
        assert_eq!(error.retry_after, Some(60));
        let head = error.head();
        assert!(matches!(head.start, StartLine::Response { status: 409 }));
        assert!(
            head.headers
                .iter()
                .any(|h| h.name == "Content-Type" && h.value == b"application/json")
        );
    }
}
