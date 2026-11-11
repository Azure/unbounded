//! Adapter-persistence caches: pages stay in memory and never reach Racer's
//! disk, and a memory miss first asks the node's own origin adapter for a
//! local copy, falling through to the walk on a miss or failure.

use super::fill::*;
use crate::read::flight::AcquisitionBudget;
use racer_wire::control::CacheMode;

fn acquire_plain(f: &mut Fixture) -> Result<PageResult> {
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
    acquire(f, &mut budget)
}

fn acquire_cipher(f: &mut Fixture) -> Result<crate::memory::CiphertextCopy> {
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
    drive(
        f.fill.acquire_ciphertext(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut budget,
        ),
        &mut f.engine,
        &f.crypto,
        f.fill.drivers(),
    )
}

/// Whether anything for `f.page` reached pending writes or the disk index.
/// Writes are queued synchronously at publication, so a queued write shows
/// up here before the background writer runs.
fn wrote_disk(f: &mut Fixture) -> bool {
    let deps = &f.fill.dependencies;
    deps.writer.pending_count() != 0 || deps.writer.index().lookup(&f.page).unwrap().is_some()
}

/// A cached owner persists its first fill; an adapter-persistence owner keeps
/// the page in memory, serves repeated reads from it, and never writes disk.
#[test]
fn adapter_reads_are_served_from_memory_and_never_written_to_disk() {
    let env = racer_io::Env::host();
    for mode in [CacheMode::Cached, CacheMode::Adapter] {
        let mut f = fixture(&env);
        f.set_mode(&env, mode);
        for _ in 0..3 {
            let result = acquire_plain(&mut f).unwrap();
            assert_eq!(result.plaintext.bytes(), b"abc");
        }
        assert_eq!(f.origin.calls.get(), 1, "{mode:?}");
        // Only adapter caches ask the adapter for a local copy, once, on
        // the first miss.
        assert_eq!(
            f.origin.local_calls.get(),
            usize::from(mode == CacheMode::Adapter),
            "{mode:?}"
        );
        assert!(
            f.fill
                .dependencies
                .memory
                .ciphertext(&f.page)
                .unwrap()
                .is_some()
        );
        assert_eq!(wrote_disk(&mut f), mode == CacheMode::Cached, "{mode:?}");
    }
}

/// A local copy at the adapter is served without the remote origin, encrypted
/// and published to memory like an origin page.
#[test]
fn adapter_local_hit_avoids_the_remote_origin() {
    let env = racer_io::Env::host();
    let mut f = fixture(&env);
    f.set_mode(&env, CacheMode::Adapter);
    f.origin.local.set(LocalReplica::Hit);
    for _ in 0..2 {
        let result = acquire_plain(&mut f).unwrap();
        assert_eq!(result.plaintext.bytes(), b"abc");
    }
    assert_eq!(f.origin.calls.get(), 0);
    assert_eq!(f.origin.local_calls.get(), 1);
    assert_eq!(f.fill.metrics.count(Event::AdapterLocalHit), 1);
    assert_eq!(f.fill.metrics.count(Event::OriginFill), 0);
    assert!(
        f.fill
            .dependencies
            .memory
            .ciphertext(&f.page)
            .unwrap()
            .is_some()
    );
    assert!(!wrote_disk(&mut f));
}

/// An owner answering a peer (ciphertext acquisition) asks its own adapter
/// first as well.
#[test]
fn adapter_owner_ciphertext_fill_uses_the_local_copy() {
    let env = racer_io::Env::host();
    let mut f = fixture(&env);
    f.set_mode(&env, CacheMode::Adapter);
    f.origin.local.set(LocalReplica::Hit);
    let copy = acquire_cipher(&mut f).unwrap();
    assert_eq!(copy.metadata.version, f.page.version);
    assert_eq!(f.origin.calls.get(), 0);
    assert_eq!(f.origin.local_calls.get(), 1);
    assert!(!wrote_disk(&mut f));
}

/// A local miss is cheap: it falls through to the walk and its origin fill
/// without failure accounting.
#[test]
fn adapter_local_miss_falls_through_without_failure_accounting() {
    let env = racer_io::Env::host();
    let mut f = fixture(&env);
    f.set_mode(&env, CacheMode::Adapter);
    let result = acquire_plain(&mut f).unwrap();
    assert_eq!(result.plaintext.bytes(), b"abc");
    assert_eq!(f.origin.local_calls.get(), 1);
    assert_eq!(f.origin.calls.get(), 1);
    assert_eq!(f.fill.metrics.count(Event::AdapterLocalMiss), 1);
    assert_eq!(f.fill.metrics.count(Event::AdapterLocalFailure), 0);
    assert_eq!(f.fill.metrics.count(Event::AdapterLocalHit), 0);
    assert_eq!(f.fill.metrics.count(Event::OriginFill), 1);
}

/// A failed or malformed local read, including a short page, is never served:
/// the fill falls through to the remote origin.
#[test]
fn adapter_local_failures_fall_through_to_the_origin() {
    let env = racer_io::Env::host();
    for local in [
        LocalReplica::Short,
        LocalReplica::Fail(Error::Internal),
        LocalReplica::Fail(Error::BadGateway),
        LocalReplica::Fail(Error::Unavailable),
    ] {
        let mut f = fixture(&env);
        f.set_mode(&env, CacheMode::Adapter);
        f.origin.local.set(local);
        let result = acquire_plain(&mut f).unwrap();
        assert_eq!(result.plaintext.bytes(), b"abc", "{local:?}");
        assert_eq!(f.origin.local_calls.get(), 1, "{local:?}");
        assert_eq!(f.origin.calls.get(), 1, "{local:?}");
        assert_eq!(
            f.fill.metrics.count(Event::AdapterLocalFailure),
            1,
            "{local:?}"
        );
        assert_eq!(f.fill.metrics.count(Event::AdapterLocalHit), 0, "{local:?}");
        assert!(!wrote_disk(&mut f), "{local:?}");
    }
}

/// A stalling local adapter costs at most the probe bound: the fill falls
/// through to the remote origin within the read's deadline.
#[test]
fn adapter_local_stall_falls_through_within_the_read_deadline() {
    let env = racer_io::Env::host();
    let mut f = fixture(&env);
    f.set_mode(&env, CacheMode::Adapter);
    f.origin.local.set(LocalReplica::Stall);
    let bound = crate::read::fill::ADAPTER_LOCAL_TIMEOUT;
    assert!(f.scope.deadline.0 > Instant::now() + bound * 2);
    let started = Instant::now();
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
    let fill = f.fill.clone();
    let mut future = Box::pin(fill.acquire(
        f.page.clone(),
        f.membership.clone(),
        &f.context,
        &f.scope,
        &mut budget,
    ));
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let result = loop {
        assert!(
            started.elapsed() < bound * 4,
            "the stalled probe was not bounded"
        );
        fill.drivers().poll(&mut cx, 64);
        crate::lifecycle::Service::poll_budgeted(&mut f.engine, &mut cx, 64).unwrap();
        f.crypto.poll_budgeted(64).unwrap();
        if let Poll::Ready(value) = future.as_mut().poll(&mut cx) {
            break value.unwrap();
        }
        std::thread::sleep(Duration::from_millis(1));
    };
    drop(future);
    assert_eq!(result.plaintext.bytes(), b"abc");
    assert!(started.elapsed() >= bound);
    assert!(Instant::now() < f.scope.deadline.0);
    assert_eq!(f.origin.local_calls.get(), 1);
    assert_eq!(f.origin.calls.get(), 1);
    assert_eq!(f.fill.metrics.count(Event::AdapterLocalFailure), 1);
    assert_eq!(f.fill.metrics.count(Event::AdapterLocalHit), 0);
}

/// Passthrough caches never ask for a local copy.
#[test]
fn passthrough_never_asks_for_a_local_copy() {
    let env = racer_io::Env::host();
    for mode in [CacheMode::PassthroughPeers, CacheMode::PassthroughLocal] {
        let mut f = fixture(&env);
        f.set_mode(&env, mode);
        f.origin.local.set(LocalReplica::Hit);
        acquire_plain(&mut f).unwrap();
        assert_eq!(f.origin.local_calls.get(), 0, "{mode:?}");
        assert_eq!(f.origin.calls.get(), 1, "{mode:?}");
    }
}
