// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Thread-local registry for the current shard's [`StorageRing`].
//!
//! A shard thread installs its `StorageRing` here at bring-up so code
//! deep in the call stack can reach the disk ring without threading it
//! through every signature. The registry is `!Send` by construction
//! (thread-local + `Rc`): each shard thread sees only its own ring.

use std::cell::RefCell;
use std::rc::Rc;

use super::storage::StorageRing;

thread_local! {
    static CURRENT: RefCell<Option<Rc<StorageRing>>> = const { RefCell::new(None) };
}

/// Install `ring` as the current thread's storage ring, replacing any
/// previous one.
pub fn set_current_storage_ring(ring: Rc<StorageRing>) {
    CURRENT.with(|c| *c.borrow_mut() = Some(ring));
}

/// Clear the current thread's storage ring, if any.
pub fn clear_current_storage_ring() {
    CURRENT.with(|c| *c.borrow_mut() = None);
}

/// Run `f` against the current thread's storage ring, returning its
/// result, or `None` if no ring is installed.
pub fn with_current_storage_ring<R>(f: impl FnOnce(&StorageRing) -> R) -> Option<R> {
    CURRENT.with(|c| c.borrow().as_ref().map(|r| f(r)))
}

/// Clone the current thread's storage ring handle, or `None` if no
/// ring is installed. Callers that need to hold the ring across an
/// `await` point (the device's async `read` / `write`) use this
/// instead of [`with_current_storage_ring`], whose borrow cannot
/// escape the closure.
pub fn current_storage_ring() -> Option<Rc<StorageRing>> {
    CURRENT.with(|c| c.borrow().clone())
}

#[cfg(test)]
mod tests {
    use super::super::storage::StorageRingConfig;
    use super::*;

    #[test]
    fn set_with_clear_round_trip() {
        // No ring installed initially.
        assert!(with_current_storage_ring(|r| r.queue_depth()).is_none());

        let ring = Rc::new(StorageRing::new(StorageRingConfig::test_local()).expect("ring"));
        set_current_storage_ring(Rc::clone(&ring));

        let qd = with_current_storage_ring(|r| r.queue_depth());
        assert_eq!(qd, Some(StorageRingConfig::test_local().queue_depth));

        clear_current_storage_ring();
        assert!(with_current_storage_ring(|r| r.queue_depth()).is_none());
    }
}
