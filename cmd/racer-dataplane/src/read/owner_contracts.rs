//! Production owner and reclamation contracts carried over from the retired
//! `src/contention.rs` `fidelity` module. That module compared payload-free
//! owner traces with real allocations; the production halves are kept here
//! as plain contract tests. The Fill persistence decision is compared with
//! `racer_core::read::policy::persist` in `src/read/tests/persist_policy.rs`.

use crate::admission::AdmissionPolicy;
use crate::admission::ResourceClass;
use crate::memory::BufferPool;
use crate::memory::MemoryCache;
use crate::memory::PageResult;
use crate::memory::VerifiedBytes;
use crate::memory::VerifiedPage;
use crate::model::CacheKey;
use crate::model::Nonce;
use crate::model::ObjectId;
use crate::model::ObjectVersion;
use crate::model::PAGE_BYTES;
use crate::model::PageEnvelope;
use crate::model::PageId;
use crate::model::PageNumber;
use crate::model::RequestId;
use crate::model::StrongEtag;
use crate::model::VersionMetadata;
use crate::model::WorkerId;
use crate::runtime::RequestScope;
use crate::security;
use crate::security::CryptoClient;
use crate::security::PageCrypto;
use crate::security::PageCryptoEngine;
use crate::worker::CryptoRuntime;
use racer_control_wire::CacheId;
use racer_core::read::policy::Persist;
use racer_core::read::policy::PersistFacts;
use std::num::NonZeroUsize;
use std::rc::Rc;
use std::sync::Arc;
use std::task::Context;
use std::time::Duration;
use std::time::Instant;
use uring_runtime::group::Service;
use uring_runtime::reactor::IoBuffer;

const PLAIN: usize = PAGE_BYTES as usize;
const CIPHER: usize = PLAIN + 16;
const CLASSES: [ResourceClass; 11] = [
    ResourceClass::Plaintext,
    ResourceClass::Ciphertext,
    ResourceClass::DirtyCiphertext,
    ResourceClass::Registered,
    ResourceClass::RequestContext,
    ResourceClass::Flight,
    ResourceClass::Waiter,
    ResourceClass::Connection,
    ResourceClass::Pipe,
    ResourceClass::ControlProgress,
    ResourceClass::Relay,
];

fn admission(pages: usize, dirty: usize) -> Rc<flow_control::Quotas<AdmissionPolicy>> {
    let mut limits = crate::test_support::cluster::config(false).limits;
    limits.plaintext_bytes = NonZeroUsize::new(pages * PLAIN).unwrap();
    limits.ciphertext_bytes = NonZeroUsize::new(pages * CIPHER).unwrap();
    limits.dirty_bytes = NonZeroUsize::new(dirty).unwrap();
    limits.metadata_entries = NonZeroUsize::new(16).unwrap();
    Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits)))
}

fn descriptor(cache: &CacheId, version: &str, length: usize) -> VersionMetadata {
    VersionMetadata {
        content_type: None,
        version: ObjectVersion {
            object: ObjectId {
                cache: cache.clone(),
                key: CacheKey([0; 32]),
            },
            etag: StrongEtag::test_value(version),
        },
        length: length as u64,
    }
}

fn page_id(metadata: &VersionMetadata) -> PageId {
    PageId {
        version: metadata.version.clone(),
        number: PageNumber(0),
    }
}

fn allocated_page(
    admission: &Rc<flow_control::Quotas<AdmissionPolicy>>,
    cache: &CacheId,
    version: &str,
    length: usize,
) -> PageResult {
    let metadata = descriptor(cache, version, length);
    let id = page_id(&metadata);
    let reserved = crate::admission::reserve_fill(admission, cache, false).unwrap();
    let pool = BufferPool::new(admission.clone());
    let mut plaintext = pool.plaintext(reserved.plaintext, length).unwrap();
    plaintext.bytes_mut().unwrap().fill(7);
    let (bytes, reservation) = plaintext.into_parts();
    // Structural fixture only: production allocation and owner layout, not an
    // authentication oracle (security/aead.rs performs this transfer).
    let plaintext = VerifiedPage {
        inner: Arc::new(VerifiedBytes {
            page: id.clone(),
            storage: flow_control::ChargedBytes {
                bytes: bytes.into_vec(),
                reservation,
            },
        }),
    };
    let ciphertext = pool
        .ciphertext(
            reserved.ciphertext,
            PageEnvelope {
                cipher: crate::model::PageCipher::Xaes256Gcm,
                page: id,
                key_id: crate::model::key_id_from_generation(1, 1).unwrap(),
                nonce: Nonce([2; 24]),
                plaintext_length: length as u32,
                ciphertext_length: (length + 16) as u32,
            },
            vec![9; length + 16],
        )
        .unwrap();
    PageResult {
        metadata: metadata.for_pin(),
        plaintext,
        ciphertext,
    }
}

fn occupancy(admission: &flow_control::Quotas<AdmissionPolicy>) -> [usize; 3] {
    admission.reclaim_buffers();
    [
        ResourceClass::Plaintext,
        ResourceClass::Ciphertext,
        ResourceClass::DirtyCiphertext,
    ]
    .map(|class| admission.used(class))
}

/// Old: `duplicate_owners_match_full_page_occupancy_until_each_last_owner`.
/// Duplicate readers charge once; the cache retains ciphertext and holds
/// plaintext weakly, so a plaintext-only reader does not pin cached
/// ciphertext; lookup removal is not a completion fence.
#[test]
fn duplicate_owners_charge_once_until_each_last_owner() {
    let real = admission(2, CIPHER);
    let cache = CacheId(crate::test_support::security::CACHE.into());
    let memory = MemoryCache::new(
        BufferPool::new(real.clone()),
        crate::test_support::availability(),
    );
    let page = allocated_page(&real, &cache, "v1", PLAIN);
    let id = page.plaintext.page().clone();
    let delivered = page.plaintext.clone();
    memory.publish(page).unwrap();
    assert_eq!(occupancy(&real), [PLAIN, CIPHER, 0], "publish");

    let read = memory.get(&id).unwrap().unwrap();
    let again = memory.get(&id).unwrap().unwrap();
    assert!(Arc::ptr_eq(&read.plaintext.inner, &again.plaintext.inner));
    assert!(Arc::ptr_eq(&read.ciphertext.inner, &again.ciphertext.inner));
    drop(again);
    assert_eq!(occupancy(&real), [PLAIN, CIPHER, 0], "duplicate reader");
    assert_eq!(memory.evict_idle(usize::MAX), Ok(0));
    drop(read.plaintext);
    assert_eq!(memory.evict_idle(usize::MAX), Ok(0), "ciphertext reader");
    drop(read.ciphertext);
    let plain = memory.get(&id).unwrap().unwrap().plaintext;
    assert!(Arc::ptr_eq(&plain.inner, &delivered.inner));
    // A plaintext reader does not pin the cached ciphertext.
    assert_eq!(memory.evict_idle(usize::MAX), Ok(CIPHER));
    assert_eq!(occupancy(&real), [PLAIN, 0, 0], "plaintext-only reader");
    drop(delivered);
    memory.remove_cache(&cache).unwrap();
    assert!(memory.get(&id).unwrap().is_none());
    assert_eq!(occupancy(&real), [PLAIN, 0, 0], "lookup removal");
    assert_eq!(plain.bytes().len(), PLAIN);
    assert_eq!(plain.bytes()[PLAIN - 1], 7);
    drop(plain);
    assert_eq!(occupancy(&real), [0, 0, 0], "last owner released");
}

/// Old: `fair_reclamation_matches_metadata_trace_and_keeps_other_cache`.
/// Cache A over its fair share cannot reserve by evicting cache B: the local
/// deficit wins over global room, busy A pages survive, and once idle only
/// A's oldest ciphertext is reclaimed. Plaintext pressure cannot be relieved
/// by the cache, which holds plaintext weakly.
#[test]
fn fair_reclamation_keeps_other_cache() {
    for class in [ResourceClass::Plaintext, ResourceClass::Ciphertext] {
        let real = admission(4, CIPHER);
        let a = CacheId(crate::test_support::security::CACHE.into());
        let b = CacheId("44444444-4444-4444-8444-444444444444".into());
        let memory = MemoryCache::new(
            BufferPool::new(real.clone()),
            crate::test_support::availability_for(vec![a.clone(), b.clone()]),
        );
        let mut ids = Vec::new();
        for (cache, version) in [(&b, "other"), (&a, "old"), (&a, "new")] {
            let page = allocated_page(&real, cache, version, PLAIN);
            ids.push(page.plaintext.page().clone());
            memory.publish(page).unwrap();
        }
        let cipher = matches!(class, ResourceClass::Ciphertext);
        assert_eq!(occupancy(&real), [0, 3 * CIPHER, 0]);
        let amount = if cipher { CIPHER } else { PLAIN };
        if cipher {
            assert!(matches!(
                real.reserve(Some(&a), class, amount),
                Err(flow_control::Error::Overloaded)
            ));
            let (owner, deficit) = real.reclamation(&a, class, amount).unwrap();
            assert_eq!(owner, Some(a.clone()));
            assert!(deficit > 0 && deficit <= amount);
        }
        // Readers of A's pages hold ciphertext and pin them.
        let reads: Vec<_> = ids[1..]
            .iter()
            .map(|id| memory.ciphertext(id).unwrap().unwrap())
            .collect();
        assert_eq!(memory.reclaim_idle(class, Some(&a), amount, |_, _| 0), 0);
        assert_eq!(occupancy(&real), [0, 3 * CIPHER, 0], "busy pages survive");
        drop(reads);
        let retained = if cipher {
            assert_eq!(
                memory.reclaim_idle(class, Some(&a), amount, |_, _| 0),
                amount
            );
            assert!(memory.ciphertext(&ids[1]).unwrap().is_none());
            2
        } else {
            assert_eq!(memory.reclaim_idle(class, Some(&a), amount, |_, _| 0), 0);
            assert!(memory.ciphertext(&ids[1]).unwrap().is_some());
            3
        };
        assert!(memory.ciphertext(&ids[0]).unwrap().is_some(), "other cache");
        assert!(memory.ciphertext(&ids[2]).unwrap().is_some());
        assert_eq!(occupancy(&real), [0, retained * CIPHER, 0]);
        let next = real.reserve(Some(&a), class, amount).unwrap();
        drop(next);
        assert_eq!(memory.evict_idle(usize::MAX), Ok(retained * CIPHER));
        assert_eq!(occupancy(&real), [0, 0, 0], "drained");
    }
}

fn scope() -> RequestScope {
    RequestScope::new(
        RequestId([1; 16]),
        Instant::now() + Duration::from_secs(600),
    )
    .unwrap()
}

/// The optional headroom boundary of `persist`: a non-owned eligible page is
/// persisted only within the optional ceiling. Production
/// `Fill::persist_verified` (`src/read/fill.rs`) re-exports and calls the same
/// `optional_headroom`; this pins the boundary both use.
#[test]
fn non_owned_persistence_uses_optional_headroom() {
    let limit = 4 * CIPHER;
    for (used, expected) in [
        (0, Persist::Write),
        (CIPHER, Persist::Write),
        (2 * CIPHER - 19, Persist::Write),
        (2 * CIPHER - 18, Persist::NoHeadroom),
        (3 * CIPHER, Persist::NoHeadroom),
    ] {
        let facts = PersistFacts {
            eligible: true,
            metadata_valid: true,
            resident: false,
            owned: false,
            dirty_limit: limit,
            dirty_used: used,
            amount: 19,
            one_page: CIPHER,
        };
        assert_eq!(racer_core::read::policy::persist(facts), expected, "{used}");
        assert_eq!(
            crate::read::fill::optional_headroom(limit, used, 19, CIPHER),
            expected == Persist::Write
        );
        assert_eq!(
            racer_core::read::policy::persist(PersistFacts {
                owned: true,
                ..facts
            }),
            Persist::Write
        );
    }
}

/// Old: `canceled_crypto_matches_metadata_owner_trace_through_completion_reap`.
/// A canceled caller is not a completion fence: page charges stay held after
/// the caller drops its work and until the crypto completion is reaped, even
/// after admission stops.
#[test]
fn canceled_crypto_holds_owners_until_completion_reap() {
    let real = admission(1, CIPHER);
    let cache = CacheId(crate::test_support::security::CACHE.into());
    let reserved = crate::admission::reserve_fill(&real, &cache, false).unwrap();
    let buffers = BufferPool::new(real.clone());
    let plaintext = buffers.plaintext(reserved.plaintext, 3).unwrap();
    let (port, engine_port) = security::pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
    let client = Rc::new(CryptoClient::new(port));
    let crypto = PageCrypto::new(
        Rc::new(crate::test_support::security::keys()),
        client.clone(),
    );
    let mut engine = PageCryptoEngine::new(CryptoRuntime { port: engine_port });
    let scope = scope();
    let mut work = crypto.encrypt(
        page_id(&descriptor(&cache, "v1", 3)),
        plaintext,
        reserved.ciphertext,
        &scope,
    );
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(work.as_mut().poll(&mut cx).is_pending());
    assert_eq!(client.outstanding(), 1);
    assert_eq!(occupancy(&real), [3, CIPHER, 0], "accepted crypto");
    scope.cancel().unwrap();
    assert!(work.as_mut().poll(&mut cx).is_pending());
    drop(work);
    real.stop();
    client.poll_budgeted(1).unwrap();
    assert_eq!(occupancy(&real), [3, CIPHER, 0], "canceled caller");
    assert_eq!(client.outstanding(), 1);
    engine.poll_budgeted(&mut cx, 1).unwrap();
    assert_eq!(occupancy(&real), [3, CIPHER, 0], "published, not reaped");
    assert_eq!(client.outstanding(), 1);
    client.poll_budgeted(1).unwrap();
    assert_eq!(client.outstanding(), 0);
    assert_eq!(occupancy(&real), [0, 0, 0], "reaped");
    assert!(CLASSES.iter().all(|class| real.used(*class) == 0));
}

/// Limits of the old model's `global_capacity_config`: 16 pages, 16 dirty
/// pages, 6 pipes, 64 queue entries.
fn global_capacity() -> flow_control::Quotas<AdmissionPolicy> {
    const CONTEXT: usize = 256;
    let nz = |n: usize| NonZeroUsize::new(n).unwrap();
    let mut limits = crate::test_support::cluster::config(false).limits;
    limits.plaintext_bytes = nz(16 * PLAIN);
    limits.ciphertext_bytes = nz(16 * CIPHER);
    limits.dirty_bytes = nz(16 * CIPHER);
    limits.pipes = nz(6);
    limits.queue_entries = nz(64);
    limits.flights = nz(16);
    limits.waiters_per_flight = nz(64);
    limits.client_connections = nz(64 + 6);
    limits.request_context_bytes = nz((64 + 6) * CONTEXT);
    limits.metadata_entries = nz(64);
    flow_control::Quotas::new(AdmissionPolicy::new(limits))
}

/// Old: `two_live_caches_can_use_global_transport_capacity_until_exhaustion`
/// and `two_caches_with_spare_pipes_do_not_park_readers`. Pipe and connection
/// leases are node-wide (no cache key, as `PipePool` and `HttpPool` charge
/// them): one live cache may use more than half while another cache holds
/// request contexts, until the global limit.
#[test]
fn transport_capacity_is_global_across_live_caches() {
    for class in [ResourceClass::Pipe, ResourceClass::Connection] {
        let admission = global_capacity();
        let caches = [CacheId("0".into()), CacheId("1".into())];
        let contexts: Vec<_> = caches
            .iter()
            .map(|cache| {
                admission
                    .reserve(Some(cache), ResourceClass::RequestContext, 256)
                    .unwrap()
            })
            .collect();
        let limit = admission.limit(class);
        let leases: Vec<_> = (0..limit)
            .map(|slot| {
                admission
                    .reserve(None, class, 1)
                    .unwrap_or_else(|_| panic!("{class:?} rejected at {slot}/{limit}"))
            })
            .collect();
        assert!(leases.iter().all(|lease| lease.key().is_none()));
        assert_eq!(admission.used(class), limit);
        assert!(admission.reserve(None, class, 1).is_err());
        let mut leases = leases;
        drop(leases.pop());
        let reused = admission.reserve(None, class, 1).unwrap();
        assert_eq!(admission.used(class), limit);
        drop((leases, reused, contexts));
        assert!(CLASSES.iter().all(|class| admission.used(*class) == 0));
    }
}

/// Old: `cache_scoped_resources_still_enforce_fairness_with_global_room`.
/// Cache-keyed classes keep per-cache fair share even with global room: a
/// cache holding half the limit is rejected once another cache is live.
#[test]
fn cache_scoped_resources_enforce_fairness_with_global_room() {
    for (class, amount) in [
        (ResourceClass::Plaintext, PLAIN),
        (ResourceClass::Ciphertext, CIPHER),
        (ResourceClass::DirtyCiphertext, CIPHER),
        (ResourceClass::Registered, CIPHER),
        (ResourceClass::RequestContext, 256),
        (ResourceClass::Flight, 1),
        (ResourceClass::Waiter, 1),
    ] {
        let admission = global_capacity();
        let (a, b) = (CacheId("0".into()), CacheId("1".into()));
        let limit = admission.limit(class);
        let first = admission.reserve(Some(&a), class, limit / 2).unwrap();
        let second = admission.reserve(Some(&b), class, amount).unwrap();
        assert_eq!(first.key(), Some(&a));
        assert_eq!(second.key(), Some(&b));
        assert!(admission.used(class) + amount <= limit);
        assert!(
            admission.reserve(Some(&a), class, amount).is_err(),
            "{class:?}"
        );
        drop((first, second));
        assert_eq!(admission.used(class), 0);
    }
}
