//! Prepared publication rendezvous. Control staging never binds sockets.
use super::*;
use crate::runtime::collections::HashSet;
use crate::{
    client::listener::PreparedListeners,
    control::state::{CacheDefinition, CacheTransition},
};
use std::cell::RefCell;

#[derive(Default)]
pub(super) struct CacheCut {
    generation: u64,
    pub(super) definitions: Vec<CacheDefinition>,
    prepared: HashSet<WorkerId>,
    pub(super) committed: bool,
}
pub(crate) struct CachePublication {
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
impl CachePublication {
    pub(crate) fn stage(
        &self,
        definitions: &[CacheDefinition],
    ) -> Result<Box<dyn CacheTransition>> {
        crate::control::state::validate_definitions(definitions)?;
        let mut cut = self.node.cache_cut.lock().map_err(|_| Error::Unavailable)?;
        if cut.generation == 0 || cut.definitions != definitions {
            if definitions.len() > self.capacity {
                return Err(Error::Overloaded);
            }
            self.listeners.borrow_mut().take();
            cut.generation = cut.generation.checked_add(1).ok_or(Error::Unavailable)?;
            cut.definitions = definitions.to_vec();
            cut.prepared.clear();
            cut.committed = false;
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
        if let Some(control) = &self.control {
            control.attach_cache_publication(Rc::new(CachePublication {
                node: self.node.clone(),
                listeners: self.prepared_listeners.clone(),
                capacity: self.runtime.admission.limits().metadata_entries.get(),
            }));
        }
    }
    pub(super) fn poll_cache_preparation(&mut self, cx: &mut Context<'_>) -> Result<()> {
        let node = self.node.clone();
        let (generation, definitions) = {
            let cut = node.cache_cut.lock().map_err(|_| Error::Unavailable)?;
            if cut.generation == 0 || cut.committed || cut.prepared.contains(&self.worker) {
                return Ok(());
            }
            (
                cut.generation,
                if self.cache_prepare_task.is_none()
                    || self.cache_preparing_generation != cut.generation
                {
                    cut.definitions.clone()
                } else {
                    Vec::new()
                },
            )
        };
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
        let (mut worker, _, _) = super::super::test_support::local_worker(&config, &node, 0);
        let (mut second, _, _) = super::super::test_support::local_worker(&config, &node, 1);
        let adapter = CachePublication {
            node: node.clone(),
            listeners: worker.prepared_listeners.clone(),
            capacity: config.limits.metadata_entries.get(),
        };
        assert!(matches!(adapter.stage(&[]), Err(Error::Unavailable)));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        worker.poll_cache_preparation(&mut cx).unwrap();
        assert!(matches!(adapter.stage(&[]), Err(Error::Unavailable)));
        assert!(worker.prepared_listeners.borrow().is_some());
        second.poll_cache_preparation(&mut cx).unwrap();
        let staged = adapter.stage(&[]).unwrap();
        drop(staged);
        assert!(matches!(adapter.stage(&[]), Err(Error::Unavailable)));
        worker.poll_cache_preparation(&mut cx).unwrap();
        adapter.stage(&[]).unwrap().commit();
        assert!(node.cache_cut.lock().unwrap().committed);
        // A topology-only update reuses committed cache resources.
        adapter.stage(&[]).unwrap().commit();
        assert!(node.cache_cut.lock().unwrap().committed);
    }

    #[test]
    fn capacity_failure_keeps_last_good_generation_and_uid_reuse_needs_no_tombstones() {
        let config = crate::test_support::cluster::config(false);
        let node = Arc::new(NodeState::default());
        let definition = super::super::test_support::definition();
        let store = SnapshotStore::new(config.cluster.clone(), node.publications.clone(), 16);
        store
            .publish(super::super::test_support::publication(
                &config,
                1,
                vec![definition.clone()],
            ))
            .unwrap();
        let mut adapter = CachePublication {
            node: node.clone(),
            listeners: Rc::new(RefCell::new(None)),
            capacity: 0,
        };
        assert!(matches!(
            adapter.stage(&[definition.clone()]),
            Err(Error::Overloaded)
        ));
        assert_eq!(node.cache_cut.lock().unwrap().generation, 0);
        adapter.capacity = 16;
        assert!(matches!(adapter.stage(&[]), Err(Error::Unavailable)));
        let previous = node.cache_cut.lock().unwrap().generation;
        assert!(matches!(
            adapter.stage(&[definition]),
            Err(Error::Unavailable)
        ));
        assert_eq!(node.cache_cut.lock().unwrap().generation, previous + 1);
        assert_eq!(
            store.cursor().unwrap(),
            Some(crate::control::wire::PublicationSequence(1))
        );
    }
}
