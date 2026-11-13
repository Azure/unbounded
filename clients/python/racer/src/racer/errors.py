# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Errors raised by the Racer client.

Status mapping follows wire.StatusError in pkg/racersdk/internal/wire/wire.go.
"""

from __future__ import annotations


class RacerError(Exception):
    """Base class for all Racer client errors."""


class ProtocolError(RacerError):
    """The server sent something the protocol does not allow."""


class TruncatedError(RacerError):
    """The connection closed before the completion frame."""


class StatusError(RacerError):
    """The server answered with a non-200 status."""

    status = 0

    def __init__(self, status: int, message: str | None = None) -> None:
        super().__init__(message or f"racer: status {status}")
        self.status = status


class InvalidArgument(StatusError):
    pass


class Unauthorized(StatusError):
    pass


class Forbidden(StatusError):
    pass


class NotFound(StatusError):
    pass


class VersionUnavailable(StatusError):
    pass


class UnsatisfiableRange(StatusError):
    pass


class HeaderLimit(StatusError):
    pass


class NotImplementedStatus(StatusError):
    pass


class Unavailable(StatusError):
    pass


class PrefetchError(StatusError):
    """A prefetch request was rejected.

    code is the machine-readable code from the JSON error body, such as
    "stale_sequence", or "" if the body was missing or invalid. retry_after is
    the Retry-After header in seconds, or None.
    """

    def __init__(self, status: int, code: str = "", message: str = "", retry_after: float | None = None) -> None:
        text = f"racer: prefetch: status {status}"
        if code:
            text += " " + code
        if message:
            text += ": " + message
        super().__init__(status, text)
        self.code = code
        self.message = message
        self.retry_after = retry_after


class WriteError(StatusError):
    """Racer rejected a write.

    code is the Racer-Write-Error diagnostic token (such as "dirty-cap"), or
    "". retry_after is the Retry-After header in seconds, or None.
    """

    def __init__(self, status: int, code: str = "", retry_after: float | None = None, op: str = "write") -> None:
        text = f"racer: {op}: status {status}"
        if code:
            text += f" ({code})"
        super().__init__(status, text)
        self.code = code
        self.retry_after = retry_after
        self.op = op


class Exists(WriteError):
    """412: the key already exists in the cache or at the origin (writes are create-once)."""


class WritesDisabled(WriteError):
    """405: the cache does not accept writes, or the dataplane predates them."""


class Busy(WriteError, Unavailable):
    """503: Racer cannot take the write now (dirty cap, owners, coordinator)."""


class UploadFailed(WriteError):
    """502: the origin upload failed under origin durability."""


class Conflict(WriteError):
    """409: the object's state does not allow the operation."""


class WriteNotFound(WriteError, NotFound):
    """404: no written object for the key."""


class WriteInvalid(WriteError, InvalidArgument):
    """400, 411, 413, 428, 431: Racer rejected the request headers or body."""


class WriteInternal(WriteError):
    """500: an internal Racer fault. Retrying the same request is not expected to help."""


class WriteUnavailable(WriteError, Unavailable):
    """504: the request deadline elapsed before durability."""


_WRITE_BY_STATUS = {
    400: WriteInvalid,
    404: WriteNotFound,
    405: WritesDisabled,
    409: Conflict,
    411: WriteInvalid,
    412: Exists,
    413: WriteInvalid,
    428: WriteInvalid,
    431: WriteInvalid,
    500: WriteInternal,
    502: UploadFailed,
    503: Busy,
    504: WriteUnavailable,
}


def write_error(status: int, code: str = "", retry_after: float | None = None, op: str = "write") -> WriteError:
    return _WRITE_BY_STATUS.get(status, WriteError)(status, code, retry_after, op)


_BY_STATUS = {
    400: InvalidArgument,
    401: Unauthorized,
    403: Forbidden,
    404: NotFound,
    405: InvalidArgument,
    412: VersionUnavailable,
    416: UnsatisfiableRange,
    431: HeaderLimit,
    501: NotImplementedStatus,
    503: Unavailable,
}


def status_error(status: int) -> StatusError:
    return _BY_STATUS.get(status, StatusError)(status)
