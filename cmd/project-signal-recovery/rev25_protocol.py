# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

import base64
import copy
import json
import re
from pathlib import Path
from urllib.parse import urlparse

import evidence_protocol as core
import rev24_protocol as rev24

from state import StateError

sign_authority_statement = rev24.sign_authority_statement


MAX_FRESHNESS_AGE_SECONDS = 300
MAX_TEXT_BYTES = 256
TERMINAL_OUTCOMES = {"PASS", "FAIL", "INCONCLUSIVE", "UNKNOWN"}
FAILURE_OUTCOMES = {
    "ATOMIC_PATCH_REJECTED",
    "PATCH_RESPONSE_INVALID",
    "PRECONDITION_FAILED",
}
IDENTITY = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,254}$")
RESOURCE_VERSION = re.compile(r"^[1-9][0-9]{0,63}$")


class Rev25Error(rev24.Rev24Error):
    pass


class TrustedResolver:
    def __init__(self, root_public_key, ledger_revision, ledger_sha256):
        if type(root_public_key) is not bytes or len(root_public_key) != 32:
            raise Rev25Error("trusted root public key mismatch")
        if type(ledger_revision) is not int or isinstance(
            ledger_revision, bool
        ) or ledger_revision < 0:
            raise Rev25Error("trusted ledger revision mismatch")
        rev24._hex(ledger_sha256, "trusted ledger SHA256")
        self._root_public_key = bytes(root_public_key)
        self._ledger_revision = ledger_revision
        self._ledger_sha256 = ledger_sha256

    @classmethod
    def from_file(cls, path):
        value = json.loads(Path(path).read_text(encoding="utf-8"))
        if set(value) != {
            "rootPublicKeyBase64",
            "ledgerRevision",
            "ledgerSHA256",
        }:
            raise Rev25Error("trusted resolver configuration mismatch")
        try:
            root = base64.b64decode(
                value["rootPublicKeyBase64"], validate=True
            )
        except (TypeError, ValueError) as error:
            raise Rev25Error("trusted root encoding mismatch") from error
        return cls(root, value["ledgerRevision"], value["ledgerSHA256"])

    def snapshot(self):
        return (
            self._root_public_key,
            self._ledger_revision,
            self._ledger_sha256,
        )


def _stream_id(binding):
    return "|".join(
        str(binding[name])
        for name in (
            "authorityRevision",
            "authoritySHA256",
            "runID",
            "transferID",
            "contractSHA256",
            "generation",
        )
    )


def _consumption_id(challenge, leaf):
    return "|".join(
        str(value)
        for value in (
            challenge["challengeNonce"],
            leaf["runID"],
            leaf["sequence"],
            leaf["event"],
            leaf["payload"]["requestSHA256"],
        )
    )


def _witness_statement_id(binding, key_id, index):
    return "|".join(
        [
            _stream_id(binding),
            key_id,
            str(index),
        ]
    )


def _closed_text(value, name):
    if (
        type(value) is not str
        or not IDENTITY.fullmatch(value)
        or len(value.encode()) > MAX_TEXT_BYTES
    ):
        raise Rev25Error(f"{name} mismatch")


def _closed_endpoint(value, name):
    _closed_text(value, name)
    parsed = urlparse(value)
    if (
        parsed.scheme != "https"
        or not parsed.hostname
        or parsed.username is not None
        or parsed.password is not None
        or parsed.fragment
    ):
        raise Rev25Error(f"{name} mismatch")


def _strict_payloads(entries):
    patch_status = None
    for entry in entries:
        leaf = entry["leaf"]
        event = leaf["event"]
        payload = leaf["payload"]
        if event in {"GET_INTENT", "PATCH_INTENT"}:
            count = payload["requestBytes"]
            if (
                type(count) is not int
                or isinstance(count, bool)
                or not 1 <= count <= rev24.MAX_BODY_BYTES
            ):
                raise Rev25Error("request byte count mismatch")
        if event == "GET_VALIDATED":
            rev24._hex(payload["predecessorSpecSHA256"], "predecessor hash")
            if not RESOURCE_VERSION.fullmatch(payload["resourceVersion"]):
                raise Rev25Error("predecessor resource version mismatch")
        if event == "PATCH_RESPONSE":
            patch_status = payload["status"]
        if event == "PATCH_VALIDATED_SUCCESS":
            rev24._hex(payload["classificationSHA256"], "classification hash")
            if (
                patch_status is None
                or not 200 <= patch_status <= 299
                or not RESOURCE_VERSION.fullmatch(payload["resourceVersion"])
            ):
                raise Rev25Error("validated success mismatch")
        if event == "PATCH_VALIDATED_FAILURE":
            rev24._hex(payload["classificationSHA256"], "classification hash")
            if payload["failureOutcome"] not in FAILURE_OUTCOMES:
                raise Rev25Error("validated failure outcome mismatch")
        if event == "RUN_TERMINAL":
            if payload["outcome"] not in TERMINAL_OUTCOMES:
                raise Rev25Error("terminal outcome mismatch")


def _strict_deployment(statement):
    _closed_text(statement["acceptanceID"], "acceptance ID")
    _closed_text(statement["serviceIdentity"], "service identity")
    _closed_endpoint(statement["serviceEndpoint"], "service endpoint")
    for identity in statement["witnessIdentities"]:
        _closed_text(identity, "witness identity")
    for endpoint in statement["witnessEndpoints"]:
        _closed_endpoint(endpoint, "witness endpoint")


class DurableEvidenceService(rev24.FreshEvidenceService):
    def __init__(self, key_id, private_key, state):
        super().__init__(key_id, private_key)
        self._state = state

    def consume_intent_ack(
        self, leaf, ack, admission, challenge, now_epoch
    ):
        if now_epoch < challenge["issuedAtEpoch"]:
            raise Rev25Error("ACK consumption predates challenge")
        proof = super().consume_intent_ack(
            leaf, ack, admission, challenge, now_epoch
        )
        consumption_id = _consumption_id(challenge, leaf)
        try:
            self._state.consume_ack(
                consumption_id,
                {"ack": copy.deepcopy(ack), "consumption": proof},
            )
        except StateError as error:
            raise Rev25Error(str(error)) from error
        return proof

    def entries_with_proofs(self, binding, challenge=None):
        entries = super().entries_with_proofs(binding)
        if challenge is None:
            return entries
        for entry in entries:
            leaf = entry["leaf"]
            if leaf["event"] not in {"GET_INTENT", "PATCH_INTENT"}:
                continue
            proof = self._state.ack_proof(_consumption_id(challenge, leaf))
            if proof is not None:
                entry["intentProof"] = proof
        return entries


class DurableValidatingWitness(rev24.ValidatingWitness):
    def __init__(self, key_id, private_key, state):
        super().__init__(key_id, private_key)
        self._state = state

    def sign_log(
        self, binding, entries, admission, challenge, checkpoint_head
    ):
        rev24.validate_entries(binding, entries, admission, challenge)
        if entries[-1]["treeHead"] != checkpoint_head:
            raise Rev25Error("witness refused arbitrary head")
        statement_id = _witness_statement_id(
            binding, self.key_id, len(entries) - 1
        )
        try:
            self._state.record_witness_head(statement_id, checkpoint_head)
        except StateError as error:
            raise Rev25Error(str(error)) from error
        return self._witness.sign(
            binding, len(entries) - 1, checkpoint_head
        )


class Verifier:
    def __init__(
        self,
        resolver,
        state,
        max_freshness_age_seconds=MAX_FRESHNESS_AGE_SECONDS,
    ):
        self._resolver = resolver
        self._state = state
        self._max_freshness_age_seconds = max_freshness_age_seconds

    def _authority(self):
        return self._resolver.snapshot()

    def observe_latest(self, binding, freshness_envelope, now_epoch):
        root, revision, digest = self._authority()
        freshness = rev24.verify_authority_statement(
            freshness_envelope,
            root,
            rev24.FRESHNESS_SCHEMA,
            revision,
            digest,
        )
        self._validate_freshness_time(freshness, now_epoch, None)
        self._state.observe_latest(
            _stream_id(binding),
            freshness["latestIndex"],
            freshness["latestTreeHead"],
            freshness["observedAtEpoch"],
        )

    def _validate_freshness_time(self, freshness, now_epoch, challenge):
        observed = freshness.get("observedAtEpoch")
        latest_index = freshness.get("latestIndex")
        if (
            type(observed) is not int
            or isinstance(observed, bool)
            or type(latest_index) is not int
            or isinstance(latest_index, bool)
            or observed > now_epoch
            or now_epoch - observed > self._max_freshness_age_seconds
            or (
                challenge is not None
                and observed < challenge["issuedAtEpoch"]
            )
        ):
            raise Rev25Error("freshness temporal closure mismatch")

    def verify_recovery(self, bundle, now_epoch):
        root, revision, digest = self._authority()
        binding = bundle["binding"]
        admission = rev24.verify_admission(
            bundle["admission"], root, revision, digest
        )
        deployment = rev24.verify_deployment(
            bundle["deployment"], root, revision, digest, admission
        )
        _strict_deployment(deployment)
        deployment_hash = core.digest(core.canonical(deployment))
        challenge = rev24.verify_challenge(
            bundle["challenge"],
            root,
            revision,
            digest,
            binding,
            deployment_hash,
            now_epoch,
        )
        entries = bundle["entries"]
        _strict_payloads(entries)
        for entry in entries:
            proof = entry.get("intentProof")
            if proof is None:
                continue
            consumed = proof["consumption"].get("consumedAtEpoch")
            if (
                type(consumed) is not int
                or isinstance(consumed, bool)
                or consumed < challenge["issuedAtEpoch"]
                or consumed > now_epoch
            ):
                raise Rev25Error("ACK consumption temporal closure mismatch")
        freshness = rev24.verify_authority_statement(
            bundle["freshness"],
            root,
            rev24.FRESHNESS_SCHEMA,
            revision,
            digest,
        )
        self._validate_freshness_time(freshness, now_epoch, challenge)
        current = self._state.latest(_stream_id(binding))
        if any(
            freshness[name] != current[name]
            for name in (
                "latestIndex",
                "latestTreeHead",
                "observedAtEpoch",
            )
        ):
            raise Rev25Error("recovery does not match durable current latest")
        return rev24.verify_recovery(
            binding,
            entries,
            bundle["checkpoint"],
            bundle["freshness"],
            bundle["admission"],
            bundle["deployment"],
            bundle["challenge"],
            root,
            revision,
            digest,
            now_epoch,
        )
