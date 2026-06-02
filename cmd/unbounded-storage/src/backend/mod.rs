// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

#![allow(async_fn_in_trait)]

mod null;

#[cfg(target_os = "linux")]
mod http;

use std::sync::Arc;

use crate::bufferpool::{BulkRef, PageRef, PageStream, Req};

pub use null::NullBackend;

#[cfg(target_os = "linux")]
pub use http::HttpBackend;

/// Origin fetch surface, sibling to `bufferpool::Transport`. A
/// `Backend` resolves a `BulkRef` from an authoritative origin (as
/// opposed to a peer) into the supplied destination pages, yielding
/// one page at a time through a `PageStream`.
pub trait Backend: Send + Sync {
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

/// Blanket impl mirroring `Transport for Arc<T>`, so a `Backend` can
/// be shared across shards by handing each consumer an `Arc`-wrapped
/// clone instead of an owned instance.
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

#[cfg(test)]
mod tests {
    use std::pin::Pin;
    use std::task::{Context, Poll, RawWaker, RawWakerVTable, Waker};

    use crate::bufferpool::{BulkRef, Error, PageRef, PageStream, Req, StripeKey};

    use super::Backend;

    fn noop_waker() -> Waker {
        fn raw() -> RawWaker {
            RawWaker::new(std::ptr::null(), &VTABLE)
        }
        static VTABLE: RawWakerVTable = RawWakerVTable::new(|_| raw(), |_| {}, |_| {}, |_| {});
        // SAFETY: VTABLE clone/wake/drop are all no-ops on static data.
        unsafe { Waker::from_raw(raw()) }
    }

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
