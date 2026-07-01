# metalman

Join bare metal nodes to a Kubernetes cluster using PXE and (optionally) Redfish.

Metalman commands are available through the `kubectl unbounded` plugin. A
dedicated `metalman` binary is also shipped as a container image for running
the PXE server inside a cluster.

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

`kubectl unbounded site serve-pxe` runs a single long-lived process that provides
everything needed to PXE-boot and manage bare metal hosts:

| Service | Default Port | Protocol | Purpose |
|---------|-------------|----------|---------|
| DHCP    | 67/udp      | DHCPv4   | Static leases derived from Machine NIC specs |
| TFTP    | 69/udp      | TFTP     | Initial bootloader delivery (e.g. shimx64.efi) |
| HTTP    | 8880/tcp    | HTTP     | Artifact serving, templated configs, attestation endpoints |
| Health  | 8081/tcp    | HTTP     | Liveness/readiness probes |

The controller also runs reconcilers for OCI image pulling (downloading and
caching machine and netboot images from container registries) and Machine resources with
Redfish BMC specs (power management, boot order configuration).

When deployed inside a cluster, the container entrypoint is `metalman` and the
`site deploy-pxe` command passes `serve-pxe` as an argument:

```bash
metalman serve-pxe --site=<site> [flags]
```

#### Deploying with `site deploy-pxe`

`kubectl unbounded site deploy-pxe` is a convenience command that creates (or
updates) a Kubernetes Deployment running `metalman serve-pxe` for a given
site. The Deployment is server-side applied into the `unbounded-kube`
namespace.

```bash
# Deploy the PXE server for a site called "rack-a"
kubectl unbounded site deploy-pxe --site=rack-a
```

The resulting Deployment (`metalman-controller-<site>`) runs with host networking for DHCP.
It exposes ports 8880/tcp (HTTP), 8081/tcp (health), 67/udp (DHCP), and 69/udp (TFTP).

`site deploy-pxe` flags:

- `--site` - Site name (required; scopes the PXE instance to machines
  labeled `unbounded-cloud.io/site=<site>`).
- `--image` - Container image for the PXE deployment (default: build-time
  value or `metalman:latest`).
- `--default-netboot-image` - OCI image containing PXE boot artifacts to use
  when a Machine omits `spec.pxe.netbootImage`.
- `--kubeconfig` - Path to kubeconfig file.

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

The `--site` flag scopes a `site serve-pxe` instance to a subset of Machines. The
value is matched against the `unbounded-cloud.io/site` label on Machine
resources:

```bash
# Manage only Machines labeled site=rack-a
kubectl unbounded site serve-pxe --site=rack-a --dhcp-interface=eth0

# Manage only unlabeled Machines (the default)
kubectl unbounded site serve-pxe --dhcp-interface=eth0
```

Each site gets its own leader-election lease (`metalman-<site>`), so
multiple sites can coexist on one cluster with independent HA. A `site serve-pxe`
instance with no `--site` manages Machines that do not have the site label
at all.

### Images

Metalman uses two OCI images when repaving a machine:

- `spec.pxe.image` is the machine image. It contains `/disk/disk.img.gz`, a
  gzip-compressed raw disk image written to the target disk.
- `spec.pxe.netbootImage` is the reusable PXE boot environment. It contains
  bootloaders, kernel, initrd, templates, and metadata. Its cloud-init template
  downloads and installs `unbounded-agent` from the configured release/source.
  If omitted, Metalman uses the release-matched `--default-netboot-image`.

Both images are built `FROM scratch` and use `/disk/` as the artifact root,
following the kubevirt containerDisk convention. Files with a `.tmpl` suffix in
the netboot image are Go templates rendered per-machine at serve time; other
files are served verbatim. A `metadata.yaml` file in the netboot image provides
image-level configuration such as `dhcpBootImageName`.

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
    dhcpLeases:
    - mac: "aa:bb:cc:dd:ee:01"
      ipv4: "10.0.0.11"
      subnetMask: "255.255.255.0"
      gateway: "10.0.0.1"
```

This is enough for the DHCP server to issue a lease and for TFTP/HTTP to serve
boot artifacts from the default netboot image. Set `spec.pxe.netbootImage` only
when a Machine needs a non-default PXE boot environment. The node must be
manually PXE-booted (or have PXE as its default boot option).

#### BMC

Adding a `redfish` block enables remote power management. The controller will
manage boot order and execute reboot cycles without physical access:

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

This increments `spec.operations.repaveCounter` and `spec.operations.rebootCounter`. The
controller handles the rest - it configures the boot order to PXE, executes a
ForceOff/On power cycle, and clears the condition once the node is back up.
