#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

import argparse
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent
sys.path.insert(0, str(ROOT / "reference" / "rev24"))

from rev25_protocol import TrustedResolver, Verifier
from state import SQLiteState


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("operation", choices=("observe", "verify"))
    parser.add_argument("--trusted-config", required=True)
    parser.add_argument("--state", required=True)
    parser.add_argument("--bundle", required=True)
    parser.add_argument("--now-epoch", required=True, type=int)
    args = parser.parse_args()

    bundle = json.loads(Path(args.bundle).read_text(encoding="utf-8"))
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

    json.dump(result, sys.stdout, sort_keys=True, separators=(",", ":"))
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
