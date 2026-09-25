//! Node-wide quiescent epoch retirement. Admission resumes only after every owner
//! has fenced accepted work and the sole coordinator invalidated recoverable cuts.
use super::*;
use crate::{control::wire::CacheKeyRef, security::keyring::RetirementBarriers};
use std::collections::HashSet;

#[derive(Default)]
pub(super) struct RetirementState {
    keys: Vec<CacheKeyRef>,
    fenced: HashSet<WorkerId>,
    quiesced: HashSet<WorkerId>,
    checkpoint_invalidated: bool,
    complete: bool,
    resumed: HashSet<WorkerId>,
    removed_caches: Vec<crate::model::identity::CacheId>,
    removed: HashSet<WorkerId>,
    cache_transition: bool,
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn barrier_requires_exact_epoch_every_worker_and_checkpoint_invalidation() {
        let barrier = Retirement::new(2);
        let key = CacheKeyRef {
            cache: crate::model::identity::CacheId("cache".into()),
            id: crate::model::envelope::KeyId([1; 16]),
            purpose: crate::control::wire::CacheKeyPurpose::Page,
        };
        barrier.state.lock().unwrap().keys.push(key.clone());
        assert!(!barrier.fence(&key).unwrap());
        barrier.state.lock().unwrap().fenced.insert(WorkerId(0));
        assert!(!barrier.fence(&key).unwrap());
        barrier.state.lock().unwrap().fenced.insert(WorkerId(1));
        assert!(!barrier.fence(&key).unwrap());
        barrier.state.lock().unwrap().checkpoint_invalidated = true;
        assert!(barrier.fence(&key).unwrap());
        let mut other = key.clone();
        other.purpose = crate::control::wire::CacheKeyPurpose::OriginCredentials;
        assert!(!barrier.fence(&other).unwrap());
        barrier.state.lock().unwrap().complete = true;
        barrier.state.lock().unwrap().resumed.insert(WorkerId(0));
        assert!(barrier.active().unwrap());
        barrier.state.lock().unwrap().resumed.insert(WorkerId(1));
        assert!(!barrier.active().unwrap());
    }
}
pub(super) struct Retirement {
    count: usize,
    state: Mutex<RetirementState>,
}
impl Retirement {
    pub(super) fn new(count: usize) -> Self {
        Self {
            count,
            state: Mutex::new(RetirementState::default()),
        }
    }
    pub(super) fn active(&self) -> Result<bool> {
        let state = self.state.lock().map_err(|_| Error::Unavailable)?;
        Ok((!state.keys.is_empty() || state.cache_transition)
            && (!state.complete || state.resumed.len() != self.count))
    }
    pub(super) fn remove(&self, caches: Vec<crate::model::identity::CacheId>) -> Result<()> {
        let mut state = self.state.lock().map_err(|_| Error::Unavailable)?;
        if (!state.keys.is_empty() || state.cache_transition)
            && (!state.complete || state.resumed.len() != self.count)
        {
            // A newer control response can supersede uncommitted preparation.
            // No cache tombstone is installed until publication commits.
            if state.cache_transition && !state.complete && state.removed.is_empty() {
                state.removed_caches = caches;
                return Ok(());
            }
            return Err(Error::Unavailable);
        }
        *state = RetirementState {
            removed_caches: caches,
            cache_transition: true,
            ..Default::default()
        };
        Ok(())
    }
    pub(super) fn quiescent(&self) -> Result<bool> {
        Ok(self
            .state
            .lock()
            .map_err(|_| Error::Unavailable)?
            .fenced
            .len()
            == self.count)
    }
}
impl RetirementBarriers for Retirement {
    fn fence(&self, key: &CacheKeyRef) -> Result<bool> {
        let state = self.state.lock().map_err(|_| Error::Unavailable)?;
        Ok(state.keys.contains(key)
            && state.fenced.len() == self.count
            && state.checkpoint_invalidated)
    }
}
impl WorkerApplication {
    pub(super) fn poll_retirement(&mut self, cx: &mut Context<'_>) -> Result<bool> {
        let Some(node) = self.node.clone() else {
            return Ok(false);
        };
        if self.control.is_some() && !node.retirement.active()? && !self.retiring {
            let keys = self.keys.pending_retirements()?;
            if !keys.is_empty() {
                *node
                    .retirement
                    .state
                    .lock()
                    .map_err(|_| Error::Unavailable)? = RetirementState {
                    keys,
                    ..Default::default()
                };
            }
        }
        if !node.retirement.active()? && !self.retiring {
            return Ok(false);
        }
        if !self.retiring
            && node
                .retirement
                .state
                .lock()
                .map_err(|_| Error::Unavailable)?
                .resumed
                .contains(&self.worker)
        {
            return Ok(true);
        }
        if !self.retiring {
            self.retiring = true;
            self.retirement_scope = Some(scope(self.shutdown_timeout)?);
            self.cache_prepare_task.take();
            self.prepared_listeners.borrow_mut().take();
            node.cache_cut
                .lock()
                .map_err(|_| Error::Unavailable)?
                .invalidate(self.worker);
            for cache in &self.caches {
                self.clients.cancel_cache(&cache.id)?;
            }
            if let Some(scope) = &self.listener_scope {
                scope.cancel()?;
            }
            if let Some(scope) = &self.diagnostic_scope {
                scope.cancel()?;
            }
            if let Some(scope) = &self.control_scope {
                scope.cancel()?;
            }
            self.control_task.take();
            let keys = node
                .retirement
                .state
                .lock()
                .map_err(|_| Error::Unavailable)?
                .keys
                .clone();
            for key in keys {
                if key.purpose == crate::control::wire::CacheKeyPurpose::Page {
                    self.memory.retire_key(&key.cache, key.id)?;
                    self.store.writer.retire_key(&key.cache, key.id)?;
                }
            }
        }
        if node
            .retirement
            .state
            .lock()
            .map_err(|_| Error::Unavailable)?
            .complete
        {
            if self.retirement_resume.is_none() {
                let clients = self.clients.clone();
                let definitions = self.snapshots.current()?.caches.clone();
                let timeout = scope(self.timeout)?;
                let owns_listeners = self.control.is_some();
                self.retirement_resume = Some(Box::pin(async move {
                    if owns_listeners {
                        clients.prepare(&definitions, &timeout).await?.commit();
                    }
                    Ok(())
                }));
            }
            if let Some(result) = poll_task(&mut self.retirement_resume, cx) {
                match result {
                    Err(
                        Error::Io
                        | Error::Unavailable
                        | Error::Overloaded
                        | Error::DeadlineExceeded,
                    ) => return Ok(true),
                    result => result?,
                }
                let current_scope = scope(self.timeout)?;
                let mut refresh = Box::pin(self.refresh_snapshot(&current_scope));
                match refresh.as_mut().poll(cx) {
                    Poll::Ready(result) => result?,
                    Poll::Pending => return Err(Error::InvalidConfiguration),
                }
                drop(refresh);
                self.restart_listeners()?;
                self.retiring = false;
                self.retirement_scope.take();
                self.retirement_registered = false;
                self.retirement_native_started = false;
                node.retirement
                    .state
                    .lock()
                    .map_err(|_| Error::Unavailable)?
                    .resumed
                    .insert(self.worker);
            }
            return Ok(true);
        }
        let deadline = self
            .retirement_scope
            .as_ref()
            .ok_or(Error::InvalidConfiguration)?;
        if let Some(endpoint) = &mut self.endpoint {
            endpoint.poll(cx, 64)?;
        }
        self.flights.poll_with_context(cx, 64)?;
        // A committed removal may already own its replacement listeners. Do not
        // accept from them until every worker has installed removal tombstones.
        if !self.retirement_registered {
            self.clients.poll_budgeted(64)?;
        }
        for task in [
            &mut self.peer_task,
            &mut self.diagnostic_task,
            &mut self.writer_task,
        ] {
            if let Some(result) = poll_task(task, cx)
                && !matches!(
                    result,
                    Ok(())
                        | Err(Error::Cancelled
                            | Error::DeadlineExceeded
                            | Error::Io
                            | Error::Unavailable
                            | Error::Overloaded
                            | Error::MissingKey)
                )
            {
                result?;
            }
        }
        if deadline.check().is_err() {
            self.store.writer.discard_unsubmitted();
        }
        if self.writer_task.is_none() && self.store.writer.pending_count() != 0 {
            let writer = self.store.writer.clone();
            let deadline = deadline.clone();
            self.writer_task = Some(Box::pin(async move {
                writer.progress(1, &deadline).await.map(|_| ())
            }));
        }
        if let Some(result) = poll_task(&mut self.retirement_native, cx) {
            result?;
        }
        // All submission producers above are closed. Zero here is a stable fence,
        // unlike observing a running worker's momentary in-flight count.
        let quiesced = self.clients.active_connections() == 0
            && self
                .endpoint
                .as_ref()
                .is_none_or(WorkerEndpoint::is_drained)
            && crate::read::drivers::pending() == 0
            && self.peer_task.is_none()
            && self.diagnostic_task.is_none()
            && self.writer_task.is_none()
            && self.store.writer.is_idle();
        {
            let mut cut = node
                .retirement
                .state
                .lock()
                .map_err(|_| Error::Unavailable)?;
            if quiesced {
                cut.quiesced.insert(self.worker);
            } else {
                cut.quiesced.remove(&self.worker);
            }
        }
        let all_quiesced = node
            .retirement
            .state
            .lock()
            .map_err(|_| Error::Unavailable)?
            .quiesced
            .len()
            == node.count;
        if all_quiesced && !self.retirement_native_started {
            if let Some(rdma) = &self.rdma {
                self.retirement_native = Some(rdma.fence_cut());
            }
            self.retirement_native_started = true;
        }
        let fenced = quiesced
            && all_quiesced
            && self.retirement_native.is_none()
            && self.runtime.crypto.outstanding() == 0
            && self.runtime.reactor.in_flight() == 0;
        if fenced && !self.retirement_registered {
            node.retirement
                .state
                .lock()
                .map_err(|_| Error::Unavailable)?
                .fenced
                .insert(self.worker);
            self.retirement_registered = true;
        }
        let (removals, cache_transition) = {
            let state = node
                .retirement
                .state
                .lock()
                .map_err(|_| Error::Unavailable)?;
            (state.removed_caches.clone(), state.cache_transition)
        };
        if cache_transition && node.retirement.quiescent()? {
            if !node
                .cache_cut
                .lock()
                .map_err(|_| Error::Unavailable)?
                .committed
            {
                self.poll_cache_preparation(cx)?;
                if self.control.is_some() {
                    self.poll_control(cx)?;
                }
                return Ok(true);
            }
            for cache in &removals {
                // Capacity was checked before acceptance. Allocation failures
                // retain the pause and retry; they must never revive ingress.
                match self
                    .memory
                    .remove_cache(cache)
                    .and_then(|()| self.store.writer.remove_cache(cache))
                {
                    Err(Error::Overloaded) => return Ok(true),
                    result => result?,
                }
            }
            node.retirement
                .state
                .lock()
                .map_err(|_| Error::Unavailable)?
                .removed
                .insert(self.worker);
            if node
                .retirement
                .state
                .lock()
                .map_err(|_| Error::Unavailable)?
                .removed
                .len()
                != node.count
            {
                return Ok(true);
            }
            node.cache_cut
                .lock()
                .map_err(|_| Error::Unavailable)?
                .removed
                .extend(removals);
        }
        if self.control.is_some()
            && node
                .retirement
                .state
                .lock()
                .map_err(|_| Error::Unavailable)?
                .fenced
                .len()
                == node.count
        {
            let invalidated = node
                .retirement
                .state
                .lock()
                .map_err(|_| Error::Unavailable)?
                .checkpoint_invalidated;
            if !invalidated {
                if self.retirement_checkpoint.is_none() {
                    let reactor = self.runtime.reactor.clone();
                    let timeout = scope(self.timeout)?;
                    self.retirement_checkpoint = Some(
                        self.store
                            .checkpoint
                            .invalidate_persisted_async(reactor, timeout)?,
                    );
                }
                match poll_task(&mut self.retirement_checkpoint, cx) {
                    Some(Ok(())) => {
                        node.retirement
                            .state
                            .lock()
                            .map_err(|_| Error::Unavailable)?
                            .checkpoint_invalidated = true;
                    }
                    Some(Err(error)) => return Err(error),
                    None => return Ok(true),
                }
            }
            let keys = node
                .retirement
                .state
                .lock()
                .map_err(|_| Error::Unavailable)?
                .keys
                .clone();
            let retirement_scope = scope(self.timeout)?;
            for key in &keys {
                if !self.keys.pending_retirements()?.contains(key) {
                    continue;
                }
                let mut retirement = self.keys.retire(key, &retirement_scope);
                match retirement.as_mut().poll(cx) {
                    Poll::Ready(Ok(())) => (),
                    Poll::Ready(Err(Error::Unavailable)) | Poll::Pending => return Ok(true),
                    Poll::Ready(Err(error)) => return Err(error),
                }
            }
            node.retirement
                .state
                .lock()
                .map_err(|_| Error::Unavailable)?
                .complete = true;
        }
        Ok(true)
    }
    fn restart_listeners(&mut self) -> Result<()> {
        if self.control.is_none() {
            return Ok(());
        }
        let listener_scope = scope(Duration::from_secs(365 * 24 * 3600))?;
        let peers = self.peers.clone();
        let peer_scope = listener_scope.clone();
        let address = self.peer_address;
        self.peer_task = Some(Box::pin(
            async move { peers.listen(address, &peer_scope).await },
        ));
        self.listener_scope = Some(listener_scope);
        let diagnostic_scope = scope(Duration::from_secs(365 * 24 * 3600))?;
        let telemetry = self.telemetry.clone();
        let task_scope = diagnostic_scope.clone();
        let address = self.diagnostics_address;
        self.diagnostic_task = Some(Box::pin(async move {
            telemetry.serve(address, &task_scope).await
        }));
        self.diagnostic_scope = Some(diagnostic_scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        for task in [&mut self.peer_task, &mut self.diagnostic_task] {
            if let Some(result) = poll_task(task, &mut cx) {
                result?;
                return Err(Error::Unavailable);
            }
        }
        Ok(())
    }
}

#[cfg(test)]
mod resume_tests {
    use super::*;

    #[test]
    fn non_listener_worker_resumes_nonempty_cache_set_without_binding_paths() {
        let config = crate::test_support::cluster::config(false);
        let node = Arc::new(NodeState::default());
        let (mut worker, _runtime, _engine) =
            super::super::integration_tests::local_worker(&config, &node, 1);
        let definition = super::super::integration_tests::definition();
        worker
            .snapshots
            .publish(super::super::integration_tests::publication(
                &config,
                1,
                vec![definition.clone()],
            ))
            .unwrap();
        *node.retirement.state.lock().unwrap() = RetirementState {
            cache_transition: true,
            complete: true,
            resumed: [WorkerId(0)].into(),
            ..Default::default()
        };
        worker.retiring = true;
        worker.retirement_scope = Some(scope(Duration::from_secs(1)).unwrap());
        worker
            .poll_retirement(&mut Context::from_waker(futures::task::noop_waker_ref()))
            .unwrap();
        assert!(!worker.retiring);
        assert!(!node.retirement.active().unwrap());
        assert_eq!(worker.caches, vec![definition]);
        assert!(worker.peer_task.is_none());
        assert!(worker.diagnostic_task.is_none());
        assert!(worker.prepared_listeners.borrow().is_none());
    }
}
