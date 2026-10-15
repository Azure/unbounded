#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Run an exact pure-flow Miri subset, without pipe FFI or recycler wiping."""

import os
from pathlib import Path
import subprocess
import sys

TOOLCHAIN = "nightly-2025-11-21"
TESTS = [
    "coalesce::tests::first_completion_wins_for_early_and_late_readers",
    "coalesce::tests::last_handle_drop_releases_leadership_and_registration",
    "coalesce::tests::capacity_bounds_keys_waiters_and_completed_readers",
    "admission::handoff::tests::handoff_constructor_keeps_first_unique_targets",
    "admission::handoff::tests::target_waker_drop_can_reenter_replacement_and_rejection",
    "admission::handoff::tests::target_waker_callbacks_can_reenter_repeated_delivery",
]


def complete(output):
    """Reject missing, renamed, ignored, or failed tests, including an empty run."""
    lines = output.splitlines()
    summary = f"test result: ok. {len(TESTS)} passed; 0 failed; 0 ignored;"
    return all(f"test {name} ... ok" in lines for name in TESTS) and any(
        line.startswith(summary) for line in lines
    )


def main():
    root = Path(__file__).resolve().parents[2]
    command = [
        "timeout", "--signal=TERM", "--kill-after=10s", "290s",
        "cargo", "+" + TOOLCHAIN, "miri", "test", "--locked",
        "--manifest-path", str(root / "cmd/racer-dataplane/Cargo.toml"),
        "--target-dir", os.environ.get("FLOW_MIRI_TARGET_DIR", str(root / "tmp/flow-miri-target")),
        "-p", "flow-control", "--no-default-features", "--features", "simulation",
        "--lib", "-j", "2", "--", "--exact", "--test-threads=1", *TESTS,
    ]
    # Do not inherit flags that disable Miri isolation, aliasing, or leak checks.
    environment = dict(os.environ, MIRIFLAGS="", CARGO_TERM_COLOR="never")
    print("Running: " + " ".join(command), flush=True)
    result = subprocess.run(command, env=environment, stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, text=True, check=False)
    print(result.stdout, end="", flush=True)
    if result.returncode:
        return result.returncode
    if not complete(result.stdout):
        print("ERROR: exact flow allowlist did not execute every test", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
