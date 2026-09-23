// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Explicitly bounded singleton response metadata, never arbitrary headers.
use std::io;

#[derive(Clone, Copy, PartialEq, Eq)]
pub struct HeaderValue<const N: usize> {
    len: u16,
    bytes: [u8; N],
}
impl<const N: usize> Default for HeaderValue<N> {
    fn default() -> Self {
        Self {
            len: 0,
            bytes: [0; N],
        }
    }
}
impl<const N: usize> std::fmt::Debug for HeaderValue<N> {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("HeaderValue")
            .field("len", &self.len)
            .finish()
    }
}
impl<const N: usize> HeaderValue<N> {
    pub fn new(value: &[u8]) -> io::Result<Self> {
        if value.is_empty()
            || value.len() > N
            || value.len() > u16::MAX as usize
            || !crate::http::value(value)
            || crate::http::trim(value) != value
        {
            return Err(crate::http::protocol("invalid bounded response header"));
        }
        let mut out = Self::default();
        out.len = value.len() as u16;
        out.bytes[..value.len()].copy_from_slice(value);
        Ok(out)
    }
    pub fn as_bytes(&self) -> Option<&[u8]> {
        (self.len != 0).then_some(&self.bytes[..self.len as usize])
    }
    pub(crate) fn parse(headers: crate::http::Headers<'_>, name: &str) -> io::Result<Self> {
        let mut fields = headers.iter().filter(|(n, _)| n.eq_ignore_ascii_case(name));
        let value = fields
            .next()
            .map(|(_, v)| Self::new(v))
            .transpose()?
            .unwrap_or_default();
        if fields.next().is_some() {
            return Err(crate::http::protocol("duplicate response metadata"));
        }
        Ok(value)
    }
    pub(crate) fn encode(&self, out: &mut [u8]) {
        assert_eq!(out.len(), N + 2);
        out[..2].copy_from_slice(&self.len.to_le_bytes());
        out[2..].copy_from_slice(&self.bytes);
    }
    pub(crate) fn decode(bytes: &[u8]) -> io::Result<Self> {
        if bytes.len() != N + 2 {
            return Err(crate::http::protocol("invalid header record"));
        }
        let len = u16::from_le_bytes(bytes[..2].try_into().unwrap()) as usize;
        if len > N || bytes[2 + len..].iter().any(|b| *b != 0) {
            return Err(crate::http::protocol("invalid header record padding"));
        }
        if len == 0 {
            Ok(Self::default())
        } else {
            Self::new(&bytes[2..2 + len])
        }
    }
}
