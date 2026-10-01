//! Node-wide checkpoint cuts: recover before admission, publish while running or drained.
//!
//! Recovery validates every shard before installation and seeds the periodic slot
//! sequence. The worker lifecycle decides when to poll or finish a cut; its writer
//! gating and shutdown drain predicates remain in app.rs.
use super::*;
use crate::runtime::collections::{HashMap, HashSet};
use crate::store::checkpoint_format::CheckpointImage;

#[derive(Default)]
pub(super) struct RecoveryCut {
    geometry: HashMap<WorkerId, CheckpointGeometry>,
    selected: bool,
    shards: HashMap<WorkerId, ShardImage>,
    failure: Option<Error>,
    installed: HashSet<WorkerId>,
}

impl WorkerApplication {
    pub(super) async fn checkpoint(&self, deadline: &RequestScope) -> Result<()> {
        let node = self.node.as_ref().ok_or(Error::InvalidConfiguration)?;
        let mut image = match self.store.checkpoint.snapshot_shard().await {
            Ok(image) => image,
            Err(error) => {
                node.checkpoint
                    .lock()
                    .map_err(|_| Error::Unavailable)?
                    .result = Some(Err(error));
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
        node.checkpoint
            .lock()
            .map_err(|_| Error::Unavailable)?
            .shards
            .push(image);
        let mut publication: Option<Operation<'_, ()>> = None;
        let result = std::future::poll_fn(|cx| {
            if let Some(publish) = publication.as_mut() {
                if let Poll::Ready(result) = std::pin::Pin::as_mut(publish).poll(cx) {
                    node.checkpoint
                        .lock()
                        .map_err(|_| Error::Unavailable)?
                        .result = Some(result);
                    return Poll::Ready(result);
                }
                return Poll::Pending;
            }
            let mut cut = node.checkpoint.lock().map_err(|_| Error::Unavailable)?;
            if let Some(result) = cut.result {
                return Poll::Ready(result);
            }
            if cut.publishing {
                cx.waker().wake_by_ref();
                return Poll::Pending;
            }
            if let Err(error) = deadline.check() {
                cut.result = Some(Err(error));
                return Poll::Ready(Err(error));
            }
            if self.worker == node.control_worker && cut.shards.len() == node.count {
                cut.publishing = true;
                let shards = std::mem::take(&mut cut.shards);
                drop(cut);
                publication = Some(self.store.checkpoint.publish(shards));
            }
            cx.waker().wake_by_ref();
            Poll::Pending
        })
        .await;
        self.store.checkpoint.finish_snapshot();
        result
    }

    pub(super) fn poll_checkpoint(&mut self, cx: &mut Context<'_>) -> Result<()> {
        let Some(node) = &self.node else {
            return Ok(());
        };
        if !self.started {
            return Ok(());
        }
        let now = crate::runtime::environment::now();
        if let Some(task) = self.checkpoint_task.as_mut() {
            if let Poll::Ready(result) = task.as_mut().poll(cx) {
                self.checkpoint_task = None;
                let mut cut = node
                    .periodic_checkpoint
                    .lock()
                    .map_err(|_| Error::Unavailable)?;
                if result.is_ok() {
                    cut.last_slot ^= 1;
                }
                cut.result = Some(result);
            }
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
            cut.periodic_generation += 1;
            cut.periodic_started = Some(now);
            cut.periodic_finished = 0;
            cut.shards.clear();
            cut.result = None;
            cut.publishing = false;
        }
        let generation = cut.periodic_generation;
        if self.stopping && !cut.publishing {
            cut.result = Some(Err(Error::Cancelled));
        }
        if cut.result.is_some() {
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
                        cut.result = Some(Err(error));
                    }
                }
            }
            return Ok(());
        }
        if self.worker == node.control_worker && cut.shards.len() == node.count && !cut.publishing {
            cut.publishing = true;
            let shards = std::mem::take(&mut cut.shards);
            let Some(sequence) = cut.last_sequence.checked_add(1) else {
                cut.result = Some(Err(Error::Unavailable));
                return Ok(());
            };
            cut.last_sequence = sequence;
            let slot = cut.last_slot ^ 1;
            drop(cut);
            match self.store.checkpoint.publish_async(
                shards,
                self.runtime.reactor.clone(),
                scope(self.timeout)?,
                sequence,
                slot,
                self.checkpoint_budget,
            ) {
                Ok(task) => {
                    let metrics = self.telemetry.metrics.clone();
                    self.checkpoint_task = Some(Box::pin(async move {
                        task.await?;
                        metrics.set_gauge(Gauge::CheckpointSequence, sequence);
                        Ok(())
                    }));
                }
                Err(error) => {
                    node.periodic_checkpoint
                        .lock()
                        .map_err(|_| Error::Unavailable)?
                        .result = Some(Err(error))
                }
            }
        }
        Ok(())
    }

    pub(super) async fn recover_node(
        &self,
        geometry: CheckpointGeometry,
        startup: &RequestScope,
    ) -> Result<()> {
        let node = self.node.as_ref().ok_or(Error::InvalidConfiguration)?;
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
            let candidates = crate::store::recovery::read_candidates(&self.slab_directory);
            let mut cut = node.recovery.lock().map_err(|_| Error::Unavailable)?;
            match candidates {
                Ok(candidates) => {
                    if let Some((slot, newest)) =
                        candidates.iter().max_by_key(|(_, image)| image.sequence)
                    {
                        let mut periodic = node
                            .periodic_checkpoint
                            .lock()
                            .map_err(|_| Error::Unavailable)?;
                        periodic.last_sequence = newest.sequence;
                        periodic.last_slot = *slot;
                    }
                    let selected = select(
                        candidates.into_iter().map(|(_, image)| image).collect(),
                        &cut.geometry,
                        &node.workers,
                        self.store.writer.index().page_capacity(),
                        &self.keys,
                        &self.snapshots.current()?.caches,
                    );
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

fn select(
    mut candidates: Vec<CheckpointImage>,
    geometry: &HashMap<WorkerId, CheckpointGeometry>,
    workers: &WorkerDirectory,
    capacity: usize,
    keys: &Keyring,
    caches: &[crate::control::caches::CacheDefinition],
) -> Option<CheckpointImage> {
    candidates.sort_by_key(|image| std::cmp::Reverse(image.sequence));
    candidates.into_iter().find_map(|mut image| {
        let ids: HashSet<_> = image.shards.iter().map(|s| s.worker).collect();
        if ids.len() != image.shards.len() || ids != geometry.keys().copied().collect() {
            return None;
        }
        let available = |cache: &crate::model::CacheId| caches.iter().any(|c| &c.id == cache);
        Recovery::filter_available(
            &mut image,
            |cache| available(cache) && keys.active(cache, KeyPurpose::Page).is_ok(),
            |cache, id| available(cache) && keys.lease(Some(cache), id, KeyPurpose::Page).is_ok(),
        );
        for shard in &image.shards {
            if geometry.get(&shard.worker) != Some(&shard.geometry) || shard.validate().is_err() {
                return None;
            }
            let index = Index::new(shard.worker, capacity);
            index.set_page_capacity(capacity).ok()?;
            index.validate_snapshot(&shard.index).ok()?;
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
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::store::{
        checkpoint_format::CHECKPOINT_VERSION, direct::DirectAlignment, index::IndexSnapshot,
    };
    fn geometry() -> CheckpointGeometry {
        CheckpointGeometry::new(
            8192,
            4096,
            2,
            DirectAlignment::validate(4096, 4096, 4096).unwrap(),
        )
        .unwrap()
    }
    fn caches() -> Vec<crate::control::caches::CacheDefinition> {
        let mut cache = super::super::integration_tests::definition();
        cache.id = crate::model::CacheId(crate::security::identity::tests::CACHE.into());
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
                    let segments = Segments::new(*worker, g.segment_bytes);
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
                        segments: segments.snapshot().unwrap(),
                    }
                })
                .collect(),
        }
    }
    #[test]
    fn newest_complete_cut_wins_and_partial_duplicate_or_foreign_workers_fall_back() {
        let node = NodeState::default();
        let keys = crate::security::keyring::tests::keys();
        let geometry = [(WorkerId(0), geometry()), (WorkerId(1), geometry())]
            .into_iter()
            .collect();
        let candidates = vec![
            image(1, &[WorkerId(0), WorkerId(1)]),
            image(2, &[WorkerId(0)]),
            image(3, &[WorkerId(0), WorkerId(0)]),
            image(4, &[WorkerId(0), WorkerId(2)]),
        ];
        assert_eq!(
            select(candidates, &geometry, &node.workers, 4, &keys, &caches())
                .unwrap()
                .sequence,
            1
        );
        let mut oversized = image(3, &[WorkerId(0), WorkerId(1)]);
        let object = crate::model::ObjectId {
            cache: crate::model::CacheId(crate::security::identity::tests::CACHE.into()),
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
                4,
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
                4,
                &keys,
                &caches()
            )
            .is_none()
        );
    }
    #[test]
    fn old_checkpoints_filter_removed_uids_unavailable_keys_and_standalone_metadata_before_install()
    {
        use crate::model::{VersionMetadata, *};
        use crate::store::{
            checkpoint_format,
            index::{IndexedPage, RecordLocation},
            segment::Segments,
        };
        let node = NodeState::new(vec![WorkerId(0)], 16).unwrap();
        let keys = crate::security::keyring::tests::keys();
        let caches = caches();
        let cache = caches[0].id.clone();
        let g = geometry();
        let geometry = [(WorkerId(0), g)].into_iter().collect();
        let mut old = image(8, &[WorkerId(0)]);
        let segments = Segments::new(WorkerId(0), g.segment_bytes);
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
                    segment: append.segment.id(),
                    generation: append.segment.generation(),
                    location: append.location,
                },
                metadata: metadata.clone(),
                key_id: crate::model::KeyId([1; 16]),
            },
        ));
        drop(append);
        old.shards[0].segments = segments.snapshot().unwrap();
        let mut standalone = metadata.clone();
        standalone.version.etag = StrongEtag::test_value("head-only");
        standalone.length = 0;
        old.shards[0].index.metadata.push(standalone.clone());
        // Both historical slots survive. Decode their exact bytes, as restart does.
        let bytes = checkpoint_format::encode(&old).unwrap();
        let decoded = || checkpoint_format::decode(&bytes).unwrap();
        let removed = select(
            vec![decoded(), decoded()],
            &geometry,
            &node.workers,
            16,
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
            16,
            &keys,
            &caches,
        )
        .unwrap();
        assert_eq!(retained.shards[0].index.entries.len(), 1);
        assert_eq!(retained.shards[0].index.metadata, vec![standalone]);
        let mut missing_page = decoded();
        missing_page.shards[0].index.entries[0].1.key_id = crate::model::KeyId([99; 16]);
        let filtered = select(
            vec![missing_page],
            &geometry,
            &node.workers,
            16,
            &keys,
            &caches,
        )
        .unwrap();
        assert!(filtered.shards[0].index.entries.is_empty());
        assert_eq!(filtered.shards[0].index.metadata.len(), 1);
        keys.install(crate::control::wire::KeyringBundle {
            schema_version: 1,
            cluster: keys.cluster().clone(),
            generation: crate::control::wire::BundleGeneration(2),
            peer_trust_roots: (*keys.peer_trust_roots().unwrap()).clone(),
            cache_keys: vec![],
        })
        .unwrap();
        let keyless = select(
            vec![decoded()],
            &geometry,
            &node.workers,
            16,
            &keys,
            &caches,
        )
        .unwrap();
        assert!(keyless.shards[0].index.entries.is_empty());
        assert!(keyless.shards[0].index.metadata.is_empty());
        let index = Rc::new(Index::new(WorkerId(0), 16));
        let segments = Rc::new(Segments::new(WorkerId(0), g.segment_bytes));
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
        use crate::model::{
            CacheId, CacheKey, ObjectId, ObjectVersion, StrongEtag, VersionMetadata,
        };
        let node = NodeState::default();
        let keys = crate::security::keyring::tests::keys();
        let geometry = [(WorkerId(0), geometry()), (WorkerId(1), geometry())]
            .into_iter()
            .collect();
        let object = ObjectId {
            cache: CacheId(crate::security::identity::tests::CACHE.into()),
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
                4,
                &keys,
                &caches()
            )
            .unwrap()
            .sequence,
            1
        );
    }
}
