// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Shared mechanics for HTTP-family origin backends.

use std::future::Future;
use std::pin::Pin;
use std::task::{Context, Poll};

use crate::bufferpool::{Error, PageRef, PageStream};

#[derive(Clone, Copy)]
pub(super) struct PageErrorMessages {
    over_read: &'static str,
    offset_overflow: &'static str,
}

impl PageErrorMessages {
    pub(super) const fn new(over_read: &'static str, offset_overflow: &'static str) -> Self {
        Self {
            over_read,
            offset_overflow,
        }
    }
}

pub(super) const HTTP_PAGE_ERRORS: PageErrorMessages = PageErrorMessages::new(
    "http backend: over read from origin",
    "http backend: page byte offset overflow",
);
pub(super) const S3_PAGE_ERRORS: PageErrorMessages = PageErrorMessages::new(
    "s3 backend: over read from origin",
    "s3 backend: page byte offset overflow",
);
pub(super) const AZURE_PAGE_ERRORS: PageErrorMessages = PageErrorMessages::new(
    "azure backend: over read from origin",
    "azure backend: page byte offset overflow",
);

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

/// Copy `body` into the destination pages in order and zero-fill any
/// destination bytes the body does not cover.
pub(super) fn copy_body_into_pages(
    body: &[u8],
    dsts: &[PageRef],
    backing_base: *mut u8,
    page_size: usize,
    errors: PageErrorMessages,
) -> Result<(), Error> {
    let capacity = pages_capacity(dsts);
    if body.len() > capacity {
        return Err(Error::from(errors.over_read));
    }
    let mut consumed = 0usize;
    for page in dsts {
        let n = page.len as usize;
        let avail = body.len().saturating_sub(consumed).min(n);
        let page_offset = page_byte_offset(page, page_size, errors)?;
        // SAFETY: the destination addresses a page inside the registered
        // backing the embedder keeps alive for the shard's lifetime. The
        // caller guarantees the page geometry and shard-local access.
        unsafe {
            let dst = backing_base.add(page_offset);
            if avail > 0 {
                std::ptr::copy_nonoverlapping(body.as_ptr().add(consumed), dst, avail);
            }
            if avail < n {
                std::ptr::write_bytes(dst.add(avail), 0, n - avail);
            }
        }
        consumed += avail;
    }
    Ok(())
}

/// Total destination byte capacity across `dsts`.
pub(super) fn pages_capacity(dsts: &[PageRef]) -> usize {
    dsts.iter().map(|p| p.len as usize).sum()
}

/// Registered byte offset of a page within the fixed backing.
pub(super) fn page_byte_offset(
    page: &PageRef,
    page_size: usize,
    errors: PageErrorMessages,
) -> Result<usize, Error> {
    (page.page_idx as usize)
        .checked_mul(page_size)
        .and_then(|base| base.checked_add(page.offset as usize))
        .ok_or_else(|| Error::from(errors.offset_overflow))
}

/// Locate the destination page covering logical body offset `at`.
pub(super) fn locate_in_pages(
    dsts: &[PageRef],
    at: usize,
    page_size: usize,
    errors: PageErrorMessages,
) -> Result<Option<(usize, usize)>, Error> {
    let mut page_start = 0usize;
    for page in dsts {
        let n = page.len as usize;
        if at < page_start + n {
            let within = at - page_start;
            let off = page_byte_offset(page, page_size, errors)?
                .checked_add(within)
                .ok_or_else(|| Error::from(errors.offset_overflow))?;
            return Ok(Some((off, n - within)));
        }
        page_start += n;
    }
    Ok(None)
}

/// Copy `src` into the destination pages starting at logical offset `start`.
pub(super) fn write_slice_into_pages(
    src: &[u8],
    dsts: &[PageRef],
    start: usize,
    backing_base: *mut u8,
    page_size: usize,
    errors: PageErrorMessages,
) -> Result<(), Error> {
    let end = start
        .checked_add(src.len())
        .ok_or_else(|| Error::from(errors.offset_overflow))?;
    if end > pages_capacity(dsts) {
        return Err(Error::from(errors.over_read));
    }
    let mut written = 0usize;
    while written < src.len() {
        let (off, room) = locate_in_pages(dsts, start + written, page_size, errors)?
            .ok_or_else(|| Error::from(errors.over_read))?;
        let n = room.min(src.len() - written);
        // SAFETY: `off` and `n` describe bytes within one destination page;
        // the caller guarantees the backing remains valid and shard-local.
        unsafe {
            std::ptr::copy_nonoverlapping(src.as_ptr().add(written), backing_base.add(off), n);
        }
        written += n;
    }
    Ok(())
}

/// Zero-fill destination bytes from logical offset `from` to capacity.
pub(super) fn zero_fill_pages_from(
    dsts: &[PageRef],
    from: usize,
    backing_base: *mut u8,
    page_size: usize,
    errors: PageErrorMessages,
) -> Result<(), Error> {
    let mut page_start = 0usize;
    for page in dsts {
        let n = page.len as usize;
        let page_end = page_start + n;
        if from < page_end {
            let within = from.saturating_sub(page_start);
            let off = page_byte_offset(page, page_size, errors)?
                .checked_add(within)
                .ok_or_else(|| Error::from(errors.offset_overflow))?;
            // SAFETY: `off` addresses the current destination page and
            // `n - within` cannot escape that page.
            unsafe {
                std::ptr::write_bytes(backing_base.add(off), 0, n - within);
            }
        }
        page_start = page_end;
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::runtime::noop_waker;

    const ERRORS: PageErrorMessages = PageErrorMessages::new(
        "test backend: over read from origin",
        "test backend: page byte offset overflow",
    );

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

    #[test]
    fn copy_body_into_pages_fills_and_zeroes_pages() {
        let page_size = 4096usize;
        let mut backing = vec![0xAAu8; page_size * 3];
        let dsts = [
            PageRef {
                page_idx: 1,
                offset: 0,
                len: 4,
            },
            PageRef {
                page_idx: 2,
                offset: 8,
                len: 3,
            },
        ];

        copy_body_into_pages(
            &[1, 2, 3, 4, 5],
            &dsts,
            backing.as_mut_ptr(),
            page_size,
            ERRORS,
        )
        .unwrap();

        assert_eq!(&backing[page_size..page_size + 4], &[1, 2, 3, 4]);
        assert_eq!(&backing[2 * page_size + 8..2 * page_size + 11], &[5, 0, 0]);
    }

    #[test]
    fn copy_body_into_pages_rejects_over_read_with_supplied_message() {
        let mut backing = [0u8; 4];
        let dsts = [PageRef {
            page_idx: 0,
            offset: 0,
            len: 4,
        }];

        let err =
            copy_body_into_pages(&[0; 5], &dsts, backing.as_mut_ptr(), 4, ERRORS).unwrap_err();

        assert_eq!(format_error(err), "test backend: over read from origin");
    }

    #[test]
    fn page_geometry_helpers_locate_capacity_and_report_overflow() {
        let dsts = [
            PageRef {
                page_idx: 1,
                offset: 0,
                len: 4,
            },
            PageRef {
                page_idx: 2,
                offset: 8,
                len: 3,
            },
        ];

        assert_eq!(pages_capacity(&dsts), 7);
        assert_eq!(pages_capacity(&[]), 0);
        assert_eq!(
            locate_in_pages(&dsts, 3, 4096, ERRORS).unwrap(),
            Some((4099, 1))
        );
        assert_eq!(
            locate_in_pages(&dsts, 4, 4096, ERRORS).unwrap(),
            Some((8200, 3))
        );
        assert_eq!(locate_in_pages(&dsts, 7, 4096, ERRORS).unwrap(), None);

        let overflow = PageRef {
            page_idx: u32::MAX,
            offset: u32::MAX,
            len: 1,
        };
        let err = page_byte_offset(&overflow, usize::MAX, ERRORS).unwrap_err();
        assert_eq!(format_error(err), "test backend: page byte offset overflow");
    }

    #[test]
    fn write_slice_into_pages_assembles_across_page_boundary() {
        let page_size = 4096usize;
        let mut backing = vec![0u8; page_size * 3];
        let dsts = [
            PageRef {
                page_idx: 1,
                offset: 0,
                len: 4,
            },
            PageRef {
                page_idx: 2,
                offset: 8,
                len: 3,
            },
        ];

        write_slice_into_pages(
            &[10, 20, 30],
            &dsts,
            0,
            backing.as_mut_ptr(),
            page_size,
            ERRORS,
        )
        .unwrap();
        write_slice_into_pages(
            &[40, 50, 60, 70],
            &dsts,
            3,
            backing.as_mut_ptr(),
            page_size,
            ERRORS,
        )
        .unwrap();

        assert_eq!(&backing[page_size..page_size + 4], &[10, 20, 30, 40]);
        assert_eq!(
            &backing[2 * page_size + 8..2 * page_size + 11],
            &[50, 60, 70]
        );
    }

    #[test]
    fn write_slice_into_pages_rejects_overflow() {
        let mut backing = [0u8; 4];
        let dsts = [PageRef {
            page_idx: 0,
            offset: 0,
            len: 4,
        }];

        let err =
            write_slice_into_pages(&[0; 3], &dsts, 2, backing.as_mut_ptr(), 4, ERRORS).unwrap_err();

        assert_eq!(format_error(err), "test backend: over read from origin");
    }

    #[test]
    fn zero_fill_pages_from_spans_remaining_pages() {
        let page_size = 4096usize;
        let mut backing = vec![0xAAu8; page_size * 3];
        let dsts = [
            PageRef {
                page_idx: 1,
                offset: 0,
                len: 4,
            },
            PageRef {
                page_idx: 2,
                offset: 0,
                len: 4,
            },
        ];

        zero_fill_pages_from(&dsts, 2, backing.as_mut_ptr(), page_size, ERRORS).unwrap();

        assert_eq!(&backing[page_size..page_size + 2], &[0xAA, 0xAA]);
        assert_eq!(&backing[page_size + 2..page_size + 4], &[0, 0]);
        assert_eq!(&backing[2 * page_size..2 * page_size + 4], &[0, 0, 0, 0]);
    }

    #[test]
    fn streaming_write_and_zero_fill_match_copy() {
        let page_size = 4096usize;
        let dsts = [
            PageRef {
                page_idx: 1,
                offset: 0,
                len: 4,
            },
            PageRef {
                page_idx: 2,
                offset: 0,
                len: 4,
            },
        ];
        let mut via_copy = vec![0xAAu8; page_size * 3];
        let mut via_stream = via_copy.clone();

        copy_body_into_pages(&[9, 8], &dsts, via_copy.as_mut_ptr(), page_size, ERRORS).unwrap();
        write_slice_into_pages(
            &[9, 8],
            &dsts,
            0,
            via_stream.as_mut_ptr(),
            page_size,
            ERRORS,
        )
        .unwrap();
        zero_fill_pages_from(&dsts, 2, via_stream.as_mut_ptr(), page_size, ERRORS).unwrap();

        assert_eq!(via_copy, via_stream);
    }

    fn format_error(error: Error) -> String {
        match error {
            Error::Transport(inner) => inner.to_string(),
            other => panic!("expected transport error, got {other:?}"),
        }
    }
}
