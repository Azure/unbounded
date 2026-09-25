//! Conditional full-page GET validation before authenticated publication.
use super::protocol;
use crate::{
    error::{Error, Result},
    http::codec::MessageHead,
    memory::pool::PlaintextBuffer,
    model::{identity::PageId, metadata::ObjectMetadata, range::PAGE_BYTES},
};
pub struct OriginPage {
    pub metadata: ObjectMetadata,
    pub plaintext: PlaintextBuffer,
}
/// Require exact If-Match, Content-Range, whole-page length, and final-page bounds.
/// Reject multipart, short/overlong bodies, and unexpected versions.
pub fn validate(
    head: &MessageHead,
    page: &PageId,
    received_bytes: usize,
) -> Result<ObjectMetadata> {
    let (metadata, length) = validate_head(head, page)?;
    if received_bytes as u64 != length {
        return Err(Error::BadGateway);
    }
    Ok(metadata)
}

/// Validate before allocating or receiving any payload.
pub(super) fn validate_head(head: &MessageHead, page: &PageId) -> Result<(ObjectMetadata, u64)> {
    let (status, length) = protocol::response(head, true)?;
    if status != 206 || protocol::required(head, "Content-Type")? != b"application/octet-stream" {
        return Err(Error::BadGateway);
    }
    let (first, last, total) = protocol::content_range(head)?;
    let expected_first = page
        .number
        .0
        .checked_mul(PAGE_BYTES)
        .ok_or(Error::InvalidRange)?;
    if first != expected_first
        || length != last - first + 1
        || length != (total - first).min(PAGE_BYTES)
    {
        return Err(Error::BadGateway);
    }
    let metadata = protocol::metadata(head, &page.version.object, total)?;
    if metadata.version != page.version {
        return Err(Error::BadGateway);
    }
    Ok((metadata, length))
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        http::codec::{Header, StartLine},
        model::identity::{CacheId, CacheKey, ObjectId, ObjectVersion, PageNumber, StrongEtag},
    };

    fn page() -> PageId {
        PageId {
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId("cache".into()),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::parse(b"\"v\"").unwrap(),
            },
            number: PageNumber(1),
        }
    }
    fn head() -> MessageHead {
        MessageHead {
            start: StartLine::Response { status: 206 },
            headers: [
                ("Content-Length", "3"),
                ("Content-Type", "application/octet-stream"),
                ("Content-Range", "bytes 16777216-16777218/16777219"),
                ("ETag", "\"v\""),
                ("Racer-Expires-At", "0"),
            ]
            .into_iter()
            .map(|(name, value)| Header {
                name: name.into(),
                value: value.as_bytes().to_vec(),
            })
            .collect(),
        }
    }

    #[test]
    fn pinned_final_page_accepts_only_exact_body_range_and_version() {
        let page = page();
        assert_eq!(validate(&head(), &page, 3).unwrap().length, PAGE_BYTES + 3);
        for count in [0, 2, 4, PAGE_BYTES as usize] {
            assert_eq!(validate(&head(), &page, count), Err(Error::BadGateway));
        }
        for range in [
            "bytes 0-2/3",
            "bytes 16777216-16777218/16777220",
            "bytes */16777219",
            "bytes 16777216-16777219/16777219",
            "bytes 016777216-16777218/16777219",
        ] {
            let mut response = head();
            response.headers[2].value = range.as_bytes().to_vec();
            assert_eq!(validate(&response, &page, 3), Err(Error::BadGateway));
        }
        let mut response = head();
        response.headers[3].value = b"\"other\"".to_vec();
        assert_eq!(validate(&response, &page, 3), Err(Error::BadGateway));
        response = head();
        response.start = StartLine::Response { status: 200 };
        assert_eq!(validate(&response, &page, 3), Err(Error::BadGateway));
        response = head();
        response.headers[1].value = b"multipart/byteranges".to_vec();
        assert_eq!(validate(&response, &page, 3), Err(Error::BadGateway));
    }
}
