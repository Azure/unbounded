// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Inert origin backend. `NullBackend` is the placeholder origin a
//! `RoutedTransport` delegates to when the local node owns a stripe
//! but no real backend has been wired in yet. Every `bulk_get`
//! yields a single error then ends, so a leader-owned stripe fails
//! loudly rather than hanging.

use std::marker::PhantomData;
use std::pin::Pin;
use std::task::{Context, Poll};

use crate::bufferpool::{BulkRef, Error, PageRef, PageStream, Req};

use super::Backend;

/// Backend that always fails. Constructed with the request type
/// fixed so it can satisfy `Backend<Req = R>` for a chosen `R`.
pub struct NullBackend<R> {
    _marker: PhantomData<fn() -> R>,
}

impl<R> NullBackend<R> {
    pub fn new() -> Self {
        Self {
            _marker: PhantomData,
        }
    }
}

impl<R> Default for NullBackend<R> {
    fn default() -> Self {
        Self::new()
    }
}

impl<R: Req> Backend for NullBackend<R> {
    type Req = R;
    type Stream<'a>
        = NullStream
    where
        Self: 'a;

    fn bulk_get<'a>(
        &'a self,
        _req: &'a Self::Req,
        _src: BulkRef,
        _dsts: &'a [PageRef],
    ) -> Self::Stream<'a> {
        NullStream { emitted: false }
    }
}

/// Stream that yields exactly one `Err` then `None`.
pub struct NullStream {
    emitted: bool,
}

impl PageStream for NullStream {
    fn poll_next(
        mut self: Pin<&mut Self>,
        _cx: &mut Context<'_>,
    ) -> Poll<Option<Result<PageRef, Error>>> {
        if self.emitted {
            Poll::Ready(None)
        } else {
            self.emitted = true;
            Poll::Ready(Some(Err(Error::BadConfig(
                "no backend configured for leader-owned stripe",
            ))))
        }
    }
}

#[cfg(test)]
mod tests {
    use std::task::{RawWaker, RawWakerVTable, Waker};

    use crate::bufferpool::{BulkRef, StripeKey};

    use super::*;

    struct TestReq(StripeKey);

    impl Req for TestReq {
        fn key(&self) -> StripeKey {
            self.0
        }
    }

    fn noop_waker() -> Waker {
        fn raw() -> RawWaker {
            RawWaker::new(std::ptr::null(), &VTABLE)
        }
        static VTABLE: RawWakerVTable = RawWakerVTable::new(|_| raw(), |_| {}, |_| {}, |_| {});
        // SAFETY: VTABLE clone/wake/drop are all no-ops on static data.
        unsafe { Waker::from_raw(raw()) }
    }

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

    #[test]
    fn null_backend_yields_exactly_one_error_then_none() {
        let backend = NullBackend::<TestReq>::new();
        let req = TestReq(StripeKey([1u8; 32]));
        let src = BulkRef {
            stripe: req.key(),
            offset: 0,
            len: 4096,
        };
        let dsts = [PageRef {
            page_idx: 0,
            offset: 0,
            len: 4096,
        }];

        block_on(async {
            let mut stream = std::pin::pin!(backend.bulk_get(&req, src, &dsts));
            let w = noop_waker();
            let mut cx = Context::from_waker(&w);

            match stream.as_mut().poll_next(&mut cx) {
                Poll::Ready(Some(Err(Error::BadConfig(msg)))) => {
                    assert!(msg.contains("no backend configured"), "got: {msg}");
                }
                other => panic!("expected BadConfig error, got {other:?}"),
            }
            assert!(matches!(
                stream.as_mut().poll_next(&mut cx),
                Poll::Ready(None)
            ));
        });
    }
}
