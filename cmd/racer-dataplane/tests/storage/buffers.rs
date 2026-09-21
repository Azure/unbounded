#[cfg(test)]
pub(crate) fn io_test_pool(count: usize) -> WorkerPool {
    io_test_pool_config(Config {
        network_flights: NonZeroUsize::new(count).unwrap(),
        ..Config::new(NonZeroUsize::new(count).unwrap())
    })
}
#[cfg(test)]
pub(crate) fn io_test_pool_config(config: Config) -> WorkerPool {
    test_pool(config, NumaNodeId(0), false)
}
/// Independent machine allocation, even for equal node IDs. Demand paging affects setup only.
#[cfg(test)]
pub fn test_pool(config: Config, node: NumaNodeId, demand_paged: bool) -> WorkerPool {
    let node = if demand_paged {
        Node::initialized(config, node, |mapping| {
            // SAFETY: setup exclusively owns this live anonymous mapping.
            if unsafe {
                libc::madvise(
                    mapping.address.as_ptr().cast(),
                    mapping.len,
                    libc::MADV_NOHUGEPAGE,
                )
            } == 0
            {
                Ok(())
            } else {
                Err(io::Error::last_os_error())
            }
        })
    } else {
        Node::new(config, node, |_| Ok(()))
    };
    WorkerPool {
        node: Rc::new(Arc::new(node.unwrap())),
    }
}
#[cfg(test)]
#[derive(Clone)]
pub(crate) struct TestPoolLink(Arc<Node>);
#[cfg(test)]
impl TestPoolLink {
    pub(crate) fn for_worker(&self) -> WorkerPool {
        WorkerPool {
            node: Rc::new(self.0.clone()),
        }
    }
}
#[cfg(test)]
#[derive(Debug, Default)]
pub(crate) struct InvariantSnapshot {
    pub refs: Vec<usize>,
    pub loading: usize,
    pub flights: usize,
    pub consumers: usize,
    pub producers: usize,
    pub network_waiters: usize,
    pub file_outcomes: usize,
}
#[cfg(test)]
impl WorkerPool {
    pub(crate) fn test_link(&self) -> TestPoolLink {
        TestPoolLink(Arc::clone(&self.node))
    }
    pub(crate) fn test_other_worker(&self) -> Self {
        self.test_link().for_worker()
    }
    /// Quiescent scheduler boundaries only: no concurrent pool calls or callbacks.
    pub(crate) fn invariant_snapshot(&self) -> InvariantSnapshot {
        let mut snapshot = InvariantSnapshot::default();
        let mut free = vec![false; self.node.slots.len()];
        for &index in self.node.free.lock().unwrap().iter() {
            assert!(
                !std::mem::replace(&mut free[index], true),
                "duplicate free slot"
            );
        }
        for (index, slot) in self.node.slots.iter().enumerate() {
            let refs = slot.refs.0.load(Ordering::Acquire);
            assert_eq!(free[index], refs == 0, "lost or prematurely freed slot");
            let info = slot.info.0.lock().unwrap();
            if let Some(len) = info.len {
                assert!(len <= BUFFER_SIZE);
            }
            snapshot.loading += usize::from(refs != 0 && info.len.is_none());
            snapshot.refs.push(refs);
        }
        let mut network_refs = vec![0; free.len()];
        self.node
            .flights
            .all_network
            .lock()
            .unwrap()
            .retain(|weak| {
                let Some(state) = weak.upgrade() else {
                    return false;
                };
                let inner = state.inner.lock().unwrap();
                assert!(
                    inner.consumers > 0
                        && inner.consumers <= self.node.flights.consumers_per_flight
                );
                assert_eq!(Arc::strong_count(&state), inner.consumers + 1);
                assert!(inner.wakers.len() <= inner.consumers);
                assert!(inner.wakers.keys().all(|id| *id < inner.next));
                let terminals = usize::from(inner.outcome.is_some())
                    + usize::from(inner.file.is_some())
                    + usize::from(inner.metadata.is_some());
                assert!(terminals <= 1);
                if terminals != 0 {
                    assert!(inner.wakers.is_empty());
                }
                if let Some(Ok(read)) = &inner.outcome {
                    assert!(Arc::ptr_eq(read.node.as_ref().unwrap(), &self.node));
                    assert_eq!(
                        self.node.slots[read.index].info.0.lock().unwrap().len,
                        Some(read.len)
                    );
                    network_refs[read.index] += 1;
                }
                snapshot.flights += 1;
                snapshot.consumers += inner.consumers;
                snapshot.producers += usize::from(inner.producer && terminals == 0);
                snapshot.network_waiters += inner.wakers.len();
                snapshot.file_outcomes += usize::from(inner.file.is_some());
                true
            });
        assert_eq!(
            snapshot.flights,
            self.node.flights.count.load(Ordering::Acquire)
        );
        assert!(snapshot.flights <= self.node.flights.capacity);
        assert!(
            network_refs
                .iter()
                .zip(&snapshot.refs)
                .all(|(n, refs)| n <= refs)
        );
        snapshot
    }
    pub(crate) fn assert_recovered(&self) {
        let snapshot = self.invariant_snapshot();
        assert_eq!(snapshot.flights, 0, "orphan flight");
        assert!(
            snapshot.refs.iter().all(|refs| *refs == 0),
            "pinned pool slot"
        );
        let slots: Vec<_> = (0..self.node.slots.len())
            .map(|_| self.private_fill().unwrap())
            .collect();
        assert!(self.private_fill().is_err());
        drop(slots);
    }
}

#[cfg(test)]
#[path = "buffer_lifetimes.rs"]
mod tests;
