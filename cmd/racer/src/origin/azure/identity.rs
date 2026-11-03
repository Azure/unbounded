//! Schema-1 object identity carried in `Racer-Metadata`.
//!
//! Port of `NewRequest`, `validBucket`, `validObject`, and `decodeObject` in
//! `internal/racerobject/racerobject.go`. The canonical bytes are exactly what Go
//! `encoding/json` produces for
//! `{"schema":1,"bucket":..,"key":..,"versionId":..}` (`versionId` omitted when
//! empty), including its HTML-safe escaping. The SHA-256 of those bytes is the
//! cache key, so alternate JSON spellings cannot give the same upstream object a
//! second cache identity.

use crate::error::{Error, Result};
use crate::model::CacheKey;
use sha2::Digest;

/// Largest canonical identity, matching the Go `8 << 10` bound.
pub const MAX_IDENTITY_BYTES: usize = 8 << 10;

/// Largest key and version ID, in bytes.
const MAX_NAME_BYTES: usize = 1024;

/// One exact upstream object. Keys and version IDs are never cleaned, decoded,
/// or normalized.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AzureObject {
    pub bucket: String,
    pub key: String,
    pub version_id: String,
}

/// Port of Go `validBucket`: legacy S3-compatible names, never paths or URLs.
pub fn valid_bucket(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 255
        && value != "."
        && value != ".."
        && value
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'-' | b'_'))
}

/// Port of Go `validObject`. Rust strings are always valid UTF-8.
fn valid_object(bucket: &str, key: &str, version_id: &str) -> bool {
    valid_bucket(bucket)
        && !key.is_empty()
        && key.len() <= MAX_NAME_BYTES
        && version_id.len() <= MAX_NAME_BYTES
}

/// Port of Go `NewRequest`: canonical identity bytes and their cache key.
pub fn canonical(bucket: &str, key: &str, version_id: &str) -> Result<(Vec<u8>, CacheKey)> {
    if !valid_object(bucket, key, version_id) {
        return Err(Error::InvalidRequest);
    }
    let mut out = Vec::with_capacity(48 + bucket.len() + key.len() + version_id.len());
    out.extend_from_slice(br#"{"schema":1,"bucket":"#);
    append_go_string(&mut out, bucket);
    out.extend_from_slice(br#","key":"#);
    append_go_string(&mut out, key);
    if !version_id.is_empty() {
        out.extend_from_slice(br#","versionId":"#);
        append_go_string(&mut out, version_id);
    }
    out.push(b'}');
    if out.len() > MAX_IDENTITY_BYTES {
        return Err(Error::InvalidRequest);
    }
    let key = CacheKey(sha2::Sha256::digest(&out).into());
    Ok((out, key))
}

/// Port of Go `decodeObject`: accept only the canonical spelling whose hash is
/// the request key. Objects outside `container` are forbidden.
///
/// Any byte string Go accepts equals `canonical(object)` for a valid object, so
/// it has the exact shape parsed here. Conversely, the re-encoding check below
/// accepts only canonical bytes, which Go decodes back to the same object.
pub fn decode(raw: &[u8], key: &CacheKey, container: &str) -> Result<AzureObject> {
    let object = parse(raw).ok_or(Error::InvalidRequest)?;
    let (bytes, hash) = canonical(&object.bucket, &object.key, &object.version_id)?;
    if bytes != raw || hash != *key {
        return Err(Error::InvalidRequest);
    }
    if object.bucket != container {
        return Err(Error::OriginForbidden);
    }
    Ok(object)
}

/// Parse the only shape `canonical` produces.
fn parse(raw: &[u8]) -> Option<AzureObject> {
    let raw = std::str::from_utf8(raw).ok()?;
    let rest = raw.strip_prefix(r#"{"schema":1,"bucket":"#)?;
    let (bucket, rest) = json_string(rest)?;
    let rest = rest.strip_prefix(r#","key":"#)?;
    let (key, rest) = json_string(rest)?;
    let (version_id, rest) = match rest.strip_prefix(r#","versionId":"#) {
        Some(rest) => json_string(rest)?,
        None => (String::new(), rest),
    };
    (rest == "}").then_some(AzureObject {
        bucket,
        key,
        version_id,
    })
}

/// Decode one JSON string literal at the start of `input`.
fn json_string(input: &str) -> Option<(String, &str)> {
    let mut chars = input.strip_prefix('"')?.char_indices();
    let body = &input[1..];
    let mut out = String::new();
    while let Some((index, c)) = chars.next() {
        match c {
            '"' => return Some((out, &body[index + 1..])),
            '\\' => {
                let (_, escape) = chars.next()?;
                out.push(match escape {
                    '"' => '"',
                    '\\' => '\\',
                    '/' => '/',
                    'b' => '\u{8}',
                    'f' => '\u{c}',
                    'n' => '\n',
                    'r' => '\r',
                    't' => '\t',
                    'u' => {
                        let high = hex4(&mut chars)?;
                        if (0xd800..0xdc00).contains(&high) {
                            if chars.next()?.1 != '\\' || chars.next()?.1 != 'u' {
                                return None;
                            }
                            let low = hex4(&mut chars)?;
                            if !(0xdc00..0xe000).contains(&low) {
                                return None;
                            }
                            char::from_u32(0x10000 + ((high - 0xd800) << 10) + (low - 0xdc00))?
                        } else {
                            // Lone low surrogates fail here; Go would substitute
                            // U+FFFD and then fail the canonical comparison.
                            char::from_u32(high)?
                        }
                    }
                    _ => return None,
                });
            }
            c if (c as u32) < 0x20 => return None,
            c => out.push(c),
        }
    }
    None
}

fn hex4(chars: &mut std::str::CharIndices<'_>) -> Option<u32> {
    let mut value = 0;
    for _ in 0..4 {
        value = value * 16 + chars.next()?.1.to_digit(16)?;
    }
    Some(value)
}

/// Go `encoding/json` string encoding with `SetEscapeHTML(true)`, the
/// `json.Marshal` default, for valid UTF-8 input.
fn append_go_string(out: &mut Vec<u8>, value: &str) {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    out.push(b'"');
    for c in value.chars() {
        match c {
            '"' | '\\' => out.extend_from_slice(&[b'\\', c as u8]),
            '\u{8}' => out.extend_from_slice(b"\\b"),
            '\u{c}' => out.extend_from_slice(b"\\f"),
            '\n' => out.extend_from_slice(b"\\n"),
            '\r' => out.extend_from_slice(b"\\r"),
            '\t' => out.extend_from_slice(b"\\t"),
            '<' | '>' | '&' | '\u{0}'..='\u{1f}' => {
                let b = c as u8;
                out.extend_from_slice(&[
                    b'\\',
                    b'u',
                    b'0',
                    b'0',
                    HEX[usize::from(b >> 4)],
                    HEX[usize::from(b & 0xf)],
                ]);
            }
            '\u{2028}' => out.extend_from_slice(b"\\u2028"),
            '\u{2029}' => out.extend_from_slice(b"\\u2029"),
            c => {
                let mut buffer = [0; 4];
                out.extend_from_slice(c.encode_utf8(&mut buffer).as_bytes());
            }
        }
    }
    out.push(b'"');
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sha(bytes: &[u8]) -> CacheKey {
        CacheKey(sha2::Sha256::digest(bytes).into())
    }

    /// Port of Go `TestNewRequestCanonicalIdentity`.
    #[test]
    fn canonical_identity_matches_go_golden() {
        let want = r#"{"schema":1,"bucket":"bucket","key":"a/../b%2Fc +雪","versionId":"v+/="}"#;
        let (bytes, key) = canonical("bucket", "a/../b%2Fc +雪", "v+/=").unwrap();
        assert_eq!(bytes, want.as_bytes());
        assert_eq!(key, sha(want.as_bytes()));
        for (bucket, object, version) in [
            ("other", "a/../b%2Fc +雪", "v+/="),
            ("bucket", "b%2Fc +雪", "v+/="),
            ("bucket", "a/../b/c +雪", "v+/="),
            ("bucket", "a/../b%2Fc +雪", ""),
        ] {
            assert_ne!(canonical(bucket, object, version).unwrap().1, key);
        }
        let (plain, _) = canonical("bucket", "key", "").unwrap();
        assert_eq!(plain, br#"{"schema":1,"bucket":"bucket","key":"key"}"#);
    }

    /// Bytes produced by Go 1.26 `json.Marshal(Object{...})`.
    #[test]
    fn escaping_matches_go_encoding_json() {
        for (key, want) in [
            ("<a>&b", r#""\u003ca\u003e\u0026b""#),
            ("q\"s\\", r#""q\"s\\""#),
            ("\u{8}\u{c}\n\r\t", r#""\b\f\n\r\t""#),
            ("\u{0}\u{1}\u{1f}", r#""\u0000\u0001\u001f""#),
            ("\u{7f}/~", "\"\u{7f}/~\""),
            ("\u{2028}\u{2029}\u{2027}", "\"\\u2028\\u2029\u{2027}\""),
            ("雪😀", "\"雪😀\""),
        ] {
            let mut out = Vec::new();
            append_go_string(&mut out, key);
            assert_eq!(String::from_utf8(out).unwrap(), want, "{key:?}");
        }
    }

    /// Port of Go `TestNewRequestValidation`. Invalid UTF-8 cases are
    /// unrepresentable in `&str`; `decode` covers them on raw bytes.
    #[test]
    fn validation_matches_go() {
        let long_key = "k".repeat(1025);
        let long_version = "v".repeat(1025);
        let long_bucket = "b".repeat(256);
        for (bucket, key, version) in [
            ("", "key", ""),
            ("with space", "key", ""),
            ("bucket\n", "key", ""),
            ("https://evil", "key", ""),
            ("../bucket", "key", ""),
            ("bucket", "", ""),
            ("bucket", long_key.as_str(), ""),
            ("bucket", "key", long_version.as_str()),
            ("bucket\u{a0}", "key", ""),
            ("bucket\u{0}", "key", ""),
            (".", "key", ""),
            ("..", "key", ""),
            (long_bucket.as_str(), "key", ""),
            ("buckét", "key", ""),
        ] {
            assert_eq!(
                canonical(bucket, key, version).map(|_| ()),
                Err(Error::InvalidRequest),
                "{bucket:?} {key:?} {version:?}"
            );
        }
        let max_key = "k".repeat(1024);
        for key in ["/", ".", "../", "a//b", "a\u{0}b", "line\nbreak", &max_key] {
            canonical("bucket", key, "null").unwrap();
        }
        let (b, k, v) = ("b".repeat(255), "k".repeat(1024), "v".repeat(1024));
        canonical(&b, &k, &v).unwrap();
        canonical("Legacy_Bucket-9.example", "key", "v\u{0}\n雪").unwrap();
        // Both strings fit the object limits, but escaping exceeds the field cap.
        let nul = "\u{0}".repeat(1024);
        assert_eq!(
            canonical("bucket", &nul, &nul).map(|_| ()),
            Err(Error::InvalidRequest)
        );
    }

    #[test]
    fn decode_round_trips_canonical_bytes() {
        for (bucket, key, version) in [
            ("models", "a/../b%2Fc +雪", "v+/="),
            ("models", "<&>\u{2028}\u{0}\"\\😀", ""),
            ("models", "k", "2024-01-01T00:00:00.0000000Z"),
        ] {
            let (raw, hash) = canonical(bucket, key, version).unwrap();
            let object = decode(&raw, &hash, "models").unwrap();
            assert_eq!(
                object,
                AzureObject {
                    bucket: bucket.into(),
                    key: key.into(),
                    version_id: version.into()
                }
            );
        }
    }

    /// Port of Go `TestOriginStrictIdentityBridge`.
    #[test]
    fn decode_rejects_alternate_spellings() {
        let (canonical, key) = canonical("bucket", "key", "").unwrap();
        let canonical = String::from_utf8(canonical).unwrap();
        let trimmed = canonical.strip_suffix('}').unwrap();
        for raw in [
            String::new(),
            "null".into(),
            format!(r#"{trimmed},"endpoint":"https://evil"}}"#),
            canonical.replace(r#""schema":1"#, r#""schema":1,"schema":1"#),
            canonical.replacen("schema", "Schema", 1),
            canonical.replace(r#""schema":1"#, r#""schema": 1"#),
            canonical.replace(r#""schema":1"#, r#""schema":2"#),
            format!("{canonical}{{}}"),
            format!(r#"{trimmed},"versionId":""}}"#),
            canonical.replace(r#""bucket":"bucket""#, r#""bucket":"\u0062ucket""#),
            canonical.replace(r#""key":"key""#, r#""key":"k\/ey""#),
        ] {
            assert_eq!(
                decode(raw.as_bytes(), &sha(raw.as_bytes()), "bucket"),
                Err(Error::InvalidRequest),
                "{raw}"
            );
        }
        // Invalid UTF-8 and escapes Go would decode differently.
        for raw in [
            &br#"{"schema":1,"bucket":"bucket","key":"\xff"}"#[..],
            br#"{"schema":1,"bucket":"bucket","key":"\ud800"}"#,
            br#"{"schema":1,"bucket":"bucket","key":"\udc00"}"#,
            br#"{"schema":1,"bucket":"bucket","key":"a"#,
            br#"{"schema":1,"bucket":"bucket","key":"\u00"}"#,
        ] {
            assert_eq!(decode(raw, &sha(raw), "bucket"), Err(Error::InvalidRequest));
        }
        let mut wrong = key;
        wrong.0[0] ^= 1;
        assert_eq!(
            decode(canonical.as_bytes(), &wrong, "bucket"),
            Err(Error::InvalidRequest)
        );
        assert_eq!(
            decode(canonical.as_bytes(), &key, "other"),
            Err(Error::OriginForbidden)
        );
        let (other, other_key) = super::canonical("other", "key", "").unwrap();
        assert_eq!(
            decode(&other, &other_key, "bucket"),
            Err(Error::OriginForbidden)
        );
    }
}
