// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! HTTP/1.1 response heads and the cached `Date` header.

use crate::object::{AZURE_VERSION, Api, Failure, fmt_u64, itoa_buf, status_text};
use std::cell::RefCell;
use std::time::{SystemTime, UNIX_EPOCH};

thread_local! {
    /// IMF-fixdate for the current second. Formatting a date per response is
    /// measurable at high request rates; it only changes once a second.
    static DATE: RefCell<(u64, [u8; 29])> = const { RefCell::new((u64::MAX, [0; 29])) };
}

/// Append `Date: <IMF-fixdate>\r\n`.
pub fn push_date(out: &mut Vec<u8>) {
    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |d| d.as_secs());
    DATE.with(|cell| {
        let mut cached = cell.borrow_mut();
        if cached.0 != now {
            *cached = (now, imf_fixdate(now));
        }
        out.extend_from_slice(b"Date: ");
        out.extend_from_slice(&cached.1);
        out.extend_from_slice(b"\r\n");
    });
}

/// Format Unix seconds as `Sun, 06 Nov 1994 08:49:37 GMT`.
pub fn imf_fixdate(secs: u64) -> [u8; 29] {
    const DAYS: [&[u8; 3]; 7] = [b"Thu", b"Fri", b"Sat", b"Sun", b"Mon", b"Tue", b"Wed"];
    const MONTHS: [&[u8; 3]; 12] = [
        b"Jan", b"Feb", b"Mar", b"Apr", b"May", b"Jun", b"Jul", b"Aug", b"Sep", b"Oct", b"Nov",
        b"Dec",
    ];
    let days = secs / 86400;
    let rem = secs % 86400;
    // Howard Hinnant's civil_from_days, for nonnegative day counts.
    let z = days as i64 + 719_468;
    let era = z / 146_097;
    let doe = z - era * 146_097;
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let day = (doy - (153 * mp + 2) / 5 + 1) as u8;
    let month = if mp < 10 { mp + 3 } else { mp - 9 } as usize;
    let year = (yoe + era * 400 + i64::from(month <= 2)) as u32;

    let mut b = [0u8; 29];
    b[..3].copy_from_slice(DAYS[(days % 7) as usize]);
    b[3..5].copy_from_slice(b", ");
    let two = |b: &mut [u8], n: u8| {
        b[0] = b'0' + n / 10;
        b[1] = b'0' + n % 10;
    };
    two(&mut b[5..7], day);
    b[7] = b' ';
    b[8..11].copy_from_slice(MONTHS[month - 1]);
    b[11] = b' ';
    b[12] = b'0' + (year / 1000 % 10) as u8;
    b[13] = b'0' + (year / 100 % 10) as u8;
    b[14] = b'0' + (year / 10 % 10) as u8;
    b[15] = b'0' + (year % 10) as u8;
    b[16] = b' ';
    two(&mut b[17..19], (rem / 3600) as u8);
    b[19] = b':';
    two(&mut b[20..22], (rem / 60 % 60) as u8);
    b[22] = b':';
    two(&mut b[23..25], (rem % 60) as u8);
    b[25..].copy_from_slice(b" GMT");
    b
}

/// Builds a response head in a reused buffer. Header names use canonical
/// MIME capitalization (`X-Ms-Blob-Type`).
pub struct HeadBuilder<'a> {
    out: &'a mut Vec<u8>,
}

impl<'a> HeadBuilder<'a> {
    /// Start a response. `out` is cleared.
    pub fn new(out: &'a mut Vec<u8>, status: u16) -> Self {
        out.clear();
        out.extend_from_slice(b"HTTP/1.1 ");
        let mut buf = itoa_buf();
        out.extend_from_slice(fmt_u64(u64::from(status), &mut buf));
        out.push(b' ');
        out.extend_from_slice(status_text(status).as_bytes());
        out.extend_from_slice(b"\r\n");
        Self { out }
    }

    pub fn header(&mut self, name: &str, value: &[u8]) -> &mut Self {
        self.out.extend_from_slice(name.as_bytes());
        self.out.extend_from_slice(b": ");
        self.out.extend_from_slice(value);
        self.out.extend_from_slice(b"\r\n");
        self
    }

    pub fn number(&mut self, name: &str, n: u64) -> &mut Self {
        let mut buf = itoa_buf();
        let value = fmt_u64(n, &mut buf);
        self.out.extend_from_slice(name.as_bytes());
        self.out.extend_from_slice(b": ");
        self.out.extend_from_slice(value);
        self.out.extend_from_slice(b"\r\n");
        self
    }

    /// `Content-Range: bytes first-last/size`.
    pub fn content_range(&mut self, first: u64, last: u64, size: u64) -> &mut Self {
        let mut buf = itoa_buf();
        self.out.extend_from_slice(b"Content-Range: bytes ");
        self.out.extend_from_slice(fmt_u64(first, &mut buf));
        self.out.push(b'-');
        self.out.extend_from_slice(fmt_u64(last, &mut buf));
        self.out.push(b'/');
        self.out.extend_from_slice(fmt_u64(size, &mut buf));
        self.out.extend_from_slice(b"\r\n");
        self
    }

    /// Azure object headers.
    pub fn azure_object(&mut self, version: &[u8]) -> &mut Self {
        self.header("X-Ms-Blob-Type", b"BlockBlob");
        if !version.is_empty() {
            self.header("X-Ms-Version-Id", version);
        }
        self
    }

    /// Add `Date`, the connection disposition, and the blank line.
    pub fn finish(&mut self, connection: Connection) {
        push_date(self.out);
        match connection {
            Connection::Default => {}
            Connection::Close => self.out.extend_from_slice(b"Connection: close\r\n"),
            Connection::KeepAlive => self.out.extend_from_slice(b"Connection: keep-alive\r\n"),
        }
        self.out.extend_from_slice(b"\r\n");
    }
}

/// How the response describes connection reuse.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Connection {
    /// HTTP/1.1 persistent default: no header.
    Default,
    Close,
    /// HTTP/1.0 keep-alive must be echoed.
    KeepAlive,
}

/// Write a complete error response: head into `out`, XML into `body`.
/// HEAD responses keep `Content-Length` but carry no body.
pub fn failure_response(
    out: &mut Vec<u8>,
    body: &mut Vec<u8>,
    api: Api,
    failure: Failure,
    head: bool,
    connection: Connection,
) {
    let status = failure.status();
    let code = failure.code(api);
    body.clear();
    body.extend_from_slice(b"<Error><Code>");
    body.extend_from_slice(code.as_bytes());
    body.extend_from_slice(b"</Code><Message>");
    body.extend_from_slice(status_text(status).as_bytes());
    body.extend_from_slice(b"</Message></Error>");
    let mut h = HeadBuilder::new(out, status);
    if api == Api::Azure {
        h.header("X-Ms-Version", AZURE_VERSION.as_bytes());
        h.header("X-Ms-Error-Code", code.as_bytes());
    }
    match failure {
        Failure::MethodNotAllowed => {
            h.header("Allow", b"GET, HEAD");
        }
        Failure::RangeNotSatisfiable(Some(size)) => {
            let mut buf = itoa_buf();
            h.out.extend_from_slice(b"Content-Range: bytes */");
            h.out.extend_from_slice(fmt_u64(size, &mut buf));
            h.out.extend_from_slice(b"\r\n");
        }
        _ => {}
    }
    h.header("Content-Type", b"application/xml");
    h.number("Content-Length", body.len() as u64);
    h.finish(connection);
    if head {
        body.clear();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn dates() {
        assert_eq!(&imf_fixdate(0), b"Thu, 01 Jan 1970 00:00:00 GMT");
        assert_eq!(&imf_fixdate(784_111_777), b"Sun, 06 Nov 1994 08:49:37 GMT");
        assert_eq!(&imf_fixdate(951_782_400), b"Tue, 29 Feb 2000 00:00:00 GMT");
        assert_eq!(
            &imf_fixdate(1_793_836_799),
            b"Wed, 04 Nov 2026 23:59:59 GMT"
        );
        assert_eq!(
            &imf_fixdate(4_102_444_800),
            b"Fri, 01 Jan 2100 00:00:00 GMT"
        );
    }

    #[test]
    fn failures() {
        let (mut out, mut body) = (Vec::new(), Vec::new());
        failure_response(
            &mut out,
            &mut body,
            Api::Azure,
            Failure::RangeNotSatisfiable(Some(4)),
            false,
            Connection::Default,
        );
        let text = String::from_utf8(out.clone()).unwrap();
        assert!(text.starts_with("HTTP/1.1 416 Requested Range Not Satisfiable\r\n"));
        assert!(text.contains("X-Ms-Error-Code: InvalidRange\r\n"));
        assert!(text.contains("Content-Range: bytes */4\r\n"));
        assert!(text.contains("X-Ms-Version: 2026-02-06\r\n"));
        assert_eq!(
            body,
            b"<Error><Code>InvalidRange</Code><Message>Requested Range Not Satisfiable</Message></Error>"
        );
        failure_response(
            &mut out,
            &mut body,
            Api::S3,
            Failure::MethodNotAllowed,
            true,
            Connection::Close,
        );
        let text = String::from_utf8(out).unwrap();
        assert!(text.contains("Allow: GET, HEAD\r\n"));
        assert!(text.contains("Content-Length: 81\r\n"), "{text}");
        assert!(text.ends_with("Connection: close\r\n\r\n"));
        assert!(body.is_empty());
    }
}
