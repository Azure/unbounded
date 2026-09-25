#!/usr/bin/env python3
"""Run real store/runtime/memory tests independently of unfinished application wiring."""
from pathlib import Path
import os
import re
import subprocess
import sys

package = Path(__file__).resolve().parents[2]
source = package / "src"
target = package / "target"
target.mkdir(exist_ok=True)


def module(name, path):
    return f'#[path = "{path}"] pub mod {name};\n'


def root(name):
    text = (source / f"{name}.rs").read_text()
    text = re.sub(r"^(pub )?mod (\w+);$", lambda m: f'#[path = "{source / name / (m[2] + ".rs")}"]\n{m[0]}', text, flags=re.M)
    return f"pub mod {name} {{\n{text}\n}}\n"


text = "#![allow(dead_code)]\n"
text += module("error", source / "error.rs") + root("model")
text += "pub mod runtime {\n" + "".join(module(n, source / "runtime" / f"{n}.rs") for n in ["admission", "deadline", "reactor"]) + "}\n"
text += "pub mod memory {\n" + "".join(module(n, source / "memory" / f"{n}.rs") for n in ["pool", "page", "cache"]) + "}\n"
# The compile-only storage API contract refers to this public re-export.
text += "pub mod read { pub mod fill { pub use crate::memory::page::PageResult; } }\n"
text += root("store") + module("config", source / "config.rs")
text += "pub mod test_support {" + module("cluster", source / "test_support" / "cluster.rs") + "}\n"
harness = target / "store-component.rs"
harness.write_text(text)
command = ["rustc", "--edition=2024", "--test", str(harness), "-L", f"dependency={target / 'debug/deps'}"]
for dependency in ["libc", "sha2", "futures", "io_uring", "zeroize", "rustls"]:
    candidates = list((target / "debug/deps").glob(f"lib{dependency}-*.rlib"))
    if not candidates:
        raise SystemExit(f"Build Cargo dependencies first: missing {dependency}")
    command += ["--extern", f"{dependency}={max(candidates, key=lambda p: p.stat().st_mtime)}"]
binary = target / "store-component-tests"
command += ["-o", str(binary)]
subprocess.run(command, check=True, env={**os.environ, "CARGO_MANIFEST_DIR": str(package)})
subprocess.run([str(binary), "store::", "--nocapture", *sys.argv[1:]], check=True)
