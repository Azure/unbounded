//! Receive-side zero-copy: received ciphertext stays in the registered pool
//! buffer for the request, and every retaining path copies it out.

use super::*;
use crate::memory::MemoryCache;
use crate::memory::UnverifiedPage;
use crate::memory::tests::admission as memory_admission;
use crate::memory::tests::bundle;

/// A world with one link and a memory-side admission and page bundle.
fn setup() -> (
    World,
    Rc<flow_control::Quotas<AdmissionPolicy>>,
    crate::memory::PageResult,
) {
    let mut w = world();
    link_up(&mut w);
    let admission = memory_admission(16);
    let page = bundle(&admission, "v1");
    (w, admission, page)
}

fn owned(w: &World) -> Option<usize> {
    w.a.links.buffers(RAIL, BufferState::Owned)
}

fn free(w: &World) -> Option<usize> {
    w.a.links.buffers(RAIL, BufferState::Free)
}

/// Wrap a freshly received copy of `page`'s ciphertext without copying.
fn registered(
    w: &mut World,
    admission: &Rc<flow_control::Quotas<AdmissionPolicy>>,
    page: &CiphertextPage,
) -> CiphertextPage {
    let received = receive_registered(w, page);
    let pointer = received.bytes().as_ptr();
    let envelope = page.envelope().clone();
    let reservation = admission
        .reserve(
            Some(&envelope.page.version.object.cache),
            ResourceClass::Ciphertext,
            received.len(),
        )
        .unwrap();
    let wrapped = BufferPool::new(admission.clone())
        .registered_ciphertext(reservation, envelope, received)
        .unwrap();
    assert!(wrapped.is_registered());
    assert_eq!(wrapped.bytes().as_ptr(), pointer, "no copy on receive");
    wrapped
}

#[test]
fn registered_ciphertext_reads_in_place_and_drop_frees_buffer() {
    let (mut w, admission, page) = setup();
    let ciphertext = registered(&mut w, &admission, &page.ciphertext);
    assert_eq!(ciphertext.bytes(), page.ciphertext.bytes());
    assert_eq!(ciphertext.checksum(), page.ciphertext.checksum());
    assert_eq!(owned(&w), Some(1));
    let before = admission.used(ResourceClass::Ciphertext);
    // A clone on another thread keeps the buffer; dropping it there frees it.
    let clone = ciphertext.clone();
    drop(ciphertext);
    assert_eq!(owned(&w), Some(1));
    std::thread::spawn(move || drop(clone)).join().unwrap();
    assert_eq!(admission.used(ResourceClass::Ciphertext), before - 19);
    pump!(w);
    assert_eq!(owned(&w), Some(0));
    assert_eq!(free(&w), Some(2));
}

#[test]
fn registered_ciphertext_rejects_bad_length_and_reservation() {
    let (mut w, admission, page) = setup();
    let pool = BufferPool::new(admission.clone());
    let cache = page.metadata.version.object.cache.clone();
    let reserve = |class, amount| admission.reserve(Some(&cache), class, amount).unwrap();
    // Length disagrees with the envelope.
    let mut short = page.ciphertext.envelope().clone();
    short.plaintext_length -= 1;
    short.ciphertext_length -= 1;
    let received = receive_registered(&mut w, &page.ciphertext);
    assert_eq!(
        pool.registered_ciphertext(reserve(ResourceClass::Ciphertext, 18), short, received)
            .err(),
        Some(Error::CorruptRecord)
    );
    pump!(w);
    assert_eq!(free(&w), Some(2), "the rejected page is freed");
    // Wrong reservation class.
    let received = receive_registered(&mut w, &page.ciphertext);
    assert_eq!(
        pool.registered_ciphertext(
            reserve(ResourceClass::Plaintext, 19),
            page.ciphertext.envelope().clone(),
            received,
        )
        .err(),
        Some(Error::InvalidConfiguration)
    );
    pump!(w);
    assert_eq!(free(&w), Some(2));
    assert_eq!(
        admission.used(ResourceClass::Plaintext),
        3,
        "only the bundle"
    );
}

#[test]
fn retainable_and_rehome_copy_out_and_release_the_buffer() {
    let (mut w, admission, page) = setup();
    let pool = BufferPool::new(admission.clone());
    let owned_page = page.ciphertext.clone();
    assert!(Arc::ptr_eq(
        &pool.retainable(owned_page).unwrap().inner,
        &page.ciphertext.inner
    ));
    let ciphertext = registered(&mut w, &admission, &page.ciphertext);
    let pointer = ciphertext.bytes().as_ptr();
    let used = admission.used(ResourceClass::Ciphertext);
    let copy = pool.retainable(ciphertext).unwrap();
    assert!(!copy.is_registered());
    assert_ne!(copy.bytes().as_ptr(), pointer);
    assert_eq!(copy.bytes(), page.ciphertext.bytes());
    // The registered charge is released; the aligned copy is charged for its
    // zero-padded length.
    let length = page.ciphertext.bytes().len();
    assert_eq!(
        admission.used(ResourceClass::Ciphertext),
        used - length + length.next_multiple_of(4096),
        "charge moved"
    );
    assert!(copy.is_disk_aligned(crate::memory::default_disk_alignment()));
    pump!(w);
    assert_eq!(free(&w), Some(2));
    // An exclusive registered page still copies on rehome.
    let ciphertext = registered(&mut w, &admission, &page.ciphertext);
    let pointer = ciphertext.bytes().as_ptr();
    let reservation = admission
        .reserve(
            Some(&page.metadata.version.object.cache),
            ResourceClass::Ciphertext,
            19,
        )
        .unwrap();
    let moved = ciphertext.rehome(reservation).unwrap();
    assert!(!moved.is_registered());
    assert_ne!(moved.bytes().as_ptr(), pointer);
    assert_eq!(moved.bytes(), page.ciphertext.bytes());
    pump!(w);
    assert_eq!(free(&w), Some(2));
}

#[test]
fn retainable_failure_drops_the_page_and_frees_the_buffer() {
    let (mut w, admission, page) = setup();
    let ciphertext = registered(&mut w, &admission, &page.ciphertext);
    let pool = BufferPool::new(admission.clone());
    // Exhaust ciphertext admission so the copy-out cannot be charged.
    let cache = page.metadata.version.object.cache.clone();
    let mut held = Vec::new();
    while let Ok(charge) = admission.reserve(Some(&cache), ResourceClass::Ciphertext, 4096) {
        held.push(charge);
    }
    while let Ok(charge) = admission.reserve(Some(&cache), ResourceClass::Ciphertext, 1) {
        held.push(charge);
    }
    assert!(pool.retainable(ciphertext).is_err());
    pump!(w);
    assert_eq!(free(&w), Some(2), "the failed copy-out drops the page");
}

#[test]
fn memory_cache_publish_copies_out_registered_ciphertext() {
    let (mut w, admission, page) = setup();
    let cache = MemoryCache::new(
        BufferPool::new(admission.clone()),
        crate::test_support::availability(),
    );
    let id = page.plaintext.page().clone();
    // Unverified ciphertext entries.
    let unverified = UnverifiedPage {
        copy: crate::memory::CiphertextCopy {
            metadata: page.metadata.clone(),
            ciphertext: registered(&mut w, &admission, &page.ciphertext),
        },
        disk_token: None,
    };
    cache.publish_ciphertext(unverified.clone()).unwrap();
    let stored = cache.unverified(&id).unwrap().unwrap();
    assert!(!stored.copy.ciphertext.is_registered());
    assert_eq!(stored.copy.ciphertext.bytes(), page.ciphertext.bytes());
    drop(stored);
    // The request-scoped original still holds the buffer, which a registered
    // invalidation matches by contents.
    assert_eq!(owned(&w), Some(1));
    cache.invalidate_ciphertext(&unverified);
    assert!(cache.unverified(&id).unwrap().is_none());
    drop(unverified);
    pump!(w);
    assert_eq!(free(&w), Some(2));
    // Verified pages.
    let mut result = page.clone();
    result.ciphertext = registered(&mut w, &admission, &page.ciphertext);
    cache.publish(result.clone()).unwrap();
    // A duplicate publish keeps the first copy and does not charge again.
    let used = admission.used(ResourceClass::Ciphertext);
    cache.publish(result.clone()).unwrap();
    assert_eq!(admission.used(ResourceClass::Ciphertext), used);
    drop(result);
    pump!(w);
    assert_eq!(free(&w), Some(2), "the cache does not pin the buffer");
    let cached = cache.get(&id).unwrap().unwrap();
    assert!(!cached.ciphertext.is_registered());
    assert_eq!(cached.ciphertext.bytes(), page.ciphertext.bytes());
}
