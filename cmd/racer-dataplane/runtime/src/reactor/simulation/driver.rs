//! Submission scheduling and completion fences, independent of application state.
use super::*;
pub(in crate::reactor) enum Op {
    Buffer {
        fd: Rc<Descriptor>,
        operation: BufferOperation,
        ptr: *mut u8,
        len: usize,
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
impl Op {
    fn disk_paths(&self, sim: &Simulation) -> Vec<PathBuf> {
        let w = sim.0.borrow();
        let file_path = |fd: &Descriptor| match fd {
            fd if fd.as_sim().is_some() => match w.resources.get(&fd.as_sim().unwrap().id) {
                Some(Resource::File { opened_path, .. }) => Some(opened_path.clone()),
                _ => None,
            },
            _ => None,
        };
        let path = |dir: Option<&Descriptor>, name: &CString| {
            use std::os::unix::ffi::OsStrExt;
            w.path(dir, Path::new(std::ffi::OsStr::from_bytes(name.as_bytes())))
                .ok()
        };
        match self {
            Self::Buffer {
                fd,
                operation: BufferOperation::Read(_) | BufferOperation::Write(_),
                ..
            }
            | Self::Stat { fd, .. }
            | Self::Sync(fd) => file_path(fd).into_iter().collect(),
            Self::Open {
                dir,
                path: name,
                resolve,
                flags,
            } => {
                use std::os::unix::ffi::OsStrExt;
                w.open_path(
                    dir.as_deref(),
                    Path::new(std::ffi::OsStr::from_bytes(name.as_bytes())),
                    *resolve,
                    *flags,
                )
                .ok()
                .into_iter()
                .collect()
            }
            Self::Mkdir { dir, name } | Self::Unlink { dir, name } => {
                path(Some(dir), name).into_iter().collect()
            }
            Self::Rename { dir, from, to } => [path(Some(dir), from), path(Some(dir), to)]
                .into_iter()
                .flatten()
                .collect(),
            _ => Vec::new(),
        }
    }
    fn name(&self) -> &'static str {
        match self {
            Self::Buffer { operation, .. } => match operation {
                BufferOperation::Read(_) => "read",
                BufferOperation::Write(_) => "write",
                BufferOperation::Recv => "recv",
                BufferOperation::Send => "send",
            },
            Self::Poll { .. } => "poll",
            Self::Accept(_) => "accept",
            Self::Connect { .. } => "connect",
            Self::Open { .. } => "open",
            Self::Stat { .. } => "stat",
            Self::Sync(_) => "fsync",
            Self::Mkdir { .. } => "mkdir",
            Self::Rename { .. } => "rename",
            Self::Unlink { .. } => "unlink",
        }
    }
    fn execute(&self, sim: &Simulation, limit: usize) -> io::Result<KernelResult> {
        use std::os::unix::ffi::OsStrExt;
        let path = |name: &CString| PathBuf::from(std::ffi::OsStr::from_bytes(name.as_bytes()));
        let handle = |fd: &Rc<Descriptor>| match &**fd {
            fd if fd.as_sim().is_some_and(|h| Rc::ptr_eq(&h.sim.0, &sim.0)) => {
                Ok(fd.as_sim().unwrap().id)
            }
            _ => Err(errno(libc::EXDEV)),
        };
        // Temporary handles are not owners; only call through references below.
        let h = |fd: &Rc<Descriptor>| {
            handle(fd)?;
            Ok::<_, io::Error>(())
        };
        match self {
            Self::Buffer {
                fd,
                operation,
                ptr,
                len,
            } => {
                h(fd)?;
                let Some(fd) = fd.as_sim() else {
                    unreachable!()
                };
                if let BufferOperation::Read(offset) | BufferOperation::Write(offset) = operation {
                    let (_, flags) = fd.node()?;
                    check_direct(flags, *offset, *ptr, *len)?;
                }
                let len = (*len).min(limit).min(i32::MAX as usize);
                let n = match operation {
                    // SAFETY: receive/read entries own exclusive IoBuffers through
                    // both fences; immutable sends may have shared aliases.
                    BufferOperation::Read(offset) => fd.file_read(*offset, unsafe {
                        std::slice::from_raw_parts_mut(*ptr, len)
                    }),
                    BufferOperation::Recv => {
                        fd.recv(unsafe { std::slice::from_raw_parts_mut(*ptr, len) })
                    }
                    BufferOperation::Write(offset) => {
                        fd.file_write(*offset, unsafe { std::slice::from_raw_parts(*ptr, len) })
                    }
                    BufferOperation::Send => {
                        fd.send(unsafe { std::slice::from_raw_parts(*ptr, len) })
                    }
                }?;
                Ok(KernelResult::Value(n as i32))
            }
            Self::Poll { fd, interest } => {
                h(fd)?;
                let Some(fd) = fd.as_sim() else {
                    unreachable!()
                };
                fd.ready(*interest).map(KernelResult::Value)
            }
            Self::Accept(fd) => {
                h(fd)?;
                let Some(fd) = fd.as_sim() else {
                    unreachable!()
                };
                fd.accept().map(KernelResult::Accepted)
            }
            Self::Connect { fd, address } => {
                h(fd)?;
                let Some(fd) = fd.as_sim() else {
                    unreachable!()
                };
                fd.connect(address)?;
                Ok(KernelResult::Value(0))
            }
            Self::Open {
                dir,
                path: name,
                flags,
                resolve,
            } => sim
                .open_resolved(dir.as_deref(), &path(name), *flags, *resolve)
                .map(KernelResult::Accepted),
            Self::Stat { fd, ptr } => {
                h(fd)?;
                let Some(fd) = fd.as_sim() else {
                    unreachable!()
                };
                unsafe {
                    **ptr = fd.stat()?;
                }
                Ok(KernelResult::Value(0))
            }
            Self::Sync(fd) => {
                h(fd)?;
                let Some(fd) = fd.as_sim() else {
                    unreachable!()
                };
                fd.sync()?;
                Ok(KernelResult::Value(0))
            }
            Self::Mkdir { dir, name } => {
                h(dir)?;
                let path = sim.0.borrow().path(Some(dir), &path(name))?;
                if sim.0.borrow().paths.contains_key(&path) {
                    return Err(errno(libc::EEXIST));
                }
                sim.mkdir(&path)?;
                Ok(KernelResult::Value(0))
            }
            Self::Rename { dir, from, to } => {
                h(dir)?;
                let a = sim.0.borrow().path(Some(dir), &path(from))?;
                let b = sim.0.borrow().path(Some(dir), &path(to))?;
                sim.rename(&a, &b, 0)?;
                Ok(KernelResult::Value(0))
            }
            Self::Unlink { dir, name } => {
                h(dir)?;
                let path = sim.0.borrow().path(Some(dir), &path(name))?;
                sim.unlink(&path)?;
                Ok(KernelResult::Value(0))
            }
        }
    }
}

pub(super) struct Pending {
    op: Op,
    delay: usize,
    limit: usize,
    error: Option<i32>,
    hold: usize,
    result: Option<KernelResult>,
    disk_crash: Rc<disk::PendingCrash>,
}
pub(in crate::reactor) struct Driver {
    sim: Simulation,
    pub(super) pending: RefCell<BTreeMap<u64, Pending>>,
    pub(super) completed: RefCell<VecDeque<(u64, KernelResult)>>,
}
impl Driver {
    pub fn new(sim: Simulation) -> Self {
        Self {
            sim,
            pending: RefCell::default(),
            completed: RefCell::default(),
        }
    }
    pub fn push(&mut self, id: u64, op: Op) -> Result<(), ()> {
        {
            let mut w = self.sim.0.borrow_mut();
            if w.reject_submissions != 0 {
                w.reject_submissions -= 1;
                w.record("reject:sq", id, 0);
                return Err(());
            }
        }
        let name = op.name();
        let fault = self.sim.0.borrow_mut().fault(name);
        let disk_paths = op.disk_paths(&self.sim);
        let mut pending = Pending {
            disk_crash: self.sim.0.borrow_mut().disk.watch(disk_paths),
            op,
            delay: 0,
            limit: usize::MAX,
            error: None,
            hold: 0,
            result: None,
        };
        match fault {
            Some(Fault::Errno(n)) => pending.error = Some(n),
            Some(Fault::Short(n)) => pending.limit = n,
            Some(Fault::Delay(n)) => pending.delay = n,
            Some(Fault::HoldCompletion(n)) => pending.hold = n,
            None => (),
        }
        self.sim
            .0
            .borrow_mut()
            .record(&format!("submit:{name}"), id, 0);
        self.pending.borrow_mut().insert(id, pending);
        Ok(())
    }
    /// Test hook: retire borrowed pointers before supplying an arbitrary original
    /// CQE. Cancellation CQEs leave the original pending until its own fence.
    #[cfg(test)]
    pub fn inject_completion(&mut self, id: u64, result: i32) -> io::Result<()> {
        if self.completed.borrow().len() >= 16384 {
            return Err(errno(libc::ENOSPC));
        }
        if id & CANCEL_BIT == 0 {
            self.pending.borrow_mut().remove(&id);
        }
        self.completed
            .borrow_mut()
            .push_back((id, KernelResult::Value(result)));
        Ok(())
    }
    #[cfg(test)]
    pub fn reorder_completions(&mut self, first: usize, second: usize) -> io::Result<()> {
        let mut completed = self.completed.borrow_mut();
        if first >= completed.len() || second >= completed.len() {
            return Err(errno(libc::EINVAL));
        }
        completed.swap(first, second);
        Ok(())
    }
    pub fn cancel(&mut self, id: u64) {
        let mut pending = self.pending.borrow_mut();
        // An executed operation cannot be canceled retroactively, nor may its
        // held CQE be replaced with ECANCELED and release the owners early.
        let removed = pending.get(&id).is_some_and(|p| p.result.is_none());
        if removed {
            pending.remove(&id);
        }
        let mut completed = self.completed.borrow_mut();
        // Deliberately emit cancel first, exercising the shared two-CQE fence.
        completed.push_back((
            id | CANCEL_BIT,
            KernelResult::Value(if removed { 0 } else { -libc::ENOENT }),
        ));
        if removed {
            completed.push_back((id, KernelResult::Value(-libc::ECANCELED)));
            if !self.sim.0.borrow().cancel_first {
                let len = completed.len();
                completed.swap(len - 2, len - 1);
            }
        }
        self.sim
            .0
            .borrow_mut()
            .record("cancel", id, i64::from(removed));
    }
    pub fn submit(&self) {
        let mut pending = self.pending.borrow_mut();
        let mut done = Vec::new();
        for (&id, operation) in pending.iter_mut() {
            if operation.result.is_some() {
                if operation.hold != 0 {
                    operation.hold -= 1;
                    continue;
                }
                self.completed
                    .borrow_mut()
                    .push_back((id, operation.result.take().unwrap()));
                done.push(id);
                continue;
            }
            if operation.delay != 0 {
                operation.delay -= 1;
                continue;
            }
            self.sim.0.borrow_mut().executing = true;
            let injected = operation.error.take();
            let result = match injected {
                Some(n) => Err(errno(n)),
                None if operation.disk_crash.crashed.get() => Err(errno(libc::EIO)),
                None => operation.op.execute(&self.sim, operation.limit),
            };
            self.sim.0.borrow_mut().executing = false;
            match result {
                Err(error)
                    if injected.is_none()
                        && error.kind() == io::ErrorKind::WouldBlock
                        && matches!(
                            operation.op,
                            Op::Buffer {
                                operation: BufferOperation::Recv | BufferOperation::Send,
                                ..
                            } | Op::Poll { .. }
                                | Op::Accept(_)
                                | Op::Connect { .. }
                        ) => {}
                result => {
                    let result = result.unwrap_or_else(|error| {
                        KernelResult::Value(-error.raw_os_error().unwrap_or(libc::EIO))
                    });
                    let value = match &result {
                        KernelResult::Value(n) => *n as i64,
                        KernelResult::Accepted(fd) if fd.as_sim().is_some() => {
                            fd.as_sim().unwrap().id as i64
                        }
                        _ => unreachable!(),
                    };
                    self.sim.0.borrow_mut().record(
                        &format!("complete:{}", operation.op.name()),
                        id,
                        value,
                    );
                    if operation.hold != 0 {
                        operation.result = Some(result);
                    } else {
                        self.completed.borrow_mut().push_back((id, result));
                        done.push(id);
                    }
                }
            }
        }
        for id in done {
            pending.remove(&id);
        }
    }
    pub fn pop(&mut self) -> Option<(u64, KernelResult)> {
        self.completed.borrow_mut().pop_front()
    }
}
