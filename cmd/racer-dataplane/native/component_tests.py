"""Emit production component modules for rustc tests during integration editing.

Application and telemetry composition are excluded; RDMA and every dependency
use their real source files. Output is compilation input, never a source rewrite.
"""
import pathlib
import re
import sys

source = pathlib.Path(sys.argv[1]).resolve() / "src"
print("#![allow(dead_code)]\n#![deny(unsafe_op_in_unsafe_fn)]")
modules = re.findall(r"(?:pub(?:\(crate\))?\s+)?mod (\w+);", (source / "lib.rs").read_text())
for module in modules:
    if module in ("app", "telemetry"):
        continue
    text = (source / f"{module}.rs").read_text()
    text = re.sub(
        r"(?m)^(\s*(?:pub(?:\(crate\))?\s+)?mod (\w+);)",
        lambda match: f'#[path = "{source / module / (match[2] + ".rs")}"]\n{match[1]}',
        text,
    )
    print(f"pub mod {module} {{\n{text}\n}}")
