// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Shared mechanics for HTTP-family origin backends.

use std::future::Future;
use std::pin::Pin;
use std::task::{Context, Poll};

use crate::bufferpool::{Error, PageRef, PageStream};

/// Drives one origin fetch to completion, then yields its destination pages in
/// order. A failed fetch yields exactly one error before terminating.
pub struct FetchStream<'a> {
    state: FetchState<'a>,
    delivered: Vec<PageRef>,
    next: usize,
}

enum FetchState<'a> {
    Running(Pin<Box<dyn Future<Output = Result<(), Error>> + 'a>>),
    Delivering,
    Failed(Option<Error>),
    Done,
}

impl<'a> FetchStream<'a> {
    pub(super) fn pending(
        fut: Pin<Box<dyn Future<Output = Result<(), Error>> + 'a>>,
        delivered: Vec<PageRef>,
    ) -> Self {
        Self {
            state: FetchState::Running(fut),
            delivered,
            next: 0,
        }
    }

    pub(super) fn immediate_error(msg: &'static str) -> Self {
        Self {
            state: FetchState::Failed(Some(Error::from(msg))),
            delivered: Vec::new(),
            next: 0,
        }
    }
}

impl PageStream for FetchStream<'_> {
    fn poll_next(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<Option<Result<PageRef, Error>>> {
        let this = self.get_mut();
        loop {
            match &mut this.state {
                FetchState::Running(fut) => match fut.as_mut().poll(cx) {
                    Poll::Pending => return Poll::Pending,
                    Poll::Ready(Ok(())) => this.state = FetchState::Delivering,
                    Poll::Ready(Err(e)) => this.state = FetchState::Failed(Some(e)),
                },
                FetchState::Delivering => {
                    if this.next >= this.delivered.len() {
                        this.state = FetchState::Done;
                        return Poll::Ready(None);
                    }
                    let page = this.delivered[this.next];
                    this.next += 1;
                    return Poll::Ready(Some(Ok(page)));
                }
                FetchState::Failed(slot) => {
                    let error = slot.take();
                    this.state = FetchState::Done;
                    return Poll::Ready(error.map(Err));
                }
                FetchState::Done => return Poll::Ready(None),
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::runtime::noop_waker;

    fn page(page_idx: u32) -> PageRef {
        PageRef {
            page_idx,
            offset: 0,
            len: 4096,
        }
    }

    fn poll(stream: &mut FetchStream<'_>) -> Poll<Option<Result<PageRef, Error>>> {
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        Pin::new(stream).poll_next(&mut cx)
    }

    fn assert_page(stream: &mut FetchStream<'_>, expected: PageRef) {
        match poll(stream) {
            Poll::Ready(Some(Ok(page))) => assert_eq!(page, expected),
            other => panic!("expected page {expected:?}, got {other:?}"),
        }
    }

    fn assert_done(stream: &mut FetchStream<'_>) {
        assert!(matches!(poll(stream), Poll::Ready(None)));
    }

    #[test]
    fn successful_fetch_yields_pages_in_order_then_stays_done() {
        let pages = vec![page(3), page(7)];
        let mut stream = FetchStream::pending(Box::pin(async { Ok(()) }), pages.clone());

        assert_page(&mut stream, pages[0]);
        assert_page(&mut stream, pages[1]);
        assert_done(&mut stream);
        assert_done(&mut stream);
    }

    #[test]
    fn failed_fetch_yields_one_error_then_stays_done() {
        let mut stream = FetchStream::pending(
            Box::pin(async { Err(Error::from("fetch failed")) }),
            vec![page(1)],
        );

        assert!(matches!(poll(&mut stream), Poll::Ready(Some(Err(_)))));
        assert_done(&mut stream);
        assert_done(&mut stream);
    }

    #[test]
    fn successful_empty_delivery_ends_cleanly() {
        let mut stream = FetchStream::pending(Box::pin(async { Ok(()) }), Vec::new());

        assert_done(&mut stream);
        assert_done(&mut stream);
    }
}
