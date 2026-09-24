// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Raw Linux queue ABI and mapping ownership. Submitted pointer lifetimes
//! remain the typed ring owner's responsibility.
use std::{
    io,
    marker::PhantomData,
    os::fd::{AsRawFd, FromRawFd, OwnedFd, RawFd},
    ptr::NonNull,
    rc::Rc,
    sync::atomic::{AtomicU32, Ordering},
    time::Duration,
};
fn invalid(message: &'static str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidInput, message)
}
pub(crate) mod abi;
struct Mapping {
    ptr: NonNull<u8>,
    len: usize,
}
impl Mapping {
    fn new(fd: RawFd, len: usize, offset: libc::off_t) -> io::Result<Self> {
        if len == 0 || len > isize::MAX as usize {
            return Err(invalid("invalid ring mapping length"));
        }
        // SAFETY: new shared mapping of the ring's kernel-provided region.
        let ptr = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                len,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_SHARED | libc::MAP_POPULATE,
                fd,
                offset,
            )
        };
        if ptr == libc::MAP_FAILED {
            return Err(io::Error::last_os_error());
        }
        let Some(ptr) = NonNull::new(ptr.cast()) else {
            // SAFETY: release the successful but unusable null mapping.
            unsafe {
                libc::munmap(ptr, len);
            }
            return Err(io::Error::other("null ring mapping"));
        };
        Ok(Self { ptr, len })
    }
    fn at<T>(&self, offset: u32, count: u32) -> io::Result<NonNull<T>> {
        let offset = offset as usize;
        let len = (count as usize)
            .checked_mul(size_of::<T>())
            .and_then(|len| offset.checked_add(len))
            .ok_or_else(|| invalid("ring layout overflow"))?;
        if len > self.len || !offset.is_multiple_of(align_of::<T>()) {
            return Err(invalid("invalid kernel ring layout"));
        }
        // SAFETY: checked range and alignment, mmap base is page aligned.
        Ok(unsafe { NonNull::new_unchecked(self.ptr.as_ptr().add(offset).cast()) })
    }
}
impl Drop for Mapping {
    fn drop(&mut self) {
        // SAFETY: exclusive mapping owner; kernel has its own mapping references.
        unsafe {
            libc::munmap(self.ptr.as_ptr().cast(), self.len);
        }
    }
}
pub(crate) struct KernelRing {
    fd: OwnedFd,
    _rings: Mapping,
    _sqes: Mapping,
    sq_head: NonNull<AtomicU32>,
    sq_tail: NonNull<AtomicU32>,
    sq_flags: NonNull<AtomicU32>,
    sq_dropped: NonNull<AtomicU32>,
    cq_head: NonNull<AtomicU32>,
    cq_tail: NonNull<AtomicU32>,
    cq_overflow: NonNull<AtomicU32>,
    array: NonNull<u32>,
    sqes: NonNull<abi::Sqe>,
    cqes: NonNull<abi::Cqe>,
    sq_size: u32,
    cq_size: u32,
    tail: u32,
    head: u32,
    _local: PhantomData<Rc<()>>,
}
fn load(ptr: NonNull<AtomicU32>) -> u32 {
    // SAFETY: callers use validated, live, aligned shared ring fields.
    unsafe { ptr.as_ref().load(Ordering::Acquire) }
}
fn store(ptr: NonNull<AtomicU32>, value: u32) {
    // SAFETY: only this worker writes the userspace-owned index.
    unsafe {
        ptr.as_ref().store(value, Ordering::Release);
    }
}
impl KernelRing {
    pub(crate) fn new(entries: u32) -> io::Result<Self> {
        let mut p = abi::Params {
            // CQSIZE | SUBMIT_ALL | TASKRUN_FLAG | SINGLE_ISSUER | DEFER_TASKRUN
            flags: (1 << 3) | (1 << 7) | (1 << 9) | (1 << 12) | (1 << 13),
            cq_entries: entries
                .checked_mul(2)
                .ok_or_else(|| invalid("ring too large"))?,
            ..Default::default()
        };
        // SAFETY: writable ABI-sized params; no borrowed memory survives setup.
        let fd = unsafe { libc::syscall(libc::SYS_io_uring_setup, entries, &mut p) };
        if fd < 0 {
            return Err(io::Error::last_os_error());
        }
        // SAFETY: setup returned a fresh owned descriptor.
        let fd = unsafe { OwnedFd::from_raw_fd(fd as RawFd) };
        // SINGLE_MMAP | NODROP | FAST_POLL | EXT_ARG
        let required = 1 | 2 | (1 << 5) | (1 << 8);
        if p.features & required != required {
            return Err(io::Error::new(
                io::ErrorKind::Unsupported,
                "io_uring requires SINGLE_MMAP, NODROP, FAST_POLL and EXT_ARG",
            ));
        }
        if !p.sq_entries.is_power_of_two()
            || !p.cq_entries.is_power_of_two()
            || p.sq_entries > 32768
            || p.cq_entries > 65536
        {
            return Err(invalid("invalid kernel queue sizes"));
        }
        let sq_len = p.sq.array_or_flags as usize + p.sq_entries as usize * 4;
        let cq_len = p.cq.dropped_or_cqes as usize + p.cq_entries as usize * size_of::<abi::Cqe>();
        let rings = Mapping::new(fd.as_raw_fd(), sq_len.max(cq_len), 0)?;
        let sqes = Mapping::new(
            fd.as_raw_fd(),
            p.sq_entries as usize * size_of::<abi::Sqe>(),
            0x10000000,
        )?;
        let raw = Self {
            sq_head: rings.at(p.sq.head, 1)?,
            sq_tail: rings.at(p.sq.tail, 1)?,
            sq_flags: rings.at(p.sq.flags_or_overflow, 1)?,
            sq_dropped: rings.at(p.sq.dropped_or_cqes, 1)?,
            cq_head: rings.at(p.cq.head, 1)?,
            cq_tail: rings.at(p.cq.tail, 1)?,
            cq_overflow: rings.at(p.cq.flags_or_overflow, 1)?,
            array: rings.at(p.sq.array_or_flags, p.sq_entries)?,
            cqes: rings.at(p.cq.dropped_or_cqes, p.cq_entries)?,
            sqes: sqes.at(0, p.sq_entries)?,
            sq_size: p.sq_entries,
            cq_size: p.cq_entries,
            tail: 0,
            head: 0,
            fd,
            _rings: rings,
            _sqes: sqes,
            _local: PhantomData,
        };
        // SAFETY: all zero is a valid probe; kernel writes at most 64 entries.
        let mut probe: abi::Probe = unsafe { std::mem::zeroed() };
        raw.register(8, &mut probe as *mut _ as *const libc::c_void, 64)?;
        for op in [
            abi::READ_FIXED,
            abi::WRITE_FIXED,
            abi::POLL,
            abi::ACCEPT,
            abi::CANCEL,
            abi::CONNECT,
            abi::READ,
            abi::SEND,
            abi::RECV,
            abi::SEND_ZC,
        ] {
            if !probe.ops[..usize::from(probe.len).min(64)]
                .iter()
                .any(|entry| entry.op == op && entry.flags & 1 != 0)
            {
                return Err(io::Error::new(
                    io::ErrorKind::Unsupported,
                    format!("missing io_uring opcode {op}"),
                ));
            }
        }
        Ok(raw)
    }
    pub(crate) fn register(&self, op: u32, arg: *const libc::c_void, count: u32) -> io::Result<()> {
        // SAFETY: private callers provide the corresponding live ABI argument.
        let result = unsafe {
            libc::syscall(
                libc::SYS_io_uring_register,
                self.fd.as_raw_fd(),
                op,
                arg,
                count,
            )
        };
        if result < 0 {
            Err(io::Error::last_os_error())
        } else {
            Ok(())
        }
    }
    pub(crate) fn space(&self) -> u32 {
        self.sq_size - self.tail.wrapping_sub(load(self.sq_head))
    }
    pub(crate) fn push(&mut self, sqe: abi::Sqe) {
        assert!(self.space() > 0);
        let index = (self.tail & (self.sq_size - 1)) as usize;
        // SAFETY: free SQ slot, only this worker writes it; tail publication follows.
        unsafe {
            self.sqes.as_ptr().add(index).write(sqe);
            self.array.as_ptr().add(index).write(index as u32);
        }
        self.tail = self.tail.wrapping_add(1);
    }
    pub(crate) fn pending(&self) -> u32 {
        self.tail.wrapping_sub(load(self.sq_head))
    }
    #[cfg(test)]
    pub(crate) fn unpublished(&self, id: u64) -> Option<abi::Sqe> {
        let mut cursor = load(self.sq_tail);
        while cursor != self.tail {
            let index = (cursor & (self.sq_size - 1)) as usize;
            // SAFETY: only this worker owns these not-yet-published SQEs.
            let sqe = unsafe { *self.sqes.as_ptr().add(index) };
            if sqe.user_data == id {
                return Some(sqe);
            }
            cursor = cursor.wrapping_add(1);
        }
        None
    }
    pub(crate) fn discard_unsubmitted(&mut self, id: u64) -> bool {
        // Only touch SQEs whose tail has never been published to the kernel.
        let mut cursor = load(self.sq_tail);
        while cursor != self.tail {
            let index = (cursor & (self.sq_size - 1)) as usize;
            // SAFETY: unpublished slots are exclusively owned by this worker.
            let sqe = unsafe { &mut *self.sqes.as_ptr().add(index) };
            if sqe.user_data == id {
                // NOP still produces a terminal CQE, preserving resource lifetime.
                *sqe = abi::Sqe {
                    user_data: id,
                    ..Default::default()
                };
                return true;
            }
            cursor = cursor.wrapping_add(1);
        }
        false
    }
    pub(crate) fn ready(&self) -> bool {
        self.head != load(self.cq_tail)
    }
    pub(crate) fn needs_enter(&self) -> bool {
        self.pending() != 0 || load(self.sq_flags) & (2 | 4) != 0
    }
    // Always GETEVENTS, including nonblocking calls, to run deferred task work.
    pub(crate) fn enter(&mut self, wait: bool, timeout: Option<Duration>) -> io::Result<()> {
        store(self.sq_tail, self.tail);
        let ts = timeout.map(|d| abi::Timespec {
            sec: d.as_secs().min(i64::MAX as u64) as i64,
            nsec: d.subsec_nanos() as i64,
        });
        let args = abi::GetEvents {
            ts: ts.as_ref().map_or(0, |ts| ts as *const _ as u64),
            ..Default::default()
        };
        // SAFETY: all arguments live through this synchronous syscall; EXT_ARG
        // timeouts are copied during enter, never retained in an asynchronous SQE.
        let result = unsafe {
            libc::syscall(
                libc::SYS_io_uring_enter,
                self.fd.as_raw_fd(),
                self.pending(),
                u32::from(wait),
                1u32 | 8u32,
                &args,
                size_of::<abi::GetEvents>(),
            )
        };
        if result < 0 {
            let error = io::Error::last_os_error();
            match error.raw_os_error() {
                Some(libc::EINTR | libc::ETIME | libc::EBUSY | libc::EAGAIN) => return Ok(()),
                _ => return Err(error),
            }
        }
        self.check_loss()
    }
    fn check_loss(&self) -> io::Result<()> {
        if load(self.sq_dropped) != 0 || load(self.cq_overflow) != 0 {
            Err(io::Error::other(
                "io_uring lost an SQE/CQE; resources must remain pinned",
            ))
        } else {
            Ok(())
        }
    }
    pub(crate) fn reap(&mut self, output: &mut Vec<abi::Cqe>, budget: usize) -> io::Result<()> {
        self.check_loss()?;
        let count = load(self.cq_tail).wrapping_sub(self.head) as usize;
        if count > self.cq_size as usize {
            return Err(io::Error::other("invalid CQ occupancy"));
        }
        for _ in 0..count.min(budget) {
            let index = (self.head & (self.cq_size - 1)) as usize;
            // SAFETY: acquire observed kernel publication; copying before releasing head.
            output.push(unsafe { self.cqes.as_ptr().add(index).read() });
            self.head = self.head.wrapping_add(1);
        }
        store(self.cq_head, self.head);
        Ok(())
    }
}
