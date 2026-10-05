//! Domain-free lifecycle scenarios exercising the public adapter surface.

use controlplane::{
    Codec, Error, Feed, Generation, Host, Published, Retention, Rollout, Sync, Target,
    feed::{FailureClass, FetchError, Preparation, Schedule, Source},
};
use std::{
    cell::{Cell, RefCell},
    collections::VecDeque,
    path::Path,
    rc::Rc,
    sync::{
        Arc,
        atomic::{AtomicBool, AtomicU64, Ordering},
    },
    time::{Duration, Instant},
};
use uring_runtime::{Operation, Scope as _, reactor::descriptor::Descriptor};
use wire_codec::rest;

/// Test adapter error keeps cancellation distinct from retry and replay.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Failure {
    Core(Error),
    Runtime(uring_runtime::Error),
    Rest(rest::Error),
    Invalid,
}

impl From<Error> for Failure {
    fn from(error: Error) -> Self {
        Self::Core(error)
    }
}

impl From<uring_runtime::Error> for Failure {
    fn from(error: uring_runtime::Error) -> Self {
        Self::Runtime(error)
    }
}

impl From<rest::Error> for Failure {
    fn from(error: rest::Error) -> Self {
        Self::Rest(error)
    }
}

/// A tiny immutable snapshot with an independently versioned retained graph.
#[derive(Debug)]
struct Snapshot {
    sequence: u64,

    version: u64,

    graph: Arc<Vec<u8>>,

    digest: u64,

    graph_digest: u64,
}

impl Generation for Snapshot {
    type Version = u64;

    type PartVersion = u64;

    type Part = Vec<u8>;

    type Digest = u64;

    // The wire cursor and retained-part version intentionally differ.
    #[allow(clippy::misnamed_getters)]
    fn version(&self) -> u64 {
        self.sequence
    }

    fn part_version(&self) -> u64 {
        self.version
    }

    fn part(&self) -> &Arc<Vec<u8>> {
        &self.graph
    }

    fn digest(&self) -> &u64 {
        &self.digest
    }

    fn part_digest(&self) -> &u64 {
        &self.graph_digest
    }

    fn part_bytes(part: &Vec<u8>) -> usize {
        part.len()
    }
}

/// Build a publication with independently selected cursor and graph version.
fn snapshot(sequence: u64, version: u64) -> Arc<Snapshot> {
    Arc::new(Snapshot {
        sequence,
        version,
        graph: Arc::new(vec![version as u8; 32]),
        digest: sequence,
        graph_digest: version,
    })
}

/// Configure an old-part budget without grace retention.
fn retention(old_parts: usize) -> Retention {
    Retention {
        old_parts,
        grace_parts: 0,
        grace_bytes: 0,
        grace: Duration::ZERO,
    }
}

/// Publish fixture state without domain validation or resource side effects.
fn publish(cell: &Published<Snapshot>, value: Arc<Snapshot>) -> Result<Arc<Snapshot>, Error> {
    cell.publish(value, Instant::now(), |_, _| Ok(()), || ())
}

/// Only current registered acknowledgments permit a cut, including after rollback.
#[test]
fn exact_worker_ids_stale_ack_supersession_and_rollback() {
    let rollout = Rollout::new([10, 20]);
    let one = rollout.propose(Arc::new("one")).unwrap();
    assert_eq!(rollout.ack(30, one), Err(Error::Stale));
    rollout.ack(10, one).unwrap();
    assert!(matches!(rollout.stage(one), Err(Error::Pending)));
    let two = rollout.propose(Arc::new("two")).unwrap();
    assert_eq!(rollout.ack(20, one), Err(Error::Stale));
    assert_eq!(rollout.pending(&10).unwrap().unwrap().generation, two);
    rollout.ack(10, two).unwrap();
    rollout.ack(10, two).unwrap();
    assert!(matches!(rollout.stage(two), Err(Error::Pending)));
    rollout.ack(20, two).unwrap();
    {
        let _guard = rollout.stage(two).unwrap();
        assert_eq!(rollout.propose(Arc::new("three")), Err(Error::Pending));
    }
    assert!(rollout.pending(&10).unwrap().is_some());
    assert!(matches!(rollout.stage(two), Err(Error::Pending)));
    rollout.ack(10, two).unwrap();
    rollout.ack(20, two).unwrap();
    rollout.stage(two).unwrap().commit();
    assert!(rollout.committed(two).unwrap());
    assert_eq!(rollout.ack(20, two), Err(Error::Stale));
    assert!(rollout.pending(&20).unwrap().is_none());
    let empty = Rollout::<u32, ()>::new([]);
    let generation = empty.propose(Arc::new(())).unwrap();
    empty.stage(generation).unwrap().commit();
}

/// Both snapshot and direct part leases retain capacity without advancing acceptance.
#[test]
fn old_snapshot_and_part_leases_block_without_advancing_cursor() {
    let cell = Published::new(retention(1));
    let old = publish(&cell, snapshot(1, 1)).unwrap();
    let current = publish(&cell, snapshot(2, 2)).unwrap();
    assert_eq!(publish(&cell, snapshot(3, 3)).unwrap_err(), Error::Capacity);
    assert_eq!(cell.current().unwrap().unwrap().sequence, 2);
    let part = cell.resolve(1, Instant::now()).unwrap().unwrap();
    drop(old);
    assert_eq!(publish(&cell, snapshot(3, 3)).unwrap_err(), Error::Capacity);
    drop(part);
    publish(&cell, snapshot(3, 3)).unwrap();
    assert!(cell.resolve(1, Instant::now()).unwrap().is_none());
    assert_eq!(current.version, 2);
}

/// Cache-only updates share graph capacity and reject conflicting replay atomically.
#[test]
fn cache_only_generations_share_one_slot_and_conflicts_are_atomic() {
    let cell = Published::new(retention(0));
    let first = publish(&cell, snapshot(1, 1)).unwrap();
    let mut history = vec![first.clone()];
    for sequence in 2..100 {
        let next = Arc::new(Snapshot {
            sequence,
            version: 1,
            graph: first.graph.clone(),
            digest: sequence,
            graph_digest: 1,
        });
        history.push(publish(&cell, next).unwrap());
    }
    assert_eq!(
        publish(&cell, snapshot(100, 2)).unwrap_err(),
        Error::Capacity
    );
    assert_eq!(
        publish(&cell, snapshot(100, 1)).unwrap_err(),
        Error::Conflict
    );
    assert_eq!(publish(&cell, snapshot(98, 1)).unwrap_err(), Error::Replay);
    let latest = cell.current().unwrap().unwrap();
    let replay = Arc::new(Snapshot {
        sequence: 99,
        version: 1,
        graph: first.graph.clone(),
        digest: 99,
        graph_digest: 1,
    });
    let committed = Cell::new(false);
    let repeated = cell
        .publish(
            replay,
            Instant::now(),
            |_, _| Ok::<_, Error>(()),
            || committed.set(true),
        )
        .unwrap();
    assert!(Arc::ptr_eq(&latest, &repeated));
    assert!(!committed.get());
    drop(repeated);
    drop(latest);
    drop(history);
    drop(first);
    for version in 2..100 {
        publish(&cell, snapshot(version + 100, version)).unwrap();
        assert!(cell.resolve(version - 1, Instant::now()).unwrap().is_none());
    }
}

/// Grace bounds expire structural owners without revoking external leases.
#[test]
fn grace_count_time_and_byte_budgets_preserve_external_leases() {
    let now = Instant::now();
    let cell = Published::new(Retention {
        old_parts: 2,
        grace_parts: 2,
        grace_bytes: 64,
        grace: Duration::from_secs(30),
    });
    for version in 1..10 {
        cell.publish(
            snapshot(version, version),
            now,
            |_, _| Ok::<_, Error>(()),
            || (),
        )
        .unwrap();
        for retained in version.saturating_sub(2).max(1)..=version {
            assert!(cell.resolve(retained, now).unwrap().is_some());
        }
        if version > 3 {
            assert!(cell.resolve(version - 3, now).unwrap().is_none());
        }
    }
    let leased = cell.resolve(8, now).unwrap().unwrap();
    assert!(
        cell.resolve(7, now + Duration::from_secs(31))
            .unwrap()
            .is_none()
    );
    assert!(
        cell.resolve(8, now + Duration::from_secs(31))
            .unwrap()
            .is_some()
    );
    drop(leased);
    assert!(
        cell.resolve(8, now + Duration::from_secs(31))
            .unwrap()
            .is_none()
    );
    let cell = Published::new(Retention {
        old_parts: 10,
        grace_parts: 10,
        grace_bytes: 32,
        grace: Duration::from_secs(30),
    });
    for version in 1..5 {
        publish(&cell, snapshot(version, version)).unwrap();
    }
    assert!(cell.resolve(2, now).unwrap().is_none());
    assert!(cell.resolve(3, now).unwrap().is_some());
}

/// Pinned parts consume capacity before optional grace owners are retained.
#[test]
fn grace_cannot_overcommit_slots_already_consumed_by_external_pins() {
    let cell = Published::new(Retention {
        old_parts: 1,
        grace_parts: 1,
        grace_bytes: 32,
        grace: Duration::from_secs(30),
    });
    let first = publish(&cell, snapshot(1, 1)).unwrap();
    publish(&cell, snapshot(2, 2)).unwrap();
    publish(&cell, snapshot(3, 3)).unwrap();
    assert!(cell.resolve(1, Instant::now()).unwrap().is_some());
    assert!(cell.resolve(2, Instant::now()).unwrap().is_none());
    let current = cell.current().unwrap().unwrap();
    assert_eq!(publish(&cell, snapshot(4, 4)).unwrap_err(), Error::Capacity);
    drop(first);
    publish(&cell, snapshot(4, 4)).unwrap();
    assert_eq!(current.version, 3);
}

/// Failed validation rolls back a cut; successful commits expose coherent resources.
#[test]
fn validation_failure_rolls_back_barrier_and_commit_visibility_is_coherent() {
    let cell = Arc::new(Published::new(retention(0)));
    let rollout = Rollout::new([0]);
    let generation = rollout.propose(Arc::new(1)).unwrap();
    rollout.ack(0, generation).unwrap();
    let staged = rollout.stage(generation).unwrap();
    let failed = cell.publish(
        snapshot(1, 1),
        Instant::now(),
        |_, _| Err(Failure::Invalid),
        || staged.commit(),
    );
    assert_eq!(failed.unwrap_err(), Failure::Invalid);
    assert!(cell.current().unwrap().is_none());
    assert!(rollout.pending(&0).unwrap().is_some());
    let resources = Arc::new(AtomicU64::new(0));
    let done = Arc::new(AtomicBool::new(false));
    let reader = {
        let cell = cell.clone();
        let resources = resources.clone();
        let done = done.clone();
        std::thread::spawn(move || {
            while !done.load(Ordering::Acquire) {
                if let Some(snapshot) = cell.current().unwrap() {
                    assert!(resources.load(Ordering::Acquire) >= snapshot.sequence);
                    assert_eq!(snapshot.graph[0], 1);
                }
            }
        })
    };
    let graph = Arc::new(vec![1; 32]);
    for sequence in 1..1000 {
        cell.publish(
            Arc::new(Snapshot {
                sequence,
                version: 1,
                graph: graph.clone(),
                digest: sequence,
                graph_digest: 1,
            }),
            Instant::now(),
            |_, _| Ok::<_, Error>(()),
            || resources.store(sequence, Ordering::Release),
        )
        .unwrap();
    }
    done.store(true, Ordering::Release);
    reader.join().unwrap();
}

/// Owner-local cancellation scope with an absolute deadline.
#[derive(Clone)]
struct Scope {
    canceled: Rc<Cell<bool>>,

    deadline: Instant,
}

impl Scope {
    /// Start an uncanceled fixture scope with a five-second deadline.
    fn new() -> Self {
        Self {
            canceled: Rc::new(Cell::new(false)),
            deadline: uring_runtime::environment::now() + Duration::from_secs(5),
        }
    }
}

impl uring_runtime::Scope for Scope {
    type Error = Failure;

    fn check(&self) -> Result<(), Failure> {
        if self.canceled.get() {
            Err(uring_runtime::Error::Cancelled.into())
        } else if uring_runtime::environment::now() >= self.deadline {
            Err(uring_runtime::Error::DeadlineExceeded.into())
        } else {
            Ok(())
        }
    }
}

impl rest::Scope for Scope {
    fn deadline(&self) -> Instant {
        self.deadline
    }

    fn narrowed(&self, until: Instant) -> Self {
        Self {
            canceled: self.canceled.clone(),
            deadline: self.deadline.min(until),
        }
    }
}

/// No real transport is used; sleep is an asynchronously delivered test timer.
struct TestHost;

impl rest::Io for TestHost {
    type Error = Failure;

    type Scope = Scope;

    type Lease = ();

    type FileBytes = Vec<u8>;

    fn lease(&self) -> Result<Option<Rc<()>>, Failure> {
        Ok(None)
    }

    fn ready<'a>(
        &'a self,
        _: Rc<Descriptor>,
        _: bool,
        _: bool,
        _: Option<Rc<()>>,
        _: &'a Scope,
    ) -> Operation<'a, (), Failure> {
        Box::pin(async { Err(Failure::Invalid) })
    }

    fn resolve<'a>(
        &'a self,
        _: &'a str,
        _: u16,
        _: &'a Scope,
    ) -> Operation<'a, Vec<std::net::SocketAddr>, Failure> {
        Box::pin(async { Err(Failure::Invalid) })
    }

    fn read_file<'a>(
        &'a self,
        _: &'a Path,
        _: usize,
        _: &'a Scope,
    ) -> Operation<'a, Vec<u8>, Failure> {
        Box::pin(async { Err(Failure::Invalid) })
    }
}

impl Host for TestHost {
    fn sleep<'a>(&'a self, until: Instant, scope: &'a Scope) -> Operation<'a, (), Failure> {
        Box::pin(async move {
            scope.check()?;
            let delay = until
                .min(scope.deadline)
                .saturating_duration_since(Instant::now());
            if !delay.is_zero() {
                let (sender, receiver) = futures::channel::oneshot::channel();
                std::thread::spawn(move || {
                    std::thread::sleep(delay);
                    let _ = sender.send(());
                });
                receiver.await.map_err(|_| Failure::Invalid)?;
            }
            scope.check()
        })
    }

    fn classify(&self, error: Failure) -> FailureClass {
        match error {
            Failure::Runtime(uring_runtime::Error::Cancelled) => FailureClass::Cancelled,
            Failure::Runtime(uring_runtime::Error::DeadlineExceeded | uring_runtime::Error::Io)
            | Failure::Core(Error::Capacity | Error::Pending)
            | Failure::Rest(rest::Error::Unavailable) => FailureClass::Retry,
            Failure::Rest(rest::Error::Unauthorized) => FailureClass::Rejected,
            _ => FailureClass::Permanent,
        }
    }
}

/// Integer document codec: F means full, D means an increment from the last base.
struct Numbers;

impl Codec for Numbers {
    type Document = u64;

    type Version = u64;

    type Error = Failure;

    fn version(&self, document: &u64) -> u64 {
        *document
    }

    fn decode(&self, bytes: &[u8]) -> Result<u64, Failure> {
        match bytes {
            [b'F', value] => Ok(u64::from(*value)),
            _ => Err(Failure::Invalid),
        }
    }

    fn delta(&self, base: &u64, bytes: &[u8]) -> Result<u64, Failure> {
        match bytes {
            [b'D', value] => Ok(*base + u64::from(*value)),
            _ => Err(Failure::Invalid),
        }
    }

    fn digest(&self, document: &u64) -> Result<String, Failure> {
        Ok(document.to_string())
    }
}

/// Scripted response or bounded long wait.
enum Reply {
    Response(u16, Vec<u8>),
    Error(Failure, Option<Duration>),
    Wait,
}

/// Scripted source with observable request cursors and delta hashes.
type Requests = Rc<RefCell<Vec<(String, Option<String>)>>>;

/// Scripted source with observable request cursors and delta hashes.
#[derive(Clone)]
struct Script {
    replies: Rc<RefCell<VecDeque<Reply>>>,

    requests: Requests,

    closed: Rc<Cell<bool>>,
}

impl Script {
    /// Queue deterministic responses with shared request and shutdown observations.
    fn new(replies: impl IntoIterator<Item = Reply>) -> Self {
        Self {
            replies: Rc::new(RefCell::new(replies.into_iter().collect())),
            requests: Rc::new(RefCell::new(Vec::new())),
            closed: Rc::new(Cell::new(false)),
        }
    }
}

impl Source<TestHost> for Script {
    fn get<'a>(
        &'a self,
        path: &'a str,
        digest: Option<&'a str>,
        _: usize,
        scope: &'a Scope,
    ) -> Operation<'a, rest::Response, FetchError<Failure>> {
        Box::pin(async move {
            scope.check()?;
            self.requests
                .borrow_mut()
                .push((path.to_owned(), digest.map(str::to_owned)));
            let reply = self.replies.borrow_mut().pop_front().unwrap_or(Reply::Wait);
            match reply {
                Reply::Response(status, body) => Ok(rest::Response {
                    status,
                    body,
                    retry_after: None,
                }),
                Reply::Error(error, retry_after) => Err(FetchError { error, retry_after }),
                Reply::Wait => {
                    TestHost.sleep(scope.deadline, scope).await?;
                    Err(Failure::Invalid.into())
                }
            }
        })
    }

    fn close(&self) {
        self.closed.set(true);
    }
}

/// Encode one complete integer document as a successful response.
fn full(value: u8) -> Reply {
    Reply::Response(200, vec![b'F', value])
}

/// Retain received delta bases and retry malformed deltas only once without a cursor.
#[test]
fn feed_retains_pending_delta_base_and_falls_back_once_with_original_scope() {
    futures::executor::block_on(async {
        let feed = Feed::new(Numbers, "/watch".into(), 16).unwrap();
        let script = Script::new([
            full(1),
            Reply::Response(200, vec![b'D', 1]),
            Reply::Response(200, vec![b'X']),
            full(3),
            Reply::Response(200, vec![b'X']),
            Reply::Response(200, vec![b'X']),
            Reply::Response(204, vec![]),
            full(1),
        ]);
        let scope = Scope::new();
        assert_eq!(
            *feed
                .fetch::<TestHost, _>(&script, &scope)
                .await
                .unwrap()
                .unwrap(),
            1
        );
        assert_eq!(
            *feed
                .fetch::<TestHost, _>(&script, &scope)
                .await
                .unwrap()
                .unwrap(),
            2
        );
        assert_eq!(
            *feed
                .fetch::<TestHost, _>(&script, &scope)
                .await
                .unwrap()
                .unwrap(),
            3
        );
        assert_eq!(
            script.requests.borrow()[1],
            ("/watch?after=1".into(), Some("1".into()))
        );
        assert_eq!(
            script.requests.borrow()[2],
            ("/watch?after=2".into(), Some("2".into()))
        );
        assert_eq!(script.requests.borrow()[3], ("/watch".into(), None));
        assert_eq!(
            feed.fetch::<TestHost, _>(&script, &scope)
                .await
                .unwrap_err()
                .error,
            Failure::Invalid
        );
        assert_eq!(script.requests.borrow().len(), 6);
        assert_eq!(*feed.last().unwrap(), 3);
        assert!(
            feed.fetch::<TestHost, _>(&script, &scope)
                .await
                .unwrap()
                .is_none()
        );
        assert_eq!(
            feed.fetch::<TestHost, _>(&script, &scope)
                .await
                .unwrap_err()
                .error,
            Failure::Core(Error::Replay)
        );
        assert_eq!(*feed.last().unwrap(), 3);
    });
}

/// Invalid framing and cancellation never create a retained document.
#[test]
fn feed_rejects_unbased_unchanged_oversized_and_canceled_responses() {
    futures::executor::block_on(async {
        let feed = Feed::new(Numbers, "/watch".into(), 2).unwrap();
        let script = Script::new([
            Reply::Response(204, vec![]),
            Reply::Response(200, vec![0; 3]),
        ]);
        let scope = Scope::new();
        assert_eq!(
            feed.fetch::<TestHost, _>(&script, &scope)
                .await
                .unwrap_err()
                .error,
            Failure::Rest(rest::Error::InvalidRequest)
        );
        assert_eq!(
            feed.fetch::<TestHost, _>(&script, &scope)
                .await
                .unwrap_err()
                .error,
            Failure::Rest(rest::Error::InvalidRequest)
        );
        scope.canceled.set(true);
        assert_eq!(
            feed.fetch::<TestHost, _>(&script, &scope)
                .await
                .unwrap_err()
                .error,
            Failure::Runtime(uring_runtime::Error::Cancelled)
        );
        assert_eq!(script.requests.borrow().len(), 2);
        assert!(feed.last().is_none());
    });
}

/// Rc-bearing target deliberately cannot cross a worker boundary.
#[derive(Clone)]
struct Installer {
    installed: Rc<RefCell<Vec<u64>>>,

    attempts: Rc<Cell<usize>>,

    blocked: Rc<Cell<bool>>,

    release: Arc<AtomicBool>,

    started: Arc<AtomicU64>,

    threads: Arc<std::sync::Mutex<Vec<std::thread::ThreadId>>>,

    fail: Arc<AtomicBool>,

    install_error: Rc<Cell<Option<Failure>>>,

    prepare_error: Rc<Cell<Option<Failure>>>,
}

impl Installer {
    /// Build an unblocked local target with observable off-thread preparation.
    fn new() -> Self {
        Self {
            installed: Rc::new(RefCell::new(Vec::new())),
            attempts: Rc::new(Cell::new(0)),
            blocked: Rc::new(Cell::new(false)),
            release: Arc::new(AtomicBool::new(true)),
            started: Arc::new(AtomicU64::new(0)),
            threads: Arc::new(std::sync::Mutex::new(Vec::new())),
            fail: Arc::new(AtomicBool::new(false)),
            install_error: Rc::new(Cell::new(None)),
            prepare_error: Rc::new(Cell::new(None)),
        }
    }
}

impl Target<Numbers> for Installer {
    type Prepared = u64;

    fn prepare(&self, document: Arc<u64>) -> Result<Preparation<u64, Failure>, Failure> {
        if let Some(error) = self.prepare_error.take() {
            return Err(error);
        }
        let release = self.release.clone();
        let started = self.started.clone();
        let threads = self.threads.clone();
        let fail = self.fail.clone();
        Ok(Box::new(move || {
            threads.lock().unwrap().push(std::thread::current().id());
            started.fetch_add(1, Ordering::SeqCst);
            let deadline = Instant::now() + Duration::from_secs(3);
            while !release.load(Ordering::Acquire) {
                assert!(Instant::now() < deadline, "test release was not signaled");
                std::thread::sleep(Duration::from_millis(1));
            }
            if fail.swap(false, Ordering::SeqCst) {
                Err(Failure::Invalid)
            } else {
                Ok(*document)
            }
        }))
    }

    fn install(&self, document: &u64, prepared: &u64) -> Result<(), Failure> {
        assert_eq!(document, prepared);
        self.attempts.set(self.attempts.get() + 1);
        if let Some(error) = self.install_error.take() {
            return Err(error);
        }
        if self.blocked.get() {
            return Err(Error::Capacity.into());
        }
        self.installed.borrow_mut().push(*prepared);
        Ok(())
    }
}

/// Use short bounded turns and retry delays for lifecycle scenarios.
fn schedule() -> Schedule {
    Schedule {
        turn: Duration::from_millis(30),
        tick: Duration::from_millis(1),
        retry_min: Duration::from_millis(1),
        retry_max: Duration::from_millis(10),
    }
}

/// Assemble the public synchronization API around the transport-free fixture.
fn driver(script: Script, target: Installer) -> Sync<TestHost, Numbers, Script, Installer> {
    Sync::new(
        Rc::new(TestHost),
        Feed::new(Numbers, "/watch".into(), 16).unwrap(),
        script,
        target,
        schedule(),
    )
    .unwrap()
}

/// Cancellation retains the sole job while later receipts supersede older candidates.
#[test]
fn latest_pending_wins_one_job_survives_cancellation_and_owner_stays_local() {
    futures::executor::block_on(async {
        let target = Installer::new();
        target.release.store(false, Ordering::Release);
        let script = Script::new([full(1), full(2), full(3)]);
        let mut sync = driver(script, target.clone());
        let scope = Scope::new();
        sync.turn(&scope).await.unwrap();
        sync.turn(&scope).await.unwrap();
        sync.turn(&scope).await.unwrap();
        assert_eq!(sync.status().accepted, None);
        assert_eq!(sync.status().pending, Some(3));
        scope.canceled.set(true);
        assert_eq!(
            sync.turn(&scope).await.unwrap_err(),
            Failure::Runtime(uring_runtime::Error::Cancelled)
        );
        assert_eq!(sync.status().error, None);
        assert!(target.started.load(Ordering::Acquire) <= 1);
        target.release.store(true, Ordering::Release);
        let result = sync.turn(&Scope::new()).await;
        assert_eq!(
            result.unwrap_err(),
            Failure::Runtime(uring_runtime::Error::DeadlineExceeded)
        );
        assert_eq!(*target.installed.borrow(), vec![3]);
        assert_eq!(sync.status().accepted, Some(3));
        assert_eq!(target.started.load(Ordering::Acquire), 2);
        for id in target.threads.lock().unwrap().iter() {
            assert_ne!(*id, std::thread::current().id());
        }
        sync.shutdown(&Scope::new()).await.unwrap();
    });
}

/// Local installation progresses independently of a server-directed fetch delay.
#[test]
fn install_retries_during_network_wait_and_retry_after_does_not_advance_acceptance() {
    futures::executor::block_on(async {
        let target = Installer::new();
        target.blocked.set(true);
        let script = Script::new([
            full(1),
            Reply::Error(
                Failure::Rest(rest::Error::Unavailable),
                Some(Duration::from_millis(70)),
            ),
        ]);
        let mut sync = driver(script.clone(), target.clone());
        sync.turn(&Scope::new()).await.unwrap();
        assert_eq!(
            sync.turn(&Scope::new()).await.unwrap_err(),
            Failure::Rest(rest::Error::Unavailable)
        );
        assert!(sync.status().next_fetch > Instant::now() + Duration::from_millis(30));
        assert_eq!(sync.status().accepted, None);
        let before = script.requests.borrow().len();
        target.blocked.set(false);
        assert_eq!(
            sync.turn(&Scope::new()).await.unwrap_err(),
            Failure::Runtime(uring_runtime::Error::DeadlineExceeded)
        );
        assert_eq!(
            script.requests.borrow().len(),
            before,
            "network backoff remains active"
        );
        assert_eq!(
            sync.status().accepted,
            Some(1),
            "install does not wait for the next fetch"
        );
        sync.shutdown(&Scope::new()).await.unwrap();
        assert!(script.closed.get());
    });
}

/// A shutdown deadline closes submissions but retains work for a later fence.
#[test]
fn bounded_shutdown_retains_unfinished_job_for_later_fence() {
    futures::executor::block_on(async {
        let target = Installer::new();
        target.release.store(false, Ordering::Release);
        let mut sync = driver(Script::new([full(1)]), target.clone());
        sync.turn(&Scope::new()).await.unwrap();
        assert_eq!(
            sync.shutdown(&Scope::new()).await.unwrap_err(),
            Failure::Runtime(uring_runtime::Error::DeadlineExceeded)
        );
        assert!(sync.status().stopped);
        assert_eq!(
            sync.turn(&Scope::new()).await.unwrap_err(),
            Failure::Runtime(uring_runtime::Error::Cancelled)
        );
        target.release.store(true, Ordering::Release);
        sync.shutdown(&Scope::new()).await.unwrap();
        assert!(target.installed.borrow().is_empty());
    });
}

/// Rejected preparation restores the accepted delta base before fetching again.
#[test]
fn failed_preparation_restores_accepted_base_and_can_fetch_again() {
    futures::executor::block_on(async {
        let target = Installer::new();
        target.fail.store(true, Ordering::SeqCst);
        let script = Script::new([full(2), Reply::Wait, full(3)]);
        let mut sync = driver(script.clone(), target.clone());
        sync.seed(Arc::new(1)).unwrap();
        sync.turn(&Scope::new()).await.unwrap();
        assert_eq!(
            sync.turn(&Scope::new()).await.unwrap_err(),
            Failure::Invalid
        );
        assert_eq!(sync.status().accepted, Some(1));
        assert_eq!(sync.status().pending, None);
        // A completion can be reaped before the next network future is polled.
        script
            .replies
            .borrow_mut()
            .retain(|reply| !matches!(reply, Reply::Wait));
        sync.turn(&Scope::new()).await.unwrap();
        assert_eq!(script.requests.borrow().last().unwrap().0, "/watch?after=1");
        let _ = sync.turn(&Scope::new()).await;
        assert_eq!(sync.status().accepted, Some(3));
        assert_eq!(*target.installed.borrow(), vec![3]);
        sync.shutdown(&Scope::new()).await.unwrap();
    });
}

/// Repeated delivery of one pending version creates only one preparation job.
#[test]
fn repeated_pending_document_does_not_spawn_duplicate_preparation() {
    futures::executor::block_on(async {
        let target = Installer::new();
        target.release.store(false, Ordering::Release);
        let script = Script::new([full(1), full(1), full(1)]);
        let mut sync = driver(script, target.clone());
        for _ in 0..3 {
            sync.turn(&Scope::new()).await.unwrap();
        }
        target.release.store(true, Ordering::Release);
        let _ = sync.turn(&Scope::new()).await;
        assert_eq!(target.started.load(Ordering::SeqCst), 1);
        assert_eq!(*target.installed.borrow(), vec![1]);
        sync.shutdown(&Scope::new()).await.unwrap();
    });
}

/// A full document whose digest validation is stricter than syntactic decoding.
struct CheckedNumbers;

impl Codec for CheckedNumbers {
    type Document = (u64, u8);

    type Version = u64;

    type Error = Failure;

    fn version(&self, document: &Self::Document) -> u64 {
        document.0
    }

    fn decode(&self, bytes: &[u8]) -> Result<Self::Document, Failure> {
        match bytes {
            [b'F', version, content] => Ok((u64::from(*version), *content)),
            _ => Err(Failure::Invalid),
        }
    }

    fn delta(&self, _: &Self::Document, _: &[u8]) -> Result<Self::Document, Failure> {
        Err(Failure::Invalid)
    }

    fn digest(&self, document: &Self::Document) -> Result<String, Failure> {
        if document.1 == 0 {
            Err(Failure::Invalid)
        } else {
            Ok(document.1.to_string())
        }
    }
}

/// Invalid digests, rollback, and conflicting fallbacks preserve the last good base.
#[test]
fn invalid_digest_and_full_fallback_never_poison_the_retained_base() {
    futures::executor::block_on(async {
        let feed = Feed::new(CheckedNumbers, "/watch".into(), 16).unwrap();
        let response = |v, c| Reply::Response(200, vec![b'F', v, c]);
        let malformed = || Reply::Response(200, vec![b'X']);
        let script = Script::new([
            response(1, 1),
            response(2, 0),
            response(2, 2),
            malformed(),
            response(1, 1),
            malformed(),
            response(2, 3),
            malformed(),
            response(3, 0),
            response(3, 3),
        ]);
        let scope = Scope::new();
        assert_eq!(
            *feed
                .fetch::<TestHost, _>(&script, &scope)
                .await
                .unwrap()
                .unwrap(),
            (1, 1)
        );
        assert_eq!(
            feed.fetch::<TestHost, _>(&script, &scope)
                .await
                .unwrap_err()
                .error,
            Failure::Invalid
        );
        assert_eq!(*feed.last().unwrap(), (1, 1));
        assert_eq!(
            *feed
                .fetch::<TestHost, _>(&script, &scope)
                .await
                .unwrap()
                .unwrap(),
            (2, 2)
        );
        assert_eq!(script.requests.borrow()[2].0, "/watch?after=1");
        for expected in [
            Failure::Core(Error::Replay),
            Failure::Core(Error::Replay),
            Failure::Invalid,
        ] {
            assert_eq!(
                feed.fetch::<TestHost, _>(&script, &scope)
                    .await
                    .unwrap_err()
                    .error,
                expected
            );
            assert_eq!(*feed.last().unwrap(), (2, 2));
        }
        assert_eq!(
            *feed
                .fetch::<TestHost, _>(&script, &scope)
                .await
                .unwrap()
                .unwrap(),
            (3, 3)
        );
        assert_eq!(script.requests.borrow().len(), 10);
    });
}

/// Dropping the driver still joins preparation after an interrupted shutdown.
#[test]
fn driver_drop_joins_unfinished_preparation_even_after_canceled_shutdown() {
    futures::executor::block_on(async {
        let target = Installer::new();
        target.release.store(false, Ordering::Release);
        let mut sync = driver(Script::new([full(1)]), target.clone());
        sync.turn(&Scope::new()).await.unwrap();
        let scope = Scope::new();
        scope.canceled.set(true);
        assert_eq!(
            sync.shutdown(&scope).await.unwrap_err(),
            Failure::Runtime(uring_runtime::Error::Cancelled)
        );
        let release = target.release.clone();
        // Release from an independent finite helper, never from the joined owner.
        let helper = std::thread::spawn(move || {
            std::thread::sleep(Duration::from_millis(10));
            release.store(true, Ordering::Release);
        });
        drop(sync);
        assert!(target.release.load(Ordering::Acquire));
        assert_eq!(target.started.load(Ordering::Acquire), 1);
        assert!(target.installed.borrow().is_empty());
        helper.join().unwrap();
    });
}

/// A rejected candidate may be retried without first advancing acceptance.
#[test]
fn rejected_preparation_and_install_allow_same_version_retry_without_acceptance() {
    futures::executor::block_on(async {
        for install_failure in [false, true] {
            let target = Installer::new();
            if install_failure {
                target.install_error.set(Some(Failure::Invalid));
            } else {
                target.prepare_error.set(Some(Failure::Invalid));
            }
            let script = Script::new([full(2), Reply::Wait]);
            let mut sync = driver(script.clone(), target.clone());
            sync.seed(Arc::new(1)).unwrap();
            let mut result = sync.turn(&Scope::new()).await;
            if install_failure {
                result.unwrap();
                result = sync.turn(&Scope::new()).await;
            }
            assert_eq!(result, Err(Failure::Invalid));
            assert_eq!(sync.status().accepted, Some(1));
            assert_eq!(sync.status().pending, None);
            assert!(target.installed.borrow().is_empty());
            script.replies.borrow_mut().clear();
            script.replies.borrow_mut().push_back(full(2));
            sync.turn(&Scope::new()).await.unwrap();
            assert_eq!(script.requests.borrow().last().unwrap().0, "/watch?after=1");
            let _ = sync.turn(&Scope::new()).await;
            assert_eq!(sync.status().accepted, Some(2));
            assert_eq!(*target.installed.borrow(), vec![2]);
            sync.shutdown(&Scope::new()).await.unwrap();
        }
    });
}

/// Canceled installation retains preparation until a newer candidate supersedes it.
#[test]
fn canceled_install_preserves_prepared_state_and_later_supersession_wins() {
    futures::executor::block_on(async {
        let target = Installer::new();
        target
            .install_error
            .set(Some(uring_runtime::Error::Cancelled.into()));
        let script = Script::new([full(2)]);
        let mut sync = driver(script.clone(), target.clone());
        sync.seed(Arc::new(1)).unwrap();
        sync.turn(&Scope::new()).await.unwrap();
        assert_eq!(
            sync.turn(&Scope::new()).await,
            Err(uring_runtime::Error::Cancelled.into())
        );
        assert_eq!(sync.status().accepted, Some(1));
        assert_eq!(sync.status().pending, Some(2));
        assert_eq!(sync.status().error, None);
        target.blocked.set(true);
        script.replies.borrow_mut().clear();
        script.replies.borrow_mut().push_back(full(3));
        sync.turn(&Scope::new()).await.unwrap();
        target.blocked.set(false);
        let _ = sync.turn(&Scope::new()).await;
        assert_eq!(*target.installed.borrow(), vec![3]);
        assert_eq!(sync.status().accepted, Some(3));
        assert_eq!(target.started.load(Ordering::Acquire), 2);
        sync.shutdown(&Scope::new()).await.unwrap();
    });
}

/// Concurrent snapshot release never hides a part still held by a strong lease.
#[test]
fn pinned_release_during_admission_never_loses_a_live_strong_lease() {
    let cell = Arc::new(Published::new(retention(1)));
    for version in 1..100 {
        let old = publish(&cell, snapshot(version, version)).unwrap();
        let part = old.graph.clone();
        let reader = {
            let cell = cell.clone();
            std::thread::spawn(move || {
                drop(old);
                assert!(Arc::ptr_eq(
                    &cell.resolve(version, Instant::now()).unwrap().unwrap(),
                    &part
                ));
                part
            })
        };
        publish(&cell, snapshot(version + 1, version + 1)).unwrap();
        let part = reader.join().unwrap();
        assert!(Arc::ptr_eq(
            &cell.resolve(version, Instant::now()).unwrap().unwrap(),
            &part
        ));
        drop(part);
    }
}

/// Capacity rejection releases a reserved cut so workers can acknowledge it again.
#[test]
fn capacity_failure_rolls_back_reserved_barrier_before_restaging() {
    let cell = Published::new(retention(0));
    let pin = publish(&cell, snapshot(1, 1)).unwrap();
    let rollout = Rollout::new([0]);
    let generation = rollout.propose(Arc::new(2)).unwrap();
    rollout.ack(0, generation).unwrap();
    let guard = rollout.stage(generation).unwrap();
    assert_eq!(
        cell.publish(
            snapshot(2, 2),
            Instant::now(),
            |_, _| Ok::<_, Error>(()),
            || guard.commit()
        )
        .unwrap_err(),
        Error::Capacity
    );
    assert!(!rollout.committed(generation).unwrap());
    assert!(rollout.pending(&0).unwrap().is_some());
    assert_eq!(cell.current().unwrap().unwrap().sequence, 1);
    drop(pin);
    rollout.ack(0, generation).unwrap();
    let guard = rollout.stage(generation).unwrap();
    cell.publish(
        snapshot(2, 2),
        Instant::now(),
        |_, _| Ok::<_, Error>(()),
        || guard.commit(),
    )
    .unwrap();
    assert!(rollout.committed(generation).unwrap());
}

/// Reject a duration that cannot be represented as an absolute schedule deadline.
#[test]
fn unrepresentable_schedule_is_rejected_before_starting_work() {
    let result = Sync::new(
        Rc::new(TestHost),
        Feed::new(Numbers, "/watch".into(), 16).unwrap(),
        Script::new([]),
        Installer::new(),
        Schedule {
            turn: Duration::MAX,
            ..schedule()
        },
    );
    assert!(matches!(
        result,
        Err(Failure::Rest(rest::Error::InvalidConfiguration))
    ));
}

/// A custom source that cancels just before returning an otherwise valid 204.
struct CanceledResponse;

impl Source<TestHost> for CanceledResponse {
    fn get<'a>(
        &'a self,
        _: &'a str,
        _: Option<&'a str>,
        _: usize,
        scope: &'a Scope,
    ) -> Operation<'a, rest::Response, FetchError<Failure>> {
        Box::pin(async move {
            scope.canceled.set(true);
            Ok(rest::Response {
                status: 204,
                body: vec![],
                retry_after: None,
            })
        })
    }

    fn close(&self) {}
}

/// Cancellation after a valid no-change reply remains cancellation, not success.
#[test]
fn cancellation_after_no_change_response_is_not_reported_as_success() {
    futures::executor::block_on(async {
        let feed = Feed::new(Numbers, "/watch".into(), 16).unwrap();
        feed.restore(Some(Arc::new(1)));
        assert_eq!(
            feed.fetch::<TestHost, _>(&CanceledResponse, &Scope::new())
                .await
                .unwrap_err()
                .error,
            Failure::Runtime(uring_runtime::Error::Cancelled)
        );
        assert_eq!(*feed.last().unwrap(), 1);
    });
}

/// A grace eviction must not erase lookup while its deferred owner is upgradeable.
#[test]
fn weak_upgrade_during_grace_retirement_preserves_live_resolution() {
    let cell = Published::new(Retention {
        old_parts: 1,
        grace_parts: 1,
        grace_bytes: 32,
        grace: Duration::from_secs(30),
    });
    let first = publish(&cell, snapshot(1, 1)).unwrap();
    let weak = Arc::downgrade(&first.graph);
    drop(first);
    publish(&cell, snapshot(2, 2)).unwrap();
    let mut upgraded = None;
    cell.publish(
        snapshot(3, 3),
        Instant::now(),
        |_, _| Ok::<_, Error>(()),
        || {
            upgraded = weak.upgrade();
        },
    )
    .unwrap();
    let upgraded = upgraded.expect("retired structural owner is still alive during commit");
    assert!(Arc::ptr_eq(
        &upgraded,
        &cell.resolve(1, Instant::now()).unwrap().unwrap()
    ));
    drop(upgraded);
    assert!(cell.resolve(1, Instant::now()).unwrap().is_none());
    assert!(weak.upgrade().is_none());
}

/// Virtual timer host advances only when the test explicitly moves its clock.
struct ClockHost;

impl rest::Io for ClockHost {
    type Error = Failure;

    type Scope = Scope;

    type Lease = ();

    type FileBytes = Vec<u8>;

    /// Reuse the transport-free lease policy.
    fn lease(&self) -> Result<Option<Rc<()>>, Failure> {
        TestHost.lease()
    }

    /// Reject real descriptor operations in this virtual timer fixture.
    fn ready<'a>(
        &'a self,
        descriptor: Rc<Descriptor>,
        read: bool,
        write: bool,
        lease: Option<Rc<()>>,
        scope: &'a Scope,
    ) -> Operation<'a, (), Failure> {
        TestHost.ready(descriptor, read, write, lease, scope)
    }

    /// Reject real DNS in this virtual timer fixture.
    fn resolve<'a>(
        &'a self,
        host: &'a str,
        port: u16,
        scope: &'a Scope,
    ) -> Operation<'a, Vec<std::net::SocketAddr>, Failure> {
        TestHost.resolve(host, port, scope)
    }

    /// Reject real file reads in this virtual timer fixture.
    fn read_file<'a>(
        &'a self,
        path: &'a Path,
        limit: usize,
        scope: &'a Scope,
    ) -> Operation<'a, Vec<u8>, Failure> {
        TestHost.read_file(path, limit, scope)
    }
}

impl Host for ClockHost {
    /// Observe virtual time without wall-clock sleeps or detached timer threads.
    fn sleep<'a>(&'a self, until: Instant, scope: &'a Scope) -> Operation<'a, (), Failure> {
        Box::pin(futures::future::poll_fn(move |_| {
            if let Err(error) = scope.check() {
                return std::task::Poll::Ready(Err(error));
            }
            if uring_runtime::environment::now() >= until {
                std::task::Poll::Ready(Ok(()))
            } else {
                std::task::Poll::Pending
            }
        }))
    }

    /// Preserve the same error policy as real-clock lifecycle tests.
    fn classify(&self, error: Failure) -> FailureClass {
        TestHost.classify(error)
    }
}

impl Source<ClockHost> for Script {
    /// Reuse immediate scripted responses; tests never request its wall timer.
    fn get<'a>(
        &'a self,
        path: &'a str,
        digest: Option<&'a str>,
        limit: usize,
        scope: &'a Scope,
    ) -> Operation<'a, rest::Response, FetchError<Failure>> {
        <Self as Source<TestHost>>::get(self, path, digest, limit, scope)
    }

    /// Record transport shutdown for the virtual host.
    fn close(&self) {
        self.closed.set(true);
    }
}

/// Same wire-valid version repeatedly fails domain checks without tick-rate retry.
/// Explicit clock steps prove no request can start before the rejection deadline.
#[test]
fn candidate_rejection_backoff_survives_successful_receipts_and_newer_recovers() {
    use std::{
        future::Future as _,
        task::{Context, Poll},
    };
    use uring_runtime::environment::{SimulationClock, now};
    for phase in 0..3 {
        let clock = SimulationClock::new(42);
        let _environment = clock.environment(0).enter();
        let target = Installer::new();
        let script = Script::new([]);
        let mut sync = Sync::new(
            Rc::new(ClockHost),
            Feed::new(Numbers, "/watch".into(), 16).unwrap(),
            script.clone(),
            target.clone(),
            Schedule {
                turn: Duration::from_secs(10),
                tick: Duration::from_millis(1),
                retry_min: Duration::from_secs(1),
                retry_max: Duration::from_secs(8),
            },
        )
        .unwrap();
        sync.seed(Arc::new(1)).unwrap();
        let scope = Scope {
            deadline: now() + Duration::from_secs(100),
            ..Scope::new()
        };
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        for attempt in 0..6 {
            match phase {
                0 => target.prepare_error.set(Some(Failure::Invalid)),
                1 => target.fail.store(true, Ordering::SeqCst),
                _ => target.install_error.set(Some(Failure::Invalid)),
            }
            script.replies.borrow_mut().push_back(full(2));
            let rejection_at = now();
            let result = futures::executor::block_on(sync.turn(&scope));
            if phase == 0 {
                assert_eq!(result, Err(Failure::Invalid));
            } else {
                result.unwrap();
                // Poll the finite CPU job without advancing virtual time. A ready
                // scripted response keeps the source from entering a host timer.
                script.replies.borrow_mut().push_back(full(2));
                let deadline = Instant::now() + Duration::from_secs(2);
                loop {
                    let mut turn = Box::pin(sync.turn(&scope));
                    match turn.as_mut().poll(&mut cx) {
                        Poll::Ready(result) => {
                            assert_eq!(result, Err(Failure::Invalid));
                            break;
                        }
                        Poll::Pending => {
                            assert!(
                                Instant::now() < deadline,
                                "finite preparation did not finish"
                            );
                            std::thread::yield_now();
                        }
                    }
                }
                script.replies.borrow_mut().clear();
            }
            assert_eq!(sync.status().accepted, Some(1));
            assert_eq!(sync.status().pending, None);
            let retry = sync.status().next_fetch;
            let cap = Duration::from_secs((1 << attempt).min(8));
            assert!(retry >= rejection_at + cap / 2);
            assert!(retry <= rejection_at + cap);
            let requests = script.requests.borrow().len();
            clock.advance(retry.duration_since(now()) - Duration::from_nanos(1));
            {
                let mut turn = Box::pin(sync.turn(&scope));
                assert!(turn.as_mut().poll(&mut cx).is_pending());
            }
            assert_eq!(script.requests.borrow().len(), requests);
            assert_eq!(requests, attempt + 1);
            assert_eq!(
                target.started.load(Ordering::Acquire),
                if phase == 0 { 0 } else { (attempt + 1) as u64 }
            );
            assert!(
                script
                    .requests
                    .borrow()
                    .iter()
                    .all(|(path, _)| path == "/watch?after=1")
            );
            clock.advance(Duration::from_nanos(1));
        }
        // A different rejected version starts at the initial delay rather than
        // inheriting the capped failure count of the previous candidate.
        target.prepare_error.set(Some(Failure::Invalid));
        script.replies.borrow_mut().push_back(full(3));
        assert_eq!(
            futures::executor::block_on(sync.turn(&scope)),
            Err(Failure::Invalid)
        );
        let retry = sync.status().next_fetch;
        assert!(retry >= now() + Duration::from_millis(500));
        assert!(retry <= now() + Duration::from_secs(1));
        clock.advance(retry.duration_since(now()));
        // Corrected same-version input remains eligible, with no poisoned cursor.
        script.replies.borrow_mut().push_back(full(3));
        futures::executor::block_on(sync.turn(&scope)).unwrap();
        // No failed candidate remains queued, and successful install clears its
        // independent retry history. Poll at fixed time until the worker finishes.
        {
            let deadline = Instant::now() + Duration::from_secs(2);
            while target.installed.borrow().is_empty() {
                let mut turn = Box::pin(sync.turn(&scope));
                assert!(turn.as_mut().poll(&mut cx).is_pending());
                assert!(Instant::now() < deadline);
                std::thread::yield_now();
            }
        }
        assert_eq!(sync.status().accepted, Some(3));
        assert_eq!(sync.status().pending, None);
        assert_eq!(sync.status().next_fetch, now() + Duration::from_millis(1));
        assert_eq!(*target.installed.borrow(), vec![3]);
        clock.advance(Duration::from_millis(1));
        target.prepare_error.set(Some(Failure::Invalid));
        script.replies.borrow_mut().push_back(full(4));
        assert_eq!(
            futures::executor::block_on(sync.turn(&scope)),
            Err(Failure::Invalid)
        );
        assert_eq!(sync.status().accepted, Some(3));
        assert_eq!(sync.status().pending, None);
        assert!(sync.status().next_fetch >= now() + Duration::from_millis(500));
        assert!(sync.status().next_fetch <= now() + Duration::from_secs(1));
        assert_eq!(script.requests.borrow().last().unwrap().0, "/watch?after=3");
        futures::executor::block_on(sync.shutdown(&scope)).unwrap();
    }
}
