# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""A real Nebraska, flatcar/nebraska's backend at a pinned commit, for centerpivot_e2e.py.

It's built from source into a small image, since its releases don't have package labels yet, and
runs in a Podman pod with Postgres, published on the host's loopback only. Its admin API takes no
authentication there (-auth-mode=noop); the harness's front (serve.Front) passes the VMs' and the
cluster's Omaha requests on to it.

Every host command goes through an injected run, so the module can be tested without Podman
(test_nebraska.py).
"""

from __future__ import annotations

import base64
import dataclasses
import json
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
from collections.abc import Callable
from pathlib import Path
from typing import Any

from errors import StepError

Run = Callable[..., subprocess.CompletedProcess[str]]
Log = Callable[[str], None]

# The first commit with package labels, which trident-acl-agent reads each COSI's checksum from.
COMMIT = "412ae9fceb211efba5005afe7d259ec742b34628"
SOURCE_URL = f"https://github.com/flatcar/nebraska/archive/{COMMIT}.tar.gz"
GO_IMAGE = "docker.io/library/golang:1.27.4-trixie"
POSTGRES_IMAGE = "docker.io/library/postgres:16-alpine"
IMAGE = f"localhost/acl-e2e-nebraska:{COMMIT[:12]}"
DB_PASSWORD = "nebraska"
DB_URL = f"postgres://postgres:{DB_PASSWORD}@127.0.0.1:5432/nebraska?sslmode=disable&connect_timeout=10"
SERVER_PORT = 8000

# Nebraska's instance statuses (backend/pkg/api/types/instance.go).
STATUS_UNDEFINED = 1
STATUS_UPDATE_GRANTED = 2
STATUS_ERROR = 3
STATUS_COMPLETE = 4
STATUS_INSTALLED = 5
STATUS_DOWNLOADED = 6
STATUS_DOWNLOADING = 7
STATUS_ON_HOLD = 8
STATUS_NAMES = {
    STATUS_UNDEFINED: "Undefined", STATUS_UPDATE_GRANTED: "UpdateGranted", STATUS_ERROR: "Error",
    STATUS_COMPLETE: "Complete", STATUS_INSTALLED: "Installed", STATUS_DOWNLOADED: "Downloaded",
    STATUS_DOWNLOADING: "Downloading", STATUS_ON_HOLD: "OnHold",
}
# A package of no particular OS, for amd64 (types/package.go, types/arch.go).
PACKAGE_TYPE_OTHER = 4
ARCH_AMD64 = 1
# The label trident-acl-agent reads the COSI metadata's SHA-384 from, as hex.
SHA384_LABEL = "cosi.metadata.sha384"

CONTAINERFILE = f"""\
FROM {GO_IMAGE} AS build
ADD {SOURCE_URL} /nebraska.tar.gz
RUN mkdir /src && tar -xzf /nebraska.tar.gz -C /src --strip-components=1
WORKDIR /src/backend
RUN CGO_ENABLED=0 go build -trimpath -o /out/nebraska ./cmd/nebraska && mkdir -p /out/static

FROM scratch
COPY --from=build /out/ /nebraska/
USER 65534:65534
ENTRYPOINT ["/nebraska/nebraska"]
"""


def status_name(status: int | None) -> str:
    return STATUS_NAMES.get(status or 0, str(status))


def app_payload(name: str) -> dict[str, Any]:
    return {"name": name, "description": "centerpivot e2e"}


def package_payload(app_id: str, *, version: str, base_url: str, filename: str, size: int,
                    sha384: str) -> dict[str, Any]:
    """A COSI as a Nebraska package. The agent downloads base_url + filename, and checks the
    metadata's SHA-384 in the label. Nebraska's own hash field holds it too, in base64, as
    trident's tests publish it."""
    if not base_url.endswith("/"):
        raise StepError(f"a package's URL is a directory, which ends in /: {base_url}")
    try:
        digest = bytes.fromhex(sha384)
    except ValueError as error:
        raise StepError(f"{sha384!r} isn't a hex SHA-384") from error
    if len(digest) != 48:
        raise StepError(f"{sha384!r} isn't a hex SHA-384")
    return {
        "application_id": app_id, "type": PACKAGE_TYPE_OTHER, "arch": ARCH_AMD64, "version": version,
        "url": base_url, "filename": filename, "size": str(size), "description": filename,
        "hash": base64.b64encode(digest).decode(), "channels_blacklist": [],
        "labels": {SHA384_LABEL: sha384.lower()},
    }


def channel_payload(app_id: str, name: str, package_id: str) -> dict[str, Any]:
    return {"application_id": app_id, "name": name, "color": "#1b5e20", "arch": ARCH_AMD64,
            "package_id": package_id}


def group_payload(name: str, channel_id: str, track: str, update_timeout_minutes: int) -> dict[str, Any]:
    """A group whose limits never hold an update back, with safe mode off, which would turn the
    group's updates off when an update fails (designs/centerpivot.md, section 5.3). Its update
    timeout must be at least the Site's updateTimeout."""
    return {
        "name": name, "description": "centerpivot e2e", "channel_id": channel_id, "track": track,
        "policy_updates_enabled": True, "policy_safe_mode": False, "policy_office_hours": False,
        "policy_timezone": "UTC", "policy_period_interval": "15 minutes",
        "policy_max_updates_per_period": 100, "policy_update_timeout": f"{update_timeout_minutes} minutes",
    }


@dataclasses.dataclass(frozen=True)
class App:
    """A Nebraska app the harness made, with the one channel and group its nodes use."""

    id: str
    name: str
    track: str
    group_id: str
    channel_id: str
    package_id: str
    version: str


class Nebraska:
    """Nebraska and its Postgres, in a Podman pod named name, and a client of its admin API."""

    def __init__(self, *, name: str, port: int, work_dir: Path, run: Run, log: Log) -> None:
        self.name = name
        self.port = port
        self.work_dir = work_dir
        self.run = run
        self.log = log
        self.sleep: Callable[[float], None] = time.sleep
        self.monotonic: Callable[[], float] = time.monotonic

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self.port}"

    @property
    def db_container(self) -> str:
        return f"{self.name}-db"

    @property
    def server_container(self) -> str:
        return f"{self.name}-server"

    def _podman(self, *args: str, check: bool = True, timeout: float = 120,
                stream: bool = False) -> subprocess.CompletedProcess[str]:
        result = self.run(["podman", *args], timeout=timeout, stream=stream)
        if check and result.returncode != 0:
            raise StepError(f"podman {' '.join(args[:3])} exited {result.returncode}: "
                            f"{(result.stderr or result.stdout or '').strip()[-600:]}")
        return result

    # The pod

    def build(self) -> None:
        """Build the image, unless it's there already."""
        if self._podman("image", "exists", IMAGE, check=False).returncode == 0:
            return
        context = self.work_dir / "nebraska-image"
        context.mkdir(parents=True, exist_ok=True)
        (context / "Containerfile").write_text(CONTAINERFILE)
        self.log(f"building Nebraska {COMMIT[:12]} from source")
        self._podman("build", "-t", IMAGE, "-f", str(context / "Containerfile"), str(context), timeout=1800,
                     stream=True)

    def start(self) -> None:
        """Start the pod afresh: an empty database, and Nebraska on 127.0.0.1:port."""
        self.stop()
        self._podman("pod", "create", "--name", self.name, "-p", f"127.0.0.1:{self.port}:{SERVER_PORT}")
        self._podman("run", "-d", "--pod", self.name, "--name", self.db_container,
                     "-e", f"POSTGRES_PASSWORD={DB_PASSWORD}", "-e", "POSTGRES_DB=nebraska", POSTGRES_IMAGE,
                     timeout=600)
        if not self._poll(120, lambda: self._podman("exec", self.db_container, "pg_isready", "-h", "127.0.0.1",
                                                    "-U", "postgres", check=False).returncode == 0):
            raise StepError(f"Postgres in pod {self.name} wasn't ready within 120s")
        self._podman("run", "-d", "--pod", self.name, "--name", self.server_container,
                     "-e", f"NEBRASKA_DB_URL={DB_URL}", IMAGE,
                     "-auth-mode=noop", "-http-static-dir=/nebraska/static", f"-port={SERVER_PORT}")
        if not self._poll(120, self.healthy):
            raise StepError(f"Nebraska didn't answer at {self.url} within 120s:\n{self.logs()[-2000:]}")
        self.log(f"Nebraska {COMMIT[:12]} is up at {self.url}")

    def stop(self) -> None:
        self._podman("pod", "rm", "-f", "--ignore", self.name, check=False)

    def running(self) -> bool:
        result = self._podman("pod", "inspect", "--format", "{{.State}}", self.name, check=False)
        return result.returncode == 0 and (result.stdout or "").strip() == "Running"

    def healthy(self) -> bool:
        try:
            with urllib.request.urlopen(f"{self.url}/health", timeout=5) as response:
                return response.status == 200
        except (OSError, urllib.error.URLError):
            return False

    def logs(self) -> str:
        result = self._podman("logs", self.server_container, check=False)
        return (result.stdout or "") + (result.stderr or "")

    def _poll(self, timeout: float, probe: Callable[[], bool], interval: float = 2) -> bool:
        deadline = self.monotonic() + timeout
        while True:
            if probe():
                return True
            if self.monotonic() >= deadline:
                return False
            self.sleep(interval)

    # The admin API

    def request(self, method: str, path: str, body: dict[str, Any] | None = None) -> Any:
        data = json.dumps(body).encode() if body is not None else None
        request = urllib.request.Request(f"{self.url}{path}", data=data, method=method,
                                         headers={"Content-Type": "application/json"} if data else {})
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                text = response.read().decode()
        except urllib.error.HTTPError as error:
            raise StepError(f"Nebraska answered {method} {path} with {error.code}: "
                            f"{error.read().decode(errors='replace')[:500]}") from error
        except OSError as error:
            raise StepError(f"Nebraska didn't answer {method} {path}: {error}") from error
        return json.loads(text) if text.strip() else None

    def create_app(self, *, name: str, track: str, version: str, base_url: str, filename: str, size: int,
                   sha384: str, update_timeout_minutes: int) -> App:
        """An app that offers one COSI, as version, to the nodes on track."""
        app = self.request("POST", "/api/apps", app_payload(name))
        app_id = app["id"]
        package = self.request("POST", f"/api/apps/{app_id}/packages", package_payload(
            app_id, version=version, base_url=base_url, filename=filename, size=size, sha384=sha384))
        channel = self.request("POST", f"/api/apps/{app_id}/channels", channel_payload(app_id, name, package["id"]))
        group = self.request("POST", f"/api/apps/{app_id}/groups",
                             group_payload(name, channel["id"], track, update_timeout_minutes))
        self.log(f"Nebraska app {name} ({app_id}) offers {filename} as {version} on track {track}")
        return App(id=app_id, name=name, track=track, group_id=group["id"], channel_id=channel["id"],
                   package_id=package["id"], version=version)

    def instances(self, app: App) -> list[dict[str, Any]]:
        """The group's instances: each node that sent an update check, the service's or its agent's.
        The list has a row for each app an instance checked in to, so only the app's own are kept."""
        query = urllib.parse.urlencode({"status": 0, "page": 1, "perpage": 50, "duration": "1d"})
        page = self.request("GET", f"/api/apps/{app.id}/groups/{app.group_id}/instances?{query}") or {}
        return [instance for instance in page.get("instances") or []
                if (instance.get("application") or {}).get("application_id") == app.id]

    def status_history(self, app: App, instance_id: str) -> list[dict[str, Any]]:
        """The instance's statuses, newest first."""
        path = f"/api/apps/{app.id}/groups/{app.group_id}/instances/{instance_id}/status_history"
        return list(self.request("GET", path) or [])
