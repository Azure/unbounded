//! Long-poll lifecycle scenarios over a scripted wire boundary, ported from
//! the former generic feed tests. Keyring bundles exercise the generic
//! lifecycle; publications exercise delta and content-conflict policy.

use super::*;
use std::{
    collections::VecDeque,
    future::Future,
    sync::atomic::{AtomicBool, AtomicU64, Ordering},
    task::{Context, Poll as Ready},
};

/// Scripted response or bounded long wait.
enum Reply {
    Response(u16, Vec<u8>),
    Error(Error, Option<Duration>),
    Wait,
}

/// Observable request paths and delta digests.
type Requests = Rc<RefCell<Vec<(String, Option<String>)>>>;

/// Scripted fetch with observable requests, shutdown, and demotions.
#[derive(Clone)]
struct Script {
    io: Rc<ReactorControlIo>,

    replies: Rc<RefCell<VecDeque<Reply>>>,

    requests: Requests,

    closed: Rc<Cell<bool>>,

    demoted: Rc<Cell<usize>>,

    /// Cancel the scope just before answering, for cancellation-after-reply checks.
    cancel: bool,
}

impl Script {
    fn new(io: &Rc<ReactorControlIo>, replies: impl IntoIterator<Item = Reply>) -> Self {
        Self {
            io: io.clone(),
            replies: Rc::new(RefCell::new(replies.into_iter().collect())),
            requests: Rc::new(RefCell::new(Vec::new())),
            closed: Rc::new(Cell::new(false)),
            demoted: Rc::new(Cell::new(0)),
            cancel: false,
        }
    }

    fn push(&self, reply: Reply) {
        self.replies.borrow_mut().push_back(reply);
    }
}

impl Fetch for Script {
    fn get<'a>(
        &'a self,
        path: &'a str,
        digest: Option<&'a str>,
        _: usize,
        scope: &'a RequestScope,
    ) -> racer_io::Operation<'a, rest::Response, FetchError> {
        Box::pin(async move {
            scope.check()?;
            self.requests
                .borrow_mut()
                .push((path.to_owned(), digest.map(str::to_owned)));
            if self.cancel {
                scope.cancel()?;
            }
            let reply = self.replies.borrow_mut().pop_front();
            match reply.unwrap_or(Reply::Wait) {
                Reply::Response(status, body) => Ok(rest::Response {
                    status,
                    body,
                    retry_after: None,
                }),
                Reply::Error(error, retry_after) => Err(FetchError { error, retry_after }),
                Reply::Wait => {
                    let far = scope.clock().now() + Duration::from_secs(3600);
                    self.io.sleep(far, scope).await?;
                    Err(Error::Unavailable.into())
                }
            }
        })
    }

    fn close(&self) {
        self.closed.set(true);
    }

    fn demote(&self) {
        self.demoted.set(self.demoted.get() + 1);
    }
}

/// Rc-bearing install hook with observable off-thread preparation.
#[derive(Clone)]
struct Installer {
    installed: Rc<RefCell<Vec<u64>>>,

    blocked: Rc<Cell<bool>>,

    release: Arc<AtomicBool>,

    started: Arc<AtomicU64>,

    threads: Arc<std::sync::Mutex<Vec<std::thread::ThreadId>>>,

    fail: Arc<AtomicBool>,

    install_error: Rc<Cell<Option<Error>>>,

    prepare_error: Rc<Cell<Option<Error>>>,
}

impl Installer {
    fn new() -> Self {
        Self {
            installed: Rc::new(RefCell::new(Vec::new())),
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

impl Install for Installer {
    fn prepare(&self, document: Document) -> Result<Preparation> {
        if let Some(error) = self.prepare_error.take() {
            return Err(error);
        }
        let Document::Keyring(document) = document else {
            panic!("lifecycle fixture follows keyrings");
        };
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
                Err(Error::InvalidRequest)
            } else {
                Ok(Prepared::Keyring(document))
            }
        }))
    }

    fn install(&self, document: &Document, prepared: &Prepared) -> Result<()> {
        let Prepared::Keyring(prepared) = prepared else {
            panic!("lifecycle fixture prepares keyrings");
        };
        assert_eq!(document.version(), prepared.generation.0);
        if let Some(error) = self.install_error.take() {
            return Err(error);
        }
        if self.blocked.get() {
            return Err(Error::Overloaded);
        }
        self.installed.borrow_mut().push(prepared.generation.0);
        Ok(())
    }
}

/// Real reactor adapter over the entered simulated environment.
fn host(env: &racer_io::Env) -> Rc<ReactorControlIo> {
    Rc::new(ReactorControlIo::new(Rc::new(
        crate::runtime::Reactor::new(
            Rc::new(flow_control::Quotas::new(
                crate::admission::AdmissionPolicy::new(
                    crate::test_support::cluster::config(false).limits,
                ),
            )),
            env,
        ),
    )))
}

/// A fresh five-second scope, as each former lifecycle turn used.
fn scope(env: &racer_io::Env) -> RequestScope {
    RequestScope::on(
        &env.ctx.clock,
        crate::model::RequestId([9; 16]),
        env.ctx.now() + Duration::from_secs(5),
    )
    .unwrap()
}

/// Short bounded turns and retry delays for lifecycle scenarios.
fn schedule() -> Schedule {
    Schedule {
        turn: Duration::from_millis(30),
        tick: Duration::from_millis(1),
        retry_min: Duration::from_millis(1),
        retry_max: Duration::from_millis(10),
    }
}

/// Advance simulated time while repolling, with a host watchdog.
fn drive<T>(clock: &racer_io::SimClock, operation: impl Future<Output = T>) -> T {
    let mut operation = std::pin::pin!(operation);
    let until = Instant::now() + Duration::from_secs(10);
    loop {
        if let Ready::Ready(result) = operation
            .as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
        {
            return result;
        }
        assert!(Instant::now() < until, "bounded scripted driver stalled");
        clock.advance(Duration::from_millis(1));
        std::thread::sleep(Duration::from_micros(200));
    }
}

/// Run `body` inside an entered simulated environment.
fn sim(seed: u64, body: impl FnOnce(&racer_io::SimClock, &racer_io::Env)) {
    let clock = racer_io::SimClock::new(seed);
    let env = racer_io::Env::new(racer_io::Env::host().driver.clone(), clock.ctx(0));
    body(&clock, &env);
}

fn bundle(generation: u64) -> wire::KeyringBundle {
    let mut bundle = wire::decode_bundle(include_bytes!(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/wire/testdata/bundle.json"
    )))
    .unwrap();
    bundle.generation.0 = generation;
    // Key ids embed their creation generation; the lifecycle needs none.
    bundle.cache_keys.clear();
    bundle
}

/// One complete keyring as a successful response.
fn full(generation: u64) -> Reply {
    Reply::Response(200, wire::encode_bundle(&bundle(generation)).unwrap())
}

fn key(generation: u64) -> Document {
    Document::Keyring(Arc::new(bundle(generation)))
}

/// Publication fixture; `variant` changes content without changing the sequence.
fn publication(sequence: u64, variant: u64) -> wire::Publication {
    let mut p = wire::decode_publication(include_bytes!(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/wire/testdata/publication.json"
    )))
    .unwrap();
    p.sequence.0 = sequence;
    if variant > 0 {
        p.membership_version.0 += variant;
        p.members[0].site = format!("edge-{variant}");
    }
    p
}

fn published(sequence: u64, variant: u64) -> Reply {
    Reply::Response(
        200,
        wire::encode_publication(&publication(sequence, variant)).unwrap(),
    )
}

fn malformed() -> Reply {
    Reply::Response(200, b"X".to_vec())
}

fn poll(io: &Rc<ReactorControlIo>, doc: Doc, script: &Script, target: &Installer) -> Poll {
    Poll::new(
        io.clone(),
        doc,
        Box::new(script.clone()),
        Box::new(target.clone()),
        schedule(),
    )
    .unwrap()
}

/// Per-document policy is fixed by the document kind.
#[test]
fn document_policy_is_per_variant() {
    assert_eq!(Doc::Publication.path(), wire::SNAPSHOT_PATH);
    assert_eq!(Doc::Keyring.path(), wire::KEYRING_PATH);
    assert_eq!(Doc::Publication.limit(), wire::MAX_PUBLICATION_BYTES);
    assert_eq!(Doc::Keyring.limit(), wire::MAX_BUNDLE_BYTES);
    assert!(Doc::Keyring.limit() < Doc::Publication.limit());
    assert_eq!(Doc::Publication.rejection(), Rejection::IdentityOnly);
    assert_eq!(Doc::Keyring.rejection(), Rejection::TokenFallback);
    assert!(Doc::Publication.deltas());
    assert!(!Doc::Keyring.deltas());
    assert_eq!(classify(Error::Cancelled), FailureClass::Cancelled);
    assert_eq!(classify(Error::Unauthorized), FailureClass::Rejected);
    assert_eq!(classify(Error::Unavailable), FailureClass::Retry);
    assert_eq!(classify(Error::InvalidRequest), FailureClass::Permanent);
}

/// A rejected response demotes its seat; success and 204 do not.
#[test]
fn feed_demotes_seat_after_stale_cursor_or_error_only() {
    sim(1, |clock, env| {
        let io = host(env);
        let script = Script::new(
            &io,
            [
                full(3),
                Reply::Response(204, vec![]),
                full(2),
                Reply::Error(Error::Replay, None),
                full(4),
            ],
        );
        let feed = poll(&io, Doc::Keyring, &script, &Installer::new());
        let scope = scope(env);
        let version =
            |r: std::result::Result<Option<Document>, FetchError>| r.unwrap().map(|d| d.version());
        assert_eq!(version(drive(clock, feed.fetch(&scope))), Some(3));
        assert_eq!(version(drive(clock, feed.fetch(&scope))), None);
        assert_eq!(script.demoted.get(), 0);
        assert_eq!(
            drive(clock, feed.fetch(&scope)).unwrap_err().error,
            Error::Replay
        );
        assert_eq!(script.demoted.get(), 1);
        assert!(drive(clock, feed.fetch(&scope)).is_err());
        assert_eq!(script.demoted.get(), 2);
        assert_eq!(version(drive(clock, feed.fetch(&scope))), Some(4));
        assert_eq!(script.demoted.get(), 2);
        assert_eq!(feed.last().unwrap().version(), 4);
    });
}

/// Keyrings never try a delta: malformed bodies retry once as a cursor-free
/// full GET under the original scope, and the retained base survives failure.
#[test]
fn feed_falls_back_once_without_cursor_and_retains_base() {
    sim(2, |clock, env| {
        let io = host(env);
        let script = Script::new(
            &io,
            [
                full(1),
                malformed(),
                full(3),
                malformed(),
                malformed(),
                Reply::Response(204, vec![]),
                full(1),
            ],
        );
        let feed = poll(&io, Doc::Keyring, &script, &Installer::new());
        let scope = scope(env);
        for expected in [1, 3] {
            let document = drive(clock, feed.fetch(&scope)).unwrap().unwrap();
            assert_eq!(document.version(), expected);
        }
        let path = wire::KEYRING_PATH;
        let digest = key(1).digest().unwrap();
        assert_eq!(
            script.requests.borrow()[1],
            (format!("{path}?after=1"), Some(digest))
        );
        assert_eq!(script.requests.borrow()[2], (path.to_owned(), None));
        assert!(drive(clock, feed.fetch(&scope)).is_err());
        assert_eq!(script.requests.borrow().len(), 5);
        assert_eq!(feed.last().unwrap().version(), 3);
        assert!(drive(clock, feed.fetch(&scope)).unwrap().is_none());
        assert_eq!(
            drive(clock, feed.fetch(&scope)).unwrap_err().error,
            Error::Replay
        );
        assert_eq!(feed.last().unwrap().version(), 3);
    });
}

/// Invalid framing and cancellation never create a retained document.
#[test]
fn feed_rejects_unbased_unchanged_oversized_and_canceled_responses() {
    sim(3, |clock, env| {
        let io = host(env);
        let script = Script::new(
            &io,
            [
                Reply::Response(204, vec![]),
                Reply::Response(200, vec![0; wire::MAX_BUNDLE_BYTES + 1]),
            ],
        );
        let feed = poll(&io, Doc::Keyring, &script, &Installer::new());
        let scope = scope(env);
        for _ in 0..2 {
            assert_eq!(
                drive(clock, feed.fetch(&scope)).unwrap_err().error,
                Error::InvalidRequest
            );
        }
        scope.cancel().unwrap();
        assert_eq!(
            drive(clock, feed.fetch(&scope)).unwrap_err().error,
            Error::Cancelled
        );
        assert_eq!(script.requests.borrow().len(), 2);
        assert!(feed.last().is_none());
    });
}

/// Rollback and conflicting same-version fallbacks preserve the last good base.
#[test]
fn conflicting_publications_and_full_fallback_never_poison_the_retained_base() {
    sim(4, |clock, env| {
        let io = host(env);
        let script = Script::new(
            &io,
            [
                published(1, 0),
                published(2, 0),
                malformed(),
                published(1, 0),
                malformed(),
                published(2, 1),
                malformed(),
                published(3, 0),
            ],
        );
        let feed = poll(&io, Doc::Publication, &script, &Installer::new());
        let scope = scope(env);
        let fetched = |expected: u64| {
            let document = drive(clock, feed.fetch(&scope)).unwrap().unwrap();
            assert_eq!(document.version(), expected);
        };
        fetched(1);
        fetched(2);
        assert_eq!(
            script.requests.borrow()[1].0,
            format!("{}?after=1", wire::SNAPSHOT_PATH)
        );
        let base = feed.last().unwrap().digest().unwrap();
        for _ in 0..2 {
            assert_eq!(
                drive(clock, feed.fetch(&scope)).unwrap_err().error,
                Error::Replay
            );
            assert_eq!(feed.last().unwrap().digest().unwrap(), base);
        }
        fetched(3);
        assert_eq!(script.requests.borrow().len(), 8);
    });
}

/// Cancellation after a valid no-change reply remains cancellation, not success.
#[test]
fn cancellation_after_no_change_response_is_not_reported_as_success() {
    sim(5, |clock, env| {
        let io = host(env);
        let mut script = Script::new(&io, [Reply::Response(204, vec![])]);
        script.cancel = true;
        let feed = poll(&io, Doc::Keyring, &script, &Installer::new());
        feed.restore(Some(key(1)));
        assert_eq!(
            drive(clock, feed.fetch(&scope(env))).unwrap_err().error,
            Error::Cancelled
        );
        assert_eq!(feed.last().unwrap().version(), 1);
    });
}

/// Cancellation retains the sole job while later receipts supersede older candidates.
#[test]
fn latest_pending_wins_one_job_survives_cancellation_and_owner_stays_local() {
    sim(6, |clock, env| {
        let io = host(env);
        let target = Installer::new();
        target.release.store(false, Ordering::Release);
        let script = Script::new(&io, [full(1), full(2), full(3)]);
        let mut sync = poll(&io, Doc::Keyring, &script, &target);
        let turn_scope = scope(env);
        for _ in 0..3 {
            drive(clock, sync.turn(&turn_scope)).unwrap();
        }
        assert_eq!(sync.status().accepted, None);
        assert_eq!(sync.status().pending, Some(3));
        turn_scope.cancel().unwrap();
        assert_eq!(drive(clock, sync.turn(&turn_scope)), Err(Error::Cancelled));
        assert_eq!(sync.status().error, None);
        assert!(target.started.load(Ordering::Acquire) <= 1);
        target.release.store(true, Ordering::Release);
        assert_eq!(
            drive(clock, sync.turn(&scope(env))),
            Err(Error::DeadlineExceeded)
        );
        assert_eq!(*target.installed.borrow(), vec![3]);
        assert_eq!(sync.status().accepted, Some(3));
        assert_eq!(target.started.load(Ordering::Acquire), 2);
        for id in target.threads.lock().unwrap().iter() {
            assert_ne!(*id, std::thread::current().id());
        }
        drive(clock, sync.shutdown(&scope(env))).unwrap();
    });
}

/// Local installation progresses independently of a server-directed fetch delay.
#[test]
fn install_retries_during_network_wait_and_retry_after_does_not_advance_acceptance() {
    sim(7, |clock, env| {
        let io = host(env);
        let target = Installer::new();
        target.blocked.set(true);
        let script = Script::new(
            &io,
            [
                full(1),
                Reply::Error(Error::Unavailable, Some(Duration::from_millis(70))),
            ],
        );
        let mut sync = poll(&io, Doc::Keyring, &script, &target);
        drive(clock, sync.turn(&scope(env))).unwrap();
        assert_eq!(
            drive(clock, sync.turn(&scope(env))),
            Err(Error::Unavailable)
        );
        assert!(sync.status().next_fetch > env.ctx.now() + Duration::from_millis(30));
        assert_eq!(sync.status().accepted, None);
        let before = script.requests.borrow().len();
        target.blocked.set(false);
        assert_eq!(
            drive(clock, sync.turn(&scope(env))),
            Err(Error::DeadlineExceeded)
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
        drive(clock, sync.shutdown(&scope(env))).unwrap();
        assert!(script.closed.get());
    });
}

/// With nothing to prepare or install, a turn parks on the network alone
/// instead of re-arming the local tick timer during the fetch backoff.
#[test]
fn idle_turn_arms_no_tick_timer_during_network_wait() {
    sim(8, |clock, env| {
        let io = host(env);
        let target = Installer::new();
        let script = Script::new(
            &io,
            [
                full(1),
                Reply::Error(Error::Unavailable, Some(Duration::from_millis(15))),
                Reply::Error(Error::Unavailable, None),
            ],
        );
        let mut sync = poll(&io, Doc::Keyring, &script, &target);
        drive(clock, sync.turn(&scope(env))).unwrap();
        drive(clock, sync.settle(&scope(env))).unwrap();
        assert_eq!(sync.status().accepted, Some(1));
        assert_eq!(
            drive(clock, sync.turn(&scope(env))),
            Err(Error::Unavailable)
        );
        let before = sync.status().ticks;
        assert_eq!(
            drive(clock, sync.turn(&scope(env))),
            Err(Error::Unavailable)
        );
        assert_eq!(
            sync.status().ticks,
            before,
            "only the fetch backoff timer is armed"
        );
        drive(clock, sync.shutdown(&scope(env))).unwrap();
    });
}

/// Settling installs a slow preparation without issuing another GET.
#[test]
fn settle_installs_slow_preparation_without_network() {
    sim(9, |clock, env| {
        let io = host(env);
        let target = Installer::new();
        target.release.store(false, Ordering::Release);
        let script = Script::new(&io, [full(1), full(2)]);
        let mut sync = poll(&io, Doc::Keyring, &script, &target);
        drive(clock, sync.settle(&scope(env))).unwrap();
        assert!(script.requests.borrow().is_empty(), "idle settle is local");
        drive(clock, sync.turn(&scope(env))).unwrap();
        assert_eq!(sync.status().pending, Some(1));
        assert_eq!(
            drive(clock, sync.settle(&scope(env))),
            Err(Error::DeadlineExceeded),
            "settle is bounded by the turn schedule"
        );
        target.release.store(true, Ordering::Release);
        drive(clock, sync.settle(&scope(env))).unwrap();
        assert_eq!(*target.installed.borrow(), vec![1]);
        assert_eq!(sync.status().accepted, Some(1));
        assert_eq!(script.requests.borrow().len(), 1, "settle never fetches");
        drive(clock, sync.shutdown(&scope(env))).unwrap();
        assert_eq!(
            drive(clock, sync.settle(&scope(env))),
            Err(Error::Cancelled)
        );
    });
}

/// A shutdown deadline closes submissions but retains work for a later fence.
#[test]
fn bounded_shutdown_retains_unfinished_job_for_later_fence() {
    sim(10, |clock, env| {
        let io = host(env);
        let target = Installer::new();
        target.release.store(false, Ordering::Release);
        let mut sync = poll(&io, Doc::Keyring, &Script::new(&io, [full(1)]), &target);
        drive(clock, sync.turn(&scope(env))).unwrap();
        assert_eq!(
            drive(clock, sync.shutdown(&scope(env))),
            Err(Error::DeadlineExceeded)
        );
        assert!(sync.status().stopped);
        assert_eq!(drive(clock, sync.turn(&scope(env))), Err(Error::Cancelled));
        target.release.store(true, Ordering::Release);
        drive(clock, sync.shutdown(&scope(env))).unwrap();
        assert!(target.installed.borrow().is_empty());
    });
}

/// Rejected preparation restores the accepted delta base before fetching again.
#[test]
fn failed_preparation_restores_accepted_base_and_can_fetch_again() {
    sim(11, |clock, env| {
        let io = host(env);
        let target = Installer::new();
        target.fail.store(true, Ordering::SeqCst);
        let script = Script::new(&io, [full(2), Reply::Wait, full(3)]);
        let mut sync = poll(&io, Doc::Keyring, &script, &target);
        sync.seed(key(1)).unwrap();
        drive(clock, sync.turn(&scope(env))).unwrap();
        assert_eq!(
            drive(clock, sync.turn(&scope(env))),
            Err(Error::InvalidRequest)
        );
        assert_eq!(sync.status().accepted, Some(1));
        assert_eq!(sync.status().pending, None);
        script
            .replies
            .borrow_mut()
            .retain(|reply| !matches!(reply, Reply::Wait));
        // The rejected-candidate floor may defer the next fetch; it still happens.
        let _ = drive(clock, sync.turn(&scope(env)));
        assert_eq!(
            script.requests.borrow().last().unwrap().0,
            format!("{}?after=1", wire::KEYRING_PATH)
        );
        let _ = drive(clock, sync.turn(&scope(env)));
        let _ = drive(clock, sync.settle(&scope(env)));
        assert_eq!(sync.status().accepted, Some(3));
        assert_eq!(*target.installed.borrow(), vec![3]);
        drive(clock, sync.shutdown(&scope(env))).unwrap();
    });
}

/// Repeated delivery of one pending version creates only one preparation job.
#[test]
fn repeated_pending_document_does_not_spawn_duplicate_preparation() {
    sim(12, |clock, env| {
        let io = host(env);
        let target = Installer::new();
        target.release.store(false, Ordering::Release);
        let script = Script::new(&io, [full(1), full(1), full(1)]);
        let mut sync = poll(&io, Doc::Keyring, &script, &target);
        for _ in 0..3 {
            drive(clock, sync.turn(&scope(env))).unwrap();
        }
        target.release.store(true, Ordering::Release);
        drive(clock, sync.settle(&scope(env))).unwrap();
        assert_eq!(target.started.load(Ordering::SeqCst), 1);
        assert_eq!(*target.installed.borrow(), vec![1]);
        drive(clock, sync.shutdown(&scope(env))).unwrap();
    });
}

/// Dropping the poll still joins preparation after an interrupted shutdown.
#[test]
fn driver_drop_joins_unfinished_preparation_even_after_canceled_shutdown() {
    sim(13, |clock, env| {
        let io = host(env);
        let target = Installer::new();
        target.release.store(false, Ordering::Release);
        let mut sync = poll(&io, Doc::Keyring, &Script::new(&io, [full(1)]), &target);
        drive(clock, sync.turn(&scope(env))).unwrap();
        let canceled = scope(env);
        canceled.cancel().unwrap();
        assert_eq!(
            drive(clock, sync.shutdown(&canceled)),
            Err(Error::Cancelled)
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
    for install_failure in [false, true] {
        sim(14, |clock, env| {
            let io = host(env);
            let target = Installer::new();
            if install_failure {
                target.install_error.set(Some(Error::InvalidRequest));
            } else {
                target.prepare_error.set(Some(Error::InvalidRequest));
            }
            let script = Script::new(&io, [full(2), Reply::Wait]);
            let mut sync = poll(&io, Doc::Keyring, &script, &target);
            sync.seed(key(1)).unwrap();
            let mut result = drive(clock, sync.turn(&scope(env)));
            if install_failure {
                result.unwrap();
                result = drive(clock, sync.turn(&scope(env)));
            }
            assert_eq!(result, Err(Error::InvalidRequest));
            assert_eq!(sync.status().accepted, Some(1));
            assert_eq!(sync.status().pending, None);
            assert!(target.installed.borrow().is_empty());
            script.replies.borrow_mut().clear();
            script.push(full(2));
            drive(clock, sync.turn(&scope(env))).unwrap();
            assert_eq!(
                script.requests.borrow().last().unwrap().0,
                format!("{}?after=1", wire::KEYRING_PATH)
            );
            drive(clock, sync.settle(&scope(env))).unwrap();
            assert_eq!(sync.status().accepted, Some(2));
            assert_eq!(*target.installed.borrow(), vec![2]);
            drive(clock, sync.shutdown(&scope(env))).unwrap();
        });
    }
}

/// Canceled installation retains preparation until a newer candidate supersedes it.
#[test]
fn canceled_install_preserves_prepared_state_and_later_supersession_wins() {
    sim(15, |clock, env| {
        let io = host(env);
        let target = Installer::new();
        target.install_error.set(Some(Error::Cancelled));
        let script = Script::new(&io, [full(2)]);
        let mut sync = poll(&io, Doc::Keyring, &script, &target);
        sync.seed(key(1)).unwrap();
        drive(clock, sync.turn(&scope(env))).unwrap();
        assert_eq!(
            drive(clock, sync.settle(&scope(env))),
            Err(Error::Cancelled)
        );
        assert_eq!(sync.status().accepted, Some(1));
        assert_eq!(sync.status().pending, Some(2));
        assert_eq!(sync.status().error, None);
        target.blocked.set(true);
        script.replies.borrow_mut().clear();
        script.push(full(3));
        drive(clock, sync.turn(&scope(env))).unwrap();
        target.blocked.set(false);
        drive(clock, sync.settle(&scope(env))).unwrap();
        assert_eq!(*target.installed.borrow(), vec![3]);
        assert_eq!(sync.status().accepted, Some(3));
        assert_eq!(target.started.load(Ordering::Acquire), 2);
        drive(clock, sync.shutdown(&scope(env))).unwrap();
    });
}

/// Reject a duration that cannot be represented as an absolute schedule deadline.
#[test]
fn unrepresentable_schedule_is_rejected_before_starting_work() {
    sim(16, |_, env| {
        let io = host(env);
        let result = Poll::new(
            io.clone(),
            Doc::Keyring,
            Box::new(Script::new(&io, [])),
            Box::new(Installer::new()),
            Schedule {
                turn: Duration::MAX,
                ..schedule()
            },
        );
        assert!(matches!(result, Err(Error::InvalidConfiguration)));
    });
}

/// Same wire-valid version repeatedly fails domain checks without tick-rate retry.
/// Explicit clock steps prove no request can start before the rejection deadline.
#[test]
fn candidate_rejection_backoff_survives_successful_receipts_and_newer_recovers() {
    for phase in 0..3 {
        sim(17, |clock, env| {
            let io = host(env);
            let target = Installer::new();
            let script = Script::new(&io, []);
            let mut sync = Poll::new(
                io.clone(),
                Doc::Keyring,
                Box::new(script.clone()),
                Box::new(target.clone()),
                Schedule {
                    turn: Duration::from_secs(10),
                    tick: Duration::from_millis(1),
                    retry_min: Duration::from_secs(1),
                    retry_max: Duration::from_secs(8),
                },
            )
            .unwrap();
            sync.seed(key(1)).unwrap();
            let scope = RequestScope::on(
                &env.ctx.clock,
                crate::model::RequestId([10; 16]),
                env.ctx.now() + Duration::from_secs(100),
            )
            .unwrap();
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            let cx = &mut cx;
            // Poll at fixed simulated time until the finite CPU job reports.
            let until_ready = |sync: &mut Poll| {
                let deadline = Instant::now() + Duration::from_secs(2);
                loop {
                    let mut turn = Box::pin(sync.turn(&scope));
                    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                    if let Ready::Ready(result) = turn.as_mut().poll(&mut cx) {
                        return result;
                    }
                    assert!(
                        Instant::now() < deadline,
                        "finite preparation did not finish"
                    );
                    std::thread::yield_now();
                }
            };
            let after = format!("{}?after=1", wire::KEYRING_PATH);
            for attempt in 0..6 {
                match phase {
                    0 => target.prepare_error.set(Some(Error::InvalidRequest)),
                    1 => target.fail.store(true, Ordering::SeqCst),
                    _ => target.install_error.set(Some(Error::InvalidRequest)),
                }
                script.push(full(2));
                let rejection_at = env.ctx.now();
                let result = until_ready(&mut sync);
                if phase == 0 {
                    assert_eq!(result, Err(Error::InvalidRequest));
                } else {
                    result.unwrap();
                    // A ready scripted response keeps the fetch out of a timer.
                    script.push(full(2));
                    assert_eq!(until_ready(&mut sync), Err(Error::InvalidRequest));
                    script.replies.borrow_mut().clear();
                }
                assert_eq!(sync.status().accepted, Some(1));
                assert_eq!(sync.status().pending, None);
                let retry = sync.status().next_fetch;
                let cap = Duration::from_secs((1 << attempt).min(8));
                assert!(retry >= rejection_at + cap / 2);
                assert!(retry <= rejection_at + cap);
                let requests = script.requests.borrow().len();
                clock.advance(retry.duration_since(env.ctx.now()) - Duration::from_nanos(1));
                {
                    let mut turn = Box::pin(sync.turn(&scope));
                    assert!(turn.as_mut().poll(cx).is_pending());
                }
                assert_eq!(script.requests.borrow().len(), requests);
                assert_eq!(
                    target.started.load(Ordering::Acquire),
                    if phase == 0 { 0 } else { (attempt + 1) as u64 }
                );
                assert!(
                    script
                        .requests
                        .borrow()
                        .iter()
                        .all(|(path, _)| *path == after)
                );
                clock.advance(Duration::from_nanos(1));
            }
            // A different rejected version starts at the initial delay.
            target.prepare_error.set(Some(Error::InvalidRequest));
            script.push(full(3));
            assert_eq!(until_ready(&mut sync), Err(Error::InvalidRequest));
            let retry = sync.status().next_fetch;
            assert!(retry >= env.ctx.now() + Duration::from_millis(500));
            assert!(retry <= env.ctx.now() + Duration::from_secs(1));
            clock.advance(retry.duration_since(env.ctx.now()));
            // Corrected same-version input remains eligible, with no poisoned cursor.
            script.push(full(3));
            until_ready(&mut sync).unwrap();
            let deadline = Instant::now() + Duration::from_secs(2);
            while target.installed.borrow().is_empty() {
                let mut turn = Box::pin(sync.turn(&scope));
                assert!(turn.as_mut().poll(cx).is_pending());
                assert!(Instant::now() < deadline);
                std::thread::yield_now();
            }
            assert_eq!(sync.status().accepted, Some(3));
            assert_eq!(sync.status().pending, None);
            assert_eq!(*target.installed.borrow(), vec![3]);
            drive(clock, sync.shutdown(&scope)).unwrap();
        });
    }
}

/// Both endpoints and very large schedules stay inside the promised bounds.
#[test]
fn jitter_is_bounded_inclusive_and_not_constant() {
    for delay in [
        Duration::from_nanos(1),
        Duration::from_secs(10),
        Duration::MAX,
    ] {
        assert_eq!(
            jitter_sample(delay, 0),
            (delay / 2).max(Duration::from_nanos(1))
        );
        assert_eq!(jitter_sample(delay, u32::MAX), delay);
        let middle = jitter_sample(delay, u32::MAX / 2);
        assert!(middle >= delay / 2 && middle <= delay);
    }
    assert_ne!(
        jitter_sample(Duration::from_secs(1), 1),
        jitter_sample(Duration::from_secs(1), u32::MAX)
    );
}
