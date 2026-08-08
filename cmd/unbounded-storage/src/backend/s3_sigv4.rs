// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! AWS Signature Version 4 signing for bodyless S3 origin requests.
//!
//! The request URI is already the URI-compatible origin-form wire target.
//! Signing canonicalizes from it without rewriting it, so serialization sends
//! the same escapes, slash layout, dot segments, and query ordering it received.

use std::fmt;
use std::time::{SystemTime, UNIX_EPOCH};

use ::http::header::{AUTHORIZATION, HOST, HeaderName, HeaderValue, RANGE};
use hmac::{Hmac, Mac};
use sha2::{Digest, Sha256};
use time::OffsetDateTime;

const ALGORITHM: &str = "AWS4-HMAC-SHA256";
const EMPTY_PAYLOAD_HASH: &str = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855";
const SERVICE: &str = "s3";
const TERMINATOR: &str = "aws4_request";
const X_AMZ_CONTENT_SHA256: HeaderName = HeaderName::from_static("x-amz-content-sha256");
const X_AMZ_DATE: HeaderName = HeaderName::from_static("x-amz-date");
const X_AMZ_SECURITY_TOKEN: HeaderName = HeaderName::from_static("x-amz-security-token");

type HmacSha256 = Hmac<Sha256>;

/// Immutable static credentials and signing region for an S3 backend.
#[derive(Clone, Eq, PartialEq)]
pub(crate) struct S3Auth {
    access_key_id: String,
    secret_access_key: String,
    region: String,
    session_token: Option<String>,
}

impl S3Auth {
    #[allow(dead_code)]
    pub(crate) fn new(
        access_key_id: impl Into<String>,
        secret_access_key: impl Into<String>,
        region: impl Into<String>,
        session_token: Option<String>,
    ) -> Self {
        Self {
            access_key_id: access_key_id.into(),
            secret_access_key: secret_access_key.into(),
            region: region.into(),
            session_token,
        }
    }
}

#[derive(Debug)]
pub(crate) enum SigningError {
    MissingHost,
    InvalidHeader(String),
    TimeBeforeUnixEpoch,
    TimeOutOfRange,
}

impl fmt::Display for SigningError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::MissingHost => write!(f, "S3 SigV4 request is missing Host"),
            Self::InvalidHeader(name) => write!(f, "invalid S3 SigV4 {name} header value"),
            Self::TimeBeforeUnixEpoch => write!(f, "S3 SigV4 time is before the Unix epoch"),
            Self::TimeOutOfRange => write!(f, "S3 SigV4 time is out of range"),
        }
    }
}

impl std::error::Error for SigningError {}

/// Sign `request` in place. With no auth, leave the request untouched.
pub(crate) fn sign_request(
    request: &mut ::http::Request<()>,
    auth: Option<&S3Auth>,
    now: SystemTime,
) -> Result<(), SigningError> {
    let Some(auth) = auth else {
        return Ok(());
    };

    let (date, amz_date) = format_time(now)?;
    insert_header(request, X_AMZ_DATE, &amz_date)?;
    insert_header(request, X_AMZ_CONTENT_SHA256, EMPTY_PAYLOAD_HASH)?;
    if let Some(token) = &auth.session_token {
        insert_header(request, X_AMZ_SECURITY_TOKEN, token)?;
    }

    let (canonical_headers, signed_headers) = canonical_headers(request)?;
    let canonical_request = format!(
        "{}\n{}\n{}\n{}\n{}\n{}",
        request.method().as_str(),
        canonical_uri(request.uri().path()),
        canonical_query(request.uri().query()),
        canonical_headers,
        signed_headers,
        EMPTY_PAYLOAD_HASH,
    );
    let scope = format!("{date}/{}/{SERVICE}/{TERMINATOR}", auth.region);
    let string_to_sign = format!(
        "{ALGORITHM}\n{amz_date}\n{scope}\n{}",
        sha256_hex(canonical_request.as_bytes())
    );
    let signature = signature(auth, &date, string_to_sign.as_bytes());
    let authorization = format!(
        "{ALGORITHM} Credential={}/{scope}, SignedHeaders={signed_headers}, Signature={signature}",
        auth.access_key_id
    );
    insert_header(request, AUTHORIZATION, &authorization)
}

fn format_time(now: SystemTime) -> Result<(String, String), SigningError> {
    let seconds = now
        .duration_since(UNIX_EPOCH)
        .map_err(|_| SigningError::TimeBeforeUnixEpoch)?
        .as_secs();
    let seconds = i64::try_from(seconds).map_err(|_| SigningError::TimeOutOfRange)?;
    let datetime =
        OffsetDateTime::from_unix_timestamp(seconds).map_err(|_| SigningError::TimeOutOfRange)?;
    if !(0..=9999).contains(&datetime.year()) {
        return Err(SigningError::TimeOutOfRange);
    }

    let date = format!(
        "{:04}{:02}{:02}",
        datetime.year(),
        u8::from(datetime.month()),
        datetime.day()
    );
    let amz_date = format!(
        "{date}T{:02}{:02}{:02}Z",
        datetime.hour(),
        datetime.minute(),
        datetime.second()
    );
    Ok((date, amz_date))
}

fn insert_header(
    request: &mut ::http::Request<()>,
    name: HeaderName,
    value: &str,
) -> Result<(), SigningError> {
    let value = HeaderValue::from_str(value)
        .map_err(|_| SigningError::InvalidHeader(name.as_str().to_string()))?;
    request.headers_mut().insert(name, value);
    Ok(())
}

fn canonical_headers(request: &::http::Request<()>) -> Result<(String, String), SigningError> {
    let mut names = vec![HOST];
    if request.headers().contains_key(RANGE) {
        names.push(RANGE);
    }
    names.push(X_AMZ_CONTENT_SHA256);
    names.push(X_AMZ_DATE);
    if request.headers().contains_key(X_AMZ_SECURITY_TOKEN) {
        names.push(X_AMZ_SECURITY_TOKEN);
    }
    names.sort_unstable_by(|a, b| a.as_str().cmp(b.as_str()));

    let mut canonical = String::new();
    let mut signed = String::new();
    for name in names {
        let values = request.headers().get_all(&name);
        if values.iter().next().is_none() {
            if name == HOST {
                return Err(SigningError::MissingHost);
            }
            return Err(SigningError::InvalidHeader(name.as_str().to_string()));
        }
        if !signed.is_empty() {
            signed.push(';');
        }
        signed.push_str(name.as_str());
        canonical.push_str(name.as_str());
        canonical.push(':');
        let mut first = true;
        for value in values {
            if !first {
                canonical.push(',');
            }
            first = false;
            let value = value
                .to_str()
                .map_err(|_| SigningError::InvalidHeader(name.as_str().to_string()))?;
            let mut words = value.split_ascii_whitespace();
            if let Some(word) = words.next() {
                canonical.push_str(word);
                for word in words {
                    canonical.push(' ');
                    canonical.push_str(word);
                }
            }
        }
        canonical.push('\n');
    }
    Ok((canonical, signed))
}

/// S3 signs the request URI's already-escaped path without normalizing path
/// segments or changing percent-escape spelling.
fn canonical_uri(path: &str) -> &str {
    if path.is_empty() {
        "/"
    } else {
        path
    }
}

fn canonical_query(query: Option<&str>) -> String {
    let Some(query) = query.filter(|query| !query.is_empty()) else {
        return String::new();
    };
    let mut params: Vec<(String, String)> = query
        .split('&')
        .map(|part| {
            let (name, value) = part.split_once('=').unwrap_or((part, ""));
            (uri_encode_component(name), uri_encode_component(value))
        })
        .collect();
    params.sort_unstable();
    params
        .into_iter()
        .map(|(name, value)| format!("{name}={value}"))
        .collect::<Vec<_>>()
        .join("&")
}

fn uri_encode_component(value: &str) -> String {
    let bytes = value.as_bytes();
    let mut out = String::with_capacity(bytes.len());
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b'%' && i + 2 < bytes.len() {
            if let Some(byte) = decode_hex_pair(bytes[i + 1], bytes[i + 2]) {
                uri_encode_byte(&mut out, byte);
                i += 3;
                continue;
            }
        }
        uri_encode_byte(&mut out, bytes[i]);
        i += 1;
    }
    out
}

fn uri_encode_byte(out: &mut String, byte: u8) {
    if byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'.' | b'_' | b'~') {
        out.push(char::from(byte));
    } else {
        const HEX: &[u8; 16] = b"0123456789ABCDEF";
        out.push('%');
        out.push(char::from(HEX[(byte >> 4) as usize]));
        out.push(char::from(HEX[(byte & 0x0f) as usize]));
    }
}

fn decode_hex_pair(high: u8, low: u8) -> Option<u8> {
    Some(hex_value(high)? << 4 | hex_value(low)?)
}

fn hex_value(byte: u8) -> Option<u8> {
    match byte {
        b'0'..=b'9' => Some(byte - b'0'),
        b'a'..=b'f' => Some(byte - b'a' + 10),
        b'A'..=b'F' => Some(byte - b'A' + 10),
        _ => None,
    }
}

fn signature(auth: &S3Auth, date: &str, string_to_sign: &[u8]) -> String {
    let date_key = hmac(
        format!("AWS4{}", auth.secret_access_key).as_bytes(),
        date.as_bytes(),
    );
    let region_key = hmac(&date_key, auth.region.as_bytes());
    let service_key = hmac(&region_key, SERVICE.as_bytes());
    let signing_key = hmac(&service_key, TERMINATOR.as_bytes());
    hex(&hmac(&signing_key, string_to_sign))
}

fn hmac(key: &[u8], value: &[u8]) -> [u8; 32] {
    let mut mac = <HmacSha256 as Mac>::new_from_slice(key).expect("HMAC accepts any key length");
    mac.update(value);
    mac.finalize().into_bytes().into()
}

fn sha256_hex(value: &[u8]) -> String {
    hex(&Sha256::digest(value))
}

fn hex(value: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut out = String::with_capacity(value.len() * 2);
    for byte in value {
        out.push(char::from(HEX[(byte >> 4) as usize]));
        out.push(char::from(HEX[(byte & 0x0f) as usize]));
    }
    out
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use super::*;
    use crate::http::{Method, serialize_request};

    fn auth(session_token: Option<&str>) -> S3Auth {
        S3Auth::new(
            "AKIAIOSFODNN7EXAMPLE",
            "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
            "us-east-1",
            session_token.map(str::to_string),
        )
    }

    fn published_time() -> SystemTime {
        UNIX_EPOCH + Duration::from_secs(1_369_353_600)
    }

    #[test]
    fn canonical_path_and_query_preserve_s3_object_identity() {
        assert_eq!(
            canonical_uri("/bucket/a%20b/%E2%98%83/%25/%2F/%2f/+//./../x"),
            "/bucket/a%20b/%E2%98%83/%25/%2F/%2f/+//./../x"
        );
        assert_eq!(
            canonical_query(Some(
                "z=last&a=hello+world&a=hello%20world&u=%e2%98%83&slash=%2f&pct=%25&empty=&novalue"
            )),
            "a=hello%20world&a=hello%2Bworld&empty=&novalue=&pct=%25&slash=%2F&u=%E2%98%83&z=last"
        );
    }

    #[test]
    fn anonymous_request_is_unchanged() {
        let mut request = ::http::Request::builder()
            .method(Method::GET)
            .uri("/bucket/key")
            .header(HOST, "example.com")
            .header(RANGE, "bytes=0-9")
            .body(())
            .unwrap();
        let before = serialize_request(&request);
        sign_request(&mut request, None, published_time()).unwrap();
        assert_eq!(serialize_request(&request), before);
    }

    #[test]
    fn published_s3_get_vector_signs_range() {
        let mut request = ::http::Request::builder()
            .method(Method::GET)
            .uri("/test.txt")
            .header(HOST, "examplebucket.s3.amazonaws.com")
            .header(RANGE, "bytes=0-9")
            .body(())
            .unwrap();
        sign_request(&mut request, Some(&auth(None)), published_time()).unwrap();

        assert_eq!(request.headers()[X_AMZ_DATE], "20130524T000000Z");
        assert_eq!(request.headers()[X_AMZ_CONTENT_SHA256], EMPTY_PAYLOAD_HASH);
        assert_eq!(
            request.headers()[AUTHORIZATION],
            "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
        );
    }

    #[test]
    fn head_with_query_and_session_token_has_hardcoded_signature() {
        let mut request = ::http::Request::builder()
            .method(Method::HEAD)
            .uri("/bucket//./snow%20%E2%98%83/%2f?versionId=a%2bb&partNumber=2")
            .header(HOST, "s3.example.com:9000")
            .body(())
            .unwrap();
        sign_request(
            &mut request,
            Some(&auth(Some("session/token+value"))),
            published_time(),
        )
        .unwrap();

        assert_eq!(
            request.headers()[X_AMZ_SECURITY_TOKEN],
            "session/token+value"
        );
        let wire = serialize_request(&request);
        assert!(
            wire.starts_with(
                b"HEAD /bucket//./snow%20%E2%98%83/%2f?versionId=a%2bb&partNumber=2 HTTP/1.1\r\n"
            ),
            "signing changed the escaped wire target: {}",
            String::from_utf8_lossy(&wire)
        );
        assert_eq!(
            request.headers()[AUTHORIZATION],
            "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-security-token, Signature=4a5793ca78889ffc5d3bc50cfedabc54d61e42b8b41284b34a783629d1a3a711"
        );
    }

    #[test]
    fn changing_range_changes_signature() {
        let make = |range: &'static str| {
            ::http::Request::builder()
                .method(Method::GET)
                .uri("/test.txt")
                .header(HOST, "examplebucket.s3.amazonaws.com")
                .header(RANGE, range)
                .body(())
                .unwrap()
        };
        let mut first = make("bytes=0-9");
        let mut second = make("bytes=10-19");
        let auth = auth(None);
        sign_request(&mut first, Some(&auth), published_time()).unwrap();
        sign_request(&mut second, Some(&auth), published_time()).unwrap();
        assert_ne!(
            first.headers()[AUTHORIZATION],
            second.headers()[AUTHORIZATION]
        );
    }
}
