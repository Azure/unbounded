// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Frozen checkpoint ownership through data sync, root write, and final sync.
use super::*;
pub(super) enum Job {
    Page(Box<uring::Page>, u64),
    Value(Rc<PayloadExtent>),
    Sync,
}
pub(super) trait Storage {
    type Ticket;
    fn submit(&mut self, job: Job) -> Result<Self::Ticket, uring::Rejected<Job>>;
    fn complete(&mut self, ticket: &mut Self::Ticket) -> io::Result<Option<io::Result<()>>>;
}
pub(super) enum IoTicket {
    Page(uring::Ticket<uring::PageIo>),
    Punch(uring::Ticket<uring::PunchHole>, Rc<PayloadExtent>),
    Detached(Rc<PayloadExtent>),
    Value(uring::Ticket<uring::Write>, Rc<PayloadExtent>),
    Sync(uring::Ticket<uring::Control>),
}
pub(super) struct RingIo<'a> {
    pub(super) ring: &'a mut Ring,
    pub(super) file: uring::File,
    pub(super) space: Rc<Space>,
}
impl Storage for RingIo<'_> {
    type Ticket = IoTicket;
    fn submit(&mut self, job: Job) -> Result<IoTicket, uring::Rejected<Job>> {
        match job {
            Job::Page(page, offset) => match self.ring.write_page(
                self.file.clone().into(),
                page,
                FileOffset::new(offset).unwrap(),
            ) {
                Ok(ticket) => {
                    self.ring.retain(&ticket, self.space.clone());
                    Ok(IoTicket::Page(ticket))
                }
                Err(e) => Err(uring::Rejected {
                    error: e.error,
                    resource: Job::Page(e.resource, offset),
                }),
            },
            Job::Value(value) => {
                match self.ring.punch_hole(
                    self.file.clone().into(),
                    FileOffset::new(value.allocation.offset()).unwrap(),
                    WIDE,
                    value.allocation.clone(),
                ) {
                    Ok(ticket) => Ok(IoTicket::Punch(ticket, value)),
                    Err(error) => Err(uring::Rejected {
                        error,
                        resource: Job::Value(value),
                    }),
                }
            }
            Job::Sync => match self.ring.sync_data(self.file.clone().into()) {
                Ok(ticket) => {
                    self.ring.retain(&ticket, self.space.clone());
                    Ok(IoTicket::Sync(ticket))
                }
                Err(error) => Err(uring::Rejected {
                    error,
                    resource: Job::Sync,
                }),
            },
        }
    }
    fn complete(&mut self, ticket: &mut IoTicket) -> io::Result<Option<io::Result<()>>> {
        fn exact(result: io::Result<usize>, len: usize) -> io::Result<()> {
            if result? != len {
                return Err(io::Error::other("short slab write or invalid sync result"));
            }
            Ok(())
        }
        Ok(match ticket {
            IoTicket::Punch(t, value) => {
                match self.ring.take_punch(t)? {
                    None => return Ok(None),
                    Some(Err(error)) => return Ok(Some(Err(error))),
                    Some(Ok(())) => *ticket = IoTicket::Detached(value.clone()),
                }
                self.complete(ticket)?
            }
            IoTicket::Detached(value) => {
                let buffer = value.buffer.borrow().as_ref().unwrap().clone();
                match self.ring.write(
                    self.file.clone().into(),
                    buffer,
                    BufferRange::new(0..value.info.len).unwrap(),
                    FileOffset::new(value.allocation.offset()).unwrap(),
                ) {
                    Ok(t) => {
                        self.ring.retain(&t, value.allocation.clone());
                        *ticket = IoTicket::Value(t, value.clone());
                        None
                    }
                    Err(e) if e.error.kind() == io::ErrorKind::WouldBlock => None,
                    Err(e) => Some(Err(e.error)),
                }
            }
            IoTicket::Page(t) => self.ring.take_page(t)?.map(|c| exact(c.result, PAGE_SIZE)),
            IoTicket::Value(t, value) => self.ring.take_write(t)?.map(|c| {
                exact(c.result, value.info.len)?;
                value.written.set(true);
                value.buffer.borrow_mut().take();
                Ok(())
            }),
            IoTicket::Sync(t) => self.ring.take_control(t)?.map(|c| exact(c.result, 0)),
        })
    }
}

// States own the checkpoint. Barrier collection and state construction stay in
// this module; the allocator cannot manufacture persistence evidence.
pub(super) struct Writes {
    checkpoint: Checkpoint,
    // Admissions covered by this frozen batch, including superseded values.
    // Later admissions stay in Allocator::charged until their own final sync.
    charged: u64,
    slot: usize,
    jobs: VecDeque<Job>,
    active: VecDeque<IoTicket>,
}
#[cfg(test)]
impl Writes {
    pub(super) fn into_checkpoint(self) -> (usize, Checkpoint) {
        (self.slot, self.checkpoint)
    }
    pub(super) fn jobs(&self) -> &VecDeque<Job> {
        &self.jobs
    }
}
pub(super) struct DataSync {
    checkpoint: Checkpoint,
    charged: u64,
    slot: usize,
    ticket: Option<IoTicket>,
}
pub(super) struct DataSynced {
    checkpoint: Checkpoint,
    charged: u64,
    slot: usize,
    ticket: Option<IoTicket>,
}
pub(super) struct MagicWritten {
    checkpoint: Checkpoint,
    charged: u64,
    slot: usize,
    ticket: Option<IoTicket>,
}
pub(super) enum Pipeline {
    Writes(Writes),
    DataSync(DataSync),
    DataSynced(DataSynced),
    MagicWritten(MagicWritten),
}

impl Writes {
    fn begin_data_sync(self) -> DataSync {
        debug_assert!(self.jobs.is_empty() && self.active.is_empty());
        DataSync {
            checkpoint: self.checkpoint,
            charged: self.charged,
            slot: self.slot,
            ticket: None,
        }
    }
}
impl DataSync {
    fn data_synced(self) -> DataSynced {
        DataSynced {
            checkpoint: self.checkpoint,
            charged: self.charged,
            slot: self.slot,
            ticket: None,
        }
    }
}
impl DataSynced {
    fn root_written(self) -> MagicWritten {
        MagicWritten {
            checkpoint: self.checkpoint,
            charged: self.charged,
            slot: self.slot,
            ticket: None,
        }
    }
}
impl MagicWritten {
    // Called only after successful final-sync collection. This is the sole
    // publication point that releases the predecessor and admission charge.
    fn publish_durable(self, allocator: &mut Allocator) {
        allocator.checkpoints[self.slot] = Some(self.checkpoint);
        allocator.release_capacity(self.charged);
        allocator.checkpoint_permit = None;
        allocator.diagnostics.counts[4] = allocator.diagnostics.counts[4].wrapping_add(1);
    }
}

fn freeze(node: &mut Rc<Node>, space: &Rc<Space>, jobs: &mut VecDeque<Job>) -> io::Result<()> {
    if node.disk.is_some() || node.len() == 0 {
        return Ok(());
    }
    let node = Rc::make_mut(node);
    match &mut node.body {
        Body::Leaf(values) => {
            for (_, value) in values {
                if let Entry::Payload(value) = value
                    && !value.written.get()
                {
                    jobs.push_back(Job::Value(value.clone()));
                }
            }
        }
        Body::Branch(children) => {
            for child in children {
                freeze(child, space, jobs)?;
            }
        }
    }
    let allocation = space.allocate(Class::Index)?;
    jobs.push_back(Job::Page(encode(node), allocation.offset()));
    node.disk = Some(allocation);
    Ok(())
}
// Visit changed paths only. Splits/merges may visit their small neighboring
// subtrees; unchanged disk nodes are shared and terminate traversal immediately.
fn changed_bits(node: &Node, other: Option<&Node>, used: &mut [u8], occupied: bool) {
    if let (Some(disk), Some(other)) = (&node.disk, other)
        && other.disk.as_ref().is_some_and(|a| a.page() == disk.page())
    {
        return;
    }
    let mut set = |page: usize, count: usize| {
        for i in page..page + count {
            if occupied {
                used[i / 8] |= 1 << (i % 8);
            } else {
                used[i / 8] &= !(1 << (i % 8));
            }
        }
    };
    if let Some(disk) = &node.disk {
        set(disk.page(), 1);
    }
    match &node.body {
        Body::Leaf(values) => {
            for (_, value) in values {
                if let Entry::Payload(value) = value {
                    set(value.allocation.page(), 1024);
                }
            }
        }
        Body::Branch(children) => {
            for child in children {
                let peer = other.and_then(|other| match &other.body {
                    Body::Branch(peers) => peers
                        .binary_search_by_key(&child.first(), |p| p.first())
                        .ok()
                        .map(|i| peers[i].as_ref()),
                    _ => None,
                });
                changed_bits(child, peer, used, occupied);
            }
        }
    }
}
impl Allocator {
    pub(super) fn prepare(&mut self) -> io::Result<Pipeline> {
        let generation = self
            .generation()
            .checked_add(1)
            .ok_or_else(|| invalid("checkpoint generation exhausted"))?;
        let slot = if self.checkpoints[0].as_ref().map_or(0, |c| c.generation)
            < self.checkpoints[1].as_ref().map_or(0, |c| c.generation)
        {
            0
        } else {
            1
        };
        let mut root = self.root.clone();
        let mut jobs = VecDeque::new();
        freeze(&mut root, &self.space, &mut jobs)?;
        let mut used = vec![0; self.space.geometry.pages().div_ceil(8)];
        let latest = self
            .checkpoints
            .iter()
            .flatten()
            .max_by_key(|c| c.generation)
            .unwrap();
        for (chunk, (_, bitmap)) in used.chunks_mut(BIT_BYTES).zip(&latest.bitmaps) {
            chunk.copy_from_slice(&bitmap.0[32..32 + chunk.len()]);
        }
        changed_bits(&latest.root, Some(&root), &mut used, false);
        changed_bits(&root, Some(&latest.root), &mut used, true);
        let mut bitmaps = Vec::new();
        for (i, chunk) in used.chunks(BIT_BYTES).enumerate() {
            let mut bitmap = Box::new(uring::Page([0; PAGE_SIZE]));
            put(&mut bitmap, 0, BITS);
            put(&mut bitmap, 16, i as u64);
            bitmap.0[32..32 + chunk.len()].copy_from_slice(chunk);
            seal(&mut bitmap);
            let allocation = if let Some((allocation, old)) = latest.bitmaps.get(i)
                && old.0 == bitmap.0
            {
                allocation.clone()
            } else {
                let allocation = self.space.allocate(Class::Index)?;
                jobs.push_back(Job::Page(
                    Box::new(uring::Page(bitmap.0)),
                    allocation.offset(),
                ));
                allocation
            };
            bitmaps.push((allocation, bitmap));
        }
        // Commit prepared locations to the live tree only after all reservations
        // succeed. Subsequent mutations CoW just the shared in-memory path.
        self.root = root.clone();
        // Any not-yet-submitted values are now owned by this frozen batch.
        self.pending.clear();
        self.changed = false;
        self.rotate = generation < self.reclaim_until;
        self.diagnostics.counts[3] = self.diagnostics.counts[3].wrapping_add(1);
        Ok(Pipeline::Writes(Writes {
            charged: self.charged,
            checkpoint: Checkpoint {
                generation,
                root,
                bitmaps,
            },
            slot,
            jobs,
            active: VecDeque::new(),
        }))
    }
    pub(super) fn progress(
        &mut self,
        io: &mut impl Storage<Ticket = IoTicket>,
        budget: usize,
    ) -> io::Result<bool> {
        self.healthy()?;
        // Remain poisoned on errors AND unwinding. A partially submitted batch
        // may still publish a magic page; never resume allocation after ambiguity.
        self.failed = true;
        let result = self.progress_inner(io, budget);
        if result.is_ok() {
            self.failed = false;
        }
        result
    }
    fn progress_inner(
        &mut self,
        io: &mut impl Storage<Ticket = IoTicket>,
        budget: usize,
    ) -> io::Result<bool> {
        let mut runnable = false;
        for _ in 0..budget.min(self.publishing.len()) {
            let mut ticket = self.publishing.pop_front().unwrap();
            match io.complete(&mut ticket)? {
                None => self.publishing.push_back(ticket),
                Some(result) => {
                    result?;
                    runnable = true;
                }
            }
        }
        // Publication is independent of checkpoint fsyncs. A checkpoint is frozen
        // only after its values are readable, preserving data-before-root order.
        let checkpoint_io = match &self.pipeline {
            Some(Pipeline::Writes(writes)) => writes.active.len(),
            Some(_) => 1,
            None => 0,
        };
        // Obsolete unflushed versions never reach disk. Queue ownership bounds
        // retained buffers, and dropping one cannot affect outstanding requests.
        for _ in 0..budget.min(self.pending.len()) {
            let (key, weak) = self.pending.pop_front().unwrap();
            if let Some(value) = weak.upgrade()
                && !value.written.get()
                && self
                    .root
                    .get(&key)
                    .and_then(Entry::payload)
                    .is_some_and(|live| Rc::ptr_eq(live, &value))
            {
                if self.publishing.len() + checkpoint_io >= self.config.max_io {
                    self.pending.push_back((key, weak));
                    continue;
                }
                match io.submit(Job::Value(value)) {
                    Ok(ticket) => {
                        self.publishing.push_back(ticket);
                        runnable = true;
                    }
                    Err(error) => {
                        self.pending.push_front((key, weak));
                        if error.error.kind() != io::ErrorKind::WouldBlock {
                            return Err(error.error);
                        }
                        break;
                    }
                }
            }
        }
        if self.pipeline.is_none()
            && self.pending.is_empty()
            && self.publishing.is_empty()
            && (self.changed || self.rotate)
        {
            // Return through progress() to clear the failure latch before
            // poll() selects victims. Do not let prepare bypass that opportunity
            // when the last write completes during this turn.
            if !self.maintenance_yielded {
                self.maintenance_yielded = true;
                return Ok(true);
            }
            let permit = self.pressure.1.lock().unwrap().acquire();
            let Some(permit) = permit else {
                // Cache maintenance polls every shard. Stay runnable so release
                // on another worker cannot leave this shard asleep indefinitely.
                return Ok(true);
            };
            self.pipeline = Some(self.prepare()?);
            self.checkpoint_permit = Some(permit);
            self.maintenance_yielded = false;
        }
        let Some(pipeline) = self.pipeline.take() else {
            return Ok(runnable || !self.pending.is_empty());
        };
        let next = match pipeline {
            Pipeline::Writes(mut writes) => {
                runnable |= writes.active.len() > budget;
                for _ in 0..budget.min(writes.active.len()) {
                    let mut ticket = writes.active.pop_front().unwrap();
                    match io.complete(&mut ticket)? {
                        None => writes.active.push_back(ticket),
                        Some(result) => {
                            result?;
                            runnable = true;
                        }
                    }
                }
                for _ in 0..budget {
                    if writes.active.len() + self.publishing.len() >= self.config.max_io {
                        break;
                    }
                    let Some(job) = writes.jobs.pop_front() else {
                        break;
                    };
                    match io.submit(job) {
                        Ok(ticket) => {
                            writes.active.push_back(ticket);
                            runnable = true;
                        }
                        Err(e) => {
                            writes.jobs.push_front(e.resource);
                            if e.error.kind() != io::ErrorKind::WouldBlock {
                                return Err(e.error);
                            }
                            break;
                        }
                    }
                }
                if writes.jobs.is_empty() && writes.active.is_empty() {
                    runnable = true;
                    Some(Pipeline::DataSync(writes.begin_data_sync()))
                } else {
                    Some(Pipeline::Writes(writes))
                }
            }
            Pipeline::DataSync(mut sync) => {
                if advance(io, &mut sync.ticket, || Job::Sync)? {
                    runnable = true;
                    Some(Pipeline::DataSynced(sync.data_synced()))
                } else {
                    Some(Pipeline::DataSync(sync))
                }
            }
            Pipeline::DataSynced(mut sync) => {
                if advance(io, &mut sync.ticket, || {
                    let c = &sync.checkpoint;
                    let bitmaps: Vec<_> = c.bitmaps.iter().map(|(a, _)| a.clone()).collect();
                    Job::Page(
                        magic(
                            self.space.geometry,
                            c.generation,
                            c.root.disk.as_ref().map_or(0, |a| a.page() as u64),
                            &bitmaps,
                        ),
                        self.space.geometry.offset(sync.slot),
                    )
                })? {
                    runnable = true;
                    Some(Pipeline::MagicWritten(sync.root_written()))
                } else {
                    Some(Pipeline::DataSynced(sync))
                }
            }
            Pipeline::MagicWritten(mut written) => {
                if advance(io, &mut written.ticket, || Job::Sync)? {
                    // Only successful final-sync collection retires this batch's
                    // reservation. Failure/quarantine retains all charged bytes.
                    written.publish_durable(self);
                    runnable = true;
                    None
                } else {
                    Some(Pipeline::MagicWritten(written))
                }
            }
        };
        self.pipeline = next;
        Ok(runnable)
    }
}
fn advance(
    io: &mut impl Storage<Ticket = IoTicket>,
    ticket: &mut Option<IoTicket>,
    job: impl FnOnce() -> Job,
) -> io::Result<bool> {
    if let Some(ticket) = ticket {
        return match io.complete(ticket)? {
            Some(result) => result.map(|()| true),
            None => Ok(false),
        };
    }
    match io.submit(job()) {
        Ok(submitted) => *ticket = Some(submitted),
        Err(e) if e.error.kind() == io::ErrorKind::WouldBlock => {}
        Err(e) => return Err(e.error),
    }
    Ok(false)
}
