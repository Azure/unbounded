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
fn shared_bucket_refills_without_fractional_loss_or_idle_credit() {
    let world = crate::simulation::World::new(911);
    let _scope = world.enter();
    let io = Io::new(config(Some("3"), None, Some("1")).unwrap());
    let peer = io.clone();
    let now = crate::environment::now();
    io.reserve(4096, now).ok().unwrap().finish(4096);
    assert_eq!(
        peer.reserve(0, now).err().unwrap(),
        now + Duration::from_nanos(333333334)
    );
    for _ in 0..3 {
        world.advance(Duration::from_millis(100));
        assert!(peer.reserve(0, crate::environment::now()).is_err());
    }
    world.advance(Duration::from_millis(34));
    peer.reserve(0, crate::environment::now())
        .ok()
        .unwrap()
        .finish(0);
    world.advance(Duration::from_secs(100));
    io.reserve(0, crate::environment::now())
        .ok()
        .unwrap()
        .finish(0);
    assert!(io.reserve(0, crate::environment::now()).is_err());
}

#[test]
fn byte_refunds_and_nondata_operations() {
    let world = crate::simulation::World::new(912);
    let _scope = world.enter();
    let io = Io::new(config(None, Some("1"), None).unwrap());
    let now = crate::environment::now();
    let charge = io.reserve(4194304, now).ok().unwrap();
    io.reserve(0, now).ok().unwrap().finish(0); // sync/punch bypass bytes
    assert!(io.reserve(1, now).is_err());
    charge.finish(100);
    io.reserve(4194204, now).ok().unwrap().finish(0); // error refunds all bytes
    assert!(io.reserve(4194204, now).is_ok());
    let mut metrics = String::new();
    io.render(&mut metrics);
    assert!(metrics.contains("racer_dataplane_slab_io_bytes_total 100\n"));
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
