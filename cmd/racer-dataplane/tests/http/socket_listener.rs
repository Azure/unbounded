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
        UnixPath::new(self.0.join("cache").to_str().unwrap()).unwrap()
    }
}

impl Drop for Directory {
    fn drop(&mut self) {
        std::fs::remove_dir_all(&self.0).unwrap();
    }
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
