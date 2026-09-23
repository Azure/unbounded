// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Persisted placement identity, separate from cache-page recovery.
use super::*;
pub(super) const ATTRIBUTE: &std::ffi::CStr = c"user.racer.layout";
// Version 1: ascending round-robin shard assignment; local replica index is the
// first little-endian u64 of the key modulo the receiving worker's shard count.
const VERSION: &[u8; 8] = b"RACERL01";

#[derive(Clone, Copy)]
pub(super) struct Layout {
    pub(super) size: u64,
    pub(super) shards: u64,
    pub(super) workers: u64,
}

fn incompatible(message: impl std::fmt::Display) -> io::Error {
    io::Error::new(
        io::ErrorKind::InvalidData,
        format!(
            "slab placement layout: {message}; keep RACER_SHARDS and the actual total I/O worker count fixed (RACER_IO_WORKERS is per NUMA node; CPU affinity/topology and RACER_COMPUTE_WORKERS also affect automatic counts). No automatic migration/reformat: stop the daemon and preserve the old slab, then use a new RACER_SLAB_PATH to refill from origin"
        ),
    )
}

impl Layout {
    pub(super) fn new(size: u64, shards: usize, workers: usize) -> io::Result<Self> {
        if workers == 0 || workers > shards {
            return Err(incompatible("I/O worker count must be in 1..=shard count"));
        }
        Ok(Self {
            size,
            shards: shards as u64,
            workers: workers as u64,
        })
    }

    pub(super) fn write(self, file: &File) -> io::Result<()> {
        let mut bytes = [0u8; 64];
        bytes[..8].copy_from_slice(VERSION);
        bytes[8..16].copy_from_slice(&self.size.to_le_bytes());
        bytes[16..24].copy_from_slice(&self.shards.to_le_bytes());
        bytes[24..32].copy_from_slice(&self.workers.to_le_bytes());
        let digest = blake3::hash(&bytes[..32]);
        bytes[32..].copy_from_slice(digest.as_bytes());
        // SAFETY: live fd, terminated name and readable bounded value. CREATE
        // prevents even an accidental rewrite of a previously recorded layout.
        if unsafe {
            libc::fsetxattr(
                file.as_raw_fd(),
                ATTRIBUTE.as_ptr(),
                bytes.as_ptr().cast(),
                bytes.len(),
                libc::XATTR_CREATE,
            )
        } != 0
        {
            let error = io::Error::last_os_error();
            return Err(io::Error::new(
                error.kind(),
                format!("persist slab placement xattr (user xattr support required): {error}"),
            ));
        }
        Ok(())
    }

    pub(super) fn validate(self, file: &File) -> io::Result<()> {
        let stored = Self::read(file)?;
        let (size, shards, workers) = (stored.size, stored.shards, stored.workers);
        if (size, shards, workers) != (self.size, self.shards, self.workers) {
            return Err(incompatible(format!(
                "persisted layout requires {workers} total I/O workers, {shards} shards, {size} slab bytes; startup selected {} total I/O workers, {} shards, {} slab bytes",
                self.workers, self.shards, self.size,
            )));
        }
        Ok(())
    }

    pub(super) fn read(file: &File) -> io::Result<Self> {
        let mut bytes = [0u8; 64];
        // SAFETY: live locked fd, terminated name and writable bounded value.
        let len = unsafe {
            libc::fgetxattr(
                file.as_raw_fd(),
                ATTRIBUTE.as_ptr(),
                bytes.as_mut_ptr().cast(),
                bytes.len(),
            )
        };
        if len < 0 {
            let error = io::Error::last_os_error();
            return Err(match error.raw_os_error() {
                Some(libc::ENODATA) => incompatible(
                    "missing user.racer.layout metadata (legacy slab or lost xattr); historical placement cannot be inferred",
                ),
                Some(libc::ERANGE) => incompatible("oversized user.racer.layout metadata"),
                _ => error,
            });
        }
        if len != bytes.len() as isize
            || &bytes[..8] != VERSION
            || blake3::hash(&bytes[..32]).as_bytes() != &bytes[32..]
        {
            return Err(incompatible(
                "invalid or unsupported user.racer.layout metadata",
            ));
        }
        let number = |start| u64::from_le_bytes(bytes[start..start + 8].try_into().unwrap());
        let (size, shards, workers) = (number(8), number(16), number(24));
        if workers == 0 || workers > shards || shards > usize::MAX as u64 {
            return Err(incompatible("invalid recorded shard/worker count"));
        }
        Ok(Self {
            size,
            shards,
            workers,
        })
    }
}
