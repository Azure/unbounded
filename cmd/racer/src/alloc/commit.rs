use std::future::Future;
use std::pin::Pin;
use std::task::{Context, Poll};

use crate::layout::{self, Class, MBLOCK};
use crate::runtime::PoolBuf;
use crate::runtime::{self, Durability};
use crate::server::{Server, Worker};

use super::shard::{Act, Maps, Ticket};
use super::{Allocator, GlobalAddr, Status, here, shard};

/// [`Act`] with the claim it hands out.
pub(super) enum Turn {
    Done,
    Wait,
    Go(Flushing),
}

/// This core's turn at flushing mblock `li`.
pub(super) fn turn(worker: &std::rc::Rc<Worker>, class: Class, li: u32, need: u64) -> Turn {
    match shard(worker, |sh| sh.flush_act(class, li, need)) {
        Act::Done => Turn::Done,
        Act::Wait => Turn::Wait,
        Act::Go => {
            here(worker, |c| {
                c.flush_started[class as usize] = Some(runtime::now())
            });
            Turn::Go(Flushing {
                worker: worker.clone(),
                class,
                li,
                seq: 0,
                active: true,
            })
        }
    }
}

impl Allocator {
    /// Wait until mblock `li` is durable at or past `need`.
    pub(super) async fn flush_until(
        &'static self,
        worker: std::rc::Rc<Worker>,
        class: Class,
        li: u32,
        need: u64,
    ) -> Result<(), Status> {
        loop {
            match turn(&worker, class, li, need) {
                Turn::Done => return Ok(()),
                Turn::Wait => {
                    here(&worker, |c| c.commit_parks[class as usize] += 1);
                    Park::new(worker.clone()).await;
                }
                Turn::Go(mark) => self.flush(&worker, li, mark).await?,
            }
        }
    }

    async fn flush(
        &'static self,
        worker: &Worker,
        li: u32,
        mut mark: Flushing,
    ) -> Result<(), Status> {
        let class = mark.class;
        if here(worker, |c| c.staging[class as usize].is_none()) {
            let b = match PoolBuf::try_alloc(MBLOCK) {
                Some(b) => b,
                None => PoolBuf::alloc(MBLOCK).await,
            };
            here(worker, |c| c.staging[class as usize] = Some(b));
        }
        let (seq, off, buf) = here(worker, |c| {
            let (seq, h, rows) = c.shard.begin_flush(class, li);
            let stage = c.staging[class as usize].as_mut().unwrap();
            layout::put_mblock(stage, h, rows);
            let off = self
                .geo
                .mblock_off(class, h.mblock_id, (h.generation % 2) as u8);
            (seq, off, stage.buf())
        });
        mark.seq = seq;
        let r = self.disk.write(off, buf, Durability::Durable).await;
        mark.settle(r.is_ok());
        r.map_err(|_| Status::Io)
    }

    pub(super) async fn commit(
        &'static self,
        worker: std::rc::Rc<Worker>,
        addr: GlobalAddr,
        class: Class,
        t: Ticket,
        crc: u32,
    ) -> Result<bool, Status> {
        let Some(st) = shard(&worker, |sh| {
            self.stage_in(&worker, sh, addr, class, t, crc)
        })?
        else {
            return Ok(false);
        };
        self.flush_until(worker.clone(), class, st.li, st.seq)
            .await?;
        if let Some(old) = st.stale {
            maps!(worker.config(), m);
            shard(&worker, |sh| sh.release(class, old, &m));
        }
        Ok(true)
    }

    /// Equal-register replacement retires and durably flushes the old row before acking.
    pub(super) async fn commit_replace_equal(
        &'static self,
        worker: std::rc::Rc<Worker>,
        addr: GlobalAddr,
        class: Class,
        t: Ticket,
        crc: u32,
    ) -> Result<bool, Status> {
        let Some(st) = shard(&worker, |sh| {
            self.stage_in(&worker, sh, addr, class, t, crc)
        })?
        else {
            return Ok(false);
        };
        self.flush_until(worker.clone(), class, st.li, st.seq)
            .await?;
        let Some(slot) = st.stale else {
            return Ok(true);
        };
        let holder = crate::runtime::CoreId::of((slot / class.k() % self.cores as u32) as usize);
        let retire = move |worker: std::rc::Rc<Worker>| async move {
            maps!(worker.config(), m);
            let flush = shard(&worker, |sh| sh.release(class, slot, &m));
            if let Some((li, seq)) = flush {
                self.flush_until(worker, class, li, seq).await?;
            }
            Ok(())
        };
        if holder == runtime::core() {
            retire(worker).await
        } else {
            runtime::to_async_with::<Server, _, _, _>(holder, retire).await
        }?;
        Ok(true)
    }
}

/// A slab marked busy for one flush, and the obligation to unmark it.
#[must_use = "an unretired flush leaves the slab busy and every committer behind it parked"]
pub(super) struct Flushing {
    worker: std::rc::Rc<Worker>,
    pub(super) class: Class,
    pub(super) li: u32,
    pub(super) seq: u64,
    active: bool,
}

impl Flushing {
    fn settle(mut self, ok: bool) {
        Flushing::retire(&self.worker, self.class, self.li, self.seq, ok);
        self.active = false;
    }

    fn retire(worker: &Worker, class: Class, li: u32, seq: u64, ok: bool) {
        here(worker, |c| {
            let ci = class as usize;
            if let Some(started) = c.flush_started[ci].take() {
                c.flush_busy_us[class as usize] += runtime::now()
                    .saturating_duration_since(started)
                    .as_micros() as u64;
            }
            c.shard.end_flush(class, li, seq, ok);
            for w in c.waiters.drain(..) {
                w.wake();
            }
        });
    }
}

impl Drop for Flushing {
    fn drop(&mut self) {
        if self.active {
            Flushing::retire(&self.worker, self.class, self.li, self.seq, false);
        }
    }
}

/// Yield once and resume when a flush completes on this core.
pub(super) struct Park {
    worker: std::rc::Rc<Worker>,
    armed: bool,
}

impl Park {
    pub(super) fn new(worker: std::rc::Rc<Worker>) -> Park {
        Park {
            worker,
            armed: false,
        }
    }
}

impl Future for Park {
    type Output = ();

    fn poll(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
        if self.armed {
            return Poll::Ready(());
        }
        self.armed = true;
        here(&self.worker, |c| c.waiters.push(cx.waker().clone()));
        Poll::Pending
    }
}
