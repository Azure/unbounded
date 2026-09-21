// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Representation identity shared by storage and transport. Payload CRCs are
//! independent of this origin-supplied checksum; the dataplane does not hash objects.
use std::io;

/// Raw origin-supplied representation version. All 256-bit values are valid;
/// parsing validates the HTTP encoding, not the object contents.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
#[repr(transparent)]
pub struct Checksum(pub [u8; 32]);

impl Checksum {
    /// Only the canonical strong HTTP representation is accepted.
    pub fn from_etag(value: &str) -> io::Result<Self> {
        let bytes = value.as_bytes();
        if bytes.len() != 66 || bytes[0] != b'"' || bytes[65] != b'"' {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "expected checksum ETag",
            ));
        }
        let digit = |b| match b {
            b'0'..=b'9' => Ok(b - b'0'),
            b'a'..=b'f' => Ok(b - b'a' + 10),
            _ => Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "noncanonical checksum ETag",
            )),
        };
        let mut checksum = [0; 32];
        for (out, pair) in checksum.iter_mut().zip(bytes[1..65].chunks_exact(2)) {
            *out = digit(pair[0])? * 16 + digit(pair[1])?;
        }
        Ok(Self(checksum))
    }

    pub fn etag(self) -> ETag {
        let mut out = [b'"'; 66];
        let digits = b"0123456789abcdef";
        for (i, byte) in self.0.iter().enumerate() {
            out[1 + i * 2] = digits[(byte >> 4) as usize];
            out[2 + i * 2] = digits[(byte & 15) as usize];
        }
        ETag(out)
    }
}

/// Stack-owned HTTP rendering, created only at transport boundaries.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct ETag([u8; 66]);
impl ETag {
    pub fn as_str(&self) -> &str {
        std::str::from_utf8(&self.0).unwrap()
    }
}

/// Fixed 48-byte record. Expiry is Unix seconds; zero is request-scoped, never reusable.
/// Encoding is checksum bytes followed by little-endian length and expiry.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
#[repr(C)]
pub struct Metadata {
    pub checksum: Checksum,
    pub len: u64,
    pub expires: u64,
}
impl Metadata {
    pub const SIZE: usize = 48;

    pub fn to_bytes(self) -> [u8; Self::SIZE] {
        let mut bytes = [0; Self::SIZE];
        bytes[..32].copy_from_slice(&self.checksum.0);
        bytes[32..40].copy_from_slice(&self.len.to_le_bytes());
        bytes[40..48].copy_from_slice(&self.expires.to_le_bytes());
        bytes
    }

    pub fn from_bytes(bytes: &[u8]) -> io::Result<Self> {
        if bytes.len() != Self::SIZE {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "invalid metadata length",
            ));
        }
        Ok(Self {
            checksum: Checksum(bytes[..32].try_into().unwrap()),
            len: u64::from_le_bytes(bytes[32..40].try_into().unwrap()),
            expires: u64::from_le_bytes(bytes[40..48].try_into().unwrap()),
        })
    }
}

#[cfg(test)]
#[path = "../tests/storage/metadata.rs"]
mod tests;
