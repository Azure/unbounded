//! Unit tests of the write service's pure parts. End-to-end write tests on a
//! simulated multi-node cluster live in `crate::app::dst::write_tests`.

use super::*;
use crate::model::CacheKey;
use crate::model::ObjectVersion;
use racer_wire::control::CacheId;

fn object() -> ObjectId {
    ObjectId {
        cache: CacheId("33333333-3333-4333-8333-333333333333".into()),
        key: CacheKey([7; 32]),
    }
}

#[test]
fn minted_etags_are_racer_versions() {
    let etag = mint_etag([0xab; 16]).unwrap();
    assert_eq!(
        etag.as_bytes(),
        b"\"racer-abababababababababababababababab\""
    );
    assert!(is_written_etag(&etag));
    assert_eq!(
        version_hex(&etag),
        Some([b'a', b'b'].repeat(16).try_into().unwrap())
    );
    for other in [
        &b"\"object-1\""[..],
        b"\"racer-ABABABABABABABABABABABABABABABAB\"",
        b"\"racer-abab\"",
        b"\"racer-abababababababababababababababab0\"",
    ] {
        let etag = StrongEtag::parse(other).unwrap();
        assert!(!is_written_etag(&etag), "{other:?}");
    }
}

#[test]
fn duplicated_contexts_keep_their_bytes() {
    let context = OriginContext {
        object: object(),
        metadata: Some(OpaqueMetadata::from_header(b"{\"schema\":1}").unwrap()),
        authorization: Some(Authorization::from_header(b"Bearer x").unwrap()),
    };
    let copy = duplicate_context(&context).unwrap();
    assert_eq!(copy.object, context.object);
    assert_eq!(copy.metadata.unwrap().as_header(), b"{\"schema\":1}");
    assert_eq!(copy.authorization.unwrap().expose_for_origin(), b"Bearer x");
    let bare = bare_context(&object());
    assert!(bare.metadata.is_none() && bare.authorization.is_none());
}

#[test]
fn staging_guards_unstage_on_drop() {
    let staging = WriteStaging::default();
    assert!(staging.is_empty());
    let page = crate::model::PageId {
        version: ObjectVersion {
            object: object(),
            etag: mint_etag([1; 16]).unwrap(),
        },
        number: crate::model::PageNumber(0),
    };
    assert!(staging.get(&page).is_none());
    assert!(!staging.remove(&page));
}

#[test]
fn host_sources_must_be_regular_files_covering_the_window() {
    use std::io::Write;
    // SAFETY: memfd_create takes a NUL-terminated name and returns a new fd.
    let fd = unsafe { libc::memfd_create(c"racer-write-test".as_ptr(), libc::MFD_CLOEXEC) };
    assert!(fd >= 0);
    // SAFETY: `fd` is a fresh descriptor this test owns.
    let mut file = unsafe { <std::fs::File as std::os::fd::FromRawFd>::from_raw_fd(fd) };
    file.write_all(&[5; 100]).unwrap();
    let fd = std::os::fd::OwnedFd::from(file.try_clone().unwrap());
    assert!(client::host_source(vec![fd], 10, 90).is_ok());
    let fd = std::os::fd::OwnedFd::from(file.try_clone().unwrap());
    assert!(client::host_source(vec![fd], 10, 91).is_err());
    let fd = std::os::fd::OwnedFd::from(file.try_clone().unwrap());
    assert!(client::host_source(vec![fd], u64::MAX, 2).is_err());
    assert!(client::host_source(vec![], 0, 1).is_err());
    let a = std::os::fd::OwnedFd::from(file.try_clone().unwrap());
    let b = std::os::fd::OwnedFd::from(file.try_clone().unwrap());
    assert!(client::host_source(vec![a, b], 0, 1).is_err());
    // A directory is not a regular file.
    let dir = std::fs::File::open(std::env::temp_dir()).unwrap();
    assert!(client::host_source(vec![std::os::fd::OwnedFd::from(dir)], 0, 0).is_err());
}

fn committed_entry(hold_until_millis: u64) -> directory::Entry {
    use racer_core::write::{Durability, Holders, Session, SessionEvent, UploadMode, WriteSpec};
    let etag = mint_etag([3; 16]).unwrap();
    let spec = WriteSpec {
        length: 10,
        page_size: 4096,
        content_type: Some("application/x-test".into()),
        replicas: 1,
        durability: Durability::Replicas(1),
        upload: UploadMode::Never,
        hold_until_millis,
    };
    let mut session = Session::open(
        racer_core::write::WriteIdentity {
            cache: object().cache.0,
            key: object().key.0,
            etag: String::from_utf8(etag.as_bytes().to_vec()).unwrap(),
            session: racer_core::write::SessionId([1; 16]),
        },
        spec,
        0,
        60_000,
    );
    let effects = session.on_event(
        SessionEvent::Committed(Holders(vec![vec![b"n".to_vec()]])),
        1,
    );
    assert!(!effects.is_empty());
    directory::Entry::new(
        directory::Role::Coordinator,
        ObjectVersion {
            object: object(),
            etag,
        },
        session,
        None,
    )
}

#[test]
fn directory_serves_committed_versions_until_their_hold() {
    let hold = 1_800_000_000_000;
    let directory = Directory::default();
    assert!(directory.insert(object(), committed_entry(hold)));
    assert!(!directory.insert(object(), committed_entry(hold)));
    // A wall clock with sub-millisecond precision still yields metadata.
    let now = std::time::UNIX_EPOCH + std::time::Duration::from_nanos(1_700_000_000_123_456_789);
    let current = directory.current(&object(), now).expect("visible");
    assert_eq!(current.length, 10);
    assert_eq!(
        current.expires_at.to_unix_millis().unwrap(),
        1_700_000_001_123
    );
    // The reader TTL never passes the hold.
    let late = crate::store::writes::from_unix_millis(hold - 10);
    assert_eq!(
        directory
            .current(&object(), late)
            .unwrap()
            .expires_at
            .to_unix_millis()
            .unwrap(),
        hold
    );
    assert!(
        directory
            .current(&object(), crate::store::writes::from_unix_millis(hold))
            .is_none()
    );
    assert_eq!(
        directory.upload_states(),
        vec![(object().cache.0, racer_core::write::UploadState::Never)]
    );
    let other = ObjectVersion {
        object: object(),
        etag: mint_etag([9; 16]).unwrap(),
    };
    assert!(directory.remove_version(&other).is_none());
    assert_eq!(directory.len(), 1);
    assert!(directory.remove(&object()).is_some());
    assert!(directory.is_empty());
}

#[test]
fn orphan_checks_run_four_times_per_orphan_age_within_bounds() {
    use std::time::Duration;
    assert_eq!(
        gc::check_interval(Duration::from_secs(3600)),
        Duration::from_secs(300)
    );
    assert_eq!(
        gc::check_interval(Duration::from_secs(600)),
        Duration::from_secs(150)
    );
    assert_eq!(
        gc::check_interval(Duration::from_secs(2)),
        Duration::from_secs(1)
    );
}

#[test]
fn new_entries_have_no_repair_and_no_role_decision() {
    let entry = committed_entry(1_800_000_000_000);
    assert!(!entry.repairing);
    assert!(entry.role_checked.is_none());
    assert_eq!(entry.commit_attempts, 0);
}

#[test]
fn upload_state_lookups_reserve_half_the_request_budget() {
    use coordinator::UPLOAD_STATE_TIMEOUT;
    use coordinator::upload_state_deadline;
    use std::time::Duration;
    use std::time::Instant;
    let at = Instant::now();
    // Half of a short budget, leaving the rest to send the response.
    assert_eq!(
        upload_state_deadline(at, Some(at + Duration::from_millis(400))),
        Some(at + Duration::from_millis(200))
    );
    // A long budget is capped.
    assert_eq!(
        upload_state_deadline(at, Some(at + Duration::from_secs(60))),
        Some(at + UPLOAD_STATE_TIMEOUT)
    );
    // An untimed request still bounds the lookup.
    assert_eq!(
        upload_state_deadline(at, None),
        Some(at + UPLOAD_STATE_TIMEOUT)
    );
    // No budget left: skip the lookup.
    assert_eq!(upload_state_deadline(at, Some(at)), None);
    assert_eq!(
        upload_state_deadline(at + Duration::from_secs(1), Some(at)),
        None
    );
    assert_eq!(
        upload_state_deadline(at, Some(at + Duration::from_nanos(1))),
        None
    );
}

/// A maintenance turn that outlasts `TICK` must not leave a past deadline
/// for the worker's park bound, or the worker spins until the turn ends.
#[test]
fn running_maintenance_turn_reports_no_deadline() {
    let service = WriteService::new(Rc::new(directory::Directory::default()));
    let past = Instant::now();
    service.next_tick.set(Some(past));
    assert_eq!(service.next_deadline(), Some(past));
    service.ticking.set(true);
    assert_eq!(service.next_deadline(), None);
    service.ticking.set(false);
    assert_eq!(service.next_deadline(), Some(past));
}
