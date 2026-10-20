#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

import argparse
import base64
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent
sys.path.insert(0, str(ROOT / "reference" / "rev24"))

from rev25_protocol import TrustedResolver, Verifier, strict_loads
from receipt import bundle_sha256, sign
from state import SQLiteState


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

    bundle = strict_loads(Path(args.bundle).read_text(encoding="utf-8"))
    resolver = TrustedResolver.from_file(args.trusted_config)
    state = SQLiteState(args.state, read_only=args.operation == "verify")
    verifier = Verifier(resolver, state)

    if args.operation == "observe":
        verifier.observe_latest(
            bundle["binding"], bundle["freshness"], args.now_epoch
        )
        result = {"observed": True}
    else:
        result = verifier.verify_recovery(bundle, args.now_epoch)
        receipt_values = (
            args.receipt_private_key,
            args.receipt_output,
            args.run_name,
            args.run_uid,
            args.workflow_uid,
        )
        if any(receipt_values) and not all(receipt_values):
            parser.error("all receipt arguments are required together")
        if all(receipt_values):
            if (
                result["patchApplied"] != "YES"
                or not result["terminalRecordPresent"]
            ):
                raise ValueError(
                    "recovery is not a terminal validated success"
                )
            private_key = base64.b64decode(
                Path(args.receipt_private_key)
                .read_text(encoding="utf-8")
                .strip(),
                validate=True,
            )
            bundle_bytes = Path(args.bundle).read_bytes()
            receipt = sign(
                private_key,
                {
                    "runName": args.run_name,
                    "runUID": args.run_uid,
                    "workflowUID": args.workflow_uid,
                    "bundleSHA256": bundle_sha256(bundle_bytes),
                    "patchApplied": "YES",
                    "terminalRecordPresent": True,
                    "issuedAtEpoch": args.now_epoch,
                    "expiresAtEpoch": args.now_epoch + 300,
                },
            )
            Path(args.receipt_output).write_text(
                json.dumps(
                    receipt, sort_keys=True, separators=(",", ":")
                )
                + "\n",
                encoding="utf-8",
            )

    json.dump(result, sys.stdout, sort_keys=True, separators=(",", ":"))
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
