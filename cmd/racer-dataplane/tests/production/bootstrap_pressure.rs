use super::*;

fn wire(key: u8) -> String {
    request("GET", "Range: bytes=0-16777215\r\n")
        .replace(&"ab".repeat(32), &format!("{key:02x}").repeat(32))
}

fn sockets(key: u8) -> (UnixStream, thread::JoinHandle<Reply>) {
    let (local, mut remote) = UnixStream::pair().unwrap();
    remote.write_all(wire(key).as_bytes()).unwrap();
    (local, thread::spawn(move || receive(remote, false)))
}

fn fetch(rig: &Rig, key: u8) -> Reply {
    let (local, reader) = sockets(key);
    rig.drive(rig.serve(local, &scope())).unwrap();
    reader.join().unwrap()
}

fn page(key: u8, version: u8) -> PageId {
    PageId {
        version: ObjectVersion {
            object: ObjectId {
                cache: CacheId(CACHE.into()),
                key: CacheKey([key; 32]),
            },
            etag: StrongEtag::parse(format!("\"v{version}\"").as_bytes()).unwrap(),
        },
        number: PageNumber(0),
    }
}

#[test]
fn zero_ttl_bootstrap_reclaims_idle_versions_and_new_objects_beyond_byte_budget() {
    for new_objects in [false, true] {
        let rig = Rig::new(113, true, 4);
        for version in 1..=10 {
            rig.adapter.state.lock().unwrap().version = version;
            let key = if new_objects { version } else { 0xab };
            let reply = fetch(&rig, key);
            check(&reply, version, 0, 113, 113);
            assert_eq!(reply.fields["racer-expires-at"], "0");
            rig.flush();
            assert_eq!(rig.adapter.calls().len(), version as usize);
            assert!(rig.admission.used(ResourceClass::Plaintext) <= 4 * P as usize);
            if version == 4 {
                assert_eq!(rig.admission.used(ResourceClass::Plaintext), 4 * P as usize);
            }
        }
        assert!(
            rig.memory
                .get(&page(if new_objects { 1 } else { 0xab }, 1))
                .unwrap()
                .is_none()
        );
        for call in rig.adapter.calls() {
            assert_eq!(call.method, "GET");
            assert_eq!(call.pin, None);
            assert_eq!(call.range.as_deref(), Some("bytes=0-16777215"));
        }
        rig.memory.evict_idle(usize::MAX).unwrap();
        assert_eq!(rig.admission.used(ResourceClass::Plaintext), 0);
        assert_eq!(rig.admission.used(ResourceClass::Ciphertext), 0);
    }
}

#[test]
fn bootstrap_preserves_active_reader_and_inflight_admission_until_cancellation() {
    let rig = Rig::new(113, true, 2);
    check(&fetch(&rig, 0xab), 1, 0, 113, 113);
    rig.flush();
    let held = rig.memory.get(&page(0xab, 1)).unwrap().unwrap().plaintext;
    {
        let mut state = rig.adapter.state.lock().unwrap();
        state.version = 2;
        state.paused = true;
    }
    let (pending, mut pending_remote) = UnixStream::pair().unwrap();
    pending_remote.write_all(wire(2).as_bytes()).unwrap();
    let (blocked, blocked_reader) = sockets(3);
    let pending_scope = scope();
    let blocked_scope = scope();
    let observer = async {
        std::future::poll_fn(|_| {
            if rig.adapter.calls().len() == 2 {
                Poll::Ready(())
            } else {
                Poll::Pending
            }
        })
        .await;
        assert_eq!(rig.admission.used(ResourceClass::Plaintext), 2 * P as usize);
        rig.serve(blocked, &blocked_scope).await.unwrap();
        assert_eq!(
            rig.adapter.calls().len(),
            2,
            "overload must precede origin I/O"
        );
        assert_eq!(rig.admission.used(ResourceClass::Plaintext), 2 * P as usize);
        assert!(rig.memory.get(&page(0xab, 1)).unwrap().is_some());
        assert_eq!(held.bytes()[0], byte(1, 0));
        pending_scope.cancel().unwrap();
    };
    let (pending_result, ()) =
        rig.drive(async { futures::join!(rig.serve(pending, &pending_scope), observer) });
    pending_result.unwrap();
    assert_eq!(receive(pending_remote, false).status, 503);
    assert_eq!(blocked_reader.join().unwrap().status, 503);
    rig.drive(std::future::poll_fn(|_| {
        if rig.admission.used(ResourceClass::Plaintext) == P as usize {
            Poll::Ready(())
        } else {
            Poll::Pending
        }
    }));
    assert_eq!(rig.admission.used(ResourceClass::Plaintext), P as usize);
    rig.adapter.state.lock().unwrap().paused = false;
    check(&fetch(&rig, 3), 2, 0, 113, 113);
    rig.flush();
    // Reclamation must leave the independently held page readable.
    rig.adapter.state.lock().unwrap().version = 3;
    check(&fetch(&rig, 4), 3, 0, 113, 113);
    rig.flush();
    assert_eq!(held.bytes()[0], byte(1, 0));
    assert!(rig.memory.get(&page(0xab, 1)).unwrap().is_some());
    drop(held);
    rig.memory.evict_idle(usize::MAX).unwrap();
    assert_eq!(rig.admission.used(ResourceClass::Plaintext), 0);
    assert_eq!(rig.admission.used(ResourceClass::Ciphertext), 0);
}

#[test]
fn failed_and_empty_bootstraps_release_reclaimed_plaintext_reservations() {
    let rig = Rig::new(113, true, 1);
    check(&fetch(&rig, 0xab), 1, 0, 113, 113);
    rig.flush();
    rig.adapter.offline();
    for attempt in 0..3 {
        assert_eq!(fetch(&rig, 0xab).status, 503);
        assert_eq!(
            rig.adapter.calls().len(),
            attempt + 2,
            "failure must reach origin"
        );
        assert_eq!(rig.admission.used(ResourceClass::Plaintext), 0);
        assert_eq!(rig.admission.used(ResourceClass::Ciphertext), 0);
        assert_eq!(rig.admission.used(ResourceClass::DirtyCiphertext), 0);
    }
    {
        let mut state = rig.adapter.state.lock().unwrap();
        state.online = true;
        state.version = 2;
        state.length = 0;
    }
    let empty = fetch(&rig, 0xab);
    assert_eq!(empty.status, 200);
    assert!(empty.body.is_empty());
    assert_eq!(rig.admission.used(ResourceClass::Plaintext), 0);
    assert_eq!(rig.admission.used(ResourceClass::Ciphertext), 0);
    rig.adapter.state.lock().unwrap().length = 113;
    // Use another version because an immutable version's length cannot change.
    rig.adapter.state.lock().unwrap().version = 3;
    check(&fetch(&rig, 0xab), 3, 0, 113, 113);
    rig.flush();
    assert_eq!(rig.adapter.calls().len(), 6);
}
