#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Module-scoped Miri runs for pure worker-runtime code; no io_uring, affinity, or filesystem syscalls.

Selection is by test module rather than by test name, so a new test in a
covered module is checked without editing this file. A test that genuinely
cannot run under Miri carries `#[cfg_attr(miri, ignore)]` next to the test, so
the exclusion and its reason live with the code.
"""

import argparse
import os
from pathlib import Path
import re
import subprocess
import sys

TOOLCHAIN = "nightly-2025-11-21"
# group -> (package, test module, floor)
#
# The floor is the number of tests the module must select, not a name list: it
# catches a renamed, deleted, or emptied module. The module may grow past it.
# "mailbox" was the original xthread::mailbox group; that queue, and its tests,
# were deleted, so this leg covers its owned-handoff successor.
GROUPS = {
    "spsc": ("racer-io", "xthread::spsc::tests", 18),
    "mailbox": ("racer-io", "xthread::command::tests", 16),
    "offload": ("racer-io", "xthread::offload::tests", 29),
    "scheduler": ("racer", "drivers::tests", 15),
    "memory": ("racer-io", "ring::owner::tests", 2),
    "rt-memory": ("racer-io", "scoped::fs::memory_tests", 1),
}

RESULT = re.compile(r"test result: ok\. (\d+) passed; (\d+) failed; (\d+) ignored;")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("group", choices=[*GROUPS, "list"])
    args = parser.parse_args()
    if args.group == "list":
        for group, (package, module, floor) in GROUPS.items():
            print(f"{group} ({package}): {module} (at least {floor} tests)")
        return 0

    root = Path(__file__).resolve().parents[2]
    package, module, floor = GROUPS[args.group]
    command = [
        "timeout", "--signal=TERM", "--kill-after=10s", "1100s" if package == "racer" else "290s",
        "cargo", "+" + TOOLCHAIN, "miri", "test", "--locked",
        "--manifest-path", str(root / "cmd/racer/Cargo.toml"),
        "--target-dir", os.environ.get("RACER_IO_MIRI_TARGET_DIR", str(root / "tmp/racer-io-miri-target")),
        "-p", package, "--lib", "-j", "2",
        "--", "--test-threads=1", module + "::",
    ]
    # Keep Miri's default isolation, aliasing, alignment, and leak checks enabled.
    # Inherit only the tool/cache placement, not caller flags that weaken checks.
    environment = dict(os.environ, MIRIFLAGS="")
    if package == "racer":
        # The pinned Miri nightly predates if-let guard stabilization, which the
        # root crate uses. Enabling the gate does not change checked semantics.
        environment["RUSTFLAGS"] = "-Zcrate-attr=feature(if_let_guard)"
    print("Running: " + " ".join(command), flush=True)
    result = subprocess.run(command, env=environment, stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, text=True, check=False)
    print(result.stdout, end="", flush=True)
    if result.returncode:
        return result.returncode
    matches = RESULT.findall(result.stdout)
    if len(matches) != 1:
        print(f"ERROR: expected one test binary result for {module}, found {len(matches)}", file=sys.stderr)
        return 1
    passed, failed, ignored = (int(count) for count in matches[0])
    selected = passed + ignored
    if failed or passed == 0 or selected < floor:
        print(
            f"ERROR: {module} passed={passed} failed={failed} ignored={ignored};"
            f" expected 0 failed, at least one executed test, and {floor} selected",
            file=sys.stderr,
        )
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
