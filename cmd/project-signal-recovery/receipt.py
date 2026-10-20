# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

import base64
import hashlib
import json

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey


SCHEMA = "signal.unbounded-cloud.io/recovery-receipt/v1"


def canonical(value):
    return json.dumps(
        value, sort_keys=True, separators=(",", ":"), ensure_ascii=True
    ).encode()


def sign(private_key_bytes, fields):
    private_key = Ed25519PrivateKey.from_private_bytes(private_key_bytes)
    unsigned = {"schema": SCHEMA, **fields}
    return {
        **unsigned,
        "signatureBase64": base64.b64encode(
            private_key.sign(canonical(unsigned))
        ).decode(),
    }


def bundle_sha256(data):
    return hashlib.sha256(data).hexdigest()
