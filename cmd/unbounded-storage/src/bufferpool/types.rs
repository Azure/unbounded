// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::fmt;
use std::sync::Arc;

/// Pinned, NUMA-local backing. The embedder allocates it (e.g.
/// `mmap(MAP_HUGETLB | MAP_HUGE_2MB)`); the pool just carves pages
/// out of it. `_own` holds the Drop handle for the underlying
/// allocation; the pool never touches it.
///
/// `base` is a raw pointer, so `Backing` is `!Send + !Sync` by
/// default. The pool relies on the embedder upholding the per-NUMA
/// pinning invariant and adds `unsafe impl Send + Sync` so
/// `Pool<T, S>` can be constructed off the executor thread before
/// being handed to the pinned executor for service.
pub struct Backing {
    pub base: *mut u8,
    pub page_size: usize,
    pub page_count: usize,
    /// Drop carrier for the underlying allocation. The pool never
    /// touches this; it exists so the mapping outlives the pool.
    pub _own: Box<dyn Send + Sync>,
}

// SAFETY: per-NUMA pinning is the embedder's responsibility (see
// the design's "Constraints" section). Inside its NUMA shard the
// `Backing` is owned by a single-threaded `Pool`; we mark it
// `Send + Sync` so the constructed `Pool` can be moved onto the
// pinned executor thread.
unsafe impl Send for Backing {}
unsafe impl Sync for Backing {}

impl fmt::Debug for Backing {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Backing")
            .field("base", &self.base)
            .field("page_size", &self.page_size)
            .field("page_count", &self.page_count)
            .finish()
    }
}

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
#[derive(Copy, Clone, Debug, PartialEq, Eq, Hash)]
pub struct StripeKey(pub [u8; 32]);

/// A byte range within a peer-side stripe, passed to
/// `Transport::bulk_get`. `len` is bounded by `page_size`.
#[derive(Copy, Clone, Debug, PartialEq, Eq, Hash)]
pub struct BulkRef {
    pub stripe: StripeKey,
    pub offset: u64,
    pub len: u32,
}

/// Opaque peer identifier minted by the p2p layer.
#[derive(Copy, Clone, Debug, PartialEq, Eq, Hash)]
pub struct PeerId(pub u64);

/// Opaque node identifier minted by the p2p layer.
#[derive(Copy, Clone, Debug, PartialEq, Eq, Hash)]
pub struct NodeId(pub u64);

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
}

impl Default for PoolConfig {
    fn default() -> Self {
        Self {
            max_concurrent_streams: 1024,
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
        }
    }
}

impl std::error::Error for Error {}
