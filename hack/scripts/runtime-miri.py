#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Exact pure-runtime Miri allowlist; no io_uring, affinity, or filesystem syscalls."""

import argparse
import os
from pathlib import Path
import subprocess
import sys

TOOLCHAIN = "nightly-2025-11-21"
GROUPS = {
    "channel": [
        "channel::tests::endpoint_layout_is_compact_but_shared_cursors_remain_isolated",
        "channel::tests::cursor_mapping_and_distance_cover_both_laps_and_maximum_capacity",
        "channel::tests::peer_caches_remain_conservative_across_wrap_and_advisory_polling",
        "channel::tests::send_wake_reentry_refreshes_caches_without_outer_restore",
        "channel::tests::receive_wake_reentry_refreshes_caches_without_outer_restore",
        "channel::tests::registration_clone_and_drop_reentry_can_wrap_cached_cursors",
        "channel::tests::endpoint_drop_orders_reclaim_each_value_once",
        "channel::tests::reentrant_cleanup_after_wrap_and_panic_resumes_from_authoritative_head",
        "channel::tests::zero_sized_values_still_have_exactly_once_destructors",
        "channel::tests::readiness_future_cancellation_and_closure",
        "channel::tests::receiver_drop_racing_send_preserves_ownership",
        "channel::tests::sender_close_racing_receive_never_reports_eof_before_final_value",
        "channel::tests::transitions_before_during_and_after_waker_registration_are_not_lost",
        "channel::tests::orphan_destructor_can_reenter_cleanup",
        "channel::tests::orphan_cleanup_advances_before_panicking_destructor",
        "channel::tests::invalid_capacity_is_rejected_before_allocation",
        "channel::tests::saturation_returns_owner_and_close_drains",
        "channel::tests::non_power_of_two_capacity_wraps_without_overwriting",
        "channel::tests::wakeups_cover_capacity_data_and_close",
        "channel::tests::transfers_fifo_between_threads",
    ],
    "offload": [
        "offload::tests::admission_raw_waker_and_scope_destructors_reenter_without_borrows",
        "offload::tests::registry_raw_waker_clone_drop_and_wake_allow_mutating_reentry",
        "offload::tests::client_completion_callbacks_can_receive_and_poll_reentrantly",
        "offload::tests::duplicate_registration_and_delivery_preserve_first_owner_and_stale_guard_is_inert",
        "offload::tests::result_destructors_can_reenter_delivery_registry",
        "offload::tests::success_and_failure_keep_credit_through_consumption",
        "offload::tests::accepted_registration_drop_fences_until_reap_and_completed_drop_reclaims",
        "offload::tests::worker_loss_fences_queued_but_not_executing_owners",
    ],
    "scheduler": [
        "drivers::tests::panic_releases_capacity_before_task_drop_and_queue_remains_usable",
        "drivers::tests::budget_backlog_wakes_outer_owner_and_nested_poll_cannot_replace_it",
        "drivers::tests::replacing_owner_drops_waker_outside_queue_borrows",
        "drivers::tests::panicking_completed_task_destructor_does_not_leak_capacity",
        "drivers::tests::blocked_driver_is_not_repolled_until_its_own_wake",
        "drivers::tests::owned_operation_progresses_after_request_receiver_disappears",
        "drivers::tests::recursive_poll_defers_children_and_crash_preserves_unused_permits",
    ],
    "memory": [
        "reactor::filesystem::memory_tests::delayed_pathname_consumer_survives_owner_moves",
        "reactor::filesystem::memory_tests::delayed_syscall_read_and_write_survive_owner_moves",
        "reactor::tests::sockaddr_encoding_is_owned_and_validated",
    ],
}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("group", choices=[*GROUPS, "list"])
    args = parser.parse_args()
    if args.group == "list":
        for group, tests in GROUPS.items():
            print(group + ":\n  " + "\n  ".join(tests))
        return 0

    root = Path(__file__).resolve().parents[2]
    tests = GROUPS[args.group]
    command = [
        "timeout", "--signal=TERM", "--kill-after=10s", "290s",
        "cargo", "+" + TOOLCHAIN, "miri", "test", "--locked",
        "--manifest-path", str(root / "cmd/racer-dataplane/Cargo.toml"),
        "--target-dir", os.environ.get("RUNTIME_MIRI_TARGET_DIR", str(root / "tmp/runtime-miri-target")),
        "-p", "uring-runtime", "--no-default-features", "--lib", "-j", "2",
        "--", "--exact", "--test-threads=1", *tests,
    ]
    # Keep Miri's default isolation, aliasing, alignment, and leak checks enabled.
    # Inherit only the tool/cache placement, not caller flags that weaken checks.
    environment = dict(os.environ, MIRIFLAGS="")
    print("Running: " + " ".join(command), flush=True)
    result = subprocess.run(command, env=environment, stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, text=True, check=False)
    print(result.stdout, end="", flush=True)
    if result.returncode:
        return result.returncode
    expected = f"test result: ok. {len(tests)} passed; 0 failed; 0 ignored;"
    if expected not in result.stdout:
        print("ERROR: exact allowlist did not execute every test: " + expected, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
