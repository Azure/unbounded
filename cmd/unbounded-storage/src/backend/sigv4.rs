// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! AWS Signature Version 4 request signing.
//!
//! Pure, cross-platform helpers that turn a request's salient parts
//! (method, canonical URI, query string, the small set of signed
//! headers, the payload hash, region, service, and a pre-rendered date
//! pair) into the `Authorization` header value an S3-compatible origin
//! expects. There is no I/O and no platform dependency here: everything
//! is a function over byte/string slices built on [`sha2::Sha256`] and
//! [`hmac::Hmac`], so it can be unit-tested against the published AWS
//! SigV4 test-suite vectors on any host.
//!
//! The chain is the canonical SigV4 pipeline:
//!
//! 1. Build the canonical request from the method, URI, query, the
//!    sorted/lowercased signed headers, and the payload hash.
//! 2. Build the string-to-sign from the algorithm id, `amz_date`, the
//!    credential scope, and the SHA-256 of the canonical request.
//! 3. Derive the signing key by chaining HMAC-SHA256 from `AWS4` +
//!    secret through date, region, service, and `aws4_request`.
//! 4. The signature is HMAC-SHA256 of the string-to-sign under that key.
//!
//! Session tokens (`x-amz-security-token`) are intentionally omitted in
//! v1; only long-lived access-key/secret credentials are supported.

use hmac::{Hmac, Mac};
use sha2::{Digest, Sha256};

/// Lowercase hex SHA-256 of the empty byte string. SigV4 requires the
/// payload hash even for bodyless GET/HEAD requests; for those the body
/// is empty and this well-known constant is used as `x-amz-content-sha256`.
pub const EMPTY_PAYLOAD_SHA256: &str =
    "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855";

/// SigV4 algorithm identifier used in the string-to-sign and the
/// `Authorization` header.
const ALGORITHM: &str = "AWS4-HMAC-SHA256";

/// Terminating string in the credential scope and the signing-key chain.
const TERMINATOR: &str = "aws4_request";

type HmacSha256 = Hmac<Sha256>;

/// Long-lived AWS credentials used to sign a request. The optional
/// session token (`x-amz-security-token`) is intentionally omitted in
/// v1; only static access-key/secret pairs are supported.
#[derive(Clone)]
pub struct Credentials {
    pub access_key_id: String,
    pub secret_access_key: String,
}

impl Credentials {
    pub fn new(access_key_id: impl Into<String>, secret_access_key: impl Into<String>) -> Self {
        Self {
            access_key_id: access_key_id.into(),
            secret_access_key: secret_access_key.into(),
        }
    }
}

/// A single header that participates in signing. Both name and value are
/// canonicalized (name lowercased, value trimmed) by the signer.
#[derive(Clone, Copy)]
pub struct SignedHeader<'a> {
    pub name: &'a str,
    pub value: &'a str,
}

/// Compute the `Authorization` header value for a SigV4-signed request.
///
/// `canonical_uri` is the (already path-encoded) object path, e.g.
/// `/bucket/key`; `canonical_query` is the canonical query string
/// (empty for the bodyless object GET/HEAD requests v1 issues).
/// `headers` are the headers to sign (host, x-amz-date,
/// x-amz-content-sha256); they are canonicalized and sorted internally,
/// so caller order does not matter. `payload_hash` is the lowercase hex
/// SHA-256 of the body ([`EMPTY_PAYLOAD_SHA256`] for empty bodies).
/// `amz_date` is `YYYYMMDD'T'HHMMSS'Z'` and `date_stamp` is `YYYYMMDD`;
/// the two must agree on the calendar date.
#[allow(clippy::too_many_arguments)]
pub fn authorization_header(
    creds: &Credentials,
    method: &str,
    canonical_uri: &str,
    canonical_query: &str,
    headers: &[SignedHeader<'_>],
    payload_hash: &str,
    region: &str,
    service: &str,
    amz_date: &str,
    date_stamp: &str,
) -> String {
    let (canonical_headers, signed_headers) = canonical_headers(headers);
    let canonical_req = canonical_request(
        method,
        canonical_uri,
        canonical_query,
        &canonical_headers,
        &signed_headers,
        payload_hash,
    );
    let scope = credential_scope(date_stamp, region, service);
    let to_sign = string_to_sign(amz_date, &scope, &sha256_hex(canonical_req.as_bytes()));
    let key = signing_key(&creds.secret_access_key, date_stamp, region, service);
    let signature = hex(&hmac_sha256(&key, to_sign.as_bytes()));

    format!(
        "{ALGORITHM} Credential={}/{scope}, SignedHeaders={signed_headers}, Signature={signature}",
        creds.access_key_id
    )
}

/// Build the canonical request string per the SigV4 spec:
/// method, URI, query, canonical headers block, signed-headers list,
/// and the payload hash, each on its own line.
fn canonical_request(
    method: &str,
    canonical_uri: &str,
    canonical_query: &str,
    canonical_headers: &str,
    signed_headers: &str,
    payload_hash: &str,
) -> String {
    format!(
        "{method}\n{canonical_uri}\n{canonical_query}\n{canonical_headers}\n{signed_headers}\n{payload_hash}"
    )
}

/// Render the canonical headers block (each `name:value\n`, sorted by
/// name) and the semicolon-joined signed-headers list. Names are
/// lowercased and values trimmed of surrounding whitespace.
fn canonical_headers(headers: &[SignedHeader<'_>]) -> (String, String) {
    let mut pairs: Vec<(String, String)> = headers
        .iter()
        .map(|h| (h.name.to_ascii_lowercase(), h.value.trim().to_string()))
        .collect();
    pairs.sort_by(|a, b| a.0.cmp(&b.0));

    let mut canonical = String::new();
    for (name, value) in &pairs {
        canonical.push_str(name);
        canonical.push(':');
        canonical.push_str(value);
        canonical.push('\n');
    }

    let signed = pairs
        .iter()
        .map(|(name, _)| name.as_str())
        .collect::<Vec<_>>()
        .join(";");

    (canonical, signed)
}

/// Build the credential scope: `date_stamp/region/service/aws4_request`.
fn credential_scope(date_stamp: &str, region: &str, service: &str) -> String {
    format!("{date_stamp}/{region}/{service}/{TERMINATOR}")
}

/// Build the string-to-sign from the algorithm id, the request
/// timestamp, the credential scope, and the hex SHA-256 of the
/// canonical request.
fn string_to_sign(amz_date: &str, scope: &str, canonical_request_hash: &str) -> String {
    format!("{ALGORITHM}\n{amz_date}\n{scope}\n{canonical_request_hash}")
}

/// Derive the SigV4 signing key by chaining HMAC-SHA256 from
/// `AWS4`+secret through the date, region, service, and the
/// `aws4_request` terminator.
fn signing_key(secret: &str, date_stamp: &str, region: &str, service: &str) -> [u8; 32] {
    let mut seed = String::from("AWS4");
    seed.push_str(secret);
    let k_date = hmac_sha256(seed.as_bytes(), date_stamp.as_bytes());
    let k_region = hmac_sha256(&k_date, region.as_bytes());
    let k_service = hmac_sha256(&k_region, service.as_bytes());
    hmac_sha256(&k_service, TERMINATOR.as_bytes())
}

fn hmac_sha256(key: &[u8], data: &[u8]) -> [u8; 32] {
    let mut mac = HmacSha256::new_from_slice(key).expect("HMAC accepts keys of any length");
    mac.update(data);
    mac.finalize().into_bytes().into()
}

fn sha256_hex(data: &[u8]) -> String {
    let mut hasher = Sha256::new();
    hasher.update(data);
    hex(&hasher.finalize())
}

fn hex(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut out = String::with_capacity(bytes.len() * 2);
    for &b in bytes {
        out.push(HEX[(b >> 4) as usize] as char);
        out.push(HEX[(b & 0x0f) as usize] as char);
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    // The AWS-published SigV4 test-suite `get-vanilla` vector.
    const ACCESS_KEY: &str = "AKIDEXAMPLE";
    const SECRET_KEY: &str = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY";
    const REGION: &str = "us-east-1";
    const SERVICE: &str = "service";
    const AMZ_DATE: &str = "20150830T123600Z";
    const DATE_STAMP: &str = "20150830";

    fn get_vanilla_headers() -> [SignedHeader<'static>; 2] {
        [
            SignedHeader {
                name: "host",
                value: "example.amazonaws.com",
            },
            SignedHeader {
                name: "x-amz-date",
                value: AMZ_DATE,
            },
        ]
    }

    #[test]
    fn empty_payload_sha256_matches_constant() {
        assert_eq!(sha256_hex(b""), EMPTY_PAYLOAD_SHA256);
    }

    #[test]
    fn get_vanilla_canonical_request_matches_aws_vector() {
        let (headers, signed) = canonical_headers(&get_vanilla_headers());
        assert_eq!(signed, "host;x-amz-date");
        let creq = canonical_request("GET", "/", "", &headers, &signed, EMPTY_PAYLOAD_SHA256);
        let expected = "\
GET
/

host:example.amazonaws.com
x-amz-date:20150830T123600Z

host;x-amz-date
e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855";
        assert_eq!(creq, expected);
        // The AWS-documented SHA-256 of the get-vanilla canonical request.
        assert_eq!(
            sha256_hex(creq.as_bytes()),
            "bb579772317eb040ac9ed261061d46c1f17a8133879d6129b6e1c25292927e63"
        );
    }

    #[test]
    fn get_vanilla_string_to_sign_matches_aws_vector() {
        let (headers, signed) = canonical_headers(&get_vanilla_headers());
        let creq = canonical_request("GET", "/", "", &headers, &signed, EMPTY_PAYLOAD_SHA256);
        let scope = credential_scope(DATE_STAMP, REGION, SERVICE);
        let sts = string_to_sign(AMZ_DATE, &scope, &sha256_hex(creq.as_bytes()));
        let expected = "\
AWS4-HMAC-SHA256
20150830T123600Z
20150830/us-east-1/service/aws4_request
bb579772317eb040ac9ed261061d46c1f17a8133879d6129b6e1c25292927e63";
        assert_eq!(sts, expected);
    }

    #[test]
    fn get_vanilla_signature_matches_aws_vector() {
        let creds = Credentials::new(ACCESS_KEY, SECRET_KEY);
        let authz = authorization_header(
            &creds,
            "GET",
            "/",
            "",
            &get_vanilla_headers(),
            EMPTY_PAYLOAD_SHA256,
            REGION,
            SERVICE,
            AMZ_DATE,
            DATE_STAMP,
        );
        // The AWS-documented get-vanilla.authz value.
        let expected = "AWS4-HMAC-SHA256 \
Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, \
SignedHeaders=host;x-amz-date, \
Signature=ea21d6f05e96a897f6000a1a293f0a5bf0f92a00343409e820dce329ca6365ea";
        assert_eq!(authz, expected);
    }

    #[test]
    fn header_canonicalization_lowercases_sorts_and_trims() {
        let headers = [
            SignedHeader {
                name: "X-Amz-Date",
                value: "  20150830T123600Z  ",
            },
            SignedHeader {
                name: "Host",
                value: "example.amazonaws.com",
            },
        ];
        let (canonical, signed) = canonical_headers(&headers);
        assert_eq!(signed, "host;x-amz-date");
        assert_eq!(
            canonical,
            "host:example.amazonaws.com\nx-amz-date:20150830T123600Z\n"
        );
    }

    #[test]
    fn signing_is_deterministic() {
        let creds = Credentials::new(ACCESS_KEY, SECRET_KEY);
        let a = authorization_header(
            &creds,
            "GET",
            "/",
            "",
            &get_vanilla_headers(),
            EMPTY_PAYLOAD_SHA256,
            REGION,
            SERVICE,
            AMZ_DATE,
            DATE_STAMP,
        );
        let b = authorization_header(
            &creds,
            "GET",
            "/",
            "",
            &get_vanilla_headers(),
            EMPTY_PAYLOAD_SHA256,
            REGION,
            SERVICE,
            AMZ_DATE,
            DATE_STAMP,
        );
        assert_eq!(a, b);
    }
}
