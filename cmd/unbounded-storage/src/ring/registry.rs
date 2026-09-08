// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

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

/// Restores the previous thread-local storage ring when an installation
/// scope ends.
pub struct CurrentStorageRingGuard {
    previous: Option<Rc<StorageRing>>,
}

impl Drop for CurrentStorageRingGuard {
    fn drop(&mut self) {
        CURRENT.with(|current| {
            *current.borrow_mut() = self.previous.take();
        });
    }
}

/// Install `ring` as the current thread's storage ring until the returned
/// guard is dropped.
pub fn install_current_storage_ring(ring: Rc<StorageRing>) -> CurrentStorageRingGuard {
    let previous = CURRENT.with(|current| current.borrow_mut().replace(ring));
    CurrentStorageRingGuard { previous }
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
    fn install_scope_restores_empty_registry() {
        // No ring installed initially.
        assert!(with_current_storage_ring(|r| r.queue_depth()).is_none());

        let ring = Rc::new(StorageRing::new(StorageRingConfig::test_local()).expect("ring"));
        let guard = install_current_storage_ring(Rc::clone(&ring));

        let qd = with_current_storage_ring(|r| r.queue_depth());
        assert_eq!(qd, Some(StorageRingConfig::test_local().queue_depth));

        drop(guard);
        assert!(with_current_storage_ring(|r| r.queue_depth()).is_none());
    }

    #[test]
    fn nested_install_restores_previous_ring() {
        clear_current_storage_ring();
        let outer = Rc::new(StorageRing::new(StorageRingConfig::test_local()).expect("outer"));
        let inner = Rc::new(StorageRing::new(StorageRingConfig::default()).expect("inner"));
        let outer_guard = install_current_storage_ring(Rc::clone(&outer));

        {
            let _inner_guard = install_current_storage_ring(Rc::clone(&inner));
            let current = current_storage_ring().expect("inner installed");
            assert!(Rc::ptr_eq(&current, &inner));
        }

        let current = current_storage_ring().expect("outer restored");
        assert!(Rc::ptr_eq(&current, &outer));
        drop(outer_guard);
        assert!(current_storage_ring().is_none());
    }

    #[test]
    fn install_is_restored_during_unwind() {
        clear_current_storage_ring();
        let result = std::panic::catch_unwind(|| {
            let ring = Rc::new(StorageRing::new(StorageRingConfig::test_local()).expect("ring"));
            let _guard = install_current_storage_ring(ring);
            panic!("test unwind");
        });

        assert!(result.is_err());
        assert!(current_storage_ring().is_none());
    }
}
