//! Migration shim: lets the legacy `uring-runtime` reactor drive a [`SimOs`]
//! with its descriptor-owning operation shape. Delete together with
//! `uring-runtime`; new code uses [`crate::Reactor`] with `Driver::Sim`.
use super::*;
use crate::{
    ctx::Clock,
    ring::{fd::encode_address, op::Op},
};
use std::{ffi::CString, os::fd::RawFd};

const CANCEL_BIT: u64 = 1 << 63;

/// One legacy submission. Descriptors stay owned until its original completion.
pub enum LegacyOp {
    Read {
        fd: Rc<Descriptor>,
        ptr: *mut u8,
        len: usize,
        offset: u64,
    },
    Write {
        fd: Rc<Descriptor>,
        ptr: *const u8,
        len: usize,
        offset: u64,
    },
    Recv {
        fd: Rc<Descriptor>,
        ptr: *mut u8,
        len: usize,
    },
    Send {
        fd: Rc<Descriptor>,
        ptr: *const u8,
        len: usize,
    },
    Writev {
        fd: Rc<Descriptor>,
        offset: u64,
        iovecs: *const libc::iovec,
        count: usize,
    },
    Poll {
        fd: Rc<Descriptor>,
        interest: u32,
    },
    Accept(Rc<Descriptor>),
    Connect {
        fd: Rc<Descriptor>,
        address: SocketAddress,
    },
    Open {
        dir: Option<Rc<Descriptor>>,
        path: CString,
        flags: i32,
        resolve: u64,
    },
    Stat {
        fd: Rc<Descriptor>,
        ptr: *mut libc::statx,
    },
    Sync(Rc<Descriptor>),
    Mkdir {
        dir: Rc<Descriptor>,
        name: CString,
    },
    Rename {
        dir: Rc<Descriptor>,
        from: CString,
        to: CString,
    },
    Unlink {
        dir: Rc<Descriptor>,
        name: CString,
    },
}

/// A legacy completion: a value, or a newly opened or accepted descriptor.
#[derive(Debug)]
pub enum LegacyResult {
    Value(i32),
    Accepted(Descriptor),
}

/// Memory an operation's pointers reference until its original completion.
struct Retained {
    _fds: Vec<Rc<Descriptor>>,
    _paths: Vec<CString>,
    _address: Box<libc::sockaddr_storage>,
    _how: Box<io_uring::types::OpenHow>,
    returns_fd: bool,
}

/// The legacy driver: turn-based execution with independent cancel fences.
pub struct LegacyDriver {
    inner: SimDriver,
    os: SimOs,
    clock: Clock,
    retained: BTreeMap<u64, Retained>,
    completed: VecDeque<(u64, i32)>,
}

impl LegacyDriver {
    /// A driver over `os`; `clock` times cost-model latency.
    pub fn new(os: SimOs, clock: Clock) -> Self {
        Self {
            inner: SimDriver::new(os.clone(), usize::MAX, clock.clone()),
            os,
            clock,
            retained: BTreeMap::new(),
            completed: VecDeque::new(),
        }
    }

    /// The simulated OS.
    pub fn os(&self) -> &SimOs {
        &self.os
    }

    /// Publish one operation. `Err` means the submission queue rejected it.
    #[allow(clippy::result_unit_err)]
    pub fn push(&mut self, id: u64, op: LegacyOp) -> std::result::Result<(), ()> {
        let mut fds = Vec::new();
        let mut paths = Vec::new();
        let mut address: Box<libc::sockaddr_storage> = Box::new(
            // SAFETY: all-zero sockaddr storage is valid.
            unsafe { std::mem::zeroed() },
        );
        let mut how = Box::new(io_uring::types::OpenHow::new());
        let mut foreign = false;
        let mut raw = |fd: &Rc<Descriptor>, fds: &mut Vec<Rc<Descriptor>>| -> RawFd {
            if !fd.sim_os().is_some_and(|os| Rc::ptr_eq(&os.0, &self.os.0)) {
                foreign = true;
            }
            fds.push(fd.clone());
            fd.raw()
        };
        let mut returns_fd = false;
        let lowered = match &op {
            LegacyOp::Read {
                fd,
                ptr,
                len,
                offset,
            } => Op::Read {
                fd: raw(fd, &mut fds),
                ptr: *ptr,
                len: (*len).min(u32::MAX as usize) as u32,
                offset: *offset,
            },
            LegacyOp::Write {
                fd,
                ptr,
                len,
                offset,
            } => Op::Write {
                fd: raw(fd, &mut fds),
                ptr: *ptr,
                len: (*len).min(u32::MAX as usize) as u32,
                offset: *offset,
            },
            LegacyOp::Recv { fd, ptr, len } => Op::Recv {
                fd: raw(fd, &mut fds),
                ptr: *ptr,
                len: (*len).min(u32::MAX as usize) as u32,
            },
            LegacyOp::Send { fd, ptr, len } => Op::Send {
                fd: raw(fd, &mut fds),
                ptr: *ptr,
                len: (*len).min(u32::MAX as usize) as u32,
            },
            LegacyOp::Writev {
                fd,
                offset,
                iovecs,
                count,
            } => Op::Writev {
                fd: raw(fd, &mut fds),
                iov: *iovecs,
                count: *count as u32,
                offset: *offset,
            },
            LegacyOp::Poll { fd, interest } => Op::Poll {
                fd: raw(fd, &mut fds),
                mask: *interest,
                multi: false,
            },
            LegacyOp::Accept(fd) => {
                returns_fd = true;
                Op::Accept {
                    fd: raw(fd, &mut fds),
                }
            }
            LegacyOp::Connect { fd, address: to } => {
                let len = encode_address(to, &mut address).map_err(|_| ())?;
                Op::Connect {
                    fd: raw(fd, &mut fds),
                    addr: (&*address as *const libc::sockaddr_storage).cast(),
                    len,
                }
            }
            LegacyOp::Open {
                dir,
                path,
                flags,
                resolve,
            } => {
                returns_fd = true;
                *how = io_uring::types::OpenHow::new()
                    .flags(*flags as u64)
                    .resolve(*resolve);
                Op::OpenAt2 {
                    dir: dir.as_ref().map_or(libc::AT_FDCWD, |d| raw(d, &mut fds)),
                    path: path.as_ptr(),
                    how: &*how,
                }
            }
            LegacyOp::Stat { fd, ptr } => Op::Statx {
                fd: raw(fd, &mut fds),
                out: *ptr,
            },
            LegacyOp::Sync(fd) => Op::Fsync {
                fd: raw(fd, &mut fds),
                datasync: false,
            },
            LegacyOp::Mkdir { dir, name } => Op::MkdirAt {
                dir: raw(dir, &mut fds),
                path: name.as_ptr(),
                mode: 0o700,
            },
            LegacyOp::Rename { dir, from, to } => Op::RenameAt {
                dir: raw(dir, &mut fds),
                from: from.as_ptr(),
                to: to.as_ptr(),
                flags: 0,
            },
            LegacyOp::Unlink { dir, name } => Op::UnlinkAt {
                dir: raw(dir, &mut fds),
                path: name.as_ptr(),
                flags: 0,
            },
        };
        match op {
            LegacyOp::Open { path, .. } => paths.push(path),
            LegacyOp::Mkdir { name, .. } | LegacyOp::Unlink { name, .. } => paths.push(name),
            LegacyOp::Rename { from, to, .. } => paths.extend([from, to]),
            _ => (),
        }
        if foreign {
            // A descriptor of another world: fail like a cross-device call.
            self.completed.push_back((id, -libc::EXDEV));
        } else {
            self.inner.push(id, &lowered)?;
        }
        self.retained.insert(
            id,
            Retained {
                _fds: fds,
                _paths: paths,
                _address: address,
                _how: how,
                returns_fd,
            },
        );
        Ok(())
    }

    /// Request cancellation; both the original and cancel CQEs follow.
    pub fn cancel(&mut self, id: u64) {
        let _ = self.inner.push(0, &Op::Cancel { target: id });
        self.drain();
    }

    /// Advance every pending operation by one turn.
    pub fn submit(&mut self) {
        self.inner.run_turn(self.clock.now());
        self.drain();
    }

    /// Pop the oldest completion without advancing execution.
    pub fn pop(&mut self) -> Option<(u64, LegacyResult)> {
        self.drain();
        let (id, value) = self.completed.pop_front()?;
        if id & CANCEL_BIT != 0 {
            return Some((id, LegacyResult::Value(value)));
        }
        let returns_fd = self.retained.remove(&id).is_some_and(|r| r.returns_fd);
        if returns_fd && value >= 0 {
            return Some((
                id,
                LegacyResult::Accepted(Descriptor::sim(self.os.clone(), value)),
            ));
        }
        Some((id, LegacyResult::Value(value)))
    }

    /// Test support: retire an original before supplying an arbitrary CQE.
    pub fn inject_completion(&mut self, id: u64, result: i32) -> io::Result<()> {
        self.drain();
        if self.completed.len() >= 16384 {
            return Err(errno(libc::ENOSPC));
        }
        if id & CANCEL_BIT == 0 {
            self.inner.forget(id);
        }
        self.completed.push_back((id, result));
        Ok(())
    }

    /// Test support: swap two queued completions.
    pub fn reorder_completions(&mut self, first: usize, second: usize) -> io::Result<()> {
        self.drain();
        if first >= self.completed.len() || second >= self.completed.len() {
            return Err(errno(libc::EINVAL));
        }
        self.completed.swap(first, second);
        Ok(())
    }

    /// Move the inner driver's completions into the legacy queue.
    fn drain(&mut self) {
        while let Some((id, value, _)) = self.inner.next_cqe() {
            self.completed.push_back((id, value));
        }
    }
}
