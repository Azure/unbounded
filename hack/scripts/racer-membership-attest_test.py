# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
import copy
import argparse
import hashlib
import importlib.util
import json
from pathlib import Path
import unittest
from unittest import mock
import io
import subprocess

spec = importlib.util.spec_from_file_location("attest", Path(__file__).with_name("racer-membership-attest.py"))
a = importlib.util.module_from_spec(spec)
spec.loader.exec_module(a)
CLUSTER = "11111111-1111-4111-8111-111111111111"
UID = "22222222-2222-4222-8222-222222222222"
IMAGE = "example/racer:exact"


def fixture():
    member = dict(node=UID, shares=4, peer_endpoint="10.0.0.1:18082", rails=[], alignment_enabled=True)
    version = dict(cluster=CLUSTER, sequence="9", membership_version="8", content_hash="a" * 64,
                   membership_hash=hashlib.sha256(a.canonical(CLUSTER, [member])).hexdigest())
    marker = dict(metadata=dict(uid="marker", resourceVersion="1"), immutable=True,
                  data=dict(cluster=CLUSTER, state="consumed", version_configmap="racer-version"))
    cm = dict(metadata=dict(uid="version", resourceVersion="2", annotations={a.PREFIX + "installation-uid": "marker"}), data=version)
    node = dict(metadata=dict(name="node-a", uid=UID,
                             annotations={a.PREFIX + "last-admitted-member": json.dumps(member)}))
    ds = [dict(metadata=dict(name=n, uid=n + "-uid")) for n in (*a.DP, a.NET)]

    def pod(name, container):
        return dict(metadata=dict(name=name + "-pod", uid=name + "-pod-uid",
                                  labels={"app.kubernetes.io/name": name},
                                  ownerReferences=[dict(name=name, uid=name + "-uid", kind="DaemonSet", controller=True)]),
                    spec=dict(nodeName="node-a", containers=[dict(name=container, image=IMAGE,
                              ports=[dict(name="diagnostics", containerPort=19090), dict(name="peer", containerPort=18082)])]),
                    status=dict(podIP="10.0.0.1", containerStatuses=[dict(name=container, containerID="container",
                                imageID="sha256:abc", restartCount=0, ready=True, state=dict(running=dict(startedAt="now")))]))
    return dict(marker=marker, version=cm, nodes=[node], ds=ds,
                pods=[pod(a.DP[0], "dataplane"), pod(a.NET, "node")])


def body(v, **updates):
    fields = dict(accepted_sequence=v["sequence"], accepted_membership=v["membership_version"],
                  accepted_membership_hash=v["membership_hash"], pending_sequence="0", pending_membership="0",
                  expected_workers="4", matching_workers="4", fully_applied="1")
    fields.update(updates)
    return " ".join(f"{k}={v}" for k, v in fields.items()) + "\n"


class AttestationTests(unittest.TestCase):
    def test_success(self):
        inv = a.inventory(fixture(), 1, IMAGE)
        results = [dict(pod_uid=inv["targets"][0]["pod_uid"], exit=0, body=body(inv["authority"]["data"]))]
        result = a.attest(inv, copy.deepcopy(inv), results)
        self.assertTrue(result["membership_attested"])
        self.assertFalse(result["controller_replicas_serving_attested"])

    def test_pending_stale_workers_versions_hashes_and_errors(self):
        version = fixture()["version"]["data"]
        for updates in (dict(pending_sequence="10"), dict(pending_membership="9"),
                        dict(fully_applied="0"), dict(matching_workers="3"),
                        dict(matching_workers="0", expected_workers="0"),
                        dict(accepted_sequence="8"), dict(accepted_membership="7"),
                        dict(accepted_membership_hash="b" * 64), dict(expected_workers="04")):
            with self.subTest(updates=updates), self.assertRaises(ValueError):
                a.diagnostic(body(version, **updates), version)
        for text in ("unavailable fully_applied=0", "404 Not Found", "x" * 1025,
                     body(version) + "fully_applied=1"):
            with self.subTest(text=text[:30]), self.assertRaises(ValueError):
                a.diagnostic(text, version)

    def test_absent_duplicate_node_bad_uid_image_owner_binding_hash(self):
        changes = [lambda r: r["nodes"].clear(),
                   lambda r: r["nodes"].append(copy.deepcopy(r["nodes"][0])),
                   lambda r: r["nodes"][0]["metadata"].update(uid="different"),
                   lambda r: r["pods"][0]["spec"]["containers"][0].update(image="old"),
                   lambda r: r["pods"][0]["metadata"]["ownerReferences"][0].update(uid="foreign"),
                   lambda r: r["marker"]["data"].update(state="fresh"),
                   lambda r: r["version"]["data"].update(membership_hash="b" * 64),
                   lambda r: r["pods"][0]["status"].update(podIP="10.0.0.2"),
                   lambda r: r["pods"].pop(0)]
        for change in changes:
            raw = fixture()
            change(raw)
            with self.subTest(change=change), self.assertRaises(ValueError):
                a.inventory(raw, 1, IMAGE)

    def test_uid_restart_and_authority_bracket_drift(self):
        before = a.inventory(fixture(), 1, IMAGE)
        for field, value in (("pod_uid", "replacement"), ("restart_count", 1),
                             ("container_id", "new"), ("image_id", "new"), ("node_uid", "new")):
            after = copy.deepcopy(before)
            after["targets"][0][field] = value
            with self.subTest(field=field), self.assertRaisesRegex(ValueError, "bracket"):
                a.attest(before, after, [])
        after = copy.deepcopy(before)
        after["authority"]["data"]["sequence"] = "10"
        with self.assertRaisesRegex(ValueError, "bracket"):
            a.attest(before, after, [])

    def test_unchanged_cas_resource_versions_are_not_identity_drift(self):
        raw = fixture()
        before = a.inventory(raw, 1, IMAGE)
        raw["version"]["metadata"]["resourceVersion"] = "300"
        self.assertEqual(before, a.inventory(raw, 1, IMAGE))
        raw["version"]["metadata"]["annotations"][a.PREFIX + "installation-uid"] = "other"
        with self.assertRaisesRegex(ValueError, "installation UID"):
            a.inventory(raw, 1, IMAGE)

    def test_command_failure_does_not_echo_stderr_and_has_external_bound(self):
        args = argparse.Namespace(context="test", namespace="test")
        reader = a.Reader(args)
        process = mock.Mock(returncode=1)
        process.communicate.return_value = ("private stdout", "private stderr")
        with mock.patch("subprocess.Popen", return_value=process) as spawn:
            with self.assertRaisesRegex(ValueError, "stderr suppressed") as error:
                reader.command(["kubectl", "get", "nodes"], seconds=2)
            self.assertNotIn("private", str(error.exception))
            self.assertEqual(spawn.call_args.args[0][:4],
                             ["timeout", "--signal=TERM", "--kill-after=10s", "2s"])
        reader.deadline = 0
        with mock.patch("subprocess.Popen") as spawn, self.assertRaisesRegex(ValueError, "deadline"):
            reader.command(["kubectl"])
        spawn.assert_not_called()

    def test_missing_duplicate_and_failed_results(self):
        inv = a.inventory(fixture(), 1, IMAGE)
        row = dict(pod_uid=inv["targets"][0]["pod_uid"], exit=1, body="")
        for results in ([], [row, row], [row], [dict(row, pod_uid="foreign")]):
            with self.subTest(results=results), self.assertRaises(ValueError):
                a.attest(inv, inv, results)

    def test_canonical_go_fixture_exact(self):
        root = Path(__file__).resolve().parents[2]
        directory = root / "internal/racer/wire/testdata"
        # Shared Go/Rust golden documents enforce field order, omitted fields and UTF-8.
        publication = json.loads((directory / "publication.json").read_text())
        expected = (directory / "membership.json").read_bytes().rstrip(b"\n")
        self.assertEqual(a.canonical(publication["cluster"], publication["members"]), expected)

    def test_canonical_unicode_omission_and_order(self):
        m = dict(node=UID, shares=1, peer_endpoint="<>&", alignment_enabled=False, site="",
                 rails=[dict(rail=2, fabric="é\u2028\u2029", numa_node=0), dict(rail=1, fabric="z")])
        encoded = a.canonical(CLUSTER, [m])
        self.assertIn(b"<>&", encoded)
        self.assertIn("é".encode() + b"\\u2028\\u2029", encoded)
        self.assertNotIn(b'"site"', encoded)
        self.assertIn(b'"numa_node":0', encoded)
        self.assertLess(encoded.index(b'"rail":1'), encoded.index(b'"rail":2'))
        with self.assertRaises(ValueError):
            a.decode('{"a":1,"a":2}')

    def test_candidate_map(self):
        raw = fixture()
        raw["nodes"][0]["metadata"]["annotations"][a.PREFIX + "shares"] = "4"
        pairs = [(UID, 4)]
        plan = dict(nodes=[dict(node="node-a", uid=UID, candidate_annotation="4")],
                    map_sha256=hashlib.sha256(json.dumps(pairs, separators=(",", ":")).encode()).hexdigest())
        a.inventory(raw, 1, IMAGE, plan)
        plan["nodes"][0]["candidate_annotation"] = "5"
        with self.assertRaisesRegex(ValueError, "candidate"):
            a.inventory(raw, 1, IMAGE, plan)

    def test_remote_collector_boundaries_and_error_redaction(self):
        rows = [dict(pod_uid=str(i), ip="10.0.0.1", port=19090) for i in range(33)]
        output = io.StringIO()
        def run(argv, **kwargs):
            self.assertEqual(argv[:4], ["timeout", "--signal=TERM", "--kill-after=10s", "4s"])
            self.assertIn("--noproxy", argv)
            self.assertEqual(argv[argv.index("--max-time") + 1], "2")
            self.assertEqual(argv[argv.index("--max-filesize") + 1], "1024")
            self.assertEqual(kwargs["timeout"], 15)
            return subprocess.CompletedProcess(argv, 22, stdout="", stderr="private error")
        with mock.patch("sys.stdin", io.StringIO(json.dumps(rows))), mock.patch("sys.stdout", output), \
                mock.patch("subprocess.run", side_effect=run):
            exec(a.REMOTE, {})
        records = [json.loads(line) for line in output.getvalue().splitlines()]
        self.assertEqual(len(records), 33)
        self.assertEqual({r["pod_uid"] for r in records}, {r["pod_uid"] for r in rows})
        self.assertTrue(all(r["exit"] == 22 for r in records))
        self.assertNotIn("private", output.getvalue())


if __name__ == "__main__":
    unittest.main()
