# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Upload destinations for ``racer-vllm import`` and ``prepare``.

A destination is an Azure Blob container URL with an optional prefix,
``https://<account>.blob.core.windows.net/<container>[/<prefix>]``, or a
local directory as ``file:///path`` (for staging and tests). Each blob
carries its sha256 in metadata, so a re-run skips unchanged files.

The Azure SDK is an optional extra (``racer-vllm[azure]``) and is imported
only when an https destination is used.
"""

from __future__ import annotations

import hashlib
import os
import shutil
from urllib.parse import urlparse

from .uri import unsafe_name

SHA_METADATA = "sha256"
AZURE_HINT = (
    "racer-vllm: uploading to Azure Blob needs the azure extra: "
    "pip install 'racer-vllm[azure]' (azure-storage-blob and azure-identity)"
)


class StoreError(RuntimeError):
    """A destination is malformed or an upload failed."""


def sha256_file(path: str, chunk: int = 8 << 20) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        while True:
            b = f.read(chunk)
            if not b:
                return h.hexdigest()
            h.update(b)


class LocalStore:
    """A directory standing in for a container prefix. Writes are atomic renames."""

    def __init__(self, root: str) -> None:
        self.root = root
        self.url = "file://" + root

    def _path(self, name: str) -> str:
        if unsafe_name(name):
            raise StoreError(f"unsafe blob name {name!r}")
        return os.path.join(self.root, name)

    def sha256(self, name: str):
        """The recorded sha256 of name, or None when it is missing."""
        p = self._path(name)
        if not os.path.isfile(p):
            return None
        side = p + ".sha256"
        if os.path.isfile(side):
            with open(side) as f:
                return f.read().strip()
        return sha256_file(p)

    def _write(self, name: str, writer, sha: str) -> None:
        p = self._path(name)
        os.makedirs(os.path.dirname(p) or ".", exist_ok=True)
        tmp = p + ".tmp"
        writer(tmp)
        os.replace(tmp, p)
        with open(p + ".sha256", "w") as f:
            f.write(sha + "\n")

    def upload_file(self, name: str, path: str, sha: str) -> None:
        self._write(name, lambda tmp: shutil.copyfile(path, tmp), sha)

    def put(self, name: str, data: bytes) -> None:
        def w(tmp):
            with open(tmp, "wb") as f:
                f.write(data)

        self._write(name, w, hashlib.sha256(data).hexdigest())

    def get(self, name: str):
        p = self._path(name)
        if not os.path.isfile(p):
            return None
        with open(p, "rb") as f:
            return f.read()

    def delete(self, name: str) -> None:
        for p in (self._path(name), self._path(name) + ".sha256"):
            try:
                os.remove(p)
            except FileNotFoundError:
                pass


class AzureStore:
    """A container prefix in Azure Blob, authenticated with DefaultAzureCredential.

    DefaultAzureCredential picks up workload identity from AZURE_CLIENT_ID,
    AZURE_TENANT_ID and AZURE_FEDERATED_TOKEN_FILE.
    """

    def __init__(self, account_url: str, container: str, prefix: str, max_concurrency: int = 8) -> None:
        try:
            from azure.core.exceptions import ResourceNotFoundError
            from azure.identity import DefaultAzureCredential
            from azure.storage.blob import ContainerClient
        except ImportError as exc:
            raise StoreError(f"{AZURE_HINT} ({exc})") from None
        self._missing = ResourceNotFoundError
        self.url = f"{account_url}/{container}" + (f"/{prefix}" if prefix else "")
        self.prefix = prefix
        self.max_concurrency = max_concurrency
        self.container = ContainerClient(account_url, container, credential=DefaultAzureCredential())

    def _blob(self, name: str) -> str:
        if unsafe_name(name):
            raise StoreError(f"unsafe blob name {name!r}")
        return f"{self.prefix}/{name}" if self.prefix else name

    def sha256(self, name: str):
        try:
            props = self.container.get_blob_client(self._blob(name)).get_blob_properties()
        except self._missing:
            return None
        return (props.metadata or {}).get(SHA_METADATA) or ""

    def upload_file(self, name: str, path: str, sha: str) -> None:
        with open(path, "rb") as f:
            self.container.upload_blob(
                self._blob(name),
                f,
                overwrite=True,
                metadata={SHA_METADATA: sha},
                max_concurrency=self.max_concurrency,
            )

    def put(self, name: str, data: bytes) -> None:
        sha = hashlib.sha256(data).hexdigest()
        self.container.upload_blob(self._blob(name), data, overwrite=True, metadata={SHA_METADATA: sha})

    def get(self, name: str):
        try:
            return self.container.download_blob(self._blob(name)).readall()
        except self._missing:
            return None

    def delete(self, name: str) -> None:
        try:
            self.container.delete_blob(self._blob(name))
        except self._missing:
            pass


def open_store(dest: str, max_concurrency: int = 8):
    """Parse a destination and return a store for it. Raises StoreError."""
    u = urlparse(dest)
    if u.scheme == "file":
        if u.netloc:
            raise StoreError(f"file destination must be file:///absolute/path, got {dest!r}")
        return LocalStore(u.path.rstrip("/") or "/")
    if u.scheme != "https" or not u.netloc:
        raise StoreError(
            f"destination must be https://<account>.blob.core.windows.net/<container>[/<prefix>] "
            f"or file:///path, got {dest!r}"
        )
    if u.query or u.fragment:
        raise StoreError("destination must not carry a query or fragment; credentials come from the environment")
    parts = u.path.strip("/").split("/", 1)
    if not parts[0]:
        raise StoreError(f"destination {dest!r} names no container")
    prefix = parts[1].strip("/") if len(parts) == 2 else ""
    if ".." in prefix.split("/"):
        raise StoreError(f"destination prefix must not contain '..': {dest!r}")
    return AzureStore(f"https://{u.netloc}", parts[0], prefix, max_concurrency)
