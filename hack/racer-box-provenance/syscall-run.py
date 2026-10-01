"""Read a prebuilt Miri installation; write evidence only in this worktree."""
import os
from pathlib import Path
import subprocess
import sys

root = Path(__file__).resolve().parents[2]
base = root / "tmp" / "syscall-provenance"
source = Path(sys.argv[1]).resolve()
binary = source / "rustup/toolchains/nightly-2025-11-21-x86_64-unknown-linux-gnu/bin/miri"
env = dict(os.environ)
env.update(RUSTUP_HOME=str(base / "rustup"), CARGO_HOME=str(base / "cargo"),
           XDG_CACHE_HOME=str(base / "cache"), TMPDIR=str(base / "scratch"),
           MIRI_SYSROOT=str(source / "sysroot"))
for key in ("MIRIFLAGS", "RUSTFLAGS", "RUSTC_WRAPPER", "RUSTC_WORKSPACE_WRAPPER"):
    env.pop(key, None)
results = []
for model, flags in (("stacked", []), ("tree", ["-Zmiri-tree-borrows"])):
    for case in ("box-output", "box-input", "fixed"):
        command = ["timeout", "--signal=TERM", "--kill-after=10s", "10s", str(binary),
                   "--sysroot", str(source / "sysroot"), "--edition=2024", *flags,
                   str(Path(__file__).with_name("syscall.rs")), "--", case]
        print(command, flush=True)
        result = subprocess.run(command, cwd=root, env=env, text=True,
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        (base / f"{model}-{case}.log").write_text(result.stdout)
        print(result.stdout, flush=True)
        results.append(f"{model} {case} exit={result.returncode}")
        expected = int(case == "box-output" or (model == "stacked" and case == "box-input"))
        if result.returncode != expected or (expected and "Undefined Behavior" not in result.stdout):
            raise SystemExit(f"Unexpected result: {results[-1]}")
(base / "results.txt").write_text("\n".join(results) + "\n")
print("\n".join(results))
