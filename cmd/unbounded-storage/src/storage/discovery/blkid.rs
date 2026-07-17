// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Minimal libblkid safe-probe wrapper.

use std::ffi::{CString, c_char, c_int};
use std::os::fd::RawFd;
use std::path::Path;

enum BlkidProbe {}

#[link(name = "blkid")]
unsafe extern "C" {
    fn blkid_new_probe_from_filename(filename: *const c_char) -> *mut BlkidProbe;
    fn blkid_new_probe() -> *mut BlkidProbe;
    fn blkid_probe_set_device(
        probe: *mut BlkidProbe,
        fd: c_int,
        offset: i64,
        size: i64,
    ) -> c_int;
    fn blkid_do_safeprobe(probe: *mut BlkidProbe) -> c_int;
    fn blkid_free_probe(probe: *mut BlkidProbe);
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ProbeResult {
    Empty,
    Signature,
    Ambiguous,
}

pub fn probe_path(path: &Path) -> Result<ProbeResult, String> {
    let path = CString::new(path.as_os_str().as_encoded_bytes())
        .map_err(|_| "device path contains a NUL byte".to_string())?;
    // SAFETY: path is a valid, NUL-terminated C string.
    let probe = unsafe { blkid_new_probe_from_filename(path.as_ptr()) };
    probe_result(probe)
}

pub fn probe_fd(fd: RawFd) -> Result<ProbeResult, String> {
    // SAFETY: libblkid returns either null or an owned probe handle.
    let probe = unsafe { blkid_new_probe() };
    if probe.is_null() {
        return Err("libblkid could not allocate a probe".to_string());
    }
    // SAFETY: probe is valid and fd remains owned by the caller for this call.
    let rc = unsafe { blkid_probe_set_device(probe, fd, 0, 0) };
    if rc != 0 {
        // SAFETY: probe is owned by this function.
        unsafe { blkid_free_probe(probe) };
        return Err(format!("libblkid could not attach fd: rc={rc}"));
    }
    probe_result(probe)
}

fn probe_result(probe: *mut BlkidProbe) -> Result<ProbeResult, String> {
    if probe.is_null() {
        return Err("libblkid could not open the device".to_string());
    }
    // SAFETY: probe is a valid libblkid handle and is freed below.
    let rc = unsafe { blkid_do_safeprobe(probe) };
    // SAFETY: probe is owned by this function and no longer used afterward.
    unsafe { blkid_free_probe(probe) };
    match rc {
        0 => Ok(ProbeResult::Signature),
        1 => Ok(ProbeResult::Empty),
        -2 => Ok(ProbeResult::Ambiguous),
        _ => Err(format!("libblkid safe probe failed: rc={rc}")),
    }
}
