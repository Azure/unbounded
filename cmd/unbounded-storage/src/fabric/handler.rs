// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Server-side handler trait for fabric RPCs.
//!
//! A handler maps a typed request `R` to a stream of `PageRef`s the
//! server should RMA-write into the client's destination MR. The
//! stream surface is intentionally separate from
//! `bufferpool::PageStream` so the server side can carry its own
//! error type without having to interoperate with the pool's
//! `Error` enum.
//!
//! `#[cfg(test)]` blocks at the bottom of this file provide a small
//! collection of canned handlers (`NoopHandler`, `NPagesHandler`,
//! `ErrorHandler`, `CancelObservingHandler`) used by `tests.rs`.

use std::pin::Pin;
use std::task::{Context, Poll};

use crate::bufferpool::{BulkRef, PageRef, Req};

/// Server-side stream produced by a `Handler`. Mirrors the
/// `bufferpool::PageStream` shape but with a handler-defined error
/// type.
pub trait HandlerStream {
    type Error: std::error::Error + Send + Sync + 'static;
    fn poll_next(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<Option<Result<PageRef, Self::Error>>>;
}

/// Handler for one request type `R`. Each invocation returns a fresh
/// stream tied to `&req`; the server drives the stream to completion
/// and forwards each yielded page over RMA to the requesting client.
pub trait Handler<R: Req>: Send + Sync + 'static {
    type Error: std::error::Error + Send + Sync + 'static;
    type Stream<'a>: HandlerStream<Error = Self::Error> + Send + 'a
    where
        Self: 'a,
        R: 'a;
    fn handle<'a>(&'a self, req: &'a R, src: BulkRef) -> Self::Stream<'a>;
}

#[cfg(test)]
pub use test_handlers::*;

#[cfg(test)]
mod test_handlers {
    use std::convert::Infallible;
    use std::marker::PhantomData;
    use std::pin::Pin;
    use std::sync::Arc;
    use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
    use std::task::{Context, Poll};

    use crate::bufferpool::{BulkRef, PageRef, Req};

    use super::{Handler, HandlerStream};

    /// Handler that always yields an empty stream.
    pub struct NoopHandler;

    pub struct NoopStream;

    impl HandlerStream for NoopStream {
        type Error = Infallible;
        fn poll_next(
            self: Pin<&mut Self>,
            _cx: &mut Context<'_>,
        ) -> Poll<Option<Result<PageRef, Infallible>>> {
            Poll::Ready(None)
        }
    }

    impl<R: Req> Handler<R> for NoopHandler {
        type Error = Infallible;
        type Stream<'a>
            = NoopStream
        where
            Self: 'a,
            R: 'a;
        fn handle<'a>(&'a self, _req: &'a R, _src: BulkRef) -> Self::Stream<'a> {
            NoopStream
        }
    }

    /// Handler that yields the configured pages in order, then ends.
    pub struct NPagesHandler {
        pub pages: Vec<PageRef>,
    }

    pub struct NPagesStream<'a> {
        pages: &'a [PageRef],
        next: usize,
    }

    impl<'a> HandlerStream for NPagesStream<'a> {
        type Error = Infallible;
        fn poll_next(
            mut self: Pin<&mut Self>,
            _cx: &mut Context<'_>,
        ) -> Poll<Option<Result<PageRef, Infallible>>> {
            if self.next < self.pages.len() {
                let p = self.pages[self.next];
                self.next += 1;
                Poll::Ready(Some(Ok(p)))
            } else {
                Poll::Ready(None)
            }
        }
    }

    impl<R: Req> Handler<R> for NPagesHandler {
        type Error = Infallible;
        type Stream<'a>
            = NPagesStream<'a>
        where
            Self: 'a,
            R: 'a;
        fn handle<'a>(&'a self, _req: &'a R, _src: BulkRef) -> Self::Stream<'a> {
            NPagesStream {
                pages: &self.pages,
                next: 0,
            }
        }
    }

    /// Test-only error that always materializes via `Default`.
    #[derive(Debug, Default)]
    pub struct TestErr(pub &'static str);

    impl std::fmt::Display for TestErr {
        fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
            write!(f, "test error: {}", self.0)
        }
    }
    impl std::error::Error for TestErr {}

    /// Handler that yields `Some(Err(E::default()))` then ends.
    pub struct ErrorHandler<E: Default + std::error::Error + Send + Sync + 'static> {
        _e: PhantomData<fn() -> E>,
    }

    impl<E: Default + std::error::Error + Send + Sync + 'static> Default for ErrorHandler<E> {
        fn default() -> Self {
            Self { _e: PhantomData }
        }
    }

    pub struct ErrorStream<E: Default + std::error::Error + Send + Sync + 'static> {
        emitted: bool,
        _e: PhantomData<fn() -> E>,
    }

    impl<E: Default + std::error::Error + Send + Sync + 'static> HandlerStream for ErrorStream<E> {
        type Error = E;
        fn poll_next(
            mut self: Pin<&mut Self>,
            _cx: &mut Context<'_>,
        ) -> Poll<Option<Result<PageRef, E>>> {
            if self.emitted {
                Poll::Ready(None)
            } else {
                self.emitted = true;
                Poll::Ready(Some(Err(E::default())))
            }
        }
    }

    impl<R: Req, E: Default + std::error::Error + Send + Sync + 'static> Handler<R>
        for ErrorHandler<E>
    {
        type Error = E;
        type Stream<'a>
            = ErrorStream<E>
        where
            Self: 'a,
            R: 'a;
        fn handle<'a>(&'a self, _req: &'a R, _src: BulkRef) -> Self::Stream<'a> {
            ErrorStream {
                emitted: false,
                _e: PhantomData,
            }
        }
    }

    /// Handler whose stream yields one page (if `page` is `Some`)
    /// then stays `Pending` forever, and sets a flag on Drop. Used
    /// to assert that the server drops in-flight handler streams on
    /// client-side cancellation.
    pub struct CancelObservingHandler {
        pub dropped: Arc<AtomicBool>,
        pub page: Option<PageRef>,
        /// Counter to ensure handle() returns a fresh per-invocation
        /// stream that owns its own `Drop` observer.
        pub instances: AtomicUsize,
    }

    pub struct CancelObservingStream {
        dropped: Arc<AtomicBool>,
        page: Option<PageRef>,
        yielded_first: bool,
    }

    impl Drop for CancelObservingStream {
        fn drop(&mut self) {
            self.dropped.store(true, Ordering::Release);
        }
    }

    impl HandlerStream for CancelObservingStream {
        type Error = TestErr;
        fn poll_next(
            mut self: Pin<&mut Self>,
            _cx: &mut Context<'_>,
        ) -> Poll<Option<Result<PageRef, TestErr>>> {
            if !self.yielded_first {
                if let Some(p) = self.page {
                    self.yielded_first = true;
                    return Poll::Ready(Some(Ok(p)));
                }
            }
            Poll::Pending
        }
    }

    impl<R: Req> Handler<R> for CancelObservingHandler {
        type Error = TestErr;
        type Stream<'a>
            = CancelObservingStream
        where
            Self: 'a,
            R: 'a;
        fn handle<'a>(&'a self, _req: &'a R, _src: BulkRef) -> Self::Stream<'a> {
            self.instances.fetch_add(1, Ordering::Relaxed);
            CancelObservingStream {
                dropped: self.dropped.clone(),
                page: self.page,
                yielded_first: false,
            }
        }
    }
}
