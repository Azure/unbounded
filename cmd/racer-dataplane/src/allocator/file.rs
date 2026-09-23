// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Shared locked inode and rate-accounted setup I/O.
use super::*;
impl SlabFile {
    pub(super) fn available_bytes(&self) -> io::Result<u64> {
        match self {
            Self::Os(file, _) => {
                let mut stat = std::mem::MaybeUninit::<libc::statvfs>::uninit();
                if unsafe { libc::fstatvfs(file.as_raw_fd(), stat.as_mut_ptr()) } != 0 {
                    return Err(io::Error::last_os_error());
                }
                let stat = unsafe { stat.assume_init() };
                Ok(stat.f_bavail.saturating_mul(stat.f_frsize))
            }
        }
    }
    pub(crate) fn io(&self) -> crate::slab_io::Io {
        match self {
            Self::Os(_, io) => io.clone(),
        }
    }
    pub(super) fn read_exact_at(&self, bytes: &mut [u8], offset: u64) -> io::Result<()> {
        match self {
            Self::Os(f, io) => {
                let mut done = 0;
                while done < bytes.len() {
                    let end = (done + BUFFER_SIZE).min(bytes.len());
                    let n = io.blocking(end - done, || {
                        f.read_at(&mut bytes[done..end], offset + done as u64)
                            .map(|n| (n, n))
                    })?;
                    if n == 0 {
                        return Err(io::ErrorKind::UnexpectedEof.into());
                    }
                    done += n;
                }
                Ok(())
            }
        }
    }
    pub(super) fn write_all_at(&self, bytes: &[u8], offset: u64) -> io::Result<()> {
        match self {
            Self::Os(f, io) => {
                let mut done = 0;
                while done < bytes.len() {
                    let end = (done + BUFFER_SIZE).min(bytes.len());
                    let n = io.blocking(end - done, || {
                        f.write_at(&bytes[done..end], offset + done as u64)
                            .map(|n| (n, n))
                    })?;
                    if n == 0 {
                        return Err(io::ErrorKind::WriteZero.into());
                    }
                    done += n;
                }
                Ok(())
            }
        }
    }
    pub(super) fn sync_data(&self) -> io::Result<()> {
        match self {
            Self::Os(f, io) => io.blocking(0, || f.sync_data().map(|()| ((), 0))),
        }
    }
    pub(super) fn descriptor(&self) -> io::Result<uring::File> {
        match self {
            Self::Os(f, io) => Ok(uring::File::new(f.try_clone()?.into()).with_slab_io(io.clone())),
        }
    }
}
