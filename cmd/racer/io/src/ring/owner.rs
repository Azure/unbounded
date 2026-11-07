//! Type-erased, address-stable storage for one operation's owners.
//!
//! Owners up to [`INLINE_BYTES`] bytes (alignment up to 16) are stored inline
//! in the reactor's preallocated slot; larger owners are boxed. The storage
//! never moves while an operation is in flight because slots live in boxed
//! chunks that are never reallocated.
use std::mem::{MaybeUninit, align_of, size_of};

/// Largest owner stored inline, matching the old per-submission charge.
pub const INLINE_BYTES: usize = 4096;

/// Inline backing for one owner.
#[repr(C, align(16))]
pub(crate) struct InlineBytes([MaybeUninit<u8>; INLINE_BYTES]);

/// A slot's owner cell: inline bytes plus the drop function of the stored type.
pub(crate) struct OwnerCell {
    bytes: InlineBytes,

    drop: Option<unsafe fn(*mut InlineBytes)>,
}

/// Whether `O` is stored inline (otherwise as `Box<O>`).
const fn fits<O>() -> bool {
    size_of::<O>() <= INLINE_BYTES && align_of::<O>() <= 16
}

impl OwnerCell {
    /// An empty cell.
    pub(crate) fn new() -> Self {
        Self {
            bytes: InlineBytes([MaybeUninit::uninit(); INLINE_BYTES]),
            drop: None,
        }
    }

    /// Whether an owner is stored.
    pub(crate) fn is_occupied(&self) -> bool {
        self.drop.is_some()
    }

    /// Store `owner`. Panics if the cell is occupied (an internal invariant).
    pub(crate) fn put<O: 'static>(&mut self, owner: O) {
        assert!(self.drop.is_none(), "owner cell occupied");
        let base = self.bytes.0.as_mut_ptr();
        if fits::<O>() {
            // SAFETY: the inline region is large and aligned enough for O.
            unsafe { base.cast::<O>().write(owner) };
        } else {
            // SAFETY: a Box pointer fits inline with pointer alignment.
            unsafe { base.cast::<Box<O>>().write(Box::new(owner)) };
        }
        self.drop = Some(drop_owner::<O>);
    }

    /// Borrow the stored owner as `O`.
    #[cfg(test)]
    ///
    /// # Safety
    /// The cell must hold an `O` stored by [`Self::put`].
    pub(crate) unsafe fn get_mut<O: 'static>(&mut self) -> &mut O {
        debug_assert!(self.drop.is_some());
        let base = self.bytes.0.as_mut_ptr();
        if fits::<O>() {
            // SAFETY: the caller guarantees an inline O lives here.
            unsafe { &mut *base.cast::<O>() }
        } else {
            // SAFETY: the caller guarantees a Box<O> lives here.
            unsafe { &mut *base.cast::<Box<O>>() }
        }
    }

    /// Move the stored owner out, leaving the cell empty.
    ///
    /// # Safety
    /// The cell must hold an `O` stored by [`Self::put`].
    pub(crate) unsafe fn take<O: 'static>(&mut self) -> O {
        debug_assert!(self.drop.is_some());
        self.drop = None;
        let base = self.bytes.0.as_mut_ptr();
        if fits::<O>() {
            // SAFETY: the caller guarantees an inline O; the cell is now empty.
            unsafe { base.cast::<O>().read() }
        } else {
            // SAFETY: the caller guarantees a Box<O>; the cell is now empty.
            unsafe { *base.cast::<Box<O>>().read() }
        }
    }

    /// Raw pointer to the inline storage (for type-erased callbacks).
    pub(crate) fn raw(&mut self) -> *mut InlineBytes {
        &mut self.bytes
    }

    /// Drop the stored owner, if any.
    pub(crate) fn clear(&mut self) {
        if let Some(drop) = self.drop.take() {
            // SAFETY: drop matches the type stored by put.
            unsafe { drop(&mut self.bytes) };
        }
    }
}

impl Drop for OwnerCell {
    /// Drop any remaining owner.
    fn drop(&mut self) {
        self.clear();
    }
}

/// Drop an `O` stored by [`OwnerCell::put`].
unsafe fn drop_owner<O: 'static>(bytes: *mut InlineBytes) {
    let base = bytes.cast::<u8>();
    if fits::<O>() {
        // SAFETY: an inline O is stored at base.
        unsafe { base.cast::<O>().drop_in_place() };
    } else {
        // SAFETY: a Box<O> is stored at base.
        unsafe { base.cast::<Box<O>>().drop_in_place() };
    }
}

/// Borrow an owner through a type-erased pointer.
///
/// # Safety
/// `bytes` must point to a cell holding an `O` stored by [`OwnerCell::put`].
pub(crate) unsafe fn erased_mut<'a, O: 'static>(bytes: *mut InlineBytes) -> &'a mut O {
    let base = bytes.cast::<u8>();
    if fits::<O>() {
        // SAFETY: guaranteed by the caller.
        unsafe { &mut *base.cast::<O>() }
    } else {
        // SAFETY: guaranteed by the caller.
        unsafe { &mut *base.cast::<Box<O>>() }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{cell::Cell, rc::Rc};

    // The payload is never read; it only sets the owner's size and alignment.
    struct Tracked(Rc<Cell<usize>>, #[allow(dead_code)] Vec<u8>);

    impl Drop for Tracked {
        fn drop(&mut self) {
            self.0.set(self.0.get() + 1);
        }
    }

    #[test]
    fn inline_and_boxed_owners_round_trip_and_drop_once() {
        let drops = Rc::new(Cell::new(0));
        let mut cell = Box::new(OwnerCell::new());
        cell.put(Tracked(drops.clone(), vec![1, 2, 3]));
        assert!(cell.is_occupied());
        // SAFETY: a Tracked was stored above.
        assert_eq!(unsafe { cell.get_mut::<Tracked>() }.1, [1, 2, 3]);
        // SAFETY: as above.
        let owner = unsafe { cell.take::<Tracked>() };
        assert!(!cell.is_occupied());
        drop(owner);
        assert_eq!(drops.get(), 1);

        let big = [7u8; INLINE_BYTES + 1];
        cell.put((Tracked(drops.clone(), Vec::new()), big));
        // SAFETY: the stored type matches.
        let erased = unsafe { erased_mut::<(Tracked, [u8; INLINE_BYTES + 1])>(cell.raw()) };
        assert_eq!(erased.1[INLINE_BYTES], 7);
        cell.clear();
        assert_eq!(drops.get(), 2);
        cell.put(Tracked(drops.clone(), Vec::new()));
        drop(cell);
        assert_eq!(drops.get(), 3);
    }

    #[test]
    fn overaligned_owners_are_boxed() {
        #[repr(align(64))]
        struct Wide(u8);
        let mut cell = Box::new(OwnerCell::new());
        cell.put(Wide(9));
        // SAFETY: a Wide was stored above.
        let wide = unsafe { cell.take::<Wide>() };
        assert_eq!(wide.0, 9);
        assert_eq!((&wide as *const Wide as usize) % 64, 0);
    }
}
