#!/usr/bin/env bash
# Compile owned telemetry tests against real runtime dependencies, without app.
set -euo pipefail
root="$(git rev-parse --show-toplevel)"
package="$root/cmd/racer-dataplane"
deps="$package/target/debug/deps"
args=()
for name in httparse zeroize libc io_uring futures; do
    matches=("$deps/lib${name}-"*.rlib)
    if [[ ! -f "${matches[0]}" ]]; then
        printf 'Missing compiled dependency %s; run cargo test first.\n' "$name" >&2
        exit 1
    fi
    args+=(--extern "$name=${matches[0]}")
done
rustc --edition 2024 --test "$package/src/telemetry/isolated_tests.rs" \
    -L "dependency=$deps" "${args[@]}" -o "$package/target/telemetry-component-tests"
"$package/target/telemetry-component-tests" telemetry:: --test-threads=1 "$@"
