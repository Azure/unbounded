import copy
import hashlib
import json
from pathlib import Path
import tempfile
import unittest

import yaml

from peer_cap import CM, KEY, NS, TARGETS, dataplane, guarded_patch
from routing_v5 import ALGORITHM, BASELINE, IMAGE, prepare, rollback_data, transform, verify_live
from test_peer_cap import fixture as peer_fixture


def fixture():
    current = peer_fixture()
    current["data"][KEY] = current["data"][KEY].replace("example@sha256:unchanged", IMAGE).replace(
        "            env:\n", f"            env:\n            - name: {ALGORITHM}\n              value: '3' # routing\n")
    config = {"metadata": {"uid": "config"}, "data": dict(BASELINE)}
    workloads = [{"metadata": {"name": e["name"], "uid": e["name"]}, "spec": copy.deepcopy(e["patch"]["spec"])}
                 for e in yaml.safe_load(current["data"][KEY])["overrides"]]
    for ds in workloads:
        dataplane(ds["spec"])["envFrom"] = [{"configMapRef": {"name": "racer-dataplane-config"}}]
    return current, config, workloads


def apply_patch(current, patch):
    result = copy.deepcopy(current)
    for operation in patch:
        keys = operation["path"].strip("/").split("/")
        parent = result
        for key in keys[:-1]:
            parent = parent[key]
        if operation["op"] == "test":
            if parent[keys[-1]] != operation["value"]:
                raise ValueError("stale guard")
        else:
            parent[keys[-1]] = operation["value"]
    return result


class FakeRunner:
    def __init__(self, state, current, config, workloads, drift=False, stale=False, bad_server=False):
        self.state = state
        self.current, self.config, self.workloads = current, config, workloads
        self.calls = []
        self.drift, self.stale, self.bad_server = drift, stale, bad_server

    def get(self, kind, name):
        self.calls.append((kind, name))
        if name == CM:
            return copy.deepcopy(self.current)
        if name == "racer-dataplane-config":
            result = copy.deepcopy(self.config)
            if self.drift and len(self.calls) > 4:
                result["data"]["OTHER"] = "drift"
            return result
        return copy.deepcopy(next(ds for ds in self.workloads if ds["metadata"]["name"] == name))

    def kubectl(self, *args):
        self.calls.append(args)
        assert "--dry-run=server" in args
        patch = json.loads((self.state / "patch.json").read_text())
        current = copy.deepcopy(self.current)
        if self.stale:
            current["metadata"]["resourceVersion"] = "stale"
        result = apply_patch(current, patch)
        if self.bad_server:
            result["data"][KEY] += "# admission drift\n"
        return json.dumps(result)

    def note(self, text):
        pass


class RoutingTests(unittest.TestCase):
    def test_only_two_digits_change_with_quotes_comments_unicode_crlf_and_aliases(self):
        for quote, newline in (("'", "\n"), ('"', "\r\n")):
            before = fixture()[0]["data"]
            before[KEY] = ("# unicode: café\n" + before[KEY]).replace("'3'", quote + "3" + quote).replace("\n", newline)
            original = copy.deepcopy(before)
            after = transform(before)
            differences = [(a, b) for a, b in zip(before[KEY].encode(), after[KEY].encode()) if a != b]
            self.assertEqual(differences, [(ord("3"), ord("5"))] * 2)
            self.assertEqual(len(before[KEY].encode()), len(after[KEY].encode()))
            self.assertEqual(after["other.yaml"], before["other.yaml"])
            self.assertEqual(before, original)
            entries = yaml.safe_load(after[KEY])["overrides"]
            self.assertIs(dataplane(entries[0]["patch"]["spec"])["env"][1],
                          dataplane(entries[1]["patch"]["spec"])["env"][1])

    def test_reject_missing_duplicate_indirect_aliased_or_already_changed_scalar(self):
        text = fixture()[0]["data"][KEY]
        cases = [text.replace(TARGETS[1], TARGETS[0]), text.replace(TARGETS[1], "unknown"),
                 text.replace(ALGORITHM, "OTHER_ROUTING"), text.replace("'3'", "'5'"),
                 text.replace("'3'", "3"), text.replace("value: '3'", "valueFrom: {}"),
                 text.replace("value: '3'", "value: '3'\n              value: '3'"),
                 text.replace("value: '3'", "value: &routing '3'", 1),
                 text.replace("value: '3'", "value: &routing '3'", 1).replace("value: '3'", "value: *routing"),
                 text.replace("            env:", "            <<: {}\n            env:")]
        for case in cases:
            with self.subTest(case=case), self.assertRaises(ValueError):
                transform({KEY: case})

    def test_reject_routing_entry_alias_with_non_target_consumer(self):
        text = fixture()[0]["data"][KEY].replace(
            f"- name: {ALGORITHM}", f"- &routing_entry\n              name: {ALGORITHM}", 1)
        text += "  non_target_consumer: *routing_entry\n"
        with self.assertRaisesRegex(ValueError, "another alias consumer"):
            transform({KEY: text})

    def test_identity_rv_full_data_tests_are_atomic_and_rollback_uses_fresh_rv(self):
        before = fixture()[0]
        desired = transform(before["data"])
        patch = guarded_patch(before, desired)
        self.assertEqual([p["path"] for p in patch],
                         ["/metadata/uid", "/metadata/resourceVersion", "/data", "/data/" + KEY])
        forward = apply_patch(before, patch)
        forward["metadata"]["resourceVersion"] = "99"
        inverse = guarded_patch(forward, rollback_data(forward, before))
        self.assertEqual(inverse[1]["value"], "99")
        self.assertEqual(apply_patch(forward, inverse)["data"], before["data"])
        for field in ("uid", "resourceVersion", "data"):
            stale = copy.deepcopy(before)
            if field == "data":
                stale["data"]["other.yaml"] += "# changed\n"
            else:
                stale["metadata"][field] = "changed"
            original = copy.deepcopy(stale)
            with self.subTest(field=field), self.assertRaises(ValueError):
                apply_patch(stale, patch)
            self.assertEqual(stale, original)
        for mutation in ("identity", "data", "not-applied"):
            stale = copy.deepcopy(forward)
            if mutation == "identity":
                stale["metadata"]["uid"] = "new"
            elif mutation == "data":
                stale["data"]["other.yaml"] += "# changed\n"
            else:
                stale["data"] = before["data"]
            with self.assertRaises(ValueError):
                rollback_data(stale, before)
        for key in ("name", "namespace"):
            wrong = copy.deepcopy(before)
            wrong["metadata"][key] = "wrong"
            with self.assertRaises(ValueError):
                guarded_patch(wrong, desired)

    def test_live_guards_and_partial_rollback(self):
        current, config, workloads = fixture()
        verify_live(current, config, workloads)
        dataplane(workloads[0]["spec"])["env"][0]["value"] = "5"
        verify_live(current, config, workloads, rollback=True)
        with self.assertRaises(ValueError):
            verify_live(current, config, workloads)
        for mutation in ("image", "envFrom", "command", "cap", "duplicate", "competing"):
            current, config, workloads = fixture()
            c = dataplane(workloads[0]["spec"])
            if mutation == "cap":
                config["data"]["RACER_CONNECTIONS_PER_NEIGHBOR"] = "4"
            elif mutation == "duplicate":
                c["env"].append(copy.deepcopy(c["env"][0]))
            elif mutation == "competing":
                current["data"]["other.yaml"] = current["data"][KEY]
            else:
                c[mutation] = "unexpected"
            with self.subTest(mutation=mutation), self.assertRaises(ValueError):
                verify_live(current, config, workloads)

    def test_plan_hash_server_dry_run_failure_and_fresh_rollback(self):
        # Never write test artifacts outside the assigned project boundary.
        root = Path(__file__).resolve().parents[3] / "tmp"
        root.mkdir(exist_ok=True)
        for mode in ("success", "drift", "stale", "bad_server"):
            with tempfile.TemporaryDirectory(dir=root) as directory:
                state = Path(directory)
                before, config, workloads = fixture()
                r = FakeRunner(state, before, config, workloads, **({mode: True} if mode != "success" else {}))
                if mode != "success":
                    with self.assertRaises(ValueError):
                        prepare(r)
                    self.assertFalse((state / "ready.json").exists())
                    continue
                prepare(r)
                for line in (state / "SHA256SUMS").read_text().splitlines():
                    digest, name = line.split("  ")
                    self.assertEqual(hashlib.sha256((state / name).read_bytes()).hexdigest(), digest)
                self.assertFalse(json.loads((state / "ready.json").read_text())["applied"])
                repeat = state / "repeat"
                repeat.mkdir()
                prepare(FakeRunner(repeat, before, config, workloads))
                self.assertEqual((state / "patch.json").read_bytes(), (repeat / "patch.json").read_bytes())
                self.assertEqual((state / "SHA256SUMS").read_bytes(), (repeat / "SHA256SUMS").read_bytes())
                after = apply_patch(before, json.loads((state / "patch.json").read_text()))
                after["metadata"]["resourceVersion"] = "999"
                rollback = state / "rollback"
                rollback.mkdir()
                prepare(FakeRunner(rollback, after, config, workloads), state)
                inverse = json.loads((rollback / "patch.json").read_text())
                self.assertEqual(inverse[1]["value"], "999")
                self.assertEqual(apply_patch(after, inverse)["data"], before["data"])


if __name__ == "__main__":
    unittest.main()
