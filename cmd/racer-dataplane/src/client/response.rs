//! Central status mapping and streaming body delivery, including late truncation.
use super::request::ReadKind;
use crate::{
    error::{Error, Operation, Result},
    http::{
        codec::{Header, MessageHead, StartLine},
        io::HttpIo,
        pool::ConnectionLease,
    },
    memory::delivery::Delivery,
    model::{metadata::ObjectMetadata, range::ResolvedRange},
    read::serve::ReadResponse,
    runtime::deadline::RequestScope,
};
use std::{
    rc::Rc,
    time::{Duration, Instant, UNIX_EPOCH},
};

pub struct Responses {
    io: Rc<HttpIo>,
    delivery: Rc<Delivery>,
}
impl Responses {
    pub fn new(io: Rc<HttpIo>, delivery: Rc<Delivery>) -> Self {
        Self { io, delivery }
    }
    /// Version unavailable -> 412, transient unavailable -> 503, range -> 206.
    /// Define malformed/unsatisfiable/unsupported method mappings in one place.
    pub fn error_head(&self, error: Error) -> Result<MessageHead> {
        error_head(error)
    }

    pub fn send_error<'a>(
        &'a self,
        mut connection: ConnectionLease,
        error: Error,
        scope: &'a RequestScope,
    ) -> Operation<'a, ConnectionLease> {
        Box::pin(async move {
            let mut head = error_head(error)?;
            head.headers.push(header("Connection", "close"));
            connection.poison();
            // A failed operation's expired scope cannot write its own 503. Allow
            // only a short, independently bounded final head, then close.
            let final_scope;
            let scope = if scope.check().is_err() {
                final_scope =
                    RequestScope::new(scope.request, Instant::now() + Duration::from_secs(1))?;
                &final_scope
            } else {
                scope
            };
            Ok(self.io.send_head(connection, head, scope).await?.connection)
        })
    }

    /// Check the coordinator's immutable result against the original operation
    /// before any headers escape. The request's sensitive context is not retained.
    pub fn validate(&self, kind: &ReadKind, response: &ReadResponse) -> Result<()> {
        if kind
            .pin()
            .is_some_and(|pin| pin != &response.metadata.version.etag)
        {
            return Err(Error::BadGateway);
        }
        match kind {
            ReadKind::Head | ReadKind::HeadPinned { .. } => {
                if response.range.is_some() || response.body.is_some() {
                    return Err(Error::BadGateway);
                }
            }
            ReadKind::Bootstrap if response.metadata.length == 0 => {
                if response.range.is_some() || response.body.is_some() {
                    return Err(Error::BadGateway);
                }
            }
            ReadKind::Bootstrap | ReadKind::Pinned { .. } => {
                let range = match kind {
                    ReadKind::Pinned { range, .. } => *range,
                    _ => crate::model::range::ByteRange::Closed {
                        first: 0,
                        last: crate::model::range::PAGE_BYTES - 1,
                    },
                }
                .resolve(response.metadata.length)
                .map_err(|_| Error::BadGateway)?;
                if response.range != Some(range) || response.body.is_none() {
                    return Err(Error::BadGateway);
                }
            }
        }
        success_head(&response.metadata, response.range)?;
        Ok(())
    }
    pub fn send<'a>(
        &'a self,
        mut connection: ConnectionLease,
        mut response: ReadResponse,
        scope: &'a RequestScope,
    ) -> Operation<'a, ConnectionLease> {
        Box::pin(async move {
            scope.check()?;
            let head = success_head(&response.metadata, response.range)?;
            let expected = response.range.map_or(0, |range| range.len());
            if response.body.is_some() != response.range.is_some() {
                return Err(Error::BadGateway);
            }
            connection = self.io.send_head(connection, head, scope).await?.connection;
            let mut sent = 0u64;
            if let Some(stream) = response.body.as_mut() {
                while let Some(reader) = stream.next_slice().await? {
                    let length = reader.remaining() as u64;
                    if length == 0 || length > expected - sent {
                        return Err(Error::BadGateway);
                    }
                    connection = self.delivery.finish_to(reader, connection, scope).await?;
                    sent += length;
                }
            }
            if sent != expected {
                return Err(Error::BadGateway);
            }
            connection.finish_exchange()?;
            Ok(connection)
        })
    }
}

fn header(name: &str, value: impl AsRef<[u8]>) -> Header {
    Header {
        name: name.into(),
        value: value.as_ref().to_vec(),
    }
}

fn error_head(error: Error) -> Result<MessageHead> {
    let status = match error {
        Error::InvalidRequest | Error::InvalidRange => 400,
        Error::MethodNotAllowed => 405,
        Error::HeaderTooLarge => 431,
        Error::Unauthorized | Error::OriginRejected => 401,
        Error::Forbidden | Error::OriginForbidden => 403,
        Error::NotFound => 404,
        Error::VersionUnavailable => 412,
        Error::UnsatisfiableRangeWithLength(length) if length <= i64::MAX as u64 => 416,
        Error::BadGateway | Error::CorruptRecord => 502,
        Error::Unavailable
        | Error::Overloaded
        | Error::DeadlineExceeded
        | Error::Cancelled
        | Error::StaleFlight
        | Error::IncompatibleMembership
        | Error::HopBudgetExhausted
        | Error::MissingKey
        | Error::Io => 503,
        // A bare unsatisfiable error has lost required version metadata. Never
        // invent a total length to make a syntactically valid but false 416.
        _ => 500,
    };
    let mut headers = vec![header("Content-Length", "0")];
    if status == 405 {
        headers.push(header("Allow", "HEAD, GET"));
    }
    if status == 416 {
        let Error::UnsatisfiableRangeWithLength(length) = error else {
            return Err(Error::Internal);
        };
        headers.push(header("Content-Range", format!("bytes */{length}")));
    }
    Ok(MessageHead {
        start: StartLine::Response { status },
        headers,
    })
}

fn success_head(metadata: &ObjectMetadata, range: Option<ResolvedRange>) -> Result<MessageHead> {
    let expiry = metadata
        .expires_at
        .0
        .duration_since(UNIX_EPOCH)
        .map_err(|_| Error::BadGateway)?;
    if metadata.length > i64::MAX as u64
        || expiry.as_millis() > i64::MAX as u128
        || expiry.subsec_nanos() % 1_000_000 != 0
    {
        return Err(Error::BadGateway);
    }
    let mut headers = vec![
        header("ETag", metadata.version.etag.as_bytes()),
        header("Racer-Expires-At", expiry.as_millis().to_string()),
    ];
    let status = if let Some(range) = range {
        if range.end() > metadata.length {
            return Err(Error::BadGateway);
        }
        headers.push(header("Content-Length", range.len().to_string()));
        headers.push(header(
            "Content-Range",
            format!(
                "bytes {}-{}/{}",
                range.start(),
                range.end() - 1,
                metadata.length
            ),
        ));
        206
    } else {
        headers.push(header("Content-Length", metadata.length.to_string()));
        200
    };
    headers.push(header("Content-Type", "application/octet-stream"));
    Ok(MessageHead {
        start: StartLine::Response { status },
        headers,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::{
        identity::{CacheId, CacheKey, ObjectId, ObjectVersion, StrongEtag},
        metadata::ExpiresAt,
        range::ByteRange,
    };
    use std::time::Duration;

    pub(super) fn metadata(length: u64) -> ObjectMetadata {
        ObjectMetadata {
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId("cache".into()),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::parse(b"\"a,b\\c\"").unwrap(),
            },
            length,
            expires_at: ExpiresAt(UNIX_EPOCH + Duration::from_millis(123456)),
        }
    }

    #[test]
    fn sdk_success_heads_are_exact() {
        for length in [0, 1, i64::MAX as u64] {
            let head = success_head(&metadata(length), None).unwrap();
            assert!(matches!(head.start, StartLine::Response { status: 200 }));
            assert_eq!(
                head.unique("Content-Length").unwrap().unwrap(),
                length.to_string().as_bytes()
            );
            assert_eq!(head.unique("ETag").unwrap().unwrap(), b"\"a,b\\c\"");
            assert_eq!(head.unique("Racer-Expires-At").unwrap().unwrap(), b"123456");
            assert!(head.unique("Content-Range").unwrap().is_none());
        }
        let range = ByteRange::Suffix(7).resolve(100).unwrap();
        let head = success_head(&metadata(100), Some(range)).unwrap();
        assert!(matches!(head.start, StartLine::Response { status: 206 }));
        assert_eq!(
            head.unique("Content-Range").unwrap().unwrap(),
            b"bytes 93-99/100"
        );
        assert_eq!(head.unique("Content-Length").unwrap().unwrap(), b"7");
        assert_eq!(
            head.unique("Content-Type").unwrap().unwrap(),
            b"application/octet-stream"
        );
    }

    #[test]
    fn sdk_error_statuses_and_required_fields() {
        for (error, status) in [
            (Error::InvalidRequest, 400),
            (Error::MethodNotAllowed, 405),
            (Error::HeaderTooLarge, 431),
            (Error::OriginRejected, 401),
            (Error::OriginForbidden, 403),
            (Error::NotFound, 404),
            (Error::VersionUnavailable, 412),
            (Error::UnsatisfiableRangeWithLength(123), 416),
            (Error::UnsatisfiableRange, 500),
            (Error::Internal, 500),
            (Error::BadGateway, 502),
            (Error::Unavailable, 503),
            (Error::DeadlineExceeded, 503),
        ] {
            let head = error_head(error).unwrap();
            assert!(
                matches!(head.start, StartLine::Response { status: actual } if actual == status)
            );
            assert_eq!(head.unique("Content-Length").unwrap().unwrap(), b"0");
            assert!(head.unique("ETag").unwrap().is_none());
            assert!(head.unique("Racer-Expires-At").unwrap().is_none());
            if status == 416 {
                assert_eq!(
                    head.unique("Content-Range").unwrap().unwrap(),
                    b"bytes */123"
                );
            } else {
                assert!(head.unique("Content-Range").unwrap().is_none());
            }
            if status == 405 {
                assert_eq!(head.unique("Allow").unwrap().unwrap(), b"HEAD, GET");
            }
        }
    }

    #[test]
    fn rejects_unrepresentable_metadata_before_headers() {
        assert!(success_head(&metadata(i64::MAX as u64 + 1), None).is_err());
        for expires_at in [
            UNIX_EPOCH - Duration::from_millis(1),
            UNIX_EPOCH + Duration::from_nanos(1),
            UNIX_EPOCH + Duration::from_millis(i64::MAX as u64 + 1),
        ] {
            let mut metadata = metadata(1);
            metadata.expires_at = ExpiresAt(expires_at);
            assert!(success_head(&metadata, None).is_err());
        }
        assert!(success_head(&metadata(1), Some(ByteRange::From(0).resolve(2).unwrap())).is_err());
    }
}
