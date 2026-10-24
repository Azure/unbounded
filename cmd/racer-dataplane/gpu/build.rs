//! Compile the host emulation of the GPU decrypt kernels (`native/gcm_host.c`,
//! which includes `native/gcm_core.h`) into a static library. Only the
//! simulation backend and tests reference it, so release builds without them
//! link none of it. `libracer_gpu_cuda.so.1` is built separately by
//! `images/racer-dataplane/build-native-cuda.sh`.
fn main() {
    println!("cargo:rerun-if-changed=native/gcm_core.h");
    println!("cargo:rerun-if-changed=native/gcm_host.c");
    cc::Build::new()
        .file("native/gcm_host.c")
        .std("c99")
        .opt_level(3)
        .warnings(true)
        .extra_warnings(true)
        .warnings_into_errors(true)
        .compile("racer_gpu_gcm_host");
}
