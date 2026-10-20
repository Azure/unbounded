# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

import copy
import os
import sys
import tempfile
import threading
import time
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "reference" / "rev24"))
sys.path.insert(0, str(ROOT / "reference" / "rev24" / "tests"))
sys.path.insert(0, str(ROOT))

import evidence_protocol as core
import rev25_protocol as rev25
import test_rev24_protocol as rev24_tests
from verify import receipt_authorization_id, require_terminal_success
from state import SQLiteState, StateError
from test_rev24_protocol import BINDING, Fixture, NOW, SUCCESS


class Rev25Fixture(Fixture):
    def __init__(self, path=None):
        super().__init__()
        self.path = path or tempfile.mktemp()
        self.state = SQLiteState(self.path)
        self.resolver = rev25.TrustedResolver(
            core.public_bytes(self.root), 355, "a" * 64
        )
        self.verifier = rev25.Verifier(self.resolver, self.state)

    def bundle(self, events=SUCCESS):
        entries, checkpoint, freshness = self.execute(events)
        return {
            "binding": copy.deepcopy(BINDING),
            "entries": entries,
            "checkpoint": checkpoint,
            "freshness": freshness,
            "admission": self.admission_envelope,
            "deployment": self.deployment_envelope,
            "challenge": self.challenge_envelope,
        }

    def observe(self, bundle):
        self.verifier.observe_latest(
            bundle["binding"], bundle["freshness"], NOW
        )


class Rev25ProtocolTests(unittest.TestCase):
    def test_valid_recovery_requires_separate_observation(self):
        fixture = Rev25Fixture()
        bundle = fixture.bundle()
        with self.assertRaises(StateError):
            fixture.verifier.verify_recovery(bundle, NOW)
        fixture.observe(bundle)
        result = fixture.verifier.verify_recovery(bundle, NOW)
        self.assertEqual(result["patchApplied"], "YES")
        self.assertEqual(result["terminalOutcome"], "PASS")

    def test_caller_cannot_supply_authority_anchor(self):
        fixture = Rev25Fixture()
        bundle = fixture.bundle()
        fixture.observe(bundle)
        bundle["admission"]["statement"]["ledgerRevision"] = 999
        with self.assertRaises(core.EvidenceError):
            fixture.verifier.verify_recovery(bundle, NOW)

    def test_stale_prefix_and_withheld_suffix_rejected_after_advance(self):
        fixture = Rev25Fixture()
        latest = fixture.bundle(SUCCESS)
        fixture.observe(latest)
        stale = copy.deepcopy(latest)
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
        stale_statement = copy.deepcopy(stale["freshness"]["statement"])
        stale_statement["latestIndex"] = len(stale["entries"]) - 1
        stale_statement["latestTreeHead"] = head
        stale["freshness"] = rev25.sign_authority_statement(
            stale_statement, fixture.root
        )
        with self.assertRaises(rev25.Rev25Error):
            fixture.verifier.verify_recovery(stale, NOW)

    def test_latest_rollback_and_fork_rejected(self):
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
        with self.assertRaises(StateError):
            fixture.state.observe_latest(
                rev25._stream_id(BINDING),
                current["latestIndex"],
                "f" * 64,
                NOW + 1,
            )

    def test_ack_consumption_survives_restart(self):
        path = tempfile.mktemp()
        state = SQLiteState(path)
        state.consume_ack("one-use", {"proof": 1})
        restarted = SQLiteState(path)
        with self.assertRaises(StateError):
            restarted.consume_ack("one-use", {"proof": 2})

    def test_witness_equivocation_survives_restart(self):
        path = tempfile.mktemp()
        state = SQLiteState(path)
        state.record_witness_head("witness-statement", "a" * 64)
        restarted = SQLiteState(path)
        with self.assertRaises(StateError):
            restarted.record_witness_head("witness-statement", "b" * 64)

    def test_pre_challenge_consumption_rejected(self):
        fixture = Rev25Fixture()
        bundle = fixture.bundle()
        fixture.observe(bundle)
        bundle["entries"][1]["intentProof"]["consumption"][
            "consumedAtEpoch"
        ] = NOW - 11
        with self.assertRaises(rev25.Rev25Error):
            fixture.verifier.verify_recovery(bundle, NOW)

    def test_stale_freshness_rejected(self):
        fixture = Rev25Fixture()
        bundle = fixture.bundle()
        bundle["freshness"]["statement"]["observedAtEpoch"] = (
            NOW - rev25.MAX_FRESHNESS_AGE_SECONDS - 1
        )
        bundle["freshness"] = rev25.sign_authority_statement(
            bundle["freshness"]["statement"], fixture.root
        )
        with self.assertRaises(rev25.Rev25Error):
            fixture.observe(bundle)

    def test_boolean_latest_index_rejected(self):
        fixture = Rev25Fixture()
        bundle = fixture.bundle()
        bundle["freshness"]["statement"]["latestIndex"] = True
        bundle["freshness"] = rev25.sign_authority_statement(
            bundle["freshness"]["statement"], fixture.root
        )
        with self.assertRaises(rev25.Rev25Error):
            fixture.observe(bundle)

    def test_duplicate_json_keys_and_non_finite_numbers_rejected(self):
        for encoded in (
            '{"binding":{},"binding":{}}',
            '{"value":NaN}',
            '{"value":Infinity}',
        ):
            with self.assertRaises(rev25.Rev25Error):
                rev25.strict_loads(encoded)

    def test_observer_cannot_cross_bind_signed_freshness(self):
        fixture = Rev25Fixture()
        bundle = fixture.bundle()
        other = copy.deepcopy(bundle["binding"])
        other["runID"] = "other-run"
        with self.assertRaises(rev25.Rev25Error):
            fixture.verifier.observe_latest(
                other, bundle["freshness"], NOW
            )

    def test_malformed_validated_success_rejected(self):
        mutations = (
            ("classificationSHA256", "bad"),
            ("resourceVersion", "0"),
        )
        for key, value in mutations:
            fixture = Rev25Fixture()
            bundle = fixture.bundle()
            fixture.observe(bundle)
            event = next(
                entry
                for entry in bundle["entries"]
                if entry["leaf"]["event"] == "PATCH_VALIDATED_SUCCESS"
            )
            event["leaf"]["payload"][key] = value
            with self.assertRaises(core.EvidenceError):
                fixture.verifier.verify_recovery(bundle, NOW)

    def test_malformed_predecessor_hash_and_terminal_outcome_rejected(self):
        mutations = (
            ("GET_VALIDATED", "predecessorSpecSHA256", "bad"),
            ("RUN_TERMINAL", "outcome", "MAYBE"),
        )
        for event_name, key, value in mutations:
            fixture = Rev25Fixture()
            bundle = fixture.bundle()
            fixture.observe(bundle)
            event = next(
                entry
                for entry in bundle["entries"]
                if entry["leaf"]["event"] == event_name
            )
            event["leaf"]["payload"][key] = value
            with self.assertRaises(core.EvidenceError):
                fixture.verifier.verify_recovery(bundle, NOW)

    def test_request_byte_bounds_rejected(self):
        for value in (True, 0, rev25.rev24.MAX_BODY_BYTES + 1):
            fixture = Rev25Fixture()
            bundle = fixture.bundle()
            fixture.observe(bundle)
            bundle["entries"][1]["leaf"]["payload"]["requestBytes"] = value
            with self.assertRaises(rev25.Rev25Error):
                fixture.verifier.verify_recovery(bundle, NOW)

    def test_deployment_identity_and_endpoint_closed(self):
        for key, value in (
            ("serviceIdentity", ""),
            ("serviceEndpoint", "http://insecure.invalid"),
        ):
            fixture = Rev25Fixture()
            bundle = fixture.bundle()
            bundle["deployment"]["statement"][key] = value
            bundle["deployment"] = rev25.sign_authority_statement(
                bundle["deployment"]["statement"], fixture.root
            )
            with self.assertRaises(rev25.Rev25Error):
                fixture.verifier.verify_recovery(bundle, NOW)

    def test_receipt_rejects_non_pass_terminal_outcomes(self):
        original_payload = rev24_tests.payload
        try:
            for outcome in ("UNKNOWN", "FAIL", "INCONCLUSIVE"):
                def terminal_payload(event, terminal_outcome=outcome):
                    value = original_payload(event)
                    if event == "RUN_TERMINAL":
                        value["outcome"] = terminal_outcome
                    return value

                rev24_tests.payload = terminal_payload
                fixture = Rev25Fixture()
                bundle = fixture.bundle()
                fixture.observe(bundle)
                result = fixture.verifier.verify_recovery(bundle, NOW)
                self.assertEqual(result["terminalOutcome"], outcome)
                with self.assertRaises(ValueError):
                    require_terminal_success(result)
        finally:
            rev24_tests.payload = original_payload

    def test_receipt_transaction_fences_latest_observation(self):
        fixture = Rev25Fixture()
        bundle = fixture.bundle()
        fixture.observe(bundle)
        current = fixture.state.latest(rev25._stream_id(BINDING))
        started = threading.Event()
        completed = threading.Event()

        def advance():
            observer = SQLiteState(fixture.path)
            started.set()
            observer.observe_latest(
                rev25._stream_id(BINDING),
                current["latestIndex"] + 1,
                "f" * 64,
                NOW + 1,
            )
            observer.close()
            completed.set()

        with fixture.state.receipt_transaction():
            result = fixture.verifier.verify_recovery(bundle, NOW)
            require_terminal_success(result)
            thread = threading.Thread(target=advance)
            thread.start()
            self.assertTrue(started.wait(1))
            time.sleep(0.05)
            self.assertFalse(completed.is_set())
            fixture.state.record_receipt_authorization(
                "authorization", {"terminalOutcome": "PASS"}
            )

        thread.join(1)
        self.assertTrue(completed.is_set())

    def test_receipt_authorization_retry_can_use_fresh_issuance(self):
        first = receipt_authorization_id(
            "run", "workflow", "a" * 64, NOW
        )
        retry = receipt_authorization_id(
            "run", "workflow", "a" * 64, NOW + 1
        )
        self.assertNotEqual(first, retry)


if __name__ == "__main__":
    unittest.main()
