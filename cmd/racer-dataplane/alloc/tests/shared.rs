//! Shared plaintext slab under its global allocator. This binary owns the
//! process-wide slab, so every assertion lives in one test.

use page_alloc::shared::{self, SLOT_BYTES, SharedAllocator};
use std::os::fd::AsRawFd;

#[global_allocator]
static ALLOCATOR: SharedAllocator = SharedAllocator::new();

#[test]
fn slab_serves_requested_allocations_and_maps_read_only() {
    assert!(shared::zeroed_vec(16).is_none(), "nothing before install");
    shared::install(3 * SLOT_BYTES - 1).expect("install");
    assert!(matches!(
        shared::install(SLOT_BYTES),
        Err(shared::InstallError::AlreadyInstalled)
    ));
    assert_eq!(shared::len(), 3 * SLOT_BYTES);
    assert_eq!(shared::free_slots(), 3);

    // Ordinary allocations never land in the slab.
    let private = vec![7u8; 1 << 20];
    assert!(!shared::contains(private.as_ptr()));
    assert!(shared::offset_of(&private).is_none());

    // Requested allocations are zeroed, slot aligned, and addressable by offset.
    let mut first = shared::zeroed_vec(SLOT_BYTES).expect("slot");
    assert!(shared::contains(first.as_ptr()));
    assert!(first.iter().all(|&b| b == 0));
    let offset = shared::offset_of(&first).expect("offset");
    assert_eq!(offset % SLOT_BYTES as u64, 0);
    assert_eq!(shared::offset_of(&first[5..9]), Some(offset + 5));
    assert!(shared::zeroed_vec(SLOT_BYTES + 1).is_none());
    first[..4].copy_from_slice(b"page");
    first[SLOT_BYTES - 1] = 9;

    // The read-only descriptor shows the same bytes and refuses writable mappings.
    let fd = shared::readonly_fd().expect("fd").as_raw_fd();
    let len = shared::len();
    let view = unsafe {
        libc::mmap(
            std::ptr::null_mut(),
            len,
            libc::PROT_READ,
            libc::MAP_SHARED,
            fd,
            0,
        )
    };
    assert_ne!(view, libc::MAP_FAILED);
    let mapped = unsafe { std::slice::from_raw_parts(view.cast::<u8>(), len) };
    let at = offset as usize;
    assert_eq!(&mapped[at..at + 4], b"page");
    assert_eq!(mapped[at + SLOT_BYTES - 1], 9);
    let writable = unsafe {
        libc::mmap(
            std::ptr::null_mut(),
            len,
            libc::PROT_READ | libc::PROT_WRITE,
            libc::MAP_SHARED,
            fd,
            0,
        )
    };
    assert_eq!(writable, libc::MAP_FAILED);
    assert_eq!(unsafe { libc::write(fd, b"x".as_ptr().cast(), 1) }, -1);

    // Freed slots are reused and zeroed again.
    drop(first);
    let again = shared::zeroed_vec(SLOT_BYTES).expect("reused slot");
    assert_eq!(shared::offset_of(&again), Some(offset));
    assert!(again.iter().all(|&b| b == 0));
    assert_eq!(mapped[at], 0);

    // Exhaustion falls back to None, never to private memory.
    let second = shared::zeroed_vec(1).expect("second");
    let mut third = shared::vec_with_capacity(SLOT_BYTES).expect("third");
    assert_eq!(shared::free_slots(), 0);
    assert!(shared::zeroed_vec(1).is_none());
    assert!(shared::vec_with_capacity(1).is_none());
    drop(second);
    assert_eq!(shared::free_slots(), 1);

    // Growth within a slot stays in place; growth beyond it moves out and frees the slot.
    third.extend_from_slice(&[3u8; 4096]);
    let pointer = third.as_ptr();
    third.shrink_to_fit();
    assert_eq!(third.as_ptr(), pointer);
    third.reserve(SLOT_BYTES);
    assert!(!shared::contains(third.as_ptr()));
    assert!(third.iter().all(|&b| b == 3));
    assert_eq!(shared::free_slots(), 2);

    drop(again);
    drop(third);
    assert_eq!(shared::free_slots(), 3);
    unsafe { libc::munmap(view, len) };
}
