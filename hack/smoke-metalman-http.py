#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Layered Metalman UEFI HTTP provisioning contract smoke test.

NOTE: This test is not currently wired into CI. Under Cloud Hypervisor the
CloudHv OVMF firmware does not auto-create a UEFI HTTPv4 boot option (only
PXEv4/PXEv6), so firmware-native UEFI HTTP boot does not trigger and this test
cannot pass as written. It is retained for when firmware-native HTTP boot is
revisited. The PXE path (hack/smoke-metalman.py) is the supported smoke test.

Metalman writes standard Redfish static IPv4 and UefiHttp settings. The
recording BMC fixture translates those settings into a dnsmasq configuration
bound to the boundary bridge: the static address becomes a DHCP reservation and
the HttpBootUri becomes the UEFI HTTP boot URL. Stock OVMF then performs a
genuine firmware-native UEFI HTTP boot, fetching the boot entrypoint over HTTP
itself. From shim/GRUB onward the real kernel, installer initrd, machine image,
cloud-init, and branch-built agent path runs unchanged.

Upstream OVMF HttpBootDxe always DHCPs, so the node address and boot URL are
delivered via a DHCP reservation rather than firmware static configuration.
Metalman's Redfish behavior is exercised and asserted unchanged.
"""

from __future__ import annotations

import atexit
import importlib.util
import json
import os
import shutil
import signal
import subprocess
import sys
import tarfile
import tempfile
import textwrap
import time
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location("metalman_pxe_smoke", ROOT / "hack/smoke-metalman.py")
assert spec and spec.loader
smoke = importlib.util.module_from_spec(spec)
spec.loader.exec_module(smoke)

TMP = Path(tempfile.mkdtemp(prefix="metalman-http-"))
os.chmod(TMP, 0o755)
VM = "unbounded-metal-http-smoke"
BRIDGE = "virbr-http"
SITE = "http-smoke"
NODE = "http-smoke-node"
MAC = "52:54:00:aa:bc:01"
SERVER_IP = "192.168.200.1"
NODE_IP = "192.168.200.10"
KIND_IP = "192.168.200.2"
HTTP_PORT = 8882
AGENT_PORT = 8883
REDFISH_PORT = 8444
REGISTRY_PORT = 5556
DHCP_PORT = 6768
REGISTRY = "unbounded-http-smoke-registry"
HOST_IMAGE = "localhost:5556/unbounded/host-ubuntu2404:http-smoke"
NETBOOT_IMAGE = "localhost:5556/unbounded/netboot:http-smoke"
AGENT_IMAGE = "localhost:5556/unbounded/agent-ubuntu2404:http-smoke"
AGENT_IMAGE_VM = f"{SERVER_IP}:{REGISTRY_PORT}/unbounded/agent-ubuntu2404:http-smoke"
SERVE_URL = f"http://{SERVER_IP}:{HTTP_PORT}"
RECORD = TMP / "redfish.jsonl"
PCAP = TMP / "traffic.pcap"

# The fixture launches cloud-hypervisor directly and owns all VM state under
# this directory. The secure-boot CloudHv OVMF firmware blob is built by
# hack/scripts/build-cloudhv-firmware.sh; it auto-enrolls the fixture-supplied
# SMBIOS Platform Key so SecureBoot=1/SetupMode=0 on every cold boot.
STATE_DIR = TMP / "vmstate"
DISK = TMP / "os.qcow2"
FIRMWARE_SECUREBOOT = str(ROOT / "bin" / "cloudhv-firmware" / "CLOUDHV_SECUREBOOT.fd")
SERIAL_SOCK = STATE_DIR / "console.sock"
REDFISH_URL = f"https://127.0.0.1:{REDFISH_PORT}"
REDFISH_USER = "smoke"
REDFISH_PASS = "smoke"
PROCS: list[subprocess.Popen[Any]] = []
PASSED = False

# Point reused wait/guest helpers at this isolated test's resources.
smoke.TMPDIR = TMP
smoke.VM_NAME = VM
smoke.NODE_NAME = NODE
smoke.SITE = SITE
smoke.SERVER_IP = SERVER_IP
smoke.NODE_IP = NODE_IP
smoke.KIND_SMOKE_IP = KIND_IP
smoke.MAC_ADDRESS = MAC
smoke.STATE_DIR = STATE_DIR
smoke.SERIAL_SOCK = SERIAL_SOCK
smoke.REDFISH_URL = REDFISH_URL
smoke.REDFISH_PORT = REDFISH_PORT
smoke.REDFISH_USER = REDFISH_USER
smoke.REDFISH_PASS = REDFISH_PASS
smoke._procs = PROCS
# The shared serial-console automation driver captured the PXE test's socket
# path at import; rebind it to this test's console.sock.
smoke._console = smoke._SerialConsole(SERIAL_SOCK)


def log(message: str) -> None:
    print(f"==> {message}", file=sys.stderr, flush=True)


def run(args: list[str], **kwargs: Any) -> subprocess.CompletedProcess[str]:
    return subprocess.run(args, check=True, **kwargs)


def quiet(args: list[str]) -> None:
    subprocess.run(args, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)


def spawn(args: list[str], name: str) -> subprocess.Popen[Any]:
    process = smoke.spawn(args, TMP / name)
    return process


def cleanup() -> None:
    log("Cleaning up HTTP smoke resources")
    for process in PROCS:
        try:
            os.killpg(process.pid, signal.SIGTERM)
        except OSError as exc:
            log(f"Could not send SIGTERM to process group {process.pid}; continuing cleanup: {exc}")
    for process in PROCS:
        try:
            process.wait(timeout=5)
        except (OSError, subprocess.TimeoutExpired):
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except OSError:
                # The process group may already have exited during teardown.
                pass
    for args in (
        ["sudo", "pkill", "-f", "metalman-redfish-fixture"],
        ["sudo", "pkill", "-f", f"cloud-hypervisor.*{STATE_DIR}"],
        ["sudo", "pkill", "-f", f"swtpm.*{STATE_DIR}"],
        ["sudo", "ip", "link", "delete", "veth-kind-http"],
        ["sudo", "ip", "link", "delete", BRIDGE],
        ["docker", "rm", "-f", REGISTRY],
        # The fixture (run under sudo) launches dnsmasq as root; ensure it is
        # gone even if the fixture was killed before it could tear it down.
        ["sudo", "pkill", "-f", str(TMP / "dnsmasq" / "dnsmasq.conf")],
    ):
        quiet(args)
    for table_args in (
        ["sudo", "iptables", "-D", "FORWARD", "-i", BRIDGE, "-j", "ACCEPT"],
        ["sudo", "iptables", "-D", "FORWARD", "-o", BRIDGE, "-j", "ACCEPT"],
        ["sudo", "iptables", "-t", "raw", "-D", "PREROUTING", "-i", BRIDGE, "-j", "ACCEPT"],
    ):
        quiet(table_args)
    if PASSED:
        shutil.rmtree(TMP, ignore_errors=True)
    else:
        log(f"Preserving failure diagnostics in {TMP}")


def write(path: Path, content: str) -> None:
    path.write_text(textwrap.dedent(content), encoding="utf-8")


def attach_kind_network() -> None:
    log("Attaching the kind control plane to the fixture-created bridge")
    # The fixture owns the bridge, host address, NAT, and bridge FORWARD rules.
    # This raw-table accept keeps conntrack from dropping the cross-bridge kind
    # traffic and is not installed by the fixture.
    run(["sudo", "iptables", "-t", "raw", "-I", "PREROUTING", "-i", BRIDGE, "-j", "ACCEPT"])
    kind_pid = run(
        ["docker", "inspect", "kind-control-plane", "--format", "{{.State.Pid}}"],
        capture_output=True, text=True,
    ).stdout.strip()
    run(["sudo", "ip", "link", "add", "veth-kind-http", "type", "veth", "peer", "name", "eth-http"])
    run(["sudo", "ip", "link", "set", "veth-kind-http", "master", BRIDGE])
    run(["sudo", "ip", "link", "set", "veth-kind-http", "up"])
    run(["sudo", "ip", "link", "set", "eth-http", "netns", kind_pid])
    run(["sudo", "nsenter", "-t", kind_pid, "-n", "ip", "addr", "add", f"{KIND_IP}/24", "dev", "eth-http"])
    run(["sudo", "nsenter", "-t", kind_pid, "-n", "ip", "link", "set", "eth-http", "up"])
    smoke.configure_kind_control_plane_node_ip("kind-control-plane", KIND_IP)
    patch = json.dumps({"spec": {"template": {"spec": {"containers": [{
        "name": "kindnet-cni",
        "env": [{"name": "CONTROL_PLANE_ENDPOINT", "value": f"{KIND_IP}:6443"}],
    }]}}}})
    run(["kubectl", "-n", "kube-system", "patch", "daemonset", "kindnet", "--type=strategic", "-p", patch])
    smoke.configure_kind_kube_proxy_apiserver(f"https://{KIND_IP}:6443")


def wait_for_bridge(timeout: int = 60) -> None:
    log(f"Waiting for the fixture to create bridge {BRIDGE}")
    for _ in range(timeout):
        if subprocess.run(["ip", "link", "show", BRIDGE],
                          stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0:
            return
        for process in PROCS:
            if process.poll() is not None:
                raise RuntimeError("fixture exited before creating the bridge")
        time.sleep(1)
    raise RuntimeError(f"bridge {BRIDGE} was not created within {timeout}s")



def build_and_prepare() -> None:
    log("Building binaries and creating the empty guest disk")
    run(["make", "machina-manifests"], cwd=ROOT)
    for output, package in (("metalman", "./cmd/metalman"), ("unbounded-agent", "./cmd/agent"), ("metalman-redfish-fixture", "./hack/metalman-redfish-fixture")):
        run(["go", "build", "-o", str(ROOT / "bin" / output), package], cwd=ROOT)
    STATE_DIR.mkdir(parents=True, exist_ok=True)
    os.chmod(STATE_DIR, 0o755)
    run(["qemu-img", "create", "-f", "qcow2", str(DISK), "20G"])


def setup_kubernetes_and_images() -> None:
    log("Applying Metalman RBAC/CRDs and publishing OCI images")
    run(["kubectl", "apply", "--server-side", "--force-conflicts", "-f", str(ROOT / "deploy/machina/rendered/01-namespace.yaml")])


def setup_kubernetes_and_images() -> None:
    log("Building binaries and applying Metalman RBAC/CRDs")
    run(["make", "machina-manifests"], cwd=ROOT)
    for output, package in (("metalman", "./cmd/metalman"), ("unbounded-agent", "./cmd/agent"), ("metalman-redfish-fixture", "./hack/metalman-redfish-fixture")):
        run(["go", "build", "-o", str(ROOT / "bin" / output), package], cwd=ROOT)
    run(["kubectl", "apply", "--server-side", "--force-conflicts", "-f", str(ROOT / "deploy/machina/rendered/01-namespace.yaml")])
    run(["kubectl", "apply", "--server-side", "--force-conflicts", "-f", str(ROOT / "deploy/machina/crd")])
    run(["kubectl", "apply", "--server-side", "--force-conflicts", "-f", str(ROOT / "deploy/machina/rendered/06-metalman-rbac.yaml")])
    for resource in (
        f"machineoperation/http-smoke-host-replace",
        f"machine/{NODE}",
        f"node/{NODE}",
        "secret/http-bmc-pass",
        "configmap/http-smoke-user-data",
    ):
        subprocess.run(
            ["kubectl", "delete", resource, "--ignore-not-found", "--wait=true"],
            stdout=subprocess.DEVNULL,
            check=False,
        )
    run(["kubectl", "-n", "default", "create", "secret", "generic", "http-bmc-pass", "--from-literal=password=smoke"])
    user_data = TMP / "user-data.yaml"
    write(user_data, """
        #cloud-config
        runcmd:
          - [/bin/bash, -c, "export UNBOUNDED_AGENT_CONFIG_FILE=/etc/unbounded/agent/config.json; bash /usr/local/bin/unbounded-agent-install.sh"]
    """)
    run(["kubectl", "-n", "default", "create", "configmap", "http-smoke-user-data",
         f"--from-file=user-data={user_data}"])
    run(["docker", "run", "-d", "--name", REGISTRY, "-p", f"{REGISTRY_PORT}:5000", "registry:2"])
    for _ in range(30):
        probe = subprocess.run(
            ["curl", "--fail", "--silent", f"http://127.0.0.1:{REGISTRY_PORT}/v2/"],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        if probe.returncode == 0:
            break
        time.sleep(0.5)
    else:
        raise RuntimeError("local OCI registry did not become ready")
    for image in (HOST_IMAGE, NETBOOT_IMAGE, AGENT_IMAGE):
        run(["docker", "push", image])
    artifact_dir = TMP / "agent"
    artifact_dir.mkdir()
    tarball = artifact_dir / "unbounded-agent-linux-amd64.tar.gz"
    with tarfile.open(tarball, "w:gz") as archive:
        archive.add(ROOT / "bin/unbounded-agent", arcname="unbounded-agent")
    spawn([sys.executable, "-m", "http.server", str(AGENT_PORT), "--bind", SERVER_IP,
           "--directory", str(artifact_dir)], "agent-http.log")


def start_fixture() -> None:
    run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
         "-subj", "/CN=metalman-http-fixture", "-addext", "subjectAltName=IP:127.0.0.1",
         "-keyout", str(TMP / "redfish.key"), "-out", str(TMP / "redfish.crt")])
    dnsmasq_dir = TMP / "dnsmasq"
    dnsmasq_dir.mkdir(exist_ok=True)
    # Run the fixture under sudo so it can create the bridge and NAT, launch
    # cloud-hypervisor with KVM, and start the dnsmasq that binds the privileged
    # DHCP port (UDP 67) on the boundary bridge.
    spawn(["sudo", "env", f"PATH={os.environ['PATH']}",
           str(ROOT / "bin/metalman-redfish-fixture"),
           "--domain", VM, "--mac", MAC, "--port", str(REDFISH_PORT),
            "--cert", str(TMP / "redfish.crt"), "--key", str(TMP / "redfish.key"),
            "--record", str(RECORD), "--bridge", BRIDGE,
            "--bridge-address", SERVER_IP,
            "--disk", str(DISK), "--state-dir", str(STATE_DIR),
            "--firmware-secureboot", FIRMWARE_SECUREBOOT, "--secure-boot",
            "--dnsmasq-dir", str(dnsmasq_dir), "--username", "smoke", "--password", "smoke"],
          "redfish.log")
    time.sleep(1)


def start_metalman_and_replace() -> None:
    kubeconfig = TMP / "metalman.kubeconfig"
    smoke.write_service_account_kubeconfig("unbounded-kube", "metalman-controller", kubeconfig)
    # The guest cannot resolve Docker's kind-control-plane hostname. Use the
    # control-plane address attached directly to this test's L2 network.
    api_url = f"https://{KIND_IP}:6443"
    spawn(["sudo", "env", f"PATH={os.environ['PATH']}", f"KUBECONFIG={kubeconfig}",
           f"METALMAN_APISERVER_URL={api_url}", str(ROOT / "bin/metalman"), "serve-pxe",
            f"--site={SITE}", f"--bind-address={SERVER_IP}", f"--serve-url={SERVE_URL}",
            f"--http-port={HTTP_PORT}",
            f"--dhcp-port={DHCP_PORT}",
            f"--cache-dir={TMP / 'cache'}", f"--default-netboot-image={NETBOOT_IMAGE}"],
          "metalman.log")
    machine = {
        "apiVersion": "unbounded-cloud.io/v1alpha3", "kind": "Machine",
        "metadata": {"name": NODE, "labels": {"unbounded-cloud.io/site": SITE}},
        "spec": {
            "pxe": {
                "image": HOST_IMAGE, "netbootImage": NETBOOT_IMAGE, "bootProtocol": "HTTP",
                "targetDisk": "/dev/vda",
                "dhcpLeases": [{"mac": MAC, "ipv4": NODE_IP, "subnetMask": "255.255.255.0",
                                "gateway": SERVER_IP, "dns": ["8.8.8.8"]}],
                "redfish": {"url": f"https://127.0.0.1:{REDFISH_PORT}", "username": "smoke",
                             "deviceID": VM, "passwordRef": {"name": "http-bmc-pass",
                             "namespace": "default", "key": "password"}},
                "cloudInit": {"userDataConfigMapRef": {"name": "http-smoke-user-data",
                                                        "namespace": "default", "key": "user-data"}},
            },
            "agent": {"image": AGENT_IMAGE_VM,
                      "url": f"http://{SERVER_IP}:{AGENT_PORT}/unbounded-agent-linux-amd64.tar.gz"},
        },
    }
    run(["kubectl", "apply", "-f", "-"], input=json.dumps(machine), text=True)
    for _ in range(120):
        fingerprint = subprocess.run(
            ["kubectl", "get", "machine", NODE, "-o", "jsonpath={.status.redfish.certFingerprint}"],
            capture_output=True, text=True,
        ).stdout.strip()
        if fingerprint:
            break
        time.sleep(1)
    else:
        raise RuntimeError("Redfish certificate fingerprint was not recorded")

    log("Waiting for host and netboot OCI images to be cached")
    cache_dir = TMP / "cache" / "oci"
    metalman_log = TMP / "metalman.log"
    for _ in range(300):
        disk_dirs = list(cache_dir.glob("*/amd64/disk"))
        host_ready = any((path / "disk.img.gz").is_file() for path in disk_dirs)
        netboot_ready = any((path / "bootx64.efi").is_file() for path in disk_dirs)
        log_text = metalman_log.read_text(encoding="utf-8") if metalman_log.exists() else ""
        host_published = any("OCI image cached" in line and f"image={HOST_IMAGE}" in line
                             for line in log_text.splitlines())
        netboot_published = any("OCI image cached" in line and f"image={NETBOOT_IMAGE}" in line
                                for line in log_text.splitlines())
        if host_ready and netboot_ready and host_published and netboot_published:
            break
        time.sleep(1)
    else:
        raise RuntimeError("host and netboot OCI images were not cached within 5 minutes")

    operation = smoke.create_machine_operation(
        "http-smoke-host-replace", "HostReplace", machine_ref=NODE
    )
    smoke.wait_machine_operation_complete(operation, timeout=1800)


def fixture_writes() -> list[dict[str, Any]]:
    return [json.loads(line) for line in RECORD.read_text(encoding="utf-8").splitlines()]


def assert_contract() -> None:
    writes = fixture_writes()
    nic_patches = [entry["body"] for entry in writes
                   if entry["method"] == "PATCH" and entry["path"].endswith("EthernetInterfaces/NIC.1")]
    expected_address = {"Address": NODE_IP, "SubnetMask": "255.255.255.0", "Gateway": SERVER_IP}
    if not any(body.get("DHCPv4") == {"DHCPEnabled": False}
               and body.get("IPv4StaticAddresses") == [expected_address]
               and body.get("StaticNameServers") == ["8.8.8.8"] for body in nic_patches):
        raise AssertionError(f"standard static EthernetInterface PATCH not recorded: {nic_patches}")
    boot_patches = [entry["body"].get("Boot", {}) for entry in writes
                    if entry["method"] == "PATCH" and entry["path"] == f"/redfish/v1/Systems/{VM}"]
    if not any(boot.get("BootSourceOverrideTarget") == "UefiHttp"
                and boot.get("BootSourceOverrideEnabled") == "Continuous"
                and boot.get("BootSourceOverrideMode") == "UEFI"
                and boot.get("HttpBootUri") == f"{SERVE_URL}/bootx64.efi" for boot in boot_patches):
        raise AssertionError(f"standard UefiHttp PATCH not recorded: {boot_patches}")


def assert_guest_network() -> str:
    smoke.wait_guest_agent(timeout=600)
    command = f"""
        set -eu
        ip -4 address show | grep -F '{NODE_IP}/24'
        ip route show default | grep -F 'via {SERVER_IP}'
        test "$(od -An -t u1 -j 4 -N 1 /sys/firmware/efi/efivars/SecureBoot-* | tr -d ' ')" = 1
        test "$(od -An -t u1 -j 4 -N 1 /sys/firmware/efi/efivars/SetupMode-* | tr -d ' ')" = 0
        grep -R -F '{NODE_IP}/24' /etc/netplan /etc/cloud/cloud.cfg.d
        ! grep -R -E '(^|[^a-z])dhcp4:[[:space:]]*true' /etc/netplan /etc/cloud/cloud.cfg.d
        test ! -s /var/lib/dhcp/dhclient.leases
        test -z "$(find /run/systemd/netif/leases /var/lib/NetworkManager -type f \\
          \\( -name '*.lease' -o -name 'lease-*' \\) -size +0c 2>/dev/null)"
        cat /proc/sys/kernel/random/boot_id
    """
    code, stdout, stderr = smoke.guest_exec(command, timeout=60)
    if code != 0:
        raise AssertionError(f"guest static network assertion failed: {stdout}\n{stderr}")
    return stdout.splitlines()[-1].strip()


def assert_dhcp_httpboot() -> None:
    # Firmware-native UEFI HTTP boot DHCPs to learn its reserved address and boot
    # URL, so DHCP from the node MAC is now expected.
    dhcp = run(
        ["sudo", "tcpdump", "-nn", "-r", str(PCAP), "ether", "src", MAC, "and",
         "(udp port 67 or udp port 68)"],
        capture_output=True, text=True,
    )
    if not dhcp.stdout.strip():
        raise AssertionError(f"no firmware DHCP traffic from {MAC}; native HTTP boot did not start")
    # The node must then fetch the boot entrypoint over HTTP from Metalman,
    # proving a genuine firmware-native HTTP boot rather than a staged disk.
    http_fetch = run(
        ["sudo", "tcpdump", "-nn", "-r", str(PCAP), "ether", "src", MAC, "and",
         "tcp", "and", "dst", "host", SERVER_IP, "and", "dst", "port", str(HTTP_PORT)],
        capture_output=True, text=True,
    )
    if not http_fetch.stdout.strip():
        raise AssertionError(f"no HTTP boot fetch from {MAC} to {SERVER_IP}:{HTTP_PORT}")


def main() -> None:
    global PASSED

    atexit.register(cleanup)
    build_and_prepare()
    start_fixture()
    wait_for_bridge()
    # Capture starts as early as the bridge exists (guest still powered off) and
    # remains active through installer power-on, installer reboot, OS validation,
    # and the final reboot.
    tcpdump = spawn(["sudo", "tcpdump", "-U", "-i", BRIDGE, "-w", str(PCAP)], "tcpdump.log")
    time.sleep(1)
    attach_kind_network()
    setup_kubernetes_and_images()
    start_metalman_and_replace()
    assert_contract()
    first_boot = assert_guest_network()
    log("Rebooting the installed guest and reasserting static networking")
    code, _, stderr = smoke.guest_exec(
        "nohup sh -c 'sleep 1; systemctl reboot' >/dev/null 2>&1 &", timeout=30
    )
    if code != 0:
        raise AssertionError(f"guest reboot failed: {stderr}")
    time.sleep(10)
    smoke.wait_vm_state("running", timeout=180)
    smoke.wait_guest_agent(timeout=600)
    second_boot = assert_guest_network()
    if first_boot == second_boot:
        raise AssertionError("guest boot ID did not change after reboot")
    tcpdump.send_signal(signal.SIGINT)
    tcpdump.wait(timeout=15)
    PROCS.remove(tcpdump)
    assert_dhcp_httpboot()
    PASSED = True
    log("Layered UEFI HTTP provisioning smoke PASSED")


if __name__ == "__main__":
    main()
