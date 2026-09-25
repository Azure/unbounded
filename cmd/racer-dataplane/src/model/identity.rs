//! Semantic identities and canonical-encoding boundaries.
//!
//! Keys are exactly 32 bytes. Strong ETags are opaque version identifiers, not
//! content hashes. Placement excludes ETag; page cache and flight identity include it.

use super::MAX_FIELD_BYTES;
use crate::error::{Error, Result};

#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct ClusterId(pub String);
/// Kubernetes ClusterCache UID, not its reusable resource name.
#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct CacheId(pub String);
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct CacheKey(pub [u8; 32]);

impl CacheKey {
    /// Decode exactly 64 lowercase hexadecimal bytes without normalization.
    pub fn parse_hex(value: &[u8]) -> Result<Self> {
        if value.len() != 64 {
            return Err(Error::InvalidRequest);
        }
        let nibble = |byte| match byte {
            b'0'..=b'9' => Ok(byte - b'0'),
            b'a'..=b'f' => Ok(byte - b'a' + 10),
            _ => Err(Error::InvalidRequest),
        };
        let mut key = [0; 32];
        for (output, pair) in key.iter_mut().zip(value.chunks_exact(2)) {
            *output = nibble(pair[0])? << 4 | nibble(pair[1])?;
        }
        Ok(Self(key))
    }

    pub fn to_hex(&self) -> String {
        const HEX: &[u8; 16] = b"0123456789abcdef";
        let mut value = String::with_capacity(64);
        for byte in self.0 {
            value.push(char::from(HEX[usize::from(byte >> 4)]));
            value.push(char::from(HEX[usize::from(byte & 15)]));
        }
        value
    }
}

#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
/// Kubernetes Node UID, not its reusable resource name.
pub struct NodeId(pub String);
#[derive(Clone, Debug, Eq, Hash, PartialEq)]
pub struct StrongEtag(String);

impl StrongEtag {
    #[cfg(test)]
    pub(crate) fn test_value(value: &str) -> Self {
        if value.starts_with('"') {
            Self::parse(value.as_bytes()).unwrap()
        } else {
            Self::parse(format!("\"{value}\"").as_bytes()).unwrap()
        }
    }
    /// Parse the SDK's quoted ASCII strong-tag grammar, preserving exact bytes.
    /// Commas and backslashes inside quotes are literal, not lists or escapes.
    pub fn parse(value: &[u8]) -> Result<Self> {
        if !(2..=MAX_FIELD_BYTES).contains(&value.len())
            || value.first() != Some(&b'"')
            || value.last() != Some(&b'"')
            || !value[1..value.len() - 1]
                .iter()
                .all(|&byte| byte == 0x21 || (0x23..=0x7e).contains(&byte))
        {
            return Err(Error::InvalidRequest);
        }
        let value = std::str::from_utf8(value).map_err(|_| Error::InvalidRequest)?;
        Ok(Self(value.to_owned()))
    }

    pub fn as_str(&self) -> &str {
        &self.0
    }

    pub fn as_bytes(&self) -> &[u8] {
        self.0.as_bytes()
    }
}

#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct PageNumber(pub u64);
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct MembershipVersion(pub u64);
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct RequestId(pub [u8; 16]);
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct AttemptId(pub [u8; 16]);
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct TransferId(pub [u8; 16]);
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct WorkerId(pub u16);

#[derive(Clone, Debug, Eq, Hash, PartialEq)]
pub struct ObjectId {
    pub cache: CacheId,
    pub key: CacheKey,
}

#[derive(Clone, Debug, Eq, Hash, PartialEq)]
pub struct ObjectVersion {
    pub object: ObjectId,
    pub etag: StrongEtag,
}

#[derive(Clone, Debug, Eq, Hash, PartialEq)]
pub struct PageId {
    pub version: ObjectVersion,
    pub number: PageNumber,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cache_key_canonical_hex_vectors() {
        for (bytes, wire) in [
            (
                [0; 32],
                "0000000000000000000000000000000000000000000000000000000000000000",
            ),
            (
                [0xff; 32],
                "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
            ),
            (
                std::array::from_fn(|i| i as u8),
                "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
            ),
        ] {
            assert_eq!(CacheKey::parse_hex(wire.as_bytes()), Ok(CacheKey(bytes)));
            assert_eq!(CacheKey(bytes).to_hex(), wire);
        }
        // Cover all byte values, including every high and low nibble.
        for start in (0..256).step_by(32) {
            let key = CacheKey(std::array::from_fn(|i| (start + i) as u8));
            assert_eq!(CacheKey::parse_hex(key.to_hex().as_bytes()), Ok(key));
        }
    }

    #[test]
    fn cache_key_rejects_noncanonical_hex() {
        for length in [0, 1, 63, 65, 128] {
            assert_eq!(
                CacheKey::parse_hex(&vec![b'0'; length]),
                Err(Error::InvalidRequest)
            );
        }
        for invalid in [b'A', b'F', b'g', b'/', b' ', b'\t', b'\n', 0, 0xff] {
            for position in [0, 1, 62, 63] {
                let mut value = [b'0'; 64];
                value[position] = invalid;
                assert_eq!(CacheKey::parse_hex(&value), Err(Error::InvalidRequest));
            }
        }
        assert_eq!(
            CacheKey::parse_hex(format!("0x{}", "0".repeat(62)).as_bytes()),
            Err(Error::InvalidRequest)
        );
    }

    #[test]
    fn cache_key_canonical_hex_vectors_round_trip() {
        for (bytes, hex) in [
            ([0; 32], "0".repeat(64)),
            ([0xff; 32], "f".repeat(64)),
            (
                std::array::from_fn(|index| index as u8),
                "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f".into(),
            ),
        ] {
            assert_eq!(CacheKey::parse_hex(hex.as_bytes()), Ok(CacheKey(bytes)));
            assert_eq!(CacheKey(bytes).to_hex(), hex);
        }
        // Exercise every byte value, including both hexadecimal nibble positions.
        for byte in 0..=u8::MAX {
            let key = CacheKey([byte; 32]);
            assert_eq!(CacheKey::parse_hex(key.to_hex().as_bytes()), Ok(key));
        }
    }

    #[test]
    fn cache_key_rejects_noncanonical_hex_byte_vectors() {
        for value in [
            Vec::new(),
            vec![b'0'; 63],
            vec![b'0'; 65],
            vec![b'A'; 64],
            vec![b'g'; 64],
            vec![b' '; 64],
            vec![0xff; 64],
        ] {
            assert_eq!(CacheKey::parse_hex(&value), Err(Error::InvalidRequest));
        }
        for invalid in [b'A', b'G', b'/', b':', b'\n', b'\0', b'%', 0x80] {
            for index in [0, 1, 62, 63] {
                let mut value = [b'0'; 64];
                value[index] = invalid;
                assert_eq!(CacheKey::parse_hex(&value), Err(Error::InvalidRequest));
            }
        }
        let prefixed = format!("0x{}", "0".repeat(62));
        assert_eq!(
            CacheKey::parse_hex(prefixed.as_bytes()),
            Err(Error::InvalidRequest)
        );
    }

    #[test]
    fn strong_tags_preserve_quotes_and_literal_punctuation() {
        for value in [br#""""#.as_slice(), br#""a,b""#, br#""a\b""#, br#""!#~""#] {
            let tag = StrongEtag::parse(value).unwrap();
            assert_eq!(tag.as_bytes(), value);
            assert_eq!(tag.as_str().as_bytes(), value);
        }
        let limit = format!("\"{}\"", "x".repeat(MAX_FIELD_BYTES - 2));
        assert_eq!(StrongEtag::parse(limit.as_bytes()).unwrap().as_str(), limit);
    }

    #[test]
    fn rejects_weak_lists_whitespace_controls_and_non_ascii() {
        for value in [
            b"".as_slice(),
            b"*",
            br#"W/"v""#,
            br#""a", "b""#,
            br#""a"b""#,
            br#""a b""#,
            b"\"\t\"",
            b"\"\x80\"",
            b"\"a\n\"",
            b" \"v\"",
            b"\"v\" ",
            b"\"",
            b"v",
            b"\"\x7f\"",
        ] {
            assert_eq!(StrongEtag::parse(value), Err(Error::InvalidRequest));
        }
        let oversized = format!("\"{}\"", "x".repeat(MAX_FIELD_BYTES - 1));
        assert_eq!(
            StrongEtag::parse(oversized.as_bytes()),
            Err(Error::InvalidRequest)
        );
    }
}
