//! Azure Blob upload: Put Block and Put Block List for written objects.
//!
//! Each call is one request (plus one retry after a 401 for a refreshed
//! token), admitted by the cache's `max_concurrent_requests` and bounded by
//! its `request_timeout`, like reads. Responses map as follows:
//!
//! - `201 Created`: success. Put Block List returns the response `ETag`.
//! - Put Block List `409 BlobAlreadyExists` or any `412`: [`CommitConflict`].
//! - `403`: `OriginForbidden`; `401`: `OriginRejected` (after the retry).
//! - `429`, `5xx` (`ServerBusy`, `InternalError`, `OperationTimedOut`):
//!   `Unavailable`, retryable (see [`crate::origin::upload::retryable`]).
//! - anything else: classified like reads (`response::classify`).

use super::request;
use super::response::{field, status};
use super::{AzureExchange, AzureOrigin, AzureTokenSource, AzureTransport};
use crate::error::{Error, Operation, Result};
use crate::model::StrongEtag;
use crate::origin::upload::{CommitConflict, UploadSink};
use crate::runtime::RequestScope;
use crate::security::OriginContext;
use racer_wire::control::OriginDefinition;

impl<T: AzureTransport, K: AzureTokenSource> AzureOrigin<T, K> {
    /// Put Block against an explicit definition.
    pub async fn put_block_with(
        &self,
        definition: &OriginDefinition,
        context: &OriginContext,
        block_id: &[u8; 64],
        plaintext: &[u8],
        scope: &RequestScope,
    ) -> Result<()> {
        let begun = self.begin(definition, context, scope).await?;
        let url = &definition.azure.container_url;
        let crc = super::crc64::checksum(plaintext);
        let length = plaintext.len() as u64;
        let exchange = self
            .send_with(
                definition,
                &begun,
                |bearer, now| {
                    request::put_block_head(url, &begun.object, block_id, length, crc, bearer, now)
                },
                Some(plaintext),
            )
            .await?;
        if status(exchange.head())? != 201 {
            return Err(self.reject(exchange, false, &begun.scope).await);
        }
        // The service echoes the CRC it verified; a mismatch means the
        // response is not for this body.
        let expected = super::crc64::header_value(crc);
        if field(exchange.head(), "x-ms-content-crc64")?
            .is_some_and(|echo| echo != expected.as_bytes())
        {
            return Err(Error::BadGateway);
        }
        // Success is final; a body that cannot be drained only costs the
        // connection.
        let _ = exchange.discard(&begun.scope).await;
        Ok(())
    }

    /// Put Block List against an explicit definition.
    pub async fn put_block_list_with(
        &self,
        definition: &OriginDefinition,
        context: &OriginContext,
        block_ids: &[[u8; 64]],
        content_type: Option<&str>,
        scope: &RequestScope,
    ) -> Result<std::result::Result<StrongEtag, CommitConflict>> {
        if block_ids.is_empty() {
            // Racer objects are nonempty, so every commit names a block.
            return Err(Error::InvalidRequest);
        }
        let body = request::block_list_xml(block_ids)?;
        let begun = self.begin(definition, context, scope).await?;
        let url = &definition.azure.container_url;
        let exchange = self
            .send_with(
                definition,
                &begun,
                |bearer, now| {
                    request::put_block_list_head(
                        url,
                        &begun.object,
                        &body,
                        content_type,
                        bearer,
                        now,
                    )
                },
                Some(&body),
            )
            .await?;
        let head = exchange.head();
        let code = status(head)?;
        let conflict = code == 412
            || (code == 409 && field(head, "x-ms-error-code")? == Some(b"BlobAlreadyExists"));
        if conflict {
            let _ = exchange.discard(&begun.scope).await;
            return Ok(Err(CommitConflict));
        }
        if code != 201 {
            return Err(self.reject(exchange, false, &begun.scope).await);
        }
        let etag = field(exchange.head(), "ETag")?
            .ok_or(Error::BadGateway)
            .and_then(|etag| StrongEtag::parse(etag).map_err(|_| Error::BadGateway))?;
        let _ = exchange.discard(&begun.scope).await;
        Ok(Ok(etag))
    }

    /// The published origin for the context's cache.
    fn upload_definition(
        &self,
        context: &OriginContext,
    ) -> Result<(std::sync::Arc<crate::control::Snapshot>, usize)> {
        self.definition(&context.object)
    }
}

impl<T: AzureTransport, K: AzureTokenSource> UploadSink for AzureOrigin<T, K> {
    fn put_block<'a>(
        &'a self,
        context: &'a OriginContext,
        block_id: &'a [u8; 64],
        plaintext: &'a [u8],
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        Box::pin(async move {
            scope.check()?;
            let (snapshot, index) = self.upload_definition(context)?;
            let definition = snapshot.caches[index]
                .origin
                .as_ref()
                .ok_or(Error::Unavailable)?;
            self.put_block_with(definition, context, block_id, plaintext, scope)
                .await
        })
    }

    fn put_block_list<'a>(
        &'a self,
        context: &'a OriginContext,
        block_ids: &'a [[u8; 64]],
        content_type: Option<&'a str>,
        scope: &'a RequestScope,
    ) -> Operation<'a, std::result::Result<StrongEtag, CommitConflict>> {
        Box::pin(async move {
            scope.check()?;
            let (snapshot, index) = self.upload_definition(context)?;
            let definition = snapshot.caches[index]
                .origin
                .as_ref()
                .ok_or(Error::Unavailable)?;
            self.put_block_list_with(definition, context, block_ids, content_type, scope)
                .await
        })
    }
}

/// Block ID for `page` of the Racer version `version_hex`: base64 of the 48
/// ASCII bytes `"{version_hex}{page:016x}"`, always 64 characters.
// WP2 provides the canonical version; switch on merge.
#[cfg(test)]
pub(crate) fn block_id(version_hex: &[u8; 32], page: u64) -> [u8; 64] {
    use base64::Engine;
    let mut raw = [0u8; 48];
    raw[..32].copy_from_slice(version_hex);
    raw[32..].copy_from_slice(format!("{page:016x}").as_bytes());
    let mut id = [0u8; 64];
    let written = base64::engine::general_purpose::STANDARD
        .encode_slice(raw, &mut id)
        .unwrap_or_default();
    debug_assert_eq!(written, 64);
    id
}

#[cfg(test)]
mod tests;
