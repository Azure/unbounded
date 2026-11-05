//! The reactor backend that executes ring operations against a [`SimOs`].
use super::*;
use std::{ffi::CString, mem::ManuallyDrop, os::unix::ffi::OsStrExt, time::Instant};

const CANCEL_BIT: u64 = 1 << 63;

/// Executes submissions in turns and emits independent cancel fences.
pub(crate) struct SimDriver {
    os: SimOs,

    pending: BTreeMap<u64, Pending>,

    completed: VecDeque<(u64, i32, u32)>,

    capacity: usize,

    /// World event sequence observed at the end of the last turn.
    seen: u64,

    /// Operations were pushed since the last turn.
    dirty: bool,

    /// The owning reactor's clock, for cost-model latency.
    clock: crate::Clock,
}

/// One operation waiting for its turn.
struct Pending {
    op: Op,

    /// Owned copies of pathname arguments, read at push time.
    paths: [Option<CString>; 2],

    delay: usize,

    limit: usize,

    error: Option<i32>,

    hold: usize,

    result: Option<i32>,

    /// Cost-model latency: the operation may not execute before this.
    ready_at: Option<Instant>,

    /// Execution was attempted and is waiting for readiness (`EAGAIN`).
    /// Until then the operation still needs a turn of its own once its
    /// delay runs out, even when nothing else in the world changes.
    attempted: bool,

    disk_crash: Rc<disk::PendingCrash>,
}

impl SimDriver {
    /// A driver over `os` with room for `capacity` queued submissions.
    pub(crate) fn new(os: SimOs, capacity: usize, clock: crate::Clock) -> Self {
        Self {
            os,
            pending: BTreeMap::new(),
            completed: VecDeque::new(),
            capacity,
            seen: u64::MAX,
            dirty: false,
            clock,
        }
    }

    /// The simulated OS.
    pub(crate) fn os(&self) -> &SimOs {
        &self.os
    }

    /// Publish one operation, capturing its fault and affected disk paths.
    pub(crate) fn push(&mut self, user_data: u64, op: &Op) -> Result<(), ()> {
        if let Op::Cancel { target } = *op {
            self.cancel(target);
            return Ok(());
        }
        if self.pending.len() >= self.capacity {
            return Err(());
        }
        {
            let mut w = self.os.0.borrow_mut();
            if w.reject_submissions != 0 {
                w.reject_submissions -= 1;
                w.record("reject:sq", user_data, 0);
                return Err(());
            }
        }
        // SAFETY: path pointers reference NUL-terminated bytes owned by the slot.
        let read = |ptr: *const libc::c_char| unsafe { CStr::from_ptr(ptr) }.to_owned();
        let paths = match *op {
            Op::OpenAt2 { path, .. } | Op::UnlinkAt { path, .. } | Op::MkdirAt { path, .. } => {
                [Some(read(path)), None]
            }
            Op::RenameAt { from, to, .. } => [Some(read(from)), Some(read(to))],
            _ => [None, None],
        };
        let name = op.name();
        let fault = self.os.0.borrow_mut().fault(name);
        let disk_paths = self.disk_paths(op, &paths);
        let cost = self.os.op_cost(&OpDesc {
            kind: name,
            bytes: op_bytes(op),
        });
        let ready_at = (!cost.is_zero()).then(|| self.clock.now() + cost);
        let mut pending = Pending {
            disk_crash: self.os.0.borrow_mut().disk.watch(disk_paths),
            op: *op,
            paths,
            delay: 0,
            limit: usize::MAX,
            error: None,
            hold: 0,
            result: None,
            ready_at,
            attempted: false,
        };
        match fault {
            Some(Fault::Errno(n)) => pending.error = Some(n),
            Some(Fault::Short(n)) => pending.limit = n,
            Some(Fault::Delay(n)) => pending.delay = n,
            Some(Fault::HoldCompletion(n)) => pending.hold = n,
            None => (),
        }
        let mut w = self.os.0.borrow_mut();
        if let Some(choices) = &mut w.choices {
            pending.delay += crate::ctx::below(choices.as_mut(), 3) as usize;
        }
        w.record(&format!("submit:{name}"), user_data, 0);
        drop(w);
        self.pending.insert(user_data, pending);
        self.dirty = true;
        Ok(())
    }

    /// Cancel unexecuted work, preserving held results and both CQE fences.
    fn cancel(&mut self, id: u64) {
        let removed = self.pending.get(&id).is_some_and(|p| p.result.is_none());
        if removed {
            self.pending.remove(&id);
        }
        let cancel = (id | CANCEL_BIT, if removed { 0 } else { -libc::ENOENT }, 0);
        let mut w = self.os.0.borrow_mut();
        let cancel_first = match &mut w.choices {
            Some(choices) => crate::ctx::below(choices.as_mut(), 2) == 0,
            None => w.cancel_first,
        };
        if removed {
            let original = (id, -libc::ECANCELED, 0);
            if cancel_first {
                self.completed.extend([cancel, original]);
            } else {
                self.completed.extend([original, cancel]);
            }
        } else {
            self.completed.push_back(cancel);
        }
        w.record("cancel", id, i64::from(removed));
    }

    /// Whether a turn at `now` could make progress. Operations still
    /// waiting out their cost-model latency do not count.
    pub(crate) fn runnable(&self, now: Instant) -> bool {
        let mut due = self.pending.values().filter(|p| p.due(now)).peekable();
        if due.peek().is_none() {
            return false;
        }
        self.dirty
            || self.seen != self.os.0.borrow().next_event
            || due.any(|p| p.delay != 0 || p.result.is_some() || !p.attempted)
    }

    /// The earliest instant a latency-gated operation becomes due.
    pub(crate) fn next_ready_at(&self) -> Option<Instant> {
        self.pending.values().filter_map(|p| p.ready_at).min()
    }

    /// Advance every pending operation by one turn, in identifier order or,
    /// for a seeded world, in a seeded shuffled order.
    pub(crate) fn run_turn(&mut self, now: Instant) {
        if !self.runnable(now) {
            return;
        }
        self.dirty = false;
        // Remember the world before this turn: any change made by the turn
        // itself (a send that unblocks an earlier receive) earns another turn.
        self.seen = self.os.0.borrow().next_event;
        let mut ids: Vec<u64> = self.pending.keys().copied().collect();
        if let Some(choices) = &mut self.os.0.borrow_mut().choices {
            // Fisher-Yates where a zero draw keeps the element in place.
            for index in (1..ids.len()).rev() {
                let back = crate::ctx::below(choices.as_mut(), index as u64 + 1) as usize;
                ids.swap(index, index - back);
            }
        }
        let mut done = Vec::new();
        for id in ids {
            let mut pending = self.pending.remove(&id).expect("pending op");
            if !pending.due(now) {
                self.pending.insert(id, pending);
                continue;
            }
            pending.ready_at = None;
            match pending.advance(&self.os, id) {
                Some(result) => {
                    self.completed.push_back((id, result, 0));
                    done.push(pending);
                }
                None => {
                    self.pending.insert(id, pending);
                }
            }
        }
        // Keep descriptor references alive until every operation had its turn.
        drop(done);
    }

    /// Pop the oldest completion.
    pub(crate) fn next_cqe(&mut self) -> Option<(u64, i32, u32)> {
        self.completed.pop_front()
    }

    /// Whether completions are queued.
    pub(crate) fn has_cqes(&self) -> bool {
        !self.completed.is_empty()
    }

    /// Storage subtrees whose crash invalidates this submission.
    fn disk_paths(&self, op: &Op, paths: &[Option<CString>; 2]) -> Vec<PathBuf> {
        let w = self.os.0.borrow();
        let file_path = |fd: i32| match w.resources.get(&(fd as u64)) {
            Some(Resource::File { opened_path, .. }) => Some(opened_path.clone()),
            _ => None,
        };
        let dir = |raw: i32| dir_view(&self.os, raw);
        let resolve = |raw: i32, name: &Option<CString>| {
            let dir = dir(raw);
            name.as_ref()
                .and_then(|name| w.path(dir.as_deref(), c_path(name)).ok())
        };
        match *op {
            Op::Read { fd, .. }
            | Op::Write { fd, .. }
            | Op::Readv { fd, .. }
            | Op::Writev { fd, .. }
            | Op::Statx { fd, .. }
            | Op::Fsync { fd, .. } => file_path(fd).into_iter().collect(),
            Op::OpenAt2 { dir: raw, how, .. } => {
                let dir = dir(raw);
                // SAFETY: the open_how lives in the slot scratch.
                let how = unsafe { &*how };
                let (flags, resolve) = open_how(how);
                paths[0]
                    .as_ref()
                    .and_then(|path| {
                        w.open_path(dir.as_deref(), c_path(path), resolve, flags)
                            .ok()
                    })
                    .into_iter()
                    .collect()
            }
            Op::MkdirAt { dir, .. } | Op::UnlinkAt { dir, .. } => {
                resolve(dir, &paths[0]).into_iter().collect()
            }
            Op::RenameAt { dir, .. } => [resolve(dir, &paths[0]), resolve(dir, &paths[1])]
                .into_iter()
                .flatten()
                .collect(),
            _ => Vec::new(),
        }
    }
}

/// A non-closing descriptor view of directory `raw` (`AT_FDCWD` is `None`).
fn dir_view(os: &SimOs, raw: i32) -> Option<ManuallyDrop<Descriptor>> {
    (raw != libc::AT_FDCWD).then(|| ManuallyDrop::new(Descriptor::sim(os.clone(), raw)))
}

/// Read the flags and resolve policy from an `open_how`.
fn open_how(how: &io_uring::types::OpenHow) -> (i32, u64) {
    // SAFETY: OpenHow is repr(transparent) over Linux's open_how
    // { u64 flags; u64 mode; u64 resolve }.
    let raw = unsafe { &*(how as *const io_uring::types::OpenHow).cast::<[u64; 3]>() };
    (raw[0] as i32, raw[2])
}

/// Decode an encoded socket address.
fn decode_address(addr: *const libc::sockaddr, len: libc::socklen_t) -> io::Result<SocketAddress> {
    // SAFETY: the reactor encoded at most sockaddr_storage bytes at addr.
    let family = unsafe { (*addr).sa_family } as i32;
    match family {
        libc::AF_INET => {
            // SAFETY: an AF_INET address is a sockaddr_in.
            let a = unsafe { &*addr.cast::<libc::sockaddr_in>() };
            let ip = std::net::Ipv4Addr::from(a.sin_addr.s_addr.to_ne_bytes());
            Ok(SocketAddress::Inet((ip, u16::from_be(a.sin_port)).into()))
        }
        libc::AF_INET6 => {
            // SAFETY: an AF_INET6 address is a sockaddr_in6.
            let a = unsafe { &*addr.cast::<libc::sockaddr_in6>() };
            let ip = std::net::Ipv6Addr::from(a.sin6_addr.s6_addr);
            Ok(SocketAddress::Inet(
                std::net::SocketAddrV6::new(
                    ip,
                    u16::from_be(a.sin6_port),
                    u32::from_be(a.sin6_flowinfo),
                    a.sin6_scope_id,
                )
                .into(),
            ))
        }
        libc::AF_UNIX => {
            // SAFETY: an AF_UNIX address is a sockaddr_un of `len` bytes.
            let a = unsafe { &*addr.cast::<libc::sockaddr_un>() };
            let max = (len as usize)
                .saturating_sub(std::mem::offset_of!(libc::sockaddr_un, sun_path))
                .min(a.sun_path.len());
            let bytes: Vec<u8> = a.sun_path[..max]
                .iter()
                .map(|c| *c as u8)
                .take_while(|c| *c != 0)
                .collect();
            Ok(SocketAddress::Unix(PathBuf::from(
                std::ffi::OsStr::from_bytes(&bytes),
            )))
        }
        _ => Err(errno(libc::EAFNOSUPPORT)),
    }
}

/// Bytes an operation asks to transfer, for the cost model.
fn op_bytes(op: &Op) -> u64 {
    // SAFETY: iovec arrays and message headers are pinned by the slot.
    let iovecs = |iov: *const libc::iovec, n: u32| unsafe {
        std::slice::from_raw_parts(iov, n as usize)
            .iter()
            .map(|iov| iov.iov_len as u64)
            .sum()
    };
    match *op {
        Op::Read { len, .. }
        | Op::Write { len, .. }
        | Op::Recv { len, .. }
        | Op::Send { len, .. }
        | Op::Splice { len, .. } => u64::from(len),
        Op::Readv { iov, count, .. } | Op::Writev { iov, count, .. } => iovecs(iov, count),
        // SAFETY: as above.
        Op::RecvMsg { msg, .. } => unsafe { iovecs((*msg).msg_iov, (*msg).msg_iovlen as u32) },
        // SAFETY: as above.
        Op::SendMsg { msg, .. } => unsafe { iovecs((*msg).msg_iov, (*msg).msg_iovlen as u32) },
        _ => 0,
    }
}

impl Pending {
    /// Whether the operation's cost-model latency has elapsed at `now`.
    fn due(&self, now: Instant) -> bool {
        self.ready_at.is_none_or(|at| at <= now)
    }

    /// Execute once when ready, then hold the result for its full delay.
    fn advance(&mut self, os: &SimOs, id: u64) -> Option<i32> {
        if self.result.is_some() {
            if self.hold != 0 {
                self.hold -= 1;
                return None;
            }
            return self.result.take();
        }
        if self.delay != 0 {
            self.delay -= 1;
            return None;
        }
        os.0.borrow_mut().executing = true;
        let injected = self.error.take();
        let result = match injected {
            Some(n) => Err(errno(n)),
            None if self.disk_crash.crashed.get() => Err(errno(libc::EIO)),
            None => self.execute(os),
        };
        os.0.borrow_mut().executing = false;
        if injected.is_none()
            && self.waits_for_readiness()
            && result
                .as_ref()
                .is_err_and(|error| error.kind() == io::ErrorKind::WouldBlock)
        {
            self.attempted = true;
            return None;
        }
        let result = result.unwrap_or_else(|error| -error.raw_os_error().unwrap_or(libc::EIO));
        os.0.borrow_mut().record(
            &format!("complete:{}", self.op.name()),
            id,
            i64::from(result),
        );
        if self.hold != 0 {
            self.result = Some(result);
            None
        } else {
            Some(result)
        }
    }

    /// Operations for which `EAGAIN` means wait for another turn.
    fn waits_for_readiness(&self) -> bool {
        matches!(
            self.op,
            Op::Recv { .. }
                | Op::Send { .. }
                | Op::RecvMsg { .. }
                | Op::SendMsg { .. }
                | Op::Poll { .. }
                | Op::Accept { .. }
                | Op::Connect { .. }
                | Op::Splice { .. }
        )
    }

    /// Perform one attempt using memory owned by the reactor slot.
    fn execute(&self, os: &SimOs) -> io::Result<i32> {
        let limit = self.limit.min(i32::MAX as usize);
        let handle = |raw: i32| -> io::Result<Handle> {
            if os.0.borrow().resources.contains_key(&(raw as u64)) {
                Ok(os.view(raw))
            } else {
                Err(errno(libc::EBADF))
            }
        };
        let count = |n: usize| Ok(n as i32);
        match self.op {
            Op::Read {
                fd,
                ptr,
                len,
                offset,
            } => {
                let fd = handle(fd)?;
                let (_, flags) = fd.node()?;
                check_direct(flags, offset, ptr, len as usize)?;
                // SAFETY: the slot pins this exclusive buffer through both fences.
                let bytes =
                    unsafe { std::slice::from_raw_parts_mut(ptr, (len as usize).min(limit)) };
                count(fd.file_read(offset, bytes)?)
            }
            Op::Write {
                fd,
                ptr,
                len,
                offset,
            } => {
                let fd = handle(fd)?;
                let (_, flags) = fd.node()?;
                check_direct(flags, offset, ptr.cast_mut(), len as usize)?;
                // SAFETY: the slot pins this buffer through both fences.
                let bytes = unsafe { std::slice::from_raw_parts(ptr, (len as usize).min(limit)) };
                count(fd.file_write(offset, bytes)?)
            }
            Op::Readv {
                fd,
                iov,
                count: n,
                offset,
            } => {
                let fd = handle(fd)?;
                let (_, flags) = fd.node()?;
                // SAFETY: the slot pins the iovec array and its segments.
                let iovecs = unsafe { std::slice::from_raw_parts(iov, n as usize) };
                for iov in iovecs {
                    check_direct(flags, offset, iov.iov_base.cast(), iov.iov_len)?;
                }
                let total: usize = iovecs.iter().map(|iov| iov.iov_len).sum();
                let mut gathered = vec![0; total.min(limit)];
                let read = fd.file_read(offset, &mut gathered)?;
                let mut copied = 0;
                for iov in iovecs {
                    let take = iov.iov_len.min(read - copied);
                    // SAFETY: as above; receive segments are exclusive.
                    unsafe {
                        std::ptr::copy_nonoverlapping(
                            gathered[copied..].as_ptr(),
                            iov.iov_base.cast::<u8>(),
                            take,
                        )
                    };
                    copied += take;
                }
                count(read)
            }
            Op::Writev {
                fd,
                iov,
                count: n,
                offset,
            } => {
                let fd = handle(fd)?;
                let (_, flags) = fd.node()?;
                // SAFETY: the slot pins the iovec array and its segments.
                let iovecs = unsafe { std::slice::from_raw_parts(iov, n as usize) };
                for iov in iovecs {
                    check_direct(flags, offset, iov.iov_base.cast(), iov.iov_len)?;
                }
                let mut remaining = limit;
                let mut gathered = Vec::new();
                for iov in iovecs {
                    let len = iov.iov_len.min(remaining);
                    // SAFETY: as above; writes only read their segments.
                    gathered.extend_from_slice(unsafe {
                        std::slice::from_raw_parts(iov.iov_base.cast::<u8>(), len)
                    });
                    remaining -= len;
                }
                count(fd.file_write(offset, &gathered)?)
            }
            Op::Recv { fd, ptr, len } => {
                // SAFETY: the slot pins this exclusive buffer.
                let bytes =
                    unsafe { std::slice::from_raw_parts_mut(ptr, (len as usize).min(limit)) };
                count(handle(fd)?.recv(bytes)?)
            }
            Op::Send { fd, ptr, len } => {
                // SAFETY: the slot pins this buffer.
                let bytes = unsafe { std::slice::from_raw_parts(ptr, (len as usize).min(limit)) };
                count(handle(fd)?.send(bytes)?)
            }
            Op::RecvMsg { fd, msg } => {
                // SAFETY: the slot pins the header, its iovec, and its buffer.
                let msg = unsafe { &mut *msg };
                let iov = unsafe { &*msg.msg_iov };
                let bytes = unsafe {
                    std::slice::from_raw_parts_mut(
                        iov.iov_base.cast::<u8>(),
                        iov.iov_len.min(limit),
                    )
                };
                let n = handle(fd)?.recv(bytes)?;
                msg.msg_controllen = 0;
                msg.msg_flags = 0;
                count(n)
            }
            Op::SendMsg { fd, msg } => {
                // SAFETY: the slot pins the header, its iovec, and its buffer.
                let msg = unsafe { &*msg };
                if msg.msg_controllen != 0 {
                    return Err(errno(libc::EOPNOTSUPP));
                }
                let iov = unsafe { &*msg.msg_iov };
                let bytes = unsafe {
                    std::slice::from_raw_parts(iov.iov_base.cast::<u8>(), iov.iov_len.min(limit))
                };
                count(handle(fd)?.send(bytes)?)
            }
            Op::Accept { fd } => Ok(handle(fd)?.accept()?.into_raw()),
            Op::Connect { fd, addr, len } => {
                let address = decode_address(addr, len)?;
                handle(fd)?.connect(&address)?;
                Ok(0)
            }
            Op::Poll { fd, mask, .. } => handle(fd)?.ready(mask),
            Op::Close { fd } => {
                handle(fd)?;
                os.close(fd);
                Ok(0)
            }
            Op::Fsync { fd, .. } => {
                handle(fd)?.sync()?;
                Ok(0)
            }
            Op::OpenAt2 { dir, how, .. } => {
                let dir = dir_view(os, dir);
                // SAFETY: the open_how lives in the slot scratch.
                let (flags, resolve) = open_how(unsafe { &*how });
                let path = self.paths[0].as_ref().expect("open path");
                Ok(os
                    .open_resolved(dir.as_deref(), c_path(path), flags, resolve)?
                    .into_raw())
            }
            Op::Statx { fd, out } => {
                let stat = handle(fd)?.stat()?;
                // SAFETY: the slot pins the statx output.
                unsafe { *out = stat };
                Ok(0)
            }
            Op::MkdirAt { dir, .. } => {
                let view = dir_view(os, dir);
                handle(dir)?;
                let path =
                    os.0.borrow()
                        .path(view.as_deref(), c_path(self.paths[0].as_ref().unwrap()))?;
                if os.0.borrow().paths.contains_key(&path) {
                    return Err(errno(libc::EEXIST));
                }
                os.mkdir(&path)?;
                Ok(0)
            }
            Op::RenameAt { dir, flags, .. } => {
                let view = dir_view(os, dir);
                handle(dir)?;
                let a =
                    os.0.borrow()
                        .path(view.as_deref(), c_path(self.paths[0].as_ref().unwrap()))?;
                let b =
                    os.0.borrow()
                        .path(view.as_deref(), c_path(self.paths[1].as_ref().unwrap()))?;
                os.rename(&a, &b, flags)?;
                Ok(0)
            }
            Op::UnlinkAt { dir, .. } => {
                let view = dir_view(os, dir);
                handle(dir)?;
                let path =
                    os.0.borrow()
                        .path(view.as_deref(), c_path(self.paths[0].as_ref().unwrap()))?;
                os.unlink(&path)?;
                Ok(0)
            }
            Op::Splice {
                fd_in, fd_out, len, ..
            } => {
                let pipe = handle(fd_in)?;
                let socket = handle(fd_out)?;
                count(pipe.splice(&socket, (len as usize).min(limit))?)
            }
            Op::Cancel { .. } => Err(errno(libc::EINVAL)),
        }
    }
}
