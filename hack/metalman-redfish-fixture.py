#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Recording Redfish fixture backed by one libvirt domain.

PXE overrides can update the libvirt boot order directly. When the optional
HTTP boundary arguments are supplied, a UefiHttp PATCH makes a prebuilt EFI
disk visible at the next power-on. That disk is the HTTP smoke test's documented
post-firmware boundary; the fixture does not pretend that OVMF implements native
DHCP-free UEFI HTTP boot.
"""

from __future__ import annotations

import argparse
import base64
import hmac
import json
import shutil
import ssl
import subprocess
import tempfile
import threading
import time
import xml.etree.ElementTree as ET
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any


def virsh(*args: str, check: bool = True) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        ["virsh", "--connect", "qemu:///system", *args],
        check=check,
        capture_output=True,
        text=True,
    )


class State:
    def __init__(self, args: argparse.Namespace) -> None:
        self.domain = args.domain
        self.mac = args.mac.lower()
        self.record = Path(args.record)
        self.efi_source = Path(args.efi_source) if args.efi_source else None
        self.efi_active = Path(args.efi_active) if args.efi_active else None
        self.bridge = args.bridge
        self.cache_dir = Path(args.cache_dir) if args.cache_dir else None
        self.manage_boot_order = args.manage_boot_order
        self.username = args.username
        self.password = args.password
        self.lock = threading.Lock()
        self.boot: dict[str, Any] = {
            "BootSourceOverrideTarget": "None",
            "BootSourceOverrideEnabled": "Disabled",
            "BootSourceOverrideMode": "UEFI",
            "HttpBootUri": "",
        }
        self.nic: dict[str, Any] = {
            "MACAddress": self.mac,
            "PermanentMACAddress": self.mac,
            "DHCPv4": {"DHCPEnabled": True},
            "IPv4StaticAddresses": [],
            "StaticNameServers": [],
        }

    def authorized(self, value: str | None) -> bool:
        if not self.username:
            return True
        expected = "Basic " + base64.b64encode(
            f"{self.username}:{self.password}".encode()
        ).decode()
        return hmac.compare_digest(value or "", expected)

    def static_address(self) -> str:
        if self.nic.get("DHCPv4") != {"DHCPEnabled": False}:
            raise ValueError("UefiHttp requested before DHCPv4 was disabled")
        addresses = self.nic.get("IPv4StaticAddresses")
        if not isinstance(addresses, list) or len(addresses) != 1:
            raise ValueError("UefiHttp requires exactly one accepted static IPv4 address")
        address = addresses[0]
        if not isinstance(address, dict) or not address.get("Address") or not address.get("SubnetMask"):
            raise ValueError("UefiHttp static IPv4 address is incomplete")
        return str(address["Address"])

    def with_client_address(self, action: Any) -> None:
        if not self.bridge:
            raise ValueError("UefiHttp requested without an HTTP boundary bridge")
        client_ip = self.static_address()
        subprocess.run(["ip", "address", "add", f"{client_ip}/32", "dev", self.bridge], check=True)
        try:
            action(client_ip)
        finally:
            subprocess.run(
                ["ip", "address", "delete", f"{client_ip}/32", "dev", self.bridge],
                check=False,
            )

    def append(self, method: str, path: str, body: Any, status: int) -> None:
        entry = {
            "time": time.time(),
            "method": method,
            "path": path,
            "body": body,
            "status": status,
        }
        with self.lock:
            with self.record.open("a", encoding="utf-8") as stream:
                stream.write(json.dumps(entry, sort_keys=True) + "\n")

    def power_state(self) -> str:
        result = virsh("domstate", self.domain, check=False)
        return "On" if result.returncode == 0 and "running" in result.stdout else "Off"

    def set_efi_boundary(self, enabled: bool) -> None:
        if not all((self.efi_source, self.efi_active, self.bridge, self.cache_dir)):
            raise ValueError("UefiHttp requested without HTTP boundary arguments")

        assert self.efi_source is not None
        assert self.efi_active is not None
        if enabled:
            boot_url = str(self.boot.get("HttpBootUri", ""))
            if not boot_url:
                raise ValueError("UefiHttp override has no HttpBootUri")
            base_url = boot_url.rsplit("/", 1)[0]

            def stage(client_ip: str) -> None:
                with subprocess.Popen(["mktemp", "-d"], stdout=subprocess.PIPE, text=True) as proc:
                    artifact_dir = Path(proc.communicate()[0].strip())
                entrypoint = boot_url.rsplit("/", 1)[-1]
                candidates = list(self.cache_dir.glob(f"oci/*/amd64/disk/{entrypoint}"))
                if len(candidates) != 1:
                    raise ValueError(
                        f"expected one cached HTTP entrypoint {entrypoint}, found {len(candidates)}"
                    )
                shutil.copyfile(candidates[0], artifact_dir / "http-entrypoint.efi")
                for path, url in (
                    ("grubx64.efi", f"{base_url}/grubx64.efi"),
                    ("vmlinuz", f"{base_url}/vmlinuz"),
                    ("initrd", f"{base_url}/initrd"),
                    ("init.cpio", f"{base_url}/init.cpio"),
                    ("grub/grub.cfg", f"{base_url}/grub/grub.cfg"),
                ):
                    target = artifact_dir / path
                    target.parent.mkdir(parents=True, exist_ok=True)
                    subprocess.run(
                        ["curl", "--fail", "--silent", "--show-error", "--interface", client_ip,
                         "--output", str(target), url],
                        check=True,
                    )
                boundary = artifact_dir / "boundary.img"
                artifact_size = sum(path.stat().st_size for path in artifact_dir.rglob("*") if path.is_file())
                boundary_size = max(64 * 1024 * 1024, artifact_size + max(32 * 1024 * 1024, artifact_size // 4))
                with boundary.open("wb") as stream:
                    stream.truncate(boundary_size)
                subprocess.run(["mkfs.vfat", "-n", "HTTPBOOT", str(boundary)], check=True)
                subprocess.run(["mmd", "-i", str(boundary), "::/EFI", "::/EFI/BOOT", "::/grub"], check=True)
                for source, target in (
                    ("http-entrypoint.efi", "::/EFI/BOOT/BOOTX64.EFI"),
                    ("grubx64.efi", "::/EFI/BOOT/grubx64.efi"),
                    ("vmlinuz", "::/vmlinuz"),
                    ("initrd", "::/initrd"),
                    ("init.cpio", "::/init.cpio"),
                    ("grub/grub.cfg", "::/grub/grub.cfg"),
                    ("grub/grub.cfg", "::/EFI/BOOT/grub.cfg"),
                ):
                    subprocess.run(["mcopy", "-o", "-i", str(boundary), str(artifact_dir / source), target], check=True)
                shutil.copyfile(boundary, self.efi_active)
                shutil.rmtree(artifact_dir, ignore_errors=True)

            self.with_client_address(stage)
        else:
            shutil.copyfile(self.efi_source, self.efi_active)

    def fetch_boot_entrypoint(self) -> None:
        boot_url = str(self.boot.get("HttpBootUri", ""))

        def fetch(client_ip: str) -> None:
            subprocess.run(
                ["curl", "--fail", "--silent", "--show-error", "--interface", client_ip,
                 "--output", "/dev/null", boot_url],
                check=True,
            )
            self.append("FIRMWARE_FETCH", boot_url, {"source": client_ip}, 200)

        self.with_client_address(fetch)

    def set_boot_order(self, target: str) -> None:
        if not self.manage_boot_order:
            return
        result = virsh("dumpxml", self.domain)
        root = ET.fromstring(result.stdout)
        os_element = root.find("os")
        if os_element is None:
            raise ValueError("libvirt domain has no os element")
        for element in os_element.findall("boot"):
            os_element.remove(element)
        devices = ("network", "hd") if target == "Pxe" else ("hd", "network")
        for device in devices:
            ET.SubElement(os_element, "boot", {"dev": device})
        with tempfile.NamedTemporaryFile(suffix=".xml") as stream:
            ET.ElementTree(root).write(stream.name, encoding="unicode")
            virsh("define", stream.name)

    def reset(self, reset_type: str) -> None:
        if reset_type == "ForceOff":
            virsh("destroy", self.domain, check=False)
        elif reset_type == "On":
            virsh("start", self.domain)
            # OVMF cannot consume Redfish static NIC settings. Emulate only its
            # initial fetch after power-on; EFI boundary preparation must not
            # advance Metalman's boot status.
            self.fetch_boot_entrypoint()
            # The staged disk substitutes only for firmware's initial HTTP
            # fetch. Remove it from the persistent domain after OVMF has loaded
            # shim/GRUB so the installer's reboot starts the written OS disk.
            threading.Thread(target=self.detach_efi_boundary, daemon=True).start()
        elif reset_type == "ForceRestart":
            if self.power_state() == "On":
                virsh("reset", self.domain)
            else:
                virsh("start", self.domain)
        else:
            raise ValueError(f"unsupported ResetType {reset_type!r}")

    def detach_efi_boundary(self) -> None:
        time.sleep(60)
        virsh("detach-disk", self.domain, "vdb", "--live", "--config", check=False)


class Handler(BaseHTTPRequestHandler):
    server: "Server"

    def log_message(self, fmt: str, *args: Any) -> None:
        print(f"redfish: {fmt % args}", flush=True)

    def body(self) -> dict[str, Any]:
        length = int(self.headers.get("Content-Length", "0"))
        if not length:
            return {}
        value = json.loads(self.rfile.read(length))
        if not isinstance(value, dict):
            raise ValueError("request body must be a JSON object")
        return value

    def reply(self, status: int, value: Any | None = None) -> None:
        data = b"" if value is None else json.dumps(value).encode()
        self.send_response(status)
        if data:
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        if data:
            self.wfile.write(data)

    def dispatch(self, method: str) -> None:
        state = self.server.state
        body: dict[str, Any] = {}
        status = 200
        response: Any | None = None
        try:
            if not state.authorized(self.headers.get("Authorization")):
                self.reply(401, {"error": {"message": "invalid Redfish credentials"}})
                return
            if method in ("PATCH", "POST"):
                body = self.body()

            if method == "GET" and self.path == "/redfish/v1/":
                response = {"Systems": {"@odata.id": "/redfish/v1/Systems"}}
            elif method == "GET" and self.path == "/redfish/v1/Systems":
                response = {"Members": [{"@odata.id": f"/redfish/v1/Systems/{state.domain}"}]}
            elif method == "GET" and self.path == f"/redfish/v1/Systems/{state.domain}":
                response = {
                    "Id": state.domain,
                    "PowerState": state.power_state(),
                    "Boot": state.boot,
                    "EthernetInterfaces": {
                        "@odata.id": f"/redfish/v1/Systems/{state.domain}/EthernetInterfaces"
                    },
                }
            elif method == "PATCH" and self.path == f"/redfish/v1/Systems/{state.domain}":
                boot = body.get("Boot", {})
                if boot.get("BootSourceOverrideTarget") == "UefiHttp":
                    if boot.get("BootSourceOverrideMode") != "UEFI":
                        raise ValueError("UefiHttp override must explicitly select UEFI mode")
                    if boot.get("BootSourceOverrideEnabled") != "Continuous":
                        raise ValueError("standard UefiHttp override must be continuous")
                    state.static_address()
                state.boot.update(boot)
                if boot.get("BootSourceOverrideTarget") == "UefiHttp":
                    state.set_efi_boundary(True)
                elif boot.get("BootSourceOverrideTarget") in ("Pxe", "Hdd"):
                    state.set_boot_order(str(boot["BootSourceOverrideTarget"]))
                if boot.get("BootSourceOverrideEnabled") == "Disabled":
                    if state.efi_source is not None:
                        state.set_efi_boundary(False)
                    state.set_boot_order("Hdd")
                status = 204
            elif method == "GET" and self.path.endswith("/EthernetInterfaces"):
                response = {"Members": [{"@odata.id": self.path + "/NIC.1"}]}
            elif method == "GET" and self.path.endswith("/EthernetInterfaces/NIC.1"):
                response = state.nic
            elif method == "PATCH" and self.path.endswith("/EthernetInterfaces/NIC.1"):
                if body.get("DHCPv4") != {"DHCPEnabled": False}:
                    raise ValueError("static EthernetInterface PATCH must disable DHCPv4")
                addresses = body.get("IPv4StaticAddresses")
                if not isinstance(addresses, list) or len(addresses) != 1:
                    raise ValueError("static EthernetInterface PATCH must contain one IPv4 address")
                state.nic.update(body)
                status = 204
            elif method == "POST" and self.path.endswith("/Actions/ComputerSystem.Reset"):
                state.reset(str(body.get("ResetType", "")))
                status = 204
            elif self.path.endswith("/Bios/Settings"):
                # Standard EthernetInterface + ComputerSystem.HttpBootUri is the
                # contract under test. Report vendor BIOS settings unsupported.
                status = 404
                response = {"error": {"message": "vendor BIOS settings unsupported"}}
            elif self.path.startswith("/redfish/v1/SessionService/"):
                status = 404
                response = {"error": {"message": "use basic authentication"}}
            else:
                status = 404
                response = {"error": {"message": "resource not found"}}
        except (OSError, ValueError, subprocess.CalledProcessError) as error:
            status = 500
            response = {"error": {"message": str(error)}}

        state.append(method, self.path, body, status)
        self.reply(status, response)

    def do_GET(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        self.dispatch("GET")

    def do_PATCH(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        self.dispatch("PATCH")

    def do_POST(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        self.dispatch("POST")


class Server(ThreadingHTTPServer):
    def __init__(self, address: tuple[str, int], state: State) -> None:
        super().__init__(address, Handler)
        self.state = state


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--bind", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=8443)
    parser.add_argument("--cert", required=True)
    parser.add_argument("--key", required=True)
    parser.add_argument("--domain", required=True)
    parser.add_argument("--mac", required=True)
    parser.add_argument("--record", required=True)
    parser.add_argument("--efi-source")
    parser.add_argument("--efi-active")
    parser.add_argument("--bridge")
    parser.add_argument("--cache-dir")
    parser.add_argument("--username", default="")
    parser.add_argument("--password", default="")
    parser.add_argument("--manage-boot-order", action="store_true")
    args = parser.parse_args()

    boundary_values = (args.efi_source, args.efi_active, args.bridge, args.cache_dir)
    if any(boundary_values) and not all(boundary_values):
        parser.error("--efi-source, --efi-active, --bridge, and --cache-dir must be used together")

    state = State(args)
    state.record.parent.mkdir(parents=True, exist_ok=True)
    state.record.touch()
    if state.efi_source is not None:
        state.set_efi_boundary(False)
    server = Server((args.bind, args.port), state)
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(args.cert, args.key)
    server.socket = context.wrap_socket(server.socket, server_side=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
