//! Real C discovery faults, without an RDMA provider or the Rust simulator.
use super::*;
use crate::{Configuration, NativeService, pair};

/// Run only with the discovery_faults.c adapter on the loader path.
#[test]
#[ignore = "requires discovery_faults.c adapter, not the production adapter"]
fn internal_close_failures_keep_admission_and_adapter() {
    let api = Api::load().expect("load ABI 3 C fault adapter");
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
