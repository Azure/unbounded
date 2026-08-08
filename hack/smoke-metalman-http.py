#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Layered DHCP-free Metalman HTTP provisioning contract smoke test.

Stock Noble OVMF and sushy cannot emulate firmware-native DHCP-free UEFI HTTP.
The VM therefore starts at the post-firmware EFI boundary: after Metalman has
written standard Redfish static IPv4 and UefiHttp settings, the recording BMC
fetches the real Metalman boot artifacts with the VM's source IP and exposes
them on an EFI disk. From shim/GRUB onward the real kernel, installer initrd,
machine image, cloud-init, and branch-built agent path runs unchanged.
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
NETWORK = "unbounded-metal-http-smoke"
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
VIRSH = ["virsh", "--connect", "qemu:///system"]
PROCS: list[subprocess.Popen[Any]] = []
PASSED = False

# Point reused wait/guest helpers at this isolated test's resources.
smoke.TMPDIR = TMP
smoke.VM_NAME = VM
smoke.NET_NAME = NETWORK
smoke.NODE_NAME = NODE
smoke.SITE = SITE
smoke.SERVER_IP = SERVER_IP
smoke.NODE_IP = NODE_IP
smoke.KIND_SMOKE_IP = KIND_IP
smoke.MAC_ADDRESS = MAC
smoke.VIRSH = VIRSH
smoke._procs = PROCS


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
        [*VIRSH, "destroy", VM],
        [*VIRSH, "undefine", VM, "--nvram"],
        [*VIRSH, "net-destroy", NETWORK],
        [*VIRSH, "net-undefine", NETWORK],
        ["sudo", "ip", "link", "delete", "veth-kind-http"],
        ["docker", "rm", "-f", REGISTRY],
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


def setup_network() -> None:
    log("Creating DHCP-free libvirt network")
    network_xml = TMP / "network.xml"
    write(network_xml, f"""
        <network>
          <name>{NETWORK}</name>
          <forward mode='nat'/>
          <bridge name='{BRIDGE}'/>
          <ip address='{SERVER_IP}' netmask='255.255.255.0'/>
        </network>
    """)
    run([*VIRSH, "net-define", str(network_xml)])
    run([*VIRSH, "net-start", NETWORK])
    for args in (
        ["sudo", "iptables", "-I", "FORWARD", "-i", BRIDGE, "-j", "ACCEPT"],
        ["sudo", "iptables", "-I", "FORWARD", "-o", BRIDGE, "-j", "ACCEPT"],
        ["sudo", "iptables", "-t", "raw", "-I", "PREROUTING", "-i", BRIDGE, "-j", "ACCEPT"],
    ):
        run(args)
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


def create_vm() -> tuple[Path, Path]:
    log("Defining powered-off VM at the documented post-firmware EFI boundary")
    os_disk = TMP / "os.qcow2"
    blank_efi = TMP / "blank-efi.img"
    active_efi = TMP / "active-efi.img"
    nvram = TMP / "OVMF_VARS.fd"
    run(["qemu-img", "create", "-f", "qcow2", str(os_disk), "20G"])
    run(["truncate", "-s", "128M", str(blank_efi)])
    run(["mkfs.vfat", "-n", "HTTPBOOT", str(blank_efi)])
    shutil.copyfile(blank_efi, active_efi)
    shutil.copyfile("/usr/share/OVMF/OVMF_VARS_4M.ms.fd", nvram)
    domain = TMP / "domain.xml"
    write(domain, f"""
        <domain type='kvm'>
          <name>{VM}</name><memory unit='MiB'>4096</memory><vcpu>2</vcpu>
          <os><type arch='x86_64' machine='q35'>hvm</type>
            <loader readonly='yes' secure='yes' type='pflash'>/usr/share/OVMF/OVMF_CODE_4M.ms.fd</loader>
            <nvram>{nvram}</nvram>
          </os>
          <features><acpi/><apic/><smm state='on'/></features><cpu mode='host-passthrough'/>
          <devices>
            <disk type='file' device='disk'><driver name='qemu' type='raw'/><source file='{active_efi}'/><target dev='vdb' bus='virtio'/><boot order='1'/></disk>
            <disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='{os_disk}'/><target dev='vda' bus='virtio'/><boot order='2'/></disk>
            <interface type='network'><mac address='{MAC}'/><source network='{NETWORK}'/><model type='virtio'/></interface>
            <tpm model='tpm-tis'><backend type='emulator' version='2.0'/></tpm>
            <serial type='file'><source path='{TMP / "console.log"}'/><target port='0'/></serial>
            <channel type='unix'><source mode='bind' path='{TMP / "qga.sock"}'/><target type='virtio' name='org.qemu.guest_agent.0'/></channel>
          </devices>
        </domain>
    """)
    run([*VIRSH, "define", str(domain)])
    return blank_efi, active_efi


def setup_kubernetes_and_images() -> None:
    log("Building binaries and applying Metalman RBAC/CRDs")
    run(["make", "machina-manifests"], cwd=ROOT)
    for output, package in (("metalman", "./cmd/metalman"), ("unbounded-agent", "./cmd/agent")):
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
        packages:
          - qemu-guest-agent
        runcmd:
          - [systemctl, start, qemu-guest-agent]
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


def start_fixture(blank_efi: Path, active_efi: Path) -> None:
    run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
         "-subj", "/CN=metalman-http-fixture", "-addext", "subjectAltName=IP:127.0.0.1",
         "-keyout", str(TMP / "redfish.key"), "-out", str(TMP / "redfish.crt")])
    spawn([sys.executable, str(ROOT / "hack/metalman-redfish-fixture.py"),
           "--domain", VM, "--mac", MAC, "--port", str(REDFISH_PORT),
            "--cert", str(TMP / "redfish.crt"), "--key", str(TMP / "redfish.key"),
            "--record", str(RECORD), "--efi-source", str(blank_efi),
            "--efi-active", str(active_efi), "--bridge", BRIDGE,
            "--cache-dir", str(TMP / "cache"), "--username", "smoke", "--password", "smoke"],
          "redfish.log")
    time.sleep(1)


def start_metalman_and_replace() -> None:
    kubeconfig = TMP / "metalman.kubeconfig"
    smoke.write_service_account_kubeconfig(smoke.METALMAN_NAMESPACE, "metalman-controller", kubeconfig)
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
    firmware_fetches = [entry for entry in writes if entry["method"] == "FIRMWARE_FETCH"]
    if not any(entry["path"] == f"{SERVE_URL}/bootx64.efi"
               and entry["body"] == {"source": NODE_IP} for entry in firmware_fetches):
        raise AssertionError(f"state-derived post-power-on firmware fetch not recorded: {firmware_fetches}")


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


def assert_no_dhcp() -> None:
    guest_traffic = run(
        ["sudo", "tcpdump", "-nn", "-r", str(PCAP), "ether", "src", MAC, "and", "not",
         "(udp port 67 or udp port 68 or udp port 546 or udp port 547)"],
        capture_output=True, text=True,
    )
    if not guest_traffic.stdout.strip():
        raise AssertionError(f"packet capture contains no non-DHCP traffic from {MAC}")
    result = run(
        ["sudo", "tcpdump", "-nn", "-r", str(PCAP), "ether", "src", MAC, "and",
         "(udp port 67 or udp port 68 or udp port 546 or udp port 547)"],
        capture_output=True, text=True,
    )
    if result.stdout.strip():
        raise AssertionError(f"DHCP traffic emitted by {MAC}:\n{result.stdout}")


def main() -> None:
    global PASSED

    atexit.register(cleanup)
    setup_network()
    blank_efi, active_efi = create_vm()
    # Capture starts while the domain is still off and remains active through
    # installer power-on, installer reboot, OS validation, and the final reboot.
    tcpdump = spawn(["sudo", "tcpdump", "-U", "-i", BRIDGE, "-w", str(PCAP)], "tcpdump.log")
    time.sleep(1)
    setup_kubernetes_and_images()
    start_fixture(blank_efi, active_efi)
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
    assert_no_dhcp()
    PASSED = True
    log("Layered DHCP-free HTTP provisioning smoke PASSED")


if __name__ == "__main__":
    main()
