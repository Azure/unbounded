/* Host entry point for kernel_select.h so the Rust tests can check which
 * kernel image the shim would load for a device. Linked into the Rust crate
 * by build.rs; never part of libracer_gpu_cuda.so.1. */
#include "kernel_select.h"

int racer_kernel_select_host(int major, int minor, int have_sm90, int have_sm100) {
    return racer_kernel_select(major, minor, have_sm90, have_sm100);
}
