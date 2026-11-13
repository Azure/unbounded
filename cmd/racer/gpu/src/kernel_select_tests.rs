//! Tests for the shim's choice of embedded kernel image
//! (`native/kernel_select.h`), run through the host build of the same header.

use std::ffi::c_int;

unsafe extern "C" {
    fn racer_kernel_select_host(
        major: c_int,
        minor: c_int,
        have_sm90: c_int,
        have_sm100: c_int,
    ) -> c_int;
}

const PTX: c_int = 0;
const SM90: c_int = 1;
const SM100: c_int = 2;

fn select(major: c_int, minor: c_int, sm90: bool, sm100: bool) -> c_int {
    unsafe { racer_kernel_select_host(major, minor, sm90.into(), sm100.into()) }
}

#[test]
fn matching_cubin_is_chosen() {
    assert_eq!(select(9, 0, true, true), SM90);
    assert_eq!(select(10, 0, true, true), SM100);
    // Same major, later minor runs the cubin (for example GB300 at 10.3).
    assert_eq!(select(10, 3, true, true), SM100);
}

#[test]
fn other_devices_use_ptx() {
    assert_eq!(select(8, 0, true, true), PTX);
    assert_eq!(select(8, 9, true, true), PTX);
    // sm_120 is a different major and cannot run the sm_100 cubin.
    assert_eq!(select(12, 0, true, true), PTX);
}

#[test]
fn missing_cubin_falls_back_to_ptx() {
    assert_eq!(select(9, 0, false, true), PTX);
    assert_eq!(select(10, 0, true, false), PTX);
    assert_eq!(select(10, 0, false, false), PTX);
}

#[test]
fn unknown_capability_uses_ptx() {
    assert_eq!(select(-1, -1, true, true), PTX);
    assert_eq!(select(9, -1, true, true), PTX);
}
