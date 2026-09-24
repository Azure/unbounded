// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Small shared HTTP/1.1 wire primitives. Payload policy belongs to callers.

pub use crate::uring::Progress;
use std::io;

pub(crate) const SCRATCH_SIZE: usize = 8192;
pub(crate) const MAX_HEADERS: usize = 64;

pub(crate) fn invalid(message: &'static str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidInput, message)
}
pub(crate) fn protocol(message: &'static str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, message)
}
pub(crate) fn token(byte: u8) -> bool {
    byte.is_ascii_alphanumeric() || b"!#$%&'*+-.^_`|~".contains(&byte)
}
pub(crate) fn value(bytes: &[u8]) -> bool {
    bytes.iter().all(|&b| b == b'\t' || (b >= 32 && b != 127))
}
pub(crate) fn target(bytes: &[u8]) -> bool {
    bytes.starts_with(b"/") && bytes.iter().all(|b| (33..=126).contains(b) && *b != b'#')
}
pub(crate) fn authority(bytes: &[u8]) -> bool {
    !bytes.is_empty()
        && bytes
            .iter()
            .all(|b| (33..=126).contains(b) && !b"/\\?#@,".contains(b))
}
pub(crate) fn trim(mut bytes: &[u8]) -> &[u8] {
    while matches!(bytes.first(), Some(b' ' | b'\t')) {
        bytes = &bytes[1..];
    }
    while matches!(bytes.last(), Some(b' ' | b'\t')) {
        bytes = &bytes[..bytes.len() - 1];
    }
    bytes
}
pub(crate) fn decimal(bytes: &[u8]) -> io::Result<u64> {
    if bytes.is_empty() {
        return Err(protocol("empty decimal"));
    }
    bytes.iter().try_fold(0u64, |n, &b| {
        if !b.is_ascii_digit() {
            return Err(protocol("invalid decimal"));
        }
        n.checked_mul(10)
            .and_then(|n| n.checked_add((b - b'0') as u64))
            .ok_or_else(|| protocol("decimal overflow"))
    })
}

#[derive(Clone, Copy, Default)]
pub(crate) struct Span {
    pub(crate) start: usize,
    pub(crate) end: usize,
}
impl Span {
    pub(crate) fn slice(self, bytes: &[u8]) -> &[u8] {
        &bytes[self.start as usize..self.end as usize]
    }
}
#[derive(Clone, Copy, Default)]
pub(crate) struct Header {
    pub(crate) name: Span,
    pub(crate) value: Span,
}

/// Borrowed header views. Names compare ASCII-insensitively; values are bytes.
/// Duplicate fields remain visible through `iter`.
#[derive(Clone, Copy)]
pub struct Headers<'a> {
    pub(crate) bytes: &'a [u8],
    pub(crate) headers: &'a [Header],
}
impl<'a> Headers<'a> {
    pub fn iter(self) -> impl Iterator<Item = (&'a str, &'a [u8])> {
        self.headers.iter().map(move |h| {
            (
                std::str::from_utf8(h.name.slice(self.bytes)).expect("validated ASCII name"),
                h.value.slice(self.bytes),
            )
        })
    }
    pub fn get(self, name: &str) -> Option<&'a [u8]> {
        self.iter()
            .find(|(n, _)| n.eq_ignore_ascii_case(name))
            .map(|(_, v)| v)
    }
}

pub(crate) fn transfer(n: usize, remaining: usize) -> io::Result<usize> {
    if n == 0 {
        Err(io::Error::new(
            io::ErrorKind::UnexpectedEof,
            "incomplete HTTP exchange",
        ))
    } else if n > remaining {
        Err(protocol("I/O exceeded requested range"))
    } else {
        Ok(n)
    }
}

// Scan each byte at most once, retaining three boundary bytes across receives.
pub(crate) fn header_end(bytes: &[u8], scan: &mut usize) -> Option<usize> {
    while *scan + 4 <= bytes.len() {
        let i = *scan;
        *scan += 1;
        if bytes[i..i + 4] == *b"\r\n\r\n" {
            return Some(i + 4);
        }
    }
    None
}
pub(crate) fn line(bytes: &[u8], start: usize, end: usize) -> io::Result<usize> {
    bytes[start..end]
        .windows(2)
        .position(|s| s == b"\r\n")
        .map(|n| start + n)
        .ok_or_else(|| protocol("unterminated HTTP line"))
}

pub(crate) fn field(bytes: &[u8], start: usize, stop: usize) -> io::Result<Header> {
    let colon = bytes[start..stop]
        .iter()
        .position(|&b| b == b':')
        .map(|n| n + start)
        .ok_or_else(|| protocol("missing header colon"))?;
    let name = &bytes[start..colon];
    let raw = &bytes[colon + 1..stop];
    if name.is_empty() || !name.iter().copied().all(token) || !value(raw) {
        return Err(protocol("invalid HTTP header"));
    }
    let v = trim(raw);
    let vstart = v.as_ptr() as usize - bytes.as_ptr() as usize;
    Ok(Header {
        name: Span { start, end: colon },
        value: Span {
            start: vstart,
            end: vstart + v.len(),
        },
    })
}

/// Account partial request headers before growing receive storage. Only one
/// Racer-Origin-Data field has a separate budget; all other bytes remain at 8 KiB.
pub(crate) fn request_budget(bytes: &[u8]) -> Result<(), u16> {
    let mut normal = 0;
    let mut data = false;
    let mut start = 0;
    let mut first = true;
    while start < bytes.len() {
        let end = bytes[start..]
            .windows(2)
            .position(|p| p == b"\r\n")
            .map(|n| start + n);
        let stop = end.unwrap_or(bytes.len());
        let line = &bytes[start..stop];
        let origin_data =
            !first && line.len() >= 18 && line[..18].eq_ignore_ascii_case(b"racer-origin-data:");
        // A split field name must not consume the normal budget before we can
        // identify its independently bounded value.
        if !first
            && end.is_none()
            && line.len() < 18
            && b"racer-origin-data:"[..line.len()].eq_ignore_ascii_case(line)
        {
            break;
        }
        if origin_data {
            if data {
                return Err(400);
            }
            data = true;
            let raw = &line[18..];
            let value = raw.strip_prefix(b" ").unwrap_or(raw);
            let value = if end.is_none() {
                value.strip_suffix(b"\r").unwrap_or(value)
            } else {
                value
            };
            if value.len() > crate::origin_data::MAX_ENCODED_ORIGIN_DATA {
                return Err(431);
            }
            if end.is_some() {
                let encoded = std::str::from_utf8(value).map_err(|_| 400u16)?;
                crate::origin_data::OriginData::from_encoded(encoded).map_err(|_| 400u16)?;
            }
        } else {
            normal += line.len() + usize::from(end.is_some()) * 2;
            if normal > SCRATCH_SIZE {
                return Err(431);
            }
        }
        first = false;
        if line.is_empty() && end.is_some() {
            break;
        }
        start = end.map_or(bytes.len(), |end| end + 2);
    }
    Ok(())
}

pub(crate) fn connection_close(bytes: &[u8]) -> io::Result<bool> {
    let mut close = false;
    for part in bytes.split(|&b| b == b',') {
        let part = trim(part);
        if part.eq_ignore_ascii_case(b"close") {
            close = true;
        } else if !part.eq_ignore_ascii_case(b"keep-alive") {
            return Err(protocol("unsupported Connection option"));
        }
    }
    Ok(close)
}
