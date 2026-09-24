// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Opaque origin input. Never part of content identity or connection state.
use std::{
    io,
    sync::{Arc, OnceLock},
};

pub const MAX_ORIGIN_DATA: usize = 64 * 1024;
pub const MAX_ENCODED_ORIGIN_DATA: usize = MAX_ORIGIN_DATA.div_ceil(3) * 4;
pub(crate) const HTTP_ORIGIN_DATA_OVERHEAD: usize = 21;

#[derive(Clone, Default)]
pub struct OriginData(Option<Arc<Secret>>);
struct Secret {
    raw: zeroize::Zeroizing<Vec<u8>>,
    encoded: zeroize::Zeroizing<String>,
}
impl OriginData {
    pub fn new(value: &[u8]) -> io::Result<Self> {
        if value.len() > MAX_ORIGIN_DATA {
            return Err(crate::http::invalid("origin data exceeds 64 KiB"));
        }
        if value.is_empty() {
            return Ok(Self::default());
        }
        Ok(Self(Some(Arc::new(Secret {
            raw: zeroize::Zeroizing::new(value.to_vec()),
            encoded: zeroize::Zeroizing::new(openssl::base64::encode_block(value)),
        }))))
    }
    pub fn from_encoded(value: &str) -> io::Result<Self> {
        if value.len() > MAX_ENCODED_ORIGIN_DATA {
            return Err(crate::http::invalid("origin data exceeds 64 KiB"));
        }
        let raw = zeroize::Zeroizing::new(
            openssl::base64::decode_block(value)
                .map_err(|_| crate::http::invalid("invalid Racer-Origin-Data"))?,
        );
        let data = Self::new(&raw)?;
        if data.encoded().unwrap_or("") != value {
            return Err(crate::http::invalid("noncanonical Racer-Origin-Data"));
        }
        Ok(data)
    }
    pub fn from_headers(headers: crate::http::Headers<'_>) -> io::Result<Self> {
        crate::cache::http_metadata::text(headers, "racer-origin-data")?
            .map(Self::from_encoded)
            .transpose()
            .map(Option::unwrap_or_default)
    }
    pub fn as_bytes(&self) -> &[u8] {
        self.0.as_ref().map_or(&[], |value| value.raw.as_slice())
    }
    pub fn encoded(&self) -> Option<&str> {
        self.0.as_ref().map(|value| value.encoded.as_str())
    }
    pub(crate) fn fingerprint(&self) -> [u8; 32] {
        static KEY: OnceLock<[u8; 32]> = OnceLock::new();
        let Some(value) = &self.0 else {
            return [0; 32];
        };
        let key = KEY.get_or_init(|| {
            let mut key = [0; 32];
            getrandom::getrandom(&mut key).expect("process origin data fingerprint entropy");
            key
        });
        *blake3::keyed_hash(key, &value.raw).as_bytes()
    }
}

/// Request binding is channel-local, never persisted. Length framing separates
/// origin data from the descriptor; the nonce is supplied by the HTTP adapter.
pub(crate) fn binding(descriptor: &[u8], data: &OriginData) -> blake3::Hash {
    let mut hash = blake3::Hasher::new();
    hash.update(b"racer/request-binding/origin-data/v1");
    hash.update(&(descriptor.len() as u32).to_le_bytes());
    hash.update(descriptor);
    let value = data.as_bytes();
    hash.update(&(value.len() as u32).to_le_bytes());
    hash.update(value);
    hash.finalize()
}

/// RO01 is only carried inside the encrypted TLS RDMA control channel.
pub(crate) fn rdma_envelope(descriptor: &[u8], data: &OriginData) -> Option<Vec<u8>> {
    let value = data.as_bytes();
    if descriptor.len() > crate::cache::MAX_PEER_INPUT
        || 8 + descriptor.len() + value.len() > crate::rdma::MAX_METADATA
    {
        return None;
    }
    let mut out = b"RO01".to_vec();
    out.extend((descriptor.len() as u16).to_le_bytes());
    out.extend((value.len() as u16).to_le_bytes());
    out.extend(descriptor);
    out.extend(value);
    Some(out)
}
pub(crate) fn rdma_decode(bytes: &[u8]) -> io::Result<(&[u8], OriginData)> {
    if bytes.len() < 8 || bytes.len() > crate::rdma::MAX_METADATA || &bytes[..4] != b"RO01" {
        return Err(crate::http::protocol("invalid request envelope"));
    }
    let d = u16::from_le_bytes(bytes[4..6].try_into().unwrap()) as usize;
    let a = u16::from_le_bytes(bytes[6..8].try_into().unwrap()) as usize;
    if d > crate::cache::MAX_PEER_INPUT || bytes.len() != 8 + d + a {
        return Err(crate::http::protocol("invalid request envelope lengths"));
    }
    let data = OriginData::new(&bytes[8 + d..])?;
    Ok((&bytes[8..8 + d], data))
}

#[cfg(test)]
#[path = "../tests/security/origin_data.rs"]
mod tests;
