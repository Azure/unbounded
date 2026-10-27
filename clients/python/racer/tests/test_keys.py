# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
import pytest

import racer

# Computed with internal/racerobject.NewRequest (Go encoding/json + SHA-256).
VECTORS = [
    (
        "models",
        "llama/config.json",
        "",
        "3af125b4fe06cdb5fc810214a357c138d4acfc54d99ef56791f805182d4044e0",
        '{"schema":1,"bucket":"models","key":"llama/config.json"}',
    ),
    (
        "models",
        "llama/model-00001-of-00002.safetensors",
        "v1",
        "982a88e7fe93c3d53ff66b9045aa99089251f66d613790c6c5888b97054bdd16",
        '{"schema":1,"bucket":"models","key":"llama/model-00001-of-00002.safetensors","versionId":"v1"}',
    ),
    (
        "b.u-c_k",
        'a<b>&c "q" \\ \n\t\b\f\x01\x7f \u00e9 \u65e5\u672c \u2028\u2029 \U0001f600',
        "3HL4kqtJlcpXroDTDmJ+rmSpXd3dIbrHY+MTRCxf3vjVBH40Nr8X8gdRQBpUMLUo",
        "fde698046567709f2f289be4e4dc246b9fbc40a04b4df1ca0f3cf2d6e891b1ac",
        None,
    ),
]


@pytest.mark.parametrize("bucket,key,version,want,canonical", VECTORS)
def test_object_key_vectors(bucket, key, version, want, canonical):
    assert racer.object_key(bucket, key, version) == want
    if canonical is not None:
        assert racer.canonical_json(bucket, key, version) == canonical
    r = racer.object_request(bucket, key, version or None)
    assert r.hex == want and r.metadata == racer.canonical_json(bucket, key, version)


@pytest.mark.parametrize(
    "bucket,key,version",
    [
        ("", "k", ""),
        (".", "k", ""),
        ("a/b", "k", ""),
        ("b", "", ""),
        ("b", "x" * 1025, ""),
        ("b", "k", "v" * 1025),
        ("b", "\ud800", ""),
    ],
)
def test_object_key_invalid(bucket, key, version):
    with pytest.raises(racer.RacerError):
        racer.object_key(bucket, key, version)


def test_request_from_hex():
    r = racer.Request.from_hex("ab" * 32)
    assert r.key == b"\xab" * 32
    with pytest.raises(racer.RacerError):
        racer.Request.from_hex("AB" * 32)
