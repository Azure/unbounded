//! The ublk control device, simulated.
//!
//! The node talks to `/dev/ublk-control` by pushing an 80-byte `uring_cmd` payload and
//! waiting for one answer, so that is what this takes: the bytes, and the command number.
//! Nothing above the seam is decoded for it and nothing below is shared with the runtime's
//! own bindings, because the whole point of this module is to be the other side of that
//! wire. If the two ever disagree about a field offset the encoding tests say so.
//!
//! What is modeled is the part the node's own logic turns on: which minors are taken, who
//! took them, and whether a device has parameters and has been started. On top of that
//! sits the data plane: the fetch a worker parks against a queue, the descriptor the
//! driver writes when a request arrives for it, and the guest pages the request carries.

use std::collections::{BTreeMap, VecDeque};
use std::io;

/// The ioctl-encoded control commands, `UBLK_U_CMD_*` in `include/uapi/linux/ublk_cmd.h`:
/// type `'u'`, a 32-byte `ublksrv_ctrl_cmd`, read or read/write. Spelled out rather than
/// recomputed so that a change to the runtime's encoding has to be made twice, and the
/// second time is a test failure.
pub(crate) const GET_QUEUE_AFFINITY: u32 = 0x8020_7501;
pub(crate) const GET_DEV_INFO: u32 = 0x8020_7502;
pub(crate) const ADD_DEV: u32 = 0xC020_7504;
pub(crate) const DEL_DEV: u32 = 0xC020_7505;
pub(crate) const START_DEV: u32 = 0xC020_7506;
pub(crate) const STOP_DEV: u32 = 0xC020_7507;
pub(crate) const SET_PARAMS: u32 = 0xC020_7508;
pub(crate) const GET_FEATURES: u32 = 0x8020_7513;
pub(crate) const DEL_DEV_ASYNC: u32 = 0x8020_7514;

/// The same nine, in the order the runtime's test copy lists them.
#[cfg(test)]
pub(crate) const TEST_CTRL_CMDS: [u32; 9] = [
    GET_QUEUE_AFFINITY,
    GET_DEV_INFO,
    ADD_DEV,
    DEL_DEV,
    START_DEV,
    STOP_DEV,
    SET_PARAMS,
    GET_FEATURES,
    DEL_DEV_ASYNC,
];

/// `UBLK_F_CMD_IOCTL_ENCODE | UBLK_F_USER_COPY | UBLK_F_AUTO_BUF_REG`, which is exactly
/// the set the node refuses to run without. A driver with fewer is a driver the node
/// declines, and that refusal is worth being able to test, so this is a field.
pub(crate) const FEATURES: u64 = (1 << 6) | (1 << 7) | (1 << 11);

/// Bytes the driver reserves per hardware queue for its `io_desc` array: the largest
/// queue the ABI allows, times the descriptor size, rounded to a page. The node derives
/// the same number from `MAX_QUEUE_DEPTH` and the page size, and a test pins them together.
pub(crate) const CMD_BUF: usize = 4096 * 24;

/// Bytes of one `struct ublksrv_io_desc`.
pub(crate) const IO_DESC: usize = 24;

/// The ioctl-encoded data-plane commands, `UBLK_U_IO_*`.
pub(crate) const IO_FETCH_REQ: u32 = 0xC010_7520;
pub(crate) const IO_COMMIT_AND_FETCH_REQ: u32 = 0xC010_7521;
pub(crate) const IO_REGISTER_IO_BUF: u32 = 0xC010_7523;
pub(crate) const IO_UNREGISTER_IO_BUF: u32 = 0xC010_7524;

/// The same four, in the order the runtime's test copy lists them.
#[cfg(test)]
pub(crate) const TEST_IO_CMDS: [u32; 4] = [
    IO_FETCH_REQ,
    IO_COMMIT_AND_FETCH_REQ,
    IO_REGISTER_IO_BUF,
    IO_UNREGISTER_IO_BUF,
];

/// `UBLK_IO_OP_*`, the operations a request can carry.
pub(crate) const OP_READ: u8 = 0;
pub(crate) const OP_WRITE: u8 = 1;
pub(crate) const OP_DISCARD: u8 = 3;

/// Where the guest payload of a request lives in the char device's address space:
/// `UBLKSRV_IO_BUF_OFFSET` plus the queue and tag that name it. The base overlaps the tag
/// field, exactly as the ABI has it, so a decode subtracts before it shifts.
const IO_BUF_OFFSET: u64 = 0x8000_0000;
const TAG_OFF: u32 = 25;
const QID_OFF: u32 = 41;

/// Bytes a sector is, which is the unit a descriptor counts in.
const SECTOR: u64 = 512;

/// What the driver asks the ring to do once it has decided.
///
/// A fetch is answered on the ring that parked it, which need not be the ring the call
/// came in on: a request arriving for a queue wakes whoever was waiting on it. So each of
/// these names its ring rather than assuming one.
pub(crate) enum Action {
    /// Hand the request's guest pages to a ring's registered-buffer table, which is what
    /// `UBLK_F_AUTO_BUF_REG` does when a fetch completes.
    Register {
        ring: u32,
        index: u16,
        addr: u64,
        len: usize,
    },
    /// Take them back, once the request they belonged to has been answered.
    Unregister { ring: u32, index: u16 },
    /// Answer a submission.
    Post {
        ring: u32,
        user_data: u64,
        result: i32,
    },
}

/// Where a request's answer is to be sent, for one a peer made rather than a test.
///
/// A write to a peer's fabric device is a request on that peer, and the submission that
/// made it is waiting on its own ring for the answer. This is how the answer gets back.
#[derive(Copy, Clone)]
pub(super) struct Reply {
    pub(super) ring: u32,
    pub(super) user_data: u64,
    /// The buffer the submission named, which a read fills on the way back.
    pub(super) addr: u64,
    pub(super) len: u32,
    pub(super) read: bool,
}

/// A request the guest has made and the driver has not yet handed back an answer for.
struct Request {
    /// What the caller who made it knows it by.
    id: u64,
    op: u8,
    lba: u64,
    /// The guest pages. A write arrives with them filled and a read leaves with them
    /// filled, which is the whole of what a block request is.
    data: Vec<u8>,
    /// Set when the request came off the fabric, in which case the answer goes back to
    /// the submission that sent it rather than into the table a test reads.
    reply: Option<Reply>,
}

/// A fetch a worker has parked against a tag, waiting for something to serve.
struct Park {
    ring: u32,
    user_data: u64,
    /// Where in that ring's buffer table the request's pages are to be registered. The
    /// node picks this, and it is the tag id, which is why one number indexes the future
    /// slot, the buffer and the descriptor alike.
    index: u16,
}

#[derive(Default)]
struct Tag {
    park: Option<Park>,
    live: Option<Request>,
}

/// One hardware queue: its tags, and what has arrived for it that no tag has taken.
struct Queue {
    tags: Vec<Tag>,
    backlog: VecDeque<Request>,
}

/// Bytes of `struct ublksrv_ctrl_dev_info`.
const DEV_INFO: usize = 64;

/// Offset of `ublksrv_pid` within it.
const PID_OFF: usize = 16;

/// What the driver holds at one minor.
struct Dev {
    /// The `ublksrv_ctrl_dev_info` the caller asked for, plus what we wrote back into it.
    info: [u8; DEV_INFO],
    /// `SET_PARAMS` has landed, without which `START_DEV` has nothing to build a disk from.
    described: bool,
    /// `START_DEV` has landed, so `/dev/ublkb<minor>` exists.
    started: bool,
    /// The `io_desc` arrays, one queue's worth per `CMD_BUF` stride. This is the memory the
    /// node maps read-only out of the char device, so it is allocated once and never moved:
    /// a mapping outlives the call that made it.
    descs: Box<[u8]>,
    /// The data plane, one entry per hardware queue.
    queues: Vec<Queue>,
}

/// The driver's device table.
#[derive(Default)]
pub(super) struct Ublk {
    devs: BTreeMap<u32, Dev>,
    /// Requests the node has answered, by the id the caller who made them holds. The
    /// payload comes back with the result because a read is only answered by its bytes.
    done: BTreeMap<u64, (i32, Vec<u8>)>,
    /// The next request id. Never reused, so a stale answer cannot be mistaken for a fresh
    /// one across a crash.
    next_id: u64,
    /// Features this driver reports. A test lowers it to watch the node refuse to boot.
    features: u64,
    /// Set once, so a default-constructed table still answers with a usable driver.
    described: bool,
}

impl Ublk {
    /// Answers a different feature set, for a test that wants a driver the node refuses.
    #[cfg(test)]
    pub(super) fn set_features(&mut self, f: u64) {
        self.features = f;
        self.described = true;
    }

    fn features(&self) -> u64 {
        if self.described {
            self.features
        } else {
            FEATURES
        }
    }

    /// Put a device at `minor` as if some other process had exported it, owned by `pid`.
    /// This is how a test arrives at the state a predecessor that died leaves behind.
    pub(super) fn preoccupy(&mut self, minor: u32, pid: i32) {
        let mut info = [0u8; DEV_INFO];
        info[PID_OFF..PID_OFF + 4].copy_from_slice(&pid.to_ne_bytes());
        self.devs.insert(
            minor,
            Dev {
                info,
                described: true,
                started: true,
                descs: Box::new([]),
                queues: Vec::new(),
            },
        );
    }

    /// Whether `minor` is currently exported.
    pub(super) fn holds(&self, minor: u32) -> bool {
        self.devs.contains_key(&minor)
    }

    /// One control command. `cmd` is the SQE payload: a `ublksrv_ctrl_cmd` in the first 32
    /// bytes, the rest zero. The answer is the CQE result, negative for `-errno`, which is
    /// the shape the caller unpacks.
    pub(super) fn exec(&mut self, op: u32, cmd: &[u8; 80], cpus: usize) -> io::Result<i32> {
        let dev_id = u32_at(cmd, 0);
        let len = u16_at(cmd, 6) as usize;
        let addr = u64_at(cmd, 8);
        let data = u64_at(cmd, 16);

        match op {
            GET_FEATURES => {
                // SAFETY: `addr` is the caller's `u64`, alive for the call, as the kernel
                // requires of it.
                unsafe { write_out(addr, len, &self.features().to_ne_bytes()) };
                Ok(0)
            }
            ADD_DEV => {
                if self.devs.contains_key(&dev_id) {
                    return Err(io::Error::from_raw_os_error(libc::EEXIST));
                }
                let mut info = [0u8; DEV_INFO];
                // SAFETY: the caller's `DevInfo`, which `ADD_DEV` reads and writes back.
                unsafe { read_in(addr, len, &mut info) };
                // The driver stamps the minor it settled on. The node always asks for one
                // it named, so this is the number it asked for.
                info[12..16].copy_from_slice(&dev_id.to_ne_bytes());
                info[PID_OFF..PID_OFF + 4].copy_from_slice(&(-1i32).to_ne_bytes());
                // SAFETY: as above.
                unsafe { write_out(addr, len, &info) };
                let queues = u16_at(&info, 0) as usize;
                let depth = u16_at(&info, 2) as usize;
                self.devs.insert(
                    dev_id,
                    Dev {
                        info,
                        described: false,
                        started: false,
                        descs: vec![0u8; queues * CMD_BUF].into_boxed_slice(),
                        queues: (0..queues)
                            .map(|_| Queue {
                                tags: (0..depth).map(|_| Tag::default()).collect(),
                                backlog: VecDeque::new(),
                            })
                            .collect(),
                    },
                );
                Ok(0)
            }
            GET_DEV_INFO => {
                let d = self.dev(dev_id)?;
                // SAFETY: the caller's `DevInfo`, which `GET_DEV_INFO` fills in.
                unsafe { write_out(addr, len, &d.info) };
                Ok(0)
            }
            SET_PARAMS => {
                // The parameters themselves are not modeled: nothing below this seam
                // reads them, and what the block layer would do with them is the guest's
                // business, not the node's.
                self.dev_mut(dev_id)?.described = true;
                Ok(0)
            }
            GET_QUEUE_AFFINITY => {
                self.dev(dev_id)?;
                // Every queue may run on every CPU. A simulated node has no NUMA topology
                // to inherit, so there is nothing for the assignment to prefer, and it
                // falls back to spreading queues evenly, which is what it is for.
                let mut mask = vec![0u8; len];
                for c in 0..cpus.min(len * 8) {
                    mask[c / 8] |= 1 << (c % 8);
                }
                // SAFETY: the caller's `cpumask` of `len` bytes.
                unsafe { write_out(addr, len, &mask) };
                Ok(0)
            }
            START_DEV => {
                let d = self.dev_mut(dev_id)?;
                if !d.described {
                    return Err(io::Error::from_raw_os_error(libc::EINVAL));
                }
                d.info[PID_OFF..PID_OFF + 4].copy_from_slice(&(data as i32).to_ne_bytes());
                d.started = true;
                Ok(0)
            }
            STOP_DEV => {
                self.dev_mut(dev_id)?.started = false;
                Ok(0)
            }
            DEL_DEV | DEL_DEV_ASYNC => {
                self.dev(dev_id)?;
                // Both free the minor. The difference between them is whether the caller
                // waits for a consumer to let go, and a simulated export has no consumer
                // outside the node, so there is never anyone to wait for.
                self.devs.remove(&dev_id);
                Ok(0)
            }
            _ => Err(io::Error::from_raw_os_error(libc::ENOTTY)),
        }
    }

    /// One data-plane command. `cmd` is the 16-byte `ublksrv_io_cmd` payload, `index` the
    /// buffer slot the submission named for automatic registration, and `ring`/`user_data`
    /// name the submission itself.
    ///
    /// A fetch that finds nothing waiting parks and is answered later, which is why this
    /// hands back a list rather than a result: sometimes the list is empty.
    pub(super) fn io(
        &mut self,
        minor: u32,
        cmd_op: u32,
        cmd: &[u8; 16],
        index: u16,
        ring: u32,
        user_data: u64,
    ) -> Vec<Action> {
        let q_id = u16_at(cmd, 0) as usize;
        let tag = u16_at(cmd, 2) as usize;
        let result = i32_at(cmd, 4);
        let mut out = Vec::new();
        let Ok(d) = self.dev_mut(minor) else {
            out.push(Action::Post {
                ring,
                user_data,
                result: -libc::ENODEV,
            });
            return out;
        };
        match cmd_op {
            IO_REGISTER_IO_BUF | IO_UNREGISTER_IO_BUF => {
                // Only ever reached when automatic registration declined, which this
                // driver never does. Answering is still cheaper than a hang.
                out.push(Action::Post {
                    ring,
                    user_data,
                    result: 0,
                });
                return out;
            }
            IO_FETCH_REQ | IO_COMMIT_AND_FETCH_REQ => {}
            _ => {
                out.push(Action::Post {
                    ring,
                    user_data,
                    result: -libc::ENOTTY,
                });
                return out;
            }
        }
        let Some(q) = d.queues.get_mut(q_id) else {
            out.push(Action::Post {
                ring,
                user_data,
                result: -libc::EINVAL,
            });
            return out;
        };
        if tag >= q.tags.len() {
            out.push(Action::Post {
                ring,
                user_data,
                result: -libc::EINVAL,
            });
            return out;
        }
        if cmd_op == IO_COMMIT_AND_FETCH_REQ {
            // One submission does two jobs: it answers the request the tag was holding and
            // asks for the next one. The answer comes first, because the pages go back
            // before they are handed out again.
            out.push(Action::Unregister { ring, index });
            if let Some(r) = q.tags[tag].live.take() {
                match r.reply {
                    // A frame off the fabric: the peer that sent it is holding a
                    // submission open, and a read is only answered by its bytes.
                    Some(rep) => {
                        if rep.read && result > 0 {
                            // SAFETY: the submission is live, so `addr` names a buffer of
                            // the worker that made it and that worker is waiting on this.
                            let mem = unsafe {
                                std::slice::from_raw_parts_mut(
                                    rep.addr as *mut u8,
                                    rep.len as usize,
                                )
                            };
                            let n = mem.len().min(r.data.len());
                            mem[..n].copy_from_slice(&r.data[..n]);
                        }
                        out.push(Action::Post {
                            ring: rep.ring,
                            user_data: rep.user_data,
                            result,
                        });
                    }
                    None => {
                        self.done.insert(r.id, (result, r.data));
                    }
                }
            }
        }
        // Whatever the tag was doing, it is now waiting.
        let d = self.dev_mut(minor).expect("device");
        d.queues[q_id].tags[tag].park = Some(Park {
            ring,
            user_data,
            index,
        });
        out.extend(serve(d, q_id));
        out
    }

    /// Hand a request to a queue, as a guest would.
    ///
    /// It waits until a tag is free, which is the backpressure the node relies on: a queue
    /// is only as deep as the tags it was built with.
    pub(super) fn submit(
        &mut self,
        minor: u32,
        q_id: usize,
        op: u8,
        lba: u64,
        data: Vec<u8>,
        reply: Option<Reply>,
    ) -> io::Result<(u64, Vec<Action>)> {
        let id = self.next_id + 1;
        self.next_id = id;
        let d = self.dev_mut(minor)?;
        if !d.started {
            return Err(io::Error::from_raw_os_error(libc::ENODEV));
        }
        let q = d
            .queues
            .get_mut(q_id)
            .ok_or_else(|| io::Error::from_raw_os_error(libc::EINVAL))?;
        q.backlog.push_back(Request {
            id,
            op,
            lba,
            data,
            reply,
        });
        let actions = serve(d, q_id);
        Ok((id, actions))
    }

    /// How many hardware queues a device was built with, so a caller can pick one.
    /// Drops a device outright, because the server that exported it is gone.
    ///
    /// The minor is free at once. There is nobody left to ask to let go of it, which is
    /// the difference between a crash and a shutdown.
    pub(super) fn forget(&mut self, minor: u32) {
        self.devs.remove(&minor);
    }

    pub(super) fn queues(&self, minor: u32) -> usize {
        self.devs.get(&minor).map_or(0, |d| d.queues.len())
    }

    /// The answer to a request, once the node has given one.
    pub(super) fn done(&mut self, id: u64) -> Option<(i32, Vec<u8>)> {
        self.done.remove(&id)
    }

    /// Move bytes between a request's guest pages and a worker's buffer, which is what a
    /// read or write against the char device at a `buf_offset` position means.
    ///
    /// `read` is from the node's side: true copies the guest's bytes out to it, which is
    /// how a write request's payload is collected, and false fills them in, which is how a
    /// read request is answered.
    ///
    /// `fixed` says the submission named its buffer by registered index rather than by
    /// address. The driver refuses that: a registered buffer reaches it as an `ITER_BVEC`
    /// iterator, and `ublk_check_and_get_req()` returns `-EACCES` unless
    /// `user_backed_iter()` holds, which only an `ITER_UBUF` or `ITER_IOVEC` does. It is
    /// the first thing it checks, before the position is even taken apart, so it is the
    /// first thing checked here.
    pub(super) fn copy(
        &mut self,
        minor: u32,
        pos: u64,
        addr: u64,
        len: u32,
        read: bool,
        fixed: bool,
    ) -> i32 {
        if fixed {
            return -libc::EACCES;
        }
        let pos = match pos.checked_sub(IO_BUF_OFFSET) {
            Some(p) => p,
            None => return -libc::EINVAL,
        };
        let q_id = (pos >> QID_OFF) as usize;
        let tag = ((pos >> TAG_OFF) & 0xFFFF) as usize;
        let at = (pos & ((1 << TAG_OFF) - 1)) as usize;
        let Ok(d) = self.dev_mut(minor) else {
            return -libc::ENODEV;
        };
        let Some(r) = d
            .queues
            .get_mut(q_id)
            .and_then(|q| q.tags.get_mut(tag))
            .and_then(|t| t.live.as_mut())
        else {
            return -libc::EINVAL;
        };
        let n = len as usize;
        if at + n > r.data.len() {
            return -libc::EINVAL;
        }
        // SAFETY: the submission is live, so `addr` names a buffer of the worker that made
        // it, and that worker is blocked on this completion.
        let mem = unsafe { std::slice::from_raw_parts_mut(addr as *mut u8, n) };
        if read {
            mem.copy_from_slice(&r.data[at..at + n]);
        } else {
            r.data[at..at + n].copy_from_slice(mem);
        }
        len as i32
    }

    /// Complete every fetch parked against a device, which is what stopping one does.
    ///
    /// A parked fetch is the only thing keeping a worker's tag armed, so this is how a
    /// queue is ever reaped: the node counts the aborts, not the stop.
    pub(super) fn abort(&mut self, minor: u32) -> Vec<Action> {
        let mut out = Vec::new();
        let Ok(d) = self.dev_mut(minor) else {
            return out;
        };
        for q in &mut d.queues {
            for t in &mut q.tags {
                if let Some(p) = t.park.take() {
                    out.push(Action::Post {
                        ring: p.ring,
                        user_data: p.user_data,
                        result: -libc::ENODEV,
                    });
                }
            }
        }
        out
    }

    /// The descriptor memory `offset..offset + len` of a device's char node, which is what
    /// the node gets back when it maps a queue. Read-only to the node, written here.
    pub(super) fn descs(&mut self, minor: u32, offset: u64, len: usize) -> io::Result<*mut u8> {
        let d = self.dev_mut(minor)?;
        let at = offset as usize;
        if at + len > d.descs.len() {
            return Err(io::Error::from_raw_os_error(libc::EINVAL));
        }
        Ok(unsafe { d.descs.as_mut_ptr().add(at) })
    }

    fn dev(&self, dev_id: u32) -> io::Result<&Dev> {
        self.devs
            .get(&dev_id)
            .ok_or_else(|| io::Error::from_raw_os_error(libc::ENODEV))
    }

    fn dev_mut(&mut self, dev_id: u32) -> io::Result<&mut Dev> {
        self.devs
            .get_mut(&dev_id)
            .ok_or_else(|| io::Error::from_raw_os_error(libc::ENODEV))
    }

    /// Whether `pid` looks like a live server to this driver. A minor whose owner is gone
    /// is one the node may reclaim; one whose owner is still there is not.
    pub(super) fn serving(&self, pid: i32) -> bool {
        self.devs.values().any(|d| {
            d.started && i32::from_ne_bytes(d.info[PID_OFF..PID_OFF + 4].try_into().unwrap()) == pid
        })
    }
}

/// Give every parked tag on a queue something to do, while there is anything to give.
///
/// The descriptor is written before the completion is handed out, because the node reads
/// it the moment the completion arrives and nothing orders the two but this.
fn serve(d: &mut Dev, q_id: usize) -> Vec<Action> {
    let mut out = Vec::new();
    let stride = q_id * CMD_BUF;
    let Dev { descs, queues, .. } = d;
    let q = &mut queues[q_id];
    for tag in 0..q.tags.len() {
        if q.tags[tag].park.is_none() || q.backlog.is_empty() {
            continue;
        }
        let r = q.backlog.pop_front().expect("a request");
        let desc = stride + tag * IO_DESC;
        let sectors = (r.data.len() as u64 / SECTOR) as u32;
        descs[desc..desc + 4].copy_from_slice(&(r.op as u32).to_ne_bytes());
        descs[desc + 4..desc + 8].copy_from_slice(&sectors.to_ne_bytes());
        descs[desc + 8..desc + 16].copy_from_slice(&(r.lba * (4096 / SECTOR)).to_ne_bytes());
        descs[desc + 16..desc + 24].copy_from_slice(&0u64.to_ne_bytes());
        let p = q.tags[tag].park.take().expect("a parked fetch");
        out.push(Action::Register {
            ring: p.ring,
            index: p.index,
            addr: r.data.as_ptr() as u64,
            len: r.data.len(),
        });
        out.push(Action::Post {
            ring: p.ring,
            user_data: p.user_data,
            result: 0,
        });
        q.tags[tag].live = Some(r);
    }
    out
}

fn i32_at(b: &[u8], off: usize) -> i32 {
    i32::from_ne_bytes(b[off..off + 4].try_into().expect("four bytes"))
}

/// The minor a control command names, which is the first field of every one of them.
pub(super) fn dev_id(cmd: &[u8; 80]) -> u32 {
    u32_at(cmd, 0)
}

fn u16_at(b: &[u8], off: usize) -> u16 {
    u16::from_ne_bytes(b[off..off + 2].try_into().unwrap())
}

fn u32_at(b: &[u8], off: usize) -> u32 {
    u32::from_ne_bytes(b[off..off + 4].try_into().unwrap())
}

fn u64_at(b: &[u8], off: usize) -> u64 {
    u64::from_ne_bytes(b[off..off + 8].try_into().unwrap())
}

/// Writes back through the command's `addr`, exactly as the driver does, truncated to what
/// the caller said it had room for.
///
/// # Safety
///
/// `addr` must name `len` writable bytes for the duration of the call.
unsafe fn write_out(addr: u64, len: usize, src: &[u8]) {
    if addr == 0 {
        return;
    }
    let n = len.min(src.len());
    unsafe { std::ptr::copy_nonoverlapping(src.as_ptr(), addr as *mut u8, n) };
}

/// Reads the buffer the command points at.
///
/// # Safety
///
/// `addr` must name `len` readable bytes for the duration of the call.
unsafe fn read_in(addr: u64, len: usize, dst: &mut [u8]) {
    if addr == 0 {
        return;
    }
    let n = len.min(dst.len());
    unsafe { std::ptr::copy_nonoverlapping(addr as *const u8, dst.as_mut_ptr(), n) };
}

#[cfg(test)]
mod tests {
    use super::*;

    fn cmd(dev_id: u32, len: u16, addr: u64, data: u64) -> [u8; 80] {
        let mut c = [0u8; 80];
        c[0..4].copy_from_slice(&dev_id.to_ne_bytes());
        c[4..6].copy_from_slice(&u16::MAX.to_ne_bytes());
        c[6..8].copy_from_slice(&len.to_ne_bytes());
        c[8..16].copy_from_slice(&addr.to_ne_bytes());
        c[16..24].copy_from_slice(&data.to_ne_bytes());
        c
    }

    fn add(u: &mut Ublk, minor: u32) -> io::Result<i32> {
        let mut info = [0u8; DEV_INFO];
        let c = cmd(minor, DEV_INFO as u16, info.as_mut_ptr() as u64, 0);
        u.exec(ADD_DEV, &c, 4)
    }

    #[test]
    fn the_driver_reports_the_features_the_node_needs() {
        let mut u = Ublk::default();
        let mut out = 0u64;
        let c = cmd(u32::MAX, 8, &mut out as *mut u64 as u64, 0);
        u.exec(GET_FEATURES, &c, 4).unwrap();
        assert_eq!(out, FEATURES);

        u.set_features(0);
        u.exec(GET_FEATURES, &c, 4).unwrap();
        assert_eq!(out, 0);
    }

    #[test]
    fn a_minor_can_only_be_taken_once() {
        let mut u = Ublk::default();
        add(&mut u, 7).unwrap();
        let again = add(&mut u, 7).unwrap_err();
        assert_eq!(again.raw_os_error(), Some(libc::EEXIST));
        assert!(u.holds(7));
    }

    #[test]
    fn adding_a_device_stamps_the_minor_and_no_server() {
        let mut u = Ublk::default();
        let mut info = [0u8; DEV_INFO];
        let c = cmd(3, DEV_INFO as u16, info.as_mut_ptr() as u64, 0);
        u.exec(ADD_DEV, &c, 4).unwrap();
        assert_eq!(u32_at(&info, 12), 3);
        assert_eq!(
            i32::from_ne_bytes(info[PID_OFF..PID_OFF + 4].try_into().unwrap()),
            -1
        );
    }

    #[test]
    fn a_free_minor_answers_nothing_at_all() {
        let mut u = Ublk::default();
        for op in [GET_DEV_INFO, SET_PARAMS, START_DEV, STOP_DEV, DEL_DEV] {
            let e = u.exec(op, &cmd(1, 0, 0, 0), 4).unwrap_err();
            assert_eq!(e.raw_os_error(), Some(libc::ENODEV), "op {op:#x}");
        }
    }

    #[test]
    fn a_device_cannot_start_before_it_is_described() {
        let mut u = Ublk::default();
        add(&mut u, 1).unwrap();
        let e = u.exec(START_DEV, &cmd(1, 0, 0, 99), 4).unwrap_err();
        assert_eq!(e.raw_os_error(), Some(libc::EINVAL));

        u.exec(SET_PARAMS, &cmd(1, 0, 0, 0), 4).unwrap();
        u.exec(START_DEV, &cmd(1, 0, 0, 99), 4).unwrap();
        assert!(u.serving(99));
    }

    #[test]
    fn a_stopped_device_no_longer_has_a_server() {
        let mut u = Ublk::default();
        add(&mut u, 1).unwrap();
        u.exec(SET_PARAMS, &cmd(1, 0, 0, 0), 4).unwrap();
        u.exec(START_DEV, &cmd(1, 0, 0, 42), 4).unwrap();
        u.exec(STOP_DEV, &cmd(1, 0, 0, 0), 4).unwrap();
        assert!(!u.serving(42));
        assert!(u.holds(1));
    }

    #[test]
    fn deleting_a_device_frees_the_minor() {
        let mut u = Ublk::default();
        add(&mut u, 5).unwrap();
        u.exec(DEL_DEV_ASYNC, &cmd(5, 0, 0, 0), 4).unwrap();
        assert!(!u.holds(5));
        add(&mut u, 5).unwrap();
    }

    #[test]
    fn a_predecessor_that_died_leaves_a_minor_that_answers_for_it() {
        let mut u = Ublk::default();
        u.preoccupy(9, 1234);
        assert!(u.holds(9));
        assert!(u.serving(1234));

        let mut info = [0u8; DEV_INFO];
        let c = cmd(9, DEV_INFO as u16, info.as_mut_ptr() as u64, 0);
        u.exec(GET_DEV_INFO, &c, 4).unwrap();
        assert_eq!(
            i32::from_ne_bytes(info[PID_OFF..PID_OFF + 4].try_into().unwrap()),
            1234
        );
    }

    #[test]
    fn every_queue_may_run_on_every_cpu() {
        let mut u = Ublk::default();
        add(&mut u, 0).unwrap();
        let mut mask = [0u64; 16];
        let c = cmd(0, size_of_val(&mask) as u16, mask.as_mut_ptr() as u64, 0);
        u.exec(GET_QUEUE_AFFINITY, &c, 4).unwrap();
        assert_eq!(mask[0], 0b1111);
    }

    #[test]
    fn a_command_the_driver_does_not_know_is_refused() {
        let mut u = Ublk::default();
        let e = u.exec(0xdead_beef, &cmd(0, 0, 0, 0), 4).unwrap_err();
        assert_eq!(e.raw_os_error(), Some(libc::ENOTTY));
    }
}
