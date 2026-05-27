// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use bytes::Bytes;
use futures::stream::BoxStream;

use super::error::Error;
use crate::catalog::ObjectMeta;

/// The single seam between the S3 server and the storage backend.
///
/// - The production impl (`BlockStoreObjectSource`) routes requests
///   through a registered `BlockStore`, copying each chunk out of the
///   shared ring before yielding.
/// - The test impl (`MemoryObjectSource`) serves in-memory buffers.
pub trait ObjectSource: Send + Sync + 'static {
    /// Return a stream of `Bytes` covering exactly `[offset, offset+len)`
    /// of the object identified by `meta.stripe`, in order. Each item
    /// is an independent heap allocation copied from the pool page;
    /// dropping the `Bytes` frees the buffer immediately.
    fn read_range(
        &self,
        meta: &ObjectMeta,
        offset: u64,
        len: u64,
    ) -> BoxStream<'static, Result<Bytes, Error>>;
}
