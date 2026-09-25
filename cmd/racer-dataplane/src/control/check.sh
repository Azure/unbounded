#!/usr/bin/env bash
# Link focused public-API tests to the real library without compiling unrelated
# unit tests that may be mid-edit in the shared implementation worktree.
set -euo pipefail
root="$(git rev-parse --show-toplevel)"
crate="$root/cmd/racer-dataplane"
cargo build --manifest-path "$crate/Cargo.toml" --lib
deps="$crate/target/debug/deps"
args=(--extern "racer_dataplane=$crate/target/debug/libracer_dataplane.rlib")
for name in rcgen rustls serde_json libc futures sha2; do
    matches=("$deps/lib$name"-*.rlib)
    if [[ ${#matches[@]} != 1 || ! -f "${matches[0]}" ]]; then
        printf 'Expected one compiled dependency for %s\n' "$name" >&2
        exit 1
    fi
    args+=(--extern "$name=${matches[0]}")
done
CARGO_MANIFEST_DIR="$crate" rustc --edition 2024 --test \
    "$crate/src/control/check.rs" -L "dependency=$deps" "${args[@]}" \
    -o "$crate/target/control-public-tests"
"$crate/target/control-public-tests" --test-threads=1
