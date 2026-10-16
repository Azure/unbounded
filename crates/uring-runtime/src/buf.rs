//! Buffers whose memory can be lent to the kernel.
//!
//! io_uring reads and writes buffers asynchronously, after the submitting call
//! has returned. The runtime therefore takes buffers by value, keeps them in
//! its in-flight table until the kernel is provably done, and hands them back
//! in a [`BufResult`]. [`IoBuf`] and [`IoBufMut`] describe buffer types whose
//! memory address survives that ownership transfer.
//!
//! ```
//! use uring_runtime::{IoBuf, IoBufMut};
//!
//! let mut buf: Vec<u8> = Vec::with_capacity(16);
//! assert_eq!(buf.bytes_init(), 0);
//! assert_eq!(buf.bytes_total(), 16);
//! let ptr = buf.stable_mut_ptr();
//! // Moving the Vec does not move its heap allocation.
//! let moved = buf;
//! assert_eq!(moved.stable_ptr(), ptr.cast_const());
//! ```
//!
//! `Box<[u8]>` is deliberately not supported: under Rust's aliasing model a
//! `Box` asserts unique ownership of its allocation every time it is moved,
//! which invalidates raw pointers previously derived from it. `Vec<u8>` does
//! not have this property.

use crate::Error;

/// A buffer the kernel may read from while the runtime owns it.
///
/// # Safety
///
/// Implementors guarantee that:
///
/// - [`stable_ptr`](IoBuf::stable_ptr) points to at least
///   [`bytes_init`](IoBuf::bytes_init) initialized bytes;
/// - the pointer and the bytes it points to remain valid and unchanged, and
///   the pointer keeps its provenance, while the value is moved (including
///   into and out of the runtime's in-flight table) and until it is dropped or
///   accessed mutably by its owner;
/// - both methods return the same values when called repeatedly without an
///   intervening mutable access.
pub unsafe trait IoBuf: 'static {
    /// Pointer to the first byte.
    fn stable_ptr(&self) -> *const u8;

    /// Number of initialized bytes starting at [`stable_ptr`](IoBuf::stable_ptr).
    fn bytes_init(&self) -> usize;
}

/// A buffer the kernel may write into while the runtime owns it.
///
/// Reads write into `[0, bytes_total)` and then call
/// [`set_init`](IoBufMut::set_init) with the number of bytes written.
///
/// # Safety
///
/// In addition to the [`IoBuf`] contract, implementors guarantee that
/// [`stable_mut_ptr`](IoBufMut::stable_mut_ptr) points to
/// [`bytes_total`](IoBufMut::bytes_total) writable bytes that are not aliased
/// by any other live reference or pointer, and that this remains true while
/// the value is moved and owned by the runtime.
pub unsafe trait IoBufMut: IoBuf {
    /// Writable pointer to the first byte. Equal to
    /// [`stable_ptr`](IoBuf::stable_ptr) as an address.
    fn stable_mut_ptr(&mut self) -> *mut u8;

    /// Total writable capacity in bytes.
    fn bytes_total(&mut self) -> usize;

    /// Mark the first `len` bytes as initialized.
    ///
    /// # Safety
    ///
    /// `len` must not exceed [`bytes_total`](IoBufMut::bytes_total), and the
    /// first `len` bytes must have been initialized.
    unsafe fn set_init(&mut self, len: usize);
}

// SAFETY: a Vec's heap allocation does not move when the Vec is moved, and
// moving a Vec does not retag or invalidate pointers into its allocation.
unsafe impl IoBuf for Vec<u8> {
    fn stable_ptr(&self) -> *const u8 {
        self.as_ptr()
    }

    fn bytes_init(&self) -> usize {
        self.len()
    }
}

// SAFETY: as for `IoBuf`; `&mut self` access guarantees no other aliases.
unsafe impl IoBufMut for Vec<u8> {
    fn stable_mut_ptr(&mut self) -> *mut u8 {
        self.as_mut_ptr()
    }

    fn bytes_total(&mut self) -> usize {
        self.capacity()
    }

    unsafe fn set_init(&mut self, len: usize) {
        if len > self.len() {
            // SAFETY: the caller guarantees `len <= capacity` and that the
            // bytes up to `len` are initialized.
            unsafe { self.set_len(len) };
        }
    }
}

// SAFETY: static data never moves and is immutable.
unsafe impl IoBuf for &'static [u8] {
    fn stable_ptr(&self) -> *const u8 {
        self.as_ptr()
    }

    fn bytes_init(&self) -> usize {
        self.len()
    }
}

// SAFETY: static data never moves and is immutable.
unsafe impl IoBuf for &'static str {
    fn stable_ptr(&self) -> *const u8 {
        self.as_ptr()
    }

    fn bytes_init(&self) -> usize {
        self.len()
    }
}

/// The outcome of an operation that borrowed a buffer, together with the
/// buffer itself.
///
/// The buffer is returned on success and on failure, so the caller never
/// loses ownership of memory it lent to the kernel.
#[derive(Debug)]
#[must_use]
pub struct BufResult<T, B, E = Error> {
    /// The operation's result.
    pub result: Result<T, E>,
    /// The buffer that was passed in.
    pub buf: B,
}

impl<T, B, E> BufResult<T, B, E> {
    /// Create a result.
    pub fn new(result: Result<T, E>, buf: B) -> Self {
        Self { result, buf }
    }

    /// Split into the result and the buffer.
    pub fn into_parts(self) -> (Result<T, E>, B) {
        (self.result, self.buf)
    }

    /// Transform the success value, keeping the buffer.
    pub fn map<U>(self, f: impl FnOnce(T) -> U) -> BufResult<U, B, E> {
        BufResult {
            result: self.result.map(f),
            buf: self.buf,
        }
    }
}

impl<T, B, E> From<BufResult<T, B, E>> for (Result<T, E>, B) {
    fn from(value: BufResult<T, B, E>) -> Self {
        value.into_parts()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::ErrorKind;

    fn take<B: IoBufMut>(mut buf: B) -> (B, *mut u8) {
        let ptr = buf.stable_mut_ptr();
        (buf, ptr)
    }

    #[test]
    fn vec_pointer_survives_moves_and_writes() {
        let buf = Vec::with_capacity(8);
        let (mut buf, ptr) = take(buf);
        let boxed = Box::new(std::mem::take(&mut buf));
        let mut buf = *boxed;
        // Write through the pointer obtained before the moves, as the kernel would.
        for i in 0..5 {
            // SAFETY: ptr points into the Vec's capacity, which is unaliased.
            unsafe { ptr.add(i).write(i as u8) };
        }
        // SAFETY: five bytes were initialized above and capacity is 8.
        unsafe { buf.set_init(5) };
        assert_eq!(buf, [0, 1, 2, 3, 4]);
        assert_eq!(buf.stable_ptr(), ptr.cast_const());
    }

    #[test]
    fn set_init_never_shrinks() {
        let mut buf = vec![1, 2, 3];
        // SAFETY: 1 <= len, so nothing changes.
        unsafe { buf.set_init(1) };
        assert_eq!(buf.bytes_init(), 3);
    }

    #[test]
    fn empty_vec_is_valid() {
        let mut buf = Vec::<u8>::new();
        assert_eq!(buf.bytes_init(), 0);
        assert_eq!(buf.bytes_total(), 0);
        assert!(!buf.stable_mut_ptr().is_null());
    }

    #[test]
    fn static_buffers() {
        let bytes: &'static [u8] = b"abc";
        assert_eq!(bytes.bytes_init(), 3);
        assert_eq!(bytes.stable_ptr(), bytes.as_ptr());
        let text: &'static str = "hello";
        assert_eq!(text.bytes_init(), 5);
    }

    #[test]
    fn buf_result_helpers() {
        let result: BufResult<usize, Vec<u8>> = BufResult::new(Ok(2), vec![1, 2]);
        let mapped = result.map(|n| n * 2);
        assert_eq!(mapped.result, Ok(4));
        let (result, buf): (Result<usize>, Vec<u8>) = mapped.into();
        assert_eq!(result, Ok(4));
        assert_eq!(buf, [1, 2]);

        let failed: BufResult<usize, &'static [u8]> =
            BufResult::new(Err(Error::new(ErrorKind::Canceled)), b"x");
        let (result, buf) = failed.into_parts();
        assert!(result.unwrap_err().is_canceled());
        assert_eq!(buf, b"x");
    }

    type Result<T> = std::result::Result<T, Error>;
}
