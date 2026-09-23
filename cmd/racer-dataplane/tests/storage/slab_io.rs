// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;

fn config(
    ops: Option<&str>,
    bytes: Option<&str>,
    burst: Option<&str>,
) -> io::Result<Option<Config>> {
    Config::parse(|name| {
        match name {
            "RACER_SLAB_IOPS" => ops,
            "RACER_SLAB_BYTES_PER_SEC" => bytes,
            "RACER_SLAB_IO_BURST" => burst,
            _ => unreachable!(),
        }
        .map(str::to_owned)
        .ok_or(env::VarError::NotPresent)
    })
}

#[test]
fn configuration_boundaries() {
    assert!(config(None, None, None).unwrap().is_none());
    for (ops, bytes, burst) in [
        (Some("1"), Some("2"), None),
        (None, None, Some("1")),
        (Some("0"), None, None),
        (None, Some("-1"), None),
        (Some(" 1"), None, None),
        (Some("+1"), None, None),
        (Some(""), None, None),
        (Some("18446744073709551616"), None, None),
        (None, Some("1"), Some("4194303")),
        (Some("1"), None, Some("0")),
    ] {
        assert!(
            config(ops, bytes, burst).is_err(),
            "{ops:?}/{bytes:?}/{burst:?}"
        );
    }
    assert_eq!(
        config(None, Some("1"), None).unwrap().unwrap().burst,
        4194304
    );
    assert_eq!(config(Some("17"), None, None).unwrap().unwrap().burst, 17);
    assert!(Config::parse(|_| Err(env::VarError::NotUnicode("bad".into()))).is_err());
}

#[test]
fn synchronous_stop_and_scope_restoration() {
    let io = Io::new(config(Some("1"), None, None).unwrap()).with_stop(|| true);
    assert_eq!(
        io.blocking::<()>(0, || panic!("must not issue I/O"))
            .unwrap_err()
            .kind(),
        io::ErrorKind::Interrupted
    );
    assert!(!Io::current().limited());
    io.scope(|| assert!(Io::current().limited()));
    assert!(!Io::current().limited());
    assert!(
        std::panic::catch_unwind(std::panic::AssertUnwindSafe(
            || io.scope(|| panic!("restore"))
        ))
        .is_err()
    );
    assert!(!Io::current().limited());
}

#[test]
fn extreme_rate_saturates_without_overflow() {
    let io = Io::new(config(Some("18446744073709551615"), None, None).unwrap());
    let now = crate::environment::now();
    io.reserve(0, now + Duration::from_secs(86400))
        .ok()
        .unwrap()
        .finish(0);
}

#[test]
fn interrupted_syscalls_retry_with_accounting_and_stop_checks() {
    let io = Io::testing(100, 100, false);
    let mut attempts = 0;
    io.blocking(4096, || {
        attempts += 1;
        if attempts == 1 {
            Err(io::ErrorKind::Interrupted.into())
        } else {
            Ok(((), 4096))
        }
    })
    .unwrap();
    assert_eq!(attempts, 2);
    let mut metrics = String::new();
    io.render(&mut metrics);
    assert!(metrics.contains("racer_dataplane_slab_io_operations_total 2\n"));
    assert!(metrics.contains("racer_dataplane_slab_io_bytes_total 4096\n"));

    use std::sync::atomic::{AtomicBool, Ordering};
    let stopped = Arc::new(AtomicBool::new(false));
    let check = stopped.clone();
    let io = io.with_stop(move || check.load(Ordering::Acquire));
    let error = io
        .blocking::<()>(0, || {
            stopped.store(true, Ordering::Release);
            Err(io::ErrorKind::Interrupted.into())
        })
        .unwrap_err();
    assert_eq!(error.to_string(), "slab I/O setup stopped");
}

#[test]
fn synchronous_token_wait_is_interruptible() {
    use std::sync::atomic::{AtomicUsize, Ordering};
    let checks = AtomicUsize::new(0);
    let io = Io::testing(1, 1, false).with_stop(move || checks.fetch_add(1, Ordering::AcqRel) >= 2);
    io.reserve(0, crate::environment::now())
        .ok()
        .unwrap()
        .finish(0);
    assert_eq!(
        io.blocking::<()>(0, || panic!("stopped before I/O"))
            .unwrap_err()
            .kind(),
        io::ErrorKind::Interrupted
    );
    let mut metrics = String::new();
    io.render(&mut metrics);
    assert!(metrics.contains("racer_dataplane_slab_io_waits_total 1\n"));
}
