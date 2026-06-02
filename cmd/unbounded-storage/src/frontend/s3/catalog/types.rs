// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! The resolved per-object metadata an S3 response is built from, plus
//! the raw YAML row it is parsed out of.

use std::fmt;

use hex::FromHexError;
use serde::Deserialize;

use crate::bufferpool::StripeKey;

/// Default `Last-Modified` value (RFC 7231 IMF-fixdate) used when a
/// catalog entry omits an explicit timestamp. The Unix epoch is a
/// deliberately conspicuous placeholder so an operator who sees it in
/// an S3 response knows the catalog did not provide a real value.
pub(crate) const EPOCH_IMF: &str = "Thu, 01 Jan 1970 00:00:00 GMT";

/// Metadata about one S3 object resolved from the catalog.
///
/// In v0 an object is exactly one stripe: [`Self::stripe`] is the
/// `StripeKey` the GET body is read from, and [`Self::size`] is the
/// object's full length within that stripe.
#[derive(Clone, Debug)]
pub struct ObjectMeta {
    pub stripe: StripeKey,
    pub size: u64,
    /// Deterministic ETag derived from the first 16 hex chars of `stripe`.
    pub etag: String,
    /// MIME type. Defaults to `application/octet-stream` when absent.
    pub content_type: String,
    /// IMF-fixdate string ready to be emitted as the `Last-Modified`
    /// response header. RFC 3339 input from the YAML is parsed and
    /// reformatted at load time so the request path never has to do
    /// any date math.
    pub last_modified: String,
}

/// Raw YAML representation of one catalog entry.
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct YamlObject {
    pub bucket: String,
    pub key: String,
    pub stripe: String,
    pub size: u64,
    #[serde(default)]
    pub content_type: Option<String>,
    /// RFC 3339 timestamp. Optional; defaults to the Unix epoch when
    /// absent so older catalogs continue to load.
    #[serde(default)]
    pub last_modified: Option<String>,
}

impl YamlObject {
    pub(crate) fn into_meta(self) -> Result<(String, String, ObjectMeta), MetaError> {
        let hex = self.stripe.trim();
        let bytes = hex::decode(hex)?;
        if bytes.len() != 32 {
            return Err(MetaError::BadStripeLength(bytes.len()));
        }
        let mut arr = [0u8; 32];
        arr.copy_from_slice(&bytes);
        let stripe = StripeKey(arr);
        let etag = format!("\"{}\"", &hex[..16]);
        let content_type = self
            .content_type
            .unwrap_or_else(|| "application/octet-stream".into());
        let last_modified = match self.last_modified.as_deref() {
            Some(s) => rfc3339_to_imf(s).map_err(MetaError::BadLastModified)?,
            None => EPOCH_IMF.to_string(),
        };
        let meta = ObjectMeta {
            stripe,
            size: self.size,
            etag,
            content_type,
            last_modified,
        };
        Ok((self.bucket, self.key, meta))
    }
}

/// Parse an RFC 3339 timestamp and reformat it as RFC 7231 IMF-fixdate.
///
/// The conversion lives here (rather than in the request path) so a
/// malformed timestamp fails catalog loading rather than every
/// request, and so the request path emits the header without any
/// per-request date work.
fn rfc3339_to_imf(s: &str) -> Result<String, String> {
    use time::OffsetDateTime;
    use time::format_description::well_known::Rfc3339;
    use time::macros::format_description;

    const IMF: &[time::format_description::BorrowedFormatItem<'_>] = format_description!(
        "[weekday repr:short], [day] [month repr:short] [year] [hour]:[minute]:[second] GMT"
    );

    let dt = OffsetDateTime::parse(s, &Rfc3339).map_err(|e| format!("{e}"))?;
    let utc = dt.to_offset(time::UtcOffset::UTC);
    utc.format(IMF).map_err(|e| format!("{e}"))
}

#[derive(Debug)]
pub(crate) enum MetaError {
    BadHex(FromHexError),
    BadStripeLength(usize),
    BadLastModified(String),
}

impl fmt::Display for MetaError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            MetaError::BadHex(e) => write!(f, "bad hex in stripe: {e}"),
            MetaError::BadStripeLength(n) => {
                write!(f, "stripe hex decoded to {n} bytes, expected 32")
            }
            MetaError::BadLastModified(e) => {
                write!(f, "bad last_modified (expected RFC 3339): {e}")
            }
        }
    }
}

impl From<FromHexError> for MetaError {
    fn from(e: FromHexError) -> Self {
        MetaError::BadHex(e)
    }
}

impl std::error::Error for MetaError {}
