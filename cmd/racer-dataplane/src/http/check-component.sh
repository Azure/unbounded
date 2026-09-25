#!/usr/bin/env bash
# Standalone production HTTP/runtime tests while the full crate is integrating.
set -euo pipefail
root="$(git rev-parse --show-toplevel)"
package="$root/cmd/racer-dataplane"
deps="$package/target/debug/deps"
args=()
for name in httparse zeroize libc io_uring futures getrandom; do
    matches=("$deps/lib${name}-"*.rlib)
    if [[ ! -f "${matches[0]}" ]]; then
        printf 'Missing compiled dependency %s; run cargo test first.\n' "$name" >&2
        exit 1
    fi
    args+=(--extern "$name=${matches[0]}")
done
rustc --edition 2024 --test "$package/src/http/isolated_tests.rs" \
    -L "dependency=$deps" "${args[@]}" -o "$package/target/http-component-tests"
# Runtime fixtures use Cargo's package-working-directory convention.
pushd "$package" >/dev/null
"$package/target/http-component-tests" --test-threads=1 "$@"
popd >/dev/null
