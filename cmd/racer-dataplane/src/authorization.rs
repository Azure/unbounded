// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Opaque request credentials. Never part of content identity or connection state.
use std::{
    io,
    sync::{Arc, OnceLock},
};

pub const MAX_AUTHORIZATION: usize = 64 * 1024;
pub(crate) const HTTP_AUTH_OVERHEAD: usize = 17;

#[derive(Clone, Default)]
pub struct Authorization(Option<Arc<Secret>>);
struct Secret(zeroize::Zeroizing<String>);
impl Authorization {
    pub fn new(value: &str) -> io::Result<Self> {
        if value.is_empty()
            || value.len() > MAX_AUTHORIZATION
            || !value.bytes().all(|b| (32..=126).contains(&b))
            || value.trim() != value
        {
            return Err(crate::http::invalid("invalid Authorization"));
        }
        Ok(Self(Some(Arc::new(Secret(zeroize::Zeroizing::new(
            value.to_owned(),
        ))))))
    }
    pub fn from_headers(headers: crate::http::Headers<'_>) -> io::Result<Self> {
        crate::cache::http_metadata::text(headers, "authorization")?
            .map(Self::new)
            .transpose()
            .map(Option::unwrap_or_default)
    }
    pub fn as_str(&self) -> Option<&str> {
        self.0.as_ref().map(|value| value.0.as_str())
    }
    pub(crate) fn fingerprint(&self) -> [u8; 32] {
        static KEY: OnceLock<[u8; 32]> = OnceLock::new();
        let Some(value) = self.as_str() else {
            return [0; 32];
        };
        let key = KEY.get_or_init(|| {
            let mut key = [0; 32];
            getrandom::getrandom(&mut key).expect("process credential fingerprint entropy");
            key
        });
        *blake3::keyed_hash(key, value.as_bytes()).as_bytes()
    }
}

/// Request binding is channel-local, never persisted. Length framing separates
/// credentials from the descriptor; the nonce is supplied by the HTTP adapter.
pub(crate) fn binding(descriptor: &[u8], auth: &Authorization) -> blake3::Hash {
    let mut hash = blake3::Hasher::new();
    hash.update(b"racer/request-binding/v2");
    hash.update(&(descriptor.len() as u32).to_le_bytes());
    hash.update(descriptor);
    let value = auth.as_str().unwrap_or("");
    hash.update(&(value.len() as u32).to_le_bytes());
    hash.update(value.as_bytes());
    hash.finalize()
}

/// RF07 is only carried inside the encrypted TLS RDMA control channel.
pub(crate) fn rdma_envelope(descriptor: &[u8], auth: &Authorization) -> Option<Vec<u8>> {
    let value = auth.as_str().unwrap_or("");
    if descriptor.len() > crate::cache::MAX_PEER_INPUT
        || 8 + descriptor.len() + value.len() > crate::rdma::MAX_METADATA
    {
        return None;
    }
    let mut out = b"RF07".to_vec();
    out.extend((descriptor.len() as u16).to_le_bytes());
    out.extend((value.len() as u16).to_le_bytes());
    out.extend(descriptor);
    out.extend(value.as_bytes());
    Some(out)
}
pub(crate) fn rdma_decode(bytes: &[u8]) -> io::Result<(&[u8], Authorization)> {
    if bytes.len() < 8 || bytes.len() > crate::rdma::MAX_METADATA || &bytes[..4] != b"RF07" {
        return Err(crate::http::protocol("invalid request envelope"));
    }
    let d = u16::from_le_bytes(bytes[4..6].try_into().unwrap()) as usize;
    let a = u16::from_le_bytes(bytes[6..8].try_into().unwrap()) as usize;
    if d > crate::cache::MAX_PEER_INPUT || bytes.len() != 8 + d + a {
        return Err(crate::http::protocol("invalid request envelope lengths"));
    }
    let auth = if a == 0 {
        Authorization::default()
    } else {
        Authorization::new(
            std::str::from_utf8(&bytes[8 + d..])
                .map_err(|_| crate::http::protocol("invalid Authorization"))?,
        )?
    };
    Ok((&bytes[8..8 + d], auth))
}

#[cfg(test)]
#[path = "../tests/security/authorization.rs"]
mod tests;
