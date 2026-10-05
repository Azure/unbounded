//! Delayed syscall consumers without invoking a kernel or disabling Miri checks.
use super::*;
use std::ffi::CStr;

#[test]
fn delayed_pathname_consumer_survives_owner_moves() {
    let path = PathArg::new(CString::new("stage-to-publish").unwrap());
    let pointer = path.as_ptr();
    // Match an SQE deriving its pointer before the owner moves into an entry and
    // later into its completion closure. A Box/CString regression must fail Miri.
    let entry = (path, pointer);
    let completion: Box<dyn FnOnce()> = Box::new(move || {
        let (path, pointer) = entry;
        assert_eq!(path.as_ptr(), pointer);
        // SAFETY: the moved owner remains live and the original NUL is retained.
        assert_eq!(
            unsafe { CStr::from_ptr(pointer) }.to_bytes(),
            b"stage-to-publish"
        );
        drop(path);
    });
    completion();
}

#[test]
fn delayed_syscall_read_and_write_survive_owner_moves() {
    let input = SyscallArg::new([11u64, 22]);
    let read = input.as_ptr();
    let mut output = SyscallArg::new([0u64; 2]);
    let write = output.as_mut_ptr();
    let entry = (input, output);
    let completion: Box<dyn FnOnce()> = Box::new(move || {
        let (input, output) = entry;
        // SAFETY: both separately allocated owners remain live, and no reference
        // to their contents exists across this delayed consumer's access.
        unsafe { write.write(read.read()) };
        assert_eq!(output.into_inner(), [11, 22]);
        assert_eq!(input.into_inner(), [11, 22]);
    });
    completion();
}
