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
apiVersion: unbounded-kube.io/v1alpha3
kind: Machine
metadata:
  name: node-01
spec:
  pxe:
    image: ghcr.io/azure/images/host-ubuntu2404:v1
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

# Reimage the node via PXE
kubectl unbounded machine reimage node-01
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
caching netboot images from container registries) and Machine resources with
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

- `--site` — Site name (required; scopes the PXE instance to machines
  labeled `unbounded-kube.io/site=<site>`).
- `--image` — Container image for the PXE deployment (default: build-time
  value or `metalman:latest`).
- `--kubeconfig` — Path to kubeconfig file.

The generated Deployment uses host networking, a `CriticalAddonsOnly`
toleration, DNS policy `ClusterFirstWithHostNet`, and a node selector
`unbounded-kube.io/site=<site>`. Resource requests are 100m CPU / 128Mi
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
nodes based on source IP — the controller looks up the Machine whose NIC
matches the requesting IP and issues a short-lived token for that node.

Bootstrap tokens are delivered using the standard TPM 2.0 credential encryption workflow.
The client's endorsement key (EK) public key is stored in `status.tpm.ekPublicKey` when first seen.
So it's possible to prove that the bootstrap token was delivered only to trusted hosts.

#### Sites

The `--site` flag scopes a `site serve-pxe` instance to a subset of Machines. The
value is matched against the `unbounded-kube.io/site` label on Machine
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

#### Bootstrap Mode

When `--bootstrap` is passed to `serve-pxe`, metalman automatically creates
Machine objects for unknown MAC addresses that send DHCP requests. This
eliminates the need to pre-register machines before they PXE boot.

```bash
metalman serve-pxe --site=rack-a --dhcp-interface=eth0 \
  --bootstrap --bootstrap-image=ghcr.io/azure/images/host-ubuntu2404:v1
```

Each auto-created Machine:

- Uses `generateName: machine-` for random naming (e.g. `machine-xk9f2`).
- Contains a single DHCP lease entry with only the MAC address (the IP
  allocator fills in IP, subnet, and gateway from the Site).
- Is labeled with `unbounded-kube.io/site=<value>` when `--site` is set.

The `--bootstrap-image` flag is required when `--bootstrap` is enabled.

Deduplication guards prevent creating multiple Machines for the same MAC
address, even under rapid DHCP retry storms.

### Images

Netboot images are standard OCI container images built `FROM scratch` that
contain all files needed for PXE booting a machine under `/disk/`. This
follows the kubevirt containerDisk convention. Files
with a `.tmpl` suffix are Go templates rendered per-machine at serve time;
other files are served verbatim. A `metadata.yaml` file provides image-level
configuration (e.g. `dhcpBootImageName`).

Images are built, tagged, and pushed using standard container tooling:

```bash
docker build -t ghcr.io/azure/images/host-ubuntu2404:v1 .
docker push ghcr.io/azure/images/host-ubuntu2404:v1
```

The OCI image layout is the one described above: boot artifacts live under
`/disk/`, `.tmpl` files are rendered per-machine at serve time, and
`metadata.yaml` carries image-level configuration.

### Machine

A Machine is a cluster-scoped custom resource representing a single bare metal
host. At minimum it needs a NIC (MAC + static IP) and a PXE image reference:

```yaml
apiVersion: unbounded-kube.io/v1alpha3
kind: Machine
metadata:
  name: node-01
spec:
  pxe:
    image: ghcr.io/azure/images/host-ubuntu2404:v1
    dhcpLeases:
    - mac: "aa:bb:cc:dd:ee:01"
      ipv4: "10.0.0.11"
      subnetMask: "255.255.255.0"
      gateway: "10.0.0.1"
```

This is enough for the DHCP server to issue a lease and for TFTP/HTTP to serve
boot artifacts. The node must be manually PXE-booted (or have PXE as its
default boot option).

#### BMC

Adding a `redfish` block enables remote power management. The controller will
manage boot order and execute reboot cycles without physical access:

```yaml
apiVersion: unbounded-kube.io/v1alpha3
kind: Machine
metadata:
  name: node-01
spec:
  pxe:
    image: ghcr.io/azure/images/host-ubuntu2404:v1
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

To reimage a node with BMC access:

```bash
kubectl unbounded machine reimage node-01
```

This increments `spec.operations.reimageCounter` and `spec.operations.rebootCounter`. The
controller handles the rest — it configures the boot order to PXE, executes a
ForceOff/On power cycle, and clears the condition once the node is back up.
