//! Completion-owned buffer contracts, ported unchanged in substance.
use crate::Result;

/// An exclusively owned, stable backing allocation with an independent lifetime.
///
/// # Safety
/// Moving the owner or calling either accessor must not relocate or resize its
/// backing allocation. Accessors expose the same initialized region, with no
/// independently accessible mutable aliases. Ownership includes the allocation's
/// quota reservation; neither may be freed or recycled before the final fence.
/// `'static` excludes request-scoped borrows; it does not require leaking memory.
/// Derived raw pointers must remain valid across owner moves into the reactor's
/// slot. Stable addresses alone are insufficient: Box backing is retagged on
/// moves in Miri's aliasing models. Use private, non-resizing Vec storage or an
/// explicitly managed allocation; never reborrow its bytes while I/O is pending.
/// Inline arrays cannot implement this contract: moving them relocates the bytes.
pub unsafe trait IoBuffer: 'static {
    /// Borrow the initialized region after its previous operation has fenced.
    fn bytes(&self) -> Result<&[u8]>;

    /// Exclusively borrow the same region before deriving a receive pointer.
    fn bytes_mut(&mut self) -> Result<&mut [u8]>;
}

/// Completion-owned immutable send storage. Shared aliases are permitted only
/// when none can mutate or relocate the initialized bytes. The owner retains
/// allocation admission through the final original/cancel completion fence.
/// This trait deliberately grants no receive or mutable-buffer capability.
///
/// # Safety
/// Accessors must expose the same initialized allocation across owner moves.
/// No shared alias may mutate, resize, or free the bytes before completion.
pub unsafe trait SendBuffer: 'static {
    /// Borrow initialized immutable bytes for one send operation.
    fn send_bytes(&self) -> Result<&[u8]>;
}

/// Completion-owned immutable storage for the body of a vectored file write.
///
/// This is the file-write counterpart of [`SendBuffer`] and carries the same
/// contract. It is blanket-implemented for every [`SendBuffer`] (and therefore
/// every [`IoBuffer`]), so shared, reference-counted bodies can be written
/// without copying them into exclusive storage.
///
/// # Safety
/// Accessors must expose the same initialized allocation across owner moves.
/// No shared alias may mutate, resize, or free the bytes before completion.
pub unsafe trait SharedWriteBuffer: 'static {
    /// Borrow initialized immutable bytes for one write operation.
    fn write_bytes(&self) -> Result<&[u8]>;
}

// SAFETY: exclusive stable storage also meets immutable send requirements.
unsafe impl<B: IoBuffer> SendBuffer for B {
    /// Borrow exclusive storage immutably under the stronger IoBuffer contract.
    fn send_bytes(&self) -> Result<&[u8]> {
        self.bytes()
    }
}

// SAFETY: the SendBuffer contract is the SharedWriteBuffer contract.
unsafe impl<B: SendBuffer> SharedWriteBuffer for B {
    /// Borrow the same immutable bytes a send would publish.
    fn write_bytes(&self) -> Result<&[u8]> {
        self.send_bytes()
    }
}

// SAFETY: a Vec owned by the reactor is never resized while I/O is pending;
// its heap allocation is stable across moves of the Vec header.
unsafe impl IoBuffer for Vec<u8> {
    /// The whole initialized vector.
    fn bytes(&self) -> Result<&[u8]> {
        Ok(self)
    }

    /// The whole initialized vector, mutably.
    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        Ok(self)
    }
}

// SAFETY: shared immutable bytes never move or change while any Rc is alive.
unsafe impl SendBuffer for std::rc::Rc<[u8]> {
    /// The shared bytes.
    fn send_bytes(&self) -> Result<&[u8]> {
        Ok(self)
    }
}

// SAFETY: as for Rc<[u8]>.
unsafe impl SendBuffer for std::sync::Arc<[u8]> {
    /// The shared bytes.
    fn send_bytes(&self) -> Result<&[u8]> {
        Ok(self)
    }
}

/// Owners returned by single-buffer operations after every fence.
#[derive(Debug)]
pub struct Completion<B, L = ()> {
    /// Original allocation.
    pub buffer: B,

    /// Bytes transferred, possibly less than the buffer length.
    pub bytes: usize,

    /// Caller-owned guard retained through completion.
    pub lease: L,
}

/// Owners returned by two-segment vectored operations after every fence.
#[derive(Debug)]
pub struct VectoredCompletion<H, B, L = ()> {
    /// First segment.
    pub header: H,

    /// Second segment, transferred immediately after the first.
    pub body: B,

    /// Bytes transferred across both segments.
    pub bytes: usize,

    /// Caller-owned guard retained through completion.
    pub lease: L,
}
