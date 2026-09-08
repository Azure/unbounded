// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

#![allow(async_fn_in_trait)]

mod null;

pub mod url;

#[cfg(target_os = "linux")]
mod conn;

#[cfg(target_os = "linux")]
mod http;

#[cfg(target_os = "linux")]
mod limiter;

#[cfg(target_os = "linux")]
mod origin;

#[cfg(target_os = "linux")]
mod registry;

#[cfg(target_os = "linux")]
mod s3;

#[cfg(target_os = "linux")]
mod s3_sigv4;

#[cfg(target_os = "linux")]
mod azure;

#[cfg(target_os = "linux")]
mod fake;

use std::sync::Arc;

use crate::bufferpool::{BulkRef, PageRef, PageStream, Req};

pub use null::NullBackend;

#[cfg(target_os = "linux")]
pub use http::HttpBackend;

#[cfg(target_os = "linux")]
pub use limiter::{Acquire, FetchLimiter, FetchPermit};

#[cfg(target_os = "linux")]
pub use origin::{OriginBackend, OriginStream};

#[cfg(target_os = "linux")]
pub use registry::{BackendRegistry, RegistryFetchStream};

#[cfg(target_os = "linux")]
pub use s3::S3Backend;

#[cfg(target_os = "linux")]
pub(crate) use s3_sigv4::S3Auth;

#[cfg(target_os = "linux")]
pub use azure::AzureBackend;

#[cfg(target_os = "linux")]
pub use fake::{FakeBackend, FakeFetchStream};

/// Origin fetch surface, sibling to `bufferpool::Transport`. A
/// `Backend` resolves a `BulkRef` from an authoritative origin (as
/// opposed to a peer) into the supplied destination pages, yielding
/// one page at a time through a `PageStream`.
pub trait Backend {
    type Req: Req;

    /// Stream of pages produced by `bulk_get`. One `poll_next` may
    /// yield `Some(Ok(page))` per delivered page; `None` ends the
    /// stream successfully. Errors surface as `Some(Err(_))`.
    type Stream<'a>: PageStream + 'a
    where
        Self: 'a;

    /// Fetch the byte range described by `src` from the origin
    /// derived from `req` into each of the pages in `dsts`. Returns
    /// a stream that yields one `PageRef` per delivered page; the
    /// stream itself drives the async work via `poll_next`.
    fn bulk_get<'a>(
        &'a self,
        req: &'a Self::Req,
        src: BulkRef,
        dsts: &'a [PageRef],
    ) -> Self::Stream<'a>;
}

/// Blanket impl mirroring `Transport for Arc<T>` for callers that
/// already own a backend through an `Arc`.
impl<T: Backend + ?Sized> Backend for Arc<T> {
    type Req = T::Req;

    type Stream<'a>
        = T::Stream<'a>
    where
        Self: 'a;

    fn bulk_get<'a>(
        &'a self,
        req: &'a Self::Req,
        src: BulkRef,
        dsts: &'a [PageRef],
    ) -> Self::Stream<'a> {
        (**self).bulk_get(req, src, dsts)
    }
}

/// Bridge a pool's [`RecvQuarantineHandle`] onto a shard's
/// [`NetworkRing`], so a canceled fixed-buffer RECV withholds its
/// destination bufferpool page from reuse until the kernel is done with
/// it (its RECV CQE is reaped). Install once per shard, before serving
/// begins, on the ring whose `recv_fixed` destinations are pool pages
/// (the shard socket ring). Rings writing into non-pool memory (e.g. a
/// worker-local scratch backing) are left without a sink and fall back
/// to the blocking-but-sound drain on drop.
pub fn install_recv_quarantine(
    ring: &crate::ring::NetworkRing,
    handle: crate::bufferpool::RecvQuarantineHandle,
) {
    use std::rc::Rc;

    /// Adapter implementing the ring's `RecvQuarantine` in terms of the
    /// bufferpool free-list handle. Both are keyed by byte offset into
    /// the registered backing, so this is a thin forward.
    struct PoolRecvQuarantine(crate::bufferpool::RecvQuarantineHandle);

    impl crate::ring::RecvQuarantine for PoolRecvQuarantine {
        fn quarantine(&self, page_byte_offset: usize) {
            self.0.quarantine(page_byte_offset);
        }

        fn reclaim(&self, page_byte_offset: usize) {
            self.0.reclaim(page_byte_offset);
        }
    }

    ring.set_recv_quarantine(Rc::new(PoolRecvQuarantine(handle)));
}

#[cfg(test)]
mod tests {
    use std::pin::Pin;
    use std::task::{Context, Poll};

    use crate::bufferpool::{BulkRef, Error, PageRef, PageStream, Req, StripeKey};
    use crate::runtime::noop_waker;

    use super::Backend;

    /// Drive a future to completion on a single thread using a noop
    /// waker, with a generous spin bound that fails loudly rather
    /// than hanging on no-progress.
    fn block_on<F: std::future::Future>(fut: F) -> F::Output {
        let w = noop_waker();
        let mut cx = Context::from_waker(&w);
        let mut fut = std::pin::pin!(fut);
        let mut spins: u64 = 0;
        loop {
            match fut.as_mut().poll(&mut cx) {
                Poll::Ready(v) => return v,
                Poll::Pending => {
                    spins += 1;
                    assert!(spins < 1_000_000, "block_on stuck without progress");
                }
            }
        }
    }

    struct TestReq(StripeKey);

    impl Req for TestReq {
        fn key(&self) -> StripeKey {
            self.0
        }
    }

    /// Yields each page in `dsts` one `poll_next` at a time, then
    /// `None`. Returns `Pending` once between pages so the executor
    /// must spin, exercising the noop-waker driver.
    struct VecStream {
        pages: Vec<PageRef>,
        next: usize,
        gated: bool,
    }

    impl PageStream for VecStream {
        fn poll_next(
            mut self: Pin<&mut Self>,
            cx: &mut Context<'_>,
        ) -> Poll<Option<Result<PageRef, Error>>> {
            if self.next >= self.pages.len() {
                return Poll::Ready(None);
            }
            if !self.gated {
                self.gated = true;
                cx.waker().wake_by_ref();
                return Poll::Pending;
            }
            let page = self.pages[self.next];
            self.next += 1;
            self.gated = false;
            Poll::Ready(Some(Ok(page)))
        }
    }

    /// In-memory `Backend` that echoes the requested `dsts` back as
    /// delivered pages, in order.
    struct EchoBackend;

    impl Backend for EchoBackend {
        type Req = TestReq;
        type Stream<'a> = VecStream;

        fn bulk_get<'a>(
            &'a self,
            _req: &'a Self::Req,
            _src: BulkRef,
            dsts: &'a [PageRef],
        ) -> Self::Stream<'a> {
            VecStream {
                pages: dsts.to_vec(),
                next: 0,
                gated: false,
            }
        }
    }

    #[test]
    fn echo_backend_yields_pages_in_order() {
        let backend = EchoBackend;
        let req = TestReq(StripeKey([7u8; 32]));
        let src = BulkRef {
            stripe: req.key(),
            offset: 0,
            len: 4096,
        };
        let dsts = [
            PageRef {
                page_idx: 0,
                offset: 0,
                len: 4096,
            },
            PageRef {
                page_idx: 1,
                offset: 0,
                len: 4096,
            },
            PageRef {
                page_idx: 2,
                offset: 0,
                len: 4096,
            },
        ];

        let collected = block_on(async {
            let mut stream = std::pin::pin!(backend.bulk_get(&req, src, &dsts));
            let mut out = Vec::new();
            loop {
                let w = noop_waker();
                let mut cx = Context::from_waker(&w);
                match stream.as_mut().poll_next(&mut cx) {
                    Poll::Ready(Some(Ok(page))) => out.push(page),
                    Poll::Ready(Some(Err(e))) => panic!("unexpected stream error: {e:?}"),
                    Poll::Ready(None) => break,
                    Poll::Pending => yield_once().await,
                }
            }
            out
        });

        assert_eq!(collected, dsts.to_vec());
    }

    /// Yield back to the driver once so a `Pending` stream poll does
    /// not busy-spin the async block.
    fn yield_once() -> YieldOnce {
        YieldOnce { yielded: false }
    }

    struct YieldOnce {
        yielded: bool,
    }

    impl std::future::Future for YieldOnce {
        type Output = ();
        fn poll(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
            if self.yielded {
                Poll::Ready(())
            } else {
                self.yielded = true;
                cx.waker().wake_by_ref();
                Poll::Pending
            }
        }
    }
}
