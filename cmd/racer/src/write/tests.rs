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
    assert_eq!(version_hex(&etag), Some([b'a', b'b'].repeat(16).try_into().unwrap()));
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
