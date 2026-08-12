//! Compile the shared config schema into the wire types `src/config.rs` includes.
//!
//! The schema lives in `api/racer/config.proto` and is shared verbatim with the Go
//! control plane, so neither side hand-writes it and neither can drift from it.

const SCHEMA: &str = "../../api/racer/config.proto";
const SCHEMA_DIR: &str = "../../api/racer";

fn main() {
    println!("cargo:rerun-if-changed={SCHEMA}");
    // Needs `protoc` on PATH, or `PROTOC` pointing at one.
    if let Err(e) = prost_build::compile_protos(&[SCHEMA], &[SCHEMA_DIR]) {
        panic!("compiling {SCHEMA}: {e}\ninstall protoc (apt install protobuf-compiler)");
    }
}
