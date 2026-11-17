//! Receive pool sizing: each rail's host pool grows by registered blocks
//! while demand rises and drains its newest blocks once demand falls.
//!
//! A [`page_alloc::Provision`] per rail turns the buffers in use (and the
//! grant requests waiting for one) into a target. The owner grows toward it
//! by reactivating a draining block, which costs nothing, or by registering
//! one new block on a helper thread, one at a time per rail. It shrinks by
//! draining the newest block: the block hands out nothing more, and once
//! every buffer is back and no window holds its region, the pool
//! deregisters it and drops its charge. Registration time sets the hold
//! time, so blocks that are slow to register are kept longer. See
//! `designs/racer-pool-control.md`.

use std::{
    rc::Rc,
    sync::{
        Arc,
        atomic::{AtomicU64, Ordering},
    },
    time::{Duration, Instant},
};

use page_alloc::{Plan, Pressure, Provision, ProvisionConfig, SAMPLE_PERIOD};

use crate::{
    Guard, GuardOwner,
    ffi::{Access, NativeDevice, RegisterWake, Registering},
    pool::Pool,
};

/// Expected time to register one block before any is measured.
const REGISTER_PRIOR: Duration = Duration::from_millis(100);

/// Wait after a refused charge or a failed registration before trying again.
const RETRY: Duration = Duration::from_secs(1);

/// Reserves the charge for one more block of the given size, on the owner
/// thread; `None` when refused.
pub type BlockCharge = dyn FnMut(usize) -> Option<Guard> + Send;

/// Lets each rail's receive pool grow past its first block (see
/// [`crate::NodeConfig::growth`]).
pub struct Growth {
    /// Bytes per block, a nonzero multiple of the buffer length and at most
    /// the pool bytes. Activation registers one block per rail, which is
    /// always kept; the pool bytes are the ceiling.
    pub block_bytes: usize,

    /// Charges each block registered after the first.
    pub charge: Box<BlockCharge>,
}

/// A rail's receive pool sizing, for telemetry. Counts are in buffers.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct PoolSizing {
    /// Buffers in active blocks.
    pub capacity: usize,

    /// Buffers the controller wants.
    pub target: usize,

    /// Buffers not free, draining blocks included.
    pub in_use: usize,

    /// Registered blocks, active and draining.
    pub blocks: usize,

    /// Blocks waiting for their buffers to return before deregistering.
    pub draining: usize,

    /// Blocks registered after activation, since the round started.
    pub registrations: u64,

    /// Blocks deregistered after draining, since the round started.
    pub deregistrations: u64,

    /// Smoothed time to register one block.
    pub register: Duration,

    /// Time surplus blocks are kept after demand falls.
    pub hold: Duration,
}

/// [`PoolSizing`] published by the owner for clients.
#[derive(Debug, Default)]
pub(crate) struct SizingStats {
    capacity: AtomicU64,
    target: AtomicU64,
    in_use: AtomicU64,
    blocks: AtomicU64,
    draining: AtomicU64,
    registrations: AtomicU64,
    deregistrations: AtomicU64,
    register_ns: AtomicU64,
    hold_ns: AtomicU64,
}

impl SizingStats {
    pub(crate) fn snapshot(&self) -> PoolSizing {
        let load = |v: &AtomicU64| v.load(Ordering::Relaxed);
        let count = |v: &AtomicU64| usize::try_from(load(v)).unwrap_or(usize::MAX);
        PoolSizing {
            capacity: count(&self.capacity),
            target: count(&self.target),
            in_use: count(&self.in_use),
            blocks: count(&self.blocks),
            draining: count(&self.draining),
            registrations: load(&self.registrations),
            deregistrations: load(&self.deregistrations),
            register: Duration::from_nanos(load(&self.register_ns)),
            hold: Duration::from_nanos(load(&self.hold_ns)),
        }
    }
}

/// A block registration in flight.
struct Pending {
    registering: Registering,

    quota: Arc<GuardOwner>,

    started: Instant,
}

/// One rail's sizing state, kept by the owner.
pub(crate) struct RailSizing {
    demand: Provision,

    /// Buffers per block.
    block: usize,

    registering: Option<Pending>,

    /// No growth is attempted before this.
    retry: Option<Instant>,

    /// Missed takes are reported at most once per sample period, so a
    /// request retried on every poll does not inflate the correction.
    last_shortfall: Option<Instant>,

    stats: Arc<SizingStats>,
}

/// What [`RailSizing::step`] changed.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub(crate) struct Changes {
    /// A block was added, reactivated, or deregistered: republish the pool.
    pub(crate) blocks: bool,

    /// A block started draining: links should drop its free windows.
    pub(crate) drained: bool,
}

impl RailSizing {
    /// Sizing for a pool whose first block holds `block` buffers, growing
    /// to at most `ceiling` buffers in whole blocks.
    pub(crate) fn new(block: usize, ceiling: usize) -> Self {
        let block = block.max(1);
        let ceiling = (ceiling / block).max(1) * block;
        Self {
            demand: Provision::new(ProvisionConfig {
                unit: block,
                floor: block,
                ceiling,
                acquire: REGISTER_PRIOR,
            }),
            block,
            registering: None,
            retry: None,
            last_shortfall: None,
            stats: Arc::default(),
        }
    }

    pub(crate) fn stats(&self) -> &Arc<SizingStats> {
        &self.stats
    }

    /// True while a block registration is in flight.
    pub(crate) fn registering(&self) -> bool {
        self.registering.is_some()
    }

    /// True while sizing needs periodic polls: buffers are in use, surplus
    /// or draining blocks remain, or growth waits to retry.
    pub(crate) fn busy(&self, pool: &Pool) -> bool {
        self.registering.is_some()
            || self.retry.is_some()
            || pool.in_use() > 0
            || pool.capacity() > self.block
            || pool.draining_blocks() > 0
    }

    /// Finish a ready registration, then size the pool for `waiting` parked
    /// requests and the buffers in use. `charge` reserves a new block,
    /// `device` registers it, and `wake` runs when it is done. While
    /// `closing`, only finishes registrations, dropping what they built.
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn step(
        &mut self,
        pool: &mut Pool,
        device: &Rc<NativeDevice>,
        charge: &mut BlockCharge,
        wake: &RegisterWake,
        waiting: usize,
        pressure: Pressure,
        closing: bool,
        now: Instant,
    ) -> Changes {
        let mut changes = Changes::default();

        if self
            .registering
            .as_ref()
            .is_some_and(|p| p.registering.ready())
        {
            let pending = self.registering.take().expect("checked");
            let took = now.saturating_duration_since(pending.started);
            match pending.registering.finish() {
                Ok(region) if !closing => match pool.add_block(region, pending.quota) {
                    Ok(_) => {
                        self.demand.acquired(now, 1, took);
                        self.stats.registrations.fetch_add(1, Ordering::Relaxed);
                        changes.blocks = true;
                    }
                    Err(error) => {
                        eprintln!("racer: stage=rdma-pool step=add-block error={error:?}");
                        self.retry = Some(now + RETRY);
                    }
                },
                Ok(_) => {}
                Err(error) => {
                    eprintln!("racer: stage=rdma-pool step=register-block error={error:?}");
                    self.retry = Some(now + RETRY);
                }
            }
        }

        if closing {
            return changes;
        }

        if self.retry.is_some_and(|at| now >= at) {
            self.retry = None;
            self.demand.set_saturated(false);
        }

        if pool.take_missed() > 0
            && self
                .last_shortfall
                .is_none_or(|at| now.saturating_duration_since(at) >= SAMPLE_PERIOD)
        {
            self.last_shortfall = Some(now);
            self.demand.shortfall(now, 1);
        }

        self.demand.observe(pool.in_use() + waiting);
        let pending = if self.registering.is_some() {
            self.block
        } else {
            0
        };

        match self.demand.tick(now, pool.capacity(), pending, pressure) {
            Plan::Hold => {}
            Plan::Grow(_) => {
                if pool.reactivate().is_some() {
                    changes.blocks = true;
                } else if self.retry.is_none() {
                    self.register(pool, device, charge, wake, now);
                }
            }
            Plan::Release(units) => {
                for _ in 0..units {
                    if pool.drain_newest().is_none() {
                        break;
                    }
                    changes.drained = true;
                    changes.blocks = true;
                }
            }
        }

        changes
    }

    /// Deregister drained blocks; true if any was.
    pub(crate) fn reap(&mut self, pool: &mut Pool) -> bool {
        let freed = pool.reap();
        if freed == 0 {
            return false;
        }
        self.stats
            .deregistrations
            .fetch_add((freed / self.block) as u64, Ordering::Relaxed);
        true
    }

    /// Reserve and start registering one block.
    fn register(
        &mut self,
        pool: &Pool,
        device: &Rc<NativeDevice>,
        charge: &mut BlockCharge,
        wake: &RegisterWake,
        now: Instant,
    ) {
        let bytes = self.block * pool.buffer_len();
        let Some(guard) = charge(bytes) else {
            // Admission is held by other users: more capacity cannot be had
            // now, so the miss correction must not build up.
            self.demand.set_saturated(true);
            self.retry = Some(now + RETRY);
            return;
        };
        let quota = GuardOwner::new(guard);
        match Registering::host(
            device.clone(),
            bytes,
            Access::PAGES,
            quota.clone(),
            wake.clone(),
        ) {
            Ok(registering) => {
                self.registering = Some(Pending {
                    registering,
                    quota,
                    started: now,
                });
            }
            Err(error) => {
                eprintln!("racer: stage=rdma-pool step=register-block error={error:?}");
                self.retry = Some(now + RETRY);
            }
        }
    }

    /// Publish the pool's sizing and the room it may still grow into.
    pub(crate) fn publish(&self, pool: &Pool) {
        let room = if self.retry.is_some() {
            0
        } else {
            self.demand.config().ceiling.saturating_sub(pool.capacity())
        };
        pool.set_room(room);
        let store = |v: &AtomicU64, n: u64| v.store(n, Ordering::Relaxed);
        let nanos = |d: Duration| u64::try_from(d.as_nanos()).unwrap_or(u64::MAX);
        store(&self.stats.capacity, pool.capacity() as u64);
        store(&self.stats.target, self.demand.target() as u64);
        store(&self.stats.in_use, pool.in_use() as u64);
        store(&self.stats.blocks, pool.block_count() as u64);
        store(&self.stats.draining, pool.draining_blocks() as u64);
        store(&self.stats.register_ns, nanos(self.demand.acquire()));
        store(&self.stats.hold_ns, nanos(self.demand.effective_hold()));
    }
}
