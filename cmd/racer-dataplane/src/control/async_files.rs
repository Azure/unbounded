//! Bounded file operations issued only through the serving worker's reactor.
use crate::{
    error::{Error, Result},
    runtime::{deadline::RequestScope, reactor::Reactor},
};
use std::{
    ffi::{CString, OsStr},
    os::{fd::OwnedFd, unix::ffi::OsStrExt},
    path::{Component, Path},
    rc::Rc,
};
use zeroize::Zeroizing;
pub(crate) type Directory = Rc<OwnedFd>;
const BENEATH: u64 = 0x08;
const NO_MAGICLINKS: u64 = 0x02;
const NO_SYMLINKS: u64 = 0x04;
fn name(s: &OsStr) -> Result<CString> {
    if s.as_bytes().len() > 4096 {
        return Err(Error::InvalidRequest);
    }
    CString::new(s.as_bytes()).map_err(|_| Error::InvalidRequest)
}
fn component(s: &str) -> Result<CString> {
    if !matches!(Path::new(s).components().next(), Some(Component::Normal(_))) || s.contains('/') {
        return Err(Error::InvalidRequest);
    }
    name(s.as_ref())
}
pub(crate) async fn directory(
    r: &Reactor,
    path: &Path,
    create: bool,
    private: bool,
    scope: &RequestScope,
) -> Result<Directory> {
    // Bound traversal work before issuing the first operation.
    name(path.as_os_str())?;
    let mut fd = r
        .file_open(
            None,
            CString::new(if path.is_absolute() { "/" } else { "." }).unwrap(),
            libc::O_RDONLY | libc::O_DIRECTORY,
            NO_SYMLINKS,
            scope,
        )
        .await?;
    for part in path.components() {
        let Component::Normal(part) = part else {
            if matches!(part, Component::RootDir | Component::CurDir) {
                continue;
            }
            return Err(Error::InvalidConfiguration);
        };
        if create {
            match r.file_mkdir(fd.clone(), name(part)?, scope).await {
                Ok(()) => r.file_sync(fd.clone(), scope).await?,
                // An earlier canceled mkdir may have created this component but
                // missed the parent fsync. Reestablish that durability fence.
                Err(Error::Replay) => r.file_sync(fd.clone(), scope).await?,
                Err(e) => return Err(e),
            }
        }
        fd = r
            .file_open(
                Some(fd),
                name(part)?,
                libc::O_RDONLY | libc::O_DIRECTORY,
                BENEATH | NO_SYMLINKS,
                scope,
            )
            .await?;
    }
    if private {
        check_private(&r.file_stat(fd.clone(), scope).await?, false)?;
    }
    Ok(fd)
}
fn check_private(stat: &libc::statx, regular: bool) -> Result<()> {
    let mask = libc::STATX_MODE | libc::STATX_UID | libc::STATX_NLINK;
    if stat.stx_mask & mask != mask {
        return Err(Error::Io);
    }
    if stat.stx_mode & 0o077 != 0
        || stat.stx_uid != unsafe { libc::geteuid() }
        || regular && stat.stx_nlink != 1
    {
        return Err(Error::Unauthorized);
    }
    Ok(())
}
pub(crate) async fn read_file(
    r: &Reactor,
    fd: Directory,
    limit: usize,
    private: bool,
    scope: &RequestScope,
) -> Result<Zeroizing<Vec<u8>>> {
    if limit > 1024 * 1024 {
        return Err(Error::Overloaded);
    }
    let stat = r.file_stat(fd.clone(), scope).await?;
    let mask = libc::STATX_TYPE | libc::STATX_SIZE;
    if stat.stx_mask & mask != mask
        || stat.stx_mode as u32 & libc::S_IFMT != libc::S_IFREG
        || stat.stx_size > limit as u64
    {
        return Err(Error::InvalidRequest);
    }
    if private {
        check_private(&stat, true)?;
    }
    let mut out = Zeroizing::new(Vec::new());
    loop {
        let buffer = r.file_buffer((limit + 1 - out.len()).min(16384))?;
        let completion = r
            .read_at(fd.clone(), out.len() as u64, buffer, (), scope)
            .await?;
        if completion.bytes == 0 {
            return Ok(out);
        }
        out.extend_from_slice(completion.buffer.prefix(completion.bytes)?);
        if out.len() > limit {
            return Err(Error::Overloaded);
        }
    }
}
pub(crate) async fn read_path(
    r: &Reactor,
    path: &Path,
    limit: usize,
    scope: &RequestScope,
) -> Result<Zeroizing<Vec<u8>>> {
    let fd = r
        .file_open(
            None,
            name(path.as_os_str())?,
            libc::O_RDONLY,
            NO_MAGICLINKS,
            scope,
        )
        .await?;
    read_file(r, fd, limit, false, scope).await
}
pub(crate) async fn read_at(
    r: &Reactor,
    dir: &Directory,
    file: &str,
    limit: usize,
    private: bool,
    scope: &RequestScope,
) -> Result<Zeroizing<Vec<u8>>> {
    let fd = r
        .file_open(
            Some(dir.clone()),
            component(file)?,
            libc::O_RDONLY,
            BENEATH | NO_SYMLINKS,
            scope,
        )
        .await?;
    read_file(r, fd, limit, private, scope).await
}
pub(crate) async fn projected_file(
    r: &Reactor,
    path: &Path,
    file: &str,
    limit: usize,
    scope: &RequestScope,
) -> Result<Zeroizing<Vec<u8>>> {
    let dir = directory(r, path, false, false, scope).await?;
    // One openat2 resolves ..data and pins the target directory across rotation.
    // BENEATH rejects absolute/escaping links; NO_MAGICLINKS rejects proc escapes.
    let generation = r
        .file_open(
            Some(dir),
            CString::new("..data").unwrap(),
            libc::O_RDONLY | libc::O_DIRECTORY,
            BENEATH | NO_MAGICLINKS,
            scope,
        )
        .await?;
    read_at(r, &generation, file, limit, false, scope).await
}
pub(crate) async fn atomic_write(
    r: &Reactor,
    dir: &Directory,
    target: &str,
    bytes: &[u8],
    scope: &RequestScope,
) -> Result<()> {
    component(target)?;
    if bytes.len() > 1024 * 1024 {
        return Err(Error::Overloaded);
    }
    let temporary = format!(".{target}.stage");
    remove(r, dir, &temporary, scope).await?;
    let fd = r
        .file_open(
            Some(dir.clone()),
            component(&temporary)?,
            libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL,
            BENEATH | NO_SYMLINKS,
            scope,
        )
        .await?;
    let mut buffer = r.file_bytes(bytes)?;
    let mut offset = 0;
    while buffer.remaining() != 0 {
        let completion = r.write_at(fd.clone(), offset, buffer, (), scope).await?;
        buffer = completion.buffer;
        buffer.advance(completion.bytes)?;
        offset += completion.bytes as u64;
    }
    r.file_sync(fd, scope).await?;
    r.file_rename(
        dir.clone(),
        component(&temporary)?,
        component(target)?,
        scope,
    )
    .await?;
    r.file_sync(dir.clone(), scope).await
}
pub(crate) async fn remove(
    r: &Reactor,
    dir: &Directory,
    file: &str,
    scope: &RequestScope,
) -> Result<()> {
    match r.file_unlink(dir.clone(), component(file)?, scope).await {
        Ok(()) | Err(Error::MissingKey) => (),
        Err(e) => return Err(e),
    }
    r.file_sync(dir.clone(), scope).await
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::control::{enrollment::Enrollment, testing};
    #[test]
    fn canceled_transaction_never_exposes_partial_replacement() {
        let Some(r) = testing::reactor() else {
            return;
        };
        let d = testing::Directory::new();
        let scope = testing::scope();
        let dir = testing::drive(
            &r,
            Box::pin(directory(&r, &d.0.join("private"), true, true, &scope)),
        )
        .unwrap();
        let old = vec![7; 1001];
        let new = vec![9; 1003];
        for cut in 0..10 {
            testing::drive(&r, Box::pin(atomic_write(&r, &dir, "state", &old, &scope))).unwrap();
            let mut request = testing::scope();
            request.request = crate::model::identity::RequestId([cut; 16]);
            let mut future = Box::pin(atomic_write(&r, &dir, "state", &new, &request));
            use std::future::Future;
            let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
            for _ in 0..cut {
                if future.as_mut().poll(&mut cx).is_ready() {
                    break;
                }
                let until = std::time::Instant::now() + std::time::Duration::from_secs(5);
                while r.in_flight() != 0 {
                    assert!(std::time::Instant::now() < until);
                    r.poll_budgeted(1).unwrap();
                    r.wait(std::time::Duration::from_millis(1)).unwrap();
                }
            }
            request.cancel().unwrap();
            drop(future);
            testing::drive(&r, r.file_fence(request.request)).unwrap();
            let bytes =
                testing::drive(&r, Box::pin(read_at(&r, &dir, "state", 2000, true, &scope)))
                    .unwrap();
            assert!(
                *bytes == old || *bytes == new,
                "partial transaction at cut {cut}"
            );
            testing::drive(
                &r,
                Box::pin(atomic_write(&r, &dir, "state", b"retry", &scope)),
            )
            .unwrap();
            assert_eq!(
                &*testing::drive(&r, Box::pin(read_at(&r, &dir, "state", 2000, true, &scope)))
                    .unwrap(),
                b"retry"
            );
        }
    }
    #[test]
    fn canceled_replacement_never_exposes_partial_state_and_retry_fences_late_work() {
        let Some(r) = testing::reactor() else {
            return;
        };
        let d = testing::Directory::new();
        let scope = testing::scope();
        let dir = testing::drive(
            &r,
            Box::pin(directory(&r, &d.0.join("private"), true, true, &scope)),
        )
        .unwrap();
        let old = vec![11; 32771];
        let new = vec![22; 65539];
        // Stop between each submission/completion boundary. The worker is not
        // allowed to publish replacement bytes before the final durability fence.
        for stop in 0..12u8 {
            testing::drive(&r, Box::pin(atomic_write(&r, &dir, "state", &old, &scope))).unwrap();
            let mut turn = testing::scope();
            turn.request = crate::model::identity::RequestId([stop; 16]);
            let mut operation: crate::error::Operation<'_, ()> =
                Box::pin(atomic_write(&r, &dir, "state", &new, &turn));
            let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
            for _ in 0..stop {
                if operation.as_mut().poll(&mut cx).is_ready() {
                    break;
                }
                let until = std::time::Instant::now() + std::time::Duration::from_secs(5);
                while r.in_flight() != 0 {
                    assert!(std::time::Instant::now() < until);
                    r.poll_budgeted(1).unwrap();
                    r.wait(std::time::Duration::from_millis(1)).unwrap();
                }
            }
            turn.cancel().unwrap();
            drop(operation);
            testing::drive(&r, r.file_fence(turn.request)).unwrap();
            let bytes = testing::drive(
                &r,
                Box::pin(read_at(&r, &dir, "state", new.len(), true, &scope)),
            )
            .unwrap();
            assert!(
                *bytes == old || *bytes == new,
                "partial state at boundary {stop}"
            );
            testing::drive(
                &r,
                Box::pin(atomic_write(&r, &dir, "state", b"retry", &scope)),
            )
            .unwrap();
            assert_eq!(
                &*testing::drive(&r, Box::pin(read_at(&r, &dir, "state", 10, true, &scope)))
                    .unwrap(),
                b"retry"
            );
        }
        assert_eq!(r.in_flight(), 0);
    }
    #[test]
    fn canceled_replacement_at_each_submission_boundary_is_complete_or_old() {
        let Some(r) = testing::reactor() else {
            return;
        };
        let d = testing::Directory::new();
        let setup = testing::scope();
        let dir = testing::drive(
            &r,
            Box::pin(directory(&r, &d.0.join("private"), true, true, &setup)),
        )
        .unwrap();
        let old = vec![b'a'; 32769];
        let new = vec![b'b'; 49153];
        testing::drive(
            &r,
            Box::pin(atomic_write(&r, &dir, "identity", &old, &setup)),
        )
        .unwrap();
        // Atomic write has seven sequential SQEs: unlink, dir sync, open, write,
        // file sync, rename, dir sync. Cancel before/after each possible boundary.
        for boundary in 0..7 {
            let scope = testing::scope();
            let mut future = Box::pin(atomic_write(&r, &dir, "identity", &new, &scope));
            let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
            for _ in 0..=boundary {
                assert!(future.as_mut().poll(&mut cx).is_pending());
                if r.in_flight() == 0 {
                    panic!("missing owned submission");
                }
                if boundary == 0 {
                    break;
                }
                let until = std::time::Instant::now() + std::time::Duration::from_secs(5);
                while r.in_flight() != 0 {
                    assert!(std::time::Instant::now() < until);
                    r.poll_budgeted(1).unwrap();
                    r.wait(std::time::Duration::from_millis(1)).unwrap();
                }
            }
            scope.cancel().unwrap();
            drop(future);
            testing::drive(&r, r.file_fence(scope.request)).unwrap();
            let check = testing::scope();
            let bytes = testing::drive(
                &r,
                Box::pin(read_at(&r, &dir, "identity", new.len(), true, &check)),
            )
            .unwrap();
            assert!(
                &*bytes == &old || &*bytes == &new,
                "partial committed identity"
            );
            testing::drive(
                &r,
                Box::pin(atomic_write(&r, &dir, "identity", &old, &check)),
            )
            .unwrap();
        }
        assert_eq!(r.in_flight(), 0);
    }
    #[test]
    fn cancel_each_persistence_boundary_preserves_complete_old_or_new_file() {
        let Some(r) = testing::reactor() else {
            return;
        };
        let d = testing::Directory::new();
        let scope = testing::scope();
        let dir = testing::drive(
            &r,
            Box::pin(directory(&r, &d.0.join("identity"), true, true, &scope)),
        )
        .unwrap();
        let old = vec![b'a'; 32769];
        let new = vec![b'b'; 32771];
        for boundary in 0..12 {
            testing::drive(&r, Box::pin(atomic_write(&r, &dir, "state", &old, &scope))).unwrap();
            let mut turn = testing::scope();
            turn.request = crate::model::identity::RequestId([boundary; 16]);
            let mut write: crate::error::Operation<'_, ()> =
                Box::pin(atomic_write(&r, &dir, "state", &new, &turn));
            let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
            for _ in 0..boundary {
                if write.as_mut().poll(&mut cx).is_ready() {
                    break;
                }
                // Complete one SQE without polling its successor, then interrupt.
                let until = std::time::Instant::now() + std::time::Duration::from_secs(5);
                while r.in_flight() != 0 {
                    assert!(std::time::Instant::now() < until);
                    r.poll_budgeted(1).unwrap();
                    r.wait(std::time::Duration::from_millis(1)).unwrap();
                }
            }
            turn.cancel().unwrap();
            drop(write);
            testing::drive(&r, r.file_fence(turn.request)).unwrap();
            let current = testing::drive(
                &r,
                Box::pin(read_at(&r, &dir, "state", new.len(), true, &scope)),
            )
            .unwrap();
            assert!(
                *current == old || *current == new,
                "partial replacement at boundary {boundary}"
            );
            // Reusing the deterministic staging name after the fence cannot be
            // overwritten by a late operation from the canceled transaction.
            testing::drive(
                &r,
                Box::pin(atomic_write(&r, &dir, "state", b"retry", &scope)),
            )
            .unwrap();
            assert_eq!(
                &*testing::drive(&r, Box::pin(read_at(&r, &dir, "state", 10, true, &scope)))
                    .unwrap(),
                b"retry"
            );
        }
        assert_eq!(r.in_flight(), 0);
    }
    #[test]
    fn real_ring_enrollment_durability_projection_and_abandoned_retry() {
        let Some(r) = testing::reactor() else {
            return;
        };
        let d = testing::Directory::new();
        let scope = testing::scope();
        let e = Enrollment::new(
            crate::model::identity::ClusterId("11111111-1111-4111-8111-111111111111".into()),
            d.0.join("token"),
            d.0.join("identity"),
        );
        e.attach_reactor(r.clone());
        let mut abandoned = e.prepare(&scope);
        let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
        assert!(abandoned.as_mut().poll(&mut cx).is_pending());
        // Merely polling cannot perform the open/mkdir. No hidden executor drives it.
        for _ in 0..10 {
            assert!(abandoned.as_mut().poll(&mut cx).is_pending());
        }
        assert!(!d.0.join("identity").exists());
        assert_eq!(r.in_flight(), 1);
        drop(abandoned);
        let pending = testing::drive(&r, e.prepare(&scope)).unwrap();
        assert_eq!(
            pending.csr_der,
            testing::drive(&r, e.prepare(&scope)).unwrap().csr_der
        );
        let (ca, key) = testing::ca();
        e.set_peer_trust_roots(vec![ca.der().to_vec()]).unwrap();
        let response = testing::issue(&pending, &ca, &key, "22222222-2222-4222-8222-222222222222");
        let identity = testing::drive(&r, e.accept_response_async(response, &scope)).unwrap();
        assert_eq!(
            identity.node(),
            testing::drive(&r, e.load_identity_async(&scope))
                .unwrap()
                .unwrap()
                .node()
        );
        assert!(!d.0.join("identity/pending.json").exists());
        let bytes = vec![42; 100_003];
        testing::drive(
            &r,
            Box::pin(async {
                let dir = directory(&r, &d.0.join("identity"), false, true, &scope).await?;
                atomic_write(&r, &dir, "large", &bytes, &scope).await?;
                assert_eq!(
                    &*read_at(&r, &dir, "large", bytes.len(), true, &scope).await?,
                    &bytes
                );
                Ok(())
            }),
        )
        .unwrap();
        std::fs::create_dir(d.0.join("epoch-a")).unwrap();
        std::fs::write(d.0.join("epoch-a/bundle.json"), b"old").unwrap();
        std::fs::create_dir(d.0.join("epoch-b")).unwrap();
        std::fs::write(d.0.join("epoch-b/bundle.json"), b"new").unwrap();
        std::os::unix::fs::symlink("epoch-a", d.0.join("..data")).unwrap();
        testing::drive(
            &r,
            Box::pin(async {
                let dir = directory(&r, &d.0, false, false, &scope).await?;
                let generation = r
                    .file_open(
                        Some(dir),
                        CString::new("..data").unwrap(),
                        libc::O_RDONLY | libc::O_DIRECTORY,
                        BENEATH | NO_MAGICLINKS,
                        &scope,
                    )
                    .await?;
                std::os::unix::fs::symlink("epoch-b", d.0.join("..next")).unwrap();
                std::fs::rename(d.0.join("..next"), d.0.join("..data")).unwrap();
                assert_eq!(
                    &*read_at(&r, &generation, "bundle.json", 3, false, &scope).await?,
                    b"old"
                );
                assert_eq!(
                    &*projected_file(&r, &d.0, "bundle.json", 3, &scope).await?,
                    b"new"
                );
                Ok(())
            }),
        )
        .unwrap();
        std::fs::remove_file(d.0.join("..data")).unwrap();
        std::os::unix::fs::symlink("../", d.0.join("..data")).unwrap();
        assert!(
            testing::drive(
                &r,
                Box::pin(projected_file(&r, &d.0, "bundle.json", 3, &scope))
            )
            .is_err()
        );
        assert_eq!(r.in_flight(), 0);
    }
}
