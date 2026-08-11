//! Compile `proto/config.proto` into the wire types `src/config.rs` includes.
//!
//! The schema is shared verbatim with the Go control plane, so neither side hand-writes
//! it and neither can drift from it.

fn main() {
    println!("cargo:rerun-if-changed=proto/config.proto");
    // Needs `protoc` on PATH, or `PROTOC` pointing at one.
    if let Err(e) = prost_build::compile_protos(&["proto/config.proto"], &["proto"]) {
        panic!("compiling proto/config.proto: {e}\ninstall protoc (apt install protobuf-compiler)");
    }
}
