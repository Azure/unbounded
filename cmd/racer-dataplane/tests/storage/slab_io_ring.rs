// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use crate::slab_io::Io;

fn metrics(io: &Io, name: &str) -> u64 {
    let mut text = String::new();
    io.render(&mut text);
    text.lines()
        .find_map(|line| {
            line.strip_prefix(&format!("racer_dataplane_slab_io_{name} "))?
                .parse()
                .ok()
        })
        .unwrap()
}

pub(super) fn kernel_wait_and_cancel(ring: &mut Ring) {
    let name = std::ffi::CString::new("racer-slab-flow").unwrap();
    let raw = unsafe { libc::memfd_create(name.as_ptr(), libc::MFD_CLOEXEC) };
    assert!(raw >= 0);
    let io = Io::testing(10, 1, false);
    let file = File::new(unsafe { OwnedFd::from_raw_fd(raw) }).with_slab_io(io.clone());
    io.reserve(0, crate::environment::now()).unwrap().finish(0);
    let mut sync = ring.sync_data(file.clone().into()).unwrap();
    ring.progress().unwrap();
    assert!(ring.take_control(&mut sync).unwrap().is_none());
    let start = Instant::now();
    drive(ring, |r| {
        r.take_control(&mut sync).unwrap().is_some_and(|c| {
            c.result.unwrap();
            true
        })
    });
    assert!(start.elapsed() >= Duration::from_millis(50));
    let mut pending = ring.sync_data(file.into()).unwrap();
    let mut ack = ring.cancel(&pending).unwrap();
    assert_eq!(
        ring.take_control(&mut pending)
            .unwrap()
            .unwrap()
            .result
            .unwrap_err()
            .raw_os_error(),
        Some(libc::ECANCELED)
    );
    drive(ring, |r| r.take_cancel(&mut ack).unwrap().is_some());
    assert_eq!(metrics(&io, "operations_total"), 2);
}
