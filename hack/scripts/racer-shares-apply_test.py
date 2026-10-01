# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
import copy
import importlib.util
import json
from pathlib import Path
import signal
import subprocess
import threading
import time
import unittest
from unittest.mock import patch, Mock

spec = importlib.util.spec_from_file_location("shares", Path(__file__).with_name("racer-shares-apply.py"))
s = importlib.util.module_from_spec(spec)
spec.loader.exec_module(s)


def row(original=None):
    return dict(node="node-a", uid="uid-a", original_annotation=original,
                candidate_annotation="400", shares=4 if original is None else 1,
                rollback=dict(operation="remove" if original is None else "set", value=original))


def node(value=None, rv="1"):
    ann = {s.ENROLLED: "4", "unrelated": "preserve"}
    if value is not None:
        ann[s.KEY] = value
    return dict(metadata=dict(name="node-a", uid="uid-a", resourceVersion=rv, annotations=ann))


class Fake:
    def __init__(self, live=None):
        self.live = live or node()
        self.stop = threading.Event()
        self.deadline = time.monotonic() + 100
        self.events = []
        self.patches = []
        self.failures = []

    def cancel(self):
        self.stop.set()

    def note(self, event, **fields):
        self.events.append((event, fields))

    def get_node(self, row):
        return copy.deepcopy(self.live)

    def patch(self, row, operations):
        self.patches.append(operations)
        if self.failures:
            self.failures.pop(0)(self)
            raise s.CommandError("ambiguous")
        op = operations[-1]
        if op["op"] == "remove":
            del self.live["metadata"]["annotations"][s.KEY]
        else:
            self.live["metadata"]["annotations"][s.KEY] = op["value"]
        self.live["metadata"]["resourceVersion"] = "20"
        return copy.deepcopy(self.live)


class StateTests(unittest.TestCase):
    def test_absent_add_has_uid_rv_not_null_test(self):
        p = s.patch_for(row(), node(), "apply")
        self.assertEqual(p, [dict(op="test", path="/metadata/uid", value="uid-a"),
                             dict(op="test", path="/metadata/resourceVersion", value="1"),
                             dict(op="add", path=s.PTR, value="400")])

    def test_apply_and_rollback_absence_and_one(self):
        for original in (None, "1"):
            f = Fake(node(original))
            s.apply_node(f, row(original), "apply")
            self.assertEqual(f.live["metadata"]["annotations"][s.KEY], "400")
            s.apply_node(f, row(original), "rollback")
            self.assertEqual(f.live["metadata"]["annotations"].get(s.KEY), original)
            self.assertEqual(f.live["metadata"]["annotations"]["unrelated"], "preserve")
            self.assertEqual(f.patches[-1][-1]["op"], "remove" if original is None else "replace")

    def test_partial_resume_skips_both_directions(self):
        f = Fake(node("400"))
        s.apply_node(f, row(), "apply")
        self.assertEqual(f.patches, [])
        f.live = node()
        s.apply_node(f, row(), "rollback")
        self.assertEqual(f.patches, [])

    def test_unsafe_states_rejected_even_for_skip(self):
        for mutate in (lambda n: n["metadata"].update(uid="other"),
                       lambda n: n["metadata"].update(deletionTimestamp="now"),
                       lambda n: n["metadata"].update(labels={s.EXCLUDE: ""}),
                       lambda n: n["metadata"]["annotations"].update({s.ENROLLED: "8"}),
                       lambda n: n["metadata"]["annotations"].update({s.KEY: None}),
                       lambda n: n["metadata"]["annotations"].update({s.KEY: "999"})):
            live = node("400")
            mutate(live)
            with self.assertRaises(ValueError):
                s.inspect(row(), live, "apply")

    def test_ambiguous_success_does_not_repeat(self):
        f = Fake()
        f.failures = [lambda f: f.live["metadata"]["annotations"].update({s.KEY: "400"})]
        s.apply_node(f, row(), "apply")
        self.assertEqual(len(f.patches), 1)
        self.assertEqual(f.events[-1][0], "desired_after_ambiguous")

    def test_changed_rv_retry_rebuilds_patch(self):
        f = Fake()
        f.failures = [lambda f: f.live["metadata"].update(resourceVersion="2")]
        s.apply_node(f, row(), "apply")
        self.assertEqual(len(f.patches), 2)
        self.assertEqual(f.patches[1][1]["value"], "2")

    def test_retry_unchanged_drift_and_exhaustion_stop(self):
        cases = [[lambda f: None],
                 [lambda f: f.live["metadata"]["annotations"].update({s.KEY: "999"})],
                 [lambda f: f.live["metadata"].update(uid="replacement", resourceVersion="2")],
                 [lambda f: f.live["metadata"].update(resourceVersion=str(int(f.live["metadata"]["resourceVersion"]) + 1))] * 3]
        for actions in cases:
            f = Fake()
            f.failures = actions
            with self.assertRaises(ValueError):
                s.apply_node(f, row(), "apply")
            self.assertLessEqual(len(f.patches), 3)

    def test_dynamic_queue_stops_after_failure(self):
        f = Fake()
        calls = []

        def fail(runner, item, direction):
            calls.append(item)
            raise ValueError("failure")

        with patch.object(s, "apply_node", fail), self.assertRaises(ValueError):
            s.dispatch(f, list(range(100)), "apply", workers=1)
        self.assertEqual(calls, [0])

    def test_dynamic_queue_bound_and_success(self):
        f = Fake()
        lock = threading.Lock()
        active = [0, 0]

        def work(*args):
            with lock:
                active[0] += 1
                active[1] = max(active)
            time.sleep(.001)
            with lock:
                active[0] -= 1

        with patch.object(s, "apply_node", work):
            self.assertEqual(s.dispatch(f, list(range(50)), "apply"), 50)
        self.assertLessEqual(active[1], 16)

    def test_deadline_blocks_dispatch(self):
        f = Fake()
        f.deadline = 0
        with patch.object(s, "apply_node") as work, self.assertRaises(ValueError):
            s.dispatch(f, [row()], "apply")
        work.assert_not_called()


def control(rows, concurrency="0"):
    return dict(metadata=dict(uid="cm-uid"), data={"concurrency": concurrency, "node-caps.json": json.dumps({
        "version": 1, "caps": {r["node"]: 1 for r in rows if r["shares"] == 1}})})


def vector(names, value=0):
    return dict(status="success", data=dict(resultType="vector", result=[
        dict(metric=dict(node=n), value=[1, str(value)]) for n in names]))


class GateTests(unittest.TestCase):
    def setUp(self):
        self.rows = [dict(row("1"), node=f"bad-{i}") for i in range(11)] + [row()]

    def test_c0_and_exact_caps(self):
        cm = control(self.rows)
        self.assertEqual(s.control_identity(cm, self.rows)[0], "cm-uid")
        for value in ("8", "", "-1"):
            with self.assertRaises(ValueError):
                s.control_identity(control(self.rows, value), self.rows)
        cm["data"]["node-caps.json"] = '{"version":1,"caps":{}}'
        with self.assertRaises(ValueError):
            s.control_identity(cm, self.rows)

    def test_missing_nonzero_nonfinite_duplicate_and_warning(self):
        for payload in (vector([]), vector(["a"], 1), vector(["a"], "NaN"),
                        vector(["a", "a"]), dict(vector(["a"]), warnings=["partial"])):
            with self.assertRaises(ValueError):
                s.coverage(payload, {"a"}, 0)
        s.coverage(vector(["a"]), {"a"}, 0)
        q = s.drain_query(*s.GAUGES[-1])
        for token in ("max_over_time", "[255s]", "count_over_time", ">= 4", "[75s] offset 180s", "timestamp", "time()-75"):
            self.assertIn(token, q)
        self.assertNotIn("or 0", q)
        self.assertEqual(len(s.GAUGES), 8)

    def test_sixty_second_scrape_phase_and_full_three_minute_history(self):
        # Independent scalar oracle for the range/count/freshness/early clauses.
        # Ages are seconds before evaluation; Prometheus ranges are (left,right].
        def accepts(samples):
            history = [(age, value) for age, value in samples if 0 <= age < 255]
            return (len(history) >= 4 and max(v for _, v in history) == 0
                    and min(age for age, _ in history) <= 75
                    and any(180 <= age < 255 for age, _ in history))

        for phase in range(60):
            samples = [(phase + 60 * i, 0) for i in range(5)]
            with self.subTest(phase=phase):
                self.assertTrue(accepts(samples))
                # A sample preceding the 180s boundary is included in the zero
                # check, preventing an unobserved leading gap from passing.
                bad = [(age, 1 if 180 <= age < 255 else value) for age, value in samples]
                self.assertFalse(accepts(bad))
        self.assertTrue(accepts([(14, 0), (74, 0), (134, 0), (194, 0), (254, 0)]))
        self.assertTrue(accepts([(0, 0), (60, 0), (120, 0), (180, 0)]))
        self.assertTrue(accepts([(75, 0), (130, 0), (190, 0), (250, 0)]))
        self.assertFalse(accepts([(76, 0), (130, 0), (190, 0), (250, 0)]))
        self.assertFalse(accepts([(0, 0), (60, 0), (120, 0), (255, 0)]))
        self.assertFalse(accepts([(0, 0), (60, 0), (120, 0)]))
        self.assertFalse(accepts([]))
        # Both zero gauges and up health gates use the expanded history.
        health = s.drain_query("up", "racer-dataplane", False)
        self.assertIn("min_over_time", health)
        self.assertIn("[255s]", health)
        self.assertIn("== 1", health)

    def test_gate_all_gauges_and_up(self):
        f = Fake()
        f.control = lambda: control(self.rows)
        names = {r["node"] for r in self.rows}
        queries = []

        def query(expression, stamp):
            queries.append((expression, stamp))
            return vector(names, 1 if "min_over_time(up{" in expression else 0)

        f.query = query
        s.gate(f, self.rows)
        self.assertEqual(len(queries), 10)
        self.assertEqual(len({x[1] for x in queries}), 1)

    def test_whole_phase_preflight_before_writes(self):
        f = Fake()
        f.control = lambda: control(self.rows)
        f.command = lambda args: {"items": [node("999")]}
        with patch.object(s, "gate", return_value=s.control_identity(control(self.rows), self.rows)), \
                patch.object(s, "dispatch") as write, self.assertRaises(ValueError):
            s.execute(f, self.rows, [row()], "apply")
        write.assert_not_called()

    def test_c0_rechecked_after_preflight(self):
        f = Fake()
        f.control = lambda: control(self.rows, "8")
        f.command = lambda args: {"items": [node()]}
        with patch.object(s, "gate", return_value=s.control_identity(control(self.rows), self.rows)), \
                patch.object(s, "dispatch") as write, self.assertRaises(ValueError):
            s.execute(f, self.rows, [row()], "apply")
        write.assert_not_called()

    def test_drain_failure_prevents_preflight_and_writes(self):
        f = Fake()
        f.control = lambda: control(self.rows)
        f.query = lambda *args: vector([])
        with patch.object(s, "dispatch") as write, self.assertRaises(ValueError):
            s.execute(f, self.rows, [row()], "apply")
        write.assert_not_called()

    def test_control_bracket_detects_uid_or_caps_change(self):
        f = Fake()
        values = [control(self.rows), control(self.rows)]
        values[-1]["metadata"]["uid"] = "replacement"
        f.control = lambda: values.pop(0)
        names = {r["node"] for r in self.rows}
        f.query = lambda q, stamp: vector(names, 1 if "min_over_time(up{" in q else 0)
        with self.assertRaisesRegex(ValueError, "control changed"):
            s.gate(f, self.rows)


class CommandTests(unittest.TestCase):
    def runner(self):
        r = object.__new__(s.Runner)
        r.context = s.CONTEXT
        r.lock = threading.RLock()
        r.stop = threading.Event()
        r.processes = set()
        r.deadline = time.monotonic() + 220
        return r

    def test_external_timeout_and_group_cleanup(self):
        r = self.runner()
        process = Mock(pid=123, returncode=0)
        process.communicate.return_value = ('{"ok":true}', "private")
        with patch.object(s.subprocess, "Popen", return_value=process) as spawn, patch.object(s.os, "killpg") as kill:
            self.assertEqual(r.command(["get", "nodes", "-o", "json"]), {"ok": True})
            self.assertEqual(spawn.call_args.args[0][:4], ["timeout", "--signal=TERM", "--kill-after=10s", "10s"])
            self.assertTrue(spawn.call_args.kwargs["start_new_session"])
            kill.assert_called_with(123, signal.SIGKILL)
            self.assertFalse(r.processes)

    def test_timeout_cleanup_and_cancellation(self):
        r = self.runner()
        process = Mock(pid=123, returncode=0)
        process.communicate.side_effect = [subprocess.TimeoutExpired("kubectl", 21), ("", "")]
        with patch.object(s.subprocess, "Popen", return_value=process), patch.object(s.os, "killpg") as kill:
            with self.assertRaises(subprocess.TimeoutExpired):
                r.command(["get", "node", "a"])
            kill.assert_called_with(123, signal.SIGKILL)
        r.processes.add(process)
        with patch.object(s.os, "killpg") as kill:
            r.cancel()
            kill.assert_called_with(123, signal.SIGTERM)
        with patch.object(s.subprocess, "Popen") as spawn, self.assertRaises(ValueError):
            r.command(["get", "nodes"])
        spawn.assert_not_called()

    def test_signal_during_spawn_cleans_registered_group(self):
        r = self.runner()
        process = Mock(pid=123, returncode=1)
        process.communicate.return_value = ("", "")

        def spawn(*args, **kwargs):
            r.cancel()
            return process

        with patch.object(s.subprocess, "Popen", side_effect=spawn), patch.object(s.os, "killpg") as kill:
            with self.assertRaises(s.CommandError):
                r.command(["get", "nodes"])
            self.assertIn(unittest.mock.call(123, signal.SIGTERM), kill.call_args_list)
            self.assertIn(unittest.mock.call(123, signal.SIGKILL), kill.call_args_list)
        self.assertFalse(r.processes)

    def test_failure_redacts_and_fresh_state_required(self):
        r = self.runner()
        process = Mock(pid=123, returncode=1)
        process.communicate.return_value = ("secret", "credential")
        with patch.object(s.subprocess, "Popen", return_value=process), patch.object(s.os, "killpg"):
            with self.assertRaisesRegex(s.CommandError, "suppressed"):
                r.command(["get", "nodes"])
        with self.assertRaises(ValueError):
            s.Runner(Path(__file__).parent, s.CONTEXT)

    def test_plan_authorization_and_bounds(self):
        with self.assertRaises(ValueError):
            s.plan_rows({"map_sha256": s.SHA}, "wrong", 0, 500)
        # Optional existing artifact, never a cluster read.
        root = Path(__file__).resolve().parents[2]
        parent = next((p for p in root.parents if (p / ".git").is_dir()), root)
        path = parent / "tmp/racer-weight-plan-artifacts/plan.json"
        if path.exists():
            plan = s.decode(path.read_text())
            self.assertEqual(len(s.plan_rows(plan, s.SHA, 500, 500)[1]), 500)
            for start, count in ((0, 501), (-1, 1), (1499, 2), (0, 0)):
                with self.assertRaises(ValueError):
                    s.plan_rows(plan, s.SHA, start, count)
            bad = copy.deepcopy(plan)
            bad["nodes"][0].update(shares=1, original_annotation="1", rollback={"operation": "set", "value": "1"})
            with self.assertRaisesRegex(ValueError, "original shares hash"):
                s.plan_rows(bad, s.SHA, 0, 500)
            plan["nodes"][0]["candidate_annotation"] = "1"
            with self.assertRaises(ValueError):
                s.plan_rows(plan, s.SHA, 0, 500)


if __name__ == "__main__":
    unittest.main()
