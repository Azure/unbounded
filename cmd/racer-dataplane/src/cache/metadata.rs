// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Bounded peer descriptors and HTTP metadata/page fact parsing.
use super::*;

/// Transport-independent RD01 metadata descriptors and bounded hexadecimal framing.
pub(crate) mod peer_wire {
    use super::{Checksum, PeerDescriptor, PeerPage, UpstreamRequest};
    use std::{io, time::Duration};

    pub(crate) const MAX_DESCRIPTOR: usize = super::MAX_PEER_INPUT;
    pub(crate) const MAX_CANDIDATE: Duration = Duration::from_secs(10);
    pub(crate) const MAX_HOPS: u8 = 8;
    pub(crate) const MAX_WORK: u8 = 255;
    pub(crate) const CHAIN_LEN: usize = 42;

    /// RC01 binds immutable storage namespace and affine forwarding allowances.
    pub(crate) fn chain(bytes: &[u8]) -> io::Result<Option<([u8; 32], u8, u8, u32)>> {
        if !bytes.starts_with(b"RC01") {
            return Ok(None);
        }
        if bytes.len() < CHAIN_LEN + 14
            || bytes.len() > MAX_DESCRIPTOR
            || bytes[36] > MAX_HOPS
            || !bytes[CHAIN_LEN..].starts_with(b"RB01")
        {
            return Err(invalid("invalid request chain"));
        }
        Ok(Some((
            bytes[4..36].try_into().unwrap(),
            bytes[36],
            bytes[37],
            u32::from_le_bytes(bytes[38..42].try_into().unwrap()),
        )))
    }
    pub(crate) fn with_chain(
        bytes: Vec<u8>,
        namespace: [u8; 32],
        hops: u8,
        work: u8,
        candidate: u32,
    ) -> io::Result<Vec<u8>> {
        let mut out = b"RC01".to_vec();
        out.extend(namespace);
        out.extend([hops, work]);
        out.extend(candidate.to_le_bytes());
        out.extend(bytes);
        chain(&out)?;
        Ok(out)
    }
    // Benchmark fidelity: keep budget framing shared with bench/fixture.rs.
    pub(crate) fn with_budget(bytes: Vec<u8>, remaining: Duration) -> io::Result<Vec<u8>> {
        let ms = remaining.min(MAX_CANDIDATE).as_millis() as u32;
        if ms == 0 {
            return Err(io::Error::new(
                io::ErrorKind::TimedOut,
                "peer budget exhausted",
            ));
        }
        let mut out = b"RB01".to_vec();
        out.extend(ms.to_le_bytes());
        out.extend(bytes);
        if out.len() > MAX_DESCRIPTOR {
            return Err(invalid("fault descriptor too large"));
        }
        Ok(out)
    }
    /// Exact RD01 plus optional RR01 cursor and RB01 budget size.
    pub(crate) fn encoded_len(target: usize, page: bool, routed: bool, budget: bool) -> usize {
        target
            .saturating_add(if page { 311 } else { 5 })
            .saturating_add(if routed {
                4 + crate::routing::Cursor::LEN
            } else {
                0
            })
            .saturating_add(if budget { 8 } else { 0 })
    }
    /// Client admission reserves the largest supported page framing even for
    /// HEAD and local owners, so ownership changes cannot
    /// change whether a representation is supported by a distributed volume.
    pub(crate) fn client_fits(target: usize) -> bool {
        encoded_len(target, true, true, true).saturating_add(CHAIN_LEN) <= MAX_DESCRIPTOR
    }
    pub(crate) fn request_len(
        request: &UpstreamRequest,
        routed: bool,
        budget: bool,
    ) -> io::Result<usize> {
        let (target, page) = match request {
            UpstreamRequest::PeerMetadata(m) => (m.target().len(), false),
            UpstreamRequest::PeerPage(p) => (p.target().len(), true),
            _ => return Err(invalid("not a peer request")),
        };
        Ok(encoded_len(target, page, routed, budget))
    }
    // RB01 budgets cover RD01/RR01 and are bound by authenticated transports.
    // Relative milliseconds are floored/capped; ingress retains the absolute cap.
    pub(crate) fn budget_descriptor(bytes: &[u8]) -> io::Result<(&[u8], Option<Duration>)> {
        let bytes = if chain(bytes)?.is_some() {
            &bytes[CHAIN_LEN..]
        } else {
            bytes
        };
        if !bytes.starts_with(b"RB01") {
            return Err(invalid("missing peer budget descriptor"));
        }
        if bytes.len() > MAX_DESCRIPTOR || bytes.len() < 14 {
            return Err(invalid("short budget descriptor"));
        }
        let ms = u32::from_le_bytes(bytes[4..8].try_into().unwrap());
        if ms == 0 || bytes[8..].starts_with(b"RB01") {
            return Err(invalid("invalid budget descriptor"));
        }
        Ok((
            &bytes[8..],
            Some(Duration::from_millis(u64::from(ms)).min(MAX_CANDIDATE)),
        ))
    }
    pub(crate) fn routed_descriptor(
        bytes: &[u8],
    ) -> super::Result<(Option<crate::routing::Cursor>, PeerDescriptor<'_>)> {
        let (bytes, _) = budget_descriptor(bytes)?;
        if bytes.starts_with(b"RR01") {
            if bytes.len() > MAX_DESCRIPTOR || bytes.len() < 4 + crate::routing::Cursor::LEN {
                return Err(invalid("short routed descriptor").into());
            }
            let end = 4 + crate::routing::Cursor::LEN;
            Ok((
                Some(crate::routing::Cursor::decode(&bytes[4..end])?),
                decode_descriptor(&bytes[end..])?,
            ))
        } else {
            Ok((None, decode_descriptor(bytes)?))
        }
    }
    fn invalid(message: &'static str) -> io::Error {
        io::Error::new(io::ErrorKind::InvalidData, message)
    }
    pub(crate) fn descriptor(request: &UpstreamRequest) -> io::Result<Vec<u8>> {
        if request_len(request, false, false)? > MAX_DESCRIPTOR {
            return Err(invalid("fault descriptor too large"));
        }
        let mut out = Vec::from(b"RD01".as_slice());
        let target = match request {
            UpstreamRequest::PeerMetadata(meta) => {
                out.push(0);
                meta.target()
            }
            UpstreamRequest::PeerPage(page) => {
                out.push(1);
                out.extend_from_slice(&page.offset().to_le_bytes());
                out.extend_from_slice(&page.object_len().to_le_bytes());
                out.extend_from_slice(page.version());
                let mut content_type = [0; 258];
                page.content_type().encode(&mut content_type);
                out.extend(content_type);
                page.target()
            }
            _ => return Err(invalid("not a peer request")),
        };
        out.extend_from_slice(target.as_bytes());
        if out.len() > MAX_DESCRIPTOR {
            return Err(invalid("fault descriptor too large"));
        }
        Ok(out)
    }
    pub(crate) fn decode_descriptor(bytes: &[u8]) -> super::Result<PeerDescriptor<'_>> {
        if bytes.len() < 6 || bytes.len() > MAX_DESCRIPTOR || &bytes[..4] != b"RD01" {
            return Err(invalid("invalid fault descriptor").into());
        }
        let utf8 = |bytes| std::str::from_utf8(bytes).map_err(|_| invalid("non-UTF8 descriptor"));
        if bytes[4] == 0 {
            return Ok(PeerDescriptor::metadata(utf8(&bytes[5..])?));
        }
        if bytes[4] != 1 || bytes.len() < 312 {
            return Err(invalid("invalid page descriptor").into());
        }
        Ok(PeerDescriptor::page(
            utf8(&bytes[311..])?,
            PeerPage::new(
                u64::from_le_bytes(bytes[5..13].try_into().unwrap()),
                u64::from_le_bytes(bytes[13..21].try_into().unwrap()),
                Checksum(bytes[21..53].try_into().unwrap()),
            )
            .with_content_type(crate::metadata::ContentType::decode(&bytes[53..311])?),
        ))
    }
    pub(crate) fn hex(bytes: &[u8]) -> String {
        const DIGITS: &[u8] = b"0123456789abcdef";
        let mut out = String::with_capacity(bytes.len() * 2);
        for b in bytes {
            out.push(DIGITS[(b >> 4) as usize] as char);
            out.push(DIGITS[(b & 15) as usize] as char);
        }
        out
    }
    pub(crate) fn unhex(value: &str) -> io::Result<Vec<u8>> {
        if !value.len().is_multiple_of(2) || value.len() > MAX_DESCRIPTOR * 2 {
            return Err(invalid("invalid hex length"));
        }
        value
            .as_bytes()
            .chunks_exact(2)
            .map(|pair| {
                let a = (pair[0] as char)
                    .to_digit(16)
                    .ok_or_else(|| invalid("invalid hex"))?;
                let b = (pair[1] as char)
                    .to_digit(16)
                    .ok_or_else(|| invalid("invalid hex"))?;
                Ok((a * 16 + b) as u8)
            })
            .collect()
    }

    #[cfg(test)]
    include!(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/tests/storage/peer_wire.rs"
    ));
}

/// HTTP boundary parsing for cache metadata and page facts. These helpers only
/// decode wire facts; cache still validates identities and controls publication.
pub(crate) mod http_metadata {
    use super::Checksum;
    use crate::{cache, http::Headers, http_client as client};
    use std::io;

    fn invalid(message: &'static str) -> io::Error {
        io::Error::new(io::ErrorKind::InvalidData, message)
    }
    pub(crate) fn text<'a>(headers: Headers<'a>, name: &str) -> io::Result<Option<&'a str>> {
        let mut fields = headers.iter().filter(|(n, _)| n.eq_ignore_ascii_case(name));
        let value = fields
            .next()
            .map(|(_, v)| std::str::from_utf8(v).map_err(|_| invalid("non-UTF8 header")))
            .transpose()?;
        if fields.next().is_some() {
            return Err(invalid("duplicate singleton header"));
        }
        Ok(value)
    }
    pub(crate) fn decimal(value: &str) -> io::Result<u64> {
        if value.is_empty() || !value.bytes().all(|b| b.is_ascii_digit()) {
            return Err(invalid("invalid unsigned decimal"));
        }
        value.parse().map_err(|_| invalid("decimal overflow"))
    }
    fn directive_value(value: &str) -> io::Result<()> {
        if let Some(quoted) = value.strip_prefix('"') {
            let quoted = quoted
                .strip_suffix('"')
                .ok_or_else(|| invalid("invalid quoted directive"))?;
            let mut escaped = false;
            for b in quoted.bytes() {
                if escaped {
                    if b != b'\t' && !(32..=126).contains(&b) && b < 128 {
                        return Err(invalid("invalid quoted escape"));
                    }
                    escaped = false;
                } else if b == b'\\' {
                    escaped = true;
                } else if b == b'"' || (b < 32 && b != b'\t') || b == 127 {
                    return Err(invalid("invalid quoted directive"));
                }
            }
            if escaped {
                return Err(invalid("invalid quoted escape"));
            }
        } else if value.is_empty() || !value.bytes().all(crate::http::token) {
            return Err(invalid("invalid directive value"));
        }
        Ok(())
    }
    fn policy(headers: Headers<'_>) -> io::Result<cache::CachePolicy> {
        let mut facts = cache::CachePolicy::default();
        for (_, value) in headers
            .iter()
            .filter(|(n, _)| n.eq_ignore_ascii_case("cache-control"))
        {
            // Commas inside quoted extension/field-list values do not delimit directives.
            let value = std::str::from_utf8(value).map_err(|_| invalid("invalid Cache-Control"))?;
            let mut quoted = false;
            let mut escaped = false;
            let mut start = 0;
            for (i, b) in value.bytes().chain([b',']).enumerate() {
                if escaped {
                    escaped = false;
                    continue;
                }
                if quoted && b == b'\\' {
                    escaped = true;
                    continue;
                }
                if b == b'"' {
                    quoted = !quoted;
                }
                if b != b',' || quoted {
                    continue;
                }
                let part = value[start..i].trim();
                start = i + 1;
                let (name, value) = part
                    .split_once('=')
                    .map_or((part, None), |(n, v)| (n.trim(), Some(v.trim())));
                if name.is_empty() || !name.bytes().all(crate::http::token) {
                    return Err(invalid("invalid cache directive"));
                }
                if let Some(value) = value {
                    directive_value(value)?;
                }
                if ["no-cache", "no-store", "private"]
                    .iter()
                    .any(|n| name.eq_ignore_ascii_case(n))
                {
                    facts.disabled = true;
                }
                let slot = if name.eq_ignore_ascii_case("max-age") {
                    &mut facts.max_age
                } else if name.eq_ignore_ascii_case("s-maxage") {
                    &mut facts.shared_max_age
                } else {
                    continue;
                };
                let value = value.ok_or_else(|| invalid("missing max-age"))?;
                let value = if value.starts_with('"') {
                    value
                        .strip_prefix('"')
                        .and_then(|v| v.strip_suffix('"'))
                        .ok_or_else(|| invalid("invalid max-age quote"))?
                } else {
                    value
                };
                if slot.replace(decimal(value)?).is_some() {
                    return Err(invalid("duplicate max-age"));
                }
            }
            if quoted || escaped {
                return Err(invalid("unterminated cache directive"));
            }
        }
        facts.age = decimal(text(headers, "age")?.unwrap_or("0"))?;
        Ok(facts)
    }
    fn content_range(value: &str) -> cache::Result<cache::ContentRange> {
        let value = value
            .strip_prefix("bytes ")
            .ok_or_else(|| invalid("invalid Content-Range"))?;
        let (interval, total) = value
            .split_once('/')
            .ok_or_else(|| invalid("invalid Content-Range"))?;
        let (start, end) = interval
            .split_once('-')
            .ok_or_else(|| invalid("invalid Content-Range"))?;
        cache::ContentRange::new(decimal(start)?, decimal(end)?, decimal(total)?)
    }
    #[derive(Debug)]
    pub(crate) struct HttpStatus(pub(crate) u16, pub(crate) crate::outcome::ResponseMetadata);
    impl std::fmt::Display for HttpStatus {
        fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
            write!(f, "unexpected upstream HTTP status {}", self.0)
        }
    }
    impl std::error::Error for HttpStatus {}
    #[cfg(test)]
    pub(crate) fn status(status: u16) -> cache::Error {
        match status {
            404 => cache::Error::NotFound,
            410 => cache::Error::Gone,
            412 => cache::Error::Precondition,
            _ => HttpStatus(status, Default::default()).into(),
        }
    }
    pub(crate) fn response_status(code: u16, headers: Headers<'_>) -> cache::Result<cache::Error> {
        Ok(HttpStatus(
            code,
            crate::outcome::ResponseMetadata {
                challenge: crate::header_value::HeaderValue::parse(headers, "www-authenticate")?,
                retry_after: crate::header_value::HeaderValue::parse(headers, "retry-after")?,
            },
        )
        .into())
    }
    pub(crate) fn metadata_facts(
        response: &client::HeadResponse,
    ) -> cache::Result<cache::BackendMetadata> {
        if response.status() != 200 {
            return Err(response_status(response.status(), response.headers())?);
        }
        identity_encoding(response.headers())?;
        Ok(cache::BackendMetadata {
            len: response
                .content_length()
                .ok_or_else(|| invalid("metadata needs Content-Length"))?,
            checksum: representation_checksum(response.headers())?,
            policy: policy(response.headers())?,
            content_type: crate::metadata::ContentType::parse(response.headers(), "content-type")?,
        })
    }
    pub(crate) fn page_facts(
        status_code: u16,
        headers: Headers<'_>,
    ) -> cache::Result<cache::BackendPage> {
        if !matches!(status_code, 200 | 206) {
            return Err(response_status(status_code, headers)?);
        }
        let range = text(headers, "content-range")?
            .map(content_range)
            .transpose()?;
        if (status_code == 206) != range.is_some() {
            return Err(cache::Error::InvalidData("Content-Range/status mismatch"));
        }
        Ok(cache::BackendPage {
            range,
            checksum: representation_checksum(headers)?,
            content_type: crate::metadata::ContentType::parse(headers, "content-type")?,
        })
    }
    pub(crate) fn representation_checksum(headers: Headers<'_>) -> cache::Result<Checksum> {
        Ok(Checksum::from_etag(
            text(headers, "etag")?.ok_or_else(|| invalid("missing checksum ETag"))?,
        )?)
    }
    pub(crate) fn peer_checksum(headers: Headers<'_>, expected: Checksum) -> cache::Result<()> {
        if representation_checksum(headers)? != expected {
            return Err(invalid("peer checksum ETag mismatch").into());
        }
        Ok(())
    }
    pub(crate) fn checksum(headers: Headers<'_>) -> io::Result<Option<u64>> {
        text(headers, "x-racer-crc64")?
            .map(|value| {
                if value.is_empty()
                    || value.len() > 16
                    || !value.bytes().all(|b| b.is_ascii_hexdigit())
                {
                    return Err(invalid("invalid CRC64"));
                }
                u64::from_str_radix(value, 16).map_err(|_| invalid("invalid CRC64"))
            })
            .transpose()
    }
    pub(crate) fn identity_encoding(headers: Headers<'_>) -> io::Result<()> {
        if text(headers, "content-encoding")?.is_some_and(|v| !v.eq_ignore_ascii_case("identity")) {
            return Err(invalid("encoded upstream representation"));
        }
        Ok(())
    }

    #[cfg(test)]
    include!(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/tests/storage/http_metadata.rs"
    ));
}
