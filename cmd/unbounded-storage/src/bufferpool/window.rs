// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Single-stripe facade over the ordered prefetch pipeline.
//!
//! [`WindowedRead`] preserves eager stream admission and retains that
//! admission until reader drop, while delegating page scheduling,
//! speculation, backoff, and cancellation to
//! [`crate::bufferpool::PipelinedRead`].

use std::rc::Rc;

use crate::bufferpool::pipeline::PipelinedRead;
use crate::bufferpool::stream::{PageGuard, StreamSrc};
use crate::bufferpool::types::Error;

/// Windowed consumer surface over one eagerly admitted stripe.
pub struct WindowedRead<'pool> {
    inner: PipelinedRead<'pool>,
}

impl std::fmt::Debug for WindowedRead<'_> {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("WindowedRead")
            .field("inner", &self.inner)
            .finish()
    }
}

impl<'pool> WindowedRead<'pool> {
    pub(super) fn new(
        src: Rc<dyn StreamSrc + 'pool>,
        offset: u64,
        end: u64,
        page_size: usize,
        window: usize,
        max_inflight_pages: usize,
    ) -> Self {
        Self {
            inner: PipelinedRead::single_stream(
                src,
                offset,
                end,
                page_size as u64,
                window,
                max_inflight_pages,
            ),
        }
    }

    /// Next page in cursor order. Returns `None` at EOF. The
    /// returned [`PageGuard`] borrows `&mut self`, enforcing the
    /// one-page-at-a-time contract.
    pub async fn next_page<'s>(&'s mut self) -> Option<Result<PageGuard<'s>, Error>> {
        self.inner.next_page().await
    }
}
