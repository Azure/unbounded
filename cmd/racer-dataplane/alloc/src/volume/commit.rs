//! Group commit: apply the frozen overlay to the trees copy-on-write, write
//! the new pages in large contiguous runs, and publish one root set.
//!
//! See the "Commit" section of `designs/racer-extent-volume.md`.

use std::collections::{BTreeMap, BTreeSet, HashMap};
use std::future::{Future, poll_fn};
use std::marker::PhantomData;
use std::pin::Pin;
use std::rc::Rc;
use std::task::Poll;

use uring_runtime::{Budget, Operation, Scope, reactor::Reactor};

use super::btree::{self, Applied, Deferred, LocalFuture, Sink, TreeRoot};
use super::format::{
    Key, NodeAddr, SLOT_UNITS, SUPERBLOCK_A_UNIT, SUPERBLOCK_B_UNIT, UNIT_BYTES, Value,
    bitmap_unit, class_records, unit_offset,
};
use super::page::{
    LOG_CAPACITY, RAW_BYTES, Raw, RecordDesc, Retired, SLOT_DATA, SlotSummary, TreeKind, TreePage,
    encode_bitmap, encode_leaf, encode_log, record_key, record_key_parts, slot_key,
};
use super::pager::Priority;
use super::store::{Durable, Failure, IO_CONCURRENCY, IndexOp, MetaWrite, RecOp, Volume};
use super::superblock::Superblock;
use super::table::{Ident, RUN_PAGES, SlotOp};
use crate::{AlignedBuffer, Charge, Error};

fn other_unit(unit: u16) -> u16 {
    if unit == SUPERBLOCK_A_UNIT {
        SUPERBLOCK_B_UNIT
    } else {
        SUPERBLOCK_A_UNIT
    }
}

fn value_raw(value: &Value) -> Raw {
    let mut raw = [0; RAW_BYTES];
    raw[..32].copy_from_slice(&value.encode());
    raw
}

fn decode_value(raw: &Raw) -> Option<Value> {
    Value::decode(raw[..32].try_into().expect("32 bytes"))
}

type WriteFuture<'a, E> = Pin<Box<dyn Future<Output = Result<(), E>> + 'a>>;

/// A contiguous run of pages being filled.
struct Run<C: Charge> {
    start: NodeAddr,
    filled: usize,
    buffer: AlignedBuffer<C>,
}

/// Encodes metadata pages into run buffers and writes each run with one
/// request. Also the eager leaf sink of the commit.
struct Writer<'a, C: Charge, S: Scope, B: Budget> {
    volume: &'a Volume<C>,
    reactor: &'a Reactor<S, B>,
    scope: &'a S,
    seq: u64,
    kind: TreeKind,
    run: Option<Run<C>>,
    // Allocated for leaves and not used yet, in address order.
    spare: std::collections::VecDeque<NodeAddr>,
    inflight: Vec<WriteFuture<'a, S::Error>>,
    error: Option<S::Error>,
    pages: u64,
    writes: u64,
    allocated: Vec<NodeAddr>,
}

impl<'a, C: Charge, S: Scope, B: Budget> Writer<'a, C, S, B>
where
    S::Error: From<Error>,
{
    fn new(volume: &'a Volume<C>, reactor: &'a Reactor<S, B>, scope: &'a S, seq: u64) -> Self {
        Self {
            volume,
            reactor,
            scope,
            seq,
            kind: TreeKind::Index,
            run: None,
            spare: std::collections::VecDeque::new(),
            inflight: Vec::new(),
            error: None,
            pages: 0,
            writes: 0,
            allocated: Vec::new(),
        }
    }

    /// Encode one page at `addr` and queue its write.
    async fn page(
        &mut self,
        addr: NodeAddr,
        cache: bool,
        encode: impl FnOnce(&mut [u8]),
    ) -> Result<(), S::Error> {
        let extends = self.run.as_ref().is_some_and(|run| {
            run.start.slot() == addr.slot()
                && u32::from(run.start.unit()) + run.filled as u32 == u32::from(addr.unit())
                && run.filled < RUN_PAGES
        });
        if !extends {
            self.submit().await?;
            let buffer = self.volume.slab.allocate(
                RUN_PAGES * UNIT_BYTES,
                (self.volume.charge)(RUN_PAGES * UNIT_BYTES)?,
            )?;
            self.run = Some(Run {
                start: addr,
                filled: 0,
                buffer,
            });
        }
        let run = self.run.as_mut().expect("run");
        let at = run.filled * UNIT_BYTES;
        let slice = &mut run.buffer.as_mut_slice()[at..at + UNIT_BYTES];
        encode(slice);
        if cache {
            let mut bytes = Box::new([0u8; UNIT_BYTES]);
            bytes.copy_from_slice(slice);
            let priority = if bytes[11] == 0 {
                Priority::Leaf
            } else {
                Priority::Inner
            };
            self.volume
                .pager
                .insert(addr, Rc::new(TreePage::trusted(bytes)), priority);
        }
        run.filled += 1;
        self.pages += 1;
        Ok(())
    }

    /// Submit the current run.
    async fn submit(&mut self) -> Result<(), S::Error> {
        let Some(mut run) = self.run.take() else {
            return Ok(());
        };
        if run.filled == 0 {
            return Ok(());
        }
        run.buffer.length = run.filled * UNIT_BYTES;
        let guard = MetaWrite::new(&self.volume.signals);
        let slab = &self.volume.slab;
        let (reactor, scope) = (self.reactor, self.scope);
        let offset = run.start.offset();
        let buffer = run.buffer;
        self.inflight.push(Box::pin(async move {
            slab.write_at(reactor, offset, buffer, guard, scope)
                .await
                .map(drop)
        }));
        self.writes += 1;
        // Submit now, and keep the in-flight count bounded.
        let limit = IO_CONCURRENCY;
        self.drain(|count| count < limit).await;
        self.check()
    }

    fn check(&mut self) -> Result<(), S::Error> {
        match self.error.take() {
            Some(error) => Err(error),
            None => Ok(()),
        }
    }

    /// Poll the in-flight writes until `done(count)` holds.
    async fn drain(&mut self, done: impl Fn(usize) -> bool) {
        let inflight = &mut self.inflight;
        let error = &mut self.error;
        poll_fn(|cx| {
            inflight.retain_mut(|write| match write.as_mut().poll(cx) {
                Poll::Ready(result) => {
                    if let Err(e) = result {
                        error.get_or_insert(e);
                    }
                    false
                }
                Poll::Pending => true,
            });
            if done(inflight.len()) {
                Poll::Ready(())
            } else {
                Poll::Pending
            }
        })
        .await;
    }

    /// Submit the last run and wait for every write.
    async fn finish(&mut self) -> Result<(), S::Error> {
        self.submit().await?;
        self.drain(|count| count == 0).await;
        self.check()
    }

    /// Take the next spare leaf address, allocating a run when needed.
    fn next_leaf_addr(&mut self) -> Result<NodeAddr, S::Error> {
        if self.spare.is_empty() {
            let pages = self.volume.shared.state().slots.alloc_pages(RUN_PAGES);
            if pages.is_empty() {
                return Err(Error::Busy.into());
            }
            self.allocated.extend(pages.iter().copied());
            self.spare.extend(pages);
        }
        Ok(self.spare.pop_front().expect("spare"))
    }

    /// Free spare leaf addresses that were never written.
    fn return_spare(&mut self) {
        let mut state = self.volume.shared.state();
        for addr in self.spare.drain(..) {
            state.slots.free_page(addr);
            self.allocated.retain(|a| *a != addr);
        }
    }
}

impl<C: Charge, S: Scope, B: Budget> Sink for Writer<'_, C, S, B>
where
    S::Error: From<Error>,
{
    type Error = S::Error;

    fn emit<'b>(
        &'b mut self,
        entries: &'b [(Key, Raw)],
    ) -> LocalFuture<'b, Result<Option<NodeAddr>, S::Error>> {
        Box::pin(async move {
            let addr = self.next_leaf_addr()?;
            let (kind, volume_id, seq) = (self.kind, self.volume.volume_id.get(), self.seq);
            self.page(addr, true, |buf| {
                encode_leaf(
                    buf,
                    kind,
                    &volume_id,
                    seq,
                    addr,
                    entries.iter().map(|(k, v)| (k, v)),
                );
            })
            .await?;
            Ok(Some(addr))
        })
    }
}

/// Undo state of a running attempt. Dropping it unfinished fails the attempt.
struct Attempt<'v, C: Charge, E: Copy + 'static + From<uring_runtime::Error>> {
    volume: &'v Volume<C>,
    id: u64,
    covered: u64,
    slot_ops: Option<BTreeMap<u32, SlotOp>>,
    claimed: Vec<(u32, u32)>,
    carry: Vec<NodeAddr>,
    dirty: BTreeSet<u32>,
    allocated: Vec<NodeAddr>,
    superblock_started: bool,
    done: bool,
    error: PhantomData<E>,
}

impl<C: Charge, E: Copy + 'static + From<uring_runtime::Error>> Attempt<'_, C, E> {
    fn fail(&mut self, error: E) {
        self.done = true;
        let volume = self.volume;
        {
            let mut state = volume.shared.state();
            let mut ops = self.slot_ops.take().unwrap_or_default();
            for (slot, incarnation) in self.claimed.drain(..) {
                ops.entry(slot).or_default().merge(&SlotOp {
                    ident: Some(Ident::Node(incarnation)),
                    ..SlotOp::default()
                });
            }
            state.slots.restore_ops(ops);
            state
                .slots
                .mark_dirty_nodes(std::mem::take(&mut self.dirty));
            let carry = std::mem::take(&mut self.carry);
            state.carry.extend(carry);
            let allocated = std::mem::take(&mut self.allocated);
            if self.superblock_started {
                // A superblock that may be durable references them.
                state.carry.extend(allocated);
            } else {
                for addr in allocated {
                    state.slots.free_page(addr);
                    volume.pager.invalidate(addr);
                }
            }
        }
        let mut commit = volume.commit.borrow_mut();
        commit.running = false;
        commit.finished = self.id;
        commit.failed += 1;
        commit.failure = Some(Failure {
            attempt: self.id,
            covered: self.covered,
            error: Box::new(error),
        });
        drop(commit);
        volume.signals.wake();
    }
}

impl<C: Charge, E: Copy + 'static + From<uring_runtime::Error>> Drop for Attempt<'_, C, E> {
    fn drop(&mut self) {
        if !self.done {
            self.fail(uring_runtime::Error::Cancelled.into());
        }
    }
}

/// What a successful attempt hands to the in-memory state.
struct Outcome {
    durable: Durable,
    unit: u16,
    log: Vec<Retired>,
    log_pages: Vec<NodeAddr>,
    log_real: bool,
    released_pages: Vec<NodeAddr>,
    effects: Vec<(u32, u16, u32)>,
    extras: Vec<NodeAddr>,
    bitmaps: Vec<(u32, u8)>,
    pages: u64,
    writes: u64,
    count_fix: i64,
}

impl<C: Charge> Volume<C> {
    /// Make every index change made before this call durable.
    ///
    /// Concurrent callers share one commit attempt. A caller whose changes
    /// came after the running attempt started waits for it and then runs (or
    /// joins) a follow-up attempt. A failed attempt reports its error to every
    /// caller it covered; the volume stays usable and the next commit retries
    /// the same changes. Dropping the future of the caller running an attempt
    /// fails that attempt with `Cancelled` for the other callers.
    ///
    /// An attempt freezes the overlay, merges it into the trees (reading
    /// only the pages on changed paths), writes the new pages in contiguous
    /// runs, fsyncs the descriptors that saw writes (which also makes all
    /// published data durable), writes the superblock that does not hold the
    /// newest durable one, and fsyncs that descriptor.
    pub fn commit<'a, S: Scope, B: Budget>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        scope: &'a S,
    ) -> Operation<'a, (), S::Error>
    where
        S::Error: From<Error>,
    {
        let target = self.shared.state().mutations;
        // Retired space is released only by a later successful commit. When
        // some is pending, this call runs (or joins) one attempt that starts
        // after it, even with no new changes.
        let reclaim = {
            let state = self.shared.state();
            state.log_real || state.slots.has_ops() || !state.active.is_empty()
        };
        let (finished_at_call, attempts_at_call) = {
            let commit = self.commit.borrow();
            (commit.finished, commit.attempts)
        };
        Box::pin(async move {
            loop {
                let wait_for = {
                    let commit = self.commit.borrow();
                    if commit.durable_mutations >= target
                        && (!reclaim || commit.finished > attempts_at_call)
                    {
                        if let Some(failure) = &commit.failure
                            && reclaim
                            && failure.attempt == commit.finished
                            && failure.attempt > attempts_at_call
                        {
                            return Err(failure
                                .error
                                .downcast_ref::<S::Error>()
                                .copied()
                                .unwrap_or_else(|| uring_runtime::Error::Io.into()));
                        }
                        return Ok(());
                    }
                    if let Some(failure) = &commit.failure
                        && failure.attempt > finished_at_call
                        && failure.covered >= target
                    {
                        return Err(failure
                            .error
                            .downcast_ref::<S::Error>()
                            .copied()
                            .unwrap_or_else(|| uring_runtime::Error::Io.into()));
                    }
                    commit.running.then_some(commit.attempts)
                };
                match wait_for {
                    Some(attempt) => {
                        poll_fn(|cx| {
                            if self.commit.borrow().finished >= attempt {
                                Poll::Ready(())
                            } else {
                                self.signals.register(cx.waker());
                                Poll::Pending
                            }
                        })
                        .await
                    }
                    None => return self.attempt(reactor, scope).await,
                }
            }
        })
    }

    async fn attempt<S: Scope, B: Budget>(
        &self,
        reactor: &Reactor<S, B>,
        scope: &S,
    ) -> std::result::Result<(), S::Error>
    where
        S::Error: From<Error>,
    {
        let id = {
            let mut commit = self.commit.borrow_mut();
            commit.running = true;
            commit.attempts += 1;
            commit.attempts
        };
        let mut attempt = Attempt::<C, S::Error> {
            volume: self,
            id,
            covered: 0,
            slot_ops: None,
            claimed: Vec::new(),
            carry: Vec::new(),
            dirty: BTreeSet::new(),
            allocated: Vec::new(),
            superblock_started: false,
            done: false,
            error: PhantomData,
        };
        match self.run_attempt(reactor, scope, &mut attempt).await {
            Ok(outcome) => {
                attempt.done = true;
                self.succeed(&attempt, outcome);
                Ok(())
            }
            Err(error) => {
                attempt.fail(error);
                Err(error)
            }
        }
    }

    fn succeed<E: Copy + 'static + From<uring_runtime::Error>>(
        &self,
        attempt: &Attempt<'_, C, E>,
        outcome: Outcome,
    ) {
        {
            let mut state = self.shared.state();
            state.durable = outcome.durable;
            state.count_fix -= outcome.count_fix;
            state.epoch += 1;
            state.frozen = None;
            for addr in outcome.released_pages {
                state.slots.release_page(addr);
                self.pager.invalidate(addr);
            }
            for (slot, unit, incarnation) in outcome.effects {
                state.slots.release(slot, unit, incarnation);
            }
            for (slot, copy) in outcome.bitmaps {
                state.slots.set_node_copy(slot, copy, outcome.durable.seq);
            }
            state.log = outcome.log;
            state.log_pages = outcome.log_pages;
            state.log_real = outcome.log_real;
            state.carry.extend(outcome.extras);
            let pending = state.log.len() as u64 + state.carry.len() as u64;
            state.slots.set_pending(pending);
        }
        let mut commit = self.commit.borrow_mut();
        commit.running = false;
        commit.finished = attempt.id;
        commit.succeeded += 1;
        commit.durable_mutations = commit.durable_mutations.max(attempt.covered);
        commit.durable_unit = outcome.unit;
        commit.pages_written = outcome.pages;
        commit.writes = outcome.writes;
        drop(commit);
        self.signals.wake();
    }

    async fn run_attempt<S: Scope, B: Budget>(
        &self,
        reactor: &Reactor<S, B>,
        scope: &S,
        attempt: &mut Attempt<'_, C, S::Error>,
    ) -> std::result::Result<Outcome, S::Error>
    where
        S::Error: From<Error>,
    {
        // A canceled attempt may leave writes in flight. Wait for them so they
        // cannot land after this attempt reuses an address or superblock.
        poll_fn(|cx| {
            if self.signals.meta_writes.get() == 0 {
                Poll::Ready(())
            } else {
                self.signals.register(cx.waker());
                Poll::Pending
            }
        })
        .await;
        scope.check()?;

        // Freeze.
        let (frozen, base, log_prev, log_pages_prev, count_fix) = {
            let mut state = self.shared.state();
            let active = std::mem::take(&mut state.active);
            let frozen = match state.frozen.take() {
                Some(previous) => {
                    let mut merged = Rc::try_unwrap(previous).unwrap_or_else(|rc| (*rc).clone());
                    merged.absorb(active, &mut state.slots);
                    merged
                }
                None => active,
            };
            let frozen = Rc::new(frozen);
            state.frozen = Some(Rc::clone(&frozen));
            state.epoch += 1;
            attempt.slot_ops = Some(state.slots.take_ops());
            attempt.covered = state.mutations;
            attempt.carry = std::mem::take(&mut state.carry);
            (
                frozen,
                state.durable,
                state.log.clone(),
                state.log_pages.clone(),
                state.count_fix,
            )
        };
        let seq = {
            let mut commit = self.commit.borrow_mut();
            commit.seq += 1;
            commit.seq
        };
        let volume_id = self.volume_id.get();
        let io = self.io(reactor, scope);
        let mut writer = Writer::new(self, reactor, scope, seq);

        // Index.
        let index_ops: Vec<(Key, IndexOp)> = frozen.index.iter().map(|(k, e)| (*k, e.op)).collect();
        let mut releases: Vec<(Key, Value)> = Vec::new();
        let mut index_delta = 0i64;
        let mut resolve_index = |key: &Key, old: Option<&Raw>, op: &IndexOp| {
            let old = old.and_then(decode_value);
            let new = match op {
                IndexOp::Put(value) => Some(*value),
                IndexOp::Delete => None,
            };
            index_delta += i64::from(new.is_some()) - i64::from(old.is_some());
            if let Some(old) = old
                && new.is_none_or(|n| {
                    (n.slot, n.unit, n.generation) != (old.slot, old.unit, old.generation)
                })
            {
                releases.push((*key, old));
            }
            new.map(|v| value_raw(&v))
        };
        writer.kind = TreeKind::Index;
        let index = btree::apply(
            &io,
            &mut writer,
            &base.trees[TreeKind::Index.index()],
            TreeKind::Index,
            base.seq,
            &index_ops,
            &mut resolve_index,
        )
        .await;
        attempt.allocated.append(&mut writer.allocated);
        let index = index?;
        drop(index_ops);

        // Record map: published records, releases found above, and retired
        // records of the previous commit that are reusable now.
        let mut record_ops: BTreeMap<Key, RecOp> = frozen.records.clone();
        for (key, value) in releases {
            record_ops
                .entry(record_key(value.slot, value.unit))
                .or_insert(RecOp::Release {
                    key,
                    generation: value.generation,
                });
        }
        for retired in log_prev.iter().filter(|r| r.record) {
            record_ops
                .entry(record_key(retired.addr.slot(), retired.addr.unit()))
                .or_insert(RecOp::Retired {
                    incarnation: retired.incarnation,
                });
        }
        let record_ops: Vec<(Key, RecOp)> = record_ops.into_iter().collect();
        let mut deltas: HashMap<(u32, u32), i64> = HashMap::new();
        let mut effects: Vec<(u32, u16, u32)> = Vec::new();
        let mut retired_records: Vec<Retired> = Vec::new();
        let mut record_delta = 0i64;
        let mut resolve_records = |rk: &Key, old: Option<&Raw>, op: &RecOp| -> Option<Raw> {
            let (slot, unit) = record_key_parts(rk);
            let old = old.and_then(|raw| RecordDesc::decode(raw));
            let remove = |deltas: &mut HashMap<(u32, u32), i64>, desc: &RecordDesc| {
                *deltas.entry((slot, desc.incarnation)).or_default() -= 1;
            };
            let new = match op {
                RecOp::Put(desc) => {
                    if let Some(old) = &old {
                        remove(&mut deltas, old);
                    }
                    *deltas.entry((slot, desc.incarnation)).or_default() += 1;
                    Some(*desc)
                }
                RecOp::Delete => {
                    if let Some(old) = &old {
                        remove(&mut deltas, old);
                    }
                    None
                }
                RecOp::Release { key, generation } => match old {
                    Some(desc) if desc.key == *key && desc.generation == *generation => {
                        let storage = self
                            .shared
                            .state
                            .borrow()
                            .slots
                            .slot_state(slot)
                            .is_some_and(|s| {
                                matches!(
                                    s.kind,
                                    super::table::SlotKind::Data {
                                        tier: super::format::Tier::Storage,
                                        ..
                                    }
                                ) && s.incarnation == desc.incarnation
                            });
                        if storage {
                            retired_records.push(Retired {
                                addr: NodeAddr::new(slot, unit),
                                incarnation: desc.incarnation,
                                record: true,
                            });
                            Some(desc)
                        } else {
                            remove(&mut deltas, &desc);
                            effects.push((slot, unit, desc.incarnation));
                            None
                        }
                    }
                    other => other,
                },
                RecOp::Retired { incarnation } => match old {
                    Some(desc) if desc.incarnation == *incarnation => {
                        remove(&mut deltas, &desc);
                        effects.push((slot, unit, desc.incarnation));
                        None
                    }
                    other => other,
                },
            };
            record_delta += i64::from(new.is_some()) - i64::from(old.is_some());
            new.map(|desc| desc.encode())
        };
        writer.kind = TreeKind::Records;
        let records = btree::apply(
            &io,
            &mut writer,
            &base.trees[TreeKind::Records.index()],
            TreeKind::Records,
            base.seq,
            &record_ops,
            &mut resolve_records,
        )
        .await;
        attempt.allocated.append(&mut writer.allocated);
        let records = records?;
        drop(record_ops);
        writer.return_spare();
        attempt
            .allocated
            .retain(|a| self.shared.state().slots.page_used(*a));

        // Metadata pages the new root set stops referencing.
        let released_pages: Vec<NodeAddr> = log_prev
            .iter()
            .filter(|r| !r.record)
            .map(|r| r.addr)
            .collect();
        let mut base_freed: Vec<Retired> = index
            .freed
            .iter()
            .chain(&records.freed)
            .map(|addr| Retired {
                addr: *addr,
                incarnation: 0,
                record: false,
            })
            .collect();
        let real = !base_freed.is_empty() || !retired_records.is_empty();
        base_freed.extend(
            log_pages_prev
                .iter()
                .chain(&attempt.carry)
                .map(|addr| Retired {
                    addr: *addr,
                    incarnation: 0,
                    record: false,
                }),
        );
        base_freed.extend(retired_records.iter().copied());

        // Slot table and the allocation fixpoint: placing pages may claim
        // node slots, which changes the slot table, which may need pages.
        let released_slots: BTreeSet<u32> = released_pages.iter().map(|a| a.slot()).collect();
        let mut reserve: Vec<NodeAddr> = Vec::new();
        let index_order = btree::placement_order(&index);
        let records_order = btree::placement_order(&records);
        let tree_need = btree::addresses_needed(&index, &index_order)
            + btree::addresses_needed(&records, &records_order);
        let (slots_applied, slots_order, slots_delta, log, log_page_count) = loop {
            let claimed = self.shared.state().slots.take_claimed();
            attempt.claimed.extend(claimed);
            let mut changes: BTreeMap<u32, SlotOp> = attempt.slot_ops.clone().unwrap_or_default();
            for (slot, incarnation) in &attempt.claimed {
                changes.entry(*slot).or_default().merge(&SlotOp {
                    ident: Some(Ident::Node(*incarnation)),
                    ..SlotOp::default()
                });
            }
            for (slot, _) in deltas.keys() {
                changes.entry(*slot).or_default();
            }
            // Bitmaps this commit writes: the copy that is not durable.
            let written: Vec<(u32, u8)> = {
                let state = self.shared.state();
                let mut dirty = state.slots.dirty_nodes().clone();
                dirty.extend(released_slots.iter().copied());
                dirty
                    .into_iter()
                    .filter_map(|slot| {
                        state
                            .slots
                            .node_copy(slot)
                            .map(|(copy, _, _)| (slot, 1 - copy))
                    })
                    .collect()
            };
            for (slot, copy) in &written {
                changes.entry(*slot).or_default().merge(&SlotOp {
                    bitmap: Some((*copy, seq)),
                    ..SlotOp::default()
                });
            }
            let ops: Vec<(Key, (u32, SlotOp))> = changes
                .into_iter()
                .map(|(slot, op)| (slot_key(slot), (slot, op)))
                .collect();
            let mut slots_delta = 0i64;
            let mut resolve_slots = |_: &Key, old: Option<&Raw>, (slot, op): &(u32, SlotOp)| {
                let old = old.and_then(|raw| SlotSummary::decode(&raw[..32]));
                let mut new = op.apply(old);
                if let Some(summary) = new.as_mut()
                    && summary.kind == SLOT_DATA
                {
                    let base = match (op.ident, old) {
                        (None, Some(old)) if old.incarnation == summary.incarnation => {
                            i64::from(old.used)
                        }
                        _ => 0,
                    };
                    let delta = deltas
                        .get(&(*slot, summary.incarnation))
                        .copied()
                        .unwrap_or(0);
                    let records = class_records(summary.class).unwrap_or(0);
                    summary.used = (base + delta).clamp(0, i64::from(records)) as u32;
                }
                slots_delta += i64::from(new.is_some()) - i64::from(old.is_some());
                new.map(|s| s.encode())
            };
            let applied: Applied = btree::apply(
                &io,
                &mut Deferred(PhantomData),
                &base.trees[TreeKind::Slots.index()],
                TreeKind::Slots,
                base.seq,
                &ops,
                &mut resolve_slots,
            )
            .await?;
            let order = btree::placement_order(&applied);
            let mut log = base_freed.clone();
            log.extend(applied.freed.iter().map(|addr| Retired {
                addr: *addr,
                incarnation: 0,
                record: false,
            }));
            let log_page_count = log.len().div_ceil(LOG_CAPACITY);
            let need = tree_need + btree::addresses_needed(&applied, &order) + log_page_count;
            if reserve.len() >= need {
                break (applied, order, slots_delta, log, log_page_count);
            }
            let more = self.shared.state().slots.alloc_pages(need - reserve.len());
            attempt.allocated.extend(more.iter().copied());
            if reserve.len() + more.len() < need {
                reserve.extend(more);
                return Err(Error::Busy.into());
            }
            reserve.extend(more);
        };
        // Slot table pages freed only to record bitmap copies do not make
        // the volume dirty; otherwise releasing them would never settle.
        let user_slot_changes = attempt.slot_ops.as_ref().is_some_and(|ops| !ops.is_empty())
            || !deltas.is_empty()
            || !attempt.claimed.is_empty();
        let real = real || (user_slot_changes && !slots_applied.freed.is_empty());
        let mut dirty = self.shared.state().slots.take_dirty_nodes();
        dirty.extend(released_slots.iter().copied());
        attempt.dirty = dirty.clone();
        reserve.sort();
        let need =
            tree_need + btree::addresses_needed(&slots_applied, &slots_order) + log_page_count;
        let extras = reserve.split_off(need);
        let mut addrs = reserve.into_iter();

        // Place and write the tree pages, children first.
        let mut roots = [TreeRoot::EMPTY; 3];
        let counts = [
            (base.trees[0].count as i64 + index_delta + count_fix).max(0) as u64,
            (base.trees[1].count as i64 + record_delta).max(0) as u64,
            (base.trees[2].count as i64 + slots_delta).max(0) as u64,
        ];
        for (kind, applied, order) in [
            (TreeKind::Index, &index, &index_order),
            (TreeKind::Records, &records, &records_order),
            (TreeKind::Slots, &slots_applied, &slots_order),
        ] {
            let mut placed = vec![[NodeAddr::NONE; 2]; applied.pages.len()];
            for &i in order.iter() {
                let page = &applied.pages[i];
                let copies = if page.level == 0 { 1 } else { 2 };
                let mut at = [NodeAddr::NONE; 2];
                for slot in at.iter_mut().take(copies) {
                    let addr = addrs.next().expect("reserved");
                    let placed_ref = &placed;
                    writer
                        .page(addr, true, |buf| {
                            btree::encode_new(buf, page, placed_ref, kind, &volume_id, seq, addr)
                        })
                        .await?;
                    *slot = addr;
                }
                placed[i] = at;
            }
            let root = applied.root.map_or([NodeAddr::NONE; 2], |child| {
                btree::resolve_child(&child, &placed)
            });
            roots[kind.index()] = TreeRoot {
                root,
                level: if root[0].is_none() { 0 } else { applied.level },
                count: if root[0].is_none() {
                    0
                } else {
                    counts[kind.index()]
                },
            };
        }

        // Retired log, chained.
        let log_pages: Vec<NodeAddr> = (0..log_page_count)
            .map(|_| addrs.next().expect("reserved"))
            .collect();
        for (i, addr) in log_pages.iter().enumerate() {
            let next = log_pages.get(i + 1).copied().unwrap_or(NodeAddr::NONE);
            let chunk = &log[i * LOG_CAPACITY..((i + 1) * LOG_CAPACITY).min(log.len())];
            writer
                .page(*addr, false, |buf| {
                    encode_log(buf, &volume_id, seq, *addr, next, chunk)
                })
                .await?;
        }

        // Node-slot bitmaps: the copy that is not durable, with the units
        // this commit releases already clear.
        let mut bitmaps = Vec::new();
        for slot in &dirty {
            let words = {
                let state = self.shared.state();
                let Some((copy, _, _)) = state.slots.node_copy(*slot) else {
                    continue;
                };
                let mut words = *state.slots.node_words(*slot).expect("node words");
                for addr in released_pages.iter().filter(|a| a.slot() == *slot) {
                    let unit = u32::from(addr.unit());
                    if unit < SLOT_UNITS {
                        words[(unit / 64) as usize] &= !(1 << (unit % 64));
                    }
                }
                bitmaps.push((*slot, 1 - copy));
                words
            };
            let copy = bitmaps.last().expect("pushed").1;
            let addr = NodeAddr::new(*slot, bitmap_unit(*slot, copy));
            writer
                .page(addr, false, |buf| {
                    encode_bitmap(buf, &volume_id, seq, addr, *slot, &words)
                })
                .await?;
        }
        writer.finish().await?;

        // Barrier 1: every page and every published record durable.
        self.slab.sync_dirty(reactor, scope).await?;

        // Barrier 2: the superblock.
        let next_generation = self.shared.state().next_generation;
        let next_incarnation = self.shared.state().slots.next_incarnation();
        let superblock = Superblock {
            volume_id,
            seq,
            unit_bytes: UNIT_BYTES as u32,
            slot_units: SLOT_UNITS,
            slot_count: self.slot_count,
            next_generation,
            next_incarnation,
            trees: roots,
            log_head: log_pages.first().copied().unwrap_or(NodeAddr::NONE),
            log_entries: if log_pages.is_empty() {
                0
            } else {
                log.len() as u64
            },
        };
        let unit = other_unit(self.commit.borrow().durable_unit);
        attempt.superblock_started = true;
        self.write_superblock(reactor, scope, unit, &superblock)
            .await?;
        self.slab
            .sync_at(reactor, unit_offset(0, unit), scope)
            .await?;
        let log_real = real;
        Ok(Outcome {
            durable: Durable { seq, trees: roots },
            unit,
            log,
            log_pages,
            log_real,
            released_pages,
            effects,
            extras,
            bitmaps,
            pages: writer.pages,
            writes: writer.writes + 1,
            count_fix,
        })
    }

    /// Write one superblock unit.
    pub(super) async fn write_superblock<S: Scope, B: Budget>(
        &self,
        reactor: &Reactor<S, B>,
        scope: &S,
        unit: u16,
        superblock: &Superblock,
    ) -> std::result::Result<(), S::Error>
    where
        S::Error: From<Error>,
    {
        let mut buffer = self.slab.allocate(UNIT_BYTES, (self.charge)(UNIT_BYTES)?)?;
        superblock.encode_into(buffer.as_mut_slice());
        let guard = MetaWrite::new(&self.signals);
        let _ = self
            .slab
            .write_at(reactor, unit_offset(0, unit), buffer, guard, scope)
            .await?;
        Ok(())
    }
}
