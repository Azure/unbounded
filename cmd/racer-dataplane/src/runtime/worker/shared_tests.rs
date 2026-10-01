use super::*;
use crate::{
    memory::pool::BufferPool,
    model::{ResourceClass, *},
    runtime::crypto::{CryptoInput, CryptoOutput},
    security::{
        aead::PageCryptoEngine,
        identity::{KeyPurpose, Keyring},
    },
};
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};

#[derive(Default)]
struct Observed {
    events: Mutex<Vec<(u16, &'static str, thread::ThreadId)>>,
    polls: Mutex<Vec<u16>>,
    submitted: AtomicUsize,
    release: AtomicBool,
    io_draining: AtomicUsize,
    shutdown: AtomicUsize,
    completed: AtomicUsize,
    native: Mutex<Vec<crate::rdma::lifecycle::IoPort>>,
}
impl Observed {
    fn event(&self, worker: WorkerId, name: &'static str) {
        self.events
            .lock()
            .unwrap()
            .push((worker.0, name, thread::current().id()));
    }
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum Failure {
    None,
    BuildCrypto,
    StartCrypto,
    PendingStart,
    BuildIo,
    StartIo,
    PollCrypto,
    DrainCrypto,
    ShutdownCrypto,
}

struct Factory {
    observed: Arc<Observed>,
    failure: Failure,
    jobs: bool,
    roundtrip: bool,
    cpu: usize,
    second_crypto_cpu: usize,
    native: bool,
}

fn fixture(cap: usize, failure: Failure, jobs: bool) -> (AffinityPlan, Factory) {
    let cpu = *current_cpus().unwrap().first().unwrap();
    let location = super::super::affinity::CpuLocation {
        cpu,
        package: 0,
        core: 0,
        numa_node: None,
    };
    let plan = AffinityPlan {
        pairs: (0..2)
            .map(|id| WorkerPair {
                worker: WorkerId(id),
                io: location.clone(),
                crypto: location.clone(),
                nic: None,
            })
            .collect(),
        max_threads: cap,
    };
    (
        plan,
        Factory {
            observed: Arc::new(Observed::default()),
            failure,
            jobs,
            roundtrip: false,
            cpu,
            second_crypto_cpu: cpu,
            native: false,
        },
    )
}

impl WorkerFactory for Factory {
    fn limits(&self) -> Limits {
        let mut limits = crate::test_support::cluster::config(false).limits;
        limits.queue_entries = NonZeroUsize::new(4).unwrap();
        limits
    }
    fn build(&self, worker: WorkerId, runtime: WorkerRuntime) -> Result<Box<dyn WorkerService>> {
        assert_eq!(
            current_cpus().unwrap(),
            std::collections::BTreeSet::from([self.cpu])
        );
        self.observed.event(worker, "io-build");
        if worker.0 == 1 && self.failure == Failure::BuildIo {
            return Err(Error::InvalidRequest);
        }
        Ok(Box::new(Io {
            worker,
            runtime,
            observed: self.observed.clone(),
            keys: crate::security::identity::keyring_tests::keys(),
            failure: self.failure,
            jobs: self.jobs,
            roundtrip: self.roundtrip,
        }))
    }
    fn build_crypto(
        &self,
        worker: WorkerId,
        runtime: CryptoRuntime,
    ) -> Result<Box<dyn CryptoService>> {
        let cpu = if worker.0 == 1 {
            self.second_crypto_cpu
        } else {
            self.cpu
        };
        assert_eq!(
            current_cpus().unwrap(),
            std::collections::BTreeSet::from([cpu])
        );
        self.observed.event(worker, "crypto-build");
        if worker.0 == 1 && self.failure == Failure::BuildCrypto {
            return Err(Error::Unauthorized);
        }
        let engine = Engine {
            worker,
            engine: PageCryptoEngine::new(runtime),
            observed: self.observed.clone(),
            failure: self.failure,
            local: Rc::new(thread::current().id()),
        };
        if self.native {
            let (io, native) = crate::rdma::lifecycle::pair(1)?;
            self.observed.native.lock().unwrap().push(io);
            Ok(Box::new(crate::rdma::lifecycle::WithNative::new(
                engine, native,
            )))
        } else {
            Ok(Box::new(engine))
        }
    }
}

struct Io {
    worker: WorkerId,
    runtime: WorkerRuntime,
    observed: Arc<Observed>,
    keys: Keyring,
    failure: Failure,
    jobs: bool,
    roundtrip: bool,
}

impl Io {
    fn input(&self) -> CryptoInput {
        let cache = CacheId(crate::security::identity::tests::CACHE.into());
        CryptoInput::Encrypt {
            page: PageId {
                version: ObjectVersion {
                    object: ObjectId {
                        cache: cache.clone(),
                        key: CacheKey([0; 32]),
                    },
                    etag: StrongEtag::test_value("v1"),
                },
                number: PageNumber(0),
            },
            plaintext: BufferPool::new(self.runtime.admission.clone())
                .plaintext(
                    self.runtime
                        .admission
                        .reserve(Some(&cache), ResourceClass::Plaintext, 1)
                        .unwrap(),
                    1,
                )
                .unwrap(),
            ciphertext: self
                .runtime
                .admission
                .reserve(Some(&cache), ResourceClass::Ciphertext, 17)
                .unwrap(),
        }
    }
}

impl WorkerService for Io {
    fn start<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            self.observed.event(self.worker, "io-start");
            if self.roundtrip {
                let cache = CacheId(crate::security::identity::tests::CACHE.into());
                let key = self.keys.active(&cache, KeyPurpose::Page)?;
                let CryptoOutput::Encrypted(plain, ciphertext) = self
                    .runtime
                    .crypto
                    .execute(self.input(), key, scope)
                    .await?
                else {
                    panic!("encrypted output")
                };
                drop(plain);
                let key = self.keys.active(&cache, KeyPurpose::Page)?;
                let input = CryptoInput::Decrypt {
                    ciphertext,
                    plaintext: self.runtime.admission.reserve(
                        Some(&cache),
                        ResourceClass::Plaintext,
                        1,
                    )?,
                };
                let CryptoOutput::Decrypted(plain, _) =
                    self.runtime.crypto.execute(input, key, scope).await?
                else {
                    panic!("decrypted output")
                };
                assert_eq!(plain.bytes(), &[0]);
                self.observed.completed.fetch_add(1, Ordering::SeqCst);
            }
            if self.jobs {
                // Submit several real AEAD jobs, abandoning delivery while their
                // bounded queue permits and allocations remain in flight.
                for _ in 0..4 {
                    let key = self
                        .keys
                        .active(
                            &CacheId(crate::security::identity::tests::CACHE.into()),
                            KeyPurpose::Page,
                        )
                        .unwrap();
                    let mut operation = self.runtime.crypto.execute(self.input(), key, scope);
                    assert!(
                        operation
                            .as_mut()
                            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
                            .is_pending()
                    );
                    drop(operation);
                    self.observed.submitted.fetch_add(1, Ordering::SeqCst);
                }
            }
            if self.worker.0 == 1 && self.failure == Failure::StartIo {
                Err(Error::InvalidRequest)
            } else {
                Ok(())
            }
        })
    }
    fn poll_budgeted(&mut self, _: &mut Context<'_>, _: usize) -> Result<()> {
        Ok(())
    }
    fn stop_admission(&mut self) -> Result<()> {
        self.observed.event(self.worker, "io-stop");
        Ok(())
    }
    fn drain<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            self.observed.event(self.worker, "io-drain");
            self.observed.io_draining.fetch_add(1, Ordering::SeqCst);
            Ok(())
        })
    }
    fn shutdown<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            assert_eq!(self.runtime.crypto.outstanding(), 0);
            assert_eq!(self.runtime.admission.used(ResourceClass::Plaintext), 0);
            assert_eq!(self.runtime.admission.used(ResourceClass::Ciphertext), 0);
            self.observed.event(self.worker, "io-shutdown");
            Ok(())
        })
    }
}

struct Engine {
    worker: WorkerId,
    engine: PageCryptoEngine,
    observed: Arc<Observed>,
    failure: Failure,
    local: Rc<thread::ThreadId>,
}
impl Drop for Engine {
    fn drop(&mut self) {
        assert_eq!(*self.local, thread::current().id());
        self.observed.event(self.worker, "crypto-drop");
    }
}
impl CryptoService for Engine {
    fn register_driver(&self, waker: &Waker) {
        self.engine.register_driver(waker);
    }
    fn start<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(std::future::poll_fn(move |_| {
            if self.worker.0 == 1 {
                if self.failure == Failure::StartCrypto {
                    return Poll::Ready(Err(Error::Unauthorized));
                }
                if self.failure == Failure::PendingStart {
                    return Poll::Pending;
                }
            }
            self.observed.event(self.worker, "crypto-start");
            Poll::Ready(Ok(()))
        }))
    }
    fn poll_budgeted(&mut self, budget: usize) -> Result<()> {
        assert_eq!(*self.local, thread::current().id());
        assert_eq!(budget, 1, "one page per shard per pass");
        let mut polls = self.observed.polls.lock().unwrap();
        if polls.len() < 128 {
            polls.push(self.worker.0);
        }
        drop(polls);
        // Retain outstanding jobs until tests request release or drain begins.
        if self.observed.release.load(Ordering::SeqCst)
            || self.observed.io_draining.load(Ordering::SeqCst) > 0
        {
            self.engine.poll_budgeted(budget)?;
        }
        if self.worker.0 == 1 && self.failure == Failure::PollCrypto {
            Err(Error::Io)
        } else {
            Ok(())
        }
    }
    fn drain<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            self.observed.event(self.worker, "crypto-drain");
            // A pending drain of one shard needs the other shard to drain. This
            // deadlocks if the shared thread blocks on lifecycle futures in order.
            std::future::poll_fn(|_| {
                let sibling_entered = self
                    .observed
                    .events
                    .lock()
                    .unwrap()
                    .iter()
                    .any(|(worker, name, _)| *worker != self.worker.0 && *name == "crypto-drain");
                if self.worker.0 == 0 && self.failure != Failure::BuildCrypto && !sibling_entered {
                    Poll::Pending
                } else {
                    Poll::Ready(())
                }
            })
            .await;
            self.engine.drain(scope).await?;
            if self.worker.0 == 1 && self.failure == Failure::DrainCrypto {
                Err(Error::Io)
            } else {
                Ok(())
            }
        })
    }
    fn shutdown<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            self.observed.event(self.worker, "crypto-shutdown");
            self.observed.shutdown.fetch_add(1, Ordering::SeqCst);
            std::future::poll_fn(|_| {
                let expected = if self.failure == Failure::BuildCrypto {
                    1
                } else {
                    2
                };
                if self.observed.shutdown.load(Ordering::SeqCst) == expected {
                    Poll::Ready(())
                } else {
                    Poll::Pending
                }
            })
            .await;
            self.engine.shutdown(scope).await?;
            if self.worker.0 == 1 && self.failure == Failure::ShutdownCrypto {
                Err(Error::Io)
            } else {
                Ok(())
            }
        })
    }
}

fn scope() -> RequestScope {
    RequestScope::new(RequestId([7; 16]), Instant::now() + Duration::from_secs(3)).unwrap()
}

#[test]
fn two_io_share_one_actual_crypto_thread_and_drain_outstanding_jobs() {
    let (plan, factory) = fixture(4, Failure::None, true);
    let observed = factory.observed.clone();
    let mut group = WorkerGroup::new(plan);
    let scope = scope();
    group.start(Arc::new(factory), &scope).unwrap();
    assert_eq!(group.threads.len(), 3);
    assert_eq!(group.control.lock().total, 3);
    assert_eq!(group.control.lock().ready, 3);
    assert_eq!(observed.submitted.load(Ordering::SeqCst), 8);
    group.drain(&scope).unwrap();
    assert_eq!(group.control.lock().drained, 3);
    group.shutdown(&scope).unwrap();
    group.join().unwrap();
    assert_eq!(group.control.lock().done, 3);
    let events = observed.events.lock().unwrap();
    let thread_for = |worker, name| {
        events
            .iter()
            .find(|(id, event, _)| *id == worker && *event == name)
            .unwrap()
            .2
    };
    let crypto = thread_for(0, "crypto-build");
    assert_eq!(crypto, thread_for(1, "crypto-build"));
    assert_ne!(crypto, thread_for(0, "io-build"));
    assert_ne!(crypto, thread_for(1, "io-build"));
    assert_ne!(thread_for(0, "io-build"), thread_for(1, "io-build"));
    for worker in 0..2 {
        assert_eq!(crypto, thread_for(worker, "crypto-drop"));
        let position = |name| {
            events
                .iter()
                .position(|(id, event, _)| *id == worker && *event == name)
                .unwrap()
        };
        assert!(position("crypto-start") < position("io-build"));
        assert!(position("io-drain") < position("crypto-drain"));
        assert!(position("crypto-drain") < position("crypto-shutdown"));
        assert!(position("crypto-shutdown") < position("crypto-drop"));
    }
    // While both shards remain active, every pass grants each a single page and
    // reverses its starting order on the next pass, even with saturated queues.
    let polls = observed.polls.lock().unwrap();
    assert!(polls.len() >= 8);
    assert_eq!(&polls[..8], &[0, 1, 1, 0, 0, 1, 1, 0]);
}

#[test]
fn shared_crypto_borrowed_run_counts_caller_and_restores_affinity() {
    let (plan, factory) = fixture(3, Failure::None, true);
    let mut group = WorkerGroup::new(plan);
    let before = current_cpus().unwrap();
    let mut scope = scope();
    // Leave startup enough room under concurrent builds: this scenario checks
    // deadline-driven teardown after all eight jobs, not startup latency.
    scope.deadline = Deadline(Instant::now() + Duration::from_secs(1));
    assert_eq!(
        group.run_with_scope(&factory, &scope),
        Err(Error::DeadlineExceeded)
    );
    assert_eq!(current_cpus().unwrap(), before);
    assert_eq!(group.control.lock().done, 3);
    assert_eq!(factory.observed.submitted.load(Ordering::SeqCst), 8);
    let (plan, factory) = fixture(3, Failure::None, false);
    assert_eq!(
        WorkerGroup::new(plan).start(Arc::new(factory), &scope),
        Err(Error::InvalidConfiguration)
    );
}

#[test]
fn partial_group_build_start_and_pending_start_failures_teardown_every_built_service() {
    for failure in [
        Failure::BuildCrypto,
        Failure::StartCrypto,
        Failure::PendingStart,
        Failure::BuildIo,
        Failure::StartIo,
    ] {
        for borrowed in [false, true] {
            let (plan, factory) = fixture(if borrowed { 3 } else { 4 }, failure, true);
            let observed = factory.observed.clone();
            let mut group = WorkerGroup::new(plan);
            let mut scope = scope();
            if failure == Failure::PendingStart {
                scope.deadline = Deadline(Instant::now() + Duration::from_millis(30));
            }
            let expected = match failure {
                Failure::PendingStart => Error::DeadlineExceeded,
                Failure::BuildIo | Failure::StartIo => Error::InvalidRequest,
                _ => Error::Unauthorized,
            };
            let result = if borrowed {
                group.run_with_scope(&factory, &scope)
            } else {
                group.start(Arc::new(factory), &scope)
            };
            assert_eq!(result, Err(expected));
            assert_eq!(group.control.lock().done, 3);
            let events = observed.events.lock().unwrap();
            for id in 0..if failure == Failure::BuildCrypto {
                1
            } else {
                2
            } {
                assert!(
                    events
                        .iter()
                        .any(|(worker, event, _)| *worker == id && *event == "crypto-shutdown")
                );
                assert!(
                    events
                        .iter()
                        .any(|(worker, event, _)| *worker == id && *event == "crypto-drop")
                );
            }
        }
    }
}

#[test]
fn shared_crypto_errors_do_not_skip_sibling_drain_or_shutdown() {
    for failure in [
        Failure::PollCrypto,
        Failure::DrainCrypto,
        Failure::ShutdownCrypto,
    ] {
        let (plan, factory) = fixture(4, failure, false);
        let observed = factory.observed.clone();
        let mut group = WorkerGroup::new(plan);
        let scope = scope();
        let started = group.start(Arc::new(factory), &scope);
        if failure == Failure::PollCrypto {
            // The poll error may race the parent's readiness observation.
            assert!(started == Ok(()) || started == Err(Error::Io));
        } else {
            started.unwrap();
        }
        let _ = group.drain(&scope);
        let _ = group.shutdown(&scope);
        assert_eq!(group.join(), Err(Error::Io));
        assert_eq!(group.control.lock().done, 3);
        assert_eq!(observed.shutdown.load(Ordering::SeqCst), 2);
    }
}

#[test]
fn shared_services_deliver_independent_real_encrypt_decrypt_results() {
    let (plan, mut factory) = fixture(4, Failure::None, false);
    factory.roundtrip = true;
    let observed = factory.observed.clone();
    observed.release.store(true, Ordering::SeqCst);
    let mut group = WorkerGroup::new(plan);
    let scope = scope();
    group.start(Arc::new(factory), &scope).unwrap();
    assert_eq!(observed.completed.load(Ordering::SeqCst), 2);
    group.drain(&scope).unwrap();
    group.shutdown(&scope).unwrap();
    group.join().unwrap();
}

#[test]
fn shared_group_cancellation_during_live_jobs_fences_before_join() {
    let (plan, factory) = fixture(4, Failure::None, true);
    let mut group = WorkerGroup::new(plan);
    let scope = scope();
    group.start(Arc::new(factory), &scope).unwrap();
    scope.cancel().unwrap();
    // A canceled drain wait may return immediately, but join must still run the
    // real crypto engines and fence every abandoned accepted job.
    let drained = group.drain(&scope);
    assert!(drained == Err(Error::Cancelled) || drained == Ok(()));
    group.join().unwrap();
    assert_eq!(group.control.lock().done, 3);
}

#[test]
fn pending_shard_start_does_not_block_ready_sibling_crypto_work() {
    let (plan, mut factory) = fixture(4, Failure::PendingStart, false);
    factory.roundtrip = true;
    let observed = factory.observed.clone();
    observed.release.store(true, Ordering::SeqCst);
    let mut group = WorkerGroup::new(plan);
    let mut scope = scope();
    scope.deadline = Deadline(Instant::now() + Duration::from_millis(100));
    assert_eq!(
        group.start(Arc::new(factory), &scope),
        Err(Error::DeadlineExceeded)
    );
    assert_eq!(observed.completed.load(Ordering::SeqCst), 1);
    assert_eq!(group.control.lock().done, 3);
}

#[test]
fn later_crypto_group_allocation_failure_rolls_back_live_first_group() {
    let allowed = current_cpus().unwrap();
    let Some(&second_cpu) = allowed.iter().nth(1) else {
        return;
    };
    for borrowed in [false, true] {
        // Only the first group is constructed, so its teardown must not wait for
        // the deliberately absent second service in this test fixture.
        let (mut plan, mut factory) =
            fixture(if borrowed { 4 } else { 5 }, Failure::BuildCrypto, false);
        plan.pairs[1].crypto.cpu = second_cpu;
        factory.second_crypto_cpu = second_cpu;
        let observed = factory.observed.clone();
        let mut group = WorkerGroup::new(plan);
        let scope = scope();
        let allocate = |worker, generation, capacity| {
            if worker == WorkerId(0) {
                return crypto::try_pair(worker, generation, capacity);
            }
            let event = if borrowed { "crypto-start" } else { "io-start" };
            while !observed
                .events
                .lock()
                .unwrap()
                .iter()
                .any(|(_, name, _)| *name == event)
            {
                scope.check()?;
                thread::sleep(IDLE_WAIT);
            }
            Err(Error::Overloaded)
        };
        let result = if borrowed {
            group.run_with_allocator(&factory, &scope, false, allocate)
        } else {
            group.start_with_allocator(Arc::new(factory), &scope, allocate)
        };
        assert_eq!(result, Err(Error::Overloaded));
        assert_eq!(group.control.lock().done, 2);
        assert_eq!(current_cpus().unwrap(), allowed);
        let events = observed.events.lock().unwrap();
        assert!(
            events
                .iter()
                .any(|(_, event, _)| *event == "crypto-shutdown")
        );
        if !borrowed {
            assert!(events.iter().any(|(_, event, _)| *event == "io-stop"));
            assert!(events.iter().any(|(_, event, _)| *event == "io-drain"));
            assert!(events.iter().any(|(_, event, _)| *event == "io-shutdown"));
        }
    }
}

#[test]
fn distinct_crypto_cpus_create_distinct_execution_threads() {
    let allowed = current_cpus().unwrap();
    let Some(&second_cpu) = allowed.iter().nth(1) else {
        return;
    };
    let (mut plan, mut factory) = fixture(5, Failure::None, false);
    plan.pairs[1].crypto.cpu = second_cpu;
    factory.second_crypto_cpu = second_cpu;
    let observed = factory.observed.clone();
    let mut group = WorkerGroup::new(plan);
    let scope = scope();
    group.start(Arc::new(factory), &scope).unwrap();
    assert_eq!(group.threads.len(), 4);
    assert_eq!(group.control.lock().ready, 4);
    group.drain(&scope).unwrap();
    group.shutdown(&scope).unwrap();
    group.join().unwrap();
    let events = observed.events.lock().unwrap();
    let crypto = events
        .iter()
        .filter(|(_, name, _)| *name == "crypto-build")
        .map(|(_, _, id)| *id)
        .collect::<HashSet<_>>();
    assert_eq!(crypto.len(), 2);
}

#[test]
fn shared_native_wrappers_are_drained_and_destroyed_on_owner_thread() {
    let (plan, mut factory) = fixture(4, Failure::None, true);
    factory.native = true;
    let observed = factory.observed.clone();
    let mut group = WorkerGroup::new(plan);
    let scope = scope();
    group.start(Arc::new(factory), &scope).unwrap();
    assert_eq!(observed.native.lock().unwrap().len(), 2);
    group.drain(&scope).unwrap();
    group.shutdown(&scope).unwrap();
    group.join().unwrap();
    for io in observed.native.lock().unwrap().iter() {
        assert_eq!(
            io.reopen(),
            Err(Error::Unavailable),
            "native owner was destroyed"
        );
    }
    assert_eq!(observed.shutdown.load(Ordering::SeqCst), 2);
    assert_eq!(group.control.lock().done, 3);
}
