//! Azure Blob response validation and error classification.
//!
//! Port of `headMetadata`, `validateAzurePage`, `azureError`, and
//! `azureHTTPError` in `internal/racerobject/azure.go`. Errors are unit
//! variants, so neither URLs nor credentials can leak into error text.
//!
//! Go error to Racer error, matching what `racersdk.ServeOrigin` and the UDS
//! client produced end to end:
//! `ErrUnauthorized` -> `OriginRejected`, `ErrForbidden` -> `OriginForbidden`,
//! `ErrNotFound` -> `NotFound`, `ErrVersionMismatch` -> `VersionUnavailable`,
//! `ErrUnavailable` -> `Unavailable`, `errInvalidAzureResponse` -> `Internal`.
//! Broken HTTP framing (duplicate fields, chunked bodies, short bodies) is
//! `BadGateway`, as on the UDS path.

use super::identity::AzureObject;
use crate::error::{Error, Result};
use crate::model::{ContentType, StrongEtag, parse_decimal};
use http1::{MessageHead, StartLine};

/// Response status, after framing checks.
pub fn status(head: &MessageHead) -> Result<u16> {
    match head.start {
        StartLine::Response { status } => Ok(status),
        StartLine::Request { .. } => Err(Error::BadGateway),
    }
}

/// Read one unique field, trimming optional whitespace.
pub fn field<'a>(head: &'a MessageHead, name: &str) -> Result<Option<&'a [u8]>> {
    Ok(head
        .unique(name)
        .map_err(|_| Error::BadGateway)?
        .map(http1::trim_ows))
}

/// Classify a non-success response (port of Go `azureError` for
/// `*azcore.ResponseError`). A pinned miss means the pinned version is gone.
pub fn classify(head: &MessageHead, pinned: bool) -> Error {
    let Ok(status) = status(head) else {
        return Error::BadGateway;
    };
    let code = field(head, "x-ms-error-code").ok().flatten().unwrap_or(b"");
    let error = match code {
        b"BlobNotFound" | b"ContainerNotFound" | b"ResourceNotFound" => Error::NotFound,
        b"AuthenticationFailed" | b"InvalidAuthenticationInfo" | b"NoAuthenticationInformation" => {
            Error::OriginRejected
        }
        b"AuthorizationFailure"
        | b"AuthorizationPermissionMismatch"
        | b"AuthorizationProtocolMismatch"
        | b"AuthorizationResourceTypeMismatch"
        | b"AuthorizationServiceMismatch"
        | b"AuthorizationSourceIPMismatch"
        | b"InsufficientAccountPermissions"
        | b"AccountIsDisabled" => Error::OriginForbidden,
        b"ConditionNotMet"
        | b"TargetConditionNotMet"
        | b"SourceConditionNotMet"
        | b"BlobOverwritten" => Error::VersionUnavailable,
        b"ServerBusy" | b"InternalError" | b"OperationTimedOut" => Error::Unavailable,
        b"BlobArchived" | b"BlobBeingRehydrated" => Error::Internal,
        _ => classify_status(status),
    };
    if pinned && error == Error::NotFound {
        Error::VersionUnavailable
    } else {
        error
    }
}

/// Port of Go `azureHTTPError`.
fn classify_status(status: u16) -> Error {
    match status {
        401 => Error::OriginRejected,
        403 => Error::OriginForbidden,
        404 => Error::NotFound,
        412 => Error::VersionUnavailable,
        408 | 429 | 500..=599 => Error::Unavailable,
        _ => Error::Internal,
    }
}

/// Map transport failures: network loss is transient, malformed responses are
/// gateway faults, and scope expiry passes through.
pub fn transport_error(error: Error) -> Error {
    match error {
        Error::Io | Error::Os(_) | Error::Unavailable => Error::Unavailable,
        Error::DeadlineExceeded
        | Error::Canceled
        | Error::Overloaded
        | Error::OriginRejected
        | Error::OriginForbidden => error,
        _ => Error::BadGateway,
    }
}

/// Validated blob identity common to HEAD and GET responses.
#[derive(Debug)]
pub struct Blob {
    pub etag: StrongEtag,
    pub content_type: Option<ContentType>,
    /// `Content-Length`: the blob size for HEAD, the body size for GET.
    pub content_length: u64,
}

/// Port of Go `headMetadata` plus the per-page checks of `validateAzurePage`.
/// The body must have a fixed length, so chunked framing is refused.
pub fn blob(head: &MessageHead, object: &AzureObject) -> Result<Blob> {
    if head
        .unique("Transfer-Encoding")
        .map_err(|_| Error::BadGateway)?
        .is_some()
    {
        return Err(Error::BadGateway);
    }
    let content_length = field(head, "Content-Length")?
        .ok_or(Error::BadGateway)
        .and_then(|value| parse_decimal(value).map_err(|_| Error::BadGateway))?;
    if field(head, "x-ms-blob-type")? != Some(b"BlockBlob")
        || (!object.version_id.is_empty()
            && field(head, "x-ms-version-id")? != Some(object.version_id.as_bytes()))
        || field(head, "x-ms-access-tier")? == Some(b"Archive")
        || field(head, "x-ms-archive-status")?.is_some_and(|value| !value.is_empty())
    {
        return Err(Error::Internal);
    }
    // Racer metadata cannot carry Content-Encoding to the reader.
    if field(head, "Content-Encoding")?
        .is_some_and(|value| !value.is_empty() && value != b"identity")
    {
        return Err(Error::Internal);
    }
    // A missing or unparsable field is a malformed upstream response.
    let etag = StrongEtag::parse(field(head, "ETag")?.ok_or(Error::BadGateway)?)
        .map_err(|_| Error::BadGateway)?;
    let content_type = match field(head, "Content-Type")? {
        None | Some(b"") => None,
        Some(value) => Some(ContentType::parse(value).map_err(|_| Error::BadGateway)?),
    };
    Ok(Blob {
        etag,
        content_type,
        content_length,
    })
}

/// Parse `Content-Range: bytes F-L/S` exactly as Go formats it. A missing
/// or malformed value is `BadGateway`.
pub fn content_range(head: &MessageHead) -> Result<(u64, u64, u64)> {
    let value = field(head, "Content-Range")?.ok_or(Error::BadGateway)?;
    let parse = || -> Option<(u64, u64, u64)> {
        let value = value.strip_prefix(b"bytes ")?;
        let dash = value.iter().position(|b| *b == b'-')?;
        let slash = value.iter().position(|b| *b == b'/')?;
        if slash < dash {
            return None;
        }
        let first = parse_decimal(&value[..dash]).ok()?;
        let last = parse_decimal(&value[dash + 1..slash]).ok()?;
        let total = parse_decimal(&value[slash + 1..]).ok()?;
        (first <= last && last < total).then_some((first, last, total))
    };
    parse().ok_or(Error::BadGateway)
}

#[cfg(test)]
pub(super) fn response(status: u16, fields: &[(&str, &[u8])]) -> MessageHead {
    MessageHead {
        start: StartLine::Response { status },
        headers: fields
            .iter()
            .map(|(name, value)| http1::Header::new(*name, value))
            .collect(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn object(version: &str) -> AzureObject {
        AzureObject {
            bucket: "models".into(),
            key: "k".into(),
            version_id: version.into(),
        }
    }

    const OK: &[(&str, &[u8])] = &[
        ("Content-Length", b"5"),
        ("x-ms-blob-type", b"BlockBlob"),
        ("ETag", b"\"0x8D\""),
        ("Content-Type", b"text/plain"),
    ];

    fn with(extra: &[(&'static str, &'static [u8])], drop: &str) -> MessageHead {
        let mut fields: Vec<_> = OK.iter().copied().filter(|f| f.0 != drop).collect();
        fields.extend_from_slice(extra);
        response(200, &fields)
    }

    #[test]
    fn classification_matches_go() {
        for (code, want) in [
            ("BlobNotFound", Error::NotFound),
            ("ContainerNotFound", Error::NotFound),
            ("ResourceNotFound", Error::NotFound),
            ("AuthenticationFailed", Error::OriginRejected),
            ("InvalidAuthenticationInfo", Error::OriginRejected),
            ("NoAuthenticationInformation", Error::OriginRejected),
            ("AuthorizationFailure", Error::OriginForbidden),
            ("AuthorizationPermissionMismatch", Error::OriginForbidden),
            ("AuthorizationProtocolMismatch", Error::OriginForbidden),
            ("AuthorizationResourceTypeMismatch", Error::OriginForbidden),
            ("AuthorizationServiceMismatch", Error::OriginForbidden),
            ("AuthorizationSourceIPMismatch", Error::OriginForbidden),
            ("InsufficientAccountPermissions", Error::OriginForbidden),
            ("AccountIsDisabled", Error::OriginForbidden),
            ("ConditionNotMet", Error::VersionUnavailable),
            ("TargetConditionNotMet", Error::VersionUnavailable),
            ("SourceConditionNotMet", Error::VersionUnavailable),
            ("BlobOverwritten", Error::VersionUnavailable),
            ("ServerBusy", Error::Unavailable),
            ("InternalError", Error::Unavailable),
            ("OperationTimedOut", Error::Unavailable),
            ("BlobArchived", Error::Internal),
            ("BlobBeingRehydrated", Error::Internal),
        ] {
            // The code wins over a contradictory status.
            let head = response(400, &[("x-ms-error-code", code.as_bytes())]);
            assert_eq!(classify(&head, false), want, "{code}");
            let pinned = if want == Error::NotFound {
                Error::VersionUnavailable
            } else {
                want
            };
            assert_eq!(classify(&head, true), pinned, "{code}");
        }
        for (status, want) in [
            (401, Error::OriginRejected),
            (403, Error::OriginForbidden),
            (404, Error::NotFound),
            (412, Error::VersionUnavailable),
            (408, Error::Unavailable),
            (429, Error::Unavailable),
            (500, Error::Unavailable),
            (503, Error::Unavailable),
            (599, Error::Unavailable),
            (400, Error::Internal),
            (409, Error::Internal),
            (416, Error::Internal),
            (302, Error::Internal),
        ] {
            let head = response(status, &[("x-ms-error-code", b"SomethingNew")]);
            assert_eq!(classify(&head, false), want, "{status}");
        }
        assert_eq!(
            classify(&response(404, &[]), true),
            Error::VersionUnavailable
        );
        let duplicate = response(
            503,
            &[
                ("x-ms-error-code", b"BlobNotFound"),
                ("x-ms-error-code", b"x"),
            ],
        );
        assert_eq!(classify(&duplicate, false), Error::Unavailable);
        for error in [Error::Io, Error::Os(104), Error::Unavailable] {
            assert_eq!(transport_error(error), Error::Unavailable);
        }
        for error in [Error::DeadlineExceeded, Error::Canceled] {
            assert_eq!(transport_error(error), error);
        }
        for error in [
            Error::HeaderTooLarge,
            Error::InvalidRequest,
            Error::CorruptRecord,
        ] {
            assert_eq!(transport_error(error), Error::BadGateway);
        }
    }

    #[test]
    fn blob_validation_matches_go_head_metadata() {
        let blob = blob(&with(&[], ""), &object("")).unwrap();
        assert_eq!(blob.etag.as_str(), "\"0x8D\"");
        assert_eq!(blob.content_length, 5);
        assert_eq!(blob.content_type.unwrap().as_str(), "text/plain");
        assert!(
            super::blob(&with(&[], "Content-Type"), &object(""))
                .unwrap()
                .content_type
                .is_none()
        );
        assert!(
            super::blob(&with(&[("Content-Type", b"")], "Content-Type"), &object(""))
                .unwrap()
                .content_type
                .is_none()
        );
        for extra in [
            &[("Content-Encoding", &b"identity"[..])][..],
            &[("Content-Encoding", b"")],
        ] {
            super::blob(&with(extra, ""), &object("")).unwrap();
        }
        super::blob(&with(&[("x-ms-version-id", b"v1")], ""), &object("v1")).unwrap();
        super::blob(&with(&[("x-ms-access-tier", b"Hot")], ""), &object("")).unwrap();

        for (extra, drop, version) in [
            (&[][..], "x-ms-blob-type", ""),
            (
                &[("x-ms-blob-type", &b"PageBlob"[..])][..],
                "x-ms-blob-type",
                "",
            ),
            (&[("x-ms-access-tier", b"Archive")], "", ""),
            (
                &[("x-ms-archive-status", b"rehydrate-pending-to-hot")],
                "",
                "",
            ),
            (&[("Content-Encoding", b"gzip")], "", ""),
            (&[], "", "v1"),
            (&[("x-ms-version-id", b"v2")], "", "v1"),
        ] {
            assert_eq!(
                super::blob(&with(extra, drop), &object(version)).map(|_| ()),
                Err(Error::Internal),
                "{extra:?} -{drop}"
            );
        }
        for (extra, drop) in [
            (&[][..], "Content-Length"),
            (&[("Content-Length", &b"05"[..])][..], "Content-Length"),
            (&[("Content-Length", b"-1")], "Content-Length"),
            (&[("Content-Length", b"5")], ""),
            (&[("Transfer-Encoding", b"chunked")], ""),
            (&[("ETag", b"\"other\"")], ""),
            (&[], "ETag"),
            (&[("ETag", b"W/\"weak\"")], "ETag"),
            (&[("ETag", b"\"bad\"x\"")], "ETag"),
            (&[("ETag", b"\"\"\"")], "ETag"),
            (&[("Content-Type", b"not a type")], "Content-Type"),
        ] {
            assert_eq!(
                super::blob(&with(extra, drop), &object("")).map(|_| ()),
                Err(Error::BadGateway),
                "{extra:?} -{drop}"
            );
        }
    }

    #[test]
    fn content_range_is_exact() {
        let range =
            |value: &'static [u8]| content_range(&response(206, &[("Content-Range", value)]));
        assert_eq!(range(b"bytes 0-4/5"), Ok((0, 4, 5)));
        assert_eq!(
            range(b"bytes 16777216-16777217/16777218"),
            Ok((16777216, 16777217, 16777218))
        );
        for bad in [
            &b"bytes 0-4/4"[..],
            b"bytes 4-0/5",
            b"bytes 00-4/5",
            b"bytes 0-4/*",
            b"bytes */5",
            b"bytes=0-4/5",
            b"0-4/5",
            b"bytes 0-4/5/6",
            b"bytes 0-4-5/6",
            b"bytes  0-4/5",
        ] {
            assert_eq!(
                range(bad),
                Err(Error::BadGateway),
                "{}",
                String::from_utf8_lossy(bad)
            );
        }
        assert_eq!(content_range(&response(206, &[])), Err(Error::BadGateway));
    }
}
