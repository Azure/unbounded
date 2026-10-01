"""Run an already provisioned, isolated Miri; never install or touch home caches."""
import os
from pathlib import Path
import subprocess


ROOT = Path(__file__).resolve().parents[2]
BASE = ROOT / "tmp" / "box-provenance"
TOOLCHAIN = "nightly-2025-11-21-x86_64-unknown-linux-gnu"
BIN = BASE / "rustup" / "toolchains" / TOOLCHAIN / "bin"
ENV = dict(os.environ)
ENV.update(
    RUSTUP_HOME=str(BASE / "rustup"),
    CARGO_HOME=str(BASE / "cargo"),
    XDG_CACHE_HOME=str(BASE / "miri-cache"),
    MIRI_SYSROOT=str(BASE / "sysroot"),
    CARGO_TARGET_DIR=str(BASE / "target"),
    TMPDIR=str(BASE / "scratch"),
    RUSTUP_TOOLCHAIN=TOOLCHAIN,
    RUSTUP_AUTO_INSTALL="0",
    PATH=str(BIN) + os.pathsep + os.environ["PATH"],
)
# Do not inherit caller compiler flags or wrappers into the reduction.
for key in ("RUSTFLAGS", "RUSTDOCFLAGS", "MIRIFLAGS", "RUSTC_WRAPPER",
            "RUSTC_WORKSPACE_WRAPPER", "RUSTC", "CARGO_ENCODED_RUSTFLAGS"):
    ENV.pop(key, None)


def run(name, command, seconds):
    command = ["timeout", "--signal=TERM", "--kill-after=10s", f"{seconds}s", *command]
    print("COMMAND", command, flush=True)
    result = subprocess.run(command, cwd=ROOT, env=ENV, stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, text=True)
    (BASE / f"{name}.log").write_text(result.stdout)
    print(result.stdout, end="", flush=True)
    print(f"RESULT {name}: exit={result.returncode}", flush=True)
    return result.returncode


if not BASE.is_dir() or not (BIN / "miri").is_file():
    raise SystemExit("Provision isolated nightly-2025-11-21 with miri and rust-src first")
assert run("version", [str(BIN / "miri"), "--version"], 10) == 0
assert run("setup", [str(BIN / "cargo-miri"), "miri", "setup"], 90) == 0
results = []
for model, flags in (("stacked", []), ("tree", ["-Zmiri-tree-borrows"])):
    for case in ("box-after-write", "box-after-read", "box-before-write",
                 "vec-after-write", "vec-after-read"):
        code = run(f"{model}-{case}", [str(BIN / "miri"), "--sysroot",
                   ENV["MIRI_SYSROOT"], "--edition=2024", *flags,
                   str(Path(__file__).with_name("main.rs")), "--", case], 10)
        results.append(f"{model} {case} exit={code}")
        if code not in (0, 1):
            raise SystemExit(f"Unexpected failure/timeout: {model} {case} exit={code}")
        expected_failure = case == "box-after-write" or (
            model == "stacked" and case == "box-after-read"
        )
        output = (BASE / f"{model}-{case}.log").read_text()
        if code != int(expected_failure):
            raise SystemExit(f"Model result changed: {model} {case} exit={code}")
        if expected_failure:
            if "Undefined Behavior" not in output or "retag" not in output and "reborrow" not in output:
                raise SystemExit(f"Not the expected aliasing diagnostic: {model} {case}")
        elif f"PASS {case}" not in output:
            raise SystemExit(f"Missing success assertion: {model} {case}")
(BASE / "results.txt").write_text("\n".join(results) + "\n")
print("\n".join(results))
