// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::fmt;
use std::sync::Arc;

use serde::{Deserialize, Serialize};

/// Stable page identity. `page_idx` indexes into the pool's backing
/// as `u32`; `u16` would cap the backing at 128 GiB at 2 MiB pages,
/// which is smaller than a single Grace socket. `offset` and `len`
/// are intra-page (bounded by `page_size`, which is 2 MiB in v1),
/// so they fit in `u32`.
#[derive(Copy, Clone, Debug, PartialEq, Eq, Hash)]
pub struct PageRef {
    pub page_idx: u32,
    pub offset: u32,
    pub len: u32,
}

/// Opaque content-addressed identifier for a 1 GiB stripe. The pool
/// uses it only for `==`/`hash` in single-flight coalescing.
#[derive(Copy, Clone, Debug, PartialEq, Eq, Hash, Serialize, Deserialize)]
pub struct StripeKey(pub [u8; 32]);

/// A byte range within a peer-side stripe, passed to
/// `Transport::bulk_get`. `len` is bounded by `page_size`.
#[derive(Copy, Clone, Debug, PartialEq, Eq, Hash)]
pub struct BulkRef {
    pub stripe: StripeKey,
    pub offset: u64,
    pub len: u32,
}

/// Opaque tracing handle. The pool does not inspect it.
#[derive(Clone, Debug, Default)]
pub struct TraceCtx;

/// Pool tunables. Most of the design's TODO knobs are still TBD;
/// see `designs/bufferpool.md` "TODO(config)".
#[derive(Clone, Debug)]
pub struct PoolConfig {
    /// Caps the number of concurrent `ReadStream`s the pool will
    /// admit. v1 enforces this only at `read()` time.
    pub max_concurrent_streams: usize,
    /// Global cap, shared across every windowed stream the pool is
    /// serving, on speculative prefetch pages: pages a
    /// [`crate::bufferpool::WindowedRead`] has launched a fetch for
    /// while they sit strictly ahead of that stream's consumer
    /// cursor and have not yet been consumed. The head-of-line page
    /// (the one at the cursor) is always fetched and never counts
    /// against this budget, so forward progress is guaranteed even
    /// when the budget is fully reserved. Bounding speculation this
    /// way keeps prefetch from starving the free list out from under
    /// any stream's head fetch.
    pub max_inflight_pages: usize,
}

impl Default for PoolConfig {
    fn default() -> Self {
        Self {
            max_concurrent_streams: 1024,
            max_inflight_pages: 64,
        }
    }
}

#[derive(Debug, Clone)]
pub enum Error {
    BadConfig(&'static str),
    Io(i32),
    BackingFull,
    PageOutOfRange,
    OffsetOutOfRange,
    StreamLimit,
    Transport(Arc<dyn std::error::Error + Send + Sync>),
    BlockStore(Arc<dyn std::error::Error + Send + Sync>),
    /// Internal sentinel: a speculative (prefetch) fetch found the
    /// free list empty and backed off rather than parking on it, so
    /// the head fetch keeps priority on scarce pages. Never reaches a
    /// stream consumer: [`crate::bufferpool::WindowedRead`] intercepts
    /// it on the speculative path and a head fetch is never
    /// speculative.
    PrefetchBackoff,
    /// Transient back-pressure: a non-blocking (cross-shard remote)
    /// head fetch found no free page available without parking, so it
    /// failed fast rather than holding pins while blocked. Retryable:
    /// the caller (a remote coordinator) should re-dispatch the fetch
    /// after yielding. See [`crate::bufferpool::Pool::read_owned`] and
    /// `FreeList::try_alloc_head`. Used only on the owned-read path;
    /// local blocking reads never produce it.
    Busy,
    /// The requested origin object does not exist (an HTTP/S3 404 from
    /// the origin). Distinct from a transport failure: the fetch
    /// completed, the origin simply has no such object.
    OriginNotFound,
    /// The origin rejected a conditional GET because the object version
    /// changed after its metadata entry was cached.
    OriginVersionMismatch,
}

impl Error {
    pub fn transport<E>(e: E) -> Self
    where
        E: std::error::Error + Send + Sync + 'static,
    {
        Error::Transport(Arc::new(e))
    }

    pub fn blockstore<E>(e: E) -> Self
    where
        E: std::error::Error + Send + Sync + 'static,
    {
        Error::BlockStore(Arc::new(e))
    }
}

impl From<&'static str> for Error {
    fn from(s: &'static str) -> Self {
        // Convenient for tests and embedders that just need a message.
        Error::Transport(Arc::new(StrError(s)))
    }
}

#[derive(Debug)]
struct StrError(&'static str);

impl fmt::Display for StrError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.0)
    }
}

impl std::error::Error for StrError {}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::BadConfig(s) => write!(f, "bad config: {s}"),
            Error::Io(e) => write!(f, "io error: errno={e}"),
            Error::BackingFull => write!(f, "backing full"),
            Error::PageOutOfRange => write!(f, "page index out of range"),
            Error::OffsetOutOfRange => write!(f, "read offset out of range"),
            Error::StreamLimit => write!(f, "max_concurrent_streams reached"),
            Error::Transport(e) => write!(f, "transport error: {e}"),
            Error::BlockStore(e) => write!(f, "block store error: {e}"),
            Error::PrefetchBackoff => write!(f, "speculative prefetch backed off"),
            Error::Busy => write!(f, "pool busy: no free page for non-blocking fetch"),
            Error::OriginNotFound => write!(f, "origin object not found"),
            Error::OriginVersionMismatch => write!(f, "origin object version mismatch"),
        }
    }
}

impl std::error::Error for Error {}
