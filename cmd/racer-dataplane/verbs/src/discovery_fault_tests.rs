//! Real C discovery and open faults, without an RDMA provider or Rust simulator.
use super::*;
use crate::{NodeConfig, Owner, node};

/// The C fixture's fault state is process-global.
static FAULTS: std::sync::Mutex<()> = std::sync::Mutex::new(());

/// Run only with the discovery_faults.c adapter on the loader path.
#[test]
#[ignore = "requires discovery_faults.c adapter, not the production adapter"]
fn internal_close_failures_keep_admission_and_adapter() {
    let _lock = FAULTS.lock().unwrap();
    let api = Api::load().expect("load C fault adapter");
    let library = api.library.unwrap();
    let reset = unsafe { libc::dlsym(library.as_ptr(), c"rdma_test_reset".as_ptr()) };
    let count = unsafe { libc::dlsym(library.as_ptr(), c"rdma_test_count".as_ptr()) };
    assert!(
        !reset.is_null() && !count.is_null(),
        "must load C fault adapter"
    );
    let reset: unsafe extern "C" fn(u32, u32) = unsafe { std::mem::transmute(reset) };
    let count: unsafe extern "C" fn(u32) -> u32 = unsafe { std::mem::transmute(count) };

    for mode in 0..8 {
        for fail_close in [0, 1, 2] {
            unsafe { reset(mode, fail_close) };
            let leaks = matches!(mode, 0..=3 | 7) && fail_close != 0;
            let mut ports = [Port {
                name: [0; 64],
                gid: [0; 16],
                mtu: 0,
                lid: 0,
                port: 0,
                link_layer: 0,
            }; 64];
            let result = unsafe { (api.discover)(ports.as_mut_ptr(), 64) };
            let expected = if leaks {
                CLEANUP_LEAK
            } else {
                match mode {
                    2 => 3,
                    4 => -libc::EIO,
                    7 => 195,
                    _ => 0,
                }
            };
            assert_eq!(result, expected, "mode={mode}, close={fail_close}");
            let expected_opens = if leaks {
                fail_close
            } else if matches!(mode, 4 | 6) {
                0
            } else {
                3
            };
            assert_eq!(unsafe { count(0) }, expected_opens);
            assert_eq!(
                unsafe { count(1) },
                if mode == 5 { 0 } else { expected_opens }
            );
            assert_eq!(unsafe { count(2) }, u32::from(mode != 4));

            unsafe { reset(mode, fail_close) };
            let (guard, charged) = crate::test_guard::guard();
            let owner = GuardOwner::new(guard);
            let references = Rc::strong_count(&api);
            let result = discover_with_api(api.clone(), || Some(owner.clone()));
            assert_eq!(owner.quarantined(), leaks);
            assert_eq!(Rc::strong_count(&api), references + usize::from(leaks));
            if leaks || matches!(mode, 4 | 7) {
                assert!(matches!(result, Err(Error::Unavailable)));
            } else {
                assert!(result.unwrap().is_empty());
            }
            drop(owner);
            assert_eq!(charged.get(), usize::from(leaks));

            unsafe { reset(mode, fail_close) };
            let (guard, charged) = crate::test_guard::guard();
            let expected = if leaks || matches!(mode, 4 | 7) {
                Err(Error::Unavailable)
            } else {
                Ok(0)
            };
            let (native, io) = activate(guard, |ports| {
                assert!(ports.is_empty());
                Ok(Vec::new())
            });
            assert_eq!(native.map(|ports| ports.len()), expected);
            let (owner, client) = io;
            owner_close(owner, &client, leaks);
            drop(client);
            assert_eq!(charged.get(), usize::from(leaks));
        }
    }
}

/// A bind is signaled and fenced behind earlier work on its QP. Without the
/// fence, mlx5 can fail a bind queued behind a zero-copy write whose implicit
/// ODP source page faults (`IBV_WC_MW_BIND_ERR`), breaking every link that
/// both writes and grants. An invalidate is signaled only.
#[test]
#[ignore = "requires discovery_faults.c adapter, not the production adapter"]
fn bind_is_signaled_and_fenced() {
    let _lock = FAULTS.lock().unwrap();
    let api = Api::load().expect("load C fault adapter");
    let library = api.library.unwrap();
    let flags = unsafe { libc::dlsym(library.as_ptr(), c"rdma_test_post_flags".as_ptr()) };
    assert!(!flags.is_null(), "must load C faults");
    let flags: unsafe extern "C" fn(u32) -> u32 = unsafe { std::mem::transmute(flags) };
    // IBV_SEND_FENCE | IBV_SEND_SIGNALED.
    assert_eq!(unsafe { flags(0) }, 1 | 2);
    // IBV_SEND_SIGNALED.
    assert_eq!(unsafe { flags(1) }, 2);
}

/// Check the real C open outcomes and retain only failed cleanup owners.
#[test]
#[ignore = "requires discovery_faults.c adapter, not the production adapter"]
fn open_cleanup_failures_keep_admission_and_adapter() {
    let _lock = FAULTS.lock().unwrap();
    let api = Api::load().expect("load C fault adapter");
    let library = api.library.unwrap();
    let reset = unsafe { libc::dlsym(library.as_ptr(), c"rdma_test_reset".as_ptr()) };
    let count = unsafe { libc::dlsym(library.as_ptr(), c"rdma_test_count".as_ptr()) };
    assert!(!reset.is_null() && !count.is_null(), "must load C faults");
    let reset: unsafe extern "C" fn(u32, u32) = unsafe { std::mem::transmute(reset) };
    let count: unsafe extern "C" fn(u32) -> u32 = unsafe { std::mem::transmute(count) };

    // List error, empty list, unknown name, context error, wrapper error,
    // clean PD failure, leaked PD failure, and successful open/close.
    for (mode, name, fail_close, opens, allocations, closes) in [
        (4, c"discovery-test", 0, 0, 0, 0),
        (6, c"discovery-test", 0, 0, 0, 0),
        (2, c"missing", 0, 0, 0, 0),
        (5, c"discovery-test", 1, 1, 0, 0),
        (10, c"discovery-test", 1, 0, 0, 0),
        (2, c"discovery-test", 0, 1, 1, 1),
        (2, c"discovery-test", 1, 1, 1, 1),
        (8, c"discovery-test", 0, 1, 1, 0),
    ] {
        unsafe { reset(mode, fail_close) };
        let raw = unsafe { (api.open)(name.as_ptr()) };
        if mode == 8 {
            assert!(!raw.is_null() && raw != OPEN_CLEANUP_LEAK);
        } else if mode == 2 && fail_close == 1 {
            assert_eq!(raw, OPEN_CLEANUP_LEAK);
        } else {
            assert!(raw.is_null());
        }
        assert_eq!(unsafe { count(0) }, opens);
        assert_eq!(unsafe { count(1) }, closes);
        assert_eq!(unsafe { count(2) }, u32::from(mode != 4));
        assert_eq!(unsafe { count(3) }, allocations);
        assert_eq!(unsafe { count(4) }, 0);
        assert_eq!(unsafe { count(5) }, u32::from(mode != 4));
        if mode == 8 {
            assert_eq!(unsafe { (api.close)(raw) }, 0);
            assert_eq!(unsafe { count(1) }, 1);
            assert_eq!(unsafe { count(4) }, 1);
        }
    }

    // Discovery closes three contexts first. Also fail after one prior clean
    // failure or successful open, to check early stop and partial-vector drop.
    for (mode, fail_close, leaks, ports, allocations, deallocations) in [
        (2, 0, false, 0, 3, 0),
        (2, 4, true, 0, 1, 0),
        (2, 5, true, 0, 2, 0),
        (9, 0, false, 2, 3, 0),
        (9, 4, true, 0, 2, 1),
        (8, 0, false, 3, 3, 0),
        (10, 0, false, 0, 0, 0),
    ] {
        unsafe { reset(mode, fail_close) };
        let mut owners = Vec::new();
        let references = Rc::strong_count(&api);
        let result = discover_with_api(api.clone(), || {
            let (owner, charged) = lifetime_tests::quota();
            owners.push((owner.clone(), charged));
            Some(owner)
        });
        assert_eq!(unsafe { count(3) }, allocations);
        assert_eq!(unsafe { count(4) }, deallocations);
        let attempts = if leaks { allocations } else { 3 };
        assert_eq!(unsafe { count(2) }, 1 + attempts);
        assert_eq!(owners.len(), 1 + attempts as usize);
        assert_eq!(
            owners
                .iter()
                .filter(|(owner, _)| owner.quarantined())
                .count(),
            usize::from(leaks)
        );
        assert!(!owners[0].0.quarantined(), "discovery itself was clean");
        assert_eq!(owners.last().unwrap().0.quarantined(), leaks);
        if leaks {
            assert!(matches!(result, Err(Error::Unavailable)));
        } else {
            assert_eq!(result.as_ref().unwrap().len(), ports);
        }
        assert_eq!(
            Rc::strong_count(&api),
            references + ports + usize::from(leaks)
        );
        drop(result);
        assert_eq!(Rc::strong_count(&api), references + usize::from(leaks));
        for (owner, charged) in owners {
            let retained = owner.quarantined();
            drop(owner);
            assert_eq!(charged.get(), usize::from(retained));
        }

        unsafe { reset(mode, fail_close) };
        let (guard, charged) = crate::test_guard::guard();
        let (activation, (owner, client)) = activate(guard, move |descriptions| {
            assert!(!leaks, "leaked open must not reach selection");
            assert_eq!(descriptions.len(), ports);
            Ok(Vec::new())
        });
        assert_eq!(
            activation.map(|ports| ports.len()),
            if leaks {
                Err(Error::Unavailable)
            } else {
                Ok(0)
            }
        );
        assert_eq!(unsafe { count(3) }, allocations);
        owner_close(owner, &client, leaks);
        drop(client);
        assert_eq!(charged.get(), usize::from(leaks));
    }
}

/// Activate a one-worker node owner with `selector` and poll it once.
fn activate(
    guard: crate::Guard,
    selector: impl FnOnce(&[crate::PortInfo]) -> Result<Vec<(u16, usize)>> + Send + 'static,
) -> (Result<Vec<crate::Selection>>, (Owner, crate::Client)) {
    let (mut clients, port) = node(1).unwrap();
    let mut owner = Owner::new(port);
    let client = clients.remove(0);
    let activation = client.activate(NodeConfig {
        selector: Box::new(selector),
        pool_bytes: 4096,
        buffer_len: 4096,
        max_links: 1,
        grants_per_link: 1,
        idle: std::time::Duration::from_secs(1),
        mr_remote_write: false,
        zero_copy: false,
        guard,
    });
    owner.poll_budgeted(2).unwrap();
    let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
    let std::task::Poll::Ready(result) = activation.poll(&mut cx) else {
        panic!("activation did not finish");
    };
    (result, (owner, client))
}

/// Close the owner; a leaked discovery charge keeps it from draining, so
/// reopen stays refused.
fn owner_close(mut owner: Owner, client: &crate::Client, leaks: bool) {
    client.close();
    owner.poll_budgeted(2).unwrap();
    assert_eq!(
        client.reopen(),
        if leaks {
            Err(Error::Overloaded)
        } else {
            Ok(())
        }
    );
    drop(owner);
}
