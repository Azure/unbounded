// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Representation identity shared by storage and transport. Payload CRCs are
//! independent of this origin-supplied checksum; the dataplane does not hash objects.
use std::io;
use std::rc::Rc;

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

pub type ContentType = crate::header_value::HeaderValue<256>;

/// Fixed 306-byte encoding: checksum, LE length/expiry, LE u16 Content-Type
/// length and 256 zero-padded bytes. Zero expiry is never reusable.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
#[repr(C)]
pub struct Metadata {
    pub checksum: Checksum,
    pub len: u64,
    pub expires: u64,
    pub content_type: ContentType,
}
impl Metadata {
    pub const SIZE: usize = 306;

    pub fn to_bytes(self) -> [u8; Self::SIZE] {
        let mut bytes = [0; Self::SIZE];
        bytes[..32].copy_from_slice(&self.checksum.0);
        bytes[32..40].copy_from_slice(&self.len.to_le_bytes());
        bytes[40..48].copy_from_slice(&self.expires.to_le_bytes());
        self.content_type.encode(&mut bytes[48..]);
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
            content_type: ContentType::decode(&bytes[48..])?,
        })
    }
}

/// Worker-local tree representation. Keep the bounded, Copy transport record
/// above at API boundaries, but do not reserve 256 header bytes in every leaf
/// slot (including spare capacity and payload slots). Missing headers allocate
/// nothing; present headers retain only their bytes and share them on CoW clone.
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct ResidentMetadata {
    pub checksum: Checksum,
    pub len: u64,
    pub expires: u64,
    content_type: Option<Rc<[u8]>>,
}

impl From<Metadata> for ResidentMetadata {
    fn from(value: Metadata) -> Self {
        Self {
            checksum: value.checksum,
            len: value.len,
            expires: value.expires,
            content_type: value.content_type.as_bytes().map(Rc::from),
        }
    }
}

impl ResidentMetadata {
    pub(crate) fn to_metadata(&self) -> Metadata {
        Metadata {
            checksum: self.checksum,
            len: self.len,
            expires: self.expires,
            content_type: self
                .content_type
                .as_deref()
                .map(|bytes| ContentType::new(bytes).expect("validated resident Content-Type"))
                .unwrap_or_default(),
        }
    }

    pub(crate) fn to_bytes(&self) -> [u8; Metadata::SIZE] {
        self.to_metadata().to_bytes()
    }

    /// Rc allocation layout includes two reference counts and alignment padding.
    pub(crate) fn header_allocation_bytes(len: usize) -> u64 {
        let word = std::mem::size_of::<usize>();
        (2 * word + len.next_multiple_of(word)) as u64
    }

    #[cfg(test)]
    pub(crate) fn header_allocation(&self) -> Option<(usize, u64)> {
        self.content_type.as_ref().map(|value| {
            (
                value.as_ptr() as usize,
                Self::header_allocation_bytes(value.len()),
            )
        })
    }
}

#[cfg(test)]
#[path = "../tests/storage/metadata.rs"]
mod tests;
