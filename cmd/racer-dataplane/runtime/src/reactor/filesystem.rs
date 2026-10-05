//! Filesystem SQEs use the parent reactor's original/cancel completion fences.
//! Every pointer backing, descriptor, and quota lives in the submission closure.
use super::*;
use std::ffi::CString;
use zeroize::Zeroize;
mod operations;
pub use operations::{Durability, Replacement};
mod chunked;
pub use chunked::publish_new;
#[cfg(all(test, feature = "simulation"))]
mod operations_tests;
pub mod secure;
#[cfg(all(test, feature = "simulation"))]
mod secure_tests;

pub struct Buffer {
    data: Vec<u8>,
    start: usize,
    _quota: Charge,
}
#[cfg(test)]
mod partial_tests {
    use super::super::tests::fixtures::{Reactor, ResourceClass};
    use super::super::tests::{drive, kernel_reactor, poll, scope};
    use super::*;
    fn reactor() -> Option<Reactor> {
        kernel_reactor(16)
    }
    #[test]
    fn partial_completion_preserves_remaining_bytes_and_quota() {
        let Some(r) = reactor() else {
            return;
        };
        let baseline = r.admission.used(ResourceClass::RequestContext);
        let mut b = r.file_bytes(b"abcdef").unwrap();
        assert!(r.admission.used(ResourceClass::RequestContext) >= baseline + 6);
        b.advance(2).unwrap();
        assert_eq!(b.bytes().unwrap(), b"cdef");
        assert_eq!(b.advance(0), Err(Error::Io));
        assert_eq!(b.advance(5), Err(Error::Io));
        b.advance(4).unwrap();
        assert_eq!(b.remaining(), 0);
        drop(b);
        assert_eq!(r.admission.used(ResourceClass::RequestContext), baseline);
        let scope = scope();
        let fd = drive(
            &r,
            r.file_open(
                None,
                CString::new("/dev/null").unwrap(),
                libc::O_RDONLY,
                0,
                &scope,
            ),
        )
        .unwrap();
        let completion =
            drive(&r, r.read_at(fd, 0, r.file_buffer(17).unwrap(), (), &scope)).unwrap();
        assert_eq!(completion.bytes, 0);
        assert_eq!(completion.buffer.remaining(), 17);
    }
    #[test]
    fn abandoned_open_and_stat_remain_owned_until_real_cqe_fences() {
        let Some(r) = reactor() else {
            return;
        };
        let scope = scope();
        let baseline = r.admission.used(ResourceClass::RequestContext);
        let mut open = r.file_open(
            None,
            CString::new("/").unwrap(),
            libc::O_RDONLY | libc::O_DIRECTORY,
            0,
            &scope,
        );
        for _ in 0..20 {
            assert!(poll(&mut open).is_pending());
        }
        assert_eq!(r.in_flight(), 1);
        drop(open);
        assert!(r.admission.used(ResourceClass::RequestContext) > baseline);
        let () = scope.request;
        drive(&r, r.file_fence(())).unwrap();
        assert_eq!(r.in_flight(), 0);
        assert_eq!(r.admission.used(ResourceClass::RequestContext), baseline);
        let fd = drive(
            &r,
            r.file_open(
                None,
                CString::new("/").unwrap(),
                libc::O_RDONLY | libc::O_DIRECTORY,
                0,
                &scope,
            ),
        )
        .unwrap();
        let weak = Rc::downgrade(&fd);
        let mut stat = r.file_stat(fd, &scope);
        assert!(poll(&mut stat).is_pending());
        scope.cancel().unwrap();
        drop(stat);
        assert!(weak.upgrade().is_some());
        let () = scope.request;
        drive(&r, r.file_fence(())).unwrap();
        assert!(weak.upgrade().is_none());
        assert_eq!(r.admission.used(ResourceClass::RequestContext), baseline);
    }
}
// SAFETY: private non-resizing Vec retains allocation and quota through completion.
unsafe impl IoBuffer for Buffer {
    type Error = Error;
    fn bytes(&self) -> Result<&[u8]> {
        Ok(&self.data[self.start..])
    }
    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        Ok(&mut self.data[self.start..])
    }
}
impl Drop for Buffer {
    fn drop(&mut self) {
        self.data.as_mut_slice().zeroize();
    }
}
impl Buffer {
    pub fn advance(&mut self, n: usize) -> Result<()> {
        if n == 0 || n > self.data.len() - self.start {
            return Err(Error::Io);
        }
        self.start += n;
        Ok(())
    }
    pub fn remaining(&self) -> usize {
        self.data.len() - self.start
    }
    pub fn prefix(&self, n: usize) -> Result<&[u8]> {
        self.data.get(..n).ok_or(Error::Io)
    }
}
fn value<E: From<Error>>(result: Result<KernelResult, E>) -> Result<i32, E> {
    match result? {
        KernelResult::Value(n) if n >= 0 => Ok(n),
        KernelResult::Value(n) if n == -libc::ENOENT => Err(Error::NotFound.into()),
        KernelResult::Value(n) if n == -libc::EEXIST => Err(Error::AlreadyExists.into()),
        KernelResult::Value(n) if n == -libc::ECANCELED => Err(Error::Cancelled.into()),
        _ => Err(Error::Io.into()),
    }
}
impl<S: Scope, B: Budget> Reactor<S, B> {
    pub fn file_buffer(&self, length: usize) -> Result<Buffer> {
        if length > 1024 * 1024 {
            return Err(Error::Overloaded);
        }
        let quota = self.charge(length)?;
        Ok(Buffer {
            data: vec![0; length],
            start: 0,
            _quota: quota,
        })
    }
    pub fn file_bytes(&self, bytes: &[u8]) -> Result<Buffer> {
        let mut buffer = self.file_buffer(bytes.len())?;
        buffer.data.copy_from_slice(bytes);
        Ok(buffer)
    }
    /// Open results use the same FD-owning CQE variant as accept, including when
    /// cancellation wins after a successful open. No integer FD can leak on drop.
    pub fn file_open<'a>(
        &'a self,
        dir: Option<Rc<Descriptor>>,
        path: CString,
        flags: i32,
        resolve: u64,
        scope: &'a S,
    ) -> Operation<'a, Rc<Descriptor>, S::Error> {
        Box::pin(async move {
            if path.as_bytes().len() > 4096 {
                return Err(Error::InvalidInput.into());
            }
            let quota = self.charge(path.as_bytes().len() + 128)?;
            let how = SyscallArg::new(
                types::OpenHow::new()
                    .flags((flags | libc::O_CLOEXEC | libc::O_NONBLOCK) as u64)
                    .mode(if flags & libc::O_CREAT != 0 { 0o600 } else { 0 })
                    .resolve(resolve),
            );
            let sqe = submission!(
                self,
                simulation::Op::Open {
                    dir: dir.clone(),
                    path: path.clone(),
                    flags,
                    resolve
                },
                opcode::OpenAt2::new(
                    types::Fd(dir.as_ref().map_or(libc::AT_FDCWD, |d| d.as_raw_fd())),
                    path.as_ptr(),
                    how.as_ptr(),
                )
                .build()
                .flags(squeue::Flags::ASYNC)
            );
            self.submit(sqe, scope, true, move |result| {
                drop((dir, path, how, quota));
                match result? {
                    KernelResult::Accepted(fd) => Ok(Rc::new(fd)),
                    result => {
                        value::<S::Error>(Ok(result))?;
                        Err(Error::Io.into())
                    }
                }
            })?
            .await
        })
    }
    pub fn file_stat<'a>(
        &'a self,
        fd: Rc<Descriptor>,
        scope: &'a S,
    ) -> Operation<'a, libc::statx, S::Error> {
        Box::pin(async move {
            let quota = self.charge(std::mem::size_of::<libc::statx>())?;
            let mut stat = SyscallArg::<libc::statx>::new(unsafe { std::mem::zeroed() });
            let sqe = submission!(
                self,
                simulation::Op::Stat {
                    fd: fd.clone(),
                    ptr: stat.as_mut_ptr()
                },
                opcode::Statx::new(
                    types::Fd(fd.as_raw_fd()),
                    c"".as_ptr(),
                    stat.as_mut_ptr().cast(),
                )
                .flags(libc::AT_EMPTY_PATH)
                .mask(libc::STATX_BASIC_STATS)
                .build()
                .flags(squeue::Flags::ASYNC)
            );
            self.submit(sqe, scope, false, move |result| {
                let _quota = quota;
                value(result)?;
                drop(fd);
                Ok(stat.into_inner())
            })?
            .await
        })
    }
    pub fn file_sync<'a>(
        &'a self,
        fd: Rc<Descriptor>,
        scope: &'a S,
    ) -> Operation<'a, (), S::Error> {
        Box::pin(async move {
            let sqe = submission!(
                self,
                simulation::Op::Sync(fd.clone()),
                opcode::Fsync::new(types::Fd(fd.as_raw_fd()))
                    .build()
                    .flags(squeue::Flags::ASYNC)
            );
            self.submit(sqe, scope, false, move |result| {
                drop(fd);
                value(result).map(|_| ())
            })?
            .await
        })
    }
    pub fn file_mkdir<'a>(
        &'a self,
        dir: Rc<Descriptor>,
        name: CString,
        scope: &'a S,
    ) -> Operation<'a, (), S::Error> {
        Box::pin(async move {
            let quota = self.file_path_quota(&[&name])?;
            let sqe = submission!(
                self,
                simulation::Op::Mkdir {
                    dir: dir.clone(),
                    name: name.clone()
                },
                opcode::MkDirAt::new(types::Fd(dir.as_raw_fd()), name.as_ptr())
                    .mode(0o700)
                    .build()
                    .flags(squeue::Flags::ASYNC)
            );
            self.submit(sqe, scope, false, move |result| {
                drop((dir, name, quota));
                value(result).map(|_| ())
            })?
            .await
        })
    }
    pub fn file_rename<'a>(
        &'a self,
        dir: Rc<Descriptor>,
        from: CString,
        to: CString,
        scope: &'a S,
    ) -> Operation<'a, (), S::Error> {
        Box::pin(async move {
            let quota = self.file_path_quota(&[&from, &to])?;
            let sqe = submission!(
                self,
                simulation::Op::Rename {
                    dir: dir.clone(),
                    from: from.clone(),
                    to: to.clone()
                },
                opcode::RenameAt::new(
                    types::Fd(dir.as_raw_fd()),
                    from.as_ptr(),
                    types::Fd(dir.as_raw_fd()),
                    to.as_ptr(),
                )
                .build()
                .flags(squeue::Flags::ASYNC)
            );
            self.submit(sqe, scope, false, move |result| {
                drop((dir, from, to, quota));
                value(result).map(|_| ())
            })?
            .await
        })
    }
    pub fn file_unlink<'a>(
        &'a self,
        dir: Rc<Descriptor>,
        name: CString,
        scope: &'a S,
    ) -> Operation<'a, (), S::Error> {
        Box::pin(async move {
            let quota = self.file_path_quota(&[&name])?;
            let sqe = submission!(
                self,
                simulation::Op::Unlink {
                    dir: dir.clone(),
                    name: name.clone()
                },
                opcode::UnlinkAt::new(types::Fd(dir.as_raw_fd()), name.as_ptr())
                    .build()
                    .flags(squeue::Flags::ASYNC)
            );
            self.submit(sqe, scope, false, move |result| {
                drop((dir, name, quota));
                value(result).map(|_| ())
            })?
            .await
        })
    }
    fn file_path_quota(&self, paths: &[&CString]) -> Result<Charge> {
        if paths.iter().any(|p| p.as_bytes().len() > 4096) {
            return Err(Error::InvalidInput);
        }
        self.charge(paths.iter().map(|p| p.as_bytes_with_nul().len()).sum())
    }
}

#[cfg(test)]
mod buffer_tests {
    use super::super::tests::fixtures::{Reactor, RequestScope, ResourceClass};
    use super::super::tests::{drive, kernel_reactor, poll};
    use super::*;
    fn reactor() -> Option<Reactor> {
        kernel_reactor(16)
    }
    #[test]
    fn real_partial_read_and_abandoned_open_fence_own_resources() {
        let Some(r) = reactor() else {
            return;
        };
        let scope =
            RequestScope::new((), std::time::Instant::now() + Duration::from_secs(10)).unwrap();
        let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("target")
            .join(format!("control-fs-{}", std::process::id()));
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        std::fs::write(&path, b"abc").unwrap();
        let filename = || CString::new(path.as_os_str().as_bytes()).unwrap();
        let baseline = r.admission.used(ResourceClass::RequestContext);
        let mut abandoned = r.file_open(None, filename(), libc::O_RDONLY, 0, &scope);
        assert!(poll(&mut abandoned).is_pending());
        for _ in 0..8 {
            assert!(poll(&mut abandoned).is_pending());
        }
        assert_eq!(r.in_flight(), 1);
        assert!(r.admission.used(ResourceClass::RequestContext) > baseline);
        drop(abandoned);
        // Dropping does not reclaim kernel-owned operation metadata.
        assert!(r.admission.used(ResourceClass::RequestContext) > baseline);
        let () = scope.request;
        drive(&r, r.file_fence(())).unwrap();
        assert_eq!(r.in_flight(), 0);
        assert_eq!(r.admission.used(ResourceClass::RequestContext), baseline);
        let fd = drive(&r, r.file_open(None, filename(), libc::O_RDONLY, 0, &scope)).unwrap();
        let read = drive(
            &r,
            r.read_at(fd.clone(), 0, r.file_buffer(64).unwrap(), (), &scope),
        )
        .unwrap();
        assert_eq!(read.bytes, 3);
        assert_eq!(read.buffer.prefix(3).unwrap(), b"abc");
        drop(read);
        let mut buffer = r.file_bytes(b"abcdef").unwrap();
        buffer.advance(2).unwrap();
        assert_eq!(buffer.bytes().unwrap(), b"cdef");
        assert_eq!(buffer.advance(0), Err(Error::Io));
        assert_eq!(buffer.advance(5), Err(Error::Io));
        drop(buffer);
        let canceled = RequestScope::new((), scope.deadline.0).unwrap();
        let mut stat = r.file_stat(fd.clone(), &canceled);
        assert!(poll(&mut stat).is_pending());
        canceled.cancel().unwrap();
        assert!(matches!(drive(&r, stat), Err(Error::Cancelled)));
        drop(fd);
        std::fs::remove_file(path).unwrap();
        assert_eq!(r.in_flight(), 0);
        assert_eq!(r.admission.used(ResourceClass::RequestContext), baseline);
    }
}
