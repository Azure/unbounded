// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use crate::buffers::{self, NetworkDependency, NetworkFlightKey};
use std::{cell::Cell, num::NonZeroUsize};

// Controlled transport latency leaves real cache faults holding their receive
// destinations. Real rings, slab publication, and NUMA-shared pools are used.
struct UpstreamFixture {
    id: u64,
    rank: usize,
    starts: Rc<Cell<usize>>,
    complete: bool,
    fail: bool,
}
impl Upstream for UpstreamFixture {
    type Exchange = (UpstreamRequest, Option<Destination>);
    fn network_scope(&self, value: [u8; 32]) -> Option<NetworkFlightKey> {
        Some(NetworkFlightKey {
            value,
            routing: [1; 32],
            version: 1,
            destination: 1,
            dependency: NetworkDependency::Independent(self.id),
        })
    }
    fn flight_reserve(&mut self, _: usize, _: Option<usize>) -> Result<usize> {
        Ok(self.rank)
    }
    fn has_peer(&self) -> bool {
        self.rank != 0
    }
    fn receive_reserve(&mut self, _: usize) -> Result<usize> {
        Ok(self.rank)
    }
    fn start_metadata(
        &mut self,
        request: UpstreamRequest,
        _: Instant,
        _: &mut Ring,
    ) -> Result<Self::Exchange> {
        self.starts.set(self.starts.get() + 1);
        Ok((request, None))
    }
    fn start(
        &mut self,
        request: UpstreamRequest,
        destination: Destination,
        _: Instant,
        _: &mut Ring,
    ) -> Result<Self::Exchange> {
        self.starts.set(self.starts.get() + 1);
        Ok((request, Some(destination)))
    }
    fn poll(
        &mut self,
        (request, destination): Self::Exchange,
        _: &mut Ring,
    ) -> Result<ExchangeProgress<Self::Exchange>> {
        if self.fail {
            return Err(Error::Unavailable);
        }
        if !self.complete {
            return Ok(ExchangeProgress::Pending {
                exchange: (request, destination),
                work: Work::default(),
            });
        }
        let result = match request {
            UpstreamRequest::BackendMetadata(_) => UpstreamResult::Metadata(record()),
            UpstreamRequest::BackendPage(page) => {
                let mut destination = destination.unwrap();
                destination.as_mut_slice()[..3].copy_from_slice(b"abc");
                UpstreamResult::BackendPage {
                    received: Received {
                        destination,
                        len: 3,
                        checksum: None,
                    },
                    facts: BackendPage {
                        range: Some(page.range()),
                        checksum: page.checksum(),
                        content_type: Default::default(),
                    },
                }
            }
            _ => panic!("only terminal owner completes in this fixture"),
        };
        Ok(ExchangeProgress::Ready(result))
    }
}
fn record() -> Record {
    Record {
        len: 3,
        checksum: Checksum([7; 32]),
        expires: now() + 60,
        content_type: Default::default(),
    }
}
fn metadata(cache: &Cache, volume: &str) -> Metadata {
    let context = Context::new(Namespace::new(volume).unwrap());
    Metadata {
        object: Object::new(context.namespace().digest(), "/same-page").unwrap(),
        record: Rc::new(record()),
        owner: cache.owner.clone(),
        context,
    }
}
fn pending(
    result: Result<Progress<Fault<UpstreamFixture>, CachedValue>>,
) -> Fault<UpstreamFixture> {
    match result.unwrap() {
        Progress::Pending { fault, .. } => fault,
        Progress::Ready(_) => panic!("pending expected"),
    }
}
fn resolve(
    cache: &mut Cache,
    ring: &mut Ring,
    upstream: &mut UpstreamFixture,
    mut fault: Fault<UpstreamFixture>,
) -> CachedValue {
    loop {
        ring.progress().unwrap();
        cache.poll(ring, 128).unwrap();
        match cache.poll_value(fault, ring, upstream).unwrap() {
            Progress::Pending { fault: next, work } => {
                fault = next;
                if !work.runnable {
                    ring.wait(work.deadline).unwrap();
                }
            }
            Progress::Ready(value) => return value,
        }
    }
}

#[test]
fn buffer_waiters_cannot_consume_downstream_flights_across_workers_and_volumes() {
    let pool = buffers::io_test_pool_config(buffers::Config::new(NonZeroUsize::new(16).unwrap()));
    let mut rings = [
        Ring::http_test_ring(pool.clone(), Default::default()).unwrap(),
        Ring::http_test_ring(pool.test_other_worker(), Default::default()).unwrap(),
    ];
    let mut caches = [tests::cache(1), tests::cache(1)];
    let starts = Rc::new(Cell::new(0));
    let deadline = Instant::now() + Duration::from_secs(10);
    let mut holders = Vec::new();
    let mut blocked = Vec::new();
    for id in 0..128 {
        let worker = id % 2;
        let meta = metadata(
            &caches[worker],
            if id < 64 {
                "first-volume"
            } else {
                "second-volume"
            },
        );
        let mut upstream = UpstreamFixture {
            id: id as u64,
            rank: 8,
            starts: starts.clone(),
            complete: false,
            fail: false,
        };
        let fault = caches[worker].page(&meta, 0, deadline).unwrap();
        let fault = pending(caches[worker].poll_value(fault, &mut rings[worker], &mut upstream));
        if fault.network.is_some() {
            holders.push((worker, fault, upstream));
        } else {
            blocked.push((worker, fault, upstream));
        }
    }
    assert_eq!(pool.invariant_snapshot().flights, 120);
    assert_eq!(starts.get(), 8);
    assert_eq!(
        holders
            .iter()
            .filter(|(_, f, _)| f.buffer_wait.is_some())
            .count(),
        112
    );
    assert_eq!(blocked.len(), 8);
    assert_eq!(
        pool.invariant_snapshot()
            .refs
            .iter()
            .filter(|&&n| n == 0)
            .count(),
        8
    );
    // Protected slots are deadline-bounded backpressure, even after the old
    // resource retry budget would have expired. No extra flight or upstream.
    let (worker, mut fault, mut upstream) = blocked.pop().unwrap();
    let until = Instant::now() + Duration::from_millis(350);
    while Instant::now() < until {
        fault = pending(caches[worker].poll_value(fault, &mut rings[worker], &mut upstream));
        assert_eq!(fault.resource_retries, 0);
        std::thread::sleep(Duration::from_millis(1));
    }
    blocked.push((worker, fault, upstream));
    assert_eq!(pool.invariant_snapshot().flights, 120);
    assert_eq!(starts.get(), 8);
    // Every downstream rank can obtain both its flight and payload slot, even
    // with the higher-rank waiters still alive. Peers share the same admission.
    for rank in (1..8).rev() {
        let worker = rank % 2;
        let meta = metadata(&caches[worker], &format!("downstream-{rank}"));
        let mut upstream = UpstreamFixture {
            id: 1000 + rank as u64,
            rank,
            starts: starts.clone(),
            complete: false,
            fail: false,
        };
        let fault = caches[worker]
            .fault(
                Spec::Page(meta.record.page(&meta.object, 0).unwrap()),
                deadline,
                true,
                &meta.context,
            )
            .unwrap();
        let fault = pending(caches[worker].poll_value(fault, &mut rings[worker], &mut upstream));
        assert!(fault.network.is_some());
        assert!(fault.buffer_wait.is_none());
        holders.push((worker, fault, upstream));
    }
    assert_eq!(pool.invariant_snapshot().flights, 127);
    assert_eq!(starts.get(), 15);
    let mut owner = UpstreamFixture {
        id: 2000,
        rank: 0,
        starts: starts.clone(),
        complete: true,
        fail: false,
    };
    // Cold metadata also requires a flight, but must not require a page buffer.
    let context = Context::new(Namespace::new("owner").unwrap());
    let fault = caches[0]
        .metadata_in(&context, "/same-page", deadline)
        .unwrap();
    let Progress::Pending { fault, .. } = caches[0]
        .poll_metadata(fault, &mut rings[0], &mut owner)
        .unwrap()
    else {
        panic!()
    };
    assert_eq!(pool.invariant_snapshot().flights, 128);
    let Progress::Ready(meta) = caches[0]
        .poll_metadata(fault, &mut rings[0], &mut owner)
        .unwrap()
    else {
        panic!()
    };
    assert_eq!(pool.invariant_snapshot().flights, 127);
    // The final owner fill completes, including slab publication, without
    // canceling any higher-rank waiter or borrowing another rank's capacity.
    let fault = caches[0].page(&meta, 0, deadline).unwrap();
    let value = resolve(&mut caches[0], &mut rings[0], &mut owner, fault);
    assert_eq!(pool.invariant_snapshot().flights, 127);
    let before = starts.get();
    // Completed cache/file ownership does not retain a coordination slot.
    for _ in 0..10 {
        let fault = caches[0].page(&meta, 0, deadline).unwrap();
        assert!(matches!(
            caches[0]
                .poll_value(fault, &mut rings[0], &mut owner)
                .unwrap(),
            Progress::Ready(CachedValue::File(_))
        ));
        let fault = caches[0]
            .metadata_in(&context, "/same-page", deadline)
            .unwrap();
        assert!(matches!(
            caches[0]
                .poll_metadata(fault, &mut rings[0], &mut owner)
                .unwrap(),
            Progress::Ready(_)
        ));
    }
    assert_eq!(starts.get(), before);
    drop(value);
    // Failure and expiry must release the protected rank-zero slot too.
    for failure in [true, false] {
        owner.id += 1;
        owner.fail = failure;
        owner.complete = false;
        let meta = metadata(&caches[0], if failure { "failure" } else { "expiry" });
        let end = Instant::now() + Duration::from_millis(30);
        let fault = caches[0].page(&meta, 0, end).unwrap();
        let fault = pending(caches[0].poll_value(fault, &mut rings[0], &mut owner));
        assert_eq!(pool.invariant_snapshot().flights, 128);
        if !failure {
            std::thread::sleep(Duration::from_millis(35));
        }
        assert!(
            caches[0]
                .poll_value(fault, &mut rings[0], &mut owner)
                .is_err()
        );
        assert_eq!(pool.invariant_snapshot().flights, 127);
    }
    drop((holders, blocked));
    for (cache, ring) in caches.iter_mut().zip(&mut rings) {
        cache.shutdown(ring).unwrap();
        ring.shutdown().unwrap();
    }
    pool.assert_recovered();
}

#[test]
fn protected_flight_wait_retries_after_release_and_expires_without_upstream() {
    let pool = buffers::io_test_pool_config(buffers::Config {
        network_flights: NonZeroUsize::new(4).unwrap(),
        ..buffers::Config::new(NonZeroUsize::new(4).unwrap())
    });
    let mut ring = Ring::http_test_ring(pool.clone(), Default::default()).unwrap();
    let mut cache = tests::cache(1);
    let starts = Rc::new(Cell::new(0));
    let meta = metadata(&cache, "retry");
    let mut upstream = UpstreamFixture {
        id: 1,
        rank: 3,
        starts: starts.clone(),
        complete: false,
        fail: false,
    };
    let first = cache
        .page(&meta, 0, Instant::now() + Duration::from_secs(2))
        .unwrap();
    let first = pending(cache.poll_value(first, &mut ring, &mut upstream));
    upstream.id = 2;
    let second = cache
        .page(&meta, 0, Instant::now() + Duration::from_secs(2))
        .unwrap();
    let mut second = pending(cache.poll_value(second, &mut ring, &mut upstream));
    assert!(second.network.is_none());
    assert_eq!(starts.get(), 1);
    drop(first);
    // The fault's existing retry timer, not a renewed candidate budget, wakes it.
    std::thread::sleep(Duration::from_millis(11));
    second = pending(cache.poll_value(second, &mut ring, &mut upstream));
    assert!(second.network.is_some());
    assert_eq!(starts.get(), 2);
    upstream.id = 3;
    let third = cache
        .metadata_in(
            &meta.context,
            "/cold-metadata",
            Instant::now() + Duration::from_millis(20),
        )
        .unwrap();
    let Progress::Pending { fault: third, .. } = cache
        .poll_metadata(third, &mut ring, &mut upstream)
        .unwrap()
    else {
        panic!()
    };
    assert!(third.0.network.is_none());
    std::thread::sleep(Duration::from_millis(25));
    assert!(matches!(
        cache.poll_metadata(third, &mut ring, &mut upstream),
        Err(Error::Timeout)
    ));
    assert_eq!(starts.get(), 2);
    assert_eq!(pool.invariant_snapshot().flights, 1);
    drop(second);
    cache.shutdown(&mut ring).unwrap();
    ring.shutdown().unwrap();
    pool.assert_recovered();
}

#[test]
fn cancelled_or_expired_buffer_waiters_release_their_ranked_flights() {
    let pool = buffers::io_test_pool_config(buffers::Config::new(NonZeroUsize::new(4).unwrap()));
    let mut ring = Ring::http_test_ring(pool.clone(), Default::default()).unwrap();
    let mut cache = tests::cache(1);
    let starts = Rc::new(Cell::new(0));
    let held: Vec<_> = (0..4).map(|_| pool.private_fill().unwrap()).collect();
    for expire in [false, true] {
        let meta = metadata(&cache, "waiting");
        let mut upstream = UpstreamFixture {
            id: if expire { 2 } else { 1 },
            rank: 3,
            starts: starts.clone(),
            complete: false,
            fail: false,
        };
        let fault = cache
            .page(&meta, 0, Instant::now() + Duration::from_millis(30))
            .unwrap();
        let fault = pending(cache.poll_value(fault, &mut ring, &mut upstream));
        assert!(fault.buffer_wait.is_some());
        assert_eq!(pool.invariant_snapshot().flights, 1);
        if expire {
            std::thread::sleep(Duration::from_millis(35));
            assert!(matches!(
                cache.poll_value(fault, &mut ring, &mut upstream),
                Err(Error::Timeout)
            ));
        } else {
            drop(fault);
        }
        assert_eq!(pool.invariant_snapshot().flights, 0);
        assert_eq!(cache.active_faults.get(), 0);
    }
    assert_eq!(starts.get(), 0);
    drop(held);
    cache.shutdown(&mut ring).unwrap();
    ring.shutdown().unwrap();
    pool.assert_recovered();
}
