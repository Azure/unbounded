"""Test live control against committed production dependencies during integration.

No dependency is replaced by a fake. Excludes concurrent uncommitted application
changes; supplementary to the full Cargo control test filter.
"""
import os
import pathlib
import re
import subprocess

root = pathlib.Path(__file__).resolve().parents[2]
repo = root.parents[1]
source = root / "src"
base = os.environ.get("CONTROL_CHECK_BASE", "HEAD")


def contents(path):
    if path == source / "control.rs" or source / "control" in path.parents:
        return path.read_text()
    return subprocess.check_output(
        ["git", "show", base + ":" + str(path.relative_to(repo))], cwd=repo, text=True
    )


def expand(path):
    text = contents(path)
    text = re.sub(r'(include(?:_(?:str|bytes))?!\()"([^"]+)"',
                  lambda m: m[1] + '"' + str((path.parent / m[2]).resolve()) + '"', text)
    text = re.sub(r'#\[path\s*=\s*"([^"]+)"\]\s*((?:pub(?:\(crate\))?\s+)?)mod (\w+);',
                  lambda m: f'{m[2]}mod {m[3]} {{\n{expand((path.parent / m[1]).resolve())}\n}}', text)
    directory = path.parent if path.name == "mod.rs" else path.with_suffix("")

    def child(match):
        target = directory / (match[2] + ".rs")
        if not target.exists():
            target = directory / match[2] / "mod.rs"
        return f'{match[1]}mod {match[2]} {{\n{expand(target)}\n}}'

    return re.sub(r"(?m)^((?:pub(?:\(crate\))?\s+)?)mod (\w+);", child, text)


modules = re.findall(r"(?:pub(?:\(crate\))?\s+)?mod (\w+);", contents(source / "lib.rs"))
parts = ["#![allow(dead_code)]\n#![deny(unsafe_op_in_unsafe_fn)]"]
for module in modules:
    if module not in ("app", "telemetry"):
        parts.append(f"pub mod {module} {{\n{expand(source / (module + '.rs'))}\n}}")
target = root / "target"
args = ["rustc", "--edition", "2024", "--test", "--crate-name", "control_component",
        "-L", "dependency=" + str(target / "debug/deps")]
for dependency in "base64 chacha20poly1305 ed25519_dalek futures getrandom httparse io_uring libc rcgen rustls rustls_pemfile serde serde_json sha2 x509_parser zeroize".split():
    candidates = sorted((target / "debug/deps").glob(f"lib{dependency}-*.rlib"))
    if len(candidates) != 1:
        raise RuntimeError(f"expected one compiled dependency: {dependency}")
    args.extend(["--extern", f"{dependency}={candidates[0]}"])
for native in (target / "debug/build").glob("ring-*/out"):
    args.extend(["-L", "native=" + str(native)])
binary = target / "control-component-tests"
args.extend(["-", "-o", str(binary)])
environment = dict(os.environ, CARGO_MANIFEST_DIR=str(root))
subprocess.run(args, input="\n".join(parts), text=True, env=environment, check=True)
subprocess.run([str(binary), "control::", "--test-threads=1", "--nocapture"], env=environment, check=True)
