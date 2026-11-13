# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""racer://<cache>/<bucket>/<key prefix> model locations."""

from __future__ import annotations

from dataclasses import dataclass

SCHEME = "racer://"


@dataclass(frozen=True)
class ModelURI:
    """A parsed racer://<cache>/<bucket>/<prefix>."""

    cache: str
    bucket: str
    prefix: str  # no leading or trailing slash; may be empty

    def key(self, name: str) -> str:
        return f"{self.prefix}/{name}" if self.prefix else name

    def request(self, name: str):
        import racer

        return racer.object_request(self.bucket, self.key(name))

    def child(self, sub: str) -> "ModelURI":
        """The URI of a sub-prefix, such as a sharded artifact directory."""
        return ModelURI(self.cache, self.bucket, self.key(sub.strip("/")))

    def __str__(self) -> str:
        return f"{SCHEME}{self.cache}/{self.bucket}/{self.prefix}".rstrip("/")


def parse_uri(uri: str) -> ModelURI:
    """Parse racer://<cache>/<bucket>/<prefix>."""
    if not isinstance(uri, str) or not uri.startswith(SCHEME):
        raise ValueError(f"not a racer:// URI: {uri!r}")
    parts = uri[len(SCHEME) :].split("/", 2)
    if len(parts) < 2 or not parts[0] or not parts[1]:
        raise ValueError(f"racer URI needs a cache and a bucket: {uri!r}")
    prefix = parts[2].strip("/") if len(parts) == 3 else ""
    if ".." in prefix.split("/"):
        raise ValueError(f"racer URI prefix must not contain '..': {uri!r}")
    return ModelURI(parts[0], parts[1], prefix)


def unsafe_name(name: str) -> bool:
    """True for names that could escape a directory."""
    return not name or name.startswith("/") or ".." in name.split("/") or "\\" in name or "\0" in name
