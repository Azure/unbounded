"""Compile unchanged read dependency modules without application composition tests.

This is a focused test harness, not a replacement for cargo integration checks.
Only app and telemetry roots are excluded; all read dependencies are production
modules and their original tests. No successful production behavior is fabricated.
"""
import os
import pathlib
import re
import subprocess

root = pathlib.Path(__file__).resolve().parents[2]
source = root / "src"
modules = re.findall(r"(?:pub(?:\(crate\))?\s+)?mod (\w+);", (source / "lib.rs").read_text())
parts = ["#![allow(dead_code)]\n#![deny(unsafe_op_in_unsafe_fn)]"]
for module in modules:
    if module in ("app", "telemetry"):
        continue
    text = (source / f"{module}.rs").read_text()
    text = re.sub(
        r"(?m)^(\s*(?:pub(?:\(crate\))?\s+)?mod (\w+);)",
        lambda match: f'#[path = "{source / module / (match[2] + ".rs")}"]\n{match[1]}',
        text,
    )
    parts.append(f"pub mod {module} {{\n{text}\n}}")
target = root / "target"
if not (target / "debug/deps").is_dir():
    raise SystemExit("Run cargo check first to build dependencies")
args = ["rustc", "--edition", "2024", "--test", "--crate-name", "read_component", "-L", f"dependency={target / 'debug/deps'}"]
dependencies = "base64 chacha20poly1305 ed25519_dalek futures getrandom httparse io_uring libc rcgen rustls rustls_pemfile serde serde_json sha2 x509_parser zeroize".split()
for dependency in dependencies:
    candidates = sorted((target / "debug/deps").glob(f"lib{dependency}-*.rlib"))
    if not candidates:
        raise SystemExit(f"Missing built dependency: {dependency}")
    args.extend(["--extern", f"{dependency}={candidates[0]}"])
for native in (target / "debug/build").glob("ring-*/out"):
    args.extend(["-L", f"native={native}"])
binary = target / "read-component-tests"
args.extend(["-", "-o", str(binary)])
environment = dict(os.environ, CARGO_MANIFEST_DIR=str(root))
subprocess.run(args, input="\n".join(parts), text=True, env=environment, check=True)
subprocess.run([str(binary), "read::", "--test-threads=1"], env=environment, check=True)
