# metalman

Join bare metal nodes to a Kubernetes cluster using PXE and (optionally) Redfish.

A dedicated `metalman` binary is shipped as a container image for running the
PXE server inside a cluster. `unbounded-operator` deploys that image when a
Site enables the Metalman component.

Run `metalman version` to print the binary version.

## Usage

```bash
# Create a Machine
kubectl apply -f - <<EOF
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: node-01
spec:
  pxe:
    image: ghcr.io/azure/host-ubuntu2404:v1
    dhcpLeases:
    - mac: "aa:bb:cc:dd:ee:01"
      ipv4: "10.0.0.11"
      subnetMask: "255.255.255.0"
      gateway: "10.0.0.1"
      dns: ["10.0.0.1"]
    redfish:
      url: https://10.0.10.11
      username: admin
      passwordRef:
        name: bmc-node-01-pass
        namespace: default
        key: password
EOF

# Store the BMC's Redfish password
kubectl create secret generic bmc-node-01-pass --from-literal=password=example-password

# Repave the node via PXE
kubectl unbounded machine repave node-01
```


## Concepts

### Controller

`metalman serve-pxe` runs a single long-lived process that provides
everything needed to PXE-boot and manage bare metal hosts:

| Service | Default Port | Protocol | Purpose |
|---------|-------------|----------|---------|
| DHCP    | 67/udp      | DHCPv4   | Static leases derived from Machine NIC specs for PXE or DHCP-assisted HTTP boot |
| TFTP    | 69/udp      | TFTP     | Initial bootloader delivery (e.g. shimx64.efi) |
| HTTP    | 8880/tcp    | HTTP     | Artifact serving, templated configs, attestation endpoints |
| Health  | 8081/tcp    | HTTP     | Liveness/readiness probes |

The controller also runs reconcilers for OCI image pulling (downloading and
caching machine and netboot images from container registries), Redfish TLS
certificate pinning, and `MachineOperation` host actions such as reboot and
repave.

When deployed inside a cluster, the container entrypoint is `metalman` and the
operator passes `serve-pxe` as an argument:

```bash
metalman serve-pxe --site=<site> [flags]
```

#### Deploying With Site Components

Enable Metalman in the Site spec and let `unbounded-operator` create or update
the per-site Deployment running `metalman serve-pxe`:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Site
metadata:
  name: rack-a
spec:
  components:
    metalman:
      enabled: true
      dhcpAutoInterface: true
```

The resulting Deployment (`metalman-controller-<site>`) runs with host networking for DHCP.
It exposes ports 8880/tcp (HTTP), 8081/tcp (health), 67/udp (DHCP), and 69/udp (TFTP).

The generated Deployment uses host networking, a `CriticalAddonsOnly`
toleration, DNS policy `ClusterFirstWithHostNet`, and a node selector
`unbounded-cloud.io/site=<site>`. Resource requests are 100m CPU / 128Mi
memory with limits of 500m CPU / 256Mi memory.

#### DHCP Modes

The DHCP server operates in one of two modes depending on whether
`--dhcp-interface` is set:

- **Interface mode** (`--dhcp-interface=eth0`): Binds to a network interface
  and listens for broadcast DHCP traffic. Use this when the controller is
  directly attached to the provisioning network.

- **Auto-interface mode** (`--dhcp-auto-interface`): Automatically detects
  the network interface from the server bind address. Mutually exclusive
  with `--dhcp-interface`.

- **Relay mode** (no `--dhcp-interface` or `--dhcp-auto-interface`): Listens
  on a UDP port for unicast packets only. Use this when a DHCP relay agent
  forwards requests from a remote subnet.

Leader election is always enabled regardless of DHCP mode. Each site gets
its own leader-election lease (`metalman-<site>`).

#### Security Model

A mostly-trusted network between the controller and the bare metal hosts is
assumed. Bootstrap tokens (Kubernetes ServiceAccount tokens) are issued to
nodes based on source IP - the controller looks up the Machine whose NIC
matches the requesting IP and issues a short-lived token for that node.

Bootstrap tokens are delivered using the standard TPM 2.0 credential encryption workflow.
The client's endorsement key (EK) public key is stored in `status.tpm.ekPublicKey` when first seen.
So it's possible to prove that the bootstrap token was delivered only to trusted hosts.

#### Sites

The `--site` flag scopes a `metalman serve-pxe` instance to a subset of Machines.
The value is matched against the `unbounded-cloud.io/site` label on Machine
resources:

```bash
# Manage only Machines labeled site=rack-a
metalman serve-pxe --site=rack-a --dhcp-interface=eth0

# Manage only unlabeled Machines (the default)
metalman serve-pxe --dhcp-interface=eth0
```

Each site gets its own leader-election lease (`metalman-<site>`), so
multiple sites can coexist on one cluster with independent HA. A `metalman serve-pxe`
instance with no `--site` manages Machines that do not have the site label
at all.

### Images

Metalman uses two OCI images when repaving a machine:

Existing Machines may continue to use the deprecated top-level `spec.pxe`
shape; Metalman resolves both forms through the same compatibility accessor.

- `spec.host.netboot.image` is the machine image. It contains `/disk/disk.img.gz`, a
  gzip-compressed raw disk image written to the target disk.
- `spec.host.netboot.netbootImage` is the reusable PXE boot environment. It contains
  bootloaders, kernel, initrd, templates, and metadata. Its cloud-init template
  downloads and installs `unbounded-agent` from the configured release/source.
  If omitted, Metalman uses the release-matched `--default-netboot-image`.

Both images are built `FROM scratch` and use `/disk/` as the artifact root,
following the kubevirt containerDisk convention. Files with a `.tmpl` suffix in
the netboot image are Go templates rendered per-machine at serve time; other
files are served verbatim. A `metadata.yaml` file in the netboot image provides
image-level configuration such as `dhcpBootImageName` and `httpBootPath`.

Images are built, tagged, and pushed using standard container tooling:

```bash
docker build -t ghcr.io/azure/host-ubuntu2404:v1 -f images/host-ubuntu2404/Containerfile .
docker build -t ghcr.io/azure/netboot:v1 -f images/netboot/Containerfile .
docker push ghcr.io/azure/host-ubuntu2404:v1
docker push ghcr.io/azure/netboot:v1
```

### Machine

A Machine is a cluster-scoped custom resource representing a single bare metal
host. At minimum it needs a NIC (MAC + static IP) and a machine image reference:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: node-01
spec:
  pxe:
    image: ghcr.io/azure/host-ubuntu2404:v1
    # Defaults to PXE. Set to HTTP to use Redfish UEFI HTTP boot.
    bootProtocol: PXE
    # Optional. Recommended when the host has multiple disks.
    targetDisk: /dev/disk/by-id/example-os-disk
    dhcpLeases:
    - mac: "aa:bb:cc:dd:ee:01"
      ipv4: "10.0.0.11"
      subnetMask: "255.255.255.0"
      gateway: "10.0.0.1"
```

This is enough for the DHCP server to issue a lease and for TFTP/HTTP to serve
boot artifacts from the default netboot image. Set `spec.host.netboot.netbootImage` only
when a Machine needs a non-default PXE boot environment. The node must be
manually PXE-booted (or have PXE as its default boot option).

When `spec.host.netboot.bootProtocol` is `HTTP`, `dhcpLeases` also supplies the static
UEFI HTTP boot client configuration. Metalman uses Redfish to disable DHCPv4 on
the host EthernetInterface matching the first lease MAC and writes that lease's
IPv4 address, subnet mask, gateway, and DNS servers before setting the UEFI HTTP
boot override. With Redfish access and an HTTP boot URL, repaving can run without
any DHCP server on the provisioning network. If a host has multiple NICs, put the
UEFI HTTP boot NIC first in `dhcpLeases`.

The default netboot template passes the selected lease MAC to the installer
initrd, which uses it to select the provisioning NIC instead of assuming a fixed
interface name such as `eth0`. It also passes the lease DNS servers, configures
the installer network without DHCP, and writes matching MAC-based static netplan
configuration into the installed system before its first boot. It disables
cloud-init network rendering so fallback DHCP configuration cannot conflict with
that file. The default netboot image serves the same lease as NoCloud
`network-config`. If `spec.host.netboot.targetDisk` is
set, the installer writes the image to that disk; otherwise it falls back to
automatic disk selection.

Stock Ubuntu OVMF and sushy-emulator cannot emulate the complete DHCP-free
Redfish-to-firmware UEFI HTTP path. Repository CI therefore tests Metalman's
Redfish writes and then starts at a staged post-firmware EFI boundary, while
capturing the guest's traffic through installation and reboot to prove it emits
no DHCP packets. Applying Redfish settings and fetching the first EFI binary
remain firmware and BMC hardware-conformance responsibilities.

#### BMC

Adding a `redfish` block enables remote power management. Metalman uses it for
`MachineOperation` host actions without physical access:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: node-01
spec:
  pxe:
    image: ghcr.io/azure/host-ubuntu2404:v1
    dhcpLeases:
    - mac: "aa:bb:cc:dd:ee:01"
      ipv4: "10.0.0.11"
      subnetMask: "255.255.255.0"
      gateway: "10.0.0.1"
    redfish:
      url: https://bmc-node-01.example.com
      username: admin
      passwordRef:
        name: bmc-node-01-pass
        namespace: default
        key: password
```

The BMC password is read from a Secret in the same namespace (key: `password`).
On first connection, the controller captures the BMC's TLS certificate
fingerprint and pins it in `status.redfish.certFingerprint` for subsequent
requests.

To repave a node with BMC access:

```bash
kubectl unbounded machine repave node-01
```

This creates a `HostReplace` `MachineOperation`. Metalman handles the rest: it
configures the boot override for the selected `spec.host.netboot.bootProtocol`, executes
a Redfish force restart, waits for the installer `/pxe/disable` signal, tracks
first-boot cloud-init on the operation, and completes after the node is back up.
