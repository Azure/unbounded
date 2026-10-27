//! Kernel-facing ublk queue helpers: the descriptor mapping, request
//! decoding, page span math, and queue command payloads. No reactor use.

use std::io;
use std::os::fd::{AsRawFd, BorrowedFd};
use std::ptr::NonNull;

use super::uapi::{
    IO_CMD_SIZE, IO_DESC_SIZE, IoCmd, IoDesc, UBLK_IO_OP_DISCARD, UBLK_IO_OP_FLUSH,
    UBLK_IO_OP_READ, UBLK_IO_OP_WRITE, UBLK_IO_OP_WRITE_SAME, UBLK_IO_OP_WRITE_ZEROES,
    descriptor_len, descriptor_offset,
};

/// Read-only mapping of one queue's descriptor array from `/dev/ublkcN`.
pub(crate) struct IoDescriptors {
    ptr: NonNull<IoDesc>,
    len: usize,
    depth: u16,
}

// SAFETY: the mapping is plain shared memory owned by this value; moving it to
// another thread does not invalidate it.
unsafe impl Send for IoDescriptors {}

impl IoDescriptors {
    /// Maps queue `q`'s descriptors for `depth` tags from the char device.
    pub(crate) fn map(char_fd: BorrowedFd<'_>, q: u16, depth: u16) -> io::Result<Self> {
        if depth == 0 {
            return Err(io::ErrorKind::InvalidInput.into());
        }
        let len = descriptor_len(depth);
        // SAFETY: a fresh shared read-only mapping of the char device with no
        // address hint; the result is checked before use.
        let ptr = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                len,
                libc::PROT_READ,
                libc::MAP_SHARED | libc::MAP_POPULATE,
                char_fd.as_raw_fd(),
                descriptor_offset(q),
            )
        };
        if ptr == libc::MAP_FAILED {
            return Err(io::Error::last_os_error());
        }
        let ptr = NonNull::new(ptr.cast()).ok_or_else(|| io::Error::other("mmap returned null"))?;
        Ok(Self { ptr, len, depth })
    }

    pub(crate) fn depth(&self) -> u16 {
        self.depth
    }

    /// Reads tag `tag`'s descriptor. Call only after the tag's fetch completed
    /// and before it is committed; the kernel rewrites it per request.
    pub(crate) fn get(&self, tag: u16) -> IoDesc {
        assert!(tag < self.depth, "ublk tag out of range");
        // SAFETY: tag < depth and the mapping covers depth * IO_DESC_SIZE bytes;
        // the array is 8-byte aligned (page aligned base, 24-byte stride). The
        // kernel writes it concurrently in general, so read it volatile.
        unsafe { std::ptr::read_volatile(self.ptr.as_ptr().add(usize::from(tag))) }
    }
}

impl Drop for IoDescriptors {
    fn drop(&mut self) {
        // SAFETY: the mapping came from mmap with self.len and is unmapped once.
        unsafe { libc::munmap(self.ptr.as_ptr().cast(), self.len) };
    }
}

const _: () = assert!(IO_DESC_SIZE == std::mem::size_of::<IoDesc>());

/// What to do with one fetched request.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum Action {
    /// Serve `len` bytes at device byte `offset`, then commit `len`.
    Read { offset: u64, len: u32 },
    /// Commit this result now (0 or a negative errno).
    Complete(i32),
}

/// Request decoding.
pub(crate) struct Request;

impl Request {
    /// Maps a descriptor to an action. FLUSH succeeds; writes fail with EROFS;
    /// unknown ops fail with EOPNOTSUPP; empty or oversized reads fail with EIO.
    pub(crate) fn decode(desc: IoDesc, max_io_bytes: u32) -> Action {
        match desc.op() {
            UBLK_IO_OP_READ => {
                let len = u64::from(desc.nr_sectors) << 9;
                if len == 0 || len > u64::from(max_io_bytes) {
                    return Action::Complete(-libc::EIO);
                }
                match desc.start_sector.checked_mul(512) {
                    Some(offset) => Action::Read {
                        offset,
                        len: len as u32,
                    },
                    None => Action::Complete(-libc::EIO),
                }
            }
            UBLK_IO_OP_FLUSH => Action::Complete(0),
            UBLK_IO_OP_WRITE
            | UBLK_IO_OP_WRITE_ZEROES
            | UBLK_IO_OP_DISCARD
            | UBLK_IO_OP_WRITE_SAME => Action::Complete(-libc::EROFS),
            _ => Action::Complete(-libc::EOPNOTSUPP),
        }
    }
}

/// One piece of a read, in request order.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum Span {
    /// `len` bytes from cache page `number` at `page_offset`, placed at
    /// `request_offset` in the request.
    Page {
        number: u64,
        page_offset: u64,
        len: u32,
        request_offset: u32,
    },
    /// `len` zero bytes at `request_offset`, past the object end.
    Zero { request_offset: u32, len: u32 },
}

/// Splits the read `[offset, offset + len)` into cache page spans and a
/// trailing zero span for bytes at or past `object_len`.
pub(crate) fn spans(offset: u64, len: u32, object_len: u64, page_bytes: u64) -> Vec<Span> {
    assert!(page_bytes > 0, "page size must be positive");
    let end = offset.saturating_add(u64::from(len));
    let data_end = end.min(object_len);
    let mut out = Vec::new();
    let mut at = offset;
    while at < data_end {
        let number = at / page_bytes;
        let page_offset = at % page_bytes;
        let take = (page_bytes - page_offset).min(data_end - at);
        out.push(Span::Page {
            number,
            page_offset,
            len: take as u32,
            request_offset: (at - offset) as u32,
        });
        at += take;
    }
    if at < end {
        out.push(Span::Zero {
            request_offset: (at - offset) as u32,
            len: (end - at) as u32,
        });
    }
    out
}

/// Payload of a FETCH_REQ or COMMIT_AND_FETCH_REQ. With `UBLK_F_USER_COPY`
/// the buffer address must be 0. `result` is ignored by FETCH_REQ.
pub(crate) fn io_cmd_bytes(q: u16, tag: u16, result: i32) -> [u8; IO_CMD_SIZE] {
    IoCmd {
        q_id: q,
        tag,
        result,
        addr: 0,
    }
    .to_bytes()
}

#[cfg(test)]
mod tests {
    use super::*;

    const MIB: u64 = 1 << 20;

    fn desc(op: u8, start_sector: u64, nr_sectors: u32) -> IoDesc {
        IoDesc {
            op_flags: u32::from(op) | 0x1200,
            nr_sectors,
            start_sector,
            addr: 0,
        }
    }

    #[test]
    fn decode_ops() {
        assert_eq!(
            Request::decode(desc(UBLK_IO_OP_READ, 8, 16), 1 << 20),
            Action::Read {
                offset: 4096,
                len: 8192
            }
        );
        assert_eq!(
            Request::decode(desc(UBLK_IO_OP_FLUSH, 0, 0), 4096),
            Action::Complete(0)
        );
        for op in [
            UBLK_IO_OP_WRITE,
            UBLK_IO_OP_WRITE_ZEROES,
            UBLK_IO_OP_DISCARD,
            UBLK_IO_OP_WRITE_SAME,
        ] {
            assert_eq!(
                Request::decode(desc(op, 0, 8), 4096),
                Action::Complete(-libc::EROFS)
            );
        }
        for op in [6, 10, 13, 18, 255] {
            assert_eq!(
                Request::decode(desc(op, 0, 8), 4096),
                Action::Complete(-libc::EOPNOTSUPP)
            );
        }
    }

    #[test]
    fn decode_rejects_bad_reads() {
        assert_eq!(
            Request::decode(desc(UBLK_IO_OP_READ, 0, 0), 4096),
            Action::Complete(-libc::EIO)
        );
        assert_eq!(
            Request::decode(desc(UBLK_IO_OP_READ, 0, 9), 4096),
            Action::Complete(-libc::EIO)
        );
        assert_eq!(
            Request::decode(desc(UBLK_IO_OP_READ, 0, 8), 4096),
            Action::Read {
                offset: 0,
                len: 4096
            }
        );
        assert_eq!(
            Request::decode(desc(UBLK_IO_OP_READ, u64::MAX, 1), 4096),
            Action::Complete(-libc::EIO)
        );
    }

    #[test]
    fn spans_within_one_page() {
        assert_eq!(
            spans(4096, 8192, 64 * MIB, 16 * MIB),
            vec![Span::Page {
                number: 0,
                page_offset: 4096,
                len: 8192,
                request_offset: 0
            }]
        );
    }

    #[test]
    fn spans_ending_on_page_boundary() {
        assert_eq!(
            spans(16 * MIB - 4096, 4096, 64 * MIB, 16 * MIB),
            vec![Span::Page {
                number: 0,
                page_offset: 16 * MIB - 4096,
                len: 4096,
                request_offset: 0
            }]
        );
        assert_eq!(
            spans(16 * MIB, 4096, 64 * MIB, 16 * MIB),
            vec![Span::Page {
                number: 1,
                page_offset: 0,
                len: 4096,
                request_offset: 0
            }]
        );
    }

    #[test]
    fn spans_crossing_pages() {
        assert_eq!(
            spans(16 * MIB - 512, 1024, 64 * MIB, 16 * MIB),
            vec![
                Span::Page {
                    number: 0,
                    page_offset: 16 * MIB - 512,
                    len: 512,
                    request_offset: 0
                },
                Span::Page {
                    number: 1,
                    page_offset: 0,
                    len: 512,
                    request_offset: 512
                },
            ]
        );
        // Small pages: one request covers several whole pages.
        assert_eq!(
            spans(1000, 10_000, 1 << 20, 4096),
            vec![
                Span::Page {
                    number: 0,
                    page_offset: 1000,
                    len: 3096,
                    request_offset: 0
                },
                Span::Page {
                    number: 1,
                    page_offset: 0,
                    len: 4096,
                    request_offset: 3096
                },
                Span::Page {
                    number: 2,
                    page_offset: 0,
                    len: 2808,
                    request_offset: 7192
                },
            ]
        );
    }

    #[test]
    fn spans_tail_past_object_end() {
        // Object length not a multiple of 512; device size rounds up.
        let object_len = 40 * MIB + 123;
        assert_eq!(
            spans(40 * MIB, 512, object_len, 16 * MIB),
            vec![
                Span::Page {
                    number: 2,
                    page_offset: 8 * MIB,
                    len: 123,
                    request_offset: 0
                },
                Span::Zero {
                    request_offset: 123,
                    len: 389
                },
            ]
        );
        // Ends exactly at the object end: no zero span.
        assert_eq!(
            spans(object_len - 23, 23, object_len, 16 * MIB),
            vec![Span::Page {
                number: 2,
                page_offset: 8 * MIB + 100,
                len: 23,
                request_offset: 0
            }]
        );
        // Entirely past the end.
        assert_eq!(
            spans(object_len + 10, 100, object_len, 16 * MIB),
            vec![Span::Zero {
                request_offset: 0,
                len: 100
            }]
        );
        // Object ends exactly on a page boundary.
        assert_eq!(
            spans(16 * MIB - 100, 200, 16 * MIB, 16 * MIB),
            vec![
                Span::Page {
                    number: 0,
                    page_offset: 16 * MIB - 100,
                    len: 100,
                    request_offset: 0
                },
                Span::Zero {
                    request_offset: 100,
                    len: 100
                },
            ]
        );
    }

    #[test]
    fn spans_cover_request_exactly() {
        for (offset, len) in [
            (0_u64, 1_u32),
            (12_345, 1 << 20),
            (16 * MIB - 1, 2),
            (40 * MIB - 7, 4096),
        ] {
            let mut next = 0_u32;
            for span in spans(offset, len, 40 * MIB + 123, 16 * MIB) {
                let (at, n) = match span {
                    Span::Page {
                        request_offset,
                        len,
                        ..
                    }
                    | Span::Zero {
                        request_offset,
                        len,
                    } => (request_offset, len),
                };
                assert_eq!(at, next);
                assert!(n > 0);
                next += n;
            }
            assert_eq!(next, len);
        }
        assert!(spans(5, 0, 100, 16).is_empty());
    }

    #[test]
    fn io_cmd_layout() {
        let bytes = io_cmd_bytes(3, 7, -libc::EROFS);
        assert_eq!(&bytes[0..2], &3_u16.to_ne_bytes());
        assert_eq!(&bytes[2..4], &7_u16.to_ne_bytes());
        assert_eq!(&bytes[4..8], &(-libc::EROFS).to_ne_bytes());
        assert_eq!(&bytes[8..16], &[0; 8]);
    }
}
