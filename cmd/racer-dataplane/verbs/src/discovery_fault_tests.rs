//! Real C discovery and open faults, without an RDMA provider or Rust simulator.
use super::*;
use crate::{Configuration, NativeService, pair};

/// The C fixture's fault state is process-global.
static FAULTS: std::sync::Mutex<()> = std::sync::Mutex::new(());

/// Run only with the discovery_faults.c adapter on the loader path.
#[test]
#[ignore = "requires discovery_faults.c adapter, not the production adapter"]
fn internal_close_failures_keep_admission_and_adapter() {
    let _lock = FAULTS.lock().unwrap();
    let api = Api::load().expect("load ABI 4 C fault adapter");
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
            let (io, port) = pair(1).unwrap();
            let mut native = NativeService::new(port);
            let (guard, charged) = crate::test_guard::guard();
            futures::executor::block_on(io.configure(Configuration {
                discover: true,
                guards: vec![guard],
                bytes: 32,
                selector: Box::new(|ports| {
                    assert!(ports.is_empty());
                    Ok(Vec::new())
                }),
            }))
            .unwrap();
            native.poll_budgeted(2).unwrap();
            let expected = if leaks || matches!(mode, 4 | 7) {
                Err(Error::Unavailable)
            } else {
                Ok(0)
            };
            assert_eq!(io.activation().unwrap().map(|ports| ports.len()), expected);
            io.close();
            native.poll_budgeted(2).unwrap();
            assert_eq!(
                io.reopen(),
                if leaks {
                    Err(Error::Overloaded)
                } else {
                    Ok(())
                }
            );
            drop(native);
            drop(io);
            assert_eq!(charged.get(), usize::from(leaks));
        }
    }
}

/// Check the real C open outcomes and retain only failed cleanup owners.
#[test]
#[ignore = "requires discovery_faults.c adapter, not the production adapter"]
fn open_cleanup_failures_keep_admission_and_adapter() {
    let _lock = FAULTS.lock().unwrap();
    let api = Api::load().expect("load ABI 4 C fault adapter");
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
        let (io, port) = pair(1).unwrap();
        let mut native = NativeService::new(port);
        let (guard, charged) = crate::test_guard::guard();
        futures::executor::block_on(io.configure(Configuration {
            discover: true,
            guards: vec![guard],
            bytes: 32,
            selector: Box::new(move |descriptions| {
                assert!(!leaks, "leaked open must not reach selection");
                assert_eq!(descriptions.len(), ports);
                Ok(Vec::new())
            }),
        }))
        .unwrap();
        native.poll_budgeted(2).unwrap();
        assert_eq!(
            io.activation().unwrap().map(|ports| ports.len()),
            if leaks {
                Err(Error::Unavailable)
            } else {
                Ok(0)
            }
        );
        assert_eq!(unsafe { count(3) }, allocations);
        io.close();
        native.poll_budgeted(2).unwrap();
        assert_eq!(
            io.reopen(),
            if leaks {
                Err(Error::Overloaded)
            } else {
                Ok(())
            }
        );
        drop(native);
        drop(io);
        assert_eq!(charged.get(), usize::from(leaks));
    }
}
