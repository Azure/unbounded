//! Filesystem primitives on both drivers.
use super::{checkpoint::Slots, *};
use crate::{
    Driver, Reactor, WorkerCtx,
    ctx::VirtualClock,
    exec::LocalExecutor,
    ring::{Config, OpCtx},
    sim::SimOs,
};
use std::path::PathBuf;

/// Run `future` to completion on `reactor`.
fn block_on<F: std::future::Future + 'static>(reactor: &Reactor, future: F) -> F::Output {
    LocalExecutor::new(Some(reactor.wake_handle()))
        .block_on(reactor, future)
        .unwrap()
}

/// Every available backend with a private root directory for the test.
fn backends(test: &str) -> Vec<(Reactor, PathBuf)> {
    let os = SimOs::new();
    let sim = Reactor::new(
        Driver::Sim(os),
        Config::new(32),
        WorkerCtx::simulated(&VirtualClock::new(), 3, 0),
    )
    .unwrap();
    let mut out = vec![(sim, PathBuf::from("/state"))];
    if let Some(kernel) = crate::ring::tests::kernel(32) {
        let tmp = Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../tmp");
        std::fs::create_dir_all(&tmp).unwrap();
        let root = std::fs::canonicalize(tmp)
            .unwrap()
            .join(format!("racer-io-fs-{test}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&root);
        out.push((kernel, root));
    }
    out
}

#[test]
fn validation_and_access_policy() {
    assert!(component("a".as_ref(), 10).is_ok());
    for bad in ["", "..", ".", "/a", "a/b", "a\0"] {
        assert_eq!(
            component(bad.as_ref(), 10),
            Err(Error::InvalidInput),
            "{bad}"
        );
    }
    assert_eq!(path_name("abcdef".as_ref(), 3), Err(Error::InvalidInput));
    // SAFETY: statx is plain data.
    let mut stat: libc::statx = unsafe { std::mem::zeroed() };
    let required = AccessRequirements {
        owner: 5,
        forbidden_mode: 0o077,
        links: Some(1),
    };
    assert_eq!(
        check_access(&stat, required),
        Err(AccessError::MissingMetadata)
    );
    stat.stx_mask = libc::STATX_MODE | libc::STATX_UID | libc::STATX_NLINK;
    stat.stx_uid = 5;
    stat.stx_nlink = 1;
    stat.stx_mode = 0o600;
    assert_eq!(check_access(&stat, required), Ok(()));
    stat.stx_mode = 0o640;
    assert_eq!(
        check_access(&stat, required),
        Err(AccessError::PermissionDenied)
    );
    stat.stx_mode = 0o600;
    stat.stx_nlink = 2;
    assert_eq!(
        check_access(&stat, required),
        Err(AccessError::PermissionDenied)
    );
    let mut buffer = Buffer::from_bytes(b"abc").unwrap();
    assert_eq!(buffer.advance(0), Err(Error::Io));
    buffer.advance(2).unwrap();
    assert_eq!(buffer.prefix(1).unwrap(), b"c");
    assert_eq!(buffer.advance(2), Err(Error::Io));
}

#[test]
fn private_directories_atomic_writes_and_bounded_reads() {
    for (reactor, root) in backends("atomic") {
        let r = reactor.clone();
        let cleanup = (!reactor.is_sim()).then(|| root.clone());
        block_on(&reactor, async move {
            let ctx = OpCtx::none();
            let dir = r
                .open_directory(&root.join("a/b"), true, true, &ctx)
                .await
                .unwrap();
            r.atomic_write(&dir, "key", b"secret", &ctx).await.unwrap();
            r.atomic_write(&dir, "key", b"rotated", &ctx).await.unwrap();
            let read = r.read_child(&dir, "key", 64, true, &ctx).await.unwrap();
            assert_eq!(&*read, b"rotated");
            assert_eq!(
                r.read_child(&dir, "key", 3, false, &ctx).await.unwrap_err(),
                Error::InvalidInput
            );
            assert_eq!(
                r.read_child(&dir, "missing", 3, false, &ctx)
                    .await
                    .unwrap_err(),
                Error::NotFound
            );
            let path = root.join("a/b/key");
            assert_eq!(&*r.read_path(&path, 64, &ctx).await.unwrap(), b"rotated");
            let file = Rc::new(
                r.open(
                    Some(dir.clone()),
                    c"key".into(),
                    libc::O_RDONLY,
                    BENEATH,
                    &ctx,
                )
                .unwrap()
                .await
                .unwrap(),
            );
            let small = NonZeroUsize::new(2).unwrap();
            assert_eq!(
                r.read_bounded(file.clone(), 3, small, &ctx)
                    .await
                    .unwrap_err(),
                Error::Overloaded
            );
            assert_eq!(
                &*r.read_bounded(file, 7, small, &ctx).await.unwrap(),
                b"rotated"
            );
            r.remove_synced(dir.clone(), c"key".into(), &ctx)
                .await
                .unwrap();
            r.remove_synced(dir, c"key".into(), &ctx).await.unwrap();
            assert!(
                r.open_directory(Path::new("/x/../y"), false, false, &ctx)
                    .await
                    .is_err()
            );
        });
        if let Some(root) = cleanup {
            let _ = std::fs::remove_dir_all(root);
        }
    }
}

#[test]
fn replacement_rejects_reused_stages() {
    for (reactor, root) in backends("stage") {
        let r = reactor.clone();
        let cleanup = (!reactor.is_sim()).then(|| root.clone());
        block_on(&reactor, async move {
            let ctx = OpCtx::none();
            let dir = r.open_directory(&root, true, false, &ctx).await.unwrap();
            let staged = r
                .create_stage(dir.clone(), "s".as_ref(), &ctx)
                .await
                .unwrap();
            let chunk = NonZeroUsize::new(4).unwrap();
            r.write_at(staged.clone(), 0, b"x".to_vec(), (), &ctx)
                .await
                .unwrap();
            let replacement = Replacement {
                directory: dir.clone(),
                staged: staged.clone(),
                temporary: c"s".into(),
                target: c"t".into(),
                durability: Durability::FileAndDirectory,
            };
            let error = r
                .replace(replacement, b"data", chunk, &ctx)
                .await
                .unwrap_err();
            assert_eq!(error, ReplacementError::BeforeRename(Error::InvalidInput));
            let same = Replacement {
                directory: dir.clone(),
                staged,
                temporary: c"s".into(),
                target: c"s".into(),
                durability: Durability::Publish,
            };
            let error = r.replace(same, b"", chunk, &ctx).await.unwrap_err();
            assert_eq!(error.cause(), Error::InvalidInput);
            let fresh = r
                .create_stage(dir.clone(), "s".as_ref(), &ctx)
                .await
                .unwrap();
            let replacement = Replacement {
                directory: dir.clone(),
                staged: fresh,
                temporary: c"s".into(),
                target: c"t".into(),
                durability: Durability::FileAndDirectory,
            };
            r.replace(replacement, b"0123456789", chunk, &ctx)
                .await
                .unwrap();
            assert_eq!(
                &*r.read_child(&dir, "t", 64, false, &ctx).await.unwrap(),
                b"0123456789"
            );
        });
        if let Some(root) = cleanup {
            let _ = std::fs::remove_dir_all(root);
        }
    }
}

const SLOTS: Slots<8> = Slots {
    names: ["a.ckpt", "b.ckpt"],
    minimum_bytes: 8,
    maximum_bytes: 64,
};

fn sequence(header: &[u8]) -> Result<u64> {
    Ok(u64::from_le_bytes(
        header.try_into().map_err(|_| Error::InvalidInput)?,
    ))
}

fn decode(bytes: &[u8], _: usize) -> Result<Vec<u8>> {
    if bytes[8..].starts_with(b"bad") {
        return Err(Error::InvalidInput);
    }
    Ok(bytes[8..].to_vec())
}

fn image(sequence: u64, body: &[u8]) -> Vec<u8> {
    let mut bytes = sequence.to_le_bytes().to_vec();
    bytes.extend_from_slice(body);
    bytes
}

#[test]
fn checkpoints_alternate_slots_and_decode_newest_first() {
    for (reactor, root) in backends("checkpoint") {
        let r = reactor.clone();
        let cleanup = (!reactor.is_sim()).then(|| root.clone());
        block_on(&reactor, async move {
            let ctx = OpCtx::none();
            let dir = root.join("ckpt");
            let chunk = NonZeroUsize::new(5).unwrap();
            let mut none = SLOTS
                .candidates(&r, &dir, 64, sequence, decode, &ctx)
                .await
                .unwrap();
            assert!(none.next(&r, &ctx).await.is_none());
            assert_eq!(SLOTS.next_publication(None), Some((0, 1)));
            SLOTS
                .publish(&r, &dir, 0, &image(1, b"one"), chunk, &ctx)
                .await
                .unwrap();
            let (slot, next) = SLOTS.next_publication(Some((0, 1))).unwrap();
            assert_eq!((slot, next), (1, 2));
            SLOTS
                .publish(&r, &dir, slot, &image(next, b"two"), chunk, &ctx)
                .await
                .unwrap();
            let mut found = SLOTS
                .candidates(&r, &dir, 64, sequence, decode, &ctx)
                .await
                .unwrap();
            assert_eq!(found.next(&r, &ctx).await, Some((1, b"two".to_vec())));
            assert_eq!(found.next(&r, &ctx).await, Some((0, b"one".to_vec())));
            assert!(found.next(&r, &ctx).await.is_none());
            // A newer slot that fails to decode falls back to the older one.
            SLOTS
                .publish(&r, &dir, 0, &image(3, b"bad"), chunk, &ctx)
                .await
                .unwrap();
            let mut fallback = SLOTS
                .candidates(&r, &dir, 64, sequence, decode, &ctx)
                .await
                .unwrap();
            assert_eq!(fallback.next(&r, &ctx).await, Some((1, b"two".to_vec())));
            assert_eq!(fallback.skipped, [(0, Error::InvalidInput)]);
            // Oversized slots are skipped before allocation.
            let mut tight = SLOTS
                .candidates(&r, &dir, 9, sequence, decode, &ctx)
                .await
                .unwrap();
            assert!(tight.next(&r, &ctx).await.is_none());
            assert_eq!(tight.skipped.len(), 2);
            assert_eq!(SLOTS.next_publication(Some((2, 1))), None);
            assert_eq!(SLOTS.next_publication(Some((0, u64::MAX))), None);
        });
        if let Some(root) = cleanup {
            let _ = std::fs::remove_dir_all(root);
        }
    }
}
