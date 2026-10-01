# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Offline only: mocked commands and fake settings, no Kubernetes access."""
import copy
import importlib.util
from pathlib import Path
import subprocess
import signal
import os
import json
import tempfile
import time
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("rx", Path(__file__).with_name("racer-rx-checksum-canary.py"))
rx = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(rx)


def original():
    return {d: {"rx-checksumming": ("on", False), "generic-receive-offload": ("on", False)}
            for d in ("eth0", rx.VF)}


class Host:
    def __init__(self):
        self.data = original()
        self.writes = []
        self.fail = False

    def features(self):
        return copy.deepcopy(self.data)

    def write(self, value):
        self.writes.append(value)
        for dev in self.data:
            self.data[dev]["rx-checksumming"] = (value, False)
        if self.fail and value == "off":
            raise RuntimeError("lost acknowledgement")


def row():
    return dict(health="ok", ready="ready", load={"racer_loadgen_applied_concurrency": 8,
                "racer_loadgen_verified_bytes_total": 1}, dataplane={"racer_crypto_decrypt_aead_rejected_total": 1,
                "racer_crypto_decrypt_crc_rejected_total": 0},
                nic={k: 1 for k in ("rx_packets", "rx_bytes", "rx_csum_none", "rx_csum_complete", "rx_csum_unnecessary")},
                tcp_csum=1, aead={"total": 1})


class Tests(unittest.TestCase):
    def setUp(self):
        self.profile = rx.PROFILE
        self.addCleanup(rx.select_profile, self.profile)

    def test_reviewed_profile_selection_and_authorization(self):
        historical = rx.AUTH
        rx.select_profile("ddv5-6o-to-adsv5-7n-20261001")
        self.assertEqual(rx.NODE, "aks-adsv5-13731677-vmss00007n")
        self.assertEqual(rx.SENDER, "024467ab-e873-46d1-9d92-d15119f3ea28")
        self.assertEqual(rx.AUTH, "4c4c2810-8c4d-49aa-8c2c-389f9e1728a4:rx:on->off->on")
        self.assertEqual((rx.IP, rx.POD, rx.VF),
                         ("10.224.5.52", "unbounded-net-node-km9tz", "enP1216s1"))
        with self.assertRaisesRegex(RuntimeError, "unknown reviewed"):
            rx.select_profile("arbitrary-node")
        for auth in (historical, rx.UID, rx.UID + ":rx:off", None):
            with patch.object(rx, "preflight") as preflight, patch.object(rx, "stream") as stream:
                with self.assertRaisesRegex(RuntimeError, "not authorized"):
                    rx.remote(dict(authorize=auth), "changed", 115)
                preflight.assert_not_called()
                stream.assert_not_called()

    def test_profile_propagates_to_remote_and_restore(self):
        rx.select_profile("ddv5-6o-to-adsv5-7n-20261001")
        config = dict(authorize=rx.AUTH, concurrency=10, load_ip="10.0.0.1")
        with patch.object(rx, "stream") as stream:
            rx.remote(config, "changed", 115)
        argv, seconds = stream.call_args.args
        self.assertEqual(seconds, 120)
        self.assertIn(rx.POD, argv)
        self.assertIn(rx.NODE, argv[argv.index("-c", argv.index("sh")) + 1])
        self.assertEqual(json.loads(argv[-1])["profile"], rx.PROFILE)
        self.test_independent_restore_verifies_both_interfaces()

    def test_identity_change_after_baseline_never_enters_restore(self):
        rows = [dict(event="original", original=original()),
                dict(event="summary", fresh_pair_rejects=1, ring_gap=False, verified_bytes=99,
                     nic_delta={"rx_bytes": 99}, state={"previous": row()})]
        with patch.object(rx, "preflight", side_effect=[{}, {"load_ip": "changed"}]), \
                patch.object(rx, "remote", return_value=rows) as remote, \
                patch.object(rx, "independent_restore") as restore:
            with self.assertRaisesRegex(RuntimeError, "identity changed"):
                rx.run(type("Args", (), {"concurrency": 10})())
        self.assertEqual(remote.call_count, 1)
        restore.assert_not_called()

    def test_remote_rejects_wrong_authorization_before_host_access(self):
        source = Path(rx.__file__).read_text().split('if __name__ == "__main__":')[0]
        source += '\nhost_identity = lambda: print("UNREACHABLE")\nmain()\n'
        config = dict(mode="changed", profile="ddv5-6o-to-adsv5-7n-20261001", authorize=rx.AUTH)
        with self.assertRaises(subprocess.CalledProcessError) as caught:
            rx.command(["python3", "-B", "-c", source, "--remote", json.dumps(config)], 3)
        self.assertNotIn("UNREACHABLE", caught.exception.stdout)
        self.assertEqual(json.loads(caught.exception.stdout)["diagnostic"], "remote mutation not authorized")

    def test_preflight_strict_profile_identity(self):
        rx.select_profile("ddv5-6o-to-adsv5-7n-20261001")
        node = dict(metadata=dict(name=rx.NODE, uid=rx.UID),
                    status=dict(addresses=[dict(type="InternalIP", address=rx.IP)]))
        def pod(name, app):
            return dict(metadata=dict(name=name, labels={"app.kubernetes.io/name": app}),
                        spec=dict(nodeName=rx.NODE, hostNetwork=True, containers=[dict(name="node")]),
                        status=dict(phase="Running", podIP=rx.IP,
                                    conditions=[dict(type="Ready", status="True")]))
        pods = [pod(rx.POD, "unbounded-net-node"), pod("load", "racer-loadgen"),
                pod("plane", "racer-dataplane")]
        def check(n, p):
            with patch.object(rx, "command", side_effect=[json.dumps(n), json.dumps(dict(items=p))]):
                return rx.preflight(10)
        self.assertEqual(check(node, pods), dict(load_ip=rx.IP, concurrency=10, profile=rx.PROFILE))
        for field in ("name", "uid"):
            bad = copy.deepcopy(node)
            bad["metadata"][field] = "wrong"
            with self.assertRaisesRegex(RuntimeError, "identity"):
                check(bad, pods)
        bad = copy.deepcopy(node)
        bad["status"]["addresses"][0]["type"] = "ExternalIP"
        with self.assertRaisesRegex(RuntimeError, "identity"):
            check(bad, pods)
        for index, group, key, value in (
                (0, "metadata", "name", "wrong-access"), (0, "spec", "nodeName", "wrong-node"),
                (0, "spec", "hostNetwork", False), (0, "spec", "containers", []),
                (0, "status", "podIP", "10.0.0.2"), (2, "status", "podIP", "10.0.0.2"),
                (2, "status", "conditions", []), (1, "metadata", "deletionTimestamp", "now")):
            bad = copy.deepcopy(pods)
            bad[index][group][key] = value
            with self.subTest(index=index, key=key), self.assertRaises(RuntimeError):
                check(node, bad)

    def test_host_ip_driver_and_vf_guards(self):
        rx.select_profile("ddv5-6o-to-adsv5-7n-20261001")
        values = [rx.NODE.upper(), json.dumps([dict(addr_info=[dict(local=rx.IP)])]),
                  "driver: hv_netvsc\n", "driver: mlx5_core\n"]
        with patch.object(rx, "command", side_effect=values), patch.object(Path, "is_dir", return_value=True) as is_dir:
            rx.host_identity()
        is_dir.assert_called_once()
        for index, value in ((0, "wrong-node"), (1, "[]"), (2, "driver: wrong"), (3, "driver: mana")):
            bad = list(values)
            bad[index] = value
            with patch.object(rx, "command", side_effect=bad), self.assertRaises(RuntimeError):
                rx.host_identity()
        with patch.object(rx, "command", side_effect=values), patch.object(Path, "is_dir", return_value=False):
            with self.assertRaisesRegex(RuntimeError, "VF association"):
                rx.host_identity()

    def test_success_and_restore(self):
        h = Host()
        stages = []
        rx.changed(h, dict(original=original(), not_after=1e20, concurrency=8),
                   report=lambda *a, **k: None, stage_fn=lambda *a, **k: stages.append(a[1]))
        self.assertEqual(h.writes, ["off", "on"])
        self.assertEqual(stages, ["off"])

    def test_error_term_and_uncertain_write_restore(self):
        for kind in ("error", "term", "write", "dependent"):
            h = Host()
            h.fail = kind == "write"
            def stage(*a, **k):
                if kind == "term":
                    rx.stop(15, None)
                raise RuntimeError("collector failure")
            def report(event, **fields):
                if kind == "dependent" and event == "before_mutation":
                    h.data[rx.VF]["generic-receive-offload"] = ("off", False)
            with self.assertRaises(RuntimeError):
                rx.changed(h, dict(original=original(), not_after=1e20, concurrency=8),
                           report=report, stage_fn=stage)
            self.assertEqual(h.writes[-1], "on")

    def test_reject_diagnostic_digest_and_readiness_abort(self):
        a, b = row(), row()
        b["dataplane"]["racer_crypto_decrypt_aead_rejected_total"] += 1
        rx.safety(a, b, 8)
        b["load"]['racer_loadgen_pull_failures_total{reason="digest_mismatch"}'] = 1
        with self.assertRaisesRegex(RuntimeError, "digest"):
            rx.safety(a, b, 8)
        b = row()
        b["ready"] = "no"
        with self.assertRaisesRegex(RuntimeError, "readiness"):
            rx.safety(a, b, 8)

    def test_no_recurrence_no_mutation_or_restore(self):
        rows = [dict(event="original", original=original()),
                dict(event="summary", fresh_pair_rejects=0, ring_gap=False, verified_bytes=99)]
        with patch.object(rx, "preflight", return_value={}), patch.object(rx, "remote", return_value=rows) as remote, \
                patch.object(rx, "independent_restore") as restore, patch.object(rx, "emit"):
            rx.run(type("Args", (), {"concurrency": 8})())
        self.assertEqual(remote.call_count, 1)
        restore.assert_not_called()

    def test_parent_restore_on_failed_remote(self):
        rows = [dict(event="original", original=original()),
                dict(event="summary", fresh_pair_rejects=1, ring_gap=False, verified_bytes=99,
                     nic_delta={"rx_bytes": 99}, state={"previous": row(), "seen": []})]
        with patch.object(rx, "preflight", return_value={}), patch.object(rx, "remote", side_effect=[rows, RuntimeError("exec")]), \
                patch.object(rx, "independent_restore") as restore, patch.object(rx.time, "sleep"), \
                patch.object(rx.time, "monotonic", side_effect=[0, 0, 131]):
            with self.assertRaises(RuntimeError):
                rx.run(type("Args", (), {"concurrency": 8})())
        restore.assert_called_once_with(original())

    def test_feature_readback_all_features(self):
        data = original()
        rx.check_features(data, copy.deepcopy(data), "on")
        data[rx.VF]["generic-receive-offload"] = ("off", False)
        with self.assertRaises(RuntimeError):
            rx.check_features(original(), data, "on")

    def test_shell_error_and_term_traps_mocked(self):
        for ending in ("return 7", "kill -TERM $$; return 7"):
            prelude = '''hostname() { printf '%s\\n' aks-adsv5-13731677-vmss00000w; }
ethtool() { printf 'RESTORED\\n' >&2; }
timeout() { shift 3; "$@"; }
sleep() { return 0; }
python3() { ENDING; }
'''.replace("ENDING", ending)
            with self.assertRaises(subprocess.CalledProcessError) as caught:
                rx.command(["sh", "-c", prelude + rx.remote_shell(True)], 3)
            self.assertIn("RESTORED", caught.exception.stderr)
        self.assertNotIn("ethtool -K", rx.remote_shell(False))

    def test_external_timeout(self):
        with patch.object(rx.subprocess, "run") as run:
            rx.command(["fake"], 3)
        self.assertEqual(run.call_args.args[0][:4], ["timeout", "--signal=TERM", "--kill-after=10s", "3s"])

    def test_cleanup_masks_repeated_term(self):
        old = signal.signal(signal.SIGTERM, rx.stop)
        try:
            with rx.cleanup_signals():
                os.kill(os.getpid(), signal.SIGTERM)
                os.kill(os.getpid(), signal.SIGTERM)
            self.assertIs(signal.getsignal(signal.SIGTERM), rx.stop)
        finally:
            signal.signal(signal.SIGTERM, old)

    def test_cross_stage_digest_aborts_before_off(self):
        h = Host()
        b = row()
        b["features"] = h.features()
        b["load"]['racer_loadgen_pull_failures_total{reason="digest_mismatch"}'] = 1
        h.snapshot = lambda: b
        with self.assertRaisesRegex(RuntimeError, "digest"):
            rx.changed(h, dict(original=original(), not_after=1e20, concurrency=8,
                               state={"previous": row()}), report=lambda *a, **k: None)
        self.assertEqual(h.writes, [])

    def test_expiry_rechecked_after_reporting(self):
        h = Host()
        config = dict(original=original(), not_after=1e20, concurrency=8)
        def report(*a, **k):
            config["not_after"] = 0
        with self.assertRaisesRegex(RuntimeError, "expired"):
            rx.changed(h, config, report=report)
        self.assertNotIn("off", h.writes)

    def test_stream_preserves_checkpoints_before_failure(self):
        with patch.object(rx, "emit") as emit:
            with self.assertRaisesRegex(RuntimeError, "remote failed"):
                rx.stream(["python3", "-c", 'print(\'{"event":"checkpoint"}\',flush=True);exit(2)'], 3)
        self.assertEqual(emit.call_args_list[0].args, ("remote",))
        self.assertEqual(emit.call_args_list[0].kwargs, dict(record={"event": "checkpoint"}))
        self.assertEqual(emit.call_args_list[-1].kwargs["status"], 2)

    def test_stream_cleans_exited_wrapper_group_without_buffering(self):
        # Real exited timeout wrapper with a TERM-resistant same-group child.
        # The test owns fallback cleanup; setsid() children are out of scope.
        for keep_pipe in (False, True):
            with self.subTest(keep_pipe=keep_pipe), tempfile.TemporaryDirectory(
                    dir=Path(__file__).resolve().parents[2]) as directory:
                pidfile = Path(directory) / "pid"
                source = '''import os,signal,sys,time
ready_r,ready_w=os.pipe()
pid=os.fork()
if pid:
 os.close(ready_w)
 os.read(ready_r,1)
 print('{"event":"ready"}',flush=True)
 os._exit(0)
os.close(ready_r)
signal.signal(signal.SIGTERM,signal.SIG_IGN)
with open(sys.argv[1],"w") as out: out.write(str(os.getpid()))
if sys.argv[2]=="False":
 os.close(1);os.close(2)
os.write(ready_w,b"x")
time.sleep(30)
'''
                popen = subprocess.Popen
                wrappers = []
                def launch(*args, **kwargs):
                    proc = popen(*args, **kwargs)
                    wrappers.append(proc)
                    return proc
                def emit(*args, **kwargs):
                    self.assertEqual(wrappers[0].wait(timeout=2), 0)
                    if keep_pipe:
                        raise RuntimeError("stop collection")
                try:
                    with patch.object(rx.subprocess, "Popen", side_effect=launch), \
                            patch.object(rx, "emit", side_effect=emit), \
                            patch.object(subprocess.Popen, "communicate", side_effect=AssertionError("must not buffer")):
                        if keep_pipe:
                            with self.assertRaisesRegex(RuntimeError, "stop collection"):
                                rx.stream(["python3", "-B", "-c", source, str(pidfile), str(keep_pipe)], 3)
                        else:
                            self.assertEqual(rx.stream(["python3", "-B", "-c", source,
                                                        str(pidfile), str(keep_pipe)], 3), [{"event": "ready"}])
                    pid = int(pidfile.read_text())
                    for _ in range(100):
                        try:
                            stopped = Path(f"/proc/{pid}/stat").read_text().split(")", 1)[1].split()[0] == "Z"
                        except (FileNotFoundError, ProcessLookupError):
                            stopped = True
                        if stopped:
                            break
                        time.sleep(.01)
                    else:
                        self.fail("same-group descendant survived stream cleanup")
                    self.assertTrue(wrappers[0].stdout.closed)
                    self.assertEqual(wrappers[0].returncode, 0)
                finally:
                    if pidfile.exists():
                        try:
                            os.kill(int(pidfile.read_text()), signal.SIGKILL)
                        except ProcessLookupError:
                            pass

    def test_uppercase_hostname_runs_real_remote_baseline_entry(self):
        # Real shell + Python + main entry; fake only host data and stage clock.
        source = Path(rx.__file__).read_text().split('if __name__ == "__main__":')[0]
        source += '''
class FakeHost:
    def __init__(self, config): pass
    def features(self):
        return {d: {"rx-checksumming": ("on", False)} for d in ("eth0", VF)}
Host = FakeHost
host_identity = lambda: None
def stage(*args, **kwargs):
    assert args[1:3] == ("baseline", 45)
    emit("baseline_entry_verified")
main()
'''
        prelude = 'hostname() { printf "%s\\n" aks-adsv5-13731677-vmss00000W; }\n'
        output = rx.command(["sh", "-c", prelude + rx.remote_shell(False), "test", source,
                             "--remote", json.dumps(dict(mode="baseline", concurrency=8, load_ip="127.0.0.1",
                                                         profile=rx.PROFILE))], 3)
        records = [json.loads(line) for line in output.splitlines()]
        self.assertEqual([r["event"] for r in records], ["original", "baseline_entry_verified"])

    def test_wrong_hostname_reports_safe_error_before_python(self):
        prelude = 'hostname() { printf "wrong-host\\n"; }\n'
        with patch.object(rx, "emit") as emit:
            with self.assertRaisesRegex(RuntimeError, "exit=41"):
                rx.stream(["sh", "-c", prelude + rx.remote_shell(False), "test", 'print("UNREACHABLE")'], 3)
        diagnostics = [c.kwargs for c in emit.call_args_list]
        self.assertIn(dict(diagnostic="rx-canary: hostname mismatch"), diagnostics)
        self.assertNotIn("UNREACHABLE", str(diagnostics))

    def test_stderr_allowlist_does_not_export_secrets(self):
        self.assertEqual(rx.safe_stderr(b"command terminated with exit code 41"),
                         "command terminated with exit code 41")
        for line in (b"Authorization: bearer SECRET", b"RuntimeError: SECRET", b"source token=SECRET"):
            self.assertIsNone(rx.safe_stderr(line))

    def test_unterminated_safe_stderr_is_preserved(self):
        with patch.object(rx, "emit") as emit:
            with self.assertRaisesRegex(RuntimeError, "exit=41"):
                rx.stream(["python3", "-c", 'import sys; sys.stderr.write("command terminated with exit code 41"); sys.exit(41)'], 3)
        self.assertEqual(emit.call_args_list[0].kwargs,
                         dict(diagnostic="command terminated with exit code 41"))

    def test_independent_restore_uses_same_case_normalization(self):
        text = "Features for interface:\nrx-checksumming: on\ngeneric-receive-offload: on\n"
        with patch.object(rx, "command", side_effect=["", text, text]) as command, patch.object(rx, "emit"):
            rx.independent_restore(original())
        script = command.call_args_list[0].args[0][-1]
        self.assertTrue(script.startswith(rx.hostname_guard()))
        prelude = '''hostname() { printf '%s\\n' aks-adsv5-13731677-vmss00000W; }
ethtool() { printf 'MOCK_RESTORE\\n'; }
'''
        self.assertEqual(rx.command(["sh", "-c", prelude + script], 3).strip(), "MOCK_RESTORE")

    def test_remote_exception_does_not_print_source_config(self):
        source = Path(rx.__file__).read_text().split('if __name__ == "__main__":')[0]
        source += '''
class FakeHost:
    def __init__(self, config): pass
    def features(self):
        raise subprocess.CalledProcessError(17, ["SECRET_ARG"], stderr="Authorization: SECRET")
Host = FakeHost
host_identity = lambda: None
main()
'''
        with self.assertRaises(subprocess.CalledProcessError) as caught:
            rx.command(["python3", "-B", "-c", source, "--remote",
                        json.dumps(dict(mode="baseline", profile=rx.PROFILE))], 3)
        record = json.loads(caught.exception.stdout)
        self.assertEqual(record["event"], "remote_error")
        self.assertEqual(record["exit"], 17)
        self.assertNotIn("SECRET", caught.exception.stdout + caught.exception.stderr)

    def test_real_shell_source_argument_and_watchdog_cleanup(self):
        # Real python, no stdin ambiguity; all host reads/writes are shell fakes.
        prelude = '''hostname() { printf '%s\\n' aks-adsv5-13731677-vmss00000w; }
ethtool() { printf 'RESTORED\\n' >&2; }
timeout() { shift 3; "$@"; }
'''
        out = rx.command(["sh", "-c", prelude + rx.remote_shell(True), "test",
                          'import sys; print(sys.argv[1]);', "SOURCE_OK"], 3)
        self.assertEqual(out.strip(), "SOURCE_OK")

    def test_watchdog_is_independent_and_bounded(self):
        script = rx.remote_shell(True)
        self.assertIn("time.sleep(110)", script)
        self.assertIn('kill -TERM "$watchdog"', script)
        self.assertIn('wait "$watchdog"', script)
        self.assertIn('wait "$child"', script)
        self.assertNotIn('python3 -u -B - "$@"', script)

    def test_watchdog_action_without_polling(self):
        # Execute the exact embedded watchdog with sleep and subprocess mocked.
        script = rx.remote_shell(True)
        source = script.split("python3 -B -c '", 1)[1].split("' </dev/null", 1)[0]
        with patch.object(rx.time, "sleep") as sleep, patch.object(subprocess, "run") as run:
            exec(source, {})
        sleep.assert_called_once_with(110)
        self.assertEqual(run.call_args.args[0], ["timeout", "--signal=TERM", "--kill-after=10s",
                                                "3s", "ethtool", "-K", "eth0", "rx", "on"])

    def test_independent_restore_verifies_both_interfaces(self):
        text = "Features for interface:\nrx-checksumming: on\ngeneric-receive-offload: on\n"
        with patch.object(rx, "command", side_effect=["", text, text]) as command, patch.object(rx, "emit"):
            rx.independent_restore(original())
        self.assertEqual(command.call_count, 3)
        self.assertIn(rx.VF, command.call_args_list[-1].args[0])
        with patch.object(rx, "command", side_effect=["", text, text.replace("rx-checksumming: on", "rx-checksumming: off")]), patch.object(rx, "emit"):
            with self.assertRaises(RuntimeError):
                rx.independent_restore(original())

    def test_restore_signal_is_ignored_inside_host_finally(self):
        h = Host()
        write = h.write
        def signal_write(value):
            if value == "on":
                os.kill(os.getpid(), signal.SIGTERM)
            write(value)
        h.write = signal_write
        old = signal.signal(signal.SIGTERM, rx.stop)
        try:
            rx.changed(h, dict(original=original(), not_after=1e20, concurrency=8),
                       report=lambda *a, **k: None, stage_fn=lambda *a, **k: None)
        finally:
            signal.signal(signal.SIGTERM, old)
        self.assertEqual(h.writes, ["off", "on"])

    def test_stage_fresh_pair_deduplicates_and_carries_state(self):
        rx.select_profile("ddv5-6o-to-adsv5-7n-20261001")
        h = Host()
        now = [0]
        def snapshot():
            b = row()
            b.update(mono=now[0], ms=int(now[0]*1000), features=h.features())
            b["load"]["racer_loadgen_verified_bytes_total"] += now[0]
            b["aead"] = dict(total=1 + int(now[0] > 0), records=[])
            if now[0]:
                b["aead"]["records"] = [dict(seq="2", ms="1000", remote=rx.SENDER,
                    acquisition="a", attempt="b", page="c", crc="d")]
            return b
        h.snapshot = snapshot
        state = {}
        summary = rx.stage(h, "baseline", 45, original(), "on", 8,
                           report=lambda *a, **k: None, clock=lambda: now[0],
                           sleep=lambda n: now.__setitem__(0, now[0]+n), state=state)
        self.assertEqual(summary["fresh_pair_rejects"], 1)
        self.assertEqual(now[0], 45)
        self.assertEqual(state["previous"]["aead"]["total"], 2)


if __name__ == "__main__":
    unittest.main()
