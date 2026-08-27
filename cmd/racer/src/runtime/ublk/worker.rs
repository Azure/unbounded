//! Worker-side ublk queue ownership and request handling.

use std::cell::{Cell, RefCell};
use std::collections::VecDeque;

use crate::kernel;
use crate::kernel::ring::{Op as RingOp, Sqe};

use super::super::sys::Mapping;
use super::{
    AUTO_BUF_REG_FALLBACK, IO_COMMIT_AND_FETCH_REQ, IO_F_NEED_REG_BUF, IO_FETCH_REQ,
    IO_REGISTER_IO_BUF, IO_RES_OK, IO_UNREGISTER_IO_BUF, IoCmd, IoDesc, MAX_IO_BYTES, OP_DISCARD,
    OP_READ, OP_WRITE, auto_buf_reg, cmd_buf_size,
};
use crate::runtime::limits::Limits;
use crate::runtime::{
    Ack, Buf, Errno, Local, Op, OpSlab, QUEUES_PER_WORKER, Request, limits, with,
};

const COMMIT_DELAY_HIGH: f32 = 0.85;
const COMMIT_DELAY_LOW: f32 = 0.60;

const CLASS_UBLK: u64 = 2;
const CLASS_BUFREG: u64 = 4;

const T_IDLE: u8 = 0;
const T_RUN: u8 = 2;
const T_UNREG: u8 = 3;

const fn user_data(class: u64, slot: u64, payload: u64) -> u64 {
    (class << 60) | ((slot & 0xF_FFFF) << 24) | (payload & 0xFF_FFFF)
}

fn ud_ublk(id: u32) -> u64 {
    user_data(CLASS_UBLK, 0, id as u64)
}

fn ud_bufreg(id: u32, unreg: bool) -> u64 {
    user_data(CLASS_BUFREG, unreg as u64, id as u64)
}

/// The tag id is the worker's universal handle: the `user_data` payload, the registered
/// buffer index, the `Buf` index the handler sees, and the future slot. Its shape follows
/// the limits the worker was built at, which are installed per thread.
pub(super) fn tag_id(slot: u16, lq: usize, tag: u16) -> u32 {
    let l = limits();
    slot as u32 * l.tags_per_dev() + lq as u32 * l.queue_depth as u32 + tag as u32
}

pub(super) fn tag_parts(id: u32) -> (u16, usize, u16) {
    let l = limits();
    (
        (id / l.tags_per_dev()) as u16,
        (id % l.tags_per_dev()) as usize / l.queue_depth as usize,
        (id % l.queue_depth as u32) as u16,
    )
}

/// One ublk hardware queue; a worker owns up to `QUEUES_PER_WORKER` per device.
struct Queue {
    q_id: u16,
    descs: Mapping,
    armed: u32,
    inflight: u32,
    tag_state: Vec<u8>,
    tag_res: Vec<i32>,
    tag_bytes: Vec<u32>,
}

impl Queue {
    fn desc(&self, tag: u16) -> IoDesc {
        let base = self.descs.as_ptr() as *const IoDesc;
        unsafe { std::ptr::read_volatile(base.add(tag as usize)) }
    }
}

#[derive(Default)]
struct DevSlot {
    active: bool,
    stopping: bool,
    reaping: bool,
    dev: u64,
    queues: [Option<Queue>; QUEUES_PER_WORKER],
    stop_ack: Option<Ack>,
    reap_ack: Option<Ack>,
}

impl DevSlot {
    fn quiesced(&self) -> bool {
        self.queues.iter().flatten().all(|q| q.inflight == 0)
    }

    fn reaped(&self) -> bool {
        self.queues.iter().flatten().all(|q| q.armed == 0)
    }
}

pub(in crate::runtime) enum Ctl {
    Start {
        slot: u16,
        dev: u64,
        cfd: kernel::FileRef,
        depth: u16,
        q_ids: Vec<u16>,
        ack: Ack,
    },
    Stop {
        slot: u16,
        ack: Ack,
    },
    Reap {
        slot: u16,
        ack: Ack,
    },
}

/// All ublk state owned by one runtime worker.
pub(in crate::runtime) struct State {
    devs: RefCell<Vec<DevSlot>>,
    commit_backlog: RefCell<VecDeque<(u32, i32)>>,
    throttled: Cell<bool>,
    draining: Cell<bool>,
}

impl State {
    pub(in crate::runtime) fn new(limits: &Limits) -> State {
        State {
            devs: RefCell::new(
                (0..limits.max_devices)
                    .map(|_| DevSlot::default())
                    .collect(),
            ),
            commit_backlog: RefCell::new(VecDeque::new()),
            throttled: Cell::new(false),
            draining: Cell::new(false),
        }
    }

    pub(in crate::runtime) fn is_idle(&self) -> bool {
        self.devs.borrow().iter().all(|d| !d.active)
    }

    fn with_queue<R>(&self, id: u32, f: impl FnOnce(&mut Queue) -> R) -> Option<R> {
        let (slot, lq, _) = tag_parts(id);
        let mut devs = self.devs.borrow_mut();
        devs[slot as usize].queues[lq].as_mut().map(f)
    }

    fn target(&self, id: u32) -> Option<(u32, u16, u16)> {
        let (slot, lq, tag) = tag_parts(id);
        self.devs.borrow()[slot as usize].queues[lq]
            .as_ref()
            .map(|q| (slot as u32, q.q_id, tag))
    }

    /// Arm one tag: fetch a fresh request, or commit one and fetch its replacement.
    fn arm(&self, local: &Local, id: u32, cmd_op: u32, result: i32) {
        let (slot, lq, tag) = tag_parts(id);
        let q_id = {
            let mut devs = self.devs.borrow_mut();
            let d = &mut devs[slot as usize];
            let Some(q) = d.queues[lq].as_mut().filter(|_| d.active) else {
                return;
            };
            q.armed += 1;
            q.q_id
        };
        let cmd = IoCmd {
            q_id,
            tag,
            result,
            addr: 0,
        };
        local.push(Sqe::new(
            RingOp::UringCmd16 {
                file: slot as u32,
                cmd_op,
                cmd: cmd.encode(),
                addr: Some(auto_buf_reg(id as u16, AUTO_BUF_REG_FALLBACK)),
            },
            ud_ublk(id),
        ));
    }

    /// `UBLK_IO_F_NEED_REG_BUF` fallback: register or unregister the buffer by hand.
    fn buf_reg(&self, local: &Local, id: u32, unreg: bool) {
        let (slot, _, tag) = tag_parts(id);
        let Some(q_id) = self.with_queue(id, |q| q.q_id) else {
            return;
        };
        let cmd = IoCmd {
            q_id,
            tag,
            result: 0,
            addr: id as u64,
        };
        let op = if unreg {
            IO_UNREGISTER_IO_BUF
        } else {
            IO_REGISTER_IO_BUF
        };
        local.push(Sqe::new(
            RingOp::UringCmd16 {
                file: slot as u32,
                cmd_op: op,
                cmd: cmd.encode(),
                addr: None,
            },
            ud_bufreg(id, unreg),
        ));
    }

    fn commit(&self, local: &Local, id: u32, res: i32) {
        debug_assert!(
            !local.ops.tag_busy(id),
            "racer: committing tag {id} while an op still references its buffer"
        );
        if self.throttled.get() {
            self.commit_backlog.borrow_mut().push_back((id, res));
            return;
        }
        self.arm(local, id, IO_COMMIT_AND_FETCH_REQ, res);
    }

    pub(in crate::runtime) fn update_throttle(&self, ops: &OpSlab) {
        let utilization = ops.utilization();
        if self.throttled.get() {
            if utilization < COMMIT_DELAY_LOW {
                self.throttled.set(false);
            }
        } else if utilization > COMMIT_DELAY_HIGH {
            self.throttled.set(true);
        }
    }

    pub(in crate::runtime) fn drain_commit_backlog(&self, local: &Local) -> usize {
        if self.throttled.get() {
            return 0;
        }
        let mut n = 0;
        while let Some((id, res)) = self.commit_backlog.borrow_mut().pop_front() {
            self.arm(local, id, IO_COMMIT_AND_FETCH_REQ, res);
            n += 1;
        }
        n
    }

    /// Handles ublk CQ classes. `Err(())` means the class belongs to another subsystem.
    pub(in crate::runtime) fn handle_cqe(
        &self,
        local: &Local,
        user_data: u64,
        res: i32,
    ) -> Result<Option<(u32, Request)>, ()> {
        let class = user_data >> 60;
        let id = (user_data & 0xFF_FFFF) as u32;
        match class {
            CLASS_UBLK => Ok(self.ublk_cqe(local, id, res)),
            CLASS_BUFREG => {
                let unreg = ((user_data >> 24) & 0xF_FFFF) == 1;
                Ok(self.bufreg_cqe(local, id, unreg, res))
            }
            _ => Err(()),
        }
    }

    fn ublk_cqe(&self, local: &Local, id: u32, res: i32) -> Option<(u32, Request)> {
        let (slot, lq, _) = tag_parts(id);
        {
            let mut devs = self.devs.borrow_mut();
            let d = &mut devs[slot as usize];
            let q = d.queues[lq].as_mut()?;
            q.armed -= 1;
            if res != IO_RES_OK || d.stopping {
                return None;
            }
        }
        self.start_request(local, id)
    }

    fn start_request(&self, local: &Local, id: u32) -> Option<(u32, Request)> {
        let (slot, lq, tag) = tag_parts(id);
        let (desc, dev) = {
            let devs = self.devs.borrow();
            let d = &devs[slot as usize];
            let q = d.queues[lq].as_ref()?;
            (q.desc(tag), d.dev)
        };

        if desc.flags() & IO_F_NEED_REG_BUF != 0 {
            self.buf_reg(local, id, false);
            return None;
        }
        self.with_queue(id, |q| q.tag_state[tag as usize] = T_RUN);
        self.dispatch(local, id, dev, desc)
    }

    fn dispatch(&self, local: &Local, id: u32, dev: u64, desc: IoDesc) -> Option<(u32, Request)> {
        let (_, _, tag) = tag_parts(id);
        let bytes = desc.nr_sectors * 512;
        debug_assert!(bytes as usize <= MAX_IO_BYTES);
        self.with_queue(id, |q| {
            q.inflight += 1;
            q.tag_bytes[tag as usize] = bytes;
        });

        let op = match desc.op() {
            OP_READ => Op::Read,
            OP_WRITE => Op::Write,
            OP_DISCARD => Op::Discard,
            _ => {
                self.finish_request(local, id, Err(Errno::EOPNOTSUPP));
                return None;
            }
        };
        Some((
            id,
            Request {
                dev,
                op,
                lba: desc.start_sector / 8,
                buf: Buf {
                    index: id as u16,
                    addr: 0,
                    len: bytes,
                },
            },
        ))
    }

    fn bufreg_cqe(&self, local: &Local, id: u32, unreg: bool, res: i32) -> Option<(u32, Request)> {
        let (slot, lq, tag) = tag_parts(id);
        if unreg {
            let stored = self
                .with_queue(id, |q| {
                    q.tag_state[tag as usize] = T_IDLE;
                    q.tag_res[tag as usize]
                })
                .unwrap_or(0);
            self.commit(local, id, stored);
            return None;
        }
        if res < 0 {
            self.with_queue(id, |q| q.tag_state[tag as usize] = T_IDLE);
            self.commit(local, id, res);
            return None;
        }
        let (desc, dev) = {
            let devs = self.devs.borrow();
            let d = &devs[slot as usize];
            let q = d.queues[lq].as_ref()?;
            (q.desc(tag), d.dev)
        };
        self.with_queue(id, |q| q.tag_state[tag as usize] = T_RUN);
        self.dispatch(local, id, dev, desc)
    }

    pub(in crate::runtime) fn finish_request(
        &self,
        local: &Local,
        id: u32,
        res: Result<(), Errno>,
    ) {
        let (_, _, tag) = tag_parts(id);
        let Some((bytes, needs_unreg)) = self.with_queue(id, |q| {
            q.inflight -= 1;
            let needs_unreg = q.tag_state[tag as usize] == T_UNREG
                || (q.tag_state[tag as usize] == T_RUN
                    && q.desc(tag).flags() & IO_F_NEED_REG_BUF != 0);
            (q.tag_bytes[tag as usize] as i32, needs_unreg)
        }) else {
            return;
        };
        let result = match res {
            Ok(()) => bytes,
            Err(e) => -e.raw(),
        };
        if needs_unreg {
            self.with_queue(id, |q| {
                q.tag_state[tag as usize] = T_UNREG;
                q.tag_res[tag as usize] = result;
            });
            self.buf_reg(local, id, true);
            return;
        }
        self.with_queue(id, |q| q.tag_state[tag as usize] = T_IDLE);
        self.commit(local, id, result);
    }

    pub(in crate::runtime) fn apply_ctl(&self, local: &Local, ctl: Ctl) {
        match ctl {
            Ctl::Start {
                slot,
                dev,
                cfd,
                depth,
                q_ids,
                ack,
            } => {
                self.start_queues(local, slot, dev, cfd, depth, &q_ids);
                let _ = ack.send(());
            }
            Ctl::Stop { slot, ack } => {
                let mut devs = self.devs.borrow_mut();
                let d = &mut devs[slot as usize];
                if !d.active {
                    drop(devs);
                    let _ = ack.send(());
                    return;
                }
                d.stopping = true;
                self.draining.set(true);
                d.stop_ack = Some(ack);
            }
            Ctl::Reap { slot, ack } => {
                let mut devs = self.devs.borrow_mut();
                let d = &mut devs[slot as usize];
                if !d.active {
                    drop(devs);
                    let _ = ack.send(());
                    return;
                }
                d.reaping = true;
                self.draining.set(true);
                d.reap_ack = Some(ack);
            }
        }
    }

    pub(in crate::runtime) fn maintenance(&self, local: &Local) -> usize {
        if !self.draining.get() {
            return 0;
        }

        enum Done {
            Stop(Option<Ack>),
            Reap(Option<Ack>),
        }

        let mut n = 0;
        let mut left = false;
        for slot in 0..limits().max_devices {
            let done = {
                let mut devs = self.devs.borrow_mut();
                let d = &mut devs[slot as usize];
                if d.reaping {
                    if !d.reaped() {
                        left = true;
                        continue;
                    }
                    d.active = false;
                    d.stopping = false;
                    d.reaping = false;
                    d.queues = Default::default();
                    Done::Reap(d.reap_ack.take())
                } else if d.stopping {
                    if !d.quiesced() {
                        left = true;
                        continue;
                    }
                    Done::Stop(d.stop_ack.take())
                } else {
                    continue;
                }
            };
            let ack = match done {
                Done::Reap(ack) => {
                    let _ = local
                        .ring
                        .register_files_update(slot as u32, &[kernel::FileRef::NONE]);
                    ack
                }
                Done::Stop(ack) => ack,
            };
            if let Some(ack) = ack {
                let _ = ack.send(());
                n += 1;
            }
        }
        self.draining.set(left);
        n
    }

    fn start_queues(
        &self,
        local: &Local,
        slot: u16,
        dev: u64,
        cfd: kernel::FileRef,
        depth: u16,
        q_ids: &[u16],
    ) {
        if let Err(e) = local.ring.register_files_update(slot as u32, &[cfd]) {
            eprintln!(
                "racer: worker {} cannot register char device: {e}",
                local.core
            );
            return;
        }
        for (lq, &q_id) in q_ids.iter().enumerate() {
            let map = Mapping::map_read(
                cfd,
                q_id as u64 * cmd_buf_size() as u64,
                depth as usize * size_of::<IoDesc>(),
            );
            let descs = match map {
                Ok(mapping) => mapping,
                Err(e) => {
                    eprintln!("racer: worker {} cannot map queue {q_id}: {e}", local.core);
                    return;
                }
            };
            let mut devs = self.devs.borrow_mut();
            let d = &mut devs[slot as usize];
            d.active = true;
            d.stopping = false;
            d.dev = dev;
            d.queues[lq] = Some(Queue {
                q_id,
                descs,
                armed: 0,
                inflight: 0,
                tag_state: vec![T_IDLE; depth as usize],
                tag_res: vec![0; depth as usize],
                tag_bytes: vec![0; depth as usize],
            });
        }
        for lq in 0..q_ids.len() {
            for tag in 0..depth {
                self.arm(local, tag_id(slot, lq, tag), IO_FETCH_REQ, 0);
            }
        }
        local.submit();
    }
}

pub(in crate::runtime) fn request_target(id: u32) -> Result<(u32, u16, u16), Errno> {
    with(|local| local.ublk.target(id)).ok_or(Errno::EIO)
}
