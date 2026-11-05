//! Azure Blob request heads.
//!
//! Paths are escaped exactly as azblob v1.8.2 `container.Client.NewBlobClient`
//! does: Go `url.PathEscape` over the whole blob name, so `/` inside a key is
//! sent as `%2F`, which the service treats as the same name. Version IDs are
//! appended as `?versionid=<url.QueryEscape(id)>`, as azblob does.

use super::identity::AzureObject;
use crate::error::Result;
use crate::model::StrongEtag;
use http1::Header;
use http1::MessageHead;
use http1::StartLine;
use racer_control_wire::AzureContainerUrl;
use std::time::{SystemTime, UNIX_EPOCH};

/// The service version azblob v1.8.2 (the Go SDK in go.mod) sends.
pub const AZURE_SERVICE_VERSION: &[u8] = b"2026-12-06";

/// Request method; Azure reads only need these two.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AzureMethod {
    /// Get Blob Properties.
    Head,
    /// Get Blob with an `x-ms-range`.
    Get,
}

/// One inclusive byte range.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ByteRange {
    pub first: u64,
    pub last: u64,
}

/// Go `url.PathEscape` (`encodePathSegment` mode).
pub fn path_escape(out: &mut String, value: &str) {
    escape(out, value, |b| {
        b.is_ascii_alphanumeric()
            || matches!(
                b,
                b'-' | b'_' | b'.' | b'~' | b'$' | b'&' | b'+' | b':' | b'=' | b'@'
            )
    });
}

/// Go `url.QueryEscape`, which writes space as `+`.
pub fn query_escape(out: &mut String, value: &str) {
    for part in value.split(' ').enumerate() {
        if part.0 > 0 {
            out.push('+');
        }
        escape(out, part.1, |b| {
            b.is_ascii_alphanumeric() || matches!(b, b'-' | b'_' | b'.' | b'~')
        });
    }
}

fn escape(out: &mut String, value: &str, keep: impl Fn(u8) -> bool) {
    const HEX: &[u8; 16] = b"0123456789ABCDEF";
    for b in value.bytes() {
        if keep(b) {
            out.push(b as char);
        } else {
            out.push('%');
            out.push(HEX[usize::from(b >> 4)] as char);
            out.push(HEX[usize::from(b & 0xf)] as char);
        }
    }
}

/// Origin-form request target for one blob.
pub fn target(url: &AzureContainerUrl, object: &AzureObject) -> String {
    let mut target = String::with_capacity(64 + object.key.len() * 3);
    target.push('/');
    if let Some(account) = url.account_path() {
        target.push_str(account);
        target.push('/');
    }
    target.push_str(url.container());
    target.push('/');
    path_escape(&mut target, &object.key);
    if !object.version_id.is_empty() {
        target.push_str("?versionid=");
        query_escape(&mut target, &object.version_id);
    }
    target
}

/// Format seconds since the Unix epoch as an RFC 1123 IMF-fixdate, for
/// example `Tue, 03 Nov 2026 20:47:23 GMT`. Uses the civil-from-days
/// algorithm so no date crate is needed.
pub fn http_date(secs: u64) -> String {
    const DAYS: [&str; 7] = ["Thu", "Fri", "Sat", "Sun", "Mon", "Tue", "Wed"];
    const MONTHS: [&str; 12] = [
        "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
    ];
    let days = secs / 86_400;
    let rem = secs % 86_400;
    let weekday = DAYS[(days % 7) as usize];
    let z = days + 719_468;
    let era = z / 146_097;
    let doe = z % 146_097;
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let day = doy - (153 * mp + 2) / 5 + 1;
    let month = if mp < 10 { mp + 3 } else { mp - 9 };
    let year = yoe + era * 400 + u64::from(month <= 2);
    format!(
        "{weekday}, {day:02} {} {year:04} {:02}:{:02}:{:02} GMT",
        MONTHS[(month - 1) as usize],
        rem / 3600,
        rem / 60 % 60,
        rem % 60
    )
}

/// Build one request head. `bearer` is the raw access token, never logged.
pub fn head(
    url: &AzureContainerUrl,
    object: &AzureObject,
    method: AzureMethod,
    range: Option<ByteRange>,
    if_match: Option<&StrongEtag>,
    bearer: Option<&[u8]>,
    now: SystemTime,
) -> Result<MessageHead> {
    let mut headers = Vec::with_capacity(7);
    headers.push(Header::new("Host", url.host_header().as_bytes()));
    headers.push(Header::new("x-ms-version", AZURE_SERVICE_VERSION));
    // Azure Storage rejects authorized requests without a request date. The
    // head is rebuilt per attempt, so retries carry a fresh date. `now` is
    // the worker clock's wall time.
    let now = now.duration_since(UNIX_EPOCH).map_or(0, |d| d.as_secs());
    headers.push(Header::new("x-ms-date", http_date(now).as_bytes()));
    headers.push(Header::new("User-Agent", b"racer"));
    if let Some(range) = range {
        headers.push(Header::new(
            "x-ms-range",
            format!("bytes={}-{}", range.first, range.last).as_bytes(),
        ));
    }
    if let Some(etag) = if_match {
        headers.push(Header::new("If-Match", etag.as_bytes()));
    }
    if let Some(token) = bearer {
        let mut value = Vec::with_capacity(7 + token.len());
        value.extend_from_slice(b"Bearer ");
        value.extend_from_slice(token);
        headers.push(Header::new("Authorization", &value));
    }
    Ok(MessageHead {
        start: StartLine::Request {
            method: match method {
                AzureMethod::Head => "HEAD",
                AzureMethod::Get => "GET",
            }
            .to_owned(),
            target: target(url, object),
        },
        headers,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn object(key: &str, version: &str) -> AzureObject {
        AzureObject {
            bucket: "models".into(),
            key: key.into(),
            version_id: version.into(),
        }
    }

    /// Expected values from Go `url.PathEscape` and `url.QueryEscape`.
    #[test]
    fn escaping_matches_go_net_url() {
        let all: String = (0u8..128).map(char::from).collect();
        let mut path = String::new();
        path_escape(&mut path, &all);
        let kept: String = path.split('%').map(|s| s.get(2..).unwrap_or(s)).collect();
        assert_eq!(
            kept,
            "$&+-.0123456789:=@ABCDEFGHIJKLMNOPQRSTUVWXYZ_abcdefghijklmnopqrstuvwxyz~"
        );
        let mut query = String::new();
        query_escape(&mut query, &all);
        let kept: String = query.split('%').map(|s| s.get(2..).unwrap_or(s)).collect();
        assert_eq!(
            kept,
            "+-.0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ_abcdefghijklmnopqrstuvwxyz~"
        );
        let mut out = String::new();
        path_escape(&mut out, "a/../b%2Fc +雪?#;,");
        assert_eq!(out, "a%2F..%2Fb%252Fc%20+%E9%9B%AA%3F%23%3B%2C");
        out.clear();
        query_escape(&mut out, "v+/= &雪");
        assert_eq!(out, "v%2B%2F%3D+%26%E9%9B%AA");
    }

    #[test]
    fn http_date_known_values() {
        assert_eq!(http_date(0), "Thu, 01 Jan 1970 00:00:00 GMT");
        assert_eq!(http_date(1_709_164_800), "Thu, 29 Feb 2024 00:00:00 GMT");
        assert_eq!(http_date(1_793_738_843), "Tue, 03 Nov 2026 20:47:23 GMT");
        assert_eq!(http_date(4_107_542_399), "Sun, 28 Feb 2100 23:59:59 GMT");
        assert_eq!(http_date(4_107_542_400), "Mon, 01 Mar 2100 00:00:00 GMT");
        assert_eq!(http_date(951_782_400), "Tue, 29 Feb 2000 00:00:00 GMT");
    }

    #[test]
    fn targets_and_headers() {
        let url = AzureContainerUrl::parse("https://acct.blob.core.windows.net/models").unwrap();
        assert_eq!(
            target(&url, &object("dir/file.bin", "")),
            "/models/dir%2Ffile.bin"
        );
        let url =
            AzureContainerUrl::parse("http://127.0.0.1:10000/devstoreaccount1/models").unwrap();
        assert_eq!(
            target(&url, &object("k", "2024-01-01T00:00:00.0000000Z")),
            "/devstoreaccount1/models/k?versionid=2024-01-01T00%3A00%3A00.0000000Z"
        );
        let etag = StrongEtag::parse(b"\"0x8D\"").unwrap();
        let head = head(
            &url,
            &object("k", ""),
            AzureMethod::Get,
            Some(ByteRange {
                first: 0,
                last: 16777215,
            }),
            Some(&etag),
            Some(b"token"),
            UNIX_EPOCH + std::time::Duration::from_secs(784_111_777),
        )
        .unwrap();
        assert!(matches!(
            &head.start,
            StartLine::Request { method, target }
                if method == "GET" && target == "/devstoreaccount1/models/k"
        ));
        let get = |name| head.unique(name).unwrap().unwrap();
        assert_eq!(get("Host"), b"127.0.0.1:10000");
        assert_eq!(get("x-ms-version"), AZURE_SERVICE_VERSION);
        assert_eq!(get("x-ms-range"), b"bytes=0-16777215");
        assert_eq!(get("If-Match"), b"\"0x8D\"");
        assert_eq!(get("Authorization"), b"Bearer token");
        // The date comes from the caller's clock, not the host's.
        assert_eq!(get("x-ms-date"), b"Sun, 06 Nov 1994 08:49:37 GMT");
        let anonymous = super::head(
            &url,
            &object("k", ""),
            AzureMethod::Head,
            None,
            None,
            None,
            SystemTime::now(),
        )
        .unwrap();
        for name in ["Authorization", "If-Match", "x-ms-range"] {
            assert_eq!(anonymous.unique(name).unwrap(), None);
        }
        assert!(anonymous.unique("x-ms-date").unwrap().is_some());
    }
}
