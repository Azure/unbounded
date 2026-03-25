#!/usr/bin/env python3

import argparse
import base64
import hashlib
import json
import os
import shutil
import subprocess
import sys
import tempfile
import urllib.request
import yaml

UBUNTU_VERSION = "24.04.2"
UBUNTU_NETBOOT_BASE = f"https://releases.ubuntu.com/{UBUNTU_VERSION}/netboot/amd64"
UBUNTU_CLOUDIMG_BASE = "https://cloud-images.ubuntu.com/releases/noble/release"
UBUNTU_CLOUDIMG_URL = f"{UBUNTU_CLOUDIMG_BASE}/ubuntu-24.04-server-cloudimg-amd64.img"

NETBOOT_FILES = [
    ("shimx64.efi", "bootx64.efi"),
    ("grubx64.efi", "grubx64.efi"),
    ("vmlinuz", "linux"),
    ("initrd", "initrd"),
]

ASSETS_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "assets")

GRUB_CFG = (
    "{{ if le .Machine.Spec.Operations.ReimageCounter .Machine.Status.Operations.ReimageCounter }}\n"
    "insmod part_gpt\n"
    "search --no-floppy --set=root --file /EFI/ubuntu/shimx64.efi\n"
    "chainloader /EFI/ubuntu/shimx64.efi\n"
    "boot\n"
    "{{ end }}\n"
    "set default=0\n"
    "set timeout=0\n"
    "\n"
    'menuentry "Unbounded Metal Install" {\n'
    "  linux /vmlinuz \\\n"
    "    unbounded.image_url={{ .ServeURL }}/disk.img.gz \\\n"
    "    unbounded.serve_url={{ .ServeURL }} \\\n"
    "    unbounded.ds_url={{ .ServeURL }}/cloud-init/ \\\n"
    "    unbounded.node_name={{ .Machine.Name }} \\\n"
    "    unbounded.node_namespace={{ .Machine.Namespace }} \\\n"
    "    unbounded.apiserver_url={{ .ApiserverURL }} \\\n"
    "    ip={{ (index .Machine.Spec.PXE.DHCPLeases 0).IPv4 }}::{{ (index .Machine.Spec.PXE.DHCPLeases 0).Gateway }}:{{ (index .Machine.Spec.PXE.DHCPLeases 0).SubnetMask }}::eth0:none \\\n"
    "    console=tty0 console=ttyS0,115200n8 \\\n"
    "    ---\n"
    "  initrd /initrd /init.cpio\n"
    "}\n"
)

META_DATA_TPL = (
    "instance-id: {{ .Machine.Name }}\n"
    "local-hostname: {{ .Machine.Name }}\n"
)

VENDOR_DATA_TPL = (
    "#cloud-config\n"
    "reporting:\n"
    "  unbounded:\n"
    "    type: webhook\n"
    "    endpoint: {{ .ServeURL }}/cloudinit/log\n"
)

BOOTSTRAP_KUBECONFIG_TPL = (
    "apiVersion: v1\n"
    "kind: Config\n"
    "clusters:\n"
    "- cluster:\n"
    "    certificate-authority: /etc/kubernetes/pki/ca.crt\n"
    "    server: {{ .ApiserverURL }}\n"
    "  name: default\n"
    "contexts:\n"
    "- context:\n"
    "    cluster: default\n"
    "    user: kubelet-bootstrap\n"
    "  name: default\n"
    "current-context: default\n"
    "users:\n"
    "- name: kubelet-bootstrap\n"
    "  user:\n"
    "    exec:\n"
    "      apiVersion: client.authentication.k8s.io/v1\n"
    "      command: /usr/local/bin/unbounded-metal-attest\n"
    "      interactiveMode: Never\n"
)

KUBELET_DROPIN = (
    "[Service]\n"
    'ExecStartPre=/bin/bash -c "test -f /etc/kubernetes/kubelet.conf || /usr/local/bin/unbounded-metal-attest 2>/dev/ttyS0"\n'
    "ExecStart=\n"
    "ExecStart=/usr/bin/kubelet --bootstrap-kubeconfig=/etc/kubernetes/bootstrap-kubelet.conf --kubeconfig=/etc/kubernetes/kubelet.conf --config=/var/lib/kubelet/config.yaml --node-labels=kubernetes.azure.com/managed=false,kubernetes.azure.com/cluster=unbounded-metal --v=2\n"
    "StandardOutput=journal+console\n"
    "StandardError=journal+console\n"
    "TTYPath=/dev/ttyS0\n"
)

KUBELET_CONFIG_TPL = (
    "apiVersion: kubelet.config.k8s.io/v1beta1\n"
    "kind: KubeletConfiguration\n"
    "cgroupDriver: systemd\n"
    "authentication:\n"
    "  anonymous:\n"
    "    enabled: false\n"
    "  webhook:\n"
    "    enabled: true\n"
    "  x509:\n"
    "    clientCAFile: /etc/kubernetes/pki/ca.crt\n"
    "authorization:\n"
    "  mode: Webhook\n"
    "clusterDNS:\n"
    "- 10.96.0.10\n"
    "clusterDomain: cluster.local\n"
    "rotateCertificates: true\n"
    "serverTLSBootstrap: true\n"
)


def die(msg):
    print(f"error: {msg}", file=sys.stderr)
    sys.exit(1)


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 16), b""):
            h.update(chunk)
    return h.hexdigest()


def download_cached(url, dest):
    name = os.path.basename(dest)
    if os.path.isfile(dest):
        print(f"  {name}: cached", file=sys.stderr)
    else:
        print(f"  {name}: downloading", file=sys.stderr)
        urllib.request.urlretrieve(url, dest)
    sha = sha256_file(dest)
    print(f"  {name}: {sha}", file=sys.stderr)
    return sha


def read_asset(name):
    with open(os.path.join(ASSETS_DIR, name)) as f:
        return f.read()


def fetch_cloudimg_sha256():
    with urllib.request.urlopen(f"{UBUNTU_CLOUDIMG_BASE}/SHA256SUMS") as resp:
        sums = resp.read().decode()
    for line in sums.splitlines():
        if "ubuntu-24.04-server-cloudimg-amd64.img" in line:
            return line.split()[0]
    return None


def build_init_cpio(cpio_path):
    initrd_root = tempfile.mkdtemp()
    try:
        dest = os.path.join(initrd_root, "init")
        shutil.copy2(os.path.join(ASSETS_DIR, "init"), dest)
        os.chmod(dest, 0o755)

        find = subprocess.Popen(
            ["find", ".", "-print0"], cwd=initrd_root, stdout=subprocess.PIPE,
        )
        with open(cpio_path, "wb") as out:
            cpio = subprocess.Popen(
                ["cpio", "--create", "--format=newc", "--quiet", "--null"],
                cwd=initrd_root, stdin=find.stdout, stdout=out,
            )
            if find.stdout:
                find.stdout.close()
            cpio.communicate()

        if cpio.returncode != 0:
            die("cpio failed")
    finally:
        shutil.rmtree(initrd_root)


def build_user_data():
    config = {
        "manage_etc_hosts": True,
        "growpart": {"mode": "auto", "devices": ["/"]},
        "resize_rootfs": True,
        "swap": {"size": 0},
        "users": [{
            "name": "unbounded-metal",
            "lock_passwd": True,
            "shell": "/bin/bash",
            "sudo": "ALL=(ALL) NOPASSWD:ALL",
        }],
        "ssh_pwauth": False,
        "write_files": [
            {
                "path": "/etc/modules-load.d/k8s.conf",
                "content": "overlay\nbr_netfilter\n",
            },
            {
                "path": "/etc/sysctl.d/k8s.conf",
                "content": (
                    "net.bridge.bridge-nf-call-iptables  = 1\n"
                    "net.bridge.bridge-nf-call-ip6tables = 1\n"
                    "net.ipv4.ip_forward                 = 1\n"
                ),
            },
            {
                "path": "/etc/containerd/config.toml",
                "defer": True,
                "content": (
                    "version = 2\n"
                    '[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]\n'
                    '  runtime_type = "io.containerd.runc.v2"\n'
                    '[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]\n'
                    "  SystemdCgroup = true\n"
                ),
            },
            {
                "path": "/usr/local/bin/unbounded-metal-attest",
                "permissions": "0755",
                "content": read_asset("unbounded-metal-attest.py"),
            },
            {
                "path": "/etc/cloud/cloud-init.disabled",
                "defer": True,
                "content": "",
            },
        ],
        "apt": {
            "sources": {
                "docker": {
                    "source": "deb [arch=amd64] https://download.docker.com/linux/ubuntu noble stable",
                    "keyid": "9DC858229FC7DD38854AE2D88D81803C0EBFCD88",
                },
                "kubernetes": {
                    "source": "deb [arch=amd64] https://pkgs.k8s.io/core:/stable:/v1.32/deb/ /",
                    "keyid": "234654DA9A296436",
                },
            },
        },
        "packages": ["containerd.io", "kubelet", "tpm2-tools", "python3-cryptography"],
        "runcmd": [
            ["modprobe", "overlay"],
            ["modprobe", "br_netfilter"],
            ["sysctl", "--system"],
            ["systemctl", "restart", "containerd"],
            ["systemctl", "start", "kubelet"],
        ],
    }
    return "#cloud-config\n" + yaml.dump(
        config, default_flow_style=False, sort_keys=False,
    )


def main():
    parser = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument(
        "--artifact-url", default=UBUNTU_NETBOOT_BASE,
        help="Base URL for boot artifacts in the Image CR (default: upstream Ubuntu netboot URL)",
    )
    parser.add_argument(
        "--artifact-dir", default="./artifacts/ubuntu-24-04",
        help="Local directory for downloaded artifacts (default: ./artifacts/ubuntu-24-04)",
    )
    parser.add_argument(
        "--output", "-o", default=None,
        help="Write YAML to FILE instead of stdout",
    )
    args = parser.parse_args()

    artifact_url = args.artifact_url.rstrip("/")
    os.makedirs(args.artifact_dir, exist_ok=True)

    if not shutil.which("cpio"):
        die("required tool 'cpio' not found in PATH")

    # ── Download netboot artifacts ──
    print("==> Downloading netboot artifacts...", file=sys.stderr)
    checksums = {}
    for local_name, remote_name in NETBOOT_FILES:
        dest = os.path.join(args.artifact_dir, local_name)
        checksums[local_name] = download_cached(f"{UBUNTU_NETBOOT_BASE}/{remote_name}", dest)

    # ── Build init.cpio overlay ──
    print("==> Building init.cpio overlay...", file=sys.stderr)
    init_cpio_path = os.path.join(args.artifact_dir, "init.cpio")
    build_init_cpio(init_cpio_path)
    with open(init_cpio_path, "rb") as f:
        init_cpio_b64 = base64.b64encode(f.read()).decode()
    print(f"  init.cpio: {len(init_cpio_b64)} bytes (base64)", file=sys.stderr)

    # ── Fetch upstream cloud image checksum ──
    print("==> Fetching upstream cloud image checksum...", file=sys.stderr)
    cloudimg_sha256 = fetch_cloudimg_sha256()
    if not cloudimg_sha256:
        die("could not find upstream SHA256 checksum for cloud image")
    print(f"  cloud image: {cloudimg_sha256}", file=sys.stderr)

    # ── Construct Image CR ──
    files = []
    for local_name, remote_name in NETBOOT_FILES:
        files.append({
            "path": local_name,
            "http": {
                "url": f"{artifact_url}/{remote_name}",
                "sha256": checksums[local_name],
            },
        })
    files.append({
        "path": "init.cpio",
        "static": {
            "content": init_cpio_b64,
            "encoding": "base64",
        },
    })
    files.append({
        "path": "disk.img.gz",
        "http": {
            "url": UBUNTU_CLOUDIMG_URL,
            "sha256": cloudimg_sha256,
            "convert": "UnpackQcow2",
        },
    })
    files.append({"path": "grub/grub.cfg", "template": {"content": GRUB_CFG}})
    files.append({"path": "cloud-init/user-data", "static": {"content": build_user_data()}})
    files.append({"path": "cloud-init/meta-data", "template": {"content": META_DATA_TPL}})
    files.append({"path": "cloud-init/vendor-data", "template": {"content": VENDOR_DATA_TPL}})
    files.append({"path": "bootstrap-kubelet.conf", "template": {"content": BOOTSTRAP_KUBECONFIG_TPL}})
    files.append({"path": "kubelet-config.yaml", "template": {"content": KUBELET_CONFIG_TPL}})
    files.append({"path": "kubelet-dropin.conf", "template": {"content": KUBELET_DROPIN}})

    image = {
        "apiVersion": "unbounded-kube.io/v1alpha3",
        "kind": "Image",
        "metadata": {"name": "ubuntu-24-04"},
        "spec": {
            "dhcpBootImageName": "shimx64.efi",
            "files": files,
        },
    }

    # ── Write output ──
    out = open(args.output, "w") if args.output else sys.stdout  # noqa: SIM115
    out.write("# Generated by: make images/ubuntu24\n")
    out.write(yaml.dump(image))
    out.write("\n")
    if args.output:
        out.close()

    print(f"==> Done. YAML written to {args.output or 'stdout'}", file=sys.stderr)


if __name__ == "__main__":
    main()
