# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Offline tests. Fake host only; never invokes kubectl or a real sysctl."""

import copy
import importlib.util
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("tso", Path(__file__).with_name("racer-tso-canary.py"))
tso = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(tso)


def apps():
    return {role: dict(health="ok", ready="ready", load={
        "racer_loadgen_applied_concurrency": 4,
        "racer_loadgen_in_flight": 4,
        "racer_loadgen_verified_bytes_total": 0,
        'racer_loadgen_pulls_total{result="success"}': 0,
    }, dataplane={"racer_crypto_decrypt_aead_rejected_total": 55,
                  "racer_crypto_decrypt_crc_rejected_total": 0,
                  "racer_peer_page_body_nanoseconds_count": 0,
                  "racer_peer_page_body_nanoseconds_sum": 0}) for role in tso.HOSTS}


class FakeHost:
    role = "bad"

    def __init__(self):
        self.value = 2
        self.writes = []
        self.now = 0
        self.fail_write = False
        self.fail_sample = False
        self.data = apps()

    def read(self):
        return self.value

    def write(self, value):
        self.writes.append(value)
        self.value = value
        if value == 32 and self.fail_write:
            raise RuntimeError("lost write acknowledgement")

    def sleep(self, seconds):
        self.now += seconds

    def snapshot(self):
        if self.value == 32 and self.fail_sample:
            raise RuntimeError("metrics unavailable")
        return dict(setting=self.value, mono=self.now, apps=copy.deepcopy(self.data),
                    nic={"hc_tx_bytes": int(self.now * 1e6)},
                    queues={"TX-0": {"cq_head": int(self.now * 100), "sq_pend_skb_qlen": 5}})


class ExperimentTest(unittest.TestCase):
    def run_fake(self, host, mode="canary", reporter=None):
        events = []
        tso.experiment(host, mode, 4, reporter or (lambda event, **fields: events.append((event, fields))),
                       clock=lambda: host.now, sleep=host.sleep)
        return events

    def test_success_fixed_stages_and_restore(self):
        host = FakeHost()
        events = self.run_fake(host)
        self.assertEqual(host.writes, [32, 2])
        self.assertEqual(host.value, 2)
        self.assertEqual(host.now, 120)
        self.assertEqual([d["stage"] for e, d in events if e == "stage_summary"],
                         ["baseline", "changed", "after"])

    def test_collect_never_writes(self):
        host = FakeHost()
        self.run_fake(host, "collect")
        self.assertEqual(host.writes, [])
        self.assertEqual(host.now, 120)

    def test_control_never_writes(self):
        host = FakeHost()
        host.role = "control"
        self.run_fake(host)
        self.assertEqual(host.writes, [])

    def test_wrong_original_refuses_without_write(self):
        host = FakeHost()
        host.value = 16
        with self.assertRaisesRegex(RuntimeError, "original"):
            self.run_fake(host)
        self.assertEqual(host.writes, [])

    def test_uncertain_write_ack_restores(self):
        host = FakeHost()
        host.fail_write = True
        with self.assertRaisesRegex(RuntimeError, "acknowledgement"):
            self.run_fake(host)
        self.assertEqual(host.writes, [32, 2])

    def test_failed_measurement_restores(self):
        host = FakeHost()
        host.fail_sample = True
        with self.assertRaisesRegex(RuntimeError, "unavailable"):
            self.run_fake(host)
        self.assertEqual(host.value, 2)

    def test_log_failure_after_write_restores(self):
        host = FakeHost()
        def report(event, **fields):
            if event == "after_write":
                raise BrokenPipeError()
        with self.assertRaises(BrokenPipeError):
            self.run_fake(host, reporter=report)
        self.assertEqual(host.value, 2)

    def test_signal_exception_restores(self):
        host = FakeHost()
        def report(event, **fields):
            if event == "after_write":
                raise KeyboardInterrupt()
        with self.assertRaises(KeyboardInterrupt):
            self.run_fake(host, reporter=report)
        self.assertEqual(host.value, 2)

    def test_preexisting_rejections_are_baseline_not_success(self):
        tso.safety(None, apps(), 4)
        tso.safety(apps(), apps(), 4)

    def test_new_failure_and_reset_and_load_change(self):
        for kind in ("integrity", "reset", "missing", "load", "health", "pull"):
            with self.subTest(kind=kind):
                a, b = apps(), apps()
                if kind == "integrity":
                    b["bad"]["dataplane"]["racer_crypto_decrypt_aead_rejected_total"] += 1
                elif kind == "reset":
                    b["bad"]["dataplane"]["racer_crypto_decrypt_aead_rejected_total"] = 0
                elif kind == "missing":
                    b["bad"]["dataplane"] = {}
                elif kind == "load":
                    b["bad"]["load"]["racer_loadgen_applied_concurrency"] = 0
                elif kind == "health":
                    b["control"]["ready"] = "no"
                else:
                    b["bad"]["load"]['racer_loadgen_pulls_total{result="error"}'] = 1
                with self.assertRaises(RuntimeError):
                    tso.safety(a, b, 4)

    def test_shell_exit_and_term_restore_without_real_sysctl(self):
        # Functions replace every command that could read/write host configuration.
        for ending in ("return 7", "kill -TERM $$"):
            with self.subTest(ending=ending):
                prelude = '''
cat() { case "$1" in */hostname) printf "%s\\n" aks-ddsv6-84072342-vmss0000bl;; *) printf "2\\n";; esac; }
sysctl() { printf "FAKE_RESTORE\\n" >&2; }
timeout() { shift 3; "$@"; }
python3() { ENDING; }
'''.replace("ENDING", ending)
                with self.assertRaises(subprocess.CalledProcessError) as caught:
                    tso.command(["sh", "-c", prelude + tso.remote_shell("bad", "canary")], 5)
                self.assertIn("FAKE_RESTORE", caught.exception.stderr)

    def test_collect_shell_has_no_restoration_trap(self):
        self.assertNotIn("sysctl -w", tso.remote_shell("bad", "collect"))
        self.assertNotIn("sysctl -w", tso.remote_shell("control", "canary"))

    def test_tcp_aggregates_do_not_export_endpoints(self):
        text = "0 100 10.0.0.1:18082 10.0.0.2:1234\n rtt:9.5/1 retrans:0/7 notsent:100 skmem:(r0,rb1,t20,tb400,w30)"
        row = tso.tcp_summary(text)
        self.assertEqual(row["memory"], {"t": 20, "tb": 400, "w": 30})
        self.assertEqual(row["rtt_p50_ms"], 9.5)
        self.assertNotIn("10.0.0", str(row))

    def test_summary_uses_real_interval_and_null_empty_body(self):
        h = FakeHost()
        a = h.snapshot()
        h.now = 10
        b = h.snapshot()
        b["nic"].update(tx_0_tso_packets=10, tx_0_tso_bytes=500000)
        a["nic"].update(tx_0_tso_packets=0, tx_0_tso_bytes=0)
        s = tso.summarize(a, b, [a, b])
        self.assertEqual(s["seconds"], 10)
        self.assertEqual(s["tso_mean_bytes"], 50000)
        self.assertEqual(s["pending_mean"], 5)
        self.assertEqual(s["cq_per_GB"], 100000)
        self.assertIsNone(s["apps"]["bad"]["body_mean_ms"])
        b["nic"]["hc_tx_bytes"] = -1
        with self.assertRaisesRegex(RuntimeError, "reset"):
            tso.summarize(a, b, [a, b])

    def test_restore_attempt_survives_before_restore_log_failure(self):
        h = FakeHost()
        def report(event, **fields):
            if event == "before_restore":
                raise RuntimeError("journal broken")
        with self.assertRaisesRegex(RuntimeError, "journal"):
            self.run_fake(h, reporter=report)
        self.assertEqual(h.value, 2)

    def test_independent_restore_uses_two_new_execs_and_checks_readback(self):
        with patch.object(tso, "command", side_effect=["", "2\n"]) as call, patch.object(tso, "emit"):
            tso.independent_restore()
        self.assertEqual(call.call_count, 2)
        self.assertIn("exec", call.call_args_list[0].args[0])
        self.assertIn("sysctl", call.call_args_list[1].args[0])
        with patch.object(tso, "command", side_effect=["", "32\n"]), patch.object(tso, "emit"):
            with self.assertRaisesRegex(RuntimeError, "verification failed"):
                tso.independent_restore()

    def test_command_always_uses_external_term_timeout(self):
        with patch.object(tso.subprocess, "run") as run:
            tso.command(["fake", "read"], 7)
        self.assertEqual(run.call_args.args[0],
                         ["timeout", "--signal=TERM", "--kill-after=10s", "7s", "fake", "read"])


if __name__ == "__main__":
    unittest.main()
