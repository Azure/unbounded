//! Node-wide checkpoint cuts: recover before admission, publish while running or drained.
//!
//! Recovery validates every shard before installation and seeds the periodic slot
//! sequence. The worker lifecycle decides when to poll or finish a cut; its writer
//! gating and shutdown drain predicates remain in app.rs.

use super::*;
use crate::runtime::HashMap;
use crate::runtime::HashSet;
use crate::store::checkpoint::CheckpointImage;

/// Shared collection, publication, and acknowledgment state for one node cut.
#[derive(Default)]
pub(super) struct CheckpointCut {
    shards: Vec<ShardImage>,

    phase: CutPhase,

    periodic_generation: u64,

    periodic_started: Option<std::time::Instant>,

    periodic_finished: usize,

    /// Highest recovered or reserved sequence, including failed publications.
    pub(super) last_sequence: u64,

    /// Last successful slot, or the initial periodic slot-selection baseline.
    last_slot: usize,
}

/// Publication ownership survives its result until every worker acknowledges it.
#[derive(Default)]
enum CutPhase {
    #[default]
    Collecting,

    Aborted(Error),

    Publishing,

    Published(Result<()>),
}

/// The two callers deliberately choose different slots for their first cold cut.
#[derive(Clone, Copy)]
enum CheckpointPolicy {
    Periodic,

    Shutdown,
}

/// A reserved sequence and slot carried through I/O before the slot is committed.
#[derive(Debug, PartialEq, Eq)]
pub(super) struct Publication {
    sequence: u64,

    slot: usize,
}

impl CheckpointCut {
    /// Report the shared result without forgetting whether publication started.
    fn result(&self) -> Option<Result<()>> {
        match self.phase {
            CutPhase::Collecting | CutPhase::Publishing => None,
            CutPhase::Aborted(error) => Some(Err(error)),
            CutPhase::Published(result) => Some(result),
        }
    }

    /// Keep publication ownership latched even after an I/O or setup failure.
    fn publication_started(&self) -> bool {
        matches!(self.phase, CutPhase::Publishing | CutPhase::Published(_))
    }

    /// Record collection failure or publication completion without changing owners.
    fn finish(&mut self, result: Result<()>) {
        self.shards.clear();
        self.phase = if self.publication_started() {
            CutPhase::Published(result)
        } else {
            CutPhase::Aborted(result.expect_err("collection cannot publish successfully"))
        };
    }

    /// Share a shard close or checkpoint error without resetting publication ownership.
    /// A close error replaces the shared result, not the publication task or its fence.
    pub(super) fn fail(&mut self, error: Error) {
        self.finish(Err(error));
    }

    /// Transfer collected images to the sole publishing worker.
    fn start_publication(&mut self) -> Vec<ShardImage> {
        debug_assert!(matches!(self.phase, CutPhase::Collecting));
        self.phase = CutPhase::Publishing;
        std::mem::take(&mut self.shards)
    }

    /// Reserve a sequence once; failed attempts never reuse their sequence.
    fn reserve(&mut self, policy: CheckpointPolicy) -> Result<Publication> {
        let sequence = self
            .last_sequence
            .checked_add(1)
            .ok_or(Error::Unavailable)?;
        if self.last_slot > 1 {
            return Err(Error::Unavailable);
        }
        let slot = if matches!(policy, CheckpointPolicy::Shutdown) && self.last_sequence == 0 {
            0
        } else {
            self.last_slot ^ 1
        };
        self.last_sequence = sequence;
        Ok(Publication { sequence, slot })
    }

    /// Advance the slot only for successful I/O belonging to the current reservation.
    fn published(&mut self, publication: Publication) -> Result<()> {
        if publication.sequence != self.last_sequence || publication.slot > 1 {
            return Err(Error::Unavailable);
        }
        self.last_slot = publication.slot;
        Ok(())
    }

    /// Start a new periodic collection after the caller checks timing and acknowledgments.
    fn restart(&mut self, now: std::time::Instant) -> Result<()> {
        let generation = self
            .periodic_generation
            .checked_add(1)
            .ok_or(Error::Unavailable)?;
        self.periodic_generation = generation;
        self.periodic_started = Some(now);
        self.periodic_finished = 0;
        self.shards.clear();
        self.phase = CutPhase::Collecting;
        Ok(())
    }

    /// Fence new write batches while a periodic cut still owns its snapshots.
    pub(super) fn blocks_writer(&self) -> bool {
        self.periodic_generation != 0 && self.result().is_none()
    }
}

/// Startup rendezvous for geometry validation and all-worker installation.
#[derive(Default)]
pub(super) struct RecoveryCut {
    geometry: HashMap<WorkerId, CheckpointGeometry>,

    selected: bool,

    shards: HashMap<WorkerId, ShardImage>,

    failure: Option<Error>,

    installed: HashSet<WorkerId>,
}

impl WorkerApplication {
    /// Publish the final drained cut, retaining every shard freeze through completion.
    pub(super) async fn checkpoint(&self, deadline: &RequestScope) -> Result<()> {
        let node = &self.node;
        let mut image = match self
            .store
            .checkpoint
            .snapshot_incremental(self.checkpoint_budget / (4 * node.count))
            .await
        {
            Ok(image) => image,
            Err(error) => {
                node.checkpoint
                    .lock()
                    .map_err(|_| Error::Unavailable)?
                    .fail(error);
                return Err(error);
            }
        };
        image.index.entries.retain(|(page, entry)| {
            self.caches
                .iter()
                .any(|c| c.id == page.version.object.cache)
                && self
                    .keys
                    .lease(
                        Some(&page.version.object.cache),
                        entry.key_id,
                        KeyPurpose::Page,
                    )
                    .is_ok()
        });
        image.index.metadata.retain(|m| {
            self.caches.iter().any(|c| c.id == m.version.object.cache)
                && self
                    .keys
                    .active(&m.version.object.cache, KeyPurpose::Page)
                    .is_ok()
        });
        {
            let mut cut = node.checkpoint.lock().map_err(|_| Error::Unavailable)?;
            if let Some(result) = cut.result() {
                self.store.checkpoint.finish_snapshot();
                return result;
            }
            cut.shards.push(image);
        }
        let mut publication: Option<Operation<'_, ()>> = None;
        let result = std::future::poll_fn(|cx| {
            if let Some(publish) = publication.as_mut() {
                if let Poll::Ready(result) = std::pin::Pin::as_mut(publish).poll(cx) {
                    node.checkpoint
                        .lock()
                        .map_err(|_| Error::Unavailable)?
                        .finish(result);
                    return Poll::Ready(result);
                }
                return Poll::Pending;
            }
            let mut cut = node.checkpoint.lock().map_err(|_| Error::Unavailable)?;
            if let Some(result) = cut.result() {
                return Poll::Ready(result);
            }
            if cut.publication_started() {
                cx.waker().wake_by_ref();
                return Poll::Pending;
            }
            if let Err(error) = deadline.check() {
                cut.fail(error);
                return Poll::Ready(Err(error));
            }
            if self.worker == node.control_worker && cut.shards.len() == node.count {
                let shards = cut.start_publication();
                drop(cut);
                publication = Some(Box::pin(async move {
                    // Drain has fenced the periodic task on every shard. Reuse its
                    // recovered sequence/slot rather than scanning or publishing
                    // checkpoint files synchronously on the worker thread.
                    let publication = node
                        .periodic_checkpoint
                        .lock()
                        .map_err(|_| Error::Unavailable)?
                        .reserve(CheckpointPolicy::Shutdown)?;
                    self.store
                        .checkpoint
                        .publish_async(
                            shards,
                            self.runtime.reactor.clone(),
                            deadline.clone(),
                            publication.sequence,
                            publication.slot,
                            self.checkpoint_budget,
                        )?
                        .await?;
                    node.periodic_checkpoint
                        .lock()
                        .map_err(|_| Error::Unavailable)?
                        .published(publication)?;
                    Ok(())
                }));
            }
            cx.waker().wake_by_ref();
            Poll::Pending
        })
        .await;
        self.store.checkpoint.finish_snapshot();
        result
    }

    /// Advance one periodic cut without changing writer drain or shutdown ownership.
    pub(super) fn poll_checkpoint(&mut self, cx: &mut Context<'_>) -> Result<()> {
        let node = &self.node;
        if !self.started {
            return Ok(());
        }
        let now = uring_runtime::environment::now();
        if let Some(task) = self.checkpoint_task.as_mut()
            && let Poll::Ready(result) = task.as_mut().poll(cx)
        {
            self.checkpoint_task = None;
            let mut cut = node
                .periodic_checkpoint
                .lock()
                .map_err(|_| Error::Unavailable)?;
            let result = result.and_then(|publication| cut.published(publication));
            cut.finish(result);
        }
        let mut cut = node
            .periodic_checkpoint
            .lock()
            .map_err(|_| Error::Unavailable)?;
        if cut.periodic_started.is_none() {
            cut.periodic_started = Some(now);
        }
        if cut.periodic_generation == 0 || cut.periodic_finished == node.count {
            if self.stopping {
                return Ok(());
            }
            if now.saturating_duration_since(cut.periodic_started.unwrap()) < Duration::from_secs(5)
            {
                return Ok(());
            }
            cut.restart(now)?;
        }
        let generation = cut.periodic_generation;
        if self.stopping && !cut.publication_started() {
            cut.fail(Error::Cancelled);
        }
        if cut.result().is_some() {
            self.checkpoint_snapshot.take();
            if self.checkpoint_completed != generation {
                self.store.checkpoint.finish_snapshot();
                self.checkpoint_generation = 0;
                self.checkpoint_completed = generation;
                cut.periodic_finished += 1;
            }
            return Ok(());
        }
        if self.checkpoint_generation != generation {
            // Stop starting batches while existing SQEs drain, then freeze only
            // this shard. No queue-idle requirement can starve a busy checkpoint.
            if self.writer_task.is_some() {
                return Ok(());
            }
            self.checkpoint_generation = generation;
            self.checkpoint_snapshot = Some(
                self.store
                    .checkpoint
                    .snapshot_incremental(self.checkpoint_budget / (4 * node.count)),
            );
        }
        if let Some(snapshot) = self.checkpoint_snapshot.as_mut() {
            drop(cut);
            if let Poll::Ready(result) = snapshot.as_mut().poll(cx) {
                self.checkpoint_snapshot = None;
                let mut cut = node
                    .periodic_checkpoint
                    .lock()
                    .map_err(|_| Error::Unavailable)?;
                match result {
                    Ok(shard) => {
                        cut.shards.push(shard);
                    }
                    Err(error) => {
                        cut.fail(error);
                    }
                }
            }
            return Ok(());
        }
        if self.worker == node.control_worker
            && cut.shards.len() == node.count
            && !cut.publication_started()
        {
            let shards = cut.start_publication();
            let publication = match cut.reserve(CheckpointPolicy::Periodic) {
                Ok(publication) => publication,
                Err(error) => {
                    cut.fail(error);
                    return Ok(());
                }
            };
            drop(cut);
            match self.store.checkpoint.publish_async(
                shards,
                self.runtime.reactor.clone(),
                scope(self.timeout)?,
                publication.sequence,
                publication.slot,
                self.checkpoint_budget,
            ) {
                Ok(task) => {
                    let metrics = self.telemetry.metrics.clone();
                    self.checkpoint_task = Some(Box::pin(async move {
                        task.await?;
                        metrics.set_gauge(Gauge::CheckpointSequence, publication.sequence);
                        Ok(publication)
                    }));
                }
                Err(error) => node
                    .periodic_checkpoint
                    .lock()
                    .map_err(|_| Error::Unavailable)?
                    .fail(error),
            }
        }
        Ok(())
    }

    /// Validate a whole-node recovery candidate before installing any worker shard.
    pub(super) async fn recover_node(
        &self,
        geometry: CheckpointGeometry,
        startup: &RequestScope,
    ) -> Result<()> {
        let node = &self.node;
        node.recovery
            .lock()
            .map_err(|_| Error::Unavailable)?
            .geometry
            .insert(self.worker, geometry);
        std::future::poll_fn(|cx| {
            startup.check()?;
            if STOP_REQUESTED.load(Ordering::Relaxed) {
                return Poll::Ready(Err(Error::Cancelled));
            }
            let cut = node.recovery.lock().map_err(|_| Error::Unavailable)?;
            if let Some(error) = cut.failure {
                return Poll::Ready(Err(error));
            }
            if cut.geometry.len() == node.count {
                Poll::Ready(Ok(()))
            } else {
                cx.waker().wake_by_ref();
                Poll::Pending
            }
        })
        .await?;
        if self.worker == node.control_worker {
            let candidates =
                crate::store::checkpoint::candidates(&self.slab_directory, self.checkpoint_budget);
            let mut cut = node.recovery.lock().map_err(|_| Error::Unavailable)?;
            match candidates {
                Ok(candidates) => {
                    let mut newest = None;
                    let images = candidates
                        .map(|(slot, image)| {
                            newest.get_or_insert((slot, image.sequence));
                            image
                        })
                        .filter(|image| {
                            node.devices.as_ref().is_none_or(|plan| {
                                image.shards.iter().all(|shard| {
                                    plan.workers.get(shard.worker.0 as usize).is_some_and(
                                        |worker| {
                                            shard
                                                .index
                                                .validate_capacity(
                                                    worker.page_entries,
                                                    self.store.writer.index().metadata_capacity(),
                                                )
                                                .is_ok()
                                        },
                                    )
                                })
                            })
                        });
                    let selected = select(
                        images,
                        &cut.geometry,
                        &node.workers,
                        (
                            self.store.writer.index().page_capacity(),
                            self.store.writer.index().metadata_capacity(),
                        ),
                        &self.keys,
                        &self.snapshots.current()?.caches,
                    );
                    if let Some((slot, sequence)) = newest {
                        let mut periodic = node
                            .periodic_checkpoint
                            .lock()
                            .map_err(|_| Error::Unavailable)?;
                        periodic.last_sequence = sequence;
                        periodic.last_slot = slot;
                    }
                    cut.shards = selected.map_or_else(HashMap::default, |image| {
                        image.shards.into_iter().map(|s| (s.worker, s)).collect()
                    });
                    cut.selected = true;
                }
                Err(error) => {
                    cut.failure = Some(error);
                    return Err(error);
                }
            }
        }
        let shard = std::future::poll_fn(|cx| {
            startup.check()?;
            let mut cut = node.recovery.lock().map_err(|_| Error::Unavailable)?;
            if let Some(error) = cut.failure {
                return Poll::Ready(Err(error));
            }
            if cut.selected {
                Poll::Ready(Ok(cut.shards.remove(&self.worker)))
            } else {
                cx.waker().wake_by_ref();
                Poll::Pending
            }
        })
        .await?;
        if let Err(error) = self.store.recovery.install_shard(shard).await {
            node.recovery
                .lock()
                .map_err(|_| Error::Unavailable)?
                .failure = Some(error);
            return Err(error);
        }
        node.recovery
            .lock()
            .map_err(|_| Error::Unavailable)?
            .installed
            .insert(self.worker);
        std::future::poll_fn(|cx| {
            startup.check()?;
            let cut = node.recovery.lock().map_err(|_| Error::Unavailable)?;
            if let Some(error) = cut.failure {
                return Poll::Ready(Err(error));
            }
            if cut.installed.len() == node.count {
                Poll::Ready(Ok(()))
            } else {
                cx.waker().wake_by_ref();
                Poll::Pending
            }
        })
        .await?;
        Ok(())
    }
}

/// Select the newest compatible candidate without retaining two decoded images.
fn select(
    candidates: impl IntoIterator<Item = CheckpointImage>,
    geometry: &HashMap<WorkerId, CheckpointGeometry>,
    workers: &WorkerDirectory,
    (page_capacity, metadata_capacity): (usize, usize),
    keys: &Keyring,
    caches: &[racer_control_wire::CacheDefinition],
) -> Option<CheckpointImage> {
    // The disk scanner supplies newest-first images, never retaining a second
    // decoded candidate while validating the first.
    candidates.into_iter().find_map(|image| {
        let sequence = image.sequence;
        let selected = validate_candidate(
            image,
            geometry,
            workers,
            (page_capacity, metadata_capacity),
            keys,
            caches,
        );
        if selected.is_none() {
            eprintln!("racer: skipping incompatible checkpoint sequence {sequence}");
        }
        selected
    })
}

/// Filter unavailable keys and caches, then validate geometry, capacity, and ownership.
fn validate_candidate(
    mut image: CheckpointImage,
    geometry: &HashMap<WorkerId, CheckpointGeometry>,
    workers: &WorkerDirectory,
    (page_capacity, metadata_capacity): (usize, usize),
    keys: &Keyring,
    caches: &[racer_control_wire::CacheDefinition],
) -> Option<CheckpointImage> {
    let ids: HashSet<_> = image.shards.iter().map(|s| s.worker).collect();
    if ids.len() != image.shards.len() || ids != geometry.keys().copied().collect() {
        return None;
    }
    let available = |cache: &racer_control_wire::CacheId| caches.iter().any(|c| &c.id == cache);
    Recovery::filter_available(
        &mut image,
        |cache| available(cache) && keys.active(cache, KeyPurpose::Page).is_ok(),
        |cache, id| available(cache) && keys.lease(Some(cache), id, KeyPurpose::Page).is_ok(),
    );
    for shard in &image.shards {
        if geometry.get(&shard.worker) != Some(&shard.geometry) || shard.validate().is_err() {
            return None;
        }
        if page_capacity == 0 {
            return None;
        }
        shard
            .index
            .validate_capacity(page_capacity, metadata_capacity)
            .ok()?;
        if shard
            .index
            .entries
            .iter()
            .any(|(page, _)| workers.page_owner(page) != Ok(shard.worker))
            || shard.index.metadata.iter().any(|metadata| {
                workers.metadata_owner(&metadata.version.object) != Ok(shard.worker)
            })
        {
            return None;
        }
    }
    Some(image)
}

#[cfg(test)]
mod tests {
    //! Publication lifecycle invariants and whole-node recovery compatibility.

    use super::*;
    use crate::store::catalog::IndexSnapshot;
    use crate::store::checkpoint::CHECKPOINT_VERSION;
    use page_alloc::Alignment;
    use page_alloc::Segments;

    /// Cold cuts keep their distinct slot policy; later cuts alternate after success.
    #[test]
    fn checkpoint_reservations_preserve_cold_policy_and_recovered_slots() {
        for (policy, cold_slot) in [
            (CheckpointPolicy::Periodic, 1),
            (CheckpointPolicy::Shutdown, 0),
        ] {
            let mut cut = CheckpointCut::default();
            let publication = cut.reserve(policy).unwrap();
            assert_eq!(
                publication,
                Publication {
                    sequence: 1,
                    slot: cold_slot
                }
            );
            assert_eq!(cut.last_sequence, 1);
            assert_eq!(cut.last_slot, 0);
            cut.published(publication).unwrap();
            assert_eq!(cut.last_slot, cold_slot);
            assert_eq!(
                cut.reserve(policy).unwrap(),
                Publication {
                    sequence: 2,
                    slot: cold_slot ^ 1
                }
            );
            for slot in 0..=1 {
                let mut recovered = CheckpointCut {
                    last_sequence: 9,
                    last_slot: slot,
                    ..Default::default()
                };
                let publication = recovered.reserve(policy).unwrap();
                assert_eq!(
                    publication,
                    Publication {
                        sequence: 10,
                        slot: slot ^ 1
                    }
                );
                recovered.published(publication).unwrap();
                assert_eq!(recovered.last_slot, slot ^ 1);
            }
        }
    }

    /// Failed I/O consumes a sequence but leaves the last successful slot intact.
    #[test]
    fn checkpoint_failed_reservations_retry_slot_without_reusing_sequence() {
        for policy in [CheckpointPolicy::Periodic, CheckpointPolicy::Shutdown] {
            for slot in 0..=1 {
                let mut cut = CheckpointCut {
                    last_sequence: 9,
                    last_slot: slot,
                    ..Default::default()
                };
                let failed = cut.reserve(policy).unwrap();
                let retry = cut.reserve(policy).unwrap();
                assert_eq!(failed.sequence, 10);
                assert_eq!(retry.sequence, 11);
                assert_eq!(failed.slot, retry.slot);
                assert_eq!(cut.last_slot, slot);
                assert_eq!(cut.published(failed), Err(Error::Unavailable));
                assert_eq!(cut.last_slot, slot);
                cut.published(retry).unwrap();
                assert_eq!(cut.last_slot, slot ^ 1);
            }
            let mut cold = CheckpointCut::default();
            let _failed = cold.reserve(policy).unwrap();
            // A failed first shutdown cut still consumes sequence 1, as before.
            assert_eq!(
                cold.reserve(policy).unwrap(),
                Publication {
                    sequence: 2,
                    slot: 1
                }
            );
        }
    }

    /// Checked sequence and slot operations reject invalid state without mutation.
    #[test]
    fn checkpoint_reservation_overflow_and_invalid_slots_leave_state_unchanged() {
        for policy in [CheckpointPolicy::Periodic, CheckpointPolicy::Shutdown] {
            for (sequence, slot) in [(u64::MAX, 0), (u64::MAX, 1), (9, 2)] {
                let mut cut = CheckpointCut {
                    last_sequence: sequence,
                    last_slot: slot,
                    ..Default::default()
                };
                assert_eq!(cut.reserve(policy), Err(Error::Unavailable));
                assert_eq!((cut.last_sequence, cut.last_slot), (sequence, slot));
            }
        }
        let mut cut = CheckpointCut {
            last_sequence: 9,
            last_slot: 1,
            ..Default::default()
        };
        assert_eq!(
            cut.published(Publication {
                sequence: 9,
                slot: 2
            }),
            Err(Error::Unavailable)
        );
        assert_eq!((cut.last_sequence, cut.last_slot), (9, 1));
    }

    /// Result recording preserves the distinction between aborted and published cuts.
    #[test]
    fn checkpoint_phases_preserve_publication_ownership_and_writer_gating() {
        let now = std::time::Instant::now();
        let mut cut = CheckpointCut::default();
        assert!(!cut.blocks_writer());
        assert_eq!(cut.result(), None);
        cut.restart(now).unwrap();
        assert!(cut.blocks_writer());
        assert!(!cut.publication_started());
        cut.finish(Err(Error::Overloaded));
        assert!(!cut.blocks_writer());
        assert!(!cut.publication_started());
        assert_eq!(cut.result(), Some(Err(Error::Overloaded)));
        cut.finish(Err(Error::Cancelled));
        assert_eq!(cut.result(), Some(Err(Error::Cancelled)));
        for result in [Ok(()), Err(Error::Io), Err(Error::Cancelled)] {
            cut.restart(now).unwrap();
            cut.shards = image(1, &[WorkerId(0)]).shards;
            assert_eq!(cut.start_publication().len(), 1);
            assert!(cut.shards.is_empty());
            assert!(cut.publication_started());
            assert!(cut.blocks_writer());
            cut.finish(result);
            assert_eq!(cut.result(), Some(result));
            assert!(cut.publication_started());
            assert!(!cut.blocks_writer());
        }
    }

    /// Shard close errors replace results but never reset a latched publication.
    #[test]
    fn checkpoint_close_error_preserves_phase_ownership_and_original_error() {
        for phase in [
            CutPhase::Collecting,
            CutPhase::Aborted(Error::Cancelled),
            CutPhase::Publishing,
            CutPhase::Published(Ok(())),
            CutPhase::Published(Err(Error::Overloaded)),
        ] {
            let mut cut = CheckpointCut {
                phase,
                last_sequence: 9,
                last_slot: 1,
                periodic_generation: 3,
                periodic_finished: 1,
                ..Default::default()
            };
            cut.shards = image(9, &[WorkerId(0)]).shards;
            let publication_started = cut.publication_started();
            cut.fail(Error::Os(libc::EIO));
            assert_eq!(cut.result(), Some(Err(Error::Os(libc::EIO))));
            assert_eq!(cut.publication_started(), publication_started);
            assert!(!cut.blocks_writer());
            assert_eq!((cut.last_sequence, cut.last_slot), (9, 1));
            assert_eq!((cut.periodic_generation, cut.periodic_finished), (3, 1));
            // Failed cuts release their bounded collected images, not worker freezes
            // or the separate in-flight publication task.
            assert!(cut.shards.is_empty());
            if publication_started {
                // An already-owned task can still report its eventual result.
                cut.finish(Ok(()));
                assert_eq!(cut.result(), Some(Ok(())));
                assert!(cut.publication_started());
            } else {
                assert!(matches!(cut.phase, CutPhase::Aborted(Error::Os(libc::EIO))));
            }
        }
    }

    /// Restart clears only generation-local state and cannot wrap the idle sentinel.
    #[test]
    fn checkpoint_restart_preserves_sequence_and_checks_generation_overflow() {
        let now = std::time::Instant::now();
        let mut cut = CheckpointCut {
            last_sequence: 9,
            last_slot: 1,
            periodic_finished: 2,
            ..Default::default()
        };
        cut.shards = image(1, &[WorkerId(0)]).shards;
        cut.finish(Err(Error::Cancelled));
        cut.restart(now).unwrap();
        assert_eq!(cut.periodic_generation, 1);
        assert_eq!(cut.periodic_started, Some(now));
        assert_eq!(cut.periodic_finished, 0);
        assert!(cut.shards.is_empty());
        assert_eq!(cut.result(), None);
        assert_eq!((cut.last_sequence, cut.last_slot), (9, 1));
        cut.periodic_generation = u64::MAX;
        cut.periodic_finished = 2;
        cut.finish(Err(Error::Io));
        assert_eq!(
            cut.restart(now + Duration::from_secs(5)),
            Err(Error::Unavailable)
        );
        assert_eq!(cut.periodic_generation, u64::MAX);
        assert_eq!(cut.periodic_finished, 2);
        assert_eq!(cut.periodic_started, Some(now));
        assert_eq!(cut.result(), Some(Err(Error::Io)));
    }

    fn geometry() -> CheckpointGeometry {
        CheckpointGeometry::new(8192, 4096, 2, Alignment::new(4096, 4096, 4096).unwrap()).unwrap()
    }
    fn caches() -> Vec<racer_control_wire::CacheDefinition> {
        let mut cache = crate::app::tests::definition();
        cache.id = racer_control_wire::CacheId(crate::test_support::security::CACHE.into());
        vec![cache]
    }
    fn image(sequence: u64, ids: &[WorkerId]) -> CheckpointImage {
        CheckpointImage {
            version: CHECKPOINT_VERSION,
            sequence,
            shards: ids
                .iter()
                .map(|worker| {
                    let g = geometry();
                    let segments = Segments::new(g.segment_bytes);
                    segments
                        .configure(
                            g.slab_bytes,
                            g.segment_count as usize,
                            g.alignment().unwrap(),
                        )
                        .unwrap();
                    ShardImage {
                        worker: *worker,
                        geometry: g,
                        index: IndexSnapshot {
                            entries: vec![],
                            metadata: vec![],
                        },
                        segments: segments.snapshot(),
                    }
                })
                .collect(),
        }
    }
    #[test]
    fn guarded_layout_rejects_both_legacy_cuts_and_one_mismatched_worker() {
        let node = NodeState::default();
        let keys = crate::test_support::security::keys();
        let ids = [WorkerId(0), WorkerId(1)];
        let layouts = super::super::devices::recovery_layout_fixture();
        let legacy = [layouts[0].1, layouts[1].1];
        let guarded = [layouts[0].0.layout_digest, layouts[1].0.layout_digest];
        assert_ne!(legacy, guarded);
        let candidate = |sequence, digests: [[u8; 32]; 2]| {
            let mut cut = image(sequence, &ids);
            for (i, shard) in cut.shards.iter_mut().enumerate() {
                shard.geometry = layouts[i].0;
                shard.geometry.layout_digest = digests[i];
                let segments = Segments::new(shard.geometry.segment_bytes);
                segments
                    .configure(
                        shard.geometry.slab_bytes,
                        6,
                        shard.geometry.alignment().unwrap(),
                    )
                    .unwrap();
                shard.segments = segments.snapshot();
            }
            cut
        };
        let live = |digests: [[u8; 32]; 2]| {
            ids.into_iter()
                .enumerate()
                .map(|(i, id)| {
                    let mut g = layouts[i].0;
                    g.layout_digest = digests[i];
                    (id, g)
                })
                .collect()
        };
        let choose = |cuts, geometry: &HashMap<WorkerId, CheckpointGeometry>| {
            select(cuts, geometry, &node.workers, (4, 4), &keys, &caches())
        };
        let g = live(guarded);
        assert!(choose(vec![candidate(2, legacy), candidate(1, legacy)], &g).is_none());
        let mut mixed = candidate(3, guarded);
        mixed.shards[1].geometry.layout_digest = legacy[1];
        assert!(choose(vec![mixed], &g).is_none());
        let mut mixed = candidate(3, guarded);
        mixed.shards[1].geometry.layout_digest = legacy[1];
        assert_eq!(
            choose(vec![mixed, candidate(2, guarded)], &g)
                .unwrap()
                .sequence,
            2
        );
        let install = |cut: CheckpointImage, reset: bool| {
            assert_eq!(cut.shards.len(), 2);
            for shard in cut.shards {
                let index = Rc::new(crate::store::catalog::Index::new(
                    shard.worker,
                    4,
                    crate::test_support::availability(),
                ));
                let segments = Rc::new(Segments::new(shard.geometry.segment_bytes));
                segments
                    .configure(
                        shard.geometry.slab_bytes,
                        shard.geometry.segment_count as usize,
                        shard.geometry.alignment().unwrap(),
                    )
                    .unwrap();
                let fresh = segments.snapshot();
                let metadata = crate::model::VersionMetadata {
                    content_type: None,
                    version: crate::model::ObjectVersion {
                        object: crate::model::ObjectId {
                            cache: caches()[0].id.clone(),
                            key: crate::model::CacheKey([9; 32]),
                        },
                        etag: crate::model::StrongEtag::test_value("old"),
                    },
                    length: 17,
                };
                let (lease, extent) = segments.append(4096).unwrap();
                let allocated = segments
                    .snapshot()
                    .into_iter()
                    .find(|s| matches!(s.state, page_alloc::SegmentState::Open))
                    .unwrap();
                index
                    .publish(
                        crate::model::PageId {
                            version: metadata.version.clone(),
                            number: crate::model::PageNumber(0),
                        },
                        crate::store::catalog::IndexedPage {
                            location: crate::store::catalog::RecordLocation {
                                segment: allocated.id,
                                generation: allocated.generation,
                                extent,
                            },
                            metadata: metadata.clone(),
                            key_id: crate::model::key_id_from_generation(1, 1).unwrap(),
                        },
                    )
                    .unwrap();
                index.publish_version(metadata).unwrap();
                drop(lease);
                assert!(!index.snapshot().unwrap().entries.is_empty());
                assert!(!index.snapshot().unwrap().metadata.is_empty());
                assert_ne!(segments.snapshot(), fresh);
                let recovery =
                    Recovery::new(std::path::PathBuf::new(), index.clone(), segments.clone());
                recovery.configure_geometry(shard.geometry).unwrap();
                futures::executor::block_on(recovery.install_shard(if reset {
                    None
                } else {
                    Some(shard)
                }))
                .unwrap();
                assert!(index.snapshot().unwrap().entries.is_empty());
                assert!(index.snapshot().unwrap().metadata.is_empty());
                assert_eq!(segments.snapshot(), fresh);
            }
        };
        install(candidate(0, guarded), true);
        install(choose(vec![candidate(3, guarded)], &g).unwrap(), false);
        let file = [crate::store::checkpoint::FILE_LAYOUT_DIGEST; 2];
        assert_eq!(
            choose(vec![candidate(2, file), candidate(1, file)], &live(file))
                .unwrap()
                .sequence,
            2
        );
        install(
            choose(vec![candidate(2, file)], &live(file)).unwrap(),
            false,
        );
    }

    #[test]
    fn newest_complete_cut_wins_and_partial_duplicate_or_foreign_workers_fall_back() {
        let node = NodeState::default();
        let keys = crate::test_support::security::keys();
        let geometry = [(WorkerId(0), geometry()), (WorkerId(1), geometry())]
            .into_iter()
            .collect();
        let candidates = vec![
            image(4, &[WorkerId(0), WorkerId(2)]),
            image(3, &[WorkerId(0), WorkerId(0)]),
            image(2, &[WorkerId(0)]),
            image(1, &[WorkerId(0), WorkerId(1)]),
        ];
        assert_eq!(
            select(
                candidates,
                &geometry,
                &node.workers,
                (4, 4),
                &keys,
                &caches()
            )
            .unwrap()
            .sequence,
            1
        );
        let mut oversized = image(3, &[WorkerId(0), WorkerId(1)]);
        let object = crate::model::ObjectId {
            cache: racer_control_wire::CacheId(crate::test_support::security::CACHE.into()),
            key: crate::model::CacheKey([9; 32]),
        };
        let owner = node.workers.metadata_owner(&object).unwrap();
        let shard = oversized
            .shards
            .iter_mut()
            .find(|s| s.worker == owner)
            .unwrap();
        for version in 0..5 {
            shard.index.metadata.push(crate::model::VersionMetadata {
                content_type: None,
                version: crate::model::ObjectVersion {
                    object: object.clone(),
                    etag: crate::model::StrongEtag::test_value(&version.to_string()),
                },
                length: 0,
            });
        }
        assert_eq!(
            select(
                vec![oversized, image(1, &[WorkerId(0), WorkerId(1)])],
                &geometry,
                &node.workers,
                (4, 4),
                &keys,
                &caches()
            )
            .unwrap()
            .sequence,
            1
        );
        assert!(
            select(
                vec![image(2, &[WorkerId(0)])],
                &geometry,
                &node.workers,
                (4, 4),
                &keys,
                &caches()
            )
            .is_none()
        );
    }
    #[test]
    fn old_checkpoints_filter_removed_uids_unavailable_keys_and_standalone_metadata_before_install()
    {
        use crate::model::VersionMetadata;
        use crate::model::*;
        use crate::store::catalog::IndexedPage;
        use crate::store::catalog::RecordLocation;
        use crate::store::checkpoint;
        let node = NodeState::new(vec![WorkerId(0)], 16).unwrap();
        let keys = crate::test_support::security::keys();
        let caches = caches();
        let cache = caches[0].id.clone();
        let g = geometry();
        let geometry = [(WorkerId(0), g)].into_iter().collect();
        let mut old = image(8, &[WorkerId(0)]);
        let segments = Segments::new(g.segment_bytes);
        segments
            .configure(
                g.slab_bytes,
                g.segment_count as usize,
                g.alignment().unwrap(),
            )
            .unwrap();
        let append = segments.append(4096).unwrap();
        let metadata = VersionMetadata {
            content_type: None,
            version: ObjectVersion {
                object: ObjectId {
                    cache: cache.clone(),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value("old"),
            },
            length: 3,
        };
        let page = PageId {
            version: metadata.version.clone(),
            number: PageNumber(0),
        };
        old.shards[0].index.entries.push((
            page.clone(),
            IndexedPage {
                location: RecordLocation {
                    segment: append.0.id(),
                    generation: append.0.generation(),
                    extent: append.1,
                },
                metadata: metadata.clone(),
                key_id: crate::model::key_id_from_generation(1, 1).unwrap(),
            },
        ));
        drop(append);
        old.shards[0].segments = segments.snapshot();
        let mut standalone = metadata.clone();
        standalone.version.etag = StrongEtag::test_value("head-only");
        standalone.length = 0;
        old.shards[0].index.metadata.push(standalone.clone());
        // Both historical slots survive. Decode their exact bytes, as restart does.
        let bytes = checkpoint::encode(&old).unwrap();
        let decoded = || checkpoint::decode(&bytes).unwrap();
        let removed = select(
            vec![decoded(), decoded()],
            &geometry,
            &node.workers,
            (16, 16),
            &keys,
            &[],
        )
        .unwrap();
        assert!(removed.shards[0].index.entries.is_empty());
        assert!(removed.shards[0].index.metadata.is_empty());
        let retained = select(
            vec![decoded()],
            &geometry,
            &node.workers,
            (16, 16),
            &keys,
            &caches,
        )
        .unwrap();
        assert_eq!(retained.shards[0].index.entries.len(), 1);
        assert_eq!(retained.shards[0].index.metadata, vec![standalone]);
        let mut missing_page = decoded();
        missing_page.shards[0].index.entries[0].1.key_id = racer_control_wire::KeyId([99; 16]);
        let filtered = select(
            vec![missing_page],
            &geometry,
            &node.workers,
            (16, 16),
            &keys,
            &caches,
        )
        .unwrap();
        assert!(filtered.shards[0].index.entries.is_empty());
        assert_eq!(filtered.shards[0].index.metadata.len(), 1);
        keys.install(racer_control_wire::KeyringBundle {
            schema_version: 1,
            cluster: keys.cluster().clone(),
            generation: racer_control_wire::BundleGeneration(2),
            peer_trust_roots: (*keys.peer_trust_roots().unwrap()).clone(),
            cache_keys: vec![],
        })
        .unwrap();
        let keyless = select(
            vec![decoded()],
            &geometry,
            &node.workers,
            (16, 16),
            &keys,
            &caches,
        )
        .unwrap();
        assert!(keyless.shards[0].index.entries.is_empty());
        assert!(keyless.shards[0].index.metadata.is_empty());
        let index = Rc::new(Index::new(
            WorkerId(0),
            16,
            crate::test_support::availability(),
        ));
        let segments = Rc::new(Segments::new(g.segment_bytes));
        segments
            .configure(
                g.slab_bytes,
                g.segment_count as usize,
                g.alignment().unwrap(),
            )
            .unwrap();
        let recovery = Recovery::new(std::path::PathBuf::new(), index.clone(), segments);
        recovery.configure_geometry(g).unwrap();
        futures::executor::block_on(recovery.install_shard(keyless.shards.into_iter().next()))
            .unwrap();
        assert!(index.lookup(&page).unwrap().is_none());
        assert!(index.version(&metadata.version).unwrap().is_none());
    }

    #[test]
    fn ownership_and_capacity_are_checked_on_every_worker() {
        use crate::model::CacheKey;
        use crate::model::ObjectId;
        use crate::model::ObjectVersion;
        use crate::model::StrongEtag;
        use crate::model::VersionMetadata;
        let node = NodeState::default();
        let keys = crate::test_support::security::keys();
        let geometry = [(WorkerId(0), geometry()), (WorkerId(1), geometry())]
            .into_iter()
            .collect();
        let object = ObjectId {
            cache: racer_control_wire::CacheId(crate::test_support::security::CACHE.into()),
            key: CacheKey([3; 32]),
        };
        let owner = node.workers.metadata_owner(&object).unwrap();
        let mut wrong = image(2, &[WorkerId(0), WorkerId(1)]);
        wrong
            .shards
            .iter_mut()
            .find(|s| s.worker != owner)
            .unwrap()
            .index
            .metadata
            .push(VersionMetadata {
                content_type: None,
                version: ObjectVersion {
                    object,
                    etag: StrongEtag::test_value("one"),
                },
                length: 0,
            });
        assert_eq!(
            select(
                vec![wrong, image(1, &[WorkerId(0), WorkerId(1)])],
                &geometry,
                &node.workers,
                (4, 4),
                &keys,
                &caches()
            )
            .unwrap()
            .sequence,
            1
        );
    }

    #[test]
    fn configured_tiny_recovery_budget_starts_cold_and_completes_installation() {
        use crate::app::tests::ControlFixture;
        use crate::app::tests::local_worker;
        use crate::app::tests::publication;
        for budget in [1, 64 * 1024 * 1024] {
            let mut fixture = ControlFixture::new();
            let mut config = fixture.config.take().unwrap();
            config.checkpoint_bytes = std::num::NonZeroUsize::new(budget).unwrap();
            let node = Arc::new(NodeState::new(vec![WorkerId(0)], 16).unwrap());
            let (app, _runtime, _crypto) = local_worker(&config, &node, 0);
            app.snapshots
                .apply(publication(&config, 1, vec![]))
                .unwrap();
            let _ = futures::executor::block_on(app.store.open()).unwrap();
            let shard = futures::executor::block_on(app.store.checkpoint.snapshot_shard()).unwrap();
            let geometry = shard.geometry;
            app.store.checkpoint.finish_snapshot();
            let bytes = crate::store::checkpoint::encode(&CheckpointImage {
                version: CHECKPOINT_VERSION,
                sequence: 9,
                shards: vec![shard],
            })
            .unwrap();
            std::fs::write(config.slab_directory.join("checkpoint.0"), bytes).unwrap();
            futures::executor::block_on(
                app.recover_node(geometry, &scope(Duration::from_secs(5)).unwrap()),
            )
            .unwrap();
            let cut = node.recovery.lock().unwrap();
            assert!(cut.selected && cut.installed.contains(&WorkerId(0)));
            assert!(cut.failure.is_none());
            assert_eq!(
                node.periodic_checkpoint.lock().unwrap().last_sequence,
                if budget == 1 { 0 } else { 9 }
            );
            assert!(
                app.store
                    .writer
                    .index()
                    .snapshot()
                    .unwrap()
                    .entries
                    .is_empty()
            );
        }
    }

    #[test]
    fn distinct_live_catalog_and_page_capacities_select_an_installable_cut() {
        let node = NodeState::new(vec![WorkerId(0)], 16).unwrap();
        let keys = crate::test_support::security::keys();
        let geometry = [(WorkerId(0), geometry())].into_iter().collect();
        let candidate = |sequence, count| {
            let mut cut = image(sequence, &[WorkerId(0)]);
            for n in 0..count {
                cut.shards[0]
                    .index
                    .metadata
                    .push(crate::model::VersionMetadata {
                        content_type: None,
                        version: crate::model::ObjectVersion {
                            object: crate::model::ObjectId {
                                cache: caches()[0].id.clone(),
                                key: crate::model::CacheKey([0; 32]),
                            },
                            etag: crate::model::StrongEtag::test_value(&n.to_string()),
                        },
                        length: 0,
                    });
            }
            cut
        };
        // Real mismatch: page capacity admits two descriptors but the live
        // catalog admits only one. Previously selection succeeded, install failed.
        let chosen = select(
            vec![candidate(2, 2), candidate(1, 1)],
            &geometry,
            &node.workers,
            (16, 1),
            &keys,
            &caches(),
        )
        .unwrap();
        assert_eq!(chosen.sequence, 1);
        let live = Index::new(WorkerId(0), 1, crate::test_support::availability());
        live.set_page_capacity(16).unwrap();
        live.restore(chosen.shards.into_iter().next().unwrap().index)
            .unwrap();
        // The inverse mismatch must not discard a catalog that is larger than
        // the page index. Zero catalog capacity can still accept an empty cut.
        assert_eq!(
            select(
                vec![candidate(2, 2)],
                &geometry,
                &node.workers,
                (1, 2),
                &keys,
                &caches()
            )
            .unwrap()
            .sequence,
            2
        );
        assert!(
            select(
                vec![candidate(2, 1)],
                &geometry,
                &node.workers,
                (16, 0),
                &keys,
                &caches()
            )
            .is_none()
        );
        assert!(
            select(
                vec![candidate(1, 0)],
                &geometry,
                &node.workers,
                (16, 0),
                &keys,
                &caches()
            )
            .is_some()
        );
    }
}
