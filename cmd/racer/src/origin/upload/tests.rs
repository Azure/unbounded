use super::*;
use crate::model::RequestId;
use crate::origin::azure::identity;
use futures::executor::block_on;
use std::cell::{Cell, RefCell};

fn scope_for(timeout: Duration) -> RequestScope {
    RequestScope::host(RequestId([3; 16]), Instant::now() + timeout).unwrap()
}

fn context(key: &str) -> OriginContext {
    let (_, hash) = identity::canonical("models", key, "").unwrap();
    OriginContext {
        object: ObjectId {
            cache: CacheId("44444444-4444-4444-8444-444444444444".into()),
            key: hash,
        },
        metadata: None,
        authorization: None,
    }
}

fn id(n: u8) -> [u8; 64] {
    let mut id = [b'A'; 64];
    id[0] = b'A' + n;
    id
}

#[test]
fn retryable_classes() {
    for error in [
        Error::Unavailable,
        Error::BadGateway,
        Error::Overloaded,
        Error::Io,
        Error::Os(libc::ECONNRESET),
    ] {
        assert!(retryable(&error), "{error:?}");
    }
    for error in [
        Error::OriginForbidden,
        Error::OriginRejected,
        Error::NotFound,
        Error::InvalidRequest,
        Error::DeadlineExceeded,
        Error::Canceled,
        Error::Internal,
    ] {
        assert!(!retryable(&error), "{error:?}");
    }
}

#[test]
fn backoff_doubles_to_cap_with_bounded_jitter() {
    let policy = RetryPolicy {
        attempts: 10,
        base: Duration::from_millis(100),
        cap: Duration::from_secs(1),
    };
    for (attempt, full) in [
        (1, 100),
        (2, 200),
        (3, 400),
        (4, 800),
        (5, 1000),
        (40, 1000),
    ] {
        let full = Duration::from_millis(full);
        for entropy in 0..64 {
            let delay = policy.delay(attempt, entropy);
            assert!(delay >= full / 2 && delay <= full, "{attempt} {delay:?}");
        }
    }
    // Jitter varies with entropy and is deterministic.
    assert_ne!(policy.delay(3, 1), policy.delay(3, 2));
    assert_eq!(policy.delay(3, 1), policy.delay(3, 1));
    assert_eq!(
        RetryPolicy {
            attempts: 1,
            base: Duration::ZERO,
            cap: Duration::ZERO
        }
        .delay(1, 9),
        Duration::ZERO
    );
    assert_eq!(RetryPolicy::default().attempts, 10);
}

fn run(
    policy: &RetryPolicy,
    scope: &RequestScope,
    outcomes: Vec<Result<u32>>,
) -> (Result<u32>, u32, Vec<Instant>) {
    let outcomes = RefCell::new(VecDeque::from(outcomes));
    let calls = Cell::new(0);
    let sleeps = RefCell::new(Vec::new());
    let result = block_on(policy.run(
        scope,
        7,
        |until| {
            sleeps.borrow_mut().push(until);
            async { Ok(()) }
        },
        |attempt| {
            calls.set(calls.get() + 1);
            assert_eq!(attempt, calls.get());
            let next = outcomes.borrow_mut().pop_front().unwrap();
            async move { next }
        },
    ));
    (result, calls.get(), sleeps.into_inner())
}

#[test]
fn retry_runs_until_success_or_final_error() {
    let policy = RetryPolicy {
        attempts: 4,
        base: Duration::from_millis(1),
        cap: Duration::from_millis(4),
    };
    let scope = scope_for(Duration::from_secs(60));
    // 503 then 500 then success.
    let (result, calls, sleeps) = run(
        &policy,
        &scope,
        vec![Err(Error::Unavailable), Err(Error::Unavailable), Ok(5)],
    );
    assert_eq!((result, calls, sleeps.len()), (Ok(5), 3, 2));
    // 403 is final.
    let (result, calls, _) = run(&policy, &scope, vec![Err(Error::OriginForbidden)]);
    assert_eq!((result, calls), (Err(Error::OriginForbidden), 1));
    // Attempts are bounded; the last error is returned.
    let (result, calls, sleeps) = run(
        &policy,
        &scope,
        vec![
            Err(Error::Unavailable),
            Err(Error::BadGateway),
            Err(Error::Unavailable),
            Err(Error::Overloaded),
        ],
    );
    assert_eq!(
        (result, calls, sleeps.len()),
        (Err(Error::Overloaded), 4, 3)
    );
}

#[test]
fn retry_stops_when_backoff_passes_deadline() {
    let policy = RetryPolicy {
        attempts: 10,
        base: Duration::from_secs(120),
        cap: Duration::from_secs(120),
    };
    let scope = scope_for(Duration::from_secs(10));
    let (result, calls, sleeps) = run(&policy, &scope, vec![Err(Error::Unavailable)]);
    assert_eq!(
        (result, calls, sleeps.len()),
        (Err(Error::Unavailable), 1, 0)
    );
}

#[test]
fn limiter_caps_concurrency() {
    let limiter = UploadLimiter::new(0, 2);
    let now = Instant::now();
    let Admission::Ready(a) = limiter.try_acquire(10, now) else {
        panic!("first")
    };
    let Admission::Ready(_b) = limiter.try_acquire(10, now) else {
        panic!("second")
    };
    assert!(matches!(limiter.try_acquire(10, now), Admission::Busy));
    assert_eq!(limiter.in_flight(), 2);
    drop(a);
    assert_eq!(limiter.in_flight(), 1);
    assert!(matches!(limiter.try_acquire(10, now), Admission::Ready(_)));
    // Zero concurrency still admits one.
    let one = UploadLimiter::new(0, 0);
    let _held = one.try_acquire(1, now);
    assert!(matches!(one.try_acquire(1, now), Admission::Busy));
}

#[test]
fn limiter_paces_bytes() {
    let limiter = UploadLimiter::new(1000, 100);
    let start = Instant::now();
    // One second of burst is available.
    assert!(matches!(
        limiter.try_acquire(1000, start),
        Admission::Ready(_)
    ));
    // The bucket is empty but not in debt: the next block is admitted and
    // puts the bucket 500 bytes in debt.
    assert!(matches!(
        limiter.try_acquire(500, start),
        Admission::Ready(_)
    ));
    let Admission::Wait(until) = limiter.try_acquire(1, start) else {
        panic!("wait")
    };
    assert_eq!(until - start, Duration::from_millis(500));
    // Just before the debt is repaid: still waiting.
    assert!(matches!(
        limiter.try_acquire(1, start + Duration::from_millis(499)),
        Admission::Wait(_)
    ));
    assert!(matches!(
        limiter.try_acquire(1, start + Duration::from_millis(500)),
        Admission::Ready(_)
    ));
    // A block larger than the burst is admitted from a full bucket, and the
    // debt it leaves is paid off at the configured rate.
    let big = UploadLimiter::new(100, 4);
    assert!(matches!(big.try_acquire(1000, start), Admission::Ready(_)));
    let Admission::Wait(until) = big.try_acquire(1, start) else {
        panic!("wait")
    };
    assert_eq!(until - start, Duration::from_secs(9));
}

#[test]
fn limiter_long_run_rate_property() {
    // Over a long run, admitted bytes never exceed rate * elapsed + burst +
    // one block.
    let rate = 4096u64;
    let block = 1000u64;
    let limiter = UploadLimiter::new(rate, 1000);
    let start = Instant::now();
    let mut now = start;
    let mut admitted = 0u64;
    let mut permits = Vec::new();
    for step in 0..10_000u64 {
        match limiter.try_acquire(block, now) {
            Admission::Ready(permit) => {
                admitted += block;
                permits.push(permit);
                if permits.len() > 50 {
                    permits.clear();
                }
            }
            Admission::Wait(until) => assert!(until > now),
            Admission::Busy => panic!("unexpected busy"),
        }
        now += Duration::from_millis(1 + step % 7);
        let elapsed = (now - start).as_secs_f64();
        assert!(admitted as f64 <= rate as f64 * elapsed + (rate + block) as f64);
    }
    let elapsed = (now - start).as_secs_f64();
    assert!(admitted as f64 >= rate as f64 * elapsed * 0.9 - (rate + block) as f64);
}

#[test]
fn limiter_reconfigure_and_registry() {
    let limits = UploadLimits::default();
    let cache = CacheId("c".into());
    let a = limits.limiter(&cache, 0, 1);
    let Admission::Ready(_p) = a.try_acquire(1, Instant::now()) else {
        panic!("ready")
    };
    // Same cache shares state; new limits apply to the shared limiter.
    let b = limits.limiter(&cache, 0, 2);
    assert_eq!(b.in_flight(), 1);
    assert!(matches!(
        a.try_acquire(1, Instant::now()),
        Admission::Ready(_)
    ));
    // Lowering the rate clamps accumulated credit.
    let r = UploadLimiter::new(1_000_000, 1);
    r.reconfigure(10, 1);
    let now = Instant::now();
    assert!(matches!(r.try_acquire(100, now), Admission::Ready(_)));
    let Admission::Wait(until) = r.try_acquire(1, now) else {
        panic!("wait")
    };
    assert_eq!(until - now, Duration::from_secs(9));
    drop(a);
    drop(b);
    // Weak entries do not keep dead limiters alive.
    for i in 0..64 {
        drop(limits.limiter(&CacheId(format!("x{i}")), 0, 1));
    }
    assert!(limits.0.lock().unwrap().len() <= 17);
}

#[test]
fn memory_sink_commits_in_order() {
    let sink = MemoryUploadSink::new();
    let scope = scope_for(Duration::from_secs(5));
    let ctx = context("obj");
    block_on(sink.put_block(&ctx, &id(1), b"world", &scope)).unwrap();
    block_on(sink.put_block(&ctx, &id(0), b"hello ", &scope)).unwrap();
    // Re-putting a block replaces it.
    block_on(sink.put_block(&ctx, &id(0), b"Hello ", &scope)).unwrap();
    block_on(sink.put_block(&ctx, &id(9), b"unused", &scope)).unwrap();
    assert_eq!(sink.staged(&ctx.object), 3);
    let etag = block_on(sink.put_block_list(&ctx, &[id(0), id(1)], Some("text/plain"), &scope))
        .unwrap()
        .unwrap();
    let blob = sink.committed(&ctx.object).unwrap();
    assert_eq!(blob.data, b"Hello world");
    assert_eq!(blob.etag, etag);
    assert_eq!(blob.content_type.as_deref(), Some("text/plain"));
    assert_eq!(blob.block_ids, vec![id(0), id(1)]);
    // Unused staged blocks are discarded on commit.
    assert_eq!(sink.staged(&ctx.object), 0);
    // If-None-Match: * semantics: a second commit conflicts.
    block_on(sink.put_block(&ctx, &id(0), b"x", &scope)).unwrap();
    assert_eq!(
        block_on(sink.put_block_list(&ctx, &[id(0)], None, &scope)).unwrap(),
        Err(CommitConflict)
    );
    assert_eq!(sink.committed(&ctx.object).unwrap().data, b"Hello world");
    assert_eq!(
        sink.calls(),
        [
            UploadCall::PutBlock,
            UploadCall::PutBlock,
            UploadCall::PutBlock,
            UploadCall::PutBlock,
            UploadCall::PutBlockList,
            UploadCall::PutBlock,
            UploadCall::PutBlockList
        ]
    );
}

#[test]
fn memory_sink_conflicts_missing_blocks_and_faults() {
    let sink = MemoryUploadSink::new();
    let scope = scope_for(Duration::from_secs(5));
    let ctx = context("existing");
    let existing = sink.insert_existing(&ctx.object, b"old".to_vec());
    block_on(sink.put_block(&ctx, &id(0), b"new", &scope)).unwrap();
    assert_eq!(
        block_on(sink.put_block_list(&ctx, &[id(0)], None, &scope)).unwrap(),
        Err(CommitConflict)
    );
    assert_eq!(sink.committed(&ctx.object).unwrap().etag, existing);

    let other = context("other");
    assert_eq!(
        block_on(sink.put_block_list(&other, &[id(0)], None, &scope)).err(),
        Some(Error::InvalidRequest)
    );
    sink.fail_next(UploadCall::PutBlockList, Error::Unavailable);
    sink.fail_next(UploadCall::PutBlock, Error::OriginForbidden);
    assert_eq!(
        block_on(sink.put_block(&other, &id(0), b"a", &scope)).err(),
        Some(Error::OriginForbidden)
    );
    assert_eq!(sink.staged(&other.object), 0);
    block_on(sink.put_block(&other, &id(0), b"a", &scope)).unwrap();
    assert_eq!(
        block_on(sink.put_block_list(&other, &[id(0)], None, &scope)).err(),
        Some(Error::Unavailable)
    );
    // ETags are distinct per commit.
    let etag = block_on(sink.put_block_list(&other, &[id(0)], None, &scope))
        .unwrap()
        .unwrap();
    assert_ne!(etag, existing);
    // A canceled scope fails before touching state.
    let expired = RequestScope::host(RequestId([4; 16]), Instant::now()).unwrap();
    assert!(block_on(sink.put_block(&other, &id(1), b"z", &expired)).is_err());
}

#[test]
fn memory_sink_recognizes_its_own_commit_after_a_lost_response() {
    let sink = MemoryUploadSink::new();
    let scope = scope_for(Duration::from_secs(5));
    let ctx = context("lost");
    block_on(sink.put_block(&ctx, &id(0), b"a", &scope)).unwrap();
    block_on(sink.put_block(&ctx, &id(1), b"b", &scope)).unwrap();
    sink.lose_next_commit_response();
    assert_eq!(
        block_on(sink.put_block_list(&ctx, &[id(0), id(1)], None, &scope)).err(),
        Some(Error::Unavailable)
    );
    let committed = sink.committed(&ctx.object).unwrap();
    // The retry finds the blob it committed: success with its ETag.
    assert_eq!(
        block_on(sink.put_block_list(&ctx, &[id(0), id(1)], None, &scope)).unwrap(),
        Ok(committed.etag.clone())
    );
    // A different block list is still a conflict.
    assert_eq!(
        block_on(sink.put_block_list(&ctx, &[id(0)], None, &scope)).unwrap(),
        Err(CommitConflict)
    );
    assert_eq!(sink.committed(&ctx.object).unwrap(), committed);
}
