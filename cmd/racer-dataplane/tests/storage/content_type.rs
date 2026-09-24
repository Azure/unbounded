// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;

fn typed(cache: &Cache, name: &[u8]) -> Metadata {
    let mut meta = metadata(cache, "/content-type-transition", 3, now() + 60);
    Rc::make_mut(&mut meta.record).content_type = crate::metadata::ContentType::new(name).unwrap();
    meta.context = meta
        .context
        .with_origin_data(crate::origin_data::OriginData::new(b"Bearer same").unwrap());
    meta
}

fn metadata_reply(record: &Record) -> Reply {
    let bytes = record.to_bytes().to_vec();
    Reply::Checked(allocator::crc64(&bytes), bytes)
}

fn admit(cache: &mut Cache, ring: &mut Ring, meta: &Metadata) -> Metadata {
    let mut upstream = Fake {
        replies: [metadata_reply(&meta.record)].into(),
        ..Default::default()
    };
    let mut fault = cache
        .metadata_in(&meta.context, meta.target(), deadline())
        .unwrap();
    loop {
        match cache.poll_metadata(fault, ring, &mut upstream).unwrap() {
            Progress::Pending { fault: next, .. } => fault = next,
            Progress::Ready(value) => return value,
        }
    }
}

#[test]
fn content_type_transition_rejects_only_exact_metadata_with_old_fault_alive() {
    let Some(mut ring) = crate::conformance::kernel_ring(4, Default::default()) else {
        return;
    };
    let mut cache = cache(1);
    let a = typed(&cache, b"application/a");
    let b = typed(&cache, b"application/b");
    let a = admit(&mut cache, &mut ring, &a);
    let mut backend = Fake {
        content_type: b.record.content_type,
        ..Default::default()
    };
    let old = cache.page::<Fake>(&a, 0, deadline()).unwrap();
    let freshness = old.freshness.clone();
    let delayed = cache.page::<Fake>(&a, 0, deadline()).unwrap();
    let rejected = cache.page::<Fake>(&a, 0, deadline()).unwrap();
    // The producer and its joiner are active before discovering the transition.
    let (old, _) = pending(cache.poll_value(old, &mut ring, &mut backend).unwrap());
    let (rejected, _) = pending(cache.poll_value(rejected, &mut ring, &mut backend).unwrap());
    let error = cache
        .poll_value(old, &mut ring, &mut backend)
        .err()
        .unwrap();
    assert_eq!(
        error.evidence().reason(),
        crate::outcome::PeerReason::MetadataChanged
    );
    let key = a.object.metadata_key().0;
    assert!(
        cache.shards[0]
            .allocator
            .lookup_metadata(&key, now())
            .is_none()
    );
    let mut stale = *a.record;
    stale.expires += 100;
    let mut late_head = Fake {
        replies: [metadata_reply(&stale)].into(),
        ..Default::default()
    };
    let context = a
        .context
        .clone()
        .with_origin_data(crate::origin_data::OriginData::new(b"Bearer late-head").unwrap());
    let late = cache.metadata_in(&context, a.target(), deadline()).unwrap();
    let (late, _) = pending(
        cache
            .poll_metadata(late, &mut ring, &mut late_head)
            .unwrap(),
    );
    let fresh = admit(&mut cache, &mut ring, &b);
    assert_eq!(fresh.record.content_type, b.record.content_type);
    // A joined old fault cannot remove the newly admitted B metadata.
    for old in [rejected] {
        assert_eq!(
            cache
                .poll_value(old, &mut ring, &mut backend)
                .err()
                .unwrap()
                .evidence()
                .reason(),
            crate::outcome::PeerReason::MetadataChanged
        );
        assert_eq!(
            cache.shards[0].allocator.lookup_metadata(&key, now()),
            Some(*b.record)
        );
    }
    let page = cache.page(&fresh, 0, deadline()).unwrap();
    let (value, _) = resolve_checked(&mut cache, &mut ring, &mut backend, page);
    assert_eq!(value.as_slice(), b"xxx");
    // A not-yet-polled old A fault must fail even with B's payload now cached.
    assert_eq!(
        cache
            .poll_value(delayed, &mut ring, &mut backend)
            .err()
            .unwrap()
            .evidence()
            .reason(),
        crate::outcome::PeerReason::MetadataChanged
    );
    assert_eq!(
        cache
            .poll_metadata(late, &mut ring, &mut late_head)
            .err()
            .unwrap()
            .evidence()
            .reason(),
        crate::outcome::PeerReason::MetadataChanged
    );
    assert_eq!(
        cache.shards[0].allocator.lookup_metadata(&key, now()),
        Some(*b.record)
    );
    // A delayed HEAD carrying A also fails, while B can be refreshed with the
    // identical checksum and length. Expiry does not create a new identity.
    assert!(matches!(
        freshness.check(&stale),
        Err(Error::MetadataChanged)
    ));
    assert!(freshness.check(&b.record).is_ok());
    drop(value);
    cache.shutdown(&mut ring).unwrap();
    ring.shutdown().unwrap();
}

#[test]
fn content_type_concurrent_page_flights_isolate_both_producer_orders() {
    let Some(mut ring) = crate::conformance::kernel_ring(4, Default::default()) else {
        return;
    };
    for b_first in [false, true] {
        let mut cache = cache(1);
        let a = typed(&cache, b"application/a");
        let b = typed(&cache, b"application/b");
        let mut backend = Fake {
            content_type: b.record.content_type,
            ..Default::default()
        };
        let a_fault = cache.page::<Fake>(&a, 0, deadline()).unwrap();
        let b_fault = cache.page::<Fake>(&b, 0, deadline()).unwrap();
        assert_eq!(
            a_fault.key(),
            b_fault.key(),
            "persistent payload identity stays independent"
        );
        let (first, second) = if b_first {
            (b_fault, a_fault)
        } else {
            (a_fault, b_fault)
        };
        let (first, _) = pending(cache.poll_value(first, &mut ring, &mut backend).unwrap());
        let (second, _) = pending(cache.poll_value(second, &mut ring, &mut backend).unwrap());
        assert_eq!(
            backend.starts.len(),
            2,
            "different expected metadata must not coalesce"
        );
        let sa = first.scope.as_ref().unwrap();
        let sb = second.scope.as_ref().unwrap();
        assert_ne!(sa.value, sb.value);
        assert_eq!(
            (sa.routing, sa.destination, &sa.dependency),
            (sb.routing, sb.destination, &sb.dependency)
        );
        let fresh = admit(&mut cache, &mut ring, &b);
        let (a_fault, b_fault) = if b_first {
            let (value, _) = resolve_checked(&mut cache, &mut ring, &mut backend, first);
            assert_eq!(value.as_slice(), b"xxx");
            drop(value);
            (second, None)
        } else {
            (first, Some(second))
        };
        let error = cache
            .poll_value(a_fault, &mut ring, &mut backend)
            .err()
            .unwrap();
        assert_eq!(
            error.evidence().reason(),
            crate::outcome::PeerReason::MetadataChanged
        );
        if let Some(b_fault) = b_fault {
            let (value, _) = resolve_checked(&mut cache, &mut ring, &mut backend, b_fault);
            assert_eq!(value.as_slice(), b"xxx");
        }
        assert_eq!(
            cache.shards[0]
                .allocator
                .lookup_metadata(&fresh.object.metadata_key().0, now()),
            Some(*b.record),
            "late A mismatch must not remove admitted B"
        );
        cache.shutdown(&mut ring).unwrap();
    }
    ring.shutdown().unwrap();
}
