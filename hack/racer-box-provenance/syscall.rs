//! No FFI: typed input/output and closure handoff, using the production owner.
#[path = "../../cmd/racer-dataplane/src/runtime/reactor/syscall_arg.rs"]
mod syscall_arg;
use syscall_arg::SyscallArg;

#[repr(C, align(8))]
#[derive(Clone, Copy)]
struct Argument {
    words: [u64; 16],
}

fn submit<T: 'static>(
    finish: impl FnOnce(bool) -> Option<T> + 'static,
) -> Box<dyn FnOnce(bool) -> Option<T>> {
    Box::new(move |success| finish(success))
}

fn main() {
    let case = std::env::args().nth(1).unwrap();
    if case.starts_with("box") {
        let mut owner = Box::new(Argument { words: [0; 16] });
        let write = case == "box-output";
        let ptr = if write {
            &mut *owner as *mut Argument
        } else {
            (&*owner as *const Argument).cast_mut()
        };
        let finish = submit(move |success| if success { Some(*owner) } else { None });
        if write {
            unsafe { (*ptr).words[0] = 7 };
        } else {
            assert_eq!(unsafe { (*ptr).words[0] }, 0);
        }
        assert_eq!(finish(true).unwrap().words[0], if write { 7 } else { 0 });
    } else {
        for success in [false, true] {
            let mut owner = SyscallArg::new(Argument { words: [0; 16] });
            let ptr = owner.as_mut_ptr();
            assert_eq!(owner.as_ptr(), ptr.cast_const());
            let finish = submit(move |ok| if ok { Some(owner.into_inner()) } else { None });
            unsafe { (*ptr).words[0] = 7 };
            let result = finish(success);
            assert_eq!(result.map(|arg| arg.words[0]), success.then_some(7));
        }
        let owner = SyscallArg::new(Argument { words: [9; 16] });
        let ptr = owner.as_ptr();
        let finish = submit(move |ok| {
            drop(owner);
            ok.then_some(())
        });
        assert_eq!(unsafe { (*ptr).words[0] }, 9);
        assert!(finish(false).is_none());
    }
    println!("PASS {case}");
}
