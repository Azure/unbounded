#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Exact Miri allowlist for pure worker-runtime code; no io_uring, affinity, or filesystem syscalls."""

import argparse
import os
from pathlib import Path
import subprocess
import sys

TOOLCHAIN = "nightly-2025-11-21"
GROUPS = {
    "spsc": ("racer-io", [
        "xthread::spsc::tests::endpoint_layout_is_compact_but_shared_cursors_remain_isolated",
        "xthread::spsc::tests::cursor_mapping_and_distance_cover_both_laps_and_maximum_capacity",
        "xthread::spsc::tests::peer_caches_remain_conservative_across_wrap_and_advisory_polling",
        "xthread::spsc::tests::send_wake_reentry_refreshes_caches_without_outer_restore",
        "xthread::spsc::tests::receive_wake_reentry_refreshes_caches_without_outer_restore",
        "xthread::spsc::tests::registration_clone_and_drop_reentry_can_wrap_cached_cursors",
        "xthread::spsc::tests::endpoint_drop_orders_reclaim_each_value_once",
        "xthread::spsc::tests::zero_sized_values_still_have_exactly_once_destructors",
        "xthread::spsc::tests::readiness_future_cancellation_and_closure",
        "xthread::spsc::tests::receiver_drop_racing_send_preserves_ownership",
        "xthread::spsc::tests::sender_close_racing_receive_never_reports_eof_before_final_value",
        "xthread::spsc::tests::transitions_before_during_and_after_waker_registration_are_not_lost",
        "xthread::spsc::tests::orphan_destructor_can_reenter_cleanup",
        "xthread::spsc::tests::invalid_capacity_is_rejected_before_allocation",
        "xthread::spsc::tests::saturation_returns_owner_and_close_drains",
        "xthread::spsc::tests::transfers_fifo_between_threads",
        "xthread::spsc::tests::non_power_of_two_capacity_wraps_without_overwriting",
        "xthread::spsc::tests::wakeups_cover_capacity_data_and_close",
    ]),
    "mailbox": ("racer-io", [
        "xthread::mailbox::tests::calls_round_trip_and_credits_return_on_response",
        "xthread::mailbox::tests::dropped_responders_fail_only_their_call",
        "xthread::mailbox::tests::abandoned_replies_hand_values_back_and_release_credits",
        "xthread::mailbox::tests::close_rejects_new_messages_and_keeps_queued_ones",
        "xthread::mailbox::tests::credits_wake_waiters_and_senders_notify_from_other_threads",
        "xthread::mailbox::tests::many_producers_deliver_every_call",
    ]),
    "offload": ("racer", [
        "rt::offload::tests::admission_raw_waker_and_scope_destructors_reenter_without_borrows",
        "rt::offload::tests::registry_raw_waker_clone_drop_and_wake_allow_mutating_reentry",
        "rt::offload::tests::client_completion_callbacks_can_receive_and_poll_reentrantly",
        "rt::offload::tests::duplicate_registration_and_delivery_preserve_first_owner_and_stale_guard_is_inert",
        "rt::offload::tests::result_destructors_can_reenter_delivery_registry",
        "rt::offload::tests::success_and_failure_keep_credit_through_consumption",
        "rt::offload::tests::accepted_registration_drop_fences_until_reap_and_completed_drop_reclaims",
        "rt::offload::tests::worker_loss_fences_queued_but_not_executing_owners",
    ]),
    "scheduler": ("racer", [
        "rt::drivers::tests::budget_backlog_wakes_outer_owner_and_nested_poll_cannot_replace_it",
        "rt::drivers::tests::replacing_owner_drops_waker_outside_queue_borrows",
        "rt::drivers::tests::blocked_driver_is_not_repolled_until_its_own_wake",
        "rt::drivers::tests::owned_operation_progresses_after_request_receiver_disappears",
        "rt::drivers::tests::recursive_poll_defers_children_and_crash_preserves_unused_permits",
    ]),
    "memory": ("racer-io", [
        "ring::owner::tests::inline_and_boxed_owners_round_trip_and_drop_once",
        "ring::owner::tests::overaligned_owners_are_boxed",
    ]),
    "rt-memory": ("racer", [
        "rt::filesystem::memory_tests::shared_allocation_preserves_cursor_and_fails_oversized",
        "rt::filesystem::memory_tests::flags_and_linux_path_bounds_are_not_application_limits",
    ]),
}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("group", choices=[*GROUPS, "list"])
    args = parser.parse_args()
    if args.group == "list":
        for group, (package, tests) in GROUPS.items():
            print(f"{group} ({package}):\n  " + "\n  ".join(tests))
        return 0

    root = Path(__file__).resolve().parents[2]
    package, tests = GROUPS[args.group]
    command = [
        "timeout", "--signal=TERM", "--kill-after=10s", "290s",
        "cargo", "+" + TOOLCHAIN, "miri", "test", "--locked",
        "--manifest-path", str(root / "cmd/racer/Cargo.toml"),
        "--target-dir", os.environ.get("RUNTIME_MIRI_TARGET_DIR", str(root / "tmp/runtime-miri-target")),
        "-p", package, "--lib", "-j", "2",
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
