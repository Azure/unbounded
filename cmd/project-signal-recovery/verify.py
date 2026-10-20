#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

import argparse
import base64
import hashlib
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent
sys.path.insert(0, str(ROOT / "reference" / "rev24"))

from rev25_protocol import TrustedResolver, Verifier, strict_loads
from receipt import bundle_sha256, sign
from state import SQLiteState


def require_terminal_success(result):
    if (
        result["patchApplied"] != "YES"
        or not result["terminalRecordPresent"]
        or result["terminalOutcome"] != "PASS"
    ):
        raise ValueError("recovery is not a terminal validated success")


def receipt_authorization_id(
    run_uid, workflow_uid, bundle_digest, issued_at_epoch
):
    return hashlib.sha256(
        (
            run_uid
            + "\0"
            + workflow_uid
            + "\0"
            + bundle_digest
            + "\0"
            + str(issued_at_epoch)
        ).encode()
    ).hexdigest()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("operation", choices=("observe", "verify"))
    parser.add_argument("--trusted-config", required=True)
    parser.add_argument("--state", required=True)
    parser.add_argument("--bundle", required=True)
    parser.add_argument("--now-epoch", required=True, type=int)
    parser.add_argument("--receipt-private-key")
    parser.add_argument("--receipt-output")
    parser.add_argument("--run-name")
    parser.add_argument("--run-uid")
    parser.add_argument("--workflow-uid")
    args = parser.parse_args()

    receipt_values = (
        args.receipt_private_key,
        args.receipt_output,
        args.run_name,
        args.run_uid,
        args.workflow_uid,
    )
    if any(receipt_values) and not all(receipt_values):
        parser.error("all receipt arguments are required together")

    bundle_path = Path(args.bundle)
    bundle_bytes = bundle_path.read_bytes()
    bundle = strict_loads(bundle_bytes.decode("utf-8"))
    resolver = TrustedResolver.from_file(args.trusted_config)
    issuing_receipt = args.operation == "verify" and all(receipt_values)
    state = SQLiteState(
        args.state,
        read_only=args.operation == "verify" and not issuing_receipt,
    )
    verifier = Verifier(resolver, state)

    if args.operation == "observe":
        verifier.observe_latest(
            bundle["binding"], bundle["freshness"], args.now_epoch
        )
        result = {"observed": True}
    else:
        if issuing_receipt:
            private_key = base64.b64decode(
                Path(args.receipt_private_key)
                .read_text(encoding="utf-8")
                .strip(),
                validate=True,
            )
            bundle_digest = bundle_sha256(bundle_bytes)
            authorization_id = receipt_authorization_id(
                args.run_uid,
                args.workflow_uid,
                bundle_digest,
                args.now_epoch,
            )
            with state.receipt_transaction():
                result = verifier.verify_recovery(bundle, args.now_epoch)
                require_terminal_success(result)
                receipt = sign(
                    private_key,
                    {
                        "runName": args.run_name,
                        "runUID": args.run_uid,
                        "workflowUID": args.workflow_uid,
                        "bundleSHA256": bundle_digest,
                        "patchApplied": "YES",
                        "terminalRecordPresent": True,
                        "terminalOutcome": "PASS",
                        "issuedAtEpoch": args.now_epoch,
                        "expiresAtEpoch": args.now_epoch + 300,
                    },
                )
                state.record_receipt_authorization(
                    authorization_id, receipt
                )
            Path(args.receipt_output).write_text(
                json.dumps(
                    receipt, sort_keys=True, separators=(",", ":")
                )
                + "\n",
                encoding="utf-8",
            )
        else:
            result = verifier.verify_recovery(bundle, args.now_epoch)

    json.dump(result, sys.stdout, sort_keys=True, separators=(",", ":"))
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
