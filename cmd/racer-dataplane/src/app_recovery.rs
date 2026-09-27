//! One node-wide checkpoint decision, validated before any owner installs a shard.
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
        let available =
            |cache: &crate::model::identity::CacheId| caches.iter().any(|c| &c.id == cache);
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
        cache.id = crate::model::identity::CacheId(crate::security::identity::tests::CACHE.into());
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
        let object = crate::model::identity::ObjectId {
            cache: crate::model::identity::CacheId(crate::security::identity::tests::CACHE.into()),
            key: crate::model::identity::CacheKey([9; 32]),
        };
        let owner = node.workers.metadata_owner(&object).unwrap();
        let shard = oversized
            .shards
            .iter_mut()
            .find(|s| s.worker == owner)
            .unwrap();
        for version in 0..5 {
            shard
                .index
                .metadata
                .push(crate::model::metadata::VersionMetadata {
                    version: crate::model::identity::ObjectVersion {
                        object: object.clone(),
                        etag: crate::model::identity::StrongEtag::test_value(&version.to_string()),
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
        use crate::model::{identity::*, metadata::VersionMetadata};
        use crate::store::{
            checkpoint_format::CheckpointCodec,
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
                key_id: crate::model::envelope::KeyId([1; 16]),
            },
        ));
        drop(append);
        old.shards[0].segments = segments.snapshot().unwrap();
        let mut standalone = metadata.clone();
        standalone.version.etag = StrongEtag::test_value("head-only");
        standalone.length = 0;
        old.shards[0].index.metadata.push(standalone.clone());
        // Both historical slots survive. Decode their exact bytes, as restart does.
        let bytes = CheckpointCodec.encode(&old).unwrap();
        let decoded = || CheckpointCodec.decode(&bytes).unwrap();
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
        missing_page.shards[0].index.entries[0].1.key_id = crate::model::envelope::KeyId([99; 16]);
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
            identity::{CacheId, CacheKey, ObjectId, ObjectVersion, StrongEtag},
            metadata::VersionMetadata,
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
