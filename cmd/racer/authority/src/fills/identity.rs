//! Schema-1 object identity carried in `Racer-Metadata`.
//!
//! This is the same canonical encoding as the Azure origin adapter
//! (`cmd/racer/src/origin/azure/identity.rs`) and the Go `internal/racerobject`
//! package, restricted to the unversioned blobs a fill pins. The bytes are
//! exactly what Go `encoding/json` produces for
//! `{"schema":1,"bucket":..,"key":..}` including its HTML-safe escaping. The
//! SHA-256 of those bytes is the Racer key, and the same bytes are sent as
//! `Racer-Metadata` on reads.

use racer_wire::object::CacheKey;
use sha2::Digest;

/// Largest canonical identity, matching the Go `8 << 10` bound.
pub const MAX_IDENTITY_BYTES: usize = 8 << 10;

/// Largest key, in bytes.
const MAX_NAME_BYTES: usize = 1024;

/// Identity inputs outside the canonical bounds.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct InvalidIdentity;

/// Canonical identity bytes and cache key for one unversioned object.
pub fn canonical(bucket: &str, key: &str) -> Result<(Vec<u8>, CacheKey), InvalidIdentity> {
    if !valid_bucket(bucket) || key.is_empty() || key.len() > MAX_NAME_BYTES {
        return Err(InvalidIdentity);
    }
    let mut out = Vec::with_capacity(48 + bucket.len() + key.len());
    out.extend_from_slice(br#"{"schema":1,"bucket":"#);
    append_go_string(&mut out, bucket);
    out.extend_from_slice(br#","key":"#);
    append_go_string(&mut out, key);
    out.push(b'}');
    if out.len() > MAX_IDENTITY_BYTES {
        return Err(InvalidIdentity);
    }
    let cache_key = CacheKey(sha2::Sha256::digest(&out).into());
    Ok((out, cache_key))
}

/// Legacy S3-compatible bucket names, never paths or URLs.
fn valid_bucket(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 255
        && value != "."
        && value != ".."
        && value
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'-' | b'_'))
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

    /// Port of the Go `TestNewRequestCanonicalIdentity` vector without a
    /// version ID.
    #[test]
    fn canonical_identity_matches_go_golden() {
        let want = r#"{"schema":1,"bucket":"bucket","key":"a/../b%2Fc +雪"}"#;
        let (bytes, key) = canonical("bucket", "a/../b%2Fc +雪").unwrap();
        assert_eq!(bytes, want.as_bytes());
        assert_eq!(key, sha(want.as_bytes()));
        let (plain, _) = canonical("bucket", "key").unwrap();
        assert_eq!(plain, br#"{"schema":1,"bucket":"bucket","key":"key"}"#);
        for (bucket, object) in [
            ("other", "a/../b%2Fc +雪"),
            ("bucket", "b%2Fc +雪"),
            ("bucket", "a/../b/c +雪"),
        ] {
            assert_ne!(canonical(bucket, object).unwrap().1, key);
        }
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

    #[test]
    fn validation_matches_go() {
        let long_key = "k".repeat(1025);
        let long_bucket = "b".repeat(256);
        for (bucket, key) in [
            ("", "key"),
            ("with space", "key"),
            ("bucket\n", "key"),
            ("https://evil", "key"),
            ("../bucket", "key"),
            ("bucket", ""),
            ("bucket", long_key.as_str()),
            ("bucket\u{a0}", "key"),
            ("bucket\u{0}", "key"),
            (".", "key"),
            ("..", "key"),
            (long_bucket.as_str(), "key"),
            ("buckét", "key"),
        ] {
            assert_eq!(
                canonical(bucket, key),
                Err(InvalidIdentity),
                "{bucket:?} {key:?}"
            );
        }
        let max_key = "k".repeat(1024);
        canonical("Legacy_Bucket-9.example", &max_key).unwrap();
    }
}
