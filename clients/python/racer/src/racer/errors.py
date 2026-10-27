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
