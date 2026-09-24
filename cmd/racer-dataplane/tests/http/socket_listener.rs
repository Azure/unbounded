// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use std::os::unix::net::UnixStream;

struct Directory(std::path::PathBuf);

impl Directory {
    fn new() -> Self {
        static NEXT: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);
        let path = std::env::temp_dir().join(format!(
            "uds-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, std::sync::atomic::Ordering::Relaxed)
        ));
        std::fs::create_dir(&path).unwrap();
        Self(path)
    }

    fn socket(&self) -> UnixPath {
        UnixPath::new(self.0.join("client").to_str().unwrap()).unwrap()
    }
}

impl Drop for Directory {
    fn drop(&mut self) {
        std::fs::remove_dir_all(&self.0).unwrap();
    }
}

#[test]
fn nested_socket_directories_inherit_group_and_reject_symlinks() {
    let directory = Directory::new();
    std::fs::set_permissions(&directory.0, std::fs::Permissions::from_mode(0o2770)).unwrap();
    let root = std::fs::metadata(&directory.0).unwrap();
    for kind in ["client", "origin"] {
        let path = UnixPath::new(
            directory
                .0
                .join(format!("cache/{kind}/socket"))
                .to_str()
                .unwrap(),
        )
        .unwrap();
        SharedUnix::prepare_directory(path).unwrap();
        SharedUnix::prepare_directory(path).unwrap();
        for parent in [
            directory.0.join("cache"),
            directory.0.join(format!("cache/{kind}")),
        ] {
            let metadata = std::fs::metadata(parent).unwrap();
            assert_eq!(metadata.mode() & 0o7777, 0o2770);
            assert_eq!(metadata.gid(), root.gid());
        }
        let listener = SharedUnix::bind(path).unwrap();
        assert_eq!(std::fs::metadata(path.as_str()).unwrap().gid(), root.gid());
        drop(listener);
    }
    std::os::unix::fs::symlink(directory.0.join("cache"), directory.0.join("alias")).unwrap();
    let alias = UnixPath::new(directory.0.join("alias/client/socket").to_str().unwrap()).unwrap();
    assert!(SharedUnix::prepare_directory(alias).is_err());
    std::fs::write(directory.0.join("file"), b"keep").unwrap();
    let file = UnixPath::new(directory.0.join("file/client/socket").to_str().unwrap()).unwrap();
    assert!(SharedUnix::prepare_directory(file).is_err());
    assert_eq!(std::fs::read(directory.0.join("file")).unwrap(), b"keep");
}

#[test]
fn workers_share_one_listener_until_last_owner_retires() {
    let directory = Directory::new();
    let path = directory.socket();
    let first = SharedUnix::bind(path).unwrap();
    let second = SharedUnix::bind(path).unwrap();
    assert!(Arc::ptr_eq(&first, &second));
    let descriptor = first.descriptor().unwrap();
    let worker = UnixListener::from(descriptor);
    let client = UnixStream::connect(path.as_str()).unwrap();
    let accepted = worker.accept().unwrap();
    drop((first, accepted, client));
    assert!(std::path::Path::new(path.as_str()).exists());
    let client = UnixStream::connect(path.as_str()).unwrap();
    let accepted = second.listener.accept().unwrap();
    drop((client, accepted));
    let lock = std::fs::metadata(format!("{}.lock", path.as_str())).unwrap();
    drop(second);
    assert!(!std::path::Path::new(path.as_str()).exists());
    assert!(
        worker.accept().is_err(),
        "retired duplicated listener must be shut down"
    );
    let replacement = SharedUnix::bind(path).unwrap();
    let lock_after = std::fs::metadata(format!("{}.lock", path.as_str())).unwrap();
    assert_eq!(
        (lock.dev(), lock.ino()),
        (lock_after.dev(), lock_after.ino())
    );
    assert_eq!(
        std::fs::metadata(path.as_str())
            .unwrap()
            .permissions()
            .mode()
            & 0o777,
        0o660
    );
    drop(replacement);
}

#[test]
fn stale_socket_is_reclaimed_but_live_and_non_socket_paths_are_preserved() {
    let directory = Directory::new();
    let path = directory.socket();
    let foreign = UnixListener::bind(path.as_str()).unwrap();
    assert!(SharedUnix::bind(path).is_err());
    assert!(UnixStream::connect(path.as_str()).is_ok());
    drop(foreign);
    let replacement = SharedUnix::bind(path).unwrap();
    assert!(UnixStream::connect(path.as_str()).is_ok());
    drop(replacement);
    std::fs::write(path.as_str(), b"preserve").unwrap();
    assert!(SharedUnix::bind(path).is_err());
    assert_eq!(std::fs::read(path.as_str()).unwrap(), b"preserve");
    std::fs::remove_file(path.as_str()).unwrap();
    std::os::unix::fs::symlink("missing", path.as_str()).unwrap();
    assert!(SharedUnix::bind(path).is_err());
    assert!(
        std::fs::symlink_metadata(path.as_str())
            .unwrap()
            .file_type()
            .is_symlink()
    );
}

#[test]
fn retirement_never_unlinks_a_replacement_inode() {
    let directory = Directory::new();
    let path = directory.socket();
    let owner = SharedUnix::bind(path).unwrap();
    std::fs::remove_file(path.as_str()).unwrap();
    let replacement = UnixListener::bind(path.as_str()).unwrap();
    drop(owner);
    assert!(UnixStream::connect(path.as_str()).is_ok());
    drop(replacement);
}

#[test]
fn competing_lock_owner_blocks_bind_without_removing_stale_socket() {
    let directory = Directory::new();
    let path = directory.socket();
    drop(UnixListener::bind(path.as_str()).unwrap());
    let lock = OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(format!("{}.lock", path.as_str()))
        .unwrap();
    // SAFETY: lock owns a live descriptor.
    assert_eq!(
        unsafe { libc::flock(lock.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) },
        0
    );
    assert!(SharedUnix::bind(path).is_err());
    assert!(
        std::fs::symlink_metadata(path.as_str())
            .unwrap()
            .file_type()
            .is_socket()
    );
    drop(lock);
    assert!(SharedUnix::bind(path).is_ok());
}
