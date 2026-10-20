# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

import copy
import json
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "reference" / "rev24"))
sys.path.insert(0, str(ROOT / "reference" / "rev24" / "tests"))
sys.path.insert(0, str(ROOT))

import evidence_protocol as core
import rev25_protocol as rev25
from state import SQLiteState, StateError
from test_rev24_protocol import BINDING, NOW
from test_rev25_protocol import Rev25Fixture


MANIFEST = ROOT / "coordinator-attacks.json"


def stale_bundle(fixture, bundle, stale_freshness):
    stale = copy.deepcopy(bundle)
    stale["entries"] = stale["entries"][:-1]
    head = stale["entries"][-1]["treeHead"]
    signatures = [
        witness.sign_log(
            BINDING,
            stale["entries"],
            fixture.admission,
            fixture.challenge,
            head,
        )
        for witness in fixture.witnesses[:2]
    ]
    stale["checkpoint"] = core.make_checkpoint(
        BINDING, stale["entries"], signatures
    )
    if stale_freshness:
        statement = copy.deepcopy(stale["freshness"]["statement"])
        statement["latestIndex"] = len(stale["entries"]) - 1
        statement["latestTreeHead"] = head
        stale["freshness"] = rev25.sign_authority_statement(
            statement, fixture.root
        )
    return stale


class Rev357CoordinatorAttackTests(unittest.TestCase):
    def test_manifest_names_exact_attack_suite(self):
        manifest = json.loads(MANIFEST.read_text(encoding="utf-8"))
        self.assertFalse(
            manifest["provenance"]["originalArtifactAvailable"]
        )
        self.assertEqual(
            [attack["id"] for attack in manifest["coordinatorAttacks"]],
            [f"C{index:02d}" for index in range(1, 11)],
        )
        self.assertEqual(
            [attack["id"] for attack in manifest["reviewerAttacks"]],
            ["R01", "R02"],
        )
        methods = {name for name in dir(self) if name.startswith("test_")}
        for attack in (
            manifest["coordinatorAttacks"]
            + manifest["reviewerAttacks"]
        ):
            self.assertIn(attack["test"], methods)

    def test_c01_self_admitted_authority_root_rejected(self):
        trusted = Rev25Fixture()
        attacker = Rev25Fixture()
        with self.assertRaises(core.EvidenceError):
            trusted.verifier.verify_recovery(attacker.bundle(), NOW)

    def test_c02_caller_ledger_revision_rejected(self):
        fixture = Rev25Fixture()
        bundle = fixture.bundle()
        fixture.observe(bundle)
        statement = copy.deepcopy(bundle["admission"]["statement"])
        statement["ledgerRevision"] += 1
        bundle["admission"] = rev25.sign_authority_statement(
            statement, fixture.root
        )
        with self.assertRaises(core.EvidenceError):
            fixture.verifier.verify_recovery(bundle, NOW)

    def test_c03_caller_ledger_digest_rejected(self):
        fixture = Rev25Fixture()
        bundle = fixture.bundle()
        fixture.observe(bundle)
        statement = copy.deepcopy(bundle["admission"]["statement"])
        statement["ledgerSHA256"] = "f" * 64
        bundle["admission"] = rev25.sign_authority_statement(
            statement, fixture.root
        )
        with self.assertRaises(core.EvidenceError):
            fixture.verifier.verify_recovery(bundle, NOW)

    def test_c04_stale_prefix_checkpoint_rejected(self):
        fixture = Rev25Fixture()
        bundle = fixture.bundle()
        fixture.observe(bundle)
        with self.assertRaises(core.EvidenceError):
            fixture.verifier.verify_recovery(
                stale_bundle(fixture, bundle, False), NOW
            )

    def test_c05_withheld_suffix_after_latest_advance_rejected(self):
        fixture = Rev25Fixture()
        bundle = fixture.bundle()
        fixture.observe(bundle)
        with self.assertRaises(rev25.Rev25Error):
            fixture.verifier.verify_recovery(
                stale_bundle(fixture, bundle, True), NOW
            )

    def test_c06_latest_rollback_rejected(self):
        fixture = Rev25Fixture()
        bundle = fixture.bundle()
        fixture.observe(bundle)
        current = fixture.state.latest(rev25._stream_id(BINDING))
        with self.assertRaises(StateError):
            fixture.state.observe_latest(
                rev25._stream_id(BINDING),
                current["latestIndex"] - 1,
                current["latestTreeHead"],
                NOW + 1,
            )

    def test_c07_same_index_fork_rejected(self):
        fixture = Rev25Fixture()
        bundle = fixture.bundle()
        fixture.observe(bundle)
        current = fixture.state.latest(rev25._stream_id(BINDING))
        with self.assertRaises(StateError):
            fixture.state.observe_latest(
                rev25._stream_id(BINDING),
                current["latestIndex"],
                "f" * 64,
                NOW + 1,
            )

    def test_c08_cross_stream_split_view_rejected(self):
        fixture = Rev25Fixture()
        bundle = fixture.bundle()
        other = copy.deepcopy(bundle["binding"])
        other["runID"] = "split-view-run"
        with self.assertRaises(rev25.Rev25Error):
            fixture.verifier.observe_latest(
                other, bundle["freshness"], NOW
            )

    def test_c09_ack_replay_after_restart_rejected(self):
        path = tempfile.mktemp()
        state = SQLiteState(path)
        state.consume_ack("one-use", {"proof": 1})
        state.close()
        restarted = SQLiteState(path)
        with self.assertRaises(StateError):
            restarted.consume_ack("one-use", {"proof": 2})

    def test_c10_pre_challenge_consumption_rejected(self):
        fixture = Rev25Fixture()
        bundle = fixture.bundle()
        fixture.observe(bundle)
        bundle["entries"][1]["intentProof"]["consumption"][
            "consumedAtEpoch"
        ] = NOW - 11
        with self.assertRaises(rev25.Rev25Error):
            fixture.verifier.verify_recovery(bundle, NOW)

    def test_r01_witness_restart_equivocation_rejected(self):
        path = tempfile.mktemp()
        state = SQLiteState(path)
        state.record_witness_head("statement", "a" * 64)
        state.close()
        restarted = SQLiteState(path)
        with self.assertRaises(StateError):
            restarted.record_witness_head("statement", "b" * 64)

    def test_r02_malformed_validated_success_rejected(self):
        fixture = Rev25Fixture()
        bundle = fixture.bundle()
        fixture.observe(bundle)
        event = next(
            entry
            for entry in bundle["entries"]
            if entry["leaf"]["event"] == "PATCH_VALIDATED_SUCCESS"
        )
        event["leaf"]["payload"]["resourceVersion"] = "0"
        with self.assertRaises(core.EvidenceError):
            fixture.verifier.verify_recovery(bundle, NOW)


if __name__ == "__main__":
    unittest.main()
