use super::*;

#[test]
fn aligned_pool_reuses_only_fenced_zeroed_admitted_storage() {
    let admission = Admission::new(crate::test_support::cluster::config(false).limits);
    let cache = CacheId("pool".into());
    let pool = Rc::new(RefCell::new(None));
    let alignment = DirectAlignment::validate(512, 512, 512).unwrap();
    let mut buffer = alignment
        .allocate(
            512,
            admission
                .reserve(Some(&cache), ResourceClass::Ciphertext, 512)
                .unwrap(),
        )
        .unwrap()
        .pooled(&pool);
    let pointer = buffer.bytes().unwrap().as_ptr();
    buffer.bytes_mut().unwrap().fill(42);
    drop(buffer);
    assert_eq!(admission.used(ResourceClass::Ciphertext), 512);
    let mut reused = pool.borrow_mut().take().unwrap();
    assert_eq!(reused.bytes().unwrap().as_ptr(), pointer);
    assert!(reused.bytes().unwrap().iter().all(|b| *b == 0));
    reused
        .rebind(
            admission
                .reserve(Some(&cache), ResourceClass::Ciphertext, 512)
                .unwrap(),
        )
        .unwrap();
    assert_eq!(admission.used(ResourceClass::Ciphertext), 512);
    drop(reused.pooled(&pool));
    drop(pool);
    assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
}

#[test]
fn geometry_rounds_without_assuming_page_size() {
    let a = DirectAlignment::validate(512, 512, 1024).unwrap();
    assert_eq!(a.extent(512, 1025).unwrap().length(), 2048);
    assert!(a.extent(1, 1).is_err());
    assert!(a.extent(0, usize::MAX).is_err());
    assert!(DirectAlignment::validate(3, 512, 512).is_err());
    assert!(DirectExtent::checked(u64::MAX, 1).is_err());
}

#[test]
fn real_file_is_direct_aligned_and_sparse_without_reactor() {
    let directory = crate::store::tests::Directory::new();
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let slabs = Slabs::new(
        WorkerId(0),
        directory.0.clone(),
        Rc::new(Reactor::new(admission.clone())),
        admission.clone(),
        64 * 1024 * 1024,
        32 * 1024 * 1024,
    );
    assert!(!directory.0.join("worker-0-slab-0.dat").exists());
    let alignment = slabs.open_now().unwrap();
    let opened = slabs.opened.borrow();
    let fd = opened.as_ref().unwrap().file.as_raw_fd();
    // SAFETY: descriptor is open and owned above; these calls synchronously borrow buffers.
    assert_ne!(
        unsafe { libc::fcntl(fd, libc::F_GETFL) } & libc::O_DIRECT,
        0
    );
    let extent = alignment.extent(0, 31).unwrap();
    let mut buffer = slabs.allocate(extent.length(), None).unwrap();
    buffer.bytes_mut().unwrap()[..31].fill(42);
    assert_eq!(
        unsafe { libc::pwrite(fd, buffer.bytes().unwrap().as_ptr().cast(), buffer.len(), 0) },
        buffer.len() as isize
    );
    buffer.bytes_mut().unwrap().fill(0);
    assert_eq!(
        unsafe {
            libc::pread(
                fd,
                buffer.bytes_mut().unwrap().as_mut_ptr().cast(),
                extent.length(),
                0,
            )
        },
        extent.length() as isize
    );
    assert_eq!(&buffer.bytes().unwrap()[..31], &[42; 31]);
    assert!(buffer.bytes().unwrap()[31..].iter().all(|b| *b == 0));
    assert_eq!(
        unsafe { libc::pwrite(fd, buffer.bytes().unwrap().as_ptr().cast(), 31, 1) },
        -1
    );
    drop(buffer);
    slabs.reclaim_buffer();
    assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
    let conflicting = Slabs::new(
        WorkerId(0),
        directory.0.clone(),
        Rc::new(Reactor::new(admission.clone())),
        admission,
        64 * 1024 * 1024,
        32 * 1024 * 1024,
    );
    assert_eq!(conflicting.open_now(), Err(Error::Unavailable));
}
