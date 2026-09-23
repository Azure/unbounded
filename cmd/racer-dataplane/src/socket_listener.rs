// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Process-wide filesystem listener ownership; accepts remain worker-local.

use crate::socket::UnixPath;
use std::{
    collections::BTreeMap,
    fs::{File, OpenOptions},
    io,
    os::{
        fd::{AsRawFd, OwnedFd},
        unix::{
            fs::{FileTypeExt, MetadataExt, OpenOptionsExt, PermissionsExt},
            net::UnixListener,
        },
    },
    sync::{Arc, Mutex, OnceLock, Weak},
};

pub(crate) struct SharedUnix {
    listener: UnixListener,
    path: UnixPath,
    identity: (u64, u64),
    // The persistent lock inode is never removed, avoiding split lock ownership.
    _lock: File,
}

impl SharedUnix {
    pub(crate) fn bind(path: UnixPath) -> io::Result<Arc<Self>> {
        static LISTENERS: OnceLock<Mutex<BTreeMap<UnixPath, Weak<SharedUnix>>>> = OnceLock::new();
        let mut listeners = LISTENERS.get_or_init(Default::default).lock().unwrap();
        listeners.retain(|_, value| value.strong_count() != 0);
        if let Some(listener) = listeners.get(&path).and_then(Weak::upgrade) {
            return Ok(listener);
        }
        let lock = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .mode(0o660)
            .custom_flags(libc::O_NOFOLLOW | libc::O_CLOEXEC)
            .open(format!("{}.lock", path.as_str()))?;
        // SAFETY: the owned file remains live for the entire listener lifetime.
        if unsafe { libc::flock(lock.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) } < 0 {
            return Err(io::Error::last_os_error());
        }
        match std::fs::symlink_metadata(path.as_str()) {
            Ok(metadata) => {
                if !metadata.file_type().is_socket() {
                    return Err(io::Error::new(
                        io::ErrorKind::AlreadyExists,
                        "socket path is not a socket",
                    ));
                }
                // Nonblocking probe: only ECONNREFUSED proves a stale endpoint.
                // A full backlog, a live listener, and permission errors all fail closed.
                use std::os::fd::FromRawFd;
                // SAFETY: socket has no pointer arguments and returns a new descriptor.
                let raw = unsafe {
                    libc::socket(
                        libc::AF_UNIX,
                        libc::SOCK_STREAM | libc::SOCK_NONBLOCK | libc::SOCK_CLOEXEC,
                        0,
                    )
                };
                if raw < 0 {
                    return Err(io::Error::last_os_error());
                }
                // SAFETY: raw is newly allocated and has no other owner.
                let probe = unsafe { OwnedFd::from_raw_fd(raw) };
                let address = path.sockaddr();
                // SAFETY: address is initialized and live for this synchronous call.
                let result = unsafe {
                    libc::connect(
                        probe.as_raw_fd(),
                        (&address as *const libc::sockaddr_un).cast(),
                        path.sockaddr_len() as _,
                    )
                };
                if result == 0
                    || io::Error::last_os_error().raw_os_error() != Some(libc::ECONNREFUSED)
                {
                    return Err(io::Error::new(
                        io::ErrorKind::AddrInUse,
                        "Unix socket already has a listener",
                    ));
                }
                let current = std::fs::symlink_metadata(path.as_str())?;
                if (current.dev(), current.ino()) != (metadata.dev(), metadata.ino()) {
                    return Err(io::Error::new(
                        io::ErrorKind::AddrInUse,
                        "Unix socket changed during stale check",
                    ));
                }
                std::fs::remove_file(path.as_str())?;
            }
            Err(error) if error.kind() == io::ErrorKind::NotFound => {}
            Err(error) => return Err(error),
        }
        let listener = UnixListener::bind(path.as_str())?;
        let metadata = std::fs::symlink_metadata(path.as_str())?;
        let shared = Arc::new(Self {
            listener,
            path,
            identity: (metadata.dev(), metadata.ino()),
            _lock: lock,
        });
        shared.listener.set_nonblocking(true)?;
        std::fs::set_permissions(path.as_str(), std::fs::Permissions::from_mode(0o660))?;
        listeners.insert(path, Arc::downgrade(&shared));
        Ok(shared)
    }

    pub(crate) fn descriptor(&self) -> io::Result<OwnedFd> {
        self.listener.try_clone().map(Into::into)
    }
}

impl Drop for SharedUnix {
    fn drop(&mut self) {
        // Only the last worker owner shuts down the shared listening endpoint.
        // In-flight accepts retain their descriptors until cancellation completes.
        // SAFETY: listener owns a live socket descriptor.
        unsafe {
            libc::shutdown(self.listener.as_raw_fd(), libc::SHUT_RDWR);
        }
        if std::fs::symlink_metadata(self.path.as_str())
            .is_ok_and(|metadata| (metadata.dev(), metadata.ino()) == self.identity)
        {
            let _ = std::fs::remove_file(self.path.as_str());
        }
    }
}

#[cfg(test)]
#[path = "../tests/http/socket_listener.rs"]
mod tests;
