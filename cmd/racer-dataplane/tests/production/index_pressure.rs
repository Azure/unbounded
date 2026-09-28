use super::*;

#[test]
fn full_page_writeback_reclaims_idle_ciphertext_at_default_worker_budget() {
    let rig = Rig::new(P, true, 4);
    // Leave exactly the default four-worker shard's 64 MiB ciphertext budget.
    // The fixture otherwise provides additional staging headroom that hides this
    // failure. This charge is unavailable to reclamation, like another live user.
    let _outside_worker_budget = rig
        .admission
        .reserve(
            None,
            ResourceClass::Ciphertext,
            rig.admission.limit(ResourceClass::Ciphertext) - 64 * 1024 * 1024,
        )
        .unwrap();
    for version in 1..=6 {
        rig.adapter.state.lock().unwrap().version = version;
        let read_scope = scope();
        let (local, mut remote) = UnixStream::pair().unwrap();
        let reader = thread::spawn(move || {
            remote
                .write_all(request("GET", "Range: bytes=0-16777215\r\n").as_bytes())
                .unwrap();
            // receive reads exactly Content-Length and drops the client socket.
            receive(remote, false)
        });
        rig.drive(rig.serve(local, &read_scope)).unwrap();
        check(&reader.join().unwrap(), version, 0, P, P);
        read_scope.cancel().unwrap();
        rig.flush();
        assert_eq!(rig.writer.pending_count(), 0);
        assert_eq!(rig.writer.discarded_count(), 0);
        let entries = rig.writer.index().snapshot().unwrap().entries;
        assert!(
            entries.iter().any(|(page, _)| page.version.etag
                == StrongEtag::parse(format!("\"v{version}\"").as_bytes()).unwrap()),
            "successful full-page fill v{version} lost disk publication with an idle writer and reclaimable cached bytes"
        );
        assert!(
            rig.admission.used(ResourceClass::Ciphertext)
                <= rig.admission.limit(ResourceClass::Ciphertext)
        );
    }
    assert_eq!(rig.adapter.calls().len(), 6);
    rig.memory.evict_idle(usize::MAX).unwrap();
    rig.adapter.offline();
    check(
        &rig.request("GET", "If-Match: \"v6\"\r\nRange: bytes=0-16777215\r\n"),
        6,
        0,
        P,
        P,
    );
    assert_eq!(
        rig.adapter.calls().len(),
        6,
        "persisted target must be reusable offline"
    );
}

#[test]
fn writeback_staging_preserves_live_readers_and_recovers_after_release() {
    let rig = Rig::new(P, true, 4);
    let _outside_worker_budget = rig
        .admission
        .reserve(
            None,
            ResourceClass::Ciphertext,
            rig.admission.limit(ResourceClass::Ciphertext) - 64 * 1024 * 1024,
        )
        .unwrap();
    let mut held = Vec::new();
    for version in 1..=2 {
        rig.adapter.state.lock().unwrap().version = version;
        check(
            &rig.request("GET", "Range: bytes=0-16777215\r\n"),
            version,
            0,
            P,
            P,
        );
        rig.flush();
        let entries = rig.writer.index().snapshot().unwrap().entries;
        let (page, _) = entries
            .iter()
            .find(|(page, _)| {
                page.version.etag
                    == StrongEtag::parse(format!("\"v{version}\"").as_bytes()).unwrap()
            })
            .unwrap();
        held.push(rig.memory.get(page).unwrap().unwrap());
    }
    rig.adapter.state.lock().unwrap().version = 3;
    check(
        &rig.request("GET", "Range: bytes=0-16777215\r\n"),
        3,
        0,
        P,
        P,
    );
    rig.flush();
    assert_eq!(
        rig.writer.index().snapshot().unwrap().entries.len(),
        2,
        "live leases cannot be reclaimed to force persistence"
    );
    for (index, page) in held.iter().enumerate() {
        assert_eq!(page.plaintext.bytes()[0], byte(index as u8 + 1, 0));
        assert!(rig.memory.get(page.plaintext.page()).unwrap().is_some());
    }
    drop(held);
    rig.adapter.state.lock().unwrap().version = 4;
    check(
        &rig.request("GET", "Range: bytes=0-16777215\r\n"),
        4,
        0,
        P,
        P,
    );
    rig.flush();
    assert!(
        rig.writer
            .index()
            .snapshot()
            .unwrap()
            .entries
            .iter()
            .any(|(page, _)| page.version.etag == StrongEtag::parse(b"\"v4\"").unwrap())
    );
    assert_eq!(rig.writer.discarded_count(), 0);
    assert!(
        rig.admission.used(ResourceClass::Ciphertext)
            <= rig.admission.limit(ResourceClass::Ciphertext)
    );
}

#[test]
fn small_versions_keep_persisting_at_index_capacity_and_serve_from_disk_offline() {
    let rig = Rig::new(113, true, 4);
    rig.writer.index().set_page_capacity(2).unwrap();
    for version in 1..=4 {
        rig.adapter.state.lock().unwrap().version = version;
        check(
            &rig.request("GET", "Range: bytes=0-16777215\r\n"),
            version,
            0,
            113,
            113,
        );
        rig.flush();
        let entries = rig.writer.index().snapshot().unwrap().entries;
        assert!(entries.len() <= 2);
        let (_, entry) = entries
            .iter()
            .find(|(page, _)| {
                page.version.etag
                    == StrongEtag::parse(format!("\"v{version}\"").as_bytes()).unwrap()
            })
            .expect("each newly served version must actually persist");
        assert_eq!(entry.location.segment.0, 0);
        assert_eq!(entry.location.generation.0, 1);
        assert!(entry.location.location.extent.offset() < 64 * 1024 * 1024);
        rig.memory.evict_idle(usize::MAX).unwrap();
        assert_eq!(rig.admission.used(ResourceClass::Plaintext), 0);
    }
    assert_eq!(rig.writer.discarded_count(), 0);
    assert_eq!(rig.adapter.calls().len(), 4);
    rig.adapter.offline();
    for version in [3, 4] {
        check(
            &rig.request(
                "GET",
                &format!("If-Match: \"v{version}\"\r\nRange: bytes=0-\r\n"),
            ),
            version,
            0,
            113,
            113,
        );
        rig.memory.evict_idle(usize::MAX).unwrap();
    }
    assert_eq!(
        rig.adapter.calls().len(),
        4,
        "disk hits must not reach origin"
    );
}
