# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Python client for Racer, the Unbounded node-local object cache."""

from .client import (
    GPU,
    HOST,
    STREAM,
    DEFAULT_MODE_RETRY_SECONDS,
    Client,
    Metadata,
    ReadResult,
    forget_modes,
    socket_path_for,
)
from .errors import (
    NotFound,
    PrefetchError,
    ProtocolError,
    RacerError,
    StatusError,
    TruncatedError,
    UnsatisfiableRange,
    VersionUnavailable,
)
from ._wire import PAGE_SIZE
from .prefetch import (
    PrefetchBytes,
    PrefetchCoverage,
    PrefetchRef,
    PrefetchRefStatus,
    PrefetchSpec,
    PrefetchStatus,
    PrefetchWindow,
    split_prefetch,
)
from .host import HostBuffer
from .keys import Request, canonical_json, object_key, object_request

__all__ = [
    "DEFAULT_MODE_RETRY_SECONDS",
    "GPU",
    "HOST",
    "PAGE_SIZE",
    "STREAM",
    "Client",
    "HostBuffer",
    "Metadata",
    "NotFound",
    "PrefetchBytes",
    "PrefetchCoverage",
    "PrefetchError",
    "PrefetchRef",
    "PrefetchRefStatus",
    "PrefetchSpec",
    "PrefetchStatus",
    "PrefetchWindow",
    "ProtocolError",
    "RacerError",
    "ReadResult",
    "Request",
    "StatusError",
    "TruncatedError",
    "UnsatisfiableRange",
    "VersionUnavailable",
    "canonical_json",
    "forget_modes",
    "object_key",
    "object_request",
    "socket_path_for",
    "split_prefetch",
]
