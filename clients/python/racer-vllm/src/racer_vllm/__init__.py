# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""vLLM plugin that loads model weights from a Racer cache."""

from .loader import LOAD_FORMAT, register
from .model import ModelURI, fetch, parse_uri

__all__ = ["LOAD_FORMAT", "ModelURI", "fetch", "parse_uri", "register"]
