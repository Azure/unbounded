//! One node-wide checkpoint decision, validated before any owner installs a shard.
use super::*;
use crate::store::checkpoint_format::CheckpointImage;
use std::collections::{HashMap, HashSet};

#[derive(Default)]
pub(super) struct RecoveryCut {
    geometry: HashMap<WorkerId, CheckpointGeometry>,
    selected: bool,
    shards: HashMap<WorkerId, ShardImage>,
    failure: Option<Error>,
}

impl WorkerApplication {
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
                    let selected = select(
                        candidates.into_iter().map(|(_, image)| image).collect(),
                        &cut.geometry,
                        &node.workers,
                        self.runtime.admission.limits().metadata_entries.get(),
                        &self.keys,
                    );
                    cut.shards = selected.map_or_else(HashMap::new, |image| {
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
        Ok(())
    }
}

fn select(
    mut candidates: Vec<CheckpointImage>,
    geometry: &HashMap<WorkerId, CheckpointGeometry>,
    workers: &WorkerDirectory,
    capacity: usize,
    keys: &Keyring,
) -> Option<CheckpointImage> {
    candidates.sort_by_key(|image| std::cmp::Reverse(image.sequence));
    candidates.into_iter().find_map(|mut image| {
        let ids: HashSet<_> = image.shards.iter().map(|s| s.worker).collect();
        if ids.len() != image.shards.len() || ids != geometry.keys().copied().collect() {
            return None;
        }
        Recovery::filter_available_keys(&mut image, |cache, id| {
            keys.lease(Some(cache), id, KeyPurpose::Page).is_ok()
        });
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
        let geometry = [(WorkerId(0), geometry()), (WorkerId(1), geometry())].into();
        let candidates = vec![
            image(1, &[WorkerId(0), WorkerId(1)]),
            image(2, &[WorkerId(0)]),
            image(3, &[WorkerId(0), WorkerId(0)]),
            image(4, &[WorkerId(0), WorkerId(2)]),
        ];
        assert_eq!(
            select(candidates, &geometry, &node.workers, 4, &keys)
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
                &keys
            )
            .is_none()
        );
    }
    #[test]
    fn ownership_and_capacity_are_checked_on_every_worker() {
        use crate::model::{
            identity::{CacheId, CacheKey, ObjectId, ObjectVersion, StrongEtag},
            metadata::VersionMetadata,
        };
        let node = NodeState::default();
        let keys = crate::security::keyring::tests::keys();
        let geometry = [(WorkerId(0), geometry()), (WorkerId(1), geometry())].into();
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
                &keys
            )
            .unwrap()
            .sequence,
            1
        );
    }
}
