//! Azure Blob request heads.
//!
//! Paths are escaped exactly as azblob v1.8.2 `container.Client.NewBlobClient`
//! does: Go `url.PathEscape` over the whole blob name, so `/` inside a key is
//! sent as `%2F`, which the service treats as the same name. Version IDs are
//! appended as `?versionid=<url.QueryEscape(id)>`, as azblob does.

use super::identity::AzureObject;
use crate::error::{Error, Result};
use crate::model::StrongEtag;
use http1::Header;
use http1::MessageHead;
use http1::StartLine;
use racer_wire::control::AzureContainerUrl;
use std::time::{SystemTime, UNIX_EPOCH};

/// The service version azblob v1.8.2 (the Go SDK in go.mod) sends.
pub const AZURE_SERVICE_VERSION: &[u8] = b"2026-12-06";

/// Request method. Reads use [`head`]; the two upload operations have their
/// own builders, [`put_block_head`] and [`put_block_list_head`].
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AzureMethod {
    /// Get Blob Properties.
    Head,
    /// Get Blob with an `x-ms-range`.
    Get,
    /// Put Block (`PUT ?comp=block&blockid=`).
    PutBlock,
    /// Put Block List (`PUT ?comp=blocklist`).
    PutBlockList,
}

impl AzureMethod {
    /// The HTTP method token.
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Head => "HEAD",
            Self::Get => "GET",
            Self::PutBlock | Self::PutBlockList => "PUT",
        }
    }
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
    if !matches!(method, AzureMethod::Head | AzureMethod::Get) {
        // Writes carry a body and extra fields; see the dedicated builders.
        return Err(Error::InvalidRequest);
    }
    let mut headers = common(url, now, 7);
    if let Some(range) = range {
        headers.push(Header::new(
            "x-ms-range",
            format!("bytes={}-{}", range.first, range.last).as_bytes(),
        ));
    }
    if let Some(etag) = if_match {
        headers.push(Header::new("If-Match", etag.as_bytes()));
    }
    authorization(&mut headers, bearer);
    Ok(MessageHead {
        start: StartLine::Request {
            method: method.as_str().to_owned(),
            target: target(url, object),
        },
        headers,
    })
}

/// `Host`, `x-ms-version`, `x-ms-date`, and `User-Agent`, in that order.
fn common(url: &AzureContainerUrl, now: SystemTime, capacity: usize) -> Vec<Header> {
    let mut headers = Vec::with_capacity(capacity);
    headers.push(Header::new("Host", url.host_header().as_bytes()));
    headers.push(Header::new("x-ms-version", AZURE_SERVICE_VERSION));
    // Azure Storage rejects authorized requests without a request date. The
    // head is rebuilt per attempt, so retries carry a fresh date. `now` is
    // the worker clock's wall time.
    let now = now.duration_since(UNIX_EPOCH).map_or(0, |d| d.as_secs());
    headers.push(Header::new("x-ms-date", http_date(now).as_bytes()));
    headers.push(Header::new("User-Agent", b"racer"));
    headers
}

/// Append `Authorization: Bearer <token>` when a token is present.
fn authorization(headers: &mut Vec<Header>, bearer: Option<&[u8]>) {
    if let Some(token) = bearer {
        let mut value = Vec::with_capacity(7 + token.len());
        value.extend_from_slice(b"Bearer ");
        value.extend_from_slice(token);
        headers.push(Header::new("Authorization", &value));
    }
}

/// Target for a write: writes never address a version.
fn write_target(url: &AzureContainerUrl, object: &AzureObject) -> Result<String> {
    if !object.version_id.is_empty() {
        return Err(Error::InvalidRequest);
    }
    Ok(target(url, object))
}

/// A block ID must be nonempty standard base64 of at most 64 decoded bytes
/// (Put Block). Racer IDs are always 64 characters (48 decoded bytes).
fn valid_block_id(id: &[u8]) -> bool {
    use base64::Engine;
    !id.is_empty()
        && base64::engine::general_purpose::STANDARD
            .decode(id)
            .is_ok_and(|raw| raw.len() <= 64)
}

/// Put Block head:
/// `PUT {target}?comp=block&blockid={url.QueryEscape(id)}` with `Host`,
/// `x-ms-version`, `x-ms-date`, `User-Agent`, `Content-Length`,
/// `x-ms-content-crc64`, then `Authorization` when a token is present.
/// `crc64` is the Azure CRC-64 of the `length` body bytes.
pub fn put_block_head(
    url: &AzureContainerUrl,
    object: &AzureObject,
    block_id: &[u8],
    length: u64,
    crc64: u64,
    bearer: Option<&[u8]>,
    now: SystemTime,
) -> Result<MessageHead> {
    if !valid_block_id(block_id) {
        return Err(Error::InvalidRequest);
    }
    let mut target = write_target(url, object)?;
    target.push_str("?comp=block&blockid=");
    // Valid base64 is ASCII.
    query_escape(
        &mut target,
        std::str::from_utf8(block_id).map_err(|_| Error::InvalidRequest)?,
    );
    let mut headers = common(url, now, 7);
    headers.push(Header::new("Content-Length", length.to_string().as_bytes()));
    headers.push(Header::new(
        "x-ms-content-crc64",
        super::crc64::header_value(crc64).as_bytes(),
    ));
    authorization(&mut headers, bearer);
    Ok(MessageHead {
        start: StartLine::Request {
            method: AzureMethod::PutBlock.as_str().to_owned(),
            target,
        },
        headers,
    })
}

/// Put Block List head: `PUT {target}?comp=blocklist` with `Host`,
/// `x-ms-version`, `x-ms-date`, `User-Agent`, `Content-Length`,
/// `Content-Type: application/xml`, `x-ms-content-crc64` of the XML body,
/// `If-None-Match: *`, `x-ms-blob-content-type` when set, then
/// `Authorization` when a token is present.
pub fn put_block_list_head(
    url: &AzureContainerUrl,
    object: &AzureObject,
    body: &[u8],
    content_type: Option<&str>,
    bearer: Option<&[u8]>,
    now: SystemTime,
) -> Result<MessageHead> {
    let mut target = write_target(url, object)?;
    target.push_str("?comp=blocklist");
    let mut headers = common(url, now, 10);
    headers.push(Header::new(
        "Content-Length",
        body.len().to_string().as_bytes(),
    ));
    headers.push(Header::new("Content-Type", b"application/xml"));
    headers.push(Header::new(
        "x-ms-content-crc64",
        super::crc64::header_value(super::crc64::checksum(body)).as_bytes(),
    ));
    // Create only: an existing blob fails with 409 BlobAlreadyExists.
    headers.push(Header::new("If-None-Match", b"*"));
    if let Some(content_type) = content_type {
        if content_type.is_empty()
            || !content_type
                .bytes()
                .all(|b| b == b' ' || b == b'\t' || b.is_ascii_graphic())
        {
            return Err(Error::InvalidRequest);
        }
        headers.push(Header::new(
            "x-ms-blob-content-type",
            content_type.as_bytes(),
        ));
    }
    authorization(&mut headers, bearer);
    Ok(MessageHead {
        start: StartLine::Request {
            method: AzureMethod::PutBlockList.as_str().to_owned(),
            target,
        },
        headers,
    })
}

/// Put Block List body committing `block_ids` in order, each as `Latest`:
/// `<?xml version="1.0" encoding="utf-8"?><BlockList><Latest>id</Latest>...</BlockList>`.
/// IDs are base64, so no XML escaping is needed; invalid IDs are rejected.
// WP2 provides the canonical version; switch on merge.
pub fn block_list_xml(block_ids: &[[u8; 64]]) -> Result<Vec<u8>> {
    const HEAD: &[u8] = b"<?xml version=\"1.0\" encoding=\"utf-8\"?><BlockList>";
    const TAIL: &[u8] = b"</BlockList>";
    let mut body = Vec::with_capacity(HEAD.len() + TAIL.len() + block_ids.len() * 81);
    body.extend_from_slice(HEAD);
    for id in block_ids {
        if !valid_block_id(id) {
            return Err(Error::InvalidRequest);
        }
        body.extend_from_slice(b"<Latest>");
        body.extend_from_slice(id);
        body.extend_from_slice(b"</Latest>");
    }
    body.extend_from_slice(TAIL);
    Ok(body)
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

    fn names(head: &MessageHead) -> Vec<&str> {
        head.headers.iter().map(|h| h.name.as_str()).collect()
    }

    /// A 64-character ID whose base64 contains `+` and `/`.
    const ID: [u8; 64] = *b"++//abcdABCD0123++//abcdABCD0123++//abcdABCD0123++//abcdABCD0123";

    #[test]
    fn put_block_golden() {
        let url = AzureContainerUrl::parse("https://acct.blob.core.windows.net/models").unwrap();
        let now = UNIX_EPOCH + std::time::Duration::from_secs(784_111_777);
        let head = put_block_head(
            &url,
            &object("dir/file.bin", ""),
            &ID,
            5,
            super::super::crc64::checksum(b"hello"),
            Some(b"token"),
            now,
        )
        .unwrap();
        let StartLine::Request { method, target } = &head.start else {
            panic!("request");
        };
        assert_eq!(method, "PUT");
        assert_eq!(
            target,
            "/models/dir%2Ffile.bin?comp=block&blockid=\
             %2B%2B%2F%2FabcdABCD0123%2B%2B%2F%2FabcdABCD0123\
             %2B%2B%2F%2FabcdABCD0123%2B%2B%2F%2FabcdABCD0123"
        );
        assert_eq!(
            names(&head),
            [
                "Host",
                "x-ms-version",
                "x-ms-date",
                "User-Agent",
                "Content-Length",
                "x-ms-content-crc64",
                "Authorization"
            ]
        );
        let get = |name| head.unique(name).unwrap().unwrap();
        assert_eq!(get("Host"), b"acct.blob.core.windows.net");
        assert_eq!(get("x-ms-version"), AZURE_SERVICE_VERSION);
        assert_eq!(get("x-ms-date"), b"Sun, 06 Nov 1994 08:49:37 GMT");
        assert_eq!(get("Content-Length"), b"5");
        assert_eq!(
            get("x-ms-content-crc64"),
            super::super::crc64::header_value(super::super::crc64::checksum(b"hello")).as_bytes()
        );
        assert_eq!(get("Authorization"), b"Bearer token");

        // Anonymous: no Authorization.
        let anonymous =
            put_block_head(&url, &object("k", ""), &ID, 0, 0, None, now).unwrap();
        assert_eq!(anonymous.unique("Authorization").unwrap(), None);
    }

    #[test]
    fn put_block_rejects_bad_input() {
        let url = AzureContainerUrl::parse("https://acct.blob.core.windows.net/models").unwrap();
        let now = SystemTime::now();
        // Writes never address a version.
        assert_eq!(
            put_block_head(&url, &object("k", "v1"), &ID, 1, 0, None, now).err(),
            Some(Error::InvalidRequest)
        );
        for bad in [&b""[..], b"not base64!", b"QUJD="] {
            assert_eq!(
                put_block_head(&url, &object("k", ""), bad, 1, 0, None, now).err(),
                Some(Error::InvalidRequest)
            );
        }
        // 64 decoded bytes is the Azure maximum; 65 is too long.
        use base64::Engine;
        let engine = base64::engine::general_purpose::STANDARD;
        let max = engine.encode([7u8; 64]);
        assert!(put_block_head(&url, &object("k", ""), max.as_bytes(), 1, 0, None, now).is_ok());
        let over = engine.encode([7u8; 65]);
        assert!(put_block_head(&url, &object("k", ""), over.as_bytes(), 1, 0, None, now).is_err());
        // Read builder refuses write methods.
        for method in [AzureMethod::PutBlock, AzureMethod::PutBlockList] {
            assert_eq!(
                head(&url, &object("k", ""), method, None, None, None, now).err(),
                Some(Error::InvalidRequest)
            );
        }
    }

    #[test]
    fn put_block_list_golden() {
        let url =
            AzureContainerUrl::parse("http://127.0.0.1:10000/devstoreaccount1/models").unwrap();
        let body = block_list_xml(&[ID, [b'A'; 64]]).unwrap();
        assert_eq!(
            std::str::from_utf8(&body).unwrap(),
            format!(
                "<?xml version=\"1.0\" encoding=\"utf-8\"?><BlockList>\
                 <Latest>{}</Latest><Latest>{}</Latest></BlockList>",
                std::str::from_utf8(&ID).unwrap(),
                "A".repeat(64)
            )
        );
        let now = UNIX_EPOCH;
        let head = put_block_list_head(
            &url,
            &object("a b", ""),
            &body,
            Some("text/plain; charset=utf-8"),
            Some(b"t"),
            now,
        )
        .unwrap();
        assert!(matches!(
            &head.start,
            StartLine::Request { method, target }
                if method == "PUT" && target == "/devstoreaccount1/models/a%20b?comp=blocklist"
        ));
        assert_eq!(
            names(&head),
            [
                "Host",
                "x-ms-version",
                "x-ms-date",
                "User-Agent",
                "Content-Length",
                "Content-Type",
                "x-ms-content-crc64",
                "If-None-Match",
                "x-ms-blob-content-type",
                "Authorization"
            ]
        );
        let get = |name| head.unique(name).unwrap().unwrap();
        assert_eq!(get("Content-Length"), body.len().to_string().as_bytes());
        assert_eq!(get("Content-Type"), b"application/xml");
        assert_eq!(get("If-None-Match"), b"*");
        assert_eq!(get("x-ms-blob-content-type"), b"text/plain; charset=utf-8");
        assert_eq!(get("x-ms-date"), b"Thu, 01 Jan 1970 00:00:00 GMT");

        let plain = put_block_list_head(&url, &object("k", ""), &body, None, None, now).unwrap();
        assert_eq!(plain.unique("x-ms-blob-content-type").unwrap(), None);
        assert_eq!(plain.unique("Authorization").unwrap(), None);
        for bad in ["", "a\r\nb", "caf\u{e9}"] {
            assert_eq!(
                put_block_list_head(&url, &object("k", ""), &body, Some(bad), None, now).err(),
                Some(Error::InvalidRequest)
            );
        }
        assert_eq!(
            put_block_list_head(&url, &object("k", "v"), &body, None, None, now).err(),
            Some(Error::InvalidRequest)
        );
        assert_eq!(block_list_xml(&[[b'!'; 64]]).err(), Some(Error::InvalidRequest));
        assert_eq!(
            std::str::from_utf8(&block_list_xml(&[]).unwrap()).unwrap(),
            "<?xml version=\"1.0\" encoding=\"utf-8\"?><BlockList></BlockList>"
        );
    }
}
