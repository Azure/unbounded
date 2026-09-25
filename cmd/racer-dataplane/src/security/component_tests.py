"""Emit a test crate using production modules, excluding only app composition.

Root modules are inlined with explicit child paths because Rust's #[path] root
module lookup differs from normal `mod name;` lookup. No production logic changes.
Output goes directly to rustc stdin, not to the shared source tree.
"""
import pathlib
import re
import sys

source = pathlib.Path(sys.argv[1]).resolve() / "src"
print("#![allow(dead_code)]\n#![deny(unsafe_op_in_unsafe_fn)]")
modules = re.findall(r"(?:pub(?:\(crate\))?\s+)?mod (\w+);", (source / "lib.rs").read_text())
for module in modules:
    if module == "app":
        continue
    text = (source / f"{module}.rs").read_text()
    text = re.sub(
        r"(?m)^(\s*(?:pub(?:\(crate\))?\s+)?mod (\w+);)",
        lambda m: f'#[path = "{source / module / (m[2] + ".rs")}"]\n{m[1]}',
        text,
    )
    print(f"pub mod {module} {{\n{text}\n}}")
