//! Prepared publication rendezvous. Control staging never binds sockets.
use super::*;
use crate::{
    client::listener::PreparedListeners,
    control::caches::{CacheDefinition, CacheLifecycle, CacheTransition},
};
use std::{cell::RefCell, collections::HashSet};

#[derive(Default)]
pub(super) struct CacheCut {
    generation: u64,
    definitions: Vec<CacheDefinition>,
    prepared: HashSet<WorkerId>,
    pub(super) committed: bool,
    removing: bool,
    pub(super) removed: HashSet<crate::model::identity::CacheId>,
}
impl CacheCut {
    pub(super) fn invalidate(&mut self, worker: WorkerId) {
        if !self.committed {
            self.prepared.remove(&worker);
        }
    }
}
pub(super) struct Adapter {
    pub(super) node: Arc<NodeState>,
    pub(super) listeners: Rc<RefCell<Option<PreparedListeners>>>,
    pub(super) capacity: usize,
}
struct Transition {
    node: Arc<NodeState>,
    generation: u64,
    listeners: Option<PreparedListeners>,
    committed: bool,
}
impl CacheTransition for Transition {
    fn commit(mut self: Box<Self>) {
        if let Some(listeners) = self.listeners.take() {
            listeners.commit();
        }
        let mut cut = self
            .node
            .cache_cut
            .lock()
            .unwrap_or_else(|e| e.into_inner());
        assert_eq!(cut.generation, self.generation);
        cut.committed = true;
        self.committed = true;
    }
}
impl Drop for Transition {
    fn drop(&mut self) {
        if !self.committed {
            let mut cut = self
                .node
                .cache_cut
                .lock()
                .unwrap_or_else(|e| e.into_inner());
            if cut.generation == self.generation && !cut.committed {
                cut.prepared.remove(&self.node.control_worker);
            }
        }
    }
}
impl CacheLifecycle for Adapter {
    fn stage(&self, definitions: &[CacheDefinition]) -> Result<Box<dyn CacheTransition>> {
        crate::control::caches::validate_definitions(definitions)?;
        let current = self.node.publications.current().ok();
        let mut cut = self.node.cache_cut.lock().map_err(|_| Error::Unavailable)?;
        if cut.generation == 0 || cut.definitions != definitions {
            let removals: Vec<_> = current
                .as_ref()
                .into_iter()
                .flat_map(|current| &current.caches)
                .filter(|old| !definitions.iter().any(|new| new.id == old.id))
                .map(|old| old.id.clone())
                .collect();
            // Bound the cumulative tombstones before pausing any accepted service.
            // Every worker independently confirms its partition during preparation.
            if definitions.iter().any(|new| cut.removed.contains(&new.id)) {
                return Err(Error::Unavailable);
            }
            if definitions.len() > self.capacity
                || cut.removed.len()
                    + removals
                        .iter()
                        .filter(|id| !cut.removed.contains(id))
                        .count()
                    > self.capacity.min(65536)
            {
                return Err(Error::Overloaded);
            }
            let removing = !removals.is_empty() || cut.removing && !cut.committed;
            if removing {
                self.node.retirement.remove(removals)?;
            }
            self.listeners.borrow_mut().take();
            cut.generation = cut.generation.checked_add(1).ok_or(Error::Unavailable)?;
            cut.definitions = definitions.to_vec();
            cut.prepared.clear();
            cut.committed = false;
            cut.removing = removing;
            return Err(Error::Unavailable);
        }
        if cut.prepared.len() != self.node.count {
            return Err(Error::Unavailable);
        }
        if cut.committed {
            return Ok(Box::new(Transition {
                node: self.node.clone(),
                generation: cut.generation,
                listeners: None,
                committed: false,
            }));
        }
        let listeners = self
            .listeners
            .borrow_mut()
            .take()
            .ok_or(Error::Unavailable)?;
        if listeners.definitions() != definitions {
            return Err(Error::Unavailable);
        }
        Ok(Box::new(Transition {
            node: self.node.clone(),
            generation: cut.generation,
            listeners: Some(listeners),
            committed: false,
        }))
    }
}
impl WorkerApplication {
    pub(super) fn attach_cache_adapter(&self) {
        if let (Some(control), Some(node)) = (&self.control, &self.node) {
            control.attach_cache_lifecycle(Rc::new(Adapter {
                node: node.clone(),
                listeners: self.prepared_listeners.clone(),
                capacity: self.runtime.admission.limits().metadata_entries.get(),
            }));
        }
    }
    pub(super) fn poll_cache_preparation(&mut self, cx: &mut Context<'_>) -> Result<()> {
        let Some(node) = self.node.clone() else {
            return Ok(());
        };
        let (generation, definitions, committed) = {
            let cut = node.cache_cut.lock().map_err(|_| Error::Unavailable)?;
            (cut.generation, cut.definitions.clone(), cut.committed)
        };
        if generation == 0 || committed {
            return Ok(());
        }
        // Removal pauses producers first. Commit publishes the decision, then all
        // owners install tombstones before any listener is polled again.
        if node
            .cache_cut
            .lock()
            .map_err(|_| Error::Unavailable)?
            .removing
            && !node.retirement.quiescent()?
        {
            return Ok(());
        }
        if self.cache_preparing_generation != generation {
            self.cache_prepare_task.take();
            self.prepared_listeners.borrow_mut().take();
            self.cache_preparing_generation = generation;
        }
        if node
            .cache_cut
            .lock()
            .map_err(|_| Error::Unavailable)?
            .prepared
            .contains(&self.worker)
        {
            return Ok(());
        }
        // Reject a publication that cannot fit the worker-local metadata catalog.
        if definitions.len() > self.runtime.admission.limits().metadata_entries.get() {
            return Ok(());
        }
        {
            let cut = node.cache_cut.lock().map_err(|_| Error::Unavailable)?;
            let newly_removed = self
                .caches
                .iter()
                .filter(|old| {
                    !definitions.iter().any(|new| new.id == old.id)
                        && !cut.removed.contains(&old.id)
                })
                .count();
            if cut.removed.len() + newly_removed
                > self
                    .runtime
                    .admission
                    .limits()
                    .metadata_entries
                    .get()
                    .min(65536)
            {
                return Ok(());
            }
            if definitions.iter().any(|new| cut.removed.contains(&new.id)) {
                return Ok(());
            }
        }
        if self.control.is_some() && self.prepared_listeners.borrow().is_none() {
            if self.cache_prepare_task.is_none() {
                let clients = self.clients.clone();
                let prepared = self.prepared_listeners.clone();
                let startup = scope(self.timeout)?;
                self.cache_prepare_task = Some(Box::pin(async move {
                    *prepared.borrow_mut() = Some(clients.prepare(&definitions, &startup).await?);
                    Ok(())
                }));
            }
            if let Some(result) = poll_task(&mut self.cache_prepare_task, cx) {
                match result {
                    Ok(())
                    | Err(
                        Error::Io
                        | Error::Overloaded
                        | Error::Unavailable
                        | Error::DeadlineExceeded,
                    ) => (),
                    Err(error) => return Err(error),
                }
            }
            if self.prepared_listeners.borrow().is_none() {
                return Ok(());
            }
        }
        let mut cut = node.cache_cut.lock().map_err(|_| Error::Unavailable)?;
        if cut.generation == generation {
            cut.prepared.insert(self.worker);
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn publication_waits_for_every_worker_and_failed_acceptance_rolls_back_preparation() {
        let config = crate::test_support::cluster::config(false);
        let node = Arc::new(NodeState::default());
        let admission = Rc::new(Admission::new(config.limits.clone()));
        let (io, _engine) =
            crate::runtime::crypto::pair(WorkerId(0), 0, config.limits.queue_entries);
        let runtime = WorkerRuntime {
            reactor: Rc::new(Reactor::new(admission.clone())),
            admission,
            crypto: Rc::new(crate::runtime::crypto::CryptoClient::new(io)),
        };
        let worker = WorkerApplication::assemble(&config, &node, WorkerId(0), runtime).unwrap();
        let adapter = Adapter {
            node: node.clone(),
            listeners: worker.prepared_listeners.clone(),
            capacity: config.limits.metadata_entries.get(),
        };
        assert!(matches!(adapter.stage(&[]), Err(Error::Unavailable)));
        let startup = scope(Duration::from_secs(2)).unwrap();
        *worker.prepared_listeners.borrow_mut() =
            Some(futures::executor::block_on(worker.clients.prepare(&[], &startup)).unwrap());
        node.cache_cut.lock().unwrap().prepared.insert(WorkerId(0));
        assert!(matches!(adapter.stage(&[]), Err(Error::Unavailable)));
        assert!(worker.prepared_listeners.borrow().is_some());
        node.cache_cut.lock().unwrap().prepared.insert(WorkerId(1));
        let staged = adapter.stage(&[]).unwrap();
        drop(staged);
        assert!(!node.cache_cut.lock().unwrap().committed);
        assert!(
            !node
                .cache_cut
                .lock()
                .unwrap()
                .prepared
                .contains(&WorkerId(0))
        );
        *worker.prepared_listeners.borrow_mut() =
            Some(futures::executor::block_on(worker.clients.prepare(&[], &startup)).unwrap());
        node.cache_cut.lock().unwrap().prepared.insert(WorkerId(0));
        adapter.stage(&[]).unwrap().commit();
        assert!(node.cache_cut.lock().unwrap().committed);
        // A topology-only update reuses committed cache resources.
        adapter.stage(&[]).unwrap().commit();
        assert!(node.cache_cut.lock().unwrap().committed);
    }

    #[test]
    fn capacity_failure_keeps_last_good_generation_and_superseded_removal_can_retain_cache() {
        let config = crate::test_support::cluster::config(false);
        let node = Arc::new(NodeState::default());
        let definition = super::super::integration_tests::definition();
        let store = SnapshotStore::new(config.cluster.clone(), node.publications.clone(), 16);
        store
            .publish(super::super::integration_tests::publication(
                &config,
                1,
                vec![definition.clone()],
            ))
            .unwrap();
        let mut adapter = Adapter {
            node: node.clone(),
            listeners: Rc::new(RefCell::new(None)),
            capacity: 0,
        };
        assert!(matches!(adapter.stage(&[]), Err(Error::Overloaded)));
        assert!(!node.retirement.active().unwrap());
        assert_eq!(node.cache_cut.lock().unwrap().generation, 0);
        adapter.capacity = 16;
        assert!(matches!(adapter.stage(&[]), Err(Error::Unavailable)));
        assert!(node.retirement.active().unwrap());
        let previous = node.cache_cut.lock().unwrap().generation;
        assert!(matches!(
            adapter.stage(&[definition]),
            Err(Error::Unavailable)
        ));
        assert_eq!(node.cache_cut.lock().unwrap().generation, previous + 1);
        assert!(
            node.cache_cut.lock().unwrap().removing,
            "superseding response still completes the pause/resume rendezvous"
        );
        assert_eq!(
            store.cursor().unwrap(),
            Some(crate::control::wire::PublicationSequence(1))
        );
    }
}
