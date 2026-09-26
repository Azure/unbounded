use super::*;

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
