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
    let env = w.env.clone();
    link_up(&env, &mut w);
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
    env: &racer_io::Env,
    w: &mut World,
    admission: &Rc<flow_control::Quotas<AdmissionPolicy>>,
    page: &CiphertextPage,
) -> CiphertextPage {
    let received = receive_registered(env, w, page);
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
    let env = racer_io::Env::host();
    let (mut w, admission, page) = setup();
    let ciphertext = registered(&env, &mut w, &admission, &page.ciphertext);
    assert_eq!(ciphertext.bytes(), page.ciphertext.bytes());
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
    let env = racer_io::Env::host();
    let (mut w, admission, page) = setup();
    let pool = BufferPool::new(admission.clone());
    let cache = page.metadata.version.object.cache.clone();
    let reserve = |class, amount| admission.reserve(Some(&cache), class, amount).unwrap();
    // Length disagrees with the envelope.
    let mut short = page.ciphertext.envelope().clone();
    short.plaintext_length -= 1;
    short.ciphertext_length -= 1;
    let received = receive_registered(&env, &mut w, &page.ciphertext);
    assert_eq!(
        pool.registered_ciphertext(reserve(ResourceClass::Ciphertext, 18), short, received)
            .err(),
        Some(Error::CorruptRecord)
    );
    pump!(w);
    assert_eq!(free(&w), Some(2), "the rejected page is freed");
    // Wrong reservation class.
    let received = receive_registered(&env, &mut w, &page.ciphertext);
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
        4096,
        "only the bundle, at one plaintext granule"
    );
}

#[test]
fn retainable_and_rehome_copy_out_and_release_the_buffer() {
    let env = racer_io::Env::host();
    let (mut w, admission, page) = setup();
    let pool = BufferPool::new(admission.clone());
    let owned_page = page.ciphertext.clone();
    assert!(Arc::ptr_eq(
        &pool.retainable(owned_page).unwrap().inner,
        &page.ciphertext.inner
    ));
    let ciphertext = registered(&env, &mut w, &admission, &page.ciphertext);
    let pointer = ciphertext.bytes().as_ptr();
    let used = admission.used(ResourceClass::Ciphertext);
    let copy = pool.retainable(ciphertext).unwrap();
    assert!(!copy.is_registered());
    assert_ne!(copy.bytes().as_ptr(), pointer);
    assert_eq!(copy.bytes(), page.ciphertext.bytes());
    // The registered charge is released; the pooled aligned copy is charged
    // for its size class.
    let length = page.ciphertext.bytes().len();
    assert_eq!(
        admission.used(ResourceClass::Ciphertext),
        used - length + pool.aligned_capacity(length).unwrap(),
        "charge moved"
    );
    assert!(copy.is_disk_aligned(crate::memory::default_disk_alignment()));
    pump!(w);
    assert_eq!(free(&w), Some(2));
    // An exclusive registered page still copies on rehome.
    let ciphertext = registered(&env, &mut w, &admission, &page.ciphertext);
    let pointer = ciphertext.bytes().as_ptr();
    let reservation = admission
        .reserve(
            Some(&page.metadata.version.object.cache),
            ResourceClass::Ciphertext,
            page_alloc::buf::GRANULE,
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
    let env = racer_io::Env::host();
    let (mut w, admission, page) = setup();
    let ciphertext = registered(&env, &mut w, &admission, &page.ciphertext);
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
    let env = racer_io::Env::host();
    let (mut w, admission, page) = setup();
    let cache = MemoryCache::new(
        BufferPool::new(admission.clone()),
        crate::test_support::availability(&env),
    );
    let id = page.plaintext.page().clone();
    // Unverified ciphertext entries.
    let unverified = UnverifiedPage {
        copy: crate::memory::CiphertextCopy {
            metadata: page.metadata.clone(),
            ciphertext: registered(&env, &mut w, &admission, &page.ciphertext),
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
    result.ciphertext = registered(&env, &mut w, &admission, &page.ciphertext);
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

/// Reserve the pooled aligned capacity for copying `page` out.
fn aligned_charge(
    pool: &BufferPool,
    admission: &Rc<flow_control::Quotas<AdmissionPolicy>>,
    page: &CiphertextPage,
) -> flow_control::Charge<AdmissionPolicy> {
    admission
        .reserve(
            Some(&page.envelope().page.version.object.cache),
            ResourceClass::Ciphertext,
            pool.aligned_capacity(page.bytes().len()).unwrap(),
        )
        .unwrap()
}

#[test]
fn detach_registered_copies_once_into_the_pool_and_shares_with_the_cache() {
    let env = racer_io::Env::host();
    let (mut w, admission, page) = setup();
    let pool = BufferPool::new(admission.clone());
    let cache = MemoryCache::new(pool.clone(), crate::test_support::availability(&env));
    let baseline = admission.used(ResourceClass::Ciphertext);
    let ciphertext = registered(&env, &mut w, &admission, &page.ciphertext);
    let pointer = ciphertext.bytes().as_ptr();
    let before = crate::memory::registered_copy_outs();
    let charge = aligned_charge(&pool, &admission, &ciphertext);
    let detached = pool.detach_registered(ciphertext, charge).unwrap();
    assert_eq!(crate::memory::registered_copy_outs(), before + 1);
    assert!(!detached.is_registered());
    assert_ne!(detached.bytes().as_ptr(), pointer);
    assert_eq!(detached.bytes(), page.ciphertext.bytes());
    assert_eq!(
        detached.envelope(),
        page.ciphertext.envelope(),
        "envelope kept"
    );
    assert!(detached.is_disk_aligned(crate::memory::default_disk_alignment()));
    // The registered buffer was released by the copy-out.
    pump!(w);
    assert_eq!(owned(&w), Some(0));
    assert_eq!(free(&w), Some(2));
    // Publishing to memory and retaining for the store copy nothing more.
    let mut result = page.clone();
    result.ciphertext = detached.clone();
    cache.publish(result.clone()).unwrap();
    let retained = pool.retainable(result.ciphertext.clone()).unwrap();
    let id = page.plaintext.page().clone();
    let cached = cache.get(&id).unwrap().unwrap();
    assert!(Arc::ptr_eq(&cached.ciphertext.inner, &detached.inner));
    assert!(Arc::ptr_eq(&retained.inner, &detached.inner));
    assert_eq!(crate::memory::registered_copy_outs(), before + 1);
    // Non-registered pages pass through and release the reservation.
    let charge = aligned_charge(&pool, &admission, &detached);
    let same = pool.detach_registered(detached.clone(), charge).unwrap();
    assert!(Arc::ptr_eq(&same.inner, &detached.inner));
    drop((same, retained, cached, result, detached));
    drop(cache);
    pool.reclaim_idle_buffers();
    assert_eq!(admission.used(ResourceClass::Ciphertext), baseline);
}

#[test]
fn detach_registered_shares_an_earlier_retained_copy() {
    let env = racer_io::Env::host();
    let (mut w, admission, page) = setup();
    let pool = BufferPool::new(admission.clone());
    let baseline = admission.used(ResourceClass::Ciphertext);
    let ciphertext = registered(&env, &mut w, &admission, &page.ciphertext);
    let before = crate::memory::registered_copy_outs();
    // A retaining path copies the page out first and the registered page
    // remembers that copy.
    let retained = pool.retainable(ciphertext.clone()).unwrap();
    assert!(!retained.is_registered());
    assert_eq!(crate::memory::registered_copy_outs(), before + 1);
    // Detaching afterwards shares it instead of copying again, and releases
    // the unused reservation.
    let charge = aligned_charge(&pool, &admission, &ciphertext);
    let detached = pool.detach_registered(ciphertext, charge).unwrap();
    assert!(Arc::ptr_eq(&detached.inner, &retained.inner));
    assert_eq!(crate::memory::registered_copy_outs(), before + 1);
    pump!(w);
    assert_eq!(free(&w), Some(2), "registered buffer returned");
    drop((detached, retained));
    pool.reclaim_idle_buffers();
    assert_eq!(admission.used(ResourceClass::Ciphertext), baseline);
}

#[test]
fn detach_registered_returns_the_buffer_to_the_worker_pool() {
    let env = racer_io::Env::host();
    let (mut w, admission, page) = setup();
    let pool = BufferPool::new(admission.clone());
    let ciphertext = registered(&env, &mut w, &admission, &page.ciphertext);
    let charge = aligned_charge(&pool, &admission, &ciphertext);
    let detached = pool.detach_registered(ciphertext, charge).unwrap();
    let idle = pool.disk_idle_bytes();
    drop(detached);
    assert!(pool.disk_idle_bytes() > idle, "the copy is pooled storage");
    pump!(w);
    assert_eq!(free(&w), Some(2));
}

#[test]
fn detach_registered_refuses_short_charges_and_frees_the_buffer() {
    let env = racer_io::Env::host();
    let (mut w, admission, page) = setup();
    let pool = BufferPool::new(admission.clone());
    let baseline = admission.used(ResourceClass::Ciphertext);
    let ciphertext = registered(&env, &mut w, &admission, &page.ciphertext);
    let before = crate::memory::registered_copy_outs();
    let short = admission
        .reserve(
            Some(&page.metadata.version.object.cache),
            ResourceClass::Ciphertext,
            19,
        )
        .unwrap();
    assert_eq!(
        pool.detach_registered(ciphertext, short).err(),
        Some(Error::InvalidConfiguration)
    );
    assert_eq!(crate::memory::registered_copy_outs(), before);
    pump!(w);
    assert_eq!(free(&w), Some(2));
    assert_eq!(admission.used(ResourceClass::Ciphertext), baseline);
}

#[test]
fn rehome_aligned_copies_shared_pages_into_the_pool() {
    let env = racer_io::Env::host();
    let (mut w, admission, page) = setup();
    let pool = BufferPool::new(admission.clone());
    let ciphertext = registered(&env, &mut w, &admission, &page.ciphertext);
    let before = crate::memory::registered_copy_outs();
    let charge = aligned_charge(&pool, &admission, &ciphertext);
    let moved = pool.rehome_aligned(ciphertext, charge).unwrap();
    assert_eq!(crate::memory::registered_copy_outs(), before + 1);
    assert!(moved.is_disk_aligned(crate::memory::default_disk_alignment()));
    // A shared owned page copies too; an exclusive one moves in place.
    let shared = page.ciphertext.clone();
    let charge = aligned_charge(&pool, &admission, &shared);
    let copy = pool.rehome_aligned(shared, charge).unwrap();
    assert!(!Arc::ptr_eq(&copy.inner, &page.ciphertext.inner));
    assert_eq!(copy.bytes(), page.ciphertext.bytes());
    assert_eq!(crate::memory::registered_copy_outs(), before + 1);
    let exclusive = copy;
    let pointer = exclusive.bytes().as_ptr();
    let charge = aligned_charge(&pool, &admission, &exclusive);
    let same = pool.rehome_aligned(exclusive, charge).unwrap();
    assert_eq!(same.bytes().as_ptr(), pointer);
    drop((moved, same));
    pump!(w);
    assert_eq!(free(&w), Some(2));
}

#[test]
fn receive_hold_is_recorded_once_the_buffer_returns_to_the_pool() {
    use crate::telemetry::Event;
    let env = racer_io::Env::host();
    let (mut w, admission, page) = setup();
    let metrics = crate::telemetry::Metrics::default();
    let clock = w.clock.ctx(1).clock.clone();
    let received = receive_registered(&env, &mut w, &page.ciphertext);
    let envelope = page.ciphertext.envelope().clone();
    let reservation = admission
        .reserve(
            Some(&envelope.page.version.object.cache),
            ResourceClass::Ciphertext,
            received.len(),
        )
        .unwrap();
    let hold = crate::memory::ReceiveHold::new(metrics.clone(), clock.clone(), clock.now());
    let pool = BufferPool::new(admission.clone());
    let ciphertext = pool
        .registered_ciphertext_held(reservation, envelope, received, Some(hold))
        .unwrap();
    w.clock.advance(std::time::Duration::from_millis(7));
    // A clone still holds the buffer: nothing is recorded yet.
    let clone = ciphertext.clone();
    drop(ciphertext);
    assert_eq!(metrics.count(Event::RdmaReceiveHoldCount), 0);
    // A copy-out drops the last registered handle and records the hold.
    let charge = aligned_charge(&pool, &admission, &clone);
    let detached = pool.detach_registered(clone, charge).unwrap();
    assert!(!detached.is_registered());
    assert_eq!(metrics.count(Event::RdmaReceiveHoldCount), 1);
    assert_eq!(metrics.count(Event::RdmaReceiveHoldNs), 7_000_000);
    pump!(w);
    assert_eq!(free(&w), Some(2), "the buffer is back while the copy lives");
    drop(detached);
}

/// A random page key, fixed for the test thread so a sealed page and the
/// key a decrypt job derives from it agree.
fn page_key() -> [u8; 32] {
    thread_local! {
        static KEY: [u8; 32] = {
            let mut key = [0; 32];
            getrandom::getrandom(&mut key).unwrap();
            key
        };
    }
    KEY.with(|key| *key)
}

/// A real sealed 3-byte page with the same envelope as the bundle, keyed with
/// `page_key()`, as aligned pooled storage. Returns it and its plaintext.
fn really_sealed(
    pool: &BufferPool,
    admission: &Rc<flow_control::Quotas<AdmissionPolicy>>,
    page: &crate::memory::PageResult,
) -> CiphertextPage {
    let envelope = page.ciphertext.envelope().clone();
    let aad = crate::security::page_aad(&envelope).unwrap();
    let mut sealed = vec![0u8; 3 + racer_crypto::TAG_LEN];
    racer_crypto::seal(&page_key(), &envelope.nonce.0, &aad, b"abc", &mut sealed).unwrap();
    let mut buffer = pool
        .encryption_output(
            admission
                .reserve(
                    Some(&envelope.page.version.object.cache),
                    ResourceClass::Ciphertext,
                    sealed.len(),
                )
                .unwrap(),
            sealed.len(),
        )
        .unwrap();
    buffer.as_mut_slice().copy_from_slice(&sealed);
    pool.aligned_ciphertext_page(buffer, envelope).unwrap()
}

fn host_memfd(size: usize) -> std::fs::File {
    use std::io::Write;
    use std::os::fd::FromRawFd;
    // SAFETY: plain syscall; the descriptor is checked and owned below.
    let fd = unsafe {
        libc::memfd_create(
            c"racer-fused".as_ptr(),
            libc::MFD_CLOEXEC | libc::MFD_ALLOW_SEALING,
        )
    };
    assert!(fd >= 0);
    // SAFETY: a fresh descriptor this test owns.
    let mut file = unsafe { std::fs::File::from_raw_fd(fd) };
    file.write_all(&vec![0xee; size]).unwrap();
    // SAFETY: adds a seal to a descriptor this test owns.
    assert_eq!(
        unsafe { libc::fcntl(fd, libc::F_ADD_SEALS, libc::F_SEAL_SHRINK) },
        0
    );
    file
}

/// A host-read decrypt of a registered page with a fused store, for
/// `range` of its 3 plaintext bytes and an AAD override.
fn fused_job(
    env: &racer_io::Env,
    w: &mut World,
    pool: &BufferPool,
    admission: &Rc<flow_control::Quotas<AdmissionPolicy>>,
    page: &crate::memory::PageResult,
    range: std::ops::Range<u64>,
    aad: Option<Vec<u8>>,
) -> (
    Box<crate::client::direct::DecryptInto>,
    std::fs::File,
    CiphertextPage,
) {
    let source = really_sealed(pool, admission, page);
    let ciphertext = registered(env, w, admission, &source);
    let charge = aligned_charge(pool, admission, &ciphertext);
    let store = pool
        .aligned_ciphertext(charge, ciphertext.bytes().len())
        .unwrap();
    let mut result = page.clone();
    result.ciphertext = ciphertext;
    let mut sealed = crate::read::sealed::tests::sealed(&result);
    sealed.key = racer_crypto::xaes_derive(&page_key(), &sealed.envelope.nonce.0);
    if let Some(aad) = aad {
        sealed.aad = aad;
    }
    sealed.fused_store = Some(store);
    let file = host_memfd(4096);
    let length = range.end - range.start;
    let mapping = crate::client::direct::HostMapping::new(
        &file,
        racer_wire::object::client::Destination { offset: 0, length },
    )
    .unwrap();
    let job = Box::new(crate::client::direct::DecryptInto {
        page: Box::new(sealed),
        range,
        mapping: Arc::new(mapping),
        destination: 0,
        stored: None,
        numa: None,
    });
    (job, file, source)
}

fn file_bytes(file: &std::fs::File, length: usize) -> Vec<u8> {
    use std::os::unix::fs::FileExt;
    let mut bytes = vec![0; length];
    file.read_exact_at(&mut bytes, 0).unwrap();
    bytes
}

#[test]
fn fused_host_decrypt_detaches_the_registered_page_in_the_same_pass() {
    let env = racer_io::Env::host();
    let (mut w, admission, page) = setup();
    let pool = BufferPool::new(admission.clone());
    for (range, expected) in [(0..3, &b"abc"[..]), (1..3, b"bc"), (1..2, b"b")] {
        let (mut job, file, source) =
            fused_job(&env, &mut w, &pool, &admission, &page, range, None);
        let before = crate::memory::registered_copy_outs();
        job.run().unwrap();
        assert!(job.fused());
        assert_eq!(file_bytes(&file, expected.len()), expected);
        job.publish_store();
        assert!(!job.fused());
        assert_eq!(crate::memory::registered_copy_outs(), before + 1);
        // The verdict's copy swaps to the detached one, so dropping the job
        // and the report returns the registered buffer.
        let mut report = job.page.report();
        assert!(
            report
                .copy
                .as_ref()
                .unwrap()
                .copy
                .ciphertext
                .is_registered()
        );
        report.prefer_retained();
        let detached = report.copy.as_ref().unwrap().copy.ciphertext.clone();
        assert!(!detached.is_registered());
        assert_eq!(detached.bytes(), source.bytes());
        assert_eq!(detached.envelope(), source.envelope());
        assert!(detached.is_disk_aligned(crate::memory::default_disk_alignment()));
        drop(job);
        pump!(w);
        assert_eq!(free(&w), Some(2), "registered buffer returned");
        drop((report, detached, source));
    }
}

#[test]
fn fused_host_decrypt_failure_zeroes_the_destination_and_publishes_nothing() {
    let env = racer_io::Env::host();
    let (mut w, admission, page) = setup();
    let pool = BufferPool::new(admission.clone());
    let baseline = admission.used(ResourceClass::Ciphertext);
    for range in [0..3, 1..2] {
        let length = (range.end - range.start) as usize;
        let (mut job, file, source) = fused_job(
            &env,
            &mut w,
            &pool,
            &admission,
            &page,
            range,
            Some(b"tampered".to_vec()),
        );
        let before = crate::memory::registered_copy_outs();
        assert_eq!(job.run().err(), Some(Error::CorruptRecord));
        assert!(!job.fused());
        assert_eq!(file_bytes(&file, length), vec![0; length]);
        job.publish_store();
        assert_eq!(crate::memory::registered_copy_outs(), before);
        let mut report = job.page.report();
        report.prefer_retained();
        assert!(
            report
                .copy
                .as_ref()
                .unwrap()
                .copy
                .ciphertext
                .is_registered()
        );
        drop((job, report, source));
        pump!(w);
        assert_eq!(free(&w), Some(2));
    }
    pool.reclaim_idle_buffers();
    assert_eq!(admission.used(ResourceClass::Ciphertext), baseline);
}
