#!/usr/bin/env bash
# Compile unchanged production modules and their tests without app composition.
set -euo pipefail
root="$(pwd)"
if [[ ! -f "$root/Cargo.toml" || ! -d "$root/target/debug/deps" ]]; then
  printf '%s\n' 'Run from cmd/racer-dataplane after cargo check has built dependencies.' >&2
  exit 1
fi
args=(--edition 2024 --test --crate-name security_component -L "dependency=$root/target/debug/deps")
for dep in base64 chacha20poly1305 ed25519_dalek futures getrandom httparse io_uring libc rcgen rustls rustls_pemfile serde serde_json sha2 x509_parser zeroize; do
  candidates=("$root"/target/debug/deps/lib"$dep"-*.rlib)
  if [[ ! -f "${candidates[0]}" ]]; then
    printf 'Missing built dependency: %s\n' "$dep" >&2
    exit 1
  fi
  args+=(--extern "$dep=${candidates[0]}")
done
for native in "$root"/target/debug/build/ring-*/out; do
  [[ ! -d "$native" ]] || args+=(-L "native=$native")
done
export CARGO_MANIFEST_DIR="$root"
output="$root/target/security-component-tests-$$"
trap 'rm -f "$output"' EXIT
# In-memory module expansion resolves nested production modules without altering
# source files or substituting component implementations.
python3 "$root/src/security/component_tests.py" "$root" |
  rustc "${args[@]}" - -o "$output"
"$output" 'security::' "$@"
