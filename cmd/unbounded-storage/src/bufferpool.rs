// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::time::Instant;

#[derive(Debug)]
pub struct Error;

/// Content-addressed identity of a fixed-length stripe.
#[derive(Copy, Clone, Eq, PartialEq, Hash, Debug)]
pub struct StripeKey(pub [u8; 32]);

/// Opaque identity of a remote node.
#[derive(Copy, Clone, Eq, PartialEq, Hash, Debug)]
pub struct PeerId(pub u64);

/// Submission/completion ring (e.g. an io_uring adapter).
pub trait Ring {
    type Sqe;
    type Cqe;
    fn push_sqe(&self, sqe: Self::Sqe) -> Result<(), Error>;
    fn submit_and_wait(&self, want: u32) -> Result<(), Error>;
    fn user_data(cqe: &Self::Cqe) -> u64;
    fn result(cqe: &Self::Cqe) -> i32;
}

/// Borrow of the ring whose CQE drove the current callback. Sinks use it to
/// push reply SQEs against the same ring on the same thread.
pub struct RingCtx<'a, R: Ring> {
    _marker: std::marker::PhantomData<&'a R>,
}

/// Per-read metadata carried end-to-end through the cache stack.
pub trait Req {
    fn key(&self) -> StripeKey;
    fn deadline(&self) -> Instant;
}

/// Reference to a pool page already registered as an io_uring fixed buffer.
/// Identical `buf_index` across every ring in the NUMA domain.
#[derive(Copy, Clone, Debug)]
pub struct FixedBuf {
    pub buf_index: u16,
    pub offset: u64,
    pub len: usize,
}

/// RAII hold on a pool page. Drop refunds credit and may release the page.
pub struct PageGuard<'pool> {
    _marker: std::marker::PhantomData<&'pool ()>,
}

/// Caller-owned destination for read results. Callbacks fire on the driver
/// thread that observed the underlying completion.
pub trait Sink<R: Ring> {
    fn on_page(&mut self, rx: &RingCtx<'_, R>, page: PageGuard<'_>);
    fn on_eof(&mut self, rx: &RingCtx<'_, R>);
    fn on_error(&mut self, rx: &RingCtx<'_, R>, err: Error);
}

/// Lifetime token tying a `Sink` borrow to the in-flight read. Drop cancels.
pub struct ReadHandle<'r> {
    _marker: std::marker::PhantomData<&'r ()>,
}

/// Top-level read entry point exposed to filesystems above the pool.
pub trait ChunkCache<R: Ring> {
    type Req: Req;
    fn read<'r>(
        &self,
        req: &Self::Req,
        offset: u64,
        len: u64,
        sink: &'r mut dyn Sink<R>,
    ) -> Result<ReadHandle<'r>, Error>;
}

/// Remote-side description of a bulk source region (RDMA address + rkey).
#[derive(Copy, Clone, Debug)]
pub struct BulkRef {
    pub addr: u64,
    pub len: u64,
    pub rkey: u32,
}

/// Bulk data transport (e.g. RDMA).
pub trait Transport {
    type Completion;
    fn bulk_get(&self, peer: PeerId, src: BulkRef, dst: FixedBuf, tag: u64);
}

/// Persistent tee. On a miss the pool calls `write_fixed` per page; the
/// embedder mints the concrete write SQE via `build_sqe`.
pub trait BlockStore<R: Ring> {
    fn write_fixed(
        &self,
        rx: &RingCtx<'_, R>,
        key: StripeKey,
        stripe_off: u64,
        page: FixedBuf,
        tag: u64,
        build_sqe: &mut dyn FnMut(FixedBuf, u64) -> R::Sqe,
    ) -> Result<(), Error>;
}
