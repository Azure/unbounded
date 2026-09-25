"""Emit a test crate using production modules, excluding only app composition.

Modules are inlined because Rust's #[path] module lookup differs from normal
`mod name;` lookup for nested children. No production logic changes.
Output goes directly to rustc stdin, not to the shared source tree.
"""
import pathlib
import re
import sys

source = pathlib.Path(sys.argv[1]).resolve() / "src"
print("#![allow(dead_code)]\n#![deny(unsafe_op_in_unsafe_fn)]")
modules = re.findall(r"(?:pub(?:\(crate\))?\s+)?mod (\w+);", (source / "lib.rs").read_text())


def expand(path):
    """Preserve module content and recurse with Cargo's normal child directory."""
    text = path.read_text()
    text = re.sub(
        r'(include(?:_(?:str|bytes))?!\()"([^"]+)"',
        lambda m: m[1] + '"' + str((path.parent / m[2]).resolve()) + '"',
        text,
    )
    text = re.sub(
        r'#\[path\s*=\s*"([^"]+)"\]\s*((?:pub(?:\(crate\))?\s+)?)mod (\w+);',
        lambda m: f'{m[2]}mod {m[3]} {{\n{expand((path.parent / m[1]).resolve())}\n}}',
        text,
    )
    child_dir = path.parent if path.name == "mod.rs" else path.with_suffix("")

    def child(match):
        name = match[2]
        target = child_dir / f"{name}.rs"
        if not target.exists():
            target = child_dir / name / "mod.rs"
        # Existing explicit #[path] handles exceptional sibling test modules.
        if not target.exists():
            return match[0]
        return f"{match[1]}mod {name} {{\n{expand(target)}\n}}"

    return re.sub(r"(?m)^((?:pub(?:\(crate\))?\s+)?)mod (\w+);", child, text)


for module in modules:
    if module == "app":
        continue
    text = expand(source / f"{module}.rs")
    print(f"pub mod {module} {{\n{text}\n}}")
